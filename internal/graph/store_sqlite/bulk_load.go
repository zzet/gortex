package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion: *Store satisfies graph.BulkLoader.
var _ graph.BulkLoader = (*Store)(nil)

// bulkDroppableIndex is one secondary index the bulk-load fast path drops
// before a first/empty cold index and rebuilds afterward.
type bulkDroppableIndex struct {
	name string
	ddl  string
}

// bulkFinalizeEvent decomposes the formerly opaque cold finalization timer.
// The observer is test-only; production telemetry is emitted to the daemon's
// captured stderr with stable stage/name fields.
type bulkFinalizeEvent struct {
	Stage              string
	Name               string
	Elapsed            time.Duration
	NodeRows           int64
	EdgeRows           int64
	Busy               int
	WALFrames          int
	CheckpointedFrames int
	Err                error
}

func (s *Store) emitBulkFinalizeEvent(event bulkFinalizeEvent) {
	if s.bulkFinalizeObserver != nil {
		s.bulkFinalizeObserver(event)
	}
	if event.Err != nil {
		log.Printf("store_sqlite: bulk finalize stage=%s name=%s elapsed=%s nodes=%d edges=%d error=%q", event.Stage, event.Name, event.Elapsed, event.NodeRows, event.EdgeRows, event.Err)
		return
	}
	if event.Stage == "checkpoint" || event.Stage == "checkpoint_passive" {
		log.Printf("store_sqlite: bulk finalize stage=%s name=%s elapsed=%s busy=%d wal_frames=%d checkpointed_frames=%d", event.Stage, event.Name, event.Elapsed, event.Busy, event.WALFrames, event.CheckpointedFrames)
		return
	}
	if event.Stage == "index_seal" {
		log.Printf("store_sqlite: bulk finalize stage=%s reason=%s elapsed=%s nodes=%d edges=%d", event.Stage, event.Name, event.Elapsed, event.NodeRows, event.EdgeRows)
		return
	}
	log.Printf("store_sqlite: bulk finalize stage=%s name=%s elapsed=%s", event.Stage, event.Name, event.Elapsed)
}

// FTS5's merge command writes approximately N pages and returns. Unlike
// optimize, its work is bounded independently of corpus size. Correctness does
// not depend on either command because every FTS row is already transactional.
const coldFTSMergePages = 64

// bulkDroppableIndexes is the single source of truth for the dense secondary
// indexes whose per-row maintenance is worth deferring for the bounded head of
// a proven cold load. createGraphCoreIndexes creates them, BeginBulkLoad drops
// them by name, and the first deterministic seal recreates them from the exact
// same DDL.
//
// None of these keys names view_gen. Only the two identity keys — the nodes
// primary key and the edges UNIQUE constraint — are taken per payload view
// generation, because only they decide whether a write collides with an
// existing row. These secondary keys serve generation-blind reads, and putting
// view_gen in front of them would leave every such read unable to seek any
// prefix. A WITHOUT ROWID secondary index entry carries the primary key, so
// each nodes entry ends in (id, view_gen) regardless — which is why the
// ID-projecting reads below stay index-only and their ORDER BY id stays free.
//
// These are exactly the standalone, NON-UNIQUE CREATE INDEX statements over
// the large nodes / edges tables. Maintaining them per-row across a
// multi-hundred-thousand-row cold load is pure overhead when the rows land
// once, so they are dropped up front and rebuilt in one pass at the end.
//
// Deliberately excluded:
//   - nodes_by_qual: resolver lookups use INDEXED BY and must fail closed
//     rather than scan the full nodes table. Keeping the compact partial index
//     live preserves that contract during every bulk-load phase.
//   - the edges UNIQUE(from_id, …, view_gen) table constraint and every WITHOUT ROWID
//     primary-key index: not standalone indexes; they cannot be dropped while
//     the table/constraint exists.
//   - edges_external (partial): a tiny index over external-call terminals,
//     created from a shared predicate const; not worth dropping.
//
// Dropping/recreating these is a runtime operation on identical DDL — it is
// NOT a schema change, so it does not touch the persisted schema version.
const (
	edgesByFromLineIndexDDL     = `CREATE INDEX IF NOT EXISTS edges_by_from_line ON edges(from_id, line)`
	edgesByFromLineKindIndexDDL = `CREATE INDEX IF NOT EXISTS edges_by_from_line_kind ON edges(from_id, line, kind)`
)

