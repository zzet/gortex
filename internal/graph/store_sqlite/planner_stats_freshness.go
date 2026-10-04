package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Runtime planner-statistics freshness.
//
// sqlite_stat1 is written in exactly two places before this file: at
// coordinated cold-load finalize (Store.EndCoordinatedBulkLoad) and by
// healPlannerStats at Open. A daemon that stays up for days never reaches
// either again, while the store keeps growing — a second tracked repository
// drains into it, watchers reindex, view generations publish payload. The
// lead's own store believed 592k nodes / 2.77M edges while holding 1.69M /
// 8.58M, and a planner costing joins against a quarter of the real corpus
// picks the wrong outer loop on exactly the receiver/edge queries issue #651
// is about.
//
// The mechanism here is growth-based, not periodic: the store is asked at a
// handful of pipeline boundaries whether it has outgrown its statistics, and
// only a store that has pays an ANALYZE. There are FOUR live in-daemon
// boundaries, and between them they cover every way a running store grows:
//
//	1. the indexer's shadow-drain site, after FlushBulk
//	2. the indexer's counter site, after persistRepoIndexState on the
//	   direct-SQLite path
//	3. the indexer's per-commit / HEAD-move incremental site, after
//	   reconcileRepoIndexState (the git-watcher / poller path, which is where
//	   a daemon spends most of its life and reaches neither of the two above)
//	4. the resolver's whole-graph attribution arm
//
// A fifth call sits in PublishAndRoute, after the route flip. It is the right
// boundary for a DIRECT caller of that API, and it has no in-daemon caller
// today: the checkout coordinator flips through FlipCheckoutRouteSlot and
// reaches the indexer sites above via the generation builder's IndexCtx.
//
// The refresh itself is COOPERATIVE, and that is not an optimization. Every
// one of those boundaries is reached with wider locks already held — the
// process-global reach-topology writer gate at the indexer sites, the shared
// batch mutation gate plus a repository lane on the watcher/poller path, the
// resolver's own ResolveMutex on the resolve arm. A refresh that queued on the
// store's write gate, and then held it for a tens-of-seconds ANALYZE of every
// critical index, would hold those wider gates for the same span: reach
// readers give up rather than wait, so MCP answers degrade to empty for the
// duration; other repositories' lanes stall behind a queued batch writer; and
// the bounded-gate writers (reindex_set.go, unresolved_edge_identity_batches.go)
// give the gate sqliteBusyRetryWindow() — 15 s — and then DROP their batches,
// so an ANALYZE outliving that window discards concurrent rebind work.
//
// So the stale path never waits and never holds the store's write gate across
// more than one index: it try-locks per index, analyzes ONE under a timeout
// strictly below that 15 s window, releases, and stops at the first busy gate.
// checkpointWALPassive has used the same try-lock-and-defer shape for the same
// reason since before this file existed.
//
// What that shape does and does not claim, stated exactly, because the three
// are easy to conflate:
//
//   - It protects other STORE writers and the 15 s bounded-gate droppers. Only
//     one bounded hold is taken at a time, and no hold can outlive
//     plannerStatsIndexTimeout, so no concurrent writer can be starved past
//     its own window by this code.
//   - It BOUNDS what one boundary pays under its wider gates — the reach
//     topology writer gate, the batch mutation gate plus repo lane,
//     ResolveMutex. A pass stops starting new indexes once it has spent
//     plannerStatsPassBudget, so the gate-holding part of a boundary is at
//     most plannerStatsPassBudget + one index's ANALYZE + one bounded
//     sqlite_schema reload, each of the latter two capped by
//     plannerStatsIndexTimeout. That is the documented bound, and it is the
//     one every call-site comment repeats. It does NOT shorten those wider
//     gates below it: a boundary over a stale store genuinely pays that much.
//
//     Four reads sit OUTSIDE that bound, because they take no gate and the
//     budget clock has not started: the health probe, the narrower re-probe,
//     the present-index list, and the stat-row set the work list is scoped
//     from. All four are read-pool queries — a schema join, a handful of
//     sqlite_stat1 seeks, one WITHOUT ROWID scan of repo_index_state, and (on
//     the first probe only) the capped receiver count. They cost the caller
//     latency under its own wider locks and nothing under the store's.
//   - It CONVERGES across boundaries, not within one. The work list is scoped
//     to the family the verdict named — a nodes verdict re-analyzes the nodes
//     indexes, not the edges ones — and a deferred pass keeps a resumable
//     cursor of the indexes it finished, so the next boundary continues AFTER
//     the last completed index instead of restarting at the head. Without the
//     cursor a store whose gate is contended at the same point every time
//     would re-analyze index 1 forever and never reach index 3. Family scoping
//     is why the growth anchor is per family too (plannerStatsBaseline): a
//     completed pass moves only the anchor of what it rebuilt, so a store that
//     doubled both relations fires a nodes verdict at one boundary, an edges
//     verdict at the next, and is quiescent at the third.
//
// The price of that scoping, stated so nobody reads it as free: a store growing
// proportionally pays TWO verdicts per doubling rather than one. The critical
// list holds ten indexes — seven on nodes, three on edges — so a doubling costs
// up to ten gate-holding boundaries (seven for the nodes verdict, three for the
// edges one) spread over however many pipeline boundaries the budget takes to
// finish them, and recycles the read pool twice, once per completed family.
// That is the intended trade: the alternative is one verdict per doubling with
// the edges statistics frozen at their cold-load figures forever, which is the
// defect this file exists for.

// plannerStatsGrowthFactor is how far a relation may outgrow the size its
// statistics were computed at before a refresh is worth its cost. Two is the
// point where a join-order decision flips in the plans this engine cares
// about; a tighter factor would buy re-analysis of a store that is merely
// bigger, and on a 12 GB store the critical indexes are ~1.8 GiB of pages.
const plannerStatsGrowthFactor = 2

// plannerStatsHealthProbeCap bounds the receiver index's verification count.
//
// plannerStatsIndexProbe.countQuery is deliberately NOT index-only — the
// partial predicate reads language, kind and file_path, so satisfying it
// probes the WITHOUT ROWID nodes table once per index entry. PR 1 could bind
// believed*2+1 unconditionally because it only ever ran on a believed count
// already known to be tiny. The health probe runs at every call site and on
// every index_health rebuild, so an uncapped believed*2+1 would turn a
// "cheap" probe into ~10^5 random table probes on a store whose receiver row
// believes tens of thousands.
//
// Capped, the receiver rule answers only the question PR 1 asked it: is this
// row the poisoned near-zero one. General growth detection belongs to the
// nodes/edges counters, which cost one WITHOUT ROWID scan of a table with one
// row per tracked repository.
const plannerStatsHealthProbeCap = 2*plannerStatsSuspectRows + 1

// The indexes whose statistics the freshness verdict reads. Nodes prefers
// nodes_by_kind and falls back to nodes_by_file; both are non-partial, so the
// leading stat token is the table's cardinality rather than a subset's.
const (
	plannerStatsNodesIndex         = "nodes_by_kind"
	plannerStatsNodesFallbackIndex = "nodes_by_file"
	plannerStatsEdgesIndex         = "edges_by_kind"
	plannerStatsReceiverIndex      = "nodes_go_receiver_type"
)

// plannerStatsBulkWindowReason is reported instead of a verdict while a bulk
// load owns the store. The droppable critical indexes do not exist for the
// duration of the window, so every rule would read "missing" on a perfectly
// healthy cold load, and the cold path refreshes statistics at its own
// finalize anyway.
const plannerStatsBulkWindowReason = "bulk_window_active"

// Reasons a stale verdict can come back UNREFRESHED. None of them is a
// failure, and none of them stamps the ledger or arms the anti-loop guard: the
// verdict stands, the resume cursor keeps the indexes already finished, and the
// next pipeline boundary continues from there.
//
//	writer_busy:<idx>  the store's write gate was held when this index's turn
//	                   came. Indexes already analyzed keep their new rows.
//	raced              another caller is refreshing right now, or has already
//	                   cleared the verdict this call was about to act on.
//	canceled:<idx>     the caller's context expired — between two indexes, or
//	                   underneath the connection acquisition / ANALYZE of this
//	                   one. A caller that walked away is not a defect in the
//	                   store, so it must not arm the anti-loop guard.
//	timeout:<idx>      this index's own ANALYZE outlived plannerStatsIndexTimeout
//	                   while the caller's context was still live. Deferred like
//	                   any other stop — but counted per index, because an index
//	                   that times out at EVERY boundary would otherwise cost the
//	                   full timeout forever (see plannerStatsIndexTimeoutRetries).
//	budget:<idx>       the pass had already spent plannerStatsPassBudget when
//	                   this index's turn came. The wider gates the caller holds
//	                   are what this bounds.
//	no_indexes         the critical indexes vanished from the schema between
//	                   the verdict and the work list (a bulk window opening on
//	                   another goroutine); defensive, unreachable in practice.
const (
	plannerStatsWriterBusyReason = "writer_busy:"
	// plannerStatsEditCycleReason: an edit-driven mutation cycle holds the
	// build lane (SetBuildLaneBusy). No ANALYZE starts inside an edit window,
	// and one in flight when a cycle takes the lane is interrupted.
	plannerStatsEditCycleReason = "edit_cycle:"
	plannerStatsRacedReason     = "raced"
	plannerStatsCanceledReason  = "canceled:"
	plannerStatsTimeoutReason   = "timeout:"
	plannerStatsBudgetReason    = "budget:"
	plannerStatsNoIndexesReason = "no_indexes"
)

// plannerStatsPassBudget bounds the wall-clock ONE cooperative pass spends
// before handing the rest of its work list to the next boundary.
//
// It is what makes the cost a caller pays under its WIDER gates a bounded
// number rather than "however long the whole family takes". The bound is not
// the budget alone: the check is made before STARTING another index, so one
// ANALYZE may overshoot it, and a completed pass that deleted a stale stat row
// then takes one more bounded hold to reload sqlite_schema. A boundary
// therefore pays at most plannerStatsPassBudget + one index's ANALYZE + one
// reload, the latter two each capped by plannerStatsIndexTimeout. Convergence
// is not sacrificed — the resume cursor carries the remaining indexes to the
// next boundary.
//
// Four reads the budget does NOT cover, and a caller sizing its own gate hold
// should know all of them, because they run BEFORE the clock starts and take
// no gate at all:
//
//	the first health probe        schema join + stat rows + the repo_index_state
//	                              scan + the capped receiver count (the only
//	                              part costing table probes rather than seeks)
//	the narrower re-probe         the same minus the receiver count, unless the
//	                              verdict under re-check is the receiver's own
//	the present-index list        plannerStatsPresentIndexList, one schema join
//	the stat-row set              plannerStatsIndexesWithStats, one sqlite_stat1
//	                              scan, used to scope a "missing:" work list
//
// All four are read-pool reads, so they cost latency under the caller's wider
// locks and nothing under the store's own.
//
// A var, not a const, for one reason: a test sets it to zero to prove a
// mid-list deferral keeps the cursor. Nothing in production writes it.
var plannerStatsPassBudget = 3 * time.Second

