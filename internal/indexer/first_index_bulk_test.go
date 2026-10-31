package indexer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/search"
)

// Embed the real store so the shadow and its optional persistence capabilities
// stay intact. Only the drain boundaries are intercepted.
type firstIndexBulkProbe struct {
	*store_sqlite.Store
	duringDrain func()
	begins      int
	flushes     int
	batches     int
}

func (s *firstIndexBulkProbe) BeginBulkLoad() {
	s.begins++
	s.Store.BeginBulkLoad()
}

func (s *firstIndexBulkProbe) beforeBatch() {
	s.batches++
	if s.duringDrain != nil {
		s.duringDrain()
	}
}

func (s *firstIndexBulkProbe) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.beforeBatch()
	s.Store.AddBatch(nodes, edges)
}

// The drain writes through the checked path when the target has one.
func (s *firstIndexBulkProbe) AddBatchCheckedContext(ctx context.Context, nodes []*graph.Node, edges []*graph.Edge) error {
	s.beforeBatch()
	return s.Store.AddBatchCheckedContext(ctx, nodes, edges)
}

func (s *firstIndexBulkProbe) FlushBulk() error {
	s.flushes++
	return s.Store.FlushBulk()
}

func newFirstIndexBulkFixture(t *testing.T) (*firstIndexBulkProbe, *sql.DB) {
	t.Helper()
	store, db := newFirstIndexStore(t)
	return &firstIndexBulkProbe{Store: store}, db
}

// newFirstIndexStore opens a fresh on-disk store for the shadow route with a
// read-only pool beside it for schema probes.
func newFirstIndexStore(t *testing.T) (*store_sqlite.Store, *sql.DB) {
	t.Helper()
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=synchronous(NORMAL)")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return store, db
}

// firstIndexWriterSynchronous reads PRAGMA synchronous off the store's writer
// connection. It takes the write gate under a deadline, so it also fails,
// rather than hangs, when a failure path leaked the gate.
func firstIndexWriterSynchronous(t *testing.T, store *store_sqlite.Store) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sync, err := store.WriterPragmaForTest(ctx, "synchronous")
	require.NoError(t, err, "the store's write gate is still held")
	return sync
}

func firstIndexFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.py"), "def fetch(url):\n    return url\n")
	return dir
}

func newFirstIndexBulkIndexer(t *testing.T, store graph.Store, prefix string) *Indexer {
	t.Helper()
	reg := parser.NewRegistry()
	reg.Register(languages.NewPythonExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	idx := New(store, reg, cfg, zap.NewNop())
	idx.SetRepoPrefix(prefix)
	t.Cleanup(idx.Close)
	return idx
}

func firstIndexBulkActive(t *testing.T, store *store_sqlite.Store) bool {
	t.Helper()
	health, err := store.PlannerStatsHealth(context.Background())
	require.NoError(t, err)
	return health.Reason == "bulk_window_active"
}

func firstIndexSchemaIndexes(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM sqlite_schema WHERE type='index' ORDER BY name")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

func assertFirstIndexNoReceipts(t *testing.T, store *store_sqlite.Store, prefix string) {
	t.Helper()
	_, found, err := store.GetRepoIndexState(prefix)
	assert.NoError(t, err)
	assert.False(t, found, "index state must not precede successful drain")
	assert.Empty(t, store.LoadFileMtimes(prefix), "mtimes must not precede successful drain")
}

func assertFirstIndexRestored(t *testing.T, store *store_sqlite.Store, db *sql.DB, indexes []string) {
	t.Helper()
	require.False(t, firstIndexBulkActive(t, store), "standalone window must be closed before restoration probes")
	assert.Equal(t, indexes, firstIndexSchemaIndexes(t, db))
	// PRAGMA synchronous is connection-local, so this read-only pool cannot
	// check writer restoration; firstIndexWriterSynchronous reads the writer.
}

// sqliteFullError provokes a genuine SQLITE_FULL from the driver on a
// throwaway in-memory database capped at a few pages, so a test can hand a
// store the exact error a full volume produces without filling a disk.
func sqliteFullError(t *testing.T) error {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA max_page_count = 2`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE filler(v BLOB)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO filler VALUES (randomblob(65536))`)
	require.Error(t, err)
	return err
}

// A drain chunk the store refuses comes back as a typed storage error, for an
// ordinary drain and for the initial claimed one, instead of panicking out of
// the void AddBatch, and none of the refused chunk is committed.
func TestDrainAddBatchReturnsStorageFailure(t *testing.T) {
	full := sqliteFullError(t)
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"ordinary", context.Background()},
		{"initial claimed", withInitialDrainCooperation(context.Background(), func() bool { return false })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			defer store.SetBatchWriteHookForTest(func(int, int) error { return full })()
			nodes := []*graph.Node{{
				ID: "app.py::fetch", Name: "fetch", Kind: graph.KindFunction,
				RepoPrefix: "repo", FilePath: "app.py", Language: "python",
			}}

			var drainErr error
			require.NotPanics(t, func() { drainErr = drainAddBatch(tc.ctx, store, nodes, nil) })
			var storageErr *store_sqlite.StorageError
			require.ErrorAs(t, drainErr, &storageErr)
			assert.Contains(t, store_sqlite.SafeStorageFailureReason(drainErr), "SQLite code 13")
			assert.Contains(t, drainBatchError(tc.ctx, drainErr).Error(), "indexer: persist bulk graph batch:")
			assert.Zero(t, store.NodeCount(), "the refused chunk must roll back")
		})
	}
}

