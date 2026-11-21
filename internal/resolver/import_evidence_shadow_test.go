package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func callsInto(g graph.Store, target string) []*graph.Edge {
	var calls []*graph.Edge
	for _, e := range g.GetInEdges(target) {
		if e.Kind == graph.EdgeCalls {
			calls = append(calls, e)
		}
	}
	return calls
}

func unresolvedCall(t *testing.T, g graph.Store, from, name string) *graph.Edge {
	t.Helper()
	call := findCallEdge(g, from, graph.UnresolvedMarker+name)
	require.NotNil(t, call, "%s should keep an unresolved call to %s", from, name)
	return call
}

// The same-directory layout matters: a shadowed call that only skipped the
// import tier would still reach the same-package tier and turn ast_resolved.
func TestImportEvidence_LexicalShadowNeverBindsImport(t *testing.T) {
	shadowed := map[string]string{
		"parameter":           "export function run(work) { return work() }",
		"arrow parameter":     "export const run = (work) => work()",
		"bare arrow":          "export const run = work => work()",
		"destructured":        "export function run({ work }) { return work() }",
		"renamed destructure": "export function run({ job: work }) { return work() }",
		"array pattern":       "export function run([work]) { return work() }",
		"rest parameter":      "export function run(...work) { return work() }",
		"default parameter":   "export function run(work = null) { return work() }",
		"method parameter":    "export class Box { run(work) { return work() } }",
		"outer parameter":     "export function run(work) { function inner() { return work() } return inner }",
		"callback closure":    "export function run(work) { return [1].map(() => work()) }",
		"callback parameter":  "export function run(items) { return items.map((work) => work()) }",
		"catch parameter":     "export function run() { try { return 1 } catch (work) { return work() } }",
		"for-of binding":      "export function run(items) { for (const work of items) { work() } }",
		"block let":           "export function run(get) { { let work = get(); return work() } }",
		"hoisted var":         "export function run(get, ok) { if (ok) { var work = get() } return work() }",
	}
	layouts := map[string]struct{ lib, caller, spec string }{
		"cross directory": {"a/one", "app/main", "../a/one"},
		"same directory":  {"app/one", "app/main", "./one"},
	}
	for name, body := range shadowed {
		for layoutName, layout := range layouts {
			for _, ext := range []string{".ts", ".js"} {
				t.Run(name+"/"+layoutName+"/"+ext, func(t *testing.T) {
					g := buildGraphFromSources(t, map[string]string{
						layout.lib + ext:    "export function work() { return 1 }",
						layout.caller + ext: "import { work } from '" + layout.spec + "'\n" + body + "\n",
					})
					New(g).ResolveAll()
					require.Empty(t, callsInto(g, layout.lib+ext+"::work"))
				})
			}
		}
	}
}

func TestImportEvidence_UnshadowedCallStillBindsImport(t *testing.T) {
	for name, body := range map[string]string{
		"plain call":                 "export function run() { return work() }",
		"sibling parameter":          "export function other(work) { return work }\nexport function run() { return work() }",
		"sibling callback parameter": "export function run(items) {\n  items.map((work) => work)\n  return work()\n}",
		"parameter initializer":      "export function run(value = work()) { return value }",
	} {
		t.Run(name, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"a/one.ts":    "export function work() { return 1 }",
				"app/main.ts": "import { work } from '../a/one'\n" + body + "\n",
			})
			New(g).ResolveAll()
			calls := callsInto(g, "a/one.ts::work")
			require.Len(t, calls, 1)
			require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
			require.Equal(t, "import_binding", calls[0].Meta["resolution"])
		})
	}
}

// A local initialized from a module loader names that module's export, so
// treating it as an opaque shadow would drop a real call.
func TestImportEvidence_ModuleLoaderLocalsKeepBinding(t *testing.T) {
	for name, src := range map[string]string{
		"module require":      "const { work } = require('../a/one')\nfunction run() { return work() }\nmodule.exports = { run }\n",
		"lazy require":        "function run() { const { work } = require('../a/one'); return work() }\nmodule.exports = { run }\n",
		"lazy require member": "function run() { const work = require('../a/one').work; return work() }\nmodule.exports = { run }\n",
	} {
		t.Run(name, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"a/one.js":    "export function work() { return 1 }",
				"app/main.js": src,
			})
			New(g).ResolveAll()
			require.Len(t, callsInto(g, "a/one.js::work"), 1)
		})
	}
}

func TestImportEvidence_BlockClassShadowsImportInTypeScript(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"app/main.ts": "import { work } from '../a/one'\nexport function run() { class work {} return work() }\n",
	})
	New(g).ResolveAll()
	require.Empty(t, callsInto(g, "a/one.ts::work"))
}

func TestImportEvidence_NestedHelperStaysLocal(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"app/main.ts": "import { work } from '../a/one'\nexport function run() { function work() { return 2 } return work() }\n",
	})
	New(g).ResolveAll()
	require.Empty(t, callsInto(g, "a/one.ts::work"))
	var local *graph.Edge
	for _, e := range g.GetOutEdges("app/main.ts::run") {
		if e.Kind == graph.EdgeCalls {
			local = e
		}
	}
	require.NotNil(t, local)
	callee := g.GetNode(local.To)
	require.NotNil(t, callee, "nested helper call should bind, got %s", local.To)
	require.Equal(t, "app/main.ts", callee.FilePath)
}

