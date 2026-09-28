package indexer

import (
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

const (
	refFactFileBatch = 32
	refFactNodeBatch = 256
	refFactEdgeBatch = 512
)

// Reference-facts sidecar persistence. After resolution settles, each resolved
// reference edge becomes a durable, auditable fact (from → to + provenance
// tier) persisted per source file in the backend's ref_facts table. Only the
// on-disk store implements graph.RefFactsWriter; the in-memory backend has no
// durable layer to seed, so persistence is a no-op there (the live edges ARE
// the facts).

// refFactsWriter returns the backend's reference-facts persistence capability
// if it implements one.
func (idx *Indexer) refFactsWriter() (graph.RefFactsWriter, bool) {
	w, ok := idx.graph.(graph.RefFactsWriter)
	return w, ok
}

// walkRefFacts derives resolved-reference facts from bounded node/edge/target
// batches and yields at most refFactEdgeBatch rows at a time. It never issues
// per-node adjacency or per-edge target lookups.
func walkRefFacts(g graph.Store, nodes []*graph.Node, yield func([]graph.RefFact) error) error {
	facts := make([]graph.RefFact, 0, refFactEdgeBatch)
	emit := func(fact graph.RefFact) error {
		facts = append(facts, fact)
		if len(facts) < refFactEdgeBatch {
			return nil
		}
		if err := yield(facts); err != nil {
			return err
		}
		facts = facts[:0]
		return nil
	}
	for nodeStart := 0; nodeStart < len(nodes); nodeStart += refFactNodeBatch {
		nodeEnd := min(nodeStart+refFactNodeBatch, len(nodes))
		nodeByID := make(map[string]*graph.Node, nodeEnd-nodeStart)
		ids := make([]string, 0, nodeEnd-nodeStart)
		for _, node := range nodes[nodeStart:nodeEnd] {
			if node == nil || node.ID == "" {
				continue
			}
			if _, exists := nodeByID[node.ID]; exists {
				continue
			}
			nodeByID[node.ID] = node
			ids = append(ids, node.ID)
		}
		if len(ids) == 0 {
			continue
		}
		out := g.GetOutEdgesByNodeIDs(ids)
		selected := make([]*graph.Edge, 0, refFactEdgeBatch)
		flush := func() error {
			if len(selected) == 0 {
				return nil
			}
			targetIDs := make([]string, 0, len(selected))
			for _, edge := range selected {
				targetIDs = append(targetIDs, edge.To)
			}
			targets := g.GetNodesByIDs(targetIDs)
			for _, edge := range selected {
				source := nodeByID[edge.From]
				if source == nil {
					continue
				}
				refName := ""
				if target := targets[edge.To]; target != nil {
					refName = target.Name
				}
				origin := edge.Origin
				if origin == "" {
					semanticSource, _ := edge.Meta["semantic_source"].(string)
					origin = graph.DefaultOriginFor(edge.Kind, edge.Confidence, semanticSource)
				}
				if err := emit(graph.RefFact{
					RepoPrefix: source.RepoPrefix,
					FromID:     edge.From, ToID: edge.To, Kind: string(edge.Kind),
					RefName: refName, Line: edge.Line, Origin: origin,
					Tier: graph.ResolvedBy(origin), FilePath: source.FilePath,
					Lang: source.Language,
				}); err != nil {
					return err
				}
			}
			selected = selected[:0]
			return nil
		}
		for _, id := range ids {
			for _, edge := range out[id] {
				if edge == nil || !graph.IsResolvableRefEdge(edge.Kind) || edge.To == "" ||
					graph.IsUnresolvedTarget(edge.To) || graph.IsStub(edge.To) {
					continue
				}
				selected = append(selected, edge)
				if len(selected) == refFactEdgeBatch {
					if err := flush(); err != nil {
						return err
					}
				}
			}
		}
		if err := flush(); err != nil {
			return err
		}
	}
	if len(facts) > 0 {
		return yield(facts)
	}
	return nil
}

// persistRefFactsForFiles re-derives and persists the resolved-reference facts
// for the given graph file paths (delete-then-set per file so stale facts from
// removed references don't linger). Every requested file is deleted even when
// it yields no fresh facts — a file whose last resolvable reference just
// degraded to an unresolved stub must drop its stale rows, not keep them
// because there is nothing new to write. No-op when the backend has no
// durable layer or the file list is empty.
func (idx *Indexer) persistRefFactsForFiles(graphPaths []string) {
	graphPaths = uniqueSortedRefFactPaths(graphPaths)
	if len(graphPaths) == 0 {
		return
	}
	if rebuilder, ok := idx.graph.(graph.RefFactsRebuilder); ok {
		if err := rebuilder.ReplaceRefFactsForFiles(idx.repoPrefix, graphPaths); err != nil {
			idx.logger.Debug("ref-facts: set-oriented replace failed", zap.Error(err))
		}
		return
	}
	w, ok := idx.refFactsWriter()
	if !ok {
		return
	}
	for start := 0; start < len(graphPaths); start += refFactFileBatch {
		end := min(start+refFactFileBatch, len(graphPaths))
		paths := graphPaths[start:end]
		byPath := idx.graph.GetFileNodesByPaths(paths)
		nodesByRepo := make(map[string][]*graph.Node)
		filesByRepo := make(map[string]map[string]struct{})
		for _, path := range paths {
			nodes := byPath[path]
			seenRepos := make(map[string]struct{})
			for _, node := range nodes {
				if node == nil {
					continue
				}
				repo := node.RepoPrefix
				nodesByRepo[repo] = append(nodesByRepo[repo], node)
				seenRepos[repo] = struct{}{}
			}
			if len(seenRepos) == 0 {
				seenRepos[idx.repoPrefix] = struct{}{}
			}
			for repo := range seenRepos {
				if filesByRepo[repo] == nil {
					filesByRepo[repo] = make(map[string]struct{})
				}
				filesByRepo[repo][path] = struct{}{}
			}
		}
		for repo, fileSet := range filesByRepo {
			files := make([]string, 0, len(fileSet))
			for file := range fileSet {
				files = append(files, file)
			}
			sort.Strings(files)
			if err := w.DeleteRefFactsByFiles(repo, files); err != nil {
				idx.logger.Debug("ref-facts: delete failed", zap.Error(err))
				continue
			}
			if err := walkRefFacts(idx.graph, nodesByRepo[repo], func(facts []graph.RefFact) error {
				return w.BulkSetRefFacts(repo, facts)
			}); err != nil {
				idx.logger.Debug("ref-facts: persist failed", zap.Error(err))
			}
		}
	}
}

// persistAllRefFacts persists reference facts for every indexed file. Called
// once after a full resolve so a cold index seeds the durable sidecar.
func (idx *Indexer) persistAllRefFacts() {
	if rebuilder, ok := idx.graph.(graph.RefFactsRebuilder); ok {
		var repos []string
		if idx.repoPrefix != "" {
			repos = []string{idx.repoPrefix}
		}
		if err := rebuilder.RebuildRefFactsForRepos(repos); err != nil {
			idx.logger.Debug("ref-facts: set-oriented rebuild failed", zap.Error(err))
		}
		return
	}
	if _, ok := idx.refFactsWriter(); !ok {
		return
	}
	files := make([]string, 0, refFactFileBatch)
	for n := range idx.graph.NodesByKind(graph.KindFile) {
		if n != nil {
			files = append(files, n.ID)
			if len(files) == refFactFileBatch {
				idx.persistRefFactsForFiles(files)
				files = files[:0]
			}
		}
	}
	if len(files) > 0 {
		idx.persistRefFactsForFiles(files)
	}
}

func uniqueSortedRefFactPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// deleteRefFactsForFiles drops persisted facts sourced in the given graph file
// paths (used when a file is evicted/deleted). No-op without a durable backend.
func (idx *Indexer) deleteRefFactsForFiles(repoPrefix string, graphPaths []string) {
	w, ok := idx.refFactsWriter()
	if !ok || len(graphPaths) == 0 {
		return
	}
	if err := w.DeleteRefFactsByFiles(repoPrefix, graphPaths); err != nil {
		idx.logger.Debug("ref-facts: delete-on-evict failed", zap.Error(err))
	}
}

// refFactLegs times the legs of one ref-facts persistence (the files' nodes,
// their out-edges, the targets' names, the delete and the write), each with
// the major page faults it took. A nil *refFactLegs records nothing.
type refFactLegs struct {
	last   time.Time
	io     editDeltaIO
	took   map[string]time.Duration
	faults map[string]int64
	order  []string
}

func newRefFactLegs() *refFactLegs {
	return &refFactLegs{last: time.Now(), io: editDeltaProcessIO(),
		took: make(map[string]time.Duration), faults: make(map[string]int64)}
}

func (l *refFactLegs) lap(name string) {
	if l == nil {
		return
	}
	now, io := time.Now(), editDeltaProcessIO()
	if _, seen := l.took[name]; !seen {
		l.order = append(l.order, name)
	}
	l.took[name] += now.Sub(l.last)
	l.faults[name] += io.since(l.io).majorFaults
	l.last, l.io = now, io
}

func (l *refFactLegs) fields(files int) []zap.Field {
	fields := []zap.Field{zap.Int("files", files)}
	for _, name := range l.order {
		fields = append(fields, zap.Duration(name, l.took[name]), zap.Int64(name+"_faults", l.faults[name]))
	}
	return fields
}
