// Package store_sqlite is the on-disk, SQLite-backed implementation of
// graph.Store. It uses the pure-Go modernc.org/sqlite driver so the
// binary stays CGO-free on this code path, and satisfies the same
// conformance suite as the in-memory store (see
// internal/graph/storetest).
//
// Hot queries are precompiled as prepared statements in Open and
// closed in Close. Writes serialize through a single Go-side mutex
// because SQLite already serialises writers internally and an explicit
// mutex sidesteps SQLITE_BUSY contention when the conformance suite
// fans out 8 concurrent writers; reads still run concurrently under
// WAL mode.
//
// Meta maps are encoded as JSON (see meta_json.go); an empty / nil Meta
// is stored as NULL so the common case adds no row weight beyond the
// column header.
//
// EdgeIdentityRevisions is tracked in memory (atomic counter) -- it
// mirrors the in-memory store's monotonic "provenance churn" signal
// and does not need to survive process restarts (the in-memory store
// resets it on every New(), so the contract is per-process).
package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed graph.Store implementation.
type Store struct {
	// db is the bounded, logically read-dedicated pool for on-disk stores.
	// writerDB is a separate read-write pool capped at one physical connection. In-memory
	// stores use the same max-one handle for both because independent
	// :memory: handles would address different databases.
	db       *sql.DB
	writerDB *sql.DB

	// busyRetryTimeout is the whole-transaction contention budget. The zero
	// value selects defaultSQLiteBusyRetryTimeout; tests shorten it to exercise
	// persistent-lock exhaustion deterministically.
	busyRetryTimeout time.Duration

	// passiveCheckpointTimeout bounds one periodic PASSIVE checkpoint. The zero
	// value selects walPassiveCheckpointTimeout; tests shorten it to exercise
	// the writer-pool deadline deterministically.
	passiveCheckpointTimeout time.Duration

	// dbPath is the on-disk SQLite file path, retained for size
	// telemetry — the WAL high-water mark surfaces in daemon_health so a
	// runaway -wal is observable rather than silently filling the disk.
	dbPath string

	// builtinSeen records ::builtin:: sentinel targets already materialised
	// as KindBuiltin stub nodes (see graph.BuiltinStubNodes), so warm
	// re-indexes don't re-upsert identical stubs on every batch.
	builtinSeen sync.Map

	// Structural integrity is owned by this logical store. Shadows forward
	// rejected attempts into the same recorder; warnings are rate-limited per
	// Store so independent workspaces never suppress each other's diagnostics.
	structuralIntegrity   graph.StructuralIntegrityMeter
	structuralWriteWarned atomic.Bool
	structuralReadWarned  atomic.Bool

	// preparedSQL registers every statement prepared at Open so the plan
	// fence can EXPLAIN the entire prepared surface against a fixture and
	// reject big-table scans mechanically.
	preparedSQL []string

	// writeMu serialises every mutation. SQLite serialises writers
	// internally; doing the same on the Go side turns SQLITE_BUSY
	// contention into clean lock-wait and keeps the conformance
	// concurrency test predictable.
	writeMu sqliteWriteGate

	// mutationReceipts is guarded only by writeMu, making Begin/End atomic
	// with every durable graph write without another lock-ordering edge.
	mutationReceipts sqliteMutationReceiptState

	// resolveMu is the resolver-coordination mutex returned by
	// ResolveMutex. Held by cross-repo / temporal / external resolver
	// passes to keep their edge mutations from interleaving. Separate
	// from writeMu so the resolver can hold it across multiple writes
	// without blocking unrelated steady-state mutations.
	resolveMu sync.Mutex

	edgeIdentityRevs atomic.Int64
	// edgeMutationRevision is a coarse monotonic generation for every durable
	// edge payload/topology mutation, including same-key replacements. Resolver
	// liveness snapshots use it to reject stale work after watcher interleaves.
	edgeMutationRevision atomic.Uint64

	// analysisMutationRevision closes the in-process race between loading or
	// computing a persisted whole-graph analysis and a concurrent graph write.
	// analysisGenerationPresent is guarded by writeMu and avoids a redundant cache
	// DELETE on every row after the first fail-closed invalidation.
	analysisMutationRevision  atomic.Uint64
	analysisGenerationPresent bool

	// wiped records that Open dropped an incompatible on-disk DB and
	// recreated it empty (a schema-version mismatch that an in-place ALTER
	// could not satisfy). Surfaced via NeedsRebuild so the daemon forces a
	// full re-index on warm restart instead of an incremental reconcile,
	// rather than relying on the side effect that a total wipe also empties
	// file_mtimes.
	wiped bool

	// WAL-checkpoint loop lifecycle. In WAL mode a COMMIT only appends to
	// the -wal file; pages move into the main DB (and the WAL becomes
	// reusable) at a checkpoint. SQLite's default passive auto-checkpoint
	// reuses the WAL in place and never shrinks the file, so under steady
	// writes with ever-present readers (the pooled connections here, plus
	// any other process holding the store open) the -wal ratchets up to a
	// large high-water mark and stays there. runCheckpointLoop periodically
	// runs `PRAGMA wal_checkpoint(TRUNCATE)` to drain the log into the DB
	// and shrink the file back down. nil for in-memory stores (no WAL).
	stopCheckpoint chan struct{} // closed by Close to stop the loop
	checkpointDone chan struct{} // closed by the loop when it returns
	stopOnce       sync.Once     // makes stopCheckpointLoop idempotent

	// bundles is the content-addressed package-scoped cache over
	// SearchSymbolBundles: a query serves cached Node + in/out edges for
	// packages whose content fingerprint is unchanged and skips the node
	// + edge fan-out for them. nil until SetBundleFingerprints is first
	// called (the daemon wires it from the analysis pass); a nil cache
	// makes SearchSymbolBundles fall through to the uncached path.
	bundles *bundleCache

	// Bulk-load fast path (graph.BulkLoader). Non-nil only between
	// BeginBulkLoad and FlushBulk, and only on a first/empty cold index.
	// database/sql PRAGMAs are connection-local, so the fast path pins one
	// connection (bulkConn) carrying synchronous=OFF, wal_autocheckpoint=0,
	// and an enlarged page cache and routes every bulk write through it;
	// bulkPrev* hold the values FlushBulk restores before the connection
	// returns to the pool. coordinatedBulkLoad is true while a
	// multi-repository cold parse owns the outer load window. Dense indexes are
	// sealed once at bounded row counts (or the outer final boundary), while
	// the pinned durability/FTS window stays open. All fields are guarded
	// by writeMu.
	bulkConn               *sql.Conn
	bulkPrevSync           int64
	bulkPrevCacheSize      int64
	bulkPrevAutoCheckpoint int64
	coordinatedBulkLoad    bool
	bulkIndexesDeferred    bool
	bulkDeferredNodeRows   int64
	bulkDeferredEdgeRows   int64
	bulkCheckpointNodeRows int64
	bulkCheckpointEdgeRows int64
	// bulkRowCheckpointBackoff suppresses only automatic row-cadence attempts
	// after contention makes one bounded checkpoint unproductive. Explicit
	// seal/planner/final checkpoints remain active. Reset at bulk begin/close.
	bulkRowCheckpointBackoff bool
	// These flags mean "bounded FTS maintenance requested" during a
	// coordinated cold load. The historical names are retained to keep the
	// cancellation/Close path stable; normal cold finalization never runs a
	// full optimize.
	deferredFTSOptimize bool
	deferredContentFTS  bool

	// batchVariableLimit is the runtime SQLITE_LIMIT_VARIABLE_NUMBER observed
	// on the active writer connection, capped by the bounded statement policy.
	// It is guarded by writeMu. A variable-limit execution failure lowers the
	// cached value so later batches do not repeat an oversized prepare.
	batchVariableLimit int

	// jsonbIngestBuffers reuses the bounded JSONB and metadata arenas across
	// AddBatch calls. It is guarded by writeMu and trimmed after every batch so
	// one exceptional first row cannot become retained heap.
	jsonbIngestBuffers jsonbIngestBuffers

	// bulkFinalizeObserver is a package-private test/diagnostic hook. It runs
	// synchronously under writeMu and therefore must not call back into Store.
	bulkFinalizeObserver func(bulkFinalizeEvent)

	// Prepared statements (compiled once in Open, closed in Close).
	stmtInsertNode         *sql.Stmt
	stmtGetNode            *sql.Stmt
	stmtGetNodeByQual      *sql.Stmt
	stmtFindByName         *sql.Stmt
	stmtFindByNameInRepo   *sql.Stmt
	stmtFileNodes          *sql.Stmt
	stmtRepoNodes          *sql.Stmt
	stmtAllNodes           *sql.Stmt
	stmtNodeCount          *sql.Stmt
	stmtRepoPrefixes       *sql.Stmt
	stmtRepoStatsNodes     *sql.Stmt
	stmtRepoStatsEdges     *sql.Stmt
	stmtRepoNodeCount      *sql.Stmt
	stmtRepoEdgeCount      *sql.Stmt
	stmtAllRepoCountsNodes *sql.Stmt
	stmtAllRepoCountsEdges *sql.Stmt
	stmtAllRepoStateCounts *sql.Stmt
	stmtStatsByKind        *sql.Stmt
	stmtStatsByLanguage    *sql.Stmt

	stmtInsertEdge       *sql.Stmt
	unresolvedInserts    atomic.Uint64
	stmtOutEdges         *sql.Stmt
	stmtInEdges          *sql.Stmt
	stmtRepoEdges        *sql.Stmt
	stmtAllEdges         *sql.Stmt
	stmtEdgeCount        *sql.Stmt
	stmtRemoveEdge       *sql.Stmt
	stmtUpdateEdgeOrigin *sql.Stmt
	stmtUpdateEdgeAttrs  *sql.Stmt
	stmtSelectEdgeOrigin *sql.Stmt
	stmtDeleteEdgeByKey  *sql.Stmt
	stmtEdgeExists       *sql.Stmt
}

// Compile-time assertion: *Store satisfies graph.Store.
var _ graph.Store = (*Store)(nil)

// The audit path behind `daemon status --exact`. Asserted here because the
// controller reaches it by optional-interface type assertion: if this store
// stopped satisfying it, --exact would quietly fall back to the counters it
// exists to check rather than failing to compile.
var _ graph.RepoMemoryEstimateScanner = (*Store)(nil)

// ResolveMutex returns the resolver-coordination mutex. Held by
// cross-repo / temporal / external resolver passes to serialise edge
// mutations. Separate from writeMu (which protects per-statement
// write serialisation against SQLITE_BUSY) so the resolver can hold
// it across multi-write batches without blocking unrelated steady-
// state mutations on the same store.
func (s *Store) ResolveMutex() *sync.Mutex { return &s.resolveMu }

// NeedsRebuild reports that Open dropped an incompatible on-disk database and
// recreated it empty, so the daemon's warm-restart path should force a full
// re-index (bypassing an incremental reconcile that would carry stale state)
// — see cmd/gortex.storeNeedsRebuild, the capability probe this satisfies.
func (s *Store) NeedsRebuild() bool { return s.wiped }

// Open opens (or creates) the SQLite database at path, runs the schema
// migration, and prepares hot statements. The DB is opened with WAL
// journaling and synchronous=NORMAL -- the same durability/throughput
// tradeoff every embedded-SQLite app uses for write-heavy workloads.
//
// Pass ":memory:" for an ephemeral in-process database (handy for
// tests when you don't need on-disk persistence).
//
// By default Open will NOT destroy an incompatible on-disk database: if the
// stored schema version requires a rebuild (a newer build's DB, or an older
// one crossing a rebuild migration) it returns ErrSchemaRebuildRequired and
// leaves the file untouched. Pass WithRebuild to permit the drop-and-recreate
// — only a caller that holds exclusive access to the store may do so (see
// WithRebuild).
func Open(path string, opts ...Option) (*Store, error) {
	var o openOptions
	for _, opt := range opts {
		opt(&o)
	}
	return openWith(path, currentSchemaVersion, schemaMigrations, o.allowRebuild)
}

