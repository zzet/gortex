package resolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
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

func TestExactExportSurface(t *testing.T) {
	names := []string{"actual as useIt"}
	for i := range 66 {
		names = append(names, fmt.Sprintf("x%d", i))
	}
	for _, tc := range []struct{ name, barrel, exported string }{
		{"private helper before star", "function useIt(){}; export * from './impl'", "useIt"},
		{"large named list", "export {" + strings.Join(names, ",") + "} from './impl'", "useIt"},
		{"local export alias", "import {actual} from './impl'; export {actual as useIt}", "useIt"},
		{"default imported value", "import {actual} from './impl'; export default actual", "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{"lib/impl.ts": "export function actual(){};export {actual as useIt}", "lib/barrel.ts": tc.barrel, "page.ts": "import {" + tc.exported + " as local} from './lib/barrel'\nexport function run(){local()}"})
			New(g).ResolveAll()
			require.Equal(t, "lib/impl.ts::actual", callEdgeTo(g, "page.ts::run", 2))
		})
	}
}
func TestNuxtLexicalClosure(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{"lib.ts": "export function useIt(){}", "page.ts": "function outer(useIt: Function){\n function inner(){useIt()}\n}\nfunction sibling(){function useIt(){}}\nexport function run(){useIt()}"})
	r := New(g)
	r.SetNuxtAutoImportResolver(func(_, name string) (string, string) { return "lib", name })
	r.ResolveAll()
	require.Equal(t, "unresolved::useIt", callEdgeTo(g, "page.ts::inner", 2))
	require.Equal(t, "lib.ts::useIt", callEdgeTo(g, "page.ts::run", 5))
}

func TestExactModuleScriptExports(t *testing.T) {
	for _, tc := range []struct{ module, body, binding, want string }{
		{"lib.cjs", "function run(){}; module.exports = run", "default", "lib.cjs::run"},
		{"lib.cjs", "function run(){}; exports.publicRun = run", "publicRun", "lib.cjs::run"},
		{"lib.cjs", "function run(){}; exports.old = run; module.exports = run", "old", "unresolved::local"},
		{"lib.cjs", "function real(){}; function other(){}; module.exports = real; module.exports.default = other", "default", "lib.cjs::real"},
		{"lib.cjs", "function run(){}; exports = {}; exports.run = run", "run", "unresolved::local"},
		{"lib.cjs", "function run(){}; var exports = {}; exports.run = run", "run", "unresolved::local"},
		{"lib.cjs", "function run(){}; module.exports = {}; exports = module.exports; exports.named = run", "named", "lib.cjs::run"},
		{"lib.cjs", "function run(){}; exports = module.exports = {}; exports.run = run", "run", "lib.cjs::run"},
		{"lib.cjs", "function run(){}; module.exports = exports = {}; exports.run = run", "run", "lib.cjs::run"},
		{"lib.cjs", "function run(){}; exports.__esModule = true; exports.default = run; module.exports = {}", "default", "unresolved::local"},
		{"lib.mjs", "function run(){}; let exports = {}; exports.named = run", "named", "unresolved::local"},
		{"lib.mjs", "function run(){}; const exports = {}; exports.named = run", "named", "unresolved::local"},
		{"lib.cjs", "function run(){}; exports.named = run; delete exports.named", "named", "unresolved::local"},
		{"lib.cjs", "function run(){}; module.exports = run; delete module.exports", "default", "unresolved::local"},
		{"lib.cjs", "function real(){}; function inner(){}; module.exports = real; module.exports.x = module.exports = inner", "x", "unresolved::local"},
		{"lib.cjs", "function run(){}; exports.__esModule = true; exports.default = run", "default", "lib.cjs::run"},
		{"lib.cjs", "function run(){}; var api = {}; module.exports = api; module.exports.__esModule = true; module.exports.default = run", "default", "lib.cjs::run"},
		{"lib.vue", "<script data-x='not setup here'>export {real as run} from './dep'</script>", "run", "dep.ts::real"},
		{"lib.vue", "<script lang='ts'>export function run(){}</script>\n<script setup>function hidden(){}</script>", "run", "lib.vue#script:1::run"},
		{"lib.vue", "<script setup>function hidden(){}</script>", "hidden", "unresolved::local"},
	} {
		t.Run(tc.module+"/"+tc.binding, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{tc.module: tc.body, "dep.ts": "export function real(){}", "page.ts": "import {" + tc.binding + " as local} from './" + tc.module + "'\nexport function go(){local()}"})
			New(g).ResolveAll()
			require.Equal(t, tc.want, callEdgeTo(g, "page.ts::go", 2))
		})
	}
}

