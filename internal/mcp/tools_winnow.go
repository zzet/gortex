package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// WinnowForEval is a thin, exported wrapper around winnowSymbols for the
// `gortex eval recall` harness. It builds a constraint chain from a plain
// text query plus caller-supplied extras (passed through as-is), runs the
// private scorer, and returns only the ranked IDs — everything the eval
// pipeline needs and nothing it doesn't. Keep the surface minimal; if
// future eval needs grow, extract the scorer into its own package.
func (s *Server) WinnowForEval(query string, extras map[string]any, limit int) []string {
	c := winnowConstraints{TextMatch: query, Limit: limit}
	if limit <= 0 {
		c.Limit = 20
	}
	if extras != nil {
		if v, ok := extras["language"].(string); ok {
			c.Language = v
		}
		if v, ok := extras["community"].(string); ok {
			c.Community = v
		}
		if v, ok := extras["min_fan_in"].(int); ok {
			c.MinFanIn = v
		}
		if v, ok := extras["min_fan_out"].(int); ok {
			c.MinFanOut = v
		}
		if v, ok := extras["min_churn"].(int); ok {
			c.MinChurn = v
		}
		if v, ok := extras["path_prefix"].([]string); ok {
			c.PathPrefix = v
		}
		if v, ok := extras["kinds"].([]string); ok {
			for _, k := range v {
				c.Kinds = append(c.Kinds, graph.NodeKind(k))
			}
		}
	}
	results := s.winnowSymbols(context.Background(), c, nil)
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Node.ID)
	}
	return out
}

// winnowAxisWeights tune the relative contribution of each ranking signal.
// text_match dominates when present; structural axes provide meaningful
// re-ranking within the filtered set.
var winnowAxisWeights = map[string]float64{
	"text_match": 1.0,
	"fan_in":     0.7,
	"fan_out":    0.3,
	"churn":      0.5,
}

// winnowConstraints carries the structured constraint chain.
type winnowConstraints struct {
	Kinds      []graph.NodeKind
	Language   string
	PathPrefix []string
	Community  string
	TextMatch  string
	MinFanIn   int
	MinFanOut  int
	MinChurn   int
	// IsTest is a tri-state test filter: nil leaves test-ness
	// unconstrained, *true keeps only test symbols, *false keeps only
	// production symbols.
	IsTest *bool
	// TestRoles, when non-empty, keeps only symbols whose
	// Meta["test_role"] is in the set (test / benchmark / fuzz /
	// example). Lowercased at parse time.
	TestRoles []string
	Limit     int
}

// winnowResult is a single ranked hit plus per-axis contribution breakdown.
type winnowResult struct {
	Node          *graph.Node
	Score         float64
	FanIn         int
	FanOut        int
	Churn         int
	Community     string
	TextScore     float64
	Contributions map[string]float64
}

func (c winnowConstraints) isEmpty() bool {
	return c.TextMatch == "" && len(c.Kinds) == 0 && c.Language == "" &&
		len(c.PathPrefix) == 0 && c.Community == "" &&
		c.MinFanIn == 0 && c.MinFanOut == 0 && c.MinChurn == 0 &&
		c.IsTest == nil && len(c.TestRoles) == 0
}

