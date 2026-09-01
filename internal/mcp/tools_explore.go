package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/elide"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// exploreToolDescription is the one-shot localization verb's advertised
// contract — engineered to be the obvious opening move for any
// task-shaped request. It promises the whole first exploration phase in
// a single call so the agent never decomposes localization into a string
// of granular search/read/callers turns (the measured turn-economy loss).
const exploreToolDescription = "Start here for any task, bug report, or " +
	"\"where is / how does X work\" question. Describe the request in plain " +
	"words (paste the issue, name the area) and get the localized neighborhood " +
	"in ONE call: the ranked likely-involved symbols with their source and call " +
	"paths (callers + callees), plus the files to change — the whole exploration " +
	"phase (5-10 search/read/callers calls) folded into one. Answer or edit " +
	"straight from it; it states when the neighborhood is complete."

// explore tuning. These are generic retrieval parameters — fan-out
// widths and a token ceiling — with no dependence on any particular
// corpus, query vocabulary, or benchmark. The verb takes arbitrary free
// text; nothing here is derived from a fixed task set.
const (
	exploreDefaultBudgetTokens = 1600
	// localizationDefaultBudgetTokens is the localize-path default. Its
	// envelope pays for ranked rows and the leading file's outline out of one
	// budget, where explore(task) prose pays for rows alone.
	// Sized so the localize page can carry real source for its whole primary
	// window, not just name/signature rows: a page that names the right
	// declaration but shows no code asks the caller to choose between
	// look-alike siblings on identifier alone. Sessions measured against an
	// unrestricted grep agent showed the answer present-but-unnamed far more
	// often than absent; serving bodies is what closes that, and the session
	// token spend stays well under the plain-search agent's.
	localizationDefaultBudgetTokens = 12000
	exploreMinBudgetTokens          = 1000
	exploreMaxBudgetTokens          = 24000
	exploreDefaultMaxSymbols        = 16
	exploreMaxMaxSymbols            = 30
	exploreRingCap                  = 5 // callers / callees shown per target
	// Kept at eight while the page itself widens: this cap also sets the
	// envelope's shed floor and the tight-budget wire guarantee, so widening
	// it is a contract change, not a serving change.
	localizationRefinementAllowedSymbolCap = 8 // preferred plus ranked alternates; preserves rank seven
	exploreCharsPerToken                   = 4 // coarse token estimate for budgeting
	// Full bodies are the largest repeated-context cost. Candidate headers,
	// locations, signatures, and graph rings preserve recall for the whole
	// neighborhood; only the top targets need full source to make the first
	// response answer-ready.
	exploreFullBodyLimit = 5
	// exploreBodyBudgetShare caps any single full body at this fraction of
	// the total budget, so one huge top-ranked symbol cannot starve the
	// rest of the neighborhood of their bodies.
	exploreBodyBudgetShare = 6
)

// registerExploreTool wires the one-shot localization verb into the tool
// surface. It ships eagerly in the coding-agent + core presets (see the
// preset roster in tool_presets.go) so it is the first thing a task-shaped
// session reaches for.
func (s *Server) registerExploreTool() {
	s.addTool(
		mcp.NewTool("explore",
			mcp.WithDescription(exploreToolDescription),
			mcp.WithString("task", mcp.Required(), mcp.Description("Natural-language description of the task, bug report, or question to localize (e.g. paste an issue body, or 'the retry backoff never triggers on a 429').")),
			mcp.WithNumber("max_symbols", mcp.Description("Max ranked candidate symbols (default 10).")),
			mcp.WithNumber("token_budget", mcp.Description("Approximate source-packing budget in tokens (default 1600). Candidate locations and signatures are always listed and may exceed it; full source is reserved for the highest-ranked targets.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("path", mcp.Description("Restrict the neighborhood to one or more sub-paths (comma-separated), anchored at the repo root — a monorepo-service slice.")),
		),
		s.handleExplore,
	)
}

// exploreTarget is one ranked candidate plus its bounded neighborhood,
// gathered before rendering so the renderer can honour the token budget.
type exploreTarget struct {
	node                   *graph.Node
	score                  float64
	callers                []*graph.Node
	callees                []*graph.Node
	directCalleesComplete  bool // false when the direct projection was truncated, bounded, or otherwise lower-bound
	causalCallees          []exploreCausalNeighbor
	causalChangeBridge     bool   // one graph-proven caller/callee retained as provenance for a promoted continuation
	causalChangeLeaf       bool   // graph-proven wrapper implementation or task-aligned cross-file change callable
	causalChangeOwner      bool   // same-file type that encloses or is uniquely returned by the causal change callable
	source                 string // full body (may be empty for non-source kinds)
	sourceWindow           *localizationSourceWindow
	divergentDefaultOwner  bool   // unique child constructor whose concrete default causes the queried behavior
	divergentDefaultType   bool   // owning type paired with divergentDefaultOwner for coherent file/symbol output
	conceptImplementation  bool   // primary identifier-backed callable; may establish answer readiness
	conceptComplement      bool   // marginal concept callable protected as evidence, never as terminal proof
	syntacticAnchor        bool   // task-spelled flag/identifier owner protected by bounded lexical competition
	sourceRange            bool   // exact task path+line owner reserved for presentation, never completion authority
	exactContent           bool   // verified full quoted-literal hit from content_fts
	exactContentAmbiguous  bool   // exact evidence has visible or possibly truncated peers
	sourceLiteral          bool   // exact source-body hit that must survive final envelope packing
	sourceLiteralCallee    bool   // exact source callsite uniquely resolved to this invoked callable
	sourceLiteralAligned   bool   // source-literal callee that instantiates the task's value; strongest literal owner
	literalPrimaryEligible bool   // authenticated unanchored literal may use the one bounded PRIMARY reserve
	literalMatchCount      int    // distinct task literals matched; stable tie-break among eligible rows
	typedAnchorProjection  bool   // bounded field-owner-call proof promoted from a task-aligned typed field
	foldedOwner            bool   // synthetic owner inserted by concept member folding
	leadingFileDepth       bool   // sibling of the ranked leading file, admitted into the expendable breadth tail
	localizationRelation   string // direct_caller/direct_callee row promoted only into the bounded terminal projection
}

func exploreTargetFromCandidate(
	candidate *rerank.Candidate,
	protectedImplementationID string,
	literalPrimaryEligible bool,
	quotedLiteralTask bool,
) exploreTarget {
	if candidate == nil || candidate.Node == nil {
		return exploreTarget{}
	}
	target := exploreTarget{
		node: candidate.Node, score: candidate.Score,
		conceptImplementation: candidate.Node.ID == protectedImplementationID,
	}
	if candidate.Signals == nil {
		return target
	}
	target.conceptComplement = candidate.Signals[exploreConceptComplementSignal] > 0
	target.syntacticAnchor = candidate.Signals[exploreSyntacticAnchorSignal] > 0
	target.sourceRange = candidate.Signals[exploreSourceRangeSignal] > 0
	target.exactContent = candidate.Signals[exploreContentRecallExactSignal] > 0
	target.exactContentAmbiguous = candidate.Signals[exploreContentRecallAmbiguousSignal] > 0
	target.sourceLiteral = candidate.Signals[exploreSourceLiteralSignal] > 0
	target.sourceLiteralCallee = candidate.Signals[exploreSourceLiteralCalleeSignal] > 0
	target.sourceLiteralAligned = candidate.Signals[exploreSourceLiteralTaskAlignSignal] > 0
	target.typedAnchorProjection = candidate.Signals[exploreTypedAnchorProjectionSignal] > 0
	// Eligibility is per-row for evidence grounded in a literal the caller
	// quoted: that the task also names a file or symbol does not make the
	// quoted value's registration site less of an answer. Only the inferred
	// bare-literal lane — whose literals are Gortex's own guess — remains
	// gated on the page having no explicit anchor.
	eligible := literalPrimaryEligible ||
		quotedLiteralTask && !target.exactContentAmbiguous
	if eligible && (target.sourceLiteral || target.exactContent) {
		target.literalPrimaryEligible = true
		matches := candidate.Signals[exploreContentRecallTermSignal]
		if coverage := candidate.Signals[exploreSourceLiteralCoverageSignal]; coverage > matches {
			matches = coverage
		}
		target.literalMatchCount = int(matches)
	}
	return target
}

type exploreCausalNeighbor struct {
	node *graph.Node
	hop  int
}

const (
	exploreCausalSeedLimit       = 3
	exploreCausalDepth           = 2
	exploreCausalAdmissionBudget = 10 * time.Millisecond
	exploreDraftPrimaryLimit     = 3
	exploreDraftTotalLimit       = 5
)

type exploreDraftEntry struct {
	node                *graph.Node
	evidence            string
	exact               bool
	overlap             int
	bodyOverlap         int
	rareOverlap         int
	bodyDensity         int
	generic             bool
	direct              bool
	structural          bool
	structuralShared    int
	structuralLocal     bool
	structuralCallee    bool
	structuralCrossFile bool
	structuralHop       int
	parentExact         bool
	parentOverlap       int
	parentRank          int
}

// exploreAnswerDraft puts a small, ready-to-use evidence set before the full
// neighborhood. The ranked head always leads; the remaining slots promote
// query-aligned deeper targets or graph neighbors without hiding any detailed
// candidate below.
func exploreQueryIsConceptTask(query string) bool {
	query = strings.TrimSpace(stripLeadingExploreDirective(query))
	if query == "" {
		return false
	}
	class := rerank.ClassifyQuery(query)
	lower := strings.ToLower(query)
	// Catch declaration-shaped signatures whose return type is project-specific
	// and therefore may not be recognized by the generic query classifier. A
	// call at the end of prose is not a declaration: its pre-call prefix is a
	// sentence, not a compact return-type/name pair.
	open, close := strings.Index(lower, "("), strings.LastIndex(lower, ")")
	if open > 0 && close > open {
		suffix := strings.TrimSpace(lower[close+1:])
		terminalDeclaration := suffix == "" || suffix == ";" || suffix == "{" || suffix == "const" || suffix == "noexcept" || strings.HasPrefix(suffix, "->") || strings.HasPrefix(suffix, ": ") || strings.HasPrefix(suffix, "throws ")
		prefixFields := strings.Fields(strings.TrimSpace(lower[:open]))
		if terminalDeclaration && len(prefixFields) >= 2 && len(prefixFields) <= 6 {
			previous := strings.Trim(prefixFields[len(prefixFields)-2], "*&[]<>,:;")
			switch previous {
			case "a", "an", "and", "at", "before", "by", "calls", "does", "for", "from", "how", "in", "into", "of", "on", "or", "reaches", "the", "through", "to", "via", "when", "where", "why", "with":
				// Natural-language call site; continue with concept detection.
			default:
				return false
			}
		}
	}
	if class == rerank.QueryClassConcept {
		return true
	}

	// A symbol, path, or signature embedded in a natural-language task must not
	// turn the whole task into an exact lookup. Pure lookup shapes are compact;
	// prose has several ordinary words around a bounded amount of code syntax.
	fields := strings.Fields(query)
	if len(fields) < 8 || len(query) < 48 {
		return false
	}
	for _, prefix := range []string{
		"func ", "fn ", "pub ", "async ", "unsafe ", "extern ",
		"def ", "class ", "interface ", "trait ", "type ",
		"public ", "private ", "protected ", "internal ", "static ",
		"export ", "declare ", "function ", "const ", "constexpr ",
		"void ", "bool ", "char ", "int ", "long ", "short ",
		"float ", "double ", "auto ", "unsigned ", "signed ", "std::", "@",
	} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	plain, codeShaped := 0, 0
	for _, field := range fields {
		token := strings.Trim(field, ".,;:!?\"'")
		if token == "" {
			continue
		}
		if strings.ContainsAny(token, `/\\(){}[]<>=&|*_:;`) {
			codeShaped++
			continue
		}
		plain++
	}
	return plain >= 6 && plain*2 >= len(fields) && codeShaped*2 < len(fields)
}

// exploreQueryClass applies the prose-aware concept decision consistently.
// The generic classifier intentionally treats compact parentheses and paths as
// signatures/lookups; long issue paraphrases may contain those shapes as small
// embedded examples and must remain concept localization everywhere.
func exploreQueryClass(query string) rerank.QueryClass {
	class := rerank.ClassifyQuery(query)
	if class != rerank.QueryClassConcept && exploreQueryIsConceptTask(query) {
		return rerank.QueryClassConcept
	}
	return class
}

func exploreQueryHasCallAnchor(query, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	isIdentifierByte := func(b byte) bool {
		return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
	}
	for offset := 0; offset < len(query); {
		index := strings.Index(query[offset:], name)
		if index < 0 {
			return false
		}
		index += offset
		beforeOK := index == 0 || !isIdentifierByte(query[index-1])
		after := index + len(name)
		for after < len(query) && (query[after] == ' ' || query[after] == '\t') {
			after++
		}
		if beforeOK && after < len(query) && query[after] == '(' {
			return true
		}
		offset = index + len(name)
	}
	return false
}

func exploreQueryHasPathSymbolAnchor(query string, n *graph.Node) bool {
	if n == nil {
		return false
	}
	normalizedPath := exploreNormalizedPath(nodeDisplayPath(n))
	name := exploreNormalizedAnchor(n.Name)
	qualName := exploreNormalizedAnchor(n.RetrievalMetadata().QualName)
	if qualName == "" || qualName == name {
		if separator := strings.LastIndex(n.ID, "::"); separator >= 0 {
			qualName = exploreNormalizedAnchor(n.ID[separator+2:])
		}
	}
	for _, field := range strings.Fields(query) {
		field = strings.Trim(field, "`'\"()[]{}<>,;")
		separator := strings.LastIndex(field, "::")
		if separator <= 0 || !strings.ContainsAny(field[:separator], "/\\") {
			continue
		}
		if exploreNormalizedPath(field[:separator]) != normalizedPath {
			continue
		}
		symbol := exploreNormalizedAnchor(field[separator+2:])
		if symbol == name || symbol == qualName || strings.HasSuffix(qualName, " "+symbol) {
			return true
		}
	}
	return false
}

func exploreStrongCausalSeed(query string, target exploreTarget) bool {
	n := target.node
	if n == nil || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) || exploreDraftIsTestNode(n) || exploreIdentifierSegmentCount(n.Name) < 2 {
		return false
	}
	queryTerms := exploreTerminalTerms(query)
	overlap, longest := exploreDraftTermOverlap(queryTerms, n)
	bodyOverlap := exploreDraftTermSetOverlap(queryTerms, exploreTerminalTerms(target.source))
	return overlap >= 2 || bodyOverlap >= 2 || (overlap == 1 && longest >= 5)
}

type exploreCausalSeedMetric struct {
	index             int
	crossFileCallees  int
	callableCallees   int
	identifierOverlap int
	bodyOverlap       int
	longest           int
	generic           bool
}

// selectExploreCausalSeeds spends the fixed depth-2 budget only after every
// final candidate has received the same source and depth-1 hydration. Selection
// is therefore evidence-based rather than arrival-order based: task-aligned
// production callables that cross a file boundary lead, while leaf accessors
// and configuration gates remain eligible only when stronger routes are absent.
func selectExploreCausalSeeds(query string, targets []exploreTarget, conceptTask, explicit bool) []int {
	if !conceptTask || explicit || len(targets) == 0 {
		return nil
	}
	queryTerms := exploreTerminalTerms(query)
	metrics := make([]exploreCausalSeedMetric, 0, len(targets))
	for index, target := range targets {
		if !exploreHydratedProductionCallable(target) || !exploreStrongCausalSeed(query, target) {
			continue
		}
		identifierOverlap, longest := exploreDraftTermOverlap(queryTerms, target.node)
		metric := exploreCausalSeedMetric{
			index:             index,
			identifierOverlap: identifierOverlap,
			bodyOverlap:       exploreDraftTermSetOverlap(queryTerms, exploreTerminalTerms(target.source)),
			longest:           longest,
			generic:           exploreDraftGenericCandidate(target.node, target.source),
		}
		seedPath := exploreNormalizedPath(nodeDisplayPath(target.node))
		for _, callee := range target.callees {
			if callee == nil || callee.ID == "" || exploreDraftIsTestNode(callee) ||
				(callee.Kind != graph.KindFunction && callee.Kind != graph.KindMethod) {
				continue
			}
			metric.callableCallees++
			calleePath := exploreNormalizedPath(nodeDisplayPath(callee))
			if seedPath != "" && calleePath != "" && calleePath != seedPath {
				metric.crossFileCallees++
			}
		}
		metrics = append(metrics, metric)
	}
	if len(metrics) == 0 {
		return nil
	}
	alignment := func(metric exploreCausalSeedMetric) int {
		body := metric.bodyOverlap
		if body > 2 {
			body = 2
		}
		return metric.identifierOverlap + body
	}
	sort.SliceStable(metrics, func(i, j int) bool {
		a, b := metrics[i], metrics[j]
		if (a.crossFileCallees > 0) != (b.crossFileCallees > 0) {
			return a.crossFileCallees > 0
		}
		if a.crossFileCallees != b.crossFileCallees {
			return a.crossFileCallees > b.crossFileCallees
		}
		if (a.callableCallees > 0) != (b.callableCallees > 0) {
			return a.callableCallees > 0
		}
		if a.generic != b.generic {
			return !a.generic
		}
		if alignment(a) != alignment(b) {
			return alignment(a) > alignment(b)
		}
		if a.identifierOverlap != b.identifierOverlap {
			return a.identifierOverlap > b.identifierOverlap
		}
		if a.longest != b.longest {
			return a.longest > b.longest
		}
		return a.index < b.index
	})
	if len(metrics) > exploreCausalSeedLimit {
		metrics = metrics[:exploreCausalSeedLimit]
	}
	selected := make([]int, len(metrics))
	for index, metric := range metrics {
		selected[index] = metric.index
	}
	return selected
}

// minimumExploreCausalHops derives a deterministic, bounded reachable set from
// the edges already returned by GetCallChain. It never re-queries the graph:
// the traversal cost is linear in the at-most-limit subgraph admitted above.
func minimumExploreCausalHops(seedID string, sg *query.SubGraph, scope query.QueryOptions, maxDepth, limit int) []exploreCausalNeighbor {
	if seedID == "" || sg == nil || maxDepth < 1 || limit < 1 {
		return nil
	}
	nodes := make(map[string]*graph.Node, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n != nil && n.ID != "" {
			nodes[n.ID] = n
		}
	}
	adjacent := make(map[string][]string, len(sg.Edges))
	for _, edge := range sg.Edges {
		if edge == nil || (edge.Kind != graph.EdgeCalls && edge.Kind != graph.EdgeMatches) || edge.From == "" || edge.To == "" {
			continue
		}
		adjacent[edge.From] = append(adjacent[edge.From], edge.To)
	}
	for from := range adjacent {
		sort.Strings(adjacent[from])
	}

	hops := map[string]int{seedID: 0}
	queue := []string{seedID}
	result := make([]exploreCausalNeighbor, 0, min(limit, len(nodes)))
	for head := 0; head < len(queue); head++ {
		from := queue[head]
		fromHop := hops[from]
		if fromHop >= maxDepth {
			continue
		}
		for _, to := range adjacent[from] {
			if _, seen := hops[to]; seen {
				continue
			}
			n := nodes[to]
			if n == nil || !exploreNodeWithinQueryScope(n, scope) {
				continue
			}
			hop := fromHop + 1
			hops[to] = hop
			queue = append(queue, to)
			if exploreDraftIsTestNode(n) || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
				continue
			}
			result = append(result, exploreCausalNeighbor{node: n, hop: hop})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].hop != result[j].hop {
			return result[i].hop < result[j].hop
		}
		return exploreDraftNodeKey(result[i].node) < exploreDraftNodeKey(result[j].node)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func exploreAnswerDraft(task string, targets []exploreTarget) []exploreDraftEntry {
	query := shapeExploreQuery(task)
	conceptTask := exploreQueryIsConceptTask(query)
	if conceptTask {
		query = stripLeadingExploreDirective(query)
	}
	queryTerms := exploreTerminalTerms(query)

	// Cache body term sets once per direct candidate. The bounded frequency
	// table supplies a small inverse-frequency/density tie-break without
	// repeatedly tokenizing source or allowing body prose to dominate names.
	bodyTermCache := make(map[string]map[string]struct{}, len(targets))
	candidateTermCache := make(map[string]map[string]struct{}, len(targets))
	termFrequency := make(map[string]int, len(queryTerms))
	if conceptTask {
		for _, target := range targets {
			if target.node == nil {
				continue
			}
			key := exploreDraftNodeKey(target.node)
			if _, cached := candidateTermCache[key]; cached {
				continue
			}
			bodyTerms := exploreTerminalTerms(target.source)
			bodyTermCache[key] = bodyTerms
			candidateTerms := exploreDraftNodeTerms(target.node)
			for term := range bodyTerms {
				candidateTerms[term] = struct{}{}
			}
			candidateTermCache[key] = candidateTerms
			for term := range queryTerms {
				if _, matched := candidateTerms[term]; matched {
					termFrequency[term]++
				}
			}
		}
	}
	makeEntry := func(n *graph.Node, source, evidence string, direct bool, parentRank int) (exploreDraftEntry, int) {
		overlap, longest := exploreDraftTermOverlap(queryTerms, n)
		explicitAnchor := exploreLocalizationExplicitAnchor(query, n) ||
			exploreTaskQualifiedSyntacticAnchorMatchesNode(task, n)
		exact := exploreDraftExactAnchor(query, n) || explicitAnchor
		// Concept prompts often mention a nearby test helper while asking how the
		// production path works. Do not let that lexical anchor outrank the
		// implementation. A genuinely explicit symbol/path request remains exact.
		conceptTestHelper := conceptTask && exploreDraftIsTestNode(n) && !explicitAnchor
		if conceptTestHelper {
			exact = false
		}
		bodyOverlap, rareOverlap, bodyDensity := 0, 0, 0
		if conceptTask && n != nil {
			key := exploreDraftNodeKey(n)
			bodyTerms, cached := bodyTermCache[key]
			if !cached && source != "" {
				bodyTerms = exploreTerminalTerms(source)
				bodyTermCache[key] = bodyTerms
			}
			bodyOverlap = exploreDraftTermSetOverlap(queryTerms, bodyTerms)
			if candidateTerms, cached := candidateTermCache[key]; cached {
				for term := range queryTerms {
					if termFrequency[term] == 1 {
						if _, matched := candidateTerms[term]; matched && rareOverlap < 2 {
							rareOverlap++
						}
					}
				}
			}
			denominator := len(bodyTerms)
			if denominator > 12 {
				denominator = 12
			}
			if denominator > 0 && bodyOverlap >= 2 {
				switch {
				case bodyOverlap*2 >= denominator:
					bodyDensity = 2
				case bodyOverlap*4 >= denominator:
					bodyDensity = 1
				}
			}
		}
		return exploreDraftEntry{
			node:        n,
			evidence:    evidence,
			exact:       exact,
			overlap:     overlap,
			bodyOverlap: bodyOverlap,
			rareOverlap: rareOverlap,
			bodyDensity: bodyDensity,
			generic:     conceptTask && !exact && (conceptTestHelper || exploreDraftGenericCandidate(n, source)),
			direct:      direct,
			parentRank:  parentRank,
		}, longest
	}

	entries := make([]exploreDraftEntry, 0, exploreDraftTotalLimit)
	seen := make(map[string]struct{}, exploreDraftTotalLimit)
	appendEntry := func(entry exploreDraftEntry) bool {
		if entry.node == nil {
			return false
		}
		key := exploreDraftNodeKey(entry.node)
		if _, ok := seen[key]; ok {
			return false
		}
		seen[key] = struct{}{}
		entries = append(entries, entry)
		return true
	}

	// Preserve the retrieval head, then reserve at most one structural caller
	// and one structural callee. The global quotas keep graph expansion useful
	// without allowing a generic call neighborhood to consume the whole draft.
	for i := 0; i < len(targets) && i < exploreDraftPrimaryLimit; i++ {
		entry, _ := makeEntry(targets[i].node, targets[i].source, fmt.Sprintf("ranked #%d", i+1), true, i)
		appendEntry(entry)
	}

	var aligned, structuralNeighbors, protectedStructuralNeighbors []exploreDraftEntry
	protectedStructuralMentions := make(map[string]int)
	consider := func(n *graph.Node, source, evidence string, direct bool, parentRank int) {
		if n == nil || (!direct && exploreDraftIsTestNode(n)) {
			return
		}
		entry, longest := makeEntry(n, source, evidence, direct, parentRank)
		if !entry.exact && entry.bodyOverlap < 2 && entry.overlap < 2 && (entry.overlap != 1 || longest < 5) {
			return
		}
		aligned = append(aligned, entry)
	}
	for i, target := range targets {
		if i >= exploreDraftPrimaryLimit {
			consider(target.node, target.source, fmt.Sprintf("ranked #%d", i+1), true, i)
		}
		for _, n := range target.callers {
			consider(n, "", fmt.Sprintf("caller of ranked #%d", i+1), false, i)
		}
		for _, n := range target.callees {
			consider(n, "", fmt.Sprintf("callee of ranked #%d", i+1), false, i)
		}

		if target.node == nil || !conceptTask || exploreDraftIsTestNode(target.node) {
			continue
		}
		parent, longest := makeEntry(target.node, target.source, "", true, i)
		strongParent := parent.exact || parent.overlap >= 2 || parent.bodyOverlap >= 2 || (parent.overlap == 1 && longest >= 5)
		if !strongParent || (!parent.exact && exploreIdentifierSegmentCount(target.node.Name) < 2) {
			continue
		}
		collectStructural := func(n *graph.Node, evidence string, callee bool, hop int) {
			if n == nil || exploreDraftIsTestNode(n) || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
				return
			}
			mentions := 0
			shortProtected := false
			if target.conceptImplementation && callee && hop == 1 && exploreSameCallableOwner(target.node, n) {
				mentions = exploreBodyIdentifierMentions(target.source, n.Name)
				shortProtected = mentions > 0
			}
			if hop <= 1 && exploreIdentifierSegmentCount(n.Name) < 3 && !shortProtected {
				return
			}
			entry, _ := makeEntry(n, "", evidence, false, i)
			shared, local := exploreStructuralSignals(target.node, n)
			crossFile := nodeDisplayPath(target.node) != nodeDisplayPath(n)
			// A concrete cross-file callee is causal graph evidence even when its
			// name shares no useful query token with the parent. A protected
			// implementation may instead contribute one short same-owner callee
			// when its already-loaded body names that callee.
			causalCrossFile := callee && crossFile
			if shared == 0 && !entry.exact && entry.overlap == 0 && entry.bodyOverlap == 0 && !local && !causalCrossFile && !shortProtected {
				return
			}
			entry.structural = true
			entry.structuralShared = shared
			entry.structuralLocal = local
			entry.structuralCallee = callee
			// The existing single-source materializer keys on this bounded
			// boundary flag. A protected same-owner callee is equally source-worthy
			// because its parent body explicitly names it, while still consuming
			// the same one structural slot and one read.
			entry.structuralCrossFile = crossFile || shortProtected
			entry.structuralHop = hop
			entry.parentExact = parent.exact
			entry.parentOverlap = parent.overlap
			if shortProtected {
				protectedStructuralMentions[exploreDraftNodeKey(n)] = mentions
				protectedStructuralNeighbors = append(protectedStructuralNeighbors, entry)
				return
			}
			structuralNeighbors = append(structuralNeighbors, entry)
		}
		for _, n := range target.callers {
			collectStructural(n, fmt.Sprintf("caller of ranked #%d", i+1), false, 1)
		}
		for _, n := range target.callees {
			collectStructural(n, fmt.Sprintf("callee of ranked #%d", i+1), true, 1)
		}
		for _, neighbor := range target.causalCallees {
			collectStructural(neighbor.node, fmt.Sprintf("%d-hop callee of ranked #%d", neighbor.hop, i+1), true, neighbor.hop)
		}
	}

	sort.SliceStable(aligned, func(i, j int) bool {
		return exploreDraftEntryLess(aligned[i], aligned[j])
	})
	sort.SliceStable(structuralNeighbors, func(i, j int) bool {
		return exploreStructuralEntryLess(structuralNeighbors[i], structuralNeighbors[j])
	})
	if len(protectedStructuralNeighbors) > 0 {
		sort.SliceStable(protectedStructuralNeighbors, func(i, j int) bool {
			left := protectedStructuralMentions[exploreDraftNodeKey(protectedStructuralNeighbors[i].node)]
			right := protectedStructuralMentions[exploreDraftNodeKey(protectedStructuralNeighbors[j].node)]
			if left != right {
				return left > right
			}
			return exploreStructuralEntryLess(protectedStructuralNeighbors[i], protectedStructuralNeighbors[j])
		})
		structuralNeighbors = append(protectedStructuralNeighbors[:1], structuralNeighbors...)
	}
	// Reserve one structural slot across both directions. A single causal
	// implementation edge adds useful breadth without displacing two ranked
	// candidates from the compact draft.
	for _, entry := range structuralNeighbors {
		if appendEntry(entry) {
			break
		}
	}
	for _, entry := range aligned {
		if len(entries) >= exploreDraftTotalLimit {
			break
		}
		appendEntry(entry)
	}
	for i := exploreDraftPrimaryLimit; i < len(targets) && len(entries) < exploreDraftTotalLimit; i++ {
		entry, _ := makeEntry(targets[i].node, targets[i].source, fmt.Sprintf("ranked #%d", i+1), true, i)
		appendEntry(entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return exploreDraftEntryLess(entries[i], entries[j])
	})
	return entries
}

func exploreDraftEntryLess(a, b exploreDraftEntry) bool {
	alignment := func(entry exploreDraftEntry) int {
		body := entry.bodyOverlap
		if body > 2 {
			body = 2
		}
		return entry.overlap + body
	}
	priority := func(entry exploreDraftEntry) int {
		switch {
		case entry.exact && entry.direct:
			return 0
		case entry.exact:
			return 1
		case alignment(entry) > 0 && entry.direct:
			return 2
		case alignment(entry) > 0:
			return 3
		case entry.structural:
			return 4
		case entry.direct:
			return 5
		default:
			return 6
		}
	}
	if ap, bp := priority(a), priority(b); ap != bp {
		return ap < bp
	}
	if aa, ba := alignment(a), alignment(b); aa != ba {
		return aa > ba
	}
	if a.rareOverlap != b.rareOverlap {
		return a.rareOverlap > b.rareOverlap
	}
	if a.bodyDensity != b.bodyDensity {
		return a.bodyDensity > b.bodyDensity
	}
	// Exact anchors and stronger term alignment already won above. Generic
	// declarations are only a bounded tie-break within the same evidence
	// family; they can never demote a better-aligned candidate.
	if !a.exact && !b.exact && a.direct == b.direct && a.structural == b.structural && a.generic != b.generic {
		return !a.generic
	}
	if a.overlap != b.overlap {
		return a.overlap > b.overlap
	}
	if a.parentOverlap != b.parentOverlap {
		return a.parentOverlap > b.parentOverlap
	}
	if a.direct != b.direct {
		return a.direct
	}
	if a.parentRank != b.parentRank {
		return a.parentRank < b.parentRank
	}
	return exploreDraftNodeKey(a.node) < exploreDraftNodeKey(b.node)
}

func exploreStructuralEntryLess(a, b exploreDraftEntry) bool {
	causalAligned := func(entry exploreDraftEntry) bool {
		return entry.structuralCallee && entry.structuralCrossFile && (entry.exact || entry.overlap > 0 || entry.bodyOverlap >= 2 || entry.rareOverlap > 0)
	}
	if ac, bc := causalAligned(a), causalAligned(b); ac != bc {
		return ac
	}
	if a.exact != b.exact {
		return a.exact
	}
	if a.overlap != b.overlap {
		return a.overlap > b.overlap
	}
	if a.structuralShared != b.structuralShared {
		return a.structuralShared > b.structuralShared
	}
	if a.generic != b.generic {
		return !a.generic
	}
	if a.structuralCallee != b.structuralCallee {
		return a.structuralCallee
	}
	if a.structuralCrossFile != b.structuralCrossFile {
		return a.structuralCrossFile
	}
	if a.structuralHop != b.structuralHop {
		return a.structuralHop < b.structuralHop
	}
	if a.structuralLocal != b.structuralLocal {
		return a.structuralLocal
	}
	if a.parentExact != b.parentExact {
		return a.parentExact
	}
	if a.parentOverlap != b.parentOverlap {
		return a.parentOverlap > b.parentOverlap
	}
	if a.parentRank != b.parentRank {
		return a.parentRank < b.parentRank
	}
	return exploreDraftNodeKey(a.node) < exploreDraftNodeKey(b.node)
}

func exploreIdentifierSegmentCount(name string) int {
	return len(rerank.Tokenize(name))
}

func exploreIdentifierTerms(name string) map[string]struct{} {
	generic := map[string]struct{}{
		"and": {}, "for": {}, "from": {}, "get": {}, "has": {}, "into": {},
		"is": {}, "new": {}, "of": {}, "or": {}, "set": {}, "the": {},
		"to": {}, "with": {}, "without": {},
	}
	terms := make(map[string]struct{})
	for _, term := range rerank.Tokenize(name) {
		term = strings.ToLower(term)
		if len(term) < 3 {
			continue
		}
		if _, skip := generic[term]; skip {
			continue
		}
		terms[term] = struct{}{}
	}
	return terms
}

func exploreStructuralSignals(parent, child *graph.Node) (shared int, local bool) {
	parentTerms := exploreIdentifierTerms(parent.Name)
	for term := range exploreIdentifierTerms(child.Name) {
		if _, ok := parentTerms[term]; ok {
			shared++
		}
	}
	parentPath := nodeDisplayPath(parent)
	childPath := nodeDisplayPath(child)
	parentSlash := strings.LastIndex(parentPath, "/")
	childSlash := strings.LastIndex(childPath, "/")
	if parentSlash >= 0 && childSlash >= 0 && parentPath != childPath {
		local = parentPath[:parentSlash] == childPath[:childSlash]
	}
	return shared, local
}

func exploreDraftIsTestNode(n *graph.Node) bool {
	if n == nil {
		return false
	}
	if auditIsTestNode(n) {
		return true
	}
	path := strings.ToLower(strings.ReplaceAll(nodeDisplayPath(n), "\\", "/"))
	segmented := "/" + strings.Trim(path, "/") + "/"
	for _, dir := range []string{"/__tests__/", "/spec/", "/specs/", "/test/", "/tests/"} {
		if strings.Contains(segmented, dir) {
			return true
		}
	}
	// .NET/JVM test assemblies name their directory with a dotted token, e.g.
	// `Humanizer.Tests.Shared` — a real path segment that the slash-delimited
	// check above cannot see. Treat a dot-delimited test/spec token inside any
	// segment as test code so test metadata never suppresses production recall.
	for _, segment := range strings.Split(strings.Trim(path, "/"), "/") {
		for _, token := range strings.Split(segment, ".") {
			switch token {
			case "test", "tests", "spec", "specs":
				return true
			}
		}
	}
	base := path
	if slash := strings.LastIndex(base, "/"); slash >= 0 {
		base = base[slash+1:]
	}
	return strings.HasPrefix(base, "test_") || strings.Contains(base, "_test.") ||
		strings.Contains(base, ".spec.") || strings.Contains(base, ".test.")
}

func exploreDraftNodeTerms(n *graph.Node) map[string]struct{} {
	if n == nil {
		return map[string]struct{}{}
	}
	retrieval := n.RetrievalMetadata()
	return exploreTerminalTerms(n.Name + " " + retrieval.QualName + " " + nodeDisplayPath(n) + " " + retrieval.Signature)
}

func exploreDraftTermSetOverlap(queryTerms, candidateTerms map[string]struct{}) int {
	count := 0
	for term := range queryTerms {
		if _, ok := candidateTerms[term]; ok {
			count++
		}
	}
	return count
}

func exploreDraftTermOverlap(queryTerms map[string]struct{}, n *graph.Node) (count, longest int) {
	if n == nil || len(queryTerms) == 0 {
		return 0, 0
	}
	for term := range exploreDraftNodeTerms(n) {
		if _, ok := queryTerms[term]; !ok {
			continue
		}
		count++
		if len(term) > longest {
			longest = len(term)
		}
	}
	return count, longest
}

// exploreDraftGenericCandidate identifies structurally generic API context:
// interface/trait declarations, declaration-only methods, tiny accessors, and
// one-step receiver forwarders. It is language-neutral and only participates
// as a Concept-query tie-break; exact anchors bypass it at the call site.
func exploreDraftGenericCandidate(n *graph.Node, source string) bool {
	if n == nil {
		return false
	}
	nameTerms := rerank.Tokenize(n.Name)
	if len(nameTerms) > 0 && len(nameTerms) <= 3 {
		switch strings.ToLower(nameTerms[0]) {
		case "get", "set", "is", "has", "no":
			return true
		}
	}

	retrieval := n.RetrievalMetadata()
	declaration := strings.ToLower(strings.TrimSpace(retrieval.Signature + "\n" + source))
	for _, keyword := range []string{"interface ", "trait ", "protocol "} {
		if strings.Contains(declaration, keyword) {
			return true
		}
	}
	trimmed := strings.TrimSpace(declaration)
	if strings.HasSuffix(trimmed, ";") && (strings.Contains(trimmed, "(") || strings.Contains(trimmed, " function ")) {
		return true
	}
	if strings.TrimSpace(source) == "" {
		return false
	}

	lines := make([]string, 0, 8)
	for _, line := range strings.Split(strings.ToLower(source), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "/*") || strings.HasPrefix(line, "*") {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) > 10 {
		return false
	}
	code := " " + strings.Join(lines, " ") + " "
	if len(code) > 700 {
		return false
	}
	for _, control := range []string{" if ", " for ", " while ", " loop ", " match ", " switch ", " let ", " try ", " catch "} {
		if strings.Contains(code, control) {
			return false
		}
	}
	// Receiver syntax varies, but trivial implementations consistently contain
	// a member access plus one return/call or one field assignment. Inspect the
	// body separately so Rust tail expressions and C#/Kotlin expression bodies
	// are recognized even though they omit an explicit return keyword.
	body := code
	if open := strings.IndexByte(body, '{'); open >= 0 {
		body = body[open+1:]
		if close := strings.LastIndexByte(body, '}'); close >= 0 {
			body = body[:close]
		}
	}
	body = strings.TrimSpace(body)
	hasMemberAccess := strings.Contains(body, ".") || strings.Contains(body, "::")
	hasCall := strings.Contains(body, "(") && strings.Contains(body, ")")
	hasOneStepReturn := strings.Contains(" "+body+" ", " return ") && hasCall
	hasAccessorAssignment := strings.Contains(body, "=")
	hasExpressionBody := strings.Contains(body, "=>") && hasCall
	// A tail forwarder is one member call with no statement-level work around
	// it. One optional trailing semicolon covers Rust and expression-bodied
	// languages without classifying small control-flow implementations as shims.
	tail := strings.TrimSpace(strings.TrimSuffix(body, ";"))
	hasTailForwarder := hasCall && hasMemberAccess && !strings.Contains(tail, ";") &&
		!strings.Contains(tail, "{") && !strings.Contains(tail, "}")
	return hasMemberAccess && (hasOneStepReturn || hasAccessorAssignment || hasExpressionBody || hasTailForwarder)
}

// exploreLocalizationRefinementRoutes classifies the already-hydrated,
// bounded localization targets once. Every authorized candidate is a hydrated
// production callable. A known generic forwarder is authorized only when every
// plausible direct callable callee resolves to the same hydrated concrete target.
func exploreLocalizationRefinementRoutes(targets []exploreTarget) map[string]localizationRefinementRoute {
	byID := make(map[string]exploreTarget, len(targets))
	for _, target := range targets {
		if target.node != nil && target.node.ID != "" {
			byID[target.node.ID] = target
		}
	}
	routes := make(map[string]localizationRefinementRoute, len(byID))
	for _, target := range targets {
		if !exploreHydratedProductionCallable(target) {
			continue
		}
		if !exploreDraftGenericCandidate(target.node, target.source) {
			routes[target.node.ID] = localizationRefinementRoute{
				enforceable: localizationStrongSourceLiteralCallee(target),
			}
			continue
		}
		if !target.directCalleesComplete {
			continue
		}

		implementationSymbol := ""
		ambiguous := false
		for _, callee := range target.callees {
			if callee == nil || callee.ID == "" || callee.ID == target.node.ID ||
				exploreDraftIsTestNode(callee) ||
				(callee.Kind != graph.KindFunction && callee.Kind != graph.KindMethod) {
				continue
			}
			candidate, visible := byID[callee.ID]
			if !visible || !exploreHydratedProductionCallable(candidate) ||
				exploreDraftGenericCandidate(candidate.node, candidate.source) {
				ambiguous = true
				break
			}
			if implementationSymbol == "" {
				implementationSymbol = candidate.node.ID
				continue
			}
			if implementationSymbol != candidate.node.ID {
				ambiguous = true
				break
			}
		}
		if implementationSymbol != "" && !ambiguous {
			implementation := byID[implementationSymbol]
			routes[target.node.ID] = localizationRefinementRoute{
				implementationSymbol: implementationSymbol,
				enforceable:          localizationStrongImplementationRoute(target, implementation),
			}
		}
	}
	// The recommended read is normally the concrete implementation, not its
	// generic wrapper. Carry the wrapper's proof onto that exact route so the
	// one-read fast path preserves trust; ordinary concrete hydration remains
	// advisory. Ranked target order deterministically chooses among equivalent
	// proven wrappers.
	for _, target := range targets {
		if target.node == nil {
			continue
		}
		wrapperRoute, ok := routes[target.node.ID]
		if !ok || !wrapperRoute.enforceable || wrapperRoute.implementationSymbol == "" {
			continue
		}
		implementationRoute, ok := routes[wrapperRoute.implementationSymbol]
		if !ok || implementationRoute.implementationSymbol != "" || implementationRoute.proofSymbol != "" {
			continue
		}
		implementationRoute.enforceable = true
		implementationRoute.proofSymbol = target.node.ID
		routes[wrapperRoute.implementationSymbol] = implementationRoute
	}
	return routes
}

func exploreHydratedProductionCallable(target exploreTarget) bool {
	if target.node == nil || target.node.ID == "" || exploreDraftIsTestNode(target.node) {
		return false
	}
	if target.node.Kind != graph.KindFunction && target.node.Kind != graph.KindMethod {
		return false
	}
	return strings.TrimSpace(target.source) != ""
}

func explorePreferredRoutedRefinementSymbol(
	preferred string,
	targets []exploreTarget,
	routes map[string]localizationRefinementRoute,
) string {
	if route, authorized := routes[preferred]; authorized {
		if route.implementationSymbol == "" {
			return preferred
		}
		if implementationRoute, implementationAuthorized := routes[route.implementationSymbol]; implementationAuthorized && implementationRoute.implementationSymbol == "" {
			return route.implementationSymbol
		}
	}
	// The semantic preferred target can be generic, ambiguous, or unhydrated.
	// Fall back deterministically to the first ranked concrete authorized target;
	// a generic wrapper is never recommended even when it is a valid alternate.
	for _, target := range targets {
		if target.node == nil {
			continue
		}
		if route, authorized := routes[target.node.ID]; authorized && route.implementationSymbol == "" {
			return target.node.ID
		}
	}
	return ""
}

// boundedLocalizationRefinementRoutes intersects precomputed routes with the
// symbols that survived envelope budgeting. A generic route is retained only
// when its concrete hop is visible in the same envelope.
func boundedLocalizationRefinementRoutes(
	symbols []string,
	routes map[string]localizationRefinementRoute,
	preferredSymbol string,
) ([]string, map[string]localizationRefinementRoute) {
	visible := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		visible[symbol] = struct{}{}
	}
	authorized := make([]string, 0, min(len(symbols), localizationRefinementAllowedSymbolCap))
	bounded := make(map[string]localizationRefinementRoute, min(len(symbols), localizationRefinementAllowedSymbolCap))
	appendAuthorized := func(symbol string) {
		if symbol == "" || len(authorized) >= localizationRefinementAllowedSymbolCap {
			return
		}
		if _, duplicate := bounded[symbol]; duplicate {
			return
		}
		route, ok := routes[symbol]
		if !ok {
			return
		}
		if route.implementationSymbol != "" {
			if _, implementationVisible := visible[route.implementationSymbol]; !implementationVisible {
				return
			}
		}
		if route.proofSymbol != "" {
			if _, proofVisible := visible[route.proofSymbol]; !proofVisible {
				return
			}
		}
		authorized = append(authorized, symbol)
		bounded[symbol] = route
	}
	// The recommendation must stay visible and authorized even when ranking
	// places it after the alternate recovery window.
	appendAuthorized(preferredSymbol)
	for _, symbol := range symbols {
		appendAuthorized(symbol)
	}
	return authorized, bounded
}

