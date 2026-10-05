package indexer

import (
	"context"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The file-by-file import of a large working-tree change.
//
// A working tree whose change set exceeds importInteractivePaths — a `git
// checkout <branch> -- <paths>`, a restore, a mass edit, the first cycle after
// a restart that found hundreds of edited files — is not built as one direct
// generation, nor in a handful of large batches. Each cycle imports ONE file as
// a chained generation over the previous one and hands the build lane back:
// an interactive build waits at most for one file, and progress survives every
// yield, abort and restart. Whenever the chain reaches the compaction depth it
// is folded by copy (dirty_chain_flatten.go), so the next file always chains
// and nothing already imported is ever re-parsed. The last file's generation
// describes the working tree and completes the import like any other cycle.
//
// A change set of at most importInteractivePaths is one delta.
//
// A HEAD move's committed half is not imported this way yet: the commit layer
// of the new tree is still one build of diff(base, new tree).

// importBatchFiles is how many working-tree files one import cycle builds.
var importBatchFiles = 1

// importInteractivePaths is the largest working-tree change built as one
// delta; a larger one is imported file by file.
var importInteractivePaths = 32

// foldImportChain folds the chain an import cycle just extended when it has
// reached the compaction depth, and routes the fold in the cycle's own route
// write. A fold that is refused leaves the chain routed; the next link then
// takes the chain's own depth rules.
func (c *CheckoutCoordinator) foldImportChain(
	ctx context.Context, commitGeneration int64, route *store_sqlite.CheckoutRoute, out *CheckoutCycle,
) {
	if out.DirtyChainDepth < dirtyChainCompactionDepth || out.DirtyGenerationID <= 0 {
		return
	}
	lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane)
	var releasePreparation, closeBase func()
	if lane != nil && lane.gate != nil && c.builder != nil && commitGeneration > 0 {
		// Catalog traversal and ancestry materialization can outlast a
		// foreground request too. Release the physical lane before this
		// read-only planning; the routed chain remains retained, and the
		// materializer pins its ancestry before assembling the reader.
		var err error
		releasePreparation, err = lane.begin(ctx)
		if err != nil {
			return
		}
		defer func() {
			// Every live decline restores the caller's ownership. Keep the
			// preparation slot and any ancestry lease through that reentry.
			if ctx.Err() == nil {
				_, _ = lane.reenter(ctx, false)
			}
			releasePreparation()
			if closeBase != nil {
				closeBase()
			}
		}()
	}
	if c.importFoldPlanningBarrier != nil {
		c.importFoldPlanningBarrier(ctx)
	}
	commit, found, err := c.catalog.GetViewGeneration(ctx, commitGeneration)
	if err != nil || !found {
		return
	}
	copier := c.copyChainAtOnce
	if releasePreparation != nil {
		// The copied inputs are sealed generations, held throughout verification
		// and publication. The stepped copier also protects its copy reservation.
		var base LayerBase
		base, closeBase, err = c.generationLayerReader(ctx, out.DirtyGenerationID)
		if err != nil {
			return
		}
		epochs, eligible, err := c.builder.importPreparationEpochs(ctx, BuildRequest{
			Base: base, importBatch: true,
			Changes:            []LayerPathChange{{Path: "fold", Kind: LayerPathAdded}},
			importReadSetReady: func(context.Context) bool { return true },
		})
		if err != nil {
			return
		}
		if eligible {
			ctx = context.WithValue(ctx, importFoldPublicationKey{}, &importFoldPublication{
				beforePublish: func(ctx context.Context) error {
					if _, err := lane.reenter(ctx, false); err != nil {
						return err
					}
					return c.builder.checkImportPreparationEpochs(epochs)
				},
				// The sealed payload is immutable; its catalog readback and
				// copy-owner cleanup do not need the physical build lane. The
				// ancestry/preparation pins remain held until the guarded flip.
				afterPublish: lane.leave,
			})
			copier = c.copyChainInSteps
		} else {
			// An unqualified ancestry uses the ordinary one-shot copy; it
			// must own the physical lane before any payload mutation.
			ctx, err = lane.reenter(ctx, false)
			if err != nil {
				return
			}
		}
	}
	built, err := c.flattenDirtyChainOver(ctx, commit, commit, out.DirtyGenerationID, copier)
	if err != nil {
		c.logger.Debug("checkout coordinator: import chain not folded",
			zap.String("checkout", c.checkoutID), zap.Error(err))
		return
	}
	previous := route.DirtyGenerationID
	if err := c.flip(ctx, route, store_sqlite.RouteSlotDirty, built.GenerationID); err != nil {
		c.abandonCopiedGeneration(context.WithoutCancel(ctx), built.GenerationID)
		return
	}
	c.retainDirty(ctx, built.Key, built.GenerationID)
	c.releaseImportPublicationLane(ctx)
	c.releaseDirtyChain(ctx, previous, built.GenerationID)
	out.DirtyGenerationID = built.GenerationID
	out.DirtyChainDepth = 1
	out.ImportFolded = true
}

// importInProgress reports whether the working-tree parent a build stands on
// is a batch of an import that has not reached the working tree yet: its
// fingerprint names a partial state (partialWorkingTreeManifest).
func (c *CheckoutCoordinator) importInProgress(ctx context.Context, parent int64) bool {
	if parent <= 0 {
		return false
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, parent)
	return err == nil && found && strings.HasPrefix(row.LowerViewFingerprint, "partial:")
}

// releaseImportPublicationLane ends only the physical publication hold. The
// route CAS and retention have succeeded; ancestry/cache bookkeeping is not
// publication. The checkout cycle lock and existing ancestry/preparation
// ownership remain. Every later fold mutation or route flip explicitly reenters.
func (c *CheckoutCoordinator) releaseImportPublicationLane(ctx context.Context) {
	if lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane); lane != nil {
		lane.leave()
		if c.importPublicationTailBarrier != nil {
			c.importPublicationTailBarrier(ctx)
		}
	}
}
