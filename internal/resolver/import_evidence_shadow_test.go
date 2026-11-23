package resolver

import (
	"path/filepath"
	"strings"
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
		"block let":                   "export function run(get) { { let work = get(); return work() } }",
		"hoisted var":                 "export function run(get, ok) { if (ok) { var work = get() } return work() }",
		"require param zero-arg":      "export function run(require) { const work = require(); return work() }",
		"require param with spec":     "export function run(require) { const work = require('$SPEC'); return work() }",
		"require param destructured":  "export function run(require) { const { work } = require('$SPEC'); return work() }",
		"require param member":        "export function run(require) { const work = require('$SPEC').work; return work() }",
		"require arrow param":         "export const run = require => { const work = require('$SPEC'); return work() }",
		"require outer closure param": "export function outer(require) { return function run() { const { work } = require('$SPEC'); return work() } }",
		"require local const":         "export function run(fn) { const require = fn; const { work } = require('$SPEC'); return work() }",
		"require hoisted var":         "export function run(fn, ok) { const { work } = require('$SPEC'); if (ok) { var require = fn } return work() }",
		"require catch param":         "export function run() { try { return 1 } catch (require) { const work = require('$SPEC'); return work() } }",
		"require for-of binding":      "export function run(xs) { for (const require of xs) { const work = require('$SPEC'); return work() } }",
		"require module function":     "function require() { return () => 2 }\nexport function run() { const work = require('$SPEC'); return work() }",
		"require zero-arg":            "export function run() { const work = require(); return work() }",
		"require dynamic arg":         "export function run(id) { const work = require(id); return work() }",
		"require extra arg":           "export function run() { const work = require('$SPEC', true); return work() }",
		"require renamed destructure": "export function run() { const { other: work } = require('$SPEC'); return work() }",
		"require mismatched member":   "export function run() { const work = require('$SPEC').other; return work() }",
		"require chained member":      "export function run() { const work = require('$SPEC').sub.work; return work() }",
		"require invoked result":      "export function run() { const work = require('$SPEC')(); return work() }",
		"require external package":      "export function run() { const work = require('external-pkg'); return work() }",
		"require conflicting var":       "export function run(get, ok) { if (ok) { var { work } = require('$SPEC') } else { var work = get() } return work() }",
		"require conflicting var rev":   "export function run(get, ok) { if (ok) { var work = get() } else { var { work } = require('$SPEC') } return work() }",
		"require conflicting var specs": "export function run(ok) { if (ok) { var { work } = require('$SPEC') } else { var { work } = require('./other') } return work() }",
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
					g := buildGraphFromSources(t, files)
					store := g
					if backend == "sqlite" {
						sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						defer sq.Close()
						sq.AddBatch(g.AllNodes(), g.AllEdges())
						store = sq
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
		t.Run(ext, func(t *testing.T) {
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
	}
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
	}
}

func TestImportEvidence_LocalRequireResolvesCrossRepo(t *testing.T) {
	t.Run("cross-repo bare dir entry", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"lib/logger/index.js": "export function log() { return 1 }\n",
			"app/web/handler.js":  "export function run() { const { log } = require('lib/logger'); return log() }\n",
		})
		for _, n := range g.AllNodes() {
			if strings.HasPrefix(n.FilePath, "lib/") {
				n.RepoPrefix = "lib"
			} else {
				n.RepoPrefix = "app"
			}
		}
		New(g).ResolveAll()
		require.Empty(t, callsInto(g, "lib/logger/index.js::log"))
		NewCrossRepo(g).ResolveAll()
		calls := callsInto(g, "lib/logger/index.js::log")
		require.Len(t, calls, 1)
		require.True(t, calls[0].CrossRepo)
		require.Equal(t, graph.OriginASTResolved, calls[0].Origin)
	})

	t.Run("cross-repo tracked npm package and isolation", func(t *testing.T) {
		g := buildGraphFromSources(t, map[string]string{
			"provider/index.js": "export function work() { return 1 }\n",
			"consumer/app.js":   "import { work } from 'p'\nexport function viaReq() { const { work } = require('p'); return work() }\nexport function viaOther() { const { work } = require('other-ext'); return work() }\n",
		})
		for _, n := range g.AllNodes() {
			if strings.HasPrefix(n.FilePath, "provider/") {
				n.RepoPrefix = "provider"
				n.Meta = map[string]any{"workspace_id": "shared", "js_named_export": true}
			} else {
				n.RepoPrefix = "consumer"
				if n.Meta == nil {
					n.Meta = map[string]any{}
				}
				n.Meta["workspace_id"] = "shared"
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
		require.NotNil(t, findCallEdge(g, "consumer/app.js::viaOther", graph.UnresolvedMarker+"work"))
	})
}

func TestImportEvidence_CollapsedSameLineNormalization(t *testing.T) {
	runBoth := func(t *testing.T, files map[string]string, check func(t *testing.T, store graph.Store)) {
		t.Helper()
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(backend, func(t *testing.T) {
				g := buildGraphFromSources(t, files)
				store := g
				if backend == "sqlite" {
					sq, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					defer sq.Close()
					sq.AddBatch(g.AllNodes(), g.AllEdges())
					store = sq
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
}

func TestImportEvidence_AdditionalLexicalRequireShadows(t *testing.T) {
	tsCases := map[string]string{
		"namespace export const":   "namespace N { export const require = () => () => 2; export function run() { const work = require('../a/one'); return work() } }",
		"namespace hoisted var":    "namespace N { export function run() { const work = require('../a/one'); return work() } if (true) { var require = () => () => 2 } }",
		"top-level namespace":      "namespace require { export const x = 1 }\nexport function run() { const work = require('../a/one'); return work() }",
		"nested namespace":         "namespace require.Sub { export const x = 1 }\nexport function run() { const work = require('../a/one'); return work() }",
		"import alias":             "namespace Loader { export function require(s: string) { return () => 2 } }\nimport require = Loader.require\nexport function run() { const work = require('../a/one'); return work() }",
		"export import alias":      "namespace Loader { export function require(s: string) { return () => 2 } }\nexport import require = Loader.require\nexport function run() { const work = require('../a/one'); return work() }",
	}
	for name, body := range tsCases {
		t.Run("ts/"+name, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"a/one.ts":    "export function work() { return 1 }",
				"app/main.ts": "import { work } from '../a/one'\n" + body + "\n",
			})
			New(g).ResolveAll()
			require.Empty(t, callsInto(g, "a/one.ts::work"))
		})
	}

	jsCases := map[string]string{
		"using declaration":       "export function run(mk) { using require = mk(); const work = require('../a/one'); return work() }",
		"await using declaration": "export async function run(mk) { await using require = mk(); const work = require('../a/one'); return work() }",
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
