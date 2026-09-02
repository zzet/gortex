package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestSQLiteConnectionPoolsApplyPragmasPerPhysicalConnection(t *testing.T) {
	path := t.TempDir() + "/graph.sqlite"
	store, err := Open(path)
	require.NoError(t, err)

	require.NotSame(t, store.db, store.writerDB)
	expectedReaders := runtime.NumCPU()
	if expectedReaders > sqliteMaxOpenConns {
		expectedReaders = sqliteMaxOpenConns
	}
	assert.Equal(t, expectedReaders, store.db.Stats().MaxOpenConnections)
	assert.Equal(t, 1, store.writerDB.Stats().MaxOpenConnections)

	ctx := context.Background()
	readers := make([]*sql.Conn, 0, expectedReaders)
	for i := 0; i < expectedReaders; i++ {
		conn, err := store.db.Conn(ctx)
		require.NoError(t, err)
		readers = append(readers, conn)
	}
	for _, conn := range readers {
		assertSQLiteConnectionPragmas(t, conn, 0)
	}
	// Candidate-selection reads use per-connection TEMP relations. They must
	// remain available on the bounded query pool even though persistent graph
	// mutations are routed exclusively through writerDB.
	_, err = readers[0].ExecContext(ctx, `CREATE TEMP TABLE reader_candidates(id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = readers[0].ExecContext(ctx, `INSERT INTO reader_candidates(id) VALUES ('candidate')`)
	require.NoError(t, err)
	var candidate string
	require.NoError(t, readers[0].QueryRowContext(ctx, `SELECT id FROM reader_candidates`).Scan(&candidate))
	assert.Equal(t, "candidate", candidate)
	// mode=ro is stronger than a logical convention: TEMP remains writable,
	// but persistent graph mutation through the reader handle is rejected.
	_, err = readers[0].ExecContext(ctx, `CREATE TABLE forbidden_reader_write(id INTEGER)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "readonly")
	for _, conn := range readers {
		require.NoError(t, conn.Close())
	}

	writer, err := store.writerDB.Conn(ctx)
	require.NoError(t, err)
	assertSQLiteConnectionPragmas(t, writer, 0)
	require.NoError(t, writer.Close())

	// Prepared mutations must be compiled against the writer handle, while the
	// corresponding read is served by the bounded query pool.
	store.AddNode(&graph.Node{ID: "repo/file.go::Fn", Kind: graph.KindFunction, Name: "Fn"})
	require.NotNil(t, store.GetNode("repo/file.go::Fn"))
	require.NoError(t, store.Close())
}

func TestSQLiteInMemoryStoreKeepsOneSharedBoundedHandle(t *testing.T) {
	store, err := Open(":memory:")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	require.Same(t, store.db, store.writerDB)
	assert.Equal(t, 1, store.db.Stats().MaxOpenConnections)
	var queryOnly int
	require.NoError(t, store.db.QueryRow(`PRAGMA query_only`).Scan(&queryOnly))
	assert.Zero(t, queryOnly)

	store.AddNode(&graph.Node{ID: "memory::Fn", Kind: graph.KindFunction, Name: "Fn"})
	require.NotNil(t, store.GetNode("memory::Fn"))
}

func TestSingleEdgeMutationsReusePinnedBulkWriter(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	store.BeginBulkLoad()
	require.NotNil(t, store.bulkConn)
	edge := &graph.Edge{
		From: "repo/caller.go::Caller", To: "unresolved::Target", Kind: graph.EdgeCalls,
		FilePath: "repo/caller.go", Line: 7, Origin: "parser",
	}
	store.AddNode(&graph.Node{ID: edge.From, Kind: graph.KindFunction, Name: "Caller"})
	store.AddEdge(edge)
	require.True(t, store.SetEdgeProvenance(edge, "resolver"))
	edge.Confidence = 0.9
	edge.ConfidenceLabel = "confirmed"
	store.PersistEdgeAttributes(edge)
	require.True(t, store.RemoveEdge(edge.From, edge.To, edge.Kind))
	require.NoError(t, store.FlushBulk())
	assert.Empty(t, store.GetOutEdges(edge.From))
}