// Option configures Open.
type Option func(*openOptions)

type openOptions struct {
	allowRebuild bool
}

// WithRebuild permits Open to drop and recreate an on-disk database whose
// schema version is incompatible (a newer build's, or an older one crossing a
// migration that an in-place ALTER cannot satisfy).
//
// The caller MUST hold exclusive cross-process access to the store file —
// removing a SQLite file another process has open silently splits its state.
// The daemon satisfies this: it takes an exclusive flock on <store>.lock for
// the writable on-disk sqlite lifecycle and passes this option only in that
// branch (see serverstack.NewSharedServer / OpenBackend). Without it, a wipe
// plan yields ErrSchemaRebuildRequired and the file is left intact, so a
// caller that does not hold the lock cannot corrupt a live store.
func WithRebuild() Option { return func(o *openOptions) { o.allowRebuild = true } }

// ErrSchemaRebuildRequired is returned by Open when an on-disk database needs a
// destructive rebuild but the caller did not pass WithRebuild (i.e. cannot
// prove it holds the store lock).
var ErrSchemaRebuildRequired = errors.New("store_sqlite: on-disk schema is incompatible and must be rebuilt; reopen with WithRebuild while holding the store lock")

// openWith is Open parameterised by the target schema version, migration
// registry, and rebuild permission so tests can drive the baseline / in-place
// / rebuild arms without mutating package globals. Open passes the package
// defaults (currentSchemaVersion, schemaMigrations) and the WithRebuild flag.
const (
	// Each modernc SQLite connection can map up to sqliteMmapSizeBytes and
	// grow a separate page cache. Bounding the pool prevents a read burst on
	// a high-core machine from multiplying clean file mappings into several
	// GiB of resident address space. Four readers retained full-scan
	// throughput in the pool benchmark while cutting the 16-reader peak by
	// roughly 75%.
	sqliteMaxOpenConns = 4
	sqliteMaxIdleConns = 1
)

func configureConnectionPool(db *sql.DB) {
	maxOpen := runtime.NumCPU()
	if maxOpen > sqliteMaxOpenConns {
		maxOpen = sqliteMaxOpenConns
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(sqliteMaxIdleConns)
}

func openWith(path string, current int, migrations []schemaMigration, allowRebuild bool) (*Store, error) {
	// Pragmas: WAL + synchronous=NORMAL is the standard write-heavy
	// embedded tradeoff. cache_size(-32768) gives each pooled connection a
	// 32 MiB page cache; temp_store(MEMORY) keeps GROUP BY / ORDER BY scratch
	// off disk; mmap_size(256 MiB) lets reads fault pages straight from the
	// OS page cache instead of copying through SQLite's. These materially
	// speed the resolver/query phases on a large graph.
	//
	// journal_size_limit(64 MiB) caps the -wal high-water mark: after any
	// checkpoint SQLite truncates the WAL back down to this size instead of
	// leaving it at whatever it grew to. Without it the WAL only ratchets
	// up (a passive checkpoint reuses the file in place, never shrinking
	// it), which is how a 535 MB DB ends up with an 11 GB -wal. This bounds
	// the file even between the explicit TRUNCATE checkpoints runCheckpointLoop
	// issues, and even if that loop is not running.
	writerDSN := sqliteWriterDSN(path)
	db, err := sql.Open("sqlite", writerDSN)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	// The canonical initialization/runtime writer handle owns exactly one
	// physical connection. This matches SQLite's single-writer model and keeps
	// a writer slot available even when every query-pool connection is busy.
	// A separate bounded query pool is opened after schema reconciliation.
	configureWriterPool(db)

	// Reconcile the on-disk schema version before applying schemaSQL. The graph
	// store is a rebuildable cache, so an incompatible (older needing a rebuild
	// step, or newer) DB is dropped and reindexed rather than migrated in place
	// (see schema_version.go). The daemon holds an exclusive store.lock around
	// Open, so wiping the file here cannot race another process.
	stored, err := readUserVersion(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite read schema version: %w", err)
	}
	plan := planSchemaMigrationWith(stored, current, migrations)
	// A rebuild migration applies to an existing pre-versioning database, but
	// not to the brand-new empty file sql.Open just created. Distinguish those
	// two user_version=0 cases before requiring destructive-rebuild authority.
	// An existing nodes/edges schema may already contain derived topology and
	// must take the conservative rebuild path even when its current row count is
	// zero; absence of both tables is the only safe fresh-store proof.
	if stored == 0 && plan.wipe && !isMemoryPath(path) {
		existing, probeErr := hasGraphStoreTables(db)
		if probeErr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite probe existing graph schema: %w", probeErr)
		}
		if !existing {
			plan = schemaPlan{stamp: true}
		}
	}
	didWipe := false
	if plan.wipe && !isMemoryPath(path) {
		// Refuse the destructive rebuild unless the caller proved it holds
		// exclusive access (WithRebuild). This keeps the file safe even if a
		// future caller reaches a wipe plan without the daemon's store lock.
		if !allowRebuild {
			_ = db.Close()
			return nil, ErrSchemaRebuildRequired
		}
		if err := db.Close(); err != nil {
			return nil, fmt.Errorf("sqlite close for rebuild: %w", err)
		}
		if err := removeStoreFiles(path); err != nil {
			return nil, fmt.Errorf("sqlite rebuild: %w", err)
		}
		db, err = sql.Open("sqlite", writerDSN)
		if err != nil {
			return nil, fmt.Errorf("sqlite reopen for rebuild: %w", err)
		}
		configureWriterPool(db)
		didWipe = true
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite schema: %w", err)
	}
	// Add the promoted node columns to databases created before they
	// existed (CREATE TABLE IF NOT EXISTS won't alter an existing table).
	// Must run before prepare(), whose node INSERT references the promoted
	// columns too.
	if err := ensureNodeColumns(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite node columns: %w", err)
	}
	// clone_shingles gained the compact finalized signature/token projection
	// in schema v5. CREATE TABLE IF NOT EXISTS cannot add those columns to a
	// dirty pre-release v5 store, so reconcile them explicitly before any
	// prepared statement or clone pass can touch the sidecar.
	if err := ensureCloneCorpusColumns(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite clone corpus columns: %w", err)
	}
	// nodes.is_stub generated column — see ensureNodeGeneratedColumns for why
	// this is a separate function from ensureNodeColumns above.
	if err := ensureNodeGeneratedColumns(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite node generated columns: %w", err)
	}
	// Same treatment for the edges table's is_unresolved generated column —
	// must run before the droppable-index loop below, which creates an index
	// over it.
	if err := ensureEdgeColumns(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite edge columns: %w", err)
	}
	// Create the droppable secondary indexes from the shared set so their
	// initial-creation DDL is byte-identical to the DDL the bulk-load fast
	// path rebuilds them with (BeginBulkLoad drops these, FlushBulk
	// recreates them — see bulk_load.go). Kept out of schemaSQL so the two
	// sites cannot drift.
	for _, idx := range bulkDroppableIndexes {
		if _, err := db.Exec(idx.ddl); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite create index %s: %w", idx.name, err)
		}
	}
	// Sparse partial indexes remain live through a cold load, but share the
	// same explicit DDL ownership rather than drifting into schemaSQL.
	for _, idx := range bulkAlwaysLiveIndexes {
		if _, err := db.Exec(idx.ddl); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite create index %s: %w", idx.name, err)
		}
	}
	// edges_external is a partial index over exactly the external-call
	// terminals, so ExternalCallCandidateEdges scans a tiny index instead
	// of the full edges table. Built from the shared predicate const (not
	// inlined in schemaSQL) so the index WHERE and the query WHERE stay
	// byte-identical — SQLite only uses a partial index when the query's
	// WHERE matches the index's.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS edges_external ON edges(kind) WHERE ` + externalCallTargetPredicate); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite edges_external index: %w", err)
	}
	// Backfill the FTS rowid sidecar for databases built before it existed,
	// so the first incremental UpsertSymbolFTS on an already-indexed symbol
	// can do its O(log n) docid delete instead of leaking a duplicate row.
	// One-time; a no-op once the map is populated or the FTS index is empty.
	if err := backfillSymbolFTSRowidMap(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite fts rowid backfill: %w", err)
	}
	// Same one-time compatibility bridge for content rows written before the
	// indexed ownership sidecar existed. Steady-state opens observe a non-empty
	// map and skip the virtual-table scan.
	if err := backfillContentFTSRowidMap(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite content fts rowid backfill: %w", err)
	}

	// Apply any in-place migration steps, then stamp the current schema version.
	// Fresh and pre-versioning (stored==0) stores run the in-place steps too —
	// they are idempotent and no-op on an empty or already-clean store — so the
	// first in-place migration ships without forcing every non-daemon Open to
	// pass WithRebuild. A wipe plan carries no in-place steps, and after a wipe
	// the store is empty and the daemon's normal indexing repopulates it.
	if plan.stamp {
		if err := applyInPlaceMigrations(db, plan.inPlace); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite schema migrate: %w", err)
		}
		if err := setUserVersion(db, current); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite stamp schema version: %w", err)
		}
	}
	// The repository index references columns introduced by the v10 vector
	// migration, so create it only after pending migrations have rebuilt the
	// legacy table. On current and fresh stores this is an idempotent no-op.
	if _, err := db.Exec(vectorRepoIndexSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite vector repository index: %w", err)
	}
	// A schema transition invalidates any generation produced against the old
	// graph shape. The v4 migration also drops the unreleased blob-only table.
	if stored != current {
		if _, err := db.Exec(`UPDATE analysis_generations SET state = ? WHERE generation_id IN (SELECT generation_id FROM analysis_active_generation)`, analysisGenerationStale); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite stale analysis generation after migration: %w", err)
		}
		if _, err := db.Exec(`DELETE FROM analysis_active_generation`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite clear active analysis generation after migration: %w", err)
		}
	}

	readDB := db
	if !isMemoryPath(path) {
		readDB, err = openSQLiteReadPool(path)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite open read pool: %w", err)
		}
	}

	s := &Store{db: readDB, writerDB: db, dbPath: path, wiped: didWipe}
	// Initialise the bundle cache at construction so its pointer is
	// never written after Open — concurrent SearchSymbolBundles reads
	// and SetBundleFingerprints writes then race only on the cache's
	// own mutex-guarded maps, not on the Store field. The cache stays
	// inert (every lookup a miss) until the daemon supplies fingerprints.
	s.bundles = newBundleCache()
	if err := s.initAnalysisGenerationState(); err != nil {
		_ = closeSQLitePools(readDB, db)
		return nil, fmt.Errorf("sqlite analysis generation state: %w", err)
	}
	if err := s.prepare(); err != nil {
		_ = closeSQLitePools(readDB, db)
		return nil, fmt.Errorf("sqlite prepare: %w", err)
	}
	// A populated store opened without planner statistics would plan blind
	// until its next cold bulk load; backfill sqlite_stat1 once here.
	healPlannerStats(db)
	// In-memory databases have no WAL file to drain, so the periodic
	// checkpoint is pointless there (and would leak a goroutine per
	// short-lived test store). Only run it for on-disk stores.
	if !strings.Contains(path, ":memory:") {
		s.stopCheckpoint = make(chan struct{})
		s.checkpointDone = make(chan struct{})
		go s.runCheckpointLoop(walCheckpointInterval)
	}
	return s, nil
}

