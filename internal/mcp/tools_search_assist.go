package mcp

import (
	"context"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// assistMode controls whether the LLM-driven query expansion and
// rerank passes run during handleSearchSymbols. Default is `auto` —
// the NL heuristic decides per-query. `on` and `off` are explicit
// overrides for callers that know their query character. `deep` is
// `on` plus a body-grounded verification pass that reads candidate
// code bodies and HONESTLY drops the ones whose code is not
// actually about the query (paid for in extra latency).
type assistMode int

const (
	assistAuto assistMode = iota
	assistOn
	assistOff
	assistDeep
)

// parseAssistMode reads the `assist` arg. Unrecognised values fall
// back to `auto` rather than erroring so callers can't accidentally
// break search by typoing the flag.
func parseAssistMode(req mcpgo.CallToolRequest) assistMode {
	switch strings.ToLower(strings.TrimSpace(req.GetString("assist", ""))) {
	case "on", "yes", "true", "force":
		return assistOn
	case "off", "no", "false", "skip":
		return assistOff
	case "deep", "verify", "body":
		return assistDeep
	default:
		return assistAuto
	}
}

// looksNaturalLanguage is the cheap pre-LLM gate. Returns true when
// the query is shaped like a natural-language description rather than
// an identifier lookup. Heuristics:
//   - Fewer than 3 whitespace tokens → identifier; skip.
//   - Any token containing dot / slash / scope-resolution → qualified
//     identifier; skip.
//   - Any token that's PascalCase or camelCase → identifier; skip.
//   - At least one common English stop word among 3+ tokens → engage.
//   - 4+ plain-word tokens with no identifier shape → engage.
//
// Empty / blank input never engages.
func looksNaturalLanguage(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return false
	}
	tokens := strings.Fields(q)
	if len(tokens) < 3 {
		return false
	}
	for _, t := range tokens {
		if strings.ContainsAny(t, "./:_") {
			return false
		}
		if hasMixedCase(t) {
			return false
		}
	}
	if hasStopWord(tokens) {
		return true
	}
	return len(tokens) >= 4
}

// hasMixedCase reports whether a token contains both upper and lower
// ASCII letters — i.e. PascalCase or camelCase. Pure lowercase /
// pure uppercase plain words don't qualify.
func hasMixedCase(t string) bool {
	var hasUpper, hasLower bool
	for _, r := range t {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		}
		if hasUpper && hasLower {
			return true
		}
	}
	return false
}

// assistStopWords is a tight list of English function words that
// rarely appear in identifier-style queries. Matching any of them in
// a 3+ token query strongly signals natural language. Kept short
// deliberately — false positives here cost LLM latency on every call.
var assistStopWords = map[string]struct{}{
	"where": {}, "how": {}, "what": {}, "why": {}, "which": {}, "when": {},
	"the": {}, "is": {}, "a": {}, "an": {},
	"in": {}, "of": {}, "to": {}, "for": {}, "with": {}, "by": {}, "from": {},
	"do": {}, "does": {}, "are": {}, "we": {}, "us": {}, "our": {},
	"and": {}, "or": {}, "not": {},
}

func hasStopWord(tokens []string) bool {
	for _, t := range tokens {
		if _, ok := assistStopWords[strings.ToLower(t)]; ok {
			return true
		}
	}
	return false
}

// shouldEngageAssist combines the caller's explicit mode with the
// auto-gate heuristic. `on` and `deep` always engage, `off` never
// engages, and `auto` defers to looksNaturalLanguage. The service-
// side enabled check is layered on top — callers wrap this with
// `s.llmService != nil && s.llmService.Enabled()` so that a stub
// build short-circuits regardless of mode.
func shouldEngageAssist(mode assistMode, query string) bool {
	switch mode {
	case assistOff:
		return false
	case assistOn, assistDeep:
		return true
	default:
		return looksNaturalLanguage(query)
	}
}

