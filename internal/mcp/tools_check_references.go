package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// registerCheckReferencesTool wires check_references — a one-call
// composite that answers "is X used anywhere?" with a unified verdict
// and the underlying evidence. Pairs with safe_delete_symbol (N26 ✅)
// as the pre-delete sanity check: agents call check_references first;
// when referenced=false they know the delete is safe; when true the
// evidence list tells them what to update first.
//
// Composes three signals from the graph:
//   - in_edges_by_kind: every incoming dependency edge grouped by
//     kind (calls, implements, extends, references, instantiates, ...)
//   - same_name_elsewhere: other symbols sharing the literal name in
//     different files — useful when the named symbol *isn't* in the
//     graph (e.g. removed during refactor) or when an extractor missed
//     a binding edge
//   - importing_files: files importing the file the symbol lives in
//     — answers "does anyone consume this package at all?"
func (s *Server) registerCheckReferencesTool() {
	s.addTool(
		mcp.NewTool("check_references",
			mcp.WithDescription("Unified 'is this symbol used?' verdict. Returns {referenced, total_references, by_kind, callers, same_name_elsewhere, importing_files, evidence}. Use either `symbol_id` (preferred — anchored on the actual graph node) or `name` (when the node was removed but a literal-name check is still useful). Composes find_usages + name lookup + file-import scan into one call so agents don't have to chain three tools before deciding whether a delete or rename is safe."),
			mcp.WithString("symbol_id", mcp.Description("Symbol ID to check (preferred). When provided, scans every in-edge kind plus same-name nodes in other files plus importing files of the symbol's home file.")),
			mcp.WithString("name", mcp.Description("Symbol name to check by literal lookup (used when symbol_id is empty). Falls back to a graph-wide same-name scan.")),
			mcp.WithBoolean("exclude_tests", mcp.Description("Drop references whose origin file path looks like a test (_test.go, .test.ts, __tests__/, test_*). Default false.")),
			mcp.WithNumber("evidence_limit", mcp.Description("Cap on the evidence row count (default: 50).")),
			mcp.WithString("min_tier", mcp.Description("Minimum edge confidence tier (lsp_resolved / lsp_dispatch / ast_resolved / ast_inferred / text_matched). Empty = no filter.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleCheckReferences,
	)
}

type checkRefsEvidence struct {
	FromID   string `json:"from_id"`
	FromName string `json:"from_name,omitempty"`
	FilePath string `json:"file_path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Kind     string `json:"kind"`
	Tier     string `json:"tier,omitempty"`
}

type checkRefsSameName struct {
	ID       string `json:"id"`
	FilePath string `json:"file_path"`
	Kind     string `json:"kind"`
}

func (s *Server) handleCheckReferences(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbolID := strings.TrimSpace(req.GetString("symbol_id", ""))
	name := strings.TrimSpace(req.GetString("name", ""))
	if symbolID == "" && name == "" {
		return mcp.NewToolResultError("check_references requires `symbol_id` or `name`"), nil
	}
	excludeTests := req.GetBool("exclude_tests", false)
	evidenceLimit := max(req.GetInt("evidence_limit", 50), 1)
	minTier := strings.TrimSpace(req.GetString("min_tier", ""))

	g := s.readerFor(ctx)

	var target *graph.Node
	if symbolID != "" {
		target = g.GetNode(symbolID)
	}

	// If name wasn't given explicitly, derive it from the resolved
	// symbol so the same-name scan still runs.
	if name == "" && target != nil {
		name = target.Name
	}

	byKind := map[string]int{}
	evidence := make([]checkRefsEvidence, 0, evidenceLimit)
	callers := map[string]bool{}
	totalEdges := 0
	if target != nil {
		// Pre-filter the in-edges and batch-fetch the surviving
		// `From` nodes in one round-trip. On a disk backend the per-edge
		// GetNode pattern was a round-trip per inbound edge —
		// for heavily-referenced symbols (hundreds of callers) the
		// cost was dominant. One GetNodesByIDs gives us the same
		// data in a single bulk query.
		inEdges := g.GetInEdges(target.ID)
		fromIDs := make([]string, 0, len(inEdges))
		seenFrom := make(map[string]struct{}, len(inEdges))
		for _, e := range inEdges {
			if !isCheckRefEdge(e.Kind) {
				continue
			}
			if minTier != "" && !atOrAboveTier(string(e.Origin), minTier) {
				continue
			}
			if _, dup := seenFrom[e.From]; dup {
				continue
			}
			seenFrom[e.From] = struct{}{}
			fromIDs = append(fromIDs, e.From)
		}
		fromByID := g.GetNodesByIDs(fromIDs)

		for _, e := range inEdges {
			if !isCheckRefEdge(e.Kind) {
				continue
			}
			if minTier != "" && !atOrAboveTier(string(e.Origin), minTier) {
				continue
			}
			from := fromByID[e.From]
			if from != nil && excludeTests && isTestPath(from.FilePath) {
				continue
			}
			byKind[string(e.Kind)]++
			totalEdges++
			if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeCrossRepoCalls {
				callers[e.From] = true
			}
			if len(evidence) < evidenceLimit {
				ev := checkRefsEvidence{FromID: e.From, Kind: string(e.Kind), Tier: string(e.Origin)}
				if from != nil {
					ev.FromName = from.Name
					ev.FilePath = from.FilePath
					ev.Line = from.StartLine
				}
				// Prefer the edge's own call-site line / file when
				// they're populated. Two distinct calls from the
				// same caller used to collapse onto the caller's
				// start line — same shape as the find_usages bug
				// fixed earlier; the evidence builder owns its own
				// copy of that pattern.
				if e.Line > 0 {
					ev.Line = e.Line
				}
				if e.FilePath != "" {
					ev.FilePath = e.FilePath
				}
				evidence = append(evidence, ev)
			}
		}
	}

	// Same-name scan — other graph nodes with the same Name living
	// in a different file. Useful when the graph hasn't materialised
	// a binding edge but the literal identifier is in use.
	sameName := []checkRefsSameName{}
	if name != "" {
		excludePath := ""
		if target != nil {
			excludePath = target.FilePath
		}
		for _, n := range s.scopedNodesLight(ctx) {
			if n.Name != name {
				continue
			}
			if target != nil && n.ID == target.ID {
				continue
			}
			if excludePath != "" && n.FilePath == excludePath {
				continue
			}
			if excludeTests && isTestPath(n.FilePath) {
				continue
			}
			sameName = append(sameName, checkRefsSameName{
				ID: n.ID, FilePath: n.FilePath, Kind: string(n.Kind),
			})
		}
	}

	// Importing-files scan — every file whose nodes carry an
	// EdgeImports edge into the target's FilePath. Backends that
	// implement graph.FileImporters serve this from one query
	// (no AllEdges() materialisation, no per-edge GetNode round-
	// trip). The legacy AllEdges + per-edge GetNode loop stays as
	// the fallback for backends that don't ship the capability.
	importingFiles := collectImportingFiles(g, target, excludeTests)

	referenced := totalEdges > 0 || len(sameName) > 0 || len(importingFiles) > 0

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"referenced":          referenced,
		"total_references":    totalEdges,
		"by_kind":             byKind,
		"caller_count":        len(callers),
		"same_name_elsewhere": sameName,
		"importing_files":     importingFiles,
		"evidence":            evidence,
		"symbol_id":           symbolID,
		"name":                name,
		"excluded_tests":      excludeTests,
	})
}

// collectImportingFiles answers "which files import the file that
// holds target?". Prefers the graph.FileImporters capability when
// the backend implements it — that path runs one query
// instead of an AllEdges() scan plus 2× per-edge GetNode round-trip.
// Returns a sorted, deduplicated, optionally test-filtered slice
// of file paths.
//
// When target is nil or has no FilePath the question is undefined;
// returns an empty slice (consistent with the legacy behaviour).
func collectImportingFiles(g graph.Reader, target *graph.Node, excludeTests bool) []string {
	importingFiles := []string{}
	if target == nil || target.FilePath == "" {
		return importingFiles
	}
	seen := map[string]bool{}
	add := func(fromFile string) {
		if fromFile == "" {
			return
		}
		if excludeTests && isTestPath(fromFile) {
			return
		}
		if seen[fromFile] {
			return
		}
		seen[fromFile] = true
		importingFiles = append(importingFiles, fromFile)
	}

	if fi, ok := g.(graph.FileImporters); ok {
		for _, row := range fi.FileImporters(target.FilePath) {
			add(row.FromFile)
		}
		sort.Strings(importingFiles)
		return importingFiles
	}

	// Fallback: pull every edge and filter Go-side. Identical
	// pre-capability behaviour — only the cgo-heavy backend ever
	// reaches this path.
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeImports {
			continue
		}
		toNode := g.GetNode(e.To)
		if toNode == nil {
			continue
		}
		if toNode.FilePath != target.FilePath && toNode.ID != target.FilePath {
			continue
		}
		fromNode := g.GetNode(e.From)
		if fromNode == nil {
			continue
		}
		add(fromNode.FilePath)
	}
	sort.Strings(importingFiles)
	return importingFiles
}

// isCheckRefEdge identifies edges that mean "this symbol is being
// used". Mirrors safe_delete_symbol's referencing-edge filter so
// the two tools agree on what "referenced" means.
func isCheckRefEdge(k graph.EdgeKind) bool {
	switch k {
	case graph.EdgeCalls,
		graph.EdgeImplements,
		graph.EdgeExtends,
		graph.EdgeReferences,
		graph.EdgeInstantiates,
		graph.EdgeCrossRepoCalls,
		graph.EdgeCrossRepoImplements,
		graph.EdgeCrossRepoExtends:
		return true
	}
	return false
}

// atOrAboveTier returns true when `actual` >= `min` in the
// 5-tier confidence ladder. The ladder, highest to lowest:
//
//	lsp_resolved → lsp_dispatch → ast_resolved → ast_inferred → text_matched
//
// Empty `actual` is treated as the lowest tier (text_matched) so old
// edges produced before tier-stamping landed don't get dropped.
func atOrAboveTier(actual, min string) bool {
	rank := func(t string) int {
		switch strings.ToLower(t) {
		case "lsp_resolved", "lspresolved":
			return 5
		case "lsp_dispatch", "lspdispatch":
			return 4
		case "ast_resolved", "astresolved":
			return 3
		case "ast_inferred", "astinferred":
			return 2
		case "", "text_matched", "textmatched":
			return 1
		}
		return 0
	}
	return rank(actual) >= rank(min)
}
