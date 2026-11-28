package resolver

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
)

// extractRawSources runs the real JS/TS extractors without pre-deduplicating
// edges through an in-memory graph, so SQLite INSERT OR IGNORE sees the exact
// raw edge stream emitted by extraction.
func extractRawSources(t *testing.T, files map[string]string) ([]*graph.Node, []*graph.Edge) {
	t.Helper()
	ts := languages.NewTypeScriptExtractor()
	js := languages.NewJavaScriptExtractor()
	var nodes []*graph.Node
	var edges []*graph.Edge
	for path, src := range files {
		if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx") || strings.HasSuffix(path, ".mts") || strings.HasSuffix(path, ".cts") {
			r, err := ts.Extract(path, []byte(src))
			require.NoError(t, err, "ts extract %s", path)
			nodes = append(nodes, r.Nodes...)
			edges = append(edges, r.Edges...)
			continue
		}
		r, err := js.Extract(path, []byte(src))
		require.NoError(t, err, "js extract %s", path)
		nodes = append(nodes, r.Nodes...)
		edges = append(edges, r.Edges...)
	}
	return nodes, edges
}

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
		"parameter":                     "export function run(work) { return work() }",
		"arrow parameter":               "export const run = (work) => work()",
		"bare arrow":                    "export const run = work => work()",
		"destructured":                  "export function run({ work }) { return work() }",
		"renamed destructure":           "export function run({ job: work }) { return work() }",
		"array pattern":                 "export function run([work]) { return work() }",
		"rest parameter":                "export function run(...work) { return work() }",
		"default parameter":             "export function run(work = null) { return work() }",
		"method parameter":              "export class Box { run(work) { return work() } }",
		"outer parameter":               "export function run(work) { function inner() { return work() } return inner }",
		"callback closure":              "export function run(work) { return [1].map(() => work()) }",
		"callback parameter":            "export function run(items) { return items.map((work) => work()) }",
		"catch parameter":               "export function run() { try { return 1 } catch (work) { return work() } }",
		"for-of binding":                "export function run(items) { for (const work of items) { work() } }",
		"block let":                     "export function run(get) { { let work = get(); return work() } }",
		"hoisted var":                   "export function run(get, ok) { if (ok) { var work = get() } return work() }",
		"require param zero-arg":        "export function run(require) { const work = require(); return work() }",
		"require param with spec":       "export function run(require) { const work = require('$SPEC'); return work() }",
		"require param destructured":    "export function run(require) { const { work } = require('$SPEC'); return work() }",
		"require param member":          "export function run(require) { const work = require('$SPEC').work; return work() }",
		"require arrow param":           "export const run = require => { const work = require('$SPEC'); return work() }",
		"require outer closure param":   "export function outer(require) { return function run() { const { work } = require('$SPEC'); return work() } }",
		"require local const":           "export function run(fn) { const require = fn; const { work } = require('$SPEC'); return work() }",
		"require hoisted var":           "export function run(fn, ok) { const { work } = require('$SPEC'); if (ok) { var require = fn } return work() }",
		"require catch param":           "export function run() { try { return 1 } catch (require) { const work = require('$SPEC'); return work() } }",
		"require for-of binding":        "export function run(xs) { for (const require of xs) { const work = require('$SPEC'); return work() } }",
		"require module function":       "function require() { return () => 2 }\nexport function run() { const work = require('$SPEC'); return work() }",
		"require zero-arg":              "export function run() { const work = require(); return work() }",
		"require dynamic arg":           "export function run(id) { const work = require(id); return work() }",
		"require extra arg":             "export function run() { const work = require('$SPEC', true); return work() }",
		"require renamed destructure":   "export function run() { const { other: work } = require('$SPEC'); return work() }",
		"require mismatched member":     "export function run() { const work = require('$SPEC').other; return work() }",
		"require chained member":        "export function run() { const work = require('$SPEC').sub.work; return work() }",
		"require invoked result":        "export function run() { const work = require('$SPEC')(); return work() }",
		"require external package":      "export function run() { const work = require('external-pkg'); return work() }",
		"require conflicting var":       "export function run(get, ok) { if (ok) { var { work } = require('$SPEC') } else { var work = get() } return work() }",
		"require conflicting var rev":   "export function run(get, ok) { if (ok) { var work = get() } else { var { work } = require('$SPEC') } return work() }",
		"require conflicting var specs": "export function run(ok) { if (ok) { var { work } = require('./other') } else { var { work } = require('$SPEC') } return work() }",
		"require named class expr":      "export const Box = class require { run() { const work = require('$SPEC'); return work() } }",
	}
	layouts := map[string]struct{ lib, caller, spec string }{
		"cross directory": {"a/one", "app/main", "../a/one"},
		"same directory":  {"app/one", "app/main", "./one"},
	}
	for name, body := range shadowed {
		for layoutName, layout := range layouts {
			for _, ext := range []string{".ts", ".js"} {
				t.Run(name+"/"+layoutName+"/"+ext, func(t *testing.T) {
					src := strings.ReplaceAll(body, "$SPEC", layout.spec)
					g := buildGraphFromSources(t, map[string]string{
						layout.lib + ext:    "export function work() { return 1 }",
						layout.caller + ext: "import { work } from '" + layout.spec + "'\n" + src + "\n",
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
		"module require":             "const { work } = require('../a/one')\nfunction run() { return work() }\nmodule.exports = { run }\n",
		"lazy require":               "function run() { const { work } = require('../a/one'); return work() }\nmodule.exports = { run }\n",
		"lazy require default value": "function run() { const { work = null } = require('../a/one'); return work() }\nmodule.exports = { run }\n",
		"lazy require pair same":     "function run() { const { work: work } = require('../a/one'); return work() }\nmodule.exports = { run }\n",
		"lazy require member":        "function run() { const work = require('../a/one').work; return work() }\nmodule.exports = { run }\n",
		"lazy require direct":        "function run() { const work = require('../a/one'); return work() }\nmodule.exports = { run }\n",
		"lazy require commented arg": "function run() { const { work } = require(/* lazy */ '../a/one'); return work() }\nmodule.exports = { run }\n",
		"inner block over param":     "function run(work) { { const { work } = require('../a/one'); return work() } }\nmodule.exports = { run }\n",
		"same line shadow first":     "function run(items) { const { work } = require('../a/one'); items.map((work) => work()); return work() }\nmodule.exports = { run }\n",
		"same line require first":    "function run(items) { const { work } = require('../a/one'); work(); return items.map((work) => work()) }\nmodule.exports = { run }\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				"a/one.js":    "export function work() { return 1 }",
				"app/main.js": src,
			}
			for _, backend := range []string{"memory", "sqlite"} {
				t.Run(backend, func(t *testing.T) {
					var store graph.Store
					if backend == "sqlite" {
						rawNodes, rawEdges := extractRawSources(t, files)
						sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						defer sq.Close()
						sq.AddBatch(rawNodes, rawEdges)
						store = sq
					} else {
						store = buildGraphFromSources(t, files)
					}
					New(store).ResolveAll()
					calls := callsInto(store, "a/one.js::work")
					require.Len(t, calls, 1)
					require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
				})
			}
		})
	}
}

func TestImportEvidence_ImportedRequireShadowsLoader(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		for name, imp := range map[string]string{
			"named import":     "import { require } from './loader'",
			"aliased import":   "import { load as require } from './loader'",
			"default import":   "import require from './loader'",
			"namespace import": "import * as require from './loader'",
		} {
			t.Run(name+"/"+ext, func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"a/one" + ext:      "export function work() { return 1 }",
					"app/loader" + ext: "export function require() { return () => 2 }\nexport function load() { return () => 2 }\nexport default require\n",
					"app/main" + ext:   "import { work } from '../a/one'\n" + imp + "\nexport function run() { const { work } = require('../a/one'); return work() }\n",
				})
				New(g).ResolveAll()
				require.Empty(t, callsInto(g, "a/one"+ext+"::work"))
			})
		}
	}
}

func TestImportEvidence_LocalRequireBindsOwnModuleNotFileImport(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		t.Run("distinct required module/"+ext, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"a/one" + ext:    "export function work() { return 1 }",
				"b/two" + ext:    "export function work() { return 2 }",
				"app/main" + ext: "import { work } from '../a/one'\nexport function run() { const { work } = require('../b/two'); return work() }\n",
			})
			New(g).ResolveAll()
			require.Empty(t, callsInto(g, "a/one"+ext+"::work"))
			calls := callsInto(g, "b/two"+ext+"::work")
			require.Len(t, calls, 1)
			require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
			require.Equal(t, "import_closure", calls[0].Meta["resolution"])
		})

		t.Run("required module lacks symbol/"+ext, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"a/one" + ext:    "export function work() { return 1 }",
				"b/two" + ext:    "export function other() { return 2 }",
				"app/main" + ext: "import { work } from '../a/one'\nexport function run() { const work = require('../b/two'); return work() }\n",
			})
			New(g).ResolveAll()
			require.Empty(t, callsInto(g, "a/one"+ext+"::work"))
			require.NotNil(t, findCallEdge(g, "app/main"+ext+"::run", graph.UnresolvedMarker+"work"))
		})

		t.Run("required barrel via SQLite/"+ext, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"a/one" + ext:    "export function work() { return 1 }",
				"b/real" + ext:   "export function work() { return 2 }",
				"b/index" + ext:  "export * from './real'",
				"app/main" + ext: "import { work } from '../a/one'\nexport function run() { const { work } = require('../b/index'); return work() }\n",
			})
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			defer store.Close()
			store.AddBatch(g.AllNodes(), g.AllEdges())
			New(store).ResolveAll()
			require.Empty(t, callsInto(store, "a/one"+ext+"::work"))
			calls := callsInto(store, "b/real"+ext+"::work")
			require.Len(t, calls, 1)
			require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
		})
	}

	t.Run("typescript ambient declare require stays eligible", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"a/one.ts":    "export function work() { return 1 }",
			"app/main.ts": "declare function require(id: string): any\nexport function run() { const { work } = require('../a/one'); return work() }\n",
		})
		New(g).ResolveAll()
		require.Len(t, callsInto(g, "a/one.ts::work"), 1)
	})
}

func TestImportEvidence_ShadowedRequireEmitsNoImportEdge(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"a/one.js":    "export function work() { return 1 }",
		"app/main.js": "function fake(require) { return require('../a/one') }\nexport function real() { return work() }\n",
	})
	for _, e := range g.GetOutEdges("app/main.js") {
		if e.Kind == graph.EdgeImports {
			t.Fatalf("shadowed require emitted unexpected import edge to %q", e.To)
		}
	}
	New(g).ResolveAll()
	for _, e := range callsInto(g, "a/one.js::work") {
		require.NotEqual(t, graph.OriginASTResolved, e.Origin)
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

func TestImportEvidence_AddedRequireIsNotIncrementallySkipped(t *testing.T) {
	before := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"app/main.ts": "export function run() { return work() }\n",
	})
	after := buildGraphFromSources(t, map[string]string{
		"a/one.ts":    "export function work() { return 1 }",
		"app/main.ts": "export function run() { const { work } = require('../a/one'); return work() }\n",
	})
	r := New(after)
	r.SetIncrementalSkip([]*graph.Edge{unresolvedCall(t, before, "app/main.ts::run", "work")})
	require.False(t, r.incrementalSkipped(unresolvedCall(t, after, "app/main.ts::run", "work")))
}

func TestImportEvidence_LocalRequireSelfAndCircularBarrel(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		t.Run("self require/"+ext, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"src/a" + ext: "export function helper() { return 1 }\nexport function run() { const { helper } = require('./a'); return helper() }\n",
			})
			New(g).ResolveAll()
			calls := callsInto(g, "src/a"+ext+"::helper")
			require.Len(t, calls, 1)
			require.Equal(t, "src/a"+ext+"::run", calls[0].From)
		})
		t.Run("circular barrel require/"+ext, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"src/a" + ext:     "export function helper() { return 1 }\nexport function run() { const { helper } = require('./index'); return helper() }\n",
				"src/index" + ext: "export * from './a'\n",
			})
			New(g).ResolveAll()
			calls := callsInto(g, "src/a"+ext+"::helper")
			require.Len(t, calls, 1)
			require.Equal(t, "src/a"+ext+"::run", calls[0].From)
		})
	}
}

