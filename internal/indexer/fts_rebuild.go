package indexer

import (
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/progress"
)

// RebuildSymbolFTS repopulates the symbol FTS corpus for every tracked repo
// from the store's resident state (issue davalillo/gortex#21).
//
// The memory backend's FTS engine is derived state: it is never journaled and
// never persisted, so every memory-backend boot — cold or warm — starts with
// an empty corpus. populateSymbolFTS cannot serve it: that path requires the
// store to implement the streaming ScopedProjectionSequencer, which the
// memory backend does not offer (*Graph provides only the set-oriented scope
// adapter), so populateSymbolFTS silently no-ops there. This rebuild walks
// the SAME admission (shouldIndexForSearch) and tokenization (ftsTokensFor)
// through the set-oriented scope adapter instead, and fills the store through
// its FTS batch-upsert capability, so the corpus is identical whichever path
// produced it. On the sqlite backend the daemon never calls this — FTS is
// persisted store state the indexer's own paths maintain.
//
// The walk is ONE AllNodes pass per distinct backing graph (issue
// davalillo/gortex#25, R4-fts-rebuild-full-rescan-per-repo): every repo used
// to re-walk the whole store and filter by its own prefix — O(repos × nodes),
// the dominant warm-boot cost on the memory backend (~9 s at 5 repos /
// 226k nodes). The single pass partitions nodes by prefix and feeds each
// indexer exactly the bucket its per-repo walk would have selected, in the
// same order, so the corpus and chunk boundaries are unchanged.
func (mi *MultiIndexer) RebuildSymbolFTS(reporter progress.Reporter) error {
	if mi == nil {
		return nil
	}
	mi.mu.Lock()
	seen := make(map[*Indexer]struct{}, len(mi.indexers))
	indexers := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		if idx == nil {
			continue
		}
		if _, dup := seen[idx]; dup {
			continue
		}
		seen[idx] = struct{}{}
		indexers = append(indexers, idx)
	}
	mi.mu.Unlock()

	// Group the indexers by backing graph so each distinct store is walked
	// exactly once. In the daemon every indexer shares one store; the
	// grouping only keeps the single-repo / test shapes honest.
	byGraph := make(map[graph.Store][]*Indexer, 1)
	graphOrder := make([]graph.Store, 0, 1)
	for _, idx := range indexers {
		if _, ok := byGraph[idx.graph]; !ok {
			graphOrder = append(graphOrder, idx.graph)
		}
		byGraph[idx.graph] = append(byGraph[idx.graph], idx)
	}

	started := time.Now()
	written := 0
	if reporter != nil {
		reporter.Report("building symbol fts", 0, 0)
	}
	for _, g := range graphOrder {
		buckets := partitionNodesByPrefix(g.AllNodes())
		for _, idx := range byGraph[g] {
			n, err := idx.rebuildSymbolFTSBucket(buckets[idx.RepoPrefix()], reporter)
			if err != nil {
				return err
			}
			written += n
		}
		// Finalize the engine-side index once per graph (FTS5 'optimize' on
		// the sqlite engine — the same finalize populateSymbolFTS runs). The
		// per-repo walk finalized after every repo; one finalize per graph
		// merges the same segments at a fraction of the calls.
		if searcher, ok := g.(graph.SymbolSearcher); ok {
			if err := searcher.BuildSymbolIndex(); err != nil {
				return fmt.Errorf("indexer: finalize backend FTS: %w", err)
			}
		}
	}
	if reporter != nil {
		reporter.Report("building symbol fts", 1, 1)
	}
	if mi.logger != nil {
		mi.logger.Info("indexer: symbol FTS rebuilt from resident state",
			zap.Int("repos", len(indexers)),
			zap.Int("fts_items", written),
			zap.Duration("elapsed", time.Since(started)))
	}
	return nil
}

