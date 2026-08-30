package mcp

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
)

// enrichedTextMatch is a trigram literal-search hit decorated with the
// graph symbol that encloses the matching line. symbol_id /
// symbol_name are empty for a match in a file-level region with no
// enclosing function / method / type.
type enrichedTextMatch struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Text       string `json:"text"`
	SymbolID   string `json:"symbol_id,omitempty"`
	SymbolName string `json:"symbol_name,omitempty"`
}

// handleSearchText runs a trigram-accelerated literal code search
// across the indexed repository -- the alt grep backbone. A trigram
// index narrows the file set, then each candidate is scanned to
// confirm the match, so a repo-wide substring search costs roughly
// the size of the matching files rather than the whole tree.
//
// Each hit is enriched with the enclosing graph symbol so an agent
// can see *which function / method* a literal match landed in
// without a follow-up get_symbol call.
func (s *Server) handleSearchText(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("search_text: query is required"), nil
	}
	if s.indexer == nil && s.multiIndexer == nil {
		return mcp.NewToolResultError("search_text: no indexer available"), nil
	}
	resolved, errResult := s.resolveScope(ctx, req, IntentLocate)
	if errResult != nil {
		return errResult, nil
	}

	limit := req.GetInt("limit", 100)
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// Multi-repo mode: the daemon owns a MultiIndexer and the per-repo
	// Indexer pointer (s.indexer) is unset or empty-rooted. Fan out
	// across every tracked repo's trigram searcher and stamp repo
	// prefixes on the match paths so downstream tooling sees the same
	// shape graph nodes use. Single-indexer callers (one-shot CLI,
	// tests) fall through to the legacy path.
	//
	// regexp mode runs the same trigram-accelerated backbone through a
	// compiled regular expression instead of a literal substring; a
	// bad pattern surfaces as a tool error rather than zero hits, and
	// the results flow through the identical enclosing-symbol
	// enrichment so callers get the same shape either way.
	useRegexp := req.GetBool("regexp", false)
	pathFilter := s.resolvePathFilter(req, fieldQuery{})
	scopedMultiGrep := s.multiIndexer != nil && (resolved.RepoAllow != nil || len(pathFilter) > 0)
	var matches []trigram.Match
	needsFinalLimit := false
	if view := requestViewFromContext(ctx); view != nil && view.baseFallback {
		// Grace is a sealed graph fallback, not a selected working copy. Literal
		// text search is filesystem-backed, so neither the removed checkout nor
		// the primary checkout's possibly dirty bytes are a truthful corpus.
		return viewTextUnavailable(view, "the labeled primary fallback has no selected working copy"), nil
	} else if view.routed() {
		// A request reading through a concrete view answers out of that view's
		// own working copy, or not at all. The canonical searchers below are
		// built over a different tree.
		viewMatches, refusal := s.searchTextInView(ctx, view, query, useRegexp, limit)
		if refusal != nil {
			return refusal, nil
		}
		matches = viewMatches
	} else if useRegexp {
		var err error
		if scopedMultiGrep {
			matches, err = s.multiIndexer.GrepRegexpForRepos(query, "", resolved.RepoAllow, limit)
			needsFinalLimit = true
		} else if s.multiIndexer != nil {
			matches, err = s.multiIndexer.GrepRegexp(query, "", limit)
		} else {
			matches, err = s.indexer.GrepRegexp(query, "", limit)
		}
		if err != nil {
			return mcp.NewToolResultError("search_text: invalid regexp: " + err.Error()), nil
		}
	} else if scopedMultiGrep {
		matches = s.multiIndexer.GrepTextForRepos(query, resolved.RepoAllow, limit)
		needsFinalLimit = true
	} else if s.multiIndexer != nil {
		matches = s.multiIndexer.GrepText(query, limit)
	} else {
		matches = s.indexer.GrepText(query, limit)
	}

	// Sub-path scoping: a `path` argument or a `scope:`-named saved
	// scope's paths narrow the literal hits to a monorepo service
	// slice. In multi-repo mode MultiIndexer.GrepText stamps a repo
	// prefix onto every match path, so the repo-relative filter is
	// expanded with the tracked repo prefixes before the anchored test.
	if len(pathFilter) > 0 {
		var repoPrefixes []string
		if s.multiIndexer != nil {
			repoPrefixes = s.multiIndexer.RepoPrefixes()
		}
		matches = filterTextMatchesByPath(matches, pathFilter, repoPrefixes)
	}
	matches = s.filterTextMatchesByResolvedScope(ctx, matches, resolved)
	if needsFinalLimit {
		matches = limitTextMatches(matches, limit)
	}

	enriched, fileIndexes := s.enrichTextMatchesContext(ctx, matches, queryOptionsForResolvedScope(resolved))
	s.captureLocalizationSearchText(ctx, enriched, fileIndexes)
	resp := map[string]any{
		"query":   query,
		"matches": enriched,
		"count":   len(enriched),
	}
	// Body-visible disclosure for a repo-narrowed zero (the _meta scope
	// fields are invisible in CLI output and most clients). No recheck
	// here — the note still names the scope and the widen escape hatch.
	if len(enriched) == 0 && len(resolved.RepoAllow) > 0 {
		resp["scope_note"] = scopeZeroNote(resolved, -1)
	}
	return s.respondScopedJSONOrTOON(ctx, req, resp, resolved)
}