func TestImportEvidence_LocalRequirePrefersDirectBarrelSymbol(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		t.Run("direct export beats re-exported private helper/"+ext, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"lib/other" + ext:  "function init() { return 0 }\nexport function other() { return init() }\n",
				"lib/barrel" + ext: "export function init() { return 1 }\nexport { other } from './other'\n",
				"app/caller" + ext: "export function run() { const { init } = require('../lib/barrel'); return init() }\n",
			})
			New(g).ResolveAll()
			calls := callsInto(g, "lib/barrel"+ext+"::init")
			require.Len(t, calls, 1)
			require.Equal(t, "app/caller"+ext+"::run", calls[0].From)
			require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
		})

		t.Run("barrel private helper does not shadow export star/"+ext, func(t *testing.T) {
			files := map[string]string{
				"lib/impl" + ext:   "export function init() { return 1 }\n",
				"lib/barrel" + ext: "function init() { return 0 }\nexport * from './impl'\n",
				"app/caller" + ext: "export function run() { const { init } = require('../lib/barrel'); return init() }\n",
			}
			g := buildGraphFromSources(t, files)
			New(g).ResolveAll()
			require.Empty(t, callsInto(g, "lib/barrel"+ext+"::init"))
			calls := callsInto(g, "lib/impl"+ext+"::init")
			require.Len(t, calls, 1)
			require.Equal(t, "app/caller"+ext+"::run", calls[0].From)
			require.Equal(t, graph.OriginASTResolved, calls[0].Origin)

			// CrossRepoResolver must apply the same rule across repos.
			xg := buildGraphFromSources(t, map[string]string{
				"lib/pkg/impl" + ext:   "export function init() { return 1 }\n",
				"lib/pkg/index" + ext:  "function init() { return 0 }\nexport * from './impl'\n",
				"app/web/caller" + ext: "export function run() { const { init } = require('lib/pkg'); return init() }\n",
			})
			for _, n := range xg.AllNodes() {
				if strings.HasPrefix(n.FilePath, "lib/") {
					n.RepoPrefix = "lib"
				} else {
					n.RepoPrefix = "app"
				}
			}
			New(xg).ResolveAll()
			NewCrossRepo(xg).ResolveAll()
			require.Empty(t, callsInto(xg, "lib/pkg/index"+ext+"::init"))
			xcalls := callsInto(xg, "lib/pkg/impl"+ext+"::init")
			require.Len(t, xcalls, 1)
			require.Equal(t, "app/web/caller"+ext+"::run", xcalls[0].From)
			require.True(t, xcalls[0].CrossRepo)
		})
		t.Run("separate-clause ESM export beats re-exports/"+ext, func(t *testing.T) {
			cases := map[string]map[string]string{
				"export star": {
					"lib/impl" + ext:   "export function init() { return 2 }\n",
					"lib/barrel" + ext: "function init() { return 1 }\nexport { init }\nexport * from './impl'\n",
					"app/caller" + ext: "export function run() { const { init } = require('../lib/barrel'); return init() }\n",
				},
				"re-export other with exported init": {
					"lib/other" + ext:  "export function init() { return 2 }\nexport function other() { return init() }\n",
					"lib/barrel" + ext: "function init() { return 1 }\nexport { init }\nexport { other } from './other'\n",
					"app/caller" + ext: "export function run() { const { init } = require('../lib/barrel'); return init() }\n",
				},
				"re-export other with private init": {
					"lib/other" + ext:  "function init() { return 2 }\nexport function other() { return init() }\n",
					"lib/barrel" + ext: "function init() { return 1 }\nexport { init }\nexport { other } from './other'\n",
					"app/caller" + ext: "export function run() { const { init } = require('../lib/barrel'); return init() }\n",
				},
			}
			for name, files := range cases {
				t.Run(name, func(t *testing.T) {
					g := buildGraphFromSources(t, files)
					New(g).ResolveAll()
					calls := callsInto(g, "lib/barrel"+ext+"::init")
					require.Len(t, calls, 1)
					require.Equal(t, "app/caller"+ext+"::run", calls[0].From)
					require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
				})
			}

			// Renamed exports (identifier, JS default keyword, and string aliases)
			// must NOT mark a private `init` as js_named_export.
			renamedCases := map[string]string{
				"export other as init":   "function other() { return 3 }\nfunction init() { return 1 }\nexport { other as init }\nexport * from './impl'\n",
				"export init as string":  "function init() { return 1 }\nexport { init as \"other\" }\nexport * from './impl'\n",
				"export init as str def": "function init() { return 1 }\nexport { init as \"default\" }\nexport * from './impl'\n",
				"export init as default": "function init() { return 1 }\nexport { init as default }\nexport * from './impl'\n",
			}
			for name, barrelSrc := range renamedCases {
				exts := []string{ext}
				if ext == ".ts" {
					exts = []string{".ts", ".tsx"}
				}
				for _, bx := range exts {
					t.Run(name+"/"+bx, func(t *testing.T) {
						g := buildGraphFromSources(t, map[string]string{
							"lib/impl" + ext:  "export function init() { return 2 }\n",
							"lib/barrel" + bx: barrelSrc,
							"app/caller" + bx: "export function run() { const { init } = require('../lib/barrel'); return init() }\n",
						})
						New(g).ResolveAll()
						require.Empty(t, callsInto(g, "lib/barrel"+bx+"::init"))
						require.Len(t, callsInto(g, "lib/impl"+ext+"::init"), 1)
					})
				}
			}

			// String alias matching the original binding name (`export { init as "init" }`)
			// IS a valid unrenamed named export.
			t.Run("export init as same string", func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"lib/impl" + ext:   "export function init() { return 2 }\n",
					"lib/barrel" + ext: "function init() { return 1 }\nexport { init as \"init\" }\nexport * from './impl'\n",
					"app/caller" + ext: "export function run() { const { init } = require('../lib/barrel'); return init() }\n",
				})
				New(g).ResolveAll()
				require.Len(t, callsInto(g, "lib/barrel"+ext+"::init"), 1)
				require.Empty(t, callsInto(g, "lib/impl"+ext+"::init"))
			})

			// Non-top-level declarations (nested functions, nested arrows, named
			// function expressions, TS namespace functions) must NOT be stamped
			// js_named_export by a local `export { init }` clause.
			nonTopCases := map[string]string{
				"nested function":     "export function setup() { function init() { return 1 }; return init() }\nexport { init }\nexport * from './impl'\n",
				"nested arrow":        "export function setup() { const init = () => 1; return init() }\nexport { init }\nexport * from './impl'\n",
				"named function expr": "export const wrap = function init() { return 1 }\nexport { init }\nexport * from './impl'\n",
			}
			if ext == ".ts" {
				nonTopCases["ts namespace function"] = "namespace Internal { export function init() { return 1 } }\nexport { init }\nexport * from './impl'\n"
			}
			for name, barrelSrc := range nonTopCases {
				t.Run("non-top-level/"+name, func(t *testing.T) {
					g := buildGraphFromSources(t, map[string]string{
						"lib/impl" + ext:   "export function init() { return 2 }\n",
						"lib/barrel" + ext: barrelSrc,
						"app/caller" + ext: "export function run() { const { init } = require('../lib/barrel'); return init() }\n",
					})
					if node := g.GetNode("lib/barrel" + ext + "::init"); node != nil {
						require.NotEqual(t, true, node.Meta["js_named_export"])
					}
					New(g).ResolveAll()
					for _, c := range callsInto(g, "lib/barrel"+ext+"::init") {
						require.NotEqual(t, "app/caller"+ext+"::run", c.From, "caller must not bind non-top-level %s", name)
					}
					implCalls := callsInto(g, "lib/impl"+ext+"::init")
					require.Len(t, implCalls, 1)
					require.Equal(t, "app/caller"+ext+"::run", implCalls[0].From)
				})
			}
		})
	}

	t.Run("pure CJS module without js_named_export still binds", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"lib/cjs.js":    "function init() { return 1 }\nmodule.exports = { init }\n",
			"app/caller.js": "function run() { const { init } = require('../lib/cjs'); return init() }\nmodule.exports = { run }\n",
		})
		New(g).ResolveAll()
		calls := callsInto(g, "lib/cjs.js::init")
		require.Len(t, calls, 1)
		require.Equal(t, "app/caller.js::run", calls[0].From)
		require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
	})
}

func TestImportEvidence_LocalRequireResolvesBareDirEntry(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		t.Run("same-repo pnpm workspace package/"+ext, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"packages/logger/index" + ext: "export function log() { return 1 }",
				"apps/web/handler" + ext:      "export function run() { const { log } = require('logger'); return log() }\n",
			})
			New(g).ResolveAll()
			calls := callsInto(g, "packages/logger/index"+ext+"::log")
			require.Len(t, calls, 1)
			require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
		})

		t.Run("Node core module never binds a same-named local directory/"+ext, func(t *testing.T) {
			for _, spec := range []string{"util", "events", "node:util"} {
				t.Run(spec, func(t *testing.T) {
					g := buildGraphFromSources(t, map[string]string{
						"packages/util/index" + ext:   "export function format() { return 1 }",
						"packages/events/index" + ext: "export function format() { return 2 }",
						"apps/web/handler" + ext:      "export function run() { const { format } = require('" + spec + "'); return format() }\n",
					})
					New(g).ResolveAll()
					require.Empty(t, callsInto(g, "packages/util/index"+ext+"::format"))
					require.Empty(t, callsInto(g, "packages/events/index"+ext+"::format"))
				})
			}
		})

		t.Run("multi-segment spec rejects 1-level and nested lastDirIndex neighbours/"+ext, func(t *testing.T) {
			for _, spec := range []string{"lib/logger", "@acme/logger"} {
				for _, neighbour := range []string{"app/logger/index", "app/internal/logger/index"} {
					t.Run(spec+"/"+neighbour, func(t *testing.T) {
						g := buildGraphFromSources(t, map[string]string{
							neighbour + ext:         "export function log() { return 0 }\n",
							"app/web/handler" + ext: "export function run() { const { log } = require('" + spec + "'); return log() }\n",
						})
						for _, n := range g.AllNodes() {
							n.RepoPrefix = "app"
						}
						New(g).ResolveAll()
						require.Empty(t, callsInto(g, neighbour+ext+"::log"))
						require.NotNil(t, findCallEdge(g, "app/web/handler"+ext+"::run", graph.UnresolvedMarker+"log"))
					})
				}
			}
		})

		t.Run("scoped package beats 1-level same-repo logger dir/"+ext, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"app/logger/index" + ext:                "export function log() { return 0 }\n",
				"app/packages/@acme/logger/index" + ext: "export function log() { return 1 }\n",
				"app/web/handler" + ext:                 "export function run() { const { log } = require('@acme/logger'); return log() }\n",
			})
			for _, n := range g.AllNodes() {
				n.RepoPrefix = "app"
			}
			New(g).ResolveAll()
			require.Empty(t, callsInto(g, "app/logger/index"+ext+"::log"))
			calls := callsInto(g, "app/packages/@acme/logger/index"+ext+"::log")
			require.Len(t, calls, 1)
			require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
		})
	}
}

func TestImportEvidence_LocalRequireResolvesCrossRepo(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		t.Run("cross-repo bare dir entry ignores 1-level and nested same-repo neighbours/"+ext, func(t *testing.T) {
			for _, neighbour := range []string{"app/logger/index", "app/internal/logger/index"} {
				for _, neighbourHasLog := range []bool{false, true} {
					name := neighbour + "/without log"
					neighbourSrc := "export function other() { return 0 }\n"
					if neighbourHasLog {
						name = neighbour + "/with log"
						neighbourSrc = "export function log() { return 0 }\n"
					}
					t.Run(name, func(t *testing.T) {
						g := buildGraphFromSources(t, map[string]string{
							"lib/logger/index" + ext: "export function log() { return 1 }\n",
							neighbour + ext:          neighbourSrc,
							"app/web/handler" + ext:  "export function run() { const { log } = require('lib/logger'); return log() }\n",
						})
						for _, n := range g.AllNodes() {
							if strings.HasPrefix(n.FilePath, "lib/") {
								n.RepoPrefix = "lib"
							} else {
								n.RepoPrefix = "app"
							}
						}
						New(g).ResolveAll()
						require.Empty(t, callsInto(g, "lib/logger/index"+ext+"::log"))
						require.Empty(t, callsInto(g, neighbour+ext+"::log"))
						NewCrossRepo(g).ResolveAll()
						require.Empty(t, callsInto(g, neighbour+ext+"::log"))
						calls := callsInto(g, "lib/logger/index"+ext+"::log")
						require.Len(t, calls, 1)
						require.True(t, calls[0].CrossRepo)
						require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
					})
				}
			}
		})
	}

	t.Run("cross-repo bare single-segment require never grabs foreign directories", func(t *testing.T) {
		for _, ext := range []string{".js", ".ts"} {
			t.Run(ext, func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"lib/src/internal/events/index" + ext: "export function emit() { return 1 }\n",
					"lib/logger/index" + ext:              "export function log() { return 1 }\n",
					"app/web/handler" + ext:               "export function run() { const { emit } = require('events'); const { log } = require('logger'); return emit() + log() }\n",
				})
				for _, n := range g.AllNodes() {
					if strings.HasPrefix(n.FilePath, "lib/") {
						n.RepoPrefix = "lib"
					} else {
						n.RepoPrefix = "app"
					}
					n.WorkspaceID = "shared"
				}
				New(g).ResolveAll()
				NewCrossRepo(g).ResolveAll()
				require.Empty(t, callsInto(g, "lib/src/internal/events/index"+ext+"::emit"))
				require.Empty(t, callsInto(g, "lib/logger/index"+ext+"::log"))
				require.NotNil(t, findCallEdge(g, "app/web/handler"+ext+"::run", graph.UnresolvedMarker+"emit"))
				require.NotNil(t, findCallEdge(g, "app/web/handler"+ext+"::run", graph.UnresolvedMarker+"log"))
			})
		}
	})

	t.Run("cross-repo scoped subpath matches resolveImport dirMatchesImport parity", func(t *testing.T) {
		for _, ext := range []string{".js", ".ts"} {
			t.Run(ext, func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"logger/sub/index" + ext: "export function log() { return 1 }\n",
					"app/web/h" + ext:        "export function run() { const { log } = require('@acme/logger/sub'); return log() }\n",
				})
				for _, n := range g.AllNodes() {
					if strings.HasPrefix(n.FilePath, "logger/") {
						n.RepoPrefix = "logger"
					} else {
						n.RepoPrefix = "app"
					}
					n.WorkspaceID = "shared"
				}
				New(g).ResolveAll()
				require.Empty(t, callsInto(g, "logger/sub/index"+ext+"::log"))
				NewCrossRepo(g).ResolveAll()
				calls := callsInto(g, "logger/sub/index"+ext+"::log")
				require.Len(t, calls, 1)
				require.True(t, calls[0].CrossRepo)
				require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
			})
		}
	})

	t.Run("cross-repo tracked npm package and isolation", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"provider/index.js": "export function work() { return 1 }\n",
			"consumer/app.js":   "import { work } from 'p'\nexport function viaReq() { const { work } = require('p'); return work() }\nexport function viaOther() { const { work } = require('other-ext'); return work() }\n",
		})
		for _, n := range g.AllNodes() {
			if strings.HasPrefix(n.FilePath, "provider/") {
				n.RepoPrefix = "provider"
				n.WorkspaceID = "shared"
			} else {
				n.RepoPrefix = "consumer"
				n.WorkspaceID = "shared"
			}
		}
		npmDep := func(filePath, spec string) bool { return spec == "p" }
		npmPkgs := func(spec string) map[string]NpmPackageTarget {
			if spec != "p" {
				return nil
			}
			return map[string]NpmPackageTarget{
				"provider": {WorkspaceID: "shared", Entries: []string{"provider/index.js"}},
			}
		}
		r := New(g)
		r.SetNpmDependencyLookup(npmDep)
		r.ResolveAll()
		cr := NewCrossRepo(g)
		cr.SetNpmDependencyLookup(npmDep)
		cr.SetNpmPackageLookup(npmPkgs)
		cr.ResolveAll()
		calls := callsInto(g, "provider/index.js::work")
		require.Len(t, calls, 1)
		require.Equal(t, "consumer/app.js::viaReq", calls[0].From)
		require.True(t, calls[0].CrossRepo)
		require.Equal(t, "p", calls[0].Meta[NpmImportSpecifierMeta])
		require.NotNil(t, findCallEdge(g, "consumer/app.js::viaOther", graph.UnresolvedMarker+"work"))
	})
}

