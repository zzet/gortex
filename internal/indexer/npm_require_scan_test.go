package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type hydrationCountingPager struct {
	*store_sqlite.Store
	hydrated int
}

func (p *hydrationCountingPager) ReadUnresolvedEdgePage(ctx context.Context, scan graph.UnresolvedEdgeScan, afterID int64, maxRows, maxBytes int) (graph.UnresolvedEdgePage, error) {
	page, err := p.Store.ReadUnresolvedEdgePage(ctx, scan, afterID, maxRows, maxBytes)
	p.hydrated += len(page.Edges)
	return page, err
}

// The require-call scan behind npm consumer discovery hydrates only unresolved
// call rows, and for a non-provider only the repository's own rows: other
// repositories' bare unresolved calls never load.
func TestUnresolvedRequireCallsHydratesOnlyScopedCallRows(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "require-scan.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	requireMeta := func(spec string) map[string]any {
		return map[string]any{graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: spec}
	}
	nodes := []*graph.Node{
		{ID: "a/x.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/x.js", RepoPrefix: "a"},
		{ID: "b/y.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "b/y.js", RepoPrefix: "b"},
	}
	edges := []*graph.Edge{
		{From: "a/x.js::run", To: "unresolved::work", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 1, Meta: requireMeta("pkg")},
		{From: "a/x.js::run", To: "unresolved::other", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 2},
		{From: "a/x.js::run", To: "unresolved::Type", Kind: graph.EdgeReferences, FilePath: "a/x.js", Line: 3},
		{From: "b/y.js::run", To: "unresolved::work", Kind: graph.EdgeCalls, FilePath: "b/y.js", Line: 1, Meta: requireMeta("pkg")},
	}
	for line := 10; line < 40; line++ {
		edges = append(edges, &graph.Edge{From: "b/y.js::run", To: "unresolved::noise", Kind: graph.EdgeCalls, FilePath: "b/y.js", Line: line})
	}
	store.AddBatch(nodes, edges)

	collect := func(p *hydrationCountingPager, repo string) []string {
		var files []string
		for e := range unresolvedRequireCalls(t.Context(), p, repo, nil) {
			files = append(files, e.FilePath)
		}
		return files
	}

	scoped := &hydrationCountingPager{Store: store}
	require.Equal(t, []string{"a/x.js"}, collect(scoped, "a"))
	require.Equal(t, 1, scoped.hydrated, "only repo a's require-bound unresolved call rows load")

	provider := &hydrationCountingPager{Store: store}
	require.ElementsMatch(t, []string{"a/x.js", "b/y.js"}, collect(provider, ""))
	require.Equal(t, 2, provider.hydrated, "a provider loads require-bound unresolved call rows only")
}

type failingUnresolvedPager struct {
	graph.Store
	beginErr error
	pageErr  error
}

func (f *failingUnresolvedPager) BeginUnresolvedEdgeScan(ctx context.Context) (graph.UnresolvedEdgeScan, error) {
	if f.beginErr != nil {
		return graph.UnresolvedEdgeScan{}, f.beginErr
	}
	return graph.UnresolvedEdgeScan{HighWaterID: 10}, nil
}

func (f *failingUnresolvedPager) ReadUnresolvedEdgePage(ctx context.Context, scan graph.UnresolvedEdgeScan, afterID int64, maxRows, maxBytes int) (graph.UnresolvedEdgePage, error) {
	if f.pageErr != nil {
		return graph.UnresolvedEdgePage{}, f.pageErr
	}
	return graph.UnresolvedEdgePage{Exhausted: true}, nil
}

func TestUnresolvedRequireCallsLogsAndHonorsContext(t *testing.T) {
	g := graph.New()
	g.AddEdge(&graph.Edge{
		From: "a/x.js::run", To: "unresolved::work", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 1,
		Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: "pkg"},
	})

	t.Run("begin error logs debug and nil logger does not panic", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		fp := &failingUnresolvedPager{Store: g, beginErr: errors.New("begin boom")}
		for range unresolvedRequireCalls(t.Context(), fp, "a", zap.New(core)) {
			t.Fatal("unexpected edge yielded on begin error")
		}
		require.NotEmpty(t, logs.All())
		for range unresolvedRequireCalls(t.Context(), fp, "a", nil) {
			t.Fatal("unexpected edge yielded with nil logger")
		}
	})

	t.Run("page error logs debug and nil logger does not panic", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		fp := &failingUnresolvedPager{Store: g, pageErr: errors.New("page boom")}
		for range unresolvedRequireCalls(t.Context(), fp, "a", zap.New(core)) {
			t.Fatal("unexpected edge yielded on page error")
		}
		require.NotEmpty(t, logs.All())
		for range unresolvedRequireCalls(t.Context(), fp, "a", nil) {
			t.Fatal("unexpected edge yielded with nil logger")
		}
	})

	t.Run("cancelled context stops both non-paging and paging scans", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for range unresolvedRequireCalls(ctx, g, "a", nil) {
			t.Fatal("non-paging store must honor cancelled ctx")
		}
		sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cancel.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = sq.Close() })
		sq.AddBatch([]*graph.Node{{ID: "a/x.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/x.js", RepoPrefix: "a"}}, []*graph.Edge{
			{From: "a/x.js::run", To: "unresolved::work", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 1, Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: "pkg"}},
		})
		for range unresolvedRequireCalls(ctx, sq, "a", nil) {
			t.Fatal("paging store must honor cancelled ctx")
		}
	})
}