func exploreLocalizationTargetSymbols(targets []exploreTarget) []string {
	symbols := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.node != nil && target.node.ID != "" {
			symbols = append(symbols, target.node.ID)
		}
	}
	return symbols
}

func exploreDraftExactAnchor(query string, n *graph.Node) bool {
	if n == nil {
		return false
	}
	if pathAnchors, hasDirectory := exploreQueryPathAnchors(query); hasDirectory {
		normalizedPath := exploreNormalizedPath(nodeDisplayPath(n))
		for _, anchor := range pathAnchors {
			if normalizedPath == anchor {
				return true
			}
		}
		// Directory-qualified requests are strict: neither a coincident symbol
		// name nor the same basename in another directory is an exact draft hit.
		return false
	}
	normalize := func(text string) string {
		return strings.ToLower(strings.Join(rerank.Tokenize(text), " "))
	}
	genericName := map[string]struct{}{
		"clear": {}, "convert": {}, "get": {}, "set": {}, "write": {},
	}
	query = " " + normalize(query) + " "
	for _, anchor := range []string{n.Name, n.QualName} {
		anchor = normalize(anchor)
		if _, generic := genericName[anchor]; generic {
			continue
		}
		if len(anchor) >= 3 && strings.Contains(query, " "+anchor+" ") {
			return true
		}
	}
	path := nodeDisplayPath(n)
	if slash := strings.LastIndex(path, "/"); slash >= 0 {
		path = path[slash+1:]
	}
	path = normalize(path)
	return len(path) >= 3 && strings.Contains(query, " "+path+" ")
}

func exploreDraftNodeKey(n *graph.Node) string {
	if n == nil {
		return ""
	}
	if n.ID != "" {
		return n.ID
	}
	return fmt.Sprintf("%s|%s|%s|%d", nodeDisplayPath(n), n.Kind, n.Name, n.StartLine)
}

func exploreDraftSymbol(n *graph.Node) string {
	if n.QualName != "" {
		return n.QualName
	}
	return n.Name
}

// exploreSingleQualifiedImplementation recognizes the narrow case where one
// hydrated production callable, and only one, satisfies the same evidence
// threshold that made localization answer-ready. Supporting graph rows may
// remain visible, but none can independently justify another implementation.
func exploreSingleQualifiedImplementation(task string, targets []exploreTarget) bool {
	// This cue is intentionally narrower than answer readiness. The direct
	// bounded result must contain exactly one hydrated production callable;
	// supporting graph relations may still be rendered later, but a selected
	// alternate can never be described as absent merely because it ranked lower.
	if len(targets) != 1 || len(exploreQuotedRecallClaimTerms(task)) > 0 ||
		!exploreHydratedProductionCallable(targets[0]) {
		return false
	}
	return exploreAnswerReady(task, targets)
}

func exploreLocalizationCompletion(
	answerReady, artifactReady bool,
	task string,
	targets []exploreTarget,
	exactSymbol string,
) localizationCompletion {
	if answerReady && !artifactReady && exploreSingleQualifiedImplementation(task, targets) {
		return newLocalizationSingleResultCompletion()
	}
	return newLocalizationCompletion(answerReady, exactSymbol)
}

// exploreLocalizationUsesConceptRefinement distinguishes an anchor mentioned as
// evidence inside prose from a compact lookup target. A graph-proven causal
// identity remains exact because its exclusivity comes from a complete route,
// not merely from text that happened to contain a declaration name.
func exploreLocalizationUsesConceptRefinement(answerReady bool, task, exactSymbol, causalExactSymbol string) bool {
	return !answerReady && causalExactSymbol == "" && exactSymbol != "" && exploreQueryIsConceptTask(task)
}

// exploreAnswerReady decides whether the ranked head is strong enough to end
// localization. A result is terminal only when its symbols visibly align with
// the shaped query; rank alone is not enough because broad review/audit prompts
// can otherwise elevate common identifiers such as current, change, or client.
func exploreAnswerReady(task string, targets []exploreTarget) bool {
	if len(targets) == 0 || targets[0].node == nil {
		return false
	}
	query := shapeExploreQuery(task)
	class := exploreQueryClass(query)
	if class == rerank.QueryClassConcept {
		query = stripLeadingExploreDirective(query)
	}
	queryTerms := exploreTerminalTerms(query)
	if len(queryTerms) == 0 {
		return false
	}

	matchedNodeFor := func(node *graph.Node, terms map[string]struct{}) int {
		if node == nil {
			return 0
		}
		candidateTerms := exploreTerminalTerms(exploreTerminalNodeText(node))
		count := 0
		for term := range terms {
			if _, ok := candidateTerms[term]; ok {
				count++
			}
		}
		return count
	}
	matchedNode := func(node *graph.Node) int { return matchedNodeFor(node, queryTerms) }
	head := targets[0]
	headMatches := matchedNode(head.node)
	strongSourceLiteral := head.sourceLiteral && head.sourceLiteralCallee && !head.exactContentAmbiguous
	if class == rerank.QueryClassConcept && !exploreLocalizationExplicitAnchor(query, head.node) &&
		!exploreSyntacticAnchorEvidenceReady(task, targets) && !strongSourceLiteral {
		return false
	}
	// Paths, signatures, and identifier-shaped queries carry explicit anchors.
	// A shared token is not enough: the ranked head must cover the complete
	// path, qualified symbol, or signature anchor from the request.
	if class == rerank.QueryClassPath || class == rerank.QueryClassSymbol || class == rerank.QueryClassSignature {
		return exploreLocalizationExplicitAnchor(query, head.node)
	}
	// A unique verified full quoted literal is direct implementation evidence.
	// Saturated pages and visible exact peers take the guarded path below.
	if head.exactContent && !head.exactContentAmbiguous {
		return true
	}

	// Structural evidence is useful only when a directly connected helper
	// independently covers multiple task concepts. Merely spreading one token
	// each across unrelated top-ranked symbols is not sufficient.
	structuralAligned := false
	for _, neighbors := range [][]*graph.Node{head.callers, head.callees} {
		for _, neighbor := range neighbors {
			if matchedNode(neighbor) >= 2 {
				structuralAligned = true
				break
			}
		}
		if structuralAligned {
			break
		}
	}

	if head.exactContent {
		nonLiteralTerms := make(map[string]struct{}, len(queryTerms))
		for term := range queryTerms {
			nonLiteralTerms[term] = struct{}{}
		}
		for _, literal := range exploreQuotedRecallTerms(task) {
			for term := range exploreTerminalTerms(literal) {
				delete(nonLiteralTerms, term)
			}
		}
		headEvidence := matchedNodeFor(head.node, nonLiteralTerms)
		if headEvidence == 0 {
			return false
		}
		for _, peer := range targets[1:] {
			if peer.node == nil || !peer.exactContent || peer.node.ID == head.node.ID {
				continue
			}
			if matchedNodeFor(peer.node, nonLiteralTerms) >= headEvidence {
				return false
			}
		}
		return headEvidence >= 2 || (headEvidence >= 1 && structuralAligned)
	}
	if exploreLocalizationExplicitAnchor(query, head.node) {
		return true
	}
	// Quoted recall terms make a factual source claim. When the ranked head has
	// no verified exact literal evidence, ordinary concept alignment must not
	// turn that claim into a terminal answer. Explicit path/symbol/signature
	// anchors and the unique/ambiguous exact paths above retain their behavior.
	if len(exploreQuotedRecallClaimTerms(task)) > 0 {
		return false
	}

	// Ordinary non-literal concept localization needs one identifier-backed
	// callable with a real body. Fields, enum variants, types, and signature-only
	// declarations remain useful evidence, but cannot terminate navigation.
	var implementation *exploreTarget
	if class == rerank.QueryClassConcept {
		hasSyntacticAnchors := exploreAuthoredSyntacticAnchorCount(exploreSyntacticAnchors(task)) > 0
		for i := range targets {
			target := &targets[i]
			callable := target.node != nil &&
				(target.node.Kind == graph.KindFunction || target.node.Kind == graph.KindMethod)
			if target.conceptImplementation && callable && strings.TrimSpace(target.source) != "" &&
				(!hasSyntacticAnchors || !exploreDraftGenericCandidate(target.node, target.source)) {
				implementation = target
				break
			}
		}
		if implementation == nil {
			return false
		}

		implementationMatches := matchedNode(implementation.node)
		identifierOverlap, longest := exploreDraftTermOverlap(queryTerms, implementation.node)
		identifierStrong := identifierOverlap >= 2 ||
			(identifierOverlap == 1 && longest >= 5 && exploreIdentifierSegmentCount(implementation.node.Name) >= 2)
		implementationStructural := false
		for _, neighbors := range [][]*graph.Node{implementation.callers, implementation.callees} {
			for _, neighbor := range neighbors {
				if matchedNode(neighbor) >= 2 {
					implementationStructural = true
				}
			}
		}
		hasCallableCallee := false
		for _, callee := range implementation.callees {
			if callee != nil && (callee.Kind == graph.KindFunction || callee.Kind == graph.KindMethod) {
				hasCallableCallee = true
				break
			}
		}
		if len(queryTerms) > 10 {
			return implementationMatches >= 3 ||
				(implementationMatches >= 1 && implementationStructural) ||
				(identifierStrong && hasCallableCallee)
		}
		if len(queryTerms) == 1 {
			return false
		}
		return implementationMatches >= 2 ||
			(implementationMatches >= 1 && implementationStructural) ||
			(identifierStrong && hasCallableCallee)
	}

	// Non-concept prompts retain the existing head-centric confidence rule.
	if len(queryTerms) > 10 {
		return headMatches >= 3 || (headMatches >= 1 && structuralAligned)
	}
	if len(queryTerms) == 1 {
		return false
	}
	return headMatches >= 2 || (headMatches >= 1 && structuralAligned)
}

func exploreNormalizedAnchor(text string) string {
	return strings.ToLower(strings.Join(rerank.Tokenize(text), " "))
}

func exploreNormalizedPath(text string) string {
	text = strings.TrimSpace(text)
	text = strings.Trim(text, "`'\"()[]{}<>,;")
	text = strings.ReplaceAll(text, "\\", "/")
	text = strings.TrimPrefix(text, "./")
	parts := strings.Split(text, "/")
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := exploreNormalizedAnchor(part); value != "" {
			normalized = append(normalized, value)
		}
	}
	return strings.Join(normalized, "/")
}

func exploreQueryPathAnchors(query string) ([]string, bool) {
	fields := make([]string, 0, 2)
	routeLike := 0
	for _, raw := range strings.Fields(query) {
		field := strings.Trim(raw, "`'\"()[]{}<>,;")
		// A route is commonly embedded in a call token such as
		// r.GET("/base/:id"). Extract the quoted value before classifying it;
		// treating the whole call as a source path poisons every later basename
		// match in the request.
		if quote := strings.IndexAny(field, "`'\""); quote >= 0 {
			field = field[quote+1:]
			if end := strings.IndexAny(field, "`'\""); end >= 0 {
				field = field[:end]
			}
			field = strings.Trim(field, "`'\"()[]{}<>,;")
		}
		if !strings.ContainsAny(field, "/\\") || strings.Contains(field, "://") {
			continue
		}
		// A fully qualified Go stack symbol (`github.com/acme/pkg.(*T).M`) is
		// a package-qualified callable, not a user-supplied source directory.
		// Let a later basename/file:line anchor such as `tree.go:637` match
		// instead of making this package token veto every basename.
		if strings.Contains(field, "(*") && strings.Contains(field, ").") {
			continue
		}
		fields = append(fields, field)
		normalized := strings.ReplaceAll(field, "\\", "/")
		if strings.HasPrefix(normalized, "/") {
			base := normalized[strings.LastIndex(normalized, "/")+1:]
			dot := strings.LastIndex(base, ".")
			if dot <= 0 || dot == len(base)-1 {
				routeLike++
			}
		}
	}

	anchors := make([]string, 0, len(fields))
	hasDirectory := false
	for _, field := range fields {
		normalized := strings.ReplaceAll(field, "\\", "/")
		base := normalized[strings.LastIndex(normalized, "/")+1:]
		dot := strings.LastIndex(base, ".")
		rootedWithoutExtension := strings.HasPrefix(normalized, "/") && (dot <= 0 || dot == len(base)-1)
		if rootedWithoutExtension && (routeLike >= 2 || strings.Contains(normalized, "/:") || strings.ContainsAny(normalized, "{}")) {
			continue
		}
		hasDirectory = true
		if anchor := exploreNormalizedPath(field); anchor != "" {
			anchors = append(anchors, anchor)
		}
	}
	return anchors, hasDirectory
}

func exploreLocalizationExplicitAnchor(query string, n *graph.Node) bool {
	if n == nil {
		return false
	}
	normalizedQueryText := exploreNormalizedAnchor(query)
	normalizedQuery := " " + normalizedQueryText + " "
	retrieval := n.RetrievalMetadata()
	name := exploreNormalizedAnchor(n.Name)
	qualName := exploreNormalizedAnchor(retrieval.QualName)
	if qualName == "" || qualName == name {
		if separator := strings.LastIndex(n.ID, "::"); separator >= 0 {
			qualName = exploreNormalizedAnchor(n.ID[separator+2:])
		}
	}
	if exploreQueryHasPathSymbolAnchor(query, n) || exploreQueryHasCallAnchor(query, n.Name) {
		return true
	}
	if qualName != "" && qualName != name && strings.Contains(normalizedQuery, " "+qualName+" ") {
		return true
	}

	path := nodeDisplayPath(n)
	normalizedPath := exploreNormalizedPath(path)
	for _, field := range strings.Fields(query) {
		field = strings.Trim(field, "`'\"()[]{}<>,;")
		separator := strings.LastIndex(field, "::")
		if separator <= 0 || !strings.ContainsAny(field[:separator], "/\\") {
			continue
		}
		if exploreNormalizedPath(field[:separator]) != normalizedPath {
			continue
		}
		symbol := exploreNormalizedAnchor(field[separator+2:])
		if symbol == name || symbol == qualName || strings.HasSuffix(qualName, " "+symbol) {
			return true
		}
	}
	pathAnchors, hasDirectory := exploreQueryPathAnchors(query)
	if hasDirectory {
		for _, anchor := range pathAnchors {
			if normalizedPath == anchor {
				return true
			}
		}
		// A directory-qualified request must never degrade to a basename match;
		// that would make same-name files in different packages terminal.
		return false
	}
	if slash := strings.LastIndex(path, "/"); slash >= 0 {
		path = path[slash+1:]
	}
	basename := exploreNormalizedAnchor(path)
	if len(basename) >= 3 && strings.Contains(normalizedQuery, " "+basename+" ") {
		return true
	}

	class := exploreQueryClass(query)
	if class == rerank.QueryClassSignature {
		signature := exploreNormalizedAnchor(retrieval.Signature)
		return signature != "" && strings.Contains(normalizedQuery, " "+signature+" ")
	}
	// An unqualified symbol cannot disambiguate same-name methods. Permit it
	// only for nodes whose canonical name is itself unqualified.
	return class == rerank.QueryClassSymbol && qualName == name && normalizedQueryText == name
}

// exploreLocalizationExplicitTarget returns only a concrete path, qualified
// symbol, signature, or call anchor. Concept-term overlap is intentionally not
// accepted here: it can rank a neighborhood but must not prescribe the one
// allowed post-localization source read.
func exploreLocalizationExplicitTarget(task string, targets []exploreTarget) string {
	// Preserve exact qualified members from the untouched issue before shaping
	// can truncate a late Owner::member mention out of a long report body.
	for _, target := range targets {
		if target.node != nil && exploreTaskQualifiedSyntacticAnchorMatchesNode(task, target.node) {
			return target.node.ID
		}
	}
	query := shapeExploreQuery(task)
	if exploreQueryClass(query) == rerank.QueryClassConcept {
		query = stripLeadingExploreDirective(query)
	}
	for _, target := range targets {
		if target.node != nil && exploreLocalizationExplicitAnchor(query, target.node) {
			return target.node.ID
		}
	}
	return ""
}

// exploreLocalizationExactTarget selects the strongest exact-like target for
// ordering and backwards-compatible callers. Explicit qualified/path anchors
// win; concept overlap may still pick a stable evidence head, but the facade
// uses exploreLocalizationExplicitTarget before issuing a refinement read.
// natural-language tasks, candidates with more independent task terms win and
// inverse candidate frequency breaks ties so repeated generic setters do not
// outrank a rarer conjunctive implementation target. Retrieval order is the
// stable final tie-breaker.
func exploreLocalizationExactTarget(task string, targets []exploreTarget) string {
	query := shapeExploreQuery(task)
	class := exploreQueryClass(query)
	if class == rerank.QueryClassConcept {
		query = stripLeadingExploreDirective(query)
	}
	for _, target := range targets {
		if target.node != nil && exploreLocalizationExplicitAnchor(query, target.node) {
			return target.node.ID
		}
	}
	queryTerms := exploreTerminalTerms(query)
	if len(queryTerms) == 0 {
		return ""
	}

	matches := make([]map[string]struct{}, len(targets))
	frequency := make(map[string]int)
	for i, target := range targets {
		matches[i] = make(map[string]struct{})
		if target.node == nil {
			continue
		}
		for term := range exploreTerminalTerms(exploreTerminalNodeText(target.node)) {
			if _, relevant := queryTerms[term]; !relevant {
				continue
			}
			matches[i][term] = struct{}{}
			frequency[term]++
		}
	}

	best := -1
	bestOverlap := -1
	bestRarity := -1
	for i, target := range targets {
		if target.node == nil {
			continue
		}
		overlap := len(matches[i])
		rarity := 0
		for term := range matches[i] {
			if frequency[term] > 0 {
				rarity += 1000 / frequency[term]
			}
		}
		if best < 0 || overlap > bestOverlap || (overlap == bestOverlap && rarity > bestRarity) {
			best = i
			bestOverlap = overlap
			bestRarity = rarity
		}
	}
	if best < 0 || bestOverlap == 0 {
		return ""
	}
	// Concept localization needs two independent lexical anchors before it may
	// prescribe an exact source read. They may jointly support an equal-evidence
	// neighborhood, in which case retrieval order remains the stable tie-breaker.
	// Repeating one generic token (for example walk/walker/walking) still leaves
	// frequency with one key and cannot turn retrieval order into exactness.
	if class == rerank.QueryClassConcept && bestOverlap < 2 {
		return ""
	}
	return targets[best].node.ID
}

func exploreTerminalNodeText(n *graph.Node) string {
	if n == nil {
		return ""
	}
	retrieval := n.RetrievalMetadata()
	return strings.TrimSpace(strings.Join([]string{n.Name, retrieval.QualName, n.FilePath, retrieval.Signature}, " "))
}

var exploreTerminalGenericTerms = map[string]struct{}{
	"audit": {}, "behavior": {}, "blocker": {}, "branch": {}, "change": {},
	"check": {}, "code": {}, "correctness": {}, "current": {}, "file": {},
	"find": {}, "fix": {}, "identify": {}, "implement": {}, "investigate": {},
	"issue": {}, "problem": {}, "quality": {}, "release": {}, "review": {},
	"source": {}, "symbol": {}, "task": {}, "validate": {},
}

func exploreTerminalTerms(text string) map[string]struct{} {
	terms := make(map[string]struct{})
	for _, raw := range rerank.Tokenize(text) {
		term := strings.ToLower(strings.TrimSpace(raw))
		if len(term) < 3 {
			continue
		}
		if _, stop := assistStopWords[term]; stop {
			continue
		}
		term = exploreTerminalTermRoot(term)
		if _, generic := exploreTerminalGenericTerms[term]; generic {
			continue
		}
		terms[term] = struct{}{}
	}
	return terms
}

func exploreTerminalTermRoot(term string) string {
	if len(term) > 4 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss") {
		return strings.TrimSuffix(term, "s")
	}
	return term
}

var exploreConceptComplementLowValueTerms = map[string]struct{}{
	"arg": {}, "argument": {}, "debug": {}, "diagnostic": {}, "input": {},
	"line": {}, "log": {}, "output": {}, "path": {}, "read": {},
	"reader": {}, "search": {}, "trace": {}, "version": {},
}

// exploreConceptIssueLead returns the report headline carried by the canonical
// explore query. shapeExploreQuery repeats a multiline headline before the
// distilled body; inline agent paraphrases keep it as their first sentence.
func exploreConceptIssueLead(query string) string {
	lead := strings.TrimSpace(query)
	if lead == "" {
		return ""
	}
	end := len(lead)
	for _, separator := range [...]string{"\n", "\r", ". ", "? ", "! ", "; ", " - "} {
		if index := strings.Index(lead, separator); index >= 12 && index < end {
			end = index
		}
	}
	return strings.TrimSpace(lead[:end])
}

// exploreConceptBehavioralTermGroup collapses the bounded inflection family
// accepted by exploreConceptTermPresent. Each behavioral root can therefore
// contribute at most one independent issue-lead group.
func exploreConceptBehavioralTermGroup(term string) string {
	for _, suffix := range [...]string{"ing", "ed"} {
		if !strings.HasSuffix(term, suffix) {
			continue
		}
		stem := strings.TrimSuffix(term, suffix)
		if len(stem) < 5 {
			continue
		}
		if len(stem) > 1 && stem[len(stem)-1] == stem[len(stem)-2] {
			stem = stem[:len(stem)-1]
		}
		return stem
	}
	return term
}

