package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
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
	if event.Stage == "checkpoint" || event.Stage == "checkpoint_passive" || event.Stage == "checkpoint_deferred" {
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
// Generation-scoped hot lookups keep their established lookup columns first
// and append view_gen before any trailing filter/order column. A scoped Reader
// can therefore constrain one payload generation without losing the leading
// lookup seek, while generation-blind diagnostics can still seek the same
// leading prefix. A WITHOUT ROWID nodes index also carries the (id, view_gen)
// primary key suffix, so ID projections remain index-only and scoped ORDER BY
// id stays free.
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
// Dropping/recreating these is a runtime operation on identical DDL. Changing
// their DDL is a schema change and requires a schemaMigrations entry so a warm
// store does not retain the prior key shape.
const (
	edgesByFromLineIndexDDL     = `CREATE INDEX IF NOT EXISTS edges_by_from_line ON edges(view_gen, from_id, line)`
	edgesByFromLineKindIndexDDL = `CREATE INDEX IF NOT EXISTS edges_by_from_line_kind ON edges(view_gen, from_id, line, kind)`
)

var bulkDroppableIndexes = []bulkDroppableIndex{
	{"nodes_by_name", `CREATE INDEX IF NOT EXISTS nodes_by_name ON nodes(name, view_gen)`},
	{"nodes_by_kind", `CREATE INDEX IF NOT EXISTS nodes_by_kind ON nodes(kind, view_gen)`},
	{"nodes_by_file", `CREATE INDEX IF NOT EXISTS nodes_by_file ON nodes(file_path, view_gen)`},
	// Keep this dense. Repository projection queries discover their prefixes
	// through json_each joins, which cannot imply a repo_prefix <> '' partial
	// predicate, and empty-prefix rows remain part of the Store query contract.
	{"nodes_by_repo", `CREATE INDEX IF NOT EXISTS nodes_by_repo ON nodes(repo_prefix, view_gen)`},
	// Resolver warmup selects definitions by exact repository, compatible
	// language family, and a bounded page of names. Keep the key minimal: kind
	// is not a query predicate and WITHOUT ROWID secondary indexes already
	// carry the primary-key id. The partial predicate excludes nameless nodes.
	{"nodes_by_repo_language_name", `CREATE INDEX IF NOT EXISTS nodes_by_repo_language_name ON nodes(repo_prefix, language, name, view_gen) WHERE name <> ''`},
	{"edges_by_from", `CREATE INDEX IF NOT EXISTS edges_by_from ON edges(view_gen, from_id, kind)`},
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
	{"edges_by_to", `CREATE INDEX IF NOT EXISTS edges_by_to ON edges(view_gen, to_id, kind)`},
	{"edges_by_kind", `CREATE INDEX IF NOT EXISTS edges_by_kind ON edges(kind)`},
	// Exact changed-file frontiers (watcher and partial indexing) must not
	// scan every edge merely to find source sites owned by one file.
	{"edges_by_file", `CREATE INDEX IF NOT EXISTS edges_by_file ON edges(file_path, kind)`},
}

// nodes_by_generation / edges_by_generation serve payload enumeration,
// garbage collection, and every generation-only graph read. They are dense:
// SQLite cannot infer a partial `view_gen > 0` predicate from `view_gen = ?`,
// even for a positive bound value, so the former partial indexes left ordinary
// generation-0 and derived-generation equality reads scanning the whole table.
// Leading with view_gen gives both populations the same bounded seek while id
// preserves stable enumeration order.
const (
	nodesByGenerationIndexName = "nodes_by_generation"
	edgesByGenerationIndexName = "edges_by_generation"

	nodesByGenerationIndexDDL = `CREATE INDEX IF NOT EXISTS nodes_by_generation ON nodes(view_gen, id)`
	edgesByGenerationIndexDDL = `CREATE INDEX IF NOT EXISTS edges_by_generation ON edges(view_gen, id)`
	// Compact supplementary-name scans never visit the wide nodes payload.
	// Repo equality narrows the scan; trailing columns cover scope predicates.
	nodesNameCandidatesIndexDDL = `CREATE INDEX IF NOT EXISTS nodes_name_candidates ON nodes(view_gen, repo_prefix, id, name, kind, file_path, workspace_id, project_id) WHERE name <> ''`
)

// bulkAlwaysLiveIndexes preserve bounded maintenance and resolver/repository
// projections as soon as the first repository publishes. The repository and
// generation lookup indexes are dense; the remaining partial indexes stay
// live so their narrow frontiers remain queryable throughout bulk loading.
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
	{"nodes_name_candidates", nodesNameCandidatesIndexDDL},
	{edgesByGenerationIndexName, edgesByGenerationIndexDDL},
	{"nodes_repo_files", `CREATE INDEX IF NOT EXISTS nodes_repo_files ON nodes(repo_prefix, workspace_id, language, file_path, id) WHERE kind = 'file'`},
	{"edges_by_unresolved", `CREATE INDEX IF NOT EXISTS edges_by_unresolved ON edges(is_unresolved) WHERE is_unresolved = 1`},
	{"edges_fnvalue_prefixed", `CREATE INDEX IF NOT EXISTS edges_fnvalue_prefixed ON edges(view_gen, to_id) WHERE to_id LIKE '%::unresolved::fnvalue::%'`},
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

// A generation bulk window is the write half of an interactive build: its
// rows are what a checkout view waits on, so a checkpoint it runs inline is
// charged straight to publication. The window therefore copies no backlog
// inline. At every row-limit interval and at window end it reads the backlog
// from the wal-index (a lock-free read of the -shm header, microseconds) and
// defers: the residue gate at window end schedules the maintenance lane's
// drain, and the WAL reclaim poller takes over once the window's checkpoint
// lease is released. Only a backlog within generationInlineCheckpointMaxFrames
// (zero by default: nothing left to copy) or an unreadable wal-index still
// runs a PASSIVE inline, bounded by generationInlineCheckpointWindow.
//
// Why no inline copy at all: SQLite publishes checkpoint progress (nBackfill)
// only when a pass completes, so a pass cut off by its deadline records nothing
// and the next one copies the same frames again; the live log showed
// "checkpoint_passive name=row_limit elapsed=1.0s error=context deadline
// exceeded" at every row-limit interval plus a second one at window end. And
// even a completed small copy is not cheap: on the real-repository clone at
// GOMAXPROCS=1 a window-end PASSIVE took 822 ms for 3,392 frames and 381 ms
// for 1,398 (two syncs plus ~0.2 ms a frame), and a 50 ms deadline on the
// latter returned only after 232 ms because the copy checks for an interrupt
// between pages.
//
// Variables rather than constants so tests can pin both branches.
// GORTEX_SQLITE_GENERATION_CHECKPOINT=inline restores the former policy (an
// inline PASSIVE bounded at the routine passive window at every row-limit
// interval and at window end, and the residue drain scheduled at window end
// rather than after publication) for comparison and as a kill switch.
var (
	generationInlineCheckpointWindow    = 50 * time.Millisecond
	generationInlineCheckpointMaxFrames = int64(0)
)

func generationCheckpointInlinePolicy() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GORTEX_SQLITE_GENERATION_CHECKPOINT")), "inline")
}

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
	if err != nil {
		return tx, err
	}
	writeTransactionsBegun.Add(1)
	if s.managedPayloadGeneration {
		if err := checkManagedPayloadWriteTx(ctx, tx, s.viewGen); err != nil {
			if tx != nil {
				_ = tx.Rollback()
			}
			return nil, err
		}
	}
	return tx, nil
}

