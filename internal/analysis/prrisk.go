package analysis

import (
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// PRRiskInput is the forge-agnostic input to ScorePRRisk: the already-mapped
// set of changed symbol IDs plus the changed file paths, with optional
// pre-computed community/process context. The scorer never reaches a forge —
// callers map a diff to symbols first (e.g. via MapGitDiff) and hand the
// result in here.
type PRRiskInput struct {
	SymbolIDs    []string
	ChangedFiles []string
	NodeToComm   map[string]string
	Communities  *CommunityResult
	Processes    *ProcessResult
}

// PRRiskFactor is one scored axis of the composite, carrying a short
// human-readable reason. The ordered slice of factors doubles as the
// review-priority list a reviewer should work top-down.
type PRRiskFactor struct {
	Axis   string  `json:"axis"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// PRRiskResult is the output of ScorePRRisk: a 0-100 composite with a coarse
// RiskLevel, the per-axis factors (sorted highest-first), and the supporting
// counts that fed the axes.
type PRRiskResult struct {
	Score            float64        `json:"score"`
	Risk             RiskLevel      `json:"risk"`
	Factors          []PRRiskFactor `json:"factors"`
	TotalAffected    int            `json:"total_affected"`
	UncoveredSymbols int            `json:"uncovered_symbols"`
	CommunitySpan    int            `json:"community_span"`
	SecurityHits     []string       `json:"security_hits"`
}

// prRisk axis weights. Centrality-like axes (blast-radius flow and the raw
// caller fan-in) carry the most weight; coverage and security are mid-weight
// review-priority signals; the community-span axis is the lightest, mirroring
// the composite-impact weighting convention.
const (
	prWeightFlow      = 2.5
	prWeightCallers   = 2.5
	prWeightCoverage  = 1.5
	prWeightSecurity  = 1.5
	prWeightCommunity = 1.0
)

// prRisk half-saturation points. Each raw count is mapped onto 0..100 with
// prSaturate, where the named constant is the value that yields 50.
const (
	prFlowK   = 12.0
	prCallerK = 8.0
	prCommK   = 3.0
)

// securityFloor is the minimum composite a PR with any security-keyword hit
// is held to — the HIGH threshold, so a security-sensitive change always
// reaches a reviewer regardless of its blast radius.
const securityFloor = 55.0

// securityKeywords is the curated set matched against changed file paths and
// changed symbol names. A hit on any of these is a strong "a human must look
// here" signal independent of blast radius — auth/crypto/secret-handling code
// is where a subtle change does the most damage.
var securityKeywords = []string{
	"auth",
	"login",
	"password",
	"passwd",
	"secret",
	"token",
	"credential",
	"crypto",
	"cipher",
	"encrypt",
	"decrypt",
	"hash",
	"jwt",
	"oauth",
	"session",
	"permission",
	"privilege",
	"sudo",
	"admin",
	"acl",
	"rbac",
	"signature",
	"tls",
	"certificate",
	"keystore",
	"vault",
}

// ScorePRRisk computes a PR-level composite risk score over a set of changed
// symbols. Five 0-100 axes are blended into a weighted normalized composite
// and bucketed into a RiskLevel via the same thresholds the composite-impact
// scorer uses. The ordered Factors slice doubles as a review-priority list.
func ScorePRRisk(g graph.Reader, in PRRiskInput) PRRiskResult {
	ids := dedupStrings(in.SymbolIDs)

	// Axis (a): blast-radius flow. AnalyzeImpact gives the total affected
	// count and the d=1 direct-dependent count; both feed the flow score,
	// floored by the structural assessRisk ladder so a heavily-depended-on
	// change can never read as low-flow.
	impact := AnalyzeImpact(g, ids, in.Communities, in.Processes)
	d1 := len(impact.ByDepth[1])
	flowRaw := float64(impact.TotalAffected) + float64(d1)
	flowScore := prSaturate(flowRaw, prFlowK)
	if floor := riskFloorScore(assessRisk(d1, len(impact.ByDepth[2]))); floor > flowScore {
		flowScore = floor
	}

	// Axis (b): community span. A change touching many communities is a
	// cross-cutting change with a wider review surface.
	commSet := make(map[string]bool)
	for _, id := range ids {
		if cid, ok := in.NodeToComm[id]; ok && cid != "" {
			commSet[cid] = true
		}
	}
	communitySpan := len(commSet)
	communityScore := prSaturate(float64(communitySpan), prCommK)

	// Axis (c): coverage gap. The ratio of changed symbols with NO covering
	// test, found via the EdgeTests inverse walk (a test→symbol edge pointing
	// at the changed symbol). Only code-bearing symbols are counted so a
	// changed import/type-alias does not dilute the ratio.
	covered, uncovered := 0, 0
	for _, id := range ids {
		n := g.GetNode(id)
		if n == nil || !isCodeSymbol(n.Kind) {
			continue
		}
		if hasCoveringTest(g, id) {
			covered++
		} else {
			uncovered++
		}
	}
	coverageScore := 0.0
	considered := covered + uncovered
	if considered > 0 {
		coverageScore = 100 * float64(uncovered) / float64(considered)
	}

	// Axis (d): security keyword. A hit on the curated set over file paths or
	// symbol names is a hard "human must review" axis; presence alone scores
	// high so the composite cannot dilute it away.
	hits := securityKeywordHits(in.ChangedFiles, symbolNames(g, ids))
	securityScore := 0.0
	if len(hits) > 0 {
		securityScore = 70.0 + prSaturate(float64(len(hits)-1), 2.0)*0.3
		if securityScore > 100 {
			securityScore = 100
		}
	}

	// Axis (e): caller fan-in. The single most-called changed symbol drives
	// this axis — a change to a hub is riskier than the blast-radius average.
	maxFanIn := 0
	for _, id := range ids {
		if fi := callerFanIn(g, id); fi > maxFanIn {
			maxFanIn = fi
		}
	}
	callerScore := prSaturate(float64(maxFanIn), prCallerK)

	// Weighted normalized composite.
	composite := (flowScore*prWeightFlow +
		callerScore*prWeightCallers +
		coverageScore*prWeightCoverage +
		securityScore*prWeightSecurity +
		communityScore*prWeightCommunity) /
		(prWeightFlow + prWeightCallers + prWeightCoverage + prWeightSecurity + prWeightCommunity)

	// Security is a hard "a human must look here" axis: a hit on the curated
	// set floors the composite at HIGH so the weighted blend can never dilute
	// a security-sensitive change down to a low-priority review — the same
	// risk-floor convention the change-impact tool uses for contract boundaries.
	if len(hits) > 0 && composite < securityFloor {
		composite = securityFloor
	}

	factors := []PRRiskFactor{
		{Axis: "flow", Score: roundPR(flowScore), Reason: flowReason(impact.TotalAffected, d1)},
		{Axis: "callers", Score: roundPR(callerScore), Reason: callerReason(maxFanIn)},
		{Axis: "coverage", Score: roundPR(coverageScore), Reason: coverageReason(uncovered, considered)},
		{Axis: "security", Score: roundPR(securityScore), Reason: securityReason(hits)},
		{Axis: "community", Score: roundPR(communityScore), Reason: communityReason(communitySpan)},
	}
	// review_priorities — highest axis first; ties broken by axis name so the
	// ordering is deterministic.
	sort.SliceStable(factors, func(i, j int) bool {
		if factors[i].Score != factors[j].Score {
			return factors[i].Score > factors[j].Score
		}
		return factors[i].Axis < factors[j].Axis
	})

	return PRRiskResult{
		Score:            roundPR(composite),
		Risk:             prRiskLevel(composite),
		Factors:          factors,
		TotalAffected:    impact.TotalAffected,
		UncoveredSymbols: uncovered,
		CommunitySpan:    communitySpan,
		SecurityHits:     hits,
	}
}

// securityKeywordHits returns the distinct curated security keywords that
// appear (case-insensitive) in any changed file path or changed symbol name,
// in curated-set order so the output is stable.
func securityKeywordHits(files []string, names []string) []string {
	hay := make([]string, 0, len(files)+len(names))
	for _, f := range files {
		hay = append(hay, strings.ToLower(f))
	}
	for _, n := range names {
		hay = append(hay, strings.ToLower(n))
	}
	var out []string
	for _, kw := range securityKeywords {
		for _, h := range hay {
			if strings.Contains(h, kw) {
				out = append(out, kw)
				break
			}
		}
	}
	return out
}

// hasCoveringTest reports whether any inbound EdgeTests edge points at the
// symbol — the same inverse-edge walk buildBlastRadius uses to find covering
// tests, run at the graph-store level (one hop, no BFS).
func hasCoveringTest(g graph.Reader, symbolID string) bool {
	for _, e := range g.GetInEdges(symbolID) {
		if e.Kind == graph.EdgeTests {
			return true
		}
	}
	return false
}

// callerFanIn counts the distinct callers of a symbol — inbound calls/
// references edges, deduped by source node. A reference edge is included so a
// symbol used as a value (not only called) still registers fan-in.
func callerFanIn(g graph.Reader, symbolID string) int {
	seen := make(map[string]bool)
	for _, e := range g.GetInEdges(symbolID) {
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
			continue
		}
		if e.From == "" || e.From == symbolID {
			continue
		}
		seen[e.From] = true
	}
	return len(seen)
}

// symbolNames resolves the changed symbol IDs to their node names for the
// security-keyword match. Unknown IDs fall back to the trailing segment of the
// ID so a name still feeds the matcher even when the node is not in the graph.
func symbolNames(g graph.Reader, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n := g.GetNode(id); n != nil && n.Name != "" {
			out = append(out, n.Name)
			continue
		}
		if idx := strings.LastIndex(id, "::"); idx >= 0 && idx+2 < len(id) {
			out = append(out, id[idx+2:])
		} else {
			out = append(out, id)
		}
	}
	return out
}

// isCodeSymbol reports whether a node kind is a code-bearing symbol worth
// holding to a coverage standard (function/method/type). Imports, fields, and
// other structural nodes are excluded from the coverage ratio.
func isCodeSymbol(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType:
		return true
	default:
		return false
	}
}

// prSaturate maps a non-negative raw value onto 0..100 with a half-saturation
// point of k: x==k yields 50, x==3k yields 75.
func prSaturate(x, k float64) float64 {
	if x <= 0 || k <= 0 {
		return 0
	}
	return 100 * x / (x + k)
}

// riskFloorScore turns a structural RiskLevel into a 0..100 floor so the flow
// axis can never read below what the assessRisk ladder already established.
func riskFloorScore(r RiskLevel) float64 {
	switch r {
	case RiskCritical:
		return 80
	case RiskHigh:
		return 60
	case RiskMedium:
		return 40
	default:
		return 0
	}
}

// prRiskLevel buckets the composite into a RiskLevel using the same
// thresholds as the composite-impact label (>=75 CRITICAL / >=55 HIGH /
// >=35 MEDIUM).
func prRiskLevel(score float64) RiskLevel {
	switch {
	case score >= 75:
		return RiskCritical
	case score >= 55:
		return RiskHigh
	case score >= 35:
		return RiskMedium
	default:
		return RiskLow
	}
}

func flowReason(total, d1 int) string {
	if total == 0 {
		return "no detected dependents"
	}
	return formatCount(d1, "direct dependent", "direct dependents") + ", " +
		formatCount(total, "symbol affected", "symbols affected")
}

func callerReason(maxFanIn int) string {
	if maxFanIn == 0 {
		return "no callers on any changed symbol"
	}
	return "widest changed symbol has " + formatCount(maxFanIn, "caller", "callers")
}

func coverageReason(uncovered, considered int) string {
	if considered == 0 {
		return "no code symbols to cover"
	}
	if uncovered == 0 {
		return "all changed symbols have covering tests"
	}
	return formatCount(uncovered, "changed symbol", "changed symbols") + " with no covering test"
}

func securityReason(hits []string) string {
	if len(hits) == 0 {
		return "no security-sensitive paths or names"
	}
	return "changed path/name matches " + strings.Join(hits, ", ")
}

func communityReason(span int) string {
	if span <= 1 {
		return "change is community-local"
	}
	return "change spans " + formatCount(span, "community", "communities")
}

func formatCount(n int, singular, plural string) string {
	noun := plural
	if n == 1 {
		noun = singular
	}
	return strconv.Itoa(n) + " " + noun
}

// dedupStrings returns the input slice with empty and duplicate entries
// removed, preserving first-seen order.
func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// roundPR rounds a score to two decimals so the wire payload is stable.
func roundPR(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// receiptVersion is the on-the-wire schema version of a ReviewReceipt. Bump it
// only on a breaking field change so a consumer can refuse an unknown shape.
const receiptVersion = 1

// receiptSpanSplit is the community-span count at or above which a change is
// considered cross-cutting enough that splitting the PR is the next safe
// action (when nothing more urgent applies).
const receiptSpanSplit = 3

// next_safe_action vocabulary. A small, fixed enum so the receipt carries a
// machine-actionable verdict with no free-form text that could leak context.
const (
	actionAddTests       = "add-tests"
	actionSplitPR        = "split-pr"
	actionReviewSecurity = "review-security"
	actionMergeReady     = "merge-ready"
)

// blocker_reason vocabulary. A fixed enum so the receipt never embeds a path,
// symbol ID, or any caller-supplied free text in the reason — safe to share.
const (
	blockerCIFailure      = "ci-failure"
	blockerCriticalRisk   = "critical-risk"
	blockerBreakingChange = "breaking-change"
)

// ReceiptFactor is the privacy-safe projection of a PRRiskFactor: only the
// axis name and its 0-100 score. The human-readable Reason is deliberately
// dropped — it can mention changed symbol names, which a shared receipt must
// not leak.
type ReceiptFactor struct {
	Axis  string  `json:"axis"`
	Score float64 `json:"score"`
}

// ReviewReceipt is a small, machine-readable projection of a PR-risk result: a
// structured blast-radius summary with a derived next-safe-action and a
// merge-blocker verdict. It carries only counts, tier labels, axis names, and
// a fixed-vocabulary action/reason — never a file path, symbol ID, or email —
// so (especially with scrub) it is safe to share across org boundaries.
type ReviewReceipt struct {
	ReceiptVersion  int             `json:"receipt_version"`
	RiskTier        RiskLevel       `json:"risk_tier"`
	NextSafeAction  string          `json:"next_safe_action"`
	MergeBlocker    bool            `json:"merge_blocker"`
	BlockerReason   string          `json:"blocker_reason"`
	AffectedCount   int             `json:"affected_count"`
	UncoveredCount  int             `json:"uncovered_count"`
	CommunitySpan   int             `json:"community_span"`
	SecurityFlagged bool            `json:"security_flagged"`
	TopFactors      []ReceiptFactor `json:"top_factors"`
}

// BuildReviewReceipt projects an already-computed PR-risk result into a small,
// privacy-safe receipt. ci is the normalized CI rollup (NONE / FAILURE /
// PENDING / SUCCESS); blocker reports an out-of-band hard blocker (e.g. a
// broken contract / breaking change the caller detected). merge_blocker is set
// when CI failed, the risk is the top tier, or the caller flagged a breaking
// change. When scrub is true the receipt is additionally sanitized so no
// path-like, "::"-bearing, or email-like value can leak — the counts, tier,
// action, and axis names are always retained.
func BuildReviewReceipt(result PRRiskResult, ci string, blocker bool, scrub bool) ReviewReceipt {
	blocked := false
	reason := ""
	switch {
	case strings.EqualFold(ci, "FAILURE"):
		blocked, reason = true, blockerCIFailure
	case result.Risk == RiskCritical:
		blocked, reason = true, blockerCriticalRisk
	case blocker:
		blocked, reason = true, blockerBreakingChange
	}

	top := make([]ReceiptFactor, 0, len(result.Factors))
	for _, f := range result.Factors {
		top = append(top, ReceiptFactor{Axis: f.Axis, Score: f.Score})
	}

	r := ReviewReceipt{
		ReceiptVersion:  receiptVersion,
		RiskTier:        result.Risk,
		NextSafeAction:  nextSafeAction(result),
		MergeBlocker:    blocked,
		BlockerReason:   reason,
		AffectedCount:   result.TotalAffected,
		UncoveredCount:  result.UncoveredSymbols,
		CommunitySpan:   result.CommunitySpan,
		SecurityFlagged: len(result.SecurityHits) > 0,
		TopFactors:      top,
	}

	if scrub {
		scrubReceipt(&r)
	}
	return r
}

// nextSafeAction derives the single most useful next step from a risk result.
// Precedence (most urgent first): uncovered changed symbols → add tests; a
// cross-cutting change → split the PR; a security-sensitive change → route to
// a security reviewer; otherwise the change is merge-ready.
func nextSafeAction(r PRRiskResult) string {
	switch {
	case r.UncoveredSymbols > 0:
		return actionAddTests
	case r.CommunitySpan >= receiptSpanSplit:
		return actionSplitPR
	case len(r.SecurityHits) > 0:
		return actionReviewSecurity
	default:
		return actionMergeReady
	}
}

// scrubReceipt strips any path-like, symbol-ID-like, or email-like value from
// the free-text fields of a receipt so it is safe to share cross-org. The
// numeric counts, the tier label, the merge verdict, the fixed-vocabulary
// action/reason, and the axis names are structurally privacy-safe and are
// retained verbatim; only fields that could ever carry caller context are
// sanitized, defensively, in case a future field is wired in.
func scrubReceipt(r *ReviewReceipt) {
	if leaksContext(r.NextSafeAction) {
		r.NextSafeAction = ""
	}
	if leaksContext(r.BlockerReason) {
		r.BlockerReason = ""
	}
	for i := range r.TopFactors {
		if leaksContext(r.TopFactors[i].Axis) {
			r.TopFactors[i].Axis = ""
		}
	}
}

// leaksContext reports whether a string carries a path-like, symbol-ID-like,
// or email-like value — the three context shapes a shared receipt must not
// expose. It is intentionally conservative: a slash, a "::" symbol-ID
// separator, or an "@" (email) all count as a leak.
func leaksContext(s string) bool {
	return strings.ContainsAny(s, "/\\@") || strings.Contains(s, "::")
}