func exploreConceptDiagnosticLiteral(token string) bool {
	token = strings.Trim(token, "`'\"()[]{}<>,;")
	if token == "" {
		return false
	}
	if colon := strings.LastIndexByte(token, ':'); colon >= 0 && colon+1 < len(token) {
		line := token[colon+1:]
		digits := true
		for index := 0; index < len(line); index++ {
			if line[index] < '0' || line[index] > '9' {
				digits = false
				break
			}
		}
		if digits {
			return true
		}
	}
	if !strings.ContainsAny(token, "/\\") {
		return false
	}
	separator := strings.LastIndexAny(token, "/\\")
	base := token[separator+1:]
	if colon := strings.LastIndexByte(base, ':'); colon >= 0 {
		base = base[:colon]
	}
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot+1 >= len(base) || len(base)-dot-1 > 8 {
		return false
	}
	for index := dot + 1; index < len(base); index++ {
		value := base[index]
		if !exploreASCIIAlphaNumeric(value) {
			return false
		}
	}
	return true
}

// exploreConceptComplementLowValueTermSet marks structural report noise only
// for complement selection. The ordinary retrieval and primary implementation
// ranking retain their established contract.
func exploreConceptComplementLowValueTermSet(query string) map[string]struct{} {
	low := make(map[string]struct{}, len(exploreConceptComplementLowValueTerms)+8)
	for term := range exploreConceptComplementLowValueTerms {
		low[term] = struct{}{}
	}
	for _, token := range strings.Fields(query) {
		if !exploreConceptDiagnosticLiteral(token) {
			continue
		}
		for _, raw := range rerank.Tokenize(token) {
			term := exploreTerminalTermRoot(strings.ToLower(strings.TrimSpace(raw)))
			if len(term) >= 3 {
				low[term] = struct{}{}
			}
		}
	}
	return low
}

func exploreConceptComplementHasTerm(node *graph.Node, term string) bool {
	if exploreConceptImplementationHasTerm(node, term) {
		return true
	}
	if node == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(node.Name + " " + node.QualName))
	return exploreConceptTermPresent(text, term)
}

func exploreConceptCallableFamily(name string) string {
	for _, raw := range rerank.Tokenize(name) {
		term := exploreTerminalTermRoot(strings.ToLower(strings.TrimSpace(raw)))
		if len(term) >= 3 {
			return exploreConceptBehavioralTermGroup(term)
		}
	}
	return ""
}

func exploreConceptOwnerFamily(node *graph.Node) string {
	if node == nil {
		return ""
	}
	owner := strings.TrimSpace(node.QualName)
	separator := strings.LastIndex(owner, "::")
	if dot := strings.LastIndexByte(owner, '.'); dot > separator {
		separator = dot
	}
	if separator <= 0 {
		return ""
	}
	return strings.ToLower(owner[:separator])
}

func exploreConceptComplementFamilyPenalty(node *graph.Node, reserved []*graph.Node) int {
	if node == nil {
		return 0
	}
	family := exploreConceptCallableFamily(node.Name)
	owner := exploreConceptOwnerFamily(node)
	file := strings.ToLower(strings.TrimSpace(node.FilePath))
	penalty := 0
	for _, other := range reserved {
		if other == nil {
			continue
		}
		if family != "" && family == exploreConceptCallableFamily(other.Name) {
			penalty += 2
		}
		if owner != "" && owner == exploreConceptOwnerFamily(other) {
			penalty += 2
		}
		if file != "" && file == strings.ToLower(strings.TrimSpace(other.FilePath)) {
			penalty++
		}
	}
	return penalty
}

type exploreConceptImplementationMetric struct {
	index       int
	overlap     int
	rare        int
	longest     int
	segments    int
	matchedMask uint64
}

const (
	exploreConceptComplementSignal = "explore_concept_complement"
	exploreSyntacticAnchorSignal   = "explore_syntactic_anchor"
)

// reserveExploreConceptImplementation keeps the strongest identifier-backed
type exploreComponentConjunctionKey struct {
	workspaceID string
	projectID   string
	repoPrefix  string
	directory   string
}

type exploreComponentConjunctionSeed struct {
	index   int
	node    *graph.Node
	file    string
	matched []bool
}

type exploreComponentConjunctionScore struct {
	groups   int
	marginal int
	rarity   int
	longest  int
}

type exploreComponentConjunctionWinner struct {
	index    int
	seedRank int
	score    exploreComponentConjunctionScore
}

func exploreComponentConjunctionFile(node *graph.Node) string {
	if node == nil {
		return ""
	}
	file := path.Clean(strings.ReplaceAll(strings.TrimSpace(node.FilePath), "\\", "/"))
	if file == "" || file == "." || file == "/" || file == ".." || strings.HasPrefix(file, "../") {
		return ""
	}
	return strings.ToLower(file)
}

func exploreComponentConjunctionComponent(node *graph.Node) (exploreComponentConjunctionKey, bool) {
	file := exploreComponentConjunctionFile(node)
	if file == "" {
		return exploreComponentConjunctionKey{}, false
	}
	directory := path.Dir(file)
	if directory == "" || directory == "." || directory == "/" || directory == ".." || strings.HasPrefix(directory, "../") {
		return exploreComponentConjunctionKey{}, false
	}
	return exploreComponentConjunctionKey{
		workspaceID: strings.ToLower(strings.TrimSpace(node.WorkspaceID)),
		projectID:   strings.ToLower(strings.TrimSpace(node.ProjectID)),
		repoPrefix:  strings.ToLower(strings.TrimSpace(node.RepoPrefix)),
		directory:   directory,
	}, true
}

func exploreComponentConjunctionMatches(node *graph.Node, terms []string) []bool {
	matched := make([]bool, len(terms))
	for index, term := range terms {
		matched[index] = exploreConceptImplementationHasTerm(node, term)
	}
	return matched
}

func exploreComponentConjunctionScoreCompare(left, right exploreComponentConjunctionScore) int {
	for _, pair := range [...][2]int{
		{left.groups, right.groups},
		{left.marginal, right.marginal},
		{left.rarity, right.rarity},
		{left.longest, right.longest},
	} {
		if pair[0] > pair[1] {
			return 1
		}
		if pair[0] < pair[1] {
			return -1
		}
	}
	return 0
}

// reserveExploreComponentConjunction recovers one callable whose identifier
// completes a task concept already visible in another file of the same exact
// component. It only reorders the bounded retrieval pool: no graph or source
// work is added, the semantic head is fixed, and ambiguous component ties are
// deliberately left untouched.
func reserveExploreComponentConjunction(
	query string,
	queryClass rerank.QueryClass,
	candidates []*rerank.Candidate,
	maxSymbols int,
) []*rerank.Candidate {
	if queryClass != rerank.QueryClassConcept || maxSymbols < 2 || len(candidates) < 2 {
		return candidates
	}
	width := min(maxSymbols, len(candidates))
	if width < 2 {
		return candidates
	}

	lowValue := exploreConceptComplementLowValueTermSet(query)
	queryTermSet := exploreTerminalTerms(query)
	terms := make([]string, 0, len(queryTermSet))
	groups := make(map[string]struct{}, len(queryTermSet))
	for term := range queryTermSet {
		if _, low := lowValue[term]; low {
			continue
		}
		terms = append(terms, term)
		groups[exploreConceptBehavioralTermGroup(term)] = struct{}{}
	}
	if len(terms) < 2 || len(groups) < 2 {
		return candidates
	}
	sort.Strings(terms)

	leadTerms := exploreTerminalTerms(exploreConceptIssueLead(query))
	lead := make([]bool, len(terms))
	leadCount := 0
	for index, term := range terms {
		if _, present := leadTerms[term]; present {
			lead[index] = true
			leadCount++
		}
	}
	if leadCount == 0 {
		return candidates
	}

	matches := make([][]bool, len(candidates))
	frequency := make([]int, len(terms))
	for candidateIndex, candidate := range candidates {
		if candidate == nil || candidate.Node == nil {
			continue
		}
		matches[candidateIndex] = exploreComponentConjunctionMatches(candidate.Node, terms)
		for termIndex, matched := range matches[candidateIndex] {
			if matched {
				frequency[termIndex]++
			}
		}
	}

	seeds := make(map[exploreComponentConjunctionKey][]exploreComponentConjunctionSeed)
	for index := 0; index < width; index++ {
		candidate := candidates[index]
		if candidate == nil || candidate.Node == nil || exploreDraftIsTestNode(candidate.Node) ||
			!exploreCodeDefinitionKind(candidate.Node.Kind) {
			continue
		}
		component, ok := exploreComponentConjunctionComponent(candidate.Node)
		if !ok {
			continue
		}
		taskAligned := false
		for _, matched := range matches[index] {
			if matched {
				taskAligned = true
				break
			}
		}
		if !taskAligned {
			continue
		}
		seeds[component] = append(seeds[component], exploreComponentConjunctionSeed{
			index: index, node: candidate.Node,
			file: exploreComponentConjunctionFile(candidate.Node), matched: matches[index],
		})
	}
	if len(seeds) == 0 {
		return candidates
	}

	sameNode := func(left, right *graph.Node) bool {
		if left == nil || right == nil {
			return false
		}
		if left.ID != "" && right.ID != "" {
			return left.ID == right.ID
		}
		return left == right
	}
	bestByComponent := make(map[exploreComponentConjunctionKey]exploreComponentConjunctionWinner)
	for index, candidate := range candidates {
		if candidate == nil || candidate.Node == nil || exploreDraftIsTestNode(candidate.Node) ||
			(candidate.Node.Kind != graph.KindFunction && candidate.Node.Kind != graph.KindMethod) ||
			exploreDraftGenericCandidate(candidate.Node, "") {
			continue
		}
		component, ok := exploreComponentConjunctionComponent(candidate.Node)
		if !ok {
			continue
		}
		componentSeeds := seeds[component]
		if len(componentSeeds) == 0 {
			continue
		}

		base := make([]bool, len(terms))
		candidateFile := exploreComponentConjunctionFile(candidate.Node)
		seedRank := len(candidates)
		hasOtherSeed := false
		differentFile := false
		for _, seed := range componentSeeds {
			if sameNode(seed.node, candidate.Node) {
				continue
			}
			hasOtherSeed = true
			if seed.index < seedRank {
				seedRank = seed.index
			}
			if seed.file != candidateFile {
				differentFile = true
			}
			for termIndex, matched := range seed.matched {
				base[termIndex] = base[termIndex] || matched
			}
		}
		if !hasOtherSeed || !differentFile {
			continue
		}

		candidateMatched := 0
		marginal := 0
		rarity := 0
		longest := 0
		unionGroups := make(map[string]struct{}, len(groups))
		leadAligned := false
		for termIndex, term := range terms {
			candidateHasTerm := matches[index][termIndex]
			if candidateHasTerm {
				candidateMatched++
				if len(term) > longest {
					longest = len(term)
				}
				if !base[termIndex] {
					marginal++
					denominator := frequency[termIndex]
					if denominator < 1 {
						denominator = 1
					}
					rarity += 1000 / denominator
				}
			}
			if base[termIndex] || candidateHasTerm {
				unionGroups[exploreConceptBehavioralTermGroup(term)] = struct{}{}
				leadAligned = leadAligned || lead[termIndex]
			}
		}
		if candidateMatched == 0 || marginal == 0 || len(unionGroups) < 2 || !leadAligned ||
			(longest < 5 && candidateMatched < 2) {
			continue
		}

		winner := exploreComponentConjunctionWinner{
			index:    index,
			seedRank: seedRank,
			score: exploreComponentConjunctionScore{
				groups: len(unionGroups), marginal: marginal, rarity: rarity, longest: longest,
			},
		}
		previous, exists := bestByComponent[component]
		comparison := exploreComponentConjunctionScoreCompare(winner.score, previous.score)
		if !exists || comparison > 0 ||
			(comparison == 0 && (winner.seedRank < previous.seedRank ||
				(winner.seedRank == previous.seedRank && winner.index < previous.index))) {
			bestByComponent[component] = winner
		}
	}
	if len(bestByComponent) == 0 {
		return candidates
	}

	best := exploreComponentConjunctionWinner{index: -1}
	ambiguous := false
	for _, winner := range bestByComponent {
		if best.index < 0 {
			best = winner
			continue
		}
		comparison := exploreComponentConjunctionScoreCompare(winner.score, best.score)
		if comparison > 0 {
			best = winner
			ambiguous = false
		} else if comparison == 0 {
			ambiguous = true
		}
	}
	if best.index < 0 || ambiguous {
		return candidates
	}

	result := append([]*rerank.Candidate(nil), candidates...)
	clone := *result[best.index]
	clone.Signals = make(map[string]float64, len(result[best.index].Signals)+1)
	for key, value := range result[best.index].Signals {
		clone.Signals[key] = value
	}
	clone.Signals[exploreConceptComplementSignal] = 1
	promoted := &clone
	if best.index < width {
		result[best.index] = promoted
		return result
	}

	target := width - 1
	copy(result[target+1:best.index+1], result[target:best.index])
	result[target] = promoted
	return result
}

// callable plus up to two callables that cover distinct task concepts in the
// final window. Retrieval width and response size remain unchanged. The
// semantic head stays first; reserved implementations occupy the next slots.
func reserveExploreConceptImplementation(
	query string,
	queryClass rerank.QueryClass,
	candidates []*rerank.Candidate,
	maxSymbols int,
) ([]*rerank.Candidate, string) {
	if queryClass != rerank.QueryClassConcept || len(candidates) == 0 || maxSymbols <= 0 {
		return candidates, ""
	}
	queryTermSet := exploreTerminalTerms(query)
	if len(queryTermSet) == 0 {
		return candidates, ""
	}

	// Candidate identifiers are compared against the query terms directly.
	// Building a token map for every row dominated this bounded selector's
	// latency and allocations even though only set membership is needed.
	var queryTermStorage [64]string
	queryTerms := queryTermStorage[:0]
	if len(queryTermSet) > len(queryTermStorage) {
		queryTerms = make([]string, 0, len(queryTermSet))
	}
	for term := range queryTermSet {
		queryTerms = append(queryTerms, term)
	}
	sort.Strings(queryTerms)
	var frequencyStorage [64]int
	var frequency []int
	if len(queryTerms) <= len(frequencyStorage) {
		frequency = frequencyStorage[:len(queryTerms)]
	} else {
		frequency = make([]int, len(queryTerms))
	}
	var metricStorage [80]exploreConceptImplementationMetric
	metrics := metricStorage[:0]
	if len(candidates) > len(metricStorage) {
		metrics = make([]exploreConceptImplementationMetric, 0, len(candidates))
	}

	for i, candidate := range candidates {
		if candidate == nil || candidate.Node == nil ||
			(candidate.Node.Kind != graph.KindFunction && candidate.Node.Kind != graph.KindMethod) {
			continue
		}
		node := candidate.Node
		metric := exploreConceptImplementationMetric{
			index: i, segments: exploreIdentifierSegmentCountBounded(node.Name),
		}
		if len(queryTerms) <= 64 {
			metric.matchedMask = exploreConceptImplementationMatches(node, queryTerms)
			for j, term := range queryTerms {
				if metric.matchedMask&(uint64(1)<<j) == 0 {
					continue
				}
				metric.overlap++
				frequency[j]++
				if len(term) > metric.longest {
					metric.longest = len(term)
				}
			}
		} else {
			// Very long issue bodies are uncommon after query shaping. Preserve
			// unbounded semantics without inflating the normal 80-row hot path.
			for j, term := range queryTerms {
				if !exploreConceptImplementationHasTerm(node, term) {
					continue
				}
				metric.overlap++
				frequency[j]++
				if len(term) > metric.longest {
					metric.longest = len(term)
				}
			}
		}
		metrics = append(metrics, metric)
	}

	best := exploreConceptImplementationMetric{index: -1}
	for _, metric := range metrics {
		if metric.overlap < 2 && (metric.overlap != 1 || metric.longest < 5 || metric.segments < 2) {
			continue
		}
		for j, count := range frequency {
			if count != 1 {
				continue
			}
			matched := len(queryTerms) <= 64 && metric.matchedMask&(uint64(1)<<j) != 0
			if len(queryTerms) > 64 {
				matched = exploreConceptImplementationHasTerm(candidates[metric.index].Node, queryTerms[j])
			}
			if matched {
				metric.rare++
			}
		}
		if best.index < 0 || metric.overlap > best.overlap ||
			(metric.overlap == best.overlap && metric.rare > best.rare) ||
			(metric.overlap == best.overlap && metric.rare == best.rare && metric.longest > best.longest) ||
			(metric.overlap == best.overlap && metric.rare == best.rare && metric.longest == best.longest && metric.segments > best.segments) {
			best = metric
		}
	}
	if best.index < 0 {
		return candidates, ""
	}

	protected := candidates[best.index]
	targetIndex := 0
	if maxSymbols > 1 && best.index > 0 {
		targetIndex = 1
	}

	// Complements are chosen iteratively. Independent behavioral groups from
	// the issue lead dominate; marginal coverage from the full canonical task
	// breaks broader ties. Low-value report mechanics can still break a final
	// tie but cannot authorize a complement by themselves.
	type complementScore struct {
		metric             exploreConceptImplementationMetric
		behavioralMarginal int
		behavioralTotal    int
		highMarginal       int
		marginalRare       int
		marginalWeight     int
		marginal           int
		familyPenalty      int
		longest            int
	}
	lowValue := exploreConceptComplementLowValueTermSet(query)
	leadTerms := exploreTerminalTerms(exploreConceptIssueLead(query))
	behavioralGroups := make([]string, len(queryTerms))
	for index, term := range queryTerms {
		if _, low := lowValue[term]; low {
			continue
		}
		if _, inLead := leadTerms[term]; inLead {
			behavioralGroups[index] = exploreConceptBehavioralTermGroup(term)
		}
	}
	complementFrequency := make([]int, len(queryTerms))
	for _, metric := range metrics {
		node := candidates[metric.index].Node
		for index, term := range queryTerms {
			if exploreConceptComplementHasTerm(node, term) {
				complementFrequency[index]++
			}
		}
	}
	coveredTerms := make([]bool, len(queryTerms))
	coveredBehavior := make(map[string]struct{}, len(leadTerms))
	markCoverage := func(node *graph.Node) {
		for index, term := range queryTerms {
			if !exploreConceptComplementHasTerm(node, term) {
				continue
			}
			coveredTerms[index] = true
			if group := behavioralGroups[index]; group != "" {
				coveredBehavior[group] = struct{}{}
			}
		}
	}
	markCoverage(candidates[0].Node)
	markCoverage(protected.Node)
	reservedNodes := []*graph.Node{candidates[0].Node}
	if protected.Node != candidates[0].Node {
		reservedNodes = append(reservedNodes, protected.Node)
	}
	available := maxSymbols - targetIndex - 1
	if available > 2 {
		available = 2
	}
	selected := make([]complementScore, 0, max(0, available))
	selectedIndexes := make(map[int]struct{}, max(0, available))
	for len(selected) < available {
		complement := complementScore{metric: exploreConceptImplementationMetric{index: -1}}
		for _, metric := range metrics {
			if metric.index == best.index {
				continue
			}
			if _, exists := selectedIndexes[metric.index]; exists {
				continue
			}
			node := candidates[metric.index].Node
			duplicateCallable := false
			for _, reserved := range reservedNodes {
				if reserved != nil && node.Name != "" && strings.EqualFold(node.Name, reserved.Name) {
					duplicateCallable = true
					break
				}
			}
			if duplicateCallable {
				continue
			}

			score := complementScore{
				metric:        metric,
				familyPenalty: exploreConceptComplementFamilyPenalty(node, reservedNodes),
			}
			candidateBehavior := make(map[string]struct{}, len(leadTerms))
			for index, term := range queryTerms {
				if !exploreConceptComplementHasTerm(node, term) {
					continue
				}
				if group := behavioralGroups[index]; group != "" {
					candidateBehavior[group] = struct{}{}
				}
				if coveredTerms[index] {
					continue
				}
				score.marginal++
				frequency := complementFrequency[index]
				if frequency < 1 {
					frequency = 1
				}
				if _, low := lowValue[term]; low {
					score.marginalWeight++
					continue
				}
				score.highMarginal++
				score.marginalWeight += 1000 / frequency
				if frequency == 1 {
					score.marginalRare++
				}
				if len(term) > score.longest {
					score.longest = len(term)
				}
			}
			score.behavioralTotal = len(candidateBehavior)
			for group := range candidateBehavior {
				if _, covered := coveredBehavior[group]; !covered {
					score.behavioralMarginal++
				}
			}
			if score.behavioralMarginal == 0 && score.highMarginal == 0 {
				continue
			}
			if score.behavioralMarginal < 2 && score.highMarginal < 2 &&
				(score.longest < 5 || metric.segments < 2) {
				continue
			}
			better := complement.metric.index < 0 ||
				score.behavioralMarginal > complement.behavioralMarginal ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal > complement.behavioralTotal) ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal == complement.behavioralTotal && score.highMarginal > complement.highMarginal) ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal == complement.behavioralTotal && score.highMarginal == complement.highMarginal && score.marginalWeight > complement.marginalWeight) ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal == complement.behavioralTotal && score.highMarginal == complement.highMarginal && score.marginalWeight == complement.marginalWeight && score.marginalRare > complement.marginalRare) ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal == complement.behavioralTotal && score.highMarginal == complement.highMarginal && score.marginalWeight == complement.marginalWeight && score.marginalRare == complement.marginalRare && score.marginal > complement.marginal) ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal == complement.behavioralTotal && score.highMarginal == complement.highMarginal && score.marginalWeight == complement.marginalWeight && score.marginalRare == complement.marginalRare && score.marginal == complement.marginal && score.familyPenalty < complement.familyPenalty) ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal == complement.behavioralTotal && score.highMarginal == complement.highMarginal && score.marginalWeight == complement.marginalWeight && score.marginalRare == complement.marginalRare && score.marginal == complement.marginal && score.familyPenalty == complement.familyPenalty && score.metric.overlap > complement.metric.overlap) ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal == complement.behavioralTotal && score.highMarginal == complement.highMarginal && score.marginalWeight == complement.marginalWeight && score.marginalRare == complement.marginalRare && score.marginal == complement.marginal && score.familyPenalty == complement.familyPenalty && score.metric.overlap == complement.metric.overlap && score.longest > complement.longest) ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal == complement.behavioralTotal && score.highMarginal == complement.highMarginal && score.marginalWeight == complement.marginalWeight && score.marginalRare == complement.marginalRare && score.marginal == complement.marginal && score.familyPenalty == complement.familyPenalty && score.metric.overlap == complement.metric.overlap && score.longest == complement.longest && score.metric.segments > complement.metric.segments) ||
				(score.behavioralMarginal == complement.behavioralMarginal && score.behavioralTotal == complement.behavioralTotal && score.highMarginal == complement.highMarginal && score.marginalWeight == complement.marginalWeight && score.marginalRare == complement.marginalRare && score.marginal == complement.marginal && score.familyPenalty == complement.familyPenalty && score.metric.overlap == complement.metric.overlap && score.longest == complement.longest && score.metric.segments == complement.metric.segments && score.metric.index < complement.metric.index)
			if better {
				complement = score
			}
		}
		if complement.metric.index < 0 {
			break
		}
		selected = append(selected, complement)
		selectedIndexes[complement.metric.index] = struct{}{}
		node := candidates[complement.metric.index].Node
		markCoverage(node)
		reservedNodes = append(reservedNodes, node)
	}
	if best.index == targetIndex && len(selected) == 0 {
		return candidates, protected.Node.ID
	}

	result := append([]*rerank.Candidate(nil), candidates...)
	candidateIndex := func(want *rerank.Candidate) int {
		for index, candidate := range result {
			if candidate == want || (candidate != nil && candidate.Node != nil && want != nil && want.Node != nil &&
				candidate.Node.ID != "" && candidate.Node.ID == want.Node.ID) {
				return index
			}
		}
		return -1
	}
	moveBefore := func(candidate *rerank.Candidate, target int) {
		current := candidateIndex(candidate)
		if current < 0 || current <= target || target < 0 || target >= len(result) {
			return
		}
		copy(result[target+1:current+1], result[target:current])
		result[target] = candidate
	}
	moveBefore(protected, targetIndex)

	for offset, complement := range selected {
		candidate := candidates[complement.metric.index]
		complementIndex := targetIndex + 1 + offset
		if complementIndex < maxSymbols && complementIndex < len(result) {
			moveBefore(candidate, complementIndex)
		}
		if index := candidateIndex(candidate); index >= 0 && index < maxSymbols {
			clone := *result[index]
			clone.Signals = make(map[string]float64, len(result[index].Signals)+1)
			for key, value := range result[index].Signals {
				clone.Signals[key] = value
			}
			clone.Signals[exploreConceptComplementSignal] = 1
			result[index] = &clone
		}
	}
	return result, protected.Node.ID
}

func exploreConceptImplementationMatches(node *graph.Node, terms []string) uint64 {
	matched := exploreIdentifierTerminalMatches(node.Name, terms)
	return exploreIdentifierTerminalMatchesWithMask(node.QualName, terms, matched)
}

func exploreConceptImplementationHasTerm(node *graph.Node, term string) bool {
	if node == nil {
		return false
	}
	return exploreIdentifierTerminalMatches(node.Name, []string{term}) != 0 ||
		exploreIdentifierTerminalMatches(node.QualName, []string{term}) != 0
}

func exploreIdentifierTerminalMatches(text string, terms []string) uint64 {
	return exploreIdentifierTerminalMatchesWithMask(text, terms, 0)
}

func exploreIdentifierTerminalMatchesWithMask(text string, terms []string, matched uint64) uint64 {
	for offset := 0; offset < len(text); {
		start, end, next, ascii := nextExploreASCIIIdentifierToken(text, offset)
		if !ascii {
			for _, token := range rerank.Tokenize(text) {
				token = exploreTerminalTermRoot(token)
				for j, term := range terms {
					if j < 64 && token == term {
						matched |= uint64(1) << j
					}
				}
			}
			return matched
		}
		if start < 0 {
			break
		}
		rootEnd := end
		if end-start > 4 && (text[end-1] == 's' || text[end-1] == 'S') &&
			text[end-2] != 's' && text[end-2] != 'S' {
			rootEnd--
		}
		token := text[start:rootEnd]
		for j, term := range terms {
			if j < 64 && strings.EqualFold(token, term) {
				matched |= uint64(1) << j
			}
		}
		offset = next
	}
	return matched
}

func exploreIdentifierSegmentCountBounded(text string) int {
	var starts, ends [16]int
	count := 0
	for offset := 0; offset < len(text); {
		start, end, next, ascii := nextExploreASCIIIdentifierToken(text, offset)
		if !ascii {
			return len(rerank.Tokenize(text))
		}
		if start < 0 {
			break
		}
		duplicate := false
		for i := 0; i < count && i < len(starts); i++ {
			if strings.EqualFold(text[start:end], text[starts[i]:ends[i]]) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			if count == len(starts) {
				return len(rerank.Tokenize(text))
			}
			starts[count], ends[count] = start, end
			count++
		}
		offset = next
	}
	return count
}

func nextExploreASCIIIdentifierToken(text string, offset int) (start, end, next int, ascii bool) {
	for offset < len(text) {
		if text[offset] >= 0x80 {
			return 0, 0, 0, false
		}
		if exploreASCIIIdentifierByte(text[offset]) {
			break
		}
		offset++
	}
	if offset >= len(text) {
		return -1, -1, len(text), true
	}
	start = offset
	for i := start + 1; i < len(text); i++ {
		current := text[i]
		if current >= 0x80 {
			return 0, 0, 0, false
		}
		if !exploreASCIIIdentifierByte(current) {
			return start, i, i + 1, true
		}
		previous := text[i-1]
		camelBoundary := exploreASCIIUpper(current) && exploreASCIILower(previous)
		acronymBoundary := exploreASCIIUpper(current) && exploreASCIIUpper(previous) &&
			i+1 < len(text) && exploreASCIILower(text[i+1])
		digitBoundary := exploreASCIIDigit(current) != exploreASCIIDigit(previous)
		if camelBoundary || acronymBoundary || digitBoundary {
			return start, i, i, true
		}
	}
	return start, len(text), len(text), true
}

func exploreASCIIIdentifierByte(value byte) bool {
	return exploreASCIILower(value) || exploreASCIIUpper(value) || exploreASCIIDigit(value)
}

func exploreASCIILower(value byte) bool { return value >= 'a' && value <= 'z' }
func exploreASCIIUpper(value byte) bool { return value >= 'A' && value <= 'Z' }
func exploreASCIIDigit(value byte) bool { return value >= '0' && value <= '9' }

func exploreSameCallableOwner(parent, child *graph.Node) bool {
	if parent == nil || child == nil || parent.Kind != graph.KindMethod || child.Kind != graph.KindMethod {
		return false
	}
	owner := func(node *graph.Node) string {
		qual := strings.ReplaceAll(strings.TrimSpace(node.QualName), "::", ".")
		qual = strings.Trim(qual, ".")
		if split := strings.LastIndexByte(qual, '.'); split >= 0 {
			return strings.ToLower(qual[:split])
		}
		return ""
	}
	parentOwner, childOwner := owner(parent), owner(child)
	return parentOwner != "" && parentOwner == childOwner
}

func exploreBodyIdentifierMentions(source, identifier string) int {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(identifier) == "" {
		return 0
	}
	wanted := exploreTerminalTerms(identifier)
	if len(wanted) == 0 {
		return 0
	}
	mentions := 0
	for _, token := range rerank.Tokenize(source) {
		if _, ok := wanted[strings.ToLower(token)]; ok {
			mentions++
		}
	}
	return mentions
}

