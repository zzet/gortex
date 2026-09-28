package indexer

import (
	"context"
	"slices"
	"time"

	"go.uber.org/zap"
)

// A chain fold in steps, and what lands it.
//
// Round 15 folds a chain in one write transaction, at about 30 µs per row:
// 0.3 s for a few files, about 13 s for hundreds. A fold that long cannot sit
// between an agent's edits, and one an edit cancels starts over. The store
// folds in steps instead (BeginChainFold / Step: each step one write
// transaction sized to about 50 ms of the write gate, given back to an edit's
// announced write and refused over a WAL mark, the target a building
// generation no view resolves, the members leased until Release; a restart
// abandons a partial fold, which the orphaned-building recovery sweeps). This
// file is the coordinator's side:
//
//   - a fold runs step after step and is never cancelled by an edit: the
//     edit publishes above the chain meanwhile (steps yield to its writes in
//     the store), and a stopped fold resumes from its cursor;
//   - when the last step commits, the fold is verified and published, and it
//     lands (planFoldLanding): when nothing was published above the folded
//     chain, the route's dirty slot flips to the fold; when layers were, the
//     lowest of them is rebased onto the fold — one guarded catalog
//     transition that rewrites that one row's base, since the fold is
//     content-equal to the chain it replaces and the layer's rows and masks
//     are relative to that content — and the layers above it keep theirs;
//   - an edit chains above the chain, a running fold or not, up to the
//     physical cap; only at the cap does it fold in its own cycle, as the last
//     resort, bounded in time and unverified (chainBoundAction,
//     dirty_chain_fold_bound.go).

// foldStepRetryPoll is how long the driver waits before it asks for the next
// step after the store gave a step back to an edit or refused one on a WAL
// mark. The store sizes each step itself (about 50 ms of the write gate).
const foldStepRetryPoll = 50 * time.Millisecond

// maxPhysicalChainDepth bounds the layers a view composes while a fold runs:
// the effective depth stays under maxDirtyChainDepth, the physical one (the
// folding prefix plus what landed above it) under this. A read costs about
// 3 ms per layer (measured), so 16 layers cost about 50 ms per read until the
// fold lands.
const maxPhysicalChainDepth = 2 * maxDirtyChainDepth

// chainFoldSteps is one fold in progress in the store.
type chainFoldSteps interface {
	// Step copies one time-sized step (about 50 ms of the write gate) in one
	// write transaction; done reports that the last step committed. A step an
	// edit's write interrupts, or one a WAL mark refuses, returns an error the
	// backend classifies as retryable, with the cursor unchanged.
	Step(ctx context.Context) (done bool, err error)
	// Release drops the fold's lease and owner references (after the publish,
	// or to give up and resume later).
	Release(ctx context.Context) error
	// Abandon marks the target failed and drops the cursor; the sweep deletes
	// what the steps wrote.
	Abandon(ctx context.Context) error
}

// chainFoldBackend is what the coordinator needs from the store.
type chainFoldBackend interface {
	BeginChainFold(ctx context.Context, chain []int64, to int64, owner string) (chainFoldSteps, error)
	RebaseViewGeneration(ctx context.Context, generationID, fromBase, toBase int64) error
	// StepRetryable reports a step the store gave back (an edit's write was
	// announced) or refused (a WAL mark): wait and step again.
	StepRetryable(err error) bool
}

// runChainFoldSteps runs a fold to its last step. Nothing it observes cancels
// it but ctx (shutdown, the checkout's retirement): an edit publishes above
// the chain meanwhile, and a step the store gives back to an edit's write or
// refuses on a WAL mark is asked for again after foldStepRetryPoll. A ctx that
// ends leaves the fold resumable within the process (Release it, or keep it).
func runChainFoldSteps(ctx context.Context, fold chainFoldSteps, retryable func(error) bool, afterStep func(step int)) (steps, retries int, err error) {
	for {
		done, err := fold.Step(ctx)
		if err != nil {
			if ctx.Err() == nil && retryable != nil && retryable(err) {
				retries++
				timer := time.NewTimer(foldStepRetryPoll)
				select {
				case <-ctx.Done():
					timer.Stop()
					return steps, retries, ctx.Err()
				case <-timer.C:
				}
				continue
			}
			return steps, retries, err
		}
		steps++
		if afterStep != nil {
			afterStep(steps)
		}
		if done {
			return steps, retries, nil
		}
	}
}