func hasGraphStoreTables(db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('nodes','edges')`).Scan(&count)
	return count > 0, err
}

// walCheckpointInterval is how often runCheckpointLoop passively drains WAL
// frames into the main database. PASSIVE never waits for long readers and does
// not shrink the file; explicit/final checkpoints use TRUNCATE instead.
const (
	walCheckpointInterval = 5 * time.Minute
	// walCheckpointTimeout bounds explicit/final pool acquisition, contention
	// retry, and SQLite execution. A caller gets an error instead of an
	// unbounded shutdown or operator-command wait.
	walCheckpointTimeout = 10 * time.Second
	// The periodic path is best-effort and one-shot. Its gate acquisition is a
	// non-blocking TryLock; this context only protects an unexpected writer-pool
	// wait or a slow driver call.
	walPassiveCheckpointTimeout = 1 * time.Second
)

var errWALCheckpointDeferredBulk = errors.New("store_sqlite: WAL checkpoint deferred while bulk writer is pinned")

type walCheckpointResult struct {
	Busy               int
	WALFrames          int
	CheckpointedFrames int
}

func (r walCheckpointResult) incomplete() bool {
	return r.Busy != 0 || r.CheckpointedFrames < r.WALFrames
}

// runCheckpointLoop attempts one non-blocking PASSIVE checkpoint per interval.
// It never queues behind graph mutation or a pinned bulk writer. An incomplete
// reader-limited checkpoint is logged with SQLite's counters and retried at the
// next interval; it is never reported as a completed drain.
func (s *Store) runCheckpointLoop(interval time.Duration) {
	defer close(s.checkpointDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCheckpoint:
			return
		case <-ticker.C:
			s.checkpointWALPassive()
		}
	}
}

func (s *Store) passiveCheckpointWindow() time.Duration {
	if s.passiveCheckpointTimeout > 0 {
		return s.passiveCheckpointTimeout
	}
	return walPassiveCheckpointTimeout
}

func (s *Store) checkpointWALPassive() {
	if !s.writeMu.TryLock() {
		log.Printf("store_sqlite: wal checkpoint deferred mode=PASSIVE reason=writer_gate")
		return
	}
	defer s.writeMu.Unlock()
	if s.bulkConn != nil {
		log.Printf("store_sqlite: wal checkpoint deferred mode=PASSIVE reason=bulk_writer")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.passiveCheckpointWindow())
	defer cancel()

	// Acquire the writer connection separately from running the PRAGMA. A
	// single QueryRowContext folds pool acquisition, execution and scanning
	// into one call, so its deadline cannot tell "never started" from "was
	// running". Only a failure to acquire proves the checkpoint never ran, and
	// that is the case worth reporting as a deferral rather than a result.
	conn, err := s.writerDB.Conn(ctx)
	if err != nil {
		log.Printf("store_sqlite: wal checkpoint deferred mode=PASSIVE reason=writer_busy error=%q", err)
		return
	}
	defer func() { _ = conn.Close() }()

	result, err := checkpointWALOnceOn(ctx, conn, "PASSIVE")
	if err == nil {
		return
	}
	log.Print(passiveCheckpointReport(result, err, ctx.Err()))
}

// passiveCheckpointReport renders the log line for a periodic checkpoint that
// executed and failed. Split out from checkpointWALPassive because the
// ordering it encodes is the whole point and racing a real deadline cannot
// pin it: a checkpoint that returned counters and then blew its window must
// be reported by its counters, not by the deadline.
func passiveCheckpointReport(result walCheckpointResult, err, ctxErr error) string {
	// Measured counters outrank the window. An incomplete checkpoint DID
	// execute and SQLite reported real numbers; that stays a warning even if
	// the window expired right after, because the measurement is trustworthy
	// and the deadline is not what the operator needs to know.
	if errors.Is(err, errSQLiteCheckpointIncomplete) {
		return fmt.Sprintf("store_sqlite: wal checkpoint incomplete mode=PASSIVE busy=%d wal_frames=%d checkpointed_frames=%d error=%q",
			result.Busy, result.WALFrames, result.CheckpointedFrames, err)
	}

	// The PRAGMA started but did not return in the window. Unlike a failure to
	// acquire the writer, it may well have drained frames, so this is not a
	// deferral — and result holds nothing measured, so it must not be printed
	// as counters either.
	if errors.Is(err, context.DeadlineExceeded) || ctxErr != nil {
		return fmt.Sprintf("store_sqlite: wal checkpoint timed out mode=PASSIVE phase=execute error=%q", err)
	}

	// Anything else is a driver/SQLite failure with no usable counters.
	return fmt.Sprintf("store_sqlite: wal checkpoint failed mode=PASSIVE error=%q", err)
}

// CheckpointWAL runs `PRAGMA wal_checkpoint(TRUNCATE)`: it flushes the
// write-ahead log into the main database file and shrinks the -wal back to
// zero. It is the explicit/final maintenance boundary; the timer uses PASSIVE.
// Acquisition and incomplete-checkpoint retries are context bounded and
// serialized with the sole SQLite writer.
func (s *Store) CheckpointWAL() error {
	ctx, cancel := context.WithTimeout(context.Background(), walCheckpointTimeout)
	defer cancel()
	return s.checkpointWALWithContext(ctx)
}

func (s *Store) checkpointWALWithContext(ctx context.Context) error {
	_, err := s.checkpointWALWithContextResult(ctx)
	return err
}

func (s *Store) checkpointWALWithContextResult(ctx context.Context) (walCheckpointResult, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return walCheckpointResult{}, err
	}
	defer s.writeMu.Unlock()
	if s.bulkConn != nil {
		return walCheckpointResult{}, errWALCheckpointDeferredBulk
	}
	return s.checkpointWALResult(ctx)
}

// checkpointWAL is retained as the error-only core used by focused tests. The
// caller holds writeMu; checkpointWALWithContextResult is the public-path gate.
func (s *Store) checkpointWAL(ctx context.Context) error {
	_, err := s.checkpointWALResult(ctx)
	return err
}

func (s *Store) checkpointWALResult(ctx context.Context) (walCheckpointResult, error) {
	var result walCheckpointResult
	err := s.withSQLiteBusyRetry(ctx, "wal_checkpoint_truncate", func(attemptCtx context.Context) error {
		var err error
		result, err = s.checkpointWALOnce(attemptCtx, "TRUNCATE")
		return err
	})
	return result, err
}

// walCheckpointQueryer is satisfied by both *sql.DB and *sql.Conn, so a caller
// that needs to separate connection acquisition from checkpoint execution can
// run the PRAGMA on a connection it already holds.
type walCheckpointQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) checkpointWALOnce(ctx context.Context, mode string) (walCheckpointResult, error) {
	return checkpointWALOnceOn(ctx, s.writerDB, mode)
}

func checkpointWALOnceOn(ctx context.Context, q walCheckpointQueryer, mode string) (walCheckpointResult, error) {
	var result walCheckpointResult
	err := q.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").Scan(
		&result.Busy,
		&result.WALFrames,
		&result.CheckpointedFrames,
	)
	if err != nil {
		return result, err
	}
	if result.incomplete() {
		return result, fmt.Errorf("%w: mode=%s busy=%d wal_frames=%d checkpointed_frames=%d", errSQLiteCheckpointIncomplete, mode, result.Busy, result.WALFrames, result.CheckpointedFrames)
	}
	return result, nil
}

// stopCheckpointLoop signals the background loop to exit and waits for it,
// so callers can be sure no checkpoint is in flight before closing s.db.
// Idempotent: safe to call from Close more than once.
func (s *Store) stopCheckpointLoop() {
	s.stopOnce.Do(func() {
		if s.stopCheckpoint != nil {
			close(s.stopCheckpoint)
			<-s.checkpointDone
		}
	})
}

// Close closes every prepared statement and the underlying *sql.DB. It
// first stops the WAL-checkpoint loop and issues one final TRUNCATE
// checkpoint so the -wal file is drained and shrunk on graceful shutdown
// rather than lingering at its high-water mark until the next open.
func (s *Store) Close() error {
	s.stopCheckpointLoop()
	// A caller normally ends an outer cold-load window explicitly, but Close is
	// also the last durability boundary on cancellation or startup failure.
	// Flush while the database and pinned connection are still live so a
	// coordinated load can never be silently discarded.
	s.writeMu.Lock()
	var bulkErr error
	hadBulk := s.bulkConn != nil
	if hadBulk {
		if s.deferredFTSOptimize {
			_, _ = s.execActiveWriteLocked(context.Background(), `INSERT INTO symbol_fts(symbol_fts, rank) VALUES('merge', ?)`, coldFTSMergePages)
		}
		if s.deferredContentFTS {
			_, _ = s.execActiveWriteLocked(context.Background(), `INSERT INTO content_fts(content_fts, rank) VALUES('merge', ?)`, coldFTSMergePages)
		}
		s.deferredFTSOptimize = false
		s.deferredContentFTS = false
		s.coordinatedBulkLoad = false
		sealErr := s.sealBulkIndexesLocked("close")
		bulkErr = errors.Join(sealErr, s.closeBulkConnectionLocked())
	}
	s.writeMu.Unlock()

	var checkpointErr error
	if s.checkpointDone != nil { // on-disk store: drain the WAL one last time
		if hadBulk {
			checkpointErr = s.checkpointBulkWAL()
		} else {
			checkpointErr = s.CheckpointWAL()
		}
	}
	stmts := []*sql.Stmt{
		s.stmtInsertNode, s.stmtGetNode, s.stmtGetNodeByQual,
		s.stmtFindByName, s.stmtFindByNameInRepo,
		s.stmtFileNodes, s.stmtRepoNodes,
		s.stmtAllNodes, s.stmtNodeCount, s.stmtRepoPrefixes,
		s.stmtRepoStatsNodes, s.stmtRepoStatsEdges,
		s.stmtRepoNodeCount, s.stmtRepoEdgeCount,
		s.stmtAllRepoCountsNodes, s.stmtAllRepoCountsEdges,
		s.stmtAllRepoStateCounts,
		s.stmtStatsByKind, s.stmtStatsByLanguage,
		s.stmtInsertEdge, s.stmtOutEdges, s.stmtInEdges,
		s.stmtRepoEdges,
		s.stmtAllEdges, s.stmtEdgeCount, s.stmtRemoveEdge,
		s.stmtUpdateEdgeOrigin, s.stmtUpdateEdgeAttrs, s.stmtSelectEdgeOrigin, s.stmtDeleteEdgeByKey,
		s.stmtEdgeExists,
	}
	for _, st := range stmts {
		if st != nil {
			_ = st.Close()
		}
	}
	return errors.Join(bulkErr, checkpointErr, closeSQLitePools(s.db, s.writerDB))
}

func (s *Store) prepare() error {
	var err error
	prepOn := func(db *sql.DB, out **sql.Stmt, q string) {
		if err != nil {
			return
		}
		var st *sql.Stmt
		st, err = db.Prepare(q)
		if err != nil {
			err = fmt.Errorf("prepare %q: %w", q, err)
			return
		}
		*out = st
	}
	// Every prepared statement is also recorded so the plan fence
	// (TestPreparedStatementPlansNeverScanBigTables) can EXPLAIN the whole
	// prepared surface mechanically. Partial-index misuse against bound
	// parameters slipped through review three independent times as a
	// "convention"; the registry turns the convention into a gate that
	// covers every future statement automatically.
	prep := func(out **sql.Stmt, q string) {
		s.preparedSQL = append(s.preparedSQL, q)
		prepOn(s.db, out, q)
	}
	prepWrite := func(out **sql.Stmt, q string) {
		s.preparedSQL = append(s.preparedSQL, q)
		prepOn(s.writerDB, out, q)
	}

	const nodeCols = nodeInsertColumns

	// Never use INSERT OR REPLACE here. SQLite implements REPLACE as
	// DELETE+INSERT; the DELETE fires the nodes->edges ON DELETE CASCADE and
	// silently erases every incident edge when a caller only intends to update
	// node metadata (reach.Lookup does exactly that when publishing its cache).
	// A true UPSERT updates the existing row in place and therefore preserves
	// graph topology.
	prepWrite(&s.stmtInsertNode,
		`INSERT INTO nodes (`+nodeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`+nodeUpsertClause)
	prep(&s.stmtGetNode,
		`SELECT `+nodeCols+` FROM nodes WHERE id = ?`)
	// The literal qual_name <> '' conjunct is what makes the partial
	// nodes_by_qual index usable: SQLite cannot prove a bound parameter is
	// non-empty, so without it this statement is a full node scan per call
	// on resolver hot paths (measured against a production store). Every
	// reader of a partial index must restate its predicate literally.
	prep(&s.stmtGetNodeByQual,
		`SELECT `+nodeCols+` FROM nodes WHERE qual_name = ? AND qual_name <> '' ORDER BY id LIMIT 1`)
	prep(&s.stmtFindByName,
		`SELECT `+nodeCols+` FROM nodes WHERE name = ?`)
	prep(&s.stmtFindByNameInRepo,
		`SELECT `+nodeCols+` FROM nodes WHERE name = ? AND repo_prefix = ?`)
	prep(&s.stmtFileNodes,
		`SELECT `+nodeCols+` FROM nodes WHERE file_path = ?`)
	prep(&s.stmtRepoNodes,
		`SELECT `+nodeCols+` FROM nodes WHERE repo_prefix = ?`)
	// ORDER BY id: nodes is WITHOUT ROWID keyed on id, so the scan already
	// walks primary-key order and the clause costs nothing (no temp b-tree in
	// the query plan). Stating it makes whole-graph enumeration reproducible
	// instead of merely happening to be stable.
	prep(&s.stmtAllNodes,
		`SELECT `+nodeCols+` FROM nodes ORDER BY id`)
	prep(&s.stmtNodeCount,
		`SELECT COUNT(*) FROM nodes`)
	prep(&s.stmtRepoPrefixes,
		`SELECT DISTINCT repo_prefix FROM nodes WHERE repo_prefix <> ''`)

	prep(&s.stmtRepoStatsNodes,
		`SELECT repo_prefix, kind, language, COUNT(*) FROM nodes WHERE repo_prefix <> '' GROUP BY repo_prefix, kind, language`)
	prep(&s.stmtRepoStatsEdges,
		`SELECT n.repo_prefix, COUNT(*)
		 FROM edges e
		 JOIN nodes n ON n.id = e.from_id
		 WHERE n.repo_prefix <> ''
		 GROUP BY n.repo_prefix`)
	prep(&s.stmtRepoNodeCount,
		`SELECT COUNT(*) FROM nodes WHERE repo_prefix = ?`)
	prep(&s.stmtRepoEdgeCount,
		`SELECT COUNT(*)
		 FROM edges e
		 JOIN nodes n ON n.id = e.from_id
		 WHERE n.repo_prefix = ?`)
	prep(&s.stmtAllRepoCountsNodes,
		`SELECT repo_prefix, COUNT(*) FROM nodes WHERE repo_prefix <> '' GROUP BY repo_prefix`)
	prep(&s.stmtAllRepoCountsEdges,
		`SELECT n.repo_prefix, COUNT(*)
		 FROM edges e
		 JOIN nodes n ON n.id = e.from_id
		 WHERE n.repo_prefix <> ''
		 GROUP BY n.repo_prefix`)
	// The counters the indexer persists on the way in. repo_index_state is
	// WITHOUT ROWID, so its primary key IS the table: this reads one row per
	// tracked repo with no scan of nodes or edges.
	prep(&s.stmtAllRepoStateCounts,
		`SELECT repo_prefix, node_count, edge_count FROM repo_index_state`)

	prep(&s.stmtStatsByKind,
		`SELECT kind, COUNT(*) FROM nodes GROUP BY kind`)
	prep(&s.stmtStatsByLanguage,
		`SELECT language, COUNT(*) FROM nodes GROUP BY language`)

	const edgeCols = edgeInsertColumns

	prepWrite(&s.stmtInsertEdge,
		`INSERT OR IGNORE INTO edges (`+edgeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	// The adjacency reads carry an explicit total order. Several callers take
	// the FIRST edge matching a predicate (the semantic enricher's
	// matching-edge lookup, for one), so several edges sharing
	// (from, to, kind) and differing only by source line used to make the
	// choice depend on undefined row order.
	//
	// Both clauses are free: they lead with the column the chosen index
	// already sorts on (edges_by_from_line for from_id, edges_by_to for
	// to_id) and break ties on the rowid every index carries as its trailing
	// key, so the planner satisfies them from the index walk with no temp
	// b-tree.
	prep(&s.stmtOutEdges,
		`SELECT `+edgeCols+` FROM edges WHERE from_id = ? ORDER BY line, id`)
	prep(&s.stmtInEdges,
		`SELECT `+edgeCols+` FROM edges WHERE to_id = ? ORDER BY kind, id`)
	prep(&s.stmtRepoEdges,
		`SELECT `+lookupQualifiedEdgeCols+`
		   FROM edges e
		   JOIN nodes n ON n.id = e.from_id
		  WHERE n.repo_prefix = ?`)
	// id is the rowid alias, so the full scan already yields insertion order
	// and the clause adds no sorter — same reasoning as stmtAllNodes above.
	prep(&s.stmtAllEdges,
		`SELECT `+edgeCols+` FROM edges ORDER BY id`)
	prep(&s.stmtEdgeCount,
		`SELECT COUNT(*) FROM edges`)
	prepWrite(&s.stmtRemoveEdge,
		`DELETE FROM edges WHERE from_id = ? AND to_id = ? AND kind = ?`)

	prep(&s.stmtSelectEdgeOrigin,
		`SELECT origin FROM edges WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
	prepWrite(&s.stmtUpdateEdgeOrigin,
		`UPDATE edges SET origin = ?, tier = ? WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
	prepWrite(&s.stmtUpdateEdgeAttrs,
		`UPDATE edges SET confidence = ?, confidence_label = ?, origin = ?, tier = ?, meta = ?, resolve_terminal = ?, resolve_terminal_reason = ?, semantic_source = ? WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
	prepWrite(&s.stmtDeleteEdgeByKey,
		`DELETE FROM edges WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
	prep(&s.stmtEdgeExists,
		`SELECT 1 FROM edges WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ? LIMIT 1`)

	return err
}

// encodeMeta / decodeMeta live in meta_json.go (JSON codec + the
// metaWire typed DTO + the legacy-gob dual-read fallback).

// -- row scanners ---------------------------------------------------------

type rowScanner interface {
	Scan(...any) error
}

// scanNode is reserved for point lookups backed by sql.Row. database/sql
// rejects sql.RawBytes for Row.Scan because Row closes its cursor before Scan
// returns, so these callers must retain the driver's defensive []byte copy.
func scanNode(scanner rowScanner) (*graph.Node, error) {
	var metaBlob []byte
	return scanNodeWithMeta(scanner, &metaBlob)
}

// scanNodeCursor decodes metadata while the Rows cursor still owns the bytes.
// The decoded map never retains the RawBytes slice beyond Rows.Next.
func scanNodeCursor(scanner rowScanner) (*graph.Node, error) {
	var metaBlob sql.RawBytes
	return scanNodeWithMeta(scanner, &metaBlob)
}

func scanNodeWithMeta[B ~[]byte](scanner rowScanner, metaBlob *B) (*graph.Node, error) {
	var (
		n graph.Node
		p promotedNodeMeta
	)
	err := scanner.Scan(
		&n.ID, &n.Kind, &n.Name, &n.QualName, &n.FilePath,
		&n.StartLine, &n.EndLine, &n.StartColumn, &n.EndColumn, &n.Language,
		&n.RepoPrefix, &n.WorkspaceID, &n.ProjectID,
		&p.sig, &p.vis, &p.doc, &p.external, &p.returnType,
		&p.isAsync, &p.isStatic, &p.isAbstract, &p.isExported, &p.updatedAt,
		&p.dataClass, &p.semanticType, &p.semanticSource, &p.cloneSig,
		&p.entryPoint, &p.entryPointKind, metaBlob,
		&p.searchSig, &p.searchQualName, &p.searchDoc, &p.searchSuppressed, &p.sectionText,
	)
	if err != nil {
		return nil, err
	}
	if len(*metaBlob) > 0 {
		m, derr := decodeMeta([]byte(*metaBlob))
		if derr != nil {
			return nil, derr
		}
		n.Meta = m
	}
	// Restore the promoted columns into Meta. They are authoritative for
	// rows written after the promotion; a NULL column (legacy gob rows)
	// is left alone so the blob-carried value survives.
	restorePromotedMeta(&n, p)
	return &n, nil
}

// scanNodeLight scans the same columns as scanNode minus the trailing meta
// blob — no decodeMeta call, so no JSON/gob parse per row. Promoted columns
// still restore into Meta via restorePromotedMeta, so any caller that only
// reads a promoted key (signature, visibility, ..., semantic_type) sees the
// exact values scanNode would produce; only non-promoted content still
// living in the row's blob is absent. See graph.LightNodeReader: a node
// from this scan must never be round-tripped back through AddNode/AddBatch.
func scanNodeLight(scanner interface {
	Scan(...any) error
}) (*graph.Node, error) {
	var (
		n graph.Node
		p promotedNodeMeta
	)
	err := scanner.Scan(
		&n.ID, &n.Kind, &n.Name, &n.QualName, &n.FilePath,
		&n.StartLine, &n.EndLine, &n.StartColumn, &n.EndColumn, &n.Language,
		&n.RepoPrefix, &n.WorkspaceID, &n.ProjectID,
		&p.sig, &p.vis, &p.doc, &p.external, &p.returnType,
		&p.isAsync, &p.isStatic, &p.isAbstract, &p.isExported, &p.updatedAt,
		&p.dataClass, &p.semanticType, &p.semanticSource, &p.cloneSig,
		&p.entryPoint, &p.entryPointKind,
	)
	if err != nil {
		return nil, err
	}
	restorePromotedMeta(&n, p)
	return &n, nil
}

// scanNodeSummary scans the identity/location projection used by whole-graph
// algorithms. It deliberately leaves Meta nil: even promoted docs and
// signatures are unnecessary for adjacency, centrality, communities, and
// concept-name mining, and allocating them once per node dominates large
// SQLite scans.
func scanNodeSummary(scanner interface {
	Scan(...any) error
}) (*graph.Node, error) {
	var n graph.Node
	err := scanner.Scan(
		&n.ID, &n.Kind, &n.Name, &n.QualName, &n.FilePath,
		&n.StartLine, &n.EndLine, &n.StartColumn, &n.EndColumn, &n.Language,
		&n.RepoPrefix, &n.WorkspaceID, &n.ProjectID,
	)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// scanEdgeCursor is cursor-only: metadata is decoded before Rows.Next, so the
// driver-owned RawBytes never escapes into the returned edge.
func (s *Store) scanEdgeCursor(scanner rowScanner) (*graph.Edge, error) {
	var metaBlob sql.RawBytes
	return scanEdgeWithMeta(s, scanner, &metaBlob)
}

func scanEdgeWithMeta[B ~[]byte](store *Store, scanner rowScanner, metaBlob *B) (*graph.Edge, error) {
	var (
		e         graph.Edge
		crossRepo int64
		p         promotedEdgeMeta
	)
	err := scanner.Scan(
		&e.From, &e.To, &e.Kind, &e.FilePath, &e.Line,
		&e.Confidence, &e.ConfidenceLabel, &e.Origin, &e.Tier,
		&crossRepo, metaBlob, &p.resolveTerminal, &p.resolveTerminalReason, &p.semanticSource,
	)
	if err != nil {
		return nil, err
	}
	e.CrossRepo = crossRepo != 0
	if len(*metaBlob) > 0 {
		m, derr := decodeMeta([]byte(*metaBlob))
		if derr != nil {
			return nil, derr
		}
		e.Meta = m
	}
	// Restore the promoted columns into Meta. They are authoritative for
	// rows written after the promotion; a NULL column (pre-promotion rows)
	// is left alone so any blob-carried value survives.
	restorePromotedEdgeMeta(&e, p)
	if graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
		store.noteStructuralReadDrop(graph.StructuralPathSQLiteFullRead, &e)
		return nil, nil
	}
	return &e, nil
}

// scanEdgeLight scans an edge WITHOUT decoding its meta blob -- for hot
// read paths (dataflow call-target lookup) that read only endpoints,
// kind, and line. Skipping the meta column avoids the JSON decode + map
// allocation that dominates large edge scans on this backend; the
// returned edge's Meta is nil.
func (s *Store) scanEdgeLight(scanner interface {
	Scan(...any) error
}) (*graph.Edge, error) {
	return scanEdgeLightForStore(s, scanner)
}

func scanEdgeLightForStore(store *Store, scanner interface {
	Scan(...any) error
}) (*graph.Edge, error) {
	var (
		e         graph.Edge
		crossRepo int64
	)
	err := scanner.Scan(
		&e.From, &e.To, &e.Kind, &e.FilePath, &e.Line,
		&e.Confidence, &e.ConfidenceLabel, &e.Origin, &e.Tier,
		&crossRepo,
	)
	if err != nil {
		return nil, err
	}
	e.CrossRepo = crossRepo != 0
	if graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
		store.noteStructuralReadDrop(graph.StructuralPathSQLiteLightRead, &e)
		return nil, nil
	}
	return &e, nil
}

// -- writes ---------------------------------------------------------------

// AddNode inserts or updates a node in place. Idempotent on the id column --
// re-adding the same id with new content does a last-write-wins update while
// preserving incident edge rows, matching the in-memory store's behaviour.
func (s *Store) AddNode(n *graph.Node) {
	if n == nil || n.ID == "" {
		return
	}
	// Cross-daemon proxy nodes are volatile remote-derived state and
	// must never reach disk. The durable writer is the single gate —
	// neither the resolver mint path nor the hydrator carries its own
	// "don't persist" branch. A dropped proxy node is re-minted on
	// demand after a restart.
	if graph.IsProxyNode(n) {
		return
	}
	// Keep the single-node API on the same transaction path as AddBatch so a
	// parser-stamped clone_shingles payload and its node can never diverge.
	s.AddBatch([]*graph.Node{n}, nil)
}

func (s *Store) insertNodeLocked(stmt *sql.Stmt, n *graph.Node) (bool, error) {
	p, blobMeta := extractPromotedMeta(stripCloneShingles(n.Meta))
	metaBlob, err := encodeMeta(blobMeta)
	if err != nil {
		return false, err
	}
	res, err := stmt.Exec(
		n.ID, string(n.Kind), n.Name, n.QualName, n.FilePath,
		n.StartLine, n.EndLine, n.StartColumn, n.EndColumn, n.Language,
		n.RepoPrefix, n.WorkspaceID, n.ProjectID,
		p.sig, p.vis, p.doc, p.external, p.returnType,
		p.isAsync, p.isStatic, p.isAbstract, p.isExported, p.updatedAt,
		p.dataClass, p.semanticType, p.semanticSource, p.cloneSig,
		p.entryPoint, p.entryPointKind, metaBlob,
		p.searchSig, p.searchQualName, p.searchDoc, p.searchSuppressed, p.sectionText,
	)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	return changed > 0, err
}

// AddEdge inserts an edge. Idempotent on the logical edge key (from,
// to, kind, file_path, line) -- a second AddEdge with the same key is
// a no-op (INSERT OR IGNORE), matching the in-memory store's "stored
// pointer replaced in place" semantics. Origin upgrades on a re-add
// are NOT applied through this path; use SetEdgeProvenance for that
// (matches the in-memory store: AddEdge replaces the *Edge pointer,
// but the conformance suite only verifies dedup-by-key, not pointer
// replacement, and the in-memory store also routes provenance
// upgrades through SetEdgeProvenance).
func (s *Store) AddEdge(e *graph.Edge) {
	if e == nil || graph.IsProxyID(e.From) || graph.IsProxyID(e.To) {
		return
	}
	if graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
		s.recordStructuralEdge(graph.StructuralDropWrite, graph.StructuralPathSQLiteAddEdge, s.structuralWriteRepo(e, nil), e)
		return
	}
	// Route through the set-oriented writer. During a coordinated cold load the
	// single writer connection is pinned; using a prepared statement through
	// database/sql here would wait for a second writer slot. AddBatch reuses the
	// active writer and preserves INSERT OR IGNORE/idempotency semantics.
	s.AddBatch(nil, []*graph.Edge{e})
}

// UnresolvedEdgeInsertions implements graph.UnresolvedInsertionCounter.
func (s *Store) UnresolvedEdgeInsertions() uint64 {
	return s.unresolvedInserts.Load()
}

func (s *Store) insertEdgeLocked(stmt *sql.Stmt, e *graph.Edge) (bool, error) {
	if graph.IsUnresolvedTarget(e.To) {
		s.unresolvedInserts.Add(1)
	}
	p, blobMeta := extractPromotedEdgeMeta(e.Meta)
	metaBlob, err := encodeMeta(blobMeta)
	if err != nil {
		return false, err
	}
	var crossRepo int64
	if e.CrossRepo {
		crossRepo = 1
	}
	res, err := stmt.Exec(
		e.From, e.To, string(e.Kind), e.FilePath, e.Line,
		e.Confidence, e.ConfidenceLabel, e.Origin, e.Tier,
		crossRepo, metaBlob, p.resolveTerminal, p.resolveTerminalReason, p.semanticSource,
	)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	return changed > 0, err
}

// AddBatch inserts nodes and edges in one transaction using bounded multi-row
// statements. This preserves single-row UPSERT/IGNORE semantics while avoiding
// one SQLite execution per corpus row.
func (s *Store) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if _, err := s.addBatchSetOriented(nodes, edges); err != nil {
		panicOnFatal(err)
	}
}

