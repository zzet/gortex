package indexer

import (
	"context"
	"github.com/zzet/gortex/internal/parser/crashpool"
	"iter"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

// Given persisted module edges and a clean restart, when warmup seeds retries,
// then the first save performs no definition-stream or external-terminal scan.
func TestGoModuleWarmSeedKeepsScansOffFirstSave(t *testing.T) {
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	g := &goModuleRetryScanStore{goModulePublishScanCountingStore: goModulePublishScanCountingStore{Store: db}}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.org/library\n")
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{{Name: "b", Path: root}}
	g.AddNode(&graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", Language: "go"})
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	require.NoError(t, mi.SeedGoModuleDeps(t.Context()))
	require.Positive(t, g.definitions, "warmup owns the initial definition scan")
	require.Equal(t, 1, g.scans, "warmup owns the initial terminal scan")
	g.definitions, g.scans = 0, 0
	mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
	assert.Zero(t, g.definitions, "first save must not seed persisted bindings")
	assert.Zero(t, g.scans, "first save must not seed external terminals")
}

type npmEvictionScanStore struct {
	graph.Store
	scans int
}

func (s *npmEvictionScanStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	s.scans++
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

// Given Go-only and JS/TS repositories, when npm eviction runs,
// then it scans definitions only for the JS/TS repository.
func TestNonJSTSRepoEvictionSkipsNpmRestub(t *testing.T) {
	for _, lang := range []string{"go", "typescript"} {
		t.Run(lang, func(t *testing.T) {
			g := &npmEvictionScanStore{Store: graph.New()}
			ext := "go"
			if lang == "typescript" {
				ext = "ts"
			}
			file := "repo/main." + ext
			g.AddNode(&graph.Node{ID: file, FilePath: file, RepoPrefix: "repo", Language: lang, Kind: graph.KindFile})
			g.AddNode(&graph.Node{ID: file + "::Hello", FilePath: file, RepoPrefix: "repo", Language: lang, Kind: graph.KindFunction, Name: "Hello"})
			restubTrackedNpmRepoIncoming(g, "repo")
			if lang == "go" {
				assert.Zero(t, g.scans)
			} else {
				assert.Equal(t, 1, g.scans)
			}
		})
	}
}

// Given an unbindable terminal, when unchanged provider definitions are saved
// twice, then the second pass must not re-extract the unchanged caller.
func TestGoModuleUnbindableSourceExtractionWork(t *testing.T) {
	mi, _, repos := goModuleLifecycleFixture(t, "memory")
	writeFile(t, filepath.Join(repos[0].Path, "main.go"), "package caller\nimport library \"example.org/library\"\nfunc Caller(){ library.Missing() }\n")
	writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other() {}\n")
	ext, ok := mi.registry.GetByLanguage("go")
	require.True(t, ok)
	spy := &goModuleSourceExtractionSpy{Extractor: ext}
	mi.registry.Register(spy)
	for _, entry := range repos[:2] {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
	spy.calls = 0
	mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
	assert.Zero(t, spy.calls, "an unchanged unbindable terminal must not re-parse the caller")
}

type goModuleSourceExtractionSpy struct {
	parser.Extractor
	calls int
}

func (s *goModuleSourceExtractionSpy) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	if path == "main.go" {
		s.calls++
	}
	return s.Extractor.Extract(path, src)
}

// Given a Go workspace plus a JS-only repository, when the JS repo is removed,
// then removal must not invalidate and reseed the Go terminal sweep.
func TestNonGoUntrackSkipsModuleSweep(t *testing.T) {
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	g := &goModuleUntrackScanStore{goModuleRetryScanStore: &goModuleRetryScanStore{goModulePublishScanCountingStore: goModulePublishScanCountingStore{Store: db}}}
	cm := newTestConfigManager(t)
	js, lib := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(js, "main.js"), "export function Hello() {}\n")
	writeFile(t, filepath.Join(lib, "go.mod"), "module example.org/library\n")
	writeFile(t, filepath.Join(lib, "lib.go"), "package library\nfunc Hello() {}\n")
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	for _, entry := range []config.RepoEntry{{Name: "a", Path: js}, {Name: "b", Path: lib}} {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	require.NoError(t, mi.SeedGoModuleDeps(t.Context()))
	g.scans = 0
	_, _, err = mi.UntrackRepoChecked(t.Context(), "a")
	require.NoError(t, err)
	assert.Zero(t, g.scans, "JS-only untrack must not rescan Go terminals")
}

type goModuleUntrackScanStore struct{ *goModuleRetryScanStore }

func (s *goModuleUntrackScanStore) PurgeRepo(repo string) error {
	return s.Store.(interface{ PurgeRepo(string) error }).PurgeRepo(repo)
}

// Given an isolated caller indexer, when module recovery re-extracts its source,
// then it uses the shared worker pool and never the parent-process extractor.
func TestGoModuleSourceRecoveryUsesIsolatedExtraction(t *testing.T) {
	mi, g, repos := goModuleLifecycleFixture(t, "memory")
	writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other() {}\n")
	for _, entry := range repos[:2] {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	idx := mi.GetIndexer("a")
	require.NotNil(t, idx)
	ext, ok := mi.registry.GetByLanguage("go")
	require.True(t, ok)
	spy := &goModuleSourceExtractionSpy{Extractor: ext}
	mi.registry.Register(spy)
	pool, err := crashpool.NewPool(crashpool.Config{Argv: []string{os.Args[0]}, Env: []string{crashWorkerEnv + "=1"}, Workers: 1})
	require.NoError(t, err)
	idx.parsePool = pool
	idx.parseQuar = crashpool.LoadQuarantine("")
	idx.config.CrashIsolation = true
	t.Setenv("GORTEX_PARSER_ISOLATION", "1")
	idx.goModuleSourceCache = nil
	writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Hello() {}\n")
	g.AddNode(&graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", WorkspaceID: "shared", Language: "go"})
	mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
	assert.Zero(t, spy.calls, "source recovery must use the isolated pool")
	assert.Equal(t, []string{"b/lib.go::Hello"}, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
}

// Given a deferred batch publication, when its per-repo tail runs,
// then the shared batch still owns cross-repo parallel materialization.
func TestGoModuleDeferredPublishLeavesParallelsToBatch(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a/main.go::Caller", RepoPrefix: "a", Kind: graph.KindFunction, FilePath: "a/main.go"})
	g.AddNode(&graph.Node{ID: "b/lib.go::Hello", RepoPrefix: "b", Kind: graph.KindFunction, FilePath: "b/lib.go"})
	g.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "b/lib.go::Hello", Kind: graph.EdgeCalls, FilePath: "a/main.go"})
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	idx := newTestIndexer(g)
	t.Cleanup(idx.Close)
	idx.repoPrefix = "a"
	idx.deferResolve.Store(true)
	require.NoError(t, mi.postPublishGoModules(t.Context(), idx, false))
	assert.Empty(t, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCrossRepoCalls))
	resolver.DetectCrossRepoEdges(g)
	assert.Equal(t, []string{"b/lib.go::Hello"}, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCrossRepoCalls))
}

// Given a go.mod snapshot with no Go files, when that repo is removed,
// then the module tail still reseeds the surviving Go workspace.
func TestGoModuleManifestOnlyUntrackRunsModuleSweep(t *testing.T) {
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	g := &goModuleUntrackScanStore{goModuleRetryScanStore: &goModuleRetryScanStore{goModulePublishScanCountingStore: goModulePublishScanCountingStore{Store: db}}}
	cm := newTestConfigManager(t)
	empty, lib := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(empty, "go.mod"), "module example.org/empty\n")
	writeFile(t, filepath.Join(lib, "go.mod"), "module example.org/library\n")
	writeFile(t, filepath.Join(lib, "lib.go"), "package library\nfunc Hello() {}\n")
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	for _, entry := range []config.RepoEntry{{Name: "empty", Path: empty}, {Name: "b", Path: lib}} {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	require.NoError(t, mi.SeedGoModuleDeps(t.Context()))
	// The snapshot, rather than a persisted Go file node, must carry this arm.
	g.EvictFile("empty/go.mod")
	require.Empty(t, graph.ReadRepoFilePaths(g, "empty", "", []string{"go"}, nil))
	g.scans = 0
	_, _, err = mi.UntrackRepoChecked(t.Context(), "empty")
	require.NoError(t, err)
	assert.Equal(t, 1, g.scans, "a manifest-only repo must invalidate the module sweep")
}

// Given extractor output with local calls and non-call edges, when cached,
// then only qualified module calls consume retained heap entries.
func TestGoModuleSourceCacheRetainsOnlyModuleCalls(t *testing.T) {
	edges := []*graph.Edge{
		{Kind: graph.EdgeCalls, To: "dep::example.org/library::Hello"},
		{Kind: graph.EdgeCalls, To: "unresolved::extern::example.org/library::World"},
		{Kind: graph.EdgeCalls, To: "a/main.go::Local"},
		{Kind: graph.EdgeImports, To: "dep::example.org/library::Hello"},
	}
	cached := cloneGoModuleSourceEdges(edges)
	require.Len(t, cached, 2)
	assert.Equal(t, edges[:2], cached)
}

// Given source facts retained by an indexer, when its file is explicitly
// evicted, deleted or renamed, then the old relative-path entry is removed.
func TestGoModuleSourceCacheEviction(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"forced", "delete", "rename"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other(){}\n")
				for _, entry := range repos[:2] {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
				idx := mi.GetIndexer("a")
				require.Contains(t, idx.goModuleSourceCache, "main.go")
				main := filepath.Join(repos[0].Path, "main.go")
				switch mode {
				case "forced":
					idx.EvictFile(main)
				case "delete":
					require.NoError(t, os.Remove(main))
					_, err := mi.IncrementalReindexRepo("a", []string{main})
					require.NoError(t, err)
				case "rename":
					renamed := filepath.Join(repos[0].Path, "renamed.go")
					require.NoError(t, os.Rename(main, renamed))
					_, err := mi.IncrementalReindexRepo("a", []string{main, renamed})
					require.NoError(t, err)
				}
				assert.NotContains(t, idx.goModuleSourceCache, "main.go")
				if mode != "forced" {
					cold := revisionColdGraph(t, backend, repos[:2])
					caller := "a/main.go::Caller"
					if mode == "rename" {
						caller = "a/renamed.go::Caller"
					}
					assert.ElementsMatch(t, revisionCallTargets(cold, caller, graph.EdgeCalls), revisionCallTargets(g, caller, graph.EdgeCalls))
				}
			})
		}
	}
}

