package indexer

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
)

// indexStateTarget names the store the per-repo counters are written to.
//
// Factored out of persistRepoIndexState because a second caller has to reach
// the same handle: the planner-statistics freshness check reads exactly these
// counters, and asserting it on diskTarget alone would silently skip the whole
// direct-SQLite path, where diskTarget is nil and the counters live on
// idx.graph. Two copies of this selection would be two chances for the reader
// of the counters to look somewhere the writer never wrote.
func (idx *Indexer) indexStateTarget(diskTarget graph.Store) graph.Store {
	if diskTarget != nil {
		return diskTarget
	}
	return idx.graph
}

// persistRepoIndexState records the per-repo freshness provenance at the
// end of a (re)index. diskTarget is the durable store when indexing
// streams to disk; nil falls back to idx.graph. Backends without durable
// state (the in-memory graph) do not implement RepoIndexStateWriter, so
// the write is skipped — exactly like the file-mtime ledger.
func (idx *Indexer) persistRepoIndexState(diskTarget graph.Store, rootAbs, workspaceFP string, nodes, edges int) {
	target := idx.indexStateTarget(diskTarget)
	w, ok := target.(graph.RepoIndexStateWriter)
	if !ok {
		return
	}
	sha, dirty := idx.headAndDirty(rootAbs)
	vers, _ := json.Marshal(extractorVersionsSnapshot())
	st := graph.RepoIndexState{
		RepoPrefix:        idx.repoPrefix,
		IndexedSHA:        sha,
		Dirty:             dirty,
		IndexedAt:         time.Now().Unix(),
		WorkspaceFP:       workspaceFP,
		NodeCount:         nodes,
		EdgeCount:         edges,
		ExtractorVersions: string(vers),
	}
	if err := w.SetRepoIndexState(st); err != nil {
		idx.logger.Warn("persist repo index state failed",
			zap.String("repo", idx.repoPrefix), zap.Error(err))
	}
}

// persistExtractorVersion advances one extraction-policy key without
// restamping unrelated repository provenance. It is used after a scoped warm
// refresh has proved every stored generated-parser projection current.
func (idx *Indexer) persistExtractorVersion(lang string) {
	r, readOK := idx.graph.(graph.RepoIndexStateReader)
	w, writeOK := idx.graph.(graph.RepoIndexStateWriter)
	if !readOK || !writeOK {
		return
	}
	st, found, err := r.GetRepoIndexState(idx.repoPrefix)
	if err != nil || !found {
		return
	}

	versions := make(map[string]int)
	if st.ExtractorVersions != "" {
		if err := json.Unmarshal([]byte(st.ExtractorVersions), &versions); err != nil {
			idx.logger.Warn("decode extractor versions failed",
				zap.String("repo", idx.repoPrefix), zap.Error(err))
			return
		}
	}
	current := extractorVersionForLang(lang)
	if versions[lang] >= current {
		return
	}
	versions[lang] = current
	encoded, err := json.Marshal(versions)
	if err != nil {
		return
	}
	st.ExtractorVersions = string(encoded)
	if err := w.SetRepoIndexState(st); err != nil {
		idx.logger.Warn("persist extractor version failed",
			zap.String("repo", idx.repoPrefix), zap.String("language", lang), zap.Error(err))
	}
}