// handleExplore is the one-shot localization verb: free text in, a ranked
// neighborhood (symbols + source + call paths + file map + completeness
// cue) out, bounded by a token budget, in a single response.
func (s *Server) handleExplore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task := strings.TrimSpace(req.GetString("task", ""))
	if task == "" {
		return mcp.NewToolResultError("task is required"), nil
	}
	// Classify artifact intent from the untouched task. Query shaping below is
	// intentionally optimized for source symbols and may discard the exact
	// paths, keys, flags, and environment names needed by config/CI searches.
	artifactIntent := classifyExploreArtifactIntent(task)
	localize := req.GetBool("localize", false)
	var sourceWindowHits *localizationSourceWindowHitCollector
	if localize {
		sourceWindowHits = &localizationSourceWindowHitCollector{}
	}
	// The same ranked spans, derived once, applied to source candidate paths.
	// Ordinary exploration keeps the zero value and so corroborates nothing.
	var pathProbes explorePathProbes
	if localize {
		pathProbes = newExplorePathProbes(task)
	}
	maxSymbols := clampInt(req.GetInt("max_symbols", exploreDefaultMaxSymbols), 1, exploreMaxMaxSymbols)
	defaultBudget := exploreDefaultBudgetTokens
	if localize {
		defaultBudget = localizationDefaultBudgetTokens
	}
	budget := clampInt(req.GetInt("token_budget", defaultBudget), exploreMinBudgetTokens, exploreMaxBudgetTokens)

	resolved, errResult := s.resolveScope(ctx, req, IntentLocate)
	if errResult != nil {
		return errResult, nil
	}
	eng := s.engineFor(ctx)
	if eng == nil {
		return mcp.NewToolResultError("no indexed repository is available; run index_repository first"), nil
	}
	opts := query.QueryOptions{
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	}
	// The lane is a strict no-op for ordinary code tasks. Active artifact
	// requests reuse existing file nodes and content FTS under fixed fan-out
	// caps, including declaration-free config files found by explicit path.
	artifactLane := s.gatherExploreArtifactLane(ctx, artifactIntent, opts)
	// The task text is pasted verbatim by the agent and is frequently a
	// whole issue report — a title, then a body of repro commands, stack
	// traces, environment tables and issue-template prompts. Fed raw to
	// retrieval, that body's command-line flags and log lines out-weigh the
	// one-line defect description and pull ranking toward the flag-definition
	// and entry-point files instead of the fix site. shapeExploreQuery
	// distils the report to its retrieval signal (lead weighted, boilerplate
	// dropped) before search; a short focused query is passed through
	// untouched. The original task is still shown in the header so the agent
	// sees what it asked.
	searchQuery := shapeExploreQuery(task)
	queryClass := exploreQueryClass(searchQuery)
	if queryClass == rerank.QueryClassConcept {
		searchQuery = stripLeadingExploreDirective(searchQuery)
	}
	rctx := s.buildRerankContext(ctx, searchQuery)
	// Over-fetch, then keep the top maxSymbols that are real localization
	// targets — params / locals / closures / imports are never a place a
	// developer edits to fix a report, and they otherwise consume ranking
	// slots and clutter the file map. Test-source symbols are demoted, not
	// dropped: production code is where a report is resolved, but a task
	// genuinely about tests still gets them when production hits run out.
	fetch := exploreCandidateFetchLimit(maxSymbols, queryClass)
	var ranked []*rerank.Candidate
	if queryClass == rerank.QueryClassConcept {
		// Gather the hybrid primary channel without truncating away vector-only
		// candidates, add one text-only concept bag, then run the session-aware
		// reranker exactly once over the union. Channel ranks survive the merge,
		// so semantic intent remains useful outside signature-rich languages.
		primaryOpts := opts
		primaryOpts.SkipInnerRerank = true
		ranked = eng.GatherSymbolCandidates(searchQuery, fetch, primaryOpts, rctx)
		terms := exploreConceptRecallTerms(searchQuery)
		if hasExploreExpansionTerms(searchQuery, terms) {
			expansionOpts := opts
			expansionOpts.SkipInnerRerank = true
			expansionOpts.SkipVectorChannel = true
			expansionOpts.SkipExactNameSplice = true
			expanded := eng.GatherSymbolCandidates(strings.Join(terms, " "), fetch, expansionOpts, rctx)
			ranked = mergeExploreCandidates(ranked, expanded, fetch)
		}
		// Gathering deliberately over-fetches each retrieval channel so scope
		// filtering cannot erase relevant results. Bound the union before the
		// graph-aware reranker: its centrality and edge hydration costs scale
		// with every candidate, not just the final response size.
		ranked = limitExploreCandidates(ranked, fetch*2)
		if content := s.gatherExploreQuotedContentCandidatesCollecting(ctx, task, ranked, fetch, opts, sourceWindowHits); len(content) > 0 {
			ranked = mergeExploreCandidates(ranked, content, 0)
			ranked = limitExploreCandidatesPreservingSourceLiteral(ranked, fetch*2)
		}
		if pipeline := eng.Rerank(); pipeline != nil {
			ranked = pipeline.Rerank(searchQuery, ranked, rctx)
		}
		ranked = rerankExploreConceptCoverage(searchQuery, ranked)
	} else {
		ranked = eng.SearchSymbolsRanked(searchQuery, fetch, opts, rctx)
		// Quoted source evidence is useful regardless of the query classifier.
		// Identifier-like issue text (for example, a short locale or protocol
		// value) previously bypassed this lane entirely even when the ordinary
		// symbol search could not see source bodies. The bounded literal scan
		// supplies at most one candidate per matching file, and the final
		// source-evidence reservation keeps its strongest result without
		// reranking the already-ranked primary channel a second time.
		if content := s.gatherExploreQuotedContentCandidatesCollecting(ctx, task, ranked, fetch, opts, sourceWindowHits); len(content) > 0 {
			ranked = mergeExploreCandidates(ranked, content, 0)
			ranked = limitExploreCandidatesPreservingSourceLiteral(ranked, fetch*2)
		}
	}
	// Distinctive CLI and identifier-shaped anchors get one bounded lexical
	// owner each. This runs after the ordinary ranking pass so it only spends
	// work on uncovered anchors and cannot perturb the semantic channel. The
	// existing source scanner is the final fallback, never a persistent body
	// index.
	var protectedSyntacticAnchors map[int]string
	if queryClass == rerank.QueryClassConcept {
		anchorCandidates, protected := s.gatherExploreSyntacticAnchorCandidatesCollecting(
			ctx, task, ranked, eng, opts, rctx, sourceWindowHits,
		)
		protectedSyntacticAnchors = protected
		if len(anchorCandidates) > 0 {
			ranked = mergeExploreCandidates(ranked, anchorCandidates, fetch)
		}
	}
	// Only an unanchored concept task may infer bare source literals. Quoted
	// literals, artifact evidence, explicit paths/names, and protected syntax
	// already have stronger bounded lanes and make this path a strict no-op.
	if exploreBareLiteralLaneEligible(
		task, searchQuery, queryClass == rerank.QueryClassConcept, artifactLane.ready,
		ranked, protectedSyntacticAnchors,
	) {
		terms := exploreBareLiteralRecallTerms(task)
		if content := s.gatherExploreContentCandidatesForTermsCollecting(ctx, task, terms, ranked, fetch, opts, sourceWindowHits); len(content) > 0 {
			ranked = mergeExploreCandidates(ranked, content, 0)
			ranked = limitExploreCandidatesPreservingSourceLiteral(ranked, fetch*2)
			ranked = rerankExploreConceptCoverage(searchQuery, ranked)
		}
	}
	// Exact source citations in issue bodies are stronger than semantic ranking:
	// map each bounded file/line range to its smallest enclosing declaration and
	// place those task-spelled candidates at the head before final selection.
	ranked = s.promoteExploreSourceRangeCandidates(ctx, task, ranked, eng.Reader(), opts)
	// Resilience ladder: a warm-restarted daemon can transiently return an
	// empty scoped ranked result (workspace stamps not yet backfilled, or
	// search bundles served before their node payloads re-materialise)
	// while the index itself is fine. A one-shot verb must not answer
	// "nothing matched" for an index that is merely re-warming, so relax in
	// two steps — unscoped ranked, then unscoped BM25 — re-applying the
	// repo boundary as a post-filter to preserve multi-repo hygiene.
	repoAllowed := func(n *graph.Node) bool {
		return len(resolved.RepoAllow) == 0 || repoNarrowAdmits(resolved.RepoAllow, n.RepoPrefix)
	}
	if len(ranked) == 0 {
		for _, c := range eng.SearchSymbolsRanked(searchQuery, fetch, query.QueryOptions{}, rctx) {
			if c != nil && c.Node != nil && repoAllowed(c.Node) {
				ranked = append(ranked, c)
			}
		}
	}
	if len(ranked) == 0 {
		// Last rung: the per-term OR-merge the ranked search handler itself
		// falls back on — whole-sentence MATCH semantics differ between the
		// in-memory and disk-resident search backends, and per-term fetch +
		// merge works on both.
		nodes, _ := fetchAndMergeBM25Timed(eng, searchQuery, exploreLexicalTerms(searchQuery), fetch, opts, nil)
		if len(nodes) == 0 && (opts.WorkspaceID != "" || opts.ProjectID != "" || len(opts.RepoAllow) > 0) {
			nodes, _ = fetchAndMergeBM25Timed(eng, searchQuery, exploreLexicalTerms(searchQuery), fetch, query.QueryOptions{}, nil)
		}
		for i, n := range nodes {
			if n != nil && repoAllowed(n) {
				ranked = append(ranked, &rerank.Candidate{Node: n, TextRank: i, VectorRank: -1})
			}
		}
	}
	var prod, test []*rerank.Candidate
	for _, c := range ranked {
		if c == nil || c.Node == nil || !exploreLocalizableKind(c.Node.Kind) {
			continue
		}
		if exploreLocalizationTestLaneCandidate(searchQuery, c) || !exploreCodeDefinitionKind(c.Node.Kind) {
			test = append(test, c)
		} else {
			prod = append(prod, c)
		}
	}
	// One bounded string pass over the ranked prefix, so the selection cut and
	// the leading-file election both read a score computed once.
	stampExplorePathProbeScores(prod, pathProbes)
	// Natural-language localization can retrieve large exact-name collision
	// sets (`client` fields, `Validate` methods, generated accessors). BM25 is
	// right that each item matches, but a useful neighborhood needs distinct
	// concepts. Keep one data declaration and at most two callable/type members
	// per repeated name, moving the remaining collisions behind distinct
	// definitions. Identifier/path lookups retain literal ordering.
	prod = diversifyRepeatedExploreNames(prod, queryClass)
	// Bounded per-file diversification (the same demote-only mechanism the
	// ranked search head uses): a localization neighborhood that spans
	// files beats one file's cluster of sibling shims crowding out every
	// other candidate. Nothing is dropped — capped files' extra hits move
	// below not-yet-capped files.
	prodNodes := make([]*graph.Node, len(prod))
	for i, c := range prod {
		prodNodes[i] = c.Node
	}
	// Retain the pre-diversification order for the localization page: the
	// leading file's demoted siblings are only recoverable from the pool as
	// ranking left it.
	var localizationRankedPool []*rerank.Candidate
	if exploreShouldDiversifyByFile(queryClass) {
		if localize {
			localizationRankedPool = append([]*rerank.Candidate(nil), prod...)
		}
		_, prod = diversifyByFile(prodNodes, prod, defaultMaxPerFile)
	}
	prod, protectedImplementationID := reserveExploreConceptImplementation(searchQuery, queryClass, prod, maxSymbols)
	prod = reserveExploreComponentConjunction(searchQuery, queryClass, prod, maxSymbols)
	// Path corroboration decides only what the cut was about to drop: one better
	// corroborated row below it replaces the weakest row no lane reserved.
	prod = liftExplorePathProbeCandidates(
		prod, maxSymbols,
		exploreFinalReservedCandidateIDs(prod, protectedSyntacticAnchors, protectedImplementationID),
	)
	if localize {
		// Last chance to keep an editable declaration from the file: once the cut
		// drops a candidate, no later reordering can bring it back. Trades happen
		// inside one file's own competition, so the per-file and cross-file
		// allocation the lanes above settled is carried through unchanged.
		prod = liftLocalizationSiblingCallables(task, prod, maxSymbols, protectedSyntacticAnchors)
	}
	cands := selectFinalExploreCandidates(prod, test, maxSymbols)
	if len(protectedSyntacticAnchors) > 0 {
		// Source-literal selection owns its own final-slot guarantees. Re-union
		// that selected window with production candidates, then enforce anchor
		// reservations against the actual final pool while retaining the selected
		// order for every remaining slot.
		anchorPool := mergeExploreCandidates(cands, prod, 0)
		anchorPool = reserveExploreSyntacticAnchorCandidates(task, anchorPool, protectedSyntacticAnchors, maxSymbols)
		if len(anchorPool) > maxSymbols {
			anchorPool = anchorPool[:maxSymbols]
		}
		cands = anchorPool
	}
	if queryClass == rerank.QueryClassConcept && len(protectedSyntacticAnchors) > 0 {
		// A protected anchor can leave a matching typed field in the final window
		// without its behavioral owner. Admit one graph-proven field-owner
		// consumer and its anchor-aligned typed member before source hydration.
		// The projection uses a fixed batch pipeline with deterministic fanout
		// bounds and is a no-op unless every structural link is resolved.
		cands = projectExploreTypedAnchorCandidates(
			ctx, task, cands, eng.Reader(), opts, maxSymbols,
			protectedSyntacticAnchors, protectedImplementationID,
		)
	}
	if localize {
		// Presentation ordering for the localization page only, applied after the
		// final window is fixed so membership cannot change: each file's own slots
		// are permuted, cross-file rank is untouched, and every member remains
		// searchable and readable through every other lane.
		cands = demoteLocalizationFileMembers(cands)
	}
	protectedFinalCandidateIDs := exploreFinalReservedCandidateIDs(
		cands, protectedSyntacticAnchors, protectedImplementationID,
	)
	if len(cands) == 0 && len(artifactLane.targets) == 0 {
		if localize {
			return s.completeEmptyLocalization(ctx, task, budget), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf(
			"EXPLORE — %s\n\nNo ranked symbols matched this request. Widen the wording, use search(operation:\"text\", query:\"<literal>\") for a literal lead, or search(operation:\"files\", query:\"<filename>\") for a path lead.",
			truncateOneLine(task, 200))), nil
	}

	ringOpts := query.QueryOptions{
		Depth: 1, Limit: exploreRingCap * 3, Detail: "brief",
		WorkspaceID: resolved.WorkspaceID, ProjectID: resolved.ProjectID, RepoAllow: resolved.RepoAllow,
	}
	explicitTarget := exploreHasExplicitCandidateTarget(searchQuery, cands)
	artifactTargets := artifactLane.targets
	literalPrimaryEligible := exploreLiteralEvidenceEligible(searchQuery, cands, protectedSyntacticAnchors)
	quotedLiteralTask := len(exploreQuotedRecallTerms(task)) > 0 ||
		len(exploreDistinctiveBareTokens(task, "")) > 0
	targets := make([]exploreTarget, 0, len(artifactTargets)+len(cands))
	// Artifact evidence leads only when the strong classifier activated its
	// lane. Ordinary source localization retains the exact prior target order.
	targets = append(targets, artifactTargets...)
	for _, c := range cands {
		if c == nil || c.Node == nil {
			continue
		}
		n := c.Node
		t := exploreTargetFromCandidate(c, protectedImplementationID, literalPrimaryEligible, quotedLiteralTask)
		t.source = s.manifestSymbolSource(ctx, n)
		if callers := eng.GetCallers(n.ID, ringOpts); callers != nil {
			t.callers = ringNeighbors(callers.Nodes, n.ID, exploreRingCap)
		}

		callees := eng.GetCallChain(n.ID, ringOpts)
		if callees != nil {
			t.directCalleesComplete = !callees.Truncated && !callees.BudgetHit && !callees.LowerBound
			var projectionComplete bool
			t.callees, projectionComplete = ringNeighborsProjection(callees.Nodes, n.ID, exploreRingCap)
			t.directCalleesComplete = t.directCalleesComplete && projectionComplete
		}
		targets = append(targets, t)
	}

	// Every final symbol now has equally-bounded depth-1 evidence. Spend the
	// deeper budget in a second pass so a strong route ranked fourth cannot be
	// starved by three earlier leaf/accessor collisions.
	symbolTargets := targets[len(artifactTargets):]
	var causalElapsed time.Duration
	for _, index := range selectExploreCausalSeeds(
		searchQuery, symbolTargets, queryClass == rerank.QueryClassConcept, explicitTarget,
	) {
		if causalElapsed >= exploreCausalAdmissionBudget {
			break
		}
		target := &symbolTargets[index]
		calleeOpts := ringOpts
		calleeOpts.Depth = exploreCausalDepth
		started := time.Now()
		callees := eng.GetCallChain(target.node.ID, calleeOpts)
		causalElapsed += time.Since(started)
		if callees == nil {
			continue
		}
		target.directCalleesComplete = !callees.Truncated && !callees.BudgetHit && !callees.LowerBound
		neighbors := minimumExploreCausalHops(target.node.ID, callees, opts, exploreCausalDepth, ringOpts.Limit)
		direct := make([]*graph.Node, 0, exploreRingCap)
		target.causalCallees = target.causalCallees[:0]
		for _, neighbor := range neighbors {
			if neighbor.hop == 1 {
				direct = append(direct, neighbor.node)
			} else {
				target.causalCallees = append(target.causalCallees, neighbor)
			}
		}
		if len(neighbors) > 0 {
			var projectionComplete bool
			target.callees, projectionComplete = ringNeighborsProjection(direct, target.node.ID, exploreRingCap)
			target.directCalleesComplete = target.directCalleesComplete && projectionComplete
		}
	}
	if exploreQueryIsConceptTask(task) && len(targets) > len(artifactTargets) {
		symbolTargets := promoteExploreDivergentDefaultOwner(
			ctx, task, targets[len(artifactTargets):], eng.Reader(), s.localizationNodeScope(ctx, opts), maxSymbols,
			func(node *graph.Node) string {
				return s.manifestSymbolSource(ctx, node)
			}, s.divergentDefaultFallbackSLOOverride,
		)
		targets = append(targets[:len(artifactTargets):len(artifactTargets)], symbolTargets...)
	}
	// Causal-change promotion is the second concept lane over the same ranked
	// head. A divergent-default proof is the stronger, pinned claim on that head
	// and already carries its own provenance pair, so only reach for a causal
	// route when that promotion did not take the lead.
	if len(targets) > len(artifactTargets) {
		symbolTargets := targets[len(artifactTargets):]
		if exploreDivergentDefaultOwnerSymbol(symbolTargets) == "" {
			symbolTargets = promoteExploreCausalChangeTargets(
				ctx, task, symbolTargets, eng.Reader(), s.localizationNodeScope(ctx, opts), maxSymbols,
				func(node *graph.Node) string {
					return s.manifestSymbolSource(ctx, node)
				}, func(node *graph.Node) ([]*graph.Node, bool) {
					callees := eng.GetCallChain(node.ID, ringOpts)
					if callees == nil {
						return nil, false
					}
					direct, projectionComplete := ringNeighborsProjection(callees.Nodes, node.ID, exploreRingCap)
					complete := !callees.Truncated && !callees.BudgetHit && !callees.LowerBound && projectionComplete
					return direct, complete
				},
			)
			targets = append(targets[:len(artifactTargets):len(artifactTargets)], symbolTargets...)
		}
	}
	// Direct retrieval owns the ranked head. Once graph promotion has selected
	// a cross-file boundary, materialize exactly that one node so both text and
	// structured responses can reserve a source body without a broad read.
	targets = s.materializeExploreStructuralSource(ctx, task, targets, opts)

	declarationScope := s.localizationNodeScopeWithTests(ctx, opts, false)
	if !localize {
		outlineProvider := newExploreTaskPageOutlineProvider(
			ctx, eng.Reader(), task, declarationScope,
		)
		return mcp.NewToolResultText(s.renderExploreTask(task, targets, budget, outlineProvider)), nil
	}
	declarations := newLocalizationFileDeclarationCache(ctx, eng.Reader(), declarationScope)
	symbolTargets = targets[len(artifactTargets):]
	// An implementation-intent query expands abstract seeds into their
	// concrete implementors before terminality is judged, so the envelope
	// carries the code that changes and answer_ready can see it.
	if exploreImplementationIntent(task) {
		symbolTargets = s.expandImplementationTargets(ctx, symbolTargets, declarationScope)
		targets = append(targets[:len(artifactTargets):len(artifactTargets)], symbolTargets...)
	} else if queryClass == rerank.QueryClassConcept {
		// Concept answers prefer the owning type when several of its members
		// rank together; implementation-intent queries are exempt because
		// they ask for exactly those members.
		symbolTargets = preserveExploreDivergentDefaultOrder(s.foldMemberOwners(ctx, symbolTargets, declarationScope))
		// Owner folding is weaker than a unique source-literal callsite whose
		// callee was resolved and hydrated. Re-promote that proof after folding
		// so terminality is judged against the same strongest evidence that the
		// final envelope exposes, rather than against a synthetic owner row.
		symbolTargets = promoteExploreStrongSourceLiteralTarget(symbolTargets)
		// Owner folding may insert a type above two retained members. Preserve the
		// fold without violating the request's max_symbols contract: evict only an
		// unreserved tail target, or the inserted owner itself when every direct
		// candidate is protected by an earlier evidence lane.
		symbolTargets = limitExploreFoldedTargets(task, symbolTargets, maxSymbols, protectedFinalCandidateIDs)
		targets = append(targets[:len(artifactTargets):len(artifactTargets)], symbolTargets...)
	}
	answerReady := exploreAnswerReady(task, symbolTargets) || artifactLane.ready
	if answerReady && !artifactLane.ready {
		eng := s.engineFor(ctx)
		if eng != nil && exploreImplementationAnswerBlocked(ctx, task, symbolTargets, eng.Reader(), declarationScope) {
			// Only abstract declarations in evidence for an implementation
			// question: stay nonterminal so the permitted refinement read
			// can reach the concrete side.
			answerReady = false
		}
	}
	// Once ranking has settled on one file, re-spend the page's expendable
	// breadth tail on that file's demoted siblings. This is an evidence-page
	// allocation only: terminality and the contract selections below continue
	// to read the diversified set.
	pageTargets := reserveExploreLeadingFileDepthTargets(
		localizationRankedPool, symbolTargets, maxSymbols, protectedFinalCandidateIDs,
		func(node *graph.Node) exploreTarget {
			return s.hydrateExploreLeadingFileDepthTarget(ctx, eng, node, ringOpts)
		},
	)
	if index, hit, ok := sourceWindowHits.elect(pageTargets); ok {
		if pageTargets[index].node != nil {
			pageTargets[index].sourceWindow = s.localizationSourceWindowForHit(ctx, hit, pageTargets[index].node.ID)
		}
	}
	targets = append(targets[:len(artifactTargets):len(artifactTargets)], pageTargets...)
	// The same leading file, indexed rather than ranked. The enumeration is
	// deferred: only a page that stays non-terminal pays for it, and it pays
	// once however many envelopes this request packs. The task's own terms ride
	// along so a bounded index keeps the declarations the task named.
	pageOutline := boundedLocalizationPageOutlineProvider(
		localizationRankedPool, pageTargets, exploreTerminalTerms(task),
		declarations.outlineDefinitions,
	)
	// File evidence can make localization answer-ready, but it never becomes a
	// synthetic exact-symbol read. Exact reads remain declaration-only.
	exactSymbol := exploreLocalizationExplicitTarget(task, symbolTargets)
	// A unique graph + default-flow proof identifies the upstream cause behind
	// an issue author's downstream symbol anchor. Read that proven constructor,
	// while retaining the explicitly named consumer immediately after it in the
	// evidence projection.
	causalExactSymbol := exploreDivergentDefaultOwnerSymbol(symbolTargets)
	if causalExactSymbol != "" {
		exactSymbol = causalExactSymbol
	}
	// An identifier embedded in a natural-language concept task is evidence, not
	// an exclusivity instruction. Keep compact path/symbol/signature lookups as
	// exact reads, but let concept tasks choose among the full bounded candidate
	// window with the explicit declaration as their preferred seed.
	conceptAnchor := exploreLocalizationUsesConceptRefinement(answerReady, task, exactSymbol, causalExactSymbol)
	if !answerReady && (exactSymbol == "" || conceptAnchor) {
		// Uncertain localization is still useful evidence. Returning it as an
		// MCP error makes hosts discard the ranked candidates and restart broad
		// exploration, multiplying turns and payloads. Name one concrete ranked
		// target for the only permitted refinement read; source-literal evidence
		// wins because it is absent from ordinary symbol metadata.
		routes := exploreLocalizationRefinementRoutes(symbolTargets)
		preferred := explorePreferredRefinementSymbol(task, symbolTargets)
		if conceptAnchor {
			if _, authorized := routes[exactSymbol]; authorized {
				preferred = exactSymbol
			}
		}
		preferredSymbol := explorePreferredRoutedRefinementSymbol(
			preferred, symbolTargets, routes,
		)
		result, refinement, boundedRoutes, digest := buildLocalizationRefinementResultForTaskWithOutlineAndDeclarations(
			preferredSymbol, task, targets, budget, routes, pageOutline, declarations,
		)
		if refinement.State != localizationStateNeedsRefinement {
			refinement.digest = digest
			s.localizationFor(ctx).armForTask(refinement, task)
			return result, nil
		}
		// Wire and server authorization are derived from the same finalized
		// post-budget set. Known generic wrappers without a visible, unique
		// concrete hop remain evidence but are never authorized.
		s.localizationFor(ctx).armRefinementRoutesForTask(
			task, refinement.refinementSymbol, refinement.AllowedSymbols, boundedRoutes, digest,
		)
		return result, nil
	}
	completion := exploreLocalizationCompletion(
		answerReady, artifactLane.ready, task, symbolTargets, exactSymbol,
	)
	// The digest derives from the same serialized projection the host sees,
	// and is retained for post-terminal replay — for the exact-read contract
	// too, whose success promotes to answer_ready with the evidence already
	// stashed.
	result, _, digest, completion := buildLocalizationExploreResultForTaskFinalizedWithOutlineAndDeclarations(
		completion, task, targets, budget, pageOutline, declarations,
	)
	// Literal-driven terminality must show its evidence: when the verdict
	// rests on a quoted-literal match but the budgeted envelope shed the
	// literal, downgrade to the bounded refinement read instead of telling
	// the host to answer from evidence it cannot see.
	if answerReady && exactSymbol == "" &&
		exploreAnswerReadyViaLiteralOnly(task, symbolTargets) && !completion.Enforceable &&
		!exploreResultCitesTaskLiteral(result, task) {
		routes := exploreLocalizationRefinementRoutes(symbolTargets)
		preferredSymbol := explorePreferredRoutedRefinementSymbol(
			explorePreferredRefinementSymbol(task, symbolTargets), symbolTargets, routes,
		)
		refined, refinement, boundedRoutes, refinedDigest := buildLocalizationRefinementResultForTaskWithOutlineAndDeclarations(
			preferredSymbol, task, targets, budget, routes, pageOutline, declarations,
		)
		if refinement.State != localizationStateNeedsRefinement {
			refinement.digest = refinedDigest
			s.localizationFor(ctx).armForTask(refinement, task)
			return refined, nil
		}
		s.localizationFor(ctx).armRefinementRoutesForTask(
			task, refinement.refinementSymbol, refinement.AllowedSymbols, boundedRoutes, refinedDigest,
		)
		return refined, nil
	}
	if completion.State == localizationStateLocalized {
		// `localized` is a wire-only confidence cue. Persisting that state would
		// make the session authorizer treat it as a live contract, so commit the
		// inactive state and leave every later navigation and coding tool open.
		s.localizationFor(ctx).keepOpenForTask(task)
		return result, nil
	}
	completion.digest = digest
	s.localizationFor(ctx).armForTask(completion, task)
	return result, nil
}

// localizationExploreEnvelope is the compact, machine-readable result for an
// explicit localization-only request. Ordinary explore(task) retains the
// human-oriented legacy rendering; localize does not duplicate it.
type localizationExploreEnvelope struct {
	Completion localizationCompletion `json:"completion"`
	Terminal   bool                   `json:"terminal"`
	Files      []string               `json:"files"`
	Symbols    []string               `json:"symbols"`
	Evidence   []localizationEvidence `json:"evidence"`
	// Outline indexes the page's leading file; Outlines indexes the page's
	// further files, deepest first. The split keeps the leading file where
	// consumers already read it.
	Outline      *localizationFileOutline   `json:"outline,omitempty"`
	Outlines     []*localizationFileOutline `json:"outlines,omitempty"`
	SourceWindow *localizationSourceWindow  `json:"source_window,omitempty"`
}