// Given cached unbindable callers, when a second provider IndexFile save keeps
// the same definition set, then it reads zero unchanged caller files.
func TestGoModuleProviderSaveSkipsCachedSourceReads(t *testing.T) {
	mi, _, repos := goModuleLifecycleFixture(t, "memory")
	lib := filepath.Join(repos[1].Path, "lib.go")
	writeFile(t, lib, "package library\nfunc Other() {}\n")
	for _, entry := range repos[:2] {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	original := readGoModuleCallerFile
	reads := 0
	readGoModuleCallerFile = func(path string) ([]byte, error) {
		if filepath.Base(path) == "main.go" {
			reads++
		}
		return original(path)
	}
	t.Cleanup(func() { readGoModuleCallerFile = original })
	writeFile(t, lib, "package library\nfunc Other(){println(1)}\n")
	require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
	reads = 0
	writeFile(t, lib, "package library\nfunc Other(){println(2)}\n")
	require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
	assert.Zero(t, reads, "unchanged caller stat must avoid source reads and transforms")
}

// Given a shared parse lease, when source reading fails or panics, then the
// per-file recovery scope must return all admitted bytes without another parse.
func TestGoModuleSourceRecoveryReleasesLease(t *testing.T) {
	for _, mode := range []string{"read_error", "read_panic"} {
		t.Run(mode, func(t *testing.T) {
			mi, _, repos := goModuleLifecycleFixture(t, "memory")
			writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Other(){}\n")
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			idx := mi.GetIndexer("a")
			idx.goModuleSourceCache = nil
			mi.SetSharedParseMemoryBudget(1 << 20)
			original := readGoModuleCallerFile
			readGoModuleCallerFile = func(string) ([]byte, error) {
				if mode == "read_panic" {
					panic("source read panic")
				}
				return nil, os.ErrPermission
			}
			t.Cleanup(func() { readGoModuleCallerFile = original })
			recovered := false
			func() {
				defer func() { recovered = recover() != nil }()
				mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
			}()
			assert.Equal(t, mode == "read_panic", recovered)
			budget := idx.parseAdmission.Load()
			budget.mu.Lock()
			used := budget.used
			budget.mu.Unlock()
			assert.Zero(t, used, "every exit must return the raw-source admission lease")
		})
	}
}

// Given cached unchanged bytes, when only the caller mtime changes, then one
// provider save reads once to refresh the stamp and the next save reads zero.
func TestGoModuleSourceCacheRefreshesTouchedStamp(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, _, repos := goModuleLifecycleFixture(t, backend)
			main := filepath.Join(repos[0].Path, "main.go")
			lib := filepath.Join(repos[1].Path, "lib.go")
			writeFile(t, main, "package caller\nimport library \"example.org/library\"\nfunc Caller(){library.Hello(library.Missing())}\n")
			writeFile(t, lib, "package library\nfunc Other(){}\n")
			for _, entry := range repos[:2] {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			mi.runCrossRepoResolveFiles([]string{"b/lib.go"})
			require.Contains(t, mi.GetIndexer("a").goModuleSourceCache, "main.go")
			info, err := os.Stat(main)
			require.NoError(t, err)
			stamp := info.ModTime().Add(2 * time.Second)
			require.NoError(t, os.Chtimes(main, stamp, stamp))
			original := readGoModuleCallerFile
			reads := 0
			readGoModuleCallerFile = func(path string) ([]byte, error) {
				if path == main {
					reads++
				}
				return original(path)
			}
			t.Cleanup(func() { readGoModuleCallerFile = original })
			writeFile(t, lib, "package library\nfunc Other(){println(1)}\n")
			require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
			assert.Equal(t, 1, reads, "new stamp with identical bytes must refresh once")
			reads = 0
			writeFile(t, lib, "package library\nfunc Other(){println(2)}\nfunc More(){}\n")
			require.NoError(t, mi.GetIndexer("b").IndexFile(lib))
			assert.Zero(t, reads, "the refreshed stamp must make the next provider save read-free")
		})
	}
}
