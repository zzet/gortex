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

	started := time.Now()
	written := 0
	for _, idx := range indexers {
		n, err := idx.rebuildSymbolFTS(reporter)
		if err != nil {
			return err
		}
		written += n
	}
	if mi.logger != nil {
		mi.logger.Info("indexer: symbol FTS rebuilt from resident state",
			zap.Int("repos", len(indexers)),
			zap.Int("fts_items", written),
			zap.Duration("elapsed", time.Since(started)))
	}
	return nil
}

// rebuildSymbolFTS repopulates one repo's FTS corpus from resident nodes. It
// mirrors populateSymbolFTS's chunking (same row/byte bounds, same admission,
// same token derivation). Streaming goes over the store's own AllNodes
// surface with the set-oriented scope predicate (exact RepoPrefix equality —
// the same matching ReadRepoNodeIDsByKinds' fallback applies) instead of
// graph.NodesInScopeSeq: that adapter requires an explicit kind list (empty
// kinds yield an empty sequence by design) and the FTS corpus must not drift
// whenever a new NodeKind is added — admission stays in
// shouldIndexForSearch, which already encodes which kinds are searchable.
func (idx *Indexer) rebuildSymbolFTS(reporter progress.Reporter) (int, error) {
	batcher, ok := idx.graph.(graph.SymbolFTSBatchUpserter)
	if !ok {
		return 0, nil
	}
	repoPrefix := idx.RepoPrefix()
	if reporter != nil {
		reporter.Report("building symbol fts", 0, 0)
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
	for _, node := range idx.graph.AllNodes() {
		if node == nil || node.RepoPrefix != repoPrefix {
			continue
		}
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
	// Finalize the engine-side index (FTS5 'optimize' on the sqlite engine —
	// the same finalize populateSymbolFTS runs).
	if searcher, ok := idx.graph.(graph.SymbolSearcher); ok {
		if err := searcher.BuildSymbolIndex(); err != nil {
			return written, fmt.Errorf("indexer: finalize backend FTS: %w", err)
		}
	}
	if reporter != nil {
		reporter.Report("building symbol fts", 1, 1)
	}
	return written, nil
}
