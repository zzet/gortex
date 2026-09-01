package indexer

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
)

// persistRepoIndexState records the per-repo freshness provenance at the
// end of a (re)index. diskTarget is the durable store when indexing
// streams to disk; nil falls back to idx.graph. Backends without durable
// state (the in-memory graph) do not implement RepoIndexStateWriter, so
// the write is skipped — exactly like the file-mtime ledger.
func (idx *Indexer) persistRepoIndexState(diskTarget graph.Store, rootAbs, workspaceFP string, nodes, edges int) {
	target := graph.Store(idx.graph)
	if diskTarget != nil {
		target = diskTarget
	}
	w, ok := target.(graph.RepoIndexStateWriter)
	if !ok {
		return
	}
	sha, dirty := repoHeadAndDirty(rootAbs)
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
func (idx *Indexer) reconcileRepoIndexState(rootAbs string) {
	prevFP := ""
	if r, ok := graph.Store(idx.graph).(graph.RepoIndexStateReader); ok {
		if prev, found, _ := r.GetRepoIndexState(idx.repoPrefix); found {
			prevFP = prev.WorkspaceFP
		}
	}
	nodes, edges := idx.repoNodeEdgeCount()
	idx.persistRepoIndexState(nil, rootAbs, prevFP, nodes, edges)
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
