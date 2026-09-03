package mcp

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// errReviewQuestionsNoRoot is returned when a `base` diff is requested
// but no repository working tree can be resolved.
var errReviewQuestionsNoRoot = errors.New("could not resolve a repository root for the base diff")

// registerReviewQuestionsTool wires suggested_review_questions — a
// reviewer-prep aid that maps the changed (or whole-repo) symbol set to
// graph anomalies and emits prioritised, symbol-anchored natural-language
// review questions. Five miners run over the candidate set: bridge
// (betweenness centrality), hub_risk (high fan-in), surprising (the
// shared surprising-edge miner), thin_community (small cluster
// membership), and untested_hotspot (load-bearing but untested). Each
// question carries a category, a base severity, and the exact symbol /
// file / line it asks about. Deferred: it lands in the lazy catalog
// unless lazy tools are disabled.
func (s *Server) registerReviewQuestionsTool() {
	s.addTool(
		mcp.NewTool("suggested_review_questions",
			mcp.WithDescription("Generate prioritised, symbol-anchored review questions for a changeset by mapping its symbols to graph anomalies. Five categories: bridge (a high-betweenness symbol the call graph routes through), hub_risk (a high-fan-in symbol many callers depend on), surprising (an anomalous edge — cross-community / cross-language / peripheral-to-hub / cross-test / unusual-kind), thin_community (a symbol in an under-clustered neighbourhood), and untested_hotspot (a load-bearing symbol with no inbound test edge). Pass `base` (a git ref — questions scope to the diff against it), `ids` (comma-separated changed symbol IDs), or neither (the whole scoped workspace). Each question is tied to a specific symbol id + file + line, carries a HIGH/MEDIUM/LOW severity, and the list is sorted highest-risk first. Use to prep a focused review before opening a PR."),
			mcp.WithString("base", mcp.Description("Base git ref (e.g. main). Scopes the questions to the symbols changed in `git diff base...HEAD` against the indexed repo.")),
			mcp.WithString("ids", mcp.Description("Comma-separated changed symbol IDs to scope the questions to. Takes precedence over `base`.")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithNumber("limit", mcp.Description("Cap the number of questions returned (default 20).")),
			mcp.WithString("path_prefix", mcp.Description("Scope analysis to symbols under this file-path prefix — e.g. 'internal/auth/'.")),
			mcp.WithString("categories", mcp.Description("Comma-separated subset of {bridge,hub_risk,surprising,thin_community,untested_hotspot} to emit. Omit for all five.")),
			mcp.WithNumber("hub_threshold", mcp.Description("Fan-in (incoming calls/references) at or above which a symbol is flagged as a hub for the hub_risk category (default 8).")),
			mcp.WithNumber("min_community_size", mcp.Description("A symbol whose community has fewer than this many members is flagged as thin_community (default 3).")),
			mcp.WithNumber("min_betweenness", mcp.Description("Normalized betweenness (0-100) at or above which a symbol is flagged as a bridge (default 20).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description("Cap the marshaled response at approximately this many tokens. Composable with max_bytes — tighter wins. Omit for no cap.")),
		),
		s.handleSuggestedReviewQuestions,
	)
}

// reviewQuestion is one prioritised, symbol-anchored review question.
// Category names the anomaly miner that produced it; Severity is the
// HIGH/MEDIUM/LOW base risk; Score is the underlying anomaly magnitude
// used as the secondary sort key; Evidence is a one-line "why" and
// Signals carries the per-miner detail an agent can render without an
// extra round-trip.
type reviewQuestion struct {
	ID         string   `json:"id"`
	Category   string   `json:"category"`
	Question   string   `json:"question"`
	SymbolID   string   `json:"symbol_id"`
	SymbolName string   `json:"symbol_name,omitempty"`
	File       string   `json:"file,omitempty"`
	Line       int      `json:"line,omitempty"`
	Severity   string   `json:"severity"`
	Score      float64  `json:"score"`
	Evidence   string   `json:"evidence,omitempty"`
	Signals    []string `json:"signals,omitempty"`
}

// review-question severity tokens. The string form rides on the wire;
// severityRankRQ orders them for the prioritised sort.
const (
	rqSevHigh   = "HIGH"
	rqSevMedium = "MEDIUM"
	rqSevLow    = "LOW"
)

// the five miner category names, kept as constants so the categories
// filter and the by_category rollup agree on spelling.
const (
	rqCatBridge          = "bridge"
	rqCatHubRisk         = "hub_risk"
	rqCatSurprising      = "surprising"
	rqCatThinCommunity   = "thin_community"
	rqCatUntestedHotspot = "untested_hotspot"
)

func severityRankRQ(sev string) int {
	switch sev {
	case rqSevHigh:
		return 3
	case rqSevMedium:
		return 2
	case rqSevLow:
		return 1
	default:
		return 0
	}
}

func (s *Server) handleSuggestedReviewQuestions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	limit := max(req.GetInt("limit", 20), 1)
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))
	hubThreshold := max(req.GetInt("hub_threshold", 8), 1)
	minCommunitySize := max(req.GetInt("min_community_size", 3), 1)
	minBetweenness := req.GetFloat("min_betweenness", 20.0)
	if minBetweenness < 0 {
		minBetweenness = 0
	}
	wantCat := parseReviewQuestionCategories(req.GetString("categories", ""))

	// Build the scoped node index once — every miner reads from it.
	scopedSet := make(map[string]*graph.Node, 1024)
	for _, n := range s.scopedNodesLight(ctx) {
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		scopedSet[n.ID] = n
	}

	// Resolve the candidate symbol set. ids / base narrow the miners to
	// the changed symbols; absent both, every function/method in scope
	// is a candidate. targetIDs==nil means "no narrowing".
	targetIDs, err := s.resolveReviewQuestionTargets(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// candidates is the function/method subset the symbol-anchored
	// miners (bridge, hub_risk, thin_community, untested_hotspot) walk.
	candidates := make([]*graph.Node, 0, len(scopedSet))
	for _, n := range scopedSet {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if targetIDs != nil {
			if _, ok := targetIDs[n.ID]; !ok {
				continue
			}
		}
		candidates = append(candidates, n)
	}

	var questions []reviewQuestion
	if wantCat[rqCatBridge] {
		questions = append(questions, s.mineBridgeQuestions(ctx, candidates, minBetweenness)...)
	}
	if wantCat[rqCatHubRisk] {
		questions = append(questions, s.mineHubRiskQuestions(ctx, candidates, hubThreshold)...)
	}
	if wantCat[rqCatThinCommunity] {
		questions = append(questions, s.mineThinCommunityQuestions(candidates, minCommunitySize)...)
	}
	if wantCat[rqCatUntestedHotspot] {
		questions = append(questions, s.mineUntestedHotspotQuestions(ctx, candidates, hubThreshold)...)
	}
	if wantCat[rqCatSurprising] {
		questions = append(questions, s.mineSurprisingQuestions(ctx, scopedSet, targetIDs, pathPrefix, hubThreshold)...)
	}

	// Prioritise: highest severity first, then score, then a stable
	// symbol/category tiebreak so identical-rank questions are
	// deterministic across runs.
	sort.SliceStable(questions, func(i, j int) bool {
		ri, rj := severityRankRQ(questions[i].Severity), severityRankRQ(questions[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if questions[i].Score != questions[j].Score {
			return questions[i].Score > questions[j].Score
		}
		if questions[i].SymbolID != questions[j].SymbolID {
			return questions[i].SymbolID < questions[j].SymbolID
		}
		return questions[i].Category < questions[j].Category
	})

	// by_category counts the full (pre-truncation) set so the rollup
	// stays honest about what was found, not just what survived limit.
	byCategory := map[string]int{}
	for _, q := range questions {
		byCategory[q.Category]++
	}

	truncated := false
	if len(questions) > limit {
		questions = questions[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeReviewQuestions(questions, byCategory, truncated))
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"questions":   questions,
		"total":       len(questions),
		"truncated":   truncated,
		"by_category": byCategory,
	})
}

// resolveReviewQuestionTargets turns the ids / base input into the set
// of symbol IDs the miners should narrow to, or nil for no narrowing.
// ids takes precedence over base; neither given returns nil.
func (s *Server) resolveReviewQuestionTargets(ctx context.Context, req mcp.CallToolRequest) (map[string]struct{}, error) {
	idsStr := strings.TrimSpace(req.GetString("ids", ""))
	base := strings.TrimSpace(req.GetString("base", ""))

	switch {
	case idsStr != "":
		out := map[string]struct{}{}
		for _, id := range strings.Split(idsStr, ",") {
			if id = strings.TrimSpace(id); id != "" {
				out[id] = struct{}{}
			}
		}
		return out, nil

	case base != "":
		repo := strings.TrimSpace(req.GetString("repo", ""))
		repoRoot, repoPrefix := s.diffRepoScope(ctx, repo)
		if repoRoot == "" {
			return nil, errReviewQuestionsNoRoot
		}
		diff, derr := analysis.MapGitDiff(s.readerFor(ctx), repoRoot, repoPrefix, "compare", base)
		if derr != nil {
			return nil, derr
		}
		out := map[string]struct{}{}
		for _, cs := range diff.ChangedSymbols {
			out[cs.ID] = struct{}{}
		}
		return out, nil

	default:
		return nil, nil
	}
}

// parseReviewQuestionCategories returns the set of enabled categories.
// An empty / unparseable filter enables all five.
func parseReviewQuestionCategories(raw string) map[string]bool {
	all := map[string]bool{
		rqCatBridge:          true,
		rqCatHubRisk:         true,
		rqCatSurprising:      true,
		rqCatThinCommunity:   true,
		rqCatUntestedHotspot: true,
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return all
	}
	out := map[string]bool{}
	for _, c := range strings.Split(raw, ",") {
		c = strings.TrimSpace(strings.ToLower(c))
		if all[c] {
			out[c] = true
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

// mineBridgeQuestions flags candidate symbols whose normalized
// betweenness centrality clears minBetweenness — the call graph routes
// many shortest paths through them, so a change there ripples widely.
func (s *Server) mineBridgeQuestions(ctx context.Context, candidates []*graph.Node, minBetweenness float64) []reviewQuestion {
	bc := analysis.ComputeBetweenness(s.readerFor(ctx))
	if bc == nil || bc.Max <= 0 {
		return nil
	}
	out := make([]reviewQuestion, 0)
	for _, n := range candidates {
		norm := (bc.ScoreOf(n.ID) / bc.Max) * 100.0
		if norm < minBetweenness {
			continue
		}
		sev := rqSevMedium
		if norm >= minBetweenness*2 {
			sev = rqSevHigh
		}
		out = append(out, reviewQuestion{
			ID:         "rq:bridge:" + n.ID,
			Category:   rqCatBridge,
			Question:   "Many call paths route through " + n.Name + " — does this change preserve its existing contract for every dependent path?",
			SymbolID:   n.ID,
			SymbolName: n.Name,
			File:       n.FilePath,
			Line:       n.StartLine,
			Severity:   sev,
			Score:      roundScore(norm),
			Evidence:   "betweenness centrality " + ftoa1(norm) + "/100",
			Signals:    []string{"betweenness=" + ftoa1(norm)},
		})
	}
	return out
}

// mineHubRiskQuestions flags candidate symbols whose fan-in (incoming
// calls/references) is at or above hubThreshold — a wide blast radius
// for any behaviour change.
func (s *Server) mineHubRiskQuestions(ctx context.Context, candidates []*graph.Node, hubThreshold int) []reviewQuestion {
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, 0, len(candidates))
	for _, n := range candidates {
		ids = append(ids, n.ID)
	}
	fanIn, _ := analysis.CollectFanCounts(s.readerFor(ctx), ids,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences},
		[]graph.EdgeKind{graph.EdgeCalls},
	)
	out := make([]reviewQuestion, 0)
	for _, n := range candidates {
		fi := fanIn[n.ID]
		if fi < hubThreshold {
			continue
		}
		sev := rqSevMedium
		if fi >= hubThreshold*2 {
			sev = rqSevHigh
		}
		out = append(out, reviewQuestion{
			ID:         "rq:hub_risk:" + n.ID,
			Category:   rqCatHubRisk,
			Question:   itoa(fi) + " callers depend on " + n.Name + " — is its behaviour backward-compatible for all of them?",
			SymbolID:   n.ID,
			SymbolName: n.Name,
			File:       n.FilePath,
			Line:       n.StartLine,
			Severity:   sev,
			Score:      float64(fi),
			Evidence:   "fan-in " + itoa(fi) + " callers",
			Signals:    []string{"fan_in=" + itoa(fi)},
		})
	}
	return out
}

// mineThinCommunityQuestions flags candidate symbols that live in a
// community with fewer than minCommunitySize members — an
// under-connected neighbourhood the symbol may not be well-tested or
// well-understood within.
func (s *Server) mineThinCommunityQuestions(candidates []*graph.Node, minCommunitySize int) []reviewQuestion {
	cr := s.getCommunities()
	if cr == nil {
		return nil
	}
	sizeByComm := make(map[string]int, len(cr.Communities))
	for _, c := range cr.Communities {
		sizeByComm[c.ID] = c.Size
	}
	out := make([]reviewQuestion, 0)
	for _, n := range candidates {
		commID, ok := cr.NodeToComm[n.ID]
		if !ok || commID == "" {
			continue
		}
		size, ok := sizeByComm[commID]
		if !ok || size >= minCommunitySize {
			continue
		}
		out = append(out, reviewQuestion{
			ID:         "rq:thin_community:" + n.ID,
			Category:   rqCatThinCommunity,
			Question:   n.Name + " sits in a thin cluster (" + itoa(size) + " members) — is it isolated by design, or missing connective code / tests?",
			SymbolID:   n.ID,
			SymbolName: n.Name,
			File:       n.FilePath,
			Line:       n.StartLine,
			Severity:   rqSevLow,
			// Smaller community → more anomalous; invert so a 1-member
			// cluster outscores a 2-member one within the category.
			Score:    float64(minCommunitySize - size),
			Evidence: "community size " + itoa(size) + " < " + itoa(minCommunitySize),
			Signals:  []string{"community_size=" + itoa(size)},
		})
	}
	return out
}

// mineUntestedHotspotQuestions flags candidate symbols that are
// load-bearing (fan-in at or above hubThreshold) yet have no inbound
// EdgeTests edge — a change there is high-impact and unguarded by tests.
func (s *Server) mineUntestedHotspotQuestions(ctx context.Context, candidates []*graph.Node, hubThreshold int) []reviewQuestion {
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, 0, len(candidates))
	for _, n := range candidates {
		ids = append(ids, n.ID)
	}
	fanIn, _ := analysis.CollectFanCounts(s.readerFor(ctx), ids,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences},
		[]graph.EdgeKind{graph.EdgeCalls},
	)
	out := make([]reviewQuestion, 0)
	for _, n := range candidates {
		fi := fanIn[n.ID]
		if fi < hubThreshold {
			continue
		}
		if s.symbolHasInboundTest(ctx, n.ID) {
			continue
		}
		sev := rqSevHigh
		if fi < hubThreshold*2 {
			sev = rqSevMedium
		}
		out = append(out, reviewQuestion{
			ID:         "rq:untested_hotspot:" + n.ID,
			Category:   rqCatUntestedHotspot,
			Question:   n.Name + " has " + itoa(fi) + " callers but no covering test — should this change ship with one?",
			SymbolID:   n.ID,
			SymbolName: n.Name,
			File:       n.FilePath,
			Line:       n.StartLine,
			Severity:   sev,
			Score:      float64(fi),
			Evidence:   "fan-in " + itoa(fi) + ", no inbound test edge",
			Signals:    []string{"fan_in=" + itoa(fi), "untested"},
		})
	}
	return out
}

// mineSurprisingQuestions reuses the shared surprising-edge miner and
// turns each anomalous edge into a question anchored on its source
// symbol (the side that introduces the surprise). When targetIDs is
// non-nil only edges touching a changed symbol are kept.
func (s *Server) mineSurprisingQuestions(
	ctx context.Context,
	scopedSet map[string]*graph.Node,
	targetIDs map[string]struct{},
	pathPrefix string,
	hubThreshold int,
) []reviewQuestion {
	// minScore 0.3 == "at least one signal fired", matching the
	// get_surprising_connections default; rareKindPct 5 matches too.
	rows := s.collectSurprisingEdges(ctx, scopedSet, pathPrefix, 0.3, hubThreshold, 5.0)
	out := make([]reviewQuestion, 0)
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if targetIDs != nil {
			_, fromHit := targetIDs[r.From]
			_, toHit := targetIDs[r.To]
			if !fromHit && !toHit {
				continue
			}
		}
		// Anchor on the source — it is the side that reaches out to
		// the surprising target. Dedup so one symbol with many
		// anomalous edges yields a single (highest-score) question.
		if _, ok := seen[r.From]; ok {
			continue
		}
		seen[r.From] = struct{}{}
		sev := rqSevMedium
		if r.Score >= 0.5 {
			sev = rqSevHigh
		}
		fromName := r.FromName
		if fromName == "" {
			fromName = shortSymbolName(r.From)
		}
		out = append(out, reviewQuestion{
			ID:         "rq:surprising:" + r.From,
			Category:   rqCatSurprising,
			Question:   fromName + " has an anomalous " + r.Kind + " edge to " + r.ToName + " — is this coupling intentional?",
			SymbolID:   r.From,
			SymbolName: fromName,
			File:       r.FromFile,
			Line:       0,
			Severity:   sev,
			Score:      r.Score,
			Evidence:   strings.Join(r.Reasons, ", "),
			Signals:    r.Reasons,
		})
	}
	return out
}

