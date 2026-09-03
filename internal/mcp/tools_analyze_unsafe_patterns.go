package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/astquery"
)

// ---------------------------------------------------------------------------
// analyze kind=unsafe_patterns
// ---------------------------------------------------------------------------
//
// Bundled scan for unsafe / panic-prone primitives across every
// supported language. Wraps the per-language astquery detectors
// listed in astquery.UnsafePatternDetectors, fans them out across
// the indexed file set, and aggregates the matches into one
// per-site row plus a per-detector summary.
//
// Why a bundled kind: the underlying detectors are already
// invocable individually via `search_ast detector=…` — the bundle
// exists so an agent can ask "show me every panic-prone site in
// this repo" with one call instead of N calls, while still being
// able to narrow by `language`, `detector`, `path_prefix`, and
// `severity`.

type unsafePatternRow struct {
	Detector string `json:"detector"`
	Severity string `json:"severity"`
	Language string `json:"language"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Symbol   string `json:"symbol,omitempty"`
	Text     string `json:"text,omitempty"`
}

type unsafePatternSummary struct {
	Detector string `json:"detector"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
}

// handleAnalyzeUnsafePatterns runs the bundled unsafe-pattern
// detectors and returns aggregated rows + per-detector counts.
//
// Filters (all optional):
//   - language     — comma-separated subset (rust,python,javascript,typescript,go).
//   - detector     — comma-separated subset (must be a member of
//     astquery.UnsafePatternDetectors). Lets the agent
//     narrow the bundle without falling back to
//     individual search_ast calls.
//   - severity     — comma-separated subset (error,warning,info).
//   - path_prefix  — keep matches whose file path starts with this.
//   - limit        — cap rows (default 200; matches error_surface's UX).
//   - exclude_tests — override per-detector default (defaults to true).
func (s *Server) handleAnalyzeUnsafePatterns(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	languageFilter := parseCSVSet(stringArg(args, "language"))
	detectorFilter := parseCSVSet(stringArg(args, "detector"))
	severityFilter := parseCSVSet(stringArg(args, "severity"))
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := intArg(args, "limit", 200)
	excludeTests, excludeTestsSet := boolArg(args, "exclude_tests")

	allowedRepos, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Build target list once; reused across every detector to avoid
	// re-walking the KindFile node set per rule.
	targets, err := s.buildASTTargets(ctx, "", pathPrefix, allowedRepos)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Reject unknown detector names early so the agent gets a
	// pointed error instead of an empty result.
	if len(detectorFilter) > 0 {
		known := make(map[string]struct{}, len(astquery.UnsafePatternDetectors))
		for _, n := range astquery.UnsafePatternDetectors {
			known[n] = struct{}{}
		}
		var unknown []string
		for n := range detectorFilter {
			if _, ok := known[n]; !ok {
				unknown = append(unknown, n)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return mcp.NewToolResultError(fmt.Sprintf(
				"analyze unsafe_patterns: unknown detector(s) %s (valid: %s)",
				strings.Join(unknown, ","),
				strings.Join(astquery.UnsafePatternDetectors, ","),
			)), nil
		}
	}

	rows := make([]unsafePatternRow, 0, 64)
	summary := make(map[string]*unsafePatternSummary, len(astquery.UnsafePatternDetectors))
	var errsAcc []string

	for _, name := range astquery.UnsafePatternDetectors {
		if len(detectorFilter) > 0 {
			if _, ok := detectorFilter[name]; !ok {
				continue
			}
		}
		opts := astquery.Options{
			Detector: name,
			Targets:  targets,
			Resolver: astquery.DefaultLanguageResolver,
			// Generous per-detector cap; the outer `limit` is the
			// agent-facing budget. Picking 5000 protects against a
			// pathological repo where one detector returns tens of
			// thousands of rows and starves the rest of the bundle.
			Limit: 5000,
		}
		if excludeTestsSet {
			opts.ExcludeTests = excludeTests
		} else {
			// Default to true — every bundled detector already
			// opts in, but we force the engine flag too so a
			// caller that wires in a detector with ExcludeTests=
			// false doesn't accidentally bleed test noise into
			// the bundle.
			opts.ExcludeTests = true
		}

		res, runErr := astquery.Run(ctx, opts)
		if runErr != nil {
			errsAcc = append(errsAcc, fmt.Sprintf("%s: %v", name, runErr))
			continue
		}
		errsAcc = append(errsAcc, res.Errors...)

		entry := summary[name]
		if entry == nil {
			sev := ""
			if len(res.Matches) > 0 {
				sev = res.Matches[0].Severity
			}
			entry = &unsafePatternSummary{Detector: name, Severity: sev}
			summary[name] = entry
		}

		for _, m := range res.Matches {
			if entry.Severity == "" {
				entry.Severity = m.Severity
			}
			if len(languageFilter) > 0 {
				if _, ok := languageFilter[strings.ToLower(m.Language)]; !ok {
					continue
				}
			}
			if len(severityFilter) > 0 {
				if _, ok := severityFilter[strings.ToLower(m.Severity)]; !ok {
					continue
				}
			}
			rows = append(rows, unsafePatternRow{
				Detector: m.Detector,
				Severity: m.Severity,
				Language: m.Language,
				File:     m.File,
				Line:     m.Line,
				Symbol:   m.SymbolID,
				Text:     m.Text,
			})
			entry.Count++
		}
	}

	// Stable order: severity-rank (error > warning > info), then
	// detector, then file, then line. Severity-first keeps the
	// most impactful rows at the top when the agent truncates by
	// `limit`.
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := severityRank(rows[i].Severity), severityRank(rows[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if rows[i].Detector != rows[j].Detector {
			return rows[i].Detector < rows[j].Detector
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})

	totalRows := len(rows)
	truncated := false
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}
	s.enrichASTSymbolIDsContext(
		ctx,
		len(rows),
		func(index int) string { return rows[index].File },
		func(index int) int { return rows[index].Line },
		func(index int, id string) { rows[index].Symbol = id },
	)

	summaries := make([]unsafePatternSummary, 0, len(summary))
	for _, name := range astquery.UnsafePatternDetectors {
		if len(detectorFilter) > 0 {
			if _, ok := detectorFilter[name]; !ok {
				continue
			}
		}
		entry := summary[name]
		if entry == nil {
			continue
		}
		summaries = append(summaries, *entry)
	}
	sort.Slice(summaries, func(i, j int) bool {
		ri, rj := severityRank(summaries[i].Severity), severityRank(summaries[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if summaries[i].Count != summaries[j].Count {
			return summaries[i].Count > summaries[j].Count
		}
		return summaries[i].Detector < summaries[j].Detector
	})

	if s.isGCX(ctx, req) {
		items := make([]unsafePatternItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, unsafePatternItem(r))
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("unsafe_patterns", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			sym := r.Symbol
			if sym == "" {
				sym = "-"
			}
			fmt.Fprintf(&b, "%-7s  %-26s  %s:%d  %s\n",
				r.Severity, r.Detector, r.File, r.Line, sym)
		}
		if len(rows) == 0 {
			b.WriteString("no unsafe patterns\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	resp := map[string]any{
		"matches":   rows,
		"summary":   summaries,
		"total":     totalRows,
		"truncated": truncated,
	}
	if truncated {
		resp["limit"] = limit
	}
	if len(errsAcc) > 0 {
		resp["errors"] = errsAcc
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// parseCSVSet splits "rust,python" into {rust:{}, python:{}}.
// Empty input returns nil so callers can `len(m) == 0` to short-
// circuit the filter.
func parseCSVSet(in string) map[string]struct{} {
	in = strings.TrimSpace(in)
	if in == "" {
		return nil
	}
	parts := strings.Split(in, ",")
	out := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "" {
			continue
		}
		out[p] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// severityRank maps a severity label to a sort key so "error" beats
// "warning" beats "info" without a fragile string comparison.
func severityRank(sev string) int {
	switch strings.ToLower(sev) {
	case "error":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}
