package review

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/tokens"
)

// Options configures a hybrid review run. The deterministic rulepack matches are
// an INPUT — the caller runs the review analyzer and hands the matches in — so
// internal/review never imports the analyze/mcp layer. A nil graph, an empty
// scope, or a disabled LLM are all tolerated: Run is total and degrades to the
// deterministic findings.
type Options struct {
	// RepoRoot is the repository root the change is read from. May be empty for
	// the pasted-diff (off-disk) path, in which case Diff must be set.
	RepoRoot string
	// RepoPrefix is the graph repo prefix anchoring RepoRoot's indexed nodes —
	// multi-repo daemons key file paths as "<prefix>/<rel>" while git emits
	// repo-relative paths. Empty in single-repo / unprefixed mode.
	RepoPrefix string
	// CoverageKnown reports whether the graph indexes test symbols for the
	// changed languages. When false the per-file coverage evidence is
	// unknowable — an index config that excludes "**/*_test.go" would
	// otherwise make every change read as untested — so the rows carry no
	// untested counts and the headline says "coverage unknown" instead.
	//
	// It is also the only thing that licenses the headline to blame the
	// index for missing coverage. Empty rows are not evidence of an index
	// without tests; see summarize.
	CoverageKnown bool
	// Scope selects the git diff range: "staged", "all", "compare", or
	// "unstaged" (default). Ignored when Diff is set.
	Scope string
	// BaseRef is the comparison base for Scope == "compare".
	BaseRef string
	// Diff, when non-empty, is raw unified-diff text reviewed off-disk instead
	// of shelling out to git (the pasted-diff path and the test seam).
	Diff string
	// RulepackMatches are the pre-computed deterministic review-rule hits the
	// caller produced (analyze review category). They are merged into the report
	// as rulepack findings; the flow never re-runs the detectors.
	RulepackMatches []astquery.Match
	// Impact is the per-changed-symbol blast-radius analysis (symbol id →
	// result), used to rank per-file risk. Optional.
	Impact map[string]*analysis.ImpactResult
	// Rules resolves the review rule governing each changed file. Optional; when
	// nil the prompt carries no per-file rule grounding and rulepack findings
	// fall back to the match's own severity.
	Rules *RuleResolver
	// TokenBudget bounds the review pack handed to the LLM and the relocation /
	// compaction loop. <= 0 means no budget.
	TokenBudget int
	// UseLLM gates the MAIN/RELOCATE LLM phases. When false (or gen is nil) the
	// report carries only the deterministic rulepack findings.
	UseLLM bool
	// MaxLLMTokens caps the freeform generation request. Defaults to a sane
	// value when <= 0.
	MaxLLMTokens int
	// Config supplies the confidence / severity / category / cap gate applied
	// to the merged findings before the report is assembled. A zero-value
	// ReviewConfig is a pass-through — no finding is dropped — so the legacy
	// callers that leave it unset see the report they always did.
	Config config.ReviewConfig
	// Suppressions is the durable per-repo false-positive filter consulted by
	// the gate: a finding whose IdentityKey is recorded for RepoKey is silently
	// dropped from the report and counted in the gate's IdentitySuppressed stat.
	// Nil disables suppression (every finding survives the gate's suppress step).
	Suppressions *SuppressionStore
	// RepoKey scopes the suppression lookup to one repository — the same
	// per-repo key the notes / memories side-stores use. Ignored when
	// Suppressions is nil.
	RepoKey string
}

const defaultMaxLLMTokens = 2048

// Run executes the hybrid PLAN → MAIN → RELOCATE → COMPRESS review flow and
// returns a ReviewReport. It is total: a nil/disabled/garbage LLM yields a report
// with just the deterministic rule findings and a verdict, never an error. Only a
// failure to assemble the deterministic substrate (e.g. an unreadable git diff on
// the on-disk path) surfaces as an error.
func Run(ctx context.Context, g graph.Reader, gen LLMGen, opts Options) (*ReviewReport, error) {
	plan, err := planReview(g, opts)
	if err != nil {
		return nil, err
	}

	// MAIN + RELOCATE: ask the LLM for findings and ground each to an exact line.
	// Disabled or failing — the slice is simply empty.
	llmFindings, dropped, truncated := mainAndRelocate(ctx, g, gen, opts, plan)

	// COMPRESS: merge the deterministic + LLM findings, dedup, rank per-file
	// risk, and resolve the worst-of verdict.
	report := compress(opts, plan, llmFindings, dropped, truncated)
	return report, nil
}