// execActiveWriteLocked and queryActiveWriteLocked keep sidecar and eviction
// writes on the pinned bulk connection when one is active. Callers hold
// writeMu, which guards bulkConn for the full operation.
func (s *Store) execActiveWriteLocked(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if s.managedPayloadGeneration {
		return s.execManagedPayloadWriteLocked(ctx, query, args...)
	}
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
	// The lazily built file-generation index goes too (the builder restores
	// it once the window closes); clear the presence cache first so no new
	// reader pins it.
	s.forgetFileGenerationIndex()
	_, _ = conn.ExecContext(ctx, "DROP INDEX IF EXISTS "+edgesByFileGenerationIndexName)

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

// ErrGenerationBulkLoadPopulated refuses a generation-scoped bulk window over
// a generation that already holds payload rows. The window's whole licence is
// that nothing can be reading or colliding with the rows it is about to write;
// a generation with rows has neither property, and a second window over one is
// the caller's bug, not a condition to work around.
var ErrGenerationBulkLoadPopulated = errors.New("store_sqlite: generation already holds payload rows")

// BeginGenerationBulkLoad opens the bulk write shape for ONE payload
// generation that holds no rows yet, and reports whether it engaged.
//
// Why it exists. The cold fast path above engages only on a proven-empty store
// (coldGraphStoreEmpty), which generation 0 consumes. Every later generation
// payload — a committed base, every delta — is therefore written through the
// ordinary incremental path at the pooled 32 MiB cache_size, where a working
// set larger than the cache spills dirty pages to the WAL and re-dirties them,
// and the same payload that costs ~227-259 MB in bulk shape costs ~841 MB.
// This window takes the cache half of the fast path's shape, which is where
// the measured -69% is: the identical payload replayed at cache_size=-262144
// alone cost 258,812,948 logical writes against 841,300,936 at store defaults.
//
// What it deliberately does NOT take, and why. The cold path also drops the
// dense secondary indexes and runs at synchronous=OFF. Neither is admissible
// here:
//
//   - The indexes are generation-blind, so dropping them would blind every
//     generation-0 reader for the length of this window and rebuild them
//     against the whole corpus at the end. The cold path can afford that
//     because nothing can read an empty store. Scoping a drop/rebuild to a
//     quiesced window is a separate step; until it exists the indexes stay
//     live and this window simply does not claim that slice (~31 MB of the
//     ~614 MB it does claim).
//   - synchronous=OFF trades power-loss corruption of the WHOLE file for
//     commit latency. On a fresh store that is a re-index; here generation 0's
//     published rows are underneath, so the trade is not ours to make — and it
//     buys no bytes anyway, which is what the replay above measures.
//
// wal_autocheckpoint IS taken to 0, so a payload cannot pay an automatic
// full-log drain halfway through; EndGenerationBulkLoad closes that loop by
// measuring the residue and scheduling a bounded TRUNCATE when it lands above
// the auto-checkpoint line.
//
// Mutation receipts are left intact for the same reason: this window changes
// neither durability nor index maintenance, so no write inside it is outside
// the ordinary row protocol.
//
// Preconditions, all refusals rather than silent no-ops:
//   - a positive generation. Generation 0 is the cold path's business and is
//     the one generation this window may never be opened on.
//   - the generation is writable, asked through the ordinary seal machinery
//     (refuseSealedPayloadWrite), so a published or retiring generation is
//     refused here exactly as its first write would be.
//   - the generation holds no nodes and no edges.
//
// It is a no-op returning false (not an error) on an in-memory store, which
// has no WAL or on-disk B-tree pressure to spare, and while another bulk
// window already owns the pinned connection — those writes already have a bulk
// shape, and stealing the window from a cold load would drop its deferred
// indexes on the floor. A false return means no window was opened and
// EndGenerationBulkLoad must not be called for it.
func (s *Store) BeginGenerationBulkLoad(generationID int64) (bool, error) {
	return s.beginGenerationBulkLoad(context.Background(), generationID, false)
}

func (s *Store) beginGenerationBulkLoad(ctx context.Context, generationID int64, reparse bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.coreless() {
		return false, fmt.Errorf("%w: a generation bulk load needs an open store", ErrCatalogInvalidValue)
	}
	if generationID <= baseViewGeneration {
		return false, fmt.Errorf("%w: generation bulk load needs a positive generation, got %d", ErrCatalogInvalidValue, generationID)
	}
	if isMemoryPath(s.dbPath) {
		return false, nil
	}
	// Asked before the write gate is taken: resolving a seal reads the catalog
	// on its own connection, and the pinned writer this method is about to
	// take is the connection it would otherwise contend with.
	if err := s.AtGeneration(generationID).refuseSealedPayloadWrite(); err != nil {
		return false, err
	}

	// Preserve the legacy false,nil result only for a physical bulk owner that
	// is already installed. A lease without an installed owner is a competing
	// Begin and must fail so callers cannot fall back beside a checkpoint.
	if err := s.lockGenerationBulkWriter(ctx, reparse); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		s.writeMu.Unlock()
		return false, err
	}
	if s.bulkConn != nil || s.coordinatedBulkLoad {
		s.writeMu.Unlock()
		return false, nil
	}
	s.writeMu.Unlock()

	lease, err := s.acquireGenerationBulkCheckpointLeaseContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("%w: %v", ErrGenerationBulkCheckpointBusy, err)
	}
	leaseBound := false
	opened := false
	if err := s.lockGenerationBulkWriter(ctx, reparse); err != nil {
		s.releaseGenerationBulkCheckpointLease(lease)
		return false, err
	}
	defer func() {
		release := lease
		if leaseBound && !opened {
			release = s.takeGenerationBulkCheckpointLease(generationID)
		}
		s.writeMu.Unlock()
		if !opened {
			s.releaseGenerationBulkCheckpointLease(release)
		}
	}()
	if s.bulkConn != nil || s.coordinatedBulkLoad {
		return false, nil
	}

	conn, err := s.writerDB.Conn(ctx)
	if err != nil {
		return false, err
	}
	if reparse {
		// Recheck through the held writer connection, not a catalog connection
		// that could wait for this gate. Per-write managed admission remains
		// authoritative throughout the window.
		if err := checkDedicatedReparseWindow(ctx, conn, generationID); err != nil {
			_ = conn.Close()
			return false, err
		}
	} else {
		empty, err := generationPayloadEmpty(ctx, conn, generationID)
		if err != nil {
			_ = conn.Close()
			return false, err
		}
		if !empty {
			_ = conn.Close()
			return false, fmt.Errorf("%w: generation %d", ErrGenerationBulkLoadPopulated, generationID)
		}
	}

	prevSync, err := pragmaInt(ctx, conn, "synchronous")
	if err != nil {
		_ = conn.Close()
		return false, err
	}
	prevCache, err := pragmaInt(ctx, conn, "cache_size")
	if err != nil {
		_ = conn.Close()
		return false, err
	}
	prevAutoCheckpoint, err := pragmaInt(ctx, conn, "wal_autocheckpoint")
	if err != nil {
		_ = conn.Close()
		return false, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size = %d", bulkCacheSizeKiB)); err != nil {
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA cache_size = %d", prevCache))
		_ = conn.Close()
		return false, err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_autocheckpoint = 0"); err != nil {
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", prevAutoCheckpoint))
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA cache_size = %d", prevCache))
		_ = conn.Close()
		return false, err
	}

	if err := ctx.Err(); err != nil {
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", prevAutoCheckpoint))
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA cache_size = %d", prevCache))
		_ = conn.Close()
		return false, err
	}

	// Binding is the final fallible transition. Everything below installs the
	// physical owner using assignments only, so a losing Begin cannot observe a
	// lease that has no recoverable owner.
	if !s.bindGenerationBulkCheckpointLease(lease, generationID) {
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", prevAutoCheckpoint))
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA cache_size = %d", prevCache))
		_ = conn.Close()
		return false, fmt.Errorf("%w: lease changed before generation %d became active", ErrGenerationBulkCheckpointBusy, generationID)
	}
	leaseBound = true
	s.bulkConn = conn
	s.generationBulkLoad = generationID
	s.syncBulkWindowLocked()
	s.bulkPrevSync = prevSync
	s.bulkPrevCacheSize = prevCache
	s.bulkPrevAutoCheckpoint = prevAutoCheckpoint
	s.bulkIndexesDeferred = false
	s.bulkDeferredNodeRows = 0
	s.bulkDeferredEdgeRows = 0
	s.bulkCheckpointNodeRows = 0
	s.bulkCheckpointEdgeRows = 0
	s.bulkRowCheckpointBackoff = false
	opened = true
	s.noteGenerationWriteWindowOpen(ctx, conn, generationID)
	s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "generation_bulk_begin", Name: strconv.FormatInt(generationID, 10)})
	return true, nil
}

