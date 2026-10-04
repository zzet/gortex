package indexer

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// A background checkout cycle holds the one build lane for its whole build,
// and gives it up to an edit-driven build by canceling its work context
// (armBackgroundLaneYield, armTreeMoveAbort). The durable drain of a whole
// index — the shadow's rows moved to disk in row/byte-capped chunks — never
// looked at that context: a background cycle bulk-indexing another repository
// kept the lane for the rest of its drain (live: an edit waited 114 s, the
// drain ran 454 s at ~9 s per 8,192-row chunk on a loaded host).
//
// Under an armed background yield the drain now writes each chunk in
// sub-batches of drainYieldRows rows and stops at the first sub-batch
// boundary after the context is canceled; the build then fails with the
// cancellation, publishes nothing, and the cycle reschedules through the
// yield (yieldedCycle). Any other drain (a foreground index, an edit's own
// build, a whole index outside the coordinator) writes the chunk as one
// batch and is never stopped here, except the initial claimed base. That base
// uses the same sub-batches but pauses in place for announced interactive work,
// without abandoning its claim or rescheduling the build.

// drainYieldRows is the sub-batch a cooperative drain writes between checks of
// its context and, for the initial claimed base, interactive write demand.
var drainYieldRows = 1024

// drainSubBatchHook, when set (tests), runs before every sub-batch write of a
// cooperative drain.
var drainSubBatchHook func()

type initialDrainCooperationKey struct{}

// Only the initial claimed base pauses in place for announced checkout work.
// Its claim and bulk window stay owned throughout; this is not lane yielding.
func withInitialDrainCooperation(ctx context.Context, wanted func() bool) context.Context {
	return context.WithValue(ctx, initialDrainCooperationKey{}, wanted)
}

func initialDrainDemand(ctx context.Context) func() bool {
	if ctx == nil {
		return nil
	}
	wanted, _ := ctx.Value(initialDrainCooperationKey{}).(func() bool)
	return wanted
}

// A continuously announced ticket must not prevent initial publication. Each
// bounded stand-down is followed by one slice, even when demand remains.
const initialDrainStandDownMax = 100 * time.Millisecond

func awaitInitialDrainTurn(ctx context.Context, wanted func() bool) error {
	if wanted == nil || !wanted() {
		return ctx.Err()
	}
	bound := time.NewTimer(initialDrainStandDownMax)
	defer bound.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for wanted() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-bound.C:
			return ctx.Err()
		case <-poll.C:
		}
	}
	return ctx.Err()
}

func drainChecksCancellation(ctx context.Context) bool {
	return drainYieldable(ctx) || initialDrainDemand(ctx) != nil
}

// drainYieldable reports whether ctx is a background build's context armed
// to give the build lane up (it carries the build commit point).
func drainYieldable(ctx context.Context) bool {
	return ctx != nil && ctx.Value(buildCommitPointKey{}) != nil
}

// drainAddBatch writes one drain chunk to target. A lane-yieldable or initial
// claimed drain uses sub-batches and stops on context cancellation. The initial
// claimed drain also stands down, boundedly, for interactive demand before each
// slice; ordinary and foreground drains keep their single-batch behavior.
func drainAddBatch(ctx context.Context, target graph.Store, nodes []*graph.Node, edges []*graph.Edge) error {
	wanted := initialDrainDemand(ctx)
	if !drainYieldable(ctx) && wanted == nil {
		target.AddBatch(nodes, edges)
		return nil
	}
	step := drainYieldRows
	if step <= 0 {
		step = 1024
	}
	for len(nodes) > 0 || len(edges) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The preceding slice has committed and released the store's writer.
		if err := awaitInitialDrainTurn(ctx, wanted); err != nil {
			return err
		}
		if drainSubBatchHook != nil {
			drainSubBatchHook()
		}
		if len(nodes) > 0 {
			n := min(len(nodes), step)
			target.AddBatch(nodes[:n], nil)
			nodes = nodes[n:]
			continue
		}
		n := min(len(edges), step)
		target.AddBatch(nil, edges[:n])
		edges = edges[n:]
	}
	return nil
}
