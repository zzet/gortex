package mcp

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

// Declaration lookup uses the same complete current corpus proof as text
// search. Its results contain parser facts rather than relationship metadata.
func (s *Server) handleSourceSearchSymbols(ctx context.Context, req mcp.CallToolRequest, view *requestView, query string, fq fieldQuery, resolved ResolvedScope) (*mcp.CallToolResult, error) {
	if view.freshness != nil && !view.freshness.deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, view.freshness.deadline)
		defer cancel()
	}
	files, err := s.sourceSearchSnapshot(ctx, view, s.resolvePathFilter(req, fq), resolved)
	if err != nil {
		if errors.Is(err, indexer.ErrSourceSearchBudget) {
			return s.fallbackSourceSearch(ctx, req, view, s.handleSearchSymbols)
		}
		return mcp.NewToolResultError("search_symbols: " + err.Error()), nil
	}
	idx := s.sourceSearchIndexer(view)
	needle := strings.ToLower(query)
	prefilter := needle
	if at := strings.LastIndexAny(prefilter, ".:"); at >= 0 {
		prefilter = prefilter[at+1:]
	}
	nodes := make([]*graph.Node, 0)
	parsed := 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Qualified names may be synthesized from package/receiver facts.
		// Unicode escape spelling can also differ from the declared name.
		graphPath := sourceSearchGraphPath(view, file.path)
		extractor, source, ok, prepareErr := idx.PrepareSourceDeclaration(file.abs, graphPath, file.content)
		if prepareErr != nil {
			return s.fallbackSourceSearch(ctx, req, view, s.handleSearchSymbols)
		}
		if !ok {
			continue
		}
		raw := string(source)
		if prefilter != "" && !strings.Contains(raw, "\\u") && !strings.Contains(raw, "\\U") && !strings.Contains(strings.ToLower(raw), prefilter) {
			continue
		}
		result, parseErr := parser.Extract(extractor, graphPath, source, parser.ExtractionOptions{})
		parsed++
		if parseErr != nil {
			if result != nil {
				result.ReleaseTree()
			}
			return s.fallbackSourceSearch(ctx, req, view, s.handleSearchSymbols)
		}
		if result == nil {
			return s.fallbackSourceSearch(ctx, req, view, s.handleSearchSymbols)
		}
		indexer.NormalizeSourceDeclarations(result, source)
		for _, node := range result.Nodes {
			if node == nil || (!strings.Contains(strings.ToLower(node.Name), needle) && !strings.Contains(strings.ToLower(node.QualName), needle) && !strings.Contains(strings.ToLower(node.RetrievalMetadata().QualName), needle)) {
				continue
			}
			node.RepoPrefix, node.WorkspaceID, node.ProjectID = view.sourceRepoPrefix, idx.WorkspaceID(), idx.ProjectID()
			nodes = append(nodes, node)
		}
		result.ReleaseTree()
	}
	kind := strings.TrimSpace(req.GetString("kind", ""))
	if kind == "" {
		kind = fq.Kind
	}
	flavor := strings.TrimSpace(req.GetString("flavor", ""))
	if flavor == "" {
		flavor = fq.Flavor
	}
	kind, movedFlavor := reclassifyKindFlavor(kind)
	if movedFlavor != "" {
		if flavor != "" {
			flavor += ","
		}
		flavor += movedFlavor
	}
	nodes = filterNodes(nodes, resolved.RepoAllow)
	nodes = filterNodesByKind(nodes, kind)
	nodes = applyFieldFilters(nodes, fq)
	if flavor != "" {
		nodes = applyFlavorFilter(nodes, flavor)
	}
	nodes = filterNodesByCorpus(nodes, corpusCode)
	if paths := s.resolvePathFilter(req, fq); len(paths) > 0 {
		nodes = applyPathFilter(nodes, paths)
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		exactA := strings.EqualFold(a.Name, query) || strings.EqualFold(a.QualName, query) || strings.EqualFold(a.RetrievalMetadata().QualName, query)
		exactB := strings.EqualFold(b.Name, query) || strings.EqualFold(b.QualName, query) || strings.EqualFold(b.RetrievalMetadata().QualName, query)
		if exactA != exactB {
			return exactA
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		return a.ID < b.ID
	})
	nodes, _ = diversifyByFile(nodes, nil, req.GetInt("max_per_file", defaultMaxPerFile))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := req.GetInt("limit", 20)
	if limit <= 0 {
		limit = 20
	}
	offset := min(max(0, decodeCursor(req.GetString("cursor", ""))), len(nodes))
	end := offset + min(limit, len(nodes)-offset)
	results := make([]map[string]any, 0, end-offset)
	for _, node := range s.withAbsPaths(ctx, nodes[offset:end]) {
		results = append(results, node.Brief())
	}
	response := map[string]any{
		"results": results, "total": len(nodes), "truncated": end < len(nodes),
		"ranking": "lexical_name", "query_class": "symbol",
		"source_evidence": map[string]any{"verified": true, "corpus_complete": true, "files_scanned": len(files), "files_parsed": parsed},
	}
	if end < len(nodes) {
		response["next_cursor"] = encodeCursor(end)
	}
	if err := s.validateSourceCheckoutIdentity(ctx, view); err != nil {
		return mcp.NewToolResultError("search_symbols: " + err.Error()), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if view.freshness != nil {
		view.freshness.fresh = true
	}
	return s.respondScopedJSONOrTOON(ctx, req, response, resolved)
}