// SetEdgeProvenance mutates an existing edge's origin in-place and
// bumps the identity-revision counter when the origin actually
// changes. Returns true iff a change was applied. Mirrors the
// in-memory store's "delete-then-insert of identity" semantics.
func (s *Store) SetEdgeProvenance(e *graph.Edge, newOrigin string) bool {
	if e == nil {
		return false
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Look up the stored origin -- the caller-supplied *Edge may be a
	// detached copy whose Origin already matches newOrigin even though
	// the row still has the old value.
	var storedOrigin string
	row := s.stmtSelectEdgeOrigin.QueryRow(e.From, e.To, string(e.Kind), e.FilePath, e.Line)
	if err := row.Scan(&storedOrigin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		panicOnFatal(err)
		return false
	}
	if storedOrigin == newOrigin {
		return false
	}
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return false
	}
	newTier := e.Tier
	if newTier != "" {
		newTier = graph.ResolvedBy(newOrigin)
	}
	if _, err := s.execActiveWriteLocked(context.Background(),
		`UPDATE edges SET origin = ?, tier = ? WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`,
		newOrigin, newTier, e.From, e.To, string(e.Kind), e.FilePath, e.Line,
	); err != nil {
		panicOnFatal(err)
		return false
	}
	// Reflect the change on the caller's struct, mirroring the
	// in-memory store which mutates the in-graph *Edge in place.
	e.Origin = newOrigin
	if e.Tier != "" {
		e.Tier = newTier
	}
	s.edgeIdentityRevs.Add(1)
	s.finishAnalysisMutationLocked(true)
	return true
}

