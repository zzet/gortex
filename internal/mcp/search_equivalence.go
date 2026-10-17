package mcp

import (
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/search/rerank"
)

// expandMode controls which query-expansion channels search_symbols
// runs. The default is `both` -- LLM expansion (when the assist gate
// engages) plus the deterministic equivalence-class expansion. The
// other modes pin a single channel or disable expansion entirely.
type expandMode int

const (
	// expandBoth runs LLM expansion (gated by assist) and equivalence
	// expansion together. The default when the `expand` arg is unset.
	expandBoth expandMode = iota
	// expandEquivalenceOnly runs only the deterministic equivalence
	// table + auto-concept expansion; the LLM channel is skipped even
	// when a provider is configured.
	expandEquivalenceOnly
	// expandLLMOnly runs only the LLM expansion channel.
	expandLLMOnly
	// expandOff disables every expansion channel -- a pure BM25 query.
	expandOff
)

// parseExpandMode reads the `expand` arg. Unrecognised values fall
// back to `both` so a typo can't silently break expansion.
func parseExpandMode(req mcpgo.CallToolRequest) expandMode {
	switch strings.ToLower(strings.TrimSpace(req.GetString("expand", ""))) {
	case "equivalence", "equiv":
		return expandEquivalenceOnly
	case "llm":
		return expandLLMOnly
	case "off", "none":
		return expandOff
	default:
		return expandBoth
	}
}

// allowsLLMExpansion reports whether the mode permits the LLM channel.
func (m expandMode) allowsLLMExpansion() bool {
	return m == expandBoth || m == expandLLMOnly
}

// allowsEquivalenceExpansion reports whether the mode permits the
// deterministic equivalence channel.
func (m expandMode) allowsEquivalenceExpansion() bool {
	return m == expandBoth || m == expandEquivalenceOnly
}

// isIdentifierClass reports whether the query class is one of the
// identifier-shape classes (symbol / path / signature) — the classes
// where the rerank's classWeightTable already proves the semantic
// channel contributes near-zero useful signal (0.65 / 0.45 / 0.80 vs
// the baseline 1.00 for concept). The handler routes these queries
// through the identifier-shape fast path: expansion off, vector
// channel off, fetch slack tightened.
func isIdentifierClass(c rerank.QueryClass) bool {
	switch c {
	case rerank.QueryClassSymbol, rerank.QueryClassPath, rerank.QueryClassSignature:
		return true
	default:
		return false
	}
}

// expandEquivalenceClasses returns the deterministic expansion terms
// for a query: for every query token, its curated-equivalence-table
// siblings and its per-repo auto-mined concept siblings. The result
// is a deduplicated slice with the query's own tokens removed, ready
// to feed the BM25 OR-merge alongside (or instead of) LLM expansion.
//
// This channel runs even when no LLM provider is configured -- it is
// the deterministic vocabulary-bridging win. Returns nil when
// equivalence expansion is disabled in config, when the query is
// empty, or when no token has a sibling.
func (s *Server) expandEquivalenceClasses(query string) []string {
	if !s.searchConfig().EquivalenceClassesEnabled() {
		return nil
	}
	tokens := rerank.Tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	// Query tokens themselves are never expansion terms -- BM25
	// already searched them.
	queryTokens := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		queryTokens[strings.ToLower(t)] = struct{}{}
	}

	table := s.equivalence
	auto := s.autoConceptsForAnswer()

	var (
		out  []string
		seen = map[string]struct{}{}
	)
	add := func(term string) {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			return
		}
		if _, isQuery := queryTokens[term]; isQuery {
			return
		}
		if _, dup := seen[term]; dup {
			return
		}
		seen[term] = struct{}{}
		out = append(out, term)
	}
	// Tier 1 -- direct synonym siblings (union-find) and per-repo
	// auto-mined concept siblings. These are interchangeable with the
	// query token, so they enter the OR-merge first / at full weight.
	for _, tok := range tokens {
		for _, sib := range table.Expand(tok) {
			add(sib)
		}
		for _, sib := range auto.Expand(tok) {
			add(sib)
		}
	}

	// Tier 2 -- concept-relatedness thesaurus siblings. "auth" pulls
	// in "token" / "session" / "jwt": adjacent concepts, NOT synonyms.
	// They run LLM-free under expand=equivalence, but at a strictly
	// lower priority than Tier 1 -- appended AFTER every direct
	// sibling so they trail in the OR-merge order, and bounded by
	// relatedTermBudget per query so a related-concept bridge widens
	// recall a little without flooding the candidate pool and eroding
	// precision. Terms already emitted as a direct sibling are skipped
	// by the shared dedupe.
	relatedBudget := relatedTermBudget
	for _, tok := range tokens {
		if relatedBudget <= 0 {
			break
		}
		for _, sib := range table.ExpandRelated(tok) {
			if relatedBudget <= 0 {
				break
			}
			before := len(out)
			add(sib)
			if len(out) > before {
				relatedBudget--
			}
		}
	}
	return out
}

// relatedTermBudget caps how many concept-relatedness (Tier 2)
// thesaurus terms a single query may contribute. Kept small so the
// adjacent-concept bridge stays a recall nudge, not a flood -- the
// related terms are not synonyms, so an unbounded fan-out would dilute
// precision. The cap admits roughly the two nearest related classes'
// members (the forward-declared neighbours rank first), enough to
// bridge an adjacent concept without flooding the candidate pool.
// Direct synonym siblings are never subject to this cap.
const relatedTermBudget = 8

// mergeExpansionTerms unions several expansion-term lists into one
// deduplicated slice, preserving the order the lists are supplied
// in (soup disjuncts first so they retain priority, then LLM
// synonyms, then equivalence siblings). Comparison is
// case-insensitive; blank terms are dropped.
func mergeExpansionTerms(lists ...[]string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, list := range lists {
		for _, term := range list {
			t := strings.TrimSpace(term)
			if t == "" {
				continue
			}
			key := strings.ToLower(t)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}