var bulkDroppableIndexes = []bulkDroppableIndex{
	{"nodes_by_name", `CREATE INDEX IF NOT EXISTS nodes_by_name ON nodes(name)`},
	{"nodes_by_kind", `CREATE INDEX IF NOT EXISTS nodes_by_kind ON nodes(kind)`},
	{"nodes_by_file", `CREATE INDEX IF NOT EXISTS nodes_by_file ON nodes(file_path)`},
	{"nodes_by_repo", `CREATE INDEX IF NOT EXISTS nodes_by_repo ON nodes(repo_prefix) WHERE repo_prefix <> ''`},
	// Resolver warmup selects definitions by exact repository, compatible
	// language family, and a bounded page of names. Keep the key minimal: kind
	// is not a query predicate and WITHOUT ROWID secondary indexes already
	// carry the primary-key id. The partial predicate excludes nameless nodes.
	{"nodes_by_repo_language_name", `CREATE INDEX IF NOT EXISTS nodes_by_repo_language_name ON nodes(repo_prefix, language, name) WHERE name <> ''`},
	{"edges_by_from", `CREATE INDEX IF NOT EXISTS edges_by_from ON edges(from_id, kind)`},
	// Site-shaped candidate probes (guard rehydration, resolve-job liveness,
	// edge identity lookups) constrain (from_id, line). Without a line-bearing
	// index the planner satisfies them through the covering WITHOUT-ROWID
	// primary key probed on from_id alone, re-reading the caller's whole
	// out-edge row set per site — hub callers (11k+ out-edges) turn a µs seek
	// into tens of milliseconds, and the cross-package guard alone paid ~980s
	// of a 28-repo cold index that way. With the index the full candidate
	// path measured ~30 → ~116k sites/s on a production store copy.
	// Preserve this exact two-column shape: ordered outgoing projections rely
	// on the rowid/primary-key suffix after line and otherwise need a temp sort.
	{"edges_by_from_line", edgesByFromLineIndexDDL},
	// Bounded exact-site adjacency also constrains kind before LIMIT. Keep that
	// predicate-shaped index separate so it cannot perturb the legacy ordered
	// outgoing plan; both participate in the same bulk drop/rebuild lifecycle.
	{"edges_by_from_line_kind", edgesByFromLineKindIndexDDL},
	{"edges_by_to", `CREATE INDEX IF NOT EXISTS edges_by_to ON edges(to_id, kind)`},
	{"edges_by_kind", `CREATE INDEX IF NOT EXISTS edges_by_kind ON edges(kind)`},
	// Exact changed-file frontiers (watcher and partial indexing) must not
	// scan every edge merely to find source sites owned by one file.
	{"edges_by_file", `CREATE INDEX IF NOT EXISTS edges_by_file ON edges(file_path, kind)`},
}

// nodes_by_generation / edges_by_generation serve sparse-generation
// enumeration and garbage collection: "which rows belong to generation g" and
// "drop every row of generation g". Every other read binds its generation as a
// residual conjunct on an existing access path, so these two are the only keys
// in the package that lead with view_gen.
//
// The WHERE view_gen > 0 predicate is what makes them affordable. A store that
// has only ever been plainly indexed holds nothing but generation-0 rows, so
// both indexes stay empty and cost nothing to maintain; a sparse derived
// generation gets a full leading seek. A reader must restate the predicate
// literally — SQLite cannot prove a bound parameter is greater than zero — so
// an ordinary `view_gen = ?` read keeps the plan it already had.
const (
	nodesByGenerationIndexName = "nodes_by_generation"
	edgesByGenerationIndexName = "edges_by_generation"

	nodesByGenerationIndexDDL = `CREATE INDEX IF NOT EXISTS nodes_by_generation ON nodes(view_gen, id) WHERE view_gen > 0`
	edgesByGenerationIndexDDL = `CREATE INDEX IF NOT EXISTS edges_by_generation ON edges(view_gen, id) WHERE view_gen > 0`
)

