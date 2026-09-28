package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The generation-scoped bulk write shape, and the deterministic drain of what a
// cold load leaves in the WAL.
//
// A committed generation's payload is written through the ordinary incremental
// path, because the cold fast path is gated on the whole STORE being empty and
// generation 0 consumed it. BeginGenerationBulkLoad gives that payload the
// enlarged page cache while leaving dense indexes and durability alone: the
// generation-0 graph remains live underneath it. SQLite's commit hook stays
// disabled on every writer connection; the application-owned pressure line is
// serviced by the bounded checkpoint loop, and a bulk end schedules one bounded
// TRUNCATE when its PASSIVE finalization leaves residue above that line.

func walFileBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path + "-wal")
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat %s-wal: %v", path, err)
	}
	return info.Size()
}

func walFileFrames(t *testing.T, path string, pageSize int64) int {
	t.Helper()
	size := walFileBytes(t, path)
	if size <= 32 {
		return 0
	}
	return int((size - 32) / (pageSize + 24))
}

func holdAReadSnapshot(t *testing.T, store *Store) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatalf("read connection: %v", err)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read transaction: %v", err)
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&n); err != nil {
		_ = tx.Rollback()
		_ = conn.Close()
		t.Fatalf("materialise the read snapshot: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(); _ = conn.Close() }) }
	t.Cleanup(release)
	return release
}

func generationRowCounts(t *testing.T, store *Store, generationID int64) (nodes, edges int64) {
	t.Helper()
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE view_gen = ?`, generationID).Scan(&nodes); err != nil {
		t.Fatalf("count nodes at generation %d: %v", generationID, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE view_gen = ?`, generationID).Scan(&edges); err != nil {
		t.Fatalf("count edges at generation %d: %v", generationID, err)
	}
	return nodes, edges
}

func TestGenerationBulkLoadDefersTheAutomaticDrainToOneAtItsEnd(t *testing.T) {
	const line = 100
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", strconv.Itoa(line))
	const generationID = int64(1)
	path := filepath.Join(t.TempDir(), "generation.sqlite")
	store, err := openPristine(t, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	pageSize := pragmaIntDB(t, store.db, "page_size")
	baseNodes, baseEdges := bulkFixture(512, 1024)
	store.AddBatch(baseNodes, baseEdges)
	if err := store.CheckpointWAL(); err != nil {
		t.Fatalf("drain the seed WAL: %v", err)
	}
	if got := pragmaIntDB(t, store.writerDB, "wal_autocheckpoint"); got != 0 {
		t.Fatalf("writer starts at wal_autocheckpoint=%d, want 0", got)
	}
	engaged, err := store.BeginGenerationBulkLoad(generationID)
	if err != nil || !engaged {
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v), want engaged", engaged, err)
	}
	defer func() { _ = store.EndGenerationBulkLoad() }()
	if got, err := pragmaInt(context.Background(), store.bulkConn, "wal_autocheckpoint"); err != nil || got != 0 {
		t.Fatalf("bulk writer wal_autocheckpoint = %d (err %v), want 0", got, err)
	}
	const nodeCount, edgeCount = 1200, 2400
	nodes, edges := bulkFixture(nodeCount, edgeCount)
	const nodeChunk, edgeChunk = 200, 400
	handle := store.AtGeneration(generationID)
	for i := 0; i < len(nodes); i += nodeChunk {
		nodeEnd := min(i+nodeChunk, len(nodes))
		edgeStart := min(i/nodeChunk*edgeChunk, len(edges))
		edgeEnd := min(edgeStart+edgeChunk, len(edges))
		if err := handle.AddBatchChecked(nodes[i:nodeEnd], edges[edgeStart:edgeEnd]); err != nil {
			t.Fatalf("AddBatchChecked: %v", err)
		}
	}
	if frames := walFileFrames(t, path, pageSize); frames <= 5*line {
		t.Fatalf("payload left only %d frames against %d-page line", frames, line)
	}
	requestsBefore := store.walDrainRequests.Load()
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatalf("EndGenerationBulkLoad: %v", err)
	}
	if got := pragmaIntDB(t, store.writerDB, "wal_autocheckpoint"); got != 0 {
		t.Fatalf("bulk end restored wal_autocheckpoint=%d, want 0", got)
	}
	if got := store.walDrainRequests.Load(); got != requestsBefore+1 {
		t.Fatalf("bulk end posted %d drains, want one", got-requestsBefore)
	}
	waitForCondition(t, "scheduled drain under line", func() bool { return store.walDrains.Load() > 0 && walFileFrames(t, path, pageSize) <= line })
	nodeRows, edgeRows := generationRowCounts(t, store, generationID)
	if nodeRows != nodeCount || edgeRows == 0 {
		t.Fatalf("generation holds %d nodes/%d edges", nodeRows, edgeRows)
	}
}