func TestWriterTransactionsBeginImmediate(t *testing.T) {
	path := t.TempDir() + "/graph.sqlite"
	store, err := Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	setWriterBusyTimeout(t, store, 0)
	locker, lockTx := holdExternalWriter(t, path)

	store.writeMu.Lock()
	tx, err := store.beginWrite()
	store.writeMu.Unlock()
	assert.Nil(t, tx)
	require.Error(t, err)
	assert.True(t, isSQLiteBusyErr(err), "BEGIN must reserve the writer instead of opening a DEFERRED read transaction")

	require.NoError(t, lockTx.Rollback())
	require.NoError(t, locker.Close())
}

func TestReindexEdgesRetriesWholeTransactionAfterBusy(t *testing.T) {
	path := t.TempDir() + "/graph.sqlite"
	store, batch := openBusyReindexFixture(t, path)
	setWriterBusyTimeout(t, store, 0)
	locker, lockTx := holdExternalWriter(t, path)

	result := make(chan error, 1)
	go func() {
		_, err := store.reindexEdgesSetOriented(batch)
		result <- err
	}()
	requireBusyOperationBlocked(t, result, "reindex")
	require.NoError(t, lockTx.Rollback())
	require.NoError(t, locker.Close())
	require.NoError(t, <-result)

	requireReindexedTarget(t, store, "repo/target.go::Target")
	require.NoError(t, store.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	requireReindexedTarget(t, reopened, "repo/target.go::Target")
	require.NoError(t, reopened.Close())
}

func TestReceiverRebindRetriesBusyBeginOnPinnedWriter(t *testing.T) {
	path := t.TempDir() + "/graph.sqlite"
	store, err := Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	const (
		canonical = "repo::pkg/types.go::T"
		method    = "repo::pkg/methods.go::T.M"
	)
	store.AddBatch([]*graph.Node{
		{ID: canonical, Kind: graph.KindType, Name: "T", FilePath: "repo::pkg/types.go", Language: "go", RepoPrefix: "repo"},
		{ID: method, Kind: graph.KindMethod, Name: "M", FilePath: "repo::pkg/methods.go", Language: "go", RepoPrefix: "repo"},
	}, []*graph.Edge{{From: method, To: "repo::pkg/methods.go::T", Kind: graph.EdgeMemberOf, FilePath: "repo::pkg/methods.go", Line: 1}})
	setWriterBusyTimeout(t, store, 0)
	locker, lockTx := holdExternalWriter(t, path)

	type result struct {
		changed int
		err     error
	}
	done := make(chan result, 1)
	go func() {
		changed, rebindErr := store.RebindGoMethodReceivers("")
		done <- result{changed: changed, err: rebindErr}
	}()
	requireBusyOperationBlocked(t, done, "receiver rebind")
	require.NoError(t, lockTx.Rollback())
	require.NoError(t, locker.Close())
	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, 1, got.changed)
}

func TestReindexEdgesPersistentBusySurfacesAndRollsBack(t *testing.T) {
	path := t.TempDir() + "/graph.sqlite"
	store, batch := openBusyReindexFixture(t, path)
	defer func() { require.NoError(t, store.Close()) }()
	setWriterBusyTimeout(t, store, 0)
	store.busyRetryTimeout = 60 * time.Millisecond
	locker, lockTx := holdExternalWriter(t, path)

	started := time.Now()
	_, err := store.reindexEdgesSetOriented(batch)
	elapsed := time.Since(started)
	require.Error(t, err)
	assert.True(t, isSQLiteBusyErr(err), "the exhausted error must retain the SQLite result code")
	assert.ErrorIs(t, err, errSQLiteBusyRetryExhausted)
	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
	assert.Less(t, elapsed, time.Second)
	requireReindexedTarget(t, store, "unresolved::Target")

	require.NoError(t, lockTx.Rollback())
	require.NoError(t, locker.Close())
	_, err = store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	requireReindexedTarget(t, store, "repo/target.go::Target")
}

