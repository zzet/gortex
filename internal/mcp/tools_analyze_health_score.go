package mcp

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// ---------------------------------------------------------------------------
// analyze kind=health_score
// ---------------------------------------------------------------------------
//
// Composite per-symbol health aggregator. Pure read-side over the
// metadata already stamped by the shipped enrichers:
//
//   - coverage_pct       — meta.coverage_pct (from `analyze kind=coverage`)
//   - complexity         — fan-in + fan-out + community-crossings, the
//                          same raw inputs `analyze kind=hotspots` uses
//   - last_authored      — meta.last_authored.timestamp (from
//                          `analyze kind=blame` / `gortex enrich blame`)
//   - churn              — Server.symHistory edit count for this session
//
// Each axis is normalised to 0..100 where 100 = healthy. The composite
// score is a weighted average over the axes that actually have data;
// missing axes are skipped (not zero-imputed) so a repo with no blame
// enrichment still produces meaningful per-symbol scores from the axes
// that ARE available. The per-axis breakdown is surfaced on every row
// so an agent can see which signal dragged a symbol down.
//
// Grades map the composite score onto A..F so a reviewer can sort
// "show me the F-grade symbols" without committing to a numeric
// threshold.

// healthWeights — default per-axis weights. Exported as named
// constants because the formula is the user-visible contract and
// changing one should be a deliberate edit, not a magic-number
// tweak. Coverage weighted highest: an untested function is the
// single strongest negative signal in code-quality literature.
const (
	healthWeightCoverage   = 3.0
	healthWeightComplexity = 2.0
	healthWeightRecency    = 1.5
	healthWeightChurn      = 1.5
)

// Recency curve constants (days).
const (
	healthRecencyFreshDays = 30   // 0..30 days → 100
	healthRecencyOKDays    = 365  // 30..365   → 100 linearly down to 50
	healthRecencyDeadDays  = 1095 // 365..1095 → 50 linearly down to 0
)

// healthRollupRow is one per-file / per-repo aggregate row produced
// when `roll_up` selects a non-symbol scope.
type healthRollupRow struct {
	Scope      string         `json:"scope"` // "file" | "repo"
	Key        string         `json:"key"`   // file path or repo prefix
	AvgScore   float64        `json:"avg_score"`
	MinScore   float64        `json:"min_score"`
	MaxScore   float64        `json:"max_score"`
	Symbols    int            `json:"symbols"`
	Grade      string         `json:"grade"` // derived from AvgScore
	GradeCount map[string]int `json:"grade_counts"`
}

// healthDistribution is the population-level summary that always
// rides on the response. The Gini coefficient measures how unevenly
// "tech debt" is distributed: 0 = every symbol equally healthy /
// unhealthy; close to 1 = a small set of symbols carry most of the
// risk. Pairs with the per-grade counts so an agent can both see the
// shape and read off the operationally-meaningful buckets.
type healthDistribution struct {
	Mean        float64        `json:"mean"`
	Median      float64        `json:"median"`
	StdDev      float64        `json:"std_dev"`
	Gini        float64        `json:"gini"`
	GradeCounts map[string]int `json:"grade_counts"`
	Total       int            `json:"total"`
}

// healthScoreRow is the per-symbol breakdown returned in the JSON
// response. Each axis carries a 0..100 health value plus the raw
// input it was derived from, so the consumer can both rank and
// explain the score.
type healthScoreRow struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	File  string  `json:"file"`
	Line  int     `json:"line"`
	Score float64 `json:"score"`
	Grade string  `json:"grade"`

	// Axes — "_pct" suffix is the 0..100 health value; "_raw" is
	// the underlying input. Pointers because "no data" is a real
	// signal distinct from "score is zero".
	CoveragePct   *float64 `json:"coverage_pct,omitempty"`
	ComplexityPct *float64 `json:"complexity_pct,omitempty"`
	RecencyPct    *float64 `json:"recency_pct,omitempty"`
	ChurnPct      *float64 `json:"churn_pct,omitempty"`

	FanIn     int  `json:"fan_in"`
	FanOut    int  `json:"fan_out"`
	Crossings int  `json:"community_crossings"`
	AgeDays   *int `json:"age_days,omitempty"`
	Mods      int  `json:"session_mods"`
	AxesUsed  int  `json:"axes_used"`
}

