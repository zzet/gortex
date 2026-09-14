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
// Two defects are pinned here, and they meet at the same place — the shape of
// the bytes a generation payload costs.
//
//   - A committed generation's payload is written through the ordinary
//     incremental path, because the bulk fast path is gated on the whole STORE
//     being empty and generation 0 consumed it. BeginGenerationBulkLoad gives
//     that payload the two parts of the bulk shape a window over a LIVE store
//     may take — the enlarged page cache and the suspended automatic
//     checkpoint — and leaves alone the two the cold path may take only because
//     nothing can read it: the dense secondary indexes and durability.
//
//     Of those two, only the suspended automatic checkpoint is measurable at
//     unit scale, and that is what the comparison below pins. The page cache's
//     share is real in production (the payload replayed against the measured
//     store cost 841,300,936 logical writes at a 2 MB cache and 258,812,948 at
//     256 MiB) but does not reproduce on WAL-byte accounting at a fixture's
//     size: the same 120k-row payload wrote a byte-identical WAL at -64 KiB,
//     -2 MB, -32 MiB and -256 MiB of cache. The cache PRAGMA is therefore
//     asserted directly, on the pinned connection, rather than inferred from a
//     byte count a unit fixture cannot move.
//   - A cold load's finalize ends on ONE bounded PASSIVE checkpoint that
//     explicitly does not wait for readers, so what a daemon carries out of a
//     cold index is whatever the readers of the moment allowed: 3,636 frames in
//     one arm of the same workload and 15,573 in another. Whichever arm later
//     crosses the auto-checkpoint line pays the whole accumulated log inside
//     whatever phase happens to be running. The residue gate turns that lottery
//     into one scheduled, bounded TRUNCATE on the maintenance lane.

// walFileBytes is the -wal file's size. With automatic checkpoints suspended
// the WAL only grows, so between two drains its size is the log the store is
// carrying — the number the residue gate and the auto-checkpoint line are both
// about.
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

// walFileFrames converts that size into frames. The WAL header is 32 bytes and
// each frame is a 24-byte header plus one page.
func walFileFrames(t *testing.T, path string, pageSize int64) int {
	t.Helper()
	size := walFileBytes(t, path)
	if size <= 32 {
		return 0
	}
	return int((size - 32) / (pageSize + 24))
}

// holdAReadSnapshot opens a real read transaction and materialises it, which is
// what pins a WAL read mark. Starving the read POOL (holdTheOnlyReadConnection)
// does not: an idle connection holds no snapshot, and the WAL a checkpoint can
// copy is bounded by the oldest snapshot, not by the pool.
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
	release = func() {
		once.Do(func() {
			_ = tx.Rollback()
			_ = conn.Close()
		})
	}
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

type generationArm struct {
	payloadFrames int
	finalFrames   int
	payloadBytes  int64
	nodeRows      int64
	edgeRows      int64
}