// deferColdLoadForAssist decides whether an implicit search-assist
// request must defer its local-model cold load. Given that assist would
// otherwise engage, it returns engage=false + deferred=true exactly when
// the model is NOT yet loaded AND a background enrichment pass is in
// flight — loading a multi-GiB model then would contend with enrichment
// for CPU/GPU/RAM. When the model is already loaded, or nothing is
// enriching, assist proceeds unchanged. The explicit `ask` path never
// calls this: user intent always loads the model.
func deferColdLoadForAssist(engage, modelLoaded, enrichmentBusy bool) (engageOut, deferred bool) {
	if !engage {
		return false, false
	}
	if modelLoaded || !enrichmentBusy {
		return true, false
	}
	return false, true
}

// decomposeMinLeafLen is the shortest leaf token the decomposition
// fallback keeps. One- and two-character fragments ("id", "to", a
// lone "s") match too broadly to rescue a query usefully and only
// inflate the OR-merge.
const decomposeMinLeafLen = 3

// queryHasDecomposableSeparator reports whether a query carries a
// structural separator the leaf-decomposition fallback can split on —
// a dot, slash, or scope-resolution qualifier, or an internal
// camelCase / PascalCase boundary. A bare prose miss ("flush the
// cache") has none of these, so the fallback skips it and pays no
// extra fetch. An underscore counts too: snake_case identifiers
// decompose the same way.
func queryHasDecomposableSeparator(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return false
	}
	if strings.ContainsAny(q, "./_\\") || strings.Contains(q, "::") {
		return true
	}
	// camelCase / PascalCase boundary: a lowercase→uppercase or
	// uppercase-run→lowercase transition inside a single token.
	var prev rune
	for i, r := range q {
		if i > 0 {
			if r >= 'A' && r <= 'Z' && prev >= 'a' && prev <= 'z' {
				return true
			}
			if r >= 'a' && r <= 'z' && prev >= 'A' && prev <= 'Z' {
				return true
			}
		}
		prev = r
	}
	return false
}