func TestImportEvidence_CrossRepoTrackedNpmRequireRestubsOnProviderChange(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(ext+"/"+backend, func(t *testing.T) {
				files := map[string]string{
					"provA/index" + ext: "export function work() { return 1 }\n",
					"provB/index" + ext: "export function work() { return 2 }\n",
					"app/main" + ext:    "export function viaReq() { const { work } = require('p'); return work() }\n",
				}
				rawNodes, rawEdges := extractRawSources(t, files)
				for _, n := range rawNodes {
					switch {
					case strings.HasPrefix(n.FilePath, "provA/"):
						n.RepoPrefix = "provA"
						n.WorkspaceID = "wsA"
					case strings.HasPrefix(n.FilePath, "provB/"):
						n.RepoPrefix = "provB"
						n.WorkspaceID = "wsB"
					default:
						n.RepoPrefix = "app"
						n.WorkspaceID = "wsB"
					}
				}
				var store graph.Store = graph.New()
				if backend == "sqlite" {
					sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					defer sq.Close()
					store = sq
				}
				store.AddBatch(rawNodes, rawEdges)

				npmDep := func(_, spec string) bool { return spec == "p" }
				providers := map[string]NpmPackageTarget{
					"provA": {WorkspaceID: "wsA", Entries: []string{"provA/index" + ext}},
				}
				npmPkgs := func(spec string) map[string]NpmPackageTarget {
					if spec != "p" {
						return nil
					}
					return providers
				}
				crossWS := func(sourceWS string) []CrossWorkspaceDepRule {
					if sourceWS == "wsB" {
						return []CrossWorkspaceDepRule{{Workspace: "wsA", Modules: []string{"p"}}}
					}
					return nil
				}

				r := New(store)
				r.SetNpmDependencyLookup(npmDep)
				r.ResolveAll()

				cr := NewCrossRepo(store)
				cr.SetNpmDependencyLookup(npmDep)
				cr.SetNpmPackageLookup(npmPkgs)
				cr.SetCrossWorkspaceDepLookup(crossWS)

				// 1. Initial resolve binds to foreign-workspace provA.
				cr.ResolveAll()
				callsA := callsInto(store, "provA/index"+ext+"::work")
				require.Len(t, callsA, 1)
				require.True(t, callsA[0].CrossRepo)
				require.Equal(t, "p", callsA[0].Meta[NpmImportSpecifierMeta])

				// 2. Adding same-workspace provB retargets directly from provA to provB.
				providers["provB"] = NpmPackageTarget{WorkspaceID: "wsB", Entries: []string{"provB/index" + ext}}
				cr.ResolveAll()
				require.Empty(t, callsInto(store, "provA/index"+ext+"::work"))
				callsB := callsInto(store, "provB/index"+ext+"::work")
				require.Len(t, callsB, 1)
				require.True(t, callsB[0].CrossRepo)
				var crossCalls []string
				for e := range graph.EdgesLightSeq(store, graph.EdgeCrossRepoCalls) {
					crossCalls = append(crossCalls, e.To)
				}
				require.Equal(t, []string{"provB/index" + ext + "::work"}, crossCalls)

				// 3. Adding a competing provider in wsB introduces ambiguity -> restubs to unresolved
				// cleanly with no leaked restub_prev_* or stale resolution metadata.
				providers["provC"] = NpmPackageTarget{WorkspaceID: "wsB", Entries: []string{"provC/index" + ext}}
				cr.ResolveAll()
				require.Empty(t, callsInto(store, "provB/index"+ext+"::work"))
				call := unresolvedCall(t, store, "app/main"+ext+"::viaReq", "work")
				require.False(t, call.CrossRepo)
				require.Empty(t, call.Origin)
				require.NotContains(t, call.Meta, NpmImportSpecifierMeta)
				require.NotContains(t, call.Meta, "resolution")
				require.False(t, graph.HasRestubProvenance(call))
				for e := range graph.EdgesLightSeq(store, graph.EdgeCrossRepoCalls) {
					t.Fatalf("unexpected stale cross_repo_calls edge to %q", e.To)
				}

				// 4. Removing ambiguity rebinds cleanly via ResolveFilesAndIncoming.
				delete(providers, "provC")
				cr.ResolveFilesAndIncoming([]string{"app/main" + ext})
				callsB = callsInto(store, "provB/index"+ext+"::work")
				require.Len(t, callsB, 1)
				require.True(t, callsB[0].CrossRepo)
			})
		}
	}
}

func TestImportEvidence_CrossRepoScopedPassPreservesOutOfScopeRequire(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			files := map[string]string{
				"provA/index.ts": "export function work() { return 1 }\n",
				"provB/index.ts": "export function work() { return 2 }\n",
				"other/x.ts":     "export function noop() { return 0 }\n",
				"other/empty.ts": "// comment-only file with zero out-edges\n",
				"emptyRepo/a.ts": "export {}\n",
				"app/main.ts":    "export function viaReq() { const { work } = require('p'); return work() }\n",
			}
			rawNodes, rawEdges := extractRawSources(t, files)
			for _, n := range rawNodes {
				switch {
				case strings.HasPrefix(n.FilePath, "provA/"):
					n.RepoPrefix = "provA"
					n.WorkspaceID = "wsA"
				case strings.HasPrefix(n.FilePath, "provB/"):
					n.RepoPrefix = "provB"
					n.WorkspaceID = "wsB"
				case strings.HasPrefix(n.FilePath, "other/"):
					n.RepoPrefix = "other"
					n.WorkspaceID = "wsB"
				case strings.HasPrefix(n.FilePath, "emptyRepo/"):
					n.RepoPrefix = "emptyRepo"
					n.WorkspaceID = "wsB"
				default:
					n.RepoPrefix = "app"
					n.WorkspaceID = "wsB"
				}
			}
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				defer sq.Close()
				store = sq
			}
			store.AddBatch(rawNodes, rawEdges)

			npmDep := func(_, spec string) bool { return spec == "p" }
			providers := map[string]NpmPackageTarget{
				"provA": {WorkspaceID: "wsA", Entries: []string{"provA/index.ts"}},
			}
			npmPkgs := func(spec string) map[string]NpmPackageTarget {
				if spec != "p" {
					return nil
				}
				return providers
			}
			crossWS := func(sourceWS string) []CrossWorkspaceDepRule {
				if sourceWS == "wsB" {
					return []CrossWorkspaceDepRule{{Workspace: "wsA", Modules: []string{"p"}}}
				}
				return nil
			}
			r := New(store)
			r.SetNpmDependencyLookup(npmDep)
			r.ResolveAll()

			cr := NewCrossRepo(store)
			cr.SetNpmDependencyLookup(npmDep)
			cr.SetNpmPackageLookup(npmPkgs)
			cr.SetCrossWorkspaceDepLookup(crossWS)
			cr.ResolveAll()
			require.Len(t, callsInto(store, "provA/index.ts::work"), 1)

			// Add provB, then run scoped passes that do NOT include app/main.ts
			// (including 0-edge files and empty repos where edges == nil).
			// Out-of-scope app/main.ts::viaReq must NOT be orphaned.
			providers["provB"] = NpmPackageTarget{WorkspaceID: "wsB", Entries: []string{"provB/index.ts"}}
			cr.ResolveForRepo("provB")
			cr.ResolveForRepo("emptyRepo")
			cr.ResolveForFile("other", "x.ts")
			cr.ResolveForFile("other", "empty.ts")
			cr.ResolveFilesAndIncoming([]string{"other/x.ts"})

			callsA := callsInto(store, "provA/index.ts::work")
			require.Len(t, callsA, 1, "scoped pass for unrelated repo/file must not restub out-of-scope require()")
			require.True(t, callsA[0].CrossRepo)
			var crossCalls []string
			for e := range graph.EdgesLightSeq(store, graph.EdgeCrossRepoCalls) {
				crossCalls = append(crossCalls, e.To)
			}
			require.Equal(t, []string{"provA/index.ts::work"}, crossCalls)

			// Now a scoped pass that DOES include app/main.ts retargets cleanly to provB.
			cr.ResolveFilesAndIncoming([]string{"app/main.ts"})
			require.Empty(t, callsInto(store, "provA/index.ts::work"))
			require.Len(t, callsInto(store, "provB/index.ts::work"), 1)
		})
	}
}

func TestImportEvidence_CrossRepoMixedAliasedImportAndLocalRequire(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			files := map[string]string{
				"provA/index.ts": "export function work() { return 1 }\n",
				"provB/index.ts": "export function work() { return 2 }\n",
				"app/main.ts":    "import { work as aliased } from 'p'\nexport function viaES() { return aliased() }\nexport function viaReq() { const { work } = require('p'); return work() }\n",
			}
			rawNodes, rawEdges := extractRawSources(t, files)
			for _, n := range rawNodes {
				switch {
				case strings.HasPrefix(n.FilePath, "provA/"):
					n.RepoPrefix = "provA"
					n.WorkspaceID = "wsA"
				case strings.HasPrefix(n.FilePath, "provB/"):
					n.RepoPrefix = "provB"
					n.WorkspaceID = "wsB"
				default:
					n.RepoPrefix = "app"
					n.WorkspaceID = "wsB"
				}
			}
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				defer sq.Close()
				store = sq
			}
			store.AddBatch(rawNodes, rawEdges)

			npmDep := func(_, spec string) bool { return spec == "p" }
			providers := map[string]NpmPackageTarget{
				"provA": {WorkspaceID: "wsA", Entries: []string{"provA/index.ts"}},
			}
			cr := NewCrossRepo(store)
			cr.SetNpmDependencyLookup(npmDep)
			cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
				if spec != "p" {
					return nil
				}
				return providers
			})
			cr.SetCrossWorkspaceDepLookup(func(sourceWS string) []CrossWorkspaceDepRule {
				if sourceWS == "wsB" {
					return []CrossWorkspaceDepRule{{Workspace: "wsA", Modules: []string{"p"}}}
				}
				return nil
			})
			r := New(store)
			r.SetNpmDependencyLookup(npmDep)
			r.ResolveAll()
			cr.ResolveAll()
			require.Len(t, callsInto(store, "provA/index.ts::work"), 2)

			// Retarget from provA to provB: viaReq must rebind cleanly (not unresolved::aliased, no duplicates).
			providers["provB"] = NpmPackageTarget{WorkspaceID: "wsB", Entries: []string{"provB/index.ts"}}
			cr.ResolveAll()
			require.Empty(t, callsInto(store, "provA/index.ts::work"))
			require.Len(t, callsInto(store, "provB/index.ts::work"), 2)
			var reqCalls []*graph.Edge
			for _, e := range store.GetOutEdges("app/main.ts::viaReq") {
				if e.Kind == graph.EdgeCalls && e.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire {
					reqCalls = append(reqCalls, e)
				}
			}
			require.Len(t, reqCalls, 1)
			require.Equal(t, "provB/index.ts::work", reqCalls[0].To)

			// Ambiguity: viaES -> unresolved::aliased, viaReq -> unresolved::work (no duplicate, clean meta).
			providers["provC"] = NpmPackageTarget{WorkspaceID: "wsB", Entries: []string{"provC/index.ts"}}
			cr.ResolveAll()
			require.Empty(t, callsInto(store, "provB/index.ts::work"))
			esCall := unresolvedCall(t, store, "app/main.ts::viaES", "aliased")
			require.True(t, graph.HasRestubProvenance(esCall))
			reqCall := unresolvedCall(t, store, "app/main.ts::viaReq", "work")
			require.Nil(t, findCallEdge(store, "app/main.ts::viaReq", graph.UnresolvedMarker+"aliased"))
			require.False(t, graph.HasRestubProvenance(reqCall))
			require.NotContains(t, reqCall.Meta, "resolution")
			require.NotContains(t, reqCall.Meta, NpmImportSpecifierMeta)
		})
	}
}

func TestImportEvidence_CrossRepoNpmAliasSiblingImportNoChurn(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"prov/index.js": "export function other() { return 1 }\nexport function work() { return 2 }\n",
		"other/x.js":    "export function noop() { return 0 }\n",
		"app/main.js":   "import { other } from 'p'\nexport function run() { const { work } = require('p-alias'); return other() + work() }\n",
	})
	for _, n := range g.AllNodes() {
		switch {
		case strings.HasPrefix(n.FilePath, "prov/"):
			n.RepoPrefix = "prov"
			n.WorkspaceID = "shared"
		case strings.HasPrefix(n.FilePath, "other/"):
			n.RepoPrefix = "other"
			n.WorkspaceID = "shared"
		default:
			n.RepoPrefix = "app"
			n.WorkspaceID = "shared"
		}
	}
	npmDep := func(_, spec string) bool { return spec == "p" || spec == "p-alias" }
	npmAlias := func(_, spec string) string {
		if spec == "p-alias" {
			return "p"
		}
		return ""
	}
	r := New(g)
	r.SetNpmDependencyLookup(npmDep)
	r.SetNpmAliasResolver(npmAlias)
	r.ResolveAll()

	cr := NewCrossRepo(g)
	cr.SetNpmDependencyLookup(npmDep)
	cr.SetNpmAliasResolver(npmAlias)
	cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
		if spec != "p" {
			return nil
		}
		return map[string]NpmPackageTarget{"prov": {WorkspaceID: "shared", Entries: []string{"prov/index.js"}}}
	})
	cr.ResolveAll()

	calls := callsInto(g, "prov/index.js::work")
	require.Len(t, calls, 1)
	require.Equal(t, "p", calls[0].Meta[NpmImportSpecifierMeta])

	// Second ResolveAll must not restub/re-resolve (zero churn).
	stats2 := cr.ResolveAll()
	require.Zero(t, stats2.Resolved)
	require.Len(t, callsInto(g, "prov/index.js::work"), 1)

	// Scoped pass on another file must preserve the binding.
	cr.ResolveForFile("other", "x.js")
	require.Len(t, callsInto(g, "prov/index.js::work"), 1)
}

type cancelAfterPagesStore struct {
	graph.Store
	cancelAfter   int
	pages         int
	sitesHydrated int
	importSites   int
	cancel        context.CancelFunc
	reindexIDs    []graph.EdgeIdentity
}

func (s *cancelAfterPagesStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	res := s.Store.GetEdgeCandidates(endpoints, sites)
	s.pages++
	s.sitesHydrated += len(sites)
	for _, site := range sites {
		if site.Kind == graph.EdgeImports {
			s.importSites++
		}
	}
	if s.cancel != nil && s.pages >= s.cancelAfter {
		s.cancel()
	}
	return res
}