// SetPlannerStatsPassBudgetForTest overrides the pass budget and returns the
// call that restores it. It exists for tests in OTHER packages and has no
// production caller.
//
// The budget is the only deterministic way to produce a DEFERRED cooperative
// pass — every other stop needs a racing writer, an expiring context or an
// ANALYZE slow enough to time out — and a deferred pass is the only state in
// which the held base exists to be observed. internal/mcp needs exactly that to
// prove index_health keeps reporting a verdict through the window. An
// export_test.go hook would not reach it: that file is compiled only into this
// package's own test binaries.
//
// Not safe against a live daemon: the var it writes is read by every concurrent
// pass, and nothing here synchronises it. Set it, run the pass, restore it.
func SetPlannerStatsPassBudgetForTest(budget time.Duration) (restore func()) {
	prior := plannerStatsPassBudget
	plannerStatsPassBudget = budget
	return func() { plannerStatsPassBudget = prior }
}

// plannerStatsIndexTimeout bounds ONE index's ANALYZE, and is deliberately
// strictly below sqliteBusyRetryWindow() (15 s).
//
// The bounded-gate writers — reindex_set.go and
// unresolved_edge_identity_batches.go — give the store's write gate that window
// and then DROP their batches. A single hold that could outlive it would make
// this mechanism discard concurrent rebind work, which is a correctness loss
// paid for a planning refinement. Timing out is cheap by comparison — but it is
// not free, and the cost is worth stating: modernc's driver marks a connection
// whose statement was interrupted as invalid, so database/sql discards it from
// the pool. The writer pool holds exactly ONE connection, so a timed-out
// ANALYZE costs that connection's warm page cache and a reopen on the next
// write (~0.4 ms; the pragmas are restored from the DSN, so nothing is lost but
// the cache). The interrupted ANALYZE itself rolls back cleanly — sqlite_stat1
// keeps the rows it had. The index is then deferred, the cursor keeps it, and
// the next boundary retries it.
//
// A var for the same reason as the budget above: a test shortens it. It is
// handed to context.WithTimeout UNGUARDED, and deliberately so — a test sets it
// to a negative duration to make the per-index context dead at creation on any
// OS, which a "clamp non-positive to the default" guard would silently undo
// (see TestEnsurePlannerStatsFresh_PerIndexTimeoutDefersThenSettles). Nothing
// in production writes it, so no production caller can reach a non-positive
// value.
var plannerStatsIndexTimeout = 10 * time.Second

// plannerStatsIndexTimeoutRetries is how many CONSECUTIVE timeouts on the same
// index one cursor tolerates before the pass takes the failure arm.
//
// Without it a pathological index — one whose ANALYZE genuinely cannot finish
// in plannerStatsIndexTimeout — would cost a full timeout at EVERY pipeline
// boundary for the life of the daemon, and the verdict would never clear. After
// this many the pass arms the settled guard, which re-arms only when the store
// materially grows, so the cost falls back to once per doubling.
const plannerStatsIndexTimeoutRetries = 3

// plannerStatsCounterQuery sums the per-repository counters the indexer
// persists on the way in.
//
// It deliberately carries NO view_gen filter, which is what separates it from
// stmtIndexStateTotals. SetRepoIndexState writes the handle's own generation,
// and SparseGenerationBuilder builds a full Indexer on a generation handle, so
// each published view generation leaves its own counter row. sqlite_stat1
// describes the PHYSICAL index — every generation's rows in one B-tree — so a
// view-scoped total undercounts it by exactly the factor observed on the
// lead's store (592k believed against 1.69M physical across 16 generations).
// Retirement keeps the sum honest: repo_index_state is a view_gen sidecar, so
// a retired generation's counter row is swept with its payload.
const plannerStatsCounterQuery = `SELECT COUNT(*), COALESCE(SUM(node_count),0), COALESCE(SUM(edge_count),0) FROM repo_index_state`

var errPlannerStatsNoCore = errors.New("store_sqlite: planner statistics unavailable on a closed store handle")

// plannerStatsBaseline is the high-water mark a growth verdict is measured
// against: the counter totals as of the last refresh (or of the first probe
// that found the statistics fresh).
//
// The baseline is what makes the mechanism converge. Measuring growth against
// the BELIEVED cardinality cannot: PRAGMA analysis_limit makes ANALYZE's
// leading token an extrapolation rather than a count, and the counter sum can
// legitimately exceed the nodes cardinality (a leftover empty-prefix row on a
// store that later became multi-repo is summed on top of the per-repo rows).
// Either one leaves actual >= factor * believed true immediately AFTER a
// refresh, which turns a rate limit into a permanent tens-of-seconds ANALYZE
// on a loop. Measured against the size the store was when it was last
// analyzed, the predicate is monotone in real growth and self-terminating: a
// refresh always moves the baseline to the size that triggered it.
//
// It is a high-water mark that DECAYS. The counter totals fall as well as
// rise — a retired view generation takes its counter row with it, an untracked
// repository takes its own — and a baseline frozen at the store's largest
// historical size would then need the store to grow back past it AND double
// again before any verdict fired. So a probe that reads totals below the
// recorded baseline lowers the baseline to what it just read (see
// plannerStatsHealth), and later growth is measured from that new floor. The
// convergence argument is unaffected: lowering only ever makes the mechanism
// more willing to notice growth, and a refresh still moves the anchor to the
// size that triggered it.
//
// The two components are anchored SEPARATELY, each by its own family's last
// completed refresh, and that separation is load bearing. A refresh is scoped
// to one family — a nodes verdict re-analyzes the indexes on nodes and no row
// describing an index on edges — so a completed pass may only claim that the
// family it rebuilt now describes a store of this size. Moving both components
// on a nodes pass is the bug this split exists to prevent: indexing grows nodes
// and edges together, the rules read nodes first and the nodes verdict wins, so
// the edges anchor would be pushed to the very total that was about to produce
// the edges verdict — and edges_by_kind / edges_by_from_line /
// edges_by_from_line_kind would stay frozen at their cold-load figures forever
// on a proportionally growing store, with nothing at Open to repair them.
//
// Anchored per family, a store that doubles both converges over consecutive
// boundaries rather than within one: the nodes verdict fires at boundary N and
// moves only the nodes anchor, the edges verdict fires at N+1 and moves only
// the edges anchor, and N+2 is quiescent. Every boundary still pays at most a
// budget plus one index and one bounded reload, and each anchor is still moved
// only by an ANALYZE that actually rebuilt that family's rows.
//
// seeded is therefore a property of the STRUCT, not of either component, and a
// component of zero means "this family has no anchor yet" rather than "this
// family held nothing". plannerStatsStaleReason reads a zero base by falling
// back to the believed row — the pre-seed rule — so a half-anchored baseline
// judges the anchored family from its anchor and the other from its statistics,
// which is exactly what a store whose first Ensure was already stale needs
// (see notePlannerStatsRefresh).
type plannerStatsBaseline struct {
	nodes  int64
	edges  int64
	seeded bool
}

// plannerStatsAttempt records the last refresh this process attempted, so an
// identical verdict over an unchanged store cannot be paid for twice. This is
// the guard the growth baseline cannot provide: a "missing:" verdict that a
// refresh fails to clear (an ANALYZE that errors, an index whose row the
// engine refuses to write) would otherwise re-fire at every call site
// forever. It is deliberately state-based rather than a time-based cooldown —
// a cooldown suppresses the very refresh this mechanism exists for when a
// second repository is tracked minutes after the first.
type plannerStatsAttempt struct {
	reason string
	nodes  int64
	edges  int64
	made   bool
}

// plannerStatsCursor is where an unfinished cooperative pass stopped.
//
// A pass can stop mid work list for four reasons that are all deferrals — a
// busy write gate, an exhausted pass budget, an expired caller context, a
// per-index timeout — and every one of them leaves indexes it never reached.
// Without a cursor the next boundary rebuilds the same work list and starts at
// the HEAD: on a store whose gate is contended at the same point every time,
// index 1 is re-analyzed forever and index 3 is never reached. That is a
// mechanism that does not converge, which is the whole property this file
// claims. With a cursor the next boundary resumes AFTER the last completed
// index, so the family is finished across boundaries.
//
// key identifies WHAT the cursor is a position in: the verdict's reason key
// plus the work list it produced. Anything else — a different verdict, a work
// list changed by an index appearing or vanishing — is a different pass, and
// the cursor resets rather than skipping indexes that were never done for THIS
// list. A completed pass resets it too, so a later identical verdict (a store
// that grew again the same way) starts from the head as it should.
//
// The reason KEY, not the reason: the measured figures are stripped, so a
// verdict whose counters moved by one indexed file still resumes rather than
// restarting. The price is bounded and deliberate — a store that grows again
// while a pass is deferred resumes after indexes analyzed at the SMALLER size,
// so those rows can trail the store by up to one doubling until the pass
// completes and the cursor clears. Keying on the full reason instead would
// reset the cursor on every indexed file and buy no resumption at all.
//
// That one-doubling bound holds across a verdict that CLEARS and later fires
// again, too, which it would not if the position merely sat here waiting for a
// matching key. A verdict can clear without any pass completing — another
// caller refreshed, or the baseline decayed under a shrinking store — and a
// position kept through that would make the identical verdict months later
// resume past indexes analyzed at a size nobody can bound. So every probe that
// finds the store NOT stale, and every failed pass, retires the cursor outright
// (resetPlannerStatsCursor); only an uninterrupted sequence of deferrals over a
// standing verdict keeps it.
//
// Alternating verdicts cannot wipe each other's progress either, and it is
// worth saying why the single-slot cursor is enough for two families. A store
// that outgrew both is judged nodes-first: the rules stop at the first hit, so
// the edges verdict is unreachable while the nodes one stands. A deferred nodes
// pass therefore meets the SAME verdict — and so the same key — at every
// following boundary, and keeps resuming. The nodes verdict clears only when a
// nodes pass COMPLETES, which is exactly the moment its position is worth
// nothing and the pass retires it itself. Only then does the edges verdict
// surface, over a cursor that is already empty.
//
// "The verdict clears only when its pass COMPLETES" is true in the ANCHORED
// regime for free and in the UNANCHORED one only because of held: a family
// whose baseline component is still zero is judged against its BELIEVED row
// (plannerStatsStaleReason's pre-seed rule), and the work list rebuilds the
// index that row is read off FIRST. So the first deferral of an unanchored pass
// would otherwise repair its own verdict: the next boundary reads a believed
// row that now agrees with the store, finds nothing stale, and — through the
// not-stale probe — retires this cursor and seeds the component from the
// current totals, leaving the family's other indexes (nodes_go_receiver_type
// among them, whose capped probe cannot fire a verdict of its own once it
// believes plannerStatsHealthProbeCap or more) frozen on their old rows until
// the store doubles again. That is the boot regime of every existing store, and
// the regime of the edges family at the boundary right after a completed nodes
// pass.
//
// held closes it. When a verdict fires for a family with no anchor, the base it
// was judged against — the number plannerStatsStaleReason measured it against
// and handed back, which under the pre-seed rule is the believed value at the
// moment it fired — is recorded here, and plannerStatsStaleReason judges that
// family against the RECORDED base for as long as this cursor stands, instead
// of against the row the pass is busy rewriting. The verdict therefore survives its own partial repair and
// keeps resuming until the pass completes, at which point the cursor is retired
// and notePlannerStatsRefresh anchors the component from the completed pass as
// usual. seedPlannerStatsBaseline honours the same window: it will not fill a
// component whose family still has a held base standing here, because filling
// it would silently replace the recorded base with the current totals and clear
// the verdict just as surely.
//
// Everything that retires a cursor retires its held base with it, which is the
// property that keeps this from being a way to pin a verdict open: a genuinely
// cleared verdict (the baseline decayed, another caller's pass completed) still
// goes through the not-stale probe's resetPlannerStatsCursor, and a failed pass
// still goes through the failure arm's.
//
// timeouts counts CONSECUTIVE timeouts per index within one cursor; a
// successful analyze of that index clears its counter. It rides on the cursor
// rather than on the store so it cannot outlive the verdict it describes.
//
// Guarded by plannerStatsMu.
type plannerStatsCursor struct {
	key      string
	done     map[string]bool
	timeouts map[string]int
	held     plannerStatsHeldBase
}

