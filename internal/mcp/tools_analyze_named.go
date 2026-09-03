package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/config"
)

// ---------------------------------------------------------------------------
// analyze kind=named
// ---------------------------------------------------------------------------
//
// Named query bundles — reusable, named selections over the bundled
// structural-detector library. A bundle picks detectors by tag or by
// name; running it (`analyze kind=named name=<q>`) fans every selected
// detector across the codebase and aggregates the matches, exactly
// like analyze kind=sast does for a whole category.
//
// Ten bundles ship built-in (the common SAST classes); a project adds
// its own via the `.gortex.yaml::queries` block. With no name argument
// the tool lists every available bundle.

// SetNamedQueries installs the config-defined `queries:` bundles.
// Called by the server / daemon entrypoint right after NewServer.
func (s *Server) SetNamedQueries(q []config.NamedQuery) {
	s.namedQueries = q
}

// builtinNamedQueries are the shipped bundles — curated tag
// selections over the bundled SAST / hygiene detector catalog.
func builtinNamedQueries() []config.NamedQuery {
	return []config.NamedQuery{
		{Name: "sql-injection", Description: "SQL injection — tainted input reaching a query", Tags: []string{"sqli", "injection"}},
		{Name: "command-injection", Description: "OS-command and dynamic-code injection", Tags: []string{"command-injection", "code-injection"}},
		{Name: "hardcoded-secrets", Description: "Hardcoded credentials, API keys, and tokens", Tags: []string{"secrets", "credentials"}},
		{Name: "weak-crypto", Description: "Weak hashes, ciphers, and transport protocols", Tags: []string{"crypto", "weak-hash", "weak-cipher", "weak-protocol"}},
		{Name: "xss", Description: "Cross-site scripting sinks", Tags: []string{"xss"}},
		{Name: "unsafe-deserialization", Description: "Unsafe deserialization of untrusted data", Tags: []string{"deserialization"}},
		{Name: "path-traversal", Description: "Path traversal and unsanitised file access", Tags: []string{"path-traversal"}},
		{Name: "ssrf", Description: "Server-side request forgery", Tags: []string{"ssrf"}},
		{Name: "xxe", Description: "XML external-entity processing", Tags: []string{"xxe", "xml"}},
		{Name: "debug-leftovers", Description: "Debug code left on production paths", Tags: []string{"debug-leftover", "debug"}},
	}
}

// resolvedNamedQueries merges the built-in bundles with the
// config-defined ones; a config bundle overrides a built-in of the
// same name.
func (s *Server) resolvedNamedQueries() map[string]config.NamedQuery {
	out := make(map[string]config.NamedQuery)
	for _, q := range builtinNamedQueries() {
		out[q.Name] = q
	}
	for _, q := range s.namedQueries {
		if name := strings.TrimSpace(q.Name); name != "" {
			out[name] = q
		}
	}
	return out
}

func (s *Server) handleAnalyzeNamed(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	name := strings.TrimSpace(stringArg(args, "name"))
	queries := s.resolvedNamedQueries()

	if name == "" {
		type queryInfo struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			Detectors   int    `json:"detectors"`
		}
		infos := make([]queryInfo, 0, len(queries))
		for _, q := range queries {
			infos = append(infos, queryInfo{
				Name:        q.Name,
				Description: q.Description,
				Detectors:   len(selectBundleDetectors(q)),
			})
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"queries": infos,
			"total":   len(infos),
		})
	}

	q, ok := queries[name]
	if !ok {
		avail := make([]string, 0, len(queries))
		for n := range queries {
			avail = append(avail, n)
		}
		sort.Strings(avail)
		return mcp.NewToolResultError(fmt.Sprintf(
			"unknown named query %q (available: %s)", name, strings.Join(avail, ", "))), nil
	}

	detectors := selectBundleDetectors(q)
	if len(detectors) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf(
			"named query %q selects no detectors — check its tags / detectors", name)), nil
	}

	languageFilter := parseCSVSet(stringArg(args, "language"))
	severityFilter := parseCSVSet(stringArg(args, "severity"))
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := intArg(args, "limit", 200)
	excludeTests, excludeTestsSet := boolArg(args, "exclude_tests")

	allowedRepos, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	targets, err := s.buildASTTargets(ctx, "", pathPrefix, allowedRepos)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	rows := make([]sastRow, 0, 64)
	summary := make(map[string]*sastSummary, len(detectors))
	var errsAcc []string
	for _, d := range detectors {
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
		entry := summary[d.Name]
		if entry == nil {
			entry = &sastSummary{Detector: d.Name, Severity: d.Severity, CWE: d.CWE}
			summary[d.Name] = entry
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
	s.enrichASTSymbolIDsContext(
		ctx,
		len(rows),
		func(index int) string { return rows[index].File },
		func(index int) int { return rows[index].Line },
		func(index int, id string) { rows[index].Symbol = id },
	)

	summaries := make([]sastSummary, 0, len(summary))
	for _, e := range summary {
		if e.Count > 0 {
			summaries = append(summaries, *e)
		}
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Count != summaries[j].Count {
			return summaries[i].Count > summaries[j].Count
		}
		return summaries[i].Detector < summaries[j].Detector
	})

	resp := map[string]any{
		"query":     name,
		"summary":   summaries,
		"matches":   rows,
		"total":     totalRows,
		"truncated": truncated,
		"detectors": len(detectors),
	}
	if truncated {
		resp["limit"] = limit
	}
	if len(errsAcc) > 0 {
		resp["errors"] = errsAcc
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// selectBundleDetectors resolves a named query to the detector set it
// runs — every detector carrying one of its Tags or named in
// Detectors, optionally floored by Severity.
func selectBundleDetectors(q config.NamedQuery) []*astquery.Detector {
	tagSet := make(map[string]bool)
	for _, t := range q.Tags {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			tagSet[t] = true
		}
	}
	nameSet := make(map[string]bool)
	for _, n := range q.Detectors {
		if n = strings.TrimSpace(n); n != "" {
			nameSet[n] = true
		}
	}
	floor := 0
	if strings.TrimSpace(q.Severity) != "" {
		floor = severityRank(q.Severity)
	}

	var out []*astquery.Detector
	for _, dn := range astquery.ListDetectors() {
		d := astquery.LookupDetector(dn)
		if d == nil {
			continue
		}
		matched := nameSet[d.Name]
		if !matched {
			for _, t := range d.Tags {
				if tagSet[strings.ToLower(t)] {
					matched = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		if floor > 0 && severityRank(d.Severity) < floor {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
