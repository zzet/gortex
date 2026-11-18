package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func revisionColdGraph(t *testing.T, backend string, repos []config.RepoEntry) graph.Store {
	t.Helper()
	var g graph.Store = graph.New()
	if backend == "sqlite" {
		db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		g = db
	}
	cm := newTestConfigManager(t)
	cm.Global().Repos = repos
	reg := newTestRegistry()
	reg.Register(languages.NewTypeScriptExtractor())
	mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.IndexAll()
	require.NoError(t, err)
	return g
}

func revisionCallTargets(g graph.Store, caller string, kind graph.EdgeKind) []string {
	var targets []string
	for _, e := range g.GetOutEdges(caller) {
		if e.Kind == kind {
			targets = append(targets, e.To)
		}
	}
	return targets
}

// Given nested package calls on one line, when provider availability changes,
// then every call must match a cold index, independently of track order.
func TestGoModuleSameLineCallsColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"provider_first", "caller_first", "rename_back", "retrack", "provider_first_mixed_first", "caller_first_mixed_first", "provider_first_mixed_last", "caller_first_mixed_last", "provider_first_save", "caller_first_save"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				writeFile(t, filepath.Join(repos[0].Path, "main.go"), "package caller\nimport library \"example.org/library\"\nfunc Caller() { library.Hello(library.World()) }\n")
				writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Hello(v int) {}\nfunc World() int { return 1 }\n")
				expected := []string{"b/lib.go::Hello", "b/lib.go::World"}
				if strings.Contains(mode, "mixed") {
					bound := "Hello"
					if strings.HasSuffix(mode, "last") {
						bound = "World"
					}
					writeFile(t, filepath.Join(repos[0].Path, "main.go"), "package caller\nimport (\"fmt\"; library \"example.org/library\"; untracked \"example.org/untracked\")\nfunc Caller() {\n library."+bound+"(library.Missing())\n fmt.Println(1)\n untracked.Do()\n}\n")
					writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc "+bound+"(v int) {}\n")
					expected = []string{"b/lib.go::" + bound, "external-call::dep::example.org/library", graph.StubID("a", graph.StubKindStdlib, "fmt", "Println"), "external-call::dep::example.org/untracked"}
				}
				if strings.HasSuffix(mode, "save") {
					writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other() {}\n")
				}
				order := []config.RepoEntry{repos[1], repos[0]}
				if strings.HasPrefix(mode, "caller_first") {
					order[0], order[1] = order[1], order[0]
				}
				for _, entry := range order {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				if strings.HasSuffix(mode, "save") {
					file := filepath.Join(repos[1].Path, "lib.go")
					writeFile(t, file, "package library\nfunc Hello(v int) {}\nfunc World() int { return 1 }\n")
					require.NoError(t, mi.GetIndexer("b").IndexFile(file))
				}
				if mode == "rename_back" {
					manifest := filepath.Join(repos[1].Path, "go.mod")
					writeFile(t, manifest, "module example.org/renamed\n")
					mi.goModuleChecked = time.Time{}
					mi.RunGlobalResolve()
					writeFile(t, manifest, "module example.org/library\n")
					mi.goModuleChecked = time.Time{}
					mi.RunGlobalResolve()
				}
				if mode == "retrack" {
					_, _, err := mi.UntrackRepoChecked(t.Context(), "b")
					require.NoError(t, err)
					mi.RunGlobalResolve()
					_, err = mi.TrackRepoCtx(t.Context(), repos[1])
					require.NoError(t, err)
				}
				cold := revisionColdGraph(t, backend, repos[:2])
				require.ElementsMatch(t, expected, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls))
				assert.ElementsMatch(t, expected, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
			})
		}
	}
}