// bulkAlwaysLiveIndexes preserve bounded maintenance and resolver/repository
// projections as soon as the first repository publishes. Most are sparse
// partial indexes; the dense repo-leading index also stays live so per-repo
// emptiness checks and exact recounts never scan the growing cold corpus.
var bulkAlwaysLiveIndexes = []bulkDroppableIndex{
	// Repo-first (repo_prefix, kind) probes for the repository projections:
	// the flat kind index invites whole-kind-range scans that a repo filter
	// then discards — measured on this workspace at 4.67s vs 0.82s (common
	// kind) and 6.21s vs 0.02s (small repo) against repo-first plans.
	// Deliberately NOT partial: the projections probe repo_prefix through a
	// json_each CTE join, and SQLite cannot prove such a join implies
	// repo_prefix <> '', so a partial index is structurally unusable there —
	// which is precisely why the partial nodes_by_repo never served these
	// queries and they fell back to kind-range scans. WITHOUT ROWID keys
	// make each entry (repo_prefix, kind, id, view_gen), so ID projections
	// are index-only.
	{"nodes_by_repo_kind", `CREATE INDEX IF NOT EXISTS nodes_by_repo_kind ON nodes(repo_prefix, kind)`},
	{nodesByGenerationIndexName, nodesByGenerationIndexDDL},
	{edgesByGenerationIndexName, edgesByGenerationIndexDDL},
	{"nodes_repo_files", `CREATE INDEX IF NOT EXISTS nodes_repo_files ON nodes(repo_prefix, workspace_id, language, file_path, id) WHERE kind = 'file'`},
	{"edges_by_unresolved", `CREATE INDEX IF NOT EXISTS edges_by_unresolved ON edges(is_unresolved) WHERE is_unresolved = 1`},
	{"edges_fnvalue_prefixed", `CREATE INDEX IF NOT EXISTS edges_fnvalue_prefixed ON edges(to_id) WHERE to_id LIKE '%::unresolved::fnvalue::%'`},
	{"nodes_go_receiver_type", `CREATE INDEX IF NOT EXISTS nodes_go_receiver_type ON nodes(repo_prefix, file_dir, name, id) WHERE language = 'go' AND kind IN ('type', 'interface') AND name <> '' AND file_path <> ''`},
}

// An ordinary cold load gets a substantial unindexed head, but never accumulates
// an unbounded serial CREATE INDEX tail. A coordinated multi-repository cold
// load defers the dense-index rebuild to its explicit outer boundary so later
// repository drains do not pay per-row maintenance on already-dense indexes.
// WAL pressure is checkpointed at smaller independent intervals in both modes,
// so index-seal thresholds do not double as checkpoint cadence.
const (
	bulkIndexSealNodeLimit = int64(512 << 10)
	bulkIndexSealEdgeLimit = int64(1 << 20)

	// Keep routine checkpoints coarse enough that their bounded scan cost stays
	// amortized during active ingestion. Dense indexes amplify dirty pages, but
	// reducing the row interval only makes a timed-out checkpoint recur sooner.
	bulkCheckpointNodeInterval = int64(64 << 10)
	bulkCheckpointEdgeInterval = int64(256 << 10)

	// The final pre-ANALYZE checkpoint coalesces the former one-second
	// post-edge attempt with the five-second planner window. One continuous
	// attempt preserves the same total drain budget without restarting the WAL
	// scan between two adjacent boundaries that have no intervening write.
	bulkPlannerStatsCheckpointTimeout = 6 * time.Second
	// The cold-load handoff must finish copying a multi-gigabyte WAL before
	// resolver-heavy random reads begin. Routine checkpoints stay at one second;
	// this one terminal PASSIVE drain gets a larger, still-bounded I/O window.
	bulkFinalCheckpointTimeout = 30 * time.Second
)

// bulkCacheSizeKiB is the page cache the fast path requests on its pinned
// connection. SQLite reads a negative cache_size as a KiB budget, so this is
// ~256 MiB — large enough to keep the cold load's working set resident.
const bulkCacheSizeKiB = -262144

// beginWrite starts a write transaction. During a bulk-load fast path it pins
// the single connection that carries synchronous=OFF + the enlarged page
// cache (database/sql PRAGMAs are connection-local, so a pooled connection
// would not see them); otherwise it uses the shared pool. The caller holds
// writeMu, which also guards s.bulkConn.
func (s *Store) beginWrite() (*sql.Tx, error) {
	return s.beginWriteContext(context.Background())
}

type sqliteTxBeginner interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func (s *Store) beginWriteContext(ctx context.Context) (*sql.Tx, error) {
	if err := s.refuseSealedPayloadWrite(); err != nil {
		return nil, err
	}
	if s.bulkConn != nil {
		return s.beginWriteOnConnContext(ctx, s.bulkConn)
	}
	return s.beginWriteOnContext(ctx, s.writerDB)
}

func (s *Store) beginWriteOnConnContext(ctx context.Context, conn *sql.Conn) (*sql.Tx, error) {
	return s.beginWriteOnContext(ctx, conn)
}

func (s *Store) beginWriteOnContext(ctx context.Context, beginner sqliteTxBeginner) (*sql.Tx, error) {
	var tx *sql.Tx
	err := s.withSQLiteBusyRetry(ctx, "begin_write", func(attemptCtx context.Context) error {
		var beginErr error
		tx, beginErr = beginner.BeginTx(attemptCtx, nil)
		return beginErr
	})
	return tx, err
}