// LLMGenWithUsage is the usage-aware variant of the LLMGen seam: it
// returns the provider's token usage for the call alongside the text.
// A flow driven through RunWithUsage threads this so the report can
// carry a real per-review CostBreakdown. Like LLMGen it is a plain func
// so internal/review stays free of an llm/svc import; a nil seam means
// the LLM tier is disabled.
type LLMGenWithUsage func(ctx context.Context, prompt string, maxTokens int) (string, llm.TokenUsage, error)

// RunWithUsage executes the same hybrid review flow as Run but threads a
// usage-aware LLM seam so the returned report carries a CostBreakdown:
// the summed token usage of every LLM call, the USD estimate from price,
// the elapsed LLM time, and per-finding GenTokens (output tokens
// distributed by finding-body weight). A nil seam — or a provider that
// reports no usage — yields a zero, Estimated:false cost block rather
// than omitting it. The plain Run path is unchanged.
func RunWithUsage(ctx context.Context, g graph.Reader, gen LLMGenWithUsage, price llm.ProviderPricing, opts Options) (*ReviewReport, error) {
	var total llm.TokenUsage
	var elapsed time.Duration
	// Adapt the usage-aware seam down to the plain LLMGen the flow uses,
	// accumulating usage + elapsed time on every call so the cost block
	// reflects the whole review (MAIN + every RELOCATE fallback call).
	var plain LLMGen
	if gen != nil {
		plain = func(ctx context.Context, prompt string, maxTokens int) (string, error) {
			t0 := time.Now()
			text, usage, err := gen(ctx, prompt, maxTokens)
			elapsed += time.Since(t0)
			total.Add(usage)
			return text, err
		}
	}

	report, err := Run(ctx, g, plain, opts)
	if err != nil {
		return nil, err
	}

	AttributeFindingTokens(report.Findings, total)
	cost := CostFromUsage(total, price, elapsed.Milliseconds())
	report.Cost = &cost
	return report, nil
}

// reviewPlan is the deterministic substrate the PLAN phase assembles: the change
// view, the diff→symbol map, the tiered review pack, the per-file resolved rules,
// and the deterministic rule findings carried in by the caller.
type reviewPlan struct {
	view        *ChangeView
	diff        *analysis.DiffResult
	pack        *ReviewPack
	rules       map[string]config.ReviewRule
	ruleFinds   []Finding
	changedFile map[string]bool
	// depth is the adaptive review depth this changeset classified into. It
	// gates whether the LLM MAIN phase runs at all (quick skips it) and whether
	// the prompt carries the reference-only planner catalogue (deep only).
	depth Depth
}

// planReview is the PLAN phase: it decides what to review by assembling the
// deterministic grounding once — the change view (from pasted diff or git), the
// diff→symbol map, the tiered review pack, the per-file rule resolution, and the
// deterministic rule findings translated from the caller's matches.
func planReview(g graph.Reader, opts Options) (*reviewPlan, error) {
	view, diff, err := buildSubstrate(g, opts)
	if err != nil {
		return nil, err
	}

	changedFiles := map[string]bool{}
	if diff != nil {
		for _, f := range diff.ChangedFiles {
			changedFiles[cleanPath(f)] = true
		}
		for _, cs := range diff.ChangedSymbols {
			changedFiles[cleanPath(cs.FilePath)] = true
		}
	}
	if view != nil {
		for f := range view.ByFile {
			changedFiles[cleanPath(f)] = true
		}
	}

	rules := resolveRules(opts.Rules, changedFiles)
	ruleFinds := ruleFindings(opts.RulepackMatches, opts.Rules)

	// The pack is built from a flat impact over the changed symbols so the
	// caller's per-symbol impact map (used for risk ranking) need not be a
	// single ImpactResult. A nil pack still renders an empty section.
	pack := BuildReviewPack(g, view, diff, mergedImpact(opts.Impact), opts.TokenBudget)

	// Classify the adaptive review depth from the change size. A zero-value
	// ReviewConfig falls back to the built-in default thresholds, so a caller
	// that configures none gets the default ladder.
	depth := ClassifyDepth(ChangedLinesFromDiff(diff), ChangedFilesFromDiff(diff), opts.Config)

	return &reviewPlan{
		view:        view,
		diff:        diff,
		pack:        pack,
		rules:       rules,
		ruleFinds:   ruleFinds,
		changedFile: changedFiles,
		depth:       depth,
	}, nil
}

