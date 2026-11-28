package resolver

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The whole-graph require re-check hydrates only calls into the repositories
// its scope names, reports completion, and with no scope checks every JS/TS
// cross-repo call. A cancelled re-check is never reported complete.
func TestNpmRequireRecheckScopesHydration(t *testing.T) {
	build := func() graph.Store {
		g := graph.New()
		node := func(id, file, repo string, kind graph.NodeKind) *graph.Node {
			return &graph.Node{ID: id, Kind: kind, Name: id, FilePath: file, Language: "typescript", RepoPrefix: repo, WorkspaceID: "shared"}
		}
		nodes := []*graph.Node{
			node("app/main.ts", "app/main.ts", "app", graph.KindFile),
			node("app/main.ts::run", "app/main.ts", "app", graph.KindFunction),
			node("prov/index.ts::work", "prov/index.ts", "prov", graph.KindFunction),
			node("lib/util.ts::helper", "lib/util.ts", "lib", graph.KindFunction),
		}
		var edges []*graph.Edge
		add := func(to string, line int, origin string, meta map[string]any) {
			call := &graph.Edge{From: "app/main.ts::run", To: to, Kind: graph.EdgeCalls, FilePath: "app/main.ts", Line: line, CrossRepo: true, Origin: origin, Meta: meta}
			x := *call
			x.Kind = graph.EdgeCrossRepoCalls
			edges = append(edges, call, &x)
		}
		add("prov/index.ts::work", 1, graph.OriginASTResolved, map[string]any{
			graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: "p", NpmImportSpecifierMeta: "p",
		})
		for line := 10; line < 20; line++ {
			add("lib/util.ts::helper", line, "", nil) // ES-import cross-repo calls
		}
		g.AddBatch(nodes, edges)
		return g
	}
	run := func(ctx context.Context, recheck NpmRequireRecheck) *cancelAfterPagesStore {
		wrapped := &cancelAfterPagesStore{Store: build()}
		cr := NewCrossRepo(wrapped)
		cr.SetNpmPackageLookup(func(string) map[string]NpmPackageTarget { return nil })
		cr.SetNpmRequireRecheck(recheck)
		memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
		cr.restubStaleTrackedNpmRequireCallsAll(ctx, memo)
		return wrapped
	}

	completed := 0
	scoped := run(context.Background(), func() ([]string, func()) {
		return []string{"prov"}, func() { completed++ }
	})
	require.Equal(t, 1, scoped.sitesHydrated, "only the call into the package provider is hydrated")
	require.Equal(t, 1, completed)

	full := run(context.Background(), func() ([]string, func()) {
		return nil, func() { completed++ }
	})
	require.Equal(t, 1, full.sitesHydrated, "an unscoped re-check skips unstamped ES-import cross-repo calls and hydrates only ast/lsp-resolved candidates")
	require.Equal(t, 2, completed)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	run(cancelled, func() ([]string, func()) { return nil, func() { completed++ } })
	require.Equal(t, 2, completed, "a cancelled re-check is not complete")
}

type failingCheckedRecheckStore struct {
	graph.Store
	seqErr   error
	candErr  error
	seqCalls int
}

func (s *failingCheckedRecheckStore) EdgesLightSeqErr(ctx context.Context, kinds ...graph.EdgeKind) (iter.Seq[*graph.Edge], func() error) {
	s.seqCalls++
	if s.seqErr != nil {
		return func(func(*graph.Edge) bool) {}, func() error { return s.seqErr }
	}
	if checked, ok := s.Store.(graph.CheckedLightEdgeSequencer); ok {
		return checked.EdgesLightSeqErr(ctx, kinds...)
	}
	return graph.EdgesLightSeq(s.Store, kinds...), func() error { return nil }
}

func (s *failingCheckedRecheckStore) GetEdgeCandidatesErr(ctx context.Context, endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) (graph.EdgeCandidateSet, error) {
	if s.candErr != nil {
		return graph.EdgeCandidateSet{}, s.candErr
	}
	if checked, ok := s.Store.(graph.CheckedEdgeCandidateLookup); ok {
		return checked.GetEdgeCandidatesErr(ctx, endpoints, sites)
	}
	return s.GetEdgeCandidates(endpoints, sites), nil
}

