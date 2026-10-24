package indexer

import (
	"context"
	"path"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

func TestMultiIndexer_TrackedNpmPackage(t *testing.T) {
	cases := []struct {
		name, packageName, manifest, entry, packageDir, consumerName, providerWS, providerName, specifier, depVersion string
		resolves, pnpm                                                                                                bool
	}{
		{name: "exports missing runtime uses types condition", packageName: "p", manifest: `"exports":{"types":"./src/index.ts","import":"./dist/index.js"}`, entry: "src/index.ts", resolves: true},
		{name: "exports subpath missing runtime uses types condition", packageName: "p", specifier: "p/utils", manifest: `"exports":{"./utils":{"types":"./src/utils.ts","import":"./dist/utils.js"}}`, entry: "src/utils.ts", resolves: true},
		{name: "unsupported subpath does not guess directory", packageName: "p", specifier: "p/utils", manifest: `"exports":{"./utils":{"browser":"./dist/utils.js"}}`, entry: "utils/index.ts"},
		{name: "scoped exports", packageName: "@scope/p", manifest: `"exports":"./src/index.ts","types":"./wrong.ts","main":"./wrong.ts"`, entry: "src/index.ts", resolves: true},
		{name: "unscoped subpath exports", packageName: "p", specifier: "p/utils", manifest: `"exports":{"./utils":"./src/util.js"}`, entry: "src/util.ts", resolves: true},
		{name: "scoped subpath exports", packageName: "@scope/p", specifier: "@scope/p/utils", manifest: `"exports":{"./utils":{"import":"./src/util.ts"}}`, entry: "src/util.ts", resolves: true},
		{name: "subpath relative", packageName: "p", specifier: "p/utils", entry: "utils/index.ts", resolves: true},
		{name: "subpath wildcard", packageName: "p", specifier: "p/utils", manifest: `"exports":{"./*":"./src/*.ts"}`, entry: "src/utils.ts", resolves: true},
		{name: "unmapped subpath refused", packageName: "p", specifier: "p/private", manifest: `"exports":{".":"./index.ts"}`, entry: "private.ts"},
		{name: "exports missing target blocks types", packageName: "p", manifest: `"exports":"./missing.ts","types":"./types.ts"`, entry: "types.ts"},
		{name: "exports missing target blocks main", packageName: "p", manifest: `"exports":"./missing.ts","main":"./main.ts"`, entry: "main.ts"},
		{name: "exports missing target blocks index", packageName: "p", manifest: `"exports":"./missing.ts"`, entry: "index.ts"},
		{name: "exports subpath only blocks root", packageName: "p", manifest: `"exports":{"./utils":"./utils.ts"},"types":"./index.ts","main":"./index.ts"`, entry: "index.ts"},
		{name: "exports empty object blocks root", packageName: "p", manifest: `"exports":{}`, entry: "index.ts"},
		{name: "exports import null blocks main", packageName: "p", manifest: `"main":"index.ts","exports":{"import":null}`, entry: "index.ts"},
		{name: "exports import null blocks require", packageName: "p", manifest: `"exports":{"import":null,"require":"./cjs.js"}`, entry: "index.ts"},
		{name: "exports default null blocks index", packageName: "p", manifest: `"exports":{"default":null}`, entry: "index.ts"},
		{name: "exports dot import null blocks types", packageName: "p", manifest: `"types":"index.ts","exports":{".":{"import":null}}`, entry: "index.ts"},
		{name: "exports node condition keeps legacy index", packageName: "p", manifest: `"exports":{"node":"./n.js"}`, entry: "index.ts", resolves: true},
		{name: "exports null uses legacy index", packageName: "p", manifest: `"exports":null`, entry: "index.ts", resolves: true},
		{name: "exports empty string blocks subpath", packageName: "p", specifier: "p/private", manifest: `"exports":""`, entry: "private.ts"},
		{name: "exports root-only unsupported conditions block subpath", packageName: "p", specifier: "p/private", manifest: `"exports":{"browser":"./index.ts"}`, entry: "private.ts"},
		{name: "exports blocked subpath", packageName: "p", specifier: "p/private", manifest: `"exports":{"./private":null}`, entry: "private.ts"},
		{name: "exports types require root uses legacy main", packageName: "p", manifest: `"main":"./src/index.ts","exports":{".":{"types":"./src/index.ts","require":"./dist/index.cjs"}}`, entry: "src/index.ts", resolves: true},
		{name: "exports types require root uses legacy types", packageName: "p", manifest: `"types":"./src/index.ts","exports":{".":{"types":"./dist/index.d.ts","require":"./dist/index.cjs"}}`, entry: "src/index.ts", resolves: true},
		{name: "exports require subpath uses legacy path", packageName: "p", specifier: "p/utils", manifest: `"exports":{"./utils":{"require":"./dist/utils.cjs"}}`, entry: "utils/index.ts"},
		{name: "exports browser root uses legacy index", packageName: "p", manifest: `"exports":{"browser":"./dist/index.js"}`, entry: "index.ts", resolves: true},
		{name: "exports require wildcard uses legacy path", packageName: "p", specifier: "p/utils", manifest: `"exports":{"./*":{"require":"./dist/*.cjs"}}`, entry: "utils/index.ts"},
		{name: "exports null overrides wildcard", packageName: "p", specifier: "p/private", manifest: `"exports":{"./*":"./src/*.ts","./private":null}`, entry: "src/private.ts"},
		{name: "exports null wildcard blocks legacy", packageName: "p", specifier: "p/private", manifest: `"exports":{"./*":null}`, entry: "private.ts"},
		{name: "exports require exact overrides wildcard", packageName: "p", specifier: "p/utils", manifest: `"exports":{"./*":"./wrong/*.ts","./utils":{"require":"./dist/utils.cjs"}}`, entry: "utils/index.ts"},
		{name: "exports conditional root", packageName: "p", manifest: `"exports":{"import":"./src/index.ts"}`, entry: "src/index.ts", resolves: true},
		{name: "exports array root", packageName: "p", manifest: `"exports":["./src/index.ts"]`, entry: "src/index.ts", resolves: true},
		{name: "exports array first target is authoritative", packageName: "p", manifest: `"exports":["./missing.ts","./src/index.ts"],"main":"./src/index.ts"`, entry: "src/index.ts"},
		{name: "exports array skips invalid and null items", packageName: "p", manifest: `"exports":[null,"src/x.ts",{"import":"./src/index.ts"}]`, entry: "src/index.ts", resolves: true},
		{name: "exports nested array", packageName: "p", manifest: `"exports":[["./src/index.ts"]]`, entry: "src/index.ts", resolves: true},
		{name: "exports empty array blocks root", packageName: "p", manifest: `"exports":[]`, entry: "index.ts"},
		{name: "exports null-only array blocks root", packageName: "p", manifest: `"exports":[null],"main":"./index.ts"`, entry: "index.ts"},
		{name: "exports array recognized null condition blocks root", packageName: "p", manifest: `"exports":[{"import":null}],"main":"./index.ts"`, entry: "index.ts"},
		{name: "exports array recognized invalid condition blocks root", packageName: "p", manifest: `"exports":[{"default":"index.ts"}],"main":"./index.ts"`, entry: "index.ts"},
		{name: "exports array with unsupported conditions uses legacy", packageName: "p", manifest: `"exports":[{"require":"./dist/index.cjs"}],"main":"./src/main.ts"`, entry: "src/main.ts", resolves: true},
		{name: "exports array blocks subpath", packageName: "p", specifier: "p/private", manifest: `"exports":["./index.ts"]`, entry: "private.ts"},
		{name: "npm alias collision refused", packageName: "p", depVersion: "npm:other@^1", entry: "index.ts"},
		{name: "types", packageName: "p", manifest: `"types":"./src/types.ts","main":"./wrong.ts"`, entry: "src/types.ts", resolves: true},
		{name: "main", packageName: "p", manifest: `"main":"./src/main.ts"`, entry: "src/main.ts", resolves: true},
		{name: "directory index", packageName: "p", entry: "index.ts", resolves: true},
		{name: "workspace package", packageName: "@scope/p", manifest: `"exports":{".":{"import":"./src/index.ts"}}`, entry: "src/index.ts", packageDir: "packages/lib", resolves: true},
		{name: "pnpm workspace package", packageName: "p", entry: "index.ts", packageDir: "packages/lib", resolves: true, pnpm: true},
		{name: "untracked package collision", packageName: "p", providerName: "other", entry: "p/index.ts"},
		{name: "same repo package refused", packageName: "p", providerName: "other", entry: "index.ts", consumerName: "p"},
		{name: "workspace boundary refused", packageName: "p", manifest: `"main":"./src/index.ts"`, entry: "src/index.ts", providerWS: "private"},
		{name: "entry escape refused", packageName: "p", manifest: `"exports":"../src/index.ts","types":"/src/index.ts","main":"../src/index.ts"`, entry: "src/index.ts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := t.TempDir()
			consumer := t.TempDir()
			packageRoot := filepath.Join(provider, tc.packageDir)
			providerName := tc.providerName
			if providerName == "" {
				providerName = tc.packageName
			}
			manifest := `{"name":` + strconv.Quote(providerName)
			if tc.manifest != "" {
				manifest += "," + tc.manifest
			}
			writePackageJSON(t, packageRoot, manifest+"}")
			if tc.packageDir != "" {
				if tc.pnpm {
					writePackageJSON(t, provider, `{"name":"monorepo"}`)
					writeWorkspaceFile(t, provider, "pnpm-workspace.yaml", "packages:\n  - 'packages/*'\n")
				} else {
					writePackageJSON(t, provider, `{"name":"monorepo","workspaces":["packages/*"]}`)
				}
			}
			writeWorkspaceFile(t, packageRoot, tc.entry, "export function f(): number { return 42; }\n")
			consumerName := tc.consumerName
			if consumerName == "" {
				consumerName = "app"
			}
			specifier := tc.specifier
			if specifier == "" {
				specifier = tc.packageName
			}
			depVersion := tc.depVersion
			if depVersion == "" {
				depVersion = "^1.0.0"
			}
			writePackageJSON(t, consumer, `{"name":`+strconv.Quote(consumerName)+`,"dependencies":{`+strconv.Quote(tc.packageName)+`:`+strconv.Quote(depVersion)+`}}`)
			writeWorkspaceFile(t, consumer, "src/app.ts", "import { f } from "+strconv.Quote(specifier)+";\nexport function run(): number { return f(); }\n")
			if tc.consumerName == "p" {
				writeWorkspaceFile(t, consumer, "index.ts", "export function unrelated(): number { return 0; }\n")
			}
			// A same-named entry point inside the importer is never package evidence.
			writeWorkspaceFile(t, consumer, "src/p/index.ts", "export function unrelated(): number { return 0; }\n")
			providerWS := tc.providerWS
			if providerWS == "" {
				providerWS = "shared"
			}
			wantFile := path.Join("provider", tc.packageDir, tc.entry)

			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			gc := &config.GlobalConfig{Repos: []config.RepoEntry{
				{Path: provider, Name: "provider", Workspace: providerWS},
				{Path: consumer, Name: "consumer", Workspace: "shared"},
			}}
			gc.SetConfigPath(cfgPath)
			require.NoError(t, gc.Save())
			cm, err := config.NewConfigManager(cfgPath)
			require.NoError(t, err)
			g := graph.New()
			reg := newTestRegistry()
			reg.Register(languages.NewTypeScriptExtractor())
			mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
			_, err = mi.IndexAll()
			require.NoError(t, err)

			require.NotNil(t, g.GetNode(wantFile+"::f"))
			var importTargets []string
			var calls int
			for _, e := range g.AllEdges() {
				if e.FilePath != "consumer/src/app.ts" {
					continue
				}
				switch e.Kind {
				case graph.EdgeImports:
					importTargets = append(importTargets, e.To)
					assert.Equal(t, tc.resolves, e.CrossRepo)
				case graph.EdgeCalls:
					calls++
					if tc.resolves {
						assert.Equal(t, wantFile+"::f", e.To)
						assert.True(t, e.CrossRepo)
					} else {
						assert.Equal(t, "unresolved::f", e.To)
						assert.False(t, e.CrossRepo)
					}
				}
			}
			if tc.resolves {
				assert.ElementsMatch(t, []string{wantFile, wantFile + "::f"}, importTargets)
			} else {
				external := "external::" + specifier
				module := external
				if tc.depVersion != "" {
					module = "external::other"
				}
				assert.ElementsMatch(t, []string{module, external + "::f"}, importTargets)
			}
			assert.Equal(t, 1, calls)
		})
	}
}

