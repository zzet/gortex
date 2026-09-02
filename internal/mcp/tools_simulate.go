package mcp

// preview_edit and simulate_chain (H1 — speculative execution /
// simulation sessions). These tools sit on top of the per-session
// overlay substrate (overlay.go, overlay_view.go, tools_overlay*.go)
// and answer "what would change if I applied this WorkspaceEdit?" —
// per-symbol caller / implementor / dependent deltas, graph blast
// radius, and LSP diagnostics — without ever mutating disk or the
// base graph.
//
// preview_edit is the single-shot form: one WorkspaceEdit in, one
// before/after report out. The simulation runs against an ephemeral
// overlay layer constructed for this single tool call and never
// persists past the response.
//
// simulate_chain is the iterative form: a sequence of WorkspaceEdits
// applied in order, with per-step impact, cumulative impact, and an
// optional `keep: true` that promotes the simulation into a real
// overlay session bound to the caller's MCP session — so the user
// can preview, then commit (write_workspace_edit / overlay flush)
// or discard (overlay_drop). Chains can also inherit from the
// caller's already-attached overlay (`inherit_overlay: true`) so a
// user can keep iterating on top of in-flight buffer state.
//
// Both tools accept a standard LSP WorkspaceEdit shape (changes /
// documentChanges with TextEdits) so any frontier-model agent that
// already produces WorkspaceEdits for code actions can speculate on
// them without a bespoke format. New files are modeled as a
// WorkspaceEdit whose `changes[uri]` opens with a full-file insert
// against an absent target.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/lspuri"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// registerSimulationTools wires preview_edit and simulate_chain. Both
// are registered without the overlay-injecting middleware (they
// manage their own simulation views) but they DO honour an existing
// session overlay when `inherit_overlay: true` is set on the chain
// form. We register through addControlTool: it keeps safety, gating, and
// telemetry middleware but skips automatic overlay injection because these
// tools compose their own simulation views explicitly.
func (s *Server) registerSimulationTools() {
	s.addControlTool(
		mcp.NewTool("preview_edit",
			mcp.WithDescription("Speculatively apply one WorkspaceEdit (LSP-shaped: `changes` or `documentChanges`) to a fresh shadow view on top of the base graph, then return the impact — touched files, before/after symbol surfaces, broken callers, broken interface implementors, suggested test targets, and (when the LSP server for the language is running) the diagnostics that the change would produce. Disk is never written; the base graph is never mutated. The shadow view is built per call and discarded with the response. Useful when you want to know if a refactor is safe before staging it."),
			mcp.WithString("workspace_edit", mcp.Required(), mcp.Description("LSP WorkspaceEdit as a JSON string. Either `{\"changes\": {uri: [TextEdit, ...], ...}}` or `{\"documentChanges\": [{\"textDocument\": {\"uri\": \"...\"}, \"edits\": [TextEdit, ...]}, ...]}`. TextEdit is `{\"range\": {\"start\": {\"line\": N, \"character\": M}, \"end\": {...}}, \"newText\": \"...\"}` with 0-indexed line/char. URIs can be `file://`, absolute paths, or repo-relative paths.")),
			mcp.WithBoolean("inherit_overlay", mcp.Description("When true, apply the edit on top of the caller's current overlay (if any) instead of pristine base. Default false. Use this to preview an edit on top of unsaved editor buffers.")),
			mcp.WithBoolean("diagnostics", mcp.Description("Run LSP diagnostics on every touched file in the simulated view and include them in the response. Default true. Set to false to skip the LSP round-trip when you only care about the graph delta.")),
			mcp.WithNumber("diagnostics_timeout_ms", mcp.Description("How long to wait for the LSP server to publish diagnostics after the simulated didChange (default 1500ms). Tighten when latency matters; loosen for cold language servers.")),
		),
		s.handlePreviewEdit,
	)
	s.addControlTool(
		mcp.NewTool("simulate_chain",
			mcp.WithDescription("Apply an ordered sequence of WorkspaceEdits to a fresh shadow view, accumulating overlay state between steps. Returns per-step impact (touched files, broken callers, broken implementors, diagnostics delta vs the previous step) and a cumulative impact rollup at the end. Disk is never written. Useful for previewing multi-step refactors where each step depends on the previous step's result (rename → fix callers → adjust signature, etc.). Pass `keep: true` to persist the final state as an editor overlay session — the response then includes `overlay_session_id` and the caller can `overlay_push` / `overlay_drop` / `compare_with_overlay` from there, or eventually write the changes to disk."),
			mcp.WithString("steps", mcp.Required(), mcp.Description("JSON array of LSP WorkspaceEdit objects, applied in order. Each step's edits are layered on top of the prior step's simulated state. An empty array is rejected; pass a single-element array for trivial chains.")),
			mcp.WithBoolean("inherit_overlay", mcp.Description("When true, start the simulation from the caller's current overlay (if any) instead of pristine base. Default false.")),
			mcp.WithBoolean("keep", mcp.Description("When true, the final simulated overlay is committed into a real overlay session bound to the calling MCP session and the response carries `overlay_session_id`. Default false — the simulation is fully discarded with the response. Mutually compatible with `inherit_overlay`: with both set, the final overlay replaces the inherited one. Has no effect when the calling context has no MCP session.")),
			mcp.WithBoolean("diagnostics", mcp.Description("Run LSP diagnostics on every touched file at each step and include the delta in the per-step output. Default true.")),
			mcp.WithNumber("diagnostics_timeout_ms", mcp.Description("Per-step LSP diagnostics wait (default 1500ms).")),
			mcp.WithBoolean("stop_on_error", mcp.Description("When true (default), the chain aborts as soon as a step introduces a new ERROR-severity diagnostic that wasn't there before. The response carries everything up to and including the failing step plus a `stopped_at` index. Set false to evaluate the chain in full regardless of intermediate errors.")),
		),
		s.handleSimulateChain,
	)
}