// firstIndexWindowWitness counts a store's batch writes, and how many of them
// landed inside an engaged bulk window with the dense indexes deferred. fail,
// when set, decides whether the store refuses a batch.
type firstIndexWindowWitness struct {
	batches, engaged, deferred int
	fail                       func(edgeRows int) error
}

func watchFirstIndexWindow(t *testing.T, store *store_sqlite.Store, db *sql.DB, indexes []string) *firstIndexWindowWitness {
	t.Helper()
	w := &firstIndexWindowWitness{}
	t.Cleanup(store.SetBatchWriteHookForTest(func(_, edgeRows int) error {
		w.batches++
		if firstIndexBulkActive(t, store) {
			w.engaged++
		}
		if !assert.ObjectsAreEqual(indexes, firstIndexSchemaIndexes(t, db)) {
			w.deferred++
		}
		if w.fail != nil {
			return w.fail(edgeRows)
		}
		return nil
	}))
	// The eviction runs just before the window opens, so this is the last
	// point at which a receipt could wrongly precede the drain.
	shadowEvictionBranchHook = func(retirement bool) {
		assert.True(t, retirement, "a SQLite target must take the shadow-replacement retirement")
		assertFirstIndexNoReceipts(t, store, "repo")
	}
	t.Cleanup(func() { shadowEvictionBranchHook = nil })
	return w
}

func (w *firstIndexWindowWitness) assertEngaged(t *testing.T) {
	t.Helper()
	require.Positive(t, w.batches, "test must observe the shadow drain")
	assert.Positive(t, w.engaged, "cold shadow drain must engage standalone bulk mode")
	assert.Positive(t, w.deferred, "dense indexes must be deferred during drain")
}

func assertFirstIndexReceipts(t *testing.T, store *store_sqlite.Store, result *IndexResult) {
	t.Helper()
	state, found, err := store.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, result.NodeCount, state.NodeCount)
	assert.Equal(t, result.EdgeCount, state.EdgeCount)
	assert.NotEmpty(t, store.LoadFileMtimes("repo"))
}