// buildSubstrate produces the change view and diff→symbol map for the run,
// preferring the pasted-diff text when present (off-disk / test path) and
// otherwise shelling out via the landed git substrate.
func buildSubstrate(g graph.Reader, opts Options) (*ChangeView, *analysis.DiffResult, error) {
	if strings.TrimSpace(opts.Diff) != "" {
		view := ChangeViewFromDiff(opts.RepoRoot, opts.Diff)
		// A pasted diff has no graph-backed symbol map; synthesize a minimal
		// DiffResult from the changed file set so the pack and risk ranking have
		// something to work with.
		return view, diffFromView(view), nil
	}

	view, err := BuildChangeView(g, opts.RepoRoot, opts.RepoPrefix, opts.Scope, opts.BaseRef)
	if err != nil {
		return nil, nil, err
	}
	diff, err := analysis.MapGitDiff(g, opts.RepoRoot, opts.RepoPrefix, opts.Scope, opts.BaseRef)
	if err != nil {
		return nil, nil, err
	}
	return view, diff, nil
}

// diffFromView synthesizes a DiffResult from a pasted-diff ChangeView so the
// pack/risk machinery has a changed-file set even without the graph, and so the
// adaptive-depth classifier can size the change. The synthesized hunks span the
// contiguous runs of added new-side lines per file — enough for the
// changed-line count even though the graph-backed symbol map is absent.
func diffFromView(view *ChangeView) *analysis.DiffResult {
	if view == nil {
		return nil
	}
	files := make([]string, 0, len(view.ByFile))
	for f := range view.ByFile {
		files = append(files, f)
	}
	sort.Strings(files)

	var hunks []analysis.DiffHunk
	for _, f := range files {
		hunks = append(hunks, addedLineHunks(f, view.ByFile[f])...)
	}
	return &analysis.DiffResult{ChangedFiles: files, Hunks: hunks}
}

// addedLineHunks groups a file change's added new-side lines into contiguous
// hunks (by new-side line number) so the synthesized DiffResult carries a
// faithful changed-line span for the depth classifier.
func addedLineHunks(file string, fc *FileChange) []analysis.DiffHunk {
	if fc == nil {
		return nil
	}
	added := make([]int, 0, len(fc.Lines))
	for _, l := range fc.Lines {
		if l.Added && l.NewLine > 0 {
			added = append(added, l.NewLine)
		}
	}
	if len(added) == 0 {
		return nil
	}
	sort.Ints(added)
	var hunks []analysis.DiffHunk
	start, prev := added[0], added[0]
	for _, n := range added[1:] {
		if n == prev+1 {
			prev = n
			continue
		}
		hunks = append(hunks, analysis.DiffHunk{FilePath: file, StartLine: start, EndLine: prev})
		start, prev = n, n
	}
	hunks = append(hunks, analysis.DiffHunk{FilePath: file, StartLine: start, EndLine: prev})
	return hunks
}

// resolveRules grounds each changed file to its governing review rule via the
// resolver. Files with no resolver, or that resolve to nothing, are omitted.
func resolveRules(resolver *RuleResolver, changedFiles map[string]bool) map[string]config.ReviewRule {
	out := map[string]config.ReviewRule{}
	if resolver == nil {
		return out
	}
	for file := range changedFiles {
		if rule, ok := resolver.RuleFor(file); ok {
			out[file] = rule
		}
	}
	return out
}

// ruleFindings translates the caller's deterministic rule matches into findings,
// applying the per-file rule's severity floor and rule label.
func ruleFindings(matches []astquery.Match, resolver *RuleResolver) []Finding {
	out := make([]Finding, 0, len(matches))
	for _, m := range matches {
		rule := config.ReviewRule{}
		if resolver != nil {
			if r, ok := resolver.RuleFor(cleanPath(m.File)); ok {
				rule = r
			}
		}
		out = append(out, ruleFindingFromMatch(m, rule))
	}
	return out
}

