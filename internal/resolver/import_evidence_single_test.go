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
			call := findCallEdge(g, "app/pages/index.ts::run", "app/composables/auth.ts::useRedirect")
			require.NotNil(t, call)
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
	call := findCallEdge(g, "app/main.ts::run", "a/one.ts::createStore")
	require.NotNil(t, call)
	require.Equal(t, graph.OriginASTResolved, call.Origin)
	require.Equal(t, "import_binding", call.Meta["resolution"])
}

func TestImportEvidence_ImportNodeIsNotLocalShadow(t *testing.T) {
	// The module basename also names the imported callable.
	g := buildGraphFromSources(t, map[string]string{
		"app/composables/useRedirect.ts": "export const useRedirect = () => 1",
		"app/pages/index.ts":             "import { useRedirect } from '../composables/useRedirect'\nexport function run() { return useRedirect() }\n",
	})
	New(g).ResolveAll()
	call := findCallEdge(g, "app/pages/index.ts::run", "app/composables/useRedirect.ts::useRedirect")
	require.NotNil(t, call)
	require.Equal(t, graph.OriginASTResolved, call.Origin)
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
	require.Equal(t, "import_binding", call.Meta["resolution"])
}

func TestImportEvidence_ExportAliasDoesNotPromotePrivateDeclaration(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "const impl = () => 1; const work = impl as () => number; export { impl as work }",
		"b/two.ts":    "const impl = () => 2; export const work = impl as () => number; export const helper = 3",
		"app/main.ts": "import { work } from '../a/one'; import { helper } from '../b/two'; export function run() { return work() }",
	})
	New(g).ResolveAll()
	require.NotNil(t, findCallEdge(g, "app/main.ts::run", graph.UnresolvedMarker+"work"))
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
		pick, _ := r.pickImportEvidenceCallee("app/main.ts", "work", candidates)
		require.Same(t, candidates[0], pick)
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
	pick, _ := r.pickImportEvidenceCallee("app/main.ts", "work", candidates)
	require.Same(t, candidates[0], pick)

	g.RemoveEdge("barrel.ts", graph.UnresolvedMarker+"import::./a/one", graph.EdgeReExports)
	g.AddEdge(&graph.Edge{From: "barrel.ts", To: "b/two.ts", Kind: graph.EdgeReExports, FilePath: "barrel.ts"})
	_, err := indexes.refreshAfterInterleave(t.Context(), g.GetOutEdges("app/main.ts::run"), true)
	require.NoError(t, err)
	pick, _ = r.pickImportEvidenceCallee("app/main.ts", "work", candidates)
	require.Same(t, candidates[1], pick)
}