func TestTrackedNpmPackageWorkspaceEscape(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b", "pkg")
	writePackageJSON(t, a, `{"name":"a","workspaces":["../b/*"]}`)
	writePackageJSON(t, b, `{"name":"p","main":"index.ts"}`)
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"a": {RootPath: a}}}
	lookup := mi.npmPackageLookup()
	require.NotNil(t, lookup)
	assert.Empty(t, lookup("p"), "an escaped workspace must not supply another repo's graph prefix")
}

func TestMultiIndexer_TrackedNpmPackageLifecycle(t *testing.T) {
	for _, binding := range []string{"f", "renamed"} {
		for _, backend := range []string{"memory", "sqlite"} {
			for _, providerFirst := range []bool{true, false} {
				t.Run(binding+"/"+backend+"/"+strconv.FormatBool(providerFirst), func(t *testing.T) {
					provider, consumer := t.TempDir(), t.TempDir()
					writePackageJSON(t, provider, `{"name":"p","exports":"./src/index.ts"}`)
					writeWorkspaceFile(t, provider, "src/index.ts", "export function f(): number { return 42; }\n")
					writePackageJSON(t, consumer, `{"name":"app","dependencies":{"p":"^1"}}`)
					importName := "f"
					if binding != "f" {
						importName += " as " + binding
					}
					writeWorkspaceFile(t, consumer, "src/app.ts", "import { "+importName+" } from 'p';\nexport function run(): number { return "+binding+"(); }\n")
					entries := []config.RepoEntry{{Path: provider, Name: "provider", Workspace: "shared"}, {Path: consumer, Name: "consumer", Workspace: "shared"}}
					if !providerFirst {
						entries[0], entries[1] = entries[1], entries[0]
					}
					cfgPath := filepath.Join(t.TempDir(), "config.yaml")
					gc := &config.GlobalConfig{Repos: entries}
					gc.SetConfigPath(cfgPath)
					require.NoError(t, gc.Save())
					cm, err := config.NewConfigManager(cfgPath)
					require.NoError(t, err)
					db := filepath.Join(t.TempDir(), "graph.sqlite")
					var g graph.Store = graph.New()
					var sqlite *store_sqlite.Store
					if backend == "sqlite" {
						sqlite, err = store_sqlite.Open(db)
						require.NoError(t, err)
						g = sqlite
						t.Cleanup(func() { require.NoError(t, sqlite.Close()) })
					}
					newIndexer := func() *MultiIndexer {
						reg := newTestRegistry()
						reg.Register(languages.NewTypeScriptExtractor())
						return NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
					}
					mi := newIndexer()
					for _, entry := range entries {
						_, err := mi.TrackRepoCtx(context.Background(), entry)
						require.NoError(t, err)
					}
					// Live tracking must link in either order without a whole-store pass.
					var liveImports, liveCalls []string
					for _, e := range g.AllEdges() {
						if e.FilePath != "consumer/src/app.ts" {
							continue
						}
						if e.Kind == graph.EdgeImports {
							liveImports = append(liveImports, e.To)
						}
						if e.Kind == graph.EdgeCalls {
							liveCalls = append(liveCalls, e.To)
						}
					}
					assert.ElementsMatch(t, []string{"provider/src/index.ts", "provider/src/index.ts::f"}, liveImports)
					assert.Equal(t, []string{"provider/src/index.ts::f"}, liveCalls)
					// The daemon runs this whole-store resolve after cold per-repo tracking.
					require.NoError(t, mi.RunPreEnrichResolve(context.Background(), nil, nil))
					check := func(stage string) {
						t.Helper()
						var imports, calls []string
						for _, e := range g.AllEdges() {
							if e.FilePath != "consumer/src/app.ts" {
								continue
							}
							switch e.Kind {
							case graph.EdgeImports:
								imports = append(imports, e.To)
								assert.True(t, e.CrossRepo, stage)
							case graph.EdgeCalls:
								calls = append(calls, e.To)
								assert.True(t, e.CrossRepo, stage)
							}
						}
						assert.ElementsMatch(t, []string{"provider/src/index.ts", "provider/src/index.ts::f"}, imports, stage)
						assert.Equal(t, []string{"provider/src/index.ts::f"}, calls, stage)
					}
					check("cold track")
					warm := mi
					if backend == "sqlite" {
						mtimes := map[string]map[string]int64{}
						for _, entry := range entries {
							mtimes[entry.Name] = mi.FileMtimes(entry.Name)
						}
						require.NoError(t, mi.Close(context.Background()))
						require.NoError(t, sqlite.Close())
						sqlite, err = store_sqlite.Open(db)
						require.NoError(t, err)
						g = sqlite
						warm = newIndexer()
						for _, entry := range entries {
							_, err := warm.ReconcileRepoCtx(context.Background(), entry, mtimes[entry.Name])
							require.NoError(t, err)
						}
						// A clean warm restart skips the global pass and keeps persisted links.
						check("warm restart")
						require.NoError(t, warm.Close(context.Background()))
						require.NoError(t, sqlite.Close())
						sqlite, err = store_sqlite.Open(db)
						require.NoError(t, err)
						g = sqlite
						warm = newIndexer()
						t.Setenv("GORTEX_WARMUP_FULL_RETRACK", "1")
						for _, entry := range entries {
							result, err := warm.ReconcileRepoCtx(context.Background(), entry, mtimes[entry.Name])
							require.NoError(t, err)
							require.True(t, result.FullRetrack)
							if entry.Name == "provider" {
								if providerFirst {
									// The consumer is not restored yet: retain its parked edges.
									var imports, calls []string
									for _, e := range g.GetOutEdges("consumer/src/app.ts") {
										if e.Kind == graph.EdgeImports {
											imports = append(imports, e.To)
										}
									}
									for _, e := range g.GetOutEdges("consumer/src/app.ts::run") {
										if e.Kind == graph.EdgeCalls {
											calls = append(calls, e.To)
										}
									}
									assert.ElementsMatch(t, []string{"external::p", "external::p::f"}, imports, "provider full retrack")
									assert.Equal(t, []string{"unresolved::" + binding}, calls, "provider full retrack")
								} else {
									check("provider full retrack")
								}
							}
						}
						check("warm full retrack")
						require.NoError(t, warm.RunPreEnrichResolve(context.Background(), nil, nil))
						check("full retrack global resolve")
					}
					t.Cleanup(func() { require.NoError(t, warm.Close(context.Background())) })
					writeWorkspaceFile(t, provider, "src/index.ts", "export function f(value = 0): number { return 43 + value; }\nexport function added(): number { return 1; }\n")
					saved, err := warm.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/index.ts")})
					require.NoError(t, err)
					require.Equal(t, 1, saved.StaleFileCount)
					require.Empty(t, saved.FailedFiles)
					check("provider save")
					unchanged, err := warm.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/index.ts")})
					require.NoError(t, err)
					assert.Zero(t, unchanged.StaleFileCount)
					check("unchanged provider")
					_, err = warm.IndexRepo("provider")
					require.NoError(t, err)
					check("provider reindex")
					warm.UntrackRepo("provider")
					var imports []string
					for _, e := range g.GetOutEdges("consumer/src/app.ts") {
						if e.Kind == graph.EdgeImports {
							imports = append(imports, e.To)
							assert.False(t, e.CrossRepo)
						}
					}
					assert.ElementsMatch(t, []string{"external::p", "external::p::f"}, imports, "provider untrack")
					var pendingCalls []string
					for _, e := range g.GetOutEdges("consumer/src/app.ts::run") {
						if e.Kind == graph.EdgeCalls {
							pendingCalls = append(pendingCalls, e.To)
							assert.False(t, e.CrossRepo)
						}
					}
					assert.Equal(t, []string{"unresolved::" + binding}, pendingCalls, "provider untrack")
					_, err = warm.TrackRepoCtx(context.Background(), config.RepoEntry{Path: provider, Name: "provider", Workspace: "shared"})
					require.NoError(t, err)
					check("provider retrack")
				})
			}
		}
	}
}