func TestLongWALReaderDoesNotBlockReindexWriter(t *testing.T) {
	path := t.TempDir() + "/graph.sqlite"
	store, batch := openBusyReindexFixture(t, path)
	defer func() { require.NoError(t, store.Close()) }()

	// Occupy every query-pool slot with an open WAL snapshot. The dedicated
	// writer must neither wait for a database/sql slot nor fail its commit.
	type heldReader struct {
		conn *sql.Conn
		tx   *sql.Tx
		rows *sql.Rows
	}
	ctx := context.Background()
	held := make([]heldReader, 0, store.db.Stats().MaxOpenConnections)
	for i := 0; i < cap(held); i++ {
		conn, err := store.db.Conn(ctx)
		require.NoError(t, err)
		readTx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		rows, err := readTx.QueryContext(ctx, `SELECT from_id FROM edges`)
		require.NoError(t, err)
		require.True(t, rows.Next())
		require.NoError(t, rows.Err())
		held = append(held, heldReader{conn: conn, tx: readTx, rows: rows})
	}

	started := time.Now()
	_, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Less(t, time.Since(started), time.Second)

	for _, reader := range held {
		require.NoError(t, reader.rows.Close())
		require.NoError(t, reader.tx.Rollback())
		require.NoError(t, reader.conn.Close())
	}
	requireReindexedTarget(t, store, "repo/target.go::Target")
}