func TestGenerationBulkLoadRefusesWhatItCannotProveEmpty(t *testing.T) {
	t.Run("generation zero", func(t *testing.T) {
		store, _ := openTempStore(t)
		engaged, err := store.BeginGenerationBulkLoad(baseViewGeneration)
		if engaged || !errors.Is(err, ErrCatalogInvalidValue) {
			t.Fatalf("got (%v,%v)", engaged, err)
		}
	})
	t.Run("populated", func(t *testing.T) {
		store, _ := openTempStore(t)
		nodes, edges := bulkFixture(64, 64)
		store.AddBatch(nodes, edges)
		store.AtGeneration(7).AddBatch(nodes, edges)
		engaged, err := store.BeginGenerationBulkLoad(7)
		if engaged || !errors.Is(err, ErrGenerationBulkLoadPopulated) {
			t.Fatalf("got (%v,%v)", engaged, err)
		}
		if store.bulkConn != nil {
			t.Fatal("refused window pinned writer")
		}
		engaged, err = store.BeginGenerationBulkLoad(8)
		if err != nil || !engaged {
			t.Fatalf("sibling got (%v,%v)", engaged, err)
		}
		if err := store.EndGenerationBulkLoad(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("published", func(t *testing.T) {
		store := openCatalogStore(t)
		ctx := context.Background()
		id, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{OwnerKind: "ref_view", GraphID: "graph-bulk", LayerID: "layer-bulk", GenerationKind: "commit", TreeOID: "tree-bulk", CreatedAt: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range []ProducerCompleteness{{Producer: "source.snapshot", State: ProducerStateComplete}, {Producer: "graph.syntax", State: ProducerStateComplete}} {
			if err := handle.SetProducerState(row); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.PublishPayloadGeneration(ctx, id, 20); err != nil {
			t.Fatal(err)
		}
		engaged, err := store.BeginGenerationBulkLoad(id)
		if engaged || !errors.Is(err, ErrPayloadGenerationSealed) {
			t.Fatalf("got (%v,%v)", engaged, err)
		}
	})
	t.Run("outer owner", func(t *testing.T) {
		store, _ := openTempStore(t)
		if !store.BeginCoordinatedBulkLoad() {
			t.Fatal("cold window declined")
		}
		engaged, err := store.BeginGenerationBulkLoad(3)
		if engaged || err != nil {
			t.Fatalf("got (%v,%v)", engaged, err)
		}
		if !store.coordinatedBulkLoad || store.bulkConn == nil {
			t.Fatal("disturbed outer")
		}
		if err := store.EndCoordinatedBulkLoad(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGenerationBulkLoadLeavesBaseReadersAndIndexesAlone(t *testing.T) {
	store, _ := openTempStore(t)
	baseNodes, baseEdges := bulkFixture(256, 512)
	store.AddBatch(baseNodes, baseEdges)
	probe := baseNodes[7].ID
	before := indexNames(t, store.db)
	syncBefore := pragmaIntDB(t, store.writerDB, "synchronous")
	cacheBefore := pragmaIntDB(t, store.writerDB, "cache_size")
	autoBefore := pragmaIntDB(t, store.writerDB, "wal_autocheckpoint")
	engaged, err := store.BeginGenerationBulkLoad(4)
	if err != nil || !engaged {
		t.Fatalf("got (%v,%v)", engaged, err)
	}
	for _, idx := range bulkDroppableIndexes {
		if !before[idx.name] {
			t.Fatalf("missing %s", idx.name)
		}
	}
	during := indexNames(t, store.db)
	for _, idx := range bulkDroppableIndexes {
		if !during[idx.name] {
			t.Fatalf("dropped %s", idx.name)
		}
	}
	ctx := context.Background()
	if got, err := pragmaInt(ctx, store.bulkConn, "synchronous"); err != nil || got != syncBefore {
		t.Fatalf("sync=%d err=%v want%d", got, err, syncBefore)
	}
	if got, err := pragmaInt(ctx, store.bulkConn, "cache_size"); err != nil || got != bulkCacheSizeKiB {
		t.Fatalf("cache=%d err=%v", got, err)
	}
	if store.GetNode(probe) == nil {
		t.Fatal("base read failed")
	}
	payloadNodes, payloadEdges := bulkFixture(512, 1024)
	if err := store.AtGeneration(4).AddBatchChecked(payloadNodes, payloadEdges); err != nil {
		t.Fatal(err)
	}
	if store.GetNode(probe) == nil {
		t.Fatal("base read failed after write")
	}
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatal(err)
	}
	if store.bulkConn != nil || store.generationBulkLoad != 0 {
		t.Fatal("window stayed open")
	}
	if got := pragmaIntDB(t, store.writerDB, "synchronous"); got != syncBefore {
		t.Fatalf("sync=%d", got)
	}
	if got := pragmaIntDB(t, store.writerDB, "cache_size"); got != cacheBefore {
		t.Fatalf("cache=%d", got)
	}
	if got := pragmaIntDB(t, store.writerDB, "wal_autocheckpoint"); got != autoBefore {
		t.Fatalf("auto=%d", got)
	}
	after := indexNames(t, store.db)
	for name := range before {
		if !after[name] {
			t.Fatalf("lost %s", name)
		}
	}
	nodes, edges := generationRowCounts(t, store, 4)
	if nodes != 512 || edges == 0 {
		t.Fatalf("rows %d/%d", nodes, edges)
	}
	integrityOK(t, store.db)
}

func TestEndGenerationBulkLoadIsInertWithoutAWindow(t *testing.T) {
	store, _ := openTempStore(t)
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatal(err)
	}
	engaged, err := store.BeginGenerationBulkLoad(2)
	if err != nil || !engaged {
		t.Fatalf("got(%v,%v)", engaged, err)
	}
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatal(err)
	}
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationBulkLoadShapeReadsTheConnectionBack(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	if _, _, ok := store.GenerationBulkLoadShape(); ok {
		t.Fatal("shape active")
	}
	engaged, err := store.BeginGenerationBulkLoad(3)
	if err != nil || !engaged {
		t.Fatal(err)
	}
	defer func() { _ = store.EndGenerationBulkLoad() }()
	cacheSize, auto, ok := store.GenerationBulkLoadShape()
	if !ok || cacheSize != bulkCacheSizeKiB || auto != 0 {
		t.Fatalf("shape %d/%d/%v", cacheSize, auto, ok)
	}
	store.writeMu.Lock()
	conn := store.bulkConn
	if conn == nil {
		store.writeMu.Unlock()
		t.Fatal("no conn")
	}
	_, e1 := conn.ExecContext(ctx, "PRAGMA cache_size=-2048")
	_, e2 := conn.ExecContext(ctx, "PRAGMA wal_autocheckpoint=977")
	store.writeMu.Unlock()
	if e1 != nil || e2 != nil {
		t.Fatalf("%v/%v", e1, e2)
	}
	cacheSize, auto, ok = store.GenerationBulkLoadShape()
	if !ok || cacheSize != -2048 || auto != 977 {
		t.Fatalf("shape %d/%d/%v", cacheSize, auto, ok)
	}
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.GenerationBulkLoadShape(); ok {
		t.Fatal("shape active after end")
	}
}

func TestGenerationBulkLoadInMemoryIsNoOp(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	engaged, err := store.BeginGenerationBulkLoad(1)
	if engaged || err != nil {
		t.Fatalf("got(%v,%v)", engaged, err)
	}
}

func TestWALAutoCheckpointPagesIsOperatorTunable(t *testing.T) {
	if got := sqliteWALAutoCheckpointPages(); got != defaultSQLiteWALAutoCheckpointPages {
		t.Fatalf("default = %d, want %d", got, defaultSQLiteWALAutoCheckpointPages)
	}
	for _, bad := range []string{"not-a-number", "-4", "  "} {
		t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", bad)
		if got := sqliteWALAutoCheckpointPages(); got != defaultSQLiteWALAutoCheckpointPages {
			t.Fatalf("bad %q = %d, want default", bad, got)
		}
	}
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "128")
	if got := sqliteWALAutoCheckpointPages(); got != 128 {
		t.Fatalf("override = %d, want 128", got)
	}
	if dsn := sqliteWriterDSN("/tmp/x.sqlite"); !strings.Contains(dsn, "wal_autocheckpoint(0)") || strings.Contains(dsn, "wal_autocheckpoint(128)") {
		t.Fatalf("writer DSN did not keep SQLite commit hook disabled: %s", dsn)
	}
	store, path := openTempStore(t)
	if got := pragmaIntDB(t, store.writerDB, "wal_autocheckpoint"); got != 0 {
		t.Fatalf("writer wal_autocheckpoint = %d, want 0", got)
	}
	pageSize := int64(pragmaIntDB(t, store.db, "page_size"))
	walPath, threshold := sqliteWALPressureTarget(store.db, path, 128)
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatalf("resolve store directory: %v", err)
	}
	wantWALPath := filepath.Join(resolvedDir, filepath.Base(path)) + "-wal"
	if walPath != wantWALPath {
		t.Fatalf("pressure WAL path = %q, want resolved path %q", walPath, wantWALPath)
	}
	if want := int64(32) + 128*(pageSize+24); threshold != want {
		t.Fatalf("pressure threshold = %d, want %d", threshold, want)
	}
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "0")
	if got := sqliteWALAutoCheckpointPages(); got != 0 {
		t.Fatalf("explicit zero = %d, want 0", got)
	}
	if _, threshold := sqliteWALPressureTarget(store.db, path, 0); threshold != 0 {
		t.Fatalf("disabled pressure threshold = %d, want 0", threshold)
	}
}

func TestSQLiteWALFallbackPathSupportsFileURIs(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "plain.sqlite")
	absolute := filepath.Join(t.TempDir(), "space # question ?.sqlite")
	absoluteURI := sqliteDSN(absolute, "mode=rwc")
	localhostURI := strings.Replace(absoluteURI, "file://", "file://localhost", 1)
	cases := []struct{ name, input, want string }{{"plain", plain, plain}, {"absolute escaped", absoluteURI, absolute}, {"relative opaque", "file:relative%20store.sqlite?mode=rwc", filepath.FromSlash("relative store.sqlite")}, {"localhost", localhostURI, absolute}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sqliteWALFallbackPath(tc.input); got != tc.want {
				t.Fatalf("got%q want%q", got, tc.want)
			}
		})
	}
}

func TestWALCheckpointSchedulePressureAndRetryPolicy(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "store.sqlite-wal")
	if err := os.WriteFile(walPath, make([]byte, 256), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	t.Run("incomplete", func(t *testing.T) {
		s := newWALCheckpointSchedule(now, time.Hour, 64)
		calls := 0
		if retry := s.attempt(now, walPath, func() (bool, bool) { calls++; return false, true }); !retry {
			t.Fatal("no retry")
		}
		completed := now.Add(time.Second)
		if retry := s.attempt(completed, walPath, func() (bool, bool) { calls++; return true, false }); retry {
			t.Fatal("retry complete")
		}
		s.attempt(completed.Add(5*time.Second), walPath, func() (bool, bool) { calls++; return true, false })
		if calls != 2 {
			t.Fatalf("early calls%d", calls)
		}
		s.attempt(completed.Add(walPressureUnchangedRecheck), walPath, func() (bool, bool) { calls++; return true, false })
		if calls != 3 {
			t.Fatalf("aged calls%d", calls)
		}
	})
	t.Run("permanent", func(t *testing.T) {
		s := newWALCheckpointSchedule(now, time.Minute, 64)
		calls := 0
		s.attempt(now, walPath, func() (bool, bool) { calls++; return false, false })
		if err := os.WriteFile(walPath, make([]byte, 512), 0o600); err != nil {
			t.Fatal(err)
		}
		s.attempt(now.Add(5*time.Second), walPath, func() (bool, bool) { calls++; return true, false })
		if calls != 1 {
			t.Fatalf("early%d", calls)
		}
		s.attempt(now.Add(time.Minute), walPath, func() (bool, bool) { calls++; return true, false })
		if calls != 2 {
			t.Fatalf("late%d", calls)
		}
	})
	t.Run("periodic below line", func(t *testing.T) {
		s := newWALCheckpointSchedule(now, 10*time.Second, 1024)
		calls := 0
		if retry := s.attempt(now.Add(10*time.Second), walPath, func() (bool, bool) { calls++; return false, true }); !retry {
			t.Fatal("no retry")
		}
		s.attempt(now.Add(11*time.Second), walPath, func() (bool, bool) { calls++; return true, false })
		if calls != 2 {
			t.Fatalf("calls%d", calls)
		}
	})
}

func TestPassiveCheckpointRetriesWALPinnedByReader(t *testing.T) {
	store, path := openTempStore(t)
	base, _ := bulkFixture(128, 0)
	store.AddBatch(base, nil)
	if err := store.CheckpointWAL(); err != nil {
		t.Fatal(err)
	}
	release := holdAReadSnapshot(t, store)
	payload, _ := bulkFixture(2048, 0)
	if err := store.AtGeneration(1).AddBatchChecked(payload, nil); err != nil {
		t.Fatal(err)
	}
	if walFileBytes(t, path) == 0 {
		t.Fatal("no WAL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := store.writerDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, checkpointErr := checkpointWALOnceOn(ctx, conn, "PASSIVE")
	_ = conn.Close()
	if result.Busy != 0 || result.CheckpointedFrames >= result.WALFrames || !errors.Is(checkpointErr, errSQLiteCheckpointIncomplete) {
		t.Fatalf("result%+v err%v", result, checkpointErr)
	}
	if complete, retry := store.checkpointWALPassiveOutcome(); complete || !retry {
		t.Fatalf("pinned %v/%v", complete, retry)
	}
	release()
	if complete, retry := store.checkpointWALPassiveOutcome(); !complete || retry {
		t.Fatalf("released %v/%v", complete, retry)
	}
}

func TestCheckpointLoopStartupProbeCannotBlockClose(t *testing.T) {
	physical := filepath.Join(t.TempDir(), "startup space # question ?.sqlite")
	uri := sqliteDSN(physical, "mode=rwc")
	store, err := Open(uri)
	if err != nil {
		t.Fatal(err)
	}
	store.stopCheckpointLoop()
	ctx := context.Background()
	connections := make([]*sql.Conn, 0, sqliteMaxOpenConns)
	for range sqliteMaxOpenConns {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, conn := range connections {
				_ = conn.Close()
			}
		})
	}
	defer release()
	store.stopOnce = sync.Once{}
	store.stopCheckpoint = make(chan struct{})
	store.checkpointDone = make(chan struct{})
	waits := store.db.Stats().WaitCount
	go store.runCheckpointLoop(time.Hour)
	waitForCondition(t, "startup probe wait", func() bool { return store.db.Stats().WaitCount > waits })
	closed := make(chan error, 1)
	started := time.Now()
	go func() { closed <- store.Close() }()
	deadline := walPassiveCheckpointTimeout + walPassiveCheckpointTimeout/2
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed >= deadline {
			t.Fatalf("elapsed%v", elapsed)
		}
	case <-time.After(deadline):
		release()
		select {
		case <-closed:
		case <-time.After(2 * walPassiveCheckpointTimeout):
		}
		t.Fatalf("Close exceeded%v", deadline)
	}
}