func (s *cancelAfterPagesStore) GetEdgeCandidatesErr(ctx context.Context, endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) (graph.EdgeCandidateSet, error) {
	if ctx != nil && ctx.Err() != nil {
		return graph.EdgeCandidateSet{}, ctx.Err()
	}
	var (
		res graph.EdgeCandidateSet
		err error
	)
	if checked, ok := s.Store.(graph.CheckedEdgeCandidateLookup); ok {
		res, err = checked.GetEdgeCandidatesErr(ctx, endpoints, sites)
	} else {
		res = s.Store.GetEdgeCandidates(endpoints, sites)
	}
	if err != nil {
		return res, err
	}
	s.pages++
	s.sitesHydrated += len(sites)
	for _, site := range sites {
		if site.Kind == graph.EdgeImports {
			s.importSites++
		}
	}
	if s.cancel != nil && s.pages >= s.cancelAfter {
		s.cancel()
	}
	return res, nil
}

func (s *cancelAfterPagesStore) ReindexEdges(batch []graph.EdgeReindex) {
	for _, item := range batch {
		if item.Edge != nil {
			s.reindexIDs = append(s.reindexIDs, graph.EdgeIdentityFor(item.Edge))
		}
	}
	s.Store.ReindexEdges(batch)
}

func TestImportEvidence_CrossRepoRestubRequirePaginationAndCancel(t *testing.T) {
	t.Run("mid-pagination ctx cancel flushes completed page without corrupting indexes", func(t *testing.T) {
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(backend, func(t *testing.T) {
				var base graph.Store = graph.New()
				if backend == "sqlite" {
					sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					defer sq.Close()
					base = sq
				}
				total := resolvePendingPageRows + 5
				nodes := make([]*graph.Node, 0, total+3)
				nodes = append(nodes,
					&graph.Node{ID: "app/main.ts", Kind: graph.KindFile, FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app", WorkspaceID: "shared"},
					&graph.Node{ID: "provA/index.ts", Kind: graph.KindFile, FilePath: "provA/index.ts", Language: "typescript", RepoPrefix: "provA", WorkspaceID: "shared"},
					&graph.Node{ID: "provA/index.ts::work", Kind: graph.KindFunction, Name: "work", FilePath: "provA/index.ts", Language: "typescript", RepoPrefix: "provA", WorkspaceID: "shared"},
				)
				edges := make([]*graph.Edge, 0, total*2)
				for i := 0; i < total; i++ {
					fnID := fmt.Sprintf("app/main.ts::run%d", i)
					nodes = append(nodes, &graph.Node{ID: fnID, Kind: graph.KindFunction, Name: fmt.Sprintf("run%d", i), FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app", WorkspaceID: "shared"})
					call := &graph.Edge{
						From: fnID, To: "provA/index.ts::work", Kind: graph.EdgeCalls,
						FilePath: "app/main.ts", Line: i + 1, CrossRepo: true, Origin: graph.OriginASTResolved,
						Meta: map[string]any{
							graph.JSLexicalBindingMeta:     graph.JSLexicalRequire,
							graph.JSLexicalRequireSpecMeta: "p",
							NpmImportSpecifierMeta:         "p",
						},
					}
					xcall := *call
					xcall.Kind = graph.EdgeCrossRepoCalls
					edges = append(edges, call, &xcall)
				}
				base.AddBatch(nodes, edges)

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				wrapped := &cancelAfterPagesStore{Store: base, cancelAfter: 1, cancel: cancel}
				cr := NewCrossRepo(wrapped)
				cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
				// Return empty providers so every inspected call is stale and must restub.
				cr.SetNpmPackageLookup(func(string) map[string]NpmPackageTarget { return nil })

				memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
				cr.restubStaleTrackedNpmRequireCallsAll(ctx, memo)

				// Exactly page 1 flushed; remaining 5 stay cleanly indexed at provA/index.ts::work.
				inProv := callsInto(base, "provA/index.ts::work")
				require.Len(t, inProv, 5)
				for _, e := range inProv {
					require.Equal(t, "provA/index.ts::work", e.To, "in-edge bucket must never hold mutated unresolved edge")
				}
				unresolvedCount := 0
				for e := range base.EdgesWithUnresolvedTarget() {
					if e.Kind == graph.EdgeCalls && e.To == graph.UnresolvedMarker+"work" {
						unresolvedCount++
					}
				}
				require.Equal(t, resolvePendingPageRows, unresolvedCount)
			})
		}
	})

	t.Run("duplicate EdgeCrossRepoCalls on same site straddling page boundary reindexes once on SQLite", func(t *testing.T) {
		sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		require.NoError(t, err)
		defer sq.Close()

		nodes := []*graph.Node{
			{ID: "app/main.ts", Kind: graph.KindFile, FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app", WorkspaceID: "shared"},
			{ID: "app/main.ts::shared", Kind: graph.KindFunction, Name: "shared", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app", WorkspaceID: "shared"},
			{ID: "provA/index.ts", Kind: graph.KindFile, FilePath: "provA/index.ts", Language: "typescript", RepoPrefix: "provA", WorkspaceID: "shared"},
			{ID: "provA/index.ts::workA", Kind: graph.KindFunction, Name: "workA", FilePath: "provA/index.ts", Language: "typescript", RepoPrefix: "provA", WorkspaceID: "shared"},
			{ID: "provA/index.ts::workB", Kind: graph.KindFunction, Name: "workB", FilePath: "provA/index.ts", Language: "typescript", RepoPrefix: "provA", WorkspaceID: "shared"},
		}
		// Insert resolvePendingPageRows-1 pad rows before the 2 {shared, 1} rows
		// and 1 trailing pad row after so the duplicate {shared, 1} rows sit at
		// raw indices [resolvePendingPageRows-1, resolvePendingPageRows]
		// (straddling the page boundary) while yielding resolvePendingPageRows+1
		// unique sites across 2 pages.
		edges := make([]*graph.Edge, 0, (resolvePendingPageRows+2)*2)
		appendPad := func(i int) {
			fnID := fmt.Sprintf("app/main.ts::pad%d", i)
			nodes = append(nodes, &graph.Node{ID: fnID, Kind: graph.KindFunction, Name: fmt.Sprintf("pad%d", i), FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app", WorkspaceID: "shared"})
			call := &graph.Edge{
				From: fnID, To: "provA/index.ts::workA", Kind: graph.EdgeCalls,
				FilePath: "app/main.ts", Line: i + 10, CrossRepo: true, Origin: graph.OriginASTResolved,
				Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: "p", NpmImportSpecifierMeta: "p"},
			}
			xcall := *call
			xcall.Kind = graph.EdgeCrossRepoCalls
			edges = append(edges, call, &xcall)
		}
		for i := 0; i < resolvePendingPageRows-1; i++ {
			appendPad(i)
		}
		for _, target := range []string{"provA/index.ts::workA", "provA/index.ts::workB"} {
			call := &graph.Edge{
				From: "app/main.ts::shared", To: target, Kind: graph.EdgeCalls,
				FilePath: "app/main.ts", Line: 1, CrossRepo: true, Origin: graph.OriginASTResolved,
				Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: "p", NpmImportSpecifierMeta: "p"},
			}
			xcall := *call
			xcall.Kind = graph.EdgeCrossRepoCalls
			edges = append(edges, call, &xcall)
		}
		appendPad(resolvePendingPageRows - 1)
		sq.AddBatch(nodes, edges)

		wrapped := &cancelAfterPagesStore{Store: sq}
		cr := NewCrossRepo(wrapped)
		cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
		cr.SetNpmPackageLookup(func(string) map[string]NpmPackageTarget { return nil })

		memo := &npmImportMemo{probes: map[npmProbeKey]*graph.Node{}, nodes: map[string]*graph.Node{}}
		cr.restubStaleTrackedNpmRequireCallsAll(context.Background(), memo)

		require.Equal(t, 2, wrapped.pages, "expected 2 hydration pages to run")
		require.Equal(t, resolvePendingPageRows+1, wrapped.sitesHydrated, "siteSeen must deduplicate {shared, 1} before paging")
		seenReindex := make(map[graph.EdgeIdentity]int)
		for _, id := range wrapped.reindexIDs {
			seenReindex[id]++
			require.Equal(t, 1, seenReindex[id], "duplicate ReindexEdges for %+v", id)
		}
		require.Equal(t, resolvePendingPageRows+2, len(wrapped.reindexIDs))
	})
}

func TestImportEvidence_CrossRepoNpmPrepassSkipsNonImportEdges(t *testing.T) {
	t.Run("ResolveForFile with cross-repo require and zero EdgeImports issues no EdgeImports hydration", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"prov/index.ts": "export function work() { return 1 }\n",
			"app/main.ts":   "export function run() { const { work } = require('p'); return work() }\n",
		})
		for _, n := range g.AllNodes() {
			if strings.HasPrefix(n.FilePath, "prov/") {
				n.RepoPrefix = "prov"
			} else {
				n.RepoPrefix = "app"
			}
			n.WorkspaceID = "shared"
		}
		npmDep := func(_, spec string) bool { return spec == "p" }
		npmPkgs := func(spec string) map[string]NpmPackageTarget {
			if spec != "p" {
				return nil
			}
			return map[string]NpmPackageTarget{"prov": {WorkspaceID: "shared", Entries: []string{"prov/index.ts"}}}
		}
		r := New(g)
		r.SetNpmDependencyLookup(npmDep)
		r.ResolveAll()

		wrapped := &cancelAfterPagesStore{Store: g}
		cr := NewCrossRepo(wrapped)
		cr.SetNpmDependencyLookup(npmDep)
		cr.SetNpmPackageLookup(npmPkgs)
		cr.ResolveAll()
		require.Len(t, callsInto(g, "prov/index.ts::work"), 1)

		wrapped.importSites = 0
		cr.ResolveForFile("app", "main.ts")
		require.Zero(t, wrapped.importSites, "resolveTrackedNpmImportsPass must not hydrate EdgeCalls as EdgeImports")
	})

	t.Run("single-line top-level require plus import retargets cleanly", func(t *testing.T) {
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(backend, func(t *testing.T) {
				files := map[string]string{
					"provA/index.ts": "export function work() { return 1 }\n",
					"provB/index.ts": "export function work() { return 2 }\n",
					"app/main.ts":    "{ const { work } = require('p'); work() }; import { work } from 'p'\n",
				}
				rawNodes, rawEdges := extractRawSources(t, files)
				for _, n := range rawNodes {
					switch {
					case strings.HasPrefix(n.FilePath, "provA/"):
						n.RepoPrefix = "provA"
						n.WorkspaceID = "wsA"
					case strings.HasPrefix(n.FilePath, "provB/"):
						n.RepoPrefix = "provB"
						n.WorkspaceID = "wsB"
					default:
						n.RepoPrefix = "app"
						n.WorkspaceID = "wsB"
					}
				}
				var store graph.Store = graph.New()
				if backend == "sqlite" {
					sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					defer sq.Close()
					store = sq
				}
				store.AddBatch(rawNodes, rawEdges)

				npmDep := func(_, spec string) bool { return spec == "p" }
				providers := map[string]NpmPackageTarget{
					"provA": {WorkspaceID: "wsA", Entries: []string{"provA/index.ts"}},
				}
				r := New(store)
				r.SetNpmDependencyLookup(npmDep)
				r.ResolveAll()

				cr := NewCrossRepo(store)
				cr.SetNpmDependencyLookup(npmDep)
				cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
					if spec != "p" {
						return nil
					}
					return providers
				})
				cr.SetCrossWorkspaceDepLookup(func(sourceWS string) []CrossWorkspaceDepRule {
					if sourceWS == "wsB" {
						return []CrossWorkspaceDepRule{{Workspace: "wsA", Modules: []string{"p"}}}
					}
					return nil
				})
				cr.ResolveAll()
				require.Len(t, callsInto(store, "provA/index.ts::work"), 1)

				providers["provB"] = NpmPackageTarget{WorkspaceID: "wsB", Entries: []string{"provB/index.ts"}}
				cr.ResolveForFile("app", "main.ts")
				require.Empty(t, callsInto(store, "provA/index.ts::work"))
				require.Len(t, callsInto(store, "provB/index.ts::work"), 1)
			})
		}
	})
}

func TestImportEvidence_CrossRepoRequireCacheClearedOnLookupClearAndNoRevYield(t *testing.T) {
	t.Run("clearLookupCache flushes require caches", func(t *testing.T) {
		cr := NewCrossRepo(graph.New())
		cr.outEdgesByFile = map[string][]*graph.Edge{"a.ts": nil}
		cr.requireTargetCache = map[string]jsRequireTarget{"a.ts\x00p": {target: "prov/index.ts"}}
		cr.clearLookupCache()
		require.Nil(t, cr.outEdgesByFile)
		require.Nil(t, cr.requireTargetCache)
	})

	t.Run("chunk yield without mutationRevisioner flushes require cache", func(t *testing.T) {
		t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")
		g := buildGraphFromSources(t, map[string]string{
			"app/main.ts": "export function run1() { const { work } = require('missing-p'); return work() }\nexport function run2() { const { work } = require('missing-p'); return work() }\n",
		})
		for _, n := range g.AllNodes() {
			n.RepoPrefix = "app"
			n.WorkspaceID = "shared"
		}
		// outEdgesCountingStore embeds the graph.Store interface, so it does NOT
		// implement mutationRevisioner (revKnown == false). Because both calls stay
		// unresolved (len(scBatch) == 0), ONLY the !revKnown yield clears the cache.
		wrapped := &outEdgesCountingStore{Store: g}
		var npmCalls atomic.Int64
		cr := NewCrossRepo(wrapped)
		cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "missing-p" })
		cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
			npmCalls.Add(1)
			return nil
		})
		cr.ResolveAll()
		require.Equal(t, int64(2), npmCalls.Load(), "!revKnown yield must flush requireTargetCache between chunks")
	})
}

func TestImportEvidence_CrossRepoParallelRequireAndReExportNoRace(t *testing.T) {
	var callerLines strings.Builder
	for i := 0; i < 16; i++ {
		fmt.Fprintf(&callerLines, "export function run%d() { const { work } = require('lib/barrel'); return work() }\n", i)
	}
	files := map[string]string{
		"lib/impl.js":         "export function work() { return 1 }\n",
		"lib/barrel/index.js": "export * from '../impl'\n",
		"app/caller.js":       callerLines.String(),
	}
	// Do NOT run single-repo Resolver first so EdgeReExports and EdgeImports
	// stay pending in the same parallel chunk as the JSLexicalRequire calls.
	g := buildGraphFromSources(t, files)
	for _, n := range g.AllNodes() {
		if strings.HasPrefix(n.FilePath, "lib/") {
			n.RepoPrefix = "lib"
		} else {
			n.RepoPrefix = "app"
		}
	}
	NewCrossRepo(g).ResolveAll()
	calls := callsInto(g, "lib/impl.js::work")
	require.Len(t, calls, 16)
	for _, c := range calls {
		require.True(t, c.CrossRepo)
		require.Equal(t, graph.OriginASTResolved, c.Origin)
	}
}

