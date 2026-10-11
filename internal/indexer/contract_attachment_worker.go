package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/fixtures"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

// ContractFollowupFile is one member of a complete accepted source census.
// Path is the indexed graph path; fingerprints/policy bind historical bytes.
type ContractFollowupFile struct {
	RepoPrefix, WorkspaceID, ProjectID        string
	Path, Language, SourceFingerprint, Policy string
}

type ContractAcceptedSource struct {
	Bytes                     []byte
	SourceFingerprint, Policy string
}

// ContractFollowupCoreFile contains complete selected declaration and outgoing
// edge evidence. The caller must use checked reads and reject truncation.
type ContractFollowupCoreFile struct {
	Nodes []*graph.Node
	Edges []*graph.Edge
}

// ContractFollowupSnapshot is captured before queue admission under the actual
// selected view handoff. Files must be the complete accepted namespace census,
// not only Work's changed files. ReadAccepted serves accepted transformed
// extractor bytes with their exact source/policy proof; it never falls back to current
// filesystem bytes. The runner owns Release on every exit.
type ContractFollowupSnapshot struct {
	Key    graph.ContractAttachmentKey
	Core   graph.Reader
	Inputs []graph.ContractInputWitness
	Work   []graph.ContractWork
	Files  []ContractFollowupFile
	// RepoConfigs is captured immutable extractor/transform policy authority.
	RepoConfigs map[string]config.IndexConfig
	// RepoExtractionOptions freezes repository parser options for baseline replay.
	RepoExtractionOptions map[string]parser.ExtractionOptions
	// TrackedRepoModules freezes accepted companion module identity.
	TrackedRepoModules map[string]string
	ReadAccepted       func(context.Context, ContractFollowupFile) (ContractAcceptedSource, error)
	ReadCoreFile       func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error)
	ValidateAccepted   func(context.Context) error
	Release            func()
}

// ContractFollowupRequest receives an installer-reserved Building managed
// payload. Catalog retains the selected core generation used for CAS; it is
// not an actor-latest handle. Leases is the materializer's shared lease manager.
// Yield lets the existing scheduler defer to interactive load between files.
type ContractFollowupRebuildReason string

const (
	ContractFollowupColdBaseline    ContractFollowupRebuildReason = "cold_baseline"
	ContractFollowupUnknownInterval ContractFollowupRebuildReason = "unknown_interval"
	ContractFollowupChangedBoundary ContractFollowupRebuildReason = "changed_boundary"
)

type ContractFollowupRequest struct {
	Snapshot ContractFollowupSnapshot
	// Explicit full rebuild reason makes boundary-edit cost visible. Unchanged
	// identity reuses the exact attachment upstream and must schedule no worker.
	RebuildReason          ContractFollowupRebuildReason
	Payload, Catalog       *store_sqlite.Store
	Registry               *parser.Registry
	Config                 config.IndexConfig
	Logger                 *zap.Logger
	Leases                 *graphview.LeaseManager
	WorkspaceID, ProjectID string
	RepoConfigs            map[string]config.IndexConfig
	ScratchParent          string
	Yield                  func(context.Context) error
}

type ContractFollowupReport struct {
	Files, Records, Nodes, Edges, Shapes             int
	SourceReads, SourceBytes                         int
	RebuildReason                                    ContractFollowupRebuildReason
	ExtractDuration, EnrichDuration, PublishDuration time.Duration
	PayloadGeneration                                int64
	ScratchNodes, ScratchEdges                       int
	ScratchBytes                                     int64
	PrepareDuration                                  time.Duration
	SymbolDocuments                                  int
	FTSDuration                                      time.Duration
	Published                                        bool
}

// ContractFollowupTarget reserves one independently published repository payload.
// Catalog is the exact selected receiver handle, not an actor-latest lookup.
type ContractFollowupTarget struct {
	Key              graph.ContractAttachmentKey
	Work             []graph.ContractWork
	Payload, Catalog *store_sqlite.Store
}

// ContractFollowupBatchRequest uses one proof-bound cohort and source lifetime.
// Snapshot.Key/Work are ignored; target keys and work remain independent.
type ContractFollowupBatchRequest struct {
	Snapshot               ContractFollowupSnapshot
	Targets                []ContractFollowupTarget
	RebuildReason          ContractFollowupRebuildReason
	Registry               *parser.Registry
	Config                 config.IndexConfig
	Logger                 *zap.Logger
	Leases                 *graphview.LeaseManager
	WorkspaceID, ProjectID string
	RepoConfigs            map[string]config.IndexConfig
	ScratchParent          string
	Yield                  func(context.Context) error
}

type ContractFollowupTargetResult struct {
	Key    graph.ContractAttachmentKey
	Report ContractFollowupReport
	Err    error
}

type ContractFollowupBatchReport struct {
	Cohort  ContractFollowupReport
	Targets []ContractFollowupTargetResult
}

type contractFollowupPrepared struct {
	registry   *contracts.Registry
	evidence   *contractFollowupEvidence
	report     ContractFollowupReport
	scratch    *store_sqlite.Store
	scratchDir string
}

