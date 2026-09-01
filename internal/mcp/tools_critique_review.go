package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/review"
)

// registerCritiqueReviewTool wires critique_review — a second, adversarial LLM
// pass over a PRIOR review's findings. Given the findings of an earlier review /
// review_pack call (or a re-run of the deterministic review over the changeset)
// plus the changeset diff as grounding, it asks the model which findings are
// false positives or low-value, drops those, and returns the kept set, the
// dropped findings each annotated with the reason it was removed, and a revised
// worst-of verdict over what survived.
//
// The pass is conservative by construction: it keeps a finding unless the model
// explicitly and confidently drops it, so a disabled / failing / unparseable LLM
// is a no-op pass-through (every finding kept). Deferred: it lands in the lazy
// catalog unless lazy tools are disabled.
func (s *Server) registerCritiqueReviewTool() {
	s.addTool(
		mcp.NewTool("critique_review",
			mcp.WithDescription("Run a second, adversarial self-critique pass over a PRIOR review's findings to catch false positives and low-value noise. Takes the findings from an earlier `review` / `review_pack` call as a JSON array (the `findings` argument) — or, when omitted, re-runs the deterministic review rulepack over the changeset (`base`/`scope`/`diff`) to produce them — then asks the configured LLM, grounded in the changeset diff, which findings are genuine versus false positives or too speculative to act on. Returns the kept findings (the filtered set), the dropped findings each with the reason it was removed, the count of uncertain-but-kept findings, and a revised BLOCK/REVIEW/APPROVE verdict recomputed over the kept set. Conservative: a finding is kept unless the model explicitly drops it, so a disabled or failing LLM is a no-op pass-through that keeps everything. Requires a configured LLM provider; without one a structured 'llm not configured' result is returned."),
			mcp.WithString("findings", mcp.Description("JSON array of prior review findings to critique (from a `review` / `review_pack` call). When omitted, the deterministic review rulepack runs over the changeset to produce findings.")),
			mcp.WithString("prior_review", mcp.Description("Alias for `findings`: a JSON array of prior findings. Accepted for parity with callers that name the prior review explicitly.")),
			mcp.WithString("base", mcp.Description("Base git ref (e.g. main) selecting the changeset when deriving findings or grounding the critique. Alias for scope=compare + base_ref=base.")),
			mcp.WithString("base_ref", mcp.Description("Base ref for scope=compare (default: main).")),
			mcp.WithString("scope", mcp.Description("Changeset scope: unstaged (default), staged, all, or compare. Ignored when `base` or `diff` is set.")),
			mcp.WithString("diff", mcp.Description("Raw unified-diff text used to ground the critique (and to derive findings off-disk when `findings` is omitted). When set, no git command runs and `scope`/`base` are ignored.")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithNumber("max_tokens", mcp.Description("Cap the critique LLM request at approximately this many tokens (default 1500).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleCritiqueReview,
	)
}

// critiqueReviewResult is the wire shape of a critique_review call: the kept
// findings, the dropped findings each with its critique reason, the count of
// uncertain-but-kept findings, the revised verdict, a one-line summary, and the
// elapsed LLM time. LLMUsed reports whether the model actually adjudicated (vs.
// the no-op pass-through).
type critiqueReviewResult struct {
	Verdict   string                    `json:"verdict"`
	Summary   string                    `json:"summary"`
	Kept      []review.Finding          `json:"kept"`
	Dropped   []review.CritiquedFinding `json:"dropped"`
	Uncertain int                       `json:"uncertain"`
	KeptCount int                       `json:"kept_count"`
	Total     int                       `json:"total"`
	LLMUsed   bool                      `json:"llm_used"`
	ElapsedMs int64                     `json:"elapsed_ms"`
}

// handleCritiqueReview resolves the prior findings (explicit JSON or a re-run of
// the deterministic review), runs the self-critique pass grounded in the
// changeset diff, and returns the kept / dropped envelope with a revised verdict.
func (s *Server) handleCritiqueReview(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	gen := s.critiqueLLMGen()
	if gen == nil {
		// Structured 'llm not configured' result (not a Go error), mirroring the
		// ask tool's degradation: the caller can branch on llm_used / verdict.
		return s.respondJSONOrTOON(ctx, req, critiqueReviewResult{
			Summary:   "llm not configured — critique skipped; no findings dropped",
			Verdict:   string(review.VerdictApprove),
			Kept:      []review.Finding{},
			Dropped:   []review.CritiquedFinding{},
			KeptCount: 0,
			LLMUsed:   false,
		})
	}

	repoRoot := s.prReviewRepoRoot(ctx, req)
	diffText := strings.TrimSpace(req.GetString("diff", ""))

	findings, err := s.critiqueFindingsFor(ctx, req, repoRoot)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Ground the critique on the changeset diff. An explicit pasted diff is used
	// as-is; otherwise read the changeset's raw diff (best-effort — a failure
	// just means the prompt carries no diff grounding, never an error).
	grounding := diffText
	if grounding == "" && repoRoot != "" {
		scope, baseRef := siblingDiffScope(req)
		if raw, derr := s.changesetRawDiff(ctx, repoRoot, scope, baseRef); derr == nil {
			grounding = raw
		}
	}

	maxTokens := req.GetInt("max_tokens", 1500)

	t0 := time.Now()
	res := review.Critique(ctx, gen, findings, grounding, maxTokens)
	elapsed := time.Since(t0).Milliseconds()

	out := critiqueReviewResult{
		Verdict:   string(res.Verdict),
		Summary:   res.Summary,
		Kept:      res.Kept,
		Dropped:   res.Dropped,
		Uncertain: res.Uncertain,
		KeptCount: len(res.Kept),
		Total:     len(findings),
		LLMUsed:   res.LLMUsed,
		ElapsedMs: elapsed,
	}
	if out.Kept == nil {
		out.Kept = []review.Finding{}
	}
	if out.Dropped == nil {
		out.Dropped = []review.CritiquedFinding{}
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeCritiqueReview(out))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(critiqueReviewPayload(out))
	}
	return s.respondJSONOrTOON(ctx, req, critiqueReviewPayload(out))
}

// critiqueLLMGen returns the critique pass's LLM seam: the test-only override
// when set, else a closure over the LLM service's Generate when one is enabled,
// else nil (the handler then returns the structured 'llm not configured'
// result). Mirrors the review tool's LLM seam.
func (s *Server) critiqueLLMGen() review.LLMGen {
	if s.critiqueLLMGenOverride != nil {
		return s.critiqueLLMGenOverride()
	}
	if s.llmService == nil || !s.llmService.Enabled() {
		return nil
	}
	return func(ctx context.Context, prompt string, maxTokens int) (string, error) {
		return s.llmService.Generate(ctx, prompt, maxTokens)
	}
}

// critiqueFindingsFor resolves the findings to critique: an explicit `findings`
// (or `prior_review`) JSON array takes precedence; otherwise the deterministic
// review rulepack runs over the changeset / pasted diff and its report findings
// are used. Mirrors postReviewFindingsFor.
func (s *Server) critiqueFindingsFor(ctx context.Context, req mcp.CallToolRequest, repoRoot string) ([]review.Finding, error) {
	raw := strings.TrimSpace(req.GetString("findings", ""))
	if raw == "" {
		raw = strings.TrimSpace(req.GetString("prior_review", ""))
	}
	if raw != "" {
		var findings []review.Finding
		if err := json.Unmarshal([]byte(raw), &findings); err != nil {
			return nil, fmt.Errorf("invalid findings JSON: %v", err)
		}
		return findings, nil
	}

	diffText := strings.TrimSpace(req.GetString("diff", ""))
	scope, baseRef := siblingDiffScope(req)
	if repoRoot == "" && diffText == "" {
		return nil, errors.New("no findings supplied and no changeset to review (set `findings` or a repo / diff)")
	}

	var (
		rulepack []astquery.Match
		impact   map[string]*analysis.ImpactResult
	)
	repoPrefix := s.diffJoinPrefix(repoRoot)
	var changedFiles []string
	if diffText == "" {
		diff, err := analysis.MapGitDiff(s.graph, repoRoot, repoPrefix, scope, baseRef)
		if err != nil {
			return nil, err
		}
		allowedRepos, err := s.resolveRepoFilter(ctx, req)
		if err != nil {
			return nil, err
		}
		rulepack = s.reviewRulepackMatches(ctx, diff.ChangedFiles, analysis.RepoRelativePath, repoPrefix, allowedRepos)
		impact = s.reviewImpact(diff.ChangedSymbols)
		changedFiles = diff.ChangedFiles
	}

	suppStore, suppRepoKey := s.reviewSuppressions()
	report, err := review.Run(ctx, s.graph, nil, review.Options{
		RepoRoot:        repoRoot,
		RepoPrefix:      repoPrefix,
		CoverageKnown:   s.coverageKnownForDiff(repoPrefix, changedFiles),
		Scope:           scope,
		BaseRef:         baseRef,
		Diff:            diffText,
		RulepackMatches: rulepack,
		Impact:          impact,
		Suppressions:    suppStore,
		RepoKey:         suppRepoKey,
	})
	if err != nil {
		return nil, err
	}
	if report == nil {
		return nil, nil
	}
	return report.Findings, nil
}

// changesetRawDiff returns the raw unified diff of the whole changeset (all
// changed files) used to ground the critique prompt. It reuses the same
// scope-aware git-diff arg selection the sibling-diff path uses, with no
// pathspec so every changed file is included.
func (s *Server) changesetRawDiff(ctx context.Context, repoRoot, scope, baseRef string) (string, error) {
	// Route through gitcmd. Use the handler ctx (cancelled on session teardown);
	// bound it at 30s when it carries no deadline. Run returns raw stdout so the
	// grounding text is byte-identical to the pre-gitcmd output.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	out, err := gitcmd.Run(ctx, repoRoot, siblingDiffArgs(scope, baseRef)...)
	if err != nil {
		if len(out) == 0 {
			return "", nil
		}
		return "", err
	}
	return string(out), nil
}

// critiqueReviewPayload projects the result onto the wire map shape so the JSON /
// TOON path and the byte/token budget machinery share one field vocabulary with
// the GCX encoder.
func critiqueReviewPayload(out critiqueReviewResult) map[string]any {
	kept := make([]map[string]any, 0, len(out.Kept))
	for _, f := range out.Kept {
		kept = append(kept, findingRowMap(f))
	}
	dropped := make([]map[string]any, 0, len(out.Dropped))
	for _, d := range out.Dropped {
		row := findingRowMap(d.Finding)
		row["critique_verdict"] = string(d.Verdict)
		row["critique_reason"] = d.Reason
		dropped = append(dropped, row)
	}
	return map[string]any{
		"verdict":    out.Verdict,
		"summary":    out.Summary,
		"kept":       kept,
		"dropped":    dropped,
		"uncertain":  out.Uncertain,
		"kept_count": out.KeptCount,
		"total":      out.Total,
		"llm_used":   out.LLMUsed,
		"elapsed_ms": out.ElapsedMs,
	}
}

// findingRowMap projects a finding onto the inline-comment field vocabulary used
// across the review surface.
func findingRowMap(f review.Finding) map[string]any {
	line := f.Line
	if line == 0 {
		line = f.StartLine
	}
	row := map[string]any{
		"file":     f.File,
		"line":     line,
		"severity": string(f.Severity),
		"message":  f.Message,
		"rule":     f.Rule,
		"category": f.Category,
	}
	if f.Source != "" {
		row["source"] = f.Source
	}
	if f.IdentityKey != "" {
		row["identity_key"] = f.IdentityKey
	}
	return row
}