// ---------------------------------------------------------------------------
// preview_edit
// ---------------------------------------------------------------------------

func (s *Server) handlePreviewEdit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s == nil || s.graph == nil {
		return mcp.NewToolResultError("simulation engine: server not fully initialised"), nil
	}
	raw, err := req.RequireString("workspace_edit")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	edit, parseErr := parseWorkspaceEdit(raw)
	if parseErr != nil {
		return mcp.NewToolResultError("invalid workspace_edit: " + parseErr.Error()), nil
	}
	if isEmptyEdit(edit) {
		return mcp.NewToolResultError("workspace_edit contains no document changes"), nil
	}

	inherit := req.GetBool("inherit_overlay", false)
	wantDiag := req.GetBool("diagnostics", true)
	diagTimeout := time.Duration(req.GetInt("diagnostics_timeout_ms", 1500)) * time.Millisecond

	sim, simErr := s.buildSimulation(ctx, []lsp.WorkspaceEdit{edit}, inherit)
	if simErr != nil {
		if ctxErr := requestContextError(ctx, simErr); ctxErr != nil {
			return nil, ctxErr
		}
		return mcp.NewToolResultError(simErr.Error()), nil
	}
	step := sim.steps[0]

	result := map[string]any{
		"touched_files":       step.touchedFiles,
		"new_files":           step.newFiles,
		"deleted_files":       step.deletedFiles,
		"missing_files":       step.missingFiles,
		"symbols_added":       step.symbolsAdded,
		"symbols_removed":     step.symbolsRemoved,
		"symbols_renamed":     step.symbolsRenamed,
		"broken_callers":      step.brokenCallers,
		"broken_implementors": step.brokenImplementors,
		"impact":              step.impact,
		"test_targets":        step.testTargets,
		"overlay_paths":       sim.coveredPaths,
		"summary":             step.summary,
	}

	if wantDiag {
		diags := s.simulateDiagnostics(ctx, sim, step.touchedFiles, diagTimeout)
		result["diagnostics"] = diags
		result["diagnostics_summary"] = summariseDiagnostics(diags)
	}

	return mcp.NewToolResultText(jsonOK(result)), nil
}

// ---------------------------------------------------------------------------
// simulate_chain
// ---------------------------------------------------------------------------

func (s *Server) handleSimulateChain(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s == nil || s.graph == nil {
		return mcp.NewToolResultError("simulation engine: server not fully initialised"), nil
	}
	rawSteps, err := req.RequireString("steps")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(rawSteps) > overlaySimulationInputMaxBytes {
		return mcp.NewToolResultError(fmt.Sprintf("steps input exceeds limit %d bytes", overlaySimulationInputMaxBytes)), nil
	}
	var rawEdits []json.RawMessage
	if err := json.Unmarshal([]byte(rawSteps), &rawEdits); err != nil {
		return mcp.NewToolResultError("steps must be a JSON array of WorkspaceEdit objects: " + err.Error()), nil
	}
	if len(rawEdits) == 0 {
		return mcp.NewToolResultError("steps array is empty — pass at least one WorkspaceEdit"), nil
	}
	if len(rawEdits) > overlaySimulationMaxSteps {
		return mcp.NewToolResultError(fmt.Sprintf("steps exceed limit %d", overlaySimulationMaxSteps)), nil
	}
	edits := make([]lsp.WorkspaceEdit, 0, len(rawEdits))
	for i, raw := range rawEdits {
		edit, parseErr := parseWorkspaceEdit(string(raw))
		if parseErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("step %d: invalid workspace_edit: %v", i, parseErr)), nil
		}
		if isEmptyEdit(edit) {
			return mcp.NewToolResultError(fmt.Sprintf("step %d: workspace_edit contains no document changes", i)), nil
		}
		edits = append(edits, edit)
	}

	inherit := req.GetBool("inherit_overlay", false)
	keep := req.GetBool("keep", false)
	wantDiag := req.GetBool("diagnostics", true)
	stopOnError := req.GetBool("stop_on_error", true)
	diagTimeout := time.Duration(req.GetInt("diagnostics_timeout_ms", 1500)) * time.Millisecond

	sim, simErr := s.buildSimulation(ctx, edits, inherit)
	if simErr != nil {
		if ctxErr := requestContextError(ctx, simErr); ctxErr != nil {
			return nil, ctxErr
		}
		return mcp.NewToolResultError(simErr.Error()), nil
	}

	stepsOut := make([]map[string]any, 0, len(sim.steps))
	var prevDiagBySeverity = map[string]int{}
	stoppedAt := -1
	for i, step := range sim.steps {
		stepView := map[string]any{
			"index":               i,
			"touched_files":       step.touchedFiles,
			"new_files":           step.newFiles,
			"deleted_files":       step.deletedFiles,
			"missing_files":       step.missingFiles,
			"symbols_added":       step.symbolsAdded,
			"symbols_removed":     step.symbolsRemoved,
			"symbols_renamed":     step.symbolsRenamed,
			"broken_callers":      step.brokenCallers,
			"broken_implementors": step.brokenImplementors,
			"impact":              step.impact,
			"test_targets":        step.testTargets,
			"summary":             step.summary,
		}
		if wantDiag {
			diags := s.simulateDiagnosticsAtStep(ctx, sim, i, step.touchedFiles, diagTimeout)
			stepView["diagnostics"] = diags
			summ := summariseDiagnostics(diags)
			stepView["diagnostics_summary"] = summ
			if prevDiagBySeverity != nil {
				delta := map[string]int{}
				for sev, n := range summ {
					delta[sev] = n - prevDiagBySeverity[sev]
				}
				for sev, n := range prevDiagBySeverity {
					if _, ok := summ[sev]; !ok {
						delta[sev] = -n
					}
				}
				stepView["diagnostics_delta"] = delta
				if stopOnError && delta["error"] > 0 {
					stoppedAt = i
					stepsOut = append(stepsOut, stepView)
					break
				}
			}
			prevDiagBySeverity = summ
		}
		stepsOut = append(stepsOut, stepView)
	}

	result := map[string]any{
		"steps":           stepsOut,
		"total_steps":     len(sim.steps),
		"applied_steps":   len(stepsOut),
		"stopped_at":      stoppedAt,
		"overlay_paths":   sim.coveredPaths,
		"cumulative":      sim.cumulative,
		"inherit_overlay": inherit,
		"kept":            false,
	}

	if keep {
		sessID, keepErr := s.persistSimulationOverlay(ctx, sim)
		if keepErr != nil {
			result["keep_error"] = keepErr.Error()
		} else if sessID != "" {
			result["kept"] = true
			result["overlay_session_id"] = sessID
		}
	}

	return mcp.NewToolResultText(jsonOK(result)), nil
}