// plannerStatsHeldBase is the base an UNANCHORED family's verdict was judged
// against, kept alive for the duration of the pass that answers it.
//
// A zero component means "this family had an anchor, or produced no verdict" —
// there is nothing to hold — so a zero is never consulted and never blocks a
// seed. See plannerStatsCursor for why the non-zero case exists at all.
type plannerStatsHeldBase struct {
	nodes int64
	edges int64
}

// syncBulkWindowLocked mirrors the writeMu-guarded bulk-window state into an
// atomic. The read-only health probe takes no lock by design, and reading
// bulkConn/coordinatedBulkLoad unguarded would be a data race; this is the one
// value it consults. Call it after every mutation of either field.
//
// The mirror LAGS the window's real edges, by construction and not by
// oversight. beginBulkLoadLocked runs its DROP INDEX loop before it assigns
// bulkConn, and sealBulkIndexesLocked rebuilds the indexes before its deferred
// sync clears the flag, so a probe can observe either skew. Both are
// safe-direction:
//
//   - flag false while the indexes are already dropped: the probe finds them
//     absent from the schema, and plannerStatsStaleReason never makes an
//     absent index a verdict, so the probe reports a clean store instead of
//     alarming about statistics no ANALYZE could write.
//   - flag true while the indexes are back: the probe reports
//     bulk_window_active, which is one boundary's worth of conservatism and
//     the next one re-asks.
//
// And the skew can never let an ANALYZE run inside a real window, because the
// atomic is not what guards that: plannerStatsHoldLocked re-reads the REAL
// bulkConn / coordinatedBulkLoad fields under writeMu, after the gate is taken,
// and turns a window it finds there into a deferral.
func (s *storeCore) syncBulkWindowLocked() {
	s.bulkWindowOpen.Store(s.bulkConn != nil || s.coordinatedBulkLoad)
}

// recycleStatsReadPool drops the read pool's idle connections after
// sqlite_stat1 has been rewritten.
//
// SQLite bumps the schema cookie only when sqlite_stat1 is first CREATED —
// never when its rows are rewritten. Every reader connection already open
// (openSQLiteReadPool pings one into existence at Open) therefore keeps the
// pre-refresh statistics for as long as it lives, which on a long-running
// daemon is forever. Dropping the idle connections makes the next reader a
// fresh physical connection that loads the new rows on open.
//
// Skipped when the two pools are the same handle: an in-memory store would
// lose the database with its last connection.
func recycleStatsReadPool(readDB, writerDB *sql.DB) {
	if readDB == nil || readDB == writerDB {
		return
	}
	readDB.SetMaxIdleConns(0)
	readDB.SetMaxIdleConns(sqliteMaxIdleConns)
}

// PlannerStatsHealth reports what the query planner believes about this store
// against what the store can cheaply prove, without refreshing anything and
// without taking the write gate.
//
// Everything it reads goes through the read pool: the critical-index list from
// sqlite_schema, the believed cardinalities from sqlite_stat1 (read as
// ordinary table data, so a reader's own cached statistics cannot distort the
// answer), the persisted per-repository counters, and one bounded probe of the
// receiver index. That is a handful of leftmost seeks, which is what lets
// EnsurePlannerStatsFresh call it before deciding whether any lock is worth
// taking.
//
// "Read-only" is a statement about the DATABASE, not about the process: the
// probe lowers the in-memory growth anchor when the counters have fallen below
// it (see decayPlannerStatsBaseline). That takes plannerStatsMu and nothing
// else, issues no ANALYZE, and can only ever make a later verdict MORE willing
// to notice growth — so an index_health poll cannot suppress a refresh the
// store is owed.
func (s *Store) PlannerStatsHealth(ctx context.Context) (graph.PlannerStatsFreshness, error) {
	health, _, err := s.plannerStatsHealthLogged(ctx, true)
	return health, err
}

// plannerStatsJudgedBases is the base plannerStatsStaleReason actually measured
// each family against on one probe: the family's standing anchor, the held base
// of an unfinished pass, or — when it has neither — the believed row the
// pre-seed rule falls back to. Zero means the family's rule consulted no base
// at all (its index is absent, its cardinality is unknowable, or the believed==0
// rule fired, which reads no base).
//
// It exists so plannerStatsUnanchoredVerdictBase can record the base a verdict
// was JUDGED against rather than re-derive it from the current believed row.
// The two agree on a fresh cursor and can disagree the moment one does not — a
// resumed pass has already rewritten that row — and an invariant that holds by
// construction is worth more here than one that holds by argument.
//
// A local return rather than a field on graph.PlannerStatsFreshness: the number
// is an implementation detail of this file's rules, nothing that reports the
// health payload has a use for it, and the receiver family (which never carries
// an anchor and never holds a base) would need a third component that means
// nothing.
type plannerStatsJudgedBases struct {
	nodes int64
	edges int64
}

// plannerStatsHealthLogged is PlannerStatsHealth with the receiver probe made
// optional, and carries the once-per-store failure logging both callers want.
//
// probeReceivers=false costs only the schema join, the stat rows and the
// counter scan. See the re-probe in EnsurePlannerStatsFresh for the one caller
// that skips it, and why skipping is safe only there.
func (s *Store) plannerStatsHealthLogged(ctx context.Context, probeReceivers bool) (graph.PlannerStatsFreshness, plannerStatsJudgedBases, error) {
	health, bases, err := s.plannerStatsHealth(ctx, probeReceivers)
	switch {
	case err == nil:
		// Re-arm: the next failure after a recovery is news again. A nil error
		// is itself the proof the core is there — plannerStatsHealth returns
		// errPlannerStatsNoCore before it reads anything else.
		s.plannerStatsProbeFailed.Store(false)
	case errors.Is(err, errPlannerStatsNoCore):
		// A closed handle, not a defect in the probe.
	default:
		// The probe reads the catalog, sqlite_stat1 and one INDEXED BY count.
		// A failure there is how a drifted partial-index predicate announces
		// itself — SQLite refuses INDEXED BY when the index cannot serve the
		// stated WHERE clause — and the only other trace it leaves is an
		// index_health payload with the planner_stats key silently absent.
		//
		// Say it ONCE per store until the probe works again. The probe runs at
		// every pipeline boundary and on every index_health rebuild, so a
		// persistent failure — a drifted predicate is permanent until someone
		// changes the DDL — would otherwise write the same line thousands of
		// times a day into the daemon log.
		if s.plannerStatsProbeFailed.CompareAndSwap(false, true) {
			log.Printf("store_sqlite: planner stats probe failed error=%q", err)
		}
	}
	return health, bases, err
}

func (s *Store) plannerStatsHealth(ctx context.Context, probeReceivers bool) (graph.PlannerStatsFreshness, plannerStatsJudgedBases, error) {
	var bases plannerStatsJudgedBases
	if s.coreless() {
		return graph.PlannerStatsFreshness{}, bases, errPlannerStatsNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}

	health := graph.PlannerStatsFreshness{
		Checks:    s.plannerStatsChecks.Load(),
		Refreshes: s.plannerStatsRefreshes.Load(),
	}
	s.plannerStatsMu.Lock()
	health.LastRefreshAt = s.plannerStatsLastRefresh
	health.LastRefreshReason = s.plannerStatsLastReason
	baseline := s.plannerStatsBaseline
	s.plannerStatsMu.Unlock()

	if s.bulkWindowOpen.Load() {
		health.Reason = plannerStatsBulkWindowReason
		return health, bases, nil
	}

	present, err := s.plannerStatsPresentIndexes(ctx)
	if err != nil {
		return health, bases, err
	}
	nodesIndex, nodesBelieved := s.plannerStatsNodesBelieved(ctx, present)
	edgesIndex := ""
	if present[plannerStatsEdgesIndex] {
		edgesIndex = plannerStatsEdgesIndex
	}
	receiverIndex := ""
	if present[plannerStatsReceiverIndex] {
		receiverIndex = plannerStatsReceiverIndex
	}

	counterRows, nodeTotal, edgeTotal, err := s.plannerStatsCounters(ctx)
	if err != nil {
		return health, bases, err
	}
	health.Nodes.Known = counterRows > 0
	health.Nodes.Actual = nodeTotal
	health.Edges.Known = counterRows > 0
	health.Edges.Actual = edgeTotal
	health.Nodes.Believed = nodesBelieved
	// Schema presence, reported per relation. It is the same membership the
	// verdict rules gate on, published so a reporter can omit a relation whose
	// index is not there rather than print a believed=0 that reads as a
	// defect. The bulk-window return above leaves all three false, which is
	// the honest answer while the droppable indexes are physically gone.
	health.Nodes.Present = nodesIndex != ""
	health.Edges.Present = edgesIndex != ""
	health.Receivers.Present = receiverIndex != ""
	if edgesIndex != "" {
		health.Edges.Believed = plannerStatsBelievedRows(ctx, s.db, edgesIndex)
	}
	// The receiver probe is the one part of this read that is not a handful of
	// leftmost seeks: it is capped, but the cap is still a couple of hundred
	// WITHOUT ROWID table probes. probeReceivers=false leaves the cardinality
	// at its zero value and Known=false, which the rules read as "cannot be
	// judged" — so a caller that skips it must already know the verdict it
	// cares about is not the receiver index's.
	if receiverIndex != "" && probeReceivers {
		health.Receivers.Believed = plannerStatsBelievedRows(ctx, s.db, receiverIndex)
		actual, capped, err := s.plannerStatsReceiverCount(ctx, receiverIndex, health.Receivers.Believed)
		if err != nil {
			return health, bases, err
		}
		health.Receivers.Actual = actual
		health.Receivers.Bounded = true
		health.Receivers.Known = !capped
	}

	// Relation-is-populated evidence for the believed==0 rule, taken from the
	// CORPUS and never from the counters.
	//
	// The counters cannot answer it. persistRepoIndexState writes them from
	// the shadow's own count before the shadow is drained to disk, so on the
	// shadow path there is a real window in which the counters describe rows
	// the physical tables do not hold yet. Believing them there makes the rule
	// ANALYZE an empty table — which writes no stat row at all, leaving the
	// verdict permanently unfixable. And they are absent entirely on a fresh
	// cold load, which is the case the rule exists for. EXISTS is a leftmost
	// seek and is asked only when the statistics are already missing.
	nodesPopulated := nodesIndex != "" && health.Nodes.Believed == 0 &&
		s.plannerStatsRelationHasRows(ctx, "nodes")
	edgesPopulated := edgesIndex != "" && health.Edges.Believed == 0 &&
		s.plannerStatsRelationHasRows(ctx, "edges")

	// Decay the high-water mark before judging against it. A retired view
	// generation's counter row is swept with its payload, and an untracked
	// repository's row goes with the repository, so the totals fall as well as
	// rise. Left frozen at the largest size the store ever reported, the
	// anchor would demand the store climb back to it AND double again before
	// any growth registered — on a workspace that churns generations, that is
	// a store that never re-analyzes. Lowering it to what this probe actually
	// measured keeps the rule a statement about growth from the CURRENT floor.
	if counterRows > 0 {
		baseline = s.decayPlannerStatsBaseline(nodeTotal, edgeTotal)
	}
	nodesBase := int64(0)
	edgesBase := int64(0)
	if baseline.seeded {
		nodesBase, edgesBase = baseline.nodes, baseline.edges
	}
	// A family with no anchor is judged against its BELIEVED row, and the pass
	// answering such a verdict rebuilds the very index that row is read off
	// first — so without this the verdict would be cleared by its own first
	// completed index and the rest of the family would never be reached. While
	// an unfinished pass stands for that family, its cursor carries the base the
	// verdict actually fired on, and that is what the rule is measured against.
	// Only a component with no anchor can be held, so this can never override a
	// real one. See plannerStatsCursor.
	held := s.plannerStatsHeldBase()
	if nodesBase == 0 {
		nodesBase = held.nodes
	}
	if edgesBase == 0 {
		edgesBase = held.edges
	}
	// Each rule reports the base it measured its family against, so the pass
	// that answers the verdict can hold THAT number rather than re-read the
	// believed row and argue the two must agree. The receiver rule's base is
	// discarded: nodes_go_receiver_type belongs to no family here and nothing
	// is ever held for it (see plannerStatsUnanchoredVerdictBase).
	health.Reason, bases.nodes = plannerStatsStaleReason(nodesIndex, health.Nodes, nodesBase, nodesPopulated)
	if health.Reason == "" {
		health.Reason, bases.edges = plannerStatsStaleReason(edgesIndex, health.Edges, edgesBase, edgesPopulated)
	}
	if health.Reason == "" {
		// The receiver index carries no counter, so its growth rule is
		// measured against the believed count alone and is bounded by the
		// probe cap. Its real job is PR 1's: catch the poisoned near-zero row
		// that inverts the receiver-rebind join order.
		health.Reason, _ = plannerStatsStaleReason(receiverIndex, health.Receivers, 0, health.Receivers.Actual > 0)
	}
	if health.Reason == "" {
		// The families above are judged by one sentinel row each. A planner
		// index outside the sentinels can still have no row: the planner then
		// chooses it by the default cost model (a store fresh from a whole
		// index had rows for nodes_by_kind and none for nodes_by_name,
		// nodes_by_repo or nodes_by_repo_language_name, and read fresh). So:
		// every present planner index that holds rows and has no statistics
		// row is owed.
		health.Reason = s.plannerStatsMissingIndex(ctx, present)
	}
	health.Stale = health.Reason != ""
	return health, bases, nil
}

