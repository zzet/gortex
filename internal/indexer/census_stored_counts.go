package indexer

import (
	"context"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The startup census's counts, reused.
//
// A clean census (cleanCensusResult) runs only after ChangedSinceMtimes has
// proved the tree unchanged since the last index, and it reported the
// repository's node and edge counts by counting them: an exact COUNT(*) per
// repository (repoNodeEdgeCount → RepoMemoryEstimate). On the clone that was
// 52.6 s of reads on one start and about 600 s on another (one read 170.5 s),
// paid again on every start of the same store. The counts the last index
// stored in repo_index_state describe the same rows, so the clean path takes
// them from there and counts only when nothing is stored.
//
// The state is read at the generation the indexer writes (an output
// generation, or generation zero), and, when that generation holds no state —
// an output generation that is a clean copy of its parent — at its parent.
//
// The other count sites keep counting: the full index, the HEAD-move
// reconcile, the file deletion path and the full-root pass all change rows.

// cleanCensusCounts counts the clean censuses that counted rows (tests and
// measurement).
var cleanCensusCounts atomic.Int64

// cleanCensusRepoCounts is the node and edge count a clean census reports:
// the stored counts when the state holds them, otherwise one count, stored
// when no state row exists at all.
func (idx *Indexer) cleanCensusRepoCounts(ctx context.Context) (nodes, edges int) {
	stored, generation, found, usable := idx.storedCensusCounts(ctx)
	if usable {
		idx.lastRepoCounts.Store(&repoCountSnapshot{nodes: stored.NodeCount, edges: stored.EdgeCount})
		idx.logCensusCounts("stored", generation, stored.NodeCount, stored.EdgeCount)
		return stored.NodeCount, stored.EdgeCount
	}
	cleanCensusCounts.Add(1)
	nodes, edges = idx.repoNodeEdgeCount()
	if !found {
		// Nothing stored: store the count, so the next start reuses it. A
		// row that exists without counts is left alone (its extractor
		// versions and baseline are not the census's to restamp).
		idx.persistRepoIndexState(nil, idx.rootPath, "", nodes, edges)
	}
	idx.logCensusCounts("counted", generation, nodes, edges)
	return nodes, edges
}

// storedCensusCounts reads the repository's stored state at the indexer's
// generation, or at its parent when the generation holds none. found reports
// that a state row exists at the generation read; usable that it carries
// counts.
func (idx *Indexer) storedCensusCounts(ctx context.Context) (state graph.RepoIndexState, generation int64, found, usable bool) {
	reader, ok := idx.graph.(graph.RepoIndexStateReader)
	if !ok {
		return state, 0, false, false
	}
	if st, isStore := idx.graph.(*store_sqlite.Store); isStore {
		generation = st.ViewGeneration()
	}
	state, found, err := reader.GetRepoIndexState(idx.repoPrefix)
	if err != nil {
		return state, generation, false, false
	}
	if found && (state.NodeCount > 0 || state.EdgeCount > 0) {
		return state, generation, true, true
	}
	st, isStore := idx.graph.(*store_sqlite.Store)
	if found || !isStore || generation <= 0 {
		return state, generation, found, false
	}
	// An output generation with no state of its own: a clean copy of its
	// parent carries the parent's rows, so the parent's counts describe it.
	row, rowFound, err := st.Catalog().GetViewGeneration(ctx, generation)
	if err != nil || !rowFound {
		return state, generation, false, false
	}
	parent := st.AtGeneration(row.BaseGenerationID)
	parentState, parentFound, err := parent.GetRepoIndexState(idx.repoPrefix)
	if err != nil || !parentFound || (parentState.NodeCount == 0 && parentState.EdgeCount == 0) {
		return state, generation, false, false
	}
	return parentState, row.BaseGenerationID, true, true
}

func (idx *Indexer) logCensusCounts(source string, generation int64, nodes, edges int) {
	if idx.logger == nil {
		return
	}
	idx.logger.Info("indexer: clean census counts",
		zap.String("repo", idx.repoPrefix), zap.String("source", source),
		zap.Int64("generation", generation), zap.Int("nodes", nodes), zap.Int("edges", edges))
}
