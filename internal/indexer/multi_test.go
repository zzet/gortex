package indexer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"pgregory.net/rapid"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// newTestConfigManager creates a ConfigManager with an empty GlobalConfig
// pointing at a temp file so Save() doesn't touch the real config.
func newTestConfigManager(t *testing.T) *config.ConfigManager {
	t.Helper()
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	cm, err := config.NewConfigManager(tmpFile)
	require.NoError(t, err)
	return cm
}

// newTestRegistry returns a parser registry with Go support.
func newTestRegistry() *parser.Registry {
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	return reg
}

func TestReconcilePhaseTimingOutcomes(t *testing.T) {
	for _, outcome := range []string{"complete", "error", "aborted"} {
		t.Run(outcome, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			timing := startReconcilePhase(zap.New(core), "repo", "test", zap.Int("files", 3))
			switch outcome {
			case "complete":
				timing.complete(nil)
			case "error":
				timing.complete(errors.New("census failed"))
			case "aborted":
				timing.abort()
			}
			// Deferred abort and duplicate completion cannot turn an error into
			// success or emit multiple terminal records.
			timing.abort()
			timing.complete(nil)
			entries := logs.All()
			require.Len(t, entries, 2)
			require.Equal(t, "indexer: reconcile phase started", entries[0].Message)
			require.Equal(t, "indexer: reconcile phase complete", entries[1].Message)
			for _, entry := range entries {
				require.Equal(t, "repo", entry.ContextMap()["repo"])
				require.Equal(t, "test", entry.ContextMap()["phase"])
				require.EqualValues(t, 3, entry.ContextMap()["files"])
			}
			fields := entries[1].ContextMap()
			require.Equal(t, outcome, fields["outcome"])
			elapsed, ok := fields["elapsed"].(time.Duration)
			require.True(t, ok, "elapsed is a structured duration")
			require.GreaterOrEqual(t, elapsed, time.Duration(0))
			if outcome == "error" {
				require.Equal(t, "census failed", fields["error"])
			}
		})
	}
}

func TestReconcileCensusTiming(t *testing.T) {
	for _, mode := range []string{"clean", "changed", "missing", "panic"} {
		t.Run(mode, func(t *testing.T) {
			dir := setupRepoDir(t, "census")
			core, logs := observer.New(zap.InfoLevel)
			idx := New(graph.New(), newTestRegistry(), config.IndexConfig{}, zap.New(core))
			t.Cleanup(func() { idx.Close() })
			idx.SetRepoPrefix("repo")
			info, err := os.Stat(filepath.Join(dir, "main.go"))
			require.NoError(t, err)
			mtime := info.ModTime().UnixNano()
			if mode == "changed" {
				mtime = 0
			}
			idx.SetFileMtimes(map[string]int64{"main.go": mtime})
			if mode == "missing" {
				dir = filepath.Join(dir, "missing")
			}
			if mode == "panic" {
				// Inject a parser lookup panic; the telemetry must not claim a
				// successful no-op merely because the named error is still nil.
				registry := idx.registry
				idx.registry = nil
				defer func() { idx.registry = registry }()
				require.Panics(t, func() { _, _, _, _ = idx.changedSinceMtimesCensus(dir) })
				entries := logs.FilterMessage("indexer: reconcile phase complete").All()
				require.Len(t, entries, 1)
				fields := entries[0].ContextMap()
				require.Equal(t, "aborted", fields["outcome"])
				require.NotContains(t, fields, "no_changes")
				return
			}
			changed, deleted, detected, err := idx.changedSinceMtimesCensus(dir)
			if mode == "missing" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, detected)
				require.Empty(t, deleted)
				require.Len(t, changed, map[string]int{"clean": 0, "changed": 1}[mode])
			}
			entries := logs.FilterMessage("indexer: reconcile phase complete").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, "census", fields["phase"])
			require.EqualValues(t, len(changed), fields["changed_files"])
			require.EqualValues(t, detected, fields["detected_files"])
			require.Equal(t, mode == "clean", fields["no_changes"])
			if mode == "missing" {
				require.Equal(t, "error", fields["outcome"])
				require.NotEmpty(t, fields["error"])
			} else {
				require.Equal(t, "complete", fields["outcome"])
			}
		})
	}
}