// plannerStatsStaleReason applies the two rules to one relation and names the
// verdict, or returns "" when the relation is fine or cannot be judged.
//
// judged is the base the growth rule actually measured against — the caller's
// baseline when it has one, the believed row under the pre-seed rule — and is
// returned whether or not the rule fired, so a caller never has to re-derive
// it. Zero means no base was consulted at all: the index is absent, the
// cardinality is unknowable, or the believed==0 rule answered, and that rule
// reads no base.
//
// An index absent from the schema (index == "") is never a verdict. The
// droppable critical indexes physically do not exist for the duration of a
// bulk window, and "missing statistics for an index that is not there" is a
// state no ANALYZE can repair — reporting it would alarm on every cold load
// and refresh forever.
func plannerStatsStaleReason(index string, card graph.PlannerStatsCardinality, baseline int64, populated bool) (reason string, judged int64) {
	if index == "" {
		return "", 0
	}
	if card.Believed == 0 {
		if populated {
			return "missing:" + index, 0
		}
		return "", 0
	}
	if !card.Known {
		return "", 0
	}
	base := card.Believed
	if baseline > 0 {
		base = baseline
	}
	if card.Actual >= plannerStatsGrowthFactor*base {
		return fmt.Sprintf("growth:%s believed=%d actual=%d base=%d", index, card.Believed, card.Actual, base), base
	}
	return "", base
}