func TestCheckpointSerializesBehindWriterGate(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	store.writeMu.Lock()
	done := make(chan error, 1)
	go func() { done <- store.CheckpointWAL() }()
	select {
	case err := <-done:
		store.writeMu.Unlock()
		require.Failf(t, "checkpoint bypassed writer gate", "returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	store.writeMu.Unlock()
	require.NoError(t, <-done)
}

func TestSQLiteEscapedPathPreservesReadOnlyReaderAcrossReopen(t *testing.T) {
	// '?' is not a portable filename character, but it must still be escaped in
	// a DSN assembled for platforms that allow it.
	dsn := sqliteReaderDSN(filepath.Join("nested dir", "graph # ?.sqlite"))
	assert.Contains(t, dsn, "file:")
	assert.Contains(t, dsn, "%20")
	assert.Contains(t, dsn, "%23")
	assert.Contains(t, dsn, "%3F")

	path := filepath.Join(t.TempDir(), "graph space #.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	store.AddNode(&graph.Node{ID: "repo/path.go::Path", Kind: graph.KindFunction, Name: "Path"})
	assertReaderTempWritableMainReadOnly(t, store)
	require.NoError(t, store.Close())

	store, err = Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	require.NotNil(t, store.GetNode("repo/path.go::Path"))
	assertReaderTempWritableMainReadOnly(t, store)
}

func assertReaderTempWritableMainReadOnly(t *testing.T, store *Store) {
	t.Helper()
	conn, err := store.db.Conn(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	_, err = conn.ExecContext(context.Background(), `CREATE TEMP TABLE path_candidates(id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `INSERT INTO path_candidates(id) VALUES ('candidate')`)
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `CREATE TABLE forbidden_path_reader_write(id INTEGER)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "readonly")
}

func TestSQLiteWriteGatePrefersCanceledContext(t *testing.T) {
	var gate sqliteWriteGate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, gate.LockContext(ctx), context.Canceled)
	// A canceled acquisition must leave the token available.
	require.True(t, gate.TryLock())
	gate.Unlock()
}

func TestCheckpointWriterGateWaitHonorsContext(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	store.writeMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	started := time.Now()
	err = store.checkpointWALWithContext(ctx)
	cancel()
	store.writeMu.Unlock()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
}

func TestReindexWriterGateWaitHonorsContext(t *testing.T) {
	store, batch := openBusyReindexFixture(t, t.TempDir()+"/graph.sqlite")
	defer func() { require.NoError(t, store.Close()) }()
	store.busyRetryTimeout = 50 * time.Millisecond

	store.writeMu.Lock()
	started := time.Now()
	stats, err := store.reindexEdgesSetOriented(batch)
	store.writeMu.Unlock()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
	assert.Equal(t, sqliteReindexSetStats{}, stats)
	requireReindexedTarget(t, store, "unresolved::Target")
}

func TestBulkCheckpointDefersThenRunsAfterRestoreAndRelease(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	store.BeginBulkLoad()
	require.NotNil(t, store.bulkConn)
	previousSync := store.bulkPrevSync
	currentSync, err := pragmaInt(context.Background(), store.bulkConn, "synchronous")
	require.NoError(t, err)
	assert.EqualValues(t, 0, currentSync)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	started := time.Now()
	err = store.checkpointWALWithContext(ctx)
	cancel()
	require.ErrorIs(t, err, errWALCheckpointDeferredBulk)
	assert.Less(t, time.Since(started), time.Second)

	checkpointEvents := 0
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
		if event.Stage != "checkpoint" {
			return
		}
		checkpointEvents++
		require.NoError(t, event.Err)
		assert.Nil(t, store.bulkConn)
		gateFree := store.writeMu.TryLock()
		assert.True(t, gateFree, "final checkpoint observer must run after releasing writeMu")
		if gateFree {
			store.writeMu.Unlock()
		}
		conn, connErr := store.writerDB.Conn(context.Background())
		require.NoError(t, connErr)
		restoredSync, syncErr := pragmaInt(context.Background(), conn, "synchronous")
		require.NoError(t, syncErr)
		assert.Equal(t, previousSync, restoredSync)
		require.NoError(t, conn.Close())
	}

	store.AddNode(&graph.Node{ID: "repo/bulk.go::Bulk", Kind: graph.KindFunction, Name: "Bulk"})
	require.NoError(t, store.FlushBulk())
	assert.Equal(t, 1, checkpointEvents)
}

func TestBulkRebuildFailureStillRestoresCheckpointsAndLeavesWriterUsable(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	originalIndexes := bulkDroppableIndexes
	bulkDroppableIndexes = append(append([]bulkDroppableIndex(nil), originalIndexes...), bulkDroppableIndex{
		name: "forced_bad_index",
		ddl:  `CREATE INDEX forced_bad_index ON definitely_missing_table(id)`,
	})
	defer func() { bulkDroppableIndexes = originalIndexes }()

	store.BeginBulkLoad()
	require.NotNil(t, store.bulkConn)
	previousSync := store.bulkPrevSync
	store.AddNode(&graph.Node{ID: "repo/bulk.go::BeforeFailure", Kind: graph.KindFunction, Name: "BeforeFailure"})

	checkpointEvents := 0
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
		if event.Stage == "checkpoint" {
			checkpointEvents++
		}
	}
	err = store.FlushBulk()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced_bad_index")
	assert.Equal(t, 1, checkpointEvents, "durability checkpoint must run despite index rebuild failure")
	assert.Nil(t, store.bulkConn)

	conn, err := store.writerDB.Conn(context.Background())
	require.NoError(t, err)
	restoredSync, err := pragmaInt(context.Background(), conn, "synchronous")
	require.NoError(t, err)
	assert.Equal(t, previousSync, restoredSync)
	require.NoError(t, conn.Close())

	store.AddNode(&graph.Node{ID: "repo/after.go::AfterFailure", Kind: graph.KindFunction, Name: "AfterFailure"})
	require.NotNil(t, store.GetNode("repo/after.go::AfterFailure"))
}

func TestCheckpointBusyResultIsRetriedAndNeverReportedAsSuccess(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"})
	readConn, err := store.db.Conn(context.Background())
	require.NoError(t, err)
	readTx, err := readConn.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() {
		_ = readTx.Rollback()
		_ = readConn.Close()
	}()
	var count int
	require.NoError(t, readTx.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count))
	store.AddNode(&graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B"})

	started := time.Now()
	store.checkpointWALPassive()
	assert.Less(t, time.Since(started), time.Second, "PASSIVE maintenance must not wait for the long reader")

	setWriterBusyTimeout(t, store, 0)
	store.busyRetryTimeout = 80 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	started = time.Now()
	err = store.checkpointWALWithContext(ctx)
	cancel()
	elapsed := time.Since(started)
	require.Error(t, err)
	assert.ErrorIs(t, err, errSQLiteCheckpointIncomplete)
	assert.ErrorIs(t, err, errSQLiteBusyRetryExhausted)
	assert.GreaterOrEqual(t, elapsed, 60*time.Millisecond)
	assert.Less(t, elapsed, time.Second)

	require.NoError(t, readTx.Rollback())
	require.NoError(t, readConn.Close())
	setWriterBusyTimeout(t, store, sqliteBusyTimeoutMillis)
	store.busyRetryTimeout = 0
	require.NoError(t, store.CheckpointWAL())
}

const (
	evictFixtureRepo     = "repo"
	evictFixtureFile     = "repo/a.go"
	evictFixtureKeepFile = "repo/keep.go"
	evictFixtureA        = "repo::a.go::A"
	evictFixtureB        = "repo::a.go::B"
	evictFixtureKeep     = "repo::keep.go::Keep"
)

func seedAtomicEvictionFixture(t *testing.T, store *Store) {
	t.Helper()
	store.AddBatch([]*graph.Node{
		{ID: evictFixtureA, Kind: graph.KindFunction, Name: "A", FilePath: evictFixtureFile, RepoPrefix: evictFixtureRepo},
		{ID: evictFixtureB, Kind: graph.KindFunction, Name: "B", FilePath: evictFixtureFile, RepoPrefix: evictFixtureRepo},
		{ID: evictFixtureKeep, Kind: graph.KindFunction, Name: "Keep", FilePath: evictFixtureKeepFile, RepoPrefix: evictFixtureRepo},
	}, []*graph.Edge{
		{From: evictFixtureA, To: evictFixtureB, Kind: graph.EdgeCalls, FilePath: evictFixtureFile, Line: 1},
		{From: evictFixtureKeep, To: evictFixtureA, Kind: graph.EdgeCalls, FilePath: evictFixtureKeepFile, Line: 2},
		{From: evictFixtureB, To: evictFixtureKeep, Kind: graph.EdgeCalls, FilePath: evictFixtureFile, Line: 3},
		{From: evictFixtureKeep, To: evictFixtureKeep, Kind: graph.EdgeCalls, FilePath: evictFixtureKeepFile, Line: 4},
	})
	require.NoError(t, store.ReplaceSemanticBindingTypes(evictFixtureRepo, []graph.SemanticBindingType{
		{Site: graph.SemanticBindingSite{RepoPrefix: evictFixtureRepo, FilePath: evictFixtureFile, Line: 1, Name: "A"}, TypeName: "repo.A"},
		{Site: graph.SemanticBindingSite{RepoPrefix: evictFixtureRepo, FilePath: evictFixtureKeepFile, Line: 1, Name: "Keep"}, TypeName: "repo.Keep"},
	}))
}

func requireAtomicEvictionFixturePresent(t *testing.T, store *Store) {
	t.Helper()
	var nodes, edges, bindings int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edges))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM semantic_binding_types`).Scan(&bindings))
	require.Equal(t, 3, nodes)
	require.Equal(t, 4, edges)
	require.Equal(t, 2, bindings)
}

func requireAtomicFileEvictionCommitted(t *testing.T, store *Store) {
	t.Helper()
	require.Nil(t, store.GetNode(evictFixtureA))
	require.Nil(t, store.GetNode(evictFixtureB))
	require.NotNil(t, store.GetNode(evictFixtureKeep))
	edges := store.GetOutEdges(evictFixtureKeep)
	require.Len(t, edges, 1)
	require.Equal(t, evictFixtureKeep, edges[0].To)
	var droppedBindings, keptBindings int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM semantic_binding_types WHERE file_path = ?`, evictFixtureFile).Scan(&droppedBindings))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM semantic_binding_types WHERE file_path = ?`, evictFixtureKeepFile).Scan(&keptBindings))
	require.Zero(t, droppedBindings)
	require.Equal(t, 1, keptBindings)
}

func TestEvictFileRetriesBusyBeginAndCommitsAtomically(t *testing.T) {
	path := t.TempDir() + "/graph.sqlite"
	store, err := Open(path)
	require.NoError(t, err)
	seedAtomicEvictionFixture(t, store)
	setWriterBusyTimeout(t, store, 0)
	locker, lockTx := holdExternalWriter(t, path)
	released := false
	defer func() {
		if !released {
			_ = lockTx.Rollback()
		}
		_ = locker.Close()
	}()

	done := make(chan [2]int, 1)
	go func() {
		nodes, edges := store.EvictFile(evictFixtureFile)
		done <- [2]int{nodes, edges}
	}()
	requireBusyOperationBlocked(t, done, "file eviction")
	require.NoError(t, lockTx.Rollback())
	require.NoError(t, locker.Close())
	released = true

	var got [2]int
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("file eviction did not finish after releasing the external writer")
	}
	require.Equal(t, [2]int{2, 3}, got)
	requireAtomicFileEvictionCommitted(t, store)
	require.NoError(t, store.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	requireAtomicFileEvictionCommitted(t, reopened)
}

func TestEvictFileRollbackKeepsGraphAnalysisReceiptAndBindings(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	seedAtomicEvictionFixture(t, store)
	generationID := buildMinimalAnalysisGeneration(t, store, "eviction-rollback", 0, true)
	require.True(t, store.analysisGenerationPresent)
	_, err = store.writerDB.Exec(`CREATE TRIGGER fail_atomic_evict
