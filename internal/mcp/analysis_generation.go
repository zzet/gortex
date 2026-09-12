package mcp

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/runtimeactivity"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

const analysisGenerationFormatVersion uint32 = 1

const (
	analysisGenerationWriteChunk = 1000
	analysisGenerationPruneBatch = 1000
	analysisGenerationPruneKeep  = 2
)

// viewGenerationScoped is the store-side generation axis, narrowed to the one
// method this package needs. Declared here rather than imported so a graph
// that is not a SQLite store (the in-memory fixtures) simply fails the type
// assertion and keeps the base-generation behaviour it always had.
type viewGenerationScoped interface {
	ViewGeneration() int64
}

// analysisViewGeneration reports the payload view generation the analysis
// passes actually read. populateAnalysisLocked walks s.graph
// (analysis_persistence.go: `analysisGraph := s.graph`), so that handle — not
// the indexer's base handle — names the corpus a cached analysis describes.
// A graph with no generation axis reads as the base corpus, generation 0.
func (s *Server) analysisViewGeneration() int64 {
	if scoped, ok := s.graph.(viewGenerationScoped); ok {
		return scoped.ViewGeneration()
	}
	return 0
}

// analysisGenerationStore returns the backend handle pinned to the generation
// the analysis is computed over.
//
// backendStore() hands back the indexer's own handle, which is the base corpus.
// When the server is serving a routed view, s.graph is a different generation,
// and persisting through the base handle stamps the cache with a generation
// the analysis was never computed over — a collision the mutation revision
// cannot catch, because it is one coarse process-local counter shared by every
// generation handle (store_sqlite/analysis_generation_state.go).
//
// When the two already agree — every non-routed deployment and every test
// fixture — the backend is returned unchanged, so this is a no-op there.
//
// WIRING STATUS: seam only. No production path can currently make the two
// disagree. serverstack.NewSharedServer opens ONE backend handle and hands the
// same object to indexer.New and to gortexmcp.NewServer, so s.graph,
// s.indexer.Graph() and the base store are one and the same and viewGen is 0 on
// all three; the other NewServer callers (cmd/gortex/eval_server.go,
// cmd/gortex/eval_recall.go, bench/daemon-latency) pass the same pair. Every
// analysis is therefore still written and read at view_gen 0 in production
// today, and this selector short-circuits on the equality below. The divergence
// it corrects is constructed by hand in analysis_generation_test.go via
// store.AtGeneration. Routing a view to a non-base generation is W4/W8 work;
// until that lands, "routed analysis caching works end to end" would be an
// over-read of this function.
func (s *Server) analysisGenerationStore() graph.Store {
	backend := s.backendStore()
	scoped, ok := backend.(*store_sqlite.Store)
	if !ok {
		return backend
	}
	viewGen := s.analysisViewGeneration()
	if viewGen == scoped.ViewGeneration() {
		return backend
	}
	if pinned := scoped.AtGeneration(viewGen); pinned != nil {
		return pinned
	}
	return backend
}

func (s *Server) analysisGenerationBackends() (graph.AnalysisGenerationStore, graph.AnalysisQueryStore) {
	backend := s.analysisGenerationStore()
	writer, _ := backend.(graph.AnalysisGenerationStore)
	query, _ := backend.(graph.AnalysisQueryStore)
	return writer, query
}

type analysisGenerationInput struct {
	header         graph.AnalysisGenerationHeader
	adjacency      analysis.AdjacencyPersistenceSnapshot
	communities    []graph.AnalysisCommunitySummary
	processes      []graph.AnalysisProcessSummary
	processSteps   map[string][]graph.AnalysisProcessStep
	concepts       []graph.AnalysisConcept
	conceptRelated map[string][]string
}

