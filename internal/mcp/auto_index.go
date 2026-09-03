package mcp

import (
	"context"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitcmd"
)

// autoIndexFileLimit bounds zero-config auto-indexing: a cwd repo with more
// git-tracked files than this is left for an explicit `gortex track`, so a
// stray tool call never silently chews through an enormous tree.
const autoIndexFileLimit = 25000

// maybeAutoIndexCWD lazily background-indexes the current working directory
// the first time a tool is called in a session, when the cwd is an untracked
// git repo. It is OFF by default and opt-in via GORTEX_AUTOINDEX=1 — the
// consent guardrail: auto-indexing is expensive, so the user asks for it.
// The request path pays only one getenv + a sync.Once check; all real work
// (git probing, the size bound, the index) runs on a background goroutine.
func (s *Server) maybeAutoIndexCWD() {
	if s == nil || s.multiIndexer == nil {
		return
	}
	if os.Getenv("GORTEX_AUTOINDEX") != "1" {
		return
	}
	s.autoIndexOnce.Do(func() { go s.autoIndexCWDBackground() })
}

// autoIndexCWDBackground performs the bounded, consented auto-index off the
// request path: resolve the git root of the cwd, skip it when already
// covered or oversized, and otherwise track it.
func (s *Server) autoIndexCWDBackground() {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return
	}
	// Already covered by a tracked repo? nothing to do.
	if s.multiIndexer.RepoForFile(cwd) != "" {
		return
	}
	root := gitRepoRoot(cwd)
	if root == "" || s.multiIndexer.RepoForFile(root) != "" {
		return
	}
	// Bounded: a tree larger than the limit is left for an explicit track.
	if n, over := repoFileCountOverLimit(root, autoIndexFileLimit); over {
		if s.logger != nil {
			s.logger.Info("auto-index: cwd repo exceeds the file-count limit; run `gortex track` to index it",
				zap.String("root", root), zap.Int("limit", autoIndexFileLimit), zap.Int("at_least", n))
		}
		return
	}
	if s.logger != nil {
		s.logger.Info("auto-index: background-indexing untracked cwd", zap.String("root", root))
	}
	ctx := context.Background()
	if _, err := s.multiIndexer.TrackRepoCtx(ctx, config.RepoEntry{Path: root}); err != nil {
		if s.logger != nil {
			s.logger.Warn("auto-index: track failed", zap.String("root", root), zap.Error(err))
		}
		return
	}
	// Implicit registration: the checkout is recorded — family, identity,
	// graph binding — and its watcher attached, but no tracking intent is
	// written. Nobody asked for this repository; the daemon indexed it on
	// its own initiative, and an intent would claim otherwise.
	if s.lifecycle != nil {
		if err := s.lifecycle.RecordImplicit(ctx, root); err != nil && s.logger != nil {
			s.logger.Warn("auto-index: recording the checkout failed",
				zap.String("root", root), zap.Error(err))
		}
	}
}

// gitRepoRoot resolves the git working-tree root containing dir, or "" when
// dir is not in a git repo.
func gitRepoRoot(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := gitcmd.Output(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// repoFileCountOverLimit counts git-tracked files under root, reporting
// whether the count exceeds limit. A git error returns (0, false) — when we
// cannot measure the size we do not block (the indexer's own caps apply).
func repoFileCountOverLimit(root string, limit int) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := gitcmd.Output(ctx, root, "ls-files")
	if err != nil {
		return 0, false
	}
	n := strings.Count(out, "\n")
	if strings.TrimSpace(out) != "" && !strings.HasSuffix(out, "\n") {
		n++ // last line without a trailing newline
	}
	return n, n > limit
}