// writeGenerationPayload runs one arm of the write-shape comparison: the same
// payload into the same empty generation of an equally seeded store, once
// through the ordinary incremental path and once inside a generation bulk
// window. It reports the WAL the payload itself left (measured before either
// arm finalizes anything) and the WAL the arm ends on.
func writeGenerationPayload(t *testing.T, bulk bool, line, nNodes, nEdges int) generationArm {
	t.Helper()
	const generationID = int64(1)
	path := filepath.Join(t.TempDir(), "generation.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	pageSize := pragmaIntDB(t, store.db, "page_size")

	// Generation 0 has to exist, or the cold fast path would engage on the
	// payload below and the two arms would not be the same measurement.
	baseNodes, baseEdges := bulkFixture(512, 1024)
	store.AddBatch(baseNodes, baseEdges)
	if err := store.CheckpointWAL(); err != nil {
		t.Fatalf("drain the seed WAL: %v", err)
	}
	if got := walFileBytes(t, path); got != 0 {
		t.Fatalf("the arm starts with a %d-byte WAL, so its growth is not its own", got)
	}

	if bulk {
		engaged, err := store.BeginGenerationBulkLoad(generationID)
		if err != nil {
			t.Fatalf("BeginGenerationBulkLoad: %v", err)
		}
		if !engaged {
			t.Fatal("BeginGenerationBulkLoad declined an empty generation on a disk store")
		}
	}

	handle := store.AtGeneration(generationID)
	nodes, edges := bulkFixture(nNodes, nEdges)
	const nodeChunk, edgeChunk = 1000, 2000
	for i := 0; i < len(nodes); i += nodeChunk {
		nodeEnd := min(i+nodeChunk, len(nodes))
		edgeStart := min(i/nodeChunk*edgeChunk, len(edges))
		edgeEnd := min(edgeStart+edgeChunk, len(edges))
		if err := handle.AddBatchChecked(nodes[i:nodeEnd], edges[edgeStart:edgeEnd]); err != nil {
			t.Fatalf("AddBatchChecked: %v", err)
		}
	}

	arm := generationArm{
		payloadFrames: walFileFrames(t, path, pageSize),
		payloadBytes:  walFileBytes(t, path),
	}
	if bulk {
		if err := store.EndGenerationBulkLoad(); err != nil {
			t.Fatalf("EndGenerationBulkLoad: %v", err)
		}
		waitForCondition(t, "the window's scheduled drain to take the WAL under the line", func() bool {
			return walFileFrames(t, path, pageSize) <= line
		})
	}
	arm.finalFrames = walFileFrames(t, path, pageSize)
	arm.nodeRows, arm.edgeRows = generationRowCounts(t, store, generationID)
	return arm
}

// A generation payload written inside the window pays no automatic checkpoint
// at all, and pays one bounded drain at the end instead.
//
// This is the half of the bulk shape that a window over a LIVE store can take,
// and it is what the incremental path cannot do: with automatic checkpoints at
// the production line, a payload larger than the line is interrupted by a full
// log drain every time it crosses one — the same db pages copied out again and
// again, mid-payload, charged to whoever happened to be writing. Inside the
// window the log accumulates once and is drained once, by the lane, after the
// payload is done.
//
// Revert-red, both directions: drop `PRAGMA wal_autocheckpoint = 0` from
// BeginGenerationBulkLoad and the window's payload is drained mid-flight like
// the incremental arm's; drop the residue gate from EndGenerationBulkLoad and
// the accumulated log is never paid off at all.
func TestGenerationBulkLoadDefersTheAutomaticDrainToOneAtItsEnd(t *testing.T) {
	const line = 200
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", strconv.Itoa(line))

	const nodes, edges = 6000, 12000
	incremental := writeGenerationPayload(t, false, line, nodes, edges)
	bulked := writeGenerationPayload(t, true, line, nodes, edges)

	t.Logf("generation payload WAL frames: incremental=%d (%d B) bulk=%d (%d B); final frames incremental=%d bulk=%d",
		incremental.payloadFrames, incremental.payloadBytes,
		bulked.payloadFrames, bulked.payloadBytes,
		incremental.finalFrames, bulked.finalFrames)

	if incremental.nodeRows != nodes || incremental.edgeRows == 0 {
		t.Fatalf("incremental arm: generation 1 holds %d nodes / %d edges, want %d nodes and some edges", incremental.nodeRows, incremental.edgeRows, nodes)
	}
	if bulked.nodeRows != incremental.nodeRows || bulked.edgeRows != incremental.edgeRows {
		t.Fatalf("the two shapes produced different payloads: bulk %d/%d, incremental %d/%d",
			bulked.nodeRows, bulked.edgeRows, incremental.nodeRows, incremental.edgeRows)
	}
	// The incremental arm is SQLite's own behaviour and the control: a payload
	// of thousands of frames never gets to hold more than a few times the line,
	// because it is drained every time it crosses one.
	if incremental.payloadFrames > 5*line {
		t.Fatalf("fixture problem: the incremental arm held %d frames, so the %d-page line was not being enforced inside the payload", incremental.payloadFrames, line)
	}
	if bulked.payloadFrames <= 5*line {
		t.Fatalf("the window held only %d frames against the incremental path's %d: its payload was drained mid-flight, which is the cost it exists to defer", bulked.payloadFrames, incremental.payloadFrames)
	}
	if bulked.finalFrames > line {
		t.Fatalf("the window deferred its drain and then never paid it: %d frames left, line %d", bulked.finalFrames, line)
	}
}

// The window's preconditions are refusals, not silent no-ops: a caller that
// opened one over a populated or sealed generation would be writing a second
// copy into rows somebody may already be reading.
func TestGenerationBulkLoadRefusesWhatItCannotProveEmpty(t *testing.T) {
	t.Run("generation zero", func(t *testing.T) {
		store, _ := openTempStore(t)
		engaged, err := store.BeginGenerationBulkLoad(baseViewGeneration)
		if engaged || !errors.Is(err, ErrCatalogInvalidValue) {
			t.Fatalf("BeginGenerationBulkLoad(0) = (%v, %v), want a refusal: generation 0 is the cold path's", engaged, err)
		}
	})

	t.Run("a generation that already holds rows", func(t *testing.T) {
		store, _ := openTempStore(t)
		nodes, edges := bulkFixture(64, 64)
		store.AddBatch(nodes, edges)
		store.AtGeneration(7).AddBatch(nodes, edges)

		engaged, err := store.BeginGenerationBulkLoad(7)
		if engaged || !errors.Is(err, ErrGenerationBulkLoadPopulated) {
			t.Fatalf("BeginGenerationBulkLoad over a populated generation = (%v, %v), want ErrGenerationBulkLoadPopulated", engaged, err)
		}
		if store.bulkConn != nil {
			t.Fatal("a refused window left the writer connection pinned")
		}
		// An empty sibling generation is still admitted, so the refusal is
		// scoped to the generation and not to the store having any rows at all.
		engaged, err = store.BeginGenerationBulkLoad(8)
		if err != nil || !engaged {
			t.Fatalf("BeginGenerationBulkLoad over an empty sibling = (%v, %v), want it engaged", engaged, err)
		}
		if err := store.EndGenerationBulkLoad(); err != nil {
			t.Fatalf("EndGenerationBulkLoad: %v", err)
		}
	})

	t.Run("a published generation", func(t *testing.T) {
		store := openCatalogStore(t)
		ctx := context.Background()
		generationID, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
			OwnerKind: "ref_view", GraphID: "graph-bulk", LayerID: "layer-bulk",
			GenerationKind: "commit", TreeOID: "tree-bulk", CreatedAt: 10,
		})
		if err != nil {
			t.Fatalf("BeginPayloadGeneration: %v", err)
		}
		for _, row := range []ProducerCompleteness{
			{Producer: "source.snapshot", State: ProducerStateComplete},
			{Producer: "graph.syntax", State: ProducerStateComplete},
		} {
			if err := handle.SetProducerState(row); err != nil {
				t.Fatalf("SetProducerState %s: %v", row.Producer, err)
			}
		}
		if err := store.PublishPayloadGeneration(ctx, generationID, 20); err != nil {
			t.Fatalf("PublishPayloadGeneration: %v", err)
		}

		engaged, err := store.BeginGenerationBulkLoad(generationID)
		if engaged || !errors.Is(err, ErrPayloadGenerationSealed) {
			t.Fatalf("BeginGenerationBulkLoad over a published generation = (%v, %v), want ErrPayloadGenerationSealed", engaged, err)
		}
	})

	t.Run("while another bulk window owns the connection", func(t *testing.T) {
		store, _ := openTempStore(t)
		if !store.BeginCoordinatedBulkLoad() {
			t.Fatal("the coordinated cold window did not engage on a fresh store")
		}
		engaged, err := store.BeginGenerationBulkLoad(3)
		if engaged || err != nil {
			t.Fatalf("BeginGenerationBulkLoad inside a cold window = (%v, %v), want it declined without an error", engaged, err)
		}
		if !store.coordinatedBulkLoad || store.bulkConn == nil {
			t.Fatal("the declined window disturbed the cold load it declined to join")
		}
		if err := store.EndCoordinatedBulkLoad(); err != nil {
			t.Fatalf("EndCoordinatedBulkLoad: %v", err)
		}
	})
}

