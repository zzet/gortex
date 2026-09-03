package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/review"
)

// ---------------------------------------------------------------------------
// analyze kind=sast  /  analyze kind=hygiene
// ---------------------------------------------------------------------------
//
// Bandit-parity SAST surface. Fan every detector whose Category is
// "sast" (resp. "hygiene") out across the indexed file set, aggregate
// rows by detector + severity + CWE, return a structured payload
// suitable for SARIF / DefectDojo / GitHub Code Scanning consumers.
//
// Filters mirror the unsafe_patterns surface (language / detector /
// severity / path_prefix / limit / exclude_tests) plus three new
// ones that only make sense for the larger SAST catalog:
//
//   - cwe       — comma-separated subset (e.g. "CWE-78,CWE-89").
//   - tag       — comma-separated subset against Detector.Tags
//                 (e.g. "injection,xss"). Lets the agent run "every
//                 injection-class rule" without enumerating names.
//   - kinds_only — when true, return only the per-category breakdown
//                 (rows omitted). Useful for the "what's my SAST
//                 surface look like" snapshot without paying row bytes.

type sastRow struct {
	Detector string   `json:"detector"`
	Severity string   `json:"severity"`
	Category string   `json:"category"`
	CWE      string   `json:"cwe,omitempty"`
	OWASP    string   `json:"owasp,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Language string   `json:"language"`
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Symbol   string   `json:"symbol,omitempty"`
	Text     string   `json:"text,omitempty"`
}

type sastSummary struct {
	Detector string `json:"detector"`
	Severity string `json:"severity"`
	CWE      string `json:"cwe,omitempty"`
	Count    int    `json:"count"`
}

type sastCWEBucket struct {
	CWE   string `json:"cwe"`
	Count int    `json:"count"`
}

func sortSASTMatchesByPriority(matches []astquery.Match) {
	sort.SliceStable(matches, func(i, j int) bool {
		ri, rj := severityRank(matches[i].Severity), severityRank(matches[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if matches[i].Detector != matches[j].Detector {
			return matches[i].Detector < matches[j].Detector
		}
		if matches[i].File != matches[j].File {
			return matches[i].File < matches[j].File
		}
		return matches[i].Line < matches[j].Line
	})
}

func (s *Server) prepareReviewSASTMatchesContext(
	ctx context.Context,
	matches []astquery.Match,
	limit int,
	enrich bool,
) {
	sortSASTMatchesByPriority(matches)
	if !enrich {
		return
	}
	enrichCount := len(matches)
	if limit > 0 && enrichCount > limit {
		enrichCount = limit
	}
	s.enrichASTMatchesContext(ctx, matches[:enrichCount])
}

// handleAnalyzeSAST runs the category bundle (sast or hygiene).
func (s *Server) handleAnalyzeSAST(ctx context.Context, req mcp.CallToolRequest, kind string) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	languageFilter := parseCSVSet(stringArg(args, "language"))
	detectorFilter := parseCSVSet(stringArg(args, "detector"))
	severityFilter := parseCSVSet(stringArg(args, "severity"))
	cweFilter := parseCSVSet(stringArg(args, "cwe"))
	tagFilter := parseCSVSet(stringArg(args, "tag"))
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := intArg(args, "limit", 200)
	excludeTests, excludeTestsSet := boolArg(args, "exclude_tests")
	kindsOnly := false
	if v, ok := args["kinds_only"].(bool); ok {
		kindsOnly = v
	}

	allowedRepos, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	targets, err := s.buildASTTargets(ctx, "", pathPrefix, allowedRepos)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	bundle := astquery.DetectorsByCategory(kind)
	if len(bundle) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("analyze %s: no detectors registered for category %q", kind, kind)), nil
	}

	// Reject unknown detector names early — same UX as unsafe_patterns.
	if len(detectorFilter) > 0 {
		known := make(map[string]struct{}, len(bundle))
		for _, d := range bundle {
			known[d.Name] = struct{}{}
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
				"analyze %s: unknown detector(s) %s for this category",
				kind, strings.Join(unknown, ","),
			)), nil
		}
	}

	rows := make([]sastRow, 0, 64)
	summary := make(map[string]*sastSummary, len(bundle))
	cweBuckets := make(map[string]*sastCWEBucket, len(bundle))
	var errsAcc []string

	// detMeta carries per-detector taxonomy (category / CWE / OWASP /
	// tags) keyed by name so row + rollup metadata survives the
	// graph-grounding post-pass, which works on the flat Match list.
	detMeta := make(map[string]*astquery.Detector, len(bundle))
	var collected []astquery.Match

	for _, d := range bundle {
		if len(detectorFilter) > 0 {
			if _, ok := detectorFilter[d.Name]; !ok {
				continue
			}
		}
		if len(cweFilter) > 0 {
			if _, ok := cweFilter[strings.ToLower(d.CWE)]; !ok {
				continue
			}
		}
		if len(tagFilter) > 0 {
			has := false
			for _, t := range d.Tags {
				if _, ok := tagFilter[strings.ToLower(t)]; ok {
					has = true
					break
				}
			}
			if !has {
				continue
			}
		}

		detMeta[d.Name] = d

		opts := astquery.Options{
			Detector: d.Name,
			Targets:  targets,
			Resolver: astquery.DefaultLanguageResolver,
			Limit:    5000,
		}
		if excludeTestsSet {
			opts.ExcludeTests = excludeTests
		} else {
			opts.ExcludeTests = true
		}

		res, runErr := astquery.Run(ctx, opts)
		if runErr != nil {
			errsAcc = append(errsAcc, fmt.Sprintf("%s: %v", d.Name, runErr))
			continue
		}
		errsAcc = append(errsAcc, res.Errors...)

		// Ensure a summary entry exists even when grounding later
		// removes every row for this detector (Count stays 0 and the
		// entry is dropped in the rollup pass).
		if summary[d.Name] == nil {
			summary[d.Name] = &sastSummary{Detector: d.Name, Severity: d.Severity, CWE: d.CWE}
		}

		for _, m := range res.Matches {
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
			collected = append(collected, m)
		}
	}

	// Graph-grounding post-pass — the load-bearing FP-reduction step.
	// The review detectors emit undecidable-from-AST-alone rows (N+1
	// query-in-loop, check-then-act) optimistically; here, one layer
	// up from the engine where the request's graph reader is reachable,
	// we drop the rows the resolved call / loop metadata refutes. Only
	// the review bundle is grounded; sast / hygiene / domain pass through.
	if kind == "review" {
		// Establish the same global priority the final rows use before the
		// bounded enrichment pass. Grounding preserves this order. Only the
		// requested result prefix is enriched: grounding may drop a prefix
		// row, but admitting later rows would exceed the caller's work bound.
		s.prepareReviewSASTMatchesContext(ctx, collected, limit, !kindsOnly)
		collected = review.GroundReviewMatches(s.readerFor(ctx), collected)
	}

	for _, m := range collected {
		d := detMeta[m.Detector]
		if d == nil {
			continue
		}
		entry := summary[d.Name]
		if entry == nil {
			entry = &sastSummary{Detector: d.Name, Severity: d.Severity, CWE: d.CWE}
			summary[d.Name] = entry
		}
		rows = append(rows, sastRow{
			Detector: m.Detector,
			Severity: m.Severity,
			Category: d.Category,
			CWE:      d.CWE,
			OWASP:    d.OWASP,
			Tags:     append([]string(nil), d.Tags...),
			Language: m.Language,
			File:     m.File,
			Line:     m.Line,
			Symbol:   m.SymbolID,
			Text:     m.Text,
		})
		entry.Count++
		if d.CWE != "" {
			b := cweBuckets[d.CWE]
			if b == nil {
				b = &sastCWEBucket{CWE: d.CWE}
				cweBuckets[d.CWE] = b
			}
			b.Count++
		}
	}

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
	if !kindsOnly && kind != "review" {
		s.enrichASTSymbolIDsContext(
			ctx,
			len(rows),
			func(index int) string { return rows[index].File },
			func(index int) int { return rows[index].Line },
			func(index int, id string) { rows[index].Symbol = id },
		)
	}

	summaries := make([]sastSummary, 0, len(summary))
	for _, entry := range summary {
		if entry.Count == 0 {
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

	cweRollup := make([]sastCWEBucket, 0, len(cweBuckets))
	for _, b := range cweBuckets {
		cweRollup = append(cweRollup, *b)
	}
	sort.Slice(cweRollup, func(i, j int) bool {
		if cweRollup[i].Count != cweRollup[j].Count {
			return cweRollup[i].Count > cweRollup[j].Count
		}
		return cweRollup[i].CWE < cweRollup[j].CWE
	})

	resp := map[string]any{
		"summary":   summaries,
		"cwes":      cweRollup,
		"total":     totalRows,
		"truncated": truncated,
		"category":  kind,
	}
	if !kindsOnly {
		resp["matches"] = rows
	}
	if truncated {
		resp["limit"] = limit
	}
	if len(errsAcc) > 0 {
		resp["errors"] = errsAcc
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}