// EndGenerationBulkLoad closes the window BeginGenerationBulkLoad opened:
// it restores the connection-local pragmas, releases the pinned writer, and
// hands the accumulated WAL to the residue gate.
//
// The drain is scheduled, not run here. A TRUNCATE waits out every reader, and
// the readers this window runs beside are generation 0's — the ones a
// committed-base build exists to keep serving. Charging that wait to the build
// is the coupling the maintenance lane exists to remove, so the measurement
// happens inline and the follow-up TRUNCATE runs on the lane. The measurement
// is the wal-index backlog, plus a PASSIVE only when that backlog is small
// enough to copy inside generationInlineCheckpointWindow (see
// generationWindowCheckpointLocked); a large backlog is never copied on the
// publication path.
//
// Idempotent and inert when no generation window is open, so a deferred call
// is always safe.
func (s *Store) EndGenerationBulkLoad() error {
	return s.endGenerationBulkLoad(0)
}

// EndGenerationBulkLoadFor closes only the physical generation owner named by
// generationID. It gives an owner that retains its generation identity a
// stale-safe finish operation; the legacy parameterless method remains an
// idempotent compatibility delegate for callers that do not retain identity.
func (s *Store) EndGenerationBulkLoadFor(generationID int64) error {
	if generationID <= baseViewGeneration {
		return fmt.Errorf("%w: generation bulk load needs a positive generation, got %d", ErrCatalogInvalidValue, generationID)
	}
	return s.endGenerationBulkLoad(generationID)
}