// decomposeQueryToLeaves splits a compound / dotted / CamelCase query
// into its leaf symbol-name tokens via the camelCase-aware tokenizer
// ("UserService.FindUser" -> [user, service, find]), drops tokens
// shorter than decomposeMinLeafLen, and drops any leaf that equals the
// original query (so a single-token query never re-searches itself).
// The deduplicated result feeds the BM25 OR-merge so each leaf is
// retrieved on its own terms. Returns nil when nothing usable
// survives.
func decomposeQueryToLeaves(q string) []string {
	qLower := strings.ToLower(strings.TrimSpace(q))
	if qLower == "" {
		return nil
	}
	var (
		out  []string
		seen = map[string]struct{}{}
	)
	for _, t := range rerank.Tokenize(q) {
		if len(t) < decomposeMinLeafLen {
			continue
		}
		if t == qLower {
			// The query was a single short token — nothing to
			// decompose; re-searching it would just repeat the miss.
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// expandSearchTerms calls the LLM expansion path and returns the
// extra terms. Returns nil (no expansion) on any failure so the
// search path stays at parity with today's behaviour when the model
// hiccups or isn't loaded yet.
//
// When vocabAnchored is set, the model's returned terms are
// post-filtered to the words that actually appear in this repo's
// symbol-name vocabulary (mined into AutoConcepts) BEFORE the caller
// dedupes / merges them. This is robust and model-agnostic: the
// expand prompt is a static const small models ignore, so anchoring
// the OUTPUT to the corpus is the only reliable way to keep a
// hallucinated-but-plausible synonym ("authenticator") from diluting
// the BM25 pool when no symbol uses that word. The filter degrades to
// a no-op (unconstrained expansion) when the vocabulary is empty --
// AutoConcepts is nil until the first RunAnalysis, and an empty graph
// mines an empty vocabulary -- so anchoring never strips every term
// away on a cold or tiny index.
func expandSearchTerms(ctx context.Context, s *Server, query string, vocabAnchored bool) []string {
	if s.llmService == nil || !s.llmService.Enabled() {
		return nil
	}
	res, err := s.llmService.ExpandQuery(ctx, query)
	if err != nil || res == nil {
		return nil
	}
	terms := res.Terms
	if vocabAnchored {
		terms = anchorTermsToVocabulary(terms, s.getAutoConcepts())
	}
	return terms
}

// anchorTermsToVocabulary keeps only the terms present in the mined
// symbol-name vocabulary. A nil or empty vocabulary (AutoConcepts not
// yet built, or an empty graph) is treated as "no anchor available"
// and the input is returned unchanged -- anchoring must never silence
// expansion on a repo that simply hasn't mined a vocabulary yet.
func anchorTermsToVocabulary(terms []string, ac *search.AutoConcepts) []string {
	if ac == nil || ac.VocabularySize() == 0 || len(terms) == 0 {
		return terms
	}
	kept := make([]string, 0, len(terms))
	for _, t := range terms {
		if ac.InVocabulary(t) {
			kept = append(kept, t)
		}
	}
	return kept
}

// fetchAndMergeBM25 fires (at most) two BM25 calls — one for the
// primary query alone (so we can attribute primaryCount honestly for
// the debug surface) and one for the combined OR-merge of every
// expansion term — then folds the results into a single deduplicated
// slice. The original query's hits win position; the combined-
// expansion hits append in their own BM25 order with duplicates
// skipped.
//
// The store's FTS treats a multi-token query as an OR-style union
// with a single global BM25 score, so one combined call replaces
// the prior N per-term fan-out (the N+1 round-trip pattern dominated
// the search hot path on disk backends).
//
// A per-fragment exact-name rescue runs after the combined call —
// one batched FindNodesByNames on the engine's reader. This
// preserves the per-term behaviour where a fragment like
// "BillingInvoice" finds its exact-name node even when BM25
// tokenisation drops the PascalCase concatenation.
//
// fetchLimit caps each call so a wide expansion can't blow up the
// candidate pool.
//
// primaryCount is the size of the original-query BM25 result before
// merging — surfaced on the assist debug field so callers can see how
// much expansion contributed.
func fetchAndMergeBM25(eng *query.Engine, original string, expanded []string, fetchLimit int, scope query.QueryOptions) (merged []*graph.Node, primaryCount int) {
	return fetchAndMergeBM25Timed(eng, original, expanded, fetchLimit, scope, nil)
}

// fetchAndMergeBM25Timed is fetchAndMergeBM25 with per-phase wall-clock
// breakdowns. The MCP handler hands a fresh SearchTimings struct so
// the resulting Debug log line attributes BM25 time honestly across
// the primary call and the combined-expansion call. Pass nil to skip
// instrumentation (e.g. unit tests that don't care).
func fetchAndMergeBM25Timed(eng *query.Engine, original string, expanded []string, fetchLimit int, scope query.QueryOptions, timings *query.SearchTimings) (merged []*graph.Node, primaryCount int) {
	// The merged candidate set is reranked by the handler with the
	// full session-aware context; the per-call inner rerank inside
	// SearchSymbolsRanked would be wasted work whose output the
	// merge discards. SkipInnerRerank collapses the N+1 engine
	// rerank invocations to zero — drops ~150-300ms per call on
	// a disk backend (each inner rerank's Context.prepare costs at minimum
	// two batched edge fetches when the bundle cache misses).
	scope.SkipInnerRerank = true
	primaryStart := time.Now()
	primary := eng.SearchSymbolsScoped(original, fetchLimit, scope)
	primaryCount = len(primary)
	if timings != nil {
		timings.BM25PrimaryMS += time.Since(primaryStart).Milliseconds()
	}

	// Trim and de-empty the expansion list. When nothing useful
	// survives we skip the combined call entirely.
	cleanedExpansion := make([]string, 0, len(expanded))
	for _, t := range expanded {
		t = strings.TrimSpace(t)
		if t != "" {
			cleanedExpansion = append(cleanedExpansion, t)
		}
	}
	if len(cleanedExpansion) == 0 {
		return primary, primaryCount
	}

	seen := make(map[string]bool, len(primary)+fetchLimit)
	merged = make([]*graph.Node, 0, len(primary)+fetchLimit)
	for _, n := range primary {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		merged = append(merged, n)
	}

	// Combined OR-merge: pass every expansion term — concatenated by
	// whitespace — as ONE BM25 call. Tokenisation + IDF scoring run
	// once across the whole bag of terms instead of N times.
	//
	// The concatenated bag of terms is never going to match any
	// node's literal Name, so the engine's exact-name splice would
	// pay a guaranteed-empty FindNodesByName round-trip every
	// fan-out. SkipExactNameSplice tells gatherBackendCandidates to
	// skip it — the per-fragment exact-name rescue below covers the
	// load-bearing PascalCase-fragment case the splice was insuring
	// against, so dropping the round-trip is safe.
	combined := strings.Join(cleanedExpansion, " ")
	expansionScope := scope
	expansionScope.SkipExactNameSplice = true
	expansionStart := time.Now()
	extra := eng.SearchSymbolsScoped(combined, fetchLimit, expansionScope)
	if timings != nil {
		timings.BM25ExpansionMS += time.Since(expansionStart).Milliseconds()
	}
	for _, n := range extra {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		merged = append(merged, n)
	}

	// Per-fragment exact-name union — cheap (one name-bucket lookup
	// per term on in-memory, a single batched name-IN query on a
	// disk backend via FindNodesByNames). Preserves the
	// per-term behaviour where a fragment like "BillingInvoice"
	// finds its exact-name node even when BM25 tokenisation misses
	// the PascalCase concatenated token. Without this rescue,
	// soup-split mode silently dropped exact matches that the
	// per-term loop used to surface via the engine's FindNodesByName
	// fallback.
	if rdr, ok := graphReaderFromEngine(eng); ok {
		nameMap := rdr.FindNodesByNames(cleanedExpansion)
		for _, term := range cleanedExpansion {
			for _, n := range nameMap[term] {
				if n == nil || seen[n.ID] {
					continue
				}
				if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
					continue
				}
				// ScopeAllows is a no-op when neither axis is set, so the
				// guard must not pre-test WorkspaceID: a session bound to
				// the repos its cwd contains carries its whole boundary in
				// RepoAllow, and pre-testing the slug skipped the check
				// exactly when it was the only one left.
				if !scope.ScopeAllows(n) {
					continue
				}
				seen[n.ID] = true
				merged = append(merged, n)
			}
		}
	}
	return merged, primaryCount
}

// graphReaderFromEngine returns the engine's underlying graph reader
// if it also exposes the batched FindNodesByNames method (every
// production backend does — in-memory, the on-disk backend, and OverlaidView via
// the layered base). Falls back to (nil, false) when an embedded
// test engine wires a stripped-down reader — the rescue step is then
// skipped, matching the contract that callers without a names-batch
// reader simply get the BM25-only result.
type namesReader interface {
	FindNodesByNames(names []string) map[string][]*graph.Node
}

func graphReaderFromEngine(eng *query.Engine) (namesReader, bool) {
	if eng == nil {
		return nil, false
	}
	r, ok := eng.Reader().(namesReader)
	return r, ok
}

// rerankCap bounds how many candidates the rerank pass sees. The
// model has limited working memory; past ~25 items its judgement
// degrades and the prompt blows the assist context. Trailing
// candidates beyond rerankCap stay in BM25 order and are appended
// after the reranked head.
const rerankCap = 20

// semanticRerankPool is the BM25 over-fetch width for a concept /
// degraded-identifier query that will be reranked by the always-on
// in-process semantic-cosine channel (no LLM assist). Wide enough that
// an intent query's target, which BM25 alone ranks past the default
// over-fetch, still enters the candidate pool the semantic rerank can
// reorder into the top-5.
const semanticRerankPool = 40

// prioritizeCallables stably re-orders nodes so functions and methods
// come first, preserving BM25 order within each bucket. Everything
// non-callable (fields, params, variables, constants, types, files,
// imports, …) sinks to the tail in its original order. The intent
// is to make sure the rerank head — which is what the LLM sees and
// reorders — is populated with the symbols that actually *do* things,
// not their structural siblings that just happen to share tokens.
func prioritizeCallables(nodes []*graph.Node) []*graph.Node {
	callable := make([]*graph.Node, 0, len(nodes))
	others := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			callable = append(callable, n)
		} else {
			others = append(others, n)
		}
	}
	return append(callable, others...)
}