func coldLoadOverAHeldSnapshot(t *testing.T, store *Store, path string, pageSize int64) (release func(), frames int) {
	t.Helper()
	release = holdAReadSnapshot(t, store)
	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("declined")
	}
	nodes, edges := bulkFixture(1200, 2400)
	for i := 0; i < len(nodes); i += 200 {
		end := min(i+200, len(nodes))
		if err := store.AddBatchChecked(nodes[i:end], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddBatchChecked(nil, edges); err != nil {
		t.Fatal(err)
	}
	if err := store.EndCoordinatedBulkLoad(); err != nil {
		t.Fatal(err)
	}
	return release, walFileFrames(t, path, pageSize)
}

func TestColdLoadFinalizeDrainsTheWALResidue(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "64")
	store, path := openTempStore(t)
	pageSize := pragmaIntDB(t, store.db, "page_size")
	release, frames := coldLoadOverAHeldSnapshot(t, store, path, pageSize)
	if frames <= 64 {
		t.Fatalf("frames%d", frames)
	}
	if store.walDrainRequests.Load() == 0 {
		t.Fatal("no request")
	}
	release()
	waitForCondition(t, "drain", func() bool { return store.walDrains.Load() > 0 && walFileFrames(t, path, pageSize) <= 64 })
}
func TestFinalizeBelowTheLineOwesNoDrain(t *testing.T) {
	store, path := openTempStore(t)
	pageSize := pragmaIntDB(t, store.db, "page_size")
	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("declined")
	}
	nodes, edges := bulkFixture(64, 64)
	if err := store.AddBatchChecked(nodes, edges); err != nil {
		t.Fatal(err)
	}
	if err := store.EndCoordinatedBulkLoad(); err != nil {
		t.Fatal(err)
	}
	if frames := walFileFrames(t, path, pageSize); frames > defaultSQLiteWALAutoCheckpointPages {
		t.Fatalf("frames%d", frames)
	}
	if got := store.walDrainRequests.Load(); got != 0 {
		t.Fatalf("requests%d", got)
	}
}