// normalizePersistedAnalysis gives cold and lazy materializations the same
// deterministic collection order. Community membership has no semantic order,
// while process Steps deliberately retain their call-tree preorder.
func normalizePersistedAnalysis(candidate *persistedAnalysis) {
	if candidate == nil {
		return
	}
	if candidate.communities != nil {
		for i := range candidate.communities.Communities {
			sort.Strings(candidate.communities.Communities[i].Members)
			sort.Strings(candidate.communities.Communities[i].Files)
		}
		sort.Slice(candidate.communities.Communities, func(i, j int) bool {
			return candidate.communities.Communities[i].ID < candidate.communities.Communities[j].ID
		})
	}
	if candidate.processes != nil {
		for i := range candidate.processes.Processes {
			sort.Strings(candidate.processes.Processes[i].Files)
		}
		sort.Slice(candidate.processes.Processes, func(i, j int) bool {
			return candidate.processes.Processes[i].ID < candidate.processes.Processes[j].ID
		})
		for nodeID := range candidate.processes.NodeToProcs {
			sort.Strings(candidate.processes.NodeToProcs[nodeID])
		}
	}
}

// sanitizeAnalysisFiles drops empty and duplicate paths from a community /
// process file list and returns it sorted.
//
// Communities and processes are keyed on nodes, and a node need not have a
// file: synthetic, external, and stub nodes carry an empty FilePath, and two
// nodes routinely share one. The store validates these lists as non-empty and
// unique and rejects the entire generation on the first violation, so an
// unfiltered list makes every analysis save fail — which silently costs both
// the warm-start cache and the header-only residency the save enables.
func sanitizeAnalysisFiles(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, file := range in {
		if file == "" {
			continue
		}
		if _, duplicate := seen[file]; duplicate {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

func prepareAnalysisGeneration(candidate persistedAnalysis, revision uint64) (analysisGenerationInput, error) {
	normalizePersistedAnalysis(&candidate)
	if candidate.communities == nil || candidate.leiden == nil || candidate.processes == nil ||
		candidate.pageRank == nil || candidate.adjacency == nil || candidate.autoConcepts == nil || candidate.hits == nil {
		return analysisGenerationInput{}, fmt.Errorf("analysis generation: incomplete candidate")
	}

	adjacency := candidate.adjacency.PersistenceSnapshot()
	communities := make([]graph.AnalysisCommunitySummary, 0, len(candidate.communities.Communities))
	for _, community := range candidate.communities.Communities {
		files := sanitizeAnalysisFiles(community.Files)
		communities = append(communities, graph.AnalysisCommunitySummary{
			ID: community.ID, Label: community.Label, Hub: community.Hub, ParentID: community.ParentID,
			Size: community.Size, Cohesion: community.Cohesion, Files: files,
		})
	}
	sort.Slice(communities, func(i, j int) bool { return communities[i].ID < communities[j].ID })

	// A process step can point at a node this generation does not carry:
	// BuildAdjacencySnapshot deliberately skips unresolved and dangling
	// endpoints to keep its dense index consistent, so analysis_nodes has no
	// row for them while DiscoverProcesses still walks them. The step insert
	// resolves node_rowid through analysis_nodes and rejects the generation
	// when it finds none. Drop those steps here, renumber the survivors so
	// ordinals stay contiguous, and report the surviving count as StepCount —
	// the store validates step_count against the persisted step rows.
	generationNodes := make(map[string]struct{}, len(adjacency.IDs))
	for _, nodeID := range adjacency.IDs {
		generationNodes[nodeID] = struct{}{}
	}
	processSteps := make(map[string][]graph.AnalysisProcessStep, len(candidate.processes.Processes))
	processes := make([]graph.AnalysisProcessSummary, 0, len(candidate.processes.Processes))
	for _, process := range candidate.processes.Processes {
		files := sanitizeAnalysisFiles(process.Files)
		steps := make([]graph.AnalysisProcessStep, 0, len(process.Steps))
		for _, step := range process.Steps {
			if _, known := generationNodes[step.ID]; !known {
				continue
			}
			steps = append(steps, graph.AnalysisProcessStep{
				ProcessID: process.ID, NodeID: step.ID, Ordinal: len(steps), Depth: step.Depth,
			})
		}
		processSteps[process.ID] = steps
		processes = append(processes, graph.AnalysisProcessSummary{
			ID: process.ID, Name: process.Name, EntryPoint: process.EntryPoint,
			StepCount: len(steps), Score: process.Score, Truncated: process.Truncated, Files: files,
		})
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].ID < processes[j].ID })

	conceptSnapshot := candidate.autoConcepts.PersistenceSnapshot()
	conceptSet := make(map[string]bool, len(conceptSnapshot.Vocab)+len(conceptSnapshot.Related))
	for _, token := range conceptSnapshot.Vocab {
		conceptSet[token] = true
	}
	for token, related := range conceptSnapshot.Related {
		if _, ok := conceptSet[token]; !ok {
			conceptSet[token] = false
		}
		for _, sibling := range related {
			if _, ok := conceptSet[sibling]; !ok {
				conceptSet[sibling] = false
			}
		}
	}
	conceptTokens := make([]string, 0, len(conceptSet))
	for token := range conceptSet {
		conceptTokens = append(conceptTokens, token)
	}
	sort.Strings(conceptTokens)
	concepts := make([]graph.AnalysisConcept, 0, len(conceptTokens))
	for _, token := range conceptTokens {
		concepts = append(concepts, graph.AnalysisConcept{Token: token, InVocabulary: conceptSet[token]})
	}

	return analysisGenerationInput{
		header: graph.AnalysisGenerationHeader{
			FormatVersion: analysisGenerationFormatVersion,
			GraphRevision: revision,
			CreatedAtUnix: time.Now().Unix(),
			NodeCount:     len(adjacency.IDs), CommunityCount: len(communities),
			ProcessCount: len(processes), ConceptCount: len(concepts),
			PageRankMax: candidate.pageRank.Max, AuthorityMax: candidate.hits.MaxAuth,
			HubMax: candidate.hits.MaxHub, Modularity: candidate.communities.Modularity,
			ProcessesTruncated:        candidate.processes.Truncated,
			ProcessesTruncationReason: candidate.processes.TruncationReason,
		},
		adjacency: adjacency, communities: communities, processes: processes, processSteps: processSteps,
		concepts: concepts, conceptRelated: conceptSnapshot.Related,
	}, nil
}

func persistAnalysisGeneration(
	writer graph.AnalysisGenerationStore,
	expectedRevision uint64,
	candidate persistedAnalysis,
) (graph.AnalysisGenerationHeader, bool, error) {
	input, err := prepareAnalysisGeneration(candidate, expectedRevision)
	if err != nil {
		return graph.AnalysisGenerationHeader{}, false, err
	}
	generationID, accepted, err := writer.BeginAnalysisGeneration(expectedRevision, input.header)
	if err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}
	activated := false
	defer func() {
		if !activated {
			_ = writer.AbortAnalysisGeneration(generationID)
		}
	}()

	// Communities precede nodes because a non-empty node community_id carries
	// an immediate foreign key to the immutable generation's community row.
	for start := 0; start < len(input.communities); start += analysisGenerationWriteChunk {
		end := min(start+analysisGenerationWriteChunk, len(input.communities))
		accepted, err = writer.AppendAnalysisCommunities(expectedRevision, generationID, input.communities[start:end])
		if err != nil || !accepted {
			return graph.AnalysisGenerationHeader{}, accepted, err
		}
	}
	if accepted, err = writer.SealAnalysisComponent(expectedRevision, generationID, graph.AnalysisComponentCommunities, input.header.CommunityCount); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}

	for start := 0; start < len(input.adjacency.IDs); start += analysisGenerationWriteChunk {
		end := min(start+analysisGenerationWriteChunk, len(input.adjacency.IDs))
		rows := make([]graph.AnalysisNodeMetric, 0, end-start)
		for _, nodeID := range input.adjacency.IDs[start:end] {
			rows = append(rows, graph.AnalysisNodeMetric{
				NodeID: nodeID, CommunityID: candidate.communities.NodeToComm[nodeID],
				PageRank: candidate.pageRank.Scores[nodeID], Authority: candidate.hits.Authorities[nodeID],
				Hub: candidate.hits.Hubs[nodeID],
			})
		}
		accepted, err = writer.AppendAnalysisNodes(expectedRevision, generationID, rows)
		if err != nil || !accepted {
			return graph.AnalysisGenerationHeader{}, accepted, err
		}
	}
	if accepted, err = writer.SealAnalysisComponent(expectedRevision, generationID, graph.AnalysisComponentNodes, input.header.NodeCount); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}

	for start := 0; start < len(input.processes); start += analysisGenerationWriteChunk {
		end := min(start+analysisGenerationWriteChunk, len(input.processes))
		accepted, err = writer.AppendAnalysisProcesses(expectedRevision, generationID, input.processes[start:end], nil)
		if err != nil || !accepted {
			return graph.AnalysisGenerationHeader{}, accepted, err
		}
	}
	for _, process := range input.processes {
		steps := input.processSteps[process.ID]
		for start := 0; start < len(steps); start += analysisGenerationWriteChunk {
			end := min(start+analysisGenerationWriteChunk, len(steps))
			accepted, err = writer.AppendAnalysisProcesses(expectedRevision, generationID, nil, steps[start:end])
			if err != nil || !accepted {
				return graph.AnalysisGenerationHeader{}, accepted, err
			}
		}
	}
	if accepted, err = writer.SealAnalysisComponent(expectedRevision, generationID, graph.AnalysisComponentProcesses, input.header.ProcessCount); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}

	for start := 0; start < len(input.concepts); start += analysisGenerationWriteChunk {
		end := min(start+analysisGenerationWriteChunk, len(input.concepts))
		accepted, err = writer.AppendAnalysisConcepts(expectedRevision, generationID, input.concepts[start:end], nil)
		if err != nil || !accepted {
			return graph.AnalysisGenerationHeader{}, accepted, err
		}
	}
	conceptTokens := make([]string, 0, len(input.conceptRelated))
	for token := range input.conceptRelated {
		conceptTokens = append(conceptTokens, token)
	}
	sort.Strings(conceptTokens)
	relations := make([]graph.AnalysisConceptRelation, 0, analysisGenerationWriteChunk)
	flushRelations := func() (bool, error) {
		if len(relations) == 0 {
			return true, nil
		}
		ok, flushErr := writer.AppendAnalysisConcepts(expectedRevision, generationID, nil, relations)
		relations = relations[:0]
		return ok, flushErr
	}
	for _, token := range conceptTokens {
		for rank, sibling := range input.conceptRelated[token] {
			relations = append(relations, graph.AnalysisConceptRelation{Token: token, RelatedToken: sibling, Rank: rank})
			if len(relations) == cap(relations) {
				accepted, err = flushRelations()
				if err != nil || !accepted {
					return graph.AnalysisGenerationHeader{}, accepted, err
				}
			}
		}
	}
	if accepted, err = flushRelations(); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}
	if accepted, err = writer.SealAnalysisComponent(expectedRevision, generationID, graph.AnalysisComponentConcepts, input.header.ConceptCount); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}

	adjacencyPayload, err := encodeAnalysisBlobValue(input.adjacency)
	if err != nil {
		return graph.AnalysisGenerationHeader{}, false, fmt.Errorf("encode adjacency: %w", err)
	}
	if accepted, err = writer.PutAnalysisBlob(expectedRevision, generationID, graph.AnalysisBlob{Component: graph.AnalysisBlobAdjacency, Payload: adjacencyPayload}); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}
	if accepted, err = writer.SealAnalysisComponent(expectedRevision, generationID, graph.AnalysisComponentAdjacency, 1); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}

	leidenPayload, err := encodeAnalysisBlobValue(candidate.leiden.PersistenceSnapshot())
	if err != nil {
		return graph.AnalysisGenerationHeader{}, false, fmt.Errorf("encode Leiden state: %w", err)
	}
	if accepted, err = writer.PutAnalysisBlob(expectedRevision, generationID, graph.AnalysisBlob{Component: graph.AnalysisBlobLeiden, Payload: leidenPayload}); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}
	if accepted, err = writer.SealAnalysisComponent(expectedRevision, generationID, graph.AnalysisComponentLeiden, 1); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}

	if accepted, err = writer.ActivateAnalysisGeneration(expectedRevision, generationID); err != nil || !accepted {
		return graph.AnalysisGenerationHeader{}, accepted, err
	}
	activated = true
	input.header.GenerationID = generationID
	return input.header, true, nil
}