// EnsurePlannerStatsFresh refreshes sqlite_stat1 when — and only when — the
// store has outgrown what the planner believes about it.
//
// The fresh path, which is the steady state at every call site, takes NO lock:
// it is a handful of catalog and counter seeks on the read pool.
//
// The stale path is cooperative rather than exclusive, for the reason the file
// header gives: every call site holds a wider gate already, so this one must
// never queue on the store's write gate, must never hold it for longer than
// one index, and must bound what the boundary pays overall. It claims a
// single-in-flight flag, re-evaluates the verdict lock-free (another caller may
// have cleared it), scopes the work to the family the verdict named — that
// family's own index first — and then try-locks the gate once PER INDEX under a
// per-index timeout, stopping when the pass budget is spent.
//
// Every one of those stops is a DEFERRAL: a "writer_busy:" / "budget:" /
// "timeout:" / "canceled:" reason, no ledger stamp and no guard armed. The
// verdict stands, the indexes already analyzed keep their new rows, and the
// resume cursor makes "the next boundary picks up where this one stopped"
// literally true — it continues after the last completed index rather than
// restarting at the head.
//
// Never call it while holding writeMu.
//
// Failure is returned but is never fatal for any call site: a store that could
// not refresh its statistics plans through the cost model it already had.
func (s *Store) EnsurePlannerStatsFresh(ctx context.Context) (graph.PlannerStatsFreshness, error) {
	if s.coreless() {
		return graph.PlannerStatsFreshness{}, errPlannerStatsNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checks := s.plannerStatsChecks.Add(1)

	health, err := s.PlannerStatsHealth(ctx)
	health.Checks = checks
	if err != nil {
		// Nothing is anchored on a probe that failed. A failed probe leaves
		// health half-filled — plannerStatsHealth sets the counter figures
		// before the reads that can still fail underneath it, so Nodes.Known is
		// already true when the capped receiver count errors on a drifted
		// partial-index predicate — and seeding the anchor from those figures
		// would freeze the growth rule at a size no probe ever fully measured.
		return health, err
	}
	if !health.Stale {
		// The verdict a resumable position belongs to is gone, and no pass
		// of ours completed it: another caller refreshed, or the baseline
		// decayed under a shrinking store. Retire the position rather than
		// let it wait for a matching key — a later identical verdict is a
		// different store, and resuming past indexes analyzed at the old
		// size would leave those rows trailing by an unbounded amount.
		//
		// Skipped while another caller holds the claim: that caller's pass
		// is mid-list, its own completion will retire the cursor, and this
		// probe may be reading a verdict it has already cleared. The load
		// is racy and deliberately so — losing the race costs a pass that
		// restarts at the head, never a wrong answer.
		if !s.plannerStatsRefreshInFlight.Load() {
			s.resetPlannerStatsCursor()
		}
		// Seeded AFTER the retire, and the order is the whole point.
		// seedPlannerStatsBaseline refuses to fill a component whose family
		// still has a held base standing on the cursor — filling it would
		// clear a verdict the standing pass is still working through. But a
		// verdict that genuinely cleared here (the baseline decayed under a
		// shrinking store, another caller finished the family) has just had
		// that cursor retired one statement ago, so nothing is owed the held
		// base any more and the component is fillable NOW. Seeding first would
		// read the base the retire is about to drop, decline the fill, and
		// leave the family unanchored until the next not-stale probe — one
		// boundary of measuring growth against the believed row for no reason.
		s.seedPlannerStatsBaseline(health)
		return health, nil
	}

	// One refresh in flight per store. A second caller must not wait for the
	// first: it holds its own wider gate while it waits, and the verdict it is
	// about to act on is the one already being acted on.
	if !s.plannerStatsRefreshInFlight.CompareAndSwap(false, true) {
		health.Reason = plannerStatsRacedReason
		return health, nil
	}
	defer s.plannerStatsRefreshInFlight.Store(false)

	// Re-read, still lock-free: another pipeline may have refreshed between
	// the probe above and the claim, and paying a second ANALYZE for the same
	// growth is the double-refresh this closes. Under the old exclusive shape
	// this had to happen under the gate; it no longer does, because the gate
	// is not what serialises refreshes any more — the flag above is.
	//
	// The re-probe asks a NARROWER question than the first one: not "what is
	// this store's verdict" but "is the verdict I just read still standing".
	// So it skips the capped receiver probe — the one part of the read that
	// costs table probes rather than seeks — leaving the schema join, the stat
	// rows and the counter scan. It cannot be skipped when the verdict under
	// re-check IS the receiver index's own: without the probe the receiver
	// rules cannot fire, the re-read would report a clean store, and the pass
	// would return "raced" over a verdict nobody cleared — forever.
	probeReceivers := strings.Contains(plannerStatsReasonKey(health.Reason), plannerStatsReceiverIndex)
	fresh, bases, err := s.plannerStatsHealthLogged(ctx, probeReceivers)
	fresh.Checks = checks
	if err != nil {
		return fresh, err
	}
	if !probeReceivers {
		// Publish the first probe's receiver figures rather than the zero
		// values the skip leaves behind: they were measured microseconds ago,
		// and a zeroed receiver block in the returned verdict reads as exactly
		// the poisoned near-zero state issue #651 is about.
		fresh.Receivers = health.Receivers
	}
	if !fresh.Stale {
		fresh.Reason = plannerStatsRacedReason
		// Same reasoning as the first probe's, and here the claim is ours, so
		// the retire is unconditional: the verdict cleared without any pass of
		// ours completing it, and a position into a pass that will never run
		// must not be handed to a later identical verdict. Retire-then-seed for
		// the same reason as there, too: the fill is owed to a family whose
		// held base this very statement just dropped.
		s.resetPlannerStatsCursor()
		s.seedPlannerStatsBaseline(fresh)
		return fresh, nil
	}
	if settled, prior := s.plannerStatsAttemptSettled(fresh); settled {
		fresh.Reason = "settled:" + prior
		return fresh, nil
	}

	present, err := s.plannerStatsPresentIndexList(ctx)
	if err != nil {
		return fresh, err
	}
	work := plannerStatsWorkList(fresh.Reason, present, s.plannerStatsIndexesWithStats(ctx))
	if len(work) == 0 {
		fresh.Reason = plannerStatsNoIndexesReason
		return fresh, nil
	}

	// Recorded now, on the cursor the pass is about to open, because the first
	// index this pass analyzes is the one the verdict was read off: from the
	// next boundary on, the believed row no longer says what this verdict fired
	// on. Ignored outright when the family already has an anchor, and kept by a
	// RESUMING pass rather than recomputed — see plannerStatsCursor. bases is
	// what the re-probe's own rules measured, so the base recorded here is the
	// one the verdict was judged against by construction.
	held := s.plannerStatsUnanchoredVerdictBase(fresh, bases)
	insist := s.plannerStatsInsist(ctx, fresh.Reason)
	started := time.Now()
	analyzed, deferred, refreshErr := s.cooperativePlannerStatsRefresh(ctx, plannerStatsCursorKey(fresh.Reason, work), held, work, insist)
	elapsed := time.Since(started)

	// A deferred pass is not a failure and not a refresh. It leaves the ledger
	// and the anti-loop guard untouched on purpose: the verdict is still true,
	// and the next pipeline boundary must be free to act on it immediately
	// rather than wait out a guard armed by a pass that did nothing wrong.
	if deferred != "" {
		fresh.Reason = deferred
		if deferred == plannerStatsBulkWindowReason {
			// Not a judgeable state at all — the droppable critical indexes
			// are physically gone for the window's duration.
			fresh.Stale = false
		}
		// Never an error: cooperativePlannerStatsRefresh names a deferral only
		// on the paths that return a nil err, and returning refreshErr here
		// would read as "a deferral can also fail", which it cannot.
		return fresh, nil
	}
	if refreshErr != nil {
		// Record the ATTEMPT and nothing else. The anti-loop guard still has
		// to arm — a verdict a refresh cannot clear must not be retried at
		// every call site for the life of the daemon, and growth past the
		// recorded totals re-arms it either way. But the rest of the ledger
		// describes a rebuild that did not happen: stamping LastRefreshAt
		// would tell index_health the statistics were rebuilt at a moment
		// they were not, and moving the growth baseline to the totals that
		// triggered the failure would suppress the next verdict over a store
		// whose statistics are still wrong.
		s.notePlannerStatsAttempt(fresh)
		// And retire the resume position. It is the right call in both regimes,
		// but for two different reasons, and only one of them ends in
		// "settled:".
		//
		// ANCHORED family: the verdict is judged against an anchor this failed
		// pass did not move, so it fires again at the next boundary and the
		// guard armed above declines it — "settled:<key>" — until the store has
		// doubled. The pass this cursor is a position in will therefore be
		// declined rather than continued, and keeping the position would hand
		// it, arbitrarily later, to the first pass whose key happens to match,
		// resuming past indexes analyzed at a size nothing bounds any more.
		//
		// UNANCHORED family: the verdict is judged against the BELIEVED row,
		// and dropping the held base is what puts it back in charge. The
		// verdict's own index is analyzed FIRST, so unless it was the index
		// that failed, this pass has already rewritten that row: the next probe
		// reads a row that now agrees with the store, reports NOT stale, seeds
		// the anchor from the current totals and returns without ever
		// consulting the guard. "settled:" is never observed on this path, the
		// verdict is gone rather than declined, and the family's remaining
		// indexes — the receiver index among them — keep their pre-growth rows
		// until the store doubles against that fresh anchor. Only when the
		// failing index was the verdict's own does an unanchored verdict
		// survive into the next boundary and meet the guard the way an anchored
		// one does.
		//
		// Either way no pass will continue from here, which is what the retire
		// is about: in the first regime the position is declined, in the second
		// the verdict it is a position into no longer exists.
		s.resetPlannerStatsCursor()
		s.plannerStatsMu.Lock()
		fresh.LastRefreshAt = s.plannerStatsLastRefresh
		fresh.LastRefreshReason = s.plannerStatsLastReason
		s.plannerStatsMu.Unlock()
		log.Printf("store_sqlite: planner stats refresh failed reason=%s indexes=%d/%d elapsed=%s error=%q",
			fresh.Reason, analyzed, len(work), elapsed, refreshErr)
		return fresh, refreshErr
	}
	// The work list, not the verdict text, is what says which relations were
	// rebuilt: a "growth:" list is one family, a "missing:" backfill can span
	// both, and matching on the reason string would re-derive — wrongly — what
	// the list already states.
	s.notePlannerStatsRefresh(fresh, plannerStatsWorkFamilies(work))
	s.plannerStatsMu.Lock()
	fresh.LastRefreshAt = s.plannerStatsLastRefresh
	fresh.LastRefreshReason = s.plannerStatsLastReason
	s.plannerStatsMu.Unlock()
	fresh.Refreshed = true
	fresh.Refreshes = s.plannerStatsRefreshes.Add(1)
	// Only a COMPLETED pass recycles, and the cost of that is worth stating: a
	// deferred pass leaves the indexes it did analyze invisible to every reader
	// connection already open, because SQLite bumps the schema cookie when
	// sqlite_stat1 is created and never when its rows are rewritten. Those
	// readers keep planning against the pre-refresh rows until some pass
	// completes. Recycling per index instead would drop the pool's idle
	// connections once per gate hold on a pass that may never finish, which
	// costs every reader a reconnect for statistics that are still half old.
	// The failure arm never reaches here at all: the settled guard then holds
	// the verdict until the store doubles again, so a partially analyzed family
	// can stay invisible to open readers for that long.
	recycleStatsReadPool(s.db, s.writerDB)
	// indexes=<analyzed this pass>/<the verdict's whole list>. The two differ
	// whenever a pass RESUMED: an earlier boundary already finished the rest,
	// and logging only the list length would make every resumed pass look like
	// it re-analyzed the whole family.
	log.Printf("store_sqlite: planner stats refreshed reason=%s indexes=%d/%d nodes=%d/%d edges=%d/%d receivers=%d/%d elapsed=%s insist=%s",
		fresh.Reason, analyzed, len(work),
		fresh.Nodes.Believed, fresh.Nodes.Actual,
		fresh.Edges.Believed, fresh.Edges.Actual,
		fresh.Receivers.Believed, fresh.Receivers.Actual,
		elapsed, insist)
	return fresh, nil
}

// cooperativePlannerStatsRefresh analyzes the work list one index per write-gate
// hold, and stops rather than waits.
//
// deferred names why the pass did not finish ("" when it did) and is never an
// error: a busy gate, a bulk window that opened underneath it, an expired
// caller context, an exhausted pass budget and a single index's timeout all
// leave a true verdict standing for the next boundary, with whatever indexes
// did get analyzed keeping their new rows AND recorded on the resume cursor.
// Only an ANALYZE that actually failed under a live context — or an index that
// has now timed out plannerStatsIndexTimeoutRetries times in a row — comes back
// as err, which is what arms the anti-loop guard.
//
// key identifies the pass for the cursor: see plannerStatsCursorKey. held is
// the base an unanchored verdict fired on, recorded on a cursor this call
// OPENS and left alone on one it resumes. analyzed counts what THIS pass
// rebuilt, which on a resumed pass is less than the work list — the log line
// says both.
func (s *Store) cooperativePlannerStatsRefresh(ctx context.Context, key string, held plannerStatsHeldBase, work []string, insist plannerStatsInsistence) (analyzed int, deferred string, err error) {
	pending := s.plannerStatsPending(key, held, work)
	if len(pending) == 0 {
		// Every index of this list is already done under the current cursor.
		// Defensive: a completed pass clears the cursor, so reaching here means
		// the list shrank under a cursor that already covered it. Treat it as
		// the completion it is rather than stamping nothing forever.
		s.clearPlannerStatsCursor(key)
		return analyzed, "", nil
	}
	removedStatRow := false
	started := time.Now()
	for i, name := range pending {
		if ctx.Err() != nil {
			return analyzed, plannerStatsCanceledReason + name, nil
		}
		// Checked before STARTING an index, and never before the first: a pass
		// that analyzed nothing would leave the cursor exactly where it was and
		// the mechanism would stop converging. So one ANALYZE may overshoot the
		// budget, and the boundary's cost is budget + one index.
		if insist == plannerStatsCooperative {
			if i > 0 && time.Since(started) >= plannerStatsPassBudget {
				return analyzed, plannerStatsBudgetReason + name, nil
			}
			if s.buildLaneBusy() {
				return analyzed, plannerStatsEditCycleReason + name, nil
			}
			if !s.writeMu.TryLock() {
				return analyzed, plannerStatsWriterBusyReason + name, nil
			}
		} else {
			// Owed at a load boundary (plannerStatsInsist): no budget, no
			// deferral to an edit cycle, and the gate is waited for.
			lockErr := s.writeMu.LockContext(ctx)
			if lockErr != nil {
				if ctx.Err() != nil {
					return analyzed, plannerStatsCanceledReason + name, nil
				}
				return analyzed, plannerStatsWriterBusyReason + name, nil
			}
		}
		// Counted at the ACQUISITION, not inside the hold: what the wider
		// gates above depend on is how many times this loop takes the store
		// gate, and a hold that spanned the whole work list would still make
		// one call per index.
		s.plannerStatsGateHolds.Add(1)
		// One hold can never outlive plannerStatsIndexTimeout, which is
		// strictly below the 15 s the bounded-gate droppers give this gate
		// before discarding their batches.
		// The per-index limit bounds what a cooperative pass costs the
		// writers beside it; at a load boundary there are none yet, and at
		// the clone's size one index's ANALYZE takes longer than the limit.
		indexCtx, cancelIndex := context.WithTimeout(ctx, plannerStatsIndexTimeout)
		stopYield, yielded := func() {}, new(atomic.Bool)
		if insist == plannerStatsCooperative {
			stopYield, yielded = s.cancelOnEditCycle(cancelIndex)
		} else {
			cancelIndex()
			indexCtx, cancelIndex = context.WithCancel(ctx)
		}
		removed, bulk, analyzeErr := s.plannerStatsHoldLocked(indexCtx, name)
		stopYield()
		timedOut := analyzeErr != nil && indexCtx.Err() != nil && ctx.Err() == nil && !yielded.Load()
		cancelIndex()
		s.writeMu.Unlock()
		if analyzeErr != nil && yielded.Load() && ctx.Err() == nil {
			return analyzed, plannerStatsEditCycleReason + name, nil
		}
		if bulk {
			return analyzed, plannerStatsBulkWindowReason, nil
		}
		switch {
		case analyzeErr == nil:
		case ctx.Err() != nil:
			// The CALLER walked away, underneath the write-connection
			// acquisition or the ANALYZE itself. That is a deferral, not a
			// failure: nothing about this store is wrong, and arming the
			// anti-loop guard here would suppress the next boundary's refresh
			// over a store whose statistics really are stale.
			//
			// The test is ctx.Err(), never errors.Is(err, context.Canceled).
			// Not because the driver hides the cancellation — modernc does
			// surface the context's own error for a statement interrupted on
			// this shape, and database/sql maps a context that died before the
			// statement started to it as well. Because ctx.Err() is right
			// whatever the driver returns: it asks about the CALLER, which is
			// the thing this branch is actually about, and it keeps answering
			// correctly if the driver's error wrapping changes underneath.
			return analyzed, plannerStatsCanceledReason + name, nil
		case timedOut:
			// This index alone ran out of time. Deferred like any other stop,
			// but counted: an index that does this at every boundary would
			// otherwise cost a full timeout forever and never clear.
			//
			// The deferral is cheap, not free. modernc marks a connection whose
			// statement was interrupted as invalid, so database/sql drops it
			// from the pool — and the writer pool holds exactly one connection,
			// so this costs its warm page cache and a reopen on the next write
			// (~0.4 ms, with the pragmas restored from the DSN). The
			// interrupted ANALYZE rolls back cleanly, so sqlite_stat1 keeps the
			// rows it had; nothing is left half-written.
			if s.notePlannerStatsIndexTimeout(key, name) >= plannerStatsIndexTimeoutRetries {
				// The settled guard absorbs the repetition from here, so the
				// streak restarts: the next pass the guard admits — after the
				// store has materially grown — gets the full allowance again
				// instead of failing on its first timeout.
				s.clearPlannerStatsIndexTimeout(key, name)
				return analyzed, "", analyzeErr
			}
			return analyzed, plannerStatsTimeoutReason + name, nil
		default:
			return analyzed, "", analyzeErr
		}
		s.markPlannerStatsIndexDone(key, name)
		analyzed++
		removedStatRow = removedStatRow || removed
	}
	if removedStatRow {
		// The reload is owed to connections that are already open; a
		// connection opened after the DELETE never sees the row. So a busy
		// gate here is not worth another pass: the next successful refresh —
		// or the next reopen — reloads, and nothing plans against a row that
		// is no longer in the table on any connection but the survivors.
		//
		// Concretely, what a skipped reload costs: the writer pool is a
		// ONE-connection pool, so "connections already open" is at most that
		// single writer plus whatever read-pool connections predate the
		// DELETE. The writer keeps the deleted row in its cached statistics —
		// and therefore keeps planning every write-path query against it —
		// until the next successful reload or until the store is reopened.
		// recycleStatsReadPool below deals with the read side.
		//
		// A departed caller skips it outright. Every other place in this loop
		// treats ctx.Err() as a deferral rather than work to push through, and
		// the reload is the one hold that could still be taken after the
		// caller walked away: WithTimeout on a dead parent fires immediately,
		// so the hold would be taken only to fail, cost the one-connection
		// writer pool its warm cache, and hand back the same skipped reload.
		if ctx.Err() == nil && s.writeMu.TryLock() {
			// Counted like an index hold, and at the same site: it is a gate
			// ACQUISITION the wider gates above pay for, and a counter that
			// omitted it would understate what a completed pass costs them. So
			// a pass that removed a stat row reports len(work)+1 holds where
			// one that removed none reports len(work).
			s.plannerStatsGateHolds.Add(1)
			// Bounded exactly like an index hold. ANALYZE sqlite_schema is the
			// only statement in this loop that is not already under a per-index
			// timeout, and an unbounded hold here would be the one way this
			// mechanism could outlive the 15 s window the bounded-gate writers
			// give the gate before dropping their batches. A timeout is a
			// best-effort skip with the same consequence as a busy gate above:
			// the deleted row survives in the caching connections until the
			// next successful reload or a reopen.
			reloadCtx, cancelReload := context.WithTimeout(ctx, plannerStatsIndexTimeout)
			s.reloadPlannerStatsLocked(reloadCtx)
			cancelReload()
			s.writeMu.Unlock()
		}
	}
	s.clearPlannerStatsCursor(key)
	return analyzed, "", nil
}

// plannerStatsCursorKey identifies one resumable pass: the verdict's reason key
// plus the work list it produced. A different verdict, or the same verdict over
// a work list an index has joined or left, is a different pass — resuming into
// it would skip indexes that were never analyzed for THIS list.
func plannerStatsCursorKey(reason string, work []string) string {
	return plannerStatsReasonKey(reason) + "|" + strings.Join(work, ",")
}

// plannerStatsPending returns the part of work this pass still owes, resetting
// the cursor when it belongs to a different pass.
//
// held is recorded only on the cursor this call OPENS. A resuming pass keeps
// the base its verdict originally fired on: recomputing it here would read the
// row the first index of this very pass has already rewritten, which is the
// self-repairing verdict plannerStatsCursor's held exists to prevent.
func (s *Store) plannerStatsPending(key string, held plannerStatsHeldBase, work []string) []string {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	if s.plannerStatsCursor.key != key {
		s.plannerStatsCursor = plannerStatsCursor{key: key, held: held}
	}
	pending := make([]string, 0, len(work))
	for _, name := range work {
		if !s.plannerStatsCursor.done[name] {
			pending = append(pending, name)
		}
	}
	return pending
}

// plannerStatsHeldBase is the base an unfinished pass's unanchored verdict was
// judged against, or a zero struct when no pass is outstanding.
//
// A cursor exists only while its pass still owes indexes — a completed pass
// retires it, and so does every arm that abandons one — so "a cursor stands"
// and "this family still has pending work" are the same statement.
func (s *Store) plannerStatsHeldBase() plannerStatsHeldBase {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	if s.plannerStatsCursor.key == "" {
		return plannerStatsHeldBase{}
	}
	return s.plannerStatsCursor.held
}

// plannerStatsUnanchoredVerdictBase is the base to hold for a verdict about to
// be acted on: the base the verdict was actually judged against, and only for
// the family the verdict's own index belongs to, and only while that family has
// no anchor of its own.
//
// bases is that number, carried out of the probe whose rules used it, rather
// than the believed row re-read here. On a fresh cursor the two are the same
// thing — an unanchored family is judged against its believed row by the
// pre-seed rule — but "the same thing" was an argument about two pieces of code
// agreeing, and this is the one place where being wrong silently strands a
// family's remaining indexes for a doubling. Carried through, the invariant
// ("what is held is what was judged") holds by construction. It also makes the
// one case where the two genuinely differ come out right: a pass re-opening a
// cursor under a standing held base records that base again instead of the row
// the earlier pass already rewrote.
//
// The family comes from the verdict's index through plannerStatsWorkFamilies,
// the same mapping notePlannerStatsRefresh attributes a completed pass with, so
// the base held here and the anchor that eventually replaces it can never
// describe different families.
//
// Two verdicts therefore hold nothing, for two different reasons, and only one
// of them is about the rule not needing a base:
//
//   - A "missing:" verdict fires on a believed cardinality of zero and does not
//     consult a base at all, so there is nothing to hold.
//   - A receiver "growth:" verdict DOES consult one — the receiver family never
//     carries an anchor, so plannerStatsHealth always judges it against its own
//     believed row — but nodes_go_receiver_type belongs to no family here, so
//     nothing is held for it and its first completed index clears it. That is
//     the right outcome rather than an oversight: the receiver rule is a
//     statement about ONE row (PR 1's poisoned near-zero row), the work list
//     rebuilds exactly that row first, and the six other nodes indexes behind
//     it are family widening no rule ever complained about. The remedy for a
//     nodes table that really has outgrown its statistics is the nodes verdict,
//     which does hold a base.
func (s *Store) plannerStatsUnanchoredVerdictBase(health graph.PlannerStatsFreshness, bases plannerStatsJudgedBases) plannerStatsHeldBase {
	families := plannerStatsWorkFamilies([]string{plannerStatsVerdictIndex(health.Reason)})
	var held plannerStatsHeldBase
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	if families.nodes && s.plannerStatsBaseline.nodes == 0 {
		held.nodes = bases.nodes
	}
	if families.edges && s.plannerStatsBaseline.edges == 0 {
		held.edges = bases.edges
	}
	return held
}

// plannerStatsVerdictIndex names the index a verdict was read off — the part
// after the verb in its reason key. A verdict carrying no index (the deferral
// reasons, "no_stats") yields "", which belongs to no family.
func plannerStatsVerdictIndex(reason string) string {
	_, index, ok := strings.Cut(plannerStatsReasonKey(reason), ":")
	if !ok {
		return ""
	}
	return index
}

// markPlannerStatsIndexDone records one finished index on the cursor, and
// clears that index's consecutive-timeout counter: the streak is about an index
// that keeps failing to finish, and it just finished.
func (s *Store) markPlannerStatsIndexDone(key, name string) {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	if s.plannerStatsCursor.key != key {
		return
	}
	if s.plannerStatsCursor.done == nil {
		s.plannerStatsCursor.done = make(map[string]bool, 4)
	}
	s.plannerStatsCursor.done[name] = true
	delete(s.plannerStatsCursor.timeouts, name)
}

// notePlannerStatsIndexTimeout counts one more consecutive timeout for an index
// and returns the new streak.
func (s *Store) notePlannerStatsIndexTimeout(key, name string) int {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	if s.plannerStatsCursor.key != key {
		return 1
	}
	if s.plannerStatsCursor.timeouts == nil {
		s.plannerStatsCursor.timeouts = make(map[string]int, 1)
	}
	s.plannerStatsCursor.timeouts[name]++
	return s.plannerStatsCursor.timeouts[name]
}

// clearPlannerStatsIndexTimeout ends one index's consecutive-timeout streak.
func (s *Store) clearPlannerStatsIndexTimeout(key, name string) {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	if s.plannerStatsCursor.key != key {
		return
	}
	delete(s.plannerStatsCursor.timeouts, name)
}

// clearPlannerStatsCursor retires a finished pass, so a later identical verdict
// — a store that grew the same way again — starts from the head.
func (s *Store) clearPlannerStatsCursor(key string) {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	if s.plannerStatsCursor.key == key {
		s.plannerStatsCursor = plannerStatsCursor{}
	}
}

// resetPlannerStatsCursor drops the resume position whatever pass it belonged
// to, for the two cases where the position has stopped meaning anything and no
// completed pass will ever retire it: a probe that finds the store NOT stale
// (the verdict cleared without this process finishing it — another caller
// refreshed, or the baseline decayed), and a pass that FAILED (the settled
// guard now holds the verdict until the store doubles again, so the pass will
// be declined rather than continued).
//
// Unconditional on the key, unlike clearPlannerStatsCursor: the point is
// precisely that no key is owed the position any more. Leaving it behind is
// what would let a much later identical verdict resume past indexes analyzed at
// a size the "trails by at most one doubling" bound no longer covers.
//
// It retires the held base with the position, which is what keeps the held base
// from becoming a way to pin a verdict open: a verdict that clears for a reason
// this process did not manufacture — the baseline decayed, another caller's
// pass completed — reaches the not-stale probe, and a pass that failed reaches
// the failure arm, and both come through here.
//
// Called only from EnsurePlannerStatsFresh, never from the read-only health
// probe, and that is enough: a cursor is consumed only by a pass, every pass
// begins with Ensure's own probe, and that probe retires the position before it
// could be resumed into. Having index_health's background rebuild retire it too
// would buy nothing and would let a poll wipe the position of a pass that is
// mid-list on another goroutine.
func (s *Store) resetPlannerStatsCursor() {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	s.plannerStatsCursor = plannerStatsCursor{}
}

// plannerStatsHoldLocked is the body of one gate hold: re-check the bulk
// window, then analyze exactly one index. Split out so the TryLock/Unlock pair
// above stays adjacent and every early return releases the gate.
func (s *Store) plannerStatsHoldLocked(ctx context.Context, name string) (removedStatRow, bulkWindow bool, err error) {
	// A bulk window can open between two holds. The cold path refreshes at its
	// own finalize, and the droppable indexes are not even in the schema now.
	if s.bulkConn != nil || s.coordinatedBulkLoad {
		return false, true, nil
	}
	removed, err := s.analyzePlannerStatsIndexLocked(ctx, name)
	return removed, false, err
}

// plannerStatsNodesBelieved names the index the nodes cardinality is read
// from, and what it believes.
//
// nodes_by_kind is preferred and nodes_by_file is the fallback: both are
// non-partial indexes over nodes, so either one's leading stat token is the
// table's cardinality, but a store can carry a row for one and not the other
// (PR 1's repair deletes rows, a hand-edited or partially analyzed store
// simply differs). Falling back on a believed of zero rather than on schema
// absence is what makes the pair useful — the index is present in both cases;
// only the row is missing. The reported index stays nodes_by_kind when neither
// row exists, because that is the verdict a refresh answers.
func (s *Store) plannerStatsNodesBelieved(ctx context.Context, present map[string]bool) (index string, believed int64) {
	if present[plannerStatsNodesIndex] {
		index = plannerStatsNodesIndex
		believed = plannerStatsBelievedRows(ctx, s.db, index)
		if believed > 0 {
			return index, believed
		}
	}
	if !present[plannerStatsNodesFallbackIndex] {
		return index, believed
	}
	if fallback := plannerStatsBelievedRows(ctx, s.db, plannerStatsNodesFallbackIndex); fallback > 0 {
		return plannerStatsNodesFallbackIndex, fallback
	}
	if index == "" {
		index = plannerStatsNodesFallbackIndex
	}
	return index, 0
}

// plannerStatsPresentIndexes lists the critical indexes that exist in the
// schema right now. Membership is what separates "no statistics for a
// populated index" from "an index a bulk window has dropped".
func (s *Store) plannerStatsPresentIndexes(ctx context.Context) (map[string]bool, error) {
	names, err := s.plannerStatsPresentIndexList(ctx)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}
	return present, nil
}

