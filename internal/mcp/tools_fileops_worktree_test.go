package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// gitInit runs git in dir, failing the test on error. Kept local so the
// fileops worktree test doesn't depend on a helper from another package.
func gitInit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, string(out))
}

// resolvePath evaluates symlinks so a macOS /var -> /private/var alias
// can't break a filesystem-path equality assertion.
func resolvePath(t *testing.T, p string) string {
	t.Helper()
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// setupWorktreePair builds a real git repo with one linked worktree.
// The main checkout and the worktree each hold the same repo-relative
// file (shared.go) plus their own file. Returns the main checkout path,
// the worktree path, and a Server whose MultiIndexer tracks both.
func setupWorktreePair(t *testing.T) (mainRepo, worktree string, srv *Server) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	mainRepo = filepath.Join(t.TempDir(), "main-checkout")
	require.NoError(t, os.MkdirAll(mainRepo, 0o755))
	gitInit(t, mainRepo, "init", "-q", "-b", "main")
	gitInit(t, mainRepo, "config", "user.email", "test@example.com")
	gitInit(t, mainRepo, "config", "user.name", "Test")
	gitInit(t, mainRepo, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, "shared.go"),
		[]byte("package main\n\nfunc Shared() string { return \"main\" }\n"), 0o644))
	gitInit(t, mainRepo, "add", ".")
	gitInit(t, mainRepo, "commit", "-q", "-m", "init")

	// A linked worktree on its own branch. `git worktree add` checks
	// out shared.go into it; the test then gives the worktree its own
	// distinct copy so a misrooted write is unambiguous.
	worktree = filepath.Join(t.TempDir(), "feature-worktree")
	gitInit(t, mainRepo, "worktree", "add", "-q", "-b", "feature", worktree)
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "shared.go"),
		[]byte("package main\n\nfunc Shared() string { return \"worktree\" }\n"), 0o644))

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: mainRepo, Name: "main-checkout"},
		{Path: worktree, Name: "feature-worktree"},
	}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	require.True(t, mi.IsMultiRepo())

	eng := query.NewEngine(g)
	srv = NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	return mainRepo, worktree, srv
}

// TestEditFile_LandsInLinkedWorktree is the core piece-2 scenario: an
// edit addressed to a file in a linked worktree must modify that
// worktree's copy on disk, not the main checkout's. Both checkouts
// share one repo identity for index-cache reuse, so the resolver has to
// re-root the write at the correct worktree.
func TestEditFile_LandsInLinkedWorktree(t *testing.T) {
	mainRepo, worktree, srv := setupWorktreePair(t)

	mainShared := filepath.Join(mainRepo, "shared.go")
	wtShared := filepath.Join(worktree, "shared.go")
	mainBefore, err := os.ReadFile(mainShared)
	require.NoError(t, err)

	// Edit shared.go via the worktree's repo prefix.
	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "feature-worktree/shared.go",
		"old_string": `return "worktree"`,
		"new_string": `return "worktree-edited"`,
	})
	assert.False(t, result.IsError, "edit of a worktree file must succeed: %+v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])

	// The worktree's copy carries the edit.
	wtAfter, err := os.ReadFile(wtShared)
	require.NoError(t, err)
	assert.Contains(t, string(wtAfter), `return "worktree-edited"`,
		"the linked worktree's file must reflect the edit")

	// The main checkout's copy is untouched — the edit did NOT leak
	// into the main repo.
	mainAfter, err := os.ReadFile(mainShared)
	require.NoError(t, err)
	assert.Equal(t, string(mainBefore), string(mainAfter),
		"the main checkout's file must NOT be modified by a worktree-targeted edit")
}

// TestWriteFile_LandsInLinkedWorktree proves write_file (full-content
// overwrite) honours the same worktree rooting as edit_file.
func TestWriteFile_LandsInLinkedWorktree(t *testing.T) {
	mainRepo, worktree, srv := setupWorktreePair(t)

	mainShared := filepath.Join(mainRepo, "shared.go")
	mainBefore, err := os.ReadFile(mainShared)
	require.NoError(t, err)

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "feature-worktree/shared.go",
		"content": "package main\n\nfunc Shared() string { return \"rewritten-in-worktree\" }\n",
	})
	assert.False(t, result.IsError, "write to a worktree file must succeed: %+v", result.Content)

	wtAfter, err := os.ReadFile(filepath.Join(worktree, "shared.go"))
	require.NoError(t, err)
	assert.Contains(t, string(wtAfter), "rewritten-in-worktree",
		"write_file must land in the worktree's copy")

	mainAfter, err := os.ReadFile(mainShared)
	require.NoError(t, err)
	assert.Equal(t, string(mainBefore), string(mainAfter),
		"write_file targeting the worktree must not overwrite the main checkout")
}

// TestEditFile_MainCheckoutPrefixStillEditsMainCheckout is the other
// side of the rooting contract: an edit addressed via the *main*
// checkout's prefix must land in the main checkout, not bleed into a
// worktree. Worktree rooting must not hijack writes that legitimately
// target the main repo.
func TestEditFile_MainCheckoutPrefixStillEditsMainCheckout(t *testing.T) {
	mainRepo, worktree, srv := setupWorktreePair(t)

	wtShared := filepath.Join(worktree, "shared.go")
	wtBefore, err := os.ReadFile(wtShared)
	require.NoError(t, err)

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "main-checkout/shared.go",
		"old_string": `return "main"`,
		"new_string": `return "main-edited"`,
	})
	assert.False(t, result.IsError, "edit of a main-checkout file must succeed: %+v", result.Content)

	mainAfter, err := os.ReadFile(filepath.Join(mainRepo, "shared.go"))
	require.NoError(t, err)
	assert.Contains(t, string(mainAfter), `return "main-edited"`,
		"the main checkout's file must reflect the edit")

	wtAfter, err := os.ReadFile(wtShared)
	require.NoError(t, err)
	assert.Equal(t, string(wtBefore), string(wtAfter),
		"the linked worktree's file must NOT be modified by a main-checkout edit")
}