func TestScheduledWALDrainNeverBlocksAReader(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "64")
	store, path := openTempStore(t)
	pageSize := pragmaIntDB(t, store.db, "page_size")
	nodes, _ := bulkFixture(2048, 0)
	store.AddBatch(nodes, nil)
	probe := nodes[11].ID
	stop := make(chan struct{})
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		reads := 0
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if store.GetNode(probe) == nil {
				done <- fmt.Errorf("read failed after%d", reads)
				return
			}
			reads++
			if reads == 1 {
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	store.scheduleWALDrain("test")
	waitForCondition(t, "drain", func() bool { return store.walDrains.Load() > 0 })
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := walFileFrames(t, path, pageSize); got > 64 {
		t.Fatalf("frames%d", got)
	}
	if got := store.maintenancePasses.Load(); got != 0 {
		t.Fatalf("passes%d", got)
	}
}

func TestScheduledWALDrainDefersRatherThanWaitingForever(t *testing.T) {
	oldAttempts, oldDelay := walDrainAttempts, walDrainRetryDelay
	walDrainAttempts, walDrainRetryDelay = 2, time.Millisecond
	t.Cleanup(func() { walDrainAttempts, walDrainRetryDelay = oldAttempts, oldDelay })
	shortenMaintenanceBudget(t, 50*time.Millisecond)
	store, _ := openTempStore(t)
	nodes, _ := bulkFixture(64, 0)
	store.AddBatch(nodes, nil)
	if err := store.maintenanceGate.LockContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.scheduleWALDrain("test")
	waitForCondition(t, "defer", func() bool {
		store.maintenanceSched.Lock()
		defer store.maintenanceSched.Unlock()
		return !store.maintenanceDrainRunning && !store.maintenanceDrainOwed
	})
	store.maintenanceGate.Unlock()
	if store.walDrains.Load() != 0 {
		t.Fatal("drained")
	}
	if store.walDrainRequests.Load() != 1 {
		t.Fatal("requests")
	}
	if store.maintenanceDeferrals.Load() == 0 {
		t.Fatal("no deferral")
	}
}