func TestTrackedNpmPackageLazySnapshot(t *testing.T) {
	root := t.TempDir()
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"lib": {RootPath: root}}}
	lookup := mi.npmPackageLookup()
	require.NotNil(t, lookup)
	// Construction must not read manifests. First use sees this newly written file.
	writePackageJSON(t, root, `{"name":"p","main":"index.ts"}`)
	require.Contains(t, lookup("p"), "lib")
	writePackageJSON(t, root, `{"name":"q","main":"other.ts"}`)
	assert.Contains(t, lookup("p"), "lib", "one pass keeps its snapshot")
	fresh := mi.npmPackageLookup()
	assert.Empty(t, fresh("p"))
	assert.Contains(t, fresh("q"), "lib", "the next pass sees manifest edits")
	mi.repos = map[string]*RepoMetadata{"new": {RootPath: root}}
	assert.Contains(t, mi.npmPackageLookup()("q"), "new", "track/untrack replaces roots")
	assert.Contains(t, fresh("q"), "lib", "old pass keeps captured roots")
}

func TestTrackedNpmPackagePnpmWorkspace(t *testing.T) {
	root := t.TempDir()
	writePackageJSON(t, root, `{"name":"monorepo"}`)
	writeWorkspaceFile(t, root, "pnpm-workspace.yaml", "packages:\n  - 'packages/*'\n")
	writePackageJSON(t, filepath.Join(root, "packages", "lib"), `{"name":"p","main":"index.ts"}`)
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"lib": {RootPath: root}}}
	lookup := mi.npmPackageLookup()
	require.NotNil(t, lookup)
	assert.Equal(t, map[string]resolver.NpmPackageTarget{"lib/packages/lib": {WorkspaceID: "lib", Entries: []string{"lib/packages/lib/index.ts", "lib/packages/lib/index"}}}, lookup("p"))
}

