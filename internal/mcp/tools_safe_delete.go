package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// registerSafeDeleteSymbolTool wires safe_delete_symbol — atomic
// dead-code removal with a graph-aware safety gate. Before touching
// disk, the tool checks for referencing edges (calls, implements,
// extends, references); a non-zero count rejects the delete unless
// the caller passes force=true.
//
// Default is dry_run=true: returns the planned delete (line range +
// preview of the bytes that would disappear) without writing. The
// agent gets one round-trip to inspect, then flips dry_run=false to
// commit.
//
// Cascade (orphan propagation). When `cascade` is "preview" or
// "apply", the tool computes the transitive closure of symbols that
// would become orphaned by the delete — every symbol whose only
// referencing in-edges originate from the target itself or another
// symbol already in the closure. Cross-workspace references and
// test-only references (unless `cascade_into_tests` is true)
// disqualify a candidate. Closure is reported on every response;
// only `cascade: "apply"` actually deletes the orphan tail.
func (s *Server) registerSafeDeleteSymbolTool() {
	s.addTool(
		mcp.NewTool("safe_delete_symbol",
			mcp.WithDescription("Atomically delete a symbol from the file system, with a graph-aware safety gate. Computes referencing edges first (calls / implements / extends / references); if any exist, the delete is REJECTED unless force=true. Default dry_run=true returns the preview without writing — flip dry_run=false to commit. The deleted range covers the symbol body plus any leading doc-comment block. The graph is re-indexed on commit so subsequent queries see the new state. Pass cascade=\"preview\" to also compute the transitive orphan closure (symbols that would become dead code once the target is gone) without deleting them, or cascade=\"apply\" to delete the closure in the same operation. Cross-workspace references and test-only references (unless cascade_into_tests is true) disqualify a candidate from the closure."),
			mcp.WithString("id", mcp.Description("Symbol ID (e.g. pkg/foo.go::Bar).")),
			mcp.WithBoolean("dry_run", mcp.Description("When true (default), returns the planned delete without writing. Set false to commit.")),
			mcp.WithBoolean("force", mcp.Description("Bypass the referencing-edge check. Use when you've already removed every caller in the same change set. Default false.")),
			mcp.WithString("cascade", mcp.Description("Orphan propagation mode: \"off\" (default — single-symbol delete only), \"preview\" (compute the transitive orphan closure and return it without deleting), or \"apply\" (compute the closure and delete every symbol in it together with the target).")),
			mcp.WithBoolean("cascade_into_tests", mcp.Description("When true, symbols referenced only from test files are eligible for the cascade closure. Default false — test-only references disqualify a candidate so the cascade never deletes a symbol just because production stopped using it but tests still do.")),
			mcp.WithBoolean("propagate", mcp.Description("Instead of rejecting on referencing edges, build a per-caller delete-and-patch plan: standalone statement calls are removed outright (parse-gate validated), embedded references are flagged for manual patching. dry_run returns the plan; dry_run=false applies the removable patches then deletes the symbol (force=true to delete even with manual / parse-blocked sites remaining).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleSafeDeleteSymbol,
	)
}

// safeDeleteReference describes a single referencing edge — enough
// information for the caller to navigate to it and remove it before
// retrying the delete.
type safeDeleteReference struct {
	FromID   string `json:"from_id,omitempty"`
	Kind     string `json:"kind"`
	FromName string `json:"from_name,omitempty"`
	FilePath string `json:"file_path,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// cascadeClosureEntry describes a symbol that the cascade pass added
// to the deletion closure. Carries enough context for the caller to
// understand why the symbol was selected and locate it.
type cascadeClosureEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// cascadeMode enumerates the accepted values of the `cascade`
// parameter. Anything else is treated as cascadeModeOff so callers
// that supply an unknown value get the conservative behaviour.
const (
	cascadeModeOff     = "off"
	cascadeModePreview = "preview"
	cascadeModeApply   = "apply"
)

// cascadeIterationCap bounds the fixed-point loop defensively. The
// algorithm converges by construction (D only grows), but a runaway
// graph or a future bug should not be able to spin forever.
const cascadeIterationCap = 50

func (s *Server) handleSafeDeleteSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	dryRun := requestBoolDefault(req, "dry_run", true)
	force := req.GetBool("force", false)
	cascadeMode := normaliseCascadeMode(req.GetString("cascade", cascadeModeOff))
	cascadeIntoTests := req.GetBool("cascade_into_tests", false)

	g := s.readerFor(ctx)

	node := g.GetNode(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}
	if node.StartLine == 0 || node.EndLine == 0 {
		return mcp.NewToolResultError("symbol has no line range: " + id), nil
	}

	// Safety check: referencing edges. We keep the four edge kinds
	// that signal actual code-level use; structural edges like
	// EdgeDefines / EdgeMemberOf are skipped (they don't represent
	// "someone calls this").
	refs := collectReferencingEdges(g, id)
	// A call that an edit added and the graph has not read yet blocks the
	// delete like a graph reference does: a provisional reference here, a
	// manual patch in propagate mode. The same snapshot labels the answer.
	gaps := s.laggingMutationReceipts(ctx)
	provisional := s.provisionalSites(g, gaps, node)
	for _, site := range provisional {
		refs = append(refs, safeDeleteReference{Kind: "provisional", FilePath: site.File, Line: site.Line})
	}
	var finishWrites func()
	var primaryAbs string
	var primaryCommit *mutationCommitRecord
	var primaryOutcome mutationReindexOutcome
	respond := func(result map[string]any) (*mcp.CallToolResult, error) {
		if finishWrites != nil {
			finishWrites()
		}
		if primaryCommit != nil {
			if primaryOutcome.Err != nil {
				result["reindex_error"] = primaryOutcome.Err.Error()
			}
			s.attachMutationFreshness(result, node.FilePath, primaryAbs, primaryOutcome)
			attachMutationCommit(result, primaryCommit)
		}
		label := s.labelRelations(ctx, gaps, []string{id}, nil)
		label.apply(result)
		res, err := s.respondJSONOrTOON(ctx, req, result)
		return label.decorate(res), err
	}

	// SRN-7: propagate-delete — patch the surviving call sites instead of
	// refusing. Builds a per-caller plan, applies the removable standalone
	// calls (parse-gate validated), and flags embedded references as manual.
	var appliedPatches, failedPatches []callerPatch
	var propagationPlan []callerPatch
	if req.GetBool("propagate", false) && len(refs) > 0 {
		var plan []callerPatch
		if dryRun {
			plan = s.buildPropagationPreview(ctx, g, node)
		} else {
			plan = s.buildPropagationPlan(ctx, g, node)
		}
		for _, site := range provisional {
			reason := "provisional: named in a file whose edit the graph has not read yet — patch by hand"
			if site.Line == 0 {
				reason = "provisional: the graph has not read the latest edit to the symbol's own file — retry once it publishes"
			}
			plan = append(plan, callerPatch{
				File: site.File, Line: site.Line, CurrentText: site.Text, Action: "manual", Reason: reason,
			})
		}
		manual := countManual(plan)
		if dryRun {
			needsRevalidation := false
			for _, patch := range plan {
				needsRevalidation = needsRevalidation || patch.NeedsSourceRevalidation
			}
			hint := "re-run with dry_run=false to apply the verified remove_line patches and delete the symbol; resolve manual patches first or pass force=true"
			if needsRevalidation {
				hint = "this is a source preview, not write authorization: publish or revalidate the changed source and scoped references before requesting apply"
			}
			return respond(map[string]any{
				"status":                       "propagation_plan",
				"source_revalidation_required": needsRevalidation,
				"symbol":                       id,
				"file":                         node.FilePath,
				"caller_patches":               plan,
				"removable":                    len(plan) - manual,
				"manual":                       manual,
				"reference_count":              len(refs),
				"dry_run":                      true,
				"hint":                         hint,
			})
		}
		if manual > 0 && !force {
			return respond(map[string]any{
				"status":         "propagation_blocked_manual",
				"symbol":         id,
				"caller_patches": plan,
				"manual":         manual,
				"hint":           fmt.Sprintf("%d call site(s) need manual patching; resolve them or pass force=true to delete anyway", manual),
			})
		}
		propagationPlan = plan
	}

	if len(refs) > 0 && !force && len(propagationPlan) == 0 {
		return respond(map[string]any{
			"status":             "rejected_has_references",
			"symbol":             id,
			"file":               node.FilePath,
			"references":         refs,
			"reference_count":    len(refs),
			"dry_run":            dryRun,
			"force":              force,
			"cascade_mode":       cascadeMode,
			"cascade_into_tests": cascadeIntoTests,
			"hint":               "remove every referencing edge first, or pass force=true to override",
		})
	}

	// Compute the cascade closure first so both preview and apply
	// branches surface the same list to the caller. Off mode skips
	// the work entirely.
	var (
		closure          []cascadeClosureEntry
		cascadeTruncated bool
	)
	if cascadeMode != cascadeModeOff {
		closure, cascadeTruncated = computeCascadeClosure(g, node, cascadeIntoTests)
	}
	if cascadeMode == cascadeModeApply {
		// The closure is orphaned by the graph's edges only. A call that an
		// unpublished edit added keeps a member alive, and force does not
		// cover the members, so the cascade waits for that edit.
		var blocked []safeDeleteReference
		var members []string
		for _, entry := range closure {
			member := g.GetNode(entry.ID)
			if member == nil {
				continue
			}
			sites := s.provisionalSites(g, gaps, member)
			if len(sites) > 0 {
				members = append(members, entry.ID)
			}
			for _, site := range sites {
				blocked = append(blocked, safeDeleteReference{Kind: "provisional", FilePath: site.File, Line: site.Line})
			}
		}
		if len(blocked) > 0 {
			return respond(map[string]any{
				"status":          "rejected_has_references",
				"symbol":          id,
				"file":            node.FilePath,
				"references":      blocked,
				"reference_count": len(blocked),
				"dry_run":         dryRun,
				"force":           force,
				"cascade_mode":    cascadeMode,
				"cascade_closure": closure,
				"hint":            "cascade member(s) " + strings.Join(members, ", ") + " are named in files whose edits the graph has not read yet; retry once those publish, or delete without cascade",
			})
		}
	}

	absPath, err := s.resolveNodePath(ctx, node)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read file: %v", err)), nil
	}
	// Refuse the same way edit_file does: the deletion splices graph-derived
	// UTF-8 lines into NUL-interleaved bytes (#846). This also covers the
	// dry-run preview, whose raw-byte line split would be NUL-laden garbage.
	if indexer.LooksUTF16Source(content) {
		return mcp.NewToolResultError(refuseUTF16Edit("safe_delete_symbol", node.FilePath).Error()), nil
	}
	lines := strings.Split(string(content), "\n")
	if dryRun {
		start, end, err := s.nodeLinesIn(ctx, node, content)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		resolved := *node
		resolved.StartLine, resolved.EndLine = start, end
		node = &resolved
	}
	// An actual delete resolves again only under its complete held path set.
	// These initial response fields are replaced by the applied snapshot.
	deleteStart, deleteEnd, linesDeleted := 0, 0, 0
	deletedChunk := ""
	if node.StartLine >= 1 && node.EndLine >= node.StartLine && node.EndLine <= len(lines) {
		deleteStart, deleteEnd = expandDeleteRange(node, lines)
		deletedChunk = strings.Join(lines[deleteStart-1:deleteEnd], "\n")
		linesDeleted = deleteEnd - deleteStart + 1
	}

	result := map[string]any{
		"symbol":             id,
		"file":               node.FilePath,
		"start_line":         deleteStart,
		"end_line":           deleteEnd,
		"lines_deleted":      linesDeleted,
		"reference_count":    len(refs),
		"references":         refs,
		"preview":            deletedChunk,
		"dry_run":            dryRun,
		"force":              force,
		"cascade_mode":       cascadeMode,
		"cascade_into_tests": cascadeIntoTests,
	}
	if cascadeMode != cascadeModeOff {
		result["cascade_closure"] = closure
		result["cascade_truncated"] = cascadeTruncated
	}

	if dryRun {
		result["status"] = "preview"
		return respond(result)
	}

	// Commit phase. cascadeModeApply expands the work to every entry
	// in the closure; off / preview only ever touch the target.
	pending := []*pendingDelete{{
		node:  node,
		abs:   absPath,
		start: deleteStart,
		end:   deleteEnd,
	}}
	if cascadeMode == cascadeModeApply {
		for _, entry := range closure {
			cn := g.GetNode(entry.ID)
			if cn == nil {
				return mcp.NewToolResultError(fmt.Sprintf("cascade target disappeared from graph: %s", entry.ID)), nil
			}
			if cn.StartLine == 0 || cn.EndLine == 0 {
				return mcp.NewToolResultError(fmt.Sprintf("cascade target has no line range: %s", entry.ID)), nil
			}
			cAbs, err := s.resolveNodePath(ctx, cn)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("resolve cascade target %s: %v", entry.ID, err)), nil
			}
			pending = append(pending, &pendingDelete{
				node:  cn,
				abs:   cAbs,
				start: 0,
				end:   0,
			})
		}
	}

	// One final union lock covers every source effect. A wait behind another
	// writer cannot silently reuse the old reference/closure/patch authority.
	paths := make([]string, 0, len(pending)+len(propagationPlan))
	for _, item := range pending {
		paths = append(paths, item.abs)
	}
	for _, patch := range propagationPlan {
		if patch.Action == "remove_line" {
			paths = append(paths, patch.abs)
		}
	}
	ctx, releasePaths, lockErr := acquireMutationPaths(ctx, paths)
	if lockErr != nil {
		return mcp.NewToolResultError("lock delete frontier: " + lockErr.Error()), nil
	}
	defer releasePaths()
	primaryAbs = absPath
	primaryKey := pathkey.MutationKey(absPath)
	var writes []committedSourceMutation
	finished := false
	finishWrites = func() {
		if finished {
			return
		}
		finished = true
		final, outcomes := s.completeCommittedSourceMutations(ctx, writes, releasePaths)
		for i, write := range final {
			if write.target.key == primaryKey {
				primaryCommit, primaryOutcome = write.record, outcomes[i]
			}
		}
	}
	defer finishWrites()
	currentNode := g.GetNode(id)
	if currentNode == nil || currentNode.FilePath != node.FilePath {
		return respond(map[string]any{"status": "source_authority_changed", "symbol": id, "written": false, "hint": "the selected declaration changed while acquiring the complete source frontier; request a new plan"})
	}
	currentGaps := s.laggingMutationReceipts(ctx)
	currentProvisional := s.provisionalSites(g, currentGaps, currentNode)
	currentRefs := collectReferencingEdges(g, id)
	for _, site := range currentProvisional {
		currentRefs = append(currentRefs, safeDeleteReference{Kind: "provisional", FilePath: site.File, Line: site.Line})
	}
	if !sameSafeDeleteReferences(refs, currentRefs) || !reflect.DeepEqual(provisional, currentProvisional) {
		return respond(map[string]any{"status": "source_authority_changed", "symbol": id, "written": false, "hint": "references or unpublished source changed while acquiring the complete source frontier; request a new plan"})
	}
	if cascadeMode != cascadeModeOff {
		currentClosure, truncated := computeCascadeClosure(g, currentNode, cascadeIntoTests)
		if truncated != cascadeTruncated || !reflect.DeepEqual(closure, currentClosure) {
			return respond(map[string]any{"status": "source_authority_changed", "symbol": id, "written": false, "hint": "cascade membership changed while acquiring the complete source frontier; request a new plan"})
		}
		for _, member := range pending[1:] {
			if len(s.provisionalSites(g, currentGaps, member.node)) > 0 {
				return respond(map[string]any{"status": "rejected_has_references", "symbol": id, "written": false, "hint": "a cascade member gained an unpublished reference; publish it and request a new plan"})
			}
		}
	}
	if len(propagationPlan) > 0 {
		// Original provisional rows stay manual. Compare exact graph-derived
		// occurrences and accepted source identities before the first write.
		currentPlan := s.buildPropagationPlan(ctx, g, currentNode)
		var originalGraphPlan []callerPatch
		for _, patch := range propagationPlan {
			if patch.kind != "" {
				originalGraphPlan = append(originalGraphPlan, patch)
			}
		}
		if !reflect.DeepEqual(originalGraphPlan, currentPlan) {
			return respond(map[string]any{"status": "source_authority_changed", "symbol": id, "written": false, "hint": "caller occurrences or accepted bytes changed while acquiring the complete source frontier; request a new plan"})
		}
		var callerWrites []committedSourceMutation
		var propagationErr error
		appliedPatches, failedPatches, callerWrites, propagationErr = s.applyRemoveLinePatchesHeld(ctx, propagationPlan)
		writes = append(writes, callerWrites...)
		if propagationErr != nil || len(failedPatches) > 0 && !force {
			partial := map[string]any{"status": "propagation_partial", "symbol": id, "patches_applied": appliedPatches, "patches_failed": failedPatches, "hint": "some source patches committed but the complete removal did not finish; inspect these exact effects before retrying"}
			if propagationErr != nil {
				partial["error"] = propagationErr.Error()
			}
			return respond(partial)
		}
		refs = referencesAfterCommittedPatches(refs, appliedPatches, id)
		if len(refs) > 0 && !force {
			return respond(map[string]any{"status": "rejected_has_references", "symbol": id, "references": refs, "reference_count": len(refs), "patches_applied": appliedPatches, "patches_failed": failedPatches, "hint": "unplanned or unverified references remain; patch them or explicitly request force"})
		}
		result["references"], result["reference_count"] = refs, len(refs)
	}
	deletedIDs, definitionWrites, err := s.applyPendingDeletesHeld(ctx, pending)
	writes = append(writes, definitionWrites...)
	if err != nil {
		if len(deletedIDs) == 0 && len(appliedPatches) == 0 {
			return mcp.NewToolResultError(err.Error()), nil
		}
		// Surface only source effects whose physical commit succeeded.
		result["status"] = "partial_failure"
		result["cascade_deleted"] = deletedIDs
		result["error"] = err.Error()
		result["patches_applied"], result["patches_failed"] = appliedPatches, failedPatches
		return respond(result)
	}

	result["start_line"], result["end_line"] = pending[0].start, pending[0].end
	result["lines_deleted"] = pending[0].end - pending[0].start + 1
	result["preview"] = pending[0].preview

	// Persist session state and re-index every touched file.
	sess := s.sessionFor(ctx)
	for _, p := range pending {
		sess.recordModified(p.node.FilePath)
		sess.recordSymbol(p.node.ID)
		if s.symHistory != nil {
			s.symHistory.Record(p.node.ID, true)
		}
	}

	result["status"] = "deleted"
	if cascadeMode == cascadeModeApply {
		result["cascade_deleted"] = deletedIDs
	}
	if len(appliedPatches) > 0 {
		result["patches_applied"] = appliedPatches
	}
	if len(failedPatches) > 0 {
		result["patches_failed"] = failedPatches
	}
	return respond(result)
}

// normaliseCascadeMode coerces the cascade parameter to a known
// value, defaulting to "off" so unknown / empty input preserves the
// legacy behaviour.
func normaliseCascadeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case cascadeModePreview:
		return cascadeModePreview
	case cascadeModeApply:
		return cascadeModeApply
	default:
		return cascadeModeOff
	}
}

// pendingDelete buffers everything needed to delete one symbol from
// its file. Every span is resolved against the bytes read under the held
// path set; the response retains that actual removed snapshot.
type pendingDelete struct {
	node    *graph.Node
	abs     string
	start   int
	end     int
	preview string
}

// applyPendingDeletes groups pending deletes by absolute file path
// and applies them in descending line order so earlier deletes do
// not shift the line numbers of later ones. Each file is read once,
// rewritten once. Returns the IDs of symbols whose bytes were
// removed; on first error, the partial list rides alongside the
// error.
func (s *Server) applyPendingDeletesHeld(ctx context.Context, pending []*pendingDelete) ([]string, []committedSourceMutation, error) {
	byFile := map[string][]*pendingDelete{}
	order := []string{}
	for _, p := range pending {
		if _, ok := byFile[p.abs]; !ok {
			order = append(order, p.abs)
		}
		byFile[p.abs] = append(byFile[p.abs], p)
	}

	deleted := make([]string, 0, len(pending))
	var writes []committedSourceMutation
	for _, abs := range order {
		bucket := byFile[abs]
		content, err := os.ReadFile(abs)
		if err != nil {
			return deleted, writes, fmt.Errorf("could not read %s: %v", abs, err)
		}
		// Refuse UTF-16 sources before splicing raw bytes (#846) — this is
		// the shared delete writer, so cascade targets are covered too.
		if indexer.LooksUTF16Source(content) {
			return deleted, writes, refuseUTF16Edit("safe_delete_symbol", abs)
		}
		lines := strings.Split(string(content), "\n")
		for _, p := range bucket {
			start, end, err := s.symbolLinesUnderLock(ctx, p.node, abs, content)
			if err != nil {
				return deleted, writes, err
			}
			resolved := *p.node
			resolved.StartLine, resolved.EndLine = start, end
			p.node = &resolved
			p.start, p.end = expandDeleteRange(p.node, lines)
			p.preview = strings.Join(lines[p.start-1:p.end], "\n")
		}
		// Sort descending so deletions earlier in the file don't
		// shift the line indexes of later ones.
		sort.Slice(bucket, func(i, j int) bool {
			return bucket[i].start > bucket[j].start
		})
		// Detect overlap — two symbols whose expanded ranges
		// intersect cannot both be deleted as separate slices
		// without losing bytes. The graph shouldn't produce that
		// for distinct symbols, but a defensive check keeps the
		// failure surface clean.
		for i := 0; i < len(bucket)-1; i++ {
			if bucket[i].start <= bucket[i+1].end {
				return deleted, writes, fmt.Errorf("overlapping delete ranges in %s for %s and %s",
					abs, bucket[i].node.ID, bucket[i+1].node.ID)
			}
		}
		for _, p := range bucket {
			lines = append(lines[:p.start-1], lines[p.end:]...)
		}
		newContent := strings.Join(lines, "\n")
		// The UTF-16 refusal above is the early, per-file UX; the commit
		// funnels through commitFileMutation so safe_delete shares the
		// cancellation gate, the atomic rename, and the mutation receipt
		// with every other mutating writer (#846: "every writer funnels
		// here" is meant literally).
		rel := abs
		if s.indexer != nil {
			if root := s.indexer.RootPath(); root != "" {
				if r, rerr := filepath.Rel(root, abs); rerr == nil {
					rel = r
				}
			}
		}
		// Keep the file's existing mode: the atomic commit applies perm to
		// the replacement, so a fixed 0o644 would strip a script's
		// executable bit.
		perm := os.FileMode(0o644)
		if info, err := os.Stat(abs); err == nil {
			perm = info.Mode().Perm()
		}
		commit, werr := s.commitFileMutation(ctx, "safe_delete_symbol", "", "", rel, abs, []byte(newContent), perm)
		if werr != nil {
			return deleted, writes, fmt.Errorf("could not write %s: %v", abs, werr)
		}
		writes = append(writes, captureCommittedSourceMutation(ctx, abs, commit))
		for _, p := range bucket {
			deleted = append(deleted, p.node.ID)
		}
	}
	return deleted, writes, nil
}

// expandDeleteRange grows a symbol's [StartLine, EndLine] range to
// also consume any leading doc-comment block and one trailing blank
// line, exactly the way the single-symbol path did before the
// cascade refactor.
func expandDeleteRange(node *graph.Node, lines []string) (int, int) {
	deleteStart := node.StartLine
	for deleteStart > 1 {
		trimmed := strings.TrimSpace(lines[deleteStart-2])
		if isCommentLine(trimmed) {
			deleteStart--
			continue
		}
		break
	}
	deleteEnd := node.EndLine
	if deleteEnd < len(lines) && strings.TrimSpace(lines[deleteEnd]) == "" {
		deleteEnd++
	}
	return deleteStart, deleteEnd
}

// computeCascadeClosure runs the fixed-point orphan-propagation
// algorithm. Starting from {target}, it repeatedly adds any symbol
// whose every referencing in-edge originates from the current
// closure (and who is in the same workspace as the target).
//
// External-reference rules a candidate must satisfy:
//   - it must not have a referencing in-edge from outside the
//     current closure (self-references inside D never disqualify);
//   - its in-edges must not include a cross-workspace caller
//     (different WorkspaceID — falling back to RepoPrefix when
//     unset);
//   - test-only callers disqualify by default; cascadeIntoTests
//     inverts that.
//
// The candidate itself must also be in the same workspace as the
// target. Iteration is bounded by cascadeIterationCap; if hit, the
// caller surfaces cascade_truncated so the agent knows the closure
// may be incomplete.
func computeCascadeClosure(g graph.Reader, target *graph.Node, cascadeIntoTests bool) ([]cascadeClosureEntry, bool) {
	closure := []cascadeClosureEntry{}
	inClosure := map[string]bool{target.ID: true}
	reasons := map[string]string{}

	targetWS := workspaceKey(target)

	truncated := false
	for iter := 0; iter < cascadeIterationCap; iter++ {
		// Candidate set: every node that an in-closure node points
		// at (the closure's downstream reachability via referencing
		// edges). We don't walk EdgeDefines or EdgeMemberOf — those
		// are structural and don't represent "use".
		candidates := collectCascadeCandidates(g, inClosure)
		added := 0
		for _, cid := range candidates {
			if inClosure[cid] {
				continue
			}
			cn := g.GetNode(cid)
			if cn == nil {
				continue
			}
			if cn.StartLine == 0 || cn.EndLine == 0 {
				// Synthetic / structural nodes have no on-disk
				// range; deleting them makes no sense.
				continue
			}
			if workspaceKey(cn) != targetWS {
				continue
			}
			reason, ok := candidateQualifies(g, cn, inClosure, cascadeIntoTests)
			if !ok {
				continue
			}
			inClosure[cid] = true
			reasons[cid] = reason
			closure = append(closure, cascadeClosureEntry{
				ID:     cn.ID,
				Name:   cn.Name,
				Kind:   string(cn.Kind),
				Path:   cn.FilePath,
				Line:   cn.StartLine,
				Reason: reason,
			})
			added++
		}
		if added == 0 {
			return closure, false
		}
		if iter == cascadeIterationCap-1 {
			truncated = true
		}
	}
	return closure, truncated
}

// collectCascadeCandidates returns every distinct node ID that an
// in-closure node points at via a referencing edge — the only
// possible new entrants to the closure on this iteration.
func collectCascadeCandidates(g graph.Reader, inClosure map[string]bool) []string {
	seen := map[string]bool{}
	out := []string{}
	for from := range inClosure {
		for _, e := range g.GetOutEdges(from) {
			if !isReferencingEdgeKind(e.Kind) {
				continue
			}
			if seen[e.To] || inClosure[e.To] {
				continue
			}
			seen[e.To] = true
			out = append(out, e.To)
		}
	}
	// Stable iteration order so the closure list is deterministic
	// for tests; map iteration above is not.
	sort.Strings(out)
	return out
}

// candidateQualifies inspects every referencing in-edge of cn and
// reports whether the node has no caller outside the current
// closure. Returns a human-readable reason string when the node
// qualifies (used for the response payload).
func candidateQualifies(g graph.Reader, cn *graph.Node, inClosure map[string]bool, cascadeIntoTests bool) (string, bool) {
	targetWS := ""
	// Build an "in-closure caller" list so the reason string can
	// name the symbol(s) that are the only ones still calling this
	// candidate.
	closureCallers := map[string]bool{}
	hasAnyIn := false
	for _, e := range g.GetInEdges(cn.ID) {
		if !isReferencingEdgeKind(e.Kind) {
			continue
		}
		hasAnyIn = true
		if inClosure[e.From] {
			closureCallers[e.From] = true
			continue
		}
		// External caller — examine it.
		from := g.GetNode(e.From)
		if from == nil {
			// Defensive: treat unknown caller as external.
			return "", false
		}
		// Establish target workspace lazily from one of the
		// in-closure callers' WorkspaceID so the comparison is
		// consistent across iterations.
		if targetWS == "" {
			targetWS = workspaceKey(cn)
		}
		if workspaceKey(from) != targetWS {
			// Cross-workspace caller disqualifies.
			return "", false
		}
		isTestCaller := indexer.IsTestFile(from.FilePath)
		if isTestCaller && !cascadeIntoTests {
			// Test-only caller and the agent did not opt in — the
			// candidate stays alive because production tests still
			// depend on it.
			return "", false
		}
		if !isTestCaller {
			// Same-workspace, non-test, out-of-closure caller — this
			// is a real production user; the candidate is not
			// orphaned.
			return "", false
		}
		// At this point: test caller + cascadeIntoTests=true. The
		// caller is acceptable; record it as if it were inside the
		// closure so the reason string can attribute the cascade.
		closureCallers[e.From] = true
	}

	if !hasAnyIn {
		// No referencing edges at all — already dead code, qualifies.
		return "no referencing edges; already orphaned", true
	}

	// All referencing in-edges came from the closure.
	if len(closureCallers) == 1 {
		var only string
		for k := range closureCallers {
			only = k
		}
		return "only referenced by " + only, true
	}
	callers := make([]string, 0, len(closureCallers))
	for k := range closureCallers {
		callers = append(callers, k)
	}
	sort.Strings(callers)
	return "only referenced by " + strings.Join(callers, ", "), true
}

// workspaceKey returns the node's workspace identity for the
// cross-workspace check. Prefers WorkspaceID; falls back to
// RepoPrefix when WorkspaceID is empty (matches the convention
// elsewhere in the graph package). An empty result is still
// comparable — two nodes with empty workspace keys are treated as
// belonging to the same notional workspace.
func workspaceKey(n *graph.Node) string {
	if n == nil {
		return ""
	}
	if n.WorkspaceID != "" {
		return n.WorkspaceID
	}
	return n.RepoPrefix
}

// collectReferencingEdges returns every in-edge to id whose kind
// represents real use (someone calls, implements, extends, or
// references this symbol). Structural edges (defines, member_of)
// are excluded because they don't block a delete.
func collectReferencingEdges(g graph.Reader, id string) []safeDeleteReference {
	out := make([]safeDeleteReference, 0)
	seen := map[string]bool{}
	for _, e := range g.GetInEdges(id) {
		if !isReferencingEdgeKind(e.Kind) {
			continue
		}
		key := fmt.Sprintf("%s|%s|%s|%d", e.From, e.Kind, e.FilePath, e.Line)
		if seen[key] {
			continue
		}
		seen[key] = true
		row := safeDeleteReference{FromID: e.From, Kind: string(e.Kind), Line: e.Line, FilePath: e.FilePath}
		if from := g.GetNode(e.From); from != nil {
			row.FromName = from.Name
			if row.FilePath == "" {
				row.FilePath = from.FilePath
			}
		}
		out = append(out, row)
	}
	return out
}

// isReferencingEdgeKind reports whether an in-edge of this kind
// counts as "real use" that should block a delete.
func isReferencingEdgeKind(k graph.EdgeKind) bool {
	switch k {
	case graph.EdgeCalls,
		graph.EdgeImplements,
		graph.EdgeExtends,
		graph.EdgeReferences,
		graph.EdgeInstantiates,
		graph.EdgeCrossRepoCalls,
		graph.EdgeCrossRepoImplements,
		graph.EdgeCrossRepoExtends:
		return true
	}
	return false
}

// isCommentLine recognises every block- and line-comment leader the
// extractors emit. Used by the doc-comment expansion above.
func isCommentLine(trimmed string) bool {
	switch {
	case strings.HasPrefix(trimmed, "//"),
		strings.HasPrefix(trimmed, "/*"),
		strings.HasPrefix(trimmed, "*"),
		strings.HasPrefix(trimmed, "#"),
		strings.HasPrefix(trimmed, "///"),
		strings.HasPrefix(trimmed, "--"):
		return true
	}
	return false
}

func sameSafeDeleteReferences(a, b []safeDeleteReference) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[safeDeleteReference]int, len(a))
	for _, ref := range a {
		counts[ref]++
	}
	for _, ref := range b {
		if counts[ref] == 0 {
			return false
		}
		counts[ref]--
	}
	return true
}

func referencesAfterCommittedPatches(refs []safeDeleteReference, applied []callerPatch, target string) []safeDeleteReference {
	var remaining []safeDeleteReference
	for _, ref := range refs {
		removed := false
		for _, patch := range applied {
			if patch.sourceHash != ([32]byte{}) && patch.target == target && patch.from == ref.FromID && string(patch.kind) == ref.Kind && patch.File == ref.FilePath && patch.Line == ref.Line && patch.Line > 0 {
				removed = true
				break
			}
		}
		if !removed {
			remaining = append(remaining, ref)
		}
	}
	return remaining
}