type outEdgesCountingStore struct {
	graph.Store
	outEdgeCalls atomic.Int64
}

func (s *outEdgesCountingStore) GetOutEdges(nodeID string) []*graph.Edge {
	s.outEdgeCalls.Add(1)
	return s.Store.GetOutEdges(nodeID)
}

func TestImportEvidence_CrossRepoRequireTargetCacheAvoidsRepeatLookups(t *testing.T) {
	var callerLines strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&callerLines, "export function run%d() { const { work } = require('p'); return work() }\n", i)
	}
	g := buildGraphFromSources(t, map[string]string{
		"prov/impl.ts":  "export function work() { return 1 }\n",
		"prov/index.ts": "export * from './impl'\n",
		"app/main.ts":   callerLines.String(),
	})
	for _, n := range g.AllNodes() {
		if strings.HasPrefix(n.FilePath, "prov/") {
			n.RepoPrefix = "prov"
			n.WorkspaceID = "shared"
		} else {
			n.RepoPrefix = "app"
			n.WorkspaceID = "shared"
		}
	}
	counting := &outEdgesCountingStore{Store: g}
	var npmCalls atomic.Int64
	cr := NewCrossRepo(counting)
	cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
	cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
		npmCalls.Add(1)
		if spec != "p" {
			return nil
		}
		return map[string]NpmPackageTarget{
			"prov": {WorkspaceID: "shared", Entries: []string{"prov/index.ts"}},
		}
	})
	cr.ResolveForFile("app", "main.ts")
	require.Len(t, callsInto(g, "prov/impl.ts::work"), 12)
	// Deterministic single-threaded scoped pass: requireTargetCache and
	// outEdgesByFile must serve all 12 require('p') calls from a single lookup.
	require.Equal(t, int64(1), npmCalls.Load())
	require.LessOrEqual(t, counting.outEdgeCalls.Load(), int64(2))
}

func TestImportEvidence_CrossRepoLexicalShadowNeverRemoteStitches(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"parameter shadow": {graph.JSLexicalBindingMeta: graph.JSPendingParameter},
		"local shadow":     {graph.JSLexicalBindingMeta: graph.JSLexicalLocal},
		"unresolved req":   {graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: "missing-pkg"},
	} {
		t.Run(name, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "app/main.ts", Kind: graph.KindFile, Name: "main.ts", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app"})
			g.AddNode(&graph.Node{ID: "app/main.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app"})
			wireImport(g, "app/main.ts", "extmod", "extmod/mod.ts")
			edge := &graph.Edge{
				From: "app/main.ts::run", To: graph.UnresolvedMarker + "Helper",
				Kind: graph.EdgeCalls, FilePath: "app/main.ts", Line: 5, Meta: meta,
			}
			g.AddEdge(edge)
			cr := NewCrossRepo(g)
			p := &fakeProber{hit: true, decl: hitDecl()}
			cr.EnableRemoteStitch(p, 100)
			cr.ResolveAll()
			require.Zero(t, p.calls, "lexically bound call must never trigger remote stitch")
			require.Equal(t, graph.UnresolvedMarker+"Helper", edge.To)
		})
	}
}

func TestImportEvidence_CollapsedSameLineNormalization(t *testing.T) {
	runBoth := func(t *testing.T, files map[string]string, check func(t *testing.T, store graph.Store)) {
		t.Helper()
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(backend, func(t *testing.T) {
				var store graph.Store
				if backend == "sqlite" {
					rawNodes, rawEdges := extractRawSources(t, files)
					// Prove SQLite's INSERT OR IGNORE genuinely receives colliding
					// same-identity EdgeCalls rather than pre-deduped in-memory edges.
					counts := make(map[graph.EdgeIdentity]int)
					hasCollision := false
					for _, e := range rawEdges {
						if e.Kind != graph.EdgeCalls {
							continue
						}
						id := graph.EdgeIdentityFor(e)
						counts[id]++
						if counts[id] >= 2 {
							hasCollision = true
						}
					}
					require.True(t, hasCollision, "expected raw colliding EdgeCalls for SQLite dedup test")
					sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					defer sq.Close()
					sq.AddBatch(rawNodes, rawEdges)
					store = sq
				} else {
					store = buildGraphFromSources(t, files)
				}
				New(store).ResolveAll()
				check(t, store)
			})
		}
	}

	t.Run("local helper and param shadow on same line", func(t *testing.T) {
		for name, body := range map[string]string{
			"local first": "export function run(items) { function work() { return 2 }\n  work(); return items.map((work) => work())\n}",
			"param first": "export function run(items) { function work() { return 2 }\n  items.map((work) => work()); return work()\n}",
		} {
			t.Run(name, func(t *testing.T) {
				runBoth(t, map[string]string{
					"a/one.ts":    "export function work() { return 1 }",
					"app/main.ts": "import { work } from '../a/one'\n" + body + "\n",
				}, func(t *testing.T, store graph.Store) {
					require.Empty(t, callsInto(store, "a/one.ts::work"))
					var boundLocal bool
					for _, e := range store.GetOutEdges("app/main.ts::run") {
						if e.Kind != graph.EdgeCalls || e.Line != 3 || graph.IsBuiltinStub(e.To) {
							continue
						}
						callee := store.GetNode(e.To)
						require.NotNil(t, callee, "same-line local call must bind, got %s", e.To)
						require.Equal(t, "app/main.ts", callee.FilePath)
						boundLocal = true
					}
					require.True(t, boundLocal)
				})
			})
		}
	})

	t.Run("colliding distinct require specs on same line downgrade deterministically", func(t *testing.T) {
		runBoth(t, map[string]string{
			"a/one.js":    "export function work() { return 1 }",
			"b/two.js":    "export function work() { return 2 }",
			"app/main.js": "export function run() { { const { work } = require('../a/one'); work() } { const { work } = require('../b/two'); work() } }\n",
		}, func(t *testing.T, store graph.Store) {
			require.Empty(t, callsInto(store, "a/one.js::work"))
			require.Empty(t, callsInto(store, "b/two.js::work"))
			call := unresolvedCall(t, store, "app/main.js::run", "work")
			require.Equal(t, graph.JSLexicalLocal, call.Meta[graph.JSLexicalBindingMeta])
			require.NotContains(t, call.Meta, graph.JSLexicalRequireSpecMeta)
		})
	})
}

func TestImportEvidence_MethodComputedKeyAndDecoratorNotShadowedByParams(t *testing.T) {
	t.Run("computed key require emits import and binds", func(t *testing.T) {
		for _, ext := range []string{".js", ".ts"} {
			t.Run(ext, func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"a/one" + ext:    "export function work() { return 1 }\nexport const key = 'k'\n",
					"app/main" + ext: "export class C { [(() => { const { work } = require('../a/one'); return work() })()](require) {} }\n",
				})
				if ext == ".js" {
					var hasImport bool
					for _, e := range g.GetOutEdges("app/main.js") {
						if e.Kind == graph.EdgeImports && e.To == graph.UnresolvedMarker+"import::../a/one" {
							hasImport = true
						}
					}
					require.True(t, hasImport, "computed key require must emit EdgeImports")
				}
				New(g).ResolveAll()
				require.Len(t, callsInto(g, "a/one"+ext+"::work"), 1)
			})
		}
	})

	t.Run("JS method decorator require emits import", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"a/one.js":    "export function dec() { return () => {} }",
			"app/main.js": "export class C { @require('../a/one') m(require) {} }\n",
		})
		var hasImport bool
		for _, e := range g.GetOutEdges("app/main.js") {
			if e.Kind == graph.EdgeImports && e.To == graph.UnresolvedMarker+"import::../a/one" {
				hasImport = true
			}
		}
		require.True(t, hasImport, "method decorator require must emit EdgeImports")
	})

	t.Run("computed key bare call not shadowed by method param", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"a/one.ts":    "export function work() { return 'k' }",
			"app/main.ts": "import { work } from '../a/one'\nexport class C { [work()](work: any) { return work() } }\n",
		})
		New(g).ResolveAll()
		calls := callsInto(g, "a/one.ts::work")
		require.Len(t, calls, 1)
		require.Equal(t, "app/main.ts", calls[0].From)
	})

	t.Run("TS parameter decorator not shadowed by method param", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"a/one.ts":    "export function work() { return () => {} }",
			"app/main.ts": "import { work } from '../a/one'\nexport class C { handle(@work() work: string) { return work() } }\n",
		})
		New(g).ResolveAll()
		calls := callsInto(g, "a/one.ts::work")
		require.Len(t, calls, 1)
		require.Equal(t, "app/main.ts::C.handle", calls[0].From)
	})

	t.Run("TS parameter decorator require not shadowed by method param", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"a/one.ts":    "export function work() { return () => {} }",
			"app/main.ts": "function dec(_fn: any) { return () => {} }\nexport class C { handle(@dec(() => { const { work } = require('../a/one'); return work() }) require: any) {} }\n",
		})
		New(g).ResolveAll()
		require.Len(t, callsInto(g, "a/one.ts::work"), 1)
	})

	t.Run("class expression decorator not shadowed by class name", func(t *testing.T) {
		for _, ext := range []string{".js", ".ts"} {
			t.Run(ext, func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"a/one" + ext:    "export function work() { return () => {} }",
					"app/main" + ext: "import { work } from '../a/one'\nexport const Box = @work() class work {}\n",
				})
				New(g).ResolveAll()
				require.Len(t, callsInto(g, "a/one"+ext+"::work"), 1)
			})
		}
	})

	t.Run("TS method decorator inside named class expr still shadowed by class name", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"a/one.ts":    "export function work() { return () => {} }",
			"app/main.ts": "import { work } from '../a/one'\nexport const Box = class work { @work() m() {} }\n",
		})
		New(g).ResolveAll()
		require.Empty(t, callsInto(g, "a/one.ts::work"))
	})
}

func TestImportEvidence_ConflictingRequireVarSpecsDowngrade(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		for name, body := range map[string]string{
			"one then two": "export function run(ok) { if (ok) { var { work } = require('../a/one') } else { var { work } = require('../b/two') } return work() }",
			"two then one": "export function run(ok) { if (ok) { var { work } = require('../b/two') } else { var { work } = require('../a/one') } return work() }",
		} {
			t.Run(name+"/"+ext, func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"a/one" + ext:    "export function work() { return 1 }",
					"b/two" + ext:    "export function work() { return 2 }",
					"app/main" + ext: "import { work } from '../a/one'\n" + body + "\n",
				})
				New(g).ResolveAll()
				require.Empty(t, callsInto(g, "a/one"+ext+"::work"))
				require.Empty(t, callsInto(g, "b/two"+ext+"::work"))
				call := unresolvedCall(t, g, "app/main"+ext+"::run", "work")
				require.Equal(t, graph.JSLexicalLocal, call.Meta[graph.JSLexicalBindingMeta])
				require.NotContains(t, call.Meta, graph.JSLexicalRequireSpecMeta)
			})
		}
	}
}

func TestImportEvidence_AdditionalLexicalRequireShadows(t *testing.T) {
	tsCases := map[string]struct {
		body            string
		assertLocalFrom string
	}{
		"namespace export const":           {body: "namespace N { export const require = () => () => 2; export function run() { const work = require('../a/one'); return work() } }"},
		"namespace export const work":      {body: "declare function get(): any\nnamespace N { export const work = get(); export function run() { return work() } }", assertLocalFrom: "app/main$EXT::run"},
		"namespace export var work":        {body: "declare function get(): any\nnamespace N { export function run() { return work() } export var work = get() }", assertLocalFrom: "app/main$EXT::run"},
		"namespace hoisted var":            {body: "namespace N { export function run() { const work = require('../a/one'); return work() } if (true) { var require = () => () => 2 } }"},
		"top-level namespace":              {body: "namespace require { export const x = 1 }\nexport function run() { const work = require('../a/one'); return work() }"},
		"nested namespace":                 {body: "namespace require.Sub { export const x = 1 }\nexport function run() { const work = require('../a/one'); return work() }"},
		"3-segment namespace":              {body: "namespace require.Sub.Leaf { export const x = 1 }\nexport function run() { const work = require('../a/one'); return work() }"},
		"export 3-segment namespace":       {body: "export namespace require.Sub.Leaf { export const x = 1 }\nexport function run() { const work = require('../a/one'); return work() }"},
		"inner 3-segment namespace work":   {body: "namespace N { namespace work.Sub.Leaf { export const x = 1 } export function run() { return work() } }", assertLocalFrom: "app/main$EXT::run"},
		"import alias":                     {body: "namespace Loader { export function require(s: string) { return () => 2 } }\nimport require = Loader.require\nexport function run() { const work = require('../a/one'); return work() }"},
		"import alias with comment":        {body: "namespace Loader { export function require(s: string) { return () => 2 } }\nimport /* compat */ require = Loader.require\nexport function run() { const work = require('../a/one'); return work() }"},
		"export import alias":              {body: "namespace Loader { export function require(s: string) { return () => 2 } }\nexport import require = Loader.require\nexport function run() { const work = require('../a/one'); return work() }"},
		"export import alias with comment": {body: "namespace Loader { export function require(s: string) { return () => 2 } }\nexport import /* compat */ require = Loader.require\nexport function run() { const work = require('../a/one'); return work() }"},
		"inner import alias with comment":  {body: "namespace Loader { export function other() { return 2 } }\nnamespace N { import /* alias */ work = Loader.other; export function run() { return work() } }", assertLocalFrom: "app/main$EXT::run"},
		"using declaration require":        {body: "export function run(mk: any) { using require = mk(); const work = require('../a/one'); return work() }"},
		"await using declaration require":  {body: "export async function run(mk: any) { await using require = mk(); const work = require('../a/one'); return work() }"},
		"using declaration work":           {body: "export function run(mk: any) { using work = mk(); return work() }", assertLocalFrom: "app/main$EXT::run"},
		"await using declaration work":     {body: "export async function run(mk: any) { await using work = mk(); return work() }", assertLocalFrom: "app/main$EXT::run"},
		"multi using first work":           {body: "export function run(mk: any) { using work = mk(), b = mk(); return work() }", assertLocalFrom: "app/main$EXT::run"},
		"multi using second work":          {body: "export function run(mk: any) { using a = mk(), work = mk(); return work() }", assertLocalFrom: "app/main$EXT::run"},
		"multi await using second work":    {body: "export async function run(mk: any) { await using a = mk(), work = mk(); return work() }", assertLocalFrom: "app/main$EXT::run"},
		"triple using require":             {body: "export function run(mk: any) { using a = mk(), b = mk(), require = mk(); const work = require('../a/one'); return work() }"},
		"for multi using work":             {body: "export function run(mk: any) { for (using a = mk(), work = mk(); false;) { return work() } }", assertLocalFrom: "app/main$EXT::run"},
	}
	for name, tc := range tsCases {
		for _, ext := range []string{".ts", ".tsx"} {
			t.Run(ext+"/"+name, func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"a/one.ts":       "export function work() { return 1 }",
					"app/main" + ext: "import { work } from '../a/one'\n" + tc.body + "\n",
				})
				New(g).ResolveAll()
				require.Empty(t, callsInto(g, "a/one.ts::work"))
				if tc.assertLocalFrom != "" {
					from := strings.ReplaceAll(tc.assertLocalFrom, "$EXT", ext)
					call := unresolvedCall(t, g, from, "work")
					require.Equal(t, graph.JSLexicalLocal, call.Meta[graph.JSLexicalBindingMeta])
				}
			})
		}
	}

	for _, ext := range []string{".ts", ".tsx"} {
		for name, body := range map[string]string{
			"using require binds":              "export function run() { using work = require('../a/one'); return work() }",
			"await using require binds":        "export async function run() { await using work = require('../a/one'); return work() }",
			"multi using second require binds": "export function run(mk: any) { using a = mk(), work = require('../a/one'); return work() }",
		} {
			t.Run(ext+"/"+name, func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"a/one.ts":       "export function work() { return 1 }",
					"app/main" + ext: body + "\n",
				})
				New(g).ResolveAll()
				calls := callsInto(g, "a/one.ts::work")
				require.Len(t, calls, 1)
				require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
			})
		}
	}

	jsCases := map[string]string{
		"using declaration":        "export function run(mk) { using require = mk(); const work = require('../a/one'); return work() }",
		"await using declaration":  "export async function run(mk) { await using require = mk(); const work = require('../a/one'); return work() }",
		"using shadows work":       "export function run(mk) { using work = mk(); return work() }",
		"await using shadows work": "export async function run(mk) { await using work = mk(); return work() }",
	}
	for name, body := range jsCases {
		t.Run("js/"+name, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"a/one.js":    "export function work() { return 1 }",
				"app/main.js": "import { work } from '../a/one'\n" + body + "\n",
			})
			New(g).ResolveAll()
			require.Empty(t, callsInto(g, "a/one.js::work"))
		})
	}
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

