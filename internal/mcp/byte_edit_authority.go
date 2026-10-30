package mcp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// byteEditRequest is deliberately narrower than generic edit dispatch. Unknown
// shapes retain graph admission. Replacements require target guards; whole-file
// writes preserve their existing optional base_sha and unconditional overwrite
// semantics. The lifecycle sample establishes checkout source authority.
func (s *Server) byteEditRequest(req *mcp.CallToolRequest, freshness requestFreshness, caps capabilityRequest) (*mcp.CallToolRequest, bool) {
	if s == nil || s.lifecycle == nil || req == nil || freshness.requireFresh || freshness.hasDeadline || caps.requireComplete {
		return nil, false
	}
	for _, cap := range append(append([]graphview.CapabilityID{}, caps.required...), caps.optional...) {
		if cap != graphview.CapSourceSnapshot {
			return nil, false
		}
	}
	lowered := *req
	if isFacadeToolName(req.Params.Name) {
		spec, ok := s.viewFacadeOperation(req)
		if !ok || (spec.Legacy != "edit_file" && spec.Legacy != "write_file") {
			return nil, false
		}
		lowered.Params.Name = spec.Legacy
		lowered.Params.Arguments = normalizeFacadeArguments(spec, req.GetArguments())
	} else if req.Params.Name != "edit_file" && req.Params.Name != "write_file" {
		return nil, false
	}
	path := lowered.GetString("path", "")
	if path == "" || len(path) > 4096 || strings.ContainsAny(path, "\x00*?[]{}\n\r") || strings.Contains(path, "::") || lowered.GetBool("dry_run", false) {
		return nil, false
	}
	if lowered.Params.Name == "write_file" {
		// The handler still owns path serialization, optional SHA/idempotency,
		// parse and evidence checks. Whole-file bytes need no graph ranges.
		if _, err := lowered.RequireString("content"); err != nil {
			return nil, false
		}
		return &lowered, true
	}
	old := lowered.GetString("old_string", "")
	next := lowered.GetString("new_string", "")
	sha := normalizeExpectedSHA(lowered.GetString("base_sha", ""))
	raw, err := hex.DecodeString(sha)
	if err != nil || len(raw) != 20 || len(sha) != 40 || old == "" || old == next || lowered.GetInt("expected_occurrences", 0) <= 0 {
		return nil, false
	}
	return &lowered, true
}

// prepareByteEditAuthority runs after all tool/session/repository-scope gates,
// before awaitMutationRoute or graph materialization. Its view is explicitly a
// source-only authority with graph=pending, never an anchored graph reader.
func (s *Server) prepareByteEditAuthority(ctx context.Context, req *mcp.CallToolRequest, selector graphview.Selector, freshness requestFreshness, caps capabilityRequest) (context.Context, *requestView, func(), *mcp.CallToolResult) {
	noop := func() {}
	if checkoutMutationFromContext(ctx) != nil {
		return ctx, nil, noop, nil
	}
	lowered, ok := s.byteEditRequest(req, freshness, caps)
	if !ok || (selector.Kind != graphview.SelectorAuto && selector.Kind != graphview.SelectorWorktree) {
		return ctx, nil, noop, nil
	}
	if OverlayViewFromContext(ctx) != nil {
		return ctx, nil, noop, nil
	}
	snapshot, err := s.snapshotOverlayRequestForCtx(ctx)
	if err != nil || (snapshot != nil && len(snapshot.files) != 0) {
		return ctx, nil, noop, nil
	}
	control, err := s.resolveCheckoutControlScope(ctx, selector, lowered)
	if err != nil {
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	if control == nil || !control.CheckoutScoped {
		return ctx, nil, noop, nil
	}
	if _, err := s.familyPrimary(ctx, control.Checkout.FamilyID); err != nil {
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	mutation, err := beginByteEditAdmission(ctx, func(ctx context.Context) (*indexer.CheckoutMutation, error) {
		return s.lifecycle.BeginCheckoutByteHandoff(ctx, control.Checkout.CheckoutID, control.Checkout.RootPath, control.Checkout.Incarnation)
	})
	if errors.Is(err, indexer.ErrCheckoutByteAnchorUnavailable) || byteEditAdmissionCanReselect(ctx, mutation, err) {
		return ctx, nil, noop, nil
	}
	if err != nil {
		nativeAllowed := s.lifecycle.RecordCheckoutMutationRefusal(ctx, err)
		message := fmt.Sprintf("%s: byte mutation admission refused; no files were changed: %v", graphview.CodeViewReadOnly, err)
		return ctx, nil, noop, mcp.NewToolResultError(checkoutMutationRecoveryMessage(message, err, nativeAllowed))
	}
	root := control.Checkout.RootPath
	info, err := indexer.SourceRootFileInfo(root)
	if err != nil {
		mutation.Close()
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		mutation.Close()
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	rider := graphview.NewViewRider(selector)
	rider.MarkExact(control.Selector.String())
	rider.CheckoutID, rider.GraphID = control.Checkout.CheckoutID, control.GraphID
	view := &requestView{kind: requestViewKindWorktree, rider: rider, viewRoot: root, sourceScope: "file", sourceRootInfo: info, sourceResolvedRoot: resolved, sourceCheckoutIncarnation: control.Checkout.Incarnation, sourceRepoPrefix: control.RepoPrefix, sourceGraphPending: true, declared: graphview.Completeness{graphview.CapSourceSnapshot: graphview.StateComplete}}
	ctx = withRequestView(ctx, view)
	ctx = withCheckoutMutation(ctx, mutation, root)
	// byteEditRequest admits only top-level single-file edit/write shapes.
	checkoutMutationFromContext(ctx).singleFile = true
	ctx = withCheckoutControl(ctx, control)
	if snapshot != nil {
		ctx = withOverlayRequestSnapshot(ctx, snapshot)
	}
	abs, _, err := s.resolveFilePath(ctx, lowered.GetString("path", ""))
	if err == nil {
		err = guardCheckoutMutationPath(ctx, abs)
	}
	if err != nil {
		mutation.Close()
		view.close()
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	return ctx, view, mutation.Close, nil
}

// byteEditAdmissionCanReselect declines only the optional pre-handler admission.
// BeginCheckoutByteMutation has returned without a lease, so its source admission,
// ancestry pins, demand and cycle-lock cleanup has already completed. The ordinary
// whole-view path retains its bounded route/disk reselection/recomposition and
// target guards, including sampled ordinary admission on unsupported HEAD layouts.
// An admitted mutation or a later Prepare/handler failure must never be replayed.
func byteEditAdmissionCanReselect(ctx context.Context, mutation *indexer.CheckoutMutation, err error) bool {
	return mutation == nil && ctx.Err() == nil && (errors.Is(err, indexer.ErrCheckoutMutationRouteMoved) || errors.Is(err, indexer.ErrCheckoutMutationDiskMoved))
}

// beginByteEditAdmission repeats only an exact pre-admission ancestry-pin race.
// Each complete call has released its per-call custody before returning nil.
// Reenter the whole admission with the original context; Prepare and the edit
// handler are outside this helper and must never be replayed.
func beginByteEditAdmission(ctx context.Context, begin func(context.Context) (*indexer.CheckoutMutation, error)) (*indexer.CheckoutMutation, error) {
	mutation, err := begin(ctx)
	if mutation == nil && ctx.Err() == nil && errors.Is(err, indexer.ErrCheckoutMutationRouteMoved) && errors.Is(err, graphview.ErrViewBuilding) {
		return begin(ctx)
	}
	return mutation, err
}
