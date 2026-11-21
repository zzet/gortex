package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestImportEvidence_SingleCandidate(t *testing.T) {
	for _, declaration := range []string{
		"export function useRedirect() { return 1 }",
		"export const useRedirect = () => 1",
		"const impl = () => 1; export const useRedirect = impl as () => number",
	} {
		t.Run(declaration, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"app/composables/auth.ts": declaration,
				"app/pages/index.ts":      "import { useRedirect } from '../composables/auth'\nexport function run() { return useRedirect() }\n",
			})
			New(g).ResolveAll()
			var call *graph.Edge
			for _, edge := range g.GetOutEdges("app/pages/index.ts::run") {
				if edge.Kind == graph.EdgeCalls && edge.Line == 2 {
					call = edge
				}
			}
			require.NotNil(t, call)
			require.Equal(t, "app/composables/auth.ts::useRedirect", call.To)
			require.Equal(t, graph.OriginASTResolved, call.Origin)
			require.Equal(t, "import_binding", call.Meta["resolution"])
		})
	}
}

func TestImportEvidence_SymbolBindingBeatsFileClosure(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "const impl = () => 1; export const createStore = impl as () => number",
		"b/two.ts":    "const impl = () => 2; export const createStore = impl as () => number; export const helper = () => 3",
		"app/main.ts": "import { createStore } from '../a/one'\nimport { helper } from '../b/two'\nexport function run() { return createStore() }\n",
	})
	New(g).ResolveAll()
	require.Equal(t, "a/one.ts::createStore", callEdgeTo(g, "app/main.ts::run", 3))
}

func TestImportEvidence_ImportNodeIsNotLocalShadow(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"app/composables/useRedirect.ts": "export const useRedirect = () => 1",
		"app/pages/index.ts":             "import { useRedirect } from '../composables/useRedirect'\nexport function run() { return useRedirect() }\n",
	})
	New(g).ResolveAll()
	for _, edge := range g.GetOutEdges("app/pages/index.ts::run") {
		if edge.Kind == graph.EdgeCalls {
			require.Equal(t, "app/composables/useRedirect.ts::useRedirect", edge.To)
			require.Equal(t, graph.OriginASTResolved, edge.Origin)
			return
		}
	}
	t.Fatal("missing imported call")
}

func TestImportEvidence_RenamedImportOnSQLite(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.js":    "export function work() { return 1 }",
		"b/two.js":    "export function work() { return 2 }",
		"app/main.js": "import { work } from '../a/one'; import { work as spare } from '../b/two'; export function run() { return work() }",
	})
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	store.AddBatch(g.AllNodes(), g.AllEdges())
	New(store).ResolveAll()
	call := findCallEdge(store, "app/main.js::run", "a/one.js::work")
	require.NotNil(t, call)
	require.Equal(t, graph.OriginASTResolved, call.Origin)
}

// The import names the exported value, so it binds the declaration exported
// as `work`, never the private declaration that shares the name.
func TestImportEvidence_ExportAliasDoesNotPromotePrivateDeclaration(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "const impl = () => 1; const work = impl as () => number; export { impl as work }",
		"b/two.ts":    "const impl = () => 2; export const work = impl as () => number; export const helper = 3",
		"app/main.ts": "import { work } from '../a/one'; import { helper } from '../b/two'; export function run() { return work() }",
	})
	New(g).ResolveAll()
	require.Nil(t, findCallEdge(g, "app/main.ts::run", "a/one.ts::work"))
	require.Nil(t, findCallEdge(g, "app/main.ts::run", "b/two.ts::work"))
}

func TestImportEvidence_RepeatedCallsReuseTargetLookups(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"app/main.ts": "import { work } from '../a/one'",
	})
	store := &resolverBatchCountingStore{Store: g}
	r := New(store)
	candidates := []*graph.Node{g.GetNode("a/one.ts::work")}
	for i := range 2 {
		before := store.getNodeCalls
		require.Same(t, candidates[0], r.pickImportEvidenceCallee("app/main.ts", "work", candidates))
		if i > 0 {
			require.Equal(t, before, store.getNodeCalls, "repeat call re-expanded its import")
		}
	}
}

func TestImportEvidence_ForcedRefreshSeesBarrelEdit(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"b/two.ts":    "export function work() { return 2 }",
		"barrel.ts":   "export * from './a/one'",
		"app/main.ts": "import { work } from '../barrel'; export function run() { return work() }",
	})
	r := New(g)
	indexes := newResolveAllPassIndexes(r)
	defer indexes.close()
	candidates := []*graph.Node{g.GetNode("a/one.ts::work"), g.GetNode("b/two.ts::work")}
	require.Same(t, candidates[0], r.pickImportEvidenceCallee("app/main.ts", "work", candidates))

	g.RemoveEdge("barrel.ts", graph.UnresolvedMarker+"import::./a/one", graph.EdgeReExports)
	g.AddEdge(&graph.Edge{From: "barrel.ts", To: "b/two.ts", Kind: graph.EdgeReExports, FilePath: "barrel.ts"})
	_, err := indexes.refreshAfterInterleave(t.Context(), g.GetOutEdges("app/main.ts::run"), true)
	require.NoError(t, err)
	require.Same(t, candidates[1], r.pickImportEvidenceCallee("app/main.ts", "work", candidates))
}
