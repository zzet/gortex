package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/audit"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// registerPRReviewContextTool wires pr_review_context — a one-call,
// LLM-free, deterministic rollup of the analysis-layer review gates for a
// changeset. It composes the existing diff-context, signature-verification,
// agent-config-audit, and (optionally) simulation passes into a single
// envelope with a top-level verdict tying them together. Side-effect free:
// the simulation path layers a shadow view on top of the base graph and
// never writes disk or mutates the base graph.
//
// It is the lightweight sibling of review_pack: where review_pack runs the
// full review engine (LLM findings, per-symbol classification, the pack),
// pr_review_context answers the cheap question "what do the deterministic
// gates say about this diff" with no LLM round-trip. Deferred: it lands in
// the lazy catalog unless lazy tools are disabled.
func (s *Server) registerPRReviewContextTool() {
	s.addTool(
		mcp.NewTool("pr_review_context",
			mcp.WithDescription("Assemble a deterministic, LLM-free PR-review rollup for a changeset in one call. Composes up to five sections: diff_context (graph-enriched changed-symbol context — signatures, callers, callees, community, per-file risk), verify_change (broken callers / interface implementors a signature edit would introduce), simulate_chain (impact of an optional edit chain — gated: only runs when an explicit overlay session id is supplied), audit_agent_config (stale-symbol / dead-path / bloat drift in the repo's agent-config files), plus a composite verdict (PASS / WARN / BLOCK) derived from the worst section. Pass `base` (a git ref — diff scopes against it) or `scope`/`base_ref`; `edits` (a JSON array of LSP WorkspaceEdit objects) plus `session_id` (the overlay session to layer them on) to enable the simulation section. Side-effect free: disk and the base graph are never mutated. The cheap deterministic counterpart to review_pack — escalate to review_pack for LLM-grade inline findings."),
			mcp.WithString("base", mcp.Description("Base git ref (e.g. main). Diffs HEAD against it (compare scope). Convenience alias for scope=compare + base_ref.")),
			mcp.WithString("scope", mcp.Description("Diff scope: unstaged (default), staged, all, or compare. Ignored when `base` is set.")),
			mcp.WithString("base_ref", mcp.Description("Base ref for compare scope (default main). Ignored when `base` is set.")),
			mcp.WithString("ids", mcp.Description("Comma-separated changed symbol IDs to scope the rollup to. Takes precedence over the git diff when supplied.")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithBoolean("verify", mcp.Description("Include the verify_change section — broken callers / implementors a signature edit would introduce (default true).")),
			mcp.WithBoolean("audit_config", mcp.Description("Include the audit_agent_config section — stale-symbol / dead-path / bloat drift in the repo's agent-config files (default true).")),
			mcp.WithString("edits", mcp.Description("JSON array of LSP WorkspaceEdit objects. With `session_id` set, the simulation section runs the chain against that overlay session and reports the per-step impact. Without a session id the simulation section is omitted with a note (no synthesized session).")),
			mcp.WithString("session_id", mcp.Description("Overlay session id the edit chain layers on top of. Required to enable the simulate_chain section — it gates the simulation behind an explicit, caller-supplied session so it never runs against a synthesized or empty session.")),
			mcp.WithString("sections", mcp.Description("Comma-separated subset of {diff_context,verify,simulate,audit_config} to emit. Omit for all requested sections. Drops heavy blocks an agent does not need.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description("Cap the marshaled response at approximately this many tokens. Composable with max_bytes — tighter wins. Omit for no cap.")),
		),
		s.handlePRReviewContext,
	)
}

// PR-review verdict tokens. The worst section sets the overall verdict.
const (
	prReviewPass  = "PASS"
	prReviewWarn  = "WARN"
	prReviewBlock = "BLOCK"
)

// reviewGate is one named deterministic gate's outcome — the section name,
// its PASS/WARN/BLOCK status, and a one-line human-readable detail.
type reviewGate struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// prReviewContext is the one-call rollup envelope. Each optional section is
// populated only when requested and when there is something to report; the
// gates slice always carries one row per evaluated section so the verdict is
// auditable.
type prReviewContext struct {
	Verdict        string                    `json:"verdict"`
	Gates          []reviewGate              `json:"gates"`
	ChangedFiles   []string                  `json:"changed_files"`
	ChangedSymbols int                       `json:"changed_symbols"`
	DiffContext    []diffContextSymbol       `json:"diff_context,omitempty"`
	Impact         *prReviewImpact           `json:"impact,omitempty"`
	Contracts      *contractImpact           `json:"contracts,omitempty"`
	Verify         *analysis.VerifyResult    `json:"verify,omitempty"`
	Guards         []analysis.GuardViolation `json:"guards,omitempty"`
	Simulation     *prReviewSimulation       `json:"simulation,omitempty"`
	ConfigAudit    *audit.Report             `json:"config_audit,omitempty"`
}

// diffContextSymbol is the graph-enriched view of one changed symbol: its
// signature, depth-1 callers/callees, community, affected processes, the
// file it lives in, and that file's blast-radius risk tier.
type diffContextSymbol struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	File      string   `json:"file"`
	Line      int      `json:"start_line"`
	Risk      string   `json:"risk"`
	Signature string   `json:"signature,omitempty"`
	Callers   []string `json:"callers,omitempty"`
	Callees   []string `json:"callees,omitempty"`
	Community string   `json:"community,omitempty"`
	Processes []string `json:"processes,omitempty"`
}

// prReviewImpact is the composite blast-radius summary over the whole
// changeset — the worst risk tier and the total affected count.
type prReviewImpact struct {
	Risk          string `json:"risk"`
	TotalAffected int    `json:"total_affected"`
	Summary       string `json:"summary,omitempty"`
}

// prReviewSimulation carries the simulation section. When the section could
// not run (no overlay session, no edits, or a build error) Ran is false and
// Note explains why so the caller sees an omitted-with-note section rather
// than a panic or a silent drop.
type prReviewSimulation struct {
	Ran            bool             `json:"ran"`
	Note           string           `json:"note,omitempty"`
	SessionID      string           `json:"session_id,omitempty"`
	TotalSteps     int              `json:"total_steps,omitempty"`
	Steps          []map[string]any `json:"steps,omitempty"`
	Cumulative     map[string]any   `json:"cumulative,omitempty"`
	GraphUntouched bool             `json:"graph_untouched"`
}

// changedSymbolsToSignatureChanges derives the verify_change input from the
// changeset: for every changed symbol that has an indexed signature, a
// SignatureChange echoing the current signature. Echoing the existing
// signature lets VerifyChanges walk the caller / implementor surface for the
// changed set without the caller having to hand-author proposed signatures —
// the broken-edge gate is "does this symbol's surface still hold for everyone
// who depends on it". Symbols with no recorded signature are skipped.
func changedSymbolsToSignatureChanges(g graph.Reader, changed []analysis.ChangedSymbol) []analysis.SignatureChange {
	if g == nil {
		return nil
	}
	var out []analysis.SignatureChange
	for _, cs := range changed {
		if cs.ID == "" {
			continue
		}
		node := g.GetNode(cs.ID)
		if node == nil || node.Meta == nil {
			continue
		}
		sig, ok := node.Meta["signature"].(string)
		if !ok || strings.TrimSpace(sig) == "" {
			continue
		}
		out = append(out, analysis.SignatureChange{
			SymbolID:     cs.ID,
			NewSignature: sig,
		})
	}
	return out
}

// handlePRReviewContext runs the deterministic review gates for the
// changeset and returns the composite envelope.
func (s *Server) handlePRReviewContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	wantSection := prReviewSectionSet(req.GetString("sections", ""))
	wantVerify := req.GetBool("verify", true) && wantSection("verify")
	wantAudit := req.GetBool("audit_config", true) && wantSection("audit_config")
	wantDiffCtx := wantSection("diff_context")
	wantSimulate := wantSection("simulate")

	repoRoot := s.prReviewRepoRoot(ctx, req)

	scope, baseRef := siblingDiffScope(req)

	// Enumerate the changeset. Caller-supplied ids take precedence; otherwise
	// map the git diff to changed symbols.
	var (
		diff *analysis.DiffResult
		ids  []string
	)
	if idsArg := strings.TrimSpace(req.GetString("ids", "")); idsArg != "" {
		diff = s.prReviewDiffFromIDs(ctx, idsArg)
		for _, cs := range diff.ChangedSymbols {
			ids = append(ids, cs.ID)
		}
	} else {
		if repoRoot == "" {
			return mcp.NewToolResultError("could not resolve a repository root for the changeset diff"), nil
		}
		d, err := analysis.MapGitDiff(s.readerFor(ctx), repoRoot, s.diffJoinPrefix(repoRoot), scope, baseRef)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		diff = d
		for _, cs := range diff.ChangedSymbols {
			if cs.ID != "" {
				ids = append(ids, cs.ID)
			}
		}
	}

	out := prReviewContext{
		ChangedFiles:   diff.ChangedFiles,
		ChangedSymbols: len(diff.ChangedSymbols),
	}

	// --- section: verify_change (computed FIRST) ---
	//
	// VerifyChanges reads the changed symbol's old signature and every
	// implementor's signature from node.Meta. The impact / diff_context
	// passes below run AnalyzeImpact and brief engine queries that re-stamp
	// (and, for the brief-detail path, can empty) node.Meta in place — so the
	// signature surface must be read before any of them runs. We compute the
	// verify result here and append its gate in section order below.
	var (
		verifyResult *analysis.VerifyResult
		verifyGate   *reviewGate
	)
	if wantVerify {
		changes := changedSymbolsToSignatureChanges(s.readerFor(ctx), diff.ChangedSymbols)
		if len(changes) > 0 {
			verifyResult = analysis.VerifyChanges(s.readerFor(ctx), s.engineFor(ctx), changes)
			out.Verify = verifyResult
			g := prReviewVerifyGate(verifyResult)
			verifyGate = &g
		} else {
			verifyGate = &reviewGate{
				Name: "verify_change", Status: prReviewPass,
				Detail: "no signature-bearing changed symbols to verify",
			}
		}
	}

	communities := s.getCommunities()
	processes := s.getProcesses()

	// --- composite impact (verdict input; also exposed) ---
	if len(ids) > 0 {
		imp := analysis.AnalyzeImpact(s.readerFor(ctx), ids, communities, processes)
		out.Impact = &prReviewImpact{
			Risk:          string(imp.Risk),
			TotalAffected: imp.TotalAffected,
			Summary:       imp.Summary,
		}
		out.Gates = append(out.Gates, prReviewImpactGate(imp.Risk))
	}

	// --- section: diff_context ---
	if wantDiffCtx {
		out.DiffContext = s.buildDiffContextSection(ctx, diff, communities, processes)
		out.Gates = append(out.Gates, reviewGate{
			Name: "diff_context", Status: prReviewPass,
			Detail: detailf("%d changed symbol(s) across %d file(s)", len(out.DiffContext), len(diff.ChangedFiles)),
		})
	}

	// --- contracts gate (always evaluated for the verdict) ---
	if len(ids) > 0 {
		contracts := s.computeContractImpact(ids)
		out.Contracts = contracts
		out.Gates = append(out.Gates, prReviewContractGate(contracts))
	}

	// --- section: verify_change gate (result computed above) ---
	if verifyGate != nil {
		out.Gates = append(out.Gates, *verifyGate)
	}

	// --- guards gate (architecture + co-change / boundary rules) ---
	if len(ids) > 0 && (s.hasGuardRules(ids) || !s.architecture.IsEmpty()) {
		guardReader := s.readerFor(ctx)
		guards := s.evaluateGuards(guardReader, ids)
		guards = append(guards, analysis.EvaluateArchitecture(guardReader, s.architecture, ids)...)
		out.Guards = guards
		out.Gates = append(out.Gates, prReviewGuardGate(guards))
	}

	// --- section: simulate_chain (gated on an explicit overlay session) ---
	if wantSimulate {
		sim, gate, simErr := s.buildPRReviewSimulation(ctx, req)
		if simErr != nil {
			return nil, simErr
		}
		out.Simulation = sim
		out.Gates = append(out.Gates, gate)
	}

	// --- section: audit_agent_config ---
	if wantAudit {
		report := s.buildConfigAuditSection(ctx, repoRoot)
		out.ConfigAudit = report
		out.Gates = append(out.Gates, prReviewAuditGate(report))
	}

	out.Verdict = worstVerdict(out.Gates)

	payload := prReviewContextPayload(out)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodePRReviewContext(out))
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// prReviewRepoRoot resolves the working-tree root for the changeset diff,
// mirroring the review tools' resolution order: an explicit repo selector
// (prefix or path), the lone tracked repo, or the session's cwd-bound repo.
func (s *Server) prReviewRepoRoot(ctx context.Context, req mcp.CallToolRequest) string {
	repoRoot, _ := s.diffRepoScope(ctx, strings.TrimSpace(req.GetString("repo", "")))
	return repoRoot
}

// prReviewDiffFromIDs synthesises a DiffResult from caller-supplied symbol
// IDs so the rollup can run without a working tree. Each ID resolves to its
// graph node for the name / kind / file, and the file set is unioned.
func (s *Server) prReviewDiffFromIDs(ctx context.Context, idsArg string) *analysis.DiffResult {
	diff := &analysis.DiffResult{}
	reader := s.readerFor(ctx)
	fileSet := map[string]bool{}
	for _, raw := range strings.Split(idsArg, ",") {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		cs := analysis.ChangedSymbol{ID: id}
		if node := reader.GetNode(id); node != nil {
			cs.Name = node.Name
			cs.Kind = string(node.Kind)
			cs.FilePath = node.FilePath
			cs.Line = node.StartLine
			if node.FilePath != "" && !fileSet[node.FilePath] {
				fileSet[node.FilePath] = true
				diff.ChangedFiles = append(diff.ChangedFiles, node.FilePath)
			}
		}
		diff.ChangedSymbols = append(diff.ChangedSymbols, cs)
	}
	sort.Strings(diff.ChangedFiles)
	return diff
}

// buildDiffContextSection builds the graph-enriched changed-symbol context —
// the same per-symbol enrichment diff_context surfaces (signature, depth-1
// callers/callees, community, processes) plus the per-file blast-radius risk
// tier, capped to keep the section bounded.
func (s *Server) buildDiffContextSection(ctx context.Context, diff *analysis.DiffResult, communities *analysis.CommunityResult, processes *analysis.ProcessResult) []diffContextSymbol {
	const cap = 50
	engine := s.engineFor(ctx)
	reader := s.readerFor(ctx)

	// Pre-compute per-file risk so every symbol in a file shares one tier.
	fileIDs := map[string][]string{}
	for _, cs := range diff.ChangedSymbols {
		if cs.ID == "" {
			continue
		}
		node := reader.GetNode(cs.ID)
		if node == nil || node.FilePath == "" {
			continue
		}
		fileIDs[node.FilePath] = append(fileIDs[node.FilePath], cs.ID)
	}
	fileRisk := map[string]string{}
	for fp, fids := range fileIDs {
		imp := analysis.AnalyzeImpact(reader, fids, communities, processes)
		fileRisk[fp] = string(imp.Risk)
	}

	var syms []diffContextSymbol
	for _, cs := range diff.ChangedSymbols {
		if len(syms) >= cap {
			break
		}
		node := reader.GetNode(cs.ID)
		if node == nil {
			continue
		}
		info := diffContextSymbol{
			ID:   cs.ID,
			Name: cs.Name,
			Kind: cs.Kind,
			File: node.FilePath,
			Line: cs.Line,
			Risk: fileRisk[node.FilePath],
		}
		if sig, ok := node.Meta["signature"].(string); ok {
			info.Signature = sig
		}
		callers := engine.GetCallers(cs.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if cn.ID != cs.ID {
				info.Callers = append(info.Callers, cn.ID)
			}
		}
		callees := engine.GetCallChain(cs.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
		for _, cn := range callees.Nodes {
			if cn.ID != cs.ID {
				info.Callees = append(info.Callees, cn.ID)
			}
		}
		if communities != nil {
			if cid, ok := communities.NodeToComm[cs.ID]; ok {
				info.Community = cid
			}
		}
		if processes != nil {
			info.Processes = processes.NodeToProcs[cs.ID]
		}
		syms = append(syms, info)
	}
	return syms
}

// buildConfigAuditSection runs the agent-config drift audit over the repo's
// discovered config files. Returns nil when no root or no config files.
func (s *Server) buildConfigAuditSection(ctx context.Context, repoRoot string) *audit.Report {
	root := repoRoot
	if root == "" && s.indexer != nil {
		root = s.indexer.RootPath()
	}
	if root == "" {
		return nil
	}
	files := audit.DiscoverConfigFiles(root)
	if len(files) == 0 {
		return &audit.Report{Root: root, FilesScanned: 0}
	}
	return audit.Audit(s.readerFor(ctx), root, files)
}

// buildPRReviewSimulation runs the optional simulation section. It is gated
// behind an explicit overlay session id: buildSimulation can layer a shadow
// view on top of the base graph, but persisting / inheriting overlay state
// requires a real MCP session, and synthesising an empty one would silently
// diverge from the caller's actual buffers. So when no session id is supplied
// (via the `session_id` param or the request context) the section is omitted
// with a note rather than run against a phantom session. The base graph is
// never mutated regardless.
func (s *Server) buildPRReviewSimulation(ctx context.Context, req mcp.CallToolRequest) (*prReviewSimulation, reviewGate, error) {
	editsArg := strings.TrimSpace(req.GetString("edits", ""))
	if editsArg == "" {
		return &prReviewSimulation{
			Ran: false, GraphUntouched: true,
			Note: "no `edits` supplied — pass a JSON array of WorkspaceEdit objects to run the simulation section",
		}, reviewGate{
			Name: "simulate_chain", Status: prReviewPass,
			Detail: "skipped: no edits supplied",
		}, nil
	}

	sessionID := strings.TrimSpace(req.GetString("session_id", ""))
	if sessionID == "" {
		sessionID = SessionIDFromContext(ctx)
	}
	if sessionID == "" {
		return &prReviewSimulation{
			Ran: false, GraphUntouched: true,
			Note: "simulation section skipped: pass an explicit `session_id` (an overlay session) — the simulation is not run against a synthesized or empty session",
		}, reviewGate{
			Name: "simulate_chain", Status: prReviewPass,
			Detail: "skipped: no overlay session id",
		}, nil
	}

	edits, err := parsePRReviewEdits(editsArg)
	if err != nil {
		return &prReviewSimulation{
			Ran: false, GraphUntouched: true, SessionID: sessionID,
			Note: "invalid edits: " + err.Error(),
		}, reviewGate{
			Name: "simulate_chain", Status: prReviewWarn,
			Detail: "invalid edits: " + err.Error(),
		}, nil
	}

	// Run the chain on top of the named session's overlay. buildSimulation
	// reads the session via the request context, so thread the explicit
	// session id onto the context it consumes.
	simCtx := WithSessionID(ctx, sessionID)
	sim, simErr := s.buildSimulation(simCtx, edits, true)
	if simErr != nil {
		if ctxErr := requestContextError(simCtx, simErr); ctxErr != nil {
			return nil, reviewGate{}, ctxErr
		}
		return &prReviewSimulation{
			Ran: false, GraphUntouched: true, SessionID: sessionID,
			Note: "simulation failed: " + simErr.Error(),
		}, reviewGate{
			Name: "simulate_chain", Status: prReviewWarn,
			Detail: "simulation failed: " + simErr.Error(),
		}, nil
	}

	steps := make([]map[string]any, 0, len(sim.steps))
	brokenAny := false
	for i, step := range sim.steps {
		steps = append(steps, map[string]any{
			"index":               i,
			"touched_files":       step.touchedFiles,
			"broken_callers":      step.brokenCallers,
			"broken_implementors": step.brokenImplementors,
			"summary":             step.summary,
		})
		if len(step.brokenCallers) > 0 || len(step.brokenImplementors) > 0 {
			brokenAny = true
		}
	}

	out := &prReviewSimulation{
		Ran:            true,
		SessionID:      sessionID,
		TotalSteps:     len(sim.steps),
		Steps:          steps,
		Cumulative:     sim.cumulative,
		GraphUntouched: true,
	}
	status := prReviewPass
	detail := detailf("%d step(s), no broken edges", len(sim.steps))
	if brokenAny {
		status = prReviewBlock
		detail = detailf("%d step(s) introduce broken callers / implementors", len(sim.steps))
	}
	return out, reviewGate{Name: "simulate_chain", Status: status, Detail: detail}, nil
}

// parsePRReviewEdits parses the `edits` JSON array into WorkspaceEdits,
// reusing the same per-edit parser the simulate_chain handler uses.
func parsePRReviewEdits(raw string) ([]lsp.WorkspaceEdit, error) {
	if len(raw) > overlaySimulationInputMaxBytes {
		return nil, fmt.Errorf("edits input exceeds limit %d bytes", overlaySimulationInputMaxBytes)
	}
	var rawEdits []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &rawEdits); err != nil {
		return nil, errors.New("edits must be a JSON array of WorkspaceEdit objects: " + err.Error())
	}
	if len(rawEdits) == 0 {
		return nil, errors.New("edits array is empty")
	}
	if len(rawEdits) > overlaySimulationMaxSteps {
		return nil, fmt.Errorf("edits exceed limit %d", overlaySimulationMaxSteps)
	}
	edits := make([]lsp.WorkspaceEdit, 0, len(rawEdits))
	for i, re := range rawEdits {
		edit, err := parseWorkspaceEdit(string(re))
		if err != nil {
			return nil, fmt.Errorf("edit %d: %w", i, err)
		}
		if isEmptyEdit(edit) {
			return nil, fmt.Errorf("edit %d: workspace_edit contains no document changes", i)
		}
		edits = append(edits, edit)
	}
	return edits, nil
}

// detailf is a thin fmt.Sprintf alias keeping the gate detail call sites terse.
func detailf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// prReviewSectionSet returns a predicate over the optional sections honouring
// a comma-separated `sections` filter. An empty filter enables everything.
func prReviewSectionSet(arg string) func(string) bool {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return func(string) bool { return true }
	}
	want := map[string]bool{}
	for _, s := range strings.Split(arg, ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	return func(name string) bool { return want[name] }
}

// prReviewImpactGate maps the composite blast-radius risk to a gate status:
// CRITICAL → BLOCK, HIGH → WARN, else PASS.
func prReviewImpactGate(risk analysis.RiskLevel) reviewGate {
	g := reviewGate{Name: "impact", Status: prReviewPass, Detail: "blast radius " + string(risk)}
	switch risk {
	case analysis.RiskCritical:
		g.Status = prReviewBlock
	case analysis.RiskHigh:
		g.Status = prReviewWarn
	}
	return g
}

// prReviewContractGate flags breaking contract drift as BLOCK, warnings as
// WARN.
func prReviewContractGate(c *contractImpact) reviewGate {
	g := reviewGate{Name: "contracts", Status: prReviewPass, Detail: "no contract-boundary impact"}
	if c == nil {
		return g
	}
	switch {
	case c.Breaking > 0:
		g.Status = prReviewBlock
		g.Detail = detailf("%d breaking contract change(s)", c.Breaking)
	case c.Warning > 0:
		g.Status = prReviewWarn
		g.Detail = detailf("%d contract warning(s)", c.Warning)
	default:
		g.Detail = detailf("%d affected contract(s)", len(c.Affected))
	}
	return g
}

// prReviewVerifyGate flags any contract violation (broken caller / implementor)
// as BLOCK.
func prReviewVerifyGate(r *analysis.VerifyResult) reviewGate {
	g := reviewGate{Name: "verify_change", Status: prReviewPass}
	if r == nil || r.Clean || len(r.Violations) == 0 {
		g.Detail = "no broken callers or implementors"
		return g
	}
	g.Status = prReviewBlock
	g.Detail = detailf("%d broken caller(s) / implementor(s)", len(r.Violations))
	return g
}

// prReviewGuardGate flags any guard / architecture violation as BLOCK.
func prReviewGuardGate(v []analysis.GuardViolation) reviewGate {
	g := reviewGate{Name: "guards", Status: prReviewPass, Detail: "no guard violations"}
	if len(v) > 0 {
		g.Status = prReviewBlock
		g.Detail = detailf("%d guard / architecture violation(s)", len(v))
	}
	return g
}

// prReviewAuditGate flags agent-config drift (stale refs / dead paths) as
// WARN — drift is advisory, never a merge blocker.
func prReviewAuditGate(r *audit.Report) reviewGate {
	g := reviewGate{Name: "audit_agent_config", Status: prReviewPass, Detail: "no agent-config drift"}
	if r == nil {
		g.Detail = "no agent-config files found"
		return g
	}
	drift := len(r.StaleRefs) + len(r.DeadPaths)
	if drift > 0 {
		g.Status = prReviewWarn
		g.Detail = detailf("%d stale ref(s) / dead path(s) across %d file(s)", drift, r.FilesScanned)
	}
	return g
}

// worstVerdict folds the gate statuses into the overall verdict: any BLOCK →
// BLOCK, else any WARN → WARN, else PASS.
func worstVerdict(gates []reviewGate) string {
	verdict := prReviewPass
	for _, g := range gates {
		switch g.Status {
		case prReviewBlock:
			return prReviewBlock
		case prReviewWarn:
			verdict = prReviewWarn
		}
	}
	return verdict
}

// prReviewContextPayload renders the envelope to a map so the JSON / TOON
// path and the GCX encoder share one field vocabulary. The struct already
// JSON-marshals cleanly; the map indirection keeps the budget degradation
// path (respondJSONOrTOON) able to trim the longest list.
func prReviewContextPayload(out prReviewContext) map[string]any {
	m := map[string]any{
		"verdict":         out.Verdict,
		"gates":           out.Gates,
		"changed_files":   out.ChangedFiles,
		"changed_symbols": out.ChangedSymbols,
	}
	if out.DiffContext != nil {
		m["diff_context"] = out.DiffContext
	}
	if out.Impact != nil {
		m["impact"] = out.Impact
	}
	if out.Contracts != nil {
		m["contracts"] = out.Contracts
	}
	if out.Verify != nil {
		m["verify"] = out.Verify
	}
	if out.Guards != nil {
		m["guards"] = out.Guards
	}
	if out.Simulation != nil {
		m["simulation"] = out.Simulation
	}
	if out.ConfigAudit != nil {
		m["config_audit"] = out.ConfigAudit
	}
	return m
}