// Given a bound provider or a refused tie, when either untrack path removes it,
// then surviving callers settle immediately to the cold graph without a save.
func TestGoModuleUntrackSettlesColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"untrack", "purge", "tie"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				order := []config.RepoEntry{repos[1], repos[0]}
				if mode == "tie" {
					order = append(order, repos[2])
				}
				for _, entry := range order {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				if mode == "purge" {
					_, _, err := mi.purgeRepoChecked(t.Context(), "b", nil)
					require.NoError(t, err)
				} else {
					_, _, err := mi.UntrackRepoChecked(t.Context(), "b")
					require.NoError(t, err)
				}
				coldRepos := repos[:1]
				want := "external-call::dep::example.org/library"
				if mode == "tie" {
					coldRepos = []config.RepoEntry{repos[0], repos[2]}
					want = "fork/lib.go::Hello"
				}
				cold := revisionColdGraph(t, backend, coldRepos)
				require.Equal(t, want, goModuleLifecycleCall(t, cold).To)
				assert.Equal(t, want, goModuleLifecycleCall(t, g).To)
			})
		}
	}
}

// Given a module-bound caller, when a provider is reparsed or loses its export,
// then its settled payload has no restub handoff or stale binding markers.
func TestGoModuleRestubPayloadColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"rebind", "demote", "disabled"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				if mode == "disabled" {
					writeFile(t, filepath.Join(repos[0].Path, ".gortex.yaml"), "index:\n  synthesize_external_calls: false\n")
				}
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				src := "package library\nfunc Hello() { println(1) }\n"
				if mode != "rebind" {
					src = "package library\nfunc Other() {}\n"
				}
				writeFile(t, filepath.Join(repos[1].Path, "lib.go"), src)
				require.NoError(t, mi.GetIndexer("b").IndexFile(filepath.Join(repos[1].Path, "lib.go")))
				cold := revisionColdGraph(t, backend, repos[:2])
				edge, want := goModuleLifecycleCall(t, g), goModuleLifecycleCall(t, cold)
				require.Equal(t, want.To, edge.To)
				for _, key := range []string{"restub_prev_to", "restub_prev_origin", "restub_prev_tier", "restub_prev_conf"} {
					assert.NotContains(t, edge.Meta, key)
				}
				assert.Equal(t, want.Meta["go_import"], edge.Meta["go_import"])
				assert.Equal(t, want.Meta["npm_import"], edge.Meta["npm_import"])
				assert.Equal(t, want.Confidence, edge.Confidence)
				assert.Equal(t, want.Origin, edge.Origin)
			})
		}
	}
}

// Given either live track order, when calls bind through the module map,
// then the parallel call layer matches the cold index without manual detection.
func TestGoModuleParallelCallsColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, callerFirst := range []bool{false, true} {
			name := "provider_first"
			if callerFirst {
				name = "caller_first"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				order := []config.RepoEntry{repos[1], repos[0]}
				if callerFirst {
					order[0], order[1] = order[1], order[0]
				}
				for _, entry := range order {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				cold := revisionColdGraph(t, backend, repos[:2])
				want := revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCrossRepoCalls)
				require.Equal(t, []string{"b/lib.go::Hello"}, want)
				assert.Equal(t, want, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCrossRepoCalls))
				mi.RunGlobalResolve()
				assert.Equal(t, want, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCrossRepoCalls))
			})
		}
	}
}

// Given a completed purge, when the control-plane finalizer cancels the request,
// then the optional module tail must not report that removal failed.
func TestGoModuleUntrackCancellationAfterFinalize(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finalized := false
			_, _, err := mi.purgeRepoChecked(ctx, "b", func(*RepoMetadata) error { finalized = true; cancel(); return nil })
			require.True(t, finalized)
			require.NoError(t, err, "authoritative removal already completed")
			assert.Nil(t, mi.GetMetadata("b"))
			assert.Nil(t, g.GetNode("b/lib.go::Hello"))
			mi.RunGlobalResolve()
			cold := revisionColdGraph(t, backend, repos[:1])
			assert.ElementsMatch(t, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls), revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
		})
	}
}

// Given parked incoming calls, when a provider is purged, then only their
// compact identities are retained and settlement refetches current rows.
func TestGoModuleUntrackRetainsOnlyParkedIdentities(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			parked := restubGoModuleRepoIncoming(g, "b")
			require.Len(t, parked, 1)
			assert.IsType(t, []graph.EdgeIdentity{}, parked, "teardown must not retain full metadata-bearing edge objects")
			g.EvictFile("b/lib.go")
			require.NoError(t, mi.finalizeUntrackedGoModuleCalls(parked))
			cold := revisionColdGraph(t, backend, repos[:1])
			assert.ElementsMatch(t, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls), revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
		})
	}
}