// handleAnalyzeHealthScore aggregates the shipped enrichment into one
// per-symbol composite + grade.
//
// Filters:
//   - path_prefix   — keep only symbols whose file path starts with this.
//   - kinds         — comma-separated (default function,method); "all"
//     keeps every blame-eligible kind.
//   - grade         — comma-separated A..F subset; keeps only matching rows.
//   - min_score     — drop rows whose composite score is below this.
//   - max_score     — drop rows whose composite score is above this.
//   - min_axes      — drop rows backed by fewer than this many axes
//     (default 1; raise to 2-3 to demand multi-signal
//     confidence at the cost of fewer rows).
//   - limit         — cap rows (default 200). Total still reports
//     pre-truncation count.
func (s *Server) handleAnalyzeHealthScore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	gradeFilter := parseCSVSet(strings.ToUpper(stringArg(args, "grade")))
	minScore := -1.0
	if v, ok := args["min_score"].(float64); ok {
		minScore = v
	}
	maxScore := -1.0
	if v, ok := args["max_score"].(float64); ok {
		maxScore = v
	}
	minAxes := intArg(args, "min_axes", 1)
	limit := intArg(args, "limit", 200)
	rollUp := strings.ToLower(strings.TrimSpace(stringArg(args, "roll_up")))
	if rollUp != "" && rollUp != "file" && rollUp != "repo" {
		return mcp.NewToolResultError("analyze health_score: roll_up must be empty, 'file', or 'repo'"), nil
	}

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	// Build fan-in / fan-out / community-crossing maps. Same
	// arithmetic shape as FindHotspots -- we read the raw axes here
	// rather than calling FindHotspots so the per-node fan-in is
	// available for symbols below its threshold.
	//
	// Fan-in / fan-out go through analysis.CollectFanCounts, which
	// uses the NodeFanAggregator capability when the backend
	// supports it (one bulk query per direction over the candidate
	// id set) and falls back to a per-kind EdgesByKind stream
	// otherwise. Crossings still need per-edge (from, to) for the
	// Calls + References kinds -- streamed via EdgesByKind so even
	// the fallback path never materialises the full edge set.
	nodeToComm := map[string]string{}
	if c := s.getCommunities(); c != nil {
		nodeToComm = c.NodeToComm
	}

	// Pull only the candidate kinds from the store — most workspaces
	// keep ~5-15% of nodes as functions/methods, so the kind pushdown
	// drops the AllNodes materialisation by 1-2 orders of magnitude.
	kindList := make([]graph.NodeKind, 0, len(allowedKinds))
	for k := range allowedKinds {
		kindList = append(kindList, k)
	}
	scoped := s.scopedNodesByKinds(ctx, kindList)
	candidateIDs := make([]string, 0, len(scoped))
	for _, n := range scoped {
		if n == nil {
			continue
		}
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		candidateIDs = append(candidateIDs, n.ID)
	}
	reader := s.readerFor(ctx)
	fanIn, fanOut := analysis.CollectFanCounts(reader, candidateIDs,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences},
		[]graph.EdgeKind{graph.EdgeCalls},
	)

	crossings := map[string]int{}
	for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
		for e := range reader.EdgesByKind(kind) {
			if e == nil {
				continue
			}
			from := nodeToComm[e.From]
			to := nodeToComm[e.To]
			if from != "" && to != "" && from != to {
				crossings[e.From]++
			}
		}
	}

	// Pull the session symHistory snapshot once instead of locking
	// it per-symbol — the analyzer walks every scoped node and
	// the map is small enough to copy cheaply.
	churnSnapshot := s.symHistory.All()

	now := time.Now()

	covRows := coverageRowsByID(reader)
	blame := blameRowsByID(reader)
	rows := make([]healthScoreRow, 0, 128)
	for _, n := range scoped {
		if n == nil {
			continue
		}
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}

		row := healthScoreRow{
			ID:        n.ID,
			Name:      n.Name,
			Kind:      string(n.Kind),
			File:      n.FilePath,
			Line:      n.StartLine,
			FanIn:     fanIn[n.ID],
			FanOut:    fanOut[n.ID],
			Crossings: crossings[n.ID],
		}

		var weighted float64
		var totalWeight float64

		// Coverage axis — direct mapping (coverage_pct is already
		// 0..100, higher is healthier).
		if pct, ok := coveragePctFrom(covRows, n); ok {
			covHealth := clamp01(pct)
			row.CoveragePct = &covHealth
			weighted += covHealth * healthWeightCoverage
			totalWeight += healthWeightCoverage
			row.AxesUsed++
		}

		// Complexity axis — same raw input as `hotspots`, scored
		// with a soft saturating curve so the worst-case (huge
		// fan-out) doesn't compress everyone else into a single
		// bucket. raw = fan_in*2 + fan_out*1.5 + crossings*3.
		raw := float64(row.FanIn)*2 + float64(row.FanOut)*1.5 + float64(row.Crossings)*3
		// Every callable symbol participates in the complexity
		// axis — even a leaf function with raw=0 carries the
		// signal "low coupling = healthy". Asymmetric with
		// coverage / blame because those genuinely require
		// out-of-graph enrichment to be meaningful.
		complexityHealth := 100.0 / (1.0 + raw/20.0)
		row.ComplexityPct = &complexityHealth
		weighted += complexityHealth * healthWeightComplexity
		totalWeight += healthWeightComplexity
		row.AxesUsed++

		// Recency axis — derived from meta.last_authored.timestamp.
		// Linear piecewise: fresh (≤30d) = 100; ok-zone
		// (30..365d) = 100→50; stale-zone (365..1095d) = 50→0;
		// dead (>1095d) = 0.
		if ts, ok := lastAuthoredTSFrom(blame, n); ok {
			ageDays := max(int(now.Sub(time.Unix(ts, 0)).Hours()/24), 0)
			row.AgeDays = &ageDays
			recHealth := recencyScore(ageDays)
			row.RecencyPct = &recHealth
			weighted += recHealth * healthWeightRecency
			totalWeight += healthWeightRecency
			row.AxesUsed++
		}

		// Churn axis — session modifications. Decay so a heavily
		// edited symbol scores worse than a never-touched one
		// (high churn is a stability signal).
		mods := len(churnSnapshot[n.ID])
		if mods > 0 {
			row.Mods = mods
			churnHealth := 100.0 / (1.0 + float64(mods)/3.0)
			row.ChurnPct = &churnHealth
			weighted += churnHealth * healthWeightChurn
			totalWeight += healthWeightChurn
			row.AxesUsed++
		}

		if row.AxesUsed < minAxes {
			continue
		}
		if totalWeight == 0 {
			continue
		}
		composite := weighted / totalWeight
		row.Score = math.Round(composite*100) / 100
		row.Grade = scoreGrade(row.Score)

		if minScore >= 0 && row.Score < minScore {
			continue
		}
		if maxScore >= 0 && row.Score > maxScore {
			continue
		}
		if len(gradeFilter) > 0 {
			if _, ok := gradeFilter[strings.ToLower(row.Grade)]; !ok {
				continue
			}
		}

		rows = append(rows, row)
	}

	// Sort ascending by score — the worst symbols surface first
	// so the agent's truncation budget always lands on the
	// highest-leverage candidates. Secondary sort by file/line for
	// stable output.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score < rows[j].Score
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})

	// Distribution is computed over the FULL filtered row set
	// (before truncation) so an agent always sees the
	// population-level shape regardless of the per-row budget.
	distribution := computeHealthDistribution(rows)

	// File / repo rollups are aggregated before truncation for
	// the same reason — partial rollups would mislead the
	// reviewer.
	var rollupRows []healthRollupRow
	switch rollUp {
	case "file":
		rollupRows = rollupHealthBy(rows, "file", func(r healthScoreRow) string {
			return r.File
		})
	case "repo":
		rollupRows = rollupHealthBy(rows, "repo", func(r healthScoreRow) string {
			return repoPrefixForPath(reader, r.File)
		})
	}

	totalRows := len(rows)
	truncated := false
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	// When rolled up, the symbol list is suppressed — the agent
	// asked for aggregates explicitly. Always include distribution
	// + weights so the rollup is self-explanatory.
	if rollUp != "" {
		if s.isGCX(ctx, req) {
			items := make([]healthRollupItem, 0, len(rollupRows))
			for _, r := range rollupRows {
				items = append(items, healthRollupItem{
					Scope:    r.Scope,
					Key:      r.Key,
					AvgScore: r.AvgScore,
					MinScore: r.MinScore,
					MaxScore: r.MaxScore,
					Symbols:  r.Symbols,
					Grade:    r.Grade,
					CountA:   r.GradeCount["A"],
					CountB:   r.GradeCount["B"],
					CountC:   r.GradeCount["C"],
					CountD:   r.GradeCount["D"],
					CountF:   r.GradeCount["F"],
				})
			}
			return s.gcxResponseWithBudget(req)(encodeAnalyze("health_score.rollup", items))
		}
		if isCompact(req) {
			var b strings.Builder
			for _, r := range rollupRows {
				fmt.Fprintf(&b, "%s  %5.1f  (n=%d)  %s\n",
					r.Grade, r.AvgScore, r.Symbols, r.Key)
			}
			if len(rollupRows) == 0 {
				b.WriteString("no rollup rows\n")
			}
			return mcp.NewToolResultText(b.String()), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"rollup":       rollupRows,
			"scope":        rollUp,
			"distribution": distribution,
			"weights":      healthWeightsMap(),
		})
	}

	if s.isGCX(ctx, req) {
		items := make([]healthScoreItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, healthScoreItem{
				ID:            r.ID,
				Name:          r.Name,
				Kind:          r.Kind,
				File:          r.File,
				Line:          r.Line,
				Score:         r.Score,
				Grade:         r.Grade,
				CoveragePct:   derefFloatOrNaN(r.CoveragePct),
				ComplexityPct: derefFloatOrNaN(r.ComplexityPct),
				RecencyPct:    derefFloatOrNaN(r.RecencyPct),
				ChurnPct:      derefFloatOrNaN(r.ChurnPct),
				FanIn:         r.FanIn,
				FanOut:        r.FanOut,
				Crossings:     r.Crossings,
				AgeDays:       derefIntOrNeg(r.AgeDays),
				Mods:          r.Mods,
				AxesUsed:      r.AxesUsed,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("health_score", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s  %5.1f  %s:%d  %s\n",
				r.Grade, r.Score, r.File, r.Line, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no health-scored symbols\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	resp := map[string]any{
		"symbols":      rows,
		"total":        totalRows,
		"truncated":    truncated,
		"weights":      healthWeightsMap(),
		"distribution": distribution,
	}
	if truncated {
		resp["limit"] = limit
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// healthWeightsMap returns the per-axis weight set as a JSON-shaped
// map. Centralised so the rollup and symbol paths emit identical
// shapes.
func healthWeightsMap() map[string]float64 {
	return map[string]float64{
		"coverage":   healthWeightCoverage,
		"complexity": healthWeightComplexity,
		"recency":    healthWeightRecency,
		"churn":      healthWeightChurn,
	}
}

// computeHealthDistribution returns mean / median / stddev / Gini /
// per-grade counts over the input row set. Centralised so the
// symbol-mode and rollup-mode responses use the same population
// summary.
func computeHealthDistribution(rows []healthScoreRow) healthDistribution {
	out := healthDistribution{
		GradeCounts: map[string]int{"A": 0, "B": 0, "C": 0, "D": 0, "F": 0},
		Total:       len(rows),
	}
	if len(rows) == 0 {
		return out
	}
	scores := make([]float64, len(rows))
	var sum float64
	for i, r := range rows {
		scores[i] = r.Score
		sum += r.Score
		out.GradeCounts[r.Grade]++
	}
	out.Mean = sum / float64(len(scores))
	// Sample stddev (n-1) is overkill for a population summary;
	// stick with the population formula matching FindHotspots.
	var variance float64
	for _, s := range scores {
		d := s - out.Mean
		variance += d * d
	}
	variance /= float64(len(scores))
	out.StdDev = math.Sqrt(variance)

	sorted := append([]float64(nil), scores...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		out.Median = (sorted[mid-1] + sorted[mid]) / 2
	} else {
		out.Median = sorted[mid]
	}

	out.Gini = giniCoefficient(sorted)
	out.Mean = roundTo(out.Mean, 2)
	out.Median = roundTo(out.Median, 2)
	out.StdDev = roundTo(out.StdDev, 2)
	out.Gini = roundTo(out.Gini, 4)
	return out
}

// giniCoefficient computes the Gini index over an already-sorted
// ascending slice of non-negative values. 0 = perfectly equal;
// approaches 1 = maximally unequal. Standard formula:
//
//	G = ( 2 · Σ i·x_i / (n · Σ x_i) ) − (n+1)/n
//
// Bails to 0 on the trivial cases (empty / all-zero) since dividing
// by zero would produce NaN and the consumer reads "0" as the
// "no inequality" signal anyway.
func giniCoefficient(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	var sum, weighted float64
	for i, v := range sorted {
		sum += v
		weighted += float64(i+1) * v
	}
	if sum == 0 {
		return 0
	}
	return (2*weighted)/(float64(n)*sum) - float64(n+1)/float64(n)
}

// rollupHealthBy aggregates per-symbol rows into per-key bins.
// keyFn picks the bin (file path / repo prefix). The resulting rows
// are sorted ascending by AvgScore so the worst bin surfaces first
// — mirrors the per-symbol sort order.
func rollupHealthBy(rows []healthScoreRow, scope string, keyFn func(healthScoreRow) string) []healthRollupRow {
	bins := map[string]*healthRollupRow{}
	for _, r := range rows {
		key := keyFn(r)
		if key == "" {
			continue
		}
		bin, ok := bins[key]
		if !ok {
			bin = &healthRollupRow{
				Scope:      scope,
				Key:        key,
				MinScore:   r.Score,
				MaxScore:   r.Score,
				GradeCount: map[string]int{"A": 0, "B": 0, "C": 0, "D": 0, "F": 0},
			}
			bins[key] = bin
		}
		if r.Score < bin.MinScore {
			bin.MinScore = r.Score
		}
		if r.Score > bin.MaxScore {
			bin.MaxScore = r.Score
		}
		bin.AvgScore += r.Score // running sum, divided below
		bin.Symbols++
		bin.GradeCount[r.Grade]++
	}

	out := make([]healthRollupRow, 0, len(bins))
	for _, b := range bins {
		if b.Symbols > 0 {
			b.AvgScore = roundTo(b.AvgScore/float64(b.Symbols), 2)
			b.MinScore = roundTo(b.MinScore, 2)
			b.MaxScore = roundTo(b.MaxScore, 2)
		}
		b.Grade = scoreGrade(b.AvgScore)
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AvgScore != out[j].AvgScore {
			return out[i].AvgScore < out[j].AvgScore
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// repoPrefixForPath returns the indexed repo prefix that owns the
// given graph file path, read through g. Falls back to the path's
// first component when no tracked repo claims it — keeps the rollup
// defined on single-repo setups that don't carry a RepoPrefix at all.
func repoPrefixForPath(g graph.Reader, path string) string {
	if path == "" {
		return ""
	}
	// Match against the KindFile node so we read the prefix the
	// indexer stamped. Cheap lookup — the graph indexes nodes by
	// ID already and KindFile IDs equal the file path.
	if g != nil {
		if n := g.GetNode(path); n != nil && n.RepoPrefix != "" {
			return n.RepoPrefix
		}
	}
	if idx := strings.Index(path, "/"); idx > 0 {
		return path[:idx]
	}
	return path
}

// recencyScore maps days-since-last-commit to a 0..100 health value.
// Piecewise linear so the curve is predictable to a human auditor;
// no exponential decay because the threshold cliffs already encode
// the operational meaning ("fresh" vs "stale" vs "dead").
func recencyScore(ageDays int) float64 {
	switch {
	case ageDays <= healthRecencyFreshDays:
		return 100
	case ageDays <= healthRecencyOKDays:
		// 30..365 → 100..50
		span := float64(healthRecencyOKDays - healthRecencyFreshDays)
		over := float64(ageDays - healthRecencyFreshDays)
		return 100 - 50*(over/span)
	case ageDays <= healthRecencyDeadDays:
		// 365..1095 → 50..0
		span := float64(healthRecencyDeadDays - healthRecencyOKDays)
		over := float64(ageDays - healthRecencyOKDays)
		return 50 - 50*(over/span)
	default:
		return 0
	}
}

// scoreGrade is the single source of truth for score → grade mapping.
// A-F split with five-point bands so border cases (84.99 vs 85.01)
// fall predictably without a tie-break rule.
func scoreGrade(score float64) string {
	switch {
	case score >= 85:
		return "A"
	case score >= 70:
		return "B"
	case score >= 55:
		return "C"
	case score >= 40:
		return "D"
	default:
		return "F"
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func derefFloatOrNaN(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func derefIntOrNeg(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