func (s *Store) endGenerationBulkLoad(expectedGeneration int64) error {
	if s.coreless() {
		return nil
	}
	s.writeMu.Lock()
	if s.generationBulkLoad == 0 || s.bulkConn == nil {
		s.writeMu.Unlock()
		return nil
	}
	generationID := s.generationBulkLoad
	if expectedGeneration > baseViewGeneration && expectedGeneration != generationID {
		s.writeMu.Unlock()
		return nil
	}
	s.noteGenerationWriteWindowClose(context.Background(), s.bulkConn, generationID)
	result := s.generationWindowCheckpointLocked("generation_bulk_end")
	closeErr := s.closeBulkConnectionLocked()
	s.jsonbIngestBuffers.release()
	// Clear the matching bound generation while writeMu still protects the
	// physical owner; release the exact token only after unlocking.
	lease := s.takeGenerationBulkCheckpointLease(generationID)
	s.writeMu.Unlock()
	s.releaseGenerationBulkCheckpointLease(lease)
	s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "generation_bulk_end", Name: strconv.FormatInt(generationID, 10), WALFrames: result.WALFrames, CheckpointedFrames: result.CheckpointedFrames})
	if generationCheckpointInlinePolicy() {
		s.scheduleWALDrainAboveLine(result, "generation_bulk_load")
	} else {
		s.oweWALDrainAfterPublication(result, "generation_bulk_load")
	}
	return closeErr
}