// Given the only cross-workspace rule is declared by a JS-only repo, when that
// repo is untracked, then the surviving Go caller must match cold refusal.
func TestGoModuleUntrackRuleOnlyRepoColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			repos[0].Workspace, repos[1].Workspace = "w1", "w2"
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "main.js"), "export function X() {}\n")
			writeFile(t, filepath.Join(root, ".gortex.yaml"), "workspace: w1\ncross_workspace_deps:\n  - workspace: w2\n    modules: [example.org/library]\n    mode: read-only\n")
			for _, entry := range []config.RepoEntry{{Name: "x", Path: root, Workspace: "w1"}, repos[1], repos[0]} {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			require.Equal(t, "b/lib.go::Hello", goModuleLifecycleCall(t, g).To)
			_, _, err := mi.UntrackRepoChecked(t.Context(), "x")
			require.NoError(t, err)
			cold := revisionColdGraph(t, backend, repos[:2])
			require.Equal(t, "external-call::dep::example.org/library", goModuleLifecycleCall(t, cold).To)
			assert.Equal(t, goModuleLifecycleCall(t, cold).To, goModuleLifecycleCall(t, g).To)
		})
	}
}

// Given cached calls to unavailable symbols, when the caller changes through
// IndexFile and a provider save adds World, then recovery uses the new caller.
func TestGoModuleSourceDigestChangeColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			main := filepath.Join(repos[0].Path, "main.go")
			lib := filepath.Join(repos[1].Path, "lib.go")
			writeFile(t, main, "package caller\nimport library \"example.org/library\"\nfunc Caller(){ library.Hello(library.Missing()) }\n")
			writeFile(t, lib, "package library\nfunc Other() {}\n")
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
			require.Contains(t, mi.GetIndexer("a").goModuleSourceCache, "main.go")
			writeFile(t, main, "package caller\nimport library \"example.org/library\"\nfunc Caller(){ library.Hello(library.World()) }\n")
			require.NoError(t, mi.GetIndexer("a").IndexFile(main))
			writeFile(t, lib, "package library\nfunc World() int {return 1}\n")
			require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
			cold := revisionColdGraph(t, backend, repos[:2])
			require.ElementsMatch(t, []string{"external-call::dep::example.org/library", "b/lib.go::World"}, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls))
			assert.ElementsMatch(t, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls), revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
		})
	}
}

// Given a stale identity lookup result, when teardown settles parked calls,
// then a mismatched row must not be passed to the mutation writer.
func TestGoModuleParkedLookupRejectsMismatchedRows(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			parked := restubGoModuleRepoIncoming(g, "b")
			require.Len(t, parked, 1)
			require.NotEmpty(t, g.(graph.EdgeIdentityBatchFinder).FindEdgesByIdentities(parked), "parked row must exist before purge")
			g.EvictFile("b/lib.go")
			require.NotEmpty(t, g.(graph.EdgeIdentityBatchFinder).FindEdgesByIdentities(parked), "parked row must survive purge")
			wrapped := &goModuleMismatchedFinderStore{Store: g}
			mi.graph = wrapped
			require.NoError(t, mi.finalizeUntrackedGoModuleCalls(parked))
			assert.Zero(t, wrapped.writes, "identity mismatch must not schedule an edge mutation")
			assert.Zero(t, wrapped.lookups, "identity mismatch must be rejected before symbol lookup")
		})
	}
}

type goModuleMismatchedFinderStore struct {
	graph.Store
	writes  int
	lookups int
}

func (s *goModuleMismatchedFinderStore) FindNodesByName(name string) []*graph.Node {
	s.lookups++
	return s.Store.FindNodesByName(name)
}

func (s *goModuleMismatchedFinderStore) FindEdgesByIdentities(ids []graph.EdgeIdentity) map[graph.EdgeIdentity]*graph.Edge {
	out := make(map[graph.EdgeIdentity]*graph.Edge)
	for id, e := range s.Store.(graph.EdgeIdentityBatchFinder).FindEdgesByIdentities(ids) {
		_ = e
		out[id] = &graph.Edge{From: id.From, To: "dep::example.org/library::Other", Kind: id.Kind, FilePath: id.FilePath, Line: id.Line, Meta: map[string]any{"go_import": "example.org/library::Hello"}}
	}
	return out
}
func (s *goModuleMismatchedFinderStore) ReindexEdges(rows []graph.EdgeReindex) {
	s.writes += len(rows)
	s.Store.ReindexEdges(rows)
}