// foldLanding is how a finished fold enters the route.
type foldLanding struct {
	// kind is foldLandFlip (nothing above the folded chain: the dirty slot
	// flips to the fold), foldLandRebase (the lowest layer above it is
	// rebased), or foldLandMoved (the route no longer stands on the folded
	// chain: the fold becomes the next build's preferred parent).
	kind string
	// rebase is the layer to rebase, from the folded chain's top to the fold.
	rebase, from int64
}

const (
	foldLandFlip   = "flip"
	foldLandRebase = "rebase"
	foldLandMoved  = "moved"
)

// planFoldLanding decides how a fold of folded (oldest first) lands on the
// routed chain (oldest first).
func planFoldLanding(routed, folded []int64) foldLanding {
	if len(folded) == 0 || len(routed) < len(folded) || !slices.Equal(routed[:len(folded)], folded) {
		return foldLanding{kind: foldLandMoved}
	}
	if len(routed) == len(folded) {
		return foldLanding{kind: foldLandFlip}
	}
	return foldLanding{kind: foldLandRebase, rebase: routed[len(folded)], from: folded[len(folded)-1]}
}

// effectiveChainDepth is the depth an edit over routed (oldest first) is held
// to: a folding prefix counts as the one layer it becomes.
func effectiveChainDepth(routed, folding []int64) int {
	if len(folding) > 1 && len(routed) >= len(folding) && slices.Equal(routed[:len(folding)], folding) {
		return len(routed) - len(folding) + 1
	}
	return len(routed)
}

// What an edit over the routed chain does (chainBoundAction).
const (
	chainActionChain     = "chain"      // publish above the chain
	chainActionFoldAll   = "fold_all"   // no fold runs: fold the chain in the cycle
	chainActionFoldUpper = "fold_upper" // a fold runs: fold the layers above it in the cycle
)

// chainBoundAction is the rule at the bound. An edit chains while the
// physical depth leaves room for its layer: the background fold, started at
// dirtyChainCompactionDepth, is what keeps the chain short, and an edit never
// folds while it runs. Only at the physical cap does an edit fold in its own
// cycle, as the last resort and under a time bound (dirty_chain_fold_bound.go):
// the whole chain when no fold runs, or only the layers above the running
// fold when one does.
// The physical limit is maxChainWalkDepth: a chain never outgrows what a reader
// can compose.
func chainBoundAction(routed, folding []int64) string {
	return chainBoundActionWithin(routed, folding, maxChainWalkDepth)
}

// chainBoundActionWithin is chainBoundAction under an explicit physical limit.
func chainBoundActionWithin(routed, folding []int64, physical int) string {
	if len(routed) < physical {
		return chainActionChain
	}
	if len(folding) > 1 && len(routed) > len(folding) && slices.Equal(routed[:len(folding)], folding) {
		return chainActionFoldUpper
	}
	return chainActionFoldAll
}

// landChainFold applies a finished, verified and published fold of folded
// (oldest first) into to. routed is the routed chain (oldest first) read under
// the cycle lock. flip moves the dirty slot; the backend rebases; release owes
// the folded members a retirement (in production, releaseDirty for each).
func (c *CheckoutCoordinator) landChainFold(
	ctx context.Context, backend chainFoldBackend, routed, folded []int64, to int64,
	flip func(ctx context.Context, to int64) error, release func(ctx context.Context, folded []int64),
) (foldLanding, error) {
	landing := planFoldLanding(routed, folded)
	switch landing.kind {
	case foldLandFlip:
		if err := flip(ctx, to); err != nil {
			return landing, err
		}
		release(ctx, folded)
	case foldLandRebase:
		if err := backend.RebaseViewGeneration(ctx, landing.rebase, landing.from, to); err != nil {
			return landing, err
		}
		release(ctx, folded)
	default:
		c.setPreferredDirtyParent(to, "folded while the route moved on")
	}
	if c.logger != nil {
		c.logger.Info("checkout coordinator: chain fold landed",
			zap.String("checkout", c.checkoutID), zap.String("landing", landing.kind),
			zap.Int64("folded_generation", to), zap.Int64("rebased", landing.rebase),
			zap.Int("folded_layers", len(folded)), zap.Int("routed_layers", len(routed)))
	}
	return landing, nil
}