// ---------------------------------------------------------------------------
// simulation engine
// ---------------------------------------------------------------------------

// simulation is one speculative WorkspaceEdit sequence. It holds the
// post-step overlay snapshots (one per applied step) along with the
// per-step and cumulative impact rollups. Snapshots are
// []daemon.OverlayFile rather than parsed graph layers; the layer is
// rebuilt from snapshots on demand (so the impact pass and the
// optional `keep` promotion both read from the same source).
type simulation struct {
	// initial is the overlay state we start from — empty when not
	// inheriting, or a copy of the caller's current overlay files
	// when inherit_overlay=true. Snapshots in `snapshots` are
	// computed by applying each step on top of this base.
	initial []daemon.OverlayFile
	// steps holds per-step impact summaries, parallel to snapshots
	// (snapshots[i] is the overlay state AFTER step i applies).
	steps []simulationStep
	// snapshots is the post-step overlay state for each applied
	// step. snapshots[len-1] is the final cumulative state.
	snapshots [][]daemon.OverlayFile
	// coveredPaths is the union of every overlay path touched by
	// the simulation, sorted. Used by both the per-call response
	// and by `keep` to know which overlays to push.
	coveredPaths []string
	// cumulative is the post-final-step rollup of touched / added /
	// removed symbols across the whole chain.
	cumulative map[string]any
}

type simulationStep struct {
	touchedFiles       []string
	newFiles           []string
	deletedFiles       []string
	missingFiles       []string
	symbolsAdded       []string
	symbolsRemoved     []string
	symbolsRenamed     []map[string]string
	brokenCallers      []map[string]any
	brokenImplementors []map[string]any
	impact             map[string]any
	testTargets        []string
	summary            string
}

