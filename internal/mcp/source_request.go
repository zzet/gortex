package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/search/rerank"
)

// Source operations prove the bytes they serve rather than wait for unrelated
// relationship publication. Explicit graph capabilities and immutable selectors
// retain whole-view selection and its freshness contract.
func (s *Server) resolveSourceRequestView(ctx context.Context, selector graphview.Selector, req *mcp.CallToolRequest, name string, freshness requestFreshness, capabilities capabilityRequest) (*requestView, error) {
	policyReq := *req
	if isFacadeToolName(req.Params.Name) {
		spec, ok := s.viewFacadeOperation(req)
		if !ok {
			return nil, nil
		}
		policyReq.Params.Name = spec.Legacy
		policyReq.Params.Arguments = normalizeFacadeArguments(spec, req.GetArguments())
		req = &policyReq
	}
	scope := ""
	fq := fieldQuery{}
	switch name {
	case "read_file":
		if req.GetString("keep", "") != "" || installedSkillPath(req.GetString("path", "")) {
			return nil, nil
		}
		scope = "file"
	case "search_text":
		scope = "text"
	case "search_symbols":
		if capabilities.requireComplete {
			return nil, nil
		}
		fq = parseFieldQuery(req.GetString("query", ""))
		for _, char := range fq.Text {
			if !unicode.IsLetter(char) && !unicode.IsDigit(char) && !strings.ContainsRune("_.$:", char) {
				return nil, nil
			}
		}
		class := rerank.ClassifyQuery(fq.Text)
		if pinned := strings.TrimSpace(req.GetString("query_class", "")); pinned != "" {
			parsed, ok := rerank.ParseQueryClass(pinned)
			if !ok {
				return nil, nil
			}
			if parsed != rerank.QueryClassUnknown {
				class = parsed
			}
		}
		corpus, err := parseCorpus(*req)
		if err != nil || corpus != corpusCode || class != rerank.QueryClassSymbol || strings.TrimSpace(fq.Text) == "" || parseAssistMode(*req) == assistDeep || parseAssistMode(*req) == assistOn || req.GetBool("debug", false) || isCompact(*req) || s.isGCX(ctx, *req) {
			return nil, nil
		}
		scope = "declarations"
		policyReq = requestWithInlineScopeClauses(*req, fq)
		req = &policyReq
	default:
		return nil, nil
	}
	for _, cap := range capabilities.required {
		if cap != graphview.CapSourceSnapshot && !(scope == "text" && cap == graphview.CapSearchText) && !(scope == "declarations" && cap == graphview.CapSearchSymbols) {
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
	resolvedRoot, err := filepath.EvalSymlinks(checkout.RootPath)
	if err != nil {
		return nil, err
	}
	rootInfo, err := indexer.SourceRootFileInfo(checkout.RootPath)
	if err != nil {
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
	graphPending := len(paths) > 0 || control.Route == nil || control.Route.State != store_sqlite.RouteActive || control.Route.DirtyGenerationID == 0
	if scope != "file" && !graphPending && len(s.resolvePathFilter(*req, fq)) == 0 {
		return nil, nil
	}
	view := &requestView{
		sourceRootInfo: rootInfo, sourceResolvedRoot: resolvedRoot,
		kind: requestViewKindWorktree, sourceRequestFreshness: freshness, sourceCapabilities: capabilities, rider: rider, viewRoot: checkout.RootPath,
		sourceScope: scope, sourceCheckoutIncarnation: checkout.Incarnation, sourceRepoPrefix: control.RepoPrefix, sourcePendingPaths: paths,
		sourceGraphPending: graphPending,
		declared:           graphview.Completeness{graphview.CapSourceSnapshot: graphview.StateComplete},
	}
	if scope != "file" {
		resolved, err := s.resolveScopeForRequest(ctx, *req, IntentLocate)
		if err != nil {
			return nil, err
		}
		if !s.sourceSearchDomainContained(view, resolved) {
			return nil, nil
		}
		if scope == "text" {
			view.declared[graphview.CapSearchText] = graphview.StateComplete
		} else {
			view.declared[graphview.CapSearchSymbols] = graphview.StateComplete
		}
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

// Broad source queries that exceed the scanner's resource budget retain the
// existing indexed path. Waiting here uses the original request budget.
func (s *Server) fallbackSourceSearch(ctx context.Context, req mcp.CallToolRequest, source *requestView, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)) (*mcp.CallToolResult, error) {
	freshness := source.sourceRequestFreshness
	freshness.requireFresh = true
	if source.freshness != nil {
		freshness.deadline = source.freshness.deadline
		freshness.hasDeadline = true
	}
	selector := graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: source.rider.CheckoutID}
	view, err := s.resolveRequestView(ctx, selector, requestViewPolicy{freshness: freshness})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	source.sourceFallback = view
	ctx = withRequestView(ctx, view)
	noteRetainedRequest(ctx, view, requestRepositoryScopeFromContext(ctx))
	if refused := s.evaluateRequestCapabilities(ctx, &req, source.sourceCapabilities); refused != nil {
		return refused, nil
	}
	ctx, _, err = s.prepareOverlayRequest(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return handler(ctx, req)
}

func (s *Server) sourceSearchDomainContained(view *requestView, resolved ResolvedScope) bool {
	if resolved.RepoAllow != nil {
		for prefix, allowed := range resolved.RepoAllow {
			if allowed && prefix != view.sourceRepoPrefix {
				return false
			}
		}
		return resolved.RepoAllow[view.sourceRepoPrefix]
	}
	if s.multiIndexer == nil {
		idx := s.sourceSearchIndexer(view)
		return idx != nil && (resolved.WorkspaceID == "" || resolved.WorkspaceID == idx.WorkspaceID()) && (resolved.ProjectID == "" || resolved.ProjectID == idx.ProjectID())
	}
	found := false
	for _, prefix := range s.multiIndexer.RepoPrefixes() {
		idx := s.multiIndexer.GetIndexer(prefix)
		if idx == nil {
			continue
		}
		if resolved.WorkspaceID != "" && resolved.WorkspaceID != idx.WorkspaceID() {
			continue
		}
		if resolved.ProjectID != "" && resolved.ProjectID != idx.ProjectID() {
			continue
		}
		if prefix != view.sourceRepoPrefix {
			return false
		}
		found = true
	}
	return found
}