func TestIncrementalReconcilePhaseTimings(t *testing.T) {
	dir := setupRepoDir(t, "incremental")
	core, logs := observer.New(zap.InfoLevel)
	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{}, zap.New(core))
	t.Cleanup(func() { idx.Close() })
	idx.SetRepoPrefix("repo")
	idx.SetDeferResolve(true)
	idx.SetDeferGlobalPasses(true)

	result, err := idx.incrementalReindexPathsMode(dir, []string{"main.go"}, incrementalPathMode{})
	require.NoError(t, err)
	require.Equal(t, 1, result.StaleFileCount)
	require.NotEmpty(t, g.AllNodes())
	phases := make(map[string]map[string]any)
	for _, entry := range logs.FilterMessage("indexer: reconcile phase complete").All() {
		fields := entry.ContextMap()
		require.Equal(t, "repo", fields["repo"])
		require.Equal(t, "complete", fields["outcome"])
		phases[fields["phase"].(string)] = fields
	}
	for _, phase := range []string{"fts_normalization", "scoped_discovery", "deletion_frontier",
		"graph_delete", "chunk_prior_graph", "chunk_parse", "chunk_graph_apply", "chunk_receipts",
		"contracts_and_metadata"} {
		require.Contains(t, phases, phase)
	}
	require.EqualValues(t, 1, phases["chunk_parse"]["consumed_files"])
	require.Equal(t, true, phases["chunk_parse"]["includes_admission_wait"])
	require.EqualValues(t, 1, phases["chunk_graph_apply"]["staged_files"])

	logs.TakeAll()
	result, err = idx.incrementalReindexPathsMode(dir, []string{"main.go"}, incrementalPathMode{})
	require.NoError(t, err)
	require.Zero(t, result.StaleFileCount)
	for _, entry := range logs.FilterMessage("indexer: reconcile phase started").All() {
		require.NotEqual(t, "chunk_parse", entry.ContextMap()["phase"], "no-op reconciliation must not invent parse work")
	}

	logs.TakeAll()
	_, err = idx.incrementalReindexPathsMode(dir, []string{"../outside.go"}, incrementalPathMode{})
	require.Error(t, err)
	aborted := false
	for _, entry := range logs.FilterMessage("indexer: reconcile phase complete").All() {
		fields := entry.ContextMap()
		if fields["phase"] == "scoped_discovery" {
			require.Equal(t, "aborted", fields["outcome"])
			aborted = true
		}
	}
	require.True(t, aborted, "early discovery failure must have a terminal phase record")
}