// What the window must NOT do is what the cold path does: generation 0's
// readers are live underneath it, so the dense secondary indexes stay in the
// schema, durability stays where it was, and reads keep being served for the
// whole window.
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
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v), want it engaged", engaged, err)
	}

	for _, idx := range bulkDroppableIndexes {
		if !before[idx.name] {
			t.Fatalf("fixture problem: %s was not in the schema before the window", idx.name)
		}
	}
	during := indexNames(t, store.db)
	for _, idx := range bulkDroppableIndexes {
		if !during[idx.name] {
			t.Fatalf("the generation window dropped %s, blinding every generation-0 reader for its length", idx.name)
		}
	}
	// Probed on the pinned connection itself: the pooled writer has exactly
	// one physical connection and the window holds it, so asking the pool
	// would deadlock rather than answer.
	ctx := context.Background()
	if got, err := pragmaInt(ctx, store.bulkConn, "synchronous"); err != nil || got != syncBefore {
		t.Fatalf("the pinned connection runs at synchronous=%d (err %v), want the store's %d: a window over a populated store may not trade durability", got, err, syncBefore)
	}
	if got, err := pragmaInt(ctx, store.bulkConn, "cache_size"); err != nil || got != bulkCacheSizeKiB {
		t.Fatalf("the pinned connection runs at cache_size=%d (err %v), want %d: the cache is the whole win", got, err, bulkCacheSizeKiB)
	}

	// Generation 0 keeps being served while the window writes generation 4.
	if node := store.GetNode(probe); node == nil {
		t.Fatalf("a generation-0 read returned nothing while the window was open: %s", probe)
	}
	payloadNodes, payloadEdges := bulkFixture(2048, 4096)
	if err := store.AtGeneration(4).AddBatchChecked(payloadNodes, payloadEdges); err != nil {
		t.Fatalf("AddBatchChecked inside the window: %v", err)
	}
	if node := store.GetNode(probe); node == nil {
		t.Fatalf("a generation-0 read returned nothing after the window wrote its payload: %s", probe)
	}

	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatalf("EndGenerationBulkLoad: %v", err)
	}
	if store.bulkConn != nil || store.generationBulkLoad != 0 {
		t.Fatal("the window stayed open past its end")
	}
	if got := pragmaIntDB(t, store.writerDB, "synchronous"); got != syncBefore {
		t.Fatalf("synchronous = %d after the window, want %d", got, syncBefore)
	}
	if got := pragmaIntDB(t, store.writerDB, "cache_size"); got != cacheBefore {
		t.Fatalf("cache_size = %d after the window, want %d", got, cacheBefore)
	}
	if got := pragmaIntDB(t, store.writerDB, "wal_autocheckpoint"); got != autoBefore {
		t.Fatalf("wal_autocheckpoint = %d after the window, want %d", got, autoBefore)
	}
	after := indexNames(t, store.db)
	for name := range before {
		if !after[name] {
			t.Fatalf("index %s did not survive the window", name)
		}
	}
	nodes, edges := generationRowCounts(t, store, 4)
	if nodes != 2048 || edges == 0 {
		t.Fatalf("generation 4 holds %d nodes / %d edges after the window", nodes, edges)
	}
	if node := store.GetNode(probe); node == nil {
		t.Fatalf("a generation-0 read returned nothing after the window closed: %s", probe)
	}
	integrityOK(t, store.db)
}