// PersistEdgeAttributes durably rewrites the mutable attribute columns
// (confidence, confidence_label, origin, tier, meta) of the edge row
// identified by e's full logical key. It is the disk-backend counterpart
// to the in-memory store's "mutate the live *Edge in place" behaviour: a
// pass that confirms an edge's full provenance bundle (not just origin)
// calls this so the confidence / label / meta survive a reload. A missing
// row is a silent no-op (UPDATE ... WHERE matches nothing).
func (s *Store) PersistEdgeAttributes(e *graph.Edge) {
	if e == nil {
		return
	}
	p, blobMeta := extractPromotedEdgeMeta(e.Meta)
	metaBlob, err := encodeMeta(blobMeta)
	if err != nil {
		panicOnFatal(err)
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return
	}
	res, err := s.execActiveWriteLocked(context.Background(),
		`UPDATE edges SET confidence = ?, confidence_label = ?, origin = ?, tier = ?, meta = ?, resolve_terminal = ?, resolve_terminal_reason = ?, semantic_source = ? WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`,
		e.Confidence, e.ConfidenceLabel, e.Origin, e.Tier, metaBlob,
		p.resolveTerminal, p.resolveTerminalReason, p.semanticSource,
		e.From, e.To, string(e.Kind), e.FilePath, e.Line,
	)
	if err != nil {
		panicOnFatal(err)
		return
	}
	changed, err := res.RowsAffected()
	if err != nil {
		panicOnFatal(err)
		return
	}
	s.finishAnalysisMutationLocked(changed > 0)
}

// Compile-time assertion: *Store satisfies the batched meta persister.
var _ graph.EdgeMetaBatchPersister = (*Store)(nil)

// PersistEdgeAttributesBatch is the batched form of PersistEdgeAttributes:
// it rewrites the mutable attribute columns (confidence, confidence_label,
// origin, tier, meta) for every edge in the batch. Each transaction covers up
// to reindexChunkSize input rows, while each SQL statement updates up to
// edgeAttributeUpdateChunkSize logical edges through one VALUES relation. A
// row with no matching key is a silent no-op (UPDATE ... WHERE matches
// nothing).
func (s *Store) PersistEdgeAttributesBatch(edges []*graph.Edge) {
	if _, err := s.persistEdgeAttributesBatch(edges); err != nil {
		panicOnFatal(err)
	}
}

// Thirteen bound values are carried per logical edge. Seventy-five rows use 975 host
// parameters, leaving headroom below SQLite's conservative 999-variable
// limit while collapsing the former one-UPDATE-per-edge loop.
const (
	edgeAttributeUpdateParamsPerRow = 13
	edgeAttributeUpdateChunkSize    = 75
)

type edgeAttributeKey struct {
	from, to, kind, filePath string
	line                     int
}

// persistEdgeAttributesBatch returns the number of set-oriented UPDATE
// statements executed. The count is intentionally internal: focused tests use
// it to lock in the no-N+1 contract without exposing instrumentation through
// graph.Store.
func (s *Store) persistEdgeAttributesBatch(edges []*graph.Edge) (statements int, err error) {
	if len(edges) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	for i := 0; i < len(edges); i += reindexChunkSize {
		end := minInt(i+reindexChunkSize, len(edges))
		tx, err := s.beginWrite()
		if err != nil {
			return statements, err
		}
		chunkChanged := false
		for j := i; j < end; j += edgeAttributeUpdateChunkSize {
			batchEnd := minInt(j+edgeAttributeUpdateChunkSize, end)
			query, args, err := edgeAttributeUpdateStatement(edges[j:batchEnd])
			if err != nil {
				_ = tx.Rollback()
				return statements, err
			}
			if len(args) == 0 {
				continue
			}
			res, err := tx.Exec(query, args...)
			statements++
			if err != nil {
				_ = tx.Rollback()
				return statements, err
			}
			if changed, rowsErr := res.RowsAffected(); rowsErr == nil && changed > 0 {
				chunkChanged = true
			}
		}

		// Attribute updates and durable analysis invalidation commit together.
		// Difference-only UPDATE predicates make RowsAffected an actual-change
		// signal, so an idempotent warm pass keeps its active generation.
		invalidatedAnalysis := false
		if chunkChanged && s.analysisGenerationPresent {
			if err := invalidateAnalysisGenerationTx(tx); err != nil {
				_ = tx.Rollback()
				return statements, err
			}
			invalidatedAnalysis = true
		}
		if err := tx.Commit(); err != nil {
			return statements, err
		}
		if invalidatedAnalysis {
			s.analysisGenerationPresent = false
		}
		s.finishAnalysisMutationLocked(chunkChanged)
	}
	return statements, nil
}

