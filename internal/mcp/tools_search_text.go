package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
	// The requested value is kept so the response can say the ceiling chose
	// the effective limit rather than the caller: a caller who asked for
	// 100000 and silently got 1000 has no way to tell that from a corpus that
	// held exactly 1000 matches.
	requestedLimit := limit
	if maxLimit := searchTextMaxLimit(); limit > maxLimit {
		limit = maxLimit
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
	if view := requestViewFromContext(ctx); view.routed() {
		// A request reading through a view answers out of that view's own
		// working copy, or not at all. The canonical searchers below are built
		// over a different tree.
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

	// Counted BEFORE the filters below, and that ordering is the whole point.
	// The searcher stops at `limit`; the path and scope filters then run over
	// what survived, so a response holding 952 matches can be a truncated
	// 1000 rather than a complete 952. Measuring after the filters would miss
	// exactly the case a caller cannot detect on its own.
	rawMatches := len(matches)

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
	// A result bound by `limit` was byte-indistinguishable from a complete
	// one: `count` was set to the same ceiling the array stopped at, so the
	// two corroborated each other at the wrong number. The byte budget has
	// always disclosed its own truncation (`_truncated_by_budget`); this is
	// the limit path's equivalent.
	if searchTextBoundByLimit(rawMatches, limit) {
		resp["_truncated_by_limit"] = true
		resp["_limit_applied"] = limit
		resp["count_is_exact"] = false
		resp["truncation_note"] = searchTextTruncationNote
		if requestedLimit > limit {
			resp["_limit_requested"] = requestedLimit
		}
	}
	// Body-visible disclosure for a repo-narrowed zero (the _meta scope
	// fields are invisible in CLI output and most clients). No recheck
	// here — the note still names the scope and the widen escape hatch.
	if len(enriched) == 0 && len(resolved.RepoAllow) > 0 {
		resp["scope_note"] = scopeZeroNote(resolved, -1)
	}
	return s.respondScopedJSONOrTOON(ctx, req, resp, resolved)
}

// searchTextDefaultMaxLimit is the ceiling `limit` is clamped to. It has been
// 1000 since search_text was added and is kept as the default so no existing
// caller's response changes shape.
const searchTextDefaultMaxLimit = 1000

// searchTextMaxLimit returns the effective ceiling, overridable through
// GORTEX_SEARCH_TEXT_MAX_LIMIT for the sweep this tool exists to serve —
// search_text is the literal-search backbone agents reach for in place of
// grep, where an unbounded pass is the normal request. An unset, unparseable
// or non-positive value keeps the default rather than lifting the bound: a
// typo must not turn into an unbounded scan.
//
// Raising it is not the fix on its own. Without the disclosure below a higher
// ceiling only moves the silent cliff, which is why the flag lands with it.
func searchTextMaxLimit() int {
	v := strings.TrimSpace(os.Getenv("GORTEX_SEARCH_TEXT_MAX_LIMIT"))
	if v == "" {
		return searchTextDefaultMaxLimit
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return searchTextDefaultMaxLimit
	}
	return n
}

const searchTextTruncationNote = "the search stopped at `limit`, so `count` is a floor rather than a total and the matches are a prefix of the real result set. Raise `limit` (or the GORTEX_SEARCH_TEXT_MAX_LIMIT ceiling) to widen. Narrowing with `path` will NOT recover the remainder: the path filter runs over what survived truncation, not over the corpus, so a subtree slice returns whatever was left of the global cut."

// searchTextBoundByLimit reports whether the search stopped because of the
// limit rather than because the corpus ran out.
//
// rawMatches is the count the searcher returned, before the path and scope
// filters. Landing on the effective limit is the signal, and it can fire on a
// corpus holding exactly that many matches — a spurious "verify this" is the
// safe direction to be wrong in, against silently losing most of the result.
func searchTextBoundByLimit(rawMatches, limit int) bool {
	return limit > 0 && rawMatches >= limit
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
