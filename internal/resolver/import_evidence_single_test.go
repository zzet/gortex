package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
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
			require.Equal(t, "import_closure", call.Meta["resolution"])
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
		"app/pages/index.ts": "import { useRedirect } from '../composables/useRedirect'\nexport function run() { return useRedirect() }\n",
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
