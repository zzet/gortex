package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/reconcile"
)

// The checkout administration surface.
//
// Five tools over one shared implementation: the lifecycle's read model
// answers the listings and the previews, and the lifecycle's own flows run the
// confirms. The CLI verbs call these same tools through the daemon, so there
// is one description of what a family looks like and one decision about what a
// destructive verb does — not one per front door.
//
// Every destructive tool is a preview by default. A call without `confirm`
// reads the catalog, returns what would happen, and writes nothing; only an
// explicit `confirm: true` runs the transaction. The one exception is an
// untrack whose plan keeps the checkout — that is not destructive, so it runs
// as it always has.

// registerCheckoutAdminTools registers the checkout administration tools:
// list_checkouts, set_primary_checkout, forget_checkout, reconcile_checkouts
// and explain_view.
func (s *Server) registerCheckoutAdminTools() {
	s.addTool(
		mcp.NewTool("list_checkouts",
			mcp.WithDescription("List the checkout families this daemon tracks. Each family reports its "+
				"primary corpus and epoch, its dedicated graphs, every registered working copy (mode, "+
				"state, both reconciler clocks with their deadlines, path evidence, route and whether a "+
				"build coordinator is live for it), and the named views rooted in its graphs. Reads the "+
				"catalog only — run reconcile_checkouts for a fresh look at the filesystem."),
			mcp.WithString("family", mcp.Description("Narrow to one family: a family id, a graph id, a repo prefix, or a path inside a tracked repository. Omit for every family.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithString("fields", mcp.Description("Comma-separated family fields to keep (for example family_id,primary_graph_id,checkouts). Drops the rest before response budgeting.")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
		),
		s.handleListCheckouts,
	)

	s.addTool(
		mcp.NewTool("set_primary_checkout",
			mcp.WithDescription("Make one corpus the base every automatic checkout of its family composes over. "+
				"Without confirm this previews the move: the incumbent, the family's epoch, whether the move "+
				"would be accepted, and every automatic checkout that has to rebuild its layers over the new base."),
			mcp.WithString("graph", mcp.Required(), mcp.Description("The corpus to promote: a graph id, a repo prefix, or a path inside a tracked repository.")),
			mcp.WithBoolean("confirm", mcp.Description("Run the move. Without it nothing is written.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleSetPrimaryCheckout,
	)

	s.addTool(
		mcp.NewTool("forget_checkout",
			mcp.WithDescription("Remove one checkout, its corpus and everything rooted in it. Unlike untrack_repository "+
				"this never demotes the checkout into the family's automatic lane — it is the deliberate removal. "+
				"Without confirm it previews the closure and writes nothing."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Path or repo prefix naming the checkout to forget")),
			mcp.WithBoolean("confirm", mcp.Description("Run the removal. Without it nothing is written.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleForgetCheckout,
	)

	s.addTool(
		mcp.NewTool("reconcile_checkouts",
			mcp.WithDescription("Reconcile checkout families against git and the filesystem now, instead of waiting "+
				"for the janitor: identities are confirmed or allocated, the availability and removal clocks move, "+
				"and the build coordinators are brought in line with the verdicts."),
			mcp.WithString("family", mcp.Description("Reconcile one family: a family id, a graph id, a repo prefix, or a path inside a tracked repository. Omit to reconcile every family the daemon knows.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleReconcileCheckouts,
	)

	s.addTool(
		mcp.NewTool("explain_view",
			mcp.WithDescription("Explain which graph answers for one filesystem path: the checkout the path binds to, "+
				"how that checkout is served, its route and the generations behind it — or, when no composed view "+
				"answers, the step in the chain that could not be taken and left the base corpus to answer."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Filesystem path to explain")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleExplainView,
	)
}

// handleListCheckouts answers the administrative read model.
func (s *Server) handleListCheckouts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	overview, err := s.lifecycle.FamiliesOverview(ctx, req.GetString("family", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if s.isTOON(ctx, req) || s.isGCX(ctx, req) {
		_, payload := prepareCheckoutOverviewResponseWithSizer(req, overview, checkoutOverviewTOONSize)
		return returnTOON(payload)
	}
	renderReq, payload := prepareCheckoutOverviewResponse(req, overview)
	return s.respondJSONOrTOON(ctx, renderReq, payload)
}

type checkoutOverviewSizer func(any) (int, error)

func checkoutOverviewJSONSize(payload any) (int, error) {
	encoded, err := json.Marshal(payload)
	return len(encoded), err
}

// checkoutOverviewTOONSize measures the exact document returnTOON will put on
// the wire, including its JSON fallback when the TOON encoder cannot represent
// a value. Prefix selection must use this size: TOON is usually compact, but a
// nested object-heavy census can be larger than JSON.
func checkoutOverviewTOONSize(payload any) (int, error) {
	result, err := returnTOON(payload)
	if err != nil {
		return 0, err
	}
	text, ok := checkoutOverviewResultText(result)
	if !ok {
		return 0, errors.New("checkout overview encoder returned no text content")
	}
	return len(text), nil
}

func checkoutOverviewResultText(result *mcp.CallToolResult) (string, bool) {
	if result == nil || len(result.Content) == 0 {
		return "", false
	}
	text, ok := result.Content[0].(mcp.TextContent)
	return text.Text, ok
}

// prepareCheckoutOverviewResponse applies list_checkouts' shape-aware response
// pipeline before the shared formatter.
// FamiliesOverview deliberately nests its potentially large collections under
// each family. Sending that shape straight to the generic budgeter leaves the
// outer `families` slice as its only candidate: a single family with hundreds
// of worktrees is therefore reduced from one family to zero instead of losing
// a suffix of its checkout rows.
//
// The order mirrors respondJSONOrTOON exactly: fields project first, token
// decoration reserves its bytes, structural trimming fits the remaining
// content, and decoration lands only after a trim. The cloned render request
// then disables those two already-completed stages so the common formatter
// cannot reserve twice or erase the preserved family on its second guard.
// Format selection still uses the original request arguments. An explicit
// max_bytes:0 bypasses trimming and returns the exhaustive census.
func prepareCheckoutOverviewResponse(
	req mcp.CallToolRequest,
	overview indexer.FamiliesOverview,
) (mcp.CallToolRequest, any) {
	return prepareCheckoutOverviewResponseWithSizer(req, overview, nil)
}

// A nil size selects the JSON fast path, which reuses the first encoded value
// as the detached-map source instead of marshaling the large census twice.
func prepareCheckoutOverviewResponseWithSizer(
	req mcp.CallToolRequest,
	overview indexer.FamiliesOverview,
	size checkoutOverviewSizer,
) (mcp.CallToolRequest, any) {
	payload := applyFieldsFilter(overview, parseFields(req.GetString("fields", "")))
	budget := effectiveBudget(req)
	if budget > 0 {
		reserve := tokenBudgetDecorationReserve(req)
		decorate := reserve == 0 || reserve < budget
		if reserve > 0 && decorate {
			budget -= reserve
		}
		var trimmed bool
		if size == nil {
			payload, trimmed = applyCheckoutOverviewBudget(payload, budget)
		} else {
			payload, trimmed = applyCheckoutOverviewBudgetWithSizer(payload, budget, size)
		}
		if trimmed && decorate {
			payload = decorateTokenBudgetJSON(payload, req)
		}
	}

	args := make(map[string]any, len(req.GetArguments())+1)
	for key, value := range req.GetArguments() {
		args[key] = value
	}
	delete(args, "fields")
	delete(args, "max_tokens")
	args["max_bytes"] = 0
	req.Params.Arguments = args
	return req, payload
}

// applyCheckoutOverviewBudget preserves family envelopes while trimming the
// three row collections nested below them: checkouts, graphs and ref_views.
// It follows the generic budgeter's stable-prefix and exact-count contract,
// but places each count beside the nested list it describes. The root marker
// lets format-agnostic callers detect that some nested data was omitted.
//
// If every nested list is exhausted and the response is still oversized, the
// generic top-level budgeter trims family envelopes as the documented
// scalar-floor fallback. The input payload is never mutated.
func applyCheckoutOverviewBudget(payload any, maxBytes int) (any, bool) {
	if maxBytes <= 0 {
		return payload, false
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) <= maxBytes {
		return payload, false
	}
	return applyOversizedCheckoutOverviewBudget(payload, raw, maxBytes, checkoutOverviewJSONSize)
}

func applyCheckoutOverviewBudgetWithSizer(
	payload any,
	maxBytes int,
	size checkoutOverviewSizer,
) (any, bool) {
	if maxBytes <= 0 {
		return payload, false
	}
	encodedSize, err := size(payload)
	if err != nil || encodedSize <= maxBytes {
		return payload, false
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return payload, false
	}
	return applyOversizedCheckoutOverviewBudget(payload, raw, maxBytes, size)
}

func applyOversizedCheckoutOverviewBudget(
	payload any,
	raw []byte,
	maxBytes int,
	size checkoutOverviewSizer,
) (any, bool) {
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return payload, false
	}
	families, ok := generic["families"].([]any)
	if !ok {
		return applyBudget(payload, maxBytes)
	}

	candidates := checkoutOverviewNestedLists(families)
	if len(candidates) == 0 {
		return applyCheckoutOverviewFamilyBudget(generic, families, maxBytes, size), true
	}

	// Start from the family-envelope skeleton. Growing stable prefixes from
	// zero avoids the destructive longest-first failure mode where one large
	// list is emptied because the other still occupies the budget, then never
	// gets a chance to grow again after the other is trimmed.
	for i := range candidates {
		candidate := &candidates[i]
		stampCheckoutOverviewTrim(generic, candidate.family, candidate.key, nil, len(candidate.rows))
	}
	encodedSize, sizeErr := size(generic)
	if sizeErr != nil {
		return payload, false
	}
	if encodedSize > maxBytes {
		// Even every family shell plus exact nested counts does not fit. The
		// top-level fallback keeps a stable family prefix and records how many
		// envelopes were omitted. If its zero-family scalar skeleton is still
		// too large, that valid document is the same documented floor JSON uses.
		return applyCheckoutOverviewFamilyBudget(generic, families, maxBytes, size), true
	}

	// First preserve one identifying row from every non-empty collection.
	// Candidate order is checkouts across all families, then graphs, then ref
	// views, so every family keeps its @main checkout before lower-priority
	// administrative detail consumes the remaining budget.
	for i := range candidates {
		candidate := &candidates[i]
		candidate.kept = 1
		candidate.family[candidate.key] = candidate.rows[:1]
		cleanupCheckoutOverviewTrimMetadata(generic, families, candidates)
		encodedSize, sizeErr = size(generic)
		if sizeErr != nil {
			return payload, false
		}
		if encodedSize <= maxBytes {
			continue
		}
		candidate.kept = 0
		candidate.family[candidate.key] = candidate.rows[:0]
		cleanupCheckoutOverviewTrimMetadata(generic, families, candidates)
	}

	// Spend the rest of the budget on deterministic stable prefixes in the
	// same priority order. Earlier families win additional rows, but the head
	// pass above prevents a later family from losing its identifying checkout.
	// Revisit higher-priority prefixes only when a later collection becomes
	// complete and sheds metadata. An unconditional fixed-point pass doubles
	// every high-cardinality fit even when no bytes were freed.
	for {
		revisitEarlier := false
		for i := range candidates {
			candidate := &candidates[i]
			_, completed := growCheckoutOverviewPrefix(generic, families, candidates, candidate, maxBytes, size)
			if completed && checkoutOverviewHasEarlierTrimmed(candidates, i) {
				revisitEarlier = true
			}
		}
		if !revisitEarlier {
			break
		}
	}

	return generic, true
}

// applyCheckoutOverviewFamilyBudget is the last structural floor after nested
// rows are gone. It returns the largest stable family prefix whose actual
// renderer output fits, plus exact outer-list metadata. When even the empty
// prefix is larger than maxBytes, the valid empty-family document is returned
// intact rather than byte-slicing JSON or TOON into invalid syntax.
func applyCheckoutOverviewFamilyBudget(
	root map[string]any,
	families []any,
	maxBytes int,
	size checkoutOverviewSizer,
) any {
	root[budgetTruncatedKey] = true
	root["_original_count_families"] = len(families)
	lo, hi := 0, len(families)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		root["families"] = families[:mid]
		root["_max_returned_families"] = mid
		encodedSize, err := size(root)
		if err == nil && encodedSize <= maxBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	root["families"] = families[:lo]
	root["_max_returned_families"] = lo
	return root
}

// growCheckoutOverviewPrefix expands one stable prefix as far as the current
// family census permits. A full collection is tried separately because
// dropping its two count fields can make the full representation smaller than
// the penultimate prefix, which would violate an ordinary binary search's
// monotonicity assumption.
func growCheckoutOverviewPrefix(
	root map[string]any,
	families []any,
	candidates []checkoutOverviewNestedList,
	candidate *checkoutOverviewNestedList,
	maxBytes int,
	size checkoutOverviewSizer,
) (grew bool, completed bool) {
	before := candidate.kept
	if before >= len(candidate.rows) {
		return false, false
	}

	candidate.kept = len(candidate.rows)
	candidate.family[candidate.key] = candidate.rows
	cleanupCheckoutOverviewTrimMetadata(root, families, candidates)
	if encodedSize, err := size(root); err == nil && encodedSize <= maxBytes {
		return true, true
	}

	lo, hi := before, len(candidate.rows)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		candidate.kept = mid
		candidate.family[candidate.key] = candidate.rows[:mid]
		cleanupCheckoutOverviewTrimMetadata(root, families, candidates)
		encodedSize, err := size(root)
		if err == nil && encodedSize <= maxBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	candidate.kept = lo
	candidate.family[candidate.key] = candidate.rows[:lo]
	cleanupCheckoutOverviewTrimMetadata(root, families, candidates)
	return lo > before, false
}

func checkoutOverviewHasEarlierTrimmed(candidates []checkoutOverviewNestedList, before int) bool {
	for i := 0; i < before; i++ {
		if candidates[i].kept < len(candidates[i].rows) {
			return true
		}
	}
	return false
}

type checkoutOverviewNestedList struct {
	familyIndex int
	family      map[string]any
	key         string
	rows        []any
	kept        int
}

// checkoutOverviewNestedLists orders collections by administrative value and
// then by the catalog's stable family order. Every checkout collection gets a
// head row before any graph or ref-view collection, and before one family's
// tail can crowd out another family's identity.
func checkoutOverviewNestedLists(families []any) []checkoutOverviewNestedList {
	var out []checkoutOverviewNestedList
	for _, key := range []string{"checkouts", "graphs", "ref_views"} {
		for familyIndex, rawFamily := range families {
			family, ok := rawFamily.(map[string]any)
			if !ok {
				continue
			}
			rows, ok := family[key].([]any)
			if !ok || len(rows) == 0 {
				continue
			}
			out = append(out, checkoutOverviewNestedList{
				familyIndex: familyIndex,
				family:      family,
				key:         key,
				rows:        rows,
			})
		}
	}
	return out
}

func stampCheckoutOverviewTrim(
	root map[string]any,
	family map[string]any,
	key string,
	rows []any,
	originalLen int,
) {
	family[key] = rows
	family[budgetTruncatedKey] = true
	family["_max_returned_"+key] = len(rows)
	family["_original_count_"+key] = originalLen
	root[budgetTruncatedKey] = true
}

func cleanupCheckoutOverviewTrimMetadata(
	root map[string]any,
	families []any,
	candidates []checkoutOverviewNestedList,
) {
	familyTrimmed := make([]bool, len(families))
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.kept < len(candidate.rows) {
			familyTrimmed[candidate.familyIndex] = true
			candidate.family["_max_returned_"+candidate.key] = candidate.kept
			candidate.family["_original_count_"+candidate.key] = len(candidate.rows)
			continue
		}
		delete(candidate.family, "_max_returned_"+candidate.key)
		delete(candidate.family, "_original_count_"+candidate.key)
	}
	for familyIndex, rawFamily := range families {
		family, ok := rawFamily.(map[string]any)
		if !ok {
			continue
		}
		if familyTrimmed[familyIndex] {
			family[budgetTruncatedKey] = true
		} else {
			delete(family, budgetTruncatedKey)
		}
	}
	root[budgetTruncatedKey] = true
}

// handleSetPrimaryCheckout previews or runs a primary move.
func (s *Server) handleSetPrimaryCheckout(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	target, err := req.RequireString("graph")
	if err != nil {
		return mcp.NewToolResultError("graph is required"), nil
	}
	dedicated, err := s.lifecycle.ResolveGraph(ctx, target)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	preview, err := s.lifecycle.PreviewSetPrimary(ctx, dedicated.GraphID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	payload := map[string]any{
		"family_id":     preview.FamilyID,
		"graph_id":      preview.GraphID,
		"repo_prefix":   preview.RepoPrefix,
		"primary_epoch": preview.PrimaryEpoch,
		"ready":         preview.Ready,
	}
	if preview.CurrentGraphID != "" {
		payload["current_graph_id"] = preview.CurrentGraphID
		payload["current_repo_prefix"] = preview.CurrentRepoPrefix
	}
	if len(preview.Blockers) > 0 {
		payload["blockers"] = preview.Blockers
	}
	if len(preview.Dependents) > 0 {
		payload["dependents"] = renderDependents(preview.Dependents)
	}

	if !req.GetBool("confirm", false) {
		payload["status"] = "preview"
		payload["confirm_required"] = true
		payload["detail"] = "nothing was written; call set_primary_checkout again with confirm:true to move the family primary"
		return s.respondJSONOrTOON(ctx, req, payload)
	}

	result, err := s.lifecycle.SetPrimary(ctx, dedicated.GraphID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	payload["status"] = "primary_set"
	payload["rebuilt"] = result.Rebuilt
	if len(result.Stale) > 0 {
		payload["stale"] = result.Stale
		payload["stale_detail"] = "these checkouts kept the route they had; the next reconcile tries again"
	}
	if len(result.Errors) > 0 {
		messages := make([]string, 0, len(result.Errors))
		for _, e := range result.Errors {
			messages = append(messages, e.Error())
		}
		payload["errors"] = messages
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// handleForgetCheckout previews or runs a deliberate removal.
func (s *Server) handleForgetCheckout(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	preview, err := s.lifecycle.PreviewForget(ctx, path)
	if errors.Is(err, indexer.ErrCheckoutNotTracked) {
		return repoNotTrackedGuidance(path), nil
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if !req.GetBool("confirm", false) {
		return s.respondJSONOrTOON(ctx, req, untrackPreviewPayload("forget", preview,
			"nothing was written; call forget_checkout again with confirm:true to remove it"))
	}
	result, err := s.lifecycle.ApplyUntrack(ctx, preview)
	if err != nil {
		return untrackFailure(path, err), nil
	}
	return s.respondJSONOrTOON(ctx, req, untrackResultPayload("forgotten", result))
}

// handleReconcileCheckouts forces a reconciliation pass.
func (s *Server) handleReconcileCheckouts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	if selector := req.GetString("family", ""); selector != "" {
		familyID, err := s.lifecycle.ResolveFamilyID(ctx, selector)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		report, err := s.lifecycle.ReconcileFamily(ctx, familyID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status":   "reconciled",
			"families": []map[string]any{renderFamilyReport(report)},
			// Counted for the family that was asked about, the way the
			// whole-daemon scope counts every family. Leaving it out renders a
			// family whose build loops are running as one running none.
			"coordinators": s.lifecycle.LiveCoordinators(familyID),
		})
	}

	sweep, err := s.lifecycle.Sweep(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	families := make([]map[string]any, 0, len(sweep.Reports))
	for _, report := range sweep.Reports {
		families = append(families, renderFamilyReport(report))
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"status":            "reconciled",
		"families":          families,
		"removed":           sweep.Removed,
		"coordinators":      sweep.Coordinators,
		"retired":           sweep.Retired,
		"ref_views_retired": sweep.RefViewsRetired,
	})
}

// handleExplainView walks one path's binding chain.
func (s *Server) handleExplainView(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	binding, err := s.lifecycle.ExplainView(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return s.respondJSONOrTOON(ctx, req, binding)
}

// destructiveUntrackPlan reports whether a plan removes anything a confirm has
// to be asked for. A plan that keeps the checkout — an eviction of a
// repository with no catalog identity, or a demotion into the family's
// automatic lane — is the ordinary untrack and runs as it always has.
func destructiveUntrackPlan(plan indexer.UntrackPlan) bool {
	switch plan {
	case indexer.UntrackPlanForget, indexer.UntrackPlanPrimaryClosure:
		return true
	default:
		return false
	}
}

// untrackPreviewPayload renders one previewed plan.
func untrackPreviewPayload(action string, preview indexer.UntrackPreview, detail string) map[string]any {
	payload := map[string]any{
		"status":           "preview",
		"action":           action,
		"plan":             string(preview.Plan),
		"prefix":           preview.Prefix,
		"accessible":       preview.Accessible,
		"is_primary":       preview.IsPrimary,
		"confirm_required": true,
		"detail":           detail,
	}
	for name, value := range map[string]string{
		"checkout_id": preview.CheckoutID,
		"family_id":   preview.FamilyID,
		"graph_id":    preview.GraphID,
	} {
		if value != "" {
			payload[name] = value
		}
	}
	if preview.IsPrimary {
		payload["sole_primary"] = preview.SolePrimary
		payload["primary_epoch"] = preview.PrimaryEpoch
	}
	if len(preview.Closure) > 0 {
		payload["closure"] = renderDependents(preview.Closure)
	}
	if len(preview.Preserved) > 0 {
		payload["preserved"] = renderDependents(preview.Preserved)
	}
	if len(preview.Blockers) > 0 {
		payload["blockers"] = preview.Blockers
	}
	return payload
}

// untrackResultPayload renders one executed plan.
func untrackResultPayload(status string, result indexer.UntrackResult) map[string]any {
	payload := map[string]any{
		"status":        status,
		"plan":          string(result.Plan),
		"prefix":        result.Prefix,
		"nodes_removed": result.NodesRemoved,
		"edges_removed": result.EdgesRemoved,
	}
	if result.Demoted {
		payload["demoted"] = true
	}
	if len(result.Revoked) > 0 {
		payload["revoked_intents"] = result.Revoked
	}
	if len(result.Dependents) > 0 {
		dependents := make([]string, 0, len(result.Dependents))
		for _, dep := range result.Dependents {
			dependents = append(dependents, dep.Detail)
		}
		payload["dependents"] = dependents
	}
	return payload
}

// renderDependents projects closure rows into the response.
func renderDependents(dependents []reconcile.Dependent) []map[string]string {
	out := make([]map[string]string, 0, len(dependents))
	for _, dep := range dependents {
		out = append(out, map[string]string{
			"kind":   string(dep.Kind),
			"id":     dep.ID,
			"detail": dep.Detail,
		})
	}
	return out
}

// renderFamilyReport projects one reconciliation verdict.
func renderFamilyReport(report reconcile.FamilyReport) map[string]any {
	checkouts := make([]map[string]any, 0, len(report.Checkouts))
	for _, entry := range report.Checkouts {
		row := map[string]any{
			"admin_name": entry.AdminName,
			"root_path":  entry.RootPath,
			"action":     string(entry.Action),
			"durable":    entry.Durable,
		}
		for name, value := range map[string]string{
			"checkout_id": entry.CheckoutID,
			"state":       string(entry.State),
			"disposition": string(entry.Classification.Disposition),
			"evidence":    string(entry.Classification.Evidence),
			"code":        entry.Classification.Code,
			"detail":      entry.Detail,
		} {
			if value != "" {
				row[name] = value
			}
		}
		checkouts = append(checkouts, row)
	}
	out := map[string]any{
		"family_id":        report.FamilyID,
		"common_dir":       report.CommonDir,
		"inventory_usable": report.InventoryUsable,
		"checkouts":        checkouts,
	}
	if report.PrimaryGraphID != "" {
		out["primary_graph_id"] = report.PrimaryGraphID
	}
	if report.Code != "" {
		out["code"] = report.Code
	}
	return out
}

// untrackFailure renders a lifecycle refusal the way the untrack tool always
// has: a non-revocable intent names the source still asking for the
// repository, and a blocked plan names the ways forward.
func untrackFailure(path string, err error) *mcp.CallToolResult {
	if errors.Is(err, reconcile.ErrIntentNotRevocable) {
		return mcp.NewToolResultError(fmt.Sprintf(
			"cannot untrack %s: it is still wanted by another tracking source (%v)", path, err))
	}
	return mcp.NewToolResultError(err.Error())
}