// setupRepoDir creates a temp directory with a Go file for testing.
func setupRepoDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func Hello() {}
`)
	return dir
}

func TestNewMultiIndexer(t *testing.T) {
	g := graph.New()
	reg := newTestRegistry()
	s := search.NewNull()
	cm := newTestConfigManager(t)

	mi := NewMultiIndexer(g, reg, s, cm, zap.NewNop())
	require.NotNil(t, mi)
	assert.False(t, mi.IsMultiRepo())
	assert.Empty(t, mi.AllMetadata())
	assert.Same(t, g, mi.Graph())
	assert.Same(t, s, mi.Search())
}

func TestMultiIndexer_IndexAll_SingleRepo(t *testing.T) {
	dir := setupRepoDir(t, "myrepo")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	results, err := mi.IndexAll()
	require.NoError(t, err)
	require.Contains(t, results, "myrepo")

	res := results["myrepo"]
	assert.Greater(t, res.NodeCount, 0)
	assert.Greater(t, res.FileCount, 0)

	// One repo: IsMultiRepo() is false — it reports the repo COUNT.
	assert.False(t, mi.IsMultiRepo())

	// ...but the ID shape does not depend on that count. A lone repo is the
	// first tracked repo and its nodes carry its prefix like any other's.
	for _, n := range g.AllNodes() {
		assert.Equal(t, "myrepo", n.RepoPrefix, "a lone repo's nodes carry its prefix")
		assert.True(t, strings.HasPrefix(n.ID, "myrepo/"), "node ID %q must carry the repo prefix", n.ID)
	}

	// Metadata should be populated.
	meta := mi.GetMetadata("myrepo")
	require.NotNil(t, meta)
	assert.Equal(t, "myrepo", meta.RepoPrefix)
	assert.Equal(t, dir, meta.RootPath)
	assert.Greater(t, meta.FileCount, 0)
}

func TestMultiIndexer_IndexAll_MultiRepo(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	results, err := mi.IndexAll()
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.Contains(t, results, "repo-a")
	assert.Contains(t, results, "repo-b")

	// Multi-repo mode.
	assert.True(t, mi.IsMultiRepo())

	// Nodes should have repo prefix set.
	for _, n := range g.AllNodes() {
		assert.NotEmpty(t, n.RepoPrefix, "multi-repo nodes should have RepoPrefix")
		assert.Contains(t, []string{"repo-a", "repo-b"}, n.RepoPrefix)
	}

	// Both repos should have metadata.
	assert.NotNil(t, mi.GetMetadata("repo-a"))
	assert.NotNil(t, mi.GetMetadata("repo-b"))
	assert.Len(t, mi.AllMetadata(), 2)
}

func TestMultiIndexer_IndexAll_SingleRepoLoadsWorkspaceExclude(t *testing.T) {
	dir := setupRepoDir(t, "myrepo")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ignored"), 0o755))
	writeFile(t, filepath.Join(dir, "ignored", "ignored.go"), "package main\nfunc Ignored() {}\n")
	writeFile(t, filepath.Join(dir, ".gortex.yaml"), "workspace: shared\nproject: app\nexclude:\n  - ignored/**\n")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}}}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.IndexAll()
	require.NoError(t, err)

	for _, n := range g.AllNodes() {
		assert.NotContains(t, n.FilePath, "ignored/ignored.go")
		assert.NotContains(t, n.ID, "Ignored")
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			assert.Equal(t, "shared", n.WorkspaceID)
			assert.Equal(t, "app", n.ProjectID)
		}
	}
}

func TestMultiIndexer_IndexAll_MultiRepoLoadsWorkspaceExclude(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	require.NoError(t, os.MkdirAll(filepath.Join(repoA, "ignored"), 0o755))
	writeFile(t, filepath.Join(repoA, "ignored", "ignored.go"), "package main\nfunc Ignored() {}\n")
	writeFile(t, filepath.Join(repoA, ".gortex.yaml"), "workspace: shared\nproject: api\nexclude:\n  - ignored/**\n")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.IndexAll()
	require.NoError(t, err)

	for _, n := range g.AllNodes() {
		assert.NotContains(t, n.FilePath, "ignored/ignored.go")
		assert.NotContains(t, n.ID, "Ignored")
		if n.RepoPrefix == "repo-a" && n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			assert.Equal(t, "shared", n.WorkspaceID)
			assert.Equal(t, "api", n.ProjectID)
		}
	}
}

func TestMultiIndexer_IndexRepo(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	writeFile(t, filepath.Join(repoA, ".gortex.yaml"), "workspace: shared\nproject: api\n")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.IndexAll()
	require.NoError(t, err)

	nodesBefore := g.NodeCount()
	repoBNodes := len(g.GetRepoNodes("repo-b"))

	// Re-index repo-a.
	result, err := mi.IndexRepo("repo-a")
	require.NoError(t, err)
	assert.Greater(t, result.NodeCount, 0)

	// Repo B should be unchanged.
	assert.Equal(t, repoBNodes, len(g.GetRepoNodes("repo-b")))
	// Total should be roughly the same (re-indexed, not duplicated).
	assert.InDelta(t, nodesBefore, g.NodeCount(), 2)
	for _, n := range g.GetRepoNodes("repo-a") {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			assert.Equal(t, "shared", n.WorkspaceID)
			assert.Equal(t, "api", n.ProjectID)
		}
	}
}

func TestMultiIndexer_IndexRepo_NotFound(t *testing.T) {
	g := graph.New()
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err := mi.IndexRepo("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "repository not found")
}

func TestMultiIndexer_TrackRepo(t *testing.T) {
	dir := setupRepoDir(t, "tracked")

	g := graph.New()
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	result, err := mi.TrackRepo(config.RepoEntry{Path: dir, Name: "tracked"})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Greater(t, result.NodeCount, 0)

	// Should be in metadata.
	meta := mi.GetMetadata("tracked")
	require.NotNil(t, meta)
	assert.Equal(t, "tracked", meta.RepoPrefix)
	assert.Equal(t, dir, meta.RootPath)

	// Track again — should return nil (already tracked).
	result2, err := mi.TrackRepo(config.RepoEntry{Path: dir, Name: "tracked"})
	require.NoError(t, err)
	assert.Nil(t, result2)
}

// TestMultiIndexer_TrackRepo_SearchSpansAllRepos verifies that scoping
// native symbol FTS writes to the current repo (the perf fix that drops the
// O(N²) re-index of every prior repo's nodes on every TrackRepo call)
// does not regress search recall. After three repos are tracked, the
// shared search backend must still find symbols defined in the first,
// second, and third repos — earlier-tracked entries are not lost
// because each repo's TrackRepo pass already added its own nodes.
func TestMultiIndexer_TrackRepo_SearchSpansAllRepos(t *testing.T) {
	mkRepo := func(name, symbol string) string {
		dir := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		writeFile(t, filepath.Join(dir, "main.go"), fmt.Sprintf("package main\n\nfunc %s() {}\n", symbol))
		return dir
	}
	dirA := mkRepo("repo-aaa", "AlphaSymbolUnique")
	dirB := mkRepo("repo-bbb", "BetaSymbolUnique")
	dirC := mkRepo("repo-ccc", "GammaSymbolUnique")

	// Pre-populate the global config so willBeMultiRepo trips on the
	// first TrackRepo too — that's the production warmup-loop path.
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: dirA, Name: "repo-aaa"},
		{Path: dirB, Name: "repo-bbb"},
		{Path: dirC, Name: "repo-ccc"},
	}}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	store := newFTSStore(t)
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewSymbolSearcherBackend(store), cm, zap.NewNop())

	for _, e := range []config.RepoEntry{
		{Path: dirA, Name: "repo-aaa"},
		{Path: dirB, Name: "repo-bbb"},
		{Path: dirC, Name: "repo-ccc"},
	} {
		_, err := mi.TrackRepo(e)
		require.NoError(t, err)
	}
	requireSymbolFTS(t, store)

	// Query the camelCase-split tokens individually — that's how the
	// write-side tokenizer stores them, and the query tokenizer doesn't
	// perform the same camelCase split. Each is also a token no node
	// carries as its whole name, so the exact-name short-circuit
	// (store_fts.go tier 0) misses and the ranked FTS tier answers.
	for _, want := range []struct{ query, prefix string }{
		{"alpha", "repo-aaa"},
		{"beta", "repo-bbb"},
		{"gamma", "repo-ccc"},
	} {
		hits := mi.Search().Search(want.query, 10)
		require.NotEmpty(t, hits, "shared search backend must find %q after all repos tracked", want.query)
		assert.True(t, strings.HasPrefix(hits[0].ID, want.prefix+"/"),
			"top hit for %q should belong to %s, got %s", want.query, want.prefix, hits[0].ID)
	}
}

// TestMultiIndexer_TrackRepo_EmptyAfterPopulated regresses the bug
// where IndexResult/RepoMetadata stamped the *whole multi-repo graph*
// counts onto an empty repo at TrackRepo time. A second tracked repo
// that contributed zero source files used to come back with the same
// node count as the populated repo because IndexResult.NodeCount was
// graph.NodeCount() rather than the per-repo contribution. Downstream
// daemon-status code multiplied search backend bytes by share = 1 for
// every empty row, attributing the entire workspace search budget to
// each empty repo. The fix: IndexResult counts come from
// graph.RepoMemoryEstimate(prefix) when repoPrefix is non-empty, and
// daemon_controller.Status no longer falls back to meta when
// RepoStats has no entry for the prefix.
func TestMultiIndexer_TrackRepo_EmptyAfterPopulated(t *testing.T) {
	populated := setupRepoDir(t, "populated")
	empty := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.MkdirAll(empty, 0o755))

	g := graph.New()
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	first, err := mi.TrackRepo(config.RepoEntry{Path: populated, Name: "populated"})
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Greater(t, first.NodeCount, 0, "populated repo must contribute nodes")

	globalNodesBefore := g.NodeCount()
	require.Greater(t, globalNodesBefore, 0)

	second, err := mi.TrackRepo(config.RepoEntry{Path: empty, Name: "empty"})
	require.NoError(t, err)
	require.NotNil(t, second)

	// Per-repo IndexResult must not echo the workspace-wide graph total.
	assert.Equal(t, 0, second.NodeCount,
		"empty repo IndexResult.NodeCount must be 0, got %d (global graph has %d)",
		second.NodeCount, globalNodesBefore)
	assert.Equal(t, 0, second.EdgeCount,
		"empty repo IndexResult.EdgeCount must be 0, got %d", second.EdgeCount)

	// RepoMetadata is stamped from IndexResult, so it must agree.
	emptyMeta := mi.GetMetadata("empty")
	require.NotNil(t, emptyMeta)
	assert.Equal(t, 0, emptyMeta.NodeCount, "empty repo metadata must record 0 nodes")
	assert.Equal(t, 0, emptyMeta.EdgeCount, "empty repo metadata must record 0 edges")

	// And the populated repo's metadata must reflect only its own
	// contribution, not its size + everything that came after.
	populatedMeta := mi.GetMetadata("populated")
	require.NotNil(t, populatedMeta)
	assert.Greater(t, populatedMeta.NodeCount, 0)
	assert.LessOrEqual(t, populatedMeta.NodeCount, globalNodesBefore,
		"populated repo metadata must not exceed graph total at its track time")
}

func TestMultiIndexer_TrackRepo_InvalidPath(t *testing.T) {
	g := graph.New()
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err := mi.TrackRepo(config.RepoEntry{Path: "/nonexistent/path/xyz"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "path does not exist")
}

func TestMultiIndexer_TrackRepo_NotADirectory(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(tmpFile, []byte("hello"), 0o644))

	g := graph.New()
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err := mi.TrackRepo(config.RepoEntry{Path: tmpFile})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestMultiIndexer_UntrackRepo(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.IndexAll()
	require.NoError(t, err)

	repoBNodesBefore := len(g.GetRepoNodes("repo-b"))
	assert.Greater(t, len(g.GetRepoNodes("repo-a")), 0)

	// Untrack repo-a.
	nodesRemoved, edgesRemoved := mi.UntrackRepo("repo-a")
	assert.Greater(t, nodesRemoved, 0)
	_ = edgesRemoved

	// Repo-a should be gone.
	assert.Nil(t, mi.GetMetadata("repo-a"))
	assert.Empty(t, g.GetRepoNodes("repo-a"))

	// Repo-b should be unchanged.
	assert.Equal(t, repoBNodesBefore, len(g.GetRepoNodes("repo-b")))
}

func TestMultiIndexer_UntrackRepo_NotFound(t *testing.T) {
	g := graph.New()
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	nodesRemoved, edgesRemoved := mi.UntrackRepo("nonexistent")
	assert.Equal(t, 0, nodesRemoved)
	assert.Equal(t, 0, edgesRemoved)
}

func TestMultiIndexer_RepoForFile(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.IndexAll()
	require.NoError(t, err)

	// File in repo-a.
	assert.Equal(t, "repo-a", mi.RepoForFile(filepath.Join(repoA, "main.go")))
	// File in repo-b.
	assert.Equal(t, "repo-b", mi.RepoForFile(filepath.Join(repoB, "main.go")))
	// Unknown file.
	assert.Empty(t, mi.RepoForFile("/some/random/path.go"))
}

func TestMultiIndexer_GetIndexer(t *testing.T) {
	dir := setupRepoDir(t, "myrepo")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.IndexAll()
	require.NoError(t, err)

	idx := mi.GetIndexer("myrepo")
	assert.NotNil(t, idx)
	assert.Nil(t, mi.GetIndexer("nonexistent"))
}

func TestMultiIndexer_IndexAll_EmptyRepos(t *testing.T) {
	cm := newTestConfigManager(t)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	results, err := mi.IndexAll()
	require.NoError(t, err)
	assert.Nil(t, results)
}

// Feature: multi-repo-support, Property 6: Node ID format
//
// TestPropertyNodeIDFormat verifies that node IDs always match
// <repo_prefix>/<path>::<Symbol> and RepoPrefix is always non-empty,
// whether the workspace holds one repo or several.
//
// This property used to be conditional — a lone repo minted <path>::<Symbol>
// with an empty RepoPrefix — which meant one graph could carry two ID
// schemes depending on how many repos happened to be tracked. Repo COUNT
// still decides IsMultiRepo(), but it no longer decides ID SHAPE.
func TestPropertyNodeIDFormat(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random function names for uniqueness per iteration.
		funcNameA := "Func" + rapid.StringMatching(`[A-Z][a-z]{2,6}`).Draw(rt, "funcNameA")
		funcNameB := "Func" + rapid.StringMatching(`[A-Z][a-z]{2,6}`).Draw(rt, "funcNameB")

		// The number of tracked repos must not change the ID shape.
		multiRepo := rapid.Bool().Draw(rt, "multiRepo")

		tmpBase := t.TempDir()

		if multiRepo {
			// --- Multi-repo mode: 2 repos ---
			repoA := filepath.Join(tmpBase, "repo-a")
			repoB := filepath.Join(tmpBase, "repo-b")
			require.NoError(t, os.MkdirAll(repoA, 0o755))
			require.NoError(t, os.MkdirAll(repoB, 0o755))

			writeFile(t, filepath.Join(repoA, "main.go"),
				fmt.Sprintf("package main\n\nfunc %s() {}\n", funcNameA))
			writeFile(t, filepath.Join(repoB, "main.go"),
				fmt.Sprintf("package main\n\nfunc %s() {}\n", funcNameB))

			tmpCfg := filepath.Join(tmpBase, "config.yaml")
			gc := &config.GlobalConfig{
				Repos: []config.RepoEntry{
					{Path: repoA, Name: "repo-a"},
					{Path: repoB, Name: "repo-b"},
				},
			}
			gc.SetConfigPath(tmpCfg)
			require.NoError(t, gc.Save())

			cm, err := config.NewConfigManager(tmpCfg)
			require.NoError(t, err)

			g := graph.New()
			mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

			results, err := mi.IndexAll()
			require.NoError(t, err)
			require.Len(t, results, 2)

			// Must be multi-repo mode.
			if !mi.IsMultiRepo() {
				rt.Error("expected IsMultiRepo() == true for 2 repos")
			}

			assertNodesArePrefixed(rt, g, "two repos")
		} else {
			// --- One repo: same ID shape, IsMultiRepo() is false ---
			repoDir := filepath.Join(tmpBase, "solo-repo")
			require.NoError(t, os.MkdirAll(repoDir, 0o755))

			writeFile(t, filepath.Join(repoDir, "main.go"),
				fmt.Sprintf("package main\n\nfunc %s() {}\n", funcNameA))

			tmpCfg := filepath.Join(tmpBase, "config.yaml")
			gc := &config.GlobalConfig{
				Repos: []config.RepoEntry{
					{Path: repoDir, Name: "solo-repo"},
				},
			}
			gc.SetConfigPath(tmpCfg)
			require.NoError(t, gc.Save())

			cm, err := config.NewConfigManager(tmpCfg)
			require.NoError(t, err)

			g := graph.New()
			mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

			results, err := mi.IndexAll()
			require.NoError(t, err)
			require.Len(t, results, 1)

			// One repo: not multi-repo, but the IDs look identical.
			if mi.IsMultiRepo() {
				rt.Error("expected IsMultiRepo() == false for 1 repo")
			}

			assertNodesArePrefixed(rt, g, "one repo")
		}
	})
}

// assertNodesArePrefixed is the single ID-shape invariant, applied to
// workspaces of every size: every node carries a repo prefix, its ID starts
// with that prefix, and symbol nodes keep the "::" path/symbol separator.
func assertNodesArePrefixed(rt *rapid.T, g *graph.Graph, workspace string) {
	for _, n := range g.AllNodes() {
		if n.RepoPrefix == "" {
			rt.Errorf("%s: node %q has empty RepoPrefix", workspace, n.ID)
			continue
		}
		prefix := n.RepoPrefix + "/"
		if !strings.HasPrefix(n.ID, prefix) {
			rt.Errorf("%s: node ID %q does not start with prefix %q", workspace, n.ID, prefix)
		}
		if n.Kind != graph.KindFile && n.Kind != graph.KindPackage && n.Kind != graph.KindImport {
			if !strings.Contains(n.ID, "::") {
				rt.Errorf("%s: node ID %q missing '::' separator", workspace, n.ID)
			}
		}
	}
}

// Feature: multi-repo-support, Property 9: Re-index isolation
//
// TestPropertyReindexIsolation verifies that re-indexing repo A does not
// modify, remove, or add any nodes or edges belonging to repo B.
// The node count and edge count for repo B before and after re-indexing A
// are identical.
func TestPropertyReindexIsolation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random function names for uniqueness per iteration.
		funcNameA := "Func" + rapid.StringMatching(`[A-Z][a-z]{2,6}`).Draw(rt, "funcNameA")
		funcNameB := "Func" + rapid.StringMatching(`[A-Z][a-z]{2,6}`).Draw(rt, "funcNameB")

		tmpBase := t.TempDir()

		repoA := filepath.Join(tmpBase, "repo-a")
		repoB := filepath.Join(tmpBase, "repo-b")
		require.NoError(t, os.MkdirAll(repoA, 0o755))
		require.NoError(t, os.MkdirAll(repoB, 0o755))

		writeFile(t, filepath.Join(repoA, "main.go"),
			fmt.Sprintf("package main\n\nfunc %s() {}\n", funcNameA))
		writeFile(t, filepath.Join(repoB, "main.go"),
			fmt.Sprintf("package main\n\nfunc %s() {}\n", funcNameB))

		tmpCfg := filepath.Join(tmpBase, "config.yaml")
		gc := &config.GlobalConfig{
			Repos: []config.RepoEntry{
				{Path: repoA, Name: "repo-a"},
				{Path: repoB, Name: "repo-b"},
			},
		}
		gc.SetConfigPath(tmpCfg)
		require.NoError(t, gc.Save())

		cm, err := config.NewConfigManager(tmpCfg)
		require.NoError(t, err)

		g := graph.New()
		mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

		_, err = mi.IndexAll()
		require.NoError(t, err)

		// Record repo B's state before re-indexing A.
		repoBNodesBefore := len(g.GetRepoNodes("repo-b"))
		repoBEdgesBefore := countRepoEdges(g, "repo-b")
		// Capture repo B node IDs for identity check.
		repoBNodeIDsBefore := make(map[string]bool)
		for _, n := range g.GetRepoNodes("repo-b") {
			repoBNodeIDsBefore[n.ID] = true
		}

		// Re-index repo A.
		_, err = mi.IndexRepo("repo-a")
		require.NoError(t, err)

		// Verify repo B node count unchanged.
		repoBNodesAfter := len(g.GetRepoNodes("repo-b"))
		if repoBNodesBefore != repoBNodesAfter {
			rt.Errorf("repo-b node count changed: before=%d after=%d", repoBNodesBefore, repoBNodesAfter)
		}

		// Verify repo B edge count unchanged.
		repoBEdgesAfter := countRepoEdges(g, "repo-b")
		if repoBEdgesBefore != repoBEdgesAfter {
			rt.Errorf("repo-b edge count changed: before=%d after=%d", repoBEdgesBefore, repoBEdgesAfter)
		}

		// Verify repo B node IDs are exactly the same.
		repoBNodeIDsAfter := make(map[string]bool)
		for _, n := range g.GetRepoNodes("repo-b") {
			repoBNodeIDsAfter[n.ID] = true
		}
		for id := range repoBNodeIDsBefore {
			if !repoBNodeIDsAfter[id] {
				rt.Errorf("repo-b node %q disappeared after re-indexing repo-a", id)
			}
		}
		for id := range repoBNodeIDsAfter {
			if !repoBNodeIDsBefore[id] {
				rt.Errorf("repo-b node %q appeared after re-indexing repo-a", id)
			}
		}
	})
}

// countRepoEdges counts edges where at least one endpoint belongs to the given repo prefix.
func countRepoEdges(g graph.Store, repoPrefix string) int {
	prefix := repoPrefix + "/"
	count := 0
	for _, e := range g.AllEdges() {
		if strings.HasPrefix(e.From, prefix) || strings.HasPrefix(e.To, prefix) {
			count++
		}
	}
	return count
}

// --- Backward compatibility verification ---

// TestLoneRepo_ConfigCompat verifies that a workspace with one repo, or a
// `.gortex.yaml` carrying no repos/workspace sections, still loads and
// indexes cleanly.
//
// It no longer asserts an unprefixed ID format: a lone repo is the first
// tracked repo and is prefixed like any other. The shape assertion lives in
// TestPropertyNodeIDFormat, which now applies one invariant to every
// workspace size.
func TestLoneRepo_ConfigCompat(t *testing.T) {
	t.Run("node_ids_carry_the_repo_prefix", func(t *testing.T) {
		dir := setupRepoDir(t, "myrepo")

		tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
		gc := &config.GlobalConfig{
			Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}},
		}
		gc.SetConfigPath(tmpCfg)
		require.NoError(t, gc.Save())

		cm, err := config.NewConfigManager(tmpCfg)
		require.NoError(t, err)

		g := graph.New()
		mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

		_, err = mi.IndexAll()
		require.NoError(t, err)

		// One repo: IsMultiRepo reports the count, not the ID shape.
		assert.False(t, mi.IsMultiRepo())

		for _, n := range g.AllNodes() {
			assert.Equal(t, "myrepo", n.RepoPrefix, "a lone repo's nodes carry its prefix")
			assert.True(t, strings.HasPrefix(n.ID, "myrepo/"),
				"node ID %q must carry the repo prefix", n.ID)
		}
	})

	t.Run("existing_gortex_yaml_loads_without_errors", func(t *testing.T) {
		// Create a minimal .gortex.yaml without repos/workspace sections.
		tmpDir := t.TempDir()
		cfgContent := `index:
  exclude:
    - "vendor/**"
  workers: 4