func TestImportEvidence_ShadowedAndRealCallsInOneFunction(t *testing.T) {
	for name, body := range map[string]struct {
		src  string
		line int
	}{
		"separate lines":  {"export function run(items) {\n  work()\n  return items.map((work) => work())\n}", 3},
		"real call first": {"export function run(items) { work(); return items.map((work) => work()) }", 2},
		"shadow first":    {"export function run(items) { items.map((work) => work()); return work() }", 2},
	} {
		t.Run(name, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"a/one.ts":    "export function work() { return 1 }",
				"app/main.ts": "import { work } from '../a/one'\n" + body.src + "\n",
			})
			New(g).ResolveAll()
			calls := callsInto(g, "a/one.ts::work")
			require.Len(t, calls, 1)
			require.Equal(t, body.line, calls[0].Line)
			require.Equal(t, "import_binding", calls[0].Meta["resolution"])
		})
	}
}

func TestImportEvidence_LexicalShadowSurvivesSQLite(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"app/main.ts": "import { work } from '../a/one'\nexport function run(work) { return work() }\n",
	})
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	store.AddBatch(g.AllNodes(), g.AllEdges())
	New(store).ResolveAll()
	require.Empty(t, callsInto(store, "a/one.ts::work"))
	require.Equal(t, graph.JSPendingParameter,
		unresolvedCall(t, store, "app/main.ts::run", "work").Meta[graph.JSLexicalBindingMeta])
}

// An edit that adds a parameter keeps the call's line and spelling, so the
// prior-binding key still matches the import the call used to reach.
func TestImportEvidence_ShadowedCallRefusesPriorBinding(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"app/main.ts": "import { work } from '../a/one'\nexport function run(work) { return work() }\n",
	})
	call := unresolvedCall(t, g, "app/main.ts::run", "work")
	r := New(g)
	r.SetPriorBindings(map[string]PriorBinding{
		PriorBindingKey(call.From, call.Kind, call.Line, call.To): {To: "a/one.ts::work"},
	})
	_, carried := r.priorBindingFor(call)
	require.False(t, carried)
}

// The reverse edit removes the parameter: the prior unresolved edge has the
// same shape as the fresh call, which must still reach the import tier.
func TestImportEvidence_RemovedShadowIsNotIncrementallySkipped(t *testing.T) {
	before := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"app/main.ts": "import { work } from '../a/one'\nexport function run(work) { return work() }\n",
	})
	after := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"app/main.ts": "import { work } from '../a/one'\nexport function run() { return work() }\n",
	})
	r := New(after)
	r.SetIncrementalSkip([]*graph.Edge{unresolvedCall(t, before, "app/main.ts::run", "work")})
	require.False(t, r.incrementalSkipped(unresolvedCall(t, after, "app/main.ts::run", "work")))
}

func TestImportEvidence_ShadowedCallSkipsReactConvention(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"src/hooks/useThing.ts": "export function useThing() { return 1 }",
		"src/app/main.tsx":      "export function Shadowed(useThing) { return useThing() }\nexport function Real() { return useThing() }\n",
	})
	require.Equal(t, 1, ResolveReactHooksContext(g))
	calls := callsInto(g, "src/hooks/useThing.ts::useThing")
	require.Len(t, calls, 1)
	require.Equal(t, "src/app/main.tsx::Real", calls[0].From)
}

func TestImportEvidence_ShadowedCallSkipsCrossRepoImport(t *testing.T) {
	for name, tc := range map[string]struct {
		meta map[string]any
		want string
	}{
		"shadowed":   {map[string]any{graph.JSLexicalBindingMeta: graph.JSPendingParameter}, graph.UnresolvedMarker + "work"},
		"unshadowed": {nil, "lib/one.ts::work"},
	} {
		t.Run(name, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "app/main.ts", Kind: graph.KindFile, Name: "main.ts", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app"})
			g.AddNode(&graph.Node{ID: "app/main.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app"})
			g.AddNode(&graph.Node{ID: "lib/one.ts::work", Kind: graph.KindFunction, Name: "work", FilePath: "lib/one.ts", Language: "typescript", RepoPrefix: "lib"})
			g.AddEdge(&graph.Edge{From: "app/main.ts", To: "lib/one.ts::work", Kind: graph.EdgeImports, FilePath: "app/main.ts", Line: 1, CrossRepo: true})
			call := &graph.Edge{From: "app/main.ts::run", To: graph.UnresolvedMarker + "work", Kind: graph.EdgeCalls, FilePath: "app/main.ts", Line: 2, Meta: tc.meta}
			g.AddEdge(call)
			NewCrossRepo(g).ResolveAll()
			require.NotNil(t, findCallEdge(g, "app/main.ts::run", tc.want))
		})
	}
}