// buildSimulation runs the full chain of edits and returns the
// resulting simulation. It does NOT take any locks on the
// OverlayManager — each step computes (or reuses) overlay file
// contents from prior steps' snapshots and never persists them
// unless the caller asks for `keep`.
func (s *Server) buildSimulation(ctx context.Context, edits []lsp.WorkspaceEdit, inherit bool) (*simulation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(edits) > overlaySimulationMaxSteps {
		return nil, fmt.Errorf("simulation steps exceed limit %d", overlaySimulationMaxSteps)
	}
	sim := &simulation{}
	current := map[string]daemon.OverlayFile{}

	if inherit {
		if sessID := OverlayCohortIDFromContext(ctx); sessID != "" && s.overlays != nil && s.overlays.Has(sessID) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			_, files, err := s.overlays.SnapshotForBounded(
				sessID,
				overlayRequestSnapshotMaxFiles,
				overlayRequestSnapshotMaxBytes,
			)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if err != nil {
				if !errors.Is(err, daemon.ErrSessionNotFound) {
					return nil, fmt.Errorf("inherit overlay snapshot: %w", err)
				}
			} else {
				for _, f := range files {
					current[filepath.Clean(f.Path)] = f
				}
				sim.initial = append(sim.initial, files...)
			}
		}
	}

	coveredSet := map[string]struct{}{}
	cumulativeAdded := map[string]struct{}{}
	cumulativeRemoved := map[string]struct{}{}
	cumulativeRenamed := map[string]string{} // oldID -> newID

	for stepIdx, edit := range edits {
		// 1. Resolve the WorkspaceEdit into per-file (absPath,
		//    overlayPath, newContent) tuples. Each tuple replaces
		//    `current[path]` for downstream steps.
		fileEdits, err := s.groupEditByFile(edit)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", stepIdx, err)
		}

		step := simulationStep{}
		for _, fe := range fileEdits {
			// Current pre-edit content: prior step's overlay if
			// present, otherwise the on-disk file. Missing files
			// are treated as empty (the edit's new content seeds
			// a fresh file).
			pre, hadPre := contentForSim(current, fe.absPath, fe.overlayPath)

			if fe.deleted {
				current[filepath.Clean(fe.overlayPath)] = daemon.OverlayFile{
					Path:    fe.overlayPath,
					Deleted: true,
				}
				step.touchedFiles = append(step.touchedFiles, fe.overlayPath)
				step.deletedFiles = append(step.deletedFiles, fe.overlayPath)
				coveredSet[fe.overlayPath] = struct{}{}
				continue
			}

			if !hadPre && len(fe.edits) > 0 {
				// New file from absent target: walk the edits in
				// document order and concatenate the newText
				// fragments. Range data is ignored for new-file
				// inserts (no offsets to map).
				var b strings.Builder
				for _, te := range fe.edits {
					b.WriteString(te.NewText)
				}
				current[filepath.Clean(fe.overlayPath)] = daemon.OverlayFile{
					Path:    fe.overlayPath,
					Content: b.String(),
				}
				step.touchedFiles = append(step.touchedFiles, fe.overlayPath)
				step.newFiles = append(step.newFiles, fe.overlayPath)
				coveredSet[fe.overlayPath] = struct{}{}
				continue
			}

			// Apply LSP TextEdits to the pre-content. We reuse
			// the same offset arithmetic the production
			// apply_code_action path uses so simulation matches
			// commit semantics exactly.
			next, applyErr := applySimulationEdits([]byte(pre), fe.edits)
			if applyErr != nil {
				return nil, fmt.Errorf("step %d: apply edits to %s: %w", stepIdx, fe.overlayPath, applyErr)
			}
			current[filepath.Clean(fe.overlayPath)] = daemon.OverlayFile{
				Path:    fe.overlayPath,
				Content: string(next),
			}
			step.touchedFiles = append(step.touchedFiles, fe.overlayPath)
			if !hadPre {
				step.missingFiles = append(step.missingFiles, fe.overlayPath)
			}
			coveredSet[fe.overlayPath] = struct{}{}
		}

		sort.Strings(step.touchedFiles)
		sort.Strings(step.newFiles)
		sort.Strings(step.deletedFiles)
		sort.Strings(step.missingFiles)

		// 2. Snapshot the overlay state at this step. Reject an oversized map
		// before allocating or retaining its sorted slice representation.
		if err := validateOverlayBuildMapEnvelope(current); err != nil {
			return nil, fmt.Errorf("step %d: overlay snapshot: %w", stepIdx, err)
		}
		snap := make([]daemon.OverlayFile, 0, len(current))
		for _, f := range current {
			snap = append(snap, f)
		}
		sort.Slice(snap, func(i, j int) bool { return snap[i].Path < snap[j].Path })
		sim.snapshots = append(sim.snapshots, snap)

		// 3. Compute graph impact for this step: build the layer,
		//    diff vs base, surface broken callers / implementors,
		//    rank test targets.
		layer, _, layerErr := s.constructOverlayLayer(ctx, snap)
		if layerErr != nil {
			return nil, fmt.Errorf("step %d: overlay parse: %w", stepIdx, layerErr)
		}
		view := graph.NewOverlaidView(s.graph, layer)
		s.fillStepImpact(&step, layer, view, step.touchedFiles)

		for _, id := range step.symbolsAdded {
			cumulativeAdded[id] = struct{}{}
		}
		for _, id := range step.symbolsRemoved {
			cumulativeRemoved[id] = struct{}{}
		}
		for _, ren := range step.symbolsRenamed {
			if from, to := ren["from"], ren["to"]; from != "" {
				cumulativeRenamed[from] = to
			}
		}

		sim.steps = append(sim.steps, step)
	}

	sim.coveredPaths = make([]string, 0, len(coveredSet))
	for p := range coveredSet {
		sim.coveredPaths = append(sim.coveredPaths, p)
	}
	sort.Strings(sim.coveredPaths)

	sim.cumulative = map[string]any{
		"symbols_added":   sortedSetKeys(cumulativeAdded),
		"symbols_removed": sortedSetKeys(cumulativeRemoved),
		"symbols_renamed": renameMapToSlice(cumulativeRenamed),
		"files_touched":   sim.coveredPaths,
	}

	return sim, nil
}

