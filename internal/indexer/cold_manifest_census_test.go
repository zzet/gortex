package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func coldReceiptFixture(t *testing.T) (*Indexer, *store_sqlite.Store, string) {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix("repo")
	root := t.TempDir()
	idx.storeRootPath(root)
	t.Cleanup(func() { idx.Close(); require.NoError(t, store.Close()) })
	return idx, store, root
}

func writeColdReceiptFile(t *testing.T, root, name, body string) (string, int64) {
	t.Helper()
	path := filepath.Join(root, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	return path, info.ModTime().UnixNano()
}

func completeColdReceiptContracts(idx *Indexer, b *coldManifestCensus) {
	idx.extractGoModContracts(b.registry)
	idx.extractExternalModulesForCensus(b.registry)
	idx.commitContracts(b.registry)
	b.contractsReady = true
	idx.finishColdManifests(b)
}

func TestColdManifestFailedReadCannotHideUnchangedMtimeRetry(t *testing.T) {
	idx, store, root := coldReceiptFixture(t)
	body := "module example.test/app\n\ngo 1.24\n"
	path, mtime := writeColdReceiptFile(t, root, "go.mod", body)
	prior := map[string]int64{"untouched.go": 101, "go.mod": mtime}
	idx.SetFileMtimes(prior)
	require.NoError(t, store.BulkSetFileMtimes("repo", prior))
	require.NoError(t, os.Remove(path))
	var b *coldManifestCensus
	idx.captureColdManifest(&b, context.Background(), path, nil) // real read failure
	require.Error(t, b.manifests["go.mod"].err)
	_, _ = writeColdReceiptFile(t, root, "go.mod", body)
	require.NoError(t, os.Chtimes(path, time.Unix(0, mtime), time.Unix(0, mtime)))
	idx.captureColdManifest(&b, context.Background(), path, nil)
	require.Error(t, b.manifests["go.mod"].err) // same-attempt error stays latched
	b.registry = contracts.NewRegistry()
	idx.finishColdIndexCensus(context.Background(), &IndexResult{}, nil, map[string]int64{}, b, false)
	completeColdReceiptContracts(idx, b)
	require.Equal(t, map[string]int64{"untouched.go": 101}, store.LoadFileMtimes("repo"))
	require.Contains(t, idx.fileIndexFailurePaths(), "repo/go.mod")

	warm := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	warm.SetRepoPrefix("repo")
	warm.storeRootPath(root)
	warm.SetFileMtimes(store.LoadFileMtimes("repo"))
	warm.loadFileIndexFailures()
	t.Cleanup(func() { warm.Close() })
	changed, _, _, err := warm.changedSinceMtimesCensus(root)
	require.NoError(t, err)
	require.Contains(t, changed, "go.mod")
	b = nil
	warm.captureColdManifest(&b, context.Background(), path, nil)
	b.registry = contracts.NewRegistry()
	warm.finishColdIndexCensus(context.Background(), &IndexResult{}, nil, map[string]int64{}, b, false)
	completeColdReceiptContracts(warm, b)
	require.Equal(t, map[string]int64{"go.mod": mtime}, store.LoadFileMtimes("repo"))
	require.NotContains(t, warm.fileIndexFailurePaths(), "repo/go.mod")
}

func TestColdManifestChangedVersionsAreNotCertified(t *testing.T) {
	for _, name := range []string{"go.mod", "go.work"} {
		t.Run(name, func(t *testing.T) {
			idx, store, root := coldReceiptFixture(t)
			body := "module example.test/app\n\ngo 1.24\nrequire example.test/old v1.0.0\n"
			if name == "go.work" {
				body = "go 1.24\nuse .\n"
			}
			path, mtime := writeColdReceiptFile(t, root, name, body)
			prior := map[string]int64{"untouched.go": 101, name: mtime}
			idx.SetFileMtimes(prior)
			require.NoError(t, store.BulkSetFileMtimes("repo", prior))
			var b *coldManifestCensus
			idx.captureColdManifest(&b, context.Background(), path, nil)
			b.registry = contracts.NewRegistry()
			idx.extractGoModContracts(b.registry)
			require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(body, "example.test/old", "example.test/new")), 0600))
			newTime := time.Unix(0, mtime).Add(time.Second)
			require.NoError(t, os.Chtimes(path, newTime, newTime))
			idx.extractExternalModulesForCensus(b.registry)
			if name == "go.mod" {
				rows, err := json.Marshal(coldReceiptGraphRows(t, store))
				require.NoError(t, err)
				require.Contains(t, string(rows), "example.test/old")
				require.NotContains(t, string(rows), "example.test/new")
			}
			idx.commitContracts(b.registry)
			b.contractsReady = true
			idx.finishColdIndexCensus(context.Background(), &IndexResult{}, nil, map[string]int64{}, b, false)
			require.Equal(t, map[string]int64{"untouched.go": 101}, store.LoadFileMtimes("repo"))
			require.Contains(t, idx.fileIndexFailurePaths(), "repo/"+name)
		})
	}
}

