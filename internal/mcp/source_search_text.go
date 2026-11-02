package mcp

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/search/trigram"
)

func (s *Server) handleSourceSearchText(ctx context.Context, req mcp.CallToolRequest, view *requestView, query string, useRegexp bool, limit, requestedLimit int, resolved ResolvedScope, pathFilter []string) (*mcp.CallToolResult, error) {
	var expression *regexp.Regexp
	if useRegexp {
		var err error
		expression, err = regexp.Compile(query)
		if err != nil {
			return mcp.NewToolResultError("search_text: invalid regexp: " + err.Error()), nil
		}
	}
	files, err := s.sourceSearchSnapshot(ctx, view, pathFilter, resolved)
	if err != nil {
		if errors.Is(err, indexer.ErrSourceSearchBudget) {
			return s.fallbackSourceSearch(ctx, req, view, s.handleSearchText)
		}
		return mcp.NewToolResultError("search_text: " + err.Error()), nil
	}
	matches := make([]enrichedTextMatch, 0)
	idx := s.sourceSearchIndexer(view)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current, err := s.scanSourceSearchText(ctx, idx, file.abs, sourceSearchGraphPath(view, file.path), file.content, query, expression, limit-len(matches))
		if err != nil {
			return nil, err
		}
		matches = append(matches, current...)
		if len(matches) >= limit {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.validateSourceCheckoutIdentity(ctx, view); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if view.freshness != nil {
		view.freshness.fresh = true
	}
	resp := map[string]any{
		"query": query, "matches": matches, "count": len(matches), "symbols_enriched": false,
		"source_evidence": map[string]any{"verified": true, "corpus_complete": true, "files_scanned": len(files)},
	}
	if len(matches) >= limit {
		resp["_truncated_by_limit"] = true
		resp["_limit_applied"] = limit
		resp["count_is_exact"] = false
		resp["truncation_note"] = "the scoped source search stopped at limit; narrow path scope or raise limit to see more matches"
		if requestedLimit > limit {
			resp["_limit_requested"] = requestedLimit
		}
	}
	return s.respondScopedJSONOrTOON(ctx, req, resp, resolved)
}

// Shared by the automatic source lane and canonical pending-file merge. Keep
// the byte decoder, binary guard, redaction and line semantics identical.
func (s *Server) scanSourceSearchText(ctx context.Context, idx *indexer.Indexer, abs, path string, raw []byte, query string, expression *regexp.Regexp, limit int) ([]enrichedTextMatch, error) {
	matches := make([]enrichedTextMatch, 0)
	if len(raw) == 0 || len(raw) > 64<<20 {
		return nil, nil
	}
	source := idx.DecodeSourceSearchText(abs, raw)
	if trigram.IsBinary(source) || len(source) > 64<<20 {
		return nil, nil
	}
	lang := s.detectLanguageForContent(abs, source[:min(512, len(source))])
	content, _ := s.maybeRedactConfigLeaf(lang, path, false, string(source))
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	for line, text := range lines {
		if line%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		text = strings.TrimSuffix(text, "\r")
		found := strings.Contains(text, query)
		if expression != nil {
			found = expression.MatchString(text)
		}
		if found {
			matches = append(matches, enrichedTextMatch{Path: path, Line: line + 1, Text: text})
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches, nil
}