// execActiveWriteLocked and queryActiveWriteLocked keep sidecar and eviction
// writes on the pinned bulk connection when one is active. Callers hold
// writeMu, which guards bulkConn for the full operation.
func (s *Store) execActiveWriteLocked(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := s.refuseSealedPayloadWrite(); err != nil {
		return nil, err
	}
	if s.bulkConn != nil {
		return s.bulkConn.ExecContext(ctx, query, args...)
	}
	return s.writerDB.ExecContext(ctx, query, args...)
}

func (s *Store) queryActiveWriteLocked(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s.bulkConn != nil {
		return s.bulkConn.QueryContext(ctx, query, args...)
	}
	return s.writerDB.QueryContext(ctx, query, args...)
}

// activeWriteConnLocked returns the sole writer connection while writeMu is
// held. A cold bulk window already pins that connection, so callers must reuse
// it and the returned release is a no-op; otherwise release returns the checked
// out writer connection to its max-one pool.
func (s *Store) activeWriteConnLocked(ctx context.Context) (*sql.Conn, func(), error) {
	if s.bulkConn != nil {
		return s.bulkConn, func() {}, nil
	}
	conn, err := s.writerDB.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	return conn, func() { _ = conn.Close() }, nil
}

// BeginBulkLoad enters the bulk-load fast path for a first/empty cold index.
// It pins one connection at synchronous=OFF with an enlarged page cache and
// drops the droppable secondary indexes, so a multi-hundred-thousand-row load
// skips per-row B-tree maintenance and per-commit fsync. FlushBulk reverses
// all of it: restore the pragmas, rebuild the indexes, and checkpoint.
//
// Gated: it engages ONLY when both graph tables and the durable warm-restart
// sidecars prove this is a fresh store. On an incremental/warm store it is a
// safe no-op — even if corruption left nodes empty while edges survived.
// In-memory stores have no WAL / on-disk B-tree pressure, so it is a no-op.
func (s *Store) BeginBulkLoad() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.coordinatedBulkLoad {
		return
	}
	s.beginBulkLoadLocked()
}

// BeginCoordinatedBulkLoad opens one outer cold-load window around a set of
// concurrently indexed repositories. It returns true only when the store was
// empty and the fast path engaged. While active, the ordinary per-repository
// BeginBulkLoad/FlushBulk pair is a no-op: every shadow drain still routes its
// writes through the pinned connection, but secondary indexes rebuild once at
// EndCoordinatedBulkLoad instead of after the first repository.
func (s *Store) BeginCoordinatedBulkLoad() bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.coordinatedBulkLoad || s.bulkConn != nil {
		return false
	}
	s.beginBulkLoadLocked()
	if s.bulkConn == nil {
		return false
	}
	s.coordinatedBulkLoad = true
	s.syncBulkWindowLocked()
	return true
}

func (s *Store) beginBulkLoadLocked() {

	// Re-entrancy / non-disk guard: a second BeginBulkLoad without an
	// intervening FlushBulk, or an in-memory store, stays a no-op.
	if s.bulkConn != nil || isMemoryPath(s.dbPath) {
		return
	}

	ctx := context.Background()
	conn, err := s.writerDB.Conn(ctx)
	if err != nil {
		return
	}

	// Nodes alone are not a cold-store proof. A degraded warm store can have
	// zero nodes while retaining millions of edges, and sidecar receipts prove
	// an incremental lifecycle even when both graph tables happen to be empty.
	if !coldGraphStoreEmpty(ctx, conn) {
		_ = conn.Close()
		return
	}

	// Capture prior pragma values so FlushBulk (and every early-return /
	// error path) can restore them. If they can't be read, don't engage —
	// a slow correct load beats a connection stuck at synchronous=OFF.
	prevSync, err := pragmaInt(ctx, conn, "synchronous")
	if err != nil {
		_ = conn.Close()
		return
	}
	prevCache, err := pragmaInt(ctx, conn, "cache_size")
	if err != nil {
		_ = conn.Close()
		return
	}
	prevAutoCheckpoint, err := pragmaInt(ctx, conn, "wal_autocheckpoint")
	if err != nil {
		_ = conn.Close()
		return
	}

	// synchronous=OFF drops crash durability for the load window —
	// acceptable only because a crash on a fresh index just re-indexes.
	if _, err := conn.ExecContext(ctx, "PRAGMA synchronous = OFF"); err != nil {
		_ = conn.Close()
		return
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size = %d", bulkCacheSizeKiB)); err != nil {
		// Roll the durability change back before bailing.
		_, _ = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA synchronous = %d", prevSync))
		_ = conn.Close()
		return
	}
	// Automatic checkpoints can make one large drain unpredictably pay the
	// accumulated WAL cost mid-transaction. Coordinated checkpoints below are
	// explicit and bounded; the prior connection-local cadence is restored at
	// every exit from the proven-fresh bulk window.
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_autocheckpoint = 0"); err != nil {
		_, _ = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size = %d", prevCache))
		_, _ = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA synchronous = %d", prevSync))
		_ = conn.Close()
		return
	}

	// Drop the droppable secondary indexes; rebuilt in one pass at
	// FlushBulk. Best-effort: a failed drop just means that index keeps
	// being maintained per-row (slower, still correct), so it is not fatal.
	for _, idx := range bulkDroppableIndexes {
		_, _ = conn.ExecContext(ctx, "DROP INDEX IF EXISTS "+idx.name)
	}

	s.bulkConn = conn
	s.syncBulkWindowLocked()
	s.bulkPrevSync = prevSync
	s.bulkPrevCacheSize = prevCache
	s.bulkPrevAutoCheckpoint = prevAutoCheckpoint
	s.bulkIndexesDeferred = true
	s.bulkDeferredNodeRows = 0
	s.bulkDeferredEdgeRows = 0
	s.bulkCheckpointNodeRows = 0
	s.bulkCheckpointEdgeRows = 0
	s.bulkRowCheckpointBackoff = false
	// The bulk path changes durability and secondary-index maintenance outside
	// the ordinary row mutation protocol. Active receipts therefore fail closed.
	s.markMutationReceiptsIncompleteLocked()
}