type localizationEvidence struct {
	Rank       int      `json:"rank"`
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	QualName   string   `json:"qual_name,omitempty"`
	Kind       string   `json:"kind"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	EndLine    int      `json:"end_line,omitempty"`
	Signature  string   `json:"signature,omitempty"`
	Callers    []string `json:"callers,omitempty"`
	Callees    []string `json:"callees,omitempty"`
	Provenance string   `json:"provenance,omitempty"`
	Source     string   `json:"source,omitempty"`

	// Request-local presentation authority. These fields are deliberately not
	// serialized: provenance remains truthful while supplemental rows and the
	// frozen initial PRIMARY cohort keep their distinct presentation roles.
	literalPrimaryEligible   bool
	taskCitedPrimaryEligible bool
	primaryCohortOrder       int
	supportingOnly           bool
	// leadingFileDepth separates the two supportingOnly writers: a row the
	// initial page admitted for leading-file completeness may still earn a
	// task-named seat, while a post-terminal supplemental row never does.
	leadingFileDepth bool
}

func (s *Server) completeEmptyLocalization(ctx context.Context, task string, budget int) *mcp.CallToolResult {
	// No bounded source read can improve an empty projection. Return a compact,
	// advisory terminal contract so the host gets a final response instead of
	// an unbounded navigation loop with an eventually empty final.
	completion := newLocalizationCompletion(true, "")
	result, _, digest, completion := buildLocalizationExploreResultForTaskFinalized(
		completion, task, nil, budget,
	)
	completion.digest = digest
	s.localizationFor(ctx).armForTask(completion, task)
	return result
}

// exploreAllowsStructuralBody keeps graph-expanded source reads exclusive to
// broad concept localization. Explicit directory-qualified paths are guarded
// independently because prose around a path can still classify as Concept;
// literal symbol and signature queries retain their direct-only behavior.
func exploreAllowsStructuralBody(task string) bool {
	shaped := shapeExploreQuery(task)
	class := exploreQueryClass(shaped)
	_, directoryQualified := exploreQueryPathAnchors(shaped)
	return !directoryQualified && class != rerank.QueryClassSymbol && class != rerank.QueryClassSignature
}

// explorePreferredFullBodyIDs reserves the first source slot for the strongest
// direct answer and, when available, the second for the promoted cross-file
// structural boundary. Remaining direct targets fill unused slots.
func explorePreferredFullBodyIDs(task string, targets []exploreTarget, draft []exploreDraftEntry, limit int) []string {
	if limit <= 0 {
		return nil
	}
	sources := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.node != nil && target.source != "" {
			sources[exploreDraftNodeKey(target.node)] = struct{}{}
		}
	}
	result := make([]string, 0, limit)
	seen := make(map[string]struct{}, limit)
	appendEntry := func(entry exploreDraftEntry) bool {
		if entry.node == nil || len(result) >= limit {
			return false
		}
		key := exploreDraftNodeKey(entry.node)
		if _, hasSource := sources[key]; !hasSource {
			return false
		}
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
		result = append(result, key)
		return true
	}
	if draft == nil {
		draft = exploreAnswerDraft(task, targets)
	}
	if !exploreAllowsStructuralBody(task) {
		for _, entry := range draft {
			if entry.direct && entry.exact {
				appendEntry(entry)
			}
		}
		return result
	}
	for _, entry := range draft {
		if entry.direct && appendEntry(entry) {
			break
		}
	}
	for _, entry := range draft {
		if entry.structural && entry.structuralCrossFile && appendEntry(entry) {
			break
		}
	}
	for _, entry := range draft {
		if entry.direct {
			appendEntry(entry)
		}
	}
	return result
}

// materializeExploreStructuralSource loads source for at most one promoted
// cross-file structural boundary. Graph expansion discovers the node without a
// broad source read; this performs the same bounded symbol-range read used for
// direct targets only after the draft has selected that boundary.
func (s *Server) materializeExploreStructuralSource(ctx context.Context, task string, targets []exploreTarget, scope query.QueryOptions) []exploreTarget {
	return materializeExploreStructuralSourceWithReader(ctx, task, targets, scope, s.manifestSymbolSource)
}

func materializeExploreStructuralSourceWithReader(
	ctx context.Context,
	task string,
	targets []exploreTarget,
	scope query.QueryOptions,
	readSource func(context.Context, *graph.Node) string,
) []exploreTarget {
	if len(targets) == 0 || !exploreAllowsStructuralBody(task) || ctx.Err() != nil || readSource == nil {
		return targets
	}
	// Promotion already spent this request's single structural read proving the
	// child forwards its divergent default. Reuse that source and never open a
	// second boundary merely for presentation.
	for _, target := range targets {
		if target.divergentDefaultOwner {
			return targets
		}
	}
	draft := exploreAnswerDraft(task, targets)
	present := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.node != nil {
			present[exploreDraftNodeKey(target.node)] = struct{}{}
		}
	}
	for _, entry := range draft {
		if entry.node == nil || !entry.structural || !entry.structuralCrossFile {
			continue
		}
		key := exploreDraftNodeKey(entry.node)
		if _, exists := present[key]; exists {
			return targets
		}
		if !exploreNodeWithinQueryScope(entry.node, scope) {
			return targets
		}
		// Cancellation may arrive while graph promotion is ranking candidates;
		// check at the actual source-read boundary so an abandoned request never
		// opens a file merely to populate an output that cannot be delivered.
		if ctx.Err() != nil {
			return targets
		}
		source := readSource(ctx, entry.node)
		if strings.TrimSpace(source) == "" {
			return targets
		}
		materialized := make([]exploreTarget, 0, len(targets)+1)
		materialized = append(materialized, targets...)
		return append(materialized, exploreTarget{node: entry.node, source: source})
	}
	return targets
}

func exploreNodeWithinQueryScope(n *graph.Node, scope query.QueryOptions) bool {
	if n == nil {
		return false
	}
	if scope.WorkspaceID != "" && n.WorkspaceID != scope.WorkspaceID {
		return false
	}
	if scope.ProjectID != "" && n.ProjectID != scope.ProjectID {
		return false
	}
	if !repoNarrowAdmits(scope.RepoAllow, n.RepoPrefix) {
		return false
	}
	return true
}

// explorePreferredRefinementSymbol turns an uncertain ranked neighborhood into
// one concrete, bounded follow-up. Exact source-literal evidence wins because
// ordinary symbol metadata cannot represent it; otherwise the ranked head is
// the deterministic refinement target.
// explorePreferredRefinementSymbol picks the single permitted refinement
// read with the same preference ladder the answer draft uses, instead of the
// raw rank-one symbol. A generic head (a builder type that shares no term
// with the query) must not consume the only read while a query-aligned
// candidate sits lower in the same envelope:
//
//  1. explicit path / qualified-symbol / call anchor
//  2. strongest non-literal semantic coverage across declaration and body
//  3. smallest actionable callable, then verified literal provenance
//  4. protected concept implementation and non-generic declaration quality
//  5. answer-draft rank and raw rank as deterministic final tie-breaks
func explorePreferredRefinementSymbol(task string, targets []exploreTarget) string {
	if len(targets) == 0 {
		return ""
	}
	query := shapeExploreQuery(task)
	syntacticAnchors := exploreSyntacticAnchors(task)
	if exploreQueryIsConceptTask(query) {
		query = stripLeadingExploreDirective(query)
	}
	semanticTerms := exploreTerminalTerms(query)
	for _, literal := range exploreQuotedRecallTerms(task) {
		for term := range exploreTerminalTerms(literal) {
			delete(semanticTerms, term)
		}
	}

	// The answer-draft order already incorporates rare-term density and generic
	// declaration demotion. Retain it as the final deterministic tie-break while
	// comparing all direct targets on their non-literal semantic evidence.
	draftRank := make(map[string]int, len(targets))
	for index, entry := range exploreAnswerDraft(task, targets) {
		if entry.node != nil {
			draftRank[exploreDraftNodeKey(entry.node)] = index
		}
	}
	type refinementCandidate struct {
		target            exploreTarget
		index             int
		draftRank         int
		identifierOverlap int
		bodyOverlap       int
		longest           int
		anchorMatches     int
		explicit          bool
		callable          bool
		generic           bool
	}
	candidates := make([]refinementCandidate, 0, len(targets))
	for index, target := range targets {
		if target.node == nil || target.node.ID == "" {
			continue
		}
		identifierOverlap, longest := exploreDraftTermOverlap(semanticTerms, target.node)
		bodyOverlap := exploreDraftTermSetOverlap(semanticTerms, exploreTerminalTerms(target.source))
		rank, ranked := draftRank[exploreDraftNodeKey(target.node)]
		if !ranked {
			rank = len(targets) + index
		}
		callable := target.node.Kind == graph.KindFunction || target.node.Kind == graph.KindMethod
		candidates = append(candidates, refinementCandidate{
			target:            target,
			index:             index,
			draftRank:         rank,
			identifierOverlap: identifierOverlap,
			bodyOverlap:       bodyOverlap,
			longest:           longest,
			anchorMatches:     exploreSyntacticAnchorTargetMatchesAnchors(syntacticAnchors, target),
			explicit:          exploreLocalizationExplicitAnchor(query, target.node),
			callable:          callable,
			generic:           exploreDraftGenericCandidate(target.node, target.source),
		})
	}
	if len(candidates) == 0 {
		return ""
	}
	alignment := func(candidate refinementCandidate) int {
		body := candidate.bodyOverlap
		if body > 2 {
			body = 2
		}
		return candidate.identifierOverlap + body
	}
	better := func(left, right refinementCandidate) bool {
		// A divergent-default owner is promoted only after a unique constructor
		// call + inheritance proof over complete bounded projections. That causal
		// proof supersedes an issue author's downstream symbol guess.
		if left.target.divergentDefaultOwner != right.target.divergentDefaultOwner {
			return left.target.divergentDefaultOwner
		}
		if left.explicit != right.explicit {
			return left.explicit
		}
		if left.anchorMatches != right.anchorMatches {
			return left.anchorMatches > right.anchorMatches
		}
		if left.anchorMatches > 0 && left.generic != right.generic {
			return !left.generic
		}
		leftAlignment, rightAlignment := alignment(left), alignment(right)
		if leftAlignment != rightAlignment {
			return leftAlignment > rightAlignment
		}
		if left.identifierOverlap != right.identifierOverlap {
			return left.identifierOverlap > right.identifierOverlap
		}
		if left.bodyOverlap != right.bodyOverlap {
			return left.bodyOverlap > right.bodyOverlap
		}
		// With no semantic alignment, verified source provenance is the only
		// concrete evidence. Do not let an arbitrary callable beat the exact
		// literal merely because the literal maps to a type or field owner.
		if leftAlignment == 0 && left.target.sourceLiteral != right.target.sourceLiteral {
			return left.target.sourceLiteral
		}
		// Once both candidates cover task concepts, prefer the smallest
		// actionable declaration before provenance: a literal mapped to an
		// enclosing type/file must not outrank an aligned callable.
		if left.callable != right.callable {
			return left.callable
		}
		if left.target.sourceLiteral != right.target.sourceLiteral {
			return left.target.sourceLiteral
		}
		if left.target.conceptImplementation != right.target.conceptImplementation {
			return left.target.conceptImplementation
		}
		if left.generic != right.generic {
			return !left.generic
		}
		if left.longest != right.longest {
			return left.longest > right.longest
		}
		if left.draftRank != right.draftRank {
			return left.draftRank < right.draftRank
		}
		return left.index < right.index
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if better(candidate, best) {
			best = candidate
		}
	}
	return best.target.node.ID
}

// localizationEvidenceTargets projects the same bounded answer-draft evidence
// used by ordinary rendering into the structured terminal envelope. Promoted
// callers/callees may replace low-ranked direct candidates, but never increase
// the response cardinality. Direct targets retain their source and neighbors.
func localizationEvidenceTargets(task, exactID string, targets []exploreTarget) []exploreTarget {
	return localizationEvidenceTargetsFromDraft(task, exactID, targets, nil)
}

func prioritizeLocalizationEvidenceTarget(requiredID string, targets []exploreTarget) []exploreTarget {
	if requiredID == "" || len(targets) < 2 {
		return targets
	}
	requiredIndex := -1
	for index, target := range targets {
		if target.node != nil && target.node.ID == requiredID {
			requiredIndex = index
			break
		}
	}
	if requiredIndex <= 0 {
		return targets
	}
	ordered := make([]exploreTarget, 0, len(targets))
	ordered = append(ordered, targets[requiredIndex])
	ordered = append(ordered, targets[:requiredIndex]...)
	ordered = append(ordered, targets[requiredIndex+1:]...)
	return ordered
}

// localizationSecondaryTaskCitedTarget reserves one independently cited
// identity already present in the bounded target window. Citation offsets used
// by the explicit target or exact source-range owners are consumed: one authored
// mention cannot seat several overloads that share the same bare name. The
// caller only reorders presentation evidence; no graph read, provenance, or
// completion authority is added.
func localizationSecondaryTaskCitedTarget(
	task string,
	targets, selected []exploreTarget,
) (exploreTarget, bool) {
	if strings.TrimSpace(task) == "" || len(targets) == 0 {
		return exploreTarget{}, false
	}
	selectedIDs := make(map[string]struct{}, len(selected))
	selectedNames := make(map[string]struct{}, len(selected))
	consumedOffsets := make(map[int]struct{}, len(selected))
	for _, target := range selected {
		if target.node == nil {
			continue
		}
		selectedIDs[exploreDraftNodeKey(target.node)] = struct{}{}
		if name := strings.TrimSpace(target.node.Name); name != "" {
			selectedNames[name] = struct{}{}
		}
		if offset := localizationDirectAdjacencyNodeTaskCitationOffset(task, target.node); offset >= 0 {
			consumedOffsets[offset] = struct{}{}
		}
	}
	bestIndex, bestOffset := -1, -1
	for index, target := range targets {
		if target.node == nil {
			continue
		}
		if _, selected := selectedIDs[exploreDraftNodeKey(target.node)]; selected {
			continue
		}
		offset := localizationDirectAdjacencyNodeTaskCitationOffset(task, target.node)
		if offset < 0 {
			continue
		}
		name := strings.TrimSpace(target.node.Name)
		if _, represented := selectedNames[name]; represented &&
			localizationTaskExactIdentifierOffset(task, name) == offset {
			continue
		}
		if _, consumed := consumedOffsets[offset]; consumed {
			continue
		}
		if bestIndex < 0 || offset < bestOffset {
			bestIndex, bestOffset = index, offset
		}
	}
	if bestIndex < 0 {
		return exploreTarget{}, false
	}
	return targets[bestIndex], true
}

func localizationEvidenceTargetsFromDraft(task, exactID string, targets []exploreTarget, draft []exploreDraftEntry) []exploreTarget {
	if len(targets) == 0 {
		return targets
	}
	limit := len(targets)
	direct := make(map[string]exploreTarget, len(targets))
	for _, target := range targets {
		if target.node != nil {
			direct[exploreDraftNodeKey(target.node)] = target
		}
	}
	selected := make([]exploreTarget, 0, limit)
	seen := make(map[string]struct{}, limit)
	appendTarget := func(target exploreTarget) {
		if len(selected) >= limit || target.node == nil {
			return
		}
		key := exploreDraftNodeKey(target.node)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		selected = append(selected, target)
	}
	if draft == nil && strings.TrimSpace(task) != "" {
		draft = exploreAnswerDraft(task, targets)
	}
	// Preserve the issue author's explicit file/symbol anchor before causal
	// owner promotion. The owner still remains adjacent, but a proven upstream
	// cause must not hide the named implementation target from PRIMARY evidence.
	if explicitID := exploreLocalizationExplicitTarget(task, targets); explicitID != "" {
		for _, target := range targets {
			if target.node != nil && target.node.ID == explicitID {
				appendTarget(target)
				break
			}
		}
	}
	// An exact task path+line citation authenticates the enclosing declaration
	// as presentation evidence. Reserve every already-bounded range owner before
	// generic draft rows, while leaving completion authority unchanged.
	for _, target := range targets {
		if target.sourceRange {
			appendTarget(target)
		}
	}
	// A graph-proven causal constructor and its owning type outrank the
	// downstream retrieval seed. Their explicit admission metadata survives
	// draft ranking, owner folding, and byte-budget packing, so this ordering is
	// evidence-driven rather than a late symbol-name sort.
	for _, target := range targets {
		if target.divergentDefaultOwner {
			appendTarget(target)
			break
		}
	}
	for _, target := range targets {
		if target.divergentDefaultType {
			appendTarget(target)
			break
		}
	}
	// Primary retrieval evidence remains contractual under tight budgets and
	// follows the causal pair as its supporting consumer. A needs-refinement
	// caller may move its authorized target ahead afterward.
	appendTarget(targets[0])
	if exactID != "" {
		for _, target := range targets {
			if target.node != nil && target.node.ID == exactID {
				appendTarget(target)
				break
			}
		}
	}
	// One further concrete identifier that the task independently cites may
	// precede generic draft ranking. Graph-proven causal pairs, the retrieval
	// head, and any prescribed exact symbol have already retained their order.
	// Offsets represented above are consumed so one authored mention cannot
	// reserve several same-name overloads.
	if target, ok := localizationSecondaryTaskCitedTarget(task, targets, selected); ok {
		appendTarget(target)
	}
	// Unanchored content evidence may occupy at most two PRIMARY seats. Candidate
	// selection already bounds this lane; order its survivors by the number of
	// distinct task literals they corroborate before the ordinary draft fills
	// the projection.
	literalPrimaries := make([]exploreTarget, 0, exploreSourceLiteralReservationMax)
	for _, target := range targets {
		if target.literalPrimaryEligible {
			literalPrimaries = append(literalPrimaries, target)
		}
	}
	sort.SliceStable(literalPrimaries, func(i, j int) bool {
		return literalPrimaries[i].literalMatchCount > literalPrimaries[j].literalMatchCount
	})
	for index, target := range literalPrimaries {
		if index == exploreSourceLiteralReservationMax {
			break
		}
		appendTarget(target)
	}
	// A source-body literal is the only direct evidence that can identify an
	// implementation absent from symbol metadata. Reserve the strongest one
	// before draft promotion and byte-budget packing can consume every slot.
	// A construction-aligned callee — one that instantiates the task's value —
	// is the strongest literal owner; reserve it ahead of a generic literal
	// callee that merely leads primary retrieval, so it is never the sacrificial
	// tail when the projection is at the max_symbols boundary.
	for _, target := range targets {
		if target.sourceLiteral && target.sourceLiteralAligned {
			appendTarget(target)
			break
		}
	}
	for _, target := range targets {
		if target.sourceLiteral {
			appendTarget(target)
			break
		}
	}
	// A typed-field projection is admitted only after a complete bounded
	// field→owner←consumer→member proof. Reserve its consumer/member pair before
	// ordinary draft expansion, but behind pre-existing exact and source-literal
	// contracts so the new lane cannot demote stronger evidence.
	for _, target := range targets {
		if target.typedAnchorProjection {
			appendTarget(target)
		}
	}
	// A graph-proven change owner, bridge, and continuation outrank concept-only
	// retrieval complements. The retrieval head remains first; this reservation
	// only replaces non-causal tail rows when the final evidence window is full.
	for _, target := range targets {
		if target.causalChangeOwner {
			appendTarget(target)
			break
		}
	}
	for _, target := range targets {
		if target.causalChangeBridge {
			appendTarget(target)
			break
		}
	}
	for _, target := range targets {
		if target.causalChangeLeaf {
			appendTarget(target)
			break
		}
	}
	// Concept reservations are weaker than exact, source-literal, typed, and
	// divergent causal evidence, but stronger than draft relations. Keep the
	// primary and up to two diverse complements adjacent whenever those stronger
	// contracts leave room; only the primary may establish answer readiness.
	for _, target := range targets {
		if target.conceptImplementation {
			appendTarget(target)
			break
		}
	}
	for _, target := range targets {
		if target.conceptComplement {
			appendTarget(target)
		}
	}
	appendEntry := func(entry exploreDraftEntry) {
		if entry.node == nil {
			return
		}
		if target, ok := direct[exploreDraftNodeKey(entry.node)]; ok {
			appendTarget(target)
		} else {
			appendTarget(exploreTarget{node: entry.node})
		}
	}
	// Reserve bounded space for promoted implementations before lower-ranked
	// direct retrieval hits consume the envelope.
	for _, entry := range draft {
		if entry.structural || !entry.direct {
			appendEntry(entry)
		}
	}
	for _, entry := range draft {
		appendEntry(entry)
	}
	for _, target := range targets {
		appendTarget(target)
	}
	return selected
}

const (
	localizationDirectRelationSourceLimit = 5
	localizationDirectEvidenceReserve     = localizationRefinementAllowedSymbolCap
)

// interleaveLocalizationDirectRelations promotes one already-hydrated direct
// caller or callee beside each high-ranked evidence row. The relationship nodes
// came from the same scoped graph walks as the parent target; this helper never
// performs a new lookup or reconstructs an identity from rendered output.
func interleaveLocalizationDirectRelations(task, requiredID string, targets []exploreTarget) []exploreTarget {
	return interleaveLocalizationDirectRelationsWithRoutes(task, requiredID, targets, nil)
}

// exploreDelegatedImplementationCallee reports whether the callee is the
// caller's own implementation, recognised by the one convention that holds
// across languages: the implementation's identifier extends the wrapper's with
// a short suffix (a recursive, inner, or impl helper on the same base name).
// This is a naming convention, never a repository fact, so it carries no
// project-specific vocabulary.
func exploreDelegatedImplementationCallee(caller, callee *graph.Node) bool {
	if caller == nil || callee == nil {
		return false
	}
	if callee.Kind != graph.KindFunction && callee.Kind != graph.KindMethod {
		return false
	}
	if exploreDraftIsTestNode(callee) {
		return false
	}
	base := strings.TrimSpace(caller.Name)
	extended := strings.TrimSpace(callee.Name)
	const (
		// A short base admits the plural/variant family — Read/ReadAll,
		// Load/LoadAll — which are sibling operations, not delegations. Require
		// enough base identifier that the extension is a helper on the same work.
		minBaseRunes   = 6
		maxSuffixRunes = 8
	)
	if len([]rune(base)) < minBaseRunes || !strings.HasPrefix(extended, base) {
		return false
	}
	suffix := []rune(extended[len(base):])
	if len(suffix) == 0 || len(suffix) > maxSuffixRunes {
		return false
	}
	// The suffix must continue the identifier rather than start an unrelated
	// one: a separator or an uppercase boundary, never a digit series.
	head := suffix[0]
	return head == '_' || unicode.IsUpper(head) || unicode.IsLower(head)
}

func interleaveLocalizationDirectRelationsWithRoutes(
	task, requiredID string,
	targets []exploreTarget,
	routes map[string]localizationRefinementRoute,
) []exploreTarget {
	if len(targets) == 0 {
		return targets
	}
	limit := min(len(targets), localizationReplayEvidenceLimit)
	type relationCandidate struct {
		node       *graph.Node
		direction  string
		delegated  bool
		overlap    int
		longest    int
		production bool
		callable   bool
		order      int
	}

	direct := make(map[string]exploreTarget, len(targets))
	protected := make(map[string]struct{}, localizationDirectEvidenceReserve+6)
	orderedPrefix := 0
	hasDivergentDefault := false
	for index, target := range targets {
		if target.node == nil || target.node.ID == "" {
			continue
		}
		direct[target.node.ID] = target
		if index < localizationDirectEvidenceReserve || target.node.ID == requiredID ||
			target.causalChangeBridge || target.causalChangeLeaf || target.causalChangeOwner ||
			target.divergentDefaultOwner || target.divergentDefaultType || target.conceptImplementation || target.conceptComplement ||
			target.sourceRange || target.exactContent || target.sourceLiteral || target.typedAnchorProjection ||
			// Depth rows already paid for the tail they occupy; a relationship
			// row must not reclaim the same slot a second time.
			target.leadingFileDepth {
			protected[target.node.ID] = struct{}{}
		}
		if target.node.ID == requiredID || target.causalChangeBridge || target.causalChangeLeaf || target.causalChangeOwner ||
			target.divergentDefaultOwner || target.divergentDefaultType ||
			target.conceptImplementation || target.conceptComplement || target.sourceRange ||
			target.exactContent || target.sourceLiteral || target.typedAnchorProjection {
			orderedPrefix = index + 1
		}
		if target.divergentDefaultOwner || target.divergentDefaultType {
			hasDivergentDefault = true
		}
	}
	// Divergent-default evidence is an ordered causal triple: promoted owner,
	// owning type, then the original consumer. Relationships may enrich the
	// projection only after that prefix, never between its proof rows.
	if hasDivergentDefault && orderedPrefix < len(targets) {
		orderedPrefix++
	}
	terms := exploreTerminalTerms(shapeExploreQuery(task))
	type refinementPair struct {
		wrapper        string
		implementation string
	}
	provenWrapperRoutes := make(map[refinementPair]struct{}, len(routes))
	for symbol, route := range routes {
		if !route.enforceable {
			continue
		}
		wrapper, implementation := "", ""
		switch {
		case route.implementationSymbol != "":
			wrapper, implementation = symbol, route.implementationSymbol
		case route.proofSymbol != "":
			wrapper, implementation = route.proofSymbol, symbol
		}
		if wrapper == "" || implementation == "" {
			continue
		}
		if _, wrapperVisible := direct[wrapper]; !wrapperVisible {
			continue
		}
		if _, implementationVisible := direct[implementation]; !implementationVisible {
			continue
		}
		provenWrapperRoutes[refinementPair{wrapper: wrapper, implementation: implementation}] = struct{}{}
	}
	selected := make([]exploreTarget, 0, limit)
	seen := make(map[string]struct{}, limit)
	appendTarget := func(target exploreTarget) bool {
		if len(selected) >= limit || target.node == nil || target.node.ID == "" || nodeDisplayPath(target.node) == "" {
			return false
		}
		if _, exists := seen[target.node.ID]; exists {
			return false
		}
		seen[target.node.ID] = struct{}{}
		selected = append(selected, target)
		return true
	}
	chooseRelation := func(target exploreTarget) (exploreTarget, bool) {
		var best relationCandidate
		found := false
		order := 0
		consider := func(nodes []*graph.Node, direction string) {
			for _, node := range nodes {
				candidateOrder := order
				order++
				if node == nil || node.ID == "" || node.ID == target.node.ID || nodeDisplayPath(node) == "" || !exploreLocalizableKind(node.Kind) {
					continue
				}
				if _, exists := seen[node.ID]; exists {
					continue
				}
				overlap, longest := exploreDraftTermOverlap(terms, node)
				_, provenWrapper := provenWrapperRoutes[refinementPair{
					wrapper: target.node.ID, implementation: node.ID,
				}]
				delegated := direction == "direct_callee" &&
					exploreDelegatedImplementationCallee(target.node, node)
				if overlap == 0 && !delegated && (direction != "direct_callee" || !provenWrapper) {
					continue
				}
				candidate := relationCandidate{
					node:       node,
					direction:  direction,
					delegated:  delegated,
					overlap:    overlap,
					longest:    longest,
					production: !exploreDraftIsTestNode(node),
					callable:   node.Kind == graph.KindFunction || node.Kind == graph.KindMethod,
					order:      candidateOrder,
				}
				// A callee whose identifier extends its caller's is that caller's
				// implementation, and it shares every task term the wrapper matched
				// — so overlap can never separate the pair. Rank the delegation
				// first, otherwise the wrapper's own siblings win on list order and
				// the code that actually changes never reaches the answer.
				better := !found || (candidate.delegated && !best.delegated) ||
					(candidate.delegated == best.delegated && candidate.overlap > best.overlap) ||
					(candidate.delegated == best.delegated && candidate.overlap == best.overlap && candidate.production && !best.production) ||
					(candidate.overlap == best.overlap && candidate.production == best.production && candidate.callable && !best.callable) ||
					(candidate.overlap == best.overlap && candidate.production == best.production && candidate.callable == best.callable && candidate.longest > best.longest) ||
					(candidate.overlap == best.overlap && candidate.production == best.production && candidate.callable == best.callable && candidate.longest == best.longest && candidate.order < best.order)
				if better {
					best = candidate
					found = true
				}
			}
		}
		// Callees win a complete tie because a wrapper-to-implementation hop is
		// the most common missing terminal relation. A task-aligned caller still
		// wins through the overlap comparison above.
		consider(target.callees, "direct_callee")
		consider(target.callers, "direct_caller")
		if !found {
			return exploreTarget{}, false
		}
		relation, exists := direct[best.node.ID]
		if !exists {
			relation = exploreTarget{node: best.node, localizationRelation: best.direction}
		} else if relation.node.ID != requiredID &&
			!relation.causalChangeBridge && !relation.causalChangeLeaf && !relation.causalChangeOwner &&
			!relation.divergentDefaultOwner && !relation.divergentDefaultType &&
			!relation.conceptImplementation && !relation.conceptComplement &&
			!relation.exactContent && !relation.sourceLiteral && !relation.typedAnchorProjection {
			// A draft-promoted graph neighbor may already be present in the direct
			// map. Preserve its hydrated data while retaining the relation proof.
			relation.localizationRelation = best.direction
		}
		return relation, true
	}

	protectedRemaining := func() int {
		remaining := 0
		for id := range protected {
			if _, exists := seen[id]; !exists {
				remaining++
			}
		}
		return remaining
	}
	for index, target := range targets {
		targetProtected := false
		if target.node != nil {
			_, targetProtected = protected[target.node.ID]
		}
		if targetProtected || len(selected)+protectedRemaining() < limit {
			appendTarget(target)
		}
		if len(selected) >= limit {
			break
		}
		if index+1 < orderedPrefix || index >= localizationDirectRelationSourceLimit ||
			target.node == nil || len(selected)+protectedRemaining() >= limit {
			continue
		}
		if relation, ok := chooseRelation(target); ok {
			appendTarget(relation)
		}
	}
	return selected
}

// prioritizeLocalizationConceptComplement keeps the implementation pair close
// enough for agents to interpret it as one unit. The semantic head and its
// primary implementation retain ranks one and two; a later task-complementary
// callable is stably rotated into rank three before the aligned wire projection
// is packed. No row is added or removed.
func prioritizeLocalizationConceptComplement(targets []exploreTarget) []exploreTarget {
	if len(targets) < 3 {
		return targets
	}
	hasLeadingImplementation := false
	for index := 0; index < 2; index++ {
		if targets[index].conceptImplementation {
			hasLeadingImplementation = true
			break
		}
	}
	if !hasLeadingImplementation {
		return targets
	}
	for index := 2; index < len(targets); index++ {
		if !targets[index].conceptComplement {
			continue
		}
		if index == 2 {
			return targets
		}
		ordered := append([]exploreTarget(nil), targets...)
		complement := ordered[index]
		copy(ordered[3:index+1], ordered[2:index])
		ordered[2] = complement
		return ordered
	}
	return targets
}

func newLocalizationExploreResult(completion localizationCompletion, targets []exploreTarget, budget int) *mcp.CallToolResult {
	return newLocalizationExploreResultForTask(completion, "", targets, budget)
}

const (
	localizationEnvelopeBytesPerToken = 4
	localizationMaxNameRunes          = 120
	localizationMaxQualNameRunes      = 180
	localizationMaxSignatureRunes     = 240
	localizationMaxNeighborIDs        = 3
	localizationMaxNeighborIDRunes    = 96
)

func compactLocalizationField(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

func localizationEnvelopeFits(envelope localizationExploreEnvelope, maxBytes int) bool {
	body, err := json.Marshal(envelope)
	return err == nil && len(body) <= maxBytes
}

func newLocalizationExploreResultForTask(completion localizationCompletion, task string, targets []exploreTarget, budget int) *mcp.CallToolResult {
	result, _, _ := buildLocalizationExploreResultForTask(completion, task, targets, budget)
	return result
}

// buildLocalizationExploreResultForTask returns the packed result and the exact
// bounded symbol projection serialized into it. Callers that also need the
// post-budget completion use buildLocalizationExploreResultForTaskFinalized.
func buildLocalizationExploreResultForTask(
	completion localizationCompletion,
	task string,
	targets []exploreTarget,
	budget int,
	finalize ...localizationCompletionFinalizer,
) (*mcp.CallToolResult, []string, *localizationEvidenceDigest) {
	result, symbols, digest, _ := buildLocalizationExploreResultForTaskFinalized(
		completion, task, targets, budget, finalize...,
	)
	return result, symbols, digest
}

type localizationCompletionFinalizer func(localizationExploreEnvelope) localizationCompletion

func buildLocalizationRefinementResultForTask(
	preferredSymbol, task string,
	targets []exploreTarget,
	budget int,
	routes map[string]localizationRefinementRoute,
) (*mcp.CallToolResult, localizationCompletion, map[string]localizationRefinementRoute, *localizationEvidenceDigest) {
	return buildLocalizationRefinementResultForTaskWithOutline(
		preferredSymbol, task, targets, budget, routes, nil,
	)
}

// The outline variant additionally offers the leading file's declaration index
// to whichever page this build settles on; see localization_file_outline.go.
func buildLocalizationRefinementResultForTaskWithOutline(
	preferredSymbol, task string,
	targets []exploreTarget,
	budget int,
	routes map[string]localizationRefinementRoute,
	outline func() *localizationPageOutline,
) (*mcp.CallToolResult, localizationCompletion, map[string]localizationRefinementRoute, *localizationEvidenceDigest) {
	return buildLocalizationRefinementResultForTaskWithOutlineAndDeclarations(
		preferredSymbol, task, targets, budget, routes, outline, nil,
	)
}

func buildLocalizationRefinementResultForTaskWithOutlineAndDeclarations(
	preferredSymbol, task string,
	targets []exploreTarget,
	budget int,
	routes map[string]localizationRefinementRoute,
	outline func() *localizationPageOutline,
	declarations *localizationFileDeclarationCache,
) (*mcp.CallToolResult, localizationCompletion, map[string]localizationRefinementRoute, *localizationEvidenceDigest) {
	choosePreferred := func(symbols []string, requested string) (string, []string, map[string]localizationRefinementRoute) {
		authorized, bounded := boundedLocalizationRefinementRoutes(symbols, routes, requested)
		if requested != "" {
			if _, ok := bounded[requested]; ok {
				return requested, authorized, bounded
			}
		}
		_, bounded = boundedLocalizationRefinementRoutes(symbols, routes, "")
		for _, symbol := range symbols {
			if _, ok := bounded[symbol]; !ok {
				continue
			}
			authorized, bounded = boundedLocalizationRefinementRoutes(symbols, routes, symbol)
			return symbol, authorized, bounded
		}
		return "", nil, nil
	}

	candidateSymbols := exploreLocalizationTargetSymbols(targets)
	preferredSymbol, preauthorized, prebounded := choosePreferred(candidateSymbols, preferredSymbol)
	if preferredSymbol == "" {
		advisory := newLocalizationCompletion(true, "")
		result, _, digest, packedCompletion := buildLocalizationExploreResultForTaskFinalizedWithOutlineAndDeclarations(
			advisory, task, targets, budget, outline, declarations,
		)
		return result, packedCompletion, nil, digest
	}

	// Budget against the largest completion this envelope can expose. The final
	// allowed set is an equal or smaller intersection with serialized symbols,
	// so replacing this provisional contract cannot invalidate the byte cap.
	budgetCompletion := newLocalizationRefinementCompletionForSymbols(preferredSymbol, preauthorized)
	budgetCompletion.refinementRoutes = prebounded
	var finalRoutes map[string]localizationRefinementRoute
	result, _, digest, packedCompletion := buildLocalizationExploreResultForTaskFinalizedWithOutlineAndDeclarations(
		budgetCompletion, task, targets, budget, outline, declarations,
		func(packed localizationExploreEnvelope) localizationCompletion {
			packedPreferred, allowedSymbols, bounded := choosePreferred(packed.Symbols, preferredSymbol)
			if packedPreferred == "" {
				finalRoutes = nil
				return newLocalizationCompletion(true, "")
			}
			finalRoutes = localizationBoundRouteEvidence(bounded, packed)
			completion := newLocalizationRefinementCompletionForSymbols(packedPreferred, allowedSymbols)
			completion.refinementRoutes = finalRoutes
			return completion
		},
	)
	return result, packedCompletion, finalRoutes, digest
}

// The finalized variant additionally returns the exact completion used by both
// visible text and authoritative host metadata after byte-budget packing.
func buildLocalizationExploreResultForTaskFinalized(
	completion localizationCompletion,
	task string,
	targets []exploreTarget,
	budget int,
	finalize ...localizationCompletionFinalizer,
) (*mcp.CallToolResult, []string, *localizationEvidenceDigest, localizationCompletion) {
	return buildLocalizationExploreResultForTaskFinalizedWithOutline(
		completion, task, targets, budget, nil, finalize...,
	)
}

func buildLocalizationExploreResultForTaskFinalizedWithOutline(
	completion localizationCompletion,
	task string,
	targets []exploreTarget,
	budget int,
	outline func() *localizationPageOutline,
	finalize ...localizationCompletionFinalizer,
) (*mcp.CallToolResult, []string, *localizationEvidenceDigest, localizationCompletion) {
	return buildLocalizationExploreResultForTaskFinalizedWithOutlineAndDeclarations(
		completion, task, targets, budget, outline, nil, finalize...,
	)
}

func buildLocalizationExploreResultForTaskFinalizedWithOutlineAndDeclarations(
	completion localizationCompletion,
	task string,
	targets []exploreTarget,
	budget int,
	outline func() *localizationPageOutline,
	declarations *localizationFileDeclarationCache,
	finalize ...localizationCompletionFinalizer,
) (*mcp.CallToolResult, []string, *localizationEvidenceDigest, localizationCompletion) {
	var draft []exploreDraftEntry
	if strings.TrimSpace(task) != "" {
		draft = exploreAnswerDraft(task, targets)
	}
	preferredBodyIDs := explorePreferredFullBodyIDs(task, targets, draft, exploreFullBodyLimit)
	primarySymbol := ""
	if len(targets) > 0 && targets[0].node != nil {
		primarySymbol = targets[0].node.ID
	}
	requiredSymbol := completion.ExactSymbol
	refinementFirst := completion.State == localizationStateNeedsRefinement
	if refinementFirst {
		requiredSymbol = completion.refinementSymbol
	}
	targets = localizationEvidenceTargetsFromDraft(task, requiredSymbol, targets, draft)
	// The draft's selection is final here; only its presentation order is not.
	// Applying the tiers before the relation, refinement, and complement lanes
	// leaves each of them authoritative over the seats they claim.
	targets = demoteLocalizationEvidenceMembers(task, requiredSymbol, targets)
	if refinementFirst {
		targets = prioritizeLocalizationEvidenceTarget(requiredSymbol, targets)
		// A divergent-default refinement is one causal unit: keep the prescribed
		// constructor adjacent to its owning type before the named consumer.
		targets = preserveExploreDivergentDefaultOrder(targets)
	}
	targets = interleaveLocalizationDirectRelationsWithRoutes(
		task, requiredSymbol, targets, completion.refinementRoutes,
	)
	targets = prioritizeLocalizationConceptComplement(targets)
	contract := localizationContractFor(completion)
	envelope := localizationExploreEnvelope{
		Completion: contract.Completion,
		Terminal:   contract.Terminal,
		Files:      make([]string, 0),
		Symbols:    make([]string, 0),
		Evidence:   make([]localizationEvidence, 0),
	}
	maxBytes := budget * localizationEnvelopeBytesPerToken
	if maxBytes < 1 {
		maxBytes = 1
	}
	// Row admission must leave room for the answer block that is attached
	// only after packing: a page whose rows fill the whole wire budget has no
	// way to carry its own conclusion. The allowance covers the rendered
	// primary block and directive at the digest shrink floor.
	admitBytes := maxBytes - localizationAnswerPageAllowanceBytes
	if admitBytes < 1 {
		admitBytes = 1
	}

	mandatoryCount := 0
	if len(targets) > 0 {
		mandatoryCount = 1
	}
	for index, target := range targets {
		if (target.divergentDefaultOwner || target.divergentDefaultType) && index+1 > mandatoryCount {
			mandatoryCount = index + 1
		}
		if target.typedAnchorProjection && index+1 > mandatoryCount {
			mandatoryCount = index + 1
		}
		if (target.conceptImplementation || target.conceptComplement) && index+1 > mandatoryCount {
			mandatoryCount = index + 1
		}
	}
	// Primary retrieval evidence and the authorized exact/refinement symbol are
	// both mandatory regardless of which one leads the serialized projection.
	// A generic preferred candidate also reserves its one prevalidated concrete
	// hop, because the route is invalid unless both IDs are visible on the wire.
	mandatoryIDs := []string{primarySymbol, requiredSymbol}
	if route, routed := completion.refinementRoutes[requiredSymbol]; routed {
		if route.implementationSymbol != "" {
			mandatoryIDs = append(mandatoryIDs, route.implementationSymbol)
		}
		if route.proofSymbol != "" {
			mandatoryIDs = append(mandatoryIDs, route.proofSymbol)
		}
	}
	// Packing operates on a prefix, so retain through the latest mandatory ID.
	for _, mandatoryID := range mandatoryIDs {
		if mandatoryID == "" {
			continue
		}
		for index, target := range targets {
			if target.node != nil && target.node.ID == mandatoryID && index+1 > mandatoryCount {
				mandatoryCount = index + 1
				break
			}
		}
	}
	literalMandatory := 0
	for index, target := range targets {
		if !target.literalPrimaryEligible && !localizationStrongSourceLiteralCallee(target) {
			continue
		}
		if index+1 > mandatoryCount {
			mandatoryCount = index + 1
		}
		literalMandatory++
		if literalMandatory == localizationFinalResponseLiteralReserve {
			break
		}
	}
	for index, target := range targets {
		if target.sourceLiteral && index+1 > mandatoryCount {
			mandatoryCount = index + 1
			break
		}
	}
	// The construction-aligned literal owner is reserved evidence: guarantee its
	// identifier survives byte-budget packing even when a generic literal callee
	// leads the projection and holds the first source-literal slot.
	for index, target := range targets {
		if target.sourceLiteral && target.sourceLiteralAligned && index+1 > mandatoryCount {
			mandatoryCount = index + 1
			break
		}
	}

	acceptedTargets := make([]exploreTarget, 0, len(targets))
	for _, target := range targets {
		if target.node == nil {
			continue
		}
		n := target.node
		path := nodeDisplayPath(n)
		retrieval := n.RetrievalMetadata()
		provenance := localizationTargetProvenance(completion, target)
		if target.localizationRelation != "" {
			provenance = target.localizationRelation
		}
		evidence := localizationEvidence{
			Rank: len(envelope.Evidence) + 1, ID: n.ID,
			Name: compactLocalizationField(n.Name, localizationMaxNameRunes),
			Kind: string(n.Kind), File: path,
			Line: n.StartLine, EndLine: n.EndLine,
			QualName:   compactLocalizationField(retrieval.QualName, localizationMaxQualNameRunes),
			Signature:  compactLocalizationField(retrieval.Signature, localizationMaxSignatureRunes),
			Callers:    boundedLocalizationNeighborIDs(target.callers, localizationMaxNeighborIDs),
			Callees:    boundedLocalizationNeighborIDs(target.callees, localizationMaxNeighborIDs),
			Provenance: provenance,

			literalPrimaryEligible: target.literalPrimaryEligible || localizationStrongSourceLiteralCallee(target),
			supportingOnly:         target.leadingFileDepth,
			leadingFileDepth:       target.leadingFileDepth,
		}

		candidate := envelope
		candidate.Files = append([]string(nil), envelope.Files...)
		candidate.Symbols = append([]string(nil), envelope.Symbols...)
		candidate.Evidence = append([]localizationEvidence(nil), envelope.Evidence...)
		// Files, Symbols, and Evidence are one positional projection. Repeated
		// files are intentional when multiple ranked symbols share a source file.
		candidate.Files = append(candidate.Files, path)
		candidate.Symbols = append(candidate.Symbols, n.ID)
		candidate.Evidence = append(candidate.Evidence, evidence)
		mandatory := len(envelope.Evidence) < mandatoryCount
		if !localizationEnvelopeFits(candidate, admitBytes) {
			if !mandatory {
				continue
			}
			// Primary and exact identifiers are contractual. If their optional
			// metadata would exceed the envelope, retain the identifiers and
			// locations while shedding expansion details first.
			evidence.QualName = ""
			evidence.Signature = ""
			evidence.Callers = nil
			evidence.Callees = nil
			candidate.Evidence[len(candidate.Evidence)-1] = evidence
		}
		envelope = candidate
		acceptedTargets = append(acceptedTargets, target)
	}

	// Add full bodies only when the complete serialized response still fits.
	// JSON marshaling here accounts for metadata arrays and escaping that the
	// previous source-only token estimate omitted. Preferred IDs reserve one
	// slot for a promoted cross-file causal boundary when it has source.
	bodyOrder := make([]int, 0, exploreFullBodyLimit)
	seenBody := make(map[int]struct{}, exploreFullBodyLimit)
	appendBodyIndex := func(index int) {
		if index < 0 || index >= len(acceptedTargets) || len(bodyOrder) >= exploreFullBodyLimit {
			return
		}
		if _, exists := seenBody[index]; exists {
			return
		}
		seenBody[index] = struct{}{}
		bodyOrder = append(bodyOrder, index)
	}
	// The authorized exact symbol's body is precisely what the prescribed read
	// would return, so it packs first: shipping it here can retire the round
	// trip, while a preferred body that loses the slot stays one call away.
	if envelope.Completion.State == localizationStateNeedsExactRead && requiredSymbol != "" {
		for index, target := range acceptedTargets {
			if target.node != nil && target.node.ID == requiredSymbol {
				appendBodyIndex(index)
				break
			}
		}
	}
	for _, preferredID := range preferredBodyIDs {
		for index, target := range acceptedTargets {
			if target.node != nil && exploreDraftNodeKey(target.node) == preferredID {
				appendBodyIndex(index)
				break
			}
		}
	}
	for index := range acceptedTargets {
		appendBodyIndex(index)
	}
	// A needs-refinement response carries no source body by default: the single
	// authorized read supplies the chosen body once, and duplicating it here
	// would pay the largest payload twice. The one exception is handled below —
	// a body that lets the page retire the read outright is not a duplicate.
	if completion.State != localizationStateNeedsRefinement {
		for _, index := range bodyOrder {
			if acceptedTargets[index].source == "" {
				continue
			}
			candidate := envelope
			candidate.Evidence = append([]localizationEvidence(nil), envelope.Evidence...)
			candidate.Evidence[index].Source = acceptedTargets[index].source
			if localizationEnvelopeFits(candidate, maxBytes) {
				envelope = candidate
			}
		}
	}

	if len(finalize) > 0 && finalize[0] != nil {
		envelope.Completion = finalize[0](envelope)
	}
	// A prescribed read this envelope already answers buys nothing: the caller
	// would spend a call to receive the body packed above. Complete instead,
	// keeping the same evidence and the same lead-alignment bar.
	satisfiedSymbol := ""
	prescribedEnvelope := envelope
	// Inlining a body is only worth its bytes if the page can then drop the
	// instruction to go and read it. Measurement is explicit here: inlining
	// alone leaves most callers reading the symbol anyway, because the page
	// still told them to. So pack the body and retire the read together, or
	// do neither — the half-measure costs payload and saves no call.
	claimedAnswer := false
	if prescribed := localizationPrescribedSymbol(envelope.Completion); prescribed != "" &&
		localizationPrescriptionHasNothingLeftToChoose(envelope.Completion) {
		// The allowance is what makes this reachable: a full page fills its
		// budget with ranked rows, so at the ordinary limit an ordinary body
		// overflows by a few hundred bytes and the round trip survives.
		retiring := localizationEnvelopePackingPrescribedBody(
			envelope, acceptedTargets, bodyOrder, prescribed,
			maxBytes+localizationRetiredReadAllowance(maxBytes))
		if localizationEvidenceCarriesPackedBody(retiring, prescribed) {
			satisfiedSymbol = prescribed
			envelope = retiring
			if localizationPrescribedReadSatisfiedByEnvelope(task, prescribed, retiring) {
				claimedAnswer = true
				envelope.Completion = localizationCompletionRetiringPrescribedRead(envelope.Completion)
			} else {
				envelope.Completion = localizationCompletionReleasingPrescribedRead(envelope.Completion)
			}
		}
	}
	// Strong enforcement is derived only from proof rows that survived final
	// byte-budget packing. Visible text, retained state, and host metadata then
	// share this one normalized completion value.
	envelope.Completion = localizationFinalizeCompletionEvidence(task, envelope.Completion, acceptedTargets, envelope)
	if claimedAnswer && envelope.Completion.State != localizationStateAnswerReady {
		// The policy would not stand behind the retirement — an unproven answer
		// is demoted to a recovery page, which is a worse offer than the bounded
		// read we started from. Put back the prescription, and with it the bytes
		// that only made sense if the read went away.
		satisfiedSymbol = ""
		envelope = prescribedEnvelope
		envelope.Completion = localizationFinalizeCompletionEvidence(
			task, envelope.Completion, acceptedTargets, envelope)
	}
	contract = localizationContractFor(envelope.Completion)
	envelope.Completion = contract.Completion
	envelope.Terminal = contract.Terminal
	digest := newLocalizationEvidenceDigestForTask(task, envelope)
	// The serialized completion, returned state, structuredContent, and host
	// metadata must be derived from the exact retained rows. This same helper is
	// called again after any later byte shedding so authorization cannot outlive
	// its identity or proof dependency.
	contract = localizationContractReconciledWithDigest(envelope.Completion, digest)
	envelope.Completion = contract.Completion
	envelope.Terminal = contract.Terminal
	// A page that still asks the caller to choose gets its files' declaration
	// indexes. The rows above name at most one symbol per file, so a caller
	// looking at the right file and the wrong row has nothing to correct with;
	// the outlines are those files' remaining declarations, and they are the
	// first payload given back when the budget cannot hold everything.
	if outline != nil && localizationPageAcceptsOutline(envelope.Completion.State) {
		if page := outline().clone(); page != nil {
			envelope.Outline, envelope.Outlines = page.Leading, page.Others
		}
	}
	// The ready-to-emit answer is derived from the retained rows, so it lands
	// after the fit checks above and can push the envelope past its budget.
	// Give back the weakest presented row first — the caller is asked to
	// reproduce these lines, so a row that cannot be shown is a row that cannot
	// be retained either — then packed source bodies, which are the largest and
	// most recoverable payload. A body that just retired a prescribed read is
	// kept: it is the evidence the caller would otherwise have paid a call for.
	shedBudget := maxBytes
	if satisfiedSymbol != "" {
		shedBudget = maxBytes + localizationRetiredReadAllowance(maxBytes)
	}
	// The rows as they stood before the breadth tail was asked to pay for an
	// index. A trade that ends with no index bought nothing, so it is undone.
	var untraded []localizationEvidence
	for !localizationEnvelopeFits(envelope, shedBudget) {
		if localizationShedSourceWindow(&envelope) {
			continue
		}
		block := &localizationPageOutline{Leading: envelope.Outline, Others: envelope.Outlines}
		if !block.empty() {
			// The outlines are a convenience over rows the caller can still
			// reach by other means, so they give way before every other payload
			// here — but they give way by degrees. A shorter index is worth far
			// more than none, and only pressure past the floor drops one.
			if block.atFloor() {
				// A rank-two-or-later file at its floor is less valuable than
				// independently useful evidence detail. Drop that expendable
				// breadth before stripping a row; the leading pair stays protected.
				if block.dropUnprotectedFloorFile() {
					envelope.Outline, envelope.Outlines = block.Leading, block.Others
					continue
				}
				// The protected index has given back everything it can and the
				// page still does not fit. The expendable evidence tail now pays,
				// in detail a caller can re-derive from the identity that stays —
				// never in a row or refinement reserve.
				if untraded == nil {
					untraded = append([]localizationEvidence(nil), envelope.Evidence...)
				}
				if localizationShedTrailingEvidenceDetail(&envelope, mandatoryCount) {
					continue
				}
			}
			block.relieve()
			envelope.Outline, envelope.Outlines = block.Leading, block.Others
			if block.empty() && untraded != nil {
				// Nothing was bought. Give the tail its detail back rather than
				// return a page that is both shorter and unindexed.
				envelope.Evidence = untraded
				untraded = nil
			}
			continue
		}
		if digest != nil && len(digest.Evidence) > localizationDigestShrinkFloorRows {
			digest.Evidence = digest.Evidence[:len(digest.Evidence)-1]
			rebuildLocalizationDigestSkeleton(digest)
			refreshLocalizationDigestResponses(digest, task, nil)
			contract = localizationContractReconciledWithDigest(envelope.Completion, digest)
			envelope.Completion = contract.Completion
			envelope.Terminal = contract.Terminal
			continue
		}
		shed := -1
		for index := range envelope.Evidence {
			if strings.TrimSpace(envelope.Evidence[index].Source) == "" ||
				envelope.Evidence[index].ID == satisfiedSymbol {
				continue
			}
			shed = index
		}
		if shed < 0 {
			// The same trade the indexed page makes, for a page that carries
			// no index: the breadth tail gives back detail the caller can
			// re-derive from the identity that stays — never a row. Without
			// this, a page whose rows filled the budget before the answer
			// block was attached has no way back under its wire bound.
			if localizationShedTrailingEvidenceDetail(&envelope, mandatoryCount) {
				continue
			}
			// Last resort. The unproven page is the answer a caller that stops
			// here would give, so it outranks every packed body and survives
			// until nothing else can be given back — and even then it goes only
			// if going makes the envelope fit. Dropping it from a response that
			// is over budget either way costs the caller its answer and buys
			// nothing.
			page := envelope.Completion.FinalResponse
			if envelope.Completion.State != localizationStateAnswerReady && page != "" {
				envelope.Completion.FinalResponse = ""
				if localizationEnvelopeFits(envelope, shedBudget) {
					break
				}
				envelope.Completion.FinalResponse = page
			}
			break
		}
		envelope.Evidence[shed].Source = ""
	}
	for _, target := range acceptedTargets {
		if target.sourceWindow == nil {
			continue
		}
		envelope = localizationEnvelopePackingSourceWindow(envelope, target.sourceWindow, maxBytes)
		break
	}
	// Freeze the ranked answer before adjacency/body/depth supplementation can
	// rebuild the digest and accidentally promote a newly materialized row.
	freezeLocalizationPrimaryCohort(task, &envelope, digest)
	if declarations != nil {
		envelope, digest = promoteLocalizationDirectAdjacency(task, envelope, declarations.reader, len(envelope.Evidence), shedBudget, digest)
	}
	envelope, digest = promoteLocalizationBodyMentions(task, envelope, declarations, shedBudget, digest)
	body, err := json.Marshal(envelope)
	if err != nil {
		return mcp.NewToolResultError("encode localization result: " + err.Error()), nil, nil, envelope.Completion
	}
	result := attachLocalizationHostEnvelope(mcp.NewToolResultText(string(body)), envelope.Completion, digest)
	return result, append([]string(nil), envelope.Symbols...), digest, envelope.Completion
}

// localizationShedTrailingEvidenceDetail gives back one trailing row's
// expansion detail — the qualified name, signature, and neighbour identifiers a
// caller can re-derive from the identity that stays. Only the breadth tail past
// the direct-evidence reserve is eligible, and the row's ID, kind, file, line,
// and provenance are untouched, so nothing the completion contract or the
// evidence policy reads can move. This is the same trade the packing loop
// already makes for a mandatory row that will not fit whole.
func localizationShedTrailingEvidenceDetail(envelope *localizationExploreEnvelope, mandatory int) bool {
	protected := max(mandatory, localizationDirectEvidenceReserve)
	for index := len(envelope.Evidence) - 1; index >= protected; index-- {
		row := &envelope.Evidence[index]
		if row.QualName == "" && row.Signature == "" && len(row.Callers) == 0 && len(row.Callees) == 0 {
			continue
		}
		row.QualName, row.Signature = "", ""
		row.Callers, row.Callees = nil, nil
		return true
	}
	return false
}

func boundedLocalizationNeighborIDs(nodes []*graph.Node, limit int) []string {
	ids := localizationNeighborIDs(nodes)
	if limit >= 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	for index := range ids {
		ids[index] = compactLocalizationField(ids[index], localizationMaxNeighborIDRunes)
	}
	return ids
}

func localizationNeighborIDs(nodes []*graph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil && node.ID != "" {
			ids = append(ids, node.ID)
		}
	}
	return ids
}

// renderExplore lays out the ranked neighborhood as a compact, agent-facing
// text block: likely targets (with call paths + source), a file map, and a
// trailing completeness cue — the measured antidote to the cross-check turn.
// Source bodies are packed newest-first until the token budget fills, then
// demoted to signature stubs; every candidate location is always listed.
func (s *Server) renderExplore(task string, targets []exploreTarget, budget int) string {
	var b strings.Builder
	files := map[string][]string{}
	fileOrder := []string{}
	addFile := func(path, sym string) {
		if _, ok := files[path]; !ok {
			fileOrder = append(fileOrder, path)
		}
		files[path] = append(files[path], sym)
	}

	fmt.Fprintf(&b, "EXPLORE — %s\n\n", truncateOneLine(task, 200))
	b.WriteString("RANKED LOCALIZATION: use the evidence below with stated uncertainty. Do not fan out or rerun broad exploration. Localization-only callers receive a separate completion contract; diagnosis and change callers continue from this evidence.\n\n")
	draft := exploreAnswerDraft(task, targets)
	b.WriteString("## Answer draft\n")
	for _, entry := range draft {
		if !entry.direct && entry.node.ID != "" {
			fmt.Fprintf(&b, "- FILE: %s  ·  SYMBOL: %s  ·  ID: %s  ·  EVIDENCE: %s\n",
				nodeLoc(entry.node), exploreDraftSymbol(entry.node), entry.node.ID, entry.evidence)
			continue
		}
		fmt.Fprintf(&b, "- FILE: %s  ·  SYMBOL: %s  ·  EVIDENCE: %s\n",
			nodeLoc(entry.node), exploreDraftSymbol(entry.node), entry.evidence)
	}
	fullBodyIDs := make(map[string]struct{}, exploreFullBodyLimit)
	for _, id := range explorePreferredFullBodyIDs(task, targets, draft, exploreFullBodyLimit) {
		fullBodyIDs[id] = struct{}{}
	}
	b.WriteString("\n## Likely targets (most-relevant first)\n")
	b.WriteString("Graph-verified details follow; all candidate locations and signatures are retained, with full source for the strongest direct draft targets.\n")

	used := estimateTokens(b.String())
	truncated := false
	for i, t := range targets {
		n := t.node
		path := nodeDisplayPath(n)
		addFile(path, n.Name)

		var head strings.Builder
		fmt.Fprintf(&head, "\n%d. %s  %s  ·  %s  ·  id: %s\n", i+1, n.Name, n.Kind, nodeLoc(n), n.ID)
		if len(t.callers) > 0 {
			fmt.Fprintf(&head, "   ^ callers: %s\n", joinNeighbors(t.callers))
		}
		if len(t.callees) > 0 {
			fmt.Fprintf(&head, "   v calls:   %s\n", joinNeighbors(t.callees))
		}
		b.WriteString(head.String())
		used += estimateTokens(head.String())

		// Source body: full for the strongest direct draft targets while the
		// budget holds (no single body may take more than 1/exploreBodyBudgetShare
		// of the whole budget), signature stub otherwise. The header/locations
		// above are always emitted so
		// file-hit / symbol-hit never depend on budget.
		body := ""
		if t.source != "" {
			cost := estimateTokens(t.source)
			_, preferred := fullBodyIDs[exploreDraftNodeKey(n)]
			if preferred && used+cost <= budget && cost <= budget/exploreBodyBudgetShare {
				body = t.source
			} else {
				if sig, err := elide.CompressString(t.source, n.Language); err == nil && sig != "" {
					body = sig
				} else {
					body = firstLines(t.source, 3)
				}
				if used+estimateTokens(body) > budget {
					body = ""
				}
				truncated = true
			}
		}
		if body != "" {
			fmt.Fprintf(&b, "```%s\n%s\n```\n", fenceLang(n.Language), strings.TrimRight(body, "\n"))
			used += estimateTokens(body)
		}
	}

	b.WriteString("\n## Candidate files\n")
	for _, f := range fileOrder {
		fmt.Fprintf(&b, "- %s  ·  %s\n", f, strings.Join(dedupStrings(files[f]), ", "))
	}

	fmt.Fprintf(&b, "\n— Coverage: %d ranked candidate symbol(s) across %d file(s); callers/callees resolved server-side. Locations and IDs above are citeable.\n",
		len(targets), len(fileOrder))
	b.WriteString("END OF LOCALIZATION — localization-only callers answer from this evidence; diagnosis and change callers proceed from it.\n")
	if truncated {
		fmt.Fprintf(&b, "Some bodies are signature-only under the %d-token budget; every candidate location remains listed.\n", budget)
	}
	return b.String()
}

// exploreCodeDefinitionKind reports whether a node kind is a code
// definition a developer edits to resolve a report. Non-code graph
// nodes (doc sections, packages, resources, contracts, ...) can rank —
// they are demoted to the fallback pool alongside test symbols rather
// than dropped, so a genuinely docs-shaped task still reaches them.
func exploreCodeDefinitionKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindField, graph.KindConstant,
		graph.KindVariable, graph.KindEnumMember, graph.KindMacro:
		return true
	default:
		return false
	}
}

// exploreDataDefinitionKind identifies leaf declarations whose repeated
// generic names carry little extra localization information. These remain real
// edit targets: the first/highest-ranked occurrence stays in place, and every
// occurrence stays available below the diversified head.
func exploreDataDefinitionKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindField, graph.KindConstant, graph.KindVariable, graph.KindEnumMember:
		return true
	default:
		return false
	}
}

func exploreShouldDiversifyByFile(class rerank.QueryClass) bool {
	return class == rerank.QueryClassConcept
}

// exploreCandidateFetchLimit leaves enough headroom for concept localization
// to survive generic exact-name collisions and non-localizable graph nodes
// before filtering and diversification. Literal identifier/path queries keep
// the tighter historical window because their exact ordering is the intent.
func exploreCandidateFetchLimit(maxSymbols int, class rerank.QueryClass) int {
	factor := 4
	if class == rerank.QueryClassConcept {
		factor = 8
	}
	return clampInt(maxSymbols*factor, maxSymbols, 80)
}

// diversifyRepeatedExploreNames performs stable, bounded name diversification
// for concept queries. One data declaration and two callable/type definitions
// per normalized name remain in the head; every overflow candidate is retained
// behind distinct concepts. Keeping two callables preserves useful overload or
// receiver families without letting a common method such as Validate consume
// the whole neighborhood.
func diversifyRepeatedExploreNames(cands []*rerank.Candidate, class rerank.QueryClass) []*rerank.Candidate {
	if class != rerank.QueryClassConcept || len(cands) < 2 {
		return cands
	}
	seen := make(map[string]int, len(cands))
	duplicates := make([]*rerank.Candidate, 0)
	head := cands[:0]
	for _, cand := range cands {
		if cand == nil || cand.Node == nil {
			head = append(head, cand)
			continue
		}
		name := strings.ToLower(strings.TrimSpace(cand.Node.Name))
		if name == "" {
			head = append(head, cand)
			continue
		}
		limit := 2
		if exploreDataDefinitionKind(cand.Node.Kind) {
			limit = 1
		}
		if seen[name] >= limit {
			duplicates = append(duplicates, cand)
			continue
		}
		seen[name]++
		head = append(head, cand)
	}
	return append(head, duplicates...)
}

// Localization member tiers order one file's own candidate slots. Extracted
// members inherit their owner type's name terms and win callable tie-breaks, so
// without an explicit tier they seat above the sibling declaration a task is
// actually about.
const (
	localizationMemberTierBehavioral = iota
	localizationMemberTierConstructor
	localizationMemberTierData
	localizationMemberTierCount
)

// exploreConstructorSpelledName reports the cross-language spellings of a
// declaration named by its keyword rather than by intent: `<Type>.<init>`
// (Java, C#, Swift) and Swift's bare `deinit` / `subscript`. The comparison is
// on the bare name after the last dot, so both the qualified and the bare
// spelling classify identically.
func exploreConstructorSpelledName(name string) bool {
	bare := name
	if dot := strings.LastIndexByte(bare, '.'); dot >= 0 {
		bare = bare[dot+1:]
	}
	switch bare {
	case "<init>", "deinit", "subscript":
		return true
	}
	return false
}

// localizationMemberTier classifies one candidate for per-file ordering. Kinds
// outside the classification stay in the leading tier so ranking they already
// earned is preserved; types lead alongside ordinary callables because a type
// declaration is the owner a task names, not a member of one.
func localizationMemberTier(n *graph.Node) int {
	switch n.Kind {
	case graph.KindFunction, graph.KindMethod:
		if exploreConstructorSpelledName(n.Name) {
			return localizationMemberTierConstructor
		}
		return localizationMemberTierBehavioral
	case graph.KindField, graph.KindEnumMember:
		return localizationMemberTierData
	default:
		return localizationMemberTierBehavioral
	}
}

// demoteLocalizationFileMembers reorders localization candidates inside the
// index positions each file already occupies. Nothing is dropped, added, or
// moved across files: a file's tiered candidates are written back into that
// file's own slots in slot order, so the window keeps the same members at the
// same count and every upstream reservation — including the typed-anchor
// projection's field — survives. Relative order inside a tier is preserved, so
// the reorder is stable and idempotent. Candidates without a node or a file
// path cannot be attributed to a file and keep their position.
func demoteLocalizationFileMembers(cands []*rerank.Candidate) []*rerank.Candidate {
	if len(cands) < 2 {
		return cands
	}
	slots := make(map[string][]int, len(cands))
	files := make([]string, 0, len(cands))
	demotable := false
	for i, cand := range cands {
		if cand == nil || cand.Node == nil || cand.Node.FilePath == "" {
			continue
		}
		key := cand.Node.RepoPrefix + "\x00" + cand.Node.FilePath
		if _, ok := slots[key]; !ok {
			files = append(files, key)
		}
		slots[key] = append(slots[key], i)
		if localizationMemberTier(cand.Node) != localizationMemberTierBehavioral {
			demotable = true
		}
	}
	if !demotable {
		return cands
	}
	out := append([]*rerank.Candidate(nil), cands...)
	for _, key := range files {
		positions := slots[key]
		if len(positions) < 2 {
			continue
		}
		var tiers [localizationMemberTierCount][]*rerank.Candidate
		for _, position := range positions {
			cand := cands[position]
			tier := localizationMemberTier(cand.Node)
			tiers[tier] = append(tiers[tier], cand)
		}
		written := 0
		for _, tier := range tiers {
			for _, cand := range tier {
				out[positions[written]] = cand
				written++
			}
		}
	}
	return out
}

// localizationSiblingLiftReach bounds "comparable standing" to the tail
// immediately past the cut: a sibling that lost by a wide margin lost on its own
// merits, and lifting it would be a ranking change rather than a tie-break.
const localizationSiblingLiftReach = 8

// localizationMemberSlotGuard decides which member candidates may not be traded
// out of the final window. Beyond the reserved-seat rule the projection uses,
// this boundary also protects the inputs of the lanes that run after the cut:
// dropping one of those is not a reordering, it silently disables a proof.
type localizationMemberSlotGuard struct {
	task    string
	ids     map[string]struct{}
	anchors []exploreSyntacticAnchor
	active  []int
}

func newLocalizationMemberSlotGuard(task string, protectedAnchors map[int]string) localizationMemberSlotGuard {
	guard := localizationMemberSlotGuard{task: task, ids: make(map[string]struct{}, len(protectedAnchors))}
	for _, id := range protectedAnchors {
		if id != "" {
			guard.ids[id] = struct{}{}
		}
	}
	if len(protectedAnchors) > 0 {
		guard.anchors = exploreSyntacticAnchors(task)
		guard.active = exploreTypedAnchorActiveIndexes(guard.anchors, protectedAnchors)
	}
	return guard
}

// protects keeps a member's slot when a proof lane identified it by something no
// member inherits from its owner type — the task's own literal text, an exact
// cited range, a graph proof, or the task naming that exact identifier — or when
// the candidate is a typed field the anchor projection may still lower into its
// owner/consumer/member proof.
func (g localizationMemberSlotGuard) protects(candidate *rerank.Candidate) bool {
	if candidate == nil || candidate.Node == nil {
		return true
	}
	if _, anchored := g.ids[candidate.Node.ID]; anchored {
		return true
	}
	for _, signal := range []string{
		exploreSourceRangeSignal, exploreContentRecallExactSignal,
		exploreSourceLiteralSignal, exploreSourceLiteralCalleeSignal,
		exploreSourceLiteralTaskAlignSignal, exploreTypedAnchorProjectionSignal,
		exploreSyntacticAnchorSignal,
	} {
		if candidate.Signals[signal] > 0 {
			return true
		}
	}
	if candidate.Node.Kind == graph.KindField && len(g.active) > 0 &&
		len(exploreTypedAnchorMatchingIndexes(
			g.anchors, g.active, candidate.Node, exploreTypedAnchorFieldType(candidate.Node),
		)) > 0 {
		return true
	}
	return localizationDirectAdjacencyNodeTaskCitationOffset(g.task, candidate.Node) >= 0
}

// liftLocalizationSiblingCallables trades a window slot a member won back to the
// ordinary sibling declaration it displaced. A file's slot count is fixed: the
// two candidates always come from the same file, one inside the cut and one in
// the bounded tail just past it, so no file gains or loses a slot and no
// cross-file allocation moves. The trade is a strict no-op when the file has no
// ordinary sibling in that tail — an unretrieved sibling cannot be selected, and
// this lane never widens retrieval to look for one.
func liftLocalizationSiblingCallables(
	task string,
	candidates []*rerank.Candidate,
	maxSymbols int,
	protectedAnchors map[int]string,
) []*rerank.Candidate {
	if maxSymbols <= 0 || len(candidates) <= maxSymbols {
		return candidates
	}
	guard := newLocalizationMemberSlotGuard(task, protectedAnchors)
	reach := min(len(candidates), maxSymbols+localizationSiblingLiftReach)
	lifted := candidates
	copied := false
	traded := make(map[int]struct{}, maxSymbols)
	for index := 0; index < maxSymbols; index++ {
		candidate := lifted[index]
		if candidate == nil || candidate.Node == nil || candidate.Node.FilePath == "" {
			continue
		}
		if localizationMemberTier(candidate.Node) == localizationMemberTierBehavioral {
			continue
		}
		if guard.protects(candidate) {
			continue
		}
		sibling := -1
		for below := maxSymbols; below < reach; below++ {
			if _, used := traded[below]; used {
				continue
			}
			other := lifted[below]
			if other == nil || other.Node == nil {
				continue
			}
			if other.Node.FilePath != candidate.Node.FilePath ||
				other.Node.RepoPrefix != candidate.Node.RepoPrefix {
				continue
			}
			if localizationMemberTier(other.Node) != localizationMemberTierBehavioral {
				continue
			}
			sibling = below
			break
		}
		if sibling < 0 {
			continue
		}
		if !copied {
			lifted = append([]*rerank.Candidate(nil), candidates...)
			copied = true
		}
		lifted[index], lifted[sibling] = lifted[sibling], lifted[index]
		traded[sibling] = struct{}{}
	}
	return lifted
}

// localizationEvidenceSeatReserved reports whether an evidence row owes its seat
// to a proof lane or to the task naming that exact identifier, rather than to
// ranking. Such a row keeps its exact position: the reservation is a contract,
// and presentation tiering is never allowed to overrule proven or explicitly
// named evidence. Citation is deliberately identifier-level — a task that names
// the file, or the type whose name every member inherits, has not asked for the
// constructor over the method.
func localizationEvidenceSeatReserved(task, requiredID string, target exploreTarget) bool {
	if target.node == nil {
		return true
	}
	if requiredID != "" && target.node.ID == requiredID {
		return true
	}
	// Proof lanes: the task's own literal text, an exact cited range, or a graph
	// proof. Each identifies a row by something a member cannot inherit.
	if target.sourceRange || target.divergentDefaultOwner || target.divergentDefaultType ||
		target.typedAnchorProjection || target.sourceLiteral || target.sourceLiteralCallee ||
		target.sourceLiteralAligned || target.literalPrimaryEligible || target.exactContent ||
		target.causalChangeOwner || target.causalChangeBridge || target.causalChangeLeaf ||
		target.localizationRelation != "" {
		return true
	}
	if localizationDirectAdjacencyNodeTaskCitationOffset(task, target.node) >= 0 {
		return true
	}
	// The name-term lanes are the ones a member rides in on: a constructor or a
	// field matches the task's wording only because its identifier carries the
	// owner type's terms. They hold a seat for declarations named by intent.
	return localizationMemberTier(target.node) == localizationMemberTierBehavioral &&
		(target.conceptImplementation || target.conceptComplement || target.syntacticAnchor)
}

// demoteLocalizationEvidenceMembers applies the same member tiers to the
// projection the draft already selected. Owner folding and draft ranking can
// re-seat a member the ranked window demoted, because both read the owner type's
// name terms off the member's own name. The reorder changes no selection: it
// permutes only rows the draft chose, only inside the slots one file already
// holds, and only among rows no lane reserved — so a constructor-spelled or data
// member yields its seat exactly when an ordinary sibling callable from the same
// file is already on the page, and otherwise keeps it.
func demoteLocalizationEvidenceMembers(task, requiredID string, targets []exploreTarget) []exploreTarget {
	if len(targets) < 2 {
		return targets
	}
	slots := make(map[string][]int, len(targets))
	files := make([]string, 0, len(targets))
	demotable := false
	for index, target := range targets {
		if localizationEvidenceSeatReserved(task, requiredID, target) || target.node.FilePath == "" {
			continue
		}
		key := target.node.RepoPrefix + "\x00" + target.node.FilePath
		if _, ok := slots[key]; !ok {
			files = append(files, key)
		}
		slots[key] = append(slots[key], index)
		if localizationMemberTier(target.node) != localizationMemberTierBehavioral {
			demotable = true
		}
	}
	if !demotable {
		return targets
	}
	out := append([]exploreTarget(nil), targets...)
	for _, key := range files {
		positions := slots[key]
		if len(positions) < 2 {
			continue
		}
		var tiers [localizationMemberTierCount][]exploreTarget
		for _, position := range positions {
			tier := localizationMemberTier(targets[position].node)
			tiers[tier] = append(tiers[tier], targets[position])
		}
		written := 0
		for _, tier := range tiers {
			for _, target := range tier {
				out[positions[written]] = target
				written++
			}
		}
	}
	return out
}

// exploreLocalizableKind reports whether a node kind is a place a
// developer would actually edit to resolve a report — the localization
// targets. Params, locals, closures, generic params, imports and file
// nodes are structurally never edit targets, so they are dropped from
// both the ranked candidate set and the call-path rings.
func exploreLocalizableKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindParam, graph.KindLocal, graph.KindClosure,
		graph.KindGenericParam, graph.KindImport, graph.KindFile:
		return false
	default:
		return true
	}
}

// ringNeighbors filters a traversal result's nodes to real neighbors (not
// the focus node itself, not param/local/import noise), capped.
func ringNeighbors(nodes []*graph.Node, selfID string, cap int) []*graph.Node {
	out, _ := ringNeighborsProjection(nodes, selfID, cap)
	return out
}

// ringNeighborsProjection also reports whether every eligible neighbor fit in
// the projection. Callers can reject uniqueness claims made from a saturated
// ring without issuing another graph query.
func ringNeighborsProjection(nodes []*graph.Node, selfID string, cap int) ([]*graph.Node, bool) {
	if cap < 1 {
		return nil, false
	}
	out := make([]*graph.Node, 0, cap)
	complete := true
	for _, n := range nodes {
		if n == nil || n.ID == selfID || !exploreLocalizableKind(n.Kind) {
			continue
		}
		if len(out) >= cap {
			complete = false
			continue
		}
		out = append(out, n)
	}
	return out, complete
}

// joinNeighbors renders a neighbor ring as "name (path:line), name (path:line)".
func joinNeighbors(nodes []*graph.Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, fmt.Sprintf("%s (%s)", n.Name, nodeLoc(n)))
	}
	return strings.Join(parts, ", ")
}

// nodeLoc is the citeable "path:startLine-endLine" (or "path:line") location.
func nodeLoc(n *graph.Node) string {
	path := nodeDisplayPath(n)
	if n.EndLine > n.StartLine {
		return fmt.Sprintf("%s:%d-%d", path, n.StartLine, n.EndLine)
	}
	if n.StartLine > 0 {
		return fmt.Sprintf("%s:%d", path, n.StartLine)
	}
	return path
}

// nodeDisplayPath is the repo-relative file path (the scorer's suffix-match
// target and the agent's citeable path).
func nodeDisplayPath(n *graph.Node) string {
	if n.FilePath != "" {
		return n.FilePath
	}
	return n.AbsoluteFilePath
}

// fenceLang maps a node language to a Markdown fence label (best-effort).
func fenceLang(lang string) string {
	if lang == "" {
		return ""
	}
	return lang
}

// stripLeadingExploreDirective removes generic agent-task verbs from the start
// of a multi-word concept query. These verbs describe what the agent should do,
// not the subsystem being localized; letting an exact symbol named Audit or
// Validate own that token can otherwise dominate every domain term. Identifier
// and short queries are left untouched by the caller/query-length guard.
func stripLeadingExploreDirective(query string) string {
	fields := strings.Fields(query)
	if len(fields) < 4 {
		return query
	}
	word := func(s string) string {
		return strings.ToLower(strings.Trim(s, "\"'`.,;:()[]{}<>!?—-"))
	}
	fillers := map[string]struct{}{
		"please": {}, "can": {}, "could": {}, "would": {}, "you": {}, "help": {}, "me": {}, "to": {},
	}
	directives := map[string]struct{}{
		"audit": {}, "check": {}, "diagnose": {}, "explain": {}, "find": {}, "fix": {},
		"implement": {}, "improve": {}, "investigate": {}, "review": {}, "trace": {},
		"understand": {}, "update": {}, "validate": {},
	}
	i := 0
	for i < len(fields) && i < 4 {
		if _, ok := fillers[word(fields[i])]; !ok {
			break
		}
		i++
	}
	if i >= len(fields) {
		return query
	}
	if _, ok := directives[word(fields[i])]; !ok {
		return query
	}
	i++
	if i < len(fields) && (word(fields[i]) == "and" || word(fields[i]) == "then") {
		i++
		if i < len(fields) {
			if _, ok := directives[word(fields[i])]; ok {
				i++
			}
		}
	}
	if len(fields)-i < 2 {
		return query
	}
	return strings.Join(fields[i:], " ")
}