func (s *Server) scheduleAnalysisGenerationPrune(writer graph.AnalysisGenerationStore) {
	if writer == nil || !s.analysisPruneScheduled.CompareAndSwap(false, true) {
		return
	}
	// Add under the gate so it cannot start at counter zero concurrently
	// with DrainBackground's Wait (a WaitGroup misuse panic), and so a
	// prune requested after the drain is refused instead of escaping it
	// to write through a store that is about to close.
	s.backgroundMaintenanceMu.Lock()
	if s.backgroundMaintenanceDrained {
		s.backgroundMaintenanceMu.Unlock()
		s.analysisPruneScheduled.Store(false)
		return
	}
	s.backgroundMaintenance.Add(1)
	s.backgroundMaintenanceMu.Unlock()
	go func() {
		defer s.backgroundMaintenance.Done()
		defer s.analysisPruneScheduled.Store(false)
		runtimeactivity.Begin("analysis_generation_gc")
		defer runtimeactivity.End("analysis_generation_gc")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := writer.PruneAnalysisGenerations(ctx, analysisGenerationPruneKeep, analysisGenerationPruneBatch); err != nil && s.logger != nil {
			s.logger.Warn("mcp: analysis generation prune failed", zap.Error(err))
		}
	}()
}

// DrainBackground waits for any in-flight analysis-generation prune and
// permanently refuses to schedule new ones. Call it before closing the backend
// store: the prune keeps writing on its already-acquired connection after
// sql.DB.Close, and a commit there recreates WAL files under a directory being
// torn down (a test TempDir, an uninstall). The wait ends once the prune's
// 30s context expires between chunks, though an in-flight chunk can first
// queue behind the store's writer gate. It does not quiesce RunAnalysis
// itself: a pass still running on a detached caller (on-demand ensureAnalysis,
// the track_repository worker) keeps writing generations through the store,
// and stopping those is the caller's lifecycle problem, not this drain's.
func (s *Server) DrainBackground() {
	s.backgroundMaintenanceMu.Lock()
	s.backgroundMaintenanceDrained = true
	s.backgroundMaintenanceMu.Unlock()
	s.backgroundMaintenance.Wait()
}