// edgeAttributeUpdateStatement builds one set-oriented UPDATE. Duplicate
// logical keys within a chunk retain their last value, matching the former
// ordered per-edge loop; duplicates across chunks are naturally overwritten
// by the later statement.
func edgeAttributeUpdateStatement(edges []*graph.Edge) (string, []any, error) {
	updates := make([]*graph.Edge, 0, len(edges))
	positions := make(map[edgeAttributeKey]int, len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		key := edgeAttributeKey{
			from: edge.From, to: edge.To, kind: string(edge.Kind),
			filePath: edge.FilePath, line: edge.Line,
		}
		if pos, ok := positions[key]; ok {
			updates[pos] = edge
			continue
		}
		positions[key] = len(updates)
		updates = append(updates, edge)
	}
	if len(updates) == 0 {
		return "", nil, nil
	}

	var values strings.Builder
	values.Grow(len(updates) * len("(?,?,?,?,?,?,?,?,?,?,?,?,?),"))
	args := make([]any, 0, len(updates)*edgeAttributeUpdateParamsPerRow)
	for i, edge := range updates {
		if i > 0 {
			values.WriteByte(',')
		}
		values.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?)")
		promoted, blobMeta := extractPromotedEdgeMeta(edge.Meta)
		metaBlob, err := encodeMeta(blobMeta)
		if err != nil {
			return "", nil, err
		}
		args = append(args,
			edge.Confidence, edge.ConfidenceLabel, edge.Origin, edge.Tier, metaBlob,
			promoted.resolveTerminal, promoted.resolveTerminalReason, promoted.semanticSource,
			edge.From, edge.To, string(edge.Kind), edge.FilePath, edge.Line,
		)
	}

	query := `WITH updates(
		confidence, confidence_label, origin, tier, meta,
		resolve_terminal, resolve_terminal_reason, semantic_source,
		from_id, to_id, kind, file_path, line
	) AS (VALUES ` + values.String() + `)
	UPDATE edges AS e
	SET confidence = u.confidence,
		confidence_label = u.confidence_label,
		origin = u.origin,
		tier = u.tier,
		meta = u.meta,
		resolve_terminal = u.resolve_terminal,
		resolve_terminal_reason = u.resolve_terminal_reason,
		semantic_source = u.semantic_source
	FROM updates AS u
	WHERE e.from_id = u.from_id
		AND e.to_id = u.to_id
		AND e.kind = u.kind
		AND e.file_path = u.file_path
		AND e.line = u.line
		AND (e.confidence IS NOT u.confidence
			OR e.confidence_label IS NOT u.confidence_label
			OR e.origin IS NOT u.origin
			OR e.tier IS NOT u.tier
			OR e.meta IS NOT u.meta
			OR e.resolve_terminal IS NOT u.resolve_terminal
			OR e.resolve_terminal_reason IS NOT u.resolve_terminal_reason
			OR e.semantic_source IS NOT u.semantic_source)`
	return query, args, nil
}

// ReindexEdge updates the stored row after e.To has been mutated from
// oldTo to e.To. Implemented as delete-old + insert-new under the
// same write lock (SQLite's UNIQUE constraint on (from,to,kind,file,
// line) makes "UPDATE to_id" a one-shot, but the delete+insert form
// keeps semantics identical when the new (from,to,...) key happens to
// already exist -- the INSERT OR IGNORE drops the dup, just like the
// in-memory store's bucket-replace).
func (s *Store) ReindexEdge(e *graph.Edge, oldTo string) {
	if e == nil || oldTo == e.To {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return
	}

	// Delete and reinsert are one topology change. Keeping them in one
	// transaction prevents an encoding/insert failure from committing only the
	// delete while both analysis and mutation receipts still describe the old
	// graph as current.
	tx, err := s.beginWrite()
	if err != nil {
		panicOnFatal(err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	receipt := s.prepareSQLiteReindexReceiptTx(tx, []graph.EdgeReindex{{Edge: e, OldTo: oldTo}})
	deleteStmt := tx.Stmt(s.stmtDeleteEdgeByKey)
	defer deleteStmt.Close()
	insertStmt := tx.Stmt(s.stmtInsertEdge)
	defer insertStmt.Close()

	res, err := deleteStmt.Exec(e.From, oldTo, string(e.Kind), e.FilePath, e.Line)
	if err != nil {
		panicOnFatal(err)
		return
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		panicOnFatal(err)
		return
	}
	inserted, err := s.insertEdgeLocked(insertStmt, e)
	if err != nil {
		panicOnFatal(err)
		return
	}
	receipt.recordInserted(e, inserted)
	if err := tx.Commit(); err != nil {
		panicOnFatal(err)
		return
	}
	committed = true
	changed := deleted > 0 || inserted
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.publishSQLiteReindexReceiptLocked(receipt)
	}
}

// reindexChunkSize bounds the number of edge re-binds per BEGIN/COMMIT.
// Same shape as the bbolt sibling: large enough to amortise the
// per-tx overhead (BEGIN+COMMIT plus WAL fsync) but small enough that
// the WAL doesn't balloon and a crash mid-batch only loses ≤chunk
// mutations.
const reindexChunkSize = 5000

// errReindexWriterGateContended marks a reindex abandoned because the writer
// gate stayed contended past its bounded window (see reindexEdgesSetOriented).
// It is recoverable by design -- the batch is dropped and its edges are rebound
// by a later resolve pass -- so ReindexEdges swallows it instead of escalating
// through panicOnFatal. The bounded gate exists precisely so a mandatory
// reindex cannot block the store indefinitely; turning that liveness signal
// into a daemon-killing panic (which it did during warmup, when a checkpoint or
// a sibling mutation held writeMu past the window) is strictly worse than
// leaving a few edges unresolved until the next pass.
var errReindexWriterGateContended = errors.New("store_sqlite: reindex writer gate contended")

// ReindexEdges applies resolver re-binds through bounded VALUES relations.
// Each bounded transaction prefetches only the relevant identities through
// variable-safe VALUES relations, simulates the prior ordered DELETE + INSERT
// OR IGNORE semantics, and persists only the net final-state differences.
func (s *Store) ReindexEdges(batch []graph.EdgeReindex) {
	for _, r := range batch {
		if r.Edge != nil && graph.IsUnresolvedTarget(r.Edge.To) {
			s.unresolvedInserts.Add(1)
		}
	}
	if _, err := s.reindexEdgesSetOriented(batch); err != nil {
		if errors.Is(err, errReindexWriterGateContended) {
			log.Printf("store_sqlite: reindex writer gate contended, dropping %d rebind(s) — a later resolve pass rebinds them: %v", len(batch), err)
			return
		}
		panicOnFatal(err)
	}
}

// SetEdgeProvenanceBatch applies origin promotions through bounded VALUES
// joins and preserves ordered duplicate/change-count semantics.
func (s *Store) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) int {
	changed, _, err := s.setEdgeProvenanceBatchSetOriented(batch)
	if err != nil {
		panicOnFatal(err)
	}
	return changed
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RemoveEdge deletes every edge between (from, to) with the given
// kind. Returns true iff at least one row was deleted.
func (s *Store) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return false
	}
	res, err := s.execActiveWriteLocked(context.Background(),
		`DELETE FROM edges WHERE from_id = ? AND to_id = ? AND kind = ?`,
		from, to, string(kind),
	)
	if err != nil {
		panicOnFatal(err)
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		panicOnFatal(err)
		return false
	}
	changed := n > 0
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return changed
}

// EvictFile removes every node anchored to filePath and every edge
// that touches one of those nodes. Returns (nodesRemoved,
// edgesRemoved).
func (s *Store) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	return s.evictByPredicate(evictFilePredicate, filePath, true)
}

// EvictRepo removes every node in repoPrefix and every edge that
// touches one. Returns (nodesRemoved, edgesRemoved).
//
// DELIBERATE DIVERGENCE from the in-memory Graph.EvictRepo, which treats ""
// as a no-op: here "" is an exact match and really does delete unprefixed
// rows. That is load-bearing today. A single-repo daemon's nodes carry
// repo_prefix="", and Indexer.IndexCtx evicts them by the indexer's own
// (empty) prefix before a warm-restart bulk reload — without the delete,
// the reload lands on top of the previous run's rows and duplicates the
// graph. Aligning the two backends now would reintroduce that.
//
// The divergence expires once every repo carries a prefix: "" then names
// only the synthetic global externals, which no caller should ever bulk
// evict, and this can refuse "" the way PurgeRepo already does.
func (s *Store) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	predicate := evictRepoPredicate
	if repoPrefix != "" {
		// Make the partial nodes_by_repo predicate explicit so SQLite can use
		// that compact index for ordinary named repositories.
		predicate = evictNonEmptyRepoPredicate
	}
	return s.evictByPredicate(predicate, repoPrefix, false)
}

// -- reads ---------------------------------------------------------------

func (s *Store) GetNode(id string) *graph.Node {
	row := s.stmtGetNode.QueryRow(id)
	n, err := scanNode(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		panicOnFatal(err)
		return nil
	}
	return n
}

func (s *Store) GetNodeByQualName(qualName string) *graph.Node {
	if qualName == "" {
		return nil
	}
	row := s.stmtGetNodeByQual.QueryRow(qualName)
	n, err := scanNode(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		panicOnFatal(err)
		return nil
	}
	return n
}

func (s *Store) FindNodesByName(name string) []*graph.Node {
	return s.queryNodes(s.stmtFindByName, name)
}

func (s *Store) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	return s.queryNodes(s.stmtFindByNameInRepo, name, repoPrefix)
}

func (s *Store) GetFileNodes(filePath string) []*graph.Node {
	return s.GetFileNodesContext(context.Background(), filePath)
}

// GetFileNodesContext is the deadline-aware file lookup used by bounded MCP
// localization. QueryContext covers both pool acquisition and SQLite execution,
// so a busy store cannot extend a request beyond its context budget.
func (s *Store) GetFileNodesContext(ctx context.Context, filePath string) []*graph.Node {
	return s.queryNodesContext(ctx, s.stmtFileNodes, filePath)
}

func (s *Store) GetRepoNodes(repoPrefix string) []*graph.Node {
	return s.queryNodes(s.stmtRepoNodes, repoPrefix)
}

func (s *Store) GetRepoNodesByLanguage(repoPrefix, language string) []*graph.Node {
	if language == "" {
		return nil
	}
	return s.queryNodesSQL(
		`SELECT `+lookupNodeCols+` FROM nodes WHERE repo_prefix = ? AND language = ? ORDER BY id`,
		repoPrefix, language,
	)
}

func (s *Store) AllNodes() []*graph.Node {
	return s.queryNodes(s.stmtAllNodes)
}

func (s *Store) queryNodes(stmt *sql.Stmt, args ...any) []*graph.Node {
	return s.queryNodesContext(context.Background(), stmt, args...)
}