// plannerStatsIndexesWithStats reports which critical indexes currently carry
// a statistics row. It is the evidence a "missing:" verdict's work list is
// built from; an unreadable answer (sqlite_stat1 does not exist yet, which is
// the commonest reason to be here) comes back nil, and a nil map reads as
// "nothing has statistics" — the correct scope for exactly that store.
func (s *Store) plannerStatsIndexesWithStats(ctx context.Context) map[string]bool {
	rows, err := s.db.QueryContext(ctx, `SELECT idx FROM sqlite_stat1 WHERE idx IS NOT NULL`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	hasStat := make(map[string]bool, 10)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil
		}
		hasStat[name] = true
	}
	if rows.Err() != nil {
		return nil
	}
	return hasStat
}

// plannerStatsPresentIndexList is the same membership in schema order, which
// is the order the refresh loop analyzes in.
func (s *Store) plannerStatsPresentIndexList(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, plannerStatsIndexQuery)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// plannerStatsWorkList scopes a refresh to the indexes one verdict is actually
// about.
//
// The batch refresh analyzes all ten critical indexes, which is right for a
// cold-load finalize — it owns the store and every index was just rebuilt —
// and wrong for a growth verdict at runtime, where the gate is taken and
// released once per index and the cost is paid against live traffic. A
// "growth:" verdict is a statement about a TABLE's cardinality: nodes grew, so
// every statistics row describing an index on nodes now understates it, and no
// row describing an index on edges does.
//
// A "missing:" verdict is a different question — a BACKFILL, not a
// re-analysis — and its scope is every present critical index that has no
// statistics row, not the one the rules happened to name first. The rules
// examine nodes, then edges, then receivers, and stop at the first verdict, so
// a store with no sqlite_stat1 at all reports "missing:nodes_by_kind" while
// every other index is equally unanalyzed. Rebuilding only the named one would
// leave the pass ending stale and hand the rest to boundaries that may be
// hours away. hasStat carries the rows that DO exist; a nil or empty map is
// read as "nothing has statistics", which is the same answer.
//
// Only "missing:" and "growth:" verdicts reach this function — those are the
// two plannerStatsStaleReason can produce. plannerStatsRepairReason's
// "stale_stat:" / "tiny_stat:" / "no_stats" verdicts belong to the Open-time
// repair, which rebuilds every critical index through refreshPlannerStatsOnConn
// and never consults a work list, so they fall through to the default arm here
// rather than carrying dead special cases.
//
// ORDER is load bearing, twice over. The verdict's own index comes FIRST:
//
//   - It is the row the verdict was read off, so it is the one whose rebuild
//     can clear the verdict at all. A pass that defers before reaching it has
//     rebuilt statistics nobody complained about.
//   - It is the cheapest plan-critical index of its family, by an order of
//     magnitude on a real store: edges_by_kind is ~44 MiB of pages against
//     edges_by_from_line_kind's 273-534 MiB. Paying it first is the most
//     planning value per second of held gate.
//
// The rest of the family follows in the schema's alphabetical order, which is
// the order plannerStatsPresentIndexList returns — stable across passes, which
// is what makes the resume cursor's "continue after the last completed index"
// meaningful.
//
// present must be the schema-present critical index names; anything not in it
// is dropped, so a bulk window that removed an index cannot put it on the list
// — including the index the verdict named. An unrecognised verdict falls back
// to the full list rather than to nothing: paying too much is a slow refresh,
// paying nothing is a store that never converges.
func plannerStatsWorkList(reason string, present []string, hasStat map[string]bool) []string {
	key := plannerStatsReasonKey(reason)
	verb, index, ok := strings.Cut(key, ":")
	if !ok || index == "" {
		return present
	}
	var rest []string
	switch verb {
	case "missing":
		for _, name := range present {
			// The index the verdict NAMED is prepended below, row or no row.
			// "missing:" fires on a believed cardinality of zero, and zero is
			// what plannerStatsBelievedRows reports for a row that exists and
			// reads zero as well as for no row at all: the '0 0 0 0 0' row an
			// older engine wrote for an empty partial index, and the smallest
			// of the duplicate rows a store with no UNIQUE constraint on
			// sqlite_stat1 can hold. Scoping on hasStat alone drops exactly
			// the index that is wrong, and the pass then stamps a success the
			// verdict outlives — or, when every other index has a row,
			// produces an empty list and a "no_indexes" return that never
			// stamps, so every boundary re-probes the same unrepaired store
			// forever. ANALYZE rewrites the index's rows (collapsing a
			// duplicate) and the empty-partial branch deletes a stale one, so
			// including it IS the repair.
			if name != index && !hasStat[name] {
				rest = append(rest, name)
			}
		}
	case "growth":
		spec, known := plannerStatsIndexProbes[index]
		if !known {
			return present
		}
		for _, name := range present {
			if name == index {
				continue
			}
			if other, ok := plannerStatsIndexProbes[name]; ok && other.table == spec.table {
				rest = append(rest, name)
			}
		}
	default:
		return present
	}
	for _, name := range present {
		if name == index {
			return append([]string{index}, rest...)
		}
	}
	return rest
}