// generationDrainWakeDelay bounds how long a drain owed by a generation window
// waits for the publication that normally follows the window. A var only so
// tests can shorten it.
var generationDrainWakeDelay = 30 * time.Second

// oweWALDrainAfterPublication is the residue gate of a generation window. It
// owes the maintenance lane's drain exactly when scheduleWALDrainAboveLine
// would, but does not wake the lane: the window closes immediately before the
// build publishes, and a TRUNCATE started now copies the residue under the
// write gate that PublishPayloadGeneration must take next (measured on the
// real-repository clone: publication 1.8 s -> 6.5 s behind a 127,916-frame
// drain). The publication's own maintenance request wakes the lane after the
// generation is published, and the lane runs an owed drain before anything
// else. A window whose build never publishes wakes the lane after
// generationDrainWakeDelay instead, so the drain is never stranded.
func (s *Store) oweWALDrainAfterPublication(result walCheckpointResult, boundary string) {
	line := sqliteWALAutoCheckpointPages()
	if line <= 0 || result.WALFrames < 0 || result.WALFrames <= line || s.coreless() {
		return
	}
	s.walDrainRequests.Add(1)
	s.maintenanceSched.Lock()
	if s.maintenanceClosed || s.maintenanceSignal == nil || s.maintenanceDrainOwed {
		s.maintenanceSched.Unlock()
		return
	}
	s.maintenanceDrainOwed = true
	s.maintenanceDrainReason = boundary
	signal := s.maintenanceSignal
	s.maintenanceSched.Unlock()
	time.AfterFunc(generationDrainWakeDelay, func() {
		s.maintenanceSched.Lock()
		defer s.maintenanceSched.Unlock()
		if s.maintenanceClosed || !s.maintenanceDrainOwed || s.maintenanceSignal != signal {
			return
		}
		select {
		case signal <- struct{}{}:
		default:
		}
	})
}