// FlushBulk exits the bulk-load fast path: it rebuilds every dropped index,
// restores synchronous + cache_size + wal_autocheckpoint, releases the pinned writer and write gate,
// then performs one bounded TRUNCATE checkpoint. The ordering matters: the
// durability checkpoint must run on a NORMAL connection, and it must not wait
// for a second writer slot while the bulk connection is still pinned.
func (s *Store) FlushBulk() error {
	s.writeMu.Lock()
	if s.coordinatedBulkLoad {
		// The outer coordinated window owns index sealing and its final
		// durability checkpoint. WAL pressure inside that window is already
		// bounded by noteBulkRowsLocked's deterministic row intervals. A nested
		// repository boundary can coincide with an unrelated read snapshot and
		// spend the full passive-checkpoint timeout without advancing the WAL;
		// repeating that timeout once per repository only stalls the cold path.
		s.writeMu.Unlock()
		return nil
	}
	hadBulk := s.bulkConn != nil
	sealErr := s.sealBulkIndexesLocked("flush")
	closeErr := s.closeBulkConnectionLocked()
	s.jsonbIngestBuffers.release()
	s.writeMu.Unlock()
	if !hadBulk {
		return errors.Join(sealErr, closeErr)
	}
	return errors.Join(sealErr, closeErr, s.checkpointBulkWAL())
}