func TestColdCensusFailureAndCancellationPreservePriorKeys(t *testing.T) {
	for _, stage := range []string{"parse", "graph_error", "deferred", "after_contracts", "parser_error"} {
		t.Run(stage, func(t *testing.T) {
			idx, store, root := coldReceiptFixture(t)
			mainPath, mainMtime := writeColdReceiptFile(t, root, "main.go", "package app\n")
			modPath, modMtime := writeColdReceiptFile(t, root, "go.mod", "module example.test/app\n")
			prior := map[string]int64{"untouched.go": 101, "go.mod": modMtime}
			idx.SetFileMtimes(prior)
			require.NoError(t, store.BulkSetFileMtimes("repo", prior))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var b *coldManifestCensus
			idx.captureColdManifest(&b, ctx, modPath, nil)
			b.registry = contracts.NewRegistry()
			candidate := map[string]int64{"main.go": mainMtime}
			if stage == "deferred" {
				idx.finishColdIndexCensus(ctx, &IndexResult{}, nil, candidate, b, false)
				require.EqualValues(t, mainMtime, store.LoadFileMtimes("repo")["main.go"])
			}
			if stage == "after_contracts" {
				completeColdReceiptContracts(idx, b)
			}
			switch stage {
			case "parser_error":
				idx.noteFileIndexFailure(mainPath, errors.New("extract failed"))
				idx.finishColdIndexCensus(ctx, &IndexResult{}, nil, map[string]int64{}, b, true)
				completeColdReceiptContracts(idx, b)
			case "graph_error":
				idx.finishColdIndexCensus(ctx, nil, errors.New("graph publish failed"), candidate, b, false)
			default:
				cancel()
				if stage == "deferred" {
					idx.finishColdManifests(b)
				} else {
					idx.finishColdIndexCensus(ctx, &IndexResult{}, nil, candidate, b, false)
				}
			}
			persisted := store.LoadFileMtimes("repo")
			require.EqualValues(t, 101, persisted["untouched.go"])
			if stage != "deferred" {
				require.NotContains(t, persisted, "main.go")
			}
			if stage != "parser_error" {
				require.NotContains(t, persisted, "go.mod", "failed or cancelled publication cannot retain an old clean receipt")
			}
			require.Nil(t, idx.pendingColdManifests)
		})
	}
}

type coldReceiptWriteFailureStore struct{ *store_sqlite.Store }

func (s *coldReceiptWriteFailureStore) BulkSetFileMtimes(string, map[string]int64) error {
	return errors.New("mtime write failed")
}
func (s *coldReceiptWriteFailureStore) ReplaceFileMtimes(string, map[string]int64) error {
	return errors.New("mtime replace failed")
}

