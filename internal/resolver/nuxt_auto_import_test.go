package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestNuxtAutoImportPrecedence(t *testing.T) {
	files := map[string]string{
		"app/composables/auth.ts": "export const useRedirect = () => 1",
		"other/auth.ts":           "export const useRedirect = () => 2",
		"app/explicit.ts":         "import { useRedirect } from '../other/auth'\nexport function run() { return useRedirect() }",
		"app/unresolved.ts":       "import { useRedirect } from 'missing-package'\nexport function run() { return useRedirect() }",
		"app/local.ts":            "export function useRedirect() { return 3 }\nexport function run() { return useRedirect() }",
	}
	g := buildGraphFromSources(t, files)
	r := New(g)
	r.SetNuxtAutoImportResolver(func(caller, name string) (string, string) { return "app/composables/auth", "useRedirect" })
	r.ResolveAll()
	require.Equal(t, "other/auth.ts::useRedirect", callEdgeTo(g, "app/explicit.ts::run", 2))
	require.Equal(t, "app/local.ts::useRedirect", callEdgeTo(g, "app/local.ts::run", 2))
	for _, edge := range g.GetOutEdges("app/unresolved.ts::run") {
		if edge.Kind == graph.EdgeCalls {
			require.NotEqual(t, "nuxt_auto_import", edge.Meta["resolution"])
		}
	}
}