func (s *Store) queryNodesContext(ctx context.Context, stmt *sql.Stmt, args ...any) []*graph.Node {
	rows, err := stmt.QueryContext(ctx, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		n, err := scanNodeCursor(rows)
		if err != nil {
			if ctx.Err() != nil {
				return out
			}
			panicOnFatal(err)
			return out
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil && ctx.Err() == nil {
		panicOnFatal(err)
	}
	return out
}

// GetRepoNonContentNodes is the graph.NonContentNodeReader fast path: a
// SQL-level enumeration that drops CONTENT (data_class="content") section
// nodes, so the code-oriented passes never materialise a content-heavy
// repo's hundreds of thousands of sections. data_class is a promoted node
// column for rows written by the flat codec; legacy JSON rows (no column)
// fall back to json_extract, guarded by json_valid so the flat / gob blobs
// — which are not JSON — are skipped without error. The NULL-safe
// `IS NOT 'content'` keeps every node whose data_class is absent or carries
// any other value. An empty repoPrefix spans all repos.
func (s *Store) GetRepoNonContentNodes(repoPrefix string) []*graph.Node {
	const filter = `COALESCE(data_class, CASE WHEN json_valid(CAST(meta AS TEXT)) THEN json_extract(CAST(meta AS TEXT), '$.data_class') END) IS NOT 'content'`
	if repoPrefix == "" {
		return s.scanNodeQuery(`SELECT ` + lookupNodeCols + ` FROM nodes WHERE ` + filter)
	}
	return s.scanNodeQuery(`SELECT `+lookupNodeCols+` FROM nodes WHERE repo_prefix = ? AND `+filter, repoPrefix)
}

// AllNodesLight implements graph.NodeLightScanner with the identity/location
// projection only. Whole-graph analyses avoid both the opaque metadata blob and
// promoted docs/signatures, so returned nodes always have nil Meta.
func (s *Store) AllNodesLight() []*graph.Node {
	rows, err := s.db.Query(`SELECT ` + lookupNodeSummaryCols + ` FROM nodes`)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		n, err := scanNodeSummary(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// GetRepoNodesLight omits the opaque meta column for repo-scoped callers that
// only need promoted structural fields. This keeps an already-enriched repo's
// metadata blobs out of the driver and decoder hot path.
func (s *Store) GetRepoNodesLight(repoPrefix string) []*graph.Node {
	rows, err := s.db.Query(`SELECT `+lookupNodeColsLight+` FROM nodes WHERE repo_prefix = ?`, repoPrefix)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		n, err := scanNodeLight(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// scanNodeQuery runs an ad-hoc node SELECT (columns = lookupNodeCols) and
// scans its rows into nodes — for the few non-hot enumerations that need a
// WHERE clause the prepared statements don't cover.
func (s *Store) scanNodeQuery(query string, args ...any) []*graph.Node {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		n, err := scanNodeCursor(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, n)
	}
	// A driver failure part-way through ends the cursor exactly like a clean
	// exhaust, so without this check a repo-wide node scan can come back
	// silently truncated and read as complete.
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

func (s *Store) GetOutEdges(nodeID string) []*graph.Edge {
	return s.queryEdges(s.stmtOutEdges, nodeID)
}

// EdgeExists reports whether an edge with exactly this identity is present --
// (from, to, kind, file_path, line) is the edges UNIQUE key, so this is a
// single indexed point lookup: no row decode, no Meta gob, no per-edge
// allocation, unlike GetOutEdges. The resolver's liveness guard
// (edgeStillLive) calls this once per applied edge on the cold/full pass; the
// difference from scanning + gob-decoding all of `from`'s out-edges is a
// dominant share of resolve cost on a large graph.
func (s *Store) EdgeExists(from, to string, kind graph.EdgeKind, filePath string, line int) bool {
	var one int
	err := s.stmtEdgeExists.QueryRow(from, to, string(kind), filePath, line).Scan(&one)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		panicOnFatal(err)
		return false
	}
	return true
}

func (s *Store) GetInEdges(nodeID string) []*graph.Edge {
	return s.queryEdges(s.stmtInEdges, nodeID)
}

// GetOutEdgesForNodes fetches the out-edges of many nodes in one batched query
// (chunked) instead of a round-trip per node. The single-file resolve path
// walks every node of the edited file, which is an N+1 query storm on a disk
// backend; this collapses it to one query per chunk. Edges are grouped by
// their from_id; nodes with no out-edges are absent from the map.
func (s *Store) GetOutEdgesForNodes(ids []string) map[string][]*graph.Edge {
	out := make(map[string][]*graph.Edge, len(ids))
	if len(ids) == 0 {
		return out
	}
	seen := make(map[string]struct{}, len(ids))
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, id := range chunk {
			ph[j] = "?"
			args[j] = id
		}
		q := `SELECT ` + lookupEdgeCols + ` FROM edges WHERE from_id IN (` + strings.Join(ph, ",") + `)`
		for _, e := range s.queryEdgesSQL(q, args...) {
			out[e.From] = append(out[e.From], e)
		}
	}
	return out
}

func (s *Store) AllEdges() []*graph.Edge {
	return s.queryEdges(s.stmtAllEdges)
}

// GetRepoEdges returns every edge whose source node has the given
// RepoPrefix. The pre-Store idiom — GetRepoNodes(r) followed by
// GetOutEdges(n.ID) per node — was O(repo_nodes) prepared-statement
// invocations, which on a multi-repo workspace dominated the
// per-repo extractor passes. A single JOIN over edges/nodes keyed
// on n.repo_prefix runs as one prepared statement and hits the
// existing repo_prefix index.
func (s *Store) GetRepoEdges(repoPrefix string) []*graph.Edge {
	if repoPrefix == "" {
		return nil
	}
	return s.queryEdges(s.stmtRepoEdges, repoPrefix)
}

func (s *Store) queryEdges(stmt *sql.Stmt, args ...any) []*graph.Edge {
	rows, err := stmt.Query(args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		e, err := s.scanEdgeCursor(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		if e == nil {
			continue
		}
		out = append(out, e)
	}
	// A driver failure part-way through the cursor ends the loop exactly like
	// a clean exhaust. Without this check the caller would receive a silently
	// truncated slice and treat it as the complete result.
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// -- counts and stats -----------------------------------------------------

func (s *Store) NodeCount() int {
	var n int
	if err := s.stmtNodeCount.QueryRow().Scan(&n); err != nil {
		panicOnFatal(err)
		return 0
	}
	return n
}

func (s *Store) EdgeCount() int {
	var n int
	if err := s.stmtEdgeCount.QueryRow().Scan(&n); err != nil {
		panicOnFatal(err)
		return 0
	}
	return n
}

func (s *Store) Stats() graph.GraphStats {
	st := graph.GraphStats{
		ByKind:     map[string]int{},
		ByLanguage: map[string]int{},
	}
	st.TotalNodes = s.NodeCount()
	st.TotalEdges = s.EdgeCount()

	rows, err := s.stmtStatsByKind.Query()
	if err != nil {
		panicOnFatal(err)
		return st
	}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return st
		}
		st.ByKind[kind] = n
	}
	// Same treatment as a Scan failure above: Close reports the driver's
	// close error, so without Err a scan that died mid-flight returns a
	// short histogram indistinguishable from a real one.
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		panicOnFatal(err)
		return st
	}
	_ = rows.Close()

	rows, err = s.stmtStatsByLanguage.Query()
	if err != nil {
		panicOnFatal(err)
		return st
	}
	for rows.Next() {
		var lang string
		var n int
		if err := rows.Scan(&lang, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return st
		}
		st.ByLanguage[lang] = n
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		panicOnFatal(err)
		return st
	}
	_ = rows.Close()
	return st
}

func (s *Store) RepoStats() map[string]graph.GraphStats {
	out := map[string]graph.GraphStats{}
	rows, err := s.stmtRepoStatsNodes.Query()
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo, kind, lang string
		var n int
		if err := rows.Scan(&repo, &kind, &lang, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		st, ok := out[repo]
		if !ok {
			st = graph.GraphStats{ByKind: map[string]int{}, ByLanguage: map[string]int{}}
		}
		st.TotalNodes += n
		st.ByKind[kind] += n
		st.ByLanguage[lang] += n
		out[repo] = st
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		panicOnFatal(err)
		return out
	}
	_ = rows.Close()

	rows, err = s.stmtRepoStatsEdges.Query()
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		st, ok := out[repo]
		if !ok {
			st = graph.GraphStats{ByKind: map[string]int{}, ByLanguage: map[string]int{}}
		}
		st.TotalEdges = n
		out[repo] = st
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		panicOnFatal(err)
		return out
	}
	_ = rows.Close()
	return out
}

func (s *Store) RepoPrefixes() []string {
	rows, err := s.stmtRepoPrefixes.Query()
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, p)
	}
	// A short prefix list feeds repo scoping and orphan purging, so a
	// truncated scan must not pass for the whole set.
	panicOnFatal(rows.Err())
	return out
}

// -- provenance verification ---------------------------------------------

func (s *Store) EdgeIdentityRevisions() int {
	return int(s.edgeIdentityRevs.Load())
}

// VerifyEdgeIdentities is a no-op for the SQL backend: the in-memory
// store's invariant is "the same *Edge pointer lives in both
// adjacency views". The SQL store has a single row per edge, so the
// invariant is trivially satisfied -- no walk can find a divergence
// to report.
func (s *Store) VerifyEdgeIdentities() error { return nil }

// -- memory estimation (advisory) ----------------------------------------

// perRowByteEstimate is a deliberately rough per-row byte cost --
// the disk backend doesn't have an in-memory footprint to report, so
// the contract (per Store interface comment) is "return what you can
// compute and callers treat the result as advisory". The conformance
// test only checks NodeCount.
const (
	perNodeByteEstimate = 256
	perEdgeByteEstimate = 128
)

func (s *Store) RepoMemoryEstimate(repoPrefix string) graph.RepoMemoryEstimate {
	var est graph.RepoMemoryEstimate
	var n, e int
	if err := s.stmtRepoNodeCount.QueryRow(repoPrefix).Scan(&n); err != nil {
		panicOnFatal(err)
		return est
	}
	if err := s.stmtRepoEdgeCount.QueryRow(repoPrefix).Scan(&e); err != nil {
		panicOnFatal(err)
		return est
	}
	est.NodeCount = n
	est.EdgeCount = e
	est.NodeBytes = uint64(n) * perNodeByteEstimate
	est.EdgeBytes = uint64(e) * perEdgeByteEstimate
	return est
}

// AllRepoMemoryEstimates reports per-repo node/edge counts and their byte
// estimates.
//
// It reads the counters the indexer already persists in repo_index_state
// rather than re-deriving them from the corpus. The GROUP BY this replaced
// was measured at 10.7s warm and 27.8s cold on a 9.4 GB store — 6.9s over
// nodes plus a nodes-to-edges join — against 0.00s for the 29-row counter
// read that produces the same numbers. Both plans were already covering;
// the cost was touching every node and every edge to recount what the
// indexer had counted on the way in.
//
// The counters are exact for every repo the indexer has written: measured
// against a full scan of a live 606k-node store, four of five sampled repos
// matched exactly and the fifth differed by 2 rows in 30,819. Drift is
// reconciled by ScanRepoMemoryEstimates, which `daemon status --exact`
// runs and persists.
//
// Repos absent from repo_index_state contribute nothing. In practice that
// is the empty prefix and the synthetic external-call bucket, neither of
// which is a tracked repo and neither of which the per-repo status table
// renders.
func (s *Store) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	out := map[string]graph.RepoMemoryEstimate{}
	rows, err := s.stmtAllRepoStateCounts.Query()
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo string
		var nodeCount, edgeCount int
		if err := rows.Scan(&repo, &nodeCount, &edgeCount); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		out[repo] = repoMemEstimateFor(nodeCount, edgeCount)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		panicOnFatal(err)
		return map[string]graph.RepoMemoryEstimate{}
	}
	_ = rows.Close()
	return out
}

// repoMemEstimateFor turns a node/edge count into the estimate shape. The
// byte figures are counts times a fixed per-record estimate, which is what
// the corpus scan produced too — only the counts ever came from the store.
func repoMemEstimateFor(nodeCount, edgeCount int) graph.RepoMemoryEstimate {
	return graph.RepoMemoryEstimate{
		NodeCount: nodeCount,
		NodeBytes: uint64(nodeCount) * perNodeByteEstimate,
		EdgeCount: edgeCount,
		EdgeBytes: uint64(edgeCount) * perEdgeByteEstimate,
	}
}

