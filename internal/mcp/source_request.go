package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Source operations prove the bytes they serve rather than wait for unrelated
// relationship publication. Explicit graph capabilities and immutable selectors
// retain whole-view selection and its freshness contract.
func (s *Server) resolveSourceRequestView(ctx context.Context, selector graphview.Selector, req *mcp.CallToolRequest, name string, freshness requestFreshness, capabilities capabilityRequest) (*requestView, error) {
	if name != "read_file" || req.GetString("keep", "") != "" || installedSkillPath(req.GetString("path", "")) {
		return nil, nil
	}
	for _, cap := range capabilities.required {
		if cap != graphview.CapSourceSnapshot {
			return nil, nil
		}
	}
	if selector.Kind != graphview.SelectorAuto && selector.Kind != graphview.SelectorWorktree {
		return nil, nil
	}
	control, err := s.resolveCheckoutControlScope(ctx, selector, req)
	if err != nil || control == nil {
		return nil, err
	}
	checkout := control.Checkout
	if selector.Kind == graphview.SelectorAuto && !graphview.ServesAutomaticView(checkout) {
		return nil, nil
	}
	if checkout.State != store_sqlite.CheckoutStateReady {
		return nil, graphview.NewViewError(graphview.CodeCheckoutInaccessible, fmt.Sprintf("checkout %q is %s", checkout.CheckoutID, checkout.State))
	}
	if _, err := s.familyPrimary(ctx, checkout.FamilyID); err != nil {
		return nil, err
	}
	rider := graphview.NewViewRider(selector)
	rider.MarkExact(control.Selector.String())
	rider.CheckoutID, rider.GraphID = checkout.CheckoutID, control.GraphID
	paths := s.pendingSourcePaths(checkout.CheckoutID, checkout.Incarnation)
	for i, path := range paths {
		if relative, err := filepath.Rel(checkout.RootPath, path); err == nil {
			paths[i] = filepath.ToSlash(relative)
		}
	}
	view := &requestView{
		kind: requestViewKindWorktree, rider: rider, viewRoot: checkout.RootPath,
		sourceScope: "file", sourceCheckoutIncarnation: checkout.Incarnation, sourceRepoPrefix: control.RepoPrefix, sourcePendingPaths: paths,
		sourceGraphPending: len(paths) > 0 || control.Route == nil || control.Route.State != store_sqlite.RouteActive || control.Route.DirtyGenerationID == 0,
		declared:           graphview.Completeness{graphview.CapSourceSnapshot: graphview.StateComplete},
	}
	if freshness.requested() {
		view.freshness = &requestFreshnessOutcome{deadline: freshness.effectiveDeadline(time.Now(), ctx)}
	}
	return view, nil
}

func sourceRequestView(ctx context.Context) *requestView {
	view := requestViewFromContext(ctx)
	if view != nil && view.sourceScope != "" {
		return view
	}
	return nil
}

// The request pins one editor cohort. Include tombstones and new files rather
// than accidentally falling through to older on-disk bytes.
func (s *Server) sourceOverlayFile(ctx context.Context, absPath string) (content string, present, deleted bool, baseSHA string) {
	if sourceRequestView(ctx) == nil {
		return
	}
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if !ok || snapshot == nil || !snapshot.canonical {
		return
	}
	for _, file := range snapshot.files {
		resolved, err := s.resolveOverlayRequestAbsPath(ctx, file.Path)
		if err == nil && filepath.Clean(resolved) == filepath.Clean(absPath) {
			return file.Content, true, file.Deleted, file.BaseSHA
		}
	}
	return
}