// verifyCap bounds how many candidates the body-grounded verifier
// sees. Each candidate ships with its function body (truncated), so
// the input is much heavier than the name+sig rerank — keep it
// smaller to stay inside the assist context.
const verifyCap = 10

// verifyBodyMaxLines and verifyBodyMaxChars cap the per-candidate
// body fed to the model. We want enough to see what the code DOES
// (a function header + a few lines of logic) without including
// every helper call. Empirically 8 non-blank lines is plenty for
// the verify decision.
const (
	verifyBodyMaxLines = 8
	verifyBodyMaxChars = 600
)

// verifyCallersPerCand caps the number of callers sent per candidate.
// More callers = more disambiguation signal, but also more tokens.
// Three is empirically enough to anchor the data-domain of most
// functions without blowing the assist context for a 10-candidate batch.
const verifyCallersPerCand = 3

// topCallersForVerify returns up to verifyCallersPerCand callers of n,
// each with name + truncated signature. The query depth is 1 (direct
// callers only) and the brief detail level keeps memory pressure low.
// Returns nil for non-callable kinds or when GetCallers yields nothing.
func topCallersForVerify(eng *query.Engine, n *graph.Node) []llm.CallerInfo {
	if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
		return nil
	}
	sg := eng.GetCallers(n.ID, query.QueryOptions{
		Depth:  1,
		Limit:  verifyCallersPerCand + 4, // over-fetch a little: self + non-callers get filtered
		Detail: "brief",
	})
	if sg == nil || len(sg.Nodes) == 0 {
		return nil
	}
	out := make([]llm.CallerInfo, 0, verifyCallersPerCand)
	for _, cn := range sg.Nodes {
		if cn == nil || cn.ID == n.ID {
			continue
		}
		if cn.Kind != graph.KindFunction && cn.Kind != graph.KindMethod {
			continue
		}
		sig, _ := cn.Meta["signature"].(string)
		out = append(out, llm.CallerInfo{
			Name:      cn.Name,
			Signature: sig,
		})
		if len(out) >= verifyCallersPerCand {
			break
		}
	}
	return out
}

