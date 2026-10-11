package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestImportFoldPlanningRestoresFallbackAndDeclineOwnership(t *testing.T) {
	for _, mode := range []string{"mutable_ancestry", "missing_generation", "cancelled", "reentry_failure"} {
		t.Run(mode, func(t *testing.T) {
			f, c, lifecycle := mcpChainFixture(t, builderTreeA(), false)
			var out CheckoutCycle
			for i := 0; i < dirtyChainCompactionDepth; i++ {
				out = mcpEdit(t, lifecycle, f, func() { foldWiringEdits[i](t, f) })
			}
			if out.DirtyChainDepth != dirtyChainCompactionDepth {
				t.Fatalf("chain depth=%d, want %d", out.DirtyChainDepth, dirtyChainCompactionDepth)
			}
			base, closeBase, err := c.generationLayerReader(t.Context(), out.DirtyGenerationID)
			if err != nil {
				t.Fatal(err)
			}
			_, eligible, err := c.builder.importPreparationEpochs(t.Context(), BuildRequest{
				Base: base, importBatch: true,
				Changes:            []LayerPathChange{{Path: "fold", Kind: LayerPathAdded}},
				importReadSetReady: func(context.Context) bool { return true },
			})
			closeBase()
			if err != nil || eligible {
				t.Fatalf("fixture must retain mutable generation-zero ancestry: eligible=%v err=%v", eligible, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c.cycleMu.Lock()
			defer c.cycleMu.Unlock()
			release, err := c.gate.Acquire(ctx, ViewBuildBackground)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { release() }()
			resumed := 0
			admissionErr := &ViewBuildQueueFullError{Priority: ViewBuildBackground, Limit: 1}
			lane := &importBuildLane{
				gate:   c.gate,
				detach: func() bool { release(); release = func() {}; return true },
				resume: func(ctx context.Context, yieldable bool) (context.Context, error) {
					if yieldable {
						t.Error("fallback did not explicitly restore the protected caller lane")
					}
					if mode == "reentry_failure" {
						return ctx, admissionErr
					}
					next, err := c.gate.Acquire(ctx, ViewBuildBackground)
					if err == nil {
						release = next
						resumed++
					}
					return ctx, err
				},
			}
			ctx = context.WithValue(ctx, importBuildLaneKey{}, lane)
			entered := false
			c.importFoldPlanningBarrier = func(context.Context) {
				entered = true
				if !lane.detached || c.gate.Stats().Active {
					t.Error("read-only fold preparation kept the physical lane")
				}
				if mode == "cancelled" {
					cancel()
				}
			}
			defer func() { c.importFoldPlanningBarrier = nil }()
			route := f.route()
			before := route.DirtyGenerationID
			heldBefore := c.leases.Held()
			if mode == "missing_generation" {
				out.DirtyGenerationID = 1 << 50
			}
			c.foldImportChain(ctx, out.CommitGenerationID, &route, &out)
			if !entered {
				t.Fatal("fold preparation barrier was not reached")
			}
			wantResumed := 1
			if mode == "mutable_ancestry" {
				// The fallback reacquires for payload mutation, again after
				// off-lane prewarm, then restores caller ownership after the
				// retained publication tail is released.
				wantResumed = 3
			}
			if mode == "cancelled" || mode == "reentry_failure" {
				wantResumed = 0
			}
			if resumed != wantResumed || c.gate.Stats().Active != (wantResumed != 0) {
				t.Fatalf("resumed=%d active=%v, want %d", resumed, c.gate.Stats().Active, wantResumed)
			}
			if mode == "reentry_failure" {
				// The optional fold reports no further payload write. Its slot
				// caller's final resume must still propagate refused admission.
				if _, err := resumeImportBuildLane(ctx, false); !errors.Is(err, admissionErr) {
					t.Fatalf("final slot admission returned %v, want %v", err, admissionErr)
				}
			}
			if c.leases.Held() != heldBefore {
				t.Fatalf("planning leaked ancestry: before=%d after=%d", heldBefore, c.leases.Held())
			}
			slotCtx, stopSlot := context.WithTimeout(t.Context(), time.Second)
			defer stopSlot()
			slotRelease, err := c.gate.acquireImportPreparation(slotCtx)
			if err != nil {
				t.Fatal(err)
			}
			slotRelease()
			if mode == "mutable_ancestry" {
				if !out.ImportFolded || f.route().DirtyGenerationID == before {
					t.Fatal("one-shot fallback did not publish and route its fold")
				}
				chainAssertFlat(t, f, "mutable-import-fold-fallback")
			} else if f.route().DirtyGenerationID != before || out.ImportFolded {
				t.Fatal("declined preparation changed the routed chain")
			}
			for _, generation := range f.generations() {
				if generation.State == store_sqlite.ViewGenerationBuilding {
					t.Fatalf("preparation left an unfinished generation %d", generation.GenerationID)
				}
			}
		})
	}
}
