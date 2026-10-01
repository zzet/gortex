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
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if trigram.IsBinary(file.content) || len(file.content) == 0 || len(file.content) > 64<<20 {
			continue
		}
		path := sourceSearchGraphPath(view, file.path)
		lang := s.detectLanguageForContent(file.abs, file.content[:min(512, len(file.content))])
		content, _ := s.maybeRedactConfigLeaf(lang, path, false, string(file.content))
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
