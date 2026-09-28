package indexer

import (
	"context"

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
// batch and is never stopped here.

// drainYieldRows is the sub-batch a yieldable drain writes between checks of
// its context.
var drainYieldRows = 1024

// drainSubBatchHook, when set (tests), runs before every sub-batch write of a
// yieldable drain.
var drainSubBatchHook func()

// drainYieldable reports whether ctx is a background build's context armed
// to give the build lane up (it carries the build commit point).
func drainYieldable(ctx context.Context) bool {
	return ctx != nil && ctx.Value(buildCommitPointKey{}) != nil
}

// drainAddBatch writes one drain chunk to target. For a yieldable drain it
// writes in sub-batches and returns the context's error, having written
// only the sub-batches before it, as soon as the context is canceled.
func drainAddBatch(ctx context.Context, target graph.Store, nodes []*graph.Node, edges []*graph.Edge) error {
	if !drainYieldable(ctx) {
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