// The window is idempotent at its end and inert when it never opened, so a
// deferred EndGenerationBulkLoad is always safe.
func TestEndGenerationBulkLoadIsInertWithoutAWindow(t *testing.T) {
	store, _ := openTempStore(t)
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatalf("EndGenerationBulkLoad with no window: %v", err)
	}
	engaged, err := store.BeginGenerationBulkLoad(2)
	if err != nil || !engaged {
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v), want it engaged", engaged, err)
	}
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatalf("EndGenerationBulkLoad: %v", err)
	}
	if err := store.EndGenerationBulkLoad(); err != nil {
		t.Fatalf("a second EndGenerationBulkLoad: %v", err)
	}
}

// An in-memory store has no WAL and no on-disk B-tree pressure to spare, so the
// window declines rather than pinning a connection it cannot help.
func TestGenerationBulkLoadInMemoryIsNoOp(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open in-memory: %v", err)
	}
	defer func() { _ = store.Close() }()
	engaged, err := store.BeginGenerationBulkLoad(1)
	if engaged || err != nil {
		t.Fatalf("BeginGenerationBulkLoad on an in-memory store = (%v, %v), want a quiet decline", engaged, err)
	}
}

// The auto-checkpoint line is one number in two places — the writer DSN's
// PRAGMA and the residue gate's threshold — and an override has to move both,
// or a measurement that toggles the class would only toggle half of it.
func TestWALAutoCheckpointPagesIsOperatorTunable(t *testing.T) {
	if got := sqliteWALAutoCheckpointPages(); got != defaultSQLiteWALAutoCheckpointPages {
		t.Fatalf("unset = %d, want the %d default", got, defaultSQLiteWALAutoCheckpointPages)
	}
	for _, bad := range []string{"not-a-number", "-4", "  "} {
		t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", bad)
		if got := sqliteWALAutoCheckpointPages(); got != defaultSQLiteWALAutoCheckpointPages {
			t.Fatalf("%q = %d, want the default: bad input must fail open", bad, got)
		}
	}

	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "128")
	if got := sqliteWALAutoCheckpointPages(); got != 128 {
		t.Fatalf("override = %d, want 128", got)
	}
	if dsn := sqliteWriterDSN("/tmp/x.sqlite"); !strings.Contains(dsn, "wal_autocheckpoint(128)") {
		t.Fatalf("the writer DSN did not carry the override: %s", dsn)
	}
	store, _ := openTempStore(t)
	if got := pragmaIntDB(t, store.writerDB, "wal_autocheckpoint"); got != 128 {
		t.Fatalf("a store opened under the override runs at wal_autocheckpoint=%d, want 128", got)
	}

	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "0")
	if got := sqliteWALAutoCheckpointPages(); got != 0 {
		t.Fatalf("explicit 0 = %d: disabling automatic checkpoints is a legitimate mode", got)
	}
}

