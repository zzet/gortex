package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

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
		for e := range unresolvedRequireCalls(p, repo) {
			files = append(files, e.FilePath)
		}
		return files
	}

	scoped := &hydrationCountingPager{Store: store}
	require.Equal(t, []string{"a/x.js"}, collect(scoped, "a"))
	require.Equal(t, 2, scoped.hydrated, "only repo a's unresolved call rows load")

	provider := &hydrationCountingPager{Store: store}
	require.ElementsMatch(t, []string{"a/x.js", "b/y.js"}, collect(provider, ""))
	require.Equal(t, 33, provider.hydrated, "a provider loads unresolved call rows only, never other kinds")
}
