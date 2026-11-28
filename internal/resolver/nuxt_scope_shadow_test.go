package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

const nuxtGeneratedTarget = "app/composables/auth.ts::useRedirect"

func resolveNuxtPage(t *testing.T, page string) graph.Store {
	t.Helper()
	g := buildGraphFromSources(t, map[string]string{
		"app/composables/auth.ts": "export const useRedirect = () => 1",
		"other/auth.ts":           "export const useRedirect = () => 2",
		"app/page.js":             page,
	})
	r := New(g)
	r.SetNuxtAutoImportResolver(func(_, name string) (string, string) {
		if name != "useRedirect" {
			return "", ""
		}
		return "app/composables/auth", "useRedirect"
	})
	r.ResolveAll()
	return g
}

// Import evidence ignores module-scope names, but a generated global is not an
// import: any visible local value hides it.
func TestNuxtGeneratedBindingYieldsToModuleLocals(t *testing.T) {
	for name, page := range map[string]string{
		"const":          "const useRedirect = makeThing()\nexport function run() { return useRedirect() }",
		"destructured":   "const { useRedirect } = makeThing()\nexport function run() { return useRedirect() }",
		"exported const": "export const useRedirect = makeThing()\nexport function run() { return useRedirect() }",
		"hoisted var":    "if (flag) { var useRedirect = makeThing() }\nexport function run() { return useRedirect() }",
	} {
		t.Run(name, func(t *testing.T) {
			got := callEdgeTo(resolveNuxtPage(t, page), "app/page.js::run", 2)
			require.NotEmpty(t, got)
			require.NotEqual(t, nuxtGeneratedTarget, got)
		})
	}
}

func TestNuxtGeneratedBindingYieldsToRequireLocals(t *testing.T) {
	for name, page := range map[string]string{
		"module": "const { useRedirect } = require('../other/auth')\nexport function run() { return useRedirect() }",
		"lazy":   "// lazy\nexport function run() { const useRedirect = require('../other/auth').useRedirect; return useRedirect() }",
	} {
		t.Run(name, func(t *testing.T) {
			g := resolveNuxtPage(t, page)
			require.Len(t, callsInto(g, "other/auth.ts::useRedirect"), 1, "a require() local keeps its import evidence")
			require.Empty(t, callsInto(g, nuxtGeneratedTarget))
		})
	}
}

// Both calls collapse into one stored edge, which must stay eligible for the
// unshadowed call's generated binding.
func TestNuxtCollapsedRequireShadowKeepsGeneratedBinding(t *testing.T) {
	page := "// collapsed\nexport function run() { { const useRedirect = require('../other/auth').useRedirect; useRedirect() } useRedirect() }"
	require.Len(t, callsInto(resolveNuxtPage(t, page), nuxtGeneratedTarget), 1)
}

func TestNuxtModuleShadowRefusesCarriedBinding(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"app/composables/auth.ts": "export const useRedirect = () => 1",
		"app/page.js":             "const useRedirect = makeThing()\nexport function run() { return useRedirect() }",
	})
	call := unresolvedCall(t, g, "app/page.js::run", "useRedirect")
	r := New(g)
	r.SetPriorBindings(map[string]PriorBinding{
		PriorBindingKey(call.From, call.Kind, call.Line, call.To): {To: nuxtGeneratedTarget},
	})
	_, carried := r.priorBindingFor(call)
	require.False(t, carried)
}

func TestNuxtRemovedModuleShadowIsNotIncrementallySkipped(t *testing.T) {
	before := buildGraphFromSources(t, map[string]string{
		"app/page.js": "const useRedirect = makeThing()\nexport function run() { return useRedirect() }",
	})
	after := buildGraphFromSources(t, map[string]string{
		"app/page.js": "const other = makeThing()\nexport function run() { return useRedirect() }",
	})
	r := New(after)
	r.SetIncrementalSkip([]*graph.Edge{unresolvedCall(t, before, "app/page.js::run", "useRedirect")})
	require.False(t, r.incrementalSkipped(unresolvedCall(t, after, "app/page.js::run", "useRedirect")))
}

// The require() exception applies to scope stamps, never to the export list.
func TestRequireInitializedExportStaysExported(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"lib.js":  "export const work = require('./impl')",
		"page.ts": "import { work } from './lib.js'\nexport function go() { return work() }",
	})
	New(g).ResolveAll()
	require.Equal(t, "lib.js::work", callEdgeTo(g, "page.ts::go", 2))
}