func restoreAdjacencyBlob(payload []byte) (*analysis.AdjacencySnapshot, error) {
	var snapshot analysis.AdjacencyPersistenceSnapshot
	if err := decodeAnalysisBlobValue(payload, &snapshot); err != nil {
		return nil, err
	}
	return analysis.RestoreAdjacencySnapshot(snapshot)
}

func restoreLeidenBlob(payload []byte, edgeIdentityRevision int) (*analysis.LeidenPartitionCache, error) {
	var snapshot analysis.LeidenPartitionSnapshot
	if err := decodeAnalysisBlobValue(payload, &snapshot); err != nil {
		return nil, err
	}
	return analysis.RestoreLeidenPartitionCache(snapshot, edgeIdentityRevision), nil
}

func conceptsFromQuery(result graph.AnalysisConceptQueryResult) *search.AutoConcepts {
	related := make(map[string][]string, len(result.Concepts))
	vocab := make([]string, 0, len(result.Concepts))
	for _, concept := range result.Concepts {
		if concept.InVocabulary {
			vocab = append(vocab, concept.Token)
		}
	}
	for _, relation := range result.Relations {
		related[relation.Token] = append(related[relation.Token], relation.RelatedToken)
	}
	return search.RestoreAutoConcepts(search.AutoConceptsPersistenceSnapshot{Related: related, Vocab: vocab})
}
