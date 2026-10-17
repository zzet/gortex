package graph

import (
	"context"
	"time"
)

// PlannerStatsCardinality pairs what the query planner believes a relation
// holds with what the store can cheaply prove it holds.
//
// Believed is the leading token of the backend's statistics row for the index
// that describes the relation — zero when no row exists, which is also the
// value a poisoned '0 0 0 0 0' row reports. Actual is a cheap independent
// measure: a persisted counter, or a bounded probe.
//
// Neither field is a census. Bounded says Actual stopped at a cap and is a
// lower bound, not a count. Known says Actual could be determined at all: a
// relation whose index is absent from the schema, or whose counters have never
// been written, reports Known=false rather than a fabricated zero, because
// "I could not look" and "there is nothing there" lead to opposite decisions.
//
// Present is the one bit that separates those two cases from each other. It
// says the index this cardinality describes exists in the schema right now, so
// a reporter can omit the relation entirely rather than publish believed=0 /
// actual=0 for an index a bulk window has dropped — or one this backend never
// had. Every other field of a relation with Present=false is meaningless.
type PlannerStatsCardinality struct {
	Believed int64
	Actual   int64
	Bounded  bool
	Known    bool
	Present  bool
}

// PlannerStatsFreshness is one verdict on a store's query-planner statistics.
//
// Stale means the store has grown past what the planner believes by enough to
// change a join order; Reason names which relation and by how much. Refreshed
// reports whether this call actually rebuilt the statistics — a health probe
// never does, and an Ensure call only does on the stale branch.
//
// Checks and Refreshes are monotone per-store counters, and they do NOT count
// the same events. Checks counts Ensure calls. Refreshes counts every
// statistics rebuild the store recorded, including the ones no Ensure call
// made — the Open-time repair and the cold-load finalize both stamp it — so
// Refreshes can be non-zero on a store no pipeline has consulted yet. They
// exist so a caller — or a test — can say "this pipeline consulted the
// freshener exactly once" without reasoning about wall-clock timestamps.
type PlannerStatsFreshness struct {
	Nodes     PlannerStatsCardinality
	Edges     PlannerStatsCardinality
	Receivers PlannerStatsCardinality

	Stale     bool
	Refreshed bool
	Reason    string

	LastRefreshAt     time.Time
	LastRefreshReason string

	Checks    int64
	Refreshes int64
}

// PlannerStatsFreshener is an optional capability for backends whose query
// planner reads persisted statistics that go stale as the graph grows.
//
// The SQLite backend refreshes sqlite_stat1 at cold-load finalize and repairs
// it at Open; between those, a long-running daemon can double or triple the
// store while the planner keeps costing joins against the size it saw at boot.
// This capability lets the indexer and the resolver — and any direct caller of
// the view-generation publisher — hand the store a chance to notice, without
// any of them depending on the SQLite package.
//
// EnsurePlannerStatsFresh must be cheap when the statistics are fresh — the
// steady state at every call site — and must never be invoked while the
// caller holds the backend's write gate: the implementation takes it itself on
// the branch that refreshes.
//
// When the statistics are NOT fresh, three separate obligations apply, and
// they are easy to conflate:
//
//   - Never queue. WIDER locks are held at every one of these call sites — a
//     reach topology writer gate, a repository mutation lane, a resolver mutex
//     — and waiting on the backend's own write gate would hold all of them for
//     the wait. An implementation may refuse the work (reporting why in
//     Reason); it may never wait for the gate.
//   - Never hold the backend's write gate for an unbounded rebuild. Other
//     writers, and any bounded-gate writer that drops its work after a
//     timeout, must be able to make progress: one bounded unit of work per
//     hold, released between units.
//   - Bound, but do not eliminate, what one call costs. A caller that arrives
//     with a stale store DOES pay: the implementation may take up to a
//     bounded budget, plus the one unit of work already in flight, plus one
//     bounded fix-up hold at the end of a completed pass, before it defers the
//     rest. It does not shorten the caller's wider gates below that, and
//     callers must not assume otherwise.
//
// Convergence therefore happens ACROSS boundaries, not within one: a deferred
// call leaves the verdict standing and enough state to continue where it
// stopped, and the next boundary finishes the job. The SQLite backend honours
// all three by try-locking once per index, analyzing one index per hold under
// a per-index timeout, stopping at the first busy gate or spent budget, and
// resuming from a cursor at the next call. Its gate-holding cost per boundary
// is therefore its pass budget + one index's ANALYZE + one bounded
// sqlite_schema reload.
//
// Reads OUTSIDE that bound are permitted and unavoidable, and a caller sizing
// its own gate hold should count them separately: they take none of the
// backend's locks but do cost latency under the caller's. In the SQLite
// backend they are two health probes, the present-index list and the set of
// indexes that already carry a statistics row — a schema join, some catalog
// seeks, one small-table scan, and one capped index probe.
//
// PlannerStatsHealth is the read-only half: the same verdict with no write
// gate taken and no statistics rebuilt. Callers serving a health payload use
// it; nothing that reports state should issue an ANALYZE as a side effect.
type PlannerStatsFreshener interface {
	EnsurePlannerStatsFresh(ctx context.Context) (PlannerStatsFreshness, error)
	PlannerStatsHealth(ctx context.Context) (PlannerStatsFreshness, error)
}

type plannerStatsLoadBoundaryKey struct{}

// WithPlannerStatsLoadBoundary marks ctx as the boundary that ends a whole
// index: its rows have just landed and no edit has been admitted over them
// yet. There, and only there, a store may run a refresh it owes to the end
// (statistics absent for an index that holds rows), holding its writer for as
// long as the ANALYZE takes: nothing waits on the writer yet, and a store
// without statistics plans its first edits by the default cost model.
func WithPlannerStatsLoadBoundary(ctx context.Context) context.Context {
	return context.WithValue(ctx, plannerStatsLoadBoundaryKey{}, true)
}

// PlannerStatsLoadBoundary reports whether ctx was marked by
// WithPlannerStatsLoadBoundary.
func PlannerStatsLoadBoundary(ctx context.Context) bool {
	v, _ := ctx.Value(plannerStatsLoadBoundaryKey{}).(bool)
	return v
}

// MaybeEnsurePlannerStatsFresh gives a store the chance to refresh its planner
// statistics, and does nothing at all for a backend without the capability.
//
// The result is discarded on purpose: the implementation logs its own line
// when it refreshes, and no call site has a decision to make on the outcome —
// a store that could not refresh its statistics still answers every query.
func MaybeEnsurePlannerStatsFresh(ctx context.Context, target any) {
	f, ok := target.(PlannerStatsFreshener)
	if !ok {
		return
	}
	_, _ = f.EnsurePlannerStatsFresh(ctx)
}

// MaybePlannerStatsHealth reads a backend's planner-statistics verdict without
// refreshing anything. The bool is false for a backend without the capability
// and for one that could not answer, so a caller can omit the field entirely
// rather than publish a zeroed struct that reads as "everything is zero".
func MaybePlannerStatsHealth(ctx context.Context, target any) (PlannerStatsFreshness, bool) {
	f, ok := target.(PlannerStatsFreshener)
	if !ok {
		return PlannerStatsFreshness{}, false
	}
	health, err := f.PlannerStatsHealth(ctx)
	if err != nil {
		return PlannerStatsFreshness{}, false
	}
	return health, true
}