// partitionNodesByPrefix groups one AllNodes stream into per-repo buckets,
// dropping nil nodes and preserving the walk order inside each bucket (the
// chunking order the per-repo walk produced). The "" bucket holds the
// deliberate unowned globals (dep:: externals); no indexer owns that prefix,
// so the bucket is simply never consumed — same as the old per-repo filter.
func partitionNodesByPrefix(nodes []*graph.Node) map[string][]*graph.Node {
	buckets := make(map[string][]*graph.Node)
	for _, node := range nodes {
		if node == nil {
			continue
		}
		buckets[node.RepoPrefix] = append(buckets[node.RepoPrefix], node)
	}
	return buckets
}

// rebuildSymbolFTS repopulates one repo's FTS corpus from resident nodes. It
// mirrors populateSymbolFTS's chunking (same row/byte bounds, same admission,
// same token derivation). The node stream comes from the store's own AllNodes
// surface with the set-oriented scope predicate (exact RepoPrefix equality —
// the same matching ReadRepoNodeIDsByKinds' fallback applies) instead of
// graph.NodesInScopeSeq: that adapter requires an explicit kind list (empty
// kinds yield an empty sequence by design) and the FTS corpus must not drift
// whenever a new NodeKind is added — admission stays in
// shouldIndexForSearch, which already encodes which kinds are searchable.
// Single-repo shape; the multi-repo daemon path goes through
// MultiIndexer.RebuildSymbolFTS's one-pass partition instead.
func (idx *Indexer) rebuildSymbolFTS(reporter progress.Reporter) (int, error) {
	repoPrefix := idx.RepoPrefix()
	var matching []*graph.Node
	for _, node := range idx.graph.AllNodes() {
		if node == nil || node.RepoPrefix != repoPrefix {
			continue
		}
		matching = append(matching, node)
	}
	written, err := idx.rebuildSymbolFTSBucket(matching, reporter)
	if err != nil {
		return written, err
	}
	// Finalize the engine-side index (FTS5 'optimize' on the sqlite engine —
	// the same finalize populateSymbolFTS runs).
	if searcher, ok := idx.graph.(graph.SymbolSearcher); ok {
		if err := searcher.BuildSymbolIndex(); err != nil {
			return written, fmt.Errorf("indexer: finalize backend FTS: %w", err)
		}
	}
	return written, nil
}

// rebuildSymbolFTSBucket admits, tokenizes and chunk-upserts one repo's
// already-partitioned node bucket. It is the admission/chunking core shared
// by the single-repo walk and the multi-repo one-pass partition — the corpus
// cannot drift between the two shapes because they run the same code.
func (idx *Indexer) rebuildSymbolFTSBucket(nodes []*graph.Node, reporter progress.Reporter) (int, error) {
	batcher, ok := idx.graph.(graph.SymbolFTSBatchUpserter)
	if !ok {
		return 0, nil
	}

	items := make([]graph.SymbolFTSItem, 0, symbolFTSDirectChunkRows)
	var pending uint64
	written := 0
	flush := func() error {
		if len(items) == 0 {
			return nil
		}
		if err := batcher.BatchUpsertSymbolFTS(items); err != nil {
			return err
		}
		written += len(items)
		items = make([]graph.SymbolFTSItem, 0, symbolFTSDirectChunkRows)
		pending = 0
		return nil
	}
	for _, node := range nodes {
		if !idx.shouldIndexForSearch(node) {
			continue
		}
		tokens := ftsTokensFor(node, idx.projectName)
		items = append(items, graph.SymbolFTSItem{NodeID: node.ID, Tokens: tokens})
		pending += uint64(len(node.ID) + len(tokens) + 32)
		if len(items) >= symbolFTSDirectChunkRows || pending >= symbolFTSDirectChunkBytes {
			if err := flush(); err != nil {
				return written, fmt.Errorf("indexer: rebuild symbol fts: %w", err)
			}
		}
	}
	if err := flush(); err != nil {
		return written, fmt.Errorf("indexer: rebuild symbol fts: %w", err)
	}
	return written, nil
}