// A cold first index drains through a standalone bulk window on every route
// into it, and again on a retry after a failed drain left rows behind.
func TestFirstIndexDrainUsesBulkWindow(t *testing.T) {
	t.Run("IndexCtx on a fresh store", func(t *testing.T) {
		store, db := newFirstIndexStore(t)
		indexes := firstIndexSchemaIndexes(t, db)
		writerSync := firstIndexWriterSynchronous(t, store)
		w := watchFirstIndexWindow(t, store, db, indexes)

		result, err := newFirstIndexBulkIndexer(t, store, "repo").IndexCtx(context.Background(), firstIndexFixtureRepo(t))
		require.NoError(t, err)
		require.Positive(t, result.NodeCount)
		w.assertEngaged(t)
		assertFirstIndexReceipts(t, store, result)
		assert.Contains(t, store.LoadFileMtimes("repo"), "app.py")
		assert.Equal(t, writerSync, firstIndexWriterSynchronous(t, store))
		assertFirstIndexRestored(t, store, db, indexes)
	})

	t.Run("TrackRepoCtx on an empty multi-repo store", func(t *testing.T) {
		store, db := newFirstIndexStore(t)
		indexes := firstIndexSchemaIndexes(t, db)
		writerSync := firstIndexWriterSynchronous(t, store)
		w := watchFirstIndexWindow(t, store, db, indexes)
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "main.go"), "package app\n\nfunc Fetch(url string) string { return url }\n")
		mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
		t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })

		result, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "repo", Path: dir})
		require.NoError(t, err)
		require.Positive(t, result.NodeCount)
		w.assertEngaged(t)
		assertFirstIndexReceipts(t, store, result)
		assert.Equal(t, writerSync, firstIndexWriterSynchronous(t, store))
		assertFirstIndexRestored(t, store, db, indexes)
	})

	t.Run("a retry after a failed drain engages the window again", func(t *testing.T) {
		store, db := newFirstIndexStore(t)
		indexes := firstIndexSchemaIndexes(t, db)
		writerSync := firstIndexWriterSynchronous(t, store)
		w := watchFirstIndexWindow(t, store, db, indexes)
		injected := errors.New("injected edge batch failure")
		refused := false
		w.fail = func(edgeRows int) error {
			if edgeRows > 0 && !refused {
				refused = true
				return injected
			}
			return nil
		}
		dir := firstIndexFixtureRepo(t)

		// The node batches land; the first edge batch is refused.
		_, err := newFirstIndexBulkIndexer(t, store, "repo").IndexCtx(context.Background(), dir)
		require.ErrorIs(t, err, injected)
		require.True(t, refused, "the drain never reached an edge batch")
		w.assertEngaged(t)
		require.Positive(t, store.NodeCount(), "the failed drain must leave rows for the retry to evict")
		assertFirstIndexNoReceipts(t, store, "repo")
		assertFirstIndexRestored(t, store, db, indexes)

		// The retry evicts them, which leaves the store cold again.
		w.batches, w.engaged, w.deferred = 0, 0, 0
		result, err := newFirstIndexBulkIndexer(t, store, "repo").IndexCtx(context.Background(), dir)
		require.NoError(t, err)
		w.assertEngaged(t)
		assertFirstIndexReceipts(t, store, result)
		assert.Equal(t, writerSync, firstIndexWriterSynchronous(t, store))
		assertFirstIndexRestored(t, store, db, indexes)
	})
}

func TestSecondRepoWarmStoreNoBulkWindow(t *testing.T) {
	probe, db := newFirstIndexBulkFixture(t)
	_, err := newFirstIndexBulkIndexer(t, probe, "a").IndexCtx(context.Background(), firstIndexFixtureRepo(t))
	require.NoError(t, err)
	require.Positive(t, probe.NodeCount())
	indexes := firstIndexSchemaIndexes(t, db)
	probe.begins, probe.flushes, probe.batches = 0, 0, 0
	probe.duringDrain = func() { assertFirstIndexRestored(t, probe.Store, db, indexes) }
	result, err := newFirstIndexBulkIndexer(t, probe, "b").IndexCtx(context.Background(), firstIndexFixtureRepo(t))
	require.NoError(t, err)
	require.Positive(t, result.NodeCount)
	require.Positive(t, probe.batches)
	assert.Equal(t, 1, probe.begins)
	assert.Equal(t, 1, probe.flushes)
	assertFirstIndexRestored(t, probe.Store, db, indexes)
	assert.Contains(t, probe.LoadFileMtimes("a"), "app.py")
	assert.Contains(t, probe.LoadFileMtimes("b"), "app.py")
}

func TestOuterCoordinatedWindowNotClosedByRepo(t *testing.T) {
	probe, db := newFirstIndexBulkFixture(t)
	indexes := firstIndexSchemaIndexes(t, db)
	require.True(t, probe.BeginCoordinatedBulkLoad())
	t.Cleanup(func() { require.NoError(t, probe.EndCoordinatedBulkLoad()) })
	during := firstIndexSchemaIndexes(t, db)
	require.NotEqual(t, indexes, during)
	probe.duringDrain = func() {
		assert.True(t, firstIndexBulkActive(t, probe.Store))
		assert.Equal(t, during, firstIndexSchemaIndexes(t, db))
	}
	_, err := newFirstIndexBulkIndexer(t, probe, "repo").IndexCtx(context.Background(), firstIndexFixtureRepo(t))
	require.NoError(t, err)
	require.Positive(t, probe.batches)
	assert.Equal(t, 1, probe.begins)
	assert.Equal(t, 1, probe.flushes)
	assert.True(t, firstIndexBulkActive(t, probe.Store), "repository must not close outer coordinated window")
	assert.Equal(t, during, firstIndexSchemaIndexes(t, db))
	require.NoError(t, probe.EndCoordinatedBulkLoad())
	assertFirstIndexRestored(t, probe.Store, db, indexes)
}