func TestColdCensusWriteFailureKeepsVisibleProgressAndMarksDirty(t *testing.T) {
	_, store, _ := coldReceiptFixture(t)
	idx := New(&coldReceiptWriteFailureStore{store}, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix("repo")
	t.Cleanup(func() { idx.Close() })
	idx.publishColdFileMtimes(map[string]int64{"main.go": 101}, false)
	require.Equal(t, map[string]int64{"main.go": 101}, idx.publishFileMtimes())
	require.True(t, idx.fileMtimePersistenceDirty.Load())
	require.Empty(t, store.LoadFileMtimes("repo"))
}

func TestColdCensusEmptySuccessAndOldAttemptFence(t *testing.T) {
	idx, store, root := coldReceiptFixture(t)
	idx.SetFileMtimes(map[string]int64{"deleted.go": 101})
	require.NoError(t, store.BulkSetFileMtimes("repo", idx.publishFileMtimes()))
	idx.finishColdIndexCensus(context.Background(), &IndexResult{}, nil, map[string]int64{}, nil, false)
	require.Empty(t, store.LoadFileMtimes("repo"))
	path, _ := writeColdReceiptFile(t, root, "go.mod", "module example.test/app\n")
	var old, current *coldManifestCensus
	idx.captureColdManifest(&old, context.Background(), path, nil)
	old.registry = contracts.NewRegistry()
	idx.captureColdManifest(&current, context.Background(), path, nil)
	current.registry = contracts.NewRegistry()
	idx.finishColdIndexCensus(context.Background(), &IndexResult{}, nil, map[string]int64{"old.go": 101}, old, false)
	old.graphReady, old.contractsReady = true, true
	idx.finishColdManifests(old)
	require.Same(t, current, idx.pendingColdManifests)
	require.Empty(t, store.LoadFileMtimes("repo"))
}

func TestColdCensusEmptySuccessUsesPersistedRoster(t *testing.T) {
	for _, mtimes := range []map[string]int64{nil, {}} {
		t.Run(map[bool]string{true: "nil", false: "empty"}[mtimes == nil], func(t *testing.T) {
			idx, store, _ := coldReceiptFixture(t)
			// This Indexer has no prior map: only the durable roster can tell
			// the successful empty census which old receipts to remove.
			require.Empty(t, idx.publishFileMtimes())
			require.NoError(t, store.BulkSetFileMtimes("repo", map[string]int64{"durable-only.go": 101}))
			require.NoError(t, store.BulkSetFileMtimes("sibling", map[string]int64{"keep.go": 202}))
			idx.markFileMtimePersistenceDirty()
			idx.publishColdFileMtimes(mtimes, true)
			require.Empty(t, store.LoadFileMtimes("repo"))
			require.Empty(t, idx.publishFileMtimes())
			require.Equal(t, map[string]int64{"keep.go": 202}, store.LoadFileMtimes("sibling"))
			require.False(t, idx.fileMtimePersistenceDirty.Load())
		})
	}
}

func TestColdCensusEmptySuccessPreservesSiblingGeneration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, store.BulkSetFileMtimes("repo", map[string]int64{"old.go": 101, "foreign.go": 202}))
	require.NoError(t, store.Close())
	// Move only the fixture's foreign receipt into a sibling generation.
	// The production store is closed while the fixture row is prepared.
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	update, err := db.Exec(`UPDATE file_mtimes SET view_gen = 7 WHERE repo_prefix = ? AND file_path = ?`, "repo", "foreign.go")
	require.NoError(t, err)
	rows, err := update.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	store, err = store_sqlite.Open(dbPath)
	require.NoError(t, err)
	idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix("repo")
	t.Cleanup(func() { idx.Close(); require.NoError(t, store.Close()) })
	idx.publishColdFileMtimes(nil, true)
	require.Empty(t, store.LoadFileMtimes("repo"))
	var mtime int64
	require.NoError(t, db.QueryRow(`SELECT mtime_ns FROM file_mtimes WHERE view_gen = 7 AND repo_prefix = ? AND file_path = ?`, "repo", "foreign.go").Scan(&mtime))
	require.EqualValues(t, 202, mtime)
}

// Hide optional reader/deleter capabilities while preserving a durable
// writer/replacer adapter's established empty-replacement no-op contract.
type coldReceiptNoClearStore struct{ graph.Store }

func (s *coldReceiptNoClearStore) BulkSetFileMtimes(string, map[string]int64) error {
	return nil
}
func (s *coldReceiptNoClearStore) ReplaceFileMtimes(string, map[string]int64) error {
	return nil
}

func TestColdCensusUnsupportedEmptyClearPreservesDurableData(t *testing.T) {
	_, store, _ := coldReceiptFixture(t)
	require.NoError(t, store.BulkSetFileMtimes("repo", map[string]int64{"keep.go": 101}))
	idx := New(&coldReceiptNoClearStore{store}, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix("repo")
	t.Cleanup(func() { idx.Close() })
	idx.publishColdFileMtimes(map[string]int64{}, true)
	require.Empty(t, idx.publishFileMtimes())
	require.True(t, idx.fileMtimePersistenceDirty.Load())
	require.Equal(t, map[string]int64{"keep.go": 101}, store.LoadFileMtimes("repo"))
}

func TestColdCensusMetadataRefreshIsImmutableAndIndexerBound(t *testing.T) {
	idx, store, root := coldReceiptFixture(t)
	idx.SetFileMtimes(map[string]int64{"main.go": 101})
	prior := &RepoMetadata{RepoPrefix: "repo", RootPath: root, FileMtimes: idx.publishFileMtimes()}
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"repo": prior}, indexers: map[string]*Indexer{"repo": idx}}
	idx.publishColdFileMtimes(map[string]int64{"go.mod": 202}, false)
	mi.refreshColdCensusMetadata(idx)
	require.Equal(t, map[string]int64{"main.go": 101}, prior.FileMtimes)
	require.Equal(t, map[string]int64{"main.go": 101, "go.mod": 202}, mi.FileMtimes("repo"))
	replacement := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(func() { replacement.Close() })
	mi.indexers["repo"] = replacement
	before := mi.repos["repo"]
	idx.publishColdFileMtimes(map[string]int64{"old.go": 303}, false)
	mi.refreshColdCensusMetadata(idx)
	require.Same(t, before, mi.repos["repo"])
}