// fakeWorktreeLookup is a multiRepoLookup stub for testing
// worktreeRootedPath without standing up a full MultiIndexer.
type fakeWorktreeLookup struct {
	worktrees map[string][]string // mainRepoPath -> linked worktree roots
}

func (f fakeWorktreeLookup) RepoPrefixes() []string                   { return nil }
func (f fakeWorktreeLookup) RepoRoot(string) (string, bool)           { return "", false }
func (f fakeWorktreeLookup) LinkedWorktreeRoots(main string) []string { return f.worktrees[main] }

// TestWorktreeRootedPath covers the re-rooting helper's decision table
// directly: it re-roots only when a file genuinely lives in a sibling
// worktree, and leaves the path alone in every conservative case.
func TestWorktreeRootedPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	// Real git repo + worktree so ResolveWorktree classifies correctly.
	mainRepo := filepath.Join(t.TempDir(), "main")
	require.NoError(t, os.MkdirAll(mainRepo, 0o755))
	gitInit(t, mainRepo, "init", "-q", "-b", "main")
	gitInit(t, mainRepo, "config", "user.email", "test@example.com")
	gitInit(t, mainRepo, "config", "user.name", "Test")
	gitInit(t, mainRepo, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, "seed.go"),
		[]byte("package main\n"), 0o644))
	gitInit(t, mainRepo, "add", ".")
	gitInit(t, mainRepo, "commit", "-q", "-m", "init")

	worktree := filepath.Join(t.TempDir(), "wt")
	gitInit(t, mainRepo, "worktree", "add", "-q", "-b", "feature", worktree)

	// A file that exists ONLY in the worktree.
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "only_wt.go"),
		[]byte("package main\n"), 0o644))
	// A file that exists in the main checkout.
	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, "in_main.go"),
		[]byte("package main\n"), 0o644))

	// worktreeRootedPath passes the resolved root through to
	// LinkedWorktreeRoots verbatim, so the stub is keyed by the same
	// path string the helper will hand it.
	mi := fakeWorktreeLookup{worktrees: map[string][]string{
		mainRepo: {worktree},
		worktree: {worktree},
	}}

	t.Run("re-roots a file that lives only in the worktree", func(t *testing.T) {
		// Resolved under the main checkout, but the file is not there.
		abs := filepath.Join(mainRepo, "only_wt.go")
		got, err := worktreeRootedPath(abs, mainRepo, mi, true)
		require.NoError(t, err)
		assert.Equal(t, resolvePath(t, filepath.Join(worktree, "only_wt.go")),
			resolvePath(t, got),
			"a file present only in the worktree must be re-rooted there")
	})

	t.Run("leaves a file that exists in the resolved root", func(t *testing.T) {
		abs := filepath.Join(mainRepo, "in_main.go")
		got, err := worktreeRootedPath(abs, mainRepo, mi, true)
		require.NoError(t, err)
		assert.Equal(t, abs, got,
			"a file that exists under the resolved root must not be moved")
	})

	// TestWorktreeRootedPath/refuses-a-file-that-exists-in-both-main-and-a-worktree
	// pins the 2026-09-24 live incident: a session ran `git worktree add`,
	// never moved its cwd into the new worktree, then issued an unrouted
	// mutating edit for a path that git worktree add had copied into BOTH
	// checkouts. worktreeRootedPath used to short-circuit on the first
	// os.Stat hit against main and silently return main's path without ever
	// looking at the linked worktrees — landing the edit in main while the
	// caller's real intent was the worktree. It must now refuse instead.
	t.Run("refuses a file that exists in both main and a worktree", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(worktree, "in_main.go"),
			[]byte("package main\n"), 0o644))
		abs := filepath.Join(mainRepo, "in_main.go")
		got, err := worktreeRootedPath(abs, mainRepo, mi, true)
		assert.Empty(t, got)
		require.Error(t, err)
		assert.ErrorIs(t, err, errPathAmbiguousCheckout,
			"a file present in both main and a ready worktree must be refused, not silently defaulted to main")
	})

	t.Run("leaves a brand-new file under the named prefix", func(t *testing.T) {
		// A file in no checkout — a fresh write_file. It must stay
		// where the caller addressed it.
		abs := filepath.Join(mainRepo, "brand_new.go")
		got, err := worktreeRootedPath(abs, mainRepo, mi, true)
		require.NoError(t, err)
		assert.Equal(t, abs, got,
			"a new file must land under the prefix the caller named")
	})

	t.Run("leaves a path already inside a worktree", func(t *testing.T) {
		// Root is the worktree itself — the file is already in the
		// right checkout, nothing to re-root.
		abs := filepath.Join(worktree, "only_wt.go")
		got, err := worktreeRootedPath(abs, worktree, mi, true)
		require.NoError(t, err)
		assert.Equal(t, abs, got,
			"a path resolved against a worktree root must be left untouched")
	})

	t.Run("nil lookup is a no-op", func(t *testing.T) {
		abs := filepath.Join(mainRepo, "only_wt.go")
		got, err := worktreeRootedPath(abs, mainRepo, nil, true)
		require.NoError(t, err)
		assert.Equal(t, abs, got)
	})
}