// extractBodyForVerify reads a node's source body, returns the first
// verifyBodyMaxLines non-blank lines truncated to verifyBodyMaxChars.
// Returns "" when no source can be read or when the node isn't a
// function/method — non-function symbols pass through to the verifier
// with signature-only context, which the prompt handles explicitly.
func extractBodyForVerify(ctx context.Context, s *Server, n *graph.Node) string {
	if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
		return ""
	}
	if n.StartLine <= 0 || n.EndLine <= 0 {
		return ""
	}
	abs, err := s.resolveNodePath(ctx, n)
	if err != nil {
		return ""
	}
	source, _, _, err := readLines(abs, n.StartLine, n.EndLine, 0)
	if err != nil {
		return ""
	}
	return truncateBody(source, verifyBodyMaxLines, verifyBodyMaxChars)
}

// truncateBody keeps the first maxLines non-blank lines, then
// caps the result at maxChars. Blank lines between code count
// against neither budget — they're skipped. Returns the truncated
// text with a trailing "…" marker when either cap fires.
func truncateBody(src string, maxLines, maxChars int) string {
	if src == "" {
		return ""
	}
	lines := strings.Split(src, "\n")
	var b strings.Builder
	kept := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(ln)
		b.WriteString("\n")
		kept++
		if kept >= maxLines {
			b.WriteString("…\n")
			break
		}
	}
	out := b.String()
	if len(out) > maxChars {
		out = out[:maxChars] + "…\n"
	}
	return out
}

// verifyDebug captures what the verify pass saw and decided, so the
// debug surface can return it for diagnostic inspection. Lightweight
// — only ID lists, no bodies.
type verifyDebug struct {
	Considered []string // IDs sent to the verifier (top-verifyCap of head)
	Kept       []string // IDs the model chose to keep, in keep order
}