// mainAndRelocate is the MAIN + RELOCATE phases. MAIN builds the rule-grounded
// prompt and asks the LLM for free-text findings; RELOCATE grounds each to an
// exact line via the deterministic tiers plus the LLM fallback, dropping any that
// stay unresolved. Returns the anchored findings, the dropped count, and whether
// a token bound trimmed candidates.
func mainAndRelocate(ctx context.Context, g graph.Reader, gen LLMGen, opts Options, plan *reviewPlan) (findings []Finding, dropped int, truncated bool) {
	if !opts.UseLLM || gen == nil {
		return nil, 0, false
	}
	// Adaptive depth gating: when the caller opted into depth thresholds, a
	// quick changeset skips the LLM MAIN phase entirely — the deterministic
	// rules alone review it, no model call. Standard and deep both run the LLM
	// pass; only deep grounds the prompt in the reference-only planner
	// catalogue. With no thresholds configured the gate is inert and the flow
	// runs its pre-adaptive single pass.
	if depthConfigured(opts.Config) && plan.depth == DepthQuick {
		return nil, 0, false
	}

	prompt := buildReviewPrompt(promptInput{
		Rules:         plan.rules,
		Pack:          plan.pack,
		Deterministic: plan.ruleFinds,
		Deep:          plan.depth == DepthDeep,
	})

	maxTok := opts.MaxLLMTokens
	if maxTok <= 0 {
		maxTok = defaultMaxLLMTokens
	}
	out, err := gen(ctx, prompt, maxTok)
	if err != nil {
		// A failing LLM is not an error for the flow — degrade to deterministic.
		return nil, 0, false
	}

	cands := parseCandidates(out)
	cands, truncated = boundCandidates(cands, opts.TokenBudget)

	for _, c := range cands {
		rule := ruleForFile(opts.Rules, c.File)
		f := candidateToFinding(c, rule)
		anchor := LocateFinding(ctx, gen, plan.view, &f, c.Snippet)
		if !anchor.Located() {
			dropped++
			continue
		}
		findings = append(findings, f)
	}
	return findings, dropped, truncated
}

// compress is the COMPRESS phase: it merges the deterministic rule findings with
// the relocated LLM findings, deduplicates overlapping findings (same file+line+
// category), ranks per-file risk, computes the worst-of verdict, and assembles
// the report with its statistics. The dedup + budget bound is the seam a later
// rolling-summary compaction hangs off; it is a real bound today, not a stub.
func compress(opts Options, plan *reviewPlan, llmFindings []Finding, dropped int, truncated bool) *ReviewReport {
	merged := make([]Finding, 0, len(plan.ruleFinds)+len(llmFindings))
	merged = append(merged, plan.ruleFinds...)
	merged = append(merged, llmFindings...)

	merged = dedupeFindings(merged)
	sortFindings(merged)

	// Apply the confidence / severity / category / cap gate, plus the durable
	// per-repo false-positive suppression filter. A zero-value Config + nil
	// suppression store yields a pass-through gate so the legacy report is
	// unchanged.
	merged, gateStats := NewGate(opts.Config).
		WithSuppression(opts.Suppressions, opts.RepoKey).
		Apply(merged)

	fileRisk := rankFileRisk(plan.diff, opts.Impact, merged, opts.RepoPrefix, opts.CoverageKnown)
	verdict := computeVerdict(merged, fileRisk)

	bySeverity := map[string]int{}
	for _, f := range merged {
		bySeverity[string(f.Severity)]++
	}

	return &ReviewReport{
		Verdict:  verdict,
		Findings: merged,
		FileRisk: fileRisk,
		Summary:  summarize(verdict, merged, fileRisk, opts.CoverageKnown),
		Depth:    plan.depth.String(),
		Stats: ReviewStats{
			Rulepack:     len(plan.ruleFinds),
			LLM:          len(llmFindings),
			Dropped:      dropped,
			Total:        len(merged),
			BySeverity:   bySeverity,
			Truncated:    truncated || (plan.pack != nil && plan.pack.Truncated),
			LLMRequested: opts.UseLLM,
			Gate:         gateStats,
			Depth:        plan.depth.String(),
		},
	}
}