// fillStepImpact computes the per-step impact summary against the
// overlay layer built for that step. The graph diff is between base
// (for the touched file paths) and the overlay layer's view of those
// paths — added / removed names, plus heuristic rename detection
// (same kind + similar signature). Broken callers / implementors are
// detected by replaying the relevant query against the overlay view
// and comparing to base; any caller that exists in base but whose
// target symbol is gone from overlay (or whose signature changed
// incompatibly) is flagged.
func (s *Server) fillStepImpact(step *simulationStep, layer *graph.OverlayLayer, view *graph.OverlaidView, _ []string) {
	if layer == nil || view == nil {
		step.summary = "simulation: no overlay layer constructed (no covered paths)"
		return
	}
	baseEng := s.engine
	overlayEng := s.engine.WithReader(view)

	addedSet := map[string]struct{}{}
	removedSet := map[string]struct{}{}
	renameByOldID := map[string]string{}
	brokenCallers := map[string]map[string]any{}
	brokenImpls := map[string]map[string]any{}
	testTargetSet := map[string]struct{}{}

	for _, graphPath := range layer.FilePaths() {
		baseNodes := s.graph.GetFileNodes(graphPath)
		overlayNodes := view.GetFileNodes(graphPath)

		baseByID := map[string]*graph.Node{}
		overlayByID := map[string]*graph.Node{}
		baseByNameKind := map[string][]*graph.Node{}
		overlayByNameKind := map[string][]*graph.Node{}
		for _, n := range baseNodes {
			if n == nil {
				continue
			}
			baseByID[n.ID] = n
			key := string(n.Kind) + ":" + n.Name
			baseByNameKind[key] = append(baseByNameKind[key], n)
		}
		for _, n := range overlayNodes {
			if n == nil {
				continue
			}
			overlayByID[n.ID] = n
			key := string(n.Kind) + ":" + n.Name
			overlayByNameKind[key] = append(overlayByNameKind[key], n)
		}

		for id, n := range baseByID {
			if _, ok := overlayByID[id]; ok {
				continue // survived intact (same ID)
			}
			// Try to spot a rename: same (kind, signature) under a
			// different name in overlay nodes for this file. Costs
			// O(overlay) per missing-base node; bounded by file
			// size in practice.
			if newID, ok := matchRename(n, overlayNodes); ok {
				renameByOldID[id] = newID
				continue
			}
			removedSet[id] = struct{}{}
			// Broken-caller pass: every base caller of `id` whose
			// edge target is no longer reachable. Note: a caller
			// that lives in an overlaid file may have been re-
			// emitted by the user's edit, but the *edge* to the
			// removed target is still gone — so we flag
			// regardless, leaving it to the agent to verify.
			callerSG := baseEng.GetCallers(id, query.QueryOptions{Depth: 1, Limit: 200})
			for _, c := range callerSG.Nodes {
				if c == nil || c.ID == id {
					continue
				}
				brokenCallers[c.ID+"->"+id] = map[string]any{
					"caller_id":   c.ID,
					"caller_name": c.Name,
					"caller_path": c.FilePath,
					"caller_line": c.StartLine,
					"target_id":   id,
					"target_name": n.Name,
					"target_kind": string(n.Kind),
					"reason":      "target removed or renamed",
				}
				if isTestNode(c) {
					testTargetSet[c.FilePath] = struct{}{}
				}
			}
			// Broken-implementor pass: if the removed node is a
			// method of an interface, surface every other
			// implementor that may now drift.
			if n.Kind == graph.KindMethod {
				for _, e := range s.graph.GetInEdges(id) {
					if e.Kind == graph.EdgeImplements {
						brokenImpls[e.From+"->"+id] = map[string]any{
							"implementor_id": e.From,
							"target_id":      id,
							"target_name":    n.Name,
							"reason":         "interface contract changed",
						}
					}
				}
			}
		}
		for id, n := range overlayByID {
			if _, ok := baseByID[id]; ok {
				continue
			}
			// Filter out rename receivers — those already show up
			// in renameByOldID as `to`.
			isRenameTarget := false
			for _, to := range renameByOldID {
				if to == id {
					isRenameTarget = true
					break
				}
			}
			if isRenameTarget {
				continue
			}
			// Filter out structural noise: param / field / closure
			// nodes generally aren't useful as "added" highlights.
			if n.Kind == graph.KindParam || n.Kind == graph.KindField || n.Kind == graph.KindImport || n.Kind == graph.KindFile {
				continue
			}
			addedSet[id] = struct{}{}
		}
	}

	for id := range addedSet {
		step.symbolsAdded = append(step.symbolsAdded, id)
	}
	sort.Strings(step.symbolsAdded)
	for id := range removedSet {
		step.symbolsRemoved = append(step.symbolsRemoved, id)
	}
	sort.Strings(step.symbolsRemoved)
	for oldID, newID := range renameByOldID {
		step.symbolsRenamed = append(step.symbolsRenamed, map[string]string{
			"from": oldID,
			"to":   newID,
		})
	}
	sort.Slice(step.symbolsRenamed, func(i, j int) bool {
		return step.symbolsRenamed[i]["from"] < step.symbolsRenamed[j]["from"]
	})

	for _, bc := range brokenCallers {
		step.brokenCallers = append(step.brokenCallers, bc)
	}
	sort.Slice(step.brokenCallers, func(i, j int) bool {
		return fmt.Sprint(step.brokenCallers[i]["caller_id"]) < fmt.Sprint(step.brokenCallers[j]["caller_id"])
	})
	for _, bi := range brokenImpls {
		step.brokenImplementors = append(step.brokenImplementors, bi)
	}
	sort.Slice(step.brokenImplementors, func(i, j int) bool {
		return fmt.Sprint(step.brokenImplementors[i]["implementor_id"]) < fmt.Sprint(step.brokenImplementors[j]["implementor_id"])
	})

	for p := range testTargetSet {
		step.testTargets = append(step.testTargets, p)
	}
	sort.Strings(step.testTargets)

	// Risk rollup via the production impact analyser. Seed with the
	// union of removed + renamed-from IDs because those are the
	// most likely contract breakers; added IDs don't have a base
	// dependency surface to walk.
	seedIDs := append([]string{}, step.symbolsRemoved...)
	for _, ren := range step.symbolsRenamed {
		seedIDs = append(seedIDs, ren["from"])
	}
	if len(seedIDs) > 0 {
		s.analysisMu.RLock()
		comms := s.communities
		procs := s.processes
		s.analysisMu.RUnlock()
		impact := analysis.AnalyzeImpact(s.graph, seedIDs, comms, procs)
		step.impact = map[string]any{
			"risk":              string(impact.Risk),
			"summary":           impact.Summary,
			"total_affected":    impact.TotalAffected,
			"test_files":        impact.TestFiles,
			"cross_repo_impact": impact.CrossRepoImpact,
		}
		for _, p := range impact.TestFiles {
			if _, ok := testTargetSet[p]; !ok {
				step.testTargets = append(step.testTargets, p)
			}
		}
		sort.Strings(step.testTargets)
	}
	_ = overlayEng // reserved for downstream queries; impact analyser uses base graph

	step.summary = fmt.Sprintf(
		"touched=%d added=%d removed=%d renamed=%d broken_callers=%d broken_implementors=%d",
		len(step.touchedFiles),
		len(step.symbolsAdded),
		len(step.symbolsRemoved),
		len(step.symbolsRenamed),
		len(step.brokenCallers),
		len(step.brokenImplementors),
	)
}

