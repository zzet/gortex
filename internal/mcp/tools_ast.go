package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// registerASTTools wires the `search_ast` MCP tool: a structural,
// graph-aware code search powered by tree-sitter queries.
//
// Two surfaces, exposed through one tool:
//  1. Bundled detectors (`detector: "<name>"`) — pre-baked rules
//     for high-signal anti-patterns. Cross-language by design;
//     one detector ships per-language patterns and the engine
//     picks the right one per file.
//  2. Raw tree-sitter S-expression patterns (`pattern: "..."`,
//     `language: "..."`) for callers who want full power. The
//     pattern syntax is tree-sitter's standard query language —
//     capture nodes with `@name`, anchor with `@match`, predicates
//     `(#eq? @x "literal")` / `(#match? @x "regex")`.
//
// Beyond ast-grep's surface, every match is enriched with
//   - `symbol_id` / `symbol_name` — the enclosing function/method/
//     closure resolved from the graph at result time.
//   - graph-aware filters: scope by `path_prefix`, `language`,
//     `repo` / `project` / `ref`, and `min_fan_in_of_enclosing_func`.
//   - `excludes_tests` defaulting to true for detectors so test
//     fixtures don't drown real findings.
func (s *Server) registerASTTools() {
	s.addTool(
		mcp.NewTool("search_ast",
			mcp.WithDescription(buildSearchASTDescription()),
			mcp.WithString("pattern", mcp.Description("Tree-sitter S-expression query. Capture nodes with `@name`, anchor the match span with `@match`. Predicates: `(#eq? @x \"literal\")`, `(#match? @x \"regex\")`. Mutually exclusive with `detector`.")),
			mcp.WithString("detector", mcp.Description("Bundled rule name. Run with no args (or with `detector: \"\"`) and check the description for the canonical list. Mutually exclusive with `pattern`.")),
			mcp.WithString("language", mcp.Description("Restrict pattern matching to a single language (\"go\", \"python\", \"javascript\", \"typescript\", \"ruby\", \"java\", \"kotlin\", \"scala\", \"rust\", \"elixir\", \"php\", \"c\", \"cpp\", \"csharp\", \"bash\"). Required when `pattern` is set; ignored for detectors (the detector decides which languages to scan).")),
			mcp.WithString("path_prefix", mcp.Description("Restrict the file set to graph paths under this prefix (e.g. `internal/payment/`).")),
			mcp.WithBoolean("exclude_tests", mcp.Description("Drop matches in test files (`_test.go`, `*.spec.ts`, `tests/`, …). Defaults to true for detectors, false for raw patterns.")),
			mcp.WithNumber("min_fan_in_of_enclosing_func", mcp.Description("Only return matches whose enclosing function has at least this many incoming edges (callers + references). Useful for narrowing audits to load-bearing code paths.")),
			mcp.WithNumber("limit", mcp.Description("Maximum matches to return (default: 50)")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleSearchAST,
	)
}

// handleSearchAST is the MCP entry point. It builds the target file
// list from the graph (applying scope predicates), wires a graph-
// backed SymbolLookup, runs the engine, and applies post-match graph
// filters (currently `min_fan_in_of_enclosing_func`) before
// returning. Stays single-pass over the graph's KindFile nodes so
// even very large indexes don't pay multiple O(n) walks.
func (s *Server) handleSearchAST(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pattern := strings.TrimSpace(stringArg(args, "pattern"))
	detector := strings.TrimSpace(stringArg(args, "detector"))
	// detector:"help" dumps the full detector catalogue (moved out of the
	// tool description to keep the cold schema lean). No graph needed.
	if strings.EqualFold(detector, "help") {
		return mcp.NewToolResultText(searchASTHelpResult()), nil
	}
	if pattern == "" && detector == "" {
		return mcp.NewToolResultError("search_ast: either `pattern` or `detector` is required (call with no args to see the bundled detector list in the tool description)"), nil
	}
	if pattern != "" && detector != "" {
		return mcp.NewToolResultError("search_ast: `pattern` and `detector` are mutually exclusive"), nil
	}

	language := strings.ToLower(strings.TrimSpace(stringArg(args, "language")))
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := intArg(args, "limit", 0)
	excludeTests, excludeTestsSet := boolArg(args, "exclude_tests")
	minFanIn := intArg(args, "min_fan_in_of_enclosing_func", 0)

	if pattern != "" && language == "" {
		return mcp.NewToolResultError("search_ast: `language` is required when using a raw `pattern`"), nil
	}

	allowedRepos, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	targets, err := s.buildASTTargets(ctx, language, pathPrefix, allowedRepos)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	opts := astquery.Options{
		Pattern:  pattern,
		Detector: detector,
		Language: language,
		Targets:  targets,
		Resolver: astquery.DefaultLanguageResolver,
		Limit:    limit,
	}
	// Honor explicit override; otherwise let the engine apply
	// its per-mode default (true for detectors, false for raw
	// patterns).
	if excludeTestsSet {
		opts.ExcludeTests = excludeTests
	} else if detector != "" {
		opts.ExcludeTests = true
	}

	res, runErr := astquery.Run(ctx, opts)
	if runErr != nil {
		return mcp.NewToolResultError(runErr.Error()), nil
	}
	s.enrichASTMatchesContext(ctx, res.Matches)

	if minFanIn > 0 {
		res.Matches = filterByMinFanIn(s.readerFor(ctx), res.Matches, minFanIn)
		res.Total = len(res.Matches)
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"matches":      res.Matches,
		"total":        res.Total,
		"truncated":    res.Truncated,
		"files_walked": res.FilesWalked,
		"errors":       res.Errors,
	})
}

// buildASTTargets walks the graph's KindFile nodes once and assembles
// the `Target` list the engine expects, applying language /
// path_prefix / repo filters before any tree-sitter parse fires.
//
// Path resolution: KindFile nodes carry repo-prefixed paths; the
// engine needs absolute paths to read file bytes, so we resolve via
// `s.resolveGraphPath` (which knows the repo roots).
func (s *Server) buildASTTargets(ctx context.Context, language, pathPrefix string, allowedRepos map[string]bool) ([]astquery.Target, error) {
	if s.graph == nil {
		return nil, fmt.Errorf("search_ast: no graph available")
	}
	out := make([]astquery.Target, 0, 256)
	// File nodes are a fraction of the node table; iterating the
	// KindFile bucket via NodesByKind lets the backend stream only
	// those rows instead of materialising the full table over cgo.
	// Repo / language / path filters compose AND, so they stay Go-
	// side — they can't be projected onto the bucket index without
	// duplicating the predicate set across both call sites.
	// Base read on purpose: these nodes only supply absolute paths, and the
	// engine re-parses each file from disk anyway.
	for n := range s.graph.NodesByKind(graph.KindFile) {
		if n == nil {
			continue
		}
		if allowedRepos != nil && n.RepoPrefix != "" && !allowedRepos[n.RepoPrefix] {
			continue
		}
		if language != "" && !strings.EqualFold(n.Language, language) {
			continue
		}
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		abs, err := s.resolveNodePath(ctx, n)
		if err != nil {
			// Indexed file whose repo we can't currently
			// resolve (rare; happens during an in-flight
			// repo eviction). Skip rather than fail the run.
			continue
		}
		lang := strings.ToLower(n.Language)
		out = append(out, astquery.Target{
			AbsPath:   abs,
			GraphPath: n.FilePath,
			Language:  lang,
		})
		// .tsx files use the tsx grammar (a strict superset of the
		// typescript grammar that adds JSX nodes). Emit a parallel
		// target tagged "tsx" so JSX-using detectors can compile
		// cleanly without losing the "typescript" scan for
		// grammar-agnostic detectors.
		if lang == "typescript" && strings.HasSuffix(strings.ToLower(n.FilePath), ".tsx") {
			out = append(out, astquery.Target{
				AbsPath:   abs,
				GraphPath: n.FilePath,
				Language:  "tsx",
			})
		}
	}
	// Stable order so identical inputs produce identical outputs
	// across daemon restarts. Cheap; the file list is bounded.
	sort.Slice(out, func(i, j int) bool { return out[i].GraphPath < out[j].GraphPath })
	return out, nil
}

// filterByMinFanIn drops matches whose enclosing symbol has fewer
// than `min` incoming edges. Without an enclosing symbol, the
// match is preserved (we'd otherwise silently swallow file-level
// matches that legitimately have no caller graph).
func filterByMinFanIn(g graph.Reader, matches []astquery.Match, min int) []astquery.Match {
	if g == nil || min <= 0 {
		return matches
	}
	cache := make(map[string]int, len(matches))
	out := matches[:0]
	for _, m := range matches {
		if m.SymbolID == "" {
			out = append(out, m)
			continue
		}
		fanIn, ok := cache[m.SymbolID]
		if !ok {
			fanIn = len(g.GetInEdges(m.SymbolID))
			cache[m.SymbolID] = fanIn
		}
		if fanIn >= min {
			out = append(out, m)
		}
	}
	return out
}

// boolArg returns (value, set) — set is false when the caller didn't
// pass the key, so we can distinguish "unset" from "explicitly false".
func boolArg(args map[string]any, key string) (bool, bool) {
	raw, ok := args[key]
	if !ok {
		return false, false
	}
	if v, ok := raw.(bool); ok {
		return v, true
	}
	return false, false
}

// intArg pulls an int from the args map with a default. Tolerates
// the float64 unmarshalling MCP / JSON does on numeric values.
func intArg(args map[string]any, key string, def int) int {
	raw, ok := args[key]
	if !ok {
		return def
	}
	switch v := raw.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}