BEFORE DELETE ON analysis_active_generation
BEGIN
    SELECT RAISE(ABORT, 'forced atomic eviction rollback');
END`)
	require.NoError(t, err)

	analysisRevision := store.AnalysisMutationRevision()
	edgeRevision := store.EdgeMutationRevision()
	token := store.BeginMutationReceipt()
	nodes, edges, evictErr := store.evictByPredicateResult(evictFilePredicate, evictFixtureFile, true)
	require.ErrorContains(t, evictErr, "forced atomic eviction rollback")
	require.Zero(t, nodes)
	require.Zero(t, edges)
	receipt := store.EndMutationReceipt(token)
	require.True(t, receipt.Complete)
	require.False(t, receipt.ResolutionRelevant)
	require.Equal(t, analysisRevision, store.AnalysisMutationRevision())
	require.Equal(t, edgeRevision, store.EdgeMutationRevision())
	require.True(t, store.analysisGenerationPresent)
	requireAtomicEvictionFixturePresent(t, store)
	var active, state int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM analysis_active_generation WHERE generation_id = ?`, generationID).Scan(&active))
	require.NoError(t, store.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&state))
	require.Equal(t, 1, active)
	require.Equal(t, analysisGenerationReady, state)

	_, err = store.writerDB.Exec(`DROP TRIGGER fail_atomic_evict`)
	require.NoError(t, err)
	token = store.BeginMutationReceipt()
	nodes, edges = store.EvictFile(evictFixtureFile)
	require.Equal(t, 2, nodes)
	require.Equal(t, 3, edges)
	receipt = store.EndMutationReceipt(token)
	// The fixture keeps a RESOLVED surviving caller (Keep -> A). The evict
	// destroys that edge rather than parking it, and no resolution pass
	// reconstructs a deleted edge, so the eviction's resolution delta is
	// still exactly describable and the receipt stays complete.
	require.True(t, receipt.Complete,
		"an eviction that destroys a surviving caller edge still describes its resolution delta exactly")
	require.False(t, store.analysisGenerationPresent)
	require.Greater(t, store.AnalysisMutationRevision(), analysisRevision)
	require.Greater(t, store.EdgeMutationRevision(), edgeRevision)
	requireAtomicFileEvictionCommitted(t, store)
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM analysis_active_generation`).Scan(&active))
	require.Zero(t, active)
	require.NoError(t, store.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&state))
	require.Equal(t, analysisGenerationStale, state)
}

func TestEvictRepoLargeScopeUsesIndexedSetCandidates(t *testing.T) {
	path := t.TempDir() + "/graph.sqlite"
	store, err := Open(path)
	require.NoError(t, err)
	const targetNodes = 2048 // exceeds two historical 900-ID frontier chunks
	nodes := make([]*graph.Node, 0, targetNodes+1)
	edges := make([]*graph.Edge, 0, targetNodes+2)
	ids := make([]string, targetNodes)
	for i := range targetNodes {
		id := "drop::pkg::N" + strconv.Itoa(i)
		ids[i] = id
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: "N", FilePath: "drop/file-" + strconv.Itoa(i%8) + ".go", RepoPrefix: "drop"})
		if i > 0 {
			edges = append(edges, &graph.Edge{From: ids[i-1], To: id, Kind: graph.EdgeCalls, FilePath: "drop/chain.go", Line: i})
		}
	}
	const keepID = "keep::pkg::Keep"
	nodes = append(nodes, &graph.Node{ID: keepID, Kind: graph.KindFunction, Name: "Keep", FilePath: "keep/keep.go", RepoPrefix: "keep"})
	edges = append(edges,
		&graph.Edge{From: ids[0], To: keepID, Kind: graph.EdgeCalls, FilePath: "drop/chain.go", Line: targetNodes + 1},
		&graph.Edge{From: keepID, To: ids[targetNodes-1], Kind: graph.EdgeCalls, FilePath: "keep/keep.go", Line: targetNodes + 2},
		&graph.Edge{From: keepID, To: keepID, Kind: graph.EdgeCalls, FilePath: "keep/keep.go", Line: targetNodes + 3},
	)
	store.AddBatch(nodes, edges)
	require.NoError(t, store.ReplaceSemanticBindingTypes("drop", []graph.SemanticBindingType{{Site: graph.SemanticBindingSite{RepoPrefix: "drop", FilePath: "drop/file-0.go", Line: 1, Name: "N"}, TypeName: "drop.N"}}))
	require.NoError(t, store.ReplaceSemanticBindingTypes("keep", []graph.SemanticBindingType{{Site: graph.SemanticBindingSite{RepoPrefix: "keep", FilePath: "keep/keep.go", Line: 1, Name: "Keep"}, TypeName: "keep.Keep"}}))

	planRows, err := store.writerDB.Query(`EXPLAIN QUERY PLAN SELECT id FROM nodes WHERE `+evictNonEmptyRepoPredicate, "drop")
	require.NoError(t, err)
	var plan strings.Builder
	for planRows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, planRows.Scan(&id, &parent, &unused, &detail))
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	require.NoError(t, planRows.Err())
	require.NoError(t, planRows.Close())
	require.Contains(t, plan.String(), "nodes_by_repo")

	nodesRemoved, edgesRemoved := store.EvictRepo("drop")
	require.Equal(t, targetNodes, nodesRemoved)
	require.Equal(t, targetNodes+1, edgesRemoved)
	var nodeCount, edgeCount, dropBindings, keepBindings int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodeCount))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edgeCount))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM semantic_binding_types WHERE repo_prefix = 'drop'`).Scan(&dropBindings))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM semantic_binding_types WHERE repo_prefix = 'keep'`).Scan(&keepBindings))
	require.Equal(t, 1, nodeCount)
	require.Equal(t, 1, edgeCount)
	require.Zero(t, dropBindings)
	require.Equal(t, 1, keepBindings)
	require.NotNil(t, store.GetNode(keepID))
	require.NoError(t, store.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	require.NotNil(t, reopened.GetNode(keepID))
	require.Nil(t, reopened.GetNode(ids[0]))
	require.Len(t, reopened.GetOutEdges(keepID), 1)
}

func assertSQLiteConnectionPragmas(t *testing.T, conn *sql.Conn, wantQueryOnly int) {
	t.Helper()
	ctx := context.Background()
	var (
		busyTimeout int
		journalMode string
		synchronous int
		foreignKeys int
		cacheSize   int
		tempStore   int
		queryOnly   int
	)
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout))
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode))
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous))
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys))
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA cache_size`).Scan(&cacheSize))
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&tempStore))
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly))
	assert.Equal(t, sqliteBusyTimeoutMillis, busyTimeout)
	assert.Equal(t, "wal", journalMode)
	assert.Equal(t, 1, synchronous) // SQLITE_SYNC_NORMAL
	assert.Equal(t, 1, foreignKeys)
	assert.Equal(t, -32768, cacheSize)
	assert.Equal(t, 2, tempStore) // SQLITE_TEMP_STORE_MEMORY
	assert.Equal(t, wantQueryOnly, queryOnly)
}