func TestNpmRequireRecheckSkipsPlainESCrossRepoCalls(t *testing.T) {
	g := graph.New()
	nodes := []*graph.Node{
		{ID: "app/main.ts", Kind: graph.KindFile, Name: "main.ts", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app", WorkspaceID: "shared"},
		{ID: "app/main.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app", WorkspaceID: "shared"},
		{ID: "prov/index.ts::work", Kind: graph.KindFunction, Name: "work", FilePath: "prov/index.ts", Language: "typescript", RepoPrefix: "prov", WorkspaceID: "shared"},
		{ID: "lib/util.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "lib/util.ts", Language: "typescript", RepoPrefix: "lib", WorkspaceID: "shared"},
	}
	var edges []*graph.Edge
	for line := 1; line <= 25; line++ {
		call := &graph.Edge{From: "app/main.ts::run", To: "prov/index.ts::work", Kind: graph.EdgeCalls, FilePath: "app/main.ts", Line: line, CrossRepo: true}
		x := *call
		x.Kind = graph.EdgeCrossRepoCalls
		edges = append(edges, call, &x)
	}
	g.AddBatch(nodes, edges)

	// 1. Plain ES-import cross-repo calls (Origin == "") incur zero hydrations.
	wrapped := &cancelAfterPagesStore{Store: g}
	cr := NewCrossRepo(wrapped)
	cr.SetNpmPackageLookup(func(string) map[string]NpmPackageTarget { return nil })
	doneCalls := 0
	cr.SetNpmRequireRecheck(func() ([]string, func()) { return nil, func() { doneCalls++ } })
	memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
	cr.restubStaleTrackedNpmRequireCallsAll(t.Context(), memo)
	require.Zero(t, wrapped.sitesHydrated, "unstamped ES-import cross-repo calls must not be hydrated")
	require.Equal(t, 1, doneCalls)

	// 2. Empty non-nil targetRepos skips EdgesLightSeq completely and still marks done.
	counted := &failingCheckedRecheckStore{Store: g}
	cr2 := NewCrossRepo(counted)
	cr2.SetNpmPackageLookup(func(string) map[string]NpmPackageTarget { return nil })
	cr2.SetNpmRequireRecheck(func() ([]string, func()) { return []string{}, func() { doneCalls++ } })
	cr2.restubStaleTrackedNpmRequireCallsAll(t.Context(), memo)
	require.Zero(t, counted.seqCalls, "empty targetRepos must short-circuit before scanning EdgeCrossRepoCalls")
	require.Equal(t, 2, doneCalls)
}

func TestNpmRequireRecheckReadErrorDoesNotMarkDone(t *testing.T) {
	seed := func(store graph.Store) {
		call := &graph.Edge{
			From: "app/main.ts::run", To: "prov/index.ts::work", Kind: graph.EdgeCalls,
			FilePath: "app/main.ts", Line: 1, CrossRepo: true, Origin: graph.OriginASTResolved,
			Meta: map[string]any{
				graph.JSLexicalBindingMeta:     graph.JSLexicalRequire,
				graph.JSLexicalRequireSpecMeta: "p",
				NpmImportSpecifierMeta:         "p",
			},
		}
		x := *call
		x.Kind = graph.EdgeCrossRepoCalls
		store.AddBatch([]*graph.Node{
			{ID: "app/main.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app"},
			{ID: "prov/index.ts::work", Kind: graph.KindFunction, Name: "work", FilePath: "prov/index.ts", Language: "typescript", RepoPrefix: "prov"},
		}, []*graph.Edge{call, &x})
	}

	t.Run("EdgesLightSeqErr failure logs debug and never marks done", func(t *testing.T) {
		g := graph.New()
		seed(g)
		boom := errors.New("seq read failed")
		core, logs := observer.New(zap.DebugLevel)
		cr := NewCrossRepo(&failingCheckedRecheckStore{Store: g, seqErr: boom})
		cr.SetLogger(zap.New(core))
		cr.SetNpmPackageLookup(func(string) map[string]NpmPackageTarget { return nil })
		doneCalls := 0
		cr.SetNpmRequireRecheck(func() ([]string, func()) { return nil, func() { doneCalls++ } })
		memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
		cr.restubStaleTrackedNpmRequireCallsAll(t.Context(), memo)
		require.Zero(t, doneCalls)
		require.NotEmpty(t, logs.All())
	})

	t.Run("GetEdgeCandidatesErr failure with nil logger never panics or marks done", func(t *testing.T) {
		g := graph.New()
		seed(g)
		boom := errors.New("cand read failed")
		cr := NewCrossRepo(&failingCheckedRecheckStore{Store: g, candErr: boom})
		cr.SetLogger(nil)
		cr.SetNpmPackageLookup(func(string) map[string]NpmPackageTarget { return nil })
		doneCalls := 0
		cr.SetNpmRequireRecheck(func() ([]string, func()) { return nil, func() { doneCalls++ } })
		memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
		cr.restubStaleTrackedNpmRequireCallsAll(t.Context(), memo)
		require.Zero(t, doneCalls)
	})

	t.Run("SQLite BindAnalysisPages cancelled scan never marks done", func(t *testing.T) {
		sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "recheck-paged.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = sq.Close() })
		seed(sq)
		paged := sq.BindAnalysisPages()
		cr := NewCrossRepo(paged)
		cr.SetNpmPackageLookup(func(string) map[string]NpmPackageTarget { return nil })
		doneCalls := 0
		cr.SetNpmRequireRecheck(func() ([]string, func()) { return nil, func() { doneCalls++ } })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
		cr.restubStaleTrackedNpmRequireCallsAll(ctx, memo)
		require.Zero(t, doneCalls)
	})
}