// EndCoordinatedBulkLoad closes an outer multi-repository cold-load window.
// It is idempotent and safe to defer: durability/cache restoration and index
// rebuild happen even when one repository failed or panicked.
func (s *Store) EndCoordinatedBulkLoad() error {
	s.writeMu.Lock()
	if !s.coordinatedBulkLoad {
		s.writeMu.Unlock()
		return nil
	}
	s.coordinatedBulkLoad = false
	s.syncBulkWindowLocked()
	if s.deferredFTSOptimize {
		// A full FTS5 optimize is unbounded and previously sat directly on the
		// cold-start critical path. One bounded merge keeps segment growth in
		// check; the index is already transactionally correct without either.
		started := time.Now()
		_, err := s.execActiveWriteLocked(context.Background(), `INSERT INTO symbol_fts(symbol_fts, rank) VALUES('merge', ?)`, coldFTSMergePages)
		s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "fts_merge", Name: "symbol_fts", Elapsed: time.Since(started), Err: err})
		s.deferredFTSOptimize = false
	}
	if s.deferredContentFTS {
		started := time.Now()
		_, err := s.execActiveWriteLocked(context.Background(), `INSERT INTO content_fts(content_fts, rank) VALUES('merge', ?)`, coldFTSMergePages)
		s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "fts_merge", Name: "content_fts", Elapsed: time.Since(started), Err: err})
		s.deferredContentFTS = false
	}
	hadBulk := s.bulkConn != nil
	sealErr := s.sealBulkIndexesLocked("final")
	if sealErr != nil {
		// Keep the pinned connection, deferred-index state, and outer boundary
		// live so an explicit retry can finish the same durable load. Closing the
		// connection here would make the failed DDL impossible to retry because
		// sealBulkIndexesLocked intentionally requires the pinned bulk writer.
		s.coordinatedBulkLoad = true
		s.syncBulkWindowLocked()
		s.writeMu.Unlock()
		return sealErr
	}
	// Give the accumulated WAL one longer, still-bounded PASSIVE drain before
	// planner statistics traverse the large graph indexes. This runs on the
	// pinned writer under writeMu, so no mutation can race it; PASSIVE still
	// returns immediately at an old read snapshot instead of waiting for it.
	if sealErr == nil && hadBulk {
		_ = s.checkpointBulkWALPassiveLockedWithin("planner_stats_before", bulkPlannerStatsCheckpointTimeout)
	}
	// Target only the graph indexes whose statistics alter competing plans:
	// ANALYZE over every FTS/sidecar or forced graph index pays unrelated B-tree
	// page counts without changing a query choice.
	var statsErr error
	if sealErr == nil && hadBulk {
		statsStarted := time.Now()
		statsErr = s.refreshPlannerStatsLocked(context.Background())
		s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "planner_stats", Name: "nodes_edges", Elapsed: time.Since(statsStarted), Err: statsErr})
	}
	closeErr := s.closeBulkConnectionLocked()
	// The refresh above ran on the pinned writer, so readers already holding a
	// connection would keep the pre-load statistics forever, and the runtime
	// freshness checker would not know an ANALYZE had just run. Both are done
	// after the pinned connection is released so neither touches it.
	if sealErr == nil && hadBulk && statsErr == nil {
		s.stampPlannerStatsRefresh(context.Background(), "cold_load_finalize")
		recycleStatsReadPool(s.db, s.writerDB)
	}
	// The writer gate prevents a competing writer, but it cannot retire a
	// snapshot held by the read-only pool. RESTART/TRUNCATE invokes SQLite's
	// busy handler until every such reader leaves the WAL, which made cold
	// finalization spend its full 10-second deadline while the daemon was
	// already queryable. PASSIVE copies every currently safe frame without
	// waiting for readers. Committed WAL frames are durable either way; later
	// passive checkpoints plus SQLite's next WAL reset/journal_size_limit reclaim
	// the file after the reader leaves.
	if hadBulk {
		ctx, cancel := context.WithTimeout(context.Background(), bulkFinalCheckpointTimeout)
		started := time.Now()
		result, err := s.checkpointWALOnce(ctx, "PASSIVE")
		cancel()
		// A reader-limited partial PASSIVE checkpoint is expected telemetry, not
		// a bulk-load failure. Preserve counters in the ordinary checkpoint log.
		if errors.Is(err, errSQLiteCheckpointIncomplete) {
			err = nil
		}
		s.emitBulkFinalizeEvent(bulkFinalizeEvent{
			Stage: "checkpoint", Name: "wal_passive", Elapsed: time.Since(started),
			Busy: result.Busy, WALFrames: result.WALFrames,
			CheckpointedFrames: result.CheckpointedFrames, Err: err,
		})
	}
	s.jsonbIngestBuffers.release()
	s.writeMu.Unlock()
	return errors.Join(sealErr, statsErr, closeErr)
}

// AbortCoordinatedBulkLoad gives up retryable finalization and restores the
// writer connection unconditionally. It is for terminal owners only: dense
// indexes whose rebuild failed may remain absent until normal schema repair or
// restart, but the store no longer holds synchronous=OFF, disabled automatic
// checkpoints, or a pinned writer indefinitely.
func (s *Store) AbortCoordinatedBulkLoad() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.coordinatedBulkLoad = false
	err := s.closeBulkConnectionLocked()
	s.jsonbIngestBuffers.release()
	return err
}

// noteBulkRowsLocked advances independent index-seal and WAL-checkpoint budgets
// after a committed AddBatch. The caller holds writeMu. A failed seal remains
// pending and is retried by the next repository/final boundary.
func (s *Store) noteBulkRowsLocked(nodeRows, edgeRows int) error {
	if s.bulkConn == nil {
		return nil
	}
	nodeDelta, edgeDelta := int64(nodeRows), int64(edgeRows)
	if !s.bulkRowCheckpointBackoff {
		s.bulkCheckpointNodeRows += nodeDelta
		s.bulkCheckpointEdgeRows += edgeDelta
	}

	var sealReason string
	if s.bulkIndexesDeferred {
		s.bulkDeferredNodeRows += nodeDelta
		s.bulkDeferredEdgeRows += edgeDelta
		// The coordinated window is itself a bounded lifecycle: its outer end
		// is the only dense-index boundary. Sealing at an inner row threshold
		// makes every later repository maintain the full index set per row,
		// defeating the point of coordinating the cold load. Ordinary one-shot
		// bulk loads retain the threshold as protection against an unbounded
		// final CREATE INDEX tail.
		if !s.coordinatedBulkLoad {
			switch {
			case s.bulkDeferredNodeRows >= bulkIndexSealNodeLimit:
				sealReason = "node_limit"
			case s.bulkDeferredEdgeRows >= bulkIndexSealEdgeLimit:
				sealReason = "edge_limit"
			}
		}
	}
	if sealReason != "" {
		return s.sealBulkIndexesLocked(sealReason)
	}
	if s.bulkRowCheckpointBackoff {
		return nil
	}
	nodeInterval, edgeInterval := s.bulkCheckpointIntervalsLocked()
	if s.bulkCheckpointNodeRows >= nodeInterval ||
		s.bulkCheckpointEdgeRows >= edgeInterval {
		err := s.checkpointBulkWALPassiveLocked("row_limit")
		s.noteBulkRowCheckpointResultLocked(err)
	}
	return nil
}

