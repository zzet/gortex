package mcp

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
)

const (
	// searchSymbolsContentFallbackMaxMatches bounds the whole recovery: at
	// most this many trigram hits are grepped, scope-filtered, and mapped to
	// an enclosing declaration, so the file-node lookups the mapping performs
	// are bounded by the same number.
	searchSymbolsContentFallbackMaxMatches = 8
	searchSymbolsContentFallbackMinRunes   = 3

	searchSymbolsContentFallbackReason = "symbol_lookup_empty"
	searchSymbolsContentFallbackNote   = "No symbol node carries this name. These are raw content matches from the text index — file:line evidence plus the smallest declaration enclosing each line, not graph symbols. Nothing here can be passed to a symbol-id argument."
)

// searchSymbolsContentFallbackSection is the clearly separated, non-symbol
// half of a search_symbols response. It rides its own field so every existing
// consumer of `results` / `total` — including the localization recovery
// contract, which reads the captured graph page rather than the wire body —
// still sees the exact empty page an unresolved identifier produced.
type searchSymbolsContentFallbackSection struct {
	Reason  string              `json:"reason"`
	Query   string              `json:"query"`
	Note    string              `json:"note"`
	Count   int                 `json:"count"`
	Matches []enrichedTextMatch `json:"matches"`
}

// searchSymbolsContentFallbackTerm reports the exact identifier an empty
// symbol lookup may re-run as a content search, and whether the query is
// identifier-shaped at all. Prose, paths, calls, and dotted qualifiers are
// refused: they either never occur verbatim in source or already have their
// own decomposition path, so grepping them is pure cost.
func searchSymbolsContentFallbackTerm(q string) (string, bool) {
	term := strings.Trim(strings.TrimSpace(q), "`'\"")
	if utf8.RuneCountInString(term) < searchSymbolsContentFallbackMinRunes {
		return "", false
	}
	first, _ := utf8.DecodeRuneInString(term)
	if !unicode.IsLetter(first) && first != '_' && first != '$' {
		return "", false
	}
	hasLetter := false
	for _, r := range term {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r), r == '_', r == '$':
		default:
			return "", false
		}
	}
	if !hasLetter {
		return "", false
	}
	return term, true
}

// searchSymbolsContentFallback runs one bounded literal search for term and
// returns the surviving hits mapped to their enclosing declarations, or nil
// when the text backend is unavailable, refuses the scope, or finds nothing.
// The wall-clock slice is the same bounded budget the source-literal recall
// spends, so an empty symbol lookup can never turn into an open-ended scan.
func (s *Server) searchSymbolsContentFallback(
	ctx context.Context,
	term string,
	resolved ResolvedScope,
	scope query.QueryOptions,
) *searchSymbolsContentFallbackSection {
	if s == nil || term == "" || ctx.Err() != nil {
		return nil
	}
	if s.indexer == nil && s.multiIndexer == nil {
		return nil
	}

	boundedCtx, cancel := context.WithTimeout(ctx, s.sourceLiteralRecallBudget())
	defer cancel()
	search := s.searchExploreSourceLiteral(
		boundedCtx, term, "", scope, searchSymbolsContentFallbackMaxMatches,
	)
	if ctx.Err() != nil {
		return nil
	}
	if len(search.matches) == 0 {
		return nil
	}

	// GrepLiteralBounded already rejects substring-only occurrences, but the
	// same word-boundary test the explore path uses keeps the two recalls
	// reporting the same notion of "exact".
	exact := make([]trigram.Match, 0, len(search.matches))
	for _, match := range search.matches {
		if exploreTextHasExactLiteral(match.Text, term) {
			exact = append(exact, match)
		}
	}
	exact = s.filterTextMatchesByResolvedScope(ctx, exact, resolved)
	if len(exact) > searchSymbolsContentFallbackMaxMatches {
		exact = exact[:searchSymbolsContentFallbackMaxMatches]
	}
	if len(exact) == 0 {
		return nil
	}

	enriched, _ := s.enrichTextMatchesContext(boundedCtx, exact, scope)
	if len(enriched) == 0 {
		return nil
	}
	return &searchSymbolsContentFallbackSection{
		Reason:  searchSymbolsContentFallbackReason,
		Query:   term,
		Note:    searchSymbolsContentFallbackNote,
		Count:   len(enriched),
		Matches: enriched,
	}
}