// coldLoadOverAHeldSnapshot runs a cold coordinated load whose finalize cannot
// drain, because a read snapshot older than every frame is held for the whole
// window. That is the production shape the residue measurement came from: the
// finalize's PASSIVE explicitly does not wait for readers, and a queryable
// daemon has them.
func coldLoadOverAHeldSnapshot(t *testing.T, store *Store, path string, pageSize int64) (release func(), frames int) {
	t.Helper()
	// The snapshot is taken before the load, which is what makes it older than
	// every frame the load writes: a reader on the WAL's first read mark holds
	// the lock backfill needs, so the finalize's PASSIVE copies nothing.
	release = holdAReadSnapshot(t, store)

	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("the coordinated cold window did not engage on a fresh store")
	}
	nodes, edges := bulkFixture(6000, 12000)
	const chunk = 1000
	for i := 0; i < len(nodes); i += chunk {
		end := min(i+chunk, len(nodes))
		if err := store.AddBatchChecked(nodes[i:end], nil); err != nil {
			t.Fatalf("AddBatchChecked: %v", err)
		}
	}
	if err := store.AddBatchChecked(nil, edges); err != nil {
		t.Fatalf("AddBatchChecked(edges): %v", err)
	}
	if err := store.EndCoordinatedBulkLoad(); err != nil {
		t.Fatalf("EndCoordinatedBulkLoad: %v", err)
	}
	return release, walFileFrames(t, path, pageSize)
}