// ErrGenerationBulkCheckpointBusy reports that a periodic checkpoint still
// owns the physical checkpoint path, so a generation build must retry instead
// of falling back to writes that compete with that checkpoint.
var ErrGenerationBulkCheckpointBusy = errors.New("store_sqlite: generation bulk checkpoint is still active")

// InGenerationBulkLoad reports the generation whose bulk window this store is
// currently holding, and whether it holds one at all.
//
// It is the observable half of the bracket: a caller that opened a window has
// no other way to state "the window was open AROUND this write" from outside
// the package, and the two facts a bracket is worth anything for — that it was
// opened before the payload and closed after it — are otherwise invisible to
// everything but the WAL. It reads the same field the window itself is, under
// the same gate, so it can never disagree with the store's own state.
//
// Not a licence: nothing may act on the answer to decide whether to write. The
// window is an optimisation over a write that is correct without it.
func (s *Store) InGenerationBulkLoad() (int64, bool) {
	if s.coreless() {
		return 0, false
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.generationBulkLoad, s.generationBulkLoad != 0 && s.bulkConn != nil
}

// GenerationBulkLoadShape reports the connection-local write shape an open
// generation window is actually holding: the pinned writer's page cache (a
// negative SQLite cache_size is a KiB budget) and its automatic-checkpoint
// line in pages. ok is false when no generation window is open.
//
// It reads the PRAGMAs back off the connection rather than echoing what
// BeginGenerationBulkLoad asked for, so it is evidence and not a restatement:
// a window that failed to take the shape, or one a foreign caller has since
// put back, answers differently. That distinction is the whole point — "a
// window is open" and "the write is in the bulk shape" are two claims, and a
// caller a package away can otherwise check only the first.
//
// The read runs under the write gate, so it cannot interleave with a write on
// the same pinned connection.
func (s *Store) GenerationBulkLoadShape() (cacheSize, walAutoCheckpoint int64, ok bool) {
	if s.coreless() {
		return 0, 0, false
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.generationBulkLoad == 0 || s.bulkConn == nil {
		return 0, 0, false
	}
	ctx := context.Background()
	cacheSize, err := pragmaInt(ctx, s.bulkConn, "cache_size")
	if err != nil {
		return 0, 0, false
	}
	walAutoCheckpoint, err = pragmaInt(ctx, s.bulkConn, "wal_autocheckpoint")
	if err != nil {
		return 0, 0, false
	}
	return cacheSize, walAutoCheckpoint, true
}

// generationPayloadEmpty reports whether one payload generation holds no rows.
// Both probes restate `view_gen > 0` literally because that is the predicate
// the generation indexes are partial on — SQLite cannot prove a bound
// parameter is positive, and without the literal both probes degrade to a scan
// of the whole nodes/edges table, which is precisely the cost this window
// exists to avoid paying.
func generationPayloadEmpty(ctx context.Context, conn *sql.Conn, generationID int64) (bool, error) {
	var empty int
	err := conn.QueryRowContext(ctx, `
SELECT NOT EXISTS(SELECT 1 FROM nodes WHERE view_gen > 0 AND view_gen = ?)
   AND NOT EXISTS(SELECT 1 FROM edges WHERE view_gen > 0 AND view_gen = ?)`,
		generationID, generationID).Scan(&empty)
	if err != nil {
		return false, err
	}
	return empty == 1, nil
}

// scheduleWALDrainAboveLine is the finalize residue gate: a bounded follow-up
// TRUNCATE is owed exactly when the WAL a finalize leaves behind is above the
// line SQLite's own automatic checkpoint fires at.
//
// Below the line there is nothing to fix — the frames cost the next writer
// nothing it would not have paid anyway. Above it the daemon is carrying a log
// that the next commit in whatever phase runs next will drain in full, and
// that is the non-determinism measured across two arms of the same workload
// (3,636 frames in one, 15,573 in another): the phase that pays is whichever
// one happens to cross the line, not the phase that wrote the frames.
//
// A disabled auto-checkpoint (0 pages) has no line to exceed, so nothing is
// owed: an operator who turned automatic drains off did not ask for this one.
// A checkpoint that never ran at all reports -1 rather than a count (SQLite
// leaves pnLog/pnCkpt at -1 when it cannot take the checkpointer lock). That is
// not evidence of a residue, so it schedules nothing: the gate acts on a
// measurement or not at all.
func (s *Store) scheduleWALDrainAboveLine(result walCheckpointResult, boundary string) {
	line := sqliteWALAutoCheckpointPages()
	if line <= 0 || result.WALFrames < 0 || result.WALFrames <= line {
		return
	}
	s.scheduleWALDrain(boundary)
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
	if s.generationBulkLoad != 0 {
		// A generation-scoped window belongs to whoever opened it, exactly as
		// the coordinated window above belongs to its outer owner, and for a
		// sharper reason: this flush is not the window's caller at all. An
		// index pass run INSIDE a generation window reaches here at the end of
		// its shadow drain, having found a pinned writer it did not open
		// (beginBulkLoadLocked is a no-op while one is held). Closing it here
		// would take the connection away from its owner mid-payload, leave
		// EndGenerationBulkLoad inert — so the residue gate, the PASSIVE
		// measurement and the off-lane drain would never run for that window —
		// and charge this pass a synchronous TRUNCATE that waits out the live
		// readers of the generations underneath, which is exactly the wait a
		// committed-base publication must not take.
		//
		// Nothing is owed here either way: a generation window defers no dense
		// index (bulkIndexesDeferred is false for it) and its WAL is already
		// bounded inside the payload by noteBulkRowsLocked's row intervals, so
		// leaving it open costs the pass nothing it would otherwise have paid.
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
	var residue walCheckpointResult
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
		// This PASSIVE is where the cold load's non-determinism lives: it is
		// bounded and it explicitly does not wait for the readers a queryable
		// daemon already has, so what it leaves behind is whatever those
		// readers allowed. Hand that number to the residue gate rather than
		// treating "we tried once" as a drained log — the follow-up TRUNCATE
		// runs on the lane, after this window's readers are gone.
		residue = result
	}
	s.jsonbIngestBuffers.release()
	s.writeMu.Unlock()
	s.scheduleWALDrainAboveLine(residue, "cold_load_finalize")
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
	s.noteGenerationBulkRows(nodeRows, edgeRows)
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
		if s.generationBulkLoad != 0 && !generationCheckpointInlinePolicy() {
			s.generationWindowCheckpointLocked("row_limit")
			return nil
		}
		err := s.checkpointBulkWALPassiveLocked("row_limit")
		s.noteBulkRowCheckpointResultLocked(err)
	}
	return nil
}

// generationWindowCheckpointLocked is the checkpoint policy of a generation
// bulk window (see generationInlineCheckpointWindow). The caller holds writeMu
// and the window's pinned writer.
//
// It returns the WAL shape the residue gate needs: the PASSIVE's own counters
// when one ran, the wal-index header's otherwise (mxFrame as the log's frame
// count, nBackfill as the frames already copied) — the same two quantities.
// When the wal-index cannot be read it falls back to a bounded PASSIVE, so the
// gate always acts on a measurement.
//
// A deferred or failed attempt backs the automatic row cadence off for the
// rest of the window: one deferral already proves the backlog outgrew the
// inline budget, and it only grows while the window writes.
func (s *Store) generationWindowCheckpointLocked(boundary string) walCheckpointResult {
	if generationCheckpointInlinePolicy() {
		result, _ := s.checkpointBulkWALPassiveResultLockedWithin(boundary, s.passiveCheckpointWindow())
		return result
	}
	nodeRows, edgeRows := s.bulkCheckpointNodeRows, s.bulkCheckpointEdgeRows
	snap, ok := readWALIndexSnapshot(s.dbPath)
	if !ok || snap.PendingFrames() <= generationInlineCheckpointMaxFrames {
		window := generationInlineCheckpointWindow
		if configured := s.passiveCheckpointWindow(); configured < window {
			window = configured
		}
		result, err := s.checkpointBulkWALPassiveResultLockedWithin(boundary, window)
		if err != nil {
			s.bulkRowCheckpointBackoff = true
		}
		return result
	}
	s.bulkCheckpointNodeRows = 0
	s.bulkCheckpointEdgeRows = 0
	s.bulkRowCheckpointBackoff = true
	result := walCheckpointResult{WALFrames: int(snap.MxFrame), CheckpointedFrames: int(snap.NBackfill)}
	s.emitBulkFinalizeEvent(bulkFinalizeEvent{
		Stage: "checkpoint_deferred", Name: boundary,
		NodeRows: nodeRows, EdgeRows: edgeRows,
		WALFrames: result.WALFrames, CheckpointedFrames: result.CheckpointedFrames,
	})
	return result
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
	s.generationBulkLoad = 0
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
	// A TRUNCATE that completed leaves nothing above any line. One that was
	// deferred (a reader that never left, the lane, a pinned writer) leaves
	// exactly the residue the gate is for, and the frames it reported are the
	// measurement — so the gate is asked on both outcomes and answers on the
	// numbers rather than on the error.
	s.scheduleWALDrainAboveLine(result, "bulk_flush")
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
	_, err := s.checkpointBulkWALPassiveResultLockedWithin(boundary, window)
	return err
}

// checkpointBulkWALPassiveResultLockedWithin is the same bounded attempt with
// its counters returned. The residue gate needs the frame count the drain
// leaves behind, and a checkpoint that reports numbers is the only place that
// count can be read without opening a second connection.
func (s *Store) checkpointBulkWALPassiveResultLockedWithin(boundary string, window time.Duration) (walCheckpointResult, error) {
	if s.bulkConn == nil {
		return walCheckpointResult{}, nil
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
	return result, err
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