type firstIndexCancelReporter struct{ cancel context.CancelFunc }

func (r firstIndexCancelReporter) Report(stage string, _, _ int) {
	if stage == "parsing" {
		r.cancel()
	}
}

type firstIndexPanicReporter struct{}

func (firstIndexPanicReporter) Report(stage string, _, _ int) {
	if stage == "building search index" {
		panic("injected body panic")
	}
}

// firstIndexDrainFTSTrigger makes every symbol FTS append on store fail, the
// way a storage error inside the drain's FTS writes would.
func firstIndexDrainFTSTrigger(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	writer, err := sql.Open("sqlite", "file:"+filepath.ToSlash(store.Path()))
	require.NoError(t, err)
	_, err = writer.Exec(`CREATE TRIGGER fail_first_index_fts BEFORE INSERT ON symbol_fts_rowid
BEGIN SELECT RAISE(ABORT, 'injected drain FTS error'); END`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
}

// Every way a first index can fail, on the real store: the bulk window must
// close (no leaked write gate, writer durability back, the dense indexes back),
// no success receipt may be written, and the indexer must hand its disk target
// back. Faults inside the store come from its own test hooks, so the eviction
// the drain picks is the production one for a SQLite target.
func TestFirstIndexFailureRestoresStore(t *testing.T) {
	for _, mode := range []string{
		"drain error", "cancel before drain", "panic in body", "panic in drain",
		"storage full", "seal panic", "seal error",
	} {
		t.Run(mode, func(t *testing.T) {
			store, db := newFirstIndexStore(t)
			indexes := firstIndexSchemaIndexes(t, db)
			writerSync := firstIndexWriterSynchronous(t, store)
			var evictions []bool
			shadowEvictionBranchHook = func(retirement bool) { evictions = append(evictions, retirement) }
			t.Cleanup(func() { shadowEvictionBranchHook = nil })
			var fault func() error
			batches, engaged := 0, false
			defer store.SetBatchWriteHookForTest(func(int, int) error {
				batches++
				engaged = engaged || firstIndexBulkActive(t, store)
				if fault != nil {
					return fault()
				}
				return nil
			})()
			seals := 0
			defer store.SetBulkSealHookForTest(func(index string) error {
				seals++
				switch {
				case mode == "seal panic" && seals == 1:
					panic("injected seal panic")
				case mode == "seal error" && index == "nodes_by_kind":
					return errors.New("injected seal failure")
				}
				return nil
			})()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "drain error":
				firstIndexDrainFTSTrigger(t, store)
			case "panic in drain":
				fault = func() error { panic("injected drain panic") }
			case "storage full":
				full := sqliteFullError(t)
				fault = func() error { return full }
			case "cancel before drain":
				ctx = progress.WithReporter(ctx, firstIndexCancelReporter{cancel: cancel})
			case "panic in body":
				ctx = progress.WithReporter(ctx, firstIndexPanicReporter{})
			}
			idx := newFirstIndexBulkIndexer(t, store, "repo")
			if mode == "seal panic" {
				idx.indexMemoryAdmission = newIndexMemoryAdmissionBudget(defaultIndexMemoryAdmissionBytes)
			}
			dir := firstIndexFixtureRepo(t)

			switch mode {
			case "panic in body":
				assert.PanicsWithValue(t, "injected body panic", func() { _, _ = idx.IndexCtx(ctx, dir) })
				assert.Zero(t, store.NodeCount(), "body panic must not drain shadow nodes")
				assert.Zero(t, store.EdgeCount(), "body panic must not drain shadow edges")
			case "panic in drain":
				require.PanicsWithValue(t, "injected drain panic", func() { _, _ = idx.IndexCtx(ctx, dir) })
			case "seal panic":
				require.PanicsWithValue(t, "injected seal panic", func() { _, _ = idx.IndexCtx(ctx, dir) })
				assert.Greater(t, seals, 1, "the backstop did not retry the panicked flush")
				assert.Positive(t, idx.indexMemoryAdmission.snapshot().peak, "fixture must acquire the memory lease")
				assert.Zero(t, idx.indexMemoryAdmission.snapshot().used, "earlier defers must release the memory lease")
			case "cancel before drain":
				_, err := idx.IndexCtx(ctx, dir)
				require.ErrorIs(t, err, context.Canceled)
			case "drain error":
				_, err := idx.IndexCtx(ctx, dir)
				require.ErrorContains(t, err, "injected drain FTS error")
			case "storage full":
				_, err := idx.IndexCtx(ctx, dir)
				var storageErr *store_sqlite.StorageError
				require.ErrorAs(t, err, &storageErr)
				assert.Contains(t, store_sqlite.SafeStorageFailureReason(err), "SQLite code 13")
				assert.Equal(t, 1, batches, "a refused write must be the drain's last")
			case "seal error":
				_, err := idx.IndexCtx(ctx, dir)
				require.ErrorContains(t, err, "nodes_by_kind")
				// Either still owed or already rebuilt by the maintenance lane,
				// which runs as soon as the window closes on a volume with room.
				health, healthErr := store.PlannerStatsHealth(context.Background())
				require.NoError(t, healthErr)
				if health.Reason == "dense_indexes_missing" {
					assert.Equal(t, []string{"nodes_by_kind"}, health.DenseIndexesMissing)
				}
				deadline := time.Now().Add(10 * time.Second)
				for !assert.ObjectsAreEqual(indexes, firstIndexSchemaIndexes(t, db)) && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
			}

			if mode == "cancel before drain" || mode == "panic in body" {
				assert.Empty(t, evictions, "a failure before the drain must not evict")
				assert.Zero(t, batches, "a failure before the drain must not write")
			} else {
				assert.Equal(t, []bool{true}, evictions, "a SQLite target must take the shadow-replacement retirement")
				require.Positive(t, batches, "test must observe the shadow drain")
				assert.True(t, engaged, "failure fixture must exercise an engaged bulk window")
			}
			assert.Equal(t, writerSync, firstIndexWriterSynchronous(t, store), "writer durability must be restored")
			assertFirstIndexRestored(t, store, db, indexes)
			assertFirstIndexNoReceipts(t, store, "repo")
			assert.Same(t, store, idx.graph, "panic and error paths must restore disk target")
		})
	}
}

