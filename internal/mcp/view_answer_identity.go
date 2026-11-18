package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// worktreeAnswerIdentity is the exact stack a worktree-view answer was read
// from. A ref view names its content with view_fingerprint alone, because a
// committed tree is immutable; a worktree route moves with every edit, so a
// client that wants to prove two answers came from the same snapshot — or that
// an answer came from the generation an edit's receipt names — needs the
// route's own coordinates: the epoch the view pinned, the commit and dirty
// generations it composed, the incarnation of the checkout, and the source
// fingerprint the dirty generation was built from.
//
// Every value is read from the materialized view itself or from the catalog
// rows it leases, never from a later route read, so it describes the stack
// that answered even if the route moved while the request ran (which
// noteWorktreeRouteDrift reports separately).
type worktreeAnswerIdentity struct {
	routeEpoch         int64
	commitGenerationID int64
	dirtyGenerationID  int64
	incarnation        string
	viewFingerprint    string
	sourceFingerprint  string
}

// worktreeAnswerIdentity derives the identity of a materialized checkout view.
//
// The view pinned the route at CheckoutRouteEpoch, and every route write bumps
// the epoch (FlipCheckoutRoute / FlipCheckoutRouteSlot), so a route row read
// back at that same epoch names exactly the two generations the view composed.
// When the route has moved since, the generations are recovered from the view
// itself instead: the dirty generation is its working-tree layer, and the
// commit generation is the first non-dirty generation below it — a dirty
// layer may sit on a chain of this checkout's earlier working-tree
// generations, so its immediate base is not necessarily the commit layer. A
// catalog read that fails leaves the affected fields out rather than guessing.
func (s *Server) worktreeAnswerIdentity(ctx context.Context, checkout store_sqlite.Checkout, view *graphview.RepoView) *worktreeAnswerIdentity {
	if view == nil {
		return nil
	}
	identity := &worktreeAnswerIdentity{
		routeEpoch:      view.CheckoutRouteEpoch,
		incarnation:     checkout.Incarnation,
		viewFingerprint: view.ID.Fingerprint(),
	}
	for _, layer := range view.ID.Layers {
		if layer.Kind == graphview.LayerDirty && layer.Generation > 0 {
			identity.dirtyGenerationID = layer.Generation
		}
	}
	if s == nil || s.materializer == nil || s.materializer.Catalog == nil {
		return identity
	}
	catalog := s.materializer.Catalog
	if route, found, err := catalog.GetCheckoutRoute(ctx, checkout.CheckoutID); err == nil && found &&
		route.RouteEpoch == view.CheckoutRouteEpoch && route.DirtyGenerationID == identity.dirtyGenerationID {
		identity.commitGenerationID = route.CommitGenerationID
	}
	if identity.dirtyGenerationID == 0 {
		if identity.commitGenerationID == 0 {
			if generations := view.Generations(); len(generations) > 0 {
				identity.commitGenerationID = generations[len(generations)-1]
			}
		}
		return identity
	}
	row, found, err := catalog.GetViewGeneration(ctx, identity.dirtyGenerationID)
	if err != nil || !found {
		return identity
	}
	identity.sourceFingerprint = row.LowerViewFingerprint
	for depth := 0; identity.commitGenerationID == 0 && depth < graphview.MaxGenerationAncestryDepth; depth++ {
		if row.BaseGenerationID <= 0 {
			break
		}
		base, found, err := catalog.GetViewGeneration(ctx, row.BaseGenerationID)
		if err != nil || !found {
			break
		}
		if base.GenerationKind != worktreeDirtyGenerationKind {
			identity.commitGenerationID = base.GenerationID
			break
		}
		row = base
	}
	return identity
}

// worktreeDirtyGenerationKind is the catalog's generation kind for a
// working-tree layer (indexer.DirtyLayerGenerationKind).
const worktreeDirtyGenerationKind = "dirty"

// riderFields renders the identity onto a response rider. A nil identity —
// every view that is not a materialized worktree — renders nothing, so those
// riders stay byte-identical.
func (i *worktreeAnswerIdentity) riderFields(fields map[string]any) {
	if i == nil || fields == nil {
		return
	}
	fields["route_epoch"] = i.routeEpoch
	if i.commitGenerationID > 0 {
		fields["commit_generation_id"] = i.commitGenerationID
	}
	if i.dirtyGenerationID > 0 {
		fields["dirty_generation_id"] = i.dirtyGenerationID
	}
	if i.incarnation != "" {
		fields["checkout_incarnation"] = i.incarnation
	}
	if i.viewFingerprint != "" {
		fields["view_fingerprint"] = i.viewFingerprint
	}
	if i.sourceFingerprint != "" {
		fields["source_fingerprint"] = i.sourceFingerprint
	}
}