func TestCompiledCommonJSDefault(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"lib.cjs":  "Object.defineProperty(exports, '__esModule', {value:true}); function actual(){}; exports.default = actual",
		"page.cts": "import def from './lib.cjs'; export function run(){def()}",
		"page.mjs": "import def from './lib.cjs'; export function run(){def()}",
	})
	New(g).ResolveAll()
	require.Equal(t, "lib.cjs::actual", callEdgeTo(g, "page.cts::run", 1))
	require.Equal(t, "unresolved::def", callEdgeTo(g, "page.mjs::run", 1))
}

func TestVueStarDoesNotImportPrivateFunction(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"lib.vue": "<script>function run(){}; export * from './dep'</script>",
		"dep.ts":  "export function run(){}",
		"page.ts": "import {run} from './lib.vue'; export function go(){run()}",
	})
	New(g).ResolveAll()
	require.Equal(t, "dep.ts::run", callEdgeTo(g, "page.ts::go", 1))
	var imports []string
	for _, edge := range g.GetOutEdges("page.ts") {
		if edge.Kind == graph.EdgeImports {
			imports = append(imports, edge.To)
		}
	}
	require.NotEmpty(t, imports)
	for _, target := range imports {
		require.Equal(t, "lib.vue", target)
	}
}

func TestVueTypeImportTargetsInterface(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"lib.vue": "<script lang=\"ts\">export interface Model { id: string }</script>",
		"page.ts": "import type {Model} from './lib.vue'; export function go(m: Model){}",
	})
	New(g).ResolveAll()
	var imports []string
	for _, edge := range g.GetOutEdges("page.ts") {
		if edge.Kind == graph.EdgeImports {
			imports = append(imports, edge.To)
		}
	}
	require.Contains(t, imports, "lib.vue#script:1::Model")
}

func TestStaticBlockVarShadowsImport(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"lib.ts":  "export function useIt(){}",
		"page.js": "import {useIt} from './lib';\nexport class Run {\n static { var useIt = globalThis.handler; useIt() }\n}",
	})
	New(g).ResolveAll()
	var targets []string
	for _, from := range []string{"page.js", "page.js::Run"} {
		if to := callEdgeTo(g, from, 3); to != "" {
			targets = append(targets, to)
		}
	}
	require.Equal(t, []string{"unresolved::useIt"}, targets)
}

func TestBodyVarRedeclaringParameter(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"page.js": "function callback(){}\nexport function run(callback){\n if(false){var callback;}\n callback()\n}",
	})
	New(g).ResolveAll()
	require.Equal(t, "unresolved::callback", callEdgeTo(g, "page.js::run", 4))
}

func TestBodyVarDoesNotShadowDefaultParameter(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"lib.ts":  "export function useIt(){}",
		"page.js": "import {useIt} from './lib';\nexport function run(x = useIt()) { if (x) { var useIt = x } }",
	})
	New(g).ResolveAll()
	require.Equal(t, "lib.ts::useIt", callEdgeTo(g, "page.js::run", 2))
}

func TestNuxtShadowingBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		line       int
	}{
		{"hoisted var", "export function go(x){if(x){var useIt=x}\nuseIt()}", 2},
		{"switch", "export function go(x){switch(x){case 0: const useIt=x;break;default:\nuseIt()}}", 2},
		{"named expression", "const other=function useIt(x){\nreturn useIt(x-1)}", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{"lib.ts": "export function useIt(){}", "page.js": tc.body + "\nexport function outside(){useIt()}"})
			r := New(g)
			r.SetNuxtAutoImportResolver(func(_, name string) (string, string) { return "lib", name })
			r.ResolveAll()
			var inner string
			for edge := range g.EdgesByKind(graph.EdgeCalls) {
				if edge.FilePath == "page.js" && edge.Line == tc.line {
					inner = edge.To
				}
			}
			require.NotEmpty(t, inner)
			require.NotEqual(t, "lib.ts::useIt", inner)
			if tc.name == "named expression" {
				require.Equal(t, "page.js::useIt", inner)
			}
			require.Equal(t, "lib.ts::useIt", callEdgeTo(g, "page.js::outside", tc.line+1))
		})
	}
}