func (s *Store) bulkCheckpointIntervalsLocked() (nodeRows, edgeRows int64) {
	return bulkCheckpointNodeInterval, bulkCheckpointEdgeInterval
}

// sealBulkIndexesLocked rebuilds the deferred dense indexes without restoring
// pragmas or releasing the pinned writer. Ordinary bulk loads may pay one
// bounded rebuild at a row threshold; coordinated loads rebuild only at their
// explicit outer boundary. A failed attempt remains pending and can be retried.
func (s *Store) sealBulkIndexesLocked(reason string) error {
	conn := s.bulkConn
	if conn == nil || !s.bulkIndexesDeferred {
		return nil
	}
	s.markMutationReceiptsIncompleteLocked()
	ctx := context.Background()
	started := time.Now()
	var sealErr error
	// Keep index DDL from repeatedly searching a large uncheckpointed WAL.
	// The group checkpoints are best-effort telemetry boundaries; DDL errors
	// still keep the seal retryable.
	_ = s.checkpointBulkWALPassiveLocked("index_seal_before")
	for _, idx := range bulkDroppableIndexes {
		indexStarted := time.Now()
		_, err := conn.ExecContext(ctx, idx.ddl)
		s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "index", Name: idx.name, Elapsed: time.Since(indexStarted), Err: err})
		if err != nil {
			sealErr = errors.Join(sealErr, fmt.Errorf("store_sqlite: rebuild index %s: %w", idx.name, err))
		}
		if idx.name == "nodes_by_repo_language_name" {
			_ = s.checkpointBulkWALPassiveLocked("index_seal_nodes")
		}
	}
	if sealErr != nil {
		// A failed seal returns before the coalesced pre-ANALYZE checkpoint.
		// Preserve the former post-edge drain so a retry does not inherit every
		// frame emitted by this attempt.
		_ = s.checkpointBulkWALPassiveLocked("index_seal_edges")
	}
	s.emitBulkFinalizeEvent(bulkFinalizeEvent{
		Stage: "index_seal", Name: reason, Elapsed: time.Since(started),
		NodeRows: s.bulkDeferredNodeRows, EdgeRows: s.bulkDeferredEdgeRows, Err: sealErr,
	})
	if sealErr != nil {
		return sealErr
	}
	s.bulkIndexesDeferred = false
	return nil
}

// closeBulkConnectionLocked restores connection-local pragmas and releases the
// pinned writer. Dense-index rebuilding deliberately lives in
// sealBulkIndexesLocked so an early seal cannot end the outer window.
func (s *Store) closeBulkConnectionLocked() error {
	// Unconditional: every caller has just cleared coordinatedBulkLoad, and
	// the early return below is the path where the pinned connection was
	// already gone but that flag still has to reach the health probe.
	defer s.syncBulkWindowLocked()
	conn := s.bulkConn
	if conn == nil {
		return nil
	}
	s.bulkConn = nil
	ctx := context.Background()
	_, syncErr := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA synchronous = %d", s.bulkPrevSync))
	_, cacheErr := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size = %d", s.bulkPrevCacheSize))
	_, autoCheckpointErr := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", s.bulkPrevAutoCheckpoint))
	closeErr := conn.Close()
	s.bulkIndexesDeferred = false
	s.bulkDeferredNodeRows = 0
	s.bulkDeferredEdgeRows = 0
	s.bulkCheckpointNodeRows = 0
	s.bulkCheckpointEdgeRows = 0
	s.bulkRowCheckpointBackoff = false
	s.bulkPrevSync = 0
	s.bulkPrevCacheSize = 0
	s.bulkPrevAutoCheckpoint = 0
	return errors.Join(
		wrapBulkRestoreError("synchronous", syncErr),
		wrapBulkRestoreError("cache_size", cacheErr),
		wrapBulkRestoreError("wal_autocheckpoint", autoCheckpointErr),
		closeErr,
	)
}