func openBusyReindexFixture(t *testing.T, path string) (*Store, []graph.EdgeReindex) {
	t.Helper()
	store, err := Open(path)
	require.NoError(t, err)
	old := &graph.Edge{
		From: "repo/caller.go::Caller", To: "unresolved::Target", Kind: graph.EdgeCalls,
		FilePath: "repo/caller.go", Line: 7, Origin: "parser",
	}
	store.AddBatch([]*graph.Node{
		{ID: old.From, Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.go", RepoPrefix: "repo"},
		{ID: "repo/target.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "repo/target.go", RepoPrefix: "repo"},
	}, []*graph.Edge{old})
	resolved := *old
	resolved.To = "repo/target.go::Target"
	resolved.Origin = "resolver"
	return store, []graph.EdgeReindex{{Edge: &resolved, OldTo: old.To}}
}

func requireBusyOperationBlocked[T any](t *testing.T, result <-chan T, operation string) {
	t.Helper()
	select {
	case value := <-result:
		t.Fatalf("%s completed while the external writer lock was held: %v", operation, value)
	case <-time.After(50 * time.Millisecond):
	}
}

func setWriterBusyTimeout(t *testing.T, store *Store, milliseconds int) {
	t.Helper()
	conn, err := store.writerDB.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `PRAGMA busy_timeout = `+strconv.Itoa(milliseconds))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func holdExternalWriter(t *testing.T, path string) (*sql.DB, *sql.Tx) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteWriterDSN(path))
	require.NoError(t, err)
	configureWriterPool(db)
	tx, err := db.Begin()
	require.NoError(t, err)
	return db, tx
}

func requireReindexedTarget(t *testing.T, store *Store, want string) {
	t.Helper()
	edges := store.GetOutEdges("repo/caller.go::Caller")
	require.Len(t, edges, 1)
	assert.Equal(t, want, edges[0].To)
}

// TestWriterDSNSpacesWALAutoCheckpoints pins the widened auto-checkpoint
// spacing on the writer DSN: the SQLite default (1000 pages ≈ 4 MB) forces a
// checkpoint flush every few MB of a scattered index-write burst. Readers
// never append to the WAL, so the reader DSN stays untouched.
func TestWriterDSNSpacesWALAutoCheckpoints(t *testing.T) {
	dsn := sqliteWriterDSN("x.sqlite")
	if !strings.Contains(dsn, "_pragma=wal_autocheckpoint(8000)") {
		t.Fatalf("writer DSN missing wal_autocheckpoint spacing: %s", dsn)
	}
	if strings.Contains(sqliteReaderDSN("x.sqlite"), "wal_autocheckpoint") {
		t.Fatal("reader DSN should not carry wal_autocheckpoint")
	}
}