// handleWinnowSymbols answers structured constraint-chain queries. Where
// search_symbols takes free text only, winnow_symbols combines BM25 text
// matching with structural filters (kind, language, fan-in, community,
// path prefix, churn) and returns a ranked list with per-axis contributions
// so the caller can see why each symbol ranked where it did.
func (s *Server) handleWinnowSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, err := parseWinnowConstraints(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if c.isEmpty() {
		return mcp.NewToolResultError("winnow_symbols requires at least one constraint (kind, language, path_prefix, community, text_match, min_fan_in, min_fan_out, min_churn, is_test, or test_role)"), nil
	}

	if c.TextMatch != "" {
		s.sessionFor(ctx).recordSearch(c.TextMatch)
	}

	allowed, filterErr := s.resolveRepoFilter(ctx, req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}

	results := s.winnowSymbols(ctx, c, allowed)
	total := len(results)
	offset := decodeCursor(req.GetString("cursor", ""))
	if offset > total {
		offset = total
	}
	end := offset + c.Limit
	if end > total {
		end = total
	}
	results = results[offset:end]
	nextCursor := ""
	if end < total {
		nextCursor = encodeCursor(end)
	}

	if s.isGCX(ctx, req) {
		var weights map[string]float64
		if pipeline := s.engineFor(ctx).Rerank(); pipeline != nil {
			weights = pipeline.Weights()
		}
		return s.gcxResponseWithBudget(req)(encodeWinnowSymbols(results, total, c.Limit, weights))
	}

	if isCompact(req) {
		return mcp.NewToolResultText(compactWinnow(results)), nil
	}

	rows := make([]map[string]any, 0, len(results))
	for _, r := range results {
		row := s.withAbsPath(ctx, r.Node).Brief()
		row["score"] = roundFloat(r.Score)
		row["fan_in"] = r.FanIn
		row["fan_out"] = r.FanOut
		row["churn"] = r.Churn
		if r.Community != "" {
			row["community"] = r.Community
		}
		row["contributions"] = floatMapRound(r.Contributions)
		rows = append(rows, row)
	}
	resp := map[string]any{
		"results":   rows,
		"total":     total,
		"truncated": end < total,
		"weights":   winnowAxisWeights,
	}
	if nextCursor != "" {
		resp["next_cursor"] = nextCursor
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// parseWinnowConstraints extracts the structured constraints from the MCP
// request. Kind and path_prefix accept comma-separated lists.
func parseWinnowConstraints(req mcp.CallToolRequest) (winnowConstraints, error) {
	c := winnowConstraints{Limit: req.GetInt("limit", 20)}
	if c.Limit <= 0 {
		c.Limit = 20
	}

	if kindArg := strings.TrimSpace(req.GetString("kind", "")); kindArg != "" {
		for _, part := range splitCSV(kindArg) {
			k := graph.NodeKind(part)
			if !graph.ValidNodeKind(k) {
				return c, fmt.Errorf("invalid kind: %s", part)
			}
			c.Kinds = append(c.Kinds, k)
		}
	}

	c.Language = strings.TrimSpace(req.GetString("language", ""))

	if pp := strings.TrimSpace(req.GetString("path_prefix", "")); pp != "" {
		c.PathPrefix = splitCSV(pp)
	}

	c.Community = strings.TrimSpace(req.GetString("community", ""))
	c.TextMatch = strings.TrimSpace(req.GetString("text_match", ""))

	c.MinFanIn = req.GetInt("min_fan_in", 0)
	c.MinFanOut = req.GetInt("min_fan_out", 0)
	c.MinChurn = req.GetInt("min_churn", 0)

	if v, set := boolArg(req.GetArguments(), "is_test"); set {
		c.IsTest = &v
	}
	if tr := strings.TrimSpace(req.GetString("test_role", "")); tr != "" {
		for _, role := range splitCSV(tr) {
			c.TestRoles = append(c.TestRoles, strings.ToLower(role))
		}
	}

	return c, nil
}

// winnowSymbols applies the constraint chain and returns ranked results. It
// is package-internal so tests can exercise the filter/rank logic without
// the MCP request plumbing. The ctx parameter scopes graph reads to the
// caller's overlay view (when any) so winnow honours editor-buffer state
// just like search_symbols.
func (s *Server) winnowSymbols(ctx context.Context, c winnowConstraints, allowed map[string]bool) []winnowResult {
	eng := s.engineFor(ctx)
	reader := s.readerFor(ctx)
	var candidates []*graph.Node
	textScores := make(map[string]float64)

	if c.TextMatch != "" && eng != nil {
		// Pull a wider slice so structural filters have headroom.
		width := c.Limit*10 + 50
		nodes := eng.SearchSymbols(c.TextMatch, width)
		for rank, n := range nodes {
			textScores[n.ID] = 1.0 / float64(rank+1)
			candidates = append(candidates, n)
		}
	} else {
		candidates = reader.AllNodes()
	}

	candidates = filterNodes(candidates, allowed)

	// Strip file/import nodes — they're not meaningful targets for winnowing.
	kept := candidates[:0]
	for _, n := range candidates {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		kept = append(kept, n)
	}
	candidates = kept

	candidates = applyWinnowPrefilter(candidates, c)

	fanIn, fanOut := computeFanInOut(reader, candidates)

	var nodeToComm map[string]string
	var labelToID map[string]string
	if cr := s.getCommunities(); cr != nil {
		nodeToComm = cr.NodeToComm
		labelToID = make(map[string]string, len(cr.Communities))
		for _, com := range cr.Communities {
			if com.Label != "" {
				labelToID[com.Label] = com.ID
			}
		}
	}

	// Accept either community ID or label. Resolve label → ID up front.
	wantedComm := c.Community
	if wantedComm != "" {
		if id, ok := labelToID[wantedComm]; ok {
			wantedComm = id
		}
	}

	churn := s.churnCounts()

	var rows []winnowResult
	for _, n := range candidates {
		fi := fanIn[n.ID]
		fo := fanOut[n.ID]
		ch := churn[n.ID]
		if fi < c.MinFanIn || fo < c.MinFanOut || ch < c.MinChurn {
			continue
		}
		comm := nodeToComm[n.ID]
		if wantedComm != "" && comm != wantedComm {
			continue
		}
		rows = append(rows, winnowResult{
			Node:      n,
			FanIn:     fi,
			FanOut:    fo,
			Churn:     ch,
			Community: comm,
			TextScore: textScores[n.ID],
		})
	}

	// Delegate scoring to the I13 11-signal rerank pipeline so winnow
	// surfaces the same Contributions map as search_symbols. The
	// pre-filtering above (kind / path / community / min-counts) stays
	// intact — winnow is a constraint chain that hands its kept rows
	// to the pipeline for ranking.
	pipeline := eng.Rerank()
	if pipeline != nil && len(rows) > 0 {
		cands := make([]*rerank.Candidate, len(rows))
		idxByID := make(map[string]int, len(rows))
		var textOrder []*winnowResult
		if c.TextMatch != "" {
			textOrder = make([]*winnowResult, 0, len(rows))
			for i := range rows {
				if rows[i].TextScore > 0 {
					textOrder = append(textOrder, &rows[i])
				}
			}
			sort.SliceStable(textOrder, func(i, j int) bool {
				return textOrder[i].TextScore > textOrder[j].TextScore
			})
		}
		textRankByID := make(map[string]int, len(textOrder))
		for rank, r := range textOrder {
			textRankByID[r.Node.ID] = rank
		}
		for i, r := range rows {
			tr := -1
			if rk, ok := textRankByID[r.Node.ID]; ok {
				tr = rk
			}
			cands[i] = &rerank.Candidate{Node: r.Node, TextRank: tr, VectorRank: -1}
			idxByID[r.Node.ID] = i
		}
		rctx := s.buildRerankContext(ctx, c.TextMatch)
		pipeline.Rerank(c.TextMatch, cands, rctx)

		ordered := make([]winnowResult, 0, len(cands))
		for _, cand := range cands {
			origIdx := idxByID[cand.Node.ID]
			row := rows[origIdx]
			row.Score = cand.Score
			row.Contributions = floatMapRound(cand.Signals)
			ordered = append(ordered, row)
		}
		return ordered
	}

	// No pipeline (e.g. test harness with Rerank disabled) — fall
	// back to the legacy 4-axis local scoring so winnow still works.
	maxFanIn, maxFanOut, maxChurn := 0, 0, 0
	var maxText float64
	for _, r := range rows {
		if r.FanIn > maxFanIn {
			maxFanIn = r.FanIn
		}
		if r.FanOut > maxFanOut {
			maxFanOut = r.FanOut
		}
		if r.Churn > maxChurn {
			maxChurn = r.Churn
		}
		if r.TextScore > maxText {
			maxText = r.TextScore
		}
	}

	for i := range rows {
		contribs := map[string]float64{}
		if c.TextMatch != "" && maxText > 0 {
			contribs["text_match"] = (rows[i].TextScore / maxText) * winnowAxisWeights["text_match"]
		}
		if maxFanIn > 0 {
			contribs["fan_in"] = (float64(rows[i].FanIn) / float64(maxFanIn)) * winnowAxisWeights["fan_in"]
		}
		if maxFanOut > 0 {
			contribs["fan_out"] = (float64(rows[i].FanOut) / float64(maxFanOut)) * winnowAxisWeights["fan_out"]
		}
		if maxChurn > 0 {
			contribs["churn"] = (float64(rows[i].Churn) / float64(maxChurn)) * winnowAxisWeights["churn"]
		}
		var sum float64
		for _, v := range contribs {
			sum += v
		}
		rows[i].Score = sum
		rows[i].Contributions = contribs
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		if len(rows[i].Node.Name) != len(rows[j].Node.Name) {
			return len(rows[i].Node.Name) < len(rows[j].Node.Name)
		}
		return rows[i].Node.ID < rows[j].Node.ID
	})

	return rows
}

// applyWinnowPrefilter drops candidates that fail kind, language, path
// prefix, or test-classification constraints — filters that don't
// require edge traversal.
func applyWinnowPrefilter(nodes []*graph.Node, c winnowConstraints) []*graph.Node {
	if len(c.Kinds) == 0 && c.Language == "" && len(c.PathPrefix) == 0 &&
		c.IsTest == nil && len(c.TestRoles) == 0 {
		return nodes
	}
	kindSet := make(map[graph.NodeKind]bool, len(c.Kinds))
	for _, k := range c.Kinds {
		kindSet[k] = true
	}
	roleSet := make(map[string]bool, len(c.TestRoles))
	for _, r := range c.TestRoles {
		roleSet[r] = true
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if len(kindSet) > 0 && !kindSet[n.Kind] {
			continue
		}
		if c.Language != "" && !strings.EqualFold(n.Language, c.Language) {
			continue
		}
		if len(c.PathPrefix) > 0 {
			matched := false
			for _, p := range c.PathPrefix {
				if strings.HasPrefix(n.FilePath, p) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if c.IsTest != nil {
			isTest := false
			if n.Meta != nil {
				isTest, _ = n.Meta["is_test"].(bool)
			}
			if isTest != *c.IsTest {
				continue
			}
		}
		if len(roleSet) > 0 {
			role := ""
			if n.Meta != nil {
				role, _ = n.Meta["test_role"].(string)
			}
			if !roleSet[role] {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}

// computeFanInOut walks incoming and outgoing edges for each candidate and
// counts fan-in (calls + references) and fan-out (calls). Scoped to the
// candidate set so cost scales with |candidates|, not total graph size.
func computeFanInOut(g graph.Reader, nodes []*graph.Node) (map[string]int, map[string]int) {
	fanIn := make(map[string]int, len(nodes))
	fanOut := make(map[string]int, len(nodes))
	if g == nil {
		return fanIn, fanOut
	}
	for _, n := range nodes {
		for _, e := range g.GetInEdges(n.ID) {
			if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
				fanIn[n.ID]++
			}
		}
		for _, e := range g.GetOutEdges(n.ID) {
			if e.Kind == graph.EdgeCalls {
				fanOut[n.ID]++
			}
		}
	}
	return fanIn, fanOut
}

// churnCounts returns symbolID → modification count from session history.
func (s *Server) churnCounts() map[string]int {
	if s.symHistory == nil {
		return map[string]int{}
	}
	all := s.symHistory.All()
	out := make(map[string]int, len(all))
	for id, mods := range all {
		out[id] = len(mods)
	}
	return out
}

// compactWinnow renders one line per result when the legacy `compact: true`
// arg is set. Keeps the columnar layout aligned with other tools.
func compactWinnow(rows []winnowResult) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%s\t%s\t%s:%d\tscore=%.3f\tfan_in=%d\tfan_out=%d\tchurn=%d",
			r.Node.ID, r.Node.Kind, r.Node.FilePath, r.Node.StartLine,
			r.Score, r.FanIn, r.FanOut, r.Churn)
		if r.Community != "" {
			fmt.Fprintf(&b, "\tcomm=%s", r.Community)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// roundFloat rounds to 3 decimal places for stable JSON output.
func roundFloat(v float64) float64 {
	if v >= 0 {
		return float64(int64(v*1000+0.5)) / 1000
	}
	return float64(int64(v*1000-0.5)) / 1000
}

// floatMapRound returns a copy of m with each value rounded.
func floatMapRound(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = roundFloat(v)
	}
	return out
}