// After a cold load the daemon must not be carrying a log above the line its
// own automatic checkpoint fires at. The finalize measures what its bounded
// PASSIVE left behind and owes the maintenance lane one bounded TRUNCATE; the
// lane runs it once the readers that made the PASSIVE incomplete have left.
//
// Revert-red: drop scheduleWALDrainAboveLine from the finalize and the frames
// stay where the finalize left them — nothing else in an idle store drains
// them, which is exactly the residue the next phase inherits.
func TestColdLoadFinalizeDrainsTheWALResidue(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "64")
	store, path := openTempStore(t)
	pageSize := pragmaIntDB(t, store.db, "page_size")

	release, frames := coldLoadOverAHeldSnapshot(t, store, path, pageSize)
	if frames <= 64 {
		t.Fatalf("the cold load left %d frames, which is not above the %d-page line this case is about", frames, 64)
	}
	if got := store.walDrainRequests.Load(); got == 0 {
		t.Fatal("the finalize measured a residue above the line and owed no drain")
	}

	// The readers leave; the drain the finalize scheduled takes the WAL back
	// under the line without anybody writing again.
	release()
	// Both halves in one wait: the file is truncated inside the drain's PRAGMA
	// and the counter is incremented after the job returns, so polling the two
	// separately would race the gap between them rather than the drain.
	waitForCondition(t, "the scheduled drain to complete and take the WAL under the auto-checkpoint line", func() bool {
		return store.walDrains.Load() > 0 && walFileFrames(t, path, pageSize) <= 64
	})
	if got := walFileFrames(t, path, pageSize); got > 64 {
		t.Fatalf("the drain completed and left %d frames, above the %d-page line", got, 64)
	}
}

// Below the line there is nothing to fix, and the gate says so: a finalize that
// ends on a small log owes no follow-up drain at all. A gate that fired on every
// finalize would put a whole-file TRUNCATE — and the pre-emption of the
// statistics pass that comes with it — behind every cold load, however small.
func TestFinalizeBelowTheLineOwesNoDrain(t *testing.T) {
	store, path := openTempStore(t)
	pageSize := pragmaIntDB(t, store.db, "page_size")
	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("the coordinated cold window did not engage on a fresh store")
	}
	nodes, edges := bulkFixture(64, 64)
	if err := store.AddBatchChecked(nodes, edges); err != nil {
		t.Fatalf("AddBatchChecked: %v", err)
	}
	if err := store.EndCoordinatedBulkLoad(); err != nil {
		t.Fatalf("EndCoordinatedBulkLoad: %v", err)
	}
	if frames := walFileFrames(t, path, pageSize); frames > defaultSQLiteWALAutoCheckpointPages {
		t.Fatalf("fixture problem: a 64-row cold load left %d frames, which is above the line", frames)
	}
	if got := store.walDrainRequests.Load(); got != 0 {
		t.Fatalf("a finalize below the auto-checkpoint line owed %d drains, want none", got)
	}
}

// The drain is scheduled rather than run inline precisely so it costs a reader
// nothing: it holds the write gate and the lane token, never the read pool.
func TestScheduledWALDrainNeverBlocksAReader(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "64")
	store, path := openTempStore(t)
	pageSize := pragmaIntDB(t, store.db, "page_size")
	nodes, _ := bulkFixture(2048, 0)
	store.AddBatch(nodes, nil)
	probe := nodes[11].ID

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		reads := 0
		for {
			select {
			case <-stop:
				if reads == 0 {
					done <- errors.New("the reader never ran")
					return
				}
				done <- nil
				return
			default:
			}
			if node := store.GetNode(probe); node == nil {
				done <- fmt.Errorf("a read was refused during the drain after %d reads", reads)
				return
			}
			reads++
		}
	}()

	store.scheduleWALDrain("test")
	waitForCondition(t, "the scheduled drain to complete", func() bool {
		return store.walDrains.Load() > 0
	})
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := walFileFrames(t, path, pageSize); got > 64 {
		t.Fatalf("the drain completed but left %d frames", got)
	}
	// The worker parks on one slot that two different requests post to, so a
	// wakeup carrying only a drain must not also spend an ANALYZE nobody asked
	// for.
	if got := store.maintenancePasses.Load(); got != 0 {
		t.Fatalf("a drain-only wakeup started %d planner-statistics passes", got)
	}
}