func TestColdManifestInlineIndexPublishesReceipts(t *testing.T) {
	idx, store, root := coldReceiptFixture(t)
	expected := make(map[string]int64)
	for name, body := range map[string]string{
		"main.go": "package app\nfunc Main() {}\n",
		"go.mod":  "module example.test/app\n\ngo 1.24\nrequire example.test/dependency v1.0.0\n",
		"go.work": "go 1.24\nuse .\n",
	} {
		_, expected[name] = writeColdReceiptFile(t, root, name, body)
	}
	// No deferral flags: exercise the normal inline contract/module ordering,
	// then the final shadow/FTS/vector publication and census callback.
	_, err := idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	require.NotNil(t, store.GetNodesByIDs([]string{"dep::example.test/dependency"})["dep::example.test/dependency"])
	require.Equal(t, expected, store.LoadFileMtimes("repo"))
	require.Equal(t, expected, idx.publishFileMtimes())
	require.Nil(t, idx.pendingColdManifests)
}

func TestColdIndexFirstRestartPreservesManifestGraph(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"main.go": "package app\nfunc Main() {}\n",
		"go.mod":  "module example.test/app\n\ngo 1.24\nrequire example.test/dependency v1.0.0\n",
		"go.work": "go 1.24\nuse .\n",
	}
	expected := make(map[string]int64)
	var paths []string
	for path, body := range files {
		_, expected[path] = writeColdReceiptFile(t, root, path, body)
		paths = append(paths, path)
	}
	sort.Strings(paths)
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		if store != nil {
			_ = store.Close()
		}
	})
	registry := newTestRegistry()
	cfg := config.IndexConfig{MaxFileSize: 128}
	cold := New(store, registry, cfg, zap.NewNop())
	closed := false
	t.Cleanup(func() {
		if !closed {
			cold.Close()
		}
	})
	cold.SetRepoPrefix("repo")
	cold.SetDeferResolve(true)
	cold.SetDeferGlobalPasses(true)
	_, err = cold.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	// Parser progress is visible before deferred contracts; the go.mod
	// receipt must still be withheld until those contracts commit.
	require.EqualValues(t, expected["main.go"], store.LoadFileMtimes("repo")["main.go"])
	require.NotContains(t, store.LoadFileMtimes("repo"), "go.mod")
	cold.RunDeferredPasses(context.Background())
	require.Equal(t, expected, store.LoadFileMtimes("repo"))
	for _, id := range []string{"repo/main.go", "repo/go.mod", "dep::example.test/dependency"} {
		require.NotNil(t, store.GetNodesByIDs([]string{id})[id], "seed endpoint %s must exist", id)
	}
	// Match the exact populations lost by the measured false manifest refresh.
	// Bridge synthesis is outside this fixture's scope; do not claim a
	// synthetic bridge preservation assertion from node/edge totals alone.
	seeded := []*graph.Edge{
		{From: "repo/main.go", To: "dep::example.test/dependency", Kind: "imports", FilePath: "repo/main.go", Line: 2},
		{From: "repo/main.go", To: "repo/go.mod", Kind: "co_change", FilePath: "repo/main.go"},
		{From: "repo/go.mod", To: "repo/main.go", Kind: "co_change", FilePath: "repo/go.mod"},
	}
	store.AddBatch(nil, seeded)
	storedEdges := store.GetOutEdgesByNodeIDs([]string{"repo/main.go", "repo/go.mod"})
	for _, want := range seeded {
		found := 0
		for _, got := range storedEdges[want.From] {
			if got.From == want.From && got.To == want.To && got.Kind == want.Kind {
				found++
			}
		}
		require.Equal(t, 1, found, "stored %s edge %s -> %s", want.Kind, want.From, want.To)
	}
	before := coldReceiptGraphRows(t, store)
	cold.Close()
	closed = true
	require.NoError(t, store.Close())
	store, err = store_sqlite.Open(dbPath)
	require.NoError(t, err)
	warm := New(store, registry, cfg, zap.NewNop())
	t.Cleanup(func() { warm.Close() })
	warm.SetRepoPrefix("repo")
	warm.storeRootPath(root)
	warm.SetFileMtimes(store.LoadFileMtimes("repo"))
	changed, deleted, detected, err := warm.changedSinceMtimesCensus(root)
	require.NoError(t, err)
	require.Empty(t, changed)
	require.Empty(t, deleted)
	require.Equal(t, len(files), detected)
	result, err := warm.incrementalReindexPathsMode(root, paths, incrementalPathMode{})
	require.NoError(t, err)
	require.Zero(t, result.StaleFileCount)
	require.Equal(t, expected, store.LoadFileMtimes("repo"))
	require.Equal(t, before, coldReceiptGraphRows(t, store))
}

