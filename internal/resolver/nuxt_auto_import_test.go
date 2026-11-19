package resolver

import (
	"fmt"
	"strings"
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

func TestNuxtForwardedBindingsAndValueScope(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"lib/impl.ts":      "export function actual() { return 1 }",
		"lib/barrel.ts":    "export { actual as useAuth } from './impl'",
		"lib/anonymous.ts": "export default function () { return 2 }",
		"app/noise.ts":     "export function useAuth() {}\nexport const helper = 1\nexport function useAnonymous() {}",
		"app/explicit.ts":  "import { useAuth } from '../lib/barrel'\nimport { helper } from './noise'\nexport function run() { return useAuth() }",
		"app/generated.ts": "class Other { useAuth() {} }\ninterface useAuth {}\nexport function run() { return useAuth() }",
		"app/page.ts":      "export function run() { return useAnonymous() }",
	})
	r := New(g)
	r.SetNuxtAutoImportResolver(func(_, name string) (string, string) {
		switch name {
		case "useAuth":
			return "lib/barrel", "useAuth"
		case "useAnonymous":
			return "lib/anonymous", "default"
		}
		return "", ""
	})
	r.ResolveAll()
	for _, caller := range []string{"app/explicit.ts", "app/generated.ts"} {
		require.Equal(t, "lib/impl.ts::actual", callEdgeTo(g, caller+"::run", 3), caller)
	}
	require.Equal(t, "unresolved::useAnonymous", callEdgeTo(g, "app/page.ts::run", 1))
}

func TestExplicitImportPastBindingCapThroughStarBarrel(t *testing.T) {
	names := []string{"publicName as local"}
	for i := range 70 {
		names = append(names, fmt.Sprintf("n%d", i))
	}
	g := buildGraphFromSources(t, map[string]string{
		"lib/impl.ts":   "export function actual() {}",
		"lib/named.ts":  "export { actual as publicName } from './impl'",
		"lib/barrel.ts": "export * from './named'",
		"app/other.ts":  "export function local() {}",
		"app/page.ts":   "import { " + strings.Join(names, ",") + " } from '../lib/barrel'\nexport function run() { return local() }",
	})
	New(g).ResolveAll()
	require.Equal(t, "lib/impl.ts::actual", callEdgeTo(g, "app/page.ts::run", 2))
}