// ScanRepoMemoryEstimates recomputes the per-repo estimates from the stored
// nodes and edges, ignoring the persisted counters. This is the expensive
// path AllRepoMemoryEstimates used to be: on a 9.4 GB store it is tens of
// seconds. It exists so `daemon status --exact` can answer "are the counters
// telling the truth" and heal them, not to serve routine status polls.
//
// The caller owns the deadline. There is no internal budget: a partial
// count is not a stale answer, it is a wrong one, so a scan that cannot
// finish returns the context error instead of a short map.
func (s *Store) ScanRepoMemoryEstimates(ctx context.Context) (map[string]graph.RepoMemoryEstimate, error) {
	nodes, err := s.scanRepoCounts(ctx, s.stmtAllRepoCountsNodes)
	if err != nil {
		return nil, err
	}
	edges, err := s.scanRepoCounts(ctx, s.stmtAllRepoCountsEdges)
	if err != nil {
		return nil, err
	}
	out := make(map[string]graph.RepoMemoryEstimate, len(nodes))
	for repo, n := range nodes {
		out[repo] = repoMemEstimateFor(n, edges[repo])
	}
	for repo, e := range edges {
		if _, ok := nodes[repo]; !ok {
			out[repo] = repoMemEstimateFor(0, e)
		}
	}
	return out, nil
}

// scanRepoCounts runs one COUNT … GROUP BY repo_prefix scan to completion.
// A scan cut short by the caller's deadline is an error, never a short map.
func (s *Store) scanRepoCounts(ctx context.Context, stmt *sql.Stmt) (map[string]int, error) {
	rows, err := stmt.QueryContext(ctx)
	if err != nil {
		if ctx.Err() == nil {
			panicOnFatal(err)
		}
		return nil, err
	}
	out := map[string]int{}
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			_ = rows.Close()
			if ctx.Err() == nil {
				panicOnFatal(err)
			}
			return nil, err
		}
		out[repo] = n
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		if ctx.Err() == nil {
			panicOnFatal(err)
		}
		return nil, err
	}
	return out, nil
}

// ReconcileRepoCounters rewrites the persisted per-repo counters from a
// scan result, healing drift between what the indexer recorded and what the
// corpus actually holds. Only the counts are touched: the freshness columns
// (indexed SHA, dirty bit, workspace fingerprint, extractor versions) describe
// the index pass that produced the rows and must survive an audit that did not
// re-index anything.
//
// Repos present in the scan but absent from repo_index_state are skipped
// rather than invented — the empty prefix and the synthetic external-call
// bucket are not tracked repos, and a row here would make them render as one.
func (s *Store) ReconcileRepoCounters(scanned map[string]graph.RepoMemoryEstimate) error {
	for repo, est := range scanned {
		st, found, err := s.GetRepoIndexState(repo)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if st.NodeCount == est.NodeCount && st.EdgeCount == est.EdgeCount {
			continue
		}
		st.NodeCount = est.NodeCount
		st.EdgeCount = est.EdgeCount
		if err := s.SetRepoIndexState(st); err != nil {
			return err
		}
	}
	return nil
}

// -- helpers --------------------------------------------------------------

// panicOnFatal turns truly catastrophic SQLite errors (closed DB,
// schema mismatch, disk-full at insert time) into a panic so callers
// see them, while letting expected sql.ErrNoRows / busy / no-affected
// callers stay quiet. The graph.Store interface deliberately does not
// surface errors -- it mirrors the in-memory store's "everything
// succeeds" contract -- so a fatal storage failure cannot be ignored.
//
// Caller contract: on a teardown-race error panicOnFatal RETURNS rather than
// panicking, so a caller that keeps using the query result after it returns
// MUST nil-check first. `rows, err := db.Query(...); panicOnFatal(err)` leaves
// rows == nil on a swallowed error, and the subsequent rows.Close() /
// rows.Next() would SIGSEGV — the aggregator reads early-return their empty
// value on nil rows for exactly this reason. In one line: fatal panics; a
// teardown-race read returns empty.
func panicOnFatal(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	// A closed statement / database / connection is a teardown race, not
	// data corruption: Close() shuts the store (daemon shutdown, restart,
	// or store swap) while an in-flight reader -- e.g. a deferred
	// parallel-enrich goroutine still holding a cached *sql.Stmt -- runs a
	// query. Crashing the whole daemon over a benign shutdown race is
	// strictly worse than the read returning empty (or a winding-down write
	// being dropped), so treat these as non-fatal.
	if errors.Is(err, sql.ErrConnDone) || isStoreClosedErr(err) {
		return
	}
	panic(fmt.Errorf("store_sqlite: %w", err))
}

// isStoreClosedErr reports whether err is the database/sql sentinel for a
// closed prepared statement or a closed database -- string-matched because
// database/sql does not export these as typed errors.
func isStoreClosedErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "statement is closed") ||
		strings.Contains(msg, "database is closed")
}

// -- predicate-shaped reads ---------------------------------------------
//
// Each method runs one indexed SELECT and streams rows back via the
// iter.Seq[T] yield callback. Stops cleanly when yield returns false.
// Heavier than the equivalent bolt path (sql parsing + driver row
// materialisation) but cuts the resolver's wasted full-table scans
// down to "match-only" cardinality, which is the whole point.

// All three predicate iterators here MATERIALISE the query result
// into a slice before yielding, then iterate the slice. This avoids
// a deadlock peculiar to the SQLite backend's single-connection
// pool: a streaming rows-cursor holds THE connection, and any
// callback in the yield body that re-enters the store (e.g. GetNode
// to resolve an edge's caller) blocks forever waiting on the same
// connection. Materialise-then-yield releases the connection before
// the body runs, so re-entrant store calls work.
//
// The "predicate-shaped" win still holds: the indexed SELECT only
// fetches matching rows, not the whole table. We give up streaming
// memory savings (we still build a Go slice of *Edge / *Node) but
// keep the structural advantage that the row count flowing through
// scanEdge is proportional to the result, not the table.

// EdgesByKind: indexed SELECT on the (kind) column.
func (s *Store) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		out := s.queryEdgesSQL(`SELECT `+lookupEdgeCols+`
FROM edges WHERE kind = ?`, string(kind))
		for _, e := range out {
			if !yield(e) {
				return
			}
		}
	}
}

// NodesByKind: indexed SELECT on the (kind) column.
func (s *Store) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		out := s.queryNodesSQL(`SELECT `+lookupNodeCols+` FROM nodes WHERE kind = ?`, string(kind))
		for _, n := range out {
			if !yield(n) {
				return
			}
		}
	}
}

// EdgesWithUnresolvedTarget yields edges whose target is an unresolved stub
// in either form graph.IsUnresolvedTarget recognises. Filters on the
// is_unresolved generated column (see isUnresolvedColumnDDL) rather than
// re-deriving the to_id pattern match in SQL: measured 2.7x faster than the
// equivalent to_id-based OR query on a real 26-repo store (7.96s -> 2.95s for
// the same 847,684-row result) because the boolean index's bookmark lookups
// land in ascending rowid order, unlike a to_id-ordered index's.
//
// Gate-owned fn-value placeholders (graph.FnValuePlaceholderMarker,
// `unresolved::fnvalue::<name>`) are excluded on top of is_unresolved: the
// master resolver can never bind them, so they are pure pending-set bloat here
// (a live store held millions). The bare form is dropped by the range predicate
// — which rides edges_by_to(to_id) — using the ':;' range end from
// isUnresolvedColumnDDL's idiom (';' == ':'+1); the multi-repo COPY-rewrite form
// is dropped by the NOT LIKE, matching IsFnValuePlaceholder's infix shape.
func (s *Store) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		scan, err := s.BeginUnresolvedEdgeScan(context.Background())
		if err != nil {
			return
		}
		var afterID int64
		for {
			page, err := s.ReadUnresolvedEdgePage(context.Background(), scan, afterID, 2048, 16<<20)
			if err != nil {
				return
			}
			for _, e := range page.Edges {
				if !yield(e) {
					return
				}
			}
			if page.Exhausted || page.NextID <= afterID {
				return
			}
			afterID = page.NextID
		}
	}
}

// queryEdgesSQL runs an edge-shaped SELECT, materialises the rows
// into a slice, and closes the rows-cursor before returning —
// releasing the underlying sql.Conn so the predicate-iterator's
// callback body is free to make re-entrant store calls without
// deadlocking on the MaxOpenConns=1 pool. Companion to the existing
// queryEdges helper that takes a *sql.Stmt; this one takes a raw
// SQL string so the predicate iterators can pass inline queries.
//
// Statement-level, row-level, and cursor-level failures all go through
// panicOnFatal: a teardown-race read still degrades to what was materialised,
// but a real storage failure is raised instead of being handed back as an
// empty or short slice the caller cannot tell from a genuinely small result.
// A row that will not decode ends the scan for the same reason a driver error
// does — skipping it hands back a result that is short by exactly the rows
// that were corrupt, which is the one failure a caller cannot detect.
func (s *Store) queryEdgesSQL(q string, args ...any) []*graph.Edge {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		e, err := s.scanEdgeCursor(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		if e == nil {
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// queryNodesSQL is the node-shaped sibling of queryEdgesSQL, with the same
// error contract.
func (s *Store) queryNodesSQL(q string, args ...any) []*graph.Node {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		n, err := scanNodeCursor(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		if n == nil {
			continue
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// lookupChunkSize bounds the IN-list parameter count per SQL query.
// SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 32766 in modern
// builds, but staying well under that keeps query plans stable and
// avoids surprising the parser on monster lists.
const lookupChunkSize = 5000

// GetNodesByIDs collapses N per-id SELECTs into ⌈N/chunk⌉ queries
// of the form `SELECT … FROM nodes WHERE id IN (?, ?, …)`. The
// resolver fires hundreds of thousands of these on a large pass;
// chunking turns hundreds of seconds into single-digit seconds.
func (s *Store) GetNodesByIDs(ids []string) map[string]*graph.Node {
	if len(ids) == 0 {
		return nil
	}
	// Dedupe + skip empty up front to keep the chunk loop honest.
	seen := make(map[string]struct{}, len(ids))
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	out := make(map[string]*graph.Node, len(uniq))
	const nodeCols = lookupNodeCols
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		placeholders := strings.Repeat(",?", len(chunk))[1:]
		q := `SELECT ` + nodeCols + ` FROM nodes WHERE id IN (` + placeholders + `)`
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		for _, n := range s.queryNodesSQL(q, args...) {
			if n != nil {
				out[n.ID] = n
			}
		}
	}
	return out
}

// FindNodesByNames collapses N per-name FindNodesByName queries into
// one `SELECT … FROM nodes WHERE name IN (…)` plus an in-Go bucket
// by name. The (name) index makes the SELECT seek-driven, and the
// caller sees the same map[name][]*Node it would have built by
// calling FindNodesByName N times.
func (s *Store) FindNodesByNames(names []string) map[string][]*graph.Node {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	uniq := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		uniq = append(uniq, name)
	}
	out := make(map[string][]*graph.Node, len(uniq))
	const nodeCols = lookupNodeCols
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		placeholders := strings.Repeat(",?", len(chunk))[1:]
		q := `SELECT ` + nodeCols + ` FROM nodes WHERE name IN (` + placeholders + `)`
		args := make([]any, len(chunk))
		for j, name := range chunk {
			args[j] = name
		}
		for _, n := range s.queryNodesSQL(q, args...) {
			if n == nil {
				continue
			}
			out[n.Name] = append(out[n.Name], n)
		}
	}
	return out
}

// -- BulkLoader implementation -------------------------------------------

// BeginBulkLoad / FlushBulk (the graph.BulkLoader bracket) live in
// bulk_load.go. The bracket exists so the indexer's in-memory shadow
// swap activates — the resolver and its post-resolve passes run against
// an in-memory *Graph at nanosecond latency, and the final drain dumps
// the resolved graph to sqlite in one shot. On a first/empty cold index
// the bracket additionally engages a bulk-persist fast path (dropped
// secondary indexes + synchronous=OFF on a pinned connection).