// Query-shaping tuning. Generic structural thresholds — no corpus-specific
// vocabulary or benchmark-derived terms.
const (
	// shapeMinReportChars is the size above which a multi-line task is
	// treated as a pasted report worth distilling. Below it (or single-line)
	// the task is a focused query and passes through untouched.
	shapeMinReportChars = 300
	// shapeBodyMaxRunes bounds the distilled body so its bulk cannot re-drown
	// the weighted lead under BM25 length normalisation.
	shapeBodyMaxRunes = 400
	// shapeMinLineWords drops lines shorter than this many words — the
	// environment answers ("14.1.0", "Cargo", "macOS 26.5") and one-word
	// section headers a report body is padded with.
	shapeMinLineWords = 4
)

var (
	// A fenced code block: repro commands, log dumps, stack traces, sample
	// code. High-noise for LOCALIZATION (it names the invocation, not the
	// fix site), so it is removed before the body is distilled.
	reFenceBlock = regexp.MustCompile("(?s)```.*?```")
	reInlineCode = regexp.MustCompile("`[^`]*`")
	reURL        = regexp.MustCompile(`https?://\S+`)
	// Collapse runs of whitespace (incl. newlines) to single spaces.
	reWhitespace = regexp.MustCompile(`\s+`)
)

// shapeExploreQuery distils a pasted issue/report into the query that best
// localizes it, using only markdown/text structure — no vocabulary, no
// language model, nothing derived from any task set.
//
// The problem it solves: an agent commonly pastes a whole issue as the task —
// a one-line title followed by a body of repro commands, stack traces,
// environment tables and issue-template prompts. Fed raw to BM25 the body's
// command-line flags and log lines out-weigh the single defect sentence and
// pull ranking toward the flag-definition / entry-point files rather than the
// fix site. The fix is structural de-noising:
//
//   - the first non-empty line is the lead (a report's headline) and is
//     repeated once so its high-signal tokens are not drowned by the body;
//   - fenced code blocks, inline code and URLs are dropped from the body;
//   - body lines that are markdown headers/quotes, issue-template prompts
//     (they end in "?") or too short to be prose (environment answers, section
//     labels) are dropped;
//   - the surviving prose is bounded so it cannot re-drown the lead.
//
// A single-line (or short multi-line) task takes the inline path instead:
// structural noise tokens — commit-SHA-shaped hex, bare version strings —
// are dropped (they can never name a code symbol) and, when the query has
// clause structure, its lead clause is repeated so the headline's tokens
// out-weigh the trailing detail. A clean prose/identifier query with no
// noise tokens is returned byte-for-byte unchanged.
func shapeExploreQuery(task string) string {
	trimmed := strings.TrimSpace(task)
	// Focused / inline query: single line, or too short to carry a report
	// body worth distilling. Token-level shaping only, gated on the
	// presence of structural noise; a clean query passes through
	// untouched.
	if !strings.ContainsAny(trimmed, "\n\r") || len(trimmed) < shapeMinReportChars {
		return shapeInlineQuery(task)
	}

	// Lead = the first non-empty line (the report's headline).
	lead := ""
	rest := trimmed
	for _, ln := range strings.Split(trimmed, "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lead = s
			if idx := strings.Index(trimmed, ln); idx >= 0 {
				rest = trimmed[idx+len(ln):]
			}
			break
		}
	}

	body := shapeReportBody(rest)
	// Weight the lead by repeating it once, then append the distilled body.
	shaped := lead + ". " + lead
	if body != "" {
		shaped += ". " + body
	}
	return strings.TrimSpace(reWhitespace.ReplaceAllString(shaped, " "))
}