func buildRawStore(t *testing.T, backend string, files map[string]string, prefixRepo func(filePath string) (repo, ws string)) graph.Store {
	t.Helper()
	var store graph.Store = graph.New()
	if backend == "sqlite" {
		sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = sq.Close() })
		store = sq
	}
	nodes, edges := extractRawSources(t, files)
	if prefixRepo != nil {
		for _, n := range nodes {
			if n != nil {
				repo, ws := prefixRepo(n.FilePath)
				n.RepoPrefix = repo
				n.WorkspaceID = ws
			}
		}
	}
	store.AddBatch(nodes, edges)
	return store
}

func TestImportEvidence_LocalRequireCommonJSBarrels(t *testing.T) {
	t.Run("three review shapes and negative controls", func(t *testing.T) {
		shapes := map[string]struct {
			barrel string
			target string
		}{
			"object_literal_require": {
				barrel: "module.exports = { foo: require('./foo') }\n",
				target: "lib/foo$EXT::foo",
			},
			"exports_prop_require_member": {
				barrel: "exports.foo = require('./foo').foo\n",
				target: "lib/foo$EXT::foo",
			},
			"module_exports_direct_require": {
				barrel: "module.exports = require('./impl')\n",
				target: "lib/impl$EXT::foo",
			},
		}
		for name, tc := range shapes {
			for _, ext := range []string{".js", ".ts"} {
				for _, backend := range []string{"memory", "sqlite"} {
					t.Run(name+"/"+ext+"/"+backend, func(t *testing.T) {
						files := map[string]string{
							"aaa/foo" + ext:   "export function foo() { return 1 }\n",
							"zzz/foo" + ext:   "export function foo() { return 2 }\n",
							"app/foo" + ext:   "export function foo() { return 3 }\n",
							"lib/foo" + ext:   "function foo() { return 4 }\nmodule.exports = { foo }\n",
							"lib/impl" + ext:  "function foo() { return 5 }\nmodule.exports = { foo }\n",
							"lib/index" + ext: tc.barrel,
							"app/main" + ext:  "function foo() { return 99 }\nexport function run() { const { foo } = require('../lib'); return foo() }\n",
						}
						store := buildRawStore(t, backend, files, nil)
						New(store).ResolveAll()
						want := strings.ReplaceAll(tc.target, "$EXT", ext)
						calls := callsInto(store, want)
						require.Len(t, calls, 1, "must bind through barrel to %s", want)
						require.Equal(t, "app/main"+ext+"::run", calls[0].From)
						require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
						require.Empty(t, callsInto(store, "aaa/foo"+ext+"::foo"))
						require.Empty(t, callsInto(store, "zzz/foo"+ext+"::foo"))
						require.Empty(t, callsInto(store, "app/foo"+ext+"::foo"))
						require.Empty(t, callsInto(store, "app/main"+ext+"::foo"))
					})
				}
			}
		}

		// Negative controls: internal-only require and shadowed module/exports must NOT re-export.
		negatives := map[string]string{
			"internal_only_require": "const helper = require('./impl')\nexport function other() { return helper.foo() }\n",
			"shadowed_module":       "const module = { exports: {} }\nmodule.exports = require('./impl')\n",
			"shadowed_exports":      "const exports = {}\nexports.foo = require('./impl').foo\n",
		}
		for name, barrelSrc := range negatives {
			t.Run("negative/"+name, func(t *testing.T) {
				store := buildRawStore(t, "memory", map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": barrelSrc,
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				}, nil)
				New(store).ResolveAll()
				require.Nil(t, findCallEdge(store, "app/main.js::run", "lib/impl.js::foo"))
				unresolvedCall(t, store, "app/main.js::run", "foo")
			})
		}
	})

	t.Run("two-step CJS barrels, hybrid ESM+CJS, spread, Object.assign, freeze, conditional, TS export=", func(t *testing.T) {
		cases := map[string]struct {
			files map[string]string
			from  string
			want  string
		}{
			"const_2step_nested_cjs": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/mid.js":   "const foo = require('./impl').foo\nmodule.exports = { foo }\n",
					"lib/index.js": "const { foo } = require('./mid')\nmodule.exports = { foo }\n",
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"let_var_2step_direct_mid": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/mid.js":   "module.exports = require('./impl')\n",
					"lib/index.js": "var m = require('./mid')\nlet foo = m.foo\nmodule.exports = { foo }\n",
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"cts_ts_2step": {
				files: map[string]string{
					"lib/impl.cts":  "export function foo() { return 1 }\n",
					"lib/index.cts": "const impl = require('./impl')\nmodule.exports = { foo: impl.foo }\n",
					"app/main.cts":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.cts::run", want: "lib/impl.cts::foo",
			},
			"cjs_wrapping_es_barrel": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/es.js":    "export { foo } from './impl'\n",
					"lib/index.js": "const { foo } = require('./es')\nmodule.exports = { foo }\n",
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"hybrid_esm_cjs_2step": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": "export const foo = require('./impl').foo\nmodule.exports = { foo }\n",
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"es_barrel_over_aliased_cjs_barrel": {
				files: map[string]string{
					"lib/deep.js":       "export function baz() { return 1 }\n",
					"lib/cjs_barrel.js": "module.exports = require('./deep')\n",
					"lib/index.js":      "export { baz as foo } from './cjs_barrel'\n",
					"app/main.js":       "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/deep.js::baz",
			},
			"module_exports_direct_member_no_empty_alias": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": "const impl = require('./impl')\nmodule.exports = impl.foo\n",
					"app/main.js":  "export function run() { const foo = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"module_exports_direct_require_member": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": "module.exports = require('./impl').foo\n",
					"app/main.js":  "export function run() { const foo = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"module_exports_prop": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": "module.exports.foo = require('./impl').foo\n",
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"spread_require": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": "module.exports = { ...require('./impl') }\n",
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"object_assign_exports": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": "Object.assign(exports, require('./impl'))\n",
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"object_freeze_exports": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": "module.exports = Object.freeze({ foo: require('./impl').foo })\n",
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"conditional_exports": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": "if (process.env.A) { module.exports = require('./impl') }\n",
					"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
			"ts_export_equals_direct": {
				files: map[string]string{
					"lib/impl.ts":  "export function foo() { return 1 }\n",
					"lib/index.ts": "export = require('./impl')\n",
					"app/main.ts":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.ts::run", want: "lib/impl.ts::foo",
			},
			"ts_import_require_export_equals": {
				files: map[string]string{
					"lib/impl.ts":  "export function foo() { return 1 }\n",
					"lib/index.ts": "import impl = require('./impl')\nexport = impl\n",
					"app/main.ts":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
				},
				from: "app/main.ts::run", want: "lib/impl.ts::foo",
			},
			"es_import_and_module_require_parity": {
				files: map[string]string{
					"lib/impl.js":  "export function foo() { return 1 }\n",
					"lib/index.js": "const foo = require('./impl').foo\nmodule.exports = { foo }\n",
					"app/main.js":  "import { foo } from '../lib'\nexport function run() { return foo() }\n",
				},
				from: "app/main.js::run", want: "lib/impl.js::foo",
			},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				store := buildRawStore(t, "memory", tc.files, nil)
				New(store).ResolveAll()
				calls := callsInto(store, tc.want)
				require.Len(t, calls, 1, "expected binding to %s", tc.want)
				require.Equal(t, tc.from, calls[0].From)
				// Ensure no EdgeReExports carries an empty import_alias key.
				for _, e := range store.GetOutEdges("lib/index.js") {
					if e.Kind == graph.EdgeReExports {
						if a, ok := e.Meta["import_alias"]; ok {
							require.NotEmpty(t, a)
						}
					}
				}
			})
		}

		// Same-file call inside a 2-step CJS barrel must not bind to an unrelated same-dir symbol.
		t.Run("same_file_call_inside_2step_cjs_barrel", func(t *testing.T) {
			store := buildRawStore(t, "memory", map[string]string{
				"lib/other.js": "export function foo() { return 99 }\n",
				"lib/foo.js":   "export function foo() { return 1 }\n",
				"lib/index.js": "const foo = require('./foo')\nfunction bar() { return foo() }\nmodule.exports = { foo, bar }\n",
			}, nil)
			New(store).ResolveAll()
			require.Empty(t, callsInto(store, "lib/other.js::foo"))
		})

		// Anonymous-default / external CJS re-export falls back to the barrel's KindVariable binding.
		t.Run("external_cjs_fallback_to_binding_node", func(t *testing.T) {
			store := buildRawStore(t, "memory", map[string]string{
				"lib/index.js": "const ext = require('unindexed-ext')\nmodule.exports = { ext }\n",
				"app/main.js":  "export function run() { const { ext } = require('../lib'); return ext() }\n",
			}, nil)
			New(store).ResolveAll()
			calls := callsInto(store, "lib/index.js::ext")
			require.Len(t, calls, 1)
			require.Equal(t, "app/main.js::run", calls[0].From)
		})
	})

	t.Run("same-line multi-binding reexport_bindings and 3-node ES forwarder cycle", func(t *testing.T) {
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(backend, func(t *testing.T) {
				files := map[string]string{
					"lib/impl.js":        "export function foo() { return 1 }\nexport function bar() { return 2 }\n",
					"lib/cjs_barrel.js":  "const m = require('./impl'); module.exports = { foo: m.foo, bar: m.bar }\n",
					"lib/es_multi.js":    "export { foo, bar } from './cjs_barrel'\n",
					"lib/es_dupalias.js": "export { foo as a, foo as b } from './impl'\n",
					"lib/cjs_dupalias.js": "const m = require('./impl'); module.exports = { a: m.foo, b: m.foo }\n",
					"lib/cjs_mix.js":     "module.exports = { ...require('./cjs_barrel'), a: require('./cjs_barrel').foo }\n",
					"lib/cyc_a.js":       "export { foo } from './cyc_b'\n",
					"lib/cyc_b.js":       "export { foo } from './cyc_c'\n",
					"lib/cyc_c.js":       "export { foo } from './cyc_b'\n",
					"app/main.js": strings.Join([]string{
						"export function r1() { const { foo, bar } = require('../lib/es_multi'); return foo() + bar() }",
						"export function r2() {",
						"  const { a, b } = require('../lib/es_dupalias')",
						"  const x = a()",
						"  return x + b()",
						"}",
						"export function r3() {",
						"  const { a, b } = require('../lib/cjs_dupalias')",
						"  const x = a()",
						"  return x + b()",
						"}",
						"export function r4() { const { a, bar } = require('../lib/cjs_mix'); return a() + bar() }",
						"export function r5() { const { foo } = require('../lib/cyc_a'); return foo() }",
					}, "\n") + "\n",
				}
				store := buildRawStore(t, backend, files, nil)
				New(store).ResolveAll()
				require.NotNil(t, findCallEdge(store, "app/main.js::r1", "lib/impl.js::foo"))
				require.NotNil(t, findCallEdge(store, "app/main.js::r1", "lib/impl.js::bar"))
				require.Len(t, findAllCallsTo(store, "app/main.js::r2", "lib/impl.js::foo"), 2)
				require.Len(t, findAllCallsTo(store, "app/main.js::r3", "lib/impl.js::foo"), 2)
				require.NotNil(t, findCallEdge(store, "app/main.js::r4", "lib/impl.js::foo"))
				require.NotNil(t, findCallEdge(store, "app/main.js::r4", "lib/impl.js::bar"))
				unresolvedCall(t, store, "app/main.js::r5", "foo")
				rCycle := New(store)
				cycRes := pickDirectReExportCandidate(jsRequireEnv{
					getNode:         rCycle.requireGetNode,
					outEdges:        rCycle.callerOutEdges,
					resolveReExport: func(re *graph.Edge) (string, string) { return rCycle.importedTargetOf(re), "" },
					eligible:        func(n *graph.Node) bool { return n != nil },
				}, "lib/cyc_a.js", "foo", 0, &reExportWalkState{})
				require.Nil(t, cycRes.pick, "3-node ES forwarder cycle must return nil pick")
				require.True(t, cycRes.cycleHit, "3-node ES forwarder cycle must terminate via inStack cycleHit")
				require.False(t, cycRes.depthTruncated, "3-node ES forwarder cycle must not hit depth-8 truncation")
			})
		}
	})

	t.Run("precedence and diamond topologies", func(t *testing.T) {
		// Wildcard siblings: private helper in a.js vs exported foo in b.js -> binds b.js::foo.
		t.Run("wildcard_siblings_exported_beats_private", func(t *testing.T) {
			store := buildRawStore(t, "memory", map[string]string{
				"lib/a.js":     "function foo() { return 1 }\n",
				"lib/b.js":     "export function foo() { return 2 }\n",
				"lib/index.js": "export * from './a'\nexport * from './b'\n",
				"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
			}, nil)
			New(store).ResolveAll()
			require.Len(t, callsInto(store, "lib/b.js::foo"), 1)
			require.Empty(t, callsInto(store, "lib/a.js::foo"))
		})

		// Nested barrel private helper must not shadow `export * from './impl'`.
		t.Run("nested_barrel_private_helper_does_not_shadow_wildcard", func(t *testing.T) {
			store := buildRawStore(t, "memory", map[string]string{
				"lib/impl.js":  "export function init() { return 1 }\n",
				"lib/mid.js":   "function init() { return 2 }\nexport * from './impl'\n",
				"lib/index.js": "export * from './mid'\n",
				"app/main.js":  "export function run() { const { init } = require('../lib'); return init() }\n",
			}, nil)
			New(store).ResolveAll()
			require.Len(t, callsInto(store, "lib/impl.js::init"), 1)
			require.Empty(t, callsInto(store, "lib/mid.js::init"))
		})

		// Mixed CJS leaf + wildcard spread where both define unmarked foo -> ambiguous/unresolved.
		t.Run("mixed_cjs_leaf_plus_spread_ambiguous", func(t *testing.T) {
			store := buildRawStore(t, "memory", map[string]string{
				"lib/bar.js":   "function foo() { return 1 }\n",
				"lib/index.js": "function foo() { return 2 }\nmodule.exports = { foo, ...require('./bar') }\n",
				"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
			}, nil)
			New(store).ResolveAll()
			unresolvedCall(t, store, "app/main.js::run", "foo")
		})

		// Diamond barrel + 2-step CJS inner-barrel diamond + wildcard+named exact promotion diamond.
		t.Run("diamonds_converge_to_single_canonical", func(t *testing.T) {
			store := buildRawStore(t, "memory", map[string]string{
				"lib/impl.js":   "export function foo() { return 1 }\n",
				"lib/a.js":      "export * from './impl'\n",
				"lib/b.js":      "export * from './impl'\n",
				"lib/diam.js":   "export * from './a'\nexport * from './b'\n",
				"lib/mid.js":    "const foo = require('./impl').foo\nmodule.exports = { foo }\n",
				"lib/ca.js":     "module.exports = require('./mid')\n",
				"lib/cb.js":     "exports.foo = require('./mid').foo\n",
				"lib/cdiam.js":  "module.exports = { ...require('./ca'), ...require('./cb') }\n",
				"lib/priv.js":   "function foo() { return 99 }\n",
				"lib/wa.js":     "export * from './impl'\n",
				"lib/wb.js":     "exports.foo = require('./impl').foo\n",
				"lib/wdiam.js":  "module.exports = { ...require('./wa'), ...require('./wb'), ...require('./priv') }\n",
				"app/main.js": strings.Join([]string{
					"export function r1() { const { foo } = require('../lib/diam'); return foo() }",
					"export function r2() { const { foo } = require('../lib/cdiam'); return foo() }",
					"export function r3() { const { foo } = require('../lib/wdiam'); return foo() }",
				}, "\n") + "\n",
			}, nil)
			New(store).ResolveAll()
			require.NotNil(t, findCallEdge(store, "app/main.js::r1", "lib/impl.js::foo"))
			require.NotNil(t, findCallEdge(store, "app/main.js::r2", "lib/impl.js::foo"))
			require.NotNil(t, findCallEdge(store, "app/main.js::r3", "lib/impl.js::foo"))
		})

		// Multi-name aliased diamond pointing to two distinct symbols -> ambiguous.
		t.Run("multi_name_aliased_diamond_ambiguous", func(t *testing.T) {
			store := buildRawStore(t, "memory", map[string]string{
				"lib/mid.js":   "export function a() { return 1 }\nexport function b() { return 2 }\n",
				"lib/p1.js":    "export { a as x } from './mid'\n",
				"lib/p2.js":    "export { b as x } from './mid'\n",
				"lib/index.js": "export * from './p1'\nexport * from './p2'\n",
				"app/main.js":  "export function run() { const { x } = require('../lib'); return x() }\n",
			}, nil)
			New(store).ResolveAll()
			unresolvedCall(t, store, "app/main.js::run", "x")
		})
	})

	t.Run("aliased re-exports, export * as ns, cross-repo bare-dir, npm-alias, innermost npm spec, staleness", func(t *testing.T) {
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run("aliased_and_namespace/"+backend, func(t *testing.T) {
				files := map[string]string{
					"lib/impl.js":     "export function foo() { return 1 }\n",
					"lib/mid.js":      "exports.bar = require('./impl').foo\n",
					"lib/es_alias.js": "export { foo as bar } from './impl'\n",
					"lib/wild.js":     "module.exports = require('./mid')\n",
					"lib/ns.js":       "export * as ns from './impl'\n",
					"app/main.js": strings.Join([]string{
						"export function r1() { const { bar } = require('../lib/mid'); return bar() }",
						"export function r2() { const { bar } = require('../lib/es_alias'); return bar() }",
						"export function r3() { const { bar } = require('../lib/wild'); return bar() }",
						"export function r4() { const { foo } = require('../lib/ns'); return foo() }",
					}, "\n") + "\n",
				}
				store := buildRawStore(t, backend, files, nil)
				New(store).ResolveAll()
				require.NotNil(t, findCallEdge(store, "app/main.js::r1", "lib/impl.js::foo"))
				require.NotNil(t, findCallEdge(store, "app/main.js::r2", "lib/impl.js::foo"))
				require.NotNil(t, findCallEdge(store, "app/main.js::r3", "lib/impl.js::foo"))
				unresolvedCall(t, store, "app/main.js::r4", "foo")
			})
		}

		// Cross-repo chained tracked-npm barrel (app -> @acme/shared -> prov("p")):
		// innermost "p" is stamped in NpmImportSpecifierMeta, zero churn on pass 2,
		// and aliased barrel export retarget/removal restubs cleanly.
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run("chained_tracked_npm_innermost_spec_and_staleness/"+backend, func(t *testing.T) {
				files := map[string]string{
					"prov/index.js":   "export function foo() { return 1 }\nexport function qux() { return 2 }\nexport function other() { return 3 }\n",
					"shared/index.js": "exports.bar = require('p').foo\nexports.other = require('p').other\n",
					"app/main.js":     "export function run() { const { bar: baz } = require('@acme/shared'); return baz() }\n",
				}
				store := buildRawStore(t, backend, files, func(fp string) (string, string) {
					repo, _, _ := strings.Cut(fp, "/")
					return repo, "ws"
				})
				npmDep := func(file, spec string) bool {
					if strings.HasPrefix(file, "shared/") {
						return spec == "p"
					}
					if strings.HasPrefix(file, "app/") {
						return spec == "@acme/shared"
					}
					return false
				}
				r := New(store)
				r.SetNpmDependencyLookup(npmDep)
				r.ResolveAll()

				cr := NewCrossRepo(store)
				cr.SetNpmDependencyLookup(npmDep)
				cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
					switch spec {
					case "p":
						return map[string]NpmPackageTarget{"prov": {WorkspaceID: "ws", Entries: []string{"prov/index.js"}}}
					case "@acme/shared":
						return map[string]NpmPackageTarget{"shared": {WorkspaceID: "ws", Entries: []string{"shared/index.js"}}}
					}
					return nil
				})
				cr.ResolveAll()

				call := findCallEdge(store, "app/main.js::run", "prov/index.js::foo")
				require.NotNil(t, call, "chained tracked-npm barrel must resolve to prov/index.js::foo")
				require.Equal(t, "p", call.Meta[NpmImportSpecifierMeta], "innermost terminal package spec must be stamped")

				// Second pass must have zero churn.
				stats2 := cr.ResolveAll()
				require.Zero(t, stats2.Resolved)
				require.NotNil(t, findCallEdge(store, "app/main.js::run", "prov/index.js::foo"))

				// Now retarget shared/index.js's exports.bar from foo -> qux (leaving exports.other -> other).
				store.EvictFile("shared/index.js")
				sNodes, sEdges := extractRawSources(t, map[string]string{
					"shared/index.js": "exports.bar = require('p').qux\nexports.other = require('p').other\n",
				})
				for _, n := range sNodes {
					n.RepoPrefix = "shared"
					n.WorkspaceID = "ws"
				}
				store.AddBatch(sNodes, sEdges)
				cr.ResolveAll()
				require.NotNil(t, findCallEdge(store, "app/main.js::run", "prov/index.js::qux"),
					"stale check must restub and rebind to prov/index.js::qux")

				// Now remove exports.bar completely while keeping exports.other -> other: must restub to unresolved::baz.
				store.EvictFile("shared/index.js")
				sNodes2, sEdges2 := extractRawSources(t, map[string]string{
					"shared/index.js": "exports.other = require('p').other\n",
				})
				for _, n := range sNodes2 {
					n.RepoPrefix = "shared"
					n.WorkspaceID = "ws"
				}
				store.AddBatch(sNodes2, sEdges2)
				cr.ResolveAll()
				unresolvedCall(t, store, "app/main.js::run", "baz")
			})
		}

		// Cross-repo bare-dir named EdgeReExports in same-chunk / 2-phase resolveScopedLocked on SQLite:
		// barrel, ES caller, and require() caller all live in repo "app" so Phase 1 resolves
		// importPending (EdgeReExports + EdgeImports), flushes & clears require cache, and Phase 2
		// rebuilds reachableReposByFile and resolves restPending in the same scoped call.
		t.Run("cross_repo_bare_dir_reexport_2phase_scoped_sqlite", func(t *testing.T) {
			files := map[string]string{
				"shared_pkg/index.js": "export function foo() { return 1 }\n",
				"app/barrel.js":       "exports.foo = require('shared_pkg').foo\n",
				"app/es_caller.js":    "import { foo } from 'shared_pkg'\nexport function esRun() { return foo() }\n",
				"app/main.js":         "export function run() { const { foo } = require('./barrel'); return foo() }\n",
			}
			store := buildRawStore(t, "sqlite", files, func(fp string) (string, string) {
				repo, _, _ := strings.Cut(fp, "/")
				return repo, "ws"
			})
			cr := NewCrossRepo(store)
			cr.ResolveForRepo("app")
			foundResolvedReExport := false
			for _, e := range store.GetOutEdges("app/barrel.js") {
				if e != nil && e.Kind == graph.EdgeReExports && e.To == "shared_pkg/index.js::foo" {
					foundResolvedReExport = true
				}
			}
			require.True(t, foundResolvedReExport, "Phase 1 must resolve app/barrel.js EdgeReExports to shared_pkg/index.js::foo")
			require.NotNil(t, findCallEdge(store, "app/main.js::run", "shared_pkg/index.js::foo"),
				"Phase 2 must resolve app/main.js require('./barrel') after Phase 1 flush & cache clear")
			require.NotNil(t, findCallEdge(store, "app/es_caller.js::esRun", "shared_pkg/index.js::foo"),
				"Phase 2 must build reachableReposByFile seeing Phase 1 resolved EdgeImports")
		})

		// npm-aliased dep ("p-alias" -> "p") must NOT misbind to local .../p/index.js.
		t.Run("npm_alias_dep_never_misbinds_local_dir", func(t *testing.T) {
			files := map[string]string{
				"app/p/index.js": "export function foo() { return 1 }\n",
				"app/main.js":    "import { foo as impFoo } from 'p-alias'\nexport { foo } from 'p-alias'\nexport function run() { const { foo } = require('p-alias'); return foo() + impFoo() }\n",
			}
			store := buildRawStore(t, "memory", files, func(string) (string, string) { return "app", "ws" })
			npmDep := func(_, spec string) bool { return spec == "p-alias" }
			npmAlias := func(_, spec string) string {
				if spec == "p-alias" {
					return "p"
				}
				return ""
			}
			assertExternalOnly := func(phase string) {
				t.Helper()
				require.Empty(t, callsInto(store, "app/p/index.js::foo"), "%s: call must not bind local app/p/index.js::foo", phase)
				foundExtReExport := false
				foundExtImport := false
				for _, e := range store.GetOutEdges("app/main.js") {
					if e == nil {
						continue
					}
					if e.Kind == graph.EdgeImports || e.Kind == graph.EdgeReExports {
						require.NotEqual(t, "app/p/index.js", e.To, "%s: %s must not misbind to local app/p/index.js", phase, e.Kind)
						require.NotEqual(t, "app/p/index.js::foo", e.To, "%s: %s must not misbind to local app/p/index.js::foo", phase, e.Kind)
					}
					if e.Kind == graph.EdgeReExports && e.To == "external::p-alias::foo" {
						foundExtReExport = true
					}
					if e.Kind == graph.EdgeImports && (e.To == "external::p-alias" || e.To == "external::p-alias::foo") {
						foundExtImport = true
					}
				}
				require.True(t, foundExtReExport, "%s: EdgeReExports must target external::p-alias::foo", phase)
				require.True(t, foundExtImport, "%s: EdgeImports must target external::p-alias", phase)
			}
			r := New(store)
			r.SetNpmDependencyLookup(npmDep)
			r.SetNpmAliasResolver(npmAlias)
			r.ResolveAll()
			assertExternalOnly("per-repo Resolver")

			cr := NewCrossRepo(store)
			cr.SetNpmDependencyLookup(npmDep)
			cr.SetNpmAliasResolver(npmAlias)
			cr.ResolveAll()
			assertExternalOnly("CrossRepoResolver")
		})
	})

	t.Run("single-repo cross-repo guard, pass-cache hygiene, and JSLexicalRequire terminal parity", func(t *testing.T) {
		// Single-repo Resolver must not steal a cross-repo tracked-npm require with CrossRepo = false.
		t.Run("single_repo_resolver_cross_repo_guard", func(t *testing.T) {
			store := buildRawStore(t, "memory", map[string]string{
				"prov/index.js": "export function foo() { return 1 }\n",
				"app/main.js":   "export function run() { const { foo } = require('../prov'); return foo() }\n",
			}, func(fp string) (string, string) {
				repo, _, _ := strings.Cut(fp, "/")
				return repo, "ws"
			})
			New(store).ResolveAll()
			require.Empty(t, callsInto(store, "prov/index.js::foo"), "single-repo Resolver must not bind cross-repo require()")
		})

		// Reused Resolver clears import evidence caches between ResolveIncomingForNames and ResolveFilesOutgoing.
		t.Run("reused_resolver_pass_cache_hygiene", func(t *testing.T) {
			store := buildRawStore(t, "memory", map[string]string{
				"lib/index.js": "export function other() { return 1 }\n",
				"app/main.js":  "export function run() { const { foo } = require('../lib'); return foo() }\n",
			}, nil)
			r := New(store)
			// Visit unresolved::foo so requireNodeByID caches negative nil for lib/index.js::foo;
			// clearPassIndexes() must flush clearImportEvidence() before ResolveFilesOutgoing runs.
			r.ResolveIncomingForNames([]string{"foo"}, nil)
			// Add foo to lib/index.js after the first pass.
			store.AddNode(&graph.Node{ID: "lib/index.js::foo", Kind: graph.KindFunction, Name: "foo", FilePath: "lib/index.js", Language: "javascript", Meta: map[string]any{"visibility": "public"}})
			r.ResolveFilesOutgoing([]string{"app/main.js"})
			require.NotNil(t, findCallEdge(store, "app/main.js::run", "lib/index.js::foo"))
		})

		// JSLexicalRequire terminal-classification parity across memory + SQLite.
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run("terminal_parity/"+backend, func(t *testing.T) {
				store := buildRawStore(t, backend, map[string]string{
					"app/main.js": "export function run() { const { missing } = require('future-pkg'); return missing() }\n",
				}, nil)
				r := New(store)
				r.SetStampTerminal(true)
				r.ResolveAll()
				call := unresolvedCall(t, store, "app/main.js::run", "missing")
				require.False(t, edgeTerminalFlag(call), "JSLexicalRequire must never be stamped terminal by ResolveAll")

				reason, term := r.classifyTerminal(call)
				require.False(t, term)
				require.Empty(t, reason)

				stamped, _ := r.reconcileTerminalStamps()
				require.Zero(t, stamped)
				call = unresolvedCall(t, store, "app/main.js::run", "missing")
				require.False(t, edgeTerminalFlag(call), "reconcileTerminalStamps must never stamp JSLexicalRequire")
			})
		}
	})
}