// plannerStatsCounters reads the un-filtered repo_index_state totals. The
// leading COUNT(*) separates "no counter rows for this store" from a genuine
// zero total, which the believed==0 rule must not confuse.
func (s *Store) plannerStatsCounters(ctx context.Context) (rows, nodes, edges int64, err error) {
	err = s.db.QueryRowContext(ctx, plannerStatsCounterQuery).Scan(&rows, &nodes, &edges)
	if err != nil {
		return 0, 0, 0, err
	}
	return rows, nodes, edges, nil
}

// plannerStatsReceiverCount bounds the receiver index's verification count at
// plannerStatsHealthProbeCap. capped reports that the question could not be
// asked in full, which the caller turns into Known=false rather than a
// misleading "the index holds exactly the cap".
func (s *Store) plannerStatsReceiverCount(ctx context.Context, index string, believed int64) (actual int64, capped bool, err error) {
	spec, known := plannerStatsIndexProbes[index]
	if !known {
		return 0, true, nil
	}
	limit := believed*plannerStatsGrowthFactor + 1
	if limit <= 0 || limit > plannerStatsHealthProbeCap {
		limit = plannerStatsHealthProbeCap
		capped = true
	}
	if err := s.db.QueryRowContext(ctx, spec.countQuery(index), limit).Scan(&actual); err != nil {
		return 0, true, err
	}
	return actual, capped, nil
}

// plannerStatsRelationHasRows answers "is this table non-empty" for the
// believed==0 rule. An unreadable probe reports false: inventing evidence that
// a relation is populated would buy an ANALYZE the store may not need.
func (s *Store) plannerStatsRelationHasRows(ctx context.Context, table string) bool {
	var populated bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+`)`).Scan(&populated); err != nil {
		return false
	}
	return populated
}

// seedPlannerStatsBaseline anchors the growth high-water mark the first time
// the statistics are found fresh. Until it is seeded, growth is measured
// against the believed cardinality, which is what lets a store that booted
// already stale be repaired once; from then on it is measured against the size
// the store was when it was last analyzed.
//
// It also fills a component a completed pass left at zero. notePlannerStatsRefresh
// anchors only the family it rebuilt, so a store whose first Ensure was already
// stale ends its nodes pass with a seeded anchor whose edges component is zero.
// That zero is a working state — plannerStatsStaleReason falls back to the
// believed row for it — but once a probe finds the edges family fresh, the size
// it measured is the honest anchor and is strictly better than the believed
// extrapolation. Filling is the only raise allowed here: a component that
// already holds a figure is never moved, because only a successful ANALYZE of
// that family earns a new anchor, and raising one here would suppress the very
// verdict this probe is measuring for.
//
// One zero is NOT fillable: a component whose family still has an unfinished
// pass standing on the cursor with a held base. That base is the only record of
// what the family's verdict fired on, and anchoring the component at the
// current totals would clear the verdict exactly as replacing the base with a
// refreshed believed row would — leaving the pass's remaining indexes frozen
// until the store doubles again. See plannerStatsCursor.
func (s *Store) seedPlannerStatsBaseline(health graph.PlannerStatsFreshness) {
	if !health.Nodes.Known && !health.Edges.Known {
		return
	}
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	var held plannerStatsHeldBase
	if s.plannerStatsCursor.key != "" {
		held = s.plannerStatsCursor.held
	}
	s.plannerStatsBaseline.seeded = true
	if s.plannerStatsBaseline.nodes == 0 && held.nodes == 0 {
		s.plannerStatsBaseline.nodes = health.Nodes.Actual
	}
	if s.plannerStatsBaseline.edges == 0 && held.edges == 0 {
		s.plannerStatsBaseline.edges = health.Edges.Actual
	}
}

// decayPlannerStatsBaseline lowers the growth anchor to totals that have
// fallen below it, and returns the anchor to judge against. Each relation
// decays on its own: a retirement can shed nodes without shedding edges.
//
// It never RAISES the anchor — only a successful refresh earns that, because
// raising it here would suppress the very verdict the growth this probe just
// measured is supposed to produce.
func (s *Store) decayPlannerStatsBaseline(nodes, edges int64) plannerStatsBaseline {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	if !s.plannerStatsBaseline.seeded {
		return s.plannerStatsBaseline
	}
	if nodes < s.plannerStatsBaseline.nodes {
		s.plannerStatsBaseline.nodes = nodes
	}
	if edges < s.plannerStatsBaseline.edges {
		s.plannerStatsBaseline.edges = edges
	}
	return s.plannerStatsBaseline
}