// RunContractFollowupBatch extracts and enriches the captured cohort once.
// Each target keeps its own payload, immutable work, publication CAS and result.
func RunContractFollowupBatch(ctx context.Context, req ContractFollowupBatchRequest) (batch ContractFollowupBatchReport, err error) {
	if req.Snapshot.Release != nil {
		defer req.Snapshot.Release()
	}
	if len(req.Targets) == 0 || req.Snapshot.Release == nil || req.Leases == nil {
		return batch, fmt.Errorf("contract followup batch: incomplete target handoff")
	}
	seen := make(map[graph.ContractAttachmentKey]bool)
	payloads := make(map[int64]bool)
	for _, target := range req.Targets {
		if target.Payload == nil || target.Catalog == nil || target.Payload.ViewGeneration() <= 0 || seen[target.Key] || payloads[target.Payload.ViewGeneration()] {
			return batch, fmt.Errorf("contract followup batch: invalid or duplicate target")
		}
		seen[target.Key] = true
		payloads[target.Payload.ViewGeneration()] = true
		lease := req.Leases.Acquire(target.Payload.ViewGeneration())
		defer lease.Release()
	}
	prepared := &contractFollowupPrepared{}
	defer func() {
		if cleanupErr := prepared.close(); err == nil {
			err = cleanupErr
		}
	}()
	for _, target := range req.Targets {
		snap := req.Snapshot
		snap.Key = target.Key
		snap.Work = target.Work
		snap.Release = func() {}
		single := ContractFollowupRequest{Snapshot: snap, RebuildReason: req.RebuildReason, Payload: target.Payload, Catalog: target.Catalog, Registry: req.Registry, Config: req.Config, Logger: req.Logger, Leases: req.Leases, WorkspaceID: req.WorkspaceID, ProjectID: req.ProjectID, RepoConfigs: req.RepoConfigs, ScratchParent: req.ScratchParent, Yield: req.Yield}
		report, targetErr := runContractFollowupPrepared(ctx, single, prepared)
		batch.Targets = append(batch.Targets, ContractFollowupTargetResult{Key: target.Key, Report: report, Err: targetErr})
		if prepared.registry == nil && targetErr != nil {
			// Preparation failure invalidates the shared cohort; never replay its source
			// census once per remaining target or publish from partial evidence.
			for _, remaining := range req.Targets[len(batch.Targets):] {
				batch.Targets = append(batch.Targets, ContractFollowupTargetResult{Key: remaining.Key, Err: targetErr})
			}
			batch.Cohort = report
			return batch, targetErr
		}
	}
	batch.Cohort = prepared.report
	return batch, nil
}

// RunContractFollowup performs no ordinary core indexing or route publication.
// All legacy contract enrichment operates on private disk-backed evidence.
func RunContractFollowup(ctx context.Context, req ContractFollowupRequest) (report ContractFollowupReport, err error) {
	prepared := &contractFollowupPrepared{}
	defer func() {
		if cleanupErr := prepared.close(); err == nil {
			err = cleanupErr
		}
	}()
	return runContractFollowupPrepared(ctx, req, prepared)
}