// matchRename heuristically pairs a removed base node with an added
// overlay node when they share kind and signature shape. Returns the
// overlay ID and true on a CONFIDENT match. Pure-name renames are
// matched when the signature is distinctive enough to rule out a
// coincidental utility-function pairing.
//
// Two guards keep false positives under control:
//
//  1. The signature must be non-trivial. A bare `func ()` matches
//     every other void no-arg function in the file — declaring those
//     as renames would burn the agent's attention on phantom
//     refactors. We require either a non-empty parameter list, a
//     return type, or a receiver to declare a rename.
//
//  2. The match must be unambiguous. If two or more overlay
//     candidates share the same kind+signature shape, we can't tell
//     which one is the renamed-to target, so we bail to the
//     removed+added classification instead of guessing.
func matchRename(base *graph.Node, overlayNodes []*graph.Node) (string, bool) {
	if base == nil {
		return "", false
	}
	baseSig := metaString(base, "signature")
	if !signatureIsDistinctive(baseSig, base.Name) {
		return "", false
	}
	normBase := stripIdentifier(baseSig, base.Name)

	var matches []string
	for _, n := range overlayNodes {
		if n == nil || n.Kind != base.Kind {
			continue
		}
		if n.Name == base.Name {
			continue // same name → not a rename
		}
		nSig := metaString(n, "signature")
		if nSig == "" {
			continue
		}
		if stripIdentifier(nSig, n.Name) == normBase {
			matches = append(matches, n.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return "", false
}

// signatureIsDistinctive rejects trivially-shaped signatures that
// would create ambiguous rename matches (every void no-arg function
// looks identical to every other one). A signature is distinctive
// when it carries a parameter, a return type, or a receiver — any
// piece that distinguishes it from the empty `func ()` shape.
func signatureIsDistinctive(sig, name string) bool {
	if sig == "" {
		return false
	}
	stripped := strings.TrimSpace(stripIdentifier(sig, name))
	// Common shapes for parameterless void functions across
	// languages: `func <id>()`, `func <id>() {}`, `def <id>()`,
	// `<id>(): void`. We reject the canonical empty forms and
	// accept anything that contains a substantive parameter or
	// return component.
	trivials := []string{
		"func <id>()",
		"func <id>() {}",
		"<id>()",
		"<id>() {}",
		"def <id>()",
		"def <id>(): void",
		"def <id>(self)",
	}
	return !slices.Contains(trivials, stripped)
}

func metaString(n *graph.Node, key string) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	if v, ok := n.Meta[key].(string); ok {
		return v
	}
	return ""
}

// stripIdentifier rewrites the symbol's own identifier inside the
// signature to a placeholder so two signatures that differ only in
// the function name compare equal.
func stripIdentifier(sig, name string) string {
	if sig == "" || name == "" {
		return sig
	}
	return strings.ReplaceAll(sig, name, "<id>")
}

func isTestNode(n *graph.Node) bool {
	if n == nil {
		return false
	}
	p := strings.ToLower(n.FilePath)
	return strings.HasSuffix(p, "_test.go") || strings.Contains(p, "/test/") || strings.Contains(p, "/__tests__/") || strings.HasSuffix(p, ".test.ts") || strings.HasSuffix(p, ".test.tsx") || strings.HasSuffix(p, ".test.js") || strings.HasSuffix(p, "_test.py") || strings.HasSuffix(p, "_spec.rb")
}

// ---------------------------------------------------------------------------
// edit grouping + apply
// ---------------------------------------------------------------------------

type simulationFileEdit struct {
	absPath     string
	overlayPath string
	edits       []lsp.TextEdit
	deleted     bool
}

// groupEditByFile resolves a WorkspaceEdit's TextEdits into per-file
// tuples keyed by absolute path. URI scheme is recognised; otherwise
// the path is taken verbatim. Files that don't resolve to any
// tracked workspace fall through with empty absPath — the simulator
// still tracks them as "touched_files" but the graph layer will skip
// them (silently, matching the disk path's behaviour for untracked
// files).
func (s *Server) groupEditByFile(edit lsp.WorkspaceEdit) ([]simulationFileEdit, error) {
	bucket := map[string]*simulationFileEdit{}
	addEdits := func(rawURI string, edits []lsp.TextEdit) error {
		path := normaliseEditURI(rawURI)
		if path == "" {
			return fmt.Errorf("workspace_edit entry has empty path or unsupported URI: %q", rawURI)
		}
		abs, err := s.resolveOverlayAbsPath(path)
		if err != nil {
			return err
		}
		key := abs
		if key == "" {
			key = path
		}
		fe, ok := bucket[key]
		if !ok {
			fe = &simulationFileEdit{absPath: abs, overlayPath: path}
			bucket[key] = fe
		}
		fe.edits = append(fe.edits, edits...)
		return nil
	}
	if len(edit.DocumentChanges) > 0 {
		for _, dc := range edit.DocumentChanges {
			if err := addEdits(dc.TextDocument.URI, dc.Edits); err != nil {
				return nil, err
			}
		}
	}
	if len(edit.Changes) > 0 {
		for uri, edits := range edit.Changes {
			if err := addEdits(uri, edits); err != nil {
				return nil, err
			}
		}
	}
	out := make([]simulationFileEdit, 0, len(bucket))
	for _, fe := range bucket {
		out = append(out, *fe)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].overlayPath < out[j].overlayPath })
	return out, nil
}

