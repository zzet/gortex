package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// setupRepoWithTestAndIface creates a repo with an interface, a type
// satisfying it, a production function, and a test function calling that
// production function. Used to assert EdgeImplements + EdgeTests get
// emitted by the global graph passes.
func setupRepoWithTestAndIface(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/"+name+"\n\ngo 1.22\n")
	writeFile(t, filepath.Join(dir, "main.go"), `package main

type Greeter interface {
	Greet() string
}

type EnglishGreeter struct{}

func (e EnglishGreeter) Greet() string { return "hello" }

func RunGreet(g Greeter) string { return g.Greet() }
`)
	writeFile(t, filepath.Join(dir, "main_test.go"), `package main

import "testing"

func TestRunGreet(t *testing.T) {
	got := RunGreet(EnglishGreeter{})
	if got != "hello" {
		t.Fatalf("want hello, got %q", got)
	}
}
`)
	return dir
}

func countEdges(g graph.Store, kind graph.EdgeKind) int {
	n := 0
	for _, e := range g.AllEdges() {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// TestMultiIndexer_IndexAll_GlobalPassesProduceEdges verifies the multi-
// repo orchestrator runs the graph-wide derivation passes (test edges,
// implements) once after the per-repo deferred loop, leaving the same
// edges in place that the old per-repo loop produced.
func TestMultiIndexer_IndexAll_GlobalPassesProduceEdges(t *testing.T) {
	repoA := setupRepoWithTestAndIface(t, "repo-a")
	repoB := setupRepoWithTestAndIface(t, "repo-b")

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
	require.Len(t, results, 2)

	// EdgeImplements: each repo has EnglishGreeter satisfying Greeter.
	// At least one per repo (InferImplements walks the full shared
	// graph; cross-repo matches via interface name only would multiply,
	// but the names are the same in both repos so only same-repo
	// satisfiers count via the identical method-set check).
	assert.Greater(t, countEdges(g, graph.EdgeImplements), 0,
		"expected EdgeImplements after IndexAll")

	// EdgeTests: TestRunGreet calls RunGreet — one per repo.
	assert.GreaterOrEqual(t, countEdges(g, graph.EdgeTests), 2,
		"expected at least one EdgeTests per repo after IndexAll")

	// Test-marker stamped on the test function nodes.
	testFuncs := 0
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if isTest, _ := n.Meta["is_test"].(bool); isTest {
			testFuncs++
		}
	}
	assert.GreaterOrEqual(t, testFuncs, 2, "is_test should be stamped on TestRunGreet in each repo")
}

// TestMultiIndexer_GlobalGraphPassPipeline_Idempotent verifies that running
// the live global-pass pipeline a second time does not mutate edge counts
// (graph dedup + resolver passes skip already-present edges).
func TestMultiIndexer_GlobalGraphPassPipeline_Idempotent(t *testing.T) {
	repoA := setupRepoWithTestAndIface(t, "repo-a")
	repoB := setupRepoWithTestAndIface(t, "repo-b")

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

	implsBefore := countEdges(g, graph.EdgeImplements)
	testsBefore := countEdges(g, graph.EdgeTests)
	overridesBefore := countEdges(g, graph.EdgeOverrides)
	require.Greater(t, implsBefore, 0)
	require.Greater(t, testsBefore, 0)

	// Re-run the global passes. None of the three should add duplicates.
	mi.runGlobalGraphPasses(context.Background(), nil, false)
	mi.runGlobalGraphPasses(context.Background(), nil, false)

	assert.Equal(t, implsBefore, countEdges(g, graph.EdgeImplements),
		"InferImplements re-emission should be idempotent")
	assert.Equal(t, overridesBefore, countEdges(g, graph.EdgeOverrides),
		"InferOverrides re-emission should be idempotent")
	assert.Equal(t, testsBefore, countEdges(g, graph.EdgeTests),
		"markTestSymbolsAndEmitEdges re-emission should be idempotent")
}

// TestMultiIndexer_BeginEndBatch_DefersGlobalPasses asserts that batch
// mode actually skips per-repo execution of the global passes and that
// EndBatch fills them in. Uses TrackRepoCtx (the warmup path), which is
// the loop where the O(R²) regression originated.
func TestMultiIndexer_BeginEndBatch_DefersGlobalPasses(t *testing.T) {
	repoA := setupRepoWithTestAndIface(t, "repo-a")
	repoB := setupRepoWithTestAndIface(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	mi.BeginBatch()

	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoA, Name: "repo-a"})
	require.NoError(t, err)
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoB, Name: "repo-b"})
	require.NoError(t, err)

	// Mid-batch: global passes should NOT have run, so EdgeTests and
	// EdgeImplements should both be absent.
	assert.Zero(t, countEdges(g, graph.EdgeTests),
		"EdgeTests should not be emitted while BeginBatch is active")
	assert.Zero(t, countEdges(g, graph.EdgeImplements),
		"EdgeImplements should not be emitted while BeginBatch is active")

	mi.EndBatch()

	assert.Greater(t, countEdges(g, graph.EdgeTests), 0,
		"EdgeTests should be emitted after EndBatch")
	assert.Greater(t, countEdges(g, graph.EdgeImplements), 0,
		"EdgeImplements should be emitted after EndBatch")
}