// verifyWithLLM runs the body-grounded verification pass on the head
// of `nodes`. Returns the model's kept-and-ordered subset followed
// by anything past verifyCap (unverified tail). On failure or empty
// service the input is returned unchanged.
//
// An empty `keep` is HONORED: when the model says "nothing here
// matches", we return only the unverified tail. The caller is meant
// to treat that as a legitimate negative result rather than fall back
// to the noisy pre-verify candidates.
func verifyWithLLM(ctx context.Context, s *Server, query string, nodes []*graph.Node) (result []*graph.Node, dbg verifyDebug, ok bool) {
	if s.llmService == nil || !s.llmService.Enabled() || len(nodes) == 0 {
		return nodes, dbg, false
	}
	head := nodes
	tail := []*graph.Node(nil)
	if len(nodes) > verifyCap {
		head = nodes[:verifyCap]
		tail = nodes[verifyCap:]
	}

	cands := make([]llm.VerifyCandidate, len(head))
	idx := make(map[string]*graph.Node, len(head))
	dbg.Considered = make([]string, len(head))
	for i, n := range head {
		sig, _ := n.Meta["signature"].(string)
		cands[i] = llm.VerifyCandidate{
			ID:        n.ID,
			Name:      n.Name,
			Signature: sig,
			Body:      extractBodyForVerify(ctx, s, n),
			Callers:   topCallersForVerify(s.engineFor(ctx), n),
		}
		idx[n.ID] = n
		dbg.Considered[i] = n.ID
	}

	res, err := s.llmService.VerifyRelevance(ctx, query, cands)
	if err != nil || res == nil {
		return nodes, dbg, false
	}

	keptNodes := make([]*graph.Node, 0, len(res.Keep))
	usedIDs := make(map[string]bool, len(res.Keep))
	for _, id := range res.Keep {
		if n, ok := idx[id]; ok && !usedIDs[id] {
			usedIDs[id] = true
			keptNodes = append(keptNodes, n)
			dbg.Kept = append(dbg.Kept, id)
		}
	}
	out := append(keptNodes, tail...)
	return out, dbg, true
}

// rerankWithLLM packs the head of `nodes` into RerankCandidates,
// calls the service, and rebuilds the slice in the model's order.
// Trailing candidates beyond rerankCap are kept verbatim after the
// reranked head. On any failure, returns the input unchanged.
//
// Before partitioning into head/tail, nodes are re-sorted so callable
// kinds (function / method) come before everything else — preserving
// retrieval order within each bucket. Without this, a high-scoring
// param or field node (e.g. `Engine.SearchSymbols#param:limit`) can
// pre-empt the enclosing method (`Engine.SearchSymbols`) inside the rerank
// window, leaving the model unable to surface the real callable.
func rerankWithLLM(ctx context.Context, s *Server, query string, nodes []*graph.Node) []*graph.Node {
	if s.llmService == nil || !s.llmService.Enabled() || len(nodes) < 2 {
		return nodes
	}
	nodes = prioritizeCallables(nodes)
	head := nodes
	tail := []*graph.Node(nil)
	if len(nodes) > rerankCap {
		head = nodes[:rerankCap]
		tail = nodes[rerankCap:]
	}

	cands := make([]llm.RerankCandidate, len(head))
	idx := make(map[string]*graph.Node, len(head))
	for i, n := range head {
		sig, _ := n.Meta["signature"].(string)
		cands[i] = llm.RerankCandidate{
			ID:        n.ID,
			Name:      n.Name,
			Signature: sig,
			Path:      n.FilePath,
		}
		idx[n.ID] = n
	}

	res, err := s.llmService.RerankSymbols(ctx, query, cands)
	if err != nil || res == nil || len(res.Order) == 0 {
		return nodes
	}

	reordered := make([]*graph.Node, 0, len(nodes))
	used := make(map[string]bool, len(head))
	for _, id := range res.Order {
		n, ok := idx[id]
		if !ok || used[id] {
			continue
		}
		used[id] = true
		reordered = append(reordered, n)
	}
	// Defensive: the service guarantees a permutation, but if any
	// head node is missing for any reason, append it after the
	// reranked head in its original position.
	for _, n := range head {
		if !used[n.ID] {
			reordered = append(reordered, n)
		}
	}
	reordered = append(reordered, tail...)
	return reordered
}
