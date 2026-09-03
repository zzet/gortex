package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// symbols_for_ranges is the lowering primitive that turns a set of
// (file, line-range) pairs into the graph symbols those ranges touch. It is
// the input adapter every range-shaped change source funnels through — an
// editor selection, an LSP code-action range, or a parsed diff hunk — so the
// rest of the change pipeline only ever has to reason about symbol IDs.

// rangeSpec is one (file, [start,end]) request.
type rangeSpec struct {
	File      string
	StartLine int
	EndLine   int
}

// rangeSymbolHit is one symbol a range resolved to.
type rangeSymbolHit struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// rangeSpecJSON is the wire shape of one entry in the `ranges` array. Both
// `file` and `path` are accepted for the file field; end_line defaults to
// start_line when omitted (a single-line range).
type rangeSpecJSON struct {
	File      string `json:"file"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// loweredRanges holds the symbols each (file, range) spec resolved to,
// deduplicated across all specs by symbol ID, plus the display paths of any
// files that could not be resolved or carry no indexed symbols, so the caller
// can report partial coverage rather than silently dropping them.
type loweredRanges struct {
	hits       []rangeSymbolHit
	unresolved []string
	saturated  []string
}

func (s *Server) lowerRangesContext(ctx context.Context, specs []rangeSpec) ([]rangeSymbolHit, []string) {
	lowered := s.lowerRangesDetailedContext(ctx, specs)
	unresolved := append(append([]string(nil), lowered.unresolved...), lowered.saturated...)
	sort.Strings(unresolved)
	return lowered.hits, dedupeStrings(unresolved)
}

func (s *Server) lowerRangesDetailedContext(ctx context.Context, specs []rangeSpec) loweredRanges {
	if s == nil || s.graph == nil || len(specs) == 0 {
		return loweredRanges{}
	}
	byGraphPath := make(map[string][]rangeSpec)
	displayOf := make(map[string]string)
	var unresolved []string
	for _, sp := range specs {
		absPath, relPath, err := s.resolveFilePath(ctx, sp.File)
		if err != nil {
			unresolved = append(unresolved, sp.File)
			continue
		}
		gp := s.resolveOverlayGraphPath(relPath, absPath)
		byGraphPath[gp] = append(byGraphPath[gp], sp)
		displayOf[gp] = relPath
	}
	if len(byGraphPath) == 0 {
		sort.Strings(unresolved)
		return loweredRanges{unresolved: dedupeStrings(unresolved)}
	}

	want := make(map[string]struct{}, len(byGraphPath))
	for gp := range byGraphPath {
		want[gp] = struct{}{}
	}
	indexes := s.buildFileSymbolIndexForPathsContext(ctx, want)

	seen := make(map[string]struct{})
	var hits []rangeSymbolHit
	var saturated []string
	for gp, specsForFile := range byGraphPath {
		idx := indexes[gp]
		if idx != nil && idx.saturated {
			saturated = append(saturated, displayOf[gp])
			continue
		}
		if idx == nil {
			unresolved = append(unresolved, displayOf[gp])
			continue
		}
		for _, sp := range specsForFile {
			for _, n := range idx.enclosingForRange(sp.StartLine, sp.EndLine) {
				if _, ok := seen[n.ID]; ok {
					continue
				}
				seen[n.ID] = struct{}{}
				hits = append(hits, rangeSymbolHit{
					ID:        n.ID,
					Name:      n.Name,
					Kind:      string(n.Kind),
					File:      n.FilePath,
					StartLine: n.StartLine,
					EndLine:   n.EndLine,
				})
			}
		}
	}
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].File != hits[b].File {
			return hits[a].File < hits[b].File
		}
		return hits[a].StartLine < hits[b].StartLine
	})
	sort.Strings(unresolved)
	sort.Strings(saturated)
	return loweredRanges{
		hits: hits, unresolved: dedupeStrings(unresolved), saturated: dedupeStrings(saturated),
	}
}

// parseRangeSpecs reads range specs from a request. Two forms are accepted:
//   - ranges: a JSON array of {file|path, start_line, end_line} objects.
//   - path + start_line [+ end_line]: a single-file convenience form.
func parseRangeSpecs(req mcp.CallToolRequest) ([]rangeSpec, error) {
	var specs []rangeSpec

	if raw := req.GetString("ranges", ""); raw != "" {
		var entries []rangeSpecJSON
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, fmt.Errorf("invalid ranges JSON: %w", err)
		}
		for i, e := range entries {
			file := e.File
			if file == "" {
				file = e.Path
			}
			if file == "" {
				return nil, fmt.Errorf("ranges[%d]: file is required", i)
			}
			if e.StartLine <= 0 {
				return nil, fmt.Errorf("ranges[%d]: start_line must be >= 1", i)
			}
			end := e.EndLine
			if end < e.StartLine {
				end = e.StartLine
			}
			specs = append(specs, rangeSpec{File: file, StartLine: e.StartLine, EndLine: end})
		}
		return specs, nil
	}

	if path := req.GetString("path", ""); path != "" {
		start := req.GetInt("start_line", 0)
		if start <= 0 {
			return nil, fmt.Errorf("start_line must be >= 1")
		}
		end := req.GetInt("end_line", start)
		if end < start {
			end = start
		}
		specs = append(specs, rangeSpec{File: path, StartLine: start, EndLine: end})
		return specs, nil
	}

	return nil, fmt.Errorf("provide either `ranges` (JSON array) or `path` + `start_line`")
}

func (s *Server) handleSymbolsForRanges(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s == nil || s.graph == nil {
		return mcp.NewToolResultError("symbols_for_ranges: server not fully initialised"), nil
	}
	specs, err := parseRangeSpecs(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	hits, unresolved := s.lowerRangesContext(ctx, specs)
	if hits == nil {
		hits = []rangeSymbolHit{}
	}
	resp := map[string]any{
		"symbols": hits,
		"total":   len(hits),
	}
	if len(unresolved) > 0 {
		resp["unresolved_files"] = unresolved
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// registerChangeContractTools registers the change-source lowering and
// verdict tools: symbols_for_ranges (this commit) and change_contract.
func (s *Server) registerChangeContractTools() {
	s.addTool(
		mcp.NewTool("symbols_for_ranges",
			mcp.WithDescription("Lower a set of (file, line-range) pairs to the graph symbols those ranges touch — the input adapter for any range-shaped change source (an editor selection, an LSP code-action range, a diff hunk). Returns the enclosing symbol(s) for each range, deduplicated by ID, so downstream impact / change-contract analysis only deals in symbol IDs. Pair with `analyze kind=impact ids:` to score the blast radius of a selection."),
			mcp.WithString("ranges", mcp.Description("JSON array of {file, start_line, end_line} objects (file or path; end_line defaults to start_line). Lines are 1-based. Use for multi-file or multi-range lowering.")),
			mcp.WithString("path", mcp.Description("Single-file convenience form: the file whose range to lower (use with start_line / end_line instead of `ranges`).")),
			mcp.WithNumber("start_line", mcp.Description("1-based start line for the single-file form.")),
			mcp.WithNumber("end_line", mcp.Description("1-based end line for the single-file form (defaults to start_line).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleSymbolsForRanges,
	)

	s.addTool(
		mcp.NewTool("change_contract",
			mcp.WithDescription("Run one change through the full pipeline — LOWER any change source to a changed-symbol set, PREDICT its blast radius, EVALUATE the guard / architecture rules, SCORE the risk, CLASSIFY the change, and EMIT one verdict envelope {verdict: allow|warn|refuse, reasons[], risk, classification, verification_command, stop_condition, edit_strategy}. The analysis advises; a pretooluse hook is the only thing that turns a `refuse` into a block. Sources: a WorkspaceEdit (true speculative simulation with broken-caller detection), a git diff range, an explicit symbol set, or file line-ranges."),
			mcp.WithString("source", mcp.Description("Change source: auto (default — pick the most specific input present), edit, diff, symbols, or ranges.")),
			mcp.WithString("lens", mcp.Description("Optional analysis lens. lens=api focuses the verdict on the public API surface — for each changed exported symbol it reports cross-file consumers and participating contracts (API-drift between two refs when paired with source=diff base=…).")),
			mcp.WithBoolean("risk_gate", mcp.Description("Enable the risk gate: load-bearing symbols (high fan-in / centrality) require a fresh impact-review ack or the verdict refuses. Also enabled by GORTEX_RISK_GATE.")),
			mcp.WithBoolean("ack", mcp.Description("Record a risk-gate acknowledgement for the changed symbols (stored as a development memory with a TTL) instead of emitting a verdict. A subsequent risk_gate run then clears those symbols until the ack expires.")),
			mcp.WithString("workspace_edit", mcp.Description("source=edit: an LSP WorkspaceEdit as a JSON string. Simulated speculatively (disk untouched) for broken callers / implementors and test targets.")),
			mcp.WithString("symbols", mcp.Description("source=symbols: comma-separated symbol IDs to treat as the changed set.")),
			mcp.WithString("ranges", mcp.Description("source=ranges: JSON array of {file, start_line, end_line} objects, lowered to enclosing symbols.")),
			mcp.WithString("path", mcp.Description("source=ranges single-file form: the file whose range to lower (with start_line / end_line).")),
			mcp.WithNumber("start_line", mcp.Description("1-based start line for the single-file ranges form.")),
			mcp.WithNumber("end_line", mcp.Description("1-based end line for the single-file ranges form (defaults to start_line).")),
			mcp.WithString("scope", mcp.Description("source=diff: unstaged (default), staged, all, or compare.")),
			mcp.WithString("base", mcp.Description("source=diff: base ref for a compare-scope diff (e.g. main); setting it implies scope=compare.")),
			mcp.WithString("repo", mcp.Description("source=diff: repository selector when more than one repo is tracked.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleChangeContract,
	)
}