func wrapBulkRestoreError(pragma string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("store_sqlite: restore bulk PRAGMA %s: %w", pragma, err)
}

func (s *Store) checkpointBulkWAL() error {
	ctx, cancel := context.WithTimeout(context.Background(), walCheckpointTimeout)
	defer cancel()
	started := time.Now()
	result, err := s.checkpointWALWithContextResult(ctx)
	s.emitBulkFinalizeEvent(bulkFinalizeEvent{
		Stage: "checkpoint", Name: "wal_truncate", Elapsed: time.Since(started),
		Busy: result.Busy, WALFrames: result.WALFrames,
		CheckpointedFrames: result.CheckpointedFrames, Err: err,
	})
	if err != nil {
		return fmt.Errorf("store_sqlite: bulk checkpoint: %w", err)
	}
	return nil
}

// checkpointBulkWALPassiveLocked is a best-effort repository boundary while
// the bulk writer remains pinned. PASSIVE never waits for readers and failure
// is telemetry-only; the NORMAL-connection TRUNCATE at final close remains the
// durability boundary.
func (s *Store) checkpointBulkWALPassiveLocked(boundary string) error {
	return s.checkpointBulkWALPassiveLockedWithin(boundary, s.passiveCheckpointWindow())
}

func (s *Store) checkpointBulkWALPassiveLockedWithin(boundary string, window time.Duration) error {
	if s.bulkConn == nil {
		return nil
	}
	nodeRows, edgeRows := s.bulkCheckpointNodeRows, s.bulkCheckpointEdgeRows

	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	started := time.Now()
	result, err := checkpointWALOnceOn(ctx, s.bulkConn, "PASSIVE")
	// Every bounded attempt consumes one cadence unit, so its counters reset even
	// when timeout/contention provides no reliable progress. Coordinated loads
	// then back off later automatic row-limit attempts; the explicit final
	// checkpoint remains the durability and WAL-drain boundary.
	s.bulkCheckpointNodeRows = 0
	s.bulkCheckpointEdgeRows = 0
	s.emitBulkFinalizeEvent(bulkFinalizeEvent{
		Stage: "checkpoint_passive", Name: boundary, Elapsed: time.Since(started),
		NodeRows: nodeRows, EdgeRows: edgeRows,
		Busy: result.Busy, WALFrames: result.WALFrames,
		CheckpointedFrames: result.CheckpointedFrames, Err: err,
	})
	return err
}

// noteBulkRowCheckpointResultLocked backs off repeated automatic attempts only
// within the current coordinated lifecycle. Explicit checkpoint boundaries do
// not call it and therefore remain independent of the automatic cadence.
func (s *Store) noteBulkRowCheckpointResultLocked(err error) {
	if s.coordinatedBulkLoad && shouldBackoffBulkRowCheckpoint(err) {
		s.bulkRowCheckpointBackoff = true
	}
}

func shouldBackoffBulkRowCheckpoint(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || isSQLiteBusyErr(err) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// coldGraphStoreEmpty proves this is a fresh, never-indexed store. Nodes and
// edges must both be empty, and neither durable warm-restart sidecar may carry
// prior lifecycle state. Any query error fails closed to the ordinary indexed
// writer path.
//
// The node/edge probes name generation 0 explicitly rather than a handle's
// generation: this fast path exists for the first index of the base corpus,
// which is the only thing a cold load writes. The sidecar probes are
// deliberately generation-unscoped: a store holding any generation's lifecycle
// rows has been indexed before, whichever view wrote them, so it is not the
// cold store this fast path is for.
func coldGraphStoreEmpty(ctx context.Context, conn *sql.Conn) bool {
	var empty int
	err := conn.QueryRowContext(ctx, `
SELECT NOT EXISTS(SELECT 1 FROM nodes WHERE view_gen = ?)
   AND NOT EXISTS(SELECT 1 FROM edges WHERE view_gen = ?)
   AND NOT EXISTS(SELECT 1 FROM file_mtimes)
   AND NOT EXISTS(SELECT 1 FROM repo_index_state)`,
		baseViewGeneration, baseViewGeneration).Scan(&empty)
	return err == nil && empty == 1
}

// pragmaInt reads a single-integer PRAGMA (synchronous, cache_size, wal_autocheckpoint) off the
// given connection.
func pragmaInt(ctx context.Context, conn *sql.Conn, pragma string) (int64, error) {
	var v int64
	if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