// Given an intermediate Store wrapper that hides optional lookup APIs, when
// parked identities are settled, then the shared fallback equals cold.
func TestGoModuleParkedLookupWithoutFinder(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			parked := restubGoModuleRepoIncoming(g, "b")
			g.EvictFile("b/lib.go")
			mi.graph = &goModuleNoFinderStore{Store: g}
			require.NoError(t, mi.finalizeUntrackedGoModuleCalls(parked))
			cold := revisionColdGraph(t, backend, repos[:1])
			assert.ElementsMatch(t, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls), revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
		})
	}
}

type goModuleNoFinderStore struct{ graph.Store }

// Given cached source calls, when a same-size caller edit has a new explicit
// mtime and the provider adds its symbol, then recovery must match cold.
func TestGoModuleSameSizeSourceStampColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			main := filepath.Join(repos[0].Path, "main.go")
			lib := filepath.Join(repos[1].Path, "lib.go")
			before := "package caller\nimport library \"example.org/library\"\nfunc Caller(){ library.Hello(library.Missing()) }\n"
			after := strings.Replace(before, "Missing", "Present", 1)
			require.Equal(t, len(before), len(after))
			writeFile(t, main, before)
			writeFile(t, lib, "package library\nfunc Other() {}\n")
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
			require.Contains(t, mi.GetIndexer("a").goModuleSourceCache, "main.go")
			info, err := os.Stat(main)
			require.NoError(t, err)
			writeFile(t, main, after)
			stamp := info.ModTime().Add(2 * time.Second)
			require.NoError(t, os.Chtimes(main, stamp, stamp))
			changed, err := os.Stat(main)
			require.NoError(t, err)
			require.Equal(t, info.Size(), changed.Size())
			require.NotEqual(t, info.ModTime().UnixNano(), changed.ModTime().UnixNano())
			writeFile(t, lib, "package library\nfunc Present() int{return 1}\n")
			require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
			cold := revisionColdGraph(t, backend, repos[:2])
			require.ElementsMatch(t, []string{"external-call::dep::example.org/library", "b/lib.go::Present"}, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls))
			assert.ElementsMatch(t, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls), revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
		})
	}
}

// Given cached C1, when same-size C2 keeps C1's mtime but IndexFile replaces
// the caller, then a provider save must recover C2's calls rather than C1's.
func TestGoModuleSameStampCallerReindexColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			main := filepath.Join(repos[0].Path, "main.go")
			lib := filepath.Join(repos[1].Path, "lib.go")
			before := "package caller\nimport library \"example.org/library\"\nfunc Caller(){ library.Hello(library.Missing()) }\n"
			after := strings.Replace(before, "Missing", "Present", 1)
			require.Equal(t, len(before), len(after))
			writeFile(t, main, before)
			writeFile(t, lib, "package library\nfunc Other(){}\n")
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
			require.Contains(t, mi.GetIndexer("a").goModuleSourceCache, "main.go")
			info, err := os.Stat(main)
			require.NoError(t, err)
			writeFile(t, main, after)
			require.NoError(t, os.Chtimes(main, info.ModTime(), info.ModTime()))
			changed, err := os.Stat(main)
			require.NoError(t, err)
			require.Equal(t, info.Size(), changed.Size())
			require.Equal(t, info.ModTime().UnixNano(), changed.ModTime().UnixNano())
			require.NoError(t, mi.GetIndexer("a").IndexFile(main))
			writeFile(t, lib, "package library\nfunc Missing() int{return 0}\nfunc Present() int{return 1}\n")
			require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
			cold := revisionColdGraph(t, backend, repos[:2])
			require.ElementsMatch(t, []string{"external-call::dep::example.org/library", "b/lib.go::Present"}, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls))
			assert.ElementsMatch(t, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls), revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
		})
	}
}
