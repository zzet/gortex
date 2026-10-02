package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type importBuildLaneKey struct{}

// Private stepped folds reenter immediately before publishing their payload.
type importFoldPublicationKey struct{}
type importFoldPublication struct{ beforePublish func(context.Context) error }

// A stale detached import starts a new admission cycle rather than retrying
// beneath the context whose interactive yield was already withdrawn.
var errImportPreparationChanged = errors.New("indexer: import preparation needs a new cycle")

// importBuildLane transfers only unpublished one-file delta preparation out
// of the build lane. The cycle still owns its checkout lock and ancestry lease;
// publication reenters the lane before checking the working-copy inputs.
type importBuildLane struct {
	gate               *ViewBuildGate
	detach             func() bool
	resume             func(context.Context, bool) (context.Context, error)
	arm                func(context.Context) error
	detached           bool
	preparationRelease func()
}

func (l *importBuildLane) begin(ctx context.Context) (func(), error) {
	if !l.detached {
		if !l.detach() {
			return nil, ctx.Err()
		}
		l.detached = true
	}
	if l.preparationRelease == nil {
		release, err := l.gate.acquireImportPreparation(ctx)
		if err != nil {
			return nil, err
		}
		var once sync.Once
		l.preparationRelease = func() {
			once.Do(func() { release(); l.preparationRelease = nil })
		}
	}
	return l.preparationRelease, nil
}

func (l *importBuildLane) reenter(ctx context.Context, yieldable bool) (context.Context, error) {
	if l.detached {
		var err error
		ctx, err = l.resume(ctx, yieldable)
		if err != nil {
			return ctx, err
		}
		l.detached = false
	}
	if yieldable && l.arm != nil {
		if err := l.arm(ctx); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

func (l *importBuildLane) leave() {
	if !l.detached {
		l.detach()
		l.detached = true
	}
}

func resumeImportBuildLane(ctx context.Context, yieldable bool) (context.Context, error) {
	if lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane); lane != nil {
		return lane.reenter(ctx, yieldable)
	}
	return ctx, nil
}

// Planning may be slower than the interactive request interval too. It writes
// no payload, and each mutation/full-build boundary reenters the lane. Only an
// ordinary imported delta passing the stricter ancestry checks below remains
// outside it for physical preparation.
func (c *CheckoutCoordinator) prepareImportPlan(ctx context.Context, commit int64, sample gitstate.DirtySnapshot, route store_sqlite.CheckoutRoute) (func(), error) {
	noop := func() {}
	lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane)
	if lane == nil || lane.gate == nil || c.builder == nil || (c.builder.Semantic != nil && !c.defersEnrichment()) {
		return noop, nil
	}
	if len(sample.Entries) <= importInteractivePaths && !c.importInProgress(ctx, route.DirtyGenerationID) {
		return noop, nil
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, commit)
	if err != nil {
		return noop, err
	}
	if !found || row.BaseGenerationID <= 0 {
		return noop, nil
	}
	verdict, err := c.sampler.ConfirmReadSet(ctx, sample, nil, nil)
	if err != nil {
		return noop, err
	}
	if !verdict.Confirmed {
		return noop, nil
	}
	return lane.begin(ctx)
}

func (g *ViewBuildGate) acquireImportPreparation(ctx context.Context) (func(), error) {
	g.mu.Lock()
	if g.importPreparation == nil {
		g.importPreparation = make(chan struct{}, 1)
	}
	slot := g.importPreparation
	g.mu.Unlock()
	select {
	case slot <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-slot }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// importPreparationEpochs excludes the mutable corpus and generations the
// startup correction still needs to modify. A lease alone prevents retirement,
// not writes to generation zero or correction of old derivation versions.
func (b *SparseGenerationBuilder) importPreparationEpochs(ctx context.Context, req BuildRequest) (map[int64]uint64, bool, error) {
	if !req.importBatch || req.followup || len(req.Changes) != 1 || (req.Enrich != nil && b.Semantic != nil) {
		return nil, false, nil
	}
	if req.importReadSetReady == nil || !req.importReadSetReady(ctx) {
		return nil, false, nil
	}
	base, ok := req.Base.(commitLayerBase)
	if !ok || len(base.stack) == 0 {
		return nil, false, nil
	}
	epochs := make(map[int64]uint64, len(base.stack))
	for _, generation := range base.stack {
		if generation <= 0 {
			return nil, false, nil
		}
		stamps, err := b.Store.AtGeneration(generation).DerivationStamps(ctx)
		if err != nil {
			return nil, false, err
		}
		for pass, version := range currentDerivationStamps() {
			if stamps[pass] < version {
				return nil, false, nil
			}
		}
		epochs[generation] = b.Store.GenerationCorrectionEpoch(generation)
	}
	return epochs, true, nil
}

func (b *SparseGenerationBuilder) checkImportPreparationEpochs(epochs map[int64]uint64) error {
	for generation, epoch := range epochs {
		if b.Store.GenerationCorrectionEpoch(generation) != epoch {
			return fmt.Errorf("%w: import ancestor %d was corrected during preparation", ErrDirtySnapshotChanged, generation)
		}
	}
	return nil
}