// filterTextMatchesByPath keeps only the trigram matches whose file
// path sits under one of the anchored sub-path prefixes. repoPrefixes
// carries the tracked repo prefixes (empty in single-repo mode) so a
// repo-relative filter still matches the repo-prefixed paths that
// MultiIndexer.GrepText stamps onto matches in multi-repo mode.
func filterTextMatchesByPath(matches []trigram.Match, paths, repoPrefixes []string) []trigram.Match {
	norm := normalizePathPrefixes(paths)
	if len(norm) == 0 {
		return matches
	}
	prefixes := expandPathPrefixesWithRepos(norm, repoPrefixes)
	out := make([]trigram.Match, 0, len(matches))
	for _, m := range matches {
		if pathMatchesAnyPrefix(m.Path, prefixes) {
			out = append(out, m)
		}
	}
	return out
}

func limitTextMatches(matches []trigram.Match, limit int) []trigram.Match {
	if limit > 0 && len(matches) > limit {
		return matches[:limit]
	}
	return matches
}

func (s *Server) filterTextMatchesByResolvedScope(ctx context.Context, matches []trigram.Match, resolved ResolvedScope) []trigram.Match {
	if resolved.WorkspaceID == "" && resolved.ProjectID == "" && len(resolved.RepoAllow) == 0 {
		return matches
	}
	opts := query.QueryOptions{
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	}
	// Every match in the batch is attributed through the same reader,
	// so the request-reader lookup is hoisted out of the loop.
	reader := s.readerFor(ctx)
	out := make([]trigram.Match, 0, len(matches))
	for _, m := range matches {
		repo, _, ok := strings.Cut(m.Path, "/")
		// Repo allow-set fast path: a match whose stamped repo prefix
		// is outside the allow-set is dropped outright. Only applies
		// when the first path segment actually names a tracked repo —
		// on an unstamped (standalone-indexer) path the first segment
		// is an ordinary directory and must not be mistaken for a repo
		// prefix. The node-attribution check below stays authoritative
		// either way.
		knownRepo := ok && repo != "" && s.multiIndexer != nil && s.multiIndexer.GetMetadata(repo) != nil
		if len(resolved.RepoAllow) > 0 && knownRepo && !resolved.RepoAllow[repo] {
			continue
		}
		// Fail CLOSED under active narrowing: keep a match only when it
		// can be positively attributed to an in-scope graph node. A match
		// whose path resolves to no node (graph unavailable, or a file the
		// graph never turned into a node) cannot be proven in-scope, so
		// dropping it is the safe choice — keeping it was a latent
		// cross-scope leak.
		if reader == nil {
			continue
		}
		n := reader.GetNode(m.Path)
		if n == nil {
			// A trigram match path is always forward-slash, but node IDs
			// keep the repo-relative remainder in the OS separator. The two
			// spellings agree only for a file at the repo root, so on
			// Windows every match below the root failed attribution here and
			// the fail-closed drop below emptied the entire result set.
			if key := graphMatchPathKey(m.Path, knownRepo); key != m.Path {
				n = reader.GetNode(key)
			}
		}
		// GrepTextForRepos stamps the registry prefix onto every match path,
		// and graph file keys carry that same prefix, so the two agree by
		// construction. They used to diverge for a lone repo — which minted
		// unprefixed node IDs — and attribution needed a provenance-gated
		// retry with the prefix stripped, or it fail-closed-dropped every
		// match in a solo daemon.
		if n == nil || !opts.ScopeAllows(n) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// graphMatchPathKey spells a trigram match path the way graph node IDs
// spell it. Match paths are always forward-slash; a node ID joins the repo
// prefix with "/" but keeps the repo-relative remainder in the OS separator,
// so the two forms diverge on Windows for every file below the repo root.
// repoPrefixed says whether the first segment names a tracked repo rather
// than an ordinary directory. Returns path unchanged where the separators
// already agree, so POSIX callers pay nothing.
func graphMatchPathKey(path string, repoPrefixed bool) string {
	if filepath.Separator == '/' {
		return path
	}
	if repoPrefixed {
		if repo, rest, ok := strings.Cut(path, "/"); ok {
			return repo + "/" + filepath.FromSlash(rest)
		}
	}
	return filepath.FromSlash(path)
}

// enrichTextMatchesContext decorates every trigram match with its enclosing
// graph symbol through the bounded file projection, and returns the same file
// indexes used for enrichment so localization evidence capture never repeats
// the file scan. Storage receives the effective request/session scope; exact
// and Windows path spellings share the request-wide 4096-node budget.
func (s *Server) enrichTextMatchesContext(
	ctx context.Context,
	matches []trigram.Match,
	opts query.QueryOptions,
) ([]enrichedTextMatch, map[string]*fileSymbolIndex) {
	out := make([]enrichedTextMatch, 0, len(matches))
	exactPaths := make([]string, 0, len(matches))
	aliasPaths := make([]string, 0, len(matches))
	exactSeen := make(map[string]struct{}, len(matches))
	aliasSeen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if _, duplicate := exactSeen[match.Path]; !duplicate {
			exactSeen[match.Path] = struct{}{}
			exactPaths = append(exactPaths, match.Path)
		}
		if alias := graphMatchPathKey(match.Path, true); alias != match.Path {
			if _, duplicate := aliasSeen[alias]; !duplicate {
				aliasSeen[alias] = struct{}{}
				aliasPaths = append(aliasPaths, alias)
			}
		}
	}
	orderedPaths := make([]string, 0, len(exactPaths)+len(aliasPaths))
	orderedPaths = append(orderedPaths, exactPaths...)
	for _, alias := range aliasPaths {
		if _, isExact := exactSeen[alias]; !isExact {
			orderedPaths = append(orderedPaths, alias)
		}
	}
	indexes := s.buildFileSymbolIndexForOrderedPathsScopedContext(ctx, orderedPaths, opts)
	for _, match := range matches {
		enriched := enrichedTextMatch{Path: match.Path, Line: match.Line, Text: match.Text}
		index := fileSymbolIndexForPath(indexes, match.Path)
		if index != nil {
			enriched.SymbolID, enriched.SymbolName = index.find(match.Line)
		}
		out = append(out, enriched)
	}
	return out, indexes
}

func fileSymbolIndexForPath(indexes map[string]*fileSymbolIndex, path string) *fileSymbolIndex {
	if index := indexes[path]; index != nil {
		return index
	}
	if key := graphMatchPathKey(path, true); key != path {
		return indexes[key]
	}
	return nil
}