// dedupeFindings drops later findings that overlap an earlier one on the same
// (file, line, category). The deterministic rule findings are appended first, so
// a duplicate LLM finding is the one dropped — the precise rulepack location wins.
func dedupeFindings(findings []Finding) []Finding {
	seen := map[string]bool{}
	out := findings[:0:0]
	for _, f := range findings {
		key := f.File + "|" + itoaInt(f.Line) + "|" + strings.ToLower(strings.TrimSpace(f.Category))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

// sortFindings orders findings worst-severity-first, then by file and line, for a
// deterministic report.
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		si, sj := severityRank(findings[i].Severity), severityRank(findings[j].Severity)
		if si != sj {
			return si > sj
		}
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
}

// summarize produces the one-line report headline. With no findings a
// non-APPROVE verdict is risk-driven (computeVerdict escalates on per-file
// blast radius, tempered by test coverage), so the headline says which tier
// earned it — and whether the risk is untested — instead of the
// self-contradictory "BLOCK: no findings".
//
// coverageKnown is the caller's attestation that the index carries test
// symbols for the changed languages, and it is the ONLY thing that licenses
// blaming the index. The rows alone cannot: FileRisk.Symbols is zero both
// when coverage was unattestable and when the changeset simply mapped to no
// graph symbols to evaluate (a pasted diff, a brand-new file), and inferring
// the first from the second made the headline assert "the index carries no
// test symbols" over a graph full of them.
func summarize(verdict Verdict, findings []Finding, fileRisk []FileRisk, coverageKnown bool) string {
	if len(findings) == 0 {
		if verdict != VerdictApprove && len(fileRisk) > 0 {
			worst := fileRisk[0].Risk // sorted worst-first
			n, untested, evaluated := 0, 0, false
			for _, fr := range fileRisk {
				if fr.Symbols > 0 {
					evaluated = true
				}
				if fr.Risk != worst {
					continue
				}
				n++
				if fr.Symbols > 0 && fr.Uncovered > 0 {
					untested++
				}
			}
			switch {
			case untested > 0:
				return fmt.Sprintf("%s: no rule findings, but %d of %d changed file(s) carry %s blast-radius risk (%d without covering tests)",
					verdict, n, len(fileRisk), worst, untested)
			case evaluated:
				return fmt.Sprintf("%s: no rule findings; %d of %d changed file(s) carry %s blast-radius risk, all test-covered",
					verdict, n, len(fileRisk), worst)
			case !coverageKnown:
				return fmt.Sprintf("%s: no rule findings, but %d of %d changed file(s) carry %s blast-radius risk (test coverage unknown — the index carries no test symbols for the changed language(s))",
					verdict, n, len(fileRisk), worst)
			default:
				return fmt.Sprintf("%s: no rule findings, but %d of %d changed file(s) carry %s blast-radius risk (coverage not evaluated — no changed symbol mapped to the graph)",
					verdict, n, len(fileRisk), worst)
			}
		}
		return fmt.Sprintf("%s: no findings across %d changed file(s)", verdict, len(fileRisk))
	}
	return fmt.Sprintf("%s: %d finding(s) across %d changed file(s)", verdict, len(findings), len(fileRisk))
}

// boundCandidates trims LLM candidates so their combined snippet+message token
// cost stays within the token budget. This is the COMPRESS-phase bound that keeps
// the relocation loop within budget; it returns the kept candidates and whether
// any were trimmed. A budget <= 0 means no bound.
func boundCandidates(cands []reviewCandidate, tokenBudget int) ([]reviewCandidate, bool) {
	if tokenBudget <= 0 || len(cands) == 0 {
		return cands, false
	}
	kept := cands[:0:0]
	used := 0
	truncated := false
	for _, c := range cands {
		cost := tokens.Count(c.Snippet) + tokens.Count(c.Message)
		if used+cost > tokenBudget && len(kept) > 0 {
			truncated = true
			continue
		}
		kept = append(kept, c)
		used += cost
	}
	return kept, truncated
}

// ruleForFile resolves the review rule for a file, returning a zero rule when no
// resolver or no match.
func ruleForFile(resolver *RuleResolver, file string) config.ReviewRule {
	if resolver == nil {
		return config.ReviewRule{}
	}
	if r, ok := resolver.RuleFor(cleanPath(file)); ok {
		return r
	}
	return config.ReviewRule{}
}

// MergeImpact folds a per-symbol impact map into a single ImpactResult for the
// review pack's caller/outline tiers — the exported entry point the packaged
// review layer uses when it already holds the per-symbol map.
func MergeImpact(impact map[string]*analysis.ImpactResult) *analysis.ImpactResult {
	return mergedImpact(impact)
}

// mergedImpact folds the caller's per-symbol impact map into a single
// ImpactResult for the review pack's caller/outline tiers. The per-symbol map is
// retained separately for per-file risk ranking. A nil/empty map yields nil so
// the pack carries no caller tier.
func mergedImpact(impact map[string]*analysis.ImpactResult) *analysis.ImpactResult {
	if len(impact) == 0 {
		return nil
	}
	out := &analysis.ImpactResult{ByDepth: map[int][]analysis.ImpactEntry{}}
	seen := map[string]bool{}
	for _, ir := range impact {
		if ir == nil {
			continue
		}
		for depth, entries := range ir.ByDepth {
			for _, e := range entries {
				key := fmt.Sprintf("%d|%s", depth, e.ID)
				if seen[key] {
					continue
				}
				seen[key] = true
				out.ByDepth[depth] = append(out.ByDepth[depth], e)
			}
		}
	}
	return out
}

func itoaInt(n int) string {
	return fmt.Sprintf("%d", n)
}
