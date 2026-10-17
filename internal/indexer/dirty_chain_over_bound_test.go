package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The rollback case for a raised chain bound: a checkout whose route names the
// top of a working-tree chain longer than this binary's bound (a newer binary
// built it with a higher one). The chain is built through the coordinator's
// own build with an explicit parent, which no bound check sits on, and routed
// directly; then this binary, with its bound, serves and edits the checkout.
// What it must do: refuse the over-deep view with a retryable, labelled error
// (never serve a truncated one), and re-root the chain over the commit on the
// next edit, after which the checkout is served again.
func TestDirtyChainDeeperThanTheBoundOnDisk(t *testing.T) {
	const onDisk = graphview.MaxDirtyChainDepth + 4
	f := newCoordinatorFixtureIndexedBy(t, kindParityTree(), builderIndex)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	ctx := context.Background()
	route := f.route()
	commit, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil || !found {
		t.Fatalf("commit generation: %v", err)
	}
	src := kindParityTree()["prod/sib_b.go"]
	var chain []int64
	for i := 0; i < onDisk; i++ {
		builderWriteFile(t, f.worktree, "prod/sib_b.go", src+fmt.Sprintf("\n// edit %d\nfunc Edit%d() int { return %d }\n", i, i, i))
		// The layer below is composed directly (dirtyChainComposed has no
		// bound): the coordinator's own reader refuses a parent past this
		// binary's bound, which is exactly what a newer binary did not.
		identity := c.dirtyIdentity(commit.GraphID, route.CommitGenerationID)
		req := DirtyLayerRequest{
			Identity:     identity,
			Base:         commitLayerBase{Reader: f.store.AtGeneration(0), corpus: f.store, stack: []int64{route.CommitGenerationID}},
			CheckoutRoot: c.root, RepoPrefix: c.repoPrefix, WorkspaceID: c.workspaceID, ProjectID: c.projectID,
			Sampler: c.sampler,
		}
		if len(chain) > 0 {
			manifest, why := loadDirtyChainManifest(ctx, c.store, chain)
			if why != "" {
				t.Fatalf("link %d: the chain's manifest: %s", i, why)
			}
			parent := chain[len(chain)-1]
			req.Base = commitLayerBase{Reader: dirtyChainComposed(t, f.store, chain), corpus: f.store,
				stack: append([]int64{route.CommitGenerationID}, chain...)}
			req.Identity.BaseGenerationID = parent
			req.parent, req.parentManifest, req.parentDepth = parent, manifest, len(chain)
		} else {
			req.Identity.BaseGenerationID = route.CommitGenerationID
		}
		id, report, err := c.builder.BuildDirtyLayer(ctx, req)
		if err != nil || id <= 0 {
			t.Fatalf("link %d: build %d: %v", i, id, err)
		}
		if len(chain) > 0 && report.ParentGenerationID != chain[len(chain)-1] {
			t.Fatalf("link %d built direct (%q)", i, report.ChainFallbackReason)
		}
		chain = append(chain, id)
	}
	func() {
		c.cycleMu.Lock()
		defer c.cycleMu.Unlock()
		current := f.route()
		if err := c.flip(ctx, &current, store_sqlite.RouteSlotDirty, chain[len(chain)-1]); err != nil {
			t.Fatalf("route the %d-deep chain: %v", onDisk, err)
		}
	}()

	// Served: refused, labelled, retryable.
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := materializer.MaterializeCheckout(ctx, f.checkoutID)
	if err == nil {
		view.Close()
		t.Fatalf("a %d-deep chain was served under the bound %d", onDisk, graphview.MaxDirtyChainDepth)
	}
	var tooDeep *graphview.AncestryTooDeepError
	if !errors.As(err, &tooDeep) {
		t.Fatalf("serving the over-deep chain failed with %T %v, want AncestryTooDeepError", err, err)
	}
	t.Logf("served: refused (%v)", err)

	// The next cycle: with the tree unchanged since the last link, and after
	// one more edit.
	out := coordinatorReconcile(t, c)
	t.Logf("cycle with the tree unchanged: dirty built %v depth %d reason %q route dirty %d", out.DirtyBuilt, out.DirtyChainDepth, out.DirtyChainReason, f.route().DirtyGenerationID)
	builderWriteFile(t, f.worktree, "prod/sib_b.go", src+"\n// after the rollback\n")
	out = coordinatorReconcile(t, c)
	t.Logf("cycle after an edit: dirty built %v depth %d reason %q", out.DirtyBuilt, out.DirtyChainDepth, out.DirtyChainReason)
	if !out.DirtyBuilt || out.DirtyChainDepth > graphview.MaxDirtyChainDepth {
		t.Fatalf("the edit after the rollback did not re-root the chain: %+v", out)
	}
	view, err = materializer.MaterializeCheckout(ctx, f.checkoutID)
	if err != nil {
		t.Fatalf("the checkout is not served after the re-rooting edit: %v", err)
	}
	view.Close()
}