// normaliseEditURI converts the various URI / path shapes a client
// might send into a single normalised form the overlay path
// resolver understands. `file://` is stripped; everything else is
// returned verbatim. URL-encoded paths are decoded.
func normaliseEditURI(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "file://") {
		// lspuri.URIToAbsPath parses the URI: drops the optional host
		// (e.g. localhost), decodes %xx, and fixes the Windows
		// drive-letter / separator shape. Any parse failure falls back
		// to the raw string.
		if p := lspuri.URIToAbsPath(raw); p != "" {
			return p
		}
	}
	return raw
}

// contentForSim returns (content, hadPriorContent) for the overlay
// path. Prior overlay state (from earlier steps or from
// inherit_overlay) wins; otherwise we read from disk. Deleted-overlay
// markers fall through as ("", false) so the caller treats them as
// the file being absent.
func contentForSim(current map[string]daemon.OverlayFile, absPath, overlayPath string) (string, bool) {
	key := filepath.Clean(overlayPath)
	if cur, ok := current[key]; ok {
		if cur.Deleted {
			return "", false
		}
		return cur.Content, true
	}
	if absPath != "" {
		if b, err := readSimFile(absPath); err == nil {
			return b, true
		}
	}
	return "", false
}

// applySimulationEdits is a thin wrapper around lsp.ApplyEditsToContent
// that returns the resulting byte slice. We avoid taking a hard
// dependency on the package's private helper by re-implementing the
// reverse-sort apply here — keeps the simulation engine self-contained
// against future refactors of the action package.
func applySimulationEdits(content []byte, edits []lsp.TextEdit) ([]byte, error) {
	if len(edits) == 0 {
		return content, nil
	}
	sorted := make([]lsp.TextEdit, len(edits))
	copy(sorted, edits)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Range.Start.Line != sorted[j].Range.Start.Line {
			return sorted[i].Range.Start.Line > sorted[j].Range.Start.Line
		}
		return sorted[i].Range.Start.Character > sorted[j].Range.Start.Character
	})
	out := make([]byte, len(content))
	copy(out, content)
	for _, e := range sorted {
		startOff, err := lspPositionToByteOffset(out, e.Range.Start)
		if err != nil {
			return nil, err
		}
		endOff, err := lspPositionToByteOffset(out, e.Range.End)
		if err != nil {
			return nil, err
		}
		if startOff > endOff || startOff < 0 || endOff > len(out) {
			return nil, fmt.Errorf("invalid edit range: start=%d end=%d len=%d", startOff, endOff, len(out))
		}
		newBytes := []byte(e.NewText)
		merged := make([]byte, 0, len(out)-(endOff-startOff)+len(newBytes))
		merged = append(merged, out[:startOff]...)
		merged = append(merged, newBytes...)
		merged = append(merged, out[endOff:]...)
		out = merged
	}
	return out, nil
}

// lspPositionToByteOffset maps an LSP (line, char) — character is in
// UTF-16 code units per spec but we treat it as bytes here for ASCII
// fast-path, matching the production apply path's behaviour. This is
// fine for simulation: any UTF-16 mismatch shows up identically on
// commit since both paths use the same arithmetic.
func lspPositionToByteOffset(content []byte, pos lsp.Position) (int, error) {
	line := 0
	for i := range content {
		if line == pos.Line {
			if pos.Character == 0 {
				return i, nil
			}
			// Walk forward `pos.Character` bytes within this line
			// (counting newline as out-of-line).
			end := min(i+pos.Character, len(content))
			for j := i; j < end; j++ {
				if content[j] == '\n' {
					return j, nil
				}
			}
			return end, nil
		}
		if content[i] == '\n' {
			line++
		}
	}
	if line == pos.Line && pos.Character == 0 {
		return len(content), nil
	}
	if pos.Line == line+1 && pos.Character == 0 {
		// "Insert at EOF" — common shape for append-style edits.
		return len(content), nil
	}
	if pos.Line > line {
		return len(content), nil
	}
	return 0, fmt.Errorf("position line=%d char=%d out of range", pos.Line, pos.Character)
}

// ---------------------------------------------------------------------------
// diagnostics integration
// ---------------------------------------------------------------------------

// simulateDiagnostics runs the LSP didOpen / didChange / wait /
// revert sequence for every touched file in the simulation's final
// state. The implementation is delicate — we MUST restore the LSP
// server to the on-disk state once the simulation completes, even on
// error, otherwise a subsequent get_diagnostics call from another
// session would see the simulated content as authoritative.
func (s *Server) simulateDiagnostics(ctx context.Context, sim *simulation, touched []string, timeout time.Duration) []map[string]any {
	if len(sim.snapshots) == 0 {
		return nil
	}
	return s.simulateDiagnosticsAtStep(ctx, sim, len(sim.snapshots)-1, touched, timeout)
}