// shapeReportBody strips code / URLs and boilerplate lines from a report body
// and returns the surviving prose, whitespace-collapsed and rune-bounded.
func shapeReportBody(body string) string {
	body = reFenceBlock.ReplaceAllString(body, " ")
	body = reInlineCode.ReplaceAllString(body, " ")
	body = reURL.ReplaceAllString(body, " ")
	var keep []string
	for _, ln := range strings.Split(body, "\n") {
		s := strings.TrimSpace(ln)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "#") || strings.HasPrefix(s, ">") {
			continue // markdown header / block quote
		}
		if strings.HasSuffix(s, "?") {
			continue // issue-template prompt ("What version are you using?")
		}
		if len(strings.Fields(s)) < shapeMinLineWords {
			continue // environment answer / one-word section label
		}
		keep = append(keep, s)
	}
	prose := reWhitespace.ReplaceAllString(strings.Join(keep, " "), " ")
	prose = strings.TrimSpace(prose)
	if r := []rune(prose); len(r) > shapeBodyMaxRunes {
		prose = strings.TrimSpace(string(r[:shapeBodyMaxRunes]))
	}
	return prose
}

// Inline (single-line) query shaping. An agent that paraphrases a report
// into one line carries the report's noise with it — command-line flag
// tokens, quoted pattern/regex literals, commit-SHA hex, bare version
// strings. Those token classes are structurally detectable with no
// vocabulary, and their presence marks the query as report-derived
// rather than hand-focused, so it is worth shaping. Measured behavior on
// report-derived queries drove the transform's shape:
//
//   - commit-SHA-shaped hex and bare version strings are DROPPED — they
//     can never name a code symbol, so they are pure ranking noise;
//   - the lead clause (text before the first ";", " - " or ": "
//     separator) is REPEATED once — the same headline-weighting the
//     report path applies to a title, since a paraphrase puts the defect
//     statement first and trailing detail after a separator;
//   - flag tokens and quoted literals are left VERBATIM: the FTS
//     tokenizer already reads "--word" as the bare word, and measurement
//     showed that removing or down-weighting them costs rank (a flag
//     name carries the feature vocabulary; a quoted literal can carry
//     the only discriminating token) — they serve as the trigger, not
//     as targets.
//
// A query with none of these token classes is returned byte-for-byte
// unchanged.
const (
	// shapeInlineMinLeadChars is the minimum length for a lead clause to
	// be worth repeating — below it the "clause" is a sentence fragment
	// whose repetition would over-weight one or two words.
	shapeInlineMinLeadChars = 20
	// A long single-line paraphrase with sentence structure is report-like even
	// when it contains no flags or hashes. Short focused queries stay byte-for-
	// byte unchanged.
	shapeInlineLongQueryChars = 180
	// shaMinHexLen / shaMaxHexLen bound a commit-SHA-shaped token: git
	// abbreviates to >=7 hex chars; a full SHA-1 is 40.
	shaMinHexLen = 7
	shaMaxHexLen = 40
)

var (
	// A command-line flag token: "--long-flag" or a lone "-x", token-
	// initial (start or whitespace) so hyphenated prose ("case-insensitive",
	// "re-searches") never matches.
	reInlineFlag = regexp.MustCompile(`(?:^|\s)(?:--[A-Za-z][A-Za-z0-9_-]*|-[A-Za-z])(?:[\s,.;:]|$)`)
	// A hex run in the SHA length band. Candidates are verified in code
	// (must contain both a digit and a hex letter) because RE2 has no
	// lookahead — that check keeps decimal numbers (issue ids) and
	// letter-only words out.
	reInlineHexRun = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	// A bare version string: 1.9.0, v2.14.1, 14.1 — dotted digits with an
	// optional leading v. Never a symbol name.
	reInlineVersion = regexp.MustCompile(`\bv?\d+\.\d+(?:\.\d+)*\b`)
	// A double-quoted span on one line ("e.x|ex", "slice index ...").
	reInlineQuoted = regexp.MustCompile(`"[^"\n]*"`)
)

// shapeInlineQuery is the single-line/short-query arm of
// shapeExploreQuery: drop provably-inert tokens and weight the lead
// clause, gated on structural noise so a clean query is untouched.
func shapeInlineQuery(task string) string {
	noisy := hasInlineNoise(task)
	lead := inlineLeadClause(task)
	if !noisy && (len(task) < shapeInlineLongQueryChars || lead == "") {
		return task
	}
	cleaned := task
	if noisy {
		cleaned = dropInertTokens(task)
		lead = inlineLeadClause(cleaned)
	}
	if lead != "" {
		cleaned += " " + lead
	}
	return cleaned
}

// hasInlineNoise reports whether the query carries any structurally-
// detectable report noise: a flag token, a commit-SHA-shaped hex token,
// a bare version string, or a quoted pattern/regex literal.
func hasInlineNoise(task string) bool {
	if reInlineFlag.MatchString(task) || reInlineVersion.MatchString(task) {
		return true
	}
	for _, m := range reInlineHexRun.FindAllString(task, -1) {
		if isSHAToken(m) {
			return true
		}
	}
	for _, m := range reInlineQuoted.FindAllString(task, -1) {
		if quotedLiteralIsNoise(strings.Trim(m, `"`)) {
			return true
		}
	}
	return false
}

// isSHAToken verifies a hex-run candidate looks like a commit hash: the
// SHA length band plus at least one digit AND one hex letter, so a
// decimal number (an issue id) or a letter-only word never qualifies.
func isSHAToken(s string) bool {
	if len(s) < shaMinHexLen || len(s) > shaMaxHexLen {
		return false
	}
	hasDigit, hasLetter := false, false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'f':
			hasLetter = true
		}
	}
	return hasDigit && hasLetter
}

// quotedLiteralIsNoise classifies a quoted span's content: regex/pattern
// literals (they carry regex metacharacters, or are short non-alphabetic
// fragments like "e-x") are noise; a quoted plain word or prose phrase
// ("setState", an error message) is signal and never triggers shaping.
func quotedLiteralIsNoise(content string) bool {
	if strings.ContainsAny(content, `|\^$*+?[]{}()<>/=~`) {
		return true
	}
	if len(content) <= 5 {
		for _, r := range content {
			if !unicodeIsLetter(r) {
				return true
			}
		}
	}
	return false
}

// unicodeIsLetter uses Unicode categories so quoted identifiers and locale
// values remain useful retrieval anchors in every indexed language.
func unicodeIsLetter(r rune) bool {
	return unicode.IsLetter(r)
}

func exploreLiteralWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// exploreTextHasExactLiteral distinguishes a true quoted-literal occurrence
// from content_fts prefix recall (for example, `ku` must not be considered an
// exact hit merely because the body contains `kubernetes`). SearchContent
// snippets are unhighlighted, so a bounded case-folded substring scan with
// Unicode-aware word boundaries is sufficient and does not read source again.
func exploreTextHasExactLiteral(text, literal string) bool {
	return exploreLowerTextHasExactLiteral(strings.ToLower(text), literal)
}

// exploreLowerTextHasExactLiteral is the allocation-aware sibling for bounded
// scans that test many declaration names against one already-lowercased body.
func exploreLowerTextHasExactLiteral(lowerText, literal string) bool {
	literal = strings.ToLower(strings.TrimSpace(literal))
	if lowerText == "" || literal == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(literal)
	last, _ := utf8.DecodeLastRuneInString(literal)
	for offset := 0; offset <= len(lowerText)-len(literal); {
		relative := strings.Index(lowerText[offset:], literal)
		if relative < 0 {
			return false
		}
		start := offset + relative
		end := start + len(literal)
		beforeOK := true
		if start > 0 && exploreLiteralWordRune(first) {
			previous, _ := utf8.DecodeLastRuneInString(lowerText[:start])
			beforeOK = !exploreLiteralWordRune(previous)
		}
		afterOK := true
		if end < len(lowerText) && exploreLiteralWordRune(last) {
			next, _ := utf8.DecodeRuneInString(lowerText[end:])
			afterOK = !exploreLiteralWordRune(next)
		}
		if beforeOK && afterOK {
			return true
		}
		offset = start + 1
	}
	return false
}

// dropInertTokens removes commit-SHA-shaped hex and bare version tokens
// and collapses the leftover whitespace. Nothing else is touched.
func dropInertTokens(task string) string {
	out := reInlineHexRun.ReplaceAllStringFunc(task, func(m string) string {
		if isSHAToken(m) {
			return ""
		}
		return m
	})
	out = reInlineVersion.ReplaceAllString(out, "")
	return strings.TrimSpace(reWhitespace.ReplaceAllString(out, " "))
}

// inlineLeadClause returns the query's lead clause — the text before the
// first ";", " - " or ": " separator (":" only when single, so a
// namespaced identifier's "::" never splits) — when that lead is a
// proper, non-trivial prefix of the query. Empty when the query has no
// clause structure worth weighting.
func inlineLeadClause(task string) string {
	t := strings.TrimSpace(task)
	end := -1
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case ';':
			end = i
		case '-':
			// " - " — a spaced dash, not a hyphen or a flag.
			if i > 0 && t[i-1] == ' ' && i+1 < len(t) && t[i+1] == ' ' {
				end = i
			}
		case ':':
			// ": " with a non-colon before it — "walk: a scoped" splits,
			// "ignore::WalkBuilder" does not.
			if i > 0 && t[i-1] != ':' && t[i-1] != ' ' && i+1 < len(t) && t[i+1] == ' ' {
				end = i
			}
		case '.', '?', '!':
			// A sentence boundary in a long single-line report is the inline
			// equivalent of a title newline. Requiring following whitespace
			// avoids splitting .ignore, dotted identifiers, and quoted ".".
			if i+1 < len(t) && (t[i+1] == ' ' || t[i+1] == '\t') {
				end = i + 1
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return ""
	}
	lead := strings.TrimSpace(t[:end])
	if len(lead) < shapeInlineMinLeadChars || len(lead) >= len(t) {
		return ""
	}
	return lead
}

const (
	exploreContentRecallRankSignal      = "explore_content_rank"
	exploreContentRecallTermSignal      = "explore_content_terms"
	exploreContentRecallExactSignal     = "explore_content_exact"
	exploreContentRecallAmbiguousSignal = "explore_content_exact_ambiguous"
	exploreSourceLiteralSignal          = "explore_source_literal"
	exploreSourceLiteralCalleeSignal    = "explore_source_literal_callee"
	exploreSourceLiteralCoverageSignal  = "explore_source_literal_coverage"
	exploreSourceLiteralTaskAlignSignal = "explore_source_literal_task_alignment"
	exploreSourceLiteralReservationMax  = 2
	exploreQuotedRecallMaxTerms         = 6
	// Literals mined from code blocks may only take the slots prose left
	// unclaimed. Explicit and mined anchors share one fixed request budget.
	exploreQuotedRecallMaxMinedTerms = 6
	exploreQuotedRecallMaxPerTerm    = 24
	exploreQuotedRecallRetryMaxRows  = 24
)

// exploreQuotedRecallTerms extracts only explicit, high-signal literal anchors
// from prose, then the literals mined from the task's code blocks. The ordinary
// symbol corpus intentionally excludes function bodies; these literals are the
// bounded bridge to the existing source-content FTS for errors, configuration
// values, protocol names, and other evidence that exists only inside an
// implementation. Regex/pattern literals remain in the shaped symbol query but
// are not sent to content recall because their punctuation decomposes into
// noisy one-character terms.
func exploreQuotedRecallTerms(task string) []string {
	seen := make(map[string]struct{}, exploreQuotedRecallMaxMinedTerms)
	out := make([]string, 0, exploreQuotedRecallMaxMinedTerms)
	out = admitExploreQuotedRecallTerms(task, exploreProseQuotedLiterals(task), out, seen, exploreQuotedRecallMaxTerms)
	return admitExploreQuotedRecallTerms(
		task, exploreFencedRecallLiterals(task), out, seen, exploreQuotedRecallMaxMinedTerms,
	)
}

// exploreQuotedRecallClaimTerms returns only the literals the requester wrote
// as prose. Terminality gates key on the requester's factual claim about the
// source, never on a literal mined out of a pasted code block: mining is our
// own inference, and an inference that retrieves nothing must not cost the
// session a turn. Retrieval keeps using the full list.
func exploreQuotedRecallClaimTerms(task string) []string {
	return admitExploreQuotedRecallTerms(
		task, exploreProseQuotedLiterals(task),
		make([]string, 0, exploreQuotedRecallMaxTerms),
		make(map[string]struct{}, exploreQuotedRecallMaxTerms),
		exploreQuotedRecallMaxTerms,
	)
}

// exploreProseQuotedLiterals returns every quoted span of the raw task, in
// document order.
func exploreProseQuotedLiterals(task string) []string {
	literals := make([]string, 0, exploreQuotedRecallMaxTerms)
	for _, match := range reInlineQuoted.FindAllString(task, -1) {
		if len(match) >= 2 {
			literals = append(literals, match[1:len(match)-1])
		}
	}
	// Backticks are common in agent-written tasks for exact config values and
	// error fragments. Scan them without treating apostrophes in prose as quote
	// delimiters.
	for rest := task; ; {
		start := strings.IndexByte(rest, '`')
		if start < 0 {
			break
		}
		rest = rest[start+1:]
		end := strings.IndexByte(rest, '`')
		if end < 0 {
			break
		}
		literals = append(literals, rest[:end])
		rest = rest[end+1:]
	}
	return literals
}

// admitExploreQuotedRecallTerms applies the shared acceptance policy — length,
// noise, short-anchor intent, and case-folded deduplication — to one lane of
// literal candidates and stops at the lane's cumulative limit.
func admitExploreQuotedRecallTerms(
	task string,
	literals, out []string,
	seen map[string]struct{},
	limit int,
) []string {
	for _, literal := range literals {
		if len(out) >= limit {
			break
		}
		literal = strings.TrimSpace(literal)
		if len(literal) < 2 || len(literal) > 128 || quotedLiteralIsNoise(literal) {
			continue
		}
		// A span the opening delimiter never closed on its line is not a source
		// literal: content FTS cannot match text that crosses a line break, and
		// admitting it would spend a term slot the mined lines can use.
		if strings.ContainsAny(literal, "\r\n") {
			continue
		}
		if exploreTwoLetterQuotedAnchor(literal) && !exploreAllowsTwoLetterQuotedAnchor(task) {
			continue
		}
		key := strings.ToLower(literal)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, literal)
	}
	return out
}

func exploreTwoLetterQuotedAnchor(literal string) bool {
	if utf8.RuneCountInString(literal) != 2 {
		return false
	}
	for _, r := range literal {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// exploreAllowsTwoLetterQuotedAnchor keeps exceptionally collision-prone
// literals out of fallback source scans unless the request identifies them as
// a culture, locale, language, registry, protocol, or configuration value.
// This is task-class gating rather than a vocabulary of specific codes, so it
// applies equally to future repositories and languages.
func exploreAllowsTwoLetterQuotedAnchor(task string) bool {
	lower := strings.ToLower(task)
	for _, phrase := range []string{
		"configuration key", "config key", "country code", "culture code",
		"language code", "language tag", "locale code", "protocol code",
		"region code", "status code",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	for _, token := range strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}) {
		switch token {
		case "bcp47", "charset", "culture", "cultureinfo", "cultures", "currency", "encoding", "ietf", "iso", "locale", "locales", "register", "registered", "registering", "registration", "registrations", "registry":
			return true
		}
	}
	return false
}

func exploreQuotedRecallHasExactSourceCandidate(
	task string,
	terms []string,
	candidates []*rerank.Candidate,
	scope query.QueryOptions,
) bool {
	for _, candidate := range candidates {
		if candidate != nil && exploreQuotedRecallHasExactSourceNode(task, terms, candidate.Node, scope) {
			return true
		}
	}
	return false
}

// exploreQuotedRecallCompactTerms reports whether every requested term is a
// compact source value. Callers test coverage one term at a time, so this is
// effectively "the term under test is a compact value" — matched against
// declaration text only, never satisfied by a request-level anchor on an
// unrelated symbol.
func exploreQuotedRecallCompactTerms(terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		if !exploreCompactSourceLiteral(term, utf8.RuneCountInString(term)) {
			return false
		}
	}
	return true
}

func exploreQuotedRecallHasExactSourceNode(
	task string,
	terms []string,
	node *graph.Node,
	scope query.QueryOptions,
) bool {
	if node == nil || nodeDisplayPath(node) == "" || !scope.ScopeAllows(node) ||
		!exploreLocalizableKind(node.Kind) || !exploreCodeDefinitionKind(node.Kind) {
		return false
	}
	if exploreDraftIsTestNode(node) && !exploreQueryHasTestIntent(task) {
		return false
	}
	// Coverage means the term is visible in this node's own declaration text —
	// name, signature, or qualified name. That a candidate happens to be some
	// other anchor of the same request says nothing about where a quoted value
	// lives; treating it as coverage suppressed the one bounded lane able to
	// find the value's registration site on exactly the tasks that both quote
	// a literal and name a file.
	retrieval := node.RetrievalMetadata()
	for _, term := range terms {
		// Compact values such as locale and protocol codes are usually source
		// literals, not declaration identities. Trust an exact declaration name
		// or signature, but never a path-derived qualified name such as a test
		// namespace ending in `.ku`.
		fields := [...]string{node.Name, retrieval.Signature, retrieval.QualName}
		fieldCount := len(fields)
		if exploreCompactSourceLiteral(term, utf8.RuneCountInString(term)) {
			fieldCount--
		}
		for _, field := range fields[:fieldCount] {
			if exploreTextHasExactLiteral(field, term) {
				return true
			}
		}
	}
	return false
}

// exploreLocalizationTestLaneNode reports whether a ranked candidate belongs in
// the demoted lane. The indexer's is_test stamp is language-dependent and misses
// test assemblies whose only marker is a path token (a dotted `.Tests.` segment,
// a `spec/` directory), so the draft detector votes too — unless the request is
// about test code or names this candidate outright, in which case the test node
// is the answer and keeps its production slot.
func exploreLocalizationTestLaneCandidate(query string, candidate *rerank.Candidate) bool {
	if candidate == nil || candidate.Node == nil {
		return false
	}
	if candidate.Signals[exploreSourceRangeSignal] > 0 {
		return false
	}
	return exploreLocalizationTestLaneNode(query, candidate.Node)
}

func exploreLocalizationTestLaneNode(query string, node *graph.Node) bool {
	if node == nil {
		return false
	}
	if stamped, _ := node.Meta["is_test"].(bool); stamped {
		return true
	}
	if !exploreDraftIsTestNode(node) {
		return false
	}
	return !exploreQueryHasTestIntent(query) && !exploreLocalizationExplicitAnchor(query, node)
}

func exploreQueryHasTestIntent(task string) bool {
	for _, token := range rerank.Tokenize(task) {
		switch strings.ToLower(token) {
		case "test", "tests", "testing", "spec", "specs", "fixture", "fixtures":
			return true
		}
	}
	return false
}

func exploreSourceLiteralConstructionIntent(task string) bool {
	for _, token := range rerank.Tokenize(task) {
		switch strings.ToLower(token) {
		case "construct", "constructed", "constructing", "construction", "constructor", "constructors",
			"initialise", "initialised", "initialises", "initialising", "initialisation",
			"initialize", "initialized", "initializes", "initializing", "initialization",
			"instantiate", "instantiated", "instantiates", "instantiating", "instantiation":
			return true
		}
	}
	return false
}

// gatherExploreQuotedContentCandidates performs at most seven bounded content
// searches (six literals plus one adaptive retry). It scans source bodies only
// when neither the ordinary nor content channels already contain an exact,
// localizable code symbol. The fallback is request-local and never persists a
// source-body index.
func (s *Server) gatherExploreQuotedContentCandidates(
	ctx context.Context,
	task string,
	ordinary []*rerank.Candidate,
	limit int,
	scope query.QueryOptions,
) []*rerank.Candidate {
	return s.gatherExploreQuotedContentCandidatesCollecting(ctx, task, ordinary, limit, scope, nil)
}

func (s *Server) gatherExploreQuotedContentCandidatesCollecting(
	ctx context.Context,
	task string,
	ordinary []*rerank.Candidate,
	limit int,
	scope query.QueryOptions,
	collector *localizationSourceWindowHitCollector,
) []*rerank.Candidate {
	return s.gatherExploreContentCandidatesForTermsCollecting(
		ctx, task, exploreQuotedRecallTerms(task), ordinary, limit, scope, collector,
	)
}

// gatherExploreContentCandidatesForTerms is the shared bounded content path for
// authored quoted literals and the separately-gated inferred bare lane.
func (s *Server) gatherExploreContentCandidatesForTerms(
	ctx context.Context,
	task string,
	terms []string,
	ordinary []*rerank.Candidate,
	limit int,
	scope query.QueryOptions,
) []*rerank.Candidate {
	return s.gatherExploreContentCandidatesForTermsCollecting(ctx, task, terms, ordinary, limit, scope, nil)
}