type firstIndexDrainPanicReporter struct{}

func (firstIndexDrainPanicReporter) Report(stage string, _, _ int) {
	if stage == "building symbol fts" {
		panic("injected drain reporter panic")
	}
}

func TestFirstIndexFailureRestoresSQLiteReplacement(t *testing.T) {
	for _, mode := range []string{"drain error", "panic after eviction"} {
		t.Run(mode, func(t *testing.T) {
			// Given an interrupted replacement with old graph rows and a Merkle baseline.
			probe, db := newFirstIndexBulkFixture(t)
			probe.Store.AddBatch([]*graph.Node{{
				ID: "repo::stale", Name: "stale", Kind: graph.KindFunction,
				RepoPrefix: "repo", FilePath: "removed.py", Language: "python",
			}}, nil)
			require.NoError(t, probe.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "repo", NodeCount: 1}))
			indexes := firstIndexSchemaIndexes(t, db)
			dir := firstIndexFixtureRepo(t)
			baseline := merkleTreeFile(dir)
			require.NoError(t, os.MkdirAll(filepath.Dir(baseline), 0o755))
			writeFile(t, baseline, "old replacement baseline")
			ctx := context.Background()
			if mode == "drain error" {
				firstIndexDrainFTSTrigger(t, probe.Store)
			} else {
				ctx = progress.WithReporter(ctx, firstIndexDrainPanicReporter{})
			}
			idx := newFirstIndexBulkIndexer(t, probe.Store, "repo")

			// When the drain fails after retiring replacement provenance.
			if mode == "drain error" {
				_, err := idx.IndexCtx(ctx, dir)
				require.ErrorContains(t, err, "injected drain FTS error")
			} else {
				require.PanicsWithValue(t, "injected drain reporter panic", func() { _, _ = idx.IndexCtx(ctx, dir) })
			}

			// Then the bulk window closes and the interrupted baseline cannot be reused.
			_, err := os.Stat(baseline)
			assert.ErrorIs(t, err, os.ErrNotExist)
			assertFirstIndexRestored(t, probe.Store, db, indexes)
			assertFirstIndexNoReceipts(t, probe.Store, "repo")
			assert.Same(t, probe.Store, idx.graph)
			assert.Nil(t, probe.GetNode("repo::stale"), "replacement must evict stale graph rows")
		})
	}
}