func TestMaintenanceCheckpointAndBulkWindowsRaceSelection(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "64")
	store, _ := openTempStore(t)
	nodes, edges := bulkFixture(512, 512)
	store.AddBatch(nodes, edges)
	var wg sync.WaitGroup
	const rounds = 12
	wg.Add(4)
	go func() {
		defer wg.Done()
		for i := range rounds {
			payload, _ := bulkFixture(64, 0)
			store.AtGeneration(int64(100+i)).AddBatch(payload, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range rounds {
			id := int64(200 + i)
			engaged, err := store.BeginGenerationBulkLoad(id)
			if err != nil || !engaged {
				continue
			}
			payload, payloadEdges := bulkFixture(128, 128)
			_ = store.AtGeneration(id).AddBatchChecked(payload, payloadEdges)
			if err := store.EndGenerationBulkLoad(); err != nil {
				t.Errorf("end:%v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range rounds {
			if err := store.CheckpointWAL(); err != nil && !errors.Is(err, ErrMaintenanceBusy) && !errors.Is(err, errWALCheckpointDeferredBulk) && !errors.Is(err, errSQLiteCheckpointIncomplete) {
				t.Errorf("checkpoint:%v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range rounds {
			store.schedulePublishMaintenance()
			store.scheduleWALDrain("race")
		}
	}()
	wg.Wait()
	settleMaintenanceLane(t, store)
	if store.bulkConn != nil || store.generationBulkLoad != 0 {
		t.Fatal("window survived")
	}
	integrityOK(t, store.db)
	var stray int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE view_gen=0`).Scan(&stray); err != nil {
		t.Fatal(err)
	}
	if stray != len(nodes) {
		t.Fatalf("stray%d", stray)
	}
}

func TestBackgroundWALCheckpointRunsOutsideWriterGate(t *testing.T) {
	store, path := openTempStore(t)
	nodes, _ := bulkFixture(1024, 0)
	store.AddBatch(nodes, nil)
	if walFileBytes(t, path) == 0 {
		t.Fatal("fixture did not create WAL residue")
	}

	checkpointDB, err := sql.Open("sqlite", sqliteCheckpointDSN(path))
	if err != nil {
		t.Fatalf("open checkpoint pool: %v", err)
	}
	configureWriterPool(checkpointDB)
	defer func() { _ = checkpointDB.Close() }()
	if got := pragmaIntDB(t, checkpointDB, "wal_autocheckpoint"); got != 0 {
		t.Fatalf("background checkpoint wal_autocheckpoint = %d, want 0", got)
	}

	type outcome struct {
		complete bool
		retry    bool
	}
	store.writeMu.Lock()
	done := make(chan outcome, 1)
	go func() {
		complete, retry := store.checkpointWALPassiveBackgroundOutcome(checkpointDB)
		done <- outcome{complete: complete, retry: retry}
	}()
	select {
	case got := <-done:
		store.writeMu.Unlock()
		if !got.complete || got.retry {
			t.Fatalf("checkpoint while writer gate held = %+v, want complete", got)
		}
	case <-time.After(2 * time.Second):
		store.writeMu.Unlock()
		t.Fatal("background checkpoint waited for the application writer gate")
	}
}

func TestCheckpointLoopCleanupPrecedesDone(t *testing.T) {
	store := &Store{storeCore: &storeCore{
		stopCheckpoint: make(chan struct{}),
		checkpointDone: make(chan struct{}),
	}}
	entered := make(chan struct{})
	release := make(chan struct{})
	cleaned := make(chan struct{})
	var enteredOnce sync.Once
	var stopOnce sync.Once
	var releaseOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(store.stopCheckpoint) }) }
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		stop()
		unblock()
	})

	go store.runCheckpointLoopWithAttemptAndCleanup(
		time.Millisecond,
		time.Millisecond,
		time.Millisecond,
		func() bool {
			enteredOnce.Do(func() { close(entered) })
			<-release
			return false
		},
		func() { close(cleaned) },
	)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("checkpoint attempt did not start")
	}
	stop()
	unblock()
	select {
	case <-store.checkpointDone:
	case <-time.After(time.Second):
		t.Fatal("checkpoint loop did not stop")
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("checkpointDone closed before the dedicated pool cleanup")
	}
}

func TestBackgroundWALCheckpointFirstConnectHonorsShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "first-connect.sqlite")
	blocker, err := sql.Open("sqlite", sqliteWriterDSN(path))
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	configureWriterPool(blocker)
	ctx := context.Background()
	conn, err := blocker.Conn(ctx)
	if err != nil {
		_ = blocker.Close()
		t.Fatalf("blocker connection: %v", err)
	}
	var released sync.Once
	release := func() {
		released.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			_ = blocker.Close()
		})
	}
	t.Cleanup(release)
	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS checkpoint_probe (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("create blocker fixture: %v", err)
	}
	var lockingMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA locking_mode=EXCLUSIVE").Scan(&lockingMode); err != nil {
		t.Fatalf("set exclusive locking mode: %v", err)
	}
	if lockingMode != "exclusive" {
		t.Fatalf("locking mode = %q, want exclusive", lockingMode)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive blocker: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO checkpoint_probe DEFAULT VALUES"); err != nil {
		t.Fatalf("write blocker fixture: %v", err)
	}

	checkpointDB, err := sql.Open("sqlite", sqliteCheckpointDSN(path))
	if err != nil {
		t.Fatalf("open lazy checkpoint pool: %v", err)
	}
	configureWriterPool(checkpointDB)
	t.Cleanup(func() { _ = checkpointDB.Close() })
	store := &Store{storeCore: &storeCore{stopCheckpoint: make(chan struct{})}}
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(store.stopCheckpoint) }) }
	t.Cleanup(stop)

	done := make(chan struct{}, 1)
	go func() {
		store.checkpointWALPassiveBackgroundOutcome(checkpointDB)
		done <- struct{}{}
	}()
	select {
	case <-done:
		t.Skip("supported SQLite DSN did not block first connection under the exclusive fixture")
	case <-time.After(50 * time.Millisecond):
	}

	started := time.Now()
	stop()
	select {
	case <-done:
		if elapsed := time.Since(started); elapsed > walPassiveCheckpointTimeout {
			t.Fatalf("first-connect cancellation took %v, want at most %v", elapsed, walPassiveCheckpointTimeout)
		}
	case <-time.After(walPassiveCheckpointTimeout):
		release()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("shutdown did not cancel the checkpoint pool's first connection")
	}
}
