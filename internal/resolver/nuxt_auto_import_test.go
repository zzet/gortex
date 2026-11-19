package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNuxtAutoImportPrecedence(t *testing.T) {
	files := map[string]string{
		"app/composables/auth.ts": "export const useRedirect = () => 1",
		"other/auth.ts":           "export const useRedirect = () => 2; export const helper = 1",
		"app/unrelated.ts":        "import { helper } from '../other/auth'\nexport function run() { return useRedirect() }",
		"app/default.js":          "import useRedirect from 'missing-package'\nexport function run() { return useRedirect() }",
		"app/type.ts":             "import type { useRedirect } from 'missing-package'\nexport function run() { return useRedirect() }",
		"app/explicit.ts":         "import { useRedirect } from '../other/auth'\nexport function run() { return useRedirect() }",
		"app/unresolved.ts":       "import { useRedirect } from 'missing-package'\nexport function run() { return useRedirect() }",
		"app/local.ts":            "export function useRedirect() { return 3 }\nexport function run() { return useRedirect() }",
		"app/scopes.ts":           "function sibling(useRedirect: Function) { return useRedirect() }\nexport function run() { return useRedirect() }",
		"barrel/index.ts":         "export { useRedirect } from '../other/auth'",
		"app/barrel.ts":           "import { useRedirect } from '../barrel/index'\nimport { other } from '../app/composables/auth'\nexport function run() { return useRedirect() }",
	}
	g := buildGraphFromSources(t, files)
	r := New(g)
	r.SetNuxtAutoImportResolver(func(caller, name string) (string, string) { return "app/composables/auth", "useRedirect" })
	r.ResolveAll()
	require.Equal(t, "other/auth.ts::useRedirect", callEdgeTo(g, "app/explicit.ts::run", 2))
	require.Equal(t, "app/local.ts::useRedirect", callEdgeTo(g, "app/local.ts::run", 2))
	for range 2 {
		require.Equal(t, "app/composables/auth.ts::useRedirect", callEdgeTo(g, "app/unrelated.ts::run", 2))
		require.Equal(t, "app/composables/auth.ts::useRedirect", callEdgeTo(g, "app/scopes.ts::run", 2))
		require.Equal(t, "unresolved::useRedirect", callEdgeTo(g, "app/scopes.ts::sibling", 1))
		require.Equal(t, "other/auth.ts::useRedirect", callEdgeTo(g, "app/barrel.ts::run", 3))
		require.Equal(t, "app/composables/auth.ts::useRedirect", callEdgeTo(g, "app/type.ts::run", 2))
		for _, caller := range []string{"app/unresolved.ts::run", "app/default.js::run"} {
			require.Equal(t, "unresolved::useRedirect", callEdgeTo(g, caller, 2), caller)
		}
		r.ResolveAll() // import edges now point at external stubs
	}
}

func TestNuxtNamedDefaultImplementation(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"lib/impl.ts": "export default function actualDefault() { return 1 }",
		"app/page.ts": "export function run() { return useDefault() }",
	})
	r := New(g)
	r.SetNuxtAutoImportResolver(func(caller, name string) (string, string) { return "lib/impl", "default" })
	r.ResolveAll()
	require.Equal(t, "lib/impl.ts::actualDefault", callEdgeTo(g, "app/page.ts::run", 1))
}
