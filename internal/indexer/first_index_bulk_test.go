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
)

// Embed the real store so the shadow and its optional persistence capabilities
// stay intact. Only the drain boundaries are intercepted.
type firstIndexBulkProbe struct {
	*store_sqlite.Store
	beforeBegin func()
	duringDrain func()
	ftsErr      error
	panicDrain  bool
	begins      int
	flushes     int
	batches     int
}

func (s *firstIndexBulkProbe) BeginBulkLoad() {
	s.begins++
	if s.beforeBegin != nil {
		s.beforeBegin()
	}
	s.Store.BeginBulkLoad()
}

func (s *firstIndexBulkProbe) beforeBatch() {
	s.batches++
	if s.duringDrain != nil {
		s.duringDrain()
	}
	if s.panicDrain {
		panic("injected drain panic")
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

func (s *firstIndexBulkProbe) BatchUpsertSymbolFTS(items []graph.SymbolFTSItem) error {
	if s.ftsErr != nil {
		return s.ftsErr
	}
	return s.Store.BatchUpsertSymbolFTS(items)
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
	// check writer restoration. TestBulkLoadDropsAndRebuildsIndexes in
	// store_sqlite checks the actual writer connection instead.
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

func TestFirstIndexDrainUsesBulkWindow(t *testing.T) {
	probe, db := newFirstIndexBulkFixture(t)
	indexes := firstIndexSchemaIndexes(t, db)
	probe.beforeBegin = func() { assertFirstIndexNoReceipts(t, probe.Store, "repo") }
	probe.duringDrain = func() {
		assert.True(t, firstIndexBulkActive(t, probe.Store), "cold shadow drain must engage standalone bulk mode")
		assertFirstIndexNoReceipts(t, probe.Store, "repo")
		assert.NotEqual(t, indexes, firstIndexSchemaIndexes(t, db), "dense indexes must be deferred during drain")
	}
	idx := newFirstIndexBulkIndexer(t, probe, "repo")
	result, err := idx.IndexCtx(context.Background(), firstIndexFixtureRepo(t))
	require.NoError(t, err)
	require.Positive(t, result.NodeCount)
	require.Positive(t, probe.batches, "test must observe the shadow drain")
	assert.Equal(t, 1, probe.begins)
	assert.Equal(t, 1, probe.flushes)
	state, found, err := probe.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, result.NodeCount, state.NodeCount)
	assert.Equal(t, result.EdgeCount, state.EdgeCount)
	assert.Contains(t, probe.LoadFileMtimes("repo"), "app.py")
	assertFirstIndexRestored(t, probe.Store, db, indexes)
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

func TestFirstIndexFailureRestoresStore(t *testing.T) {
	for _, mode := range []string{"drain error", "cancel before drain", "panic in body", "panic in drain"} {
		t.Run(mode, func(t *testing.T) {
			probe, db := newFirstIndexBulkFixture(t)
			indexes := firstIndexSchemaIndexes(t, db)
			injected := errors.New("injected drain FTS error")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "drain error":
				probe.ftsErr = injected
			case "panic in drain":
				probe.panicDrain = true
			case "cancel before drain":
				ctx = progress.WithReporter(ctx, firstIndexCancelReporter{cancel: cancel})
			case "panic in body":
				ctx = progress.WithReporter(ctx, firstIndexPanicReporter{})
			}
			probe.duringDrain = func() {
				assert.True(t, firstIndexBulkActive(t, probe.Store), "failure fixture must exercise an engaged bulk window")
			}
			idx := newFirstIndexBulkIndexer(t, probe, "repo")
			dir := firstIndexFixtureRepo(t)
			switch mode {
			case "panic in body":
				assert.PanicsWithValue(t, "injected body panic", func() { _, _ = idx.IndexCtx(ctx, dir) })
				assert.Zero(t, probe.NodeCount(), "body panic must not drain shadow nodes")
				assert.Zero(t, probe.EdgeCount(), "body panic must not drain shadow edges")
			case "panic in drain":
				require.PanicsWithValue(t, "injected drain panic", func() { _, _ = idx.IndexCtx(ctx, dir) })
			case "drain error":
				_, err := idx.IndexCtx(ctx, dir)
				require.ErrorIs(t, err, injected)
			default:
				_, err := idx.IndexCtx(ctx, dir)
				require.ErrorIs(t, err, context.Canceled)
			}
			if mode == "cancel before drain" || mode == "panic in body" {
				assert.Zero(t, probe.begins)
				assert.Zero(t, probe.batches)
			} else {
				require.Positive(t, probe.batches)
				assert.Equal(t, 1, probe.begins)
			}
			assertFirstIndexRestored(t, probe.Store, db, indexes)
			assertFirstIndexNoReceipts(t, probe.Store, "repo")
			assert.Same(t, probe, idx.graph, "panic and error paths must restore disk target")
		})
	}
}

func TestFirstIndexFlushPanicPropagates(t *testing.T) {
	// Given a real store whose dense-index seal panics once, inside the
	// drain's explicit FlushBulk.
	store, db := newFirstIndexStore(t)
	indexes := firstIndexSchemaIndexes(t, db)
	writerSync := firstIndexWriterSynchronous(t, store)
	seals := 0
	defer store.SetBulkSealHookForTest(func(string) error {
		seals++
		if seals == 1 {
			panic("injected seal panic")
		}
		return nil
	})()
	idx := newFirstIndexBulkIndexer(t, store, "repo")
	idx.indexMemoryAdmission = newIndexMemoryAdmissionBudget(defaultIndexMemoryAdmissionBytes)

	// When the completed shadow drain flushes, the panic reaches the caller.
	require.PanicsWithValue(t, "injected seal panic", func() {
		_, _ = idx.IndexCtx(context.Background(), firstIndexFixtureRepo(t))
	})

	// Then the backstop retried the flush once while the panic unwound, and
	// the retry sealed and closed the window: the write gate is free, the
	// writer is back at its own durability, and every index is rebuilt.
	assert.Greater(t, seals, 1, "the backstop did not retry the panicked flush")
	assert.Equal(t, writerSync, firstIndexWriterSynchronous(t, store))
	assertFirstIndexRestored(t, store, db, indexes)
	assert.Same(t, store, idx.graph)
	assert.Positive(t, idx.indexMemoryAdmission.snapshot().peak, "fixture must acquire the memory lease")
	assert.Zero(t, idx.indexMemoryAdmission.snapshot().used, "earlier defers must release the memory lease")
	assertFirstIndexNoReceipts(t, store, "repo")
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
				writer, err := sql.Open("sqlite", "file:"+filepath.ToSlash(probe.Path()))
				require.NoError(t, err)
				_, err = writer.Exec(`CREATE TRIGGER fail_first_index_fts BEFORE INSERT ON symbol_fts_rowid
BEGIN SELECT RAISE(ABORT, 'injected drain FTS error'); END`)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
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
