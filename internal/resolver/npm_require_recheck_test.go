package resolver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
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
		add := func(to string, line int, meta map[string]any) {
			call := &graph.Edge{From: "app/main.ts::run", To: to, Kind: graph.EdgeCalls, FilePath: "app/main.ts", Line: line, CrossRepo: true, Meta: meta}
			x := *call
			x.Kind = graph.EdgeCrossRepoCalls
			edges = append(edges, call, &x)
		}
		add("prov/index.ts::work", 1, map[string]any{
			graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: "p", NpmImportSpecifierMeta: "p",
		})
		for line := 10; line < 20; line++ {
			add("lib/util.ts::helper", line, nil) // ES-import cross-repo calls
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
	require.Equal(t, 11, full.sitesHydrated, "an unscoped re-check hydrates every JS/TS cross-repo call")
	require.Equal(t, 2, completed)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	run(cancelled, func() ([]string, func()) { return nil, func() { completed++ } })
	require.Equal(t, 2, completed, "a cancelled re-check is not complete")
}