func TestTrackedNpmCallShapes(t *testing.T) {
	for _, tc := range []struct {
		name, source              string
		calls, imports, reexports []string
	}{
		{"namespace", "import * as m from 'p'; export function run() { return m.f(); }", []string{"external-call::stdlib::p"}, []string{"provider/index.ts"}, nil},
		{"default", "import f from 'p'; export function run() { return f(); }", []string{"unresolved::f"}, []string{"provider/index.ts"}, nil},
		{"require member", "export function run() { return require('p').f(); }", []string{"unresolved::require", "unresolved::*.f"}, nil, nil},
		{"require destructured", "const {f} = require('p'); export function run() { return f(); }", []string{"unresolved::require", "unresolved::f"}, nil, nil},
		{"reexport", "export * from 'p';", nil, nil, []string{"external::p"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, consumer := t.TempDir(), t.TempDir()
			writePackageJSON(t, provider, `{"name":"p","exports":"./index.ts"}`)
			writeWorkspaceFile(t, provider, "index.ts", "export default function f() { return 42; }\n")
			writePackageJSON(t, consumer, `{"name":"app","dependencies":{"p":"^1"}}`)
			writeWorkspaceFile(t, consumer, "app.ts", tc.source)
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: provider, Name: "provider", Workspace: "shared"}, {Path: consumer, Name: "consumer", Workspace: "shared"}}}
			gc.SetConfigPath(cfgPath)
			require.NoError(t, gc.Save())
			cm, err := config.NewConfigManager(cfgPath)
			require.NoError(t, err)
			reg := newTestRegistry()
			reg.Register(languages.NewTypeScriptExtractor())
			g := graph.New()
			mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			_, err = mi.IndexAll()
			require.NoError(t, err)
			var calls, imports, reexports []string
			for _, e := range g.AllEdges() {
				if e.FilePath != "consumer/app.ts" {
					continue
				}
				switch e.Kind {
				case graph.EdgeCalls:
					calls = append(calls, e.To)
					assert.False(t, e.CrossRepo, "no module-wide call-name guesses")
				case graph.EdgeImports:
					imports = append(imports, e.To)
				case graph.EdgeReExports:
					reexports = append(reexports, e.To)
				}
			}
			assert.ElementsMatch(t, tc.calls, calls)
			assert.ElementsMatch(t, tc.imports, imports)
			assert.ElementsMatch(t, tc.reexports, reexports)
		})
	}
}