// A drain that cannot get past a reader inside its bounded attempts is a
// deferral, not a loss: the store is untouched, the counters record the
// difference between what was requested and what ran, and the next finalize
// asks again.
func TestScheduledWALDrainDefersRatherThanWaitingForever(t *testing.T) {
	oldAttempts, oldDelay := walDrainAttempts, walDrainRetryDelay
	walDrainAttempts, walDrainRetryDelay = 2, time.Millisecond
	t.Cleanup(func() { walDrainAttempts, walDrainRetryDelay = oldAttempts, oldDelay })
	shortenMaintenanceBudget(t, 50*time.Millisecond)

	store, _ := openTempStore(t)
	nodes, _ := bulkFixture(64, 0)
	store.AddBatch(nodes, nil)

	// Hold the lane against the drain, so every attempt it makes is refused
	// inside a bounded budget instead of parking on the token.
	if err := store.maintenanceGate.LockContext(context.Background()); err != nil {
		t.Fatalf("hold the lane: %v", err)
	}
	store.scheduleWALDrain("test")
	waitForCondition(t, "the drain to give up its attempts", func() bool {
		store.maintenanceSched.Lock()
		defer store.maintenanceSched.Unlock()
		return !store.maintenanceDrainRunning && !store.maintenanceDrainOwed
	})
	store.maintenanceGate.Unlock()

	if got := store.walDrains.Load(); got != 0 {
		t.Fatalf("a drain that never got the lane counted %d completions", got)
	}
	if got := store.walDrainRequests.Load(); got != 1 {
		t.Fatalf("walDrainRequests = %d, want the one request that deferred", got)
	}
	if got := store.maintenanceDeferrals.Load(); got == 0 {
		t.Fatal("a drain that could not enter the lane was not counted as a deferral")
	}
}

// The lane's three occupants — the statistics pass, a TRUNCATE checkpoint and a
// bulk window — are selected against each other by one token and one write
// gate. Run them together under -race: the properties being pinned are that
// nothing deadlocks, no state is torn, and every window that opened closed.
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
			store.AtGeneration(int64(100 + i)).AddBatch(payload, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range rounds {
			generationID := int64(200 + i)
			engaged, err := store.BeginGenerationBulkLoad(generationID)
			if err != nil || !engaged {
				continue
			}
			payload, payloadEdges := bulkFixture(128, 128)
			_ = store.AtGeneration(generationID).AddBatchChecked(payload, payloadEdges)
			if err := store.EndGenerationBulkLoad(); err != nil {
				t.Errorf("EndGenerationBulkLoad: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range rounds {
			if err := store.CheckpointWAL(); err != nil && !errors.Is(err, ErrMaintenanceBusy) &&
				!errors.Is(err, errWALCheckpointDeferredBulk) && !errors.Is(err, errSQLiteCheckpointIncomplete) {
				t.Errorf("CheckpointWAL: %v", err)
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
		t.Fatal("a bulk window survived the run")
	}
	integrityOK(t, store.db)
	var stray int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE view_gen = 0`).Scan(&stray); err != nil {
		t.Fatalf("count base nodes: %v", err)
	}
	if stray != len(nodes) {
		t.Fatalf("generation 0 holds %d nodes, want the %d it started with: a generation window wrote through the base", stray, len(nodes))
	}
}