// symbolHasInboundTest reports whether any inbound EdgeTests edge points
// at the symbol — the same one-hop inverse-edge walk the blast-radius
// analyzer uses, kept inline so this file doesn't reach into analysis
// for one predicate.
func (s *Server) symbolHasInboundTest(ctx context.Context, symbolID string) bool {
	for _, e := range s.readerFor(ctx).GetInEdges(symbolID) {
		if e.Kind == graph.EdgeTests {
			return true
		}
	}
	return false
}

// shortSymbolName returns whatever follows the last "::" in a symbol
// ID, falling back to the whole ID — the string-keyed sibling of
// nodeShort for the surprising miner, which holds endpoint IDs, not
// *Node pointers.
func shortSymbolName(id string) string {
	if idx := strings.LastIndex(id, "::"); idx >= 0 {
		return id[idx+2:]
	}
	return id
}

// ftoa1 renders a float with one decimal place without pulling in fmt
// on the per-question hot path. Negative inputs are clamped to 0 — every
// caller feeds a non-negative score.
func ftoa1(v float64) string {
	if v < 0 {
		v = 0
	}
	whole := int(v)
	frac := int((v-float64(whole))*10 + 0.5)
	if frac >= 10 {
		whole++
		frac = 0
	}
	return itoa(whole) + "." + itoa(frac)
}