func runContractFollowupPrepared(ctx context.Context, req ContractFollowupRequest, prepared *contractFollowupPrepared) (report ContractFollowupReport, err error) {
	snap := req.Snapshot
	if snap.Release != nil {
		defer snap.Release()
	}
	if ctx == nil || snap.Release == nil || snap.Core == nil || snap.ReadAccepted == nil || snap.ReadCoreFile == nil || req.Payload == nil || req.Catalog == nil || req.Leases == nil || req.Yield == nil {
		return report, fmt.Errorf("contract followup: incomplete selected handoff")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if req.RebuildReason != ContractFollowupColdBaseline && req.RebuildReason != ContractFollowupUnknownInterval && req.RebuildReason != ContractFollowupChangedBoundary {
		return report, fmt.Errorf("contract followup: explicit rebuild reason required")
	}
	report.RebuildReason = req.RebuildReason
	generation := req.Payload.ViewGeneration()
	if generation <= 0 {
		return report, fmt.Errorf("contract followup: payload must be isolated positive generation")
	}
	lease := req.Leases.Acquire(generation)
	defer lease.Release()
	report.PayloadGeneration = generation
	logical, err := graph.ComposeContractInputState(snap.Key.RepoPrefix, snap.Key.CheckoutID, snap.Inputs)
	if err != nil {
		return report, err
	}
	if !logical.Accepted || logical.InputVersion != snap.Key.InputVersion || logical.InputFingerprint != snap.Key.InputFingerprint {
		return report, graph.ErrContractProjectionStale
	}
	registry, evidence := prepared.registry, prepared.evidence
	if registry == nil {
		fileByPath := make(map[string]ContractFollowupFile, len(snap.Files))
		repos := make(map[string]bool)
		for _, w := range snap.Inputs {
			if w.Found && w.State.Accepted {
				repos[w.State.RepoPrefix] = true
			}
		}
		for _, file := range snap.Files {
			if file.Path == "" || file.SourceFingerprint == "" || file.Policy == "" || !repos[file.RepoPrefix] {
				return report, fmt.Errorf("contract followup: uncertified source census member %q", file.Path)
			}
			if _, duplicate := fileByPath[file.Path]; duplicate {
				return report, fmt.Errorf("contract followup: duplicate source path %q", file.Path)
			}
			if file.RepoPrefix != "" && !strings.HasPrefix(file.Path, file.RepoPrefix+"/") {
				return report, fmt.Errorf("contract followup: source namespace mismatch %q", file.Path)
			}
			fileByPath[file.Path] = file
		}
		prepareStart := time.Now()
		scratchDir, openErr := os.MkdirTemp(req.ScratchParent, "gortex-contract-evidence-")
		if openErr != nil {
			return report, openErr
		}
		prepared.scratchDir = scratchDir
		scratch, openErr := store_sqlite.Open(filepath.Join(scratchDir, "evidence.sqlite"))
		if openErr != nil {
			return report, openErr
		}
		prepared.scratch = scratch
		evidence = &contractFollowupEvidence{Store: scratch, scratch: scratch, ctx: ctx, core: snap.Core, files: fileByPath, readCore: snap.ReadCoreFile, allowedRepos: repos}
		logger := req.Logger
		if logger == nil {
			logger = zap.NewNop()
		}
		indexers := make(map[string]*Indexer)
		readSource := func(path string) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			file, ok := fileByPath[path]
			if !ok {
				return nil, fmt.Errorf("contract followup: accepted source outside census %q", path)
			}
			accepted, err := snap.ReadAccepted(ctx, file)
			if err != nil {
				return nil, err
			}
			if len(accepted.Bytes) > contractFollowupCompactLimit {
				return nil, graph.ErrContractProjectionLimit
			}
			if accepted.SourceFingerprint != file.SourceFingerprint || accepted.Policy != file.Policy || contractInputHash(accepted.Bytes) != file.SourceFingerprint {
				return nil, fmt.Errorf("%w: accepted source proof %s", errContractInputsChanged, path)
			}
			report.SourceReads++
			report.SourceBytes += len(accepted.Bytes)
			return bytes.Clone(accepted.Bytes), nil
		}
		for _, file := range snap.Files {
			if prior := indexers[file.RepoPrefix]; prior != nil {
				if prior.workspaceID != file.WorkspaceID || prior.projectID != file.ProjectID {
					return report, fmt.Errorf("contract followup: inconsistent accepted namespace scope %q", file.RepoPrefix)
				}
				continue
			}
			cfg := req.Config
			if override, ok := req.RepoConfigs[file.RepoPrefix]; ok {
				cfg = override
			}
			idx := &Indexer{graph: evidence, rootPath: filepath.Join(string(filepath.Separator), "contract-followup", file.RepoPrefix), repoPrefix: file.RepoPrefix, workspaceID: file.WorkspaceID, projectID: file.ProjectID, config: cfg, logger: logger, registry: req.Registry}
			opts := snap.RepoExtractionOptions[file.RepoPrefix]
			idx.extractionOptions.Store(&opts)
			idx.contractAnalysisOnly = true
			idx.contractAcceptedFileSource = readSource
			if reader, ok := snap.Core.(graph.SemanticBindingTypeReader); ok {
				idx.contractSemanticReader = reader
			}
			indexers[file.RepoPrefix] = idx
		}
		// A certified empty own namespace still receives a complete empty snapshot.
		if indexers[snap.Key.RepoPrefix] == nil {
			idx := &Indexer{graph: evidence, rootPath: "/contract-followup", repoPrefix: snap.Key.RepoPrefix, workspaceID: req.WorkspaceID, projectID: req.ProjectID, config: req.Config, logger: logger, registry: req.Registry}
			opts := snap.RepoExtractionOptions[snap.Key.RepoPrefix]
			idx.extractionOptions.Store(&opts)
			idx.contractAnalysisOnly = true
			idx.contractAcceptedFileSource = readSource
			indexers[snap.Key.RepoPrefix] = idx
		}
		files := slices.Clone(snap.Files)
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		extractStart := time.Now()
		registry = contracts.NewRegistry()
		retainedRecordBytes, retainedRecords := 0, 0
		for _, file := range files {
			if req.Yield != nil {
				if err := req.Yield(ctx); err != nil {
					return report, err
				}
			}
			if err := ctx.Err(); err != nil {
				return report, err
			}
			idx := indexers[file.RepoPrefix]
			projected, err := idx.contractFollowupPolicyMode(file.Language, file.Policy)
			if err != nil {
				return report, err
			}
			nodes, edges, err := evidence.loadFile(file.Path)
			if err != nil {
				return report, err
			}
			src, err := readSource(file.Path)
			if err != nil {
				return report, err
			}
			_, byLanguage := idx.buildPerFileContractExtractors()
			var tree *parser.ParseTree
			if projected {
				if !idx.contractGeneratedCoreProof(ctx, file, src, nodes, edges, snap.Core) {
					return report, fmt.Errorf("contract followup: unproved accepted projection %s", file.Path)
				}
			} else {
				tree = contracts.ParseTreeForLang(file.Language, src)
			}
			found := idx.collectContractRecordsForFile(file.Path, src, nodes, edges, byLanguage[file.Language], tree, evidence)
			if tree != nil {
				tree.Release()
			}
			if file.Language == "gomod" || strings.HasSuffix(file.Path, "/go.mod") || file.Path == "go.mod" {
				found = append(found, (&contracts.GoModExtractor{TrackedRepos: snap.TrackedRepoModules}).Extract(file.Path, src, nodes, edges)...)
			}
			registry.AddAllScoped(found, file.RepoPrefix, file.WorkspaceID, file.ProjectID)
			encoded, encodeErr := json.Marshal(found)
			if encodeErr != nil {
				return report, encodeErr
			}
			retainedRecordBytes += len(encoded)
			retainedRecords += len(found)
			if retainedRecordBytes > contractFollowupCompactLimit || retainedRecords > graph.ContractProjectionRowLimit {
				return report, graph.ErrContractProjectionLimit
			}
			report.Files++
			if evidence.err != nil {
				return report, evidence.err
			}
		}
		report.ExtractDuration = time.Since(extractStart)
		enrichStart := time.Now()
		// All postpasses see only detached evidence and proof-bound source reads.
		prefixes := make([]string, 0, len(indexers))
		for repo := range indexers {
			prefixes = append(prefixes, repo)
		}
		sort.Strings(prefixes)
		for _, repo := range prefixes {
			if req.Yield != nil {
				if err := req.Yield(ctx); err != nil {
					return report, err
				}
			}
			idx := indexers[repo]
			local := contracts.NewRegistry()
			local.AddAll(registry.ByRepo(repo), repo)
			idx.extractDIContracts(local)
			idx.upgradeContractBareTypeRefs(local)
			idx.resolveProviderHandlers(local)
			if scans := idx.routerPrefixScanFiles(local); len(scans) > 0 {
				contracts.JoinRouterPrefixes(local, scans, idx.contractFileSrc)
			}
			contracts.BindSpringConfig(evidence, contracts.SpringConfigScope{RepoPrefix: repo, RepoRoot: idx.rootPath, WorkspaceID: idx.workspaceID, ReadSource: func(path string) ([]byte, error) {
				data, err := readSource(path)
				if err != nil {
					idx.rememberContractInputError(err)
				}
				return data, err
			}})
			idx.resolveCallReturnTypes(local)
			idx.snapshotContractShapes(local)
			idx.inlineEnvelopeShapes(local)
			if err := idx.contractInputError(); err != nil {
				return report, err
			}
			if evidence.err != nil {
				return report, evidence.err
			}
			for _, id := range registry.AllIDs() {
				registry.ReplaceByID(id, contractFollowupOtherOwners(registry.ByID(id), repo))
			}
			registry.AddAll(local.All(), repo)

		}
		allRecords := registry.All()
		encodedRecords, encodeErr := json.Marshal(allRecords)
		if encodeErr != nil {
			return report, encodeErr
		}
		if len(encodedRecords) > contractFollowupCompactLimit || len(allRecords) > graph.ContractProjectionRowLimit {
			return report, graph.ErrContractProjectionLimit
		}
		report.EnrichDuration = time.Since(enrichStart)
		report.PrepareDuration = time.Since(prepareStart)
		report.ScratchNodes, report.ScratchEdges = evidence.writtenNodes, evidence.writtenEdges
		report.ScratchBytes = contractFollowupScratchBytes(prepared.scratchDir)
		prepared.registry, prepared.evidence, prepared.report = registry, evidence, report
	} else {
		report.Files = prepared.report.Files
		report.SourceReads = prepared.report.SourceReads
		report.SourceBytes = prepared.report.SourceBytes
		report.ExtractDuration = prepared.report.ExtractDuration
		report.EnrichDuration = prepared.report.EnrichDuration
		report.ScratchNodes = prepared.report.ScratchNodes
		report.ScratchEdges = prepared.report.ScratchEdges
		report.ScratchBytes = prepared.report.ScratchBytes
		report.PrepareDuration = prepared.report.PrepareDuration
	}
	own := registry.ByRepo(snap.Key.RepoPrefix)
	nodes, allOwnerEdges, missing := contractGraphRows(evidence, registry.All(), true)
	var edges []*graph.Edge
	for _, edge := range allOwnerEdges {
		if owner, _ := edge.Meta["contract_owner_repo_prefix"].(string); owner == snap.Key.RepoPrefix {
			edges = append(edges, edge)
		}
	}
	if missing != 0 {
		return report, fmt.Errorf("contract followup: %d missing selected source owners", missing)
	}
	report.Records = len(own)
	matches := contracts.Match(registry).Matched
	groups := make(map[bridgeGroupKey]bool)
	for _, match := range matches {
		if match.Provider.RepoPrefix == snap.Key.RepoPrefix || match.Consumer.RepoPrefix == snap.Key.RepoPrefix {
			groups[contractLinkBridgeGroup(match)] = true
		}
	}
	var relevant []contracts.CrossLink
	for _, match := range matches {
		if groups[contractLinkBridgeGroup(match)] {
			relevant = append(relevant, match)
			if match.Consumer.SymbolID != "" && match.Provider.SymbolID != "" {
				edges = append(edges, contractMatchEdge(match))
			}
		}
	}
	bridgeNodes, bridgeEdges := buildContractBridgeBatch(relevant)
	nodes = append(nodes, bridgeNodes...)
	edges = append(edges, bridgeEdges...)
	ids := make([]string, 0, len(evidence.outputIDs))
	for id := range evidence.outputIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for start := 0; start < len(ids); start += contractFrontierReadBatchSize {
		found, readErr := evidence.scratch.GetNodesByIDsContext(ctx, ids[start:min(start+contractFrontierReadBatchSize, len(ids))])
		if readErr == nil && len(found) != min(contractFrontierReadBatchSize, len(ids)-start) {
			return report, fmt.Errorf("contract followup: analysis output endpoint missing")
		}
		if readErr != nil {
			return report, readErr
		}
		for _, node := range found {
			nodes = append(nodes, node)
			if (node.Kind == graph.KindType || node.Kind == graph.KindInterface) && node.Meta["shape"] != nil {
				report.Shapes++
			}
		}
	}
	for _, edge := range evidence.outputEdges {
		edges = append(edges, edge)
	}
	if evidence.err != nil {
		return report, evidence.err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	encodedRows, encodeErr := json.Marshal(struct {
		Nodes []*graph.Node
		Edges []*graph.Edge
	}{nodes, edges})
	if encodeErr != nil {
		return report, encodeErr
	}
	if len(encodedRows) > contractFollowupCompactLimit || len(nodes)+len(edges) > graph.ContractProjectionRowLimit {
		return report, graph.ErrContractProjectionLimit
	}
	publishStart := time.Now()
	for start := 0; start < len(nodes); start += contractFrontierReadBatchSize {
		if req.Yield != nil {
			if err := req.Yield(ctx); err != nil {
				return report, err
			}
		}
		if err := req.Payload.AddBatchChecked(nodes[start:min(start+contractFrontierReadBatchSize, len(nodes))], nil); err != nil {
			return report, err
		}
	}
	for start := 0; start < len(edges); start += contractFrontierReadBatchSize {
		if req.Yield != nil {
			if err := req.Yield(ctx); err != nil {
				return report, err
			}
		}
		if err := req.Payload.AddBatchChecked(nil, edges[start:min(start+contractFrontierReadBatchSize, len(edges))]); err != nil {
			return report, err
		}
	}
	report.Nodes = len(nodes)
	report.Edges = len(edges)
	ftsStart := time.Now()
	seenDocuments := make(map[string]bool)
	documentRepos := map[string]bool{snap.Key.RepoPrefix: true}
	var documents []graph.SymbolFTSItem
	flushDocuments := func() error {
		if len(documents) == 0 {
			return nil
		}
		if err := req.Yield(ctx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := req.Payload.BatchUpsertSymbolFTS(documents); err != nil {
			return err
		}
		documents = documents[:0]
		return nil
	}
	documentBytes := 0
	for _, node := range nodes {
		if node.Kind != graph.KindContract && node.Kind != graph.KindContractBridge && node.Kind != graph.KindConfigKey {
			continue
		}
		if seenDocuments[node.ID] {
			continue
		}
		seenDocuments[node.ID] = true
		documentRepos[node.RepoPrefix] = true
		tokens := ftsTokensFor(node, "")
		documents = append(documents, graph.SymbolFTSItem{NodeID: node.ID, Tokens: tokens})
		report.SymbolDocuments++
		documentBytes += len(node.ID) + len(tokens)
		if len(documents) >= symbolFTSDirectChunkRows || documentBytes >= symbolFTSDirectChunkBytes {
			if err := flushDocuments(); err != nil {
				return report, err
			}
			documentBytes = 0
		}
	}
	if err := flushDocuments(); err != nil {
		return report, err
	}
	repos := make([]string, 0, len(documentRepos))
	for repo := range documentRepos {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := req.Payload.SetSymbolFTSNormalization(repo, search.FTSNormalizationMode()); err != nil {
			return report, err
		}
	}
	report.FTSDuration = time.Since(ftsStart)
	if err := req.Payload.SetProducerState(store_sqlite.ProducerCompleteness{Producer: "graph.contracts", State: store_sqlite.ProducerStateComplete}); err != nil {
		return report, err
	}
	tokens := make([]string, 0, len(snap.Work))
	for _, w := range snap.Work {
		tokens = append(tokens, w.Token)
	}
	attachment := graph.ContractAttachment{RepoPrefix: snap.Key.RepoPrefix, CheckoutID: snap.Key.CheckoutID, InputVersion: snap.Key.InputVersion, InputFingerprint: snap.Key.InputFingerprint, PayloadGeneration: generation, CompletedTokens: tokens}
	if snap.ValidateAccepted != nil {
		if err := snap.ValidateAccepted(ctx); err != nil {
			return report, err
		}
	}
	if err := req.Catalog.PublishContractAttachmentWithInputsContext(ctx, logical, snap.Inputs, attachment, snap.Work, time.Now().Unix()); err != nil {
		return report, err
	}
	report.PublishDuration = time.Since(publishStart)
	report.Published = true
	return report, nil
}

func contractFollowupOtherOwners(rows []contracts.Contract, repo string) []contracts.Contract {
	var out []contracts.Contract
	for _, row := range rows {
		if row.RepoPrefix != repo {
			out = append(out, row)
		}
	}
	return out
}
func contractFollowupPolicy(idx *Indexer, language string) (string, error) {
	return contractBoundaryPolicy(idx, language, 0)
}

const contractFollowupCompactLimit = 128 << 20

func (p *contractFollowupPrepared) close() error {
	var err error
	if p.scratch != nil {
		err = p.scratch.Close()
	}
	if p.scratchDir != "" {
		if removeErr := os.RemoveAll(p.scratchDir); err == nil {
			err = removeErr
		}
	}
	return err
}
func contractFollowupScratchBytes(dir string) int64 {
	var total int64
	for _, name := range []string{"evidence.sqlite", "evidence.sqlite-wal", "evidence.sqlite-shm"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Name imports use a synchronous, read-only selected visitor and private,
// single-worker evidence; they do not support reentrant scratch mutations.
// A page preserves the first
// missing ID and existing scratch precedence, while avoiding one commit per row.
// Completion markers are installed only after every page and live lookup succeed.
type contractNameImportPage struct {
	evidence     *contractFollowupEvidence
	nodes        []*graph.Node
	ids          map[string]bool
	encodedBytes int
	byteLimit    int
}

const contractNameImportEnvelopeBytes = len(`{"Nodes":[],"Edges":null}`)

func (p *contractNameImportPage) flush() bool {
	if len(p.nodes) == 0 {
		return p.evidence.err == nil
	}
	p.evidence.AddBatch(p.nodes, nil)
	if p.evidence.err != nil {
		return false
	}
	p.nodes = nil
	p.ids = nil
	p.encodedBytes = 0
	return true
}

func (p *contractNameImportPage) add(node *graph.Node) bool {
	if p.ids[node.ID] {
		return true
	}
	encoded, err := json.Marshal(node)
	if err != nil {
		p.evidence.fail(err)
		return false
	}
	if len(encoded) > p.byteLimit-contractNameImportEnvelopeBytes {
		p.evidence.fail(graph.ErrContractProjectionLimit)
		return false
	}
	additional := len(encoded)
	if len(p.nodes) != 0 {
		additional++ // JSON array separator.
	}
	if contractNameImportEnvelopeBytes+p.encodedBytes+additional > p.byteLimit {
		if !p.flush() {
			return false
		}
		additional = len(encoded)
	}
	if p.ids == nil {
		p.ids = make(map[string]bool)
	}
	p.ids[node.ID] = true
	p.nodes = append(p.nodes, node)
	p.encodedBytes += additional
	return len(p.nodes) < contractFrontierReadBatchSize || p.flush()
}

func (e *contractFollowupEvidence) AddNode(node *graph.Node) { e.AddBatch([]*graph.Node{node}, nil) }
func (e *contractFollowupEvidence) AddEdge(edge *graph.Edge) { e.AddBatch(nil, []*graph.Edge{edge}) }
func (e *contractFollowupEvidence) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if e.err != nil {
		return
	}
	if err := e.ctx.Err(); err != nil {
		e.fail(err)
		return
	}
	encodedRows, encodeErr := json.Marshal(struct {
		Nodes []*graph.Node
		Edges []*graph.Edge
	}{nodes, edges})
	if encodeErr != nil {
		e.fail(encodeErr)
		return
	}
	if len(encodedRows) > contractFollowupCompactLimit {
		e.fail(graph.ErrContractProjectionLimit)
		return
	}
	if err := e.scratch.AddBatchChecked(nodes, edges); err != nil {
		e.fail(err)
		return
	}
	e.writtenNodes += len(nodes)
	e.writtenEdges += len(edges)
	if e.outputIDs == nil {
		e.outputIDs = make(map[string]bool)
		e.outputEdges = make(map[string]*graph.Edge)
		e.diEdges = make(map[string]*graph.Edge)
		e.springIDs = make(map[string]bool)
		e.javaMethodIDs = make(map[string]bool)
	}
	for _, node := range nodes {
		if node != nil {
			if _, ok := node.Meta["spring_config_keys"]; ok {
				e.springIDs[node.ID] = true
			}
			if node.Kind == graph.KindMethod && node.Language == "java" {
				e.javaMethodIDs[node.ID] = true
			}
		}
	}
	for _, node := range nodes {
		if node != nil && (node.Kind == graph.KindConfigKey || ((node.Kind == graph.KindType || node.Kind == graph.KindInterface) && node.Meta["shape"] != nil)) {
			e.outputIDs[node.ID] = true
		}
	}
	for _, edge := range edges {
		if edge != nil && (edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes) {
			if _, di := diContractFromEdge(edge); di {
				encoded, encodeErr := json.Marshal(edge)
				if encodeErr != nil {
					e.fail(encodeErr)
					return
				}
				e.diEdges[string(encoded)] = edge
			}
		}
	}
	for _, edge := range edges {
		if edge != nil && (edge.Kind == graph.EdgeReadsConfig || (edge.Kind == graph.EdgeCalls && edge.Meta["via"] == "spring.Bean")) {
			e.outputIDs[edge.From] = true
			e.outputIDs[edge.To] = true
			encoded, encodeErr := json.Marshal(edge)
			if encodeErr != nil {
				e.fail(encodeErr)
				return
			}
			e.outputEdges[string(encoded)] = edge
		}
	}
	if len(e.outputIDs)+len(e.outputEdges)+len(e.diEdges)+len(e.springIDs)+len(e.javaMethodIDs) > graph.ContractProjectionRowLimit {
		e.fail(graph.ErrContractProjectionLimit)
	}
}

// Legacy enrichment gets a private graph.Store facade. Every missing core
// lookup is checked and sticky; writes target the private scratch store.
type contractFollowupEvidence struct {
	graph.Store
	scratch                    *store_sqlite.Store
	writtenNodes, writtenEdges int
	outputIDs                  map[string]bool
	outputEdges                map[string]*graph.Edge
	diEdges                    map[string]*graph.Edge
	springIDs                  map[string]bool
	javaMethodIDs              map[string]bool
	ctx                        context.Context
	core                       graph.Reader
	files                      map[string]ContractFollowupFile
	readCore                   func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error)
	allowedRepos               map[string]bool
	loaded                     map[string]bool
	// Completed names belong to this private, single-worker captured evidence.
	// Keep raw row counts so cached names still consume the request's budget;
	// scratch results and final accepted-source fences remain live.
	completedCoreNames     map[string]int
	completedCoreNameBytes int
	err                    error
}

// RepoFilePaths serves the complete accepted census directly; no errorless
// repository-wide scratch query is needed for Spring configuration discovery.
func (e *contractFollowupEvidence) RepoFilePaths(repo, workspace string, languages, extensions []string) []string {
	var out []string
	for path, file := range e.files {
		if file.RepoPrefix != repo || (workspace != "" && file.WorkspaceID != workspace) {
			continue
		}
		match := false
		for _, language := range languages {
			match = match || file.Language == language
		}
		for _, extension := range extensions {
			match = match || strings.HasSuffix(path, extension)
		}
		if match {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}
func (e *contractFollowupEvidence) selectedScratchNodes(ids map[string]bool, repo, workspace string, kinds []graph.NodeKind) []*graph.Node {
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	wanted := make(map[graph.NodeKind]bool)
	for _, kind := range kinds {
		wanted[kind] = true
	}
	var out []*graph.Node
	for start := 0; start < len(keys); start += contractFrontierReadBatchSize {
		found, err := e.scratch.GetNodesByIDsContext(e.ctx, keys[start:min(start+contractFrontierReadBatchSize, len(keys))])
		if err != nil {
			e.fail(err)
			return nil
		}
		for _, node := range found {
			if node.RepoPrefix == repo && (workspace == "" || node.WorkspaceID == workspace) && wanted[node.Kind] {
				out = append(out, node)
			}
		}
	}
	return out
}
func (e *contractFollowupEvidence) RepoNodesByKindsWithMetaKey(repo, workspace string, kinds []graph.NodeKind, key string) []*graph.Node {
	if key != "spring_config_keys" {
		e.fail(fmt.Errorf("contract followup: unsupported private metadata projection %s", key))
		return nil
	}
	return e.selectedScratchNodes(e.springIDs, repo, workspace, kinds)
}

// The worker's sole repo-language enrichment caller needs Java method inputs.
// Keep that admitted compact projection instead of scanning all Java payload.
func (e *contractFollowupEvidence) GetRepoNodesByLanguage(repo, language string) []*graph.Node {
	if language != "java" {
		e.fail(fmt.Errorf("contract followup: unsupported private language projection %s", language))
		return nil
	}
	return e.selectedScratchNodes(e.javaMethodIDs, repo, "", []graph.NodeKind{graph.KindMethod})
}

// RepoEdgesByKinds serves only the admitted DI seeds required by enrichment.
// It never invokes an errorless scratch SQL scan or loads all ordinary edges.
func (e *contractFollowupEvidence) RepoEdgesByKinds(repos []string, kinds []graph.EdgeKind) []graph.RepoEdgeRow {
	wantedRepos := make(map[string]bool)
	for _, repo := range repos {
		wantedRepos[repo] = true
	}
	wantedKinds := make(map[graph.EdgeKind]bool)
	for _, kind := range kinds {
		wantedKinds[kind] = true
	}
	var rows []graph.RepoEdgeRow
	for _, edge := range e.diEdges {
		if !wantedKinds[edge.Kind] {
			continue
		}
		found, err := e.scratch.GetNodesByIDsContext(e.ctx, []string{edge.From})
		if err != nil {
			e.fail(err)
			return nil
		}
		source := found[edge.From]
		if source == nil {
			e.fail(fmt.Errorf("contract followup: missing DI source %s", edge.From))
			return nil
		}
		if wantedRepos[source.RepoPrefix] {
			rows = append(rows, graph.RepoEdgeRow{Edge: edge, RepoPrefix: source.RepoPrefix})
		}
	}
	return rows
}
func (e *contractFollowupEvidence) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	rows, truncated, err := e.scratch.GetOutEdgesByNodeIDsWithMetadataContext(e.ctx, ids, graph.ContractProjectionRowLimit)
	if err != nil {
		e.fail(err)
		return nil
	}
	if truncated {
		e.fail(graph.ErrContractProjectionLimit)
		return nil
	}
	return rows
}
func (e *contractFollowupEvidence) GetOutEdges(id string) []*graph.Edge {
	return e.GetOutEdgesByNodeIDs([]string{id})[id]
}

func (e *contractFollowupEvidence) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}
func (e *contractFollowupEvidence) detach(node *graph.Node) *graph.Node {
	if node == nil || node.ID == "" {
		e.fail(fmt.Errorf("contract followup: malformed selected node"))
		return nil
	}
	if !e.allowedRepos[node.RepoPrefix] {
		e.fail(fmt.Errorf("contract followup: unwitnessed selected namespace %s", node.RepoPrefix))
		return nil
	}
	copyNode := *node
	if node.Meta != nil {
		copyNode.Meta = contractFollowupCloneValue(reflect.ValueOf(node.Meta)).Interface().(map[string]any)
	}
	if copyNode.Kind == graph.KindType || copyNode.Kind == graph.KindInterface {
		delete(copyNode.Meta, "shape")
	}
	return &copyNode
}
func contractFollowupCloneValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(contractFollowupCloneValue(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), contractFollowupCloneValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(contractFollowupCloneValue(v.Index(i)))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(contractFollowupCloneValue(v.Elem()))
		return out
	default:
		return v
	}
}
func (e *contractFollowupEvidence) loadFile(path string) ([]*graph.Node, []*graph.Edge, error) {
	if e.loaded == nil {
		e.loaded = make(map[string]bool)
	}
	if e.loaded[path] {
		file := e.files[path]
		p, readErr := e.scratch.LayerContractFileProjectionContext(e.ctx, file.RepoPrefix, []string{path})
		if readErr != nil {
			e.fail(readErr)
			return nil, nil, readErr
		}
		nodes := p.FileNodes[path]
		var ids []string
		for _, node := range nodes {
			ids = append(ids, node.ID)
		}
		var edges []*graph.Edge
		bySource, truncated, readErr := e.scratch.GetOutEdgesByNodeIDsWithMetadataContext(e.ctx, ids, graph.ContractProjectionRowLimit)
		if readErr != nil {
			e.fail(readErr)
			return nil, nil, readErr
		}
		if truncated {
			e.fail(graph.ErrContractProjectionLimit)
			return nil, nil, e.err
		}
		for _, rows := range bySource {
			edges = append(edges, rows...)
		}
		return nodes, edges, e.err
	}
	file, ok := e.files[path]
	if !ok {
		return nil, nil, fmt.Errorf("contract followup: selected file outside accepted census %q", path)
	}
	rows, err := e.readCore(e.ctx, file)
	if err != nil {
		e.fail(err)
		return nil, nil, err
	}
	if len(rows.Nodes)+len(rows.Edges) > graph.ContractProjectionRowLimit {
		e.fail(graph.ErrContractProjectionLimit)
		return nil, nil, e.err
	}
	var nodes []*graph.Node
	fileFound := false
	for _, node := range rows.Nodes {
		if node == nil || node.FilePath != path || node.RepoPrefix != file.RepoPrefix {
			e.fail(fmt.Errorf("contract followup: selected file identity mismatch %q", path))
			return nil, nil, e.err
		}
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge || node.Kind == graph.KindConfigKey {
			continue
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return nil, nil, e.err
		}
		nodes = append(nodes, copyNode)
		if node.Kind == graph.KindFile || (node.Kind == graph.KindFixture && node.ID == path && fixtures.IsFixturePath(path) && node.Meta["fixture"] == true) {
			fileFound = true
		}
	}
	if !fileFound {
		e.fail(fmt.Errorf("contract followup: accepted census file has no selected core owner %q", path))
		return nil, nil, e.err
	}
	var edges []*graph.Edge
	for _, edge := range rows.Edges {
		if edge == nil || edge.From == "" || edge.To == "" {
			e.fail(fmt.Errorf("contract followup: malformed selected edge"))
			return nil, nil, e.err
		}
		// Existing contract output must never seed an isolated new analysis.
		if edge.Kind == graph.EdgeHandlesRoute || edge.Kind == graph.EdgeMatches || edge.Kind == graph.EdgeBridges || edge.Kind == graph.EdgeReadsConfig {
			continue
		}
		if edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes {
			if _, di := diContractFromEdge(edge); !di {
				continue
			}
		}
		copyEdge := *edge
		if edge.Meta != nil {
			copyEdge.Meta = contractFollowupCloneValue(reflect.ValueOf(edge.Meta)).Interface().(map[string]any)
		}
		edges = append(edges, &copyEdge)
	}
	e.AddBatch(nodes, edges)
	e.loaded[path] = true
	return nodes, edges, e.err
}
func (e *contractFollowupEvidence) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node, len(paths))
	for _, path := range paths {
		nodes, _, err := e.loadFile(path)
		if err != nil {
			e.fail(err)
			return nil
		}
		out[path] = nodes
	}
	return out
}
func (e *contractFollowupEvidence) GetNodesByIDs(ids []string) map[string]*graph.Node {
	out, readErr := e.scratch.GetNodesByIDsContext(e.ctx, ids)
	if readErr != nil {
		e.fail(readErr)
		return nil
	}
	var missing []string
	for _, id := range ids {
		if out[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return out
	}
	found, err := graph.ContractSourceNodesContext(e.ctx, e.core, missing)
	if err != nil {
		e.fail(err)
		return nil
	}
	for _, node := range found {
		if node == nil {
			e.fail(fmt.Errorf("contract followup: nil selected source"))
			return nil
		}
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge {
			e.fail(fmt.Errorf("contract followup: attempted to import core contract output"))
			return nil
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return nil
		}
		e.AddNode(copyNode)
		out[copyNode.ID] = copyNode
	}
	return out
}
func (e *contractFollowupEvidence) GetNode(id string) *graph.Node {
	return e.GetNodesByIDs([]string{id})[id]
}
func (e *contractFollowupEvidence) FindNodesByNames(names []string) map[string][]*graph.Node {
	if e.err != nil {
		return nil
	}
	if e.ctx != nil && e.ctx.Err() != nil {
		e.fail(e.ctx.Err())
		return nil
	}
	count := 0
	page := contractNameImportPage{evidence: e, byteLimit: contractFollowupCompactLimit}
	pending := names
	newCounts := make(map[string]int, len(names))
	cacheable := e.core != nil
	for _, name := range names {
		if _, duplicate := newCounts[name]; duplicate {
			// Optional fallback readers may visit duplicate names repeatedly.
			// Preserve that exact legacy visit and its row budget, without memo.
			cacheable = false
		}
		newCounts[name] = 0
	}
	if cacheable {
		pending = make([]string, 0, len(names))
		for _, name := range names {
			if rows, done := e.completedCoreNames[name]; done {
				count += rows
				delete(newCounts, name)
				if count > graph.ContractProjectionRowLimit {
					e.fail(graph.ErrContractProjectionLimit)
					return nil
				}
			} else {
				pending = append(pending, name)
			}
		}
	}
	err := graph.VisitNodesByNamesContext(e.ctx, e.core, pending, func(node *graph.Node) bool {
		count++
		if count > graph.ContractProjectionRowLimit {
			e.fail(graph.ErrContractProjectionLimit)
			return false
		}
		if node == nil {
			cacheable = false
			return true
		}
		if _, exact := newCounts[node.Name]; exact {
			newCounts[node.Name]++
		} else {
			cacheable = false
		}
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge {
			return true
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return false
		}
		prior, readErr := e.scratch.GetNodesByIDsContext(e.ctx, []string{copyNode.ID})
		if readErr != nil {
			e.fail(readErr)
			return false
		}
		if prior[copyNode.ID] == nil {
			return page.add(copyNode)
		}
		return true
	})
	if err != nil {
		e.fail(err)
	}
	if e.err != nil || !page.flush() {
		return nil
	}
	out := make(map[string][]*graph.Node)
	err = graph.VisitNodesByNamesContext(e.ctx, e.scratch, names, func(node *graph.Node) bool { out[node.Name] = append(out[node.Name], node); return true })
	if err != nil {
		e.fail(err)
		return nil
	}
	if cacheable {
		if e.completedCoreNames == nil {
			e.completedCoreNames = make(map[string]int)
		}
		for _, name := range pending {
			if len(e.completedCoreNames) >= graph.ContractProjectionRowLimit || len(name) > contractCoreReceiptPayloadLimit-e.completedCoreNameBytes {
				continue
			}
			e.completedCoreNames[name] = newCounts[name]
			e.completedCoreNameBytes += len(name)
		}
	}
	return out
}
func (e *contractFollowupEvidence) FindNodesByName(name string) []*graph.Node {
	return e.FindNodesByNames([]string{name})[name]
}
func (e *contractFollowupEvidence) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	out, err := graph.ConstantValuesByNodeIDsContext(e.ctx, e.core, ids)
	if err != nil {
		e.fail(err)
	}
	return out, err
}