// plannerStatsAttemptSettled reports a verdict this process has already acted
// on over a store that has not materially grown since. It returns the prior
// verdict so the caller can say which one settled.
//
// "Materially" is the same factor the growth rule uses, and deliberately so.
// The guard exists for a verdict a refresh cannot clear — an ANALYZE that
// errors, an index whose row the engine refuses to write — and such a verdict
// re-fires at every one of the call sites. Re-arming on ANY counter
// movement would retry it on the next indexed file, which on a busy daemon is
// the loop the guard exists to prevent. Re-arming once per doubling matches
// the baseline's own convergence argument: the store has to be a materially
// different store before the same failing verdict is worth paying for again.
func (s *Store) plannerStatsAttemptSettled(health graph.PlannerStatsFreshness) (bool, string) {
	key := plannerStatsReasonKey(health.Reason)
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	prior := s.plannerStatsLastAttempt
	if !prior.made || prior.reason != key {
		return false, ""
	}
	if plannerStatsGrewByFactor(health.Nodes.Actual, prior.nodes) ||
		plannerStatsGrewByFactor(health.Edges.Actual, prior.edges) {
		return false, ""
	}
	return true, key
}

// plannerStatsGrewByFactor reports growth worth re-arming the anti-loop guard
// for. A recorded total of zero is the store that has no counter rows at all;
// it re-arms on any positive total and never on another zero, so a store that
// will never have counters cannot loop.
func plannerStatsGrewByFactor(now, recorded int64) bool {
	if now <= recorded {
		return false
	}
	return now >= plannerStatsGrowthFactor*recorded
}

// notePlannerStatsAttempt arms the anti-loop guard for a refresh this process
// attempted: the verdict that triggered it and the totals it was triggered at.
// It says nothing about whether the refresh worked, and is therefore the ONLY
// state a failed refresh may leave behind.
func (s *Store) notePlannerStatsAttempt(health graph.PlannerStatsFreshness) {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	s.plannerStatsLastAttempt = plannerStatsAttempt{
		reason: plannerStatsReasonKey(health.Reason),
		nodes:  health.Nodes.Actual,
		edges:  health.Edges.Actual,
		made:   true,
	}
}

// plannerStatsFamilies names the relations one completed pass rebuilt the
// statistics of. It is what a refresh is allowed to move the growth anchor for:
// see plannerStatsBaseline on why moving the other component too freezes the
// unrefreshed family's rows forever on a proportionally growing store.
type plannerStatsFamilies struct {
	nodes bool
	edges bool
}

// plannerStatsWorkFamilies reads the families off the work list rather than off
// the verdict text. The list is the authority: a "growth:" verdict scopes it to
// one family, a "missing:" backfill can span both, and a fallback list covers
// everything present. Deriving it from the reason string instead would be a
// second, drift-prone copy of plannerStatsWorkList's own rule.
//
// A family is earned by the index its VERDICT is read off, not by the table the
// index sits on, and the difference is the whole point. The growth rules read
// exactly three rows — nodes_by_kind (falling back to nodes_by_file) and
// edges_by_kind — so those are the only rows an ANALYZE can bring back into
// agreement with the store. Seven critical indexes sit on nodes; a pass that
// rebuilt only nodes_go_receiver_type (the work list a receiver-only "missing:"
// verdict produces) has not touched the row the nodes verdict is judged from,
// and moving the nodes anchor for it lets the nodes family reach FOUR times the
// analyzed size before the next verdict fires. So an unattributable list moves
// nothing, and the family's own rule — believed row or standing anchor — stays
// in charge until a pass rebuilds the row it reads.
func plannerStatsWorkFamilies(work []string) plannerStatsFamilies {
	var families plannerStatsFamilies
	for _, name := range work {
		switch name {
		case plannerStatsNodesIndex, plannerStatsNodesFallbackIndex:
			families.nodes = true
		case plannerStatsEdgesIndex:
			families.edges = true
		}
	}
	return families
}

// notePlannerStatsRefresh records a refresh that SUCCEEDED: the attempt above,
// the ledger index_health publishes, and the new growth baseline. Everything
// beyond the attempt is a claim that sqlite_stat1 now describes a store of
// this size, which only a successful ANALYZE earns.
//
// families is what the pass actually rebuilt, and only those components of the
// anchor move. A nodes pass may not claim anything about the edge statistics it
// did not touch: moving the edges component to the total that was about to
// produce the edges verdict is exactly how edges_by_kind stays frozen at its
// cold-load figure while the store doubles again and again.
func (s *Store) notePlannerStatsRefresh(health graph.PlannerStatsFreshness, families plannerStatsFamilies) {
	s.notePlannerStatsAttempt(health)
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	s.plannerStatsLastRefresh = time.Now()
	s.plannerStatsLastReason = health.Reason
	if !health.Nodes.Known && !health.Edges.Known {
		return
	}
	// The baseline is seeded by a NOT-stale probe, and a store whose very first
	// Ensure is already stale never reaches one — which is exactly the lead's
	// store: an existing on-disk database the daemon opens with nodes_by_kind
	// and edges_by_kind believing 592k / 2.77M over a corpus of 1.69M / 8.58M.
	// Those rows are non-partial and far above plannerStatsSuspectRows, so the
	// Open-time repair does not touch them, and the first probe is a nodes
	// growth verdict.
	//
	// So the unseeded branch marks the anchor seeded and anchors ONLY what this
	// pass rebuilt. Seeding BOTH here is the bug this shape exists to prevent:
	// the nodes pass would anchor edges at 8.58M, the edges verdict would need
	// the store to reach 17.16M before it fired, and edges_by_kind would keep
	// its 2.77M row for the life of the daemon — the frozen edge statistics
	// issue #651's receiver/edge joins are costed from.
	//
	// A component left at zero is not a hole. plannerStatsStaleReason reads a
	// zero base as "no anchor" and falls back to the BELIEVED row, which is the
	// pre-seed rule — the one that fired correctly on the lead's store in the
	// first place — and it self-terminates: the family's own pass rewrites the
	// believed row and then anchors the component. A later not-stale probe
	// fills a still-zero component too (seedPlannerStatsBaseline).
	if !s.plannerStatsBaseline.seeded {
		s.plannerStatsBaseline.seeded = true
	}
	// No both-families fallback. An unattributable work list — one that
	// re-analyzed neither family's verdict index — has earned neither anchor,
	// and moving both on the strength of a receiver-only backfill lets the
	// nodes family drift to twice the anchor without a single ANALYZE of
	// nodes_by_kind. Moving neither leaves the verdict's own rule in charge.
	if families.nodes {
		s.plannerStatsBaseline.nodes = health.Nodes.Actual
	}
	if families.edges {
		s.plannerStatsBaseline.edges = health.Edges.Actual
	}
}

// stampPlannerStatsRefresh records a refresh performed OUTSIDE
// EnsurePlannerStatsFresh — the cold path's finalize and the Open-time repair
// — so the first runtime call after either does not pay a second ANALYZE for
// statistics that were just rebuilt.
//
// It reads the counters through the read pool, and both callers hold writeMu
// while a writer connection is pinned — so the read is only safe when the two
// pools are distinct handles. That is every on-disk store. When they are the
// SAME handle the counter read is skipped outright rather than left to the
// coincidence that bulk loads do not engage in memory: s.writerDB is a
// one-connection pool, and a query issued on it while the caller holds that
// connection would block until the caller it is nested inside returns.
//
// Skipping costs only the baseline seed. The ledger and the anti-loop guard
// are still stamped, and the guard armed with zero totals re-arms on the first
// positive counter total, so the next verdict over a store that has grown is
// not suppressed.
//
// The guard this arms is in fact inert BY CONSTRUCTION, and that is worth
// saying because "a cold-load finalize arms the settled guard" would otherwise
// read as a way to suppress the first runtime verdict. Its key is this
// function's own reason — "cold_load_finalize" from the bulk finalize,
// "open_repair:<plannerStatsRepairReason verdict>" from the Open-time repair —
// while plannerStatsAttemptSettled only ever compares it against a key
// plannerStatsStaleReason produced, and that function emits nothing but
// "growth:<idx>" and "missing:<idx>". The two vocabularies are disjoint, so the
// keys can never match and the guard can never decline a runtime pass. A future
// change to either vocabulary must keep them disjoint; the day a repair verdict
// and a runtime verdict share a key, the first pass after a cold load is
// declined as already settled.
func (s *Store) stampPlannerStatsRefresh(ctx context.Context, reason string) {
	if s.coreless() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var rows, nodes, edges int64
	countersRead := false
	if s.db != s.writerDB {
		var err error
		if rows, nodes, edges, err = s.plannerStatsCounters(ctx); err == nil {
			countersRead = true
		}
	}
	s.plannerStatsMu.Lock()
	s.plannerStatsLastRefresh = time.Now()
	s.plannerStatsLastReason = reason
	s.plannerStatsLastAttempt = plannerStatsAttempt{
		reason: plannerStatsReasonKey(reason),
		nodes:  nodes,
		edges:  edges,
		made:   true,
	}
	if countersRead && rows > 0 {
		s.plannerStatsBaseline = plannerStatsBaseline{nodes: nodes, edges: edges, seeded: true}
	}
	s.plannerStatsMu.Unlock()
	s.plannerStatsRefreshes.Add(1)
}

// plannerStatsReasonKey strips the measured figures off a verdict, leaving the
// part that identifies WHICH rule fired on WHICH index. Two verdicts with the
// same key describe the same defect.
func plannerStatsReasonKey(reason string) string {
	if i := strings.IndexByte(reason, ' '); i >= 0 {
		return reason[:i]
	}
	return reason
}

// plannerStatsInsistence is how a refresh pass treats edits.
type plannerStatsInsistence string

const (
	// plannerStatsCooperative: the pass defers to an edit cycle, a busy
	// gate and its budget, and resumes at the next boundary.
	plannerStatsCooperative plannerStatsInsistence = ""
	// plannerStatsOwed: a planner index holds rows but has no statistics
	// (a store fresh from a whole index), at the boundary that ends the index
	// (graph.WithPlannerStatsLoadBoundary): the pass runs to the end. It
	// waits for the gate, does not give way to an edit cycle, and has no
	// per-index limit, since no edit is admitted yet and one index's ANALYZE
	// holds the writer for up to 17 s at the clone's size (2.2M nodes, 11M
	// edges). Nowhere else: at runtime a pass stays cooperative, and the
	// lookups do not depend on the statistics for their index.
	plannerStatsOwed plannerStatsInsistence = "owed"
)

// plannerStatsInsist decides how the pass for this verdict treats edits.
func (s *Store) plannerStatsInsist(ctx context.Context, reason string) plannerStatsInsistence {
	key := plannerStatsReasonKey(reason)
	if (key == "no_stats" || strings.HasPrefix(key, "missing:")) && graph.PlannerStatsLoadBoundary(ctx) {
		return plannerStatsOwed
	}
	return plannerStatsCooperative
}

// plannerStatsMissingIndex names the first present planner index (by name)
// that holds rows and has no sqlite_stat1 row, as a "missing:" verdict, or ""
// when there is none. Only indexes without a row are probed, one bounded
// EXISTS each (through the index, with a partial index's own predicate).
func (s *Store) plannerStatsMissingIndex(ctx context.Context, present map[string]bool) string {
	hasStat := s.plannerStatsIndexesWithStats(ctx)
	names := make([]string, 0, len(present))
	for name := range present {
		if !hasStat[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		spec, known := plannerStatsIndexProbes[name]
		if !known {
			continue
		}
		var hasRows bool
		if err := s.db.QueryRowContext(ctx, spec.existsQuery(name)).Scan(&hasRows); err != nil {
			continue
		}
		if hasRows {
			return "missing:" + name
		}
	}
	return ""
}