// TestMultiIndexer_TrackRepoCtx_NoBatch_RunsGlobalPassesInline asserts
// the default (non-batch) TrackRepoCtx path still runs the global
// derivation passes inline — ad-hoc single-track callers must not
// regress.
func TestMultiIndexer_TrackRepoCtx_NoBatch_RunsGlobalPassesInline(t *testing.T) {
	repoA := setupRepoWithTestAndIface(t, "repo-a")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoA, Name: "repo-a"})
	require.NoError(t, err)

	// Without BeginBatch, the inline path inside IndexCtx should have
	// emitted EdgeTests and EdgeImplements already.
	assert.Greater(t, countEdges(g, graph.EdgeTests), 0,
		"non-batched TrackRepoCtx must emit EdgeTests inline")
	assert.Greater(t, countEdges(g, graph.EdgeImplements), 0,
		"non-batched TrackRepoCtx must emit EdgeImplements inline")
}

// setupForeignRepoRetargetFrontier builds the state where the scoped batch
// projection and the frontier discard disagree: two repos cold-indexed,
// repo-b's test call restubbed with its projection evicted (repo-b itself
// unchanged), then rebound by repo-a's resolver. Per-repo resolvers are
// never SetScope'd, so repo-a's ResolveAll walks the whole shared graph and
// parks repo-b/main_test.go on repo-a's frontier — a caller a scoped
// {repo-a} projection will never walk (it reads only repo-a's calls edges).
func setupForeignRepoRetargetFrontier(t *testing.T) (*MultiIndexer, graph.Store) {
	t.Helper()
	repoA := filepath.Join(t.TempDir(), "repo-a")
	require.NoError(t, os.MkdirAll(repoA, 0o755))
	writeFile(t, filepath.Join(repoA, "go.mod"), "module example.com/repo-a\n\ngo 1.22\n")
	writeFile(t, filepath.Join(repoA, "main.go"), "package main\n\nfunc Alpha() {}\n")
	repoB := filepath.Join(t.TempDir(), "repo-b")
	require.NoError(t, os.MkdirAll(repoB, 0o755))
	writeFile(t, filepath.Join(repoB, "go.mod"), "module example.com/repo-b\n\ngo 1.22\n")
	writeFile(t, filepath.Join(repoB, "main.go"), "package main\n\nfunc Beta() {}\n")
	writeFile(t, filepath.Join(repoB, "main_test.go"),
		"package main\n\nimport \"testing\"\n\nfunc TestBeta(t *testing.T) { Beta() }\n")

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
	require.NotNil(t,
		findEdge(g, graph.EdgeTests, "repo-b/main_test.go::TestBeta", "repo-b/main.go::Beta"),
		"fixture: cold index must project repo-b's test call")

	// Restub: evict the projection and put the call back to unresolved —
	// the post-restub graph state, with repo-b untouched on disk.
	require.True(t, g.RemoveEdge("repo-b/main_test.go::TestBeta", "repo-b/main.go::Beta", graph.EdgeTests))
	require.True(t, g.RemoveEdge("repo-b/main_test.go::TestBeta", "repo-b/main.go::Beta", graph.EdgeCalls))
	g.AddEdge(&graph.Edge{From: "repo-b/main_test.go::TestBeta", To: graph.UnresolvedMarker + "Beta",
		Kind: graph.EdgeCalls, FilePath: "repo-b/main_test.go", Line: 5})

	// repo-a's resolver rebinds it — the same call indexCtxRaw and
	// RunDeferredPasses make.
	mi.indexers["repo-a"].resolver.ResolveAll()
	require.NotNil(t,
		findEdge(g, graph.EdgeCalls, "repo-b/main_test.go::TestBeta", "repo-b/main.go::Beta"),
		"fixture: repo-a's unscoped ResolveAll must rebind repo-b's call")
	return mi, g
}

// A scoped batch projects by the repo of the calls edge but discards
// per-indexer frontiers whole, so a repo-b caller parked on repo-a's
// frontier is neither projected nor carried to the next save. The
// projection must end up either emitted or still deferred on a frontier —
// never gone.
func TestScopedGlobalPassKeepsForeignRepoRetargetFrontier(t *testing.T) {
	mi, g := setupForeignRepoRetargetFrontier(t)
	mi.runGlobalGraphPasses(context.Background(), map[string]struct{}{"repo-a": {}}, false)
	if findEdge(g, graph.EdgeTests, "repo-b/main_test.go::TestBeta", "repo-b/main.go::Beta") != nil {
		return // projected outright — even better than deferred
	}
	for _, idx := range mi.indexers {
		for _, f := range idx.resolver.TakeRetargetedTestCallFiles() {
			if f == "repo-b/main_test.go" {
				return // deferred — the next save's scoped pass reconciles it
			}
		}
	}
	t.Fatalf("scoped batch permanently lost repo-b's test projection: " +
		"not projected, and no retarget frontier carries repo-b/main_test.go")
}

// The surviving discard, pinned: a full-coverage batch projection is
// graph-wide, so every per-repo frontier must be empty afterwards — the
// perf win of the cold-path discard, without the unsafe scoped case.
func TestFullCoverageGlobalPassDiscardsAllRetargetFrontiers(t *testing.T) {
	mi, g := setupForeignRepoRetargetFrontier(t)
	mi.runGlobalGraphPasses(context.Background(), nil, false)
	if findEdge(g, graph.EdgeTests, "repo-b/main_test.go::TestBeta", "repo-b/main.go::Beta") == nil {
		t.Fatalf("full-coverage batch must project the rebound test call")
	}
	for prefix, idx := range mi.indexers {
		if files := idx.resolver.TakeRetargetedTestCallFiles(); len(files) != 0 {
			t.Fatalf("full-coverage batch left %d file(s) parked on %s's frontier: %v",
				len(files), prefix, files)
		}
	}
}