func (s *Server) simulateDiagnosticsAtStep(_ context.Context, sim *simulation, stepIdx int, touched []string, timeout time.Duration) []map[string]any {
	if s.semanticMgr == nil || sim == nil || stepIdx < 0 || stepIdx >= len(sim.snapshots) {
		return nil
	}
	snapshot := sim.snapshots[stepIdx]
	// Build a lookup from absPath -> overlay content for this step.
	bySnapshotPath := map[string]daemon.OverlayFile{}
	for _, f := range snapshot {
		abs, _ := s.resolveOverlayAbsPath(f.Path)
		if abs == "" {
			continue
		}
		bySnapshotPath[filepath.Clean(abs)] = f
	}

	out := []map[string]any{}
	for _, overlayPath := range touched {
		absPath, err := s.resolveOverlayAbsPath(overlayPath)
		if err != nil || absPath == "" {
			continue
		}
		provider, _, perr := s.lspProviderForPath(absPath)
		if perr != nil || provider == nil {
			continue
		}
		// Open the file on the LSP server (idempotent — keeps base
		// content visible to other sessions until the change pass).
		if err := provider.EnsureFileOpen(filepath.Dir(absPath), filepath.Base(absPath)); err != nil {
			continue
		}
		// Read the on-disk authoritative content so we can restore.
		original, readErr := readSimFile(absPath)
		if readErr != nil {
			// File may not exist on disk yet (new file via overlay).
			// Treat the LSP server as having "" as the prior state;
			// restore via didChange("") at the end.
			original = ""
		}
		// Push the simulated content via didChange (full-text
		// replace) so the server re-analyses.
		ov, hadSim := bySnapshotPath[filepath.Clean(absPath)]
		simContent := ""
		if hadSim && !ov.Deleted {
			simContent = ov.Content
		}
		if err := provider.PushSimulatedContent(absPath, simContent); err != nil {
			continue
		}
		diags := provider.WaitForDiagnostics(absPath, timeout)
		// Restore the on-disk state so other sessions don't see the
		// simulation. We use the same didChange path, which bumps
		// the version monotonically (the simulated push already
		// did) — no leaks.
		_ = provider.PushSimulatedContent(absPath, original)
		out = append(out, map[string]any{
			"path":        overlayPath,
			"server":      provider.Name(),
			"diagnostics": diagsToWire(diags),
			"total":       len(diags),
		})
	}
	return out
}

// summariseDiagnostics buckets a per-file diagnostics payload into
// counts by severity. Severity numbers follow LSP convention (1=error,
// 2=warning, 3=info, 4=hint). The summary keys are stable strings so
// chain-step deltas read cleanly.
func summariseDiagnostics(payload []map[string]any) map[string]int {
	out := map[string]int{}
	for _, fileEntry := range payload {
		raw, ok := fileEntry["diagnostics"].([]map[string]any)
		if !ok {
			continue
		}
		for _, d := range raw {
			sev := severityLabel(d["severity"])
			out[sev]++
		}
	}
	return out
}

func severityLabel(v any) string {
	n, ok := toInt(v)
	if !ok {
		return "unknown"
	}
	switch n {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "unknown"
	}
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// keep: promote the final simulation overlay to a real session
// ---------------------------------------------------------------------------

// persistSimulationOverlay copies the simulation's final snapshot
// into a real overlay session bound to the calling MCP session and
// returns the session ID. When the calling context has no MCP
// session (embedded stdio, no Mcp-Session-Id) the promotion fails
// cleanly with an empty ID and no error so the chain response still
// reports `kept: false` and the simulation result is otherwise
// complete.
func (s *Server) persistSimulationOverlay(ctx context.Context, sim *simulation) (string, error) {
	if sim == nil || len(sim.snapshots) == 0 {
		return "", nil
	}
	if s.overlays == nil {
		return "", errors.New("overlay support is not enabled on this server")
	}
	sessID := OverlayCohortIDFromContext(ctx)
	if sessID == "" {
		return "", nil
	}
	// Idempotent register; matches handleOverlayPush's implicit-
	// register fallback.
	if !s.overlays.Has(sessID) {
		workspace, _, _ := s.sessionScope(ctx)
		_ = s.overlays.RegisterWithID(sessID, workspace)
	}
	final := sim.snapshots[len(sim.snapshots)-1]
	for _, f := range final {
		if err := s.overlays.Push(sessID, f, nil); err != nil {
			return sessID, err
		}
	}
	s.overlayCacheInvalidate(sessID)
	return sessID, nil
}

// ---------------------------------------------------------------------------
// parsing helpers
// ---------------------------------------------------------------------------

// parseWorkspaceEdit accepts the WorkspaceEdit JSON form the agent
// supplies. We trim leading whitespace because some clients
// pretty-print the payload before stringifying it inside the tool
// argument. The unmarshaller accepts either `changes` (legacy form,
// `{uri: [TextEdit,...]}`) or `documentChanges` (modern form) — both
// are valid LSP shapes.
func parseWorkspaceEdit(raw string) (lsp.WorkspaceEdit, error) {
	if len(raw) > overlaySimulationInputMaxBytes {
		return lsp.WorkspaceEdit{}, fmt.Errorf("workspace_edit input exceeds limit %d bytes", overlaySimulationInputMaxBytes)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return lsp.WorkspaceEdit{}, errors.New("workspace_edit is empty")
	}
	var edit lsp.WorkspaceEdit
	if err := json.Unmarshal([]byte(raw), &edit); err != nil {
		return lsp.WorkspaceEdit{}, err
	}
	return edit, nil
}

func isEmptyEdit(edit lsp.WorkspaceEdit) bool {
	if len(edit.Changes) > 0 {
		for _, edits := range edit.Changes {
			if len(edits) > 0 {
				return false
			}
		}
	}
	if len(edit.DocumentChanges) > 0 {
		for _, dc := range edit.DocumentChanges {
			if len(dc.Edits) > 0 {
				return false
			}
		}
	}
	return len(edit.Changes) == 0 && len(edit.DocumentChanges) == 0
}

// sortedSetKeys returns the map keys in sorted order. Helper to keep
// JSON output deterministic.
func sortedSetKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func renameMapToSlice(renames map[string]string) []map[string]string {
	out := make([]map[string]string, 0, len(renames))
	for from, to := range renames {
		out = append(out, map[string]string{"from": from, "to": to})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["from"] < out[j]["from"] })
	return out
}