// reconcileRepoIndexState re-stamps the per-repo freshness row at the
// current HEAD after the git-watcher catches the index up to a new
// commit. The full (re)index is otherwise the only writer of this row,
// so without this the row keeps the SHA from the last full index and
// `gortex repos` reports the repo stale even though the in-memory graph
// already reflects HEAD. The Merkle baseline (WorkspaceFP) from that
// last full index is preserved — the incremental reconcile diffs against
// it but never rebuilds it. No-op on backends without durable index
// state (the in-memory graph is not a RepoIndexStateWriter).
func (idx *Indexer) reconcileRepoIndexState(ctx context.Context, rootAbs string) {
	prevFP := ""
	if r, ok := graph.Store(idx.graph).(graph.RepoIndexStateReader); ok {
		if prev, found, _ := r.GetRepoIndexState(idx.repoPrefix); found {
			prevFP = prev.WorkspaceFP
		}
	}
	nodes, edges := idx.repoNodeEdgeCount()
	idx.persistRepoIndexState(nil, rootAbs, prevFP, nodes, edges)
	// The per-commit / HEAD-move incremental path — git-watcher and poller
	// both land here — reaches none of the other freshness call sites: it
	// never drains a shadow and never runs the full pass that ends at the
	// counter site. A daemon that only ever reindexes changed files would
	// therefore keep the statistics of the cold load that created the store,
	// which is the state issue #651 describes. Both halves of the verdict are
	// current here: the rows are already in the physical tables and the
	// counters describing them were written a line ago.
	//
	// The store's own write gate is free — SetRepoIndexState released it — but
	// WIDER locks are held all the way down to here, and they are why the
	// refresh has to be cooperative. Both callers arrive through
	// coordinateRepositoryMutation, so the shared batchMutationGate is held
	// for reading and this repository's lane is held exclusively
	// (repository_mutation_coordinator.go). A refresh that queued on the store
	// gate, or held it across a whole-store ANALYZE, would hold those for the
	// same span and stall every other repository's lane behind it.
	// EnsurePlannerStatsFresh therefore try-locks, analyzes ONE index per hold
	// under a per-index timeout, and stops rather than waits — which is what
	// keeps other store writers, and the bounded-gate writers that drop their
	// batches after 15 s, alive.
	//
	// It bounds what this boundary pays, it does not make it free: the pass
	// stops starting new indexes once its budget is spent, so the gates above
	// are held for that budget, plus the one index already in flight, plus one
	// bounded sqlite_schema reload at the end of a completed pass. The rest of
	// the work list rides a resume cursor to the next commit or HEAD move,
	// which is where the family finishes. Four reads sit outside that bound
	// and are paid under these same gates — two health probes, the
	// present-index list and the stat-row set — but all four go to the read
	// pool and take no store lock.
	//
	// And ctx is not a shutdown lever on every path that reaches here. The
	// git-watcher threads its own cancellable context, but the poller and the
	// extractor-version restage caller both pass context.Background(), so a
	// daemon shutting down cannot cancel a pass in flight on those. What bounds
	// them is the budget plus one index timeout — which is the reason both
	// numbers are small enough to sit inside a shutdown without being noticed,
	// and the reason the cancellation branch is a deferral rather than
	// something the caller has to react to.
	graph.MaybeEnsurePlannerStatsFresh(ctx, idx.indexStateTarget(nil))
}

// repoHeadAndDirty returns the working tree's current commit SHA and
// whether it has uncommitted changes. Best-effort: a non-git directory or
// any git error yields ("", false) — freshness provenance never blocks
// indexing. Git shell-outs route through the shared concurrency limiter.
func repoHeadAndDirty(rootAbs string) (sha string, dirty bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sha, err := gitcmd.Output(ctx, rootAbs, "rev-parse", "HEAD")
	if err != nil {
		return "", false
	}
	// Status may otherwise refresh Git's cached stat data by taking the
	// worktree index lock. Indexing is a read-only observer and can run beside
	// user commands such as git add, so it must not compete for index.lock.
	status, err := gitcmd.Output(ctx, rootAbs, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return sha, false
	}
	return sha, status != ""
}

// repoHeadProvenance is a HEAD commit and dirty bit a caller already holds.
type repoHeadProvenance struct {
	sha   string
	dirty bool
}

// headAndDirty is repoHeadAndDirty, answered from the caller's provenance
// when it supplied one: a working-tree generation build sampled HEAD and the
// dirty set before it started, and asking git again (rev-parse plus a
// whole-checkout status, through the shared git limiter) cost every edit
// hundreds of milliseconds on a busy daemon.
func (idx *Indexer) headAndDirty(rootAbs string) (string, bool) {
	if p := idx.headProvenance; p != nil {
		return p.sha, p.dirty
	}
	return repoHeadAndDirty(rootAbs)
}

// head is repoHead, answered from the caller's provenance when set.
func (idx *Indexer) head(rootAbs string) string {
	if p := idx.headProvenance; p != nil {
		return p.sha
	}
	return repoHead(rootAbs)
}

// repoHead returns the working tree's current commit SHA, or "" for a non-git
// directory or any git error. A lighter probe than repoHeadAndDirty when the
// caller does not yet need the dirty bit — it skips the git status shell-out,
// which is the slow half on a large repo. The dirty bit is fetched separately
// (via repoHeadAndDirty) only when the cheap sha check leaves the decision open.
func repoHead(rootAbs string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sha, err := gitcmd.Output(ctx, rootAbs, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return sha
}