func findAllCallsTo(g graph.Store, from, to string) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.GetOutEdges(from) {
		if e != nil && e.Kind == graph.EdgeCalls && e.To == to {
			out = append(out, e)
		}
	}
	return out
}

func TestImportEvidence_LocalRequireRenamedDestructuringAndMember(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run("local_shapes/"+backend, func(t *testing.T) {
			files := map[string]string{
				"lib/foo.js":   "export function foo() { return 1 }\n",
				"lib/index.js": "exports.foo = require('./foo').foo\n",
				"app/main.js": strings.Join([]string{
					"export function r1() { const { foo: bar } = require('../lib/foo'); return bar() }",
					"export function r2() { const { foo: bar = () => 0 } = require('../lib/foo'); return bar() }",
					"export function r3() { const { \"foo\": bar } = require('../lib/foo'); return bar() }",
					"export function r4() { const bar = require('../lib/foo').foo; return bar() }",
					"export function r5() { const { foo: bar } = require('../lib'); return bar() }",
					"export function r6() { const bar = require('../lib').foo; return bar() }",
				}, "\n") + "\n",
			}
			store := buildRawStore(t, backend, files, nil)
			New(store).ResolveAll()
			for _, fn := range []string{"r1", "r2", "r3", "r4", "r5", "r6"} {
				call := findCallEdge(store, "app/main.js::"+fn, "lib/foo.js::foo")
				require.NotNil(t, call, "expected %s to bind lib/foo.js::foo", fn)
				require.Equal(t, "bar", call.Meta[graph.JSLexicalRequireLocalMeta])
				require.Equal(t, "foo", call.Meta[graph.JSLexicalRequireSymbolMeta])
			}
		})

		t.Run("tracked_npm_retarget_and_ambiguity_restub_to_local_name/"+backend, func(t *testing.T) {
			files := map[string]string{
				"provA/index.ts": "export function foo() { return 1 }\n",
				"provB/index.ts": "export function foo() { return 2 }\n",
				"app/main.ts":    "export function run() { const { foo: bar } = require('p'); return bar() }\n",
			}
			store := buildRawStore(t, backend, files, func(fp string) (string, string) {
				repo, _, _ := strings.Cut(fp, "/")
				return repo, "ws"
			})
			providers := map[string]NpmPackageTarget{
				"provA": {WorkspaceID: "ws", Entries: []string{"provA/index.ts"}},
			}
			cr := NewCrossRepo(store)
			cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
			cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
				if spec != "p" {
					return nil
				}
				return providers
			})
			cr.ResolveAll()
			call := findCallEdge(store, "app/main.ts::run", "provA/index.ts::foo")
			require.NotNil(t, call)

			// Make ambiguous -> must restub to unresolved::bar (the local name), NOT unresolved::foo.
			providers["provB"] = NpmPackageTarget{WorkspaceID: "ws", Entries: []string{"provB/index.ts"}}
			cr.ResolveAll()
			restubbed := unresolvedCall(t, store, "app/main.ts::run", "bar")
			require.Nil(t, findCallEdge(store, "app/main.ts::run", graph.UnresolvedMarker+"foo"))
			require.NotContains(t, restubbed.Meta, NpmImportSpecifierMeta)

			// Remove provA -> rebinds to provB/index.ts::foo.
			delete(providers, "provA")
			cr.ResolveAll()
			require.NotNil(t, findCallEdge(store, "app/main.ts::run", "provB/index.ts::foo"))
		})
	}
}