func TestMultiIndexerColdManifestReceiptsSurviveDeferredTail(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var entries []config.RepoEntry
	expected := make(map[string]map[string]int64)
	for _, prefix := range []string{"repo-a", "repo-b"} {
		root := t.TempDir()
		mtimes := make(map[string]int64)
		for name, body := range map[string]string{
			"main.go": "package app\nfunc Main() {}\n",
			"go.mod":  "module example.test/" + prefix + "\n\ngo 1.24\nrequire example.test/dependency v1.0.0\n",
			"go.work": "go 1.24\nuse .\n",
		} {
			_, mtimes[name] = writeColdReceiptFile(t, root, name, body)
		}
		expected[prefix] = mtimes
		entries = append(entries, config.RepoEntry{Path: root, Name: prefix})
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	global := &config.GlobalConfig{Repos: entries}
	global.SetConfigPath(configPath)
	require.NoError(t, global.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
	t.Cleanup(func() {
		for _, idx := range mi.indexers {
			idx.Close()
		}
	})

	// Exercise actual raw-worker publication and the coordinated deferred
	// tail. A parse-only context cancelled by ordinary worker cleanup must
	// not prevent this successful tail from publishing manifest receipts.
	results, err := mi.IndexAll()
	require.NoError(t, err)
	require.Len(t, results, len(entries))
	for prefix, mtimes := range expected {
		require.Equal(t, mtimes, store.LoadFileMtimes(prefix), prefix)
		require.Equal(t, mtimes, mi.FileMtimes(prefix), "late metadata refresh for %s", prefix)
		idx := mi.indexers[prefix]
		require.NotNil(t, idx)
		require.Nil(t, idx.pendingColdManifests, "deferred receipt bundle drained for %s", prefix)
		changed, deleted, _, err := idx.changedSinceMtimesCensus(mi.GetMetadata(prefix).RootPath)
		require.NoError(t, err)
		require.Empty(t, changed, prefix)
		require.Empty(t, deleted, prefix)
	}
}

func TestColdDeferredCohortRetainsRegisteredUniverseScope(t *testing.T) {
	mi, idx, store, root := deferredOverlapFixture(t)
	indexDeferredOverlapAttempt(t, idx, root, "scoped", time.Now().Add(-time.Minute))
	mi.mu.Lock()
	for _, prefix := range []string{"other-a", "other-b"} {
		other := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
		other.SetRepoPrefix(prefix)
		mi.indexers[prefix] = other
	}
	mi.mu.Unlock()
	// The explicit cold route is valid only while this cohort's lane is
	// held. The two other registered Indexers are not admitted as work.
	err := idx.coordinateRepositoryMutation(context.Background(), func() error {
		run := mi.beginDeferredPasses(context.Background(), nil, []*Indexer{idx}, true)
		defer run.FinishTailResult()
		require.Len(t, run.workIndexers, 1)
		require.Equal(t, 3, run.indexerCount)
		require.Equal(t, map[string]struct{}{"overlap": {}},
			normalizeDeferredCatchupScope(run.catchupScope, run.catchupKnown, run.indexerCount))
		return nil
	})
	require.NoError(t, err)
	require.Nil(t, normalizeDeferredCatchupScope(map[string]struct{}{"overlap": {}}, false, 3))
	require.Nil(t, normalizeDeferredCatchupScope(map[string]struct{}{
		"overlap": {}, "other-a": {}, "other-b": {},
	}, true, 3))
}

func coldReceiptGraphRows(t *testing.T, store graph.Store) map[string]string {
	t.Helper()
	rows := make(map[string]string)
	var ids []string
	for _, node := range store.AllNodes() {
		encoded, err := json.Marshal(node)
		require.NoError(t, err)
		rows["node:"+node.ID] = string(encoded)
		ids = append(ids, node.ID)
	}
	for _, edges := range store.GetOutEdgesByNodeIDs(ids) {
		for _, edge := range edges {
			encoded, err := json.Marshal(edge)
			require.NoError(t, err)
			rows["edge:"+string(encoded)] = string(encoded)
		}
	}
	return rows
}

// Embed only the base Store method set: the real SQLite backend supplies
// graph behavior but cannot accidentally promote its optional Deleter.
type manifestReplacerWithoutDelete struct {
	graph.Store
	graph.FileMtimeReader
	replacer     graph.FileMtimeReplacer
	replaceCalls int
}

func (s *manifestReplacerWithoutDelete) ReplaceFileMtimes(repo string, mtimes map[string]int64) error {
	s.replaceCalls++
	// Delegate to the real contract: empty input is deliberately a no-op,
	// while nonempty replacement prunes omitted keys for this repository.
	return s.replacer.ReplaceFileMtimes(repo, mtimes)
}

type manifestWriterReplacerWithoutDelete struct {
	*manifestReplacerWithoutDelete
	graph.FileMtimeWriter
}

var _ graph.FileMtimeReplacer = (*manifestReplacerWithoutDelete)(nil)
var _ graph.FileMtimeWriter = (*manifestWriterReplacerWithoutDelete)(nil)

func manifestCapabilityFixture(t *testing.T, withWriter bool) (*Indexer, *store_sqlite.Store, *manifestReplacerWithoutDelete) {
	t.Helper()
	backend, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	adapter := &manifestReplacerWithoutDelete{
		Store: backend, FileMtimeReader: backend, replacer: backend,
	}
	var store graph.Store = adapter
	if withWriter {
		store = &manifestWriterReplacerWithoutDelete{manifestReplacerWithoutDelete: adapter, FileMtimeWriter: backend}
	}
	_, canWrite := store.(graph.FileMtimeWriter)
	require.Equal(t, withWriter, canWrite)
	_, canReplace := store.(graph.FileMtimeReplacer)
	require.True(t, canReplace)
	_, canDelete := store.(graph.FileMtimeDeleter)
	require.False(t, canDelete, "fixture must not inherit SQLite's deletion capability")
	idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix("repo")
	idx.storeRootPath(t.TempDir())
	t.Cleanup(idx.Close)
	return idx, backend, adapter
}

func TestColdManifestCapabilitiesRejectDurableReplacerWithoutDelete(t *testing.T) {
	for _, withWriter := range []bool{false, true} {
		name := "replacer_only"
		if withWriter {
			name = "writer_and_replacer"
		}
		t.Run(name, func(t *testing.T) {
			idx, backend, adapter := manifestCapabilityFixture(t, withWriter)
			assert.False(t, supportsColdManifestReceipts(idx.graph))
			path := filepath.Join(t.TempDir(), "go.work")
			idx.storeRootPath(filepath.Dir(path))
			require.NoError(t, os.WriteFile(path, []byte("go 1.24\nuse .\n"), 0600))
			var bundle *coldManifestCensus
			idx.captureColdManifest(&bundle, context.Background(), path, nil)
			assert.Nil(t, bundle, "unsupported durable backend must not capture new receipts")
			// If the broken gate captures anyway, complete the ordinary go.work
			// receipt path to expose actual new durable rows, not just a bool.
			if bundle != nil {
				bundle.graphReady = true
				bundle.contractsReady = true
				bundle.census = make(map[string]int64)
				idx.finishColdManifests(bundle)
			}
			assert.Empty(t, backend.LoadFileMtimes("repo"), "unsupported backend must not acquire an unrevocable new receipt")
			assert.Zero(t, adapter.replaceCalls)
		})
	}
}

func TestColdManifestCapabilitiesUnsupportedInvalidationIsLocalAndDirty(t *testing.T) {
	for _, withWriter := range []bool{false, true} {
		capability := "replacer_only"
		if withWriter {
			capability = "writer_and_replacer"
		}
		for _, includePeers := range []bool{false, true} {
			shape := "sole_manifest"
			if includePeers {
				shape = "persisted_only_peer"
			}
			t.Run(capability+"/"+shape, func(t *testing.T) {
				idx, backend, adapter := manifestCapabilityFixture(t, withWriter)
				durable := map[string]int64{"go.mod": 101}
				local := map[string]int64{"go.mod": 101}
				wantLocal := map[string]int64{}
				if includePeers {
					durable["disk-only.go"] = 202
					durable["visible.go"] = 303
					local["visible.go"] = 303
					wantLocal["visible.go"] = 303
				}
				require.NoError(t, backend.BulkSetFileMtimes("repo", durable))
				sibling := map[string]int64{"go.mod": 404}
				require.NoError(t, backend.BulkSetFileMtimes("sibling", sibling))
				idx.SetFileMtimes(local)
				require.False(t, idx.fileMtimePersistenceDirty.Load())
				idx.invalidateColdFileMtimes([]string{"go.mod"})
				assert.Equal(t, wantLocal, idx.publishFileMtimes(), "invalidate only the exact local key")
				assert.Equal(t, durable, backend.LoadFileMtimes("repo"), "without a deleter do not claim durable deletion or prune unknown peers")
				assert.Equal(t, sibling, backend.LoadFileMtimes("sibling"))
				assert.True(t, idx.fileMtimePersistenceDirty.Load(), "unsupported durable invalidation needs retry state")
				assert.Zero(t, adapter.replaceCalls, "ReplaceFileMtimes is not an invalidation fallback")
			})
		}
	}
}

func TestColdManifestCapabilitiesMemoryOnlyRemainsSupported(t *testing.T) {
	// These helpers inspect capabilities and own the Indexer's mtime map;
	// they do not invoke graph operations, so no graph fixture is required.
	memory := &graph.Graph{}
	_, writer := any(memory).(graph.FileMtimeWriter)
	_, replacer := any(memory).(graph.FileMtimeReplacer)
	require.False(t, writer)
	require.False(t, replacer)
	require.True(t, supportsColdManifestReceipts(memory))
	idx := &Indexer{graph: memory, logger: zap.NewNop()}
	idx.publishColdFileMtimes(map[string]int64{"go.work": 101, "keep.go": 202}, true)
	require.Equal(t, map[string]int64{"go.work": 101, "keep.go": 202}, idx.publishFileMtimes())
	idx.invalidateColdFileMtimes([]string{"go.work"})
	require.Equal(t, map[string]int64{"keep.go": 202}, idx.publishFileMtimes())
	require.False(t, idx.fileMtimePersistenceDirty.Load())
}

// Register the repository through real public orchestration, then retain its
// actual Indexer. Every attempt below calls public IndexCtx on this same object;
// no pending registry, census bundle, or completion flag is manufactured.
func deferredOverlapFixture(t *testing.T) (*MultiIndexer, *Indexer, *store_sqlite.Store, string) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package app\nfunc Main() {}\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/base\n\ngo 1.24\n"), 0600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	global := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: root, Name: "overlap"}}}
	global.SetConfigPath(configPath)
	require.NoError(t, global.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
	t.Cleanup(func() {
		for _, idx := range mi.indexers {
			idx.Close()
		}
	})
	_, err = mi.IndexAll()
	require.NoError(t, err)
	idx := mi.indexers["overlap"]
	require.NotNil(t, idx)
	idx.SetDeferResolve(true)
	idx.SetDeferGlobalPasses(true)
	return mi, idx, store, root
}

func indexDeferredOverlapAttempt(t *testing.T, idx *Indexer, root, name string, mtime time.Time) int64 {
	t.Helper()
	path := filepath.Join(root, "go.mod")
	body := "module example.test/" + name + "\n\ngo 1.24\nrequire example.test/dependency v1.0.0\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	info, err := os.Stat(path)
	require.NoError(t, err)
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	require.NotNil(t, idx.pendingContractReg, "public IndexCtx must leave this attempt's contracts deferred")
	return info.ModTime().UnixNano()
}

// Waiting for the enrichment pool creates a deterministic public-API gap:
// no goroutine scheduling, sleeps, provider internals, or source hooks needed.
// Cleanup drains each run exactly once, including after a failed prerequisite.
func beginDeferredOverlapRun(t *testing.T, mi *MultiIndexer) func() {
	t.Helper()
	run := mi.BeginDeferredPasses(context.Background(), nil)
	run.Wait()
	finished := false
	finish := func() {
		if !finished {
			finished = true
			run.FinishTailResult()
		}
	}
	t.Cleanup(finish)
	return finish
}

func TestDeferredOlderTailDoesNotConsumeNewerRegistry(t *testing.T) {
	mi, idx, _, root := deferredOverlapFixture(t)
	start := time.Now().Add(-time.Minute)
	indexDeferredOverlapAttempt(t, idx, root, "attempt-a", start)
	registryA := idx.pendingContractReg
	finishA := beginDeferredOverlapRun(t, mi)

	indexDeferredOverlapAttempt(t, idx, root, "attempt-b", start.Add(time.Second))
	registryB := idx.pendingContractReg
	require.NotSame(t, registryA, registryB, "the public reindex must actually create a newer attempt")
	finishA()
	assert.Same(t, registryB, idx.pendingContractReg,
		"A's deferred tail must not consume or clear B's newer pending registry")

	// B owns its own tail. This is a separate run, not a second finish of A.
	finishB := beginDeferredOverlapRun(t, mi)
	finishB()
}

func TestDeferredOlderTailDoesNotCertifyNewerManifest(t *testing.T) {
	mi, idx, store, root := deferredOverlapFixture(t)
	start := time.Now().Add(-time.Minute)
	indexDeferredOverlapAttempt(t, idx, root, "attempt-a", start)
	registryA := idx.pendingContractReg
	finishA := beginDeferredOverlapRun(t, mi)

	mtimeB := indexDeferredOverlapAttempt(t, idx, root, "attempt-b", start.Add(time.Second))
	registryB := idx.pendingContractReg
	require.NotSame(t, registryA, registryB)
	bundleB := idx.pendingColdManifests
	require.NotNil(t, bundleB, "receipt integration prerequisite: B must own a real captured manifest bundle")
	require.Same(t, registryB, bundleB.registry, "receipt integration prerequisite: raw indexing must bind B's registry")
	require.True(t, bundleB.graphReady, "receipt integration prerequisite: B's graph publication must finish")
	require.Contains(t, bundleB.manifests, "go.mod")
	require.NotContains(t, store.LoadFileMtimes("overlap"), "go.mod", "B has not finished its contract tail")

	// Begin B's real dependency phase before old A finishes. Otherwise the
	// dependencyDone=false guard would withhold the receipt and mask whether
	// an old tail can wrongly certify a newer, fully prepared bundle.
	finishB := beginDeferredOverlapRun(t, mi)
	require.True(t, bundleB.manifests["go.mod"].dependencyDone,
		"receipt integration prerequisite: B's actual deferred go.mod phase must run")
	require.False(t, bundleB.contractsReady, "B's own tail has not run")
	finishA()
	assert.Same(t, registryB, idx.pendingContractReg,
		"registry ownership: A must not consume B")
	assert.Same(t, bundleB, idx.pendingColdManifests,
		"receipt ownership: A must not finalize B's census bundle")
	assert.False(t, bundleB.contractsReady,
		"receipt ownership: A must not mark B's contracts complete")
	assert.NotContains(t, store.LoadFileMtimes("overlap"), "go.mod",
		"A must not publish B's durable success receipt before B's own tail")

	finishB()
	require.Nil(t, idx.pendingContractReg)
	require.Nil(t, idx.pendingColdManifests)
	require.EqualValues(t, mtimeB, store.LoadFileMtimes("overlap")["go.mod"],
		"B's legitimate tail must eventually publish its own manifest version")
}

func TestColdScopedDeferredTailPreservesUnselectedAttempt(t *testing.T) {
	mi, unselected, _, root := deferredOverlapFixture(t)
	indexDeferredOverlapAttempt(t, unselected, root, "unselected-pending", time.Now().Add(-time.Minute))
	registry := unselected.pendingContractReg
	bundle := unselected.pendingColdManifests

	selectedRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(selectedRoot, "main.go"), []byte("package selected\nfunc Main() {}\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(selectedRoot, "go.mod"), []byte("module example.test/selected\n\ngo 1.24\n"), 0600))
	// This is the actual cold orchestration entry whose sorted lane set only
	// contains "selected". The previously registered "overlap" repository's
	// lane is not part of this batch, despite its pending deferred contracts.
	results, err := mi.indexMultiRepo([]config.RepoEntry{{Path: selectedRoot, Name: "selected"}})
	require.NoError(t, err)
	require.Contains(t, results, "selected")
	assert.Same(t, registry, unselected.pendingContractReg,
		"a selected cold batch must not drain an unrelated, unheld repository")
	assert.Same(t, bundle, unselected.pendingColdManifests,
		"the unrelated attempt's pending census must remain owned by that attempt")
	selected := mi.indexers["selected"]
	require.NotNil(t, selected)
	require.Nil(t, selected.pendingContractReg, "the selected batch's own tail must complete")

	// Drain any still-pending unselected work via the legitimate public path.
	finishUnselected := beginDeferredOverlapRun(t, mi)
	finishUnselected()
}