func (s *Server) gatherExploreContentCandidatesForTermsCollecting(
	ctx context.Context,
	task string,
	terms []string,
	ordinary []*rerank.Candidate,
	limit int,
	scope query.QueryOptions,
	collector *localizationSourceWindowHitCollector,
) []*rerank.Candidate {
	if s == nil || ctx.Err() != nil {
		return nil
	}
	reader := s.readerFor(ctx)
	if reader == nil {
		return nil
	}
	// Durable content FTS remains the discovery backend. Overlay-owned files
	// are filtered below before any snippet can authenticate a candidate.
	content, hasContent := s.graph.(graph.ContentSearcher)
	perTerm := clampInt(limit/3, 4, exploreQuotedRecallMaxPerTerm)
	repoPrefix := ""
	if len(scope.RepoAllow) == 1 {
		for prefix, allowed := range scope.RepoAllow {
			if allowed {
				repoPrefix = prefix
			}
		}
	}
	if repoPrefix == "" {
		repoPrefix, _ = s.sessionLocality(ctx)
	}
	// A task that quotes nothing still names its subject: identifier-shaped
	// prose tokens feed the same bounded source-literal recall below, so an
	// unquoted issue is not blind to source bodies.
	bareTokens := exploreDistinctiveBareTokens(task, repoPrefix)
	if len(terms) == 0 && len(bareTokens) == 0 {
		return nil
	}

	type recallPage struct {
		term      string
		hits      []graph.ContentHit
		saturated bool
	}
	pages := make([]recallPage, 0, len(terms))
	for _, term := range terms {
		if !hasContent {
			break
		}
		if ctx.Err() != nil {
			break
		}
		hits, err := content.SearchContent(term, repoPrefix, perTerm)
		if err != nil {
			continue
		}
		saturated := len(hits) >= perTerm
		hits = s.filterOverlayContentHits(ctx, hits)
		pages = append(pages, recallPage{term: term, hits: hits, saturated: saturated})
	}

	// A short or collision-heavy literal can fill the first page before its
	// exact body appears. Retry one saturated page globally, prioritizing a page
	// with no visible exact peer and then shorter terms. Replacing (not merging)
	// the page keeps each term's evidence count idempotent.
	retry := -1
	retryPriority := -1
	for i := range pages {
		if !pages[i].saturated {
			continue
		}
		exactIDs := make(map[string]struct{})
		for _, hit := range pages[i].hits {
			if hit.NodeID != "" && exploreTextHasExactLiteral(hit.Snippet, pages[i].term) {
				exactIDs[hit.NodeID] = struct{}{}
			}
		}
		priority := 0
		if len(exactIDs) == 0 {
			priority += 4
		} else if len(exactIDs) > 1 {
			priority++
		}
		if len([]rune(pages[i].term)) <= 3 {
			priority += 2
		}
		if priority > retryPriority {
			retry, retryPriority = i, priority
		}
	}
	if retry >= 0 && ctx.Err() == nil {
		if hits, err := content.SearchContent(pages[retry].term, repoPrefix, exploreQuotedRecallRetryMaxRows); err == nil {
			pages[retry].saturated = len(hits) >= exploreQuotedRecallRetryMaxRows
			pages[retry].hits = s.filterOverlayContentHits(ctx, hits)
		}
	}

	order := make([]string, 0, len(pages)*perTerm)
	bestRank := make(map[string]int, len(pages)*perTerm)
	termCount := make(map[string]int, len(pages)*perTerm)
	exactHit := make(map[string]bool, len(pages)*perTerm)
	uniqueExact := make(map[string]bool, len(pages)*perTerm)
	ambiguousExact := make(map[string]bool, len(pages)*perTerm)
	sourceLiteralHit := make(map[string]float64)
	sourceLiteralAnchors := make(map[string]map[int]struct{})
	sourceLiteralAmbiguous := make(map[string]bool)
	sourceLiteralSettled := make(map[string]bool)
	sourceLiteralCallee := make(map[string]bool)
	sourceLiteralTaskAligned := make(map[string]bool)
	sourceLiteralCoordinates := make(map[string][]exploreSourceLiteralHit)
	for _, page := range pages {
		seenForTerm := make(map[string]struct{}, len(page.hits))
		exactIDs := make(map[string]struct{})
		for rank, hit := range page.hits {
			if hit.NodeID == "" {
				continue
			}
			if _, duplicate := seenForTerm[hit.NodeID]; duplicate {
				continue
			}
			seenForTerm[hit.NodeID] = struct{}{}
			if previous, exists := bestRank[hit.NodeID]; !exists {
				order = append(order, hit.NodeID)
				bestRank[hit.NodeID] = rank
			} else if rank < previous {
				bestRank[hit.NodeID] = rank
			}
			termCount[hit.NodeID]++
			if exploreTextHasExactLiteral(hit.Snippet, page.term) {
				exactIDs[hit.NodeID] = struct{}{}
			}
		}
		ambiguous := page.saturated || len(exactIDs) > 1
		for id := range exactIDs {
			exactHit[id] = true
			if ambiguous {
				ambiguousExact[id] = true
			} else {
				uniqueExact[id] = true
			}
		}
	}
	nodes := make(map[string]*graph.Node, len(order))
	if len(order) > 0 {
		nodes = reader.GetNodesByIDs(order)
	}
	// Decide source coverage per quoted term. An exact metadata hit for one
	// symbol-like term must not suppress a different compact value whose only
	// useful evidence is inside a registration body. The selected missing term
	// still feeds one bounded source scan, so this preserves the fixed I/O cap.
	sourceRecallTerms := make([]string, 0, len(terms)+len(bareTokens))
	seenRecallTerms := make(map[string]struct{}, len(terms)+len(bareTokens))
	appendRecallTerm := func(term string) {
		key := strings.ToLower(term)
		if _, duplicate := seenRecallTerms[key]; duplicate {
			return
		}
		seenRecallTerms[key] = struct{}{}
		oneTerm := []string{term}
		exactSourceFound := exploreQuotedRecallHasExactSourceCandidate(task, oneTerm, ordinary, scope)
		if !exactSourceFound {
			for _, id := range order {
				if exploreQuotedRecallHasExactSourceNode(task, oneTerm, nodes[id], scope) {
					exactSourceFound = true
					break
				}
			}
		}
		if !exactSourceFound {
			sourceRecallTerms = append(sourceRecallTerms, term)
		}
	}
	for _, term := range terms {
		appendRecallTerm(term)
	}
	// Quoted values keep priority in the bounded term budget; mined tokens
	// fill behind them and are subject to the same declaration-coverage test,
	// so a token already visible in ranked metadata never triggers a scan.
	for _, term := range bareTokens {
		if len(sourceRecallTerms) >= exploreSourceLiteralRecallMaxTerms+2 {
			break
		}
		appendRecallTerm(term)
	}

	// content_fts stores content-class nodes rather than ordinary source bodies.
	// An exact document hit therefore does not prove that the source declaration
	// containing the literal is represented. Only a per-term miss across both
	// ordinary retrieval and hydrated content symbols activates the bounded scan.
	if ctx.Err() == nil && len(sourceRecallTerms) > 0 {
		sourceRecall := s.gatherExploreSourceLiteralRecall(ctx, sourceRecallTerms, repoPrefix, scope)
		missingNodes := make([]string, 0, len(sourceRecall.hits))
		for _, hit := range sourceRecall.hits {
			sourceLiteralCoordinates[hit.nodeID] = append(sourceLiteralCoordinates[hit.nodeID], hit)
			if previous, exists := bestRank[hit.nodeID]; !exists {
				order = append(order, hit.nodeID)
				bestRank[hit.nodeID] = hit.rank
				missingNodes = append(missingNodes, hit.nodeID)
			} else if hit.rank < previous {
				bestRank[hit.nodeID] = hit.rank
			}
			anchors := sourceLiteralAnchors[hit.nodeID]
			if anchors == nil {
				anchors = make(map[int]struct{}, exploreSourceLiteralRecallMaxTerms)
				sourceLiteralAnchors[hit.nodeID] = anchors
			}
			anchors[hit.anchor] = struct{}{}
			if termCount[hit.nodeID] < len(anchors) {
				termCount[hit.nodeID] = len(anchors)
			}
			exactHit[hit.nodeID] = true
			sourceRank := 1.0
			if hit.rank > 0 {
				sourceRank = 1 / float64(hit.rank+1)
			}
			if sourceRank > sourceLiteralHit[hit.nodeID] {
				sourceLiteralHit[hit.nodeID] = sourceRank
			}
			if hit.callee {
				sourceLiteralCallee[hit.nodeID] = true
			}
			if hit.ambiguous {
				sourceLiteralAmbiguous[hit.nodeID] = true
			} else {
				sourceLiteralSettled[hit.nodeID] = true
			}
		}
		for id := range sourceLiteralAnchors {
			if sourceLiteralAmbiguous[id] && !sourceLiteralSettled[id] {
				ambiguousExact[id] = true
			} else {
				uniqueExact[id] = true
			}
		}
		if len(missingNodes) > 0 {
			for id, node := range reader.GetNodesByIDs(missingNodes) {
				nodes[id] = node
			}
		}
		// A collision-heavy literal alone cannot justify replacing two semantic
		// candidates. When the task explicitly asks about construction, however,
		// an exact callsite whose resolved callable instantiates a value is a
		// language-neutral causal discriminator. One batched edge lookup covers
		// the already-bounded owner set; no source body or recursive walk is added.
		if exploreSourceLiteralConstructionIntent(task) {
			calleeIDs := make([]string, 0, len(sourceLiteralCallee))
			for id, callee := range sourceLiteralCallee {
				if callee {
					calleeIDs = append(calleeIDs, id)
				}
			}
			sourceLiteralTaskAligned = projectExploreSourceLiteralConstructionAlignment(
				ctx, reader, calleeIDs,
			)
		}
	}
	if len(order) == 0 {
		return nil
	}
	candidates := make([]*rerank.Candidate, 0, len(order))
	for _, id := range order {
		node := nodes[id]
		if node == nil || !scope.ScopeAllows(node) {
			continue
		}
		rank := bestRank[id]
		signals := map[string]float64{
			exploreContentRecallRankSignal: 1 / float64(rank+1),
			exploreContentRecallTermSignal: float64(termCount[id]),
		}
		if exactHit[id] {
			signals[exploreContentRecallExactSignal] = 1
			if ambiguousExact[id] && !uniqueExact[id] {
				signals[exploreContentRecallAmbiguousSignal] = 1
			}
		}
		if sourceRank := sourceLiteralHit[id]; sourceRank > 0 {
			collector.add(sourceLiteralCoordinates[id]...)
			signals[exploreSourceLiteralSignal] = sourceRank
			signals[exploreSourceLiteralCoverageSignal] = float64(len(sourceLiteralAnchors[id]))
			if sourceLiteralCallee[id] {
				signals[exploreSourceLiteralCalleeSignal] = 1
			}
			if sourceLiteralTaskAligned[id] {
				signals[exploreSourceLiteralTaskAlignSignal] = 1
			}
		}
		candidates = append(candidates, &rerank.Candidate{
			Node:       node,
			TextRank:   rank,
			VectorRank: -1,
			Signals:    signals,
		})
	}
	return candidates
}

// exploreConceptTermPresent recognizes a bounded family of inflectional forms
// when concept prose and source identifiers use different grammatical forms
// (for example matching versus matched_ignore). It never uses a raw stem
// substring: that would incorrectly equate building with Builder.
func exploreConceptTermPresent(text, term string) bool {
	if strings.Contains(text, term) {
		return true
	}
	for _, suffix := range [...]string{"ing", "ed"} {
		if !strings.HasSuffix(term, suffix) {
			continue
		}
		stem := strings.TrimSuffix(term, suffix)
		if len(stem) < 5 {
			continue
		}
		alternate := stem + "ing"
		if suffix == "ing" {
			alternate = stem + "ed"
		}
		if exploreConceptFormAtBoundary(text, alternate, false) ||
			exploreConceptFormAtBoundary(text, stem, true) {
			return true
		}
	}
	return false
}

func exploreConceptFormAtBoundary(text, form string, requireEnd bool) bool {
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], form)
		if index < 0 {
			return false
		}
		index += offset
		startOK := index == 0 || !exploreASCIIAlphaNumeric(text[index-1])
		end := index + len(form)
		endOK := !requireEnd || end == len(text) || !exploreASCIIAlphaNumeric(text[end])
		if startOK && endOK {
			return true
		}
		offset = index + 1
	}
	return false
}

func exploreASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

// rerankExploreConceptCoverage corrects the principal weakness of prefix-OR
// symbol retrieval: one rare identifier can otherwise outrank an implementation
// whose metadata covers several independent task concepts. Explicit anchors and
// bounded exact body-literal hits remain strongest. Vector-backed candidates
// retain their semantic ordering; coverage only reorders lexical-only rows.
func rerankExploreConceptCoverage(query string, candidates []*rerank.Candidate) []*rerank.Candidate {
	if exploreQueryClass(query) != rerank.QueryClassConcept || len(candidates) < 2 {
		return candidates
	}
	queryTerms := exploreTerminalTerms(query)
	if len(queryTerms) < 2 {
		return candidates
	}
	type evidence struct {
		text           string
		explicit       bool
		exactContent   bool
		ambiguousExact bool
		callable       bool
		generic        bool
		overlap        int
		contentTerms   float64
		contentRank    float64
	}
	metrics := make(map[*rerank.Candidate]evidence, len(candidates))
	termFrequency := make(map[string]int, len(queryTerms))
	candidateCount := 0
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil {
			continue
		}
		retrieval := candidate.Node.RetrievalMetadata()
		doc := retrieval.Doc
		if len(doc) > 768 {
			doc = doc[:768]
		}
		text := strings.ToLower(strings.Join([]string{
			candidate.Node.Name, retrieval.QualName, nodeDisplayPath(candidate.Node),
			retrieval.Signature, doc,
		}, " "))
		metrics[candidate] = evidence{text: text}
		candidateCount++
		for term := range queryTerms {
			if exploreConceptTermPresent(text, term) {
				termFrequency[term]++
			}
		}
	}
	// A task noun shared by more than a third of the retrieval window (path is
	// the common case) carries no discriminating evidence. Counting it as a
	// second concept turns every path wrapper into a strong semantic peer and
	// freezes the upstream rank order. Frequency is computed only inside the
	// already-bounded window, so this adds no graph or index work.
	maxFrequency := max(1, candidateCount/3)
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil {
			continue
		}
		metric := metrics[candidate]
		text := metric.text
		overlap := 0
		for term := range queryTerms {
			if frequency := termFrequency[term]; frequency > 0 && frequency <= maxFrequency &&
				exploreConceptTermPresent(text, term) {
				overlap++
			}
		}
		metric.explicit = exploreLocalizationExplicitAnchor(query, candidate.Node)
		metric.callable = candidate.Node.Kind == graph.KindFunction || candidate.Node.Kind == graph.KindMethod
		metric.generic = exploreDraftGenericCandidate(candidate.Node, "")
		metric.overlap = overlap
		if candidate.Signals != nil {
			metric.contentTerms = candidate.Signals[exploreContentRecallTermSignal]
			metric.contentRank = candidate.Signals[exploreContentRecallRankSignal]
			metric.exactContent = candidate.Signals[exploreContentRecallExactSignal] > 0
			metric.ambiguousExact = candidate.Signals[exploreContentRecallAmbiguousSignal] > 0
		}
		metrics[candidate] = metric
	}
	coverageTier := func(overlap int) int {
		switch {
		case overlap >= 4:
			return 3
		case overlap >= 2:
			return 2
		case overlap == 1:
			return 1
		default:
			return 0
		}
	}
	priorityTier := func(candidate *rerank.Candidate) int {
		metric := metrics[candidate]
		switch {
		case metric.explicit:
			return 3
		case metric.exactContent && !metric.ambiguousExact:
			return 2
		case metric.exactContent:
			return 1
		default:
			return 0
		}
	}
	strongConjunctive := func(candidate *rerank.Candidate) bool {
		metric := metrics[candidate]
		return metric.callable && !metric.generic && metric.overlap >= 2
	}
	weakCollision := func(candidate *rerank.Candidate) bool {
		metric := metrics[candidate]
		return metric.generic || metric.overlap <= 1
	}
	// Explicit anchors and verified full quoted literals may cross semantic
	// candidates. Unique exact evidence leads ambiguous pages; ambiguous exact
	// peers use task coverage before content-channel rank. Outside those proof
	// tiers, one concrete callable covering two independent concepts may cross a
	// generic one-token exact/vector collision, but never another multi-concept
	// semantic candidate.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		at, bt := priorityTier(a), priorityTier(b)
		if at != bt {
			return at > bt
		}
		am, bm := metrics[a], metrics[b]
		if at == 0 {
			return false
		}
		if at != 1 {
			return false
		}
		ac, bc := coverageTier(am.overlap), coverageTier(bm.overlap)
		if ac != bc {
			return ac > bc
		}
		if am.overlap != bm.overlap {
			return am.overlap > bm.overlap
		}
		if am.contentTerms != bm.contentTerms {
			return am.contentTerms > bm.contentTerms
		}
		return am.contentRank > bm.contentRank
	})
	// Within the ordinary evidence tier, promote a strong conjunctive callable
	// only across adjacent weak collisions. A multi-concept non-callable is a
	// barrier: crossing it would violate the promise that semantic peers retain
	// their upstream order. Expressing that barrier relation inside sort.Less is
	// non-transitive (strong~medium, medium~weak, strong<weak), so use this
	// explicit stable insertion pass instead.
	for index := 1; index < len(candidates); index++ {
		candidate := candidates[index]
		if priorityTier(candidate) != 0 || !strongConjunctive(candidate) {
			continue
		}
		insert := index
		for insert > 0 && priorityTier(candidates[insert-1]) == 0 && weakCollision(candidates[insert-1]) {
			candidates[insert] = candidates[insert-1]
			insert--
		}
		candidates[insert] = candidate
	}
	priorityEnd := 0
	for priorityEnd < len(candidates) && priorityTier(candidates[priorityEnd]) > 0 {
		priorityEnd++
	}
	positions := make([]int, 0, len(candidates)-priorityEnd)
	lexical := make([]*rerank.Candidate, 0, len(candidates)-priorityEnd)
	for i := priorityEnd; i < len(candidates); i++ {
		candidate := candidates[i]
		if candidate != nil && candidate.VectorRank < 0 {
			positions = append(positions, i)
			lexical = append(lexical, candidate)
		}
	}
	sort.SliceStable(lexical, func(i, j int) bool {
		a, b := lexical[i], lexical[j]
		am, bm := metrics[a], metrics[b]
		if am.contentTerms != bm.contentTerms {
			return am.contentTerms > bm.contentTerms
		}
		if am.contentRank != bm.contentRank {
			return am.contentRank > bm.contentRank
		}
		at, bt := coverageTier(am.overlap), coverageTier(bm.overlap)
		if at != bt {
			return at > bt
		}
		if am.overlap != bm.overlap {
			return am.overlap > bm.overlap
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return false
	})
	for i, position := range positions {
		candidates[position] = lexical[i]
	}
	return candidates
}

// mergeExploreCandidates unions retrieval passes by symbol ID without mutating
// either input. Expansion text ranks are offset so a reduced concept bag can
// add recall without claiming the same authority as the original query.
func mergeExploreCandidates(primary, expanded []*rerank.Candidate, expansionRankOffset int) []*rerank.Candidate {
	if expansionRankOffset < 0 {
		expansionRankOffset = 0
	}
	byID := make(map[string]*rerank.Candidate, len(primary)+len(expanded))
	out := make([]*rerank.Candidate, 0, len(primary)+len(expanded))
	add := func(candidate *rerank.Candidate, expansion bool) {
		if candidate == nil || candidate.Node == nil {
			return
		}
		clone := *candidate
		if expansion && clone.TextRank >= 0 {
			clone.TextRank += expansionRankOffset
		}
		if current, ok := byID[clone.Node.ID]; ok {
			// A primary lexical rank always wins. Expansion may add a weaker
			// lexical signal to a semantic-only primary candidate.
			if clone.TextRank >= 0 && current.TextRank < 0 {
				current.TextRank = clone.TextRank
			} else if !expansion && clone.TextRank >= 0 && clone.TextRank < current.TextRank {
				current.TextRank = clone.TextRank
			}
			if clone.VectorRank >= 0 && (current.VectorRank < 0 || clone.VectorRank < current.VectorRank) {
				current.VectorRank = clone.VectorRank
			}
			// Request-local recall annotations survive dedup when a source-body
			// hit names a symbol already present in the ordinary metadata channel.
			signalsDetached := false
			for _, key := range []string{
				exploreContentRecallRankSignal,
				exploreContentRecallTermSignal,
				exploreContentRecallExactSignal,
				exploreContentRecallAmbiguousSignal,
				exploreSourceLiteralSignal,
				exploreSourceLiteralCalleeSignal,
				exploreSourceLiteralCoverageSignal,
				exploreSourceLiteralTaskAlignSignal,
			} {
				if clone.Signals == nil || clone.Signals[key] <= current.Signals[key] {
					continue
				}
				if !signalsDetached {
					copied := make(map[string]float64, len(current.Signals)+1)
					for existingKey, value := range current.Signals {
						copied[existingKey] = value
					}
					current.Signals = copied
					signalsDetached = true
				}
				current.Signals[key] = clone.Signals[key]
			}
			return
		}
		byID[clone.Node.ID] = &clone
		out = append(out, &clone)
	}
	for _, candidate := range primary {
		add(candidate, false)
	}
	for _, candidate := range expanded {
		add(candidate, true)
	}
	return out
}

// hasExploreExpansionTerms reports whether concept normalization introduced a
// genuinely new discriminative term. A subset/equivalent bag would repeat the
// same FTS and materialization work without adding a retrieval channel.
func hasExploreExpansionTerms(query string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	raw := rerank.Tokenize(query)
	base := make(map[string]struct{}, len(raw)*2)
	for _, token := range raw {
		base[token] = struct{}{}
		base[exploreTerminalTermRoot(token)] = struct{}{}
	}
	for _, token := range search.NormalizeFTSTokens(raw) {
		base[token] = struct{}{}
	}
	for _, term := range search.NormalizeFTSTokens(terms) {
		if _, ok := base[term]; !ok {
			return true
		}
	}
	return false
}

// limitExploreCandidates cheaply fuses text and vector ranks before the full
// graph-aware reranker. It keeps vector-only evidence while bounding centrality
// and edge-hydration work to a predictable multiple of the response size.
func limitExploreCandidates(candidates []*rerank.Candidate, limit int) []*rerank.Candidate {
	if limit <= 0 || len(candidates) <= limit {
		return candidates
	}
	bounded := append([]*rerank.Candidate(nil), candidates...)
	rrf := func(candidate *rerank.Candidate) float64 {
		if candidate == nil || candidate.Node == nil {
			return -1
		}
		score := 0.0
		if candidate.TextRank >= 0 {
			score += 1 / (60 + float64(candidate.TextRank+1))
		}
		if candidate.VectorRank >= 0 {
			score += 1 / (60 + float64(candidate.VectorRank+1))
		}
		return score
	}
	sort.SliceStable(bounded, func(i, j int) bool {
		return rrf(bounded[i]) > rrf(bounded[j])
	})
	return bounded[:limit]
}

// limitExploreCandidatesPreservingSourceLiteral reserves one slot for the best
// request-local raw-source literal owners after the ordinary retrieval union is
// full. The bounded reservation prefers multi-anchor owners, preserves one
// semantic candidate, and never grows the candidate set or performs another
// graph lookup.
func limitExploreCandidatesPreservingSourceLiteral(candidates []*rerank.Candidate, limit int) []*rerank.Candidate {
	bounded := limitExploreCandidates(candidates, limit)
	if limit <= 0 || len(candidates) <= limit || len(bounded) == 0 {
		return bounded
	}
	return reserveExploreSourceLiteralCandidate(candidates, bounded)
}

// reserveExploreSourceLiteralCandidate inserts up to two bounded source-literal
// owners into an already-selected window, preferring owners that cover multiple
// anchors while preserving at least one semantic candidate. It leaves the
// established order untouched unless replacement is required.
func reserveExploreSourceLiteralCandidate(candidates, bounded []*rerank.Candidate) []*rerank.Candidate {
	if len(bounded) == 0 || len(candidates) <= len(bounded) {
		return bounded
	}
	// A source-body fallback may occupy at most the slots after the ranked
	// semantic head. This remains true even for a one-slot response.
	reservationLimit := min(exploreSourceLiteralReservationMax, len(bounded)-1)
	if reservationLimit <= 0 {
		return bounded
	}

	sources := make([]*rerank.Candidate, 0, exploreSourceLiteralReservationMax)
	seenSourceIDs := make(map[string]struct{}, exploreSourceLiteralReservationMax)
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil || candidate.Node.ID == "" || candidate.Signals == nil {
			continue
		}
		// A code definition holding an exact content match is literal evidence
		// with the same standing as a grep-lane owner: without a slot here it
		// falls off the ranked cut on every non-concept query class.
		literalEvidence := candidate.Signals[exploreSourceLiteralSignal] > 0 ||
			candidate.Signals[exploreContentRecallExactSignal] > 0 &&
				exploreCodeDefinitionKind(candidate.Node.Kind)
		if !literalEvidence {
			continue
		}
		if _, duplicate := seenSourceIDs[candidate.Node.ID]; duplicate {
			continue
		}
		seenSourceIDs[candidate.Node.ID] = struct{}{}
		sources = append(sources, candidate)
	}
	directProductionCallee := func(candidate *rerank.Candidate) bool {
		return candidate != nil && candidate.Node != nil && candidate.Signals != nil &&
			candidate.Signals[exploreSourceLiteralCalleeSignal] > 0 &&
			!exploreDraftIsTestNode(candidate.Node)
	}
	callableSpecificity := func(candidate *rerank.Candidate) int {
		if !directProductionCallee(candidate) {
			return 0
		}
		return exploreIdentifierSegmentCountBounded(candidate.Node.Name)
	}
	sort.SliceStable(sources, func(i, j int) bool {
		leftCoverage := sources[i].Signals[exploreSourceLiteralCoverageSignal]
		rightCoverage := sources[j].Signals[exploreSourceLiteralCoverageSignal]
		if leftCoverage != rightCoverage {
			return leftCoverage > rightCoverage
		}
		leftAligned := sources[i].Signals[exploreSourceLiteralTaskAlignSignal] > 0
		rightAligned := sources[j].Signals[exploreSourceLiteralTaskAlignSignal] > 0
		if leftAligned != rightAligned {
			return leftAligned
		}
		leftDirect := directProductionCallee(sources[i])
		rightDirect := directProductionCallee(sources[j])
		if leftDirect != rightDirect {
			return leftDirect
		}
		leftSettled := sources[i].Signals[exploreContentRecallAmbiguousSignal] <= 0
		rightSettled := sources[j].Signals[exploreContentRecallAmbiguousSignal] <= 0
		if leftSettled != rightSettled {
			return leftSettled
		}
		leftRank := sources[i].Signals[exploreSourceLiteralSignal]
		rightRank := sources[j].Signals[exploreSourceLiteralSignal]
		if leftRank != rightRank {
			return leftRank > rightRank
		}
		return callableSpecificity(sources[i]) > callableSpecificity(sources[j])
	})
	selectedSources := sources[:0]
	// A crowded source field is collision evidence: when more owners carry
	// literal signals than the reservation could ever seat, a second
	// single-anchor slot would spend the answer on one literal's noise. A
	// lone extra settled owner is the opposite — a distinct proven site.
	crowded := len(sources) > exploreSourceLiteralReservationMax
	for _, source := range sources {
		coverage := source.Signals[exploreSourceLiteralCoverageSignal]
		settled := source.Signals[exploreContentRecallAmbiguousSignal] <= 0
		if len(selectedSources) > 0 && coverage < 2 && (!settled || crowded) {
			continue
		}
		selectedSources = append(selectedSources, source)
		if len(selectedSources) == reservationLimit {
			break
		}
	}
	sources = selectedSources
	if len(sources) == 0 {
		return bounded
	}

	desired := make(map[string]struct{}, len(sources))
	present := make(map[string]struct{}, len(bounded))
	for _, candidate := range sources {
		desired[candidate.Node.ID] = struct{}{}
	}
	for _, candidate := range bounded {
		if candidate != nil && candidate.Node != nil {
			present[candidate.Node.ID] = struct{}{}
		}
	}
	reserved := append([]*rerank.Candidate(nil), bounded...)
	for _, source := range sources {
		if _, exists := present[source.Node.ID]; exists {
			continue
		}
		replace := -1
		for index := len(reserved) - 1; index >= 1; index-- {
			candidate := reserved[index]
			if candidate == nil || candidate.Node == nil {
				replace = index
				break
			}
			if _, protected := desired[candidate.Node.ID]; !protected {
				replace = index
				break
			}
		}
		if replace < 0 {
			break
		}
		if previous := reserved[replace]; previous != nil && previous.Node != nil {
			delete(present, previous.Node.ID)
		}
		reserved[replace] = source
		present[source.Node.ID] = struct{}{}
	}
	return reserved
}

// promoteExploreSourceLiteralCandidate moves the strongest complete source
// literal hit to the ranked head after final selection. Deadline-truncated or
// otherwise ambiguous evidence remains in the bounded set but cannot displace
// the ordinary retrieval head.
func promoteExploreSourceLiteralCandidate(candidates []*rerank.Candidate) []*rerank.Candidate {
	bestIndex := -1
	bestCoverage := 0.0
	bestSignal := 0.0
	for index, candidate := range candidates {
		if candidate == nil || candidate.Node == nil || candidate.Signals == nil ||
			candidate.Signals[exploreContentRecallAmbiguousSignal] > 0 {
			continue
		}
		coverage := candidate.Signals[exploreSourceLiteralCoverageSignal]
		signal := candidate.Signals[exploreSourceLiteralSignal]
		if signal > 0 && (coverage > bestCoverage || coverage == bestCoverage && signal > bestSignal) {
			bestIndex = index
			bestCoverage = coverage
			bestSignal = signal
		}
	}
	if bestIndex <= 0 {
		return candidates
	}

	promoted := append([]*rerank.Candidate(nil), candidates...)
	best := promoted[bestIndex]
	copy(promoted[1:bestIndex+1], promoted[:bestIndex])
	promoted[0] = best
	return promoted
}

// promoteExploreStrongSourceLiteralTarget keeps terminality aligned with the
// final evidence projection after concept owner folding. A unique, hydrated
// source-literal callee is direct implementation proof; an owner inserted from
// member_of metadata must not displace it. Ambiguous or incomplete literal
// hits deliberately retain their established order.
func promoteExploreStrongSourceLiteralTarget(targets []exploreTarget) []exploreTarget {
	hasDivergentOwner, hasDivergentType := false, false
	for _, target := range targets {
		hasDivergentOwner = hasDivergentOwner || target.divergentDefaultOwner
		hasDivergentType = hasDivergentType || target.divergentDefaultType
	}
	if hasDivergentOwner && hasDivergentType {
		// The paired constructor/default-flow proof identifies why behavior
		// diverges, while a literal callee identifies only where the literal is
		// consumed. Preserve the stronger causal ordering.
		return targets
	}
	best := -1
	for index, target := range targets {
		if localizationStrongSourceLiteralCallee(target) {
			best = index
			break
		}
	}
	if best <= 0 {
		return targets
	}
	promoted := append([]exploreTarget(nil), targets...)
	target := promoted[best]
	copy(promoted[1:best+1], promoted[:best])
	promoted[0] = target
	return promoted
}

// selectFinalExploreCandidates applies the source-literal reservation at the
// last production boundary, after every reranker and concept reservation has
// settled candidate order. The source hit replaces the weakest bounded
// production candidate, so response width never grows. With a very small
// maxSymbols value there may be no room for the semantic head, a concept
// implementation, and direct source evidence simultaneously; direct source
// evidence owns the weakest final slot in that case.
func selectFinalExploreCandidates(prod, test []*rerank.Candidate, maxSymbols int) []*rerank.Candidate {
	if maxSymbols <= 0 {
		return nil
	}
	cands := prod
	if len(cands) > maxSymbols {
		cands = cands[:maxSymbols]
	}
	cands = reserveExploreSourceLiteralCandidate(prod, cands)
	if len(cands) < maxSymbols {
		room := maxSymbols - len(cands)
		if room > len(test) {
			room = len(test)
		}
		cands = append(cands, test[:room]...)
	}
	return promoteExploreSourceLiteralCandidate(cands)
}

// exploreConceptRecallTerms preserves the query's discriminative concepts in
// first-seen order for the ordinary concept recall channel. The same generic
// and stopword filter used by answer-readiness keeps agent verbs and task
// boilerplate from consuming the bounded expansion bag.
func exploreConceptRecallTokenStream(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	tokens := make([]string, 0, len(fields))
	for _, field := range fields {
		tokens = append(tokens, rerank.Tokenize(field)...)
	}
	return tokens
}

func exploreConceptRecallTerms(text string) []string {
	const (
		maxTerms     = 12
		frequentCap  = 8
		lateMinChars = 6
	)
	allowed := exploreTerminalTerms(text)
	type termStat struct {
		term        string
		first, last int
		count       int
	}
	stats := make([]termStat, 0, len(allowed))
	byTerm := make(map[string]int, len(allowed))
	weightedTokens := exploreConceptRecallTokenStream(text)
	for position, raw := range weightedTokens {
		term := exploreTerminalTermRoot(strings.ToLower(strings.TrimSpace(raw)))
		if _, ok := allowed[term]; !ok {
			continue
		}
		if index, ok := byTerm[term]; ok {
			stats[index].count++
			stats[index].last = position
			continue
		}
		byTerm[term] = len(stats)
		stats = append(stats, termStat{term: term, first: position, last: position, count: 1})
	}
	if len(stats) <= maxTerms {
		out := make([]string, len(stats))
		for index := range stats {
			out[index] = stats[index].term
		}
		return out
	}

	// Repetition captures the weighted lead; length breaks ties toward more
	// informative concepts. Reserve the remaining slots by scanning the
	// original tail so late technical constraints survive the fixed bag even
	// when the lead was deliberately repeated for primary retrieval.
	ranked := append([]termStat(nil), stats...)
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		if len(ranked[i].term) != len(ranked[j].term) {
			return len(ranked[i].term) > len(ranked[j].term)
		}
		return ranked[i].first < ranked[j].first
	})
	selected := make(map[string]struct{}, maxTerms)
	for _, stat := range ranked {
		if len(selected) >= frequentCap {
			break
		}
		selected[stat.term] = struct{}{}
	}
	baseText := strings.TrimSpace(text)
	if lead := inlineLeadClause(baseText); lead != "" {
		suffix := " " + lead
		if strings.HasSuffix(baseText, suffix) {
			baseText = strings.TrimSpace(strings.TrimSuffix(baseText, suffix))
		}
	}
	baseTokens := exploreConceptRecallTokenStream(baseText)
	for index := len(baseTokens) - 1; index >= 0 && len(selected) < maxTerms; index-- {
		term := exploreTerminalTermRoot(strings.ToLower(strings.TrimSpace(baseTokens[index])))
		if len(term) < lateMinChars {
			continue
		}
		if _, ok := allowed[term]; !ok {
			continue
		}
		selected[term] = struct{}{}
	}
	for _, stat := range ranked {
		if len(selected) >= maxTerms {
			break
		}
		selected[stat.term] = struct{}{}
	}
	chosen := make([]termStat, 0, len(selected))
	for _, stat := range stats {
		if _, ok := selected[stat.term]; ok {
			chosen = append(chosen, stat)
		}
	}
	sort.SliceStable(chosen, func(i, j int) bool { return chosen[i].first < chosen[j].first })
	out := make([]string, len(chosen))
	for index := range chosen {
		out[index] = chosen[index].term
	}
	return out
}

// exploreLexicalTerms splits free task text into the distinct word/identifier
// terms (length >= 3, capped) that feed the per-term BM25 OR-merge fallback.
// Purely lexical — no vocabulary, no language model.
func exploreLexicalTerms(task string) []string {
	const maxTerms = 12
	seen := map[string]struct{}{}
	var out []string
	for _, f := range strings.Fields(task) {
		f = strings.Trim(f, "\"'`.,;:()[]{}<>!?—-")
		if len(f) < 3 {
			continue
		}
		key := strings.ToLower(f)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
		if len(out) >= maxTerms {
			break
		}
	}
	return out
}

func estimateTokens(s string) int { return len(s) / exploreCharsPerToken }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncateOneLine(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func dedupStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