type cancelInChunkStore struct {
	graph.Store
	cancel     context.CancelFunc
	cancelOnce atomic.Bool
	evictID    string
	evictOnce  atomic.Bool
}

func (s *cancelInChunkStore) GetNode(id string) *graph.Node {
	if s.cancel != nil && strings.HasPrefix(id, "prov/") && s.cancelOnce.CompareAndSwap(false, true) {
		s.cancel()
	}
	return s.Store.GetNode(id)
}

func (s *cancelInChunkStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	if s.evictID != "" && slices.Contains(ids, s.evictID) && s.evictOnce.CompareAndSwap(false, true) {
		// Simulate target eviction right before liveness validation.
		out := s.Store.GetNodesByIDs(ids)
		delete(out, s.evictID)
		return out
	}
	return s.Store.GetNodesByIDs(ids)
}

func TestCrossRepoResolveAllMidChunkCancelRevertsInMemory(t *testing.T) {
	t.Run("mid_chunk_cancel_lossless_revert_and_zero_side_effect_leak", func(t *testing.T) {
		g := graph.New()
		g.AddBatch([]*graph.Node{
			{ID: "app/main.js", Kind: graph.KindFile, Name: "main.js", FilePath: "app/main.js", Language: "javascript", RepoPrefix: "app", WorkspaceID: "ws"},
			{ID: "app/main.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/main.js", Language: "javascript", RepoPrefix: "app", WorkspaceID: "ws"},
			{ID: "prov/index.js", Kind: graph.KindFile, Name: "index.js", FilePath: "prov/index.js", Language: "javascript", RepoPrefix: "prov", WorkspaceID: "ws"},
			{ID: "prov/index.js::foo", Kind: graph.KindFunction, Name: "foo", FilePath: "prov/index.js", Language: "javascript", RepoPrefix: "prov", WorkspaceID: "ws", Meta: map[string]any{"visibility": "public"}},
			{ID: "goapp/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "goapp/a.go", Language: "go", RepoPrefix: "goapp"},
			{ID: "goapp/a.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "goapp/a.go", Language: "go", RepoPrefix: "goapp"},
		}, []*graph.Edge{
			{
				From: "app/main.js::run", To: graph.UnresolvedMarker + "foo", Kind: graph.EdgeCalls,
				FilePath: "app/main.js", Line: 1,
				Meta: map[string]any{
					graph.JSLexicalBindingMeta:      graph.JSLexicalRequire,
					graph.JSLexicalRequireSpecMeta:  "p",
					graph.JSLexicalRequireLocalMeta: "foo",
					"keep":                          "orig",
				},
			},
			{
				From: "goapp/a.go::Fn", To: "unresolved::extern::example.com/missing::Bar", Kind: graph.EdgeCalls,
				FilePath: "goapp/a.go", Line: 2,
				Meta: map[string]any{"go_import": "example.com/missing", "orig_key": "v1"},
			},
			{
				From: "goapp/a.go::Fn", To: "unresolved::extern::example.com/cold::Baz", Kind: graph.EdgeCalls,
				FilePath: "goapp/a.go", Line: 3,
				Meta: map[string]any{"orig_cold": "v2"},
			},
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		wrapped := &cancelInChunkStore{Store: g, cancel: cancel}
		cr := NewCrossRepo(wrapped)
		cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
		cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
			if spec != "p" {
				return nil
			}
			return map[string]NpmPackageTarget{"prov": {WorkspaceID: "ws", Entries: []string{"prov/index.js"}}}
		})
		_, err := cr.ResolveAllContext(ctx)
		require.ErrorIs(t, err, context.Canceled)

		// All in-memory edges must be losslessly restored to their pre-chunk state.
		reqEdge := findCallEdge(g, "app/main.js::run", graph.UnresolvedMarker+"foo")
		require.NotNil(t, reqEdge, "require edge must revert to unresolved::foo")
		require.Equal(t, "orig", reqEdge.Meta["keep"])
		require.NotContains(t, reqEdge.Meta, NpmImportSpecifierMeta)
		require.Empty(t, callsInto(g, "prov/index.js::foo"))

		goEdge1 := findCallEdge(g, "goapp/a.go::Fn", "unresolved::extern::example.com/missing::Bar")
		require.NotNil(t, goEdge1)
		require.Equal(t, "example.com/missing", goEdge1.Meta["go_import"])
		require.Equal(t, "v1", goEdge1.Meta["orig_key"])

		goEdge2 := findCallEdge(g, "goapp/a.go::Fn", "unresolved::extern::example.com/cold::Baz")
		require.NotNil(t, goEdge2)
		require.Equal(t, "v2", goEdge2.Meta["orig_cold"])

		require.Empty(t, cr.TakeDemotedGoModuleCallFiles(), "cancelled chunk must not leak goModuleDemotedFiles")
		require.Empty(t, cr.TakeRetargetedTestCallFiles(), "cancelled chunk must not leak retargetedTestCallFiles")
	})

	t.Run("filterLiveReindex_target_eviction_reverts_both_cloned_and_uncloned_live_edges", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			evictID string
		}{
			{name: "uncloned_call_edge", evictID: "lib/index.js::foo"},
			{name: "cloned_import_edge", evictID: "lib/index.js"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := graph.New()
				g.AddBatch([]*graph.Node{
					{ID: "app/main.js", Kind: graph.KindFile, Name: "main.js", FilePath: "app/main.js", Language: "javascript", RepoPrefix: "app", WorkspaceID: "ws"},
					{ID: "app/main.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/main.js", Language: "javascript", RepoPrefix: "app", WorkspaceID: "ws"},
					{ID: "lib/index.js", Kind: graph.KindFile, Name: "index.js", FilePath: "lib/index.js", Language: "javascript", RepoPrefix: "lib", WorkspaceID: "ws"},
					{ID: "lib/index.js::foo", Kind: graph.KindFunction, Name: "foo", FilePath: "lib/index.js", Language: "javascript", RepoPrefix: "lib", WorkspaceID: "ws", Meta: map[string]any{"visibility": "public"}},
				}, []*graph.Edge{
					{
						From: "app/main.js", To: "unresolved::import::lib", Kind: graph.EdgeImports,
						FilePath: "app/main.js", Line: 1,
						Meta: map[string]any{"orig_imp": true},
					},
					{
						From: "app/main.js::run", To: graph.UnresolvedMarker + "foo", Kind: graph.EdgeCalls,
						FilePath: "app/main.js", Line: 2,
						Meta: map[string]any{
							graph.JSLexicalBindingMeta:      graph.JSLexicalRequire,
							graph.JSLexicalRequireSpecMeta:  "lib",
							graph.JSLexicalRequireLocalMeta: "foo",
							"orig_call":                     true,
						},
					},
				})

				wrapped := &cancelInChunkStore{Store: g, evictID: tc.evictID}
				cr := NewCrossRepo(wrapped)
				cr.ResolveAll()

				if tc.evictID == "lib/index.js::foo" {
					call := findCallEdge(g, "app/main.js::run", graph.UnresolvedMarker+"foo")
					require.NotNil(t, call, "rejected live call edge must revert to unresolved::foo")
					require.Equal(t, true, call.Meta["orig_call"])
					require.False(t, call.CrossRepo)
					require.Empty(t, call.Origin)
					require.Empty(t, callsInto(g, "lib/index.js::foo"))
				} else {
					var imp *graph.Edge
					for _, e := range g.GetOutEdges("app/main.js") {
						if e.Kind == graph.EdgeImports {
							imp = e
						}
					}
					require.NotNil(t, imp)
					require.Equal(t, "unresolved::import::lib", imp.To, "rejected live cloned import edge must revert to unresolved::import::lib")
					require.Equal(t, true, imp.Meta["orig_imp"])
					require.False(t, imp.CrossRepo)
				}
			})
		}
	})

	t.Run("cold_go_extern_without_go_import_does_not_mark_demoted_file", func(t *testing.T) {
		g := graph.New()
		g.AddBatch([]*graph.Node{
			{ID: "goapp/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "goapp/a.go", Language: "go", RepoPrefix: "goapp"},
			{ID: "goapp/a.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "goapp/a.go", Language: "go", RepoPrefix: "goapp"},
		}, []*graph.Edge{
			{
				From: "goapp/a.go::Fn", To: "unresolved::extern::example.com/cold::Baz", Kind: graph.EdgeCalls,
				FilePath: "goapp/a.go", Line: 3,
			},
		})
		cr := NewCrossRepo(g)
		cr.ResolveAll()
		require.Empty(t, cr.TakeDemotedGoModuleCallFiles(), "cold extern without go_import must not mark demoted file")
	})
}
