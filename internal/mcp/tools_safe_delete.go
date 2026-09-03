package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
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
	FromID   string `json:"from_id"`
	Kind     string `json:"kind"`
	FromName string `json:"from_name,omitempty"`
	FilePath string `json:"file_path,omitempty"`
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

	// SRN-7: propagate-delete — patch the surviving call sites instead of
	// refusing. Builds a per-caller plan, applies the removable standalone
	// calls (parse-gate validated), and flags embedded references as manual.
	var appliedPatches, failedPatches []callerPatch
	if req.GetBool("propagate", false) && len(refs) > 0 {
		plan := s.buildPropagationPlan(ctx, g, node)
		manual := countManual(plan)
		if dryRun {
			return s.respondJSONOrTOON(ctx, req, map[string]any{
				"status":          "propagation_plan",
				"symbol":          id,
				"file":            node.FilePath,
				"caller_patches":  plan,
				"removable":       len(plan) - manual,
				"manual":          manual,
				"reference_count": len(refs),
				"dry_run":         true,
				"hint":            "re-run with dry_run=false to apply the remove_line patches and delete the symbol; resolve manual patches first or pass force=true",
			})
		}
		if manual > 0 && !force {
			return s.respondJSONOrTOON(ctx, req, map[string]any{
				"status":         "propagation_blocked_manual",
				"symbol":         id,
				"caller_patches": plan,
				"manual":         manual,
				"hint":           fmt.Sprintf("%d call site(s) need manual patching; resolve them or pass force=true to delete anyway", manual),
			})
		}
		applied, failed, perr := s.applyRemoveLinePatches(plan)
		if perr != nil {
			return mcp.NewToolResultError(perr.Error()), nil
		}
		appliedPatches, failedPatches = applied, failed
		if len(failed) > 0 && !force {
			return s.respondJSONOrTOON(ctx, req, map[string]any{
				"status":          "propagation_partial",
				"symbol":          id,
				"patches_applied": applied,
				"patches_failed":  failed,
				"hint":            "some removals would break their file's syntax and were skipped; patch them by hand or pass force=true to delete the symbol anyway",
			})
		}
		// Callers patched — proceed with the deletion. Re-fetch the node in
		// case a same-file removal shifted its line range.
		force = true
		node = g.GetNode(id)
		if node == nil {
			return s.respondJSONOrTOON(ctx, req, map[string]any{
				"status":          "deleted_by_propagation",
				"symbol":          id,
				"patches_applied": appliedPatches,
				"note":            "the symbol's definition was removed as a side effect of patching (it no longer resolves in the graph)",
			})
		}
		if node.StartLine == 0 || node.EndLine == 0 {
			return mcp.NewToolResultError("symbol has no line range after propagation: " + id), nil
		}
	}

	if len(refs) > 0 && !force {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
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

	absPath, err := s.resolveNodePath(ctx, node)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read file: %v", err)), nil
	}
	lines := strings.Split(string(content), "\n")
	if node.StartLine > len(lines) || node.EndLine > len(lines) {
		return mcp.NewToolResultError("symbol line range exceeds file length"), nil
	}

	deleteStart, deleteEnd := expandDeleteRange(node, lines)

	deletedChunk := strings.Join(lines[deleteStart-1:deleteEnd], "\n")
	linesDeleted := deleteEnd - deleteStart + 1

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
		return s.respondJSONOrTOON(ctx, req, result)
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

	deletedIDs, err := applyPendingDeletes(pending)
	if err != nil {
		// Fail-fast: surface what was done up to this point so the
		// caller can recover. Treat the failure as a tool error.
		result["status"] = "partial_failure"
		result["cascade_deleted"] = deletedIDs
		result["error"] = err.Error()
		return s.respondJSONOrTOON(ctx, req, result)
	}

	// Persist session state and re-index every touched file.
	sess := s.sessionFor(ctx)
	touchedFiles := make(map[string]struct{}, len(pending))
	for _, p := range pending {
		sess.recordModified(p.node.FilePath)
		sess.recordSymbol(p.node.ID)
		if s.symHistory != nil {
			s.symHistory.Record(p.node.ID, true)
		}
		touchedFiles[p.abs] = struct{}{}
	}
	for abs := range touchedFiles {
		s.reindexFile(abs)
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
	return s.respondJSONOrTOON(ctx, req, result)
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
// its file. start/end of zero mean "compute lazily from the node and
// the file contents" — only the original target arrives pre-computed
// because the dry_run preview already needed those numbers.
type pendingDelete struct {
	node  *graph.Node
	abs   string
	start int
	end   int
}

// applyPendingDeletes groups pending deletes by absolute file path
// and applies them in descending line order so earlier deletes do
// not shift the line numbers of later ones. Each file is read once,
// rewritten once. Returns the IDs of symbols whose bytes were
// removed; on first error, the partial list rides alongside the
// error.
func applyPendingDeletes(pending []*pendingDelete) ([]string, error) {
	byFile := map[string][]*pendingDelete{}
	order := []string{}
	for _, p := range pending {
		if _, ok := byFile[p.abs]; !ok {
			order = append(order, p.abs)
		}
		byFile[p.abs] = append(byFile[p.abs], p)
	}

	deleted := make([]string, 0, len(pending))
	for _, abs := range order {
		bucket := byFile[abs]
		content, err := os.ReadFile(abs)
		if err != nil {
			return deleted, fmt.Errorf("could not read %s: %v", abs, err)
		}
		lines := strings.Split(string(content), "\n")
		// Materialise ranges for entries that arrived with zero
		// start/end (the cascade tail; the original target arrives
		// pre-computed for parity with the dry_run preview).
		for _, p := range bucket {
			if p.start == 0 || p.end == 0 {
				if p.node.StartLine > len(lines) || p.node.EndLine > len(lines) {
					return deleted, fmt.Errorf("symbol %s line range exceeds file %s", p.node.ID, abs)
				}
				p.start, p.end = expandDeleteRange(p.node, lines)
			}
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
				return deleted, fmt.Errorf("overlapping delete ranges in %s for %s and %s",
					abs, bucket[i].node.ID, bucket[i+1].node.ID)
			}
		}
		for _, p := range bucket {
			lines = append(lines[:p.start-1], lines[p.end:]...)
			deleted = append(deleted, p.node.ID)
		}
		newContent := strings.Join(lines, "\n")
		if err := os.WriteFile(abs, []byte(newContent), 0o644); err != nil {
			return deleted, fmt.Errorf("could not write %s: %v", abs, err)
		}
	}
	return deleted, nil
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
		key := e.From + "|" + string(e.Kind)
		if seen[key] {
			continue
		}
		seen[key] = true
		row := safeDeleteReference{FromID: e.From, Kind: string(e.Kind)}
		if from := g.GetNode(e.From); from != nil {
			row.FromName = from.Name
			row.FilePath = from.FilePath
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