watch:
  enabled: true
  debounce_ms: 200
guards:
  rules:
    - name: test-rule
      kind: co-change
      source: "internal/parser"
      target: "internal/parser/languages"
      message: "Parser changes require language extractor test updates"
`
		cfgPath := filepath.Join(tmpDir, ".gortex.yaml")
		require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

		cfg, err := config.Load(cfgPath)
		require.NoError(t, err)
		require.NotNil(t, cfg)

		// Existing fields should be loaded.
		assert.Equal(t, 4, cfg.Index.Workers)
		assert.True(t, cfg.Watch.Enabled)
		assert.Len(t, cfg.Guards.Rules, 1)
	})

	t.Run("no_global_config_required", func(t *testing.T) {
		// Loading a non-existent GlobalConfig should not error.
		gc, err := config.LoadGlobal("/nonexistent/path/config.yaml")
		require.NoError(t, err)
		require.NotNil(t, gc)
		assert.Empty(t, gc.Repos)
		assert.Empty(t, gc.Projects)
		assert.Empty(t, gc.ActiveProject)
	})

	t.Run("mcp_tools_work_without_repo_param", func(t *testing.T) {
		// In single-repo mode, the graph should be queryable without repo filtering.
		dir := setupRepoDir(t, "solo")

		tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
		gc := &config.GlobalConfig{
			Repos: []config.RepoEntry{{Path: dir, Name: "solo"}},
		}
		gc.SetConfigPath(tmpCfg)
		require.NoError(t, gc.Save())

		cm, err := config.NewConfigManager(tmpCfg)
		require.NoError(t, err)

		g := graph.New()
		mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

		_, err = mi.IndexAll()
		require.NoError(t, err)

		// Query engine should work without repo scoping.
		nodes := g.AllNodes()
		assert.Greater(t, len(nodes), 0, "should have indexed nodes")

		// All nodes should be accessible without repo prefix filtering.
		for _, n := range nodes {
			found := g.GetNode(n.ID)
			assert.NotNil(t, found, "node %q should be findable by ID", n.ID)
		}
	})
}

// --- Growing a workspace from one repo to two ---

// TestGrowWorkspaceFromOneRepoToTwo verifies that adding a second repo is not
// a migration: the first repo's node IDs are already qualified, so they are
// unchanged by the second repo joining, and a full re-index reproduces them.
//
// This used to be the single→multi TRANSITION test, and the interesting
// assertion was that repo-a's IDs changed shape at the moment repo-b arrived.
// Nothing changes shape any more; the point of the test is now that nothing
// does.
func TestGrowWorkspaceFromOneRepoToTwo(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")

	// Step 1: one tracked repo.
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: repoA, Name: "repo-a"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.IndexAll()
	require.NoError(t, err)

	// One repo, already fully qualified.
	assert.False(t, mi.IsMultiRepo())
	for _, n := range g.AllNodes() {
		assert.Equal(t, "repo-a", n.RepoPrefix)
		assert.True(t, strings.HasPrefix(n.ID, "repo-a/"), "node ID %q must carry the repo prefix", n.ID)
	}

	// Step 2: track a second repo.
	result, err := mi.TrackRepo(config.RepoEntry{Path: repoB, Name: "repo-b"})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.True(t, mi.IsMultiRepo())

	// repo-a's nodes are untouched by repo-b joining — no re-mint happened
	// because there was never a second ID shape to migrate away from.
	require.NotNil(t, g.GetNode("repo-a/main.go::Hello"))

	// Step 3: a full cold re-index reproduces the same IDs.
	gc2 := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	tmpCfg2 := filepath.Join(t.TempDir(), "config2.yaml")
	gc2.SetConfigPath(tmpCfg2)
	require.NoError(t, gc2.Save())

	cm2, err := config.NewConfigManager(tmpCfg2)
	require.NoError(t, err)

	g2 := graph.New()
	mi2 := NewMultiIndexer(g2, newTestRegistry(), search.NewNull(), cm2, zap.NewNop())

	results, err := mi2.IndexAll()
	require.NoError(t, err)
	require.Len(t, results, 2)

	// All nodes should now have Qualified_Node_IDs with repo prefix.
	assert.True(t, mi2.IsMultiRepo())
	for _, n := range g2.AllNodes() {
		assert.NotEmpty(t, n.RepoPrefix, "multi-repo node %q should have RepoPrefix", n.ID)
		prefix := n.RepoPrefix + "/"
		assert.True(t, strings.HasPrefix(n.ID, prefix),
			"multi-repo node ID %q should start with %q", n.ID, prefix)
	}
}

// TestScopedReindexPreservesRepoMetadataFileCount is the end-to-end
// guard on the bug that made `gortex daemon status` report an
// actively-edited repo's file count as the size of its last
// changed-file batch. IncrementalReindexRepo is the door the git
// watcher and the MCP reindex_repository tool both come through, and it
// full-replaces RepoMetadata from the scoped IndexResult — so a scoped
// pass must still carry the repo-wide count.
func TestScopedReindexPreservesRepoMetadataFileCount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "myrepo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	const total = 6
	for i := range total {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%d.go", i)),
			fmt.Sprintf("package main\n\nfunc F%d() {}\n", i))
	}

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}}}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	meta := mi.GetMetadata("myrepo")
	require.NotNil(t, meta)
	require.Equal(t, total, meta.FileCount, "baseline: the full pass counts the repo")

	// The watcher's door: one changed file, scoped.
	one := filepath.Join(dir, "f0.go")
	writeFile(t, one, "package main\n\nfunc F0() {}\n\nfunc F0Edited() {}\n")
	_, err = mi.IncrementalReindexRepo("myrepo", []string{one})
	require.NoError(t, err)

	meta = mi.GetMetadata("myrepo")
	require.NotNil(t, meta)
	assert.Equal(t, total, meta.FileCount,
		"a scoped reindex must not overwrite the repo's file count with its batch size")
}
