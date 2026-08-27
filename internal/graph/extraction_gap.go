package graph

// ZeroEdgeClass classifies why a symbol's graph query returned nothing an
// agent can act on. A thin result has several very different causes that
// an agent cannot otherwise tell apart — genuinely unused code, a symbol
// the extractor never processed, and resolution that landed too weakly to
// prove anything — and a pre-edit safety check that trusts a false
// "0 usages" is silently disarmed.
type ZeroEdgeClass string

const (
	// ZeroEdgeNone means the symbol has incoming call/reference edges:
	// the query was not empty and no caveat is warranted.
	ZeroEdgeNone ZeroEdgeClass = "none"

	// ZeroEdgeLikelyUnused means the symbol has no incoming
	// call/reference edges but DOES carry other graph edges — the
	// structural edge from its file (`defines`), a method's
	// `member_of`, or outgoing calls / references / type references.
	// This is consistent with genuine dead code: the extractor saw
	// the symbol, nothing uses it.
	ZeroEdgeLikelyUnused ZeroEdgeClass = "likely_unused"

	// ZeroEdgePossibleExtractionGap means the symbol has zero edges of
	// any kind. A normally indexed function or method always carries
	// at least one structural edge — the file `defines` it, a method
	// is `member_of` its type — so zero total edges most likely means
	// the extractor never processed the symbol or its file. The
	// symbol may well be live; the graph just does not know. This is
	// the dangerous case for a delete-or-rewrite decision.
	ZeroEdgePossibleExtractionGap ZeroEdgeClass = "possible_extraction_gap"

	// ZeroEdgeCoverageIncomplete means the symbol has no *trustworthy*
	// incoming call/reference edges, but the graph does carry weaker
	// evidence that consumers exist, from any of three sources:
	// inbound `imports` / `re_exports` edges landing on the symbol
	// itself or (for a public JS/TS symbol) on its file; unresolved
	// same-name call candidates the resolver never bound; or incoming
	// usage edges whose only provenance is a name-only match. The usage
	// query is incomplete, not empty — reference-level resolution
	// failed somewhere upstream — so an agent must NOT read the result
	// as safe-to-remove.
	ZeroEdgeCoverageIncomplete ZeroEdgeClass = "coverage_incomplete"
)

// ZeroEdgeCaveat is the structured caveat attached to a graph query
// result an agent must not take at face value — one that came back empty,
// or one whose every usage row is a name-only match. Class is
// machine-checkable so a safety gate can branch on it; Message is a short
// human-readable explanation.
type ZeroEdgeCaveat struct {
	Class   ZeroEdgeClass `json:"class" toon:"class"`
	Message string        `json:"message" toon:"message"`
}

// ZeroEdgeClassConservatism ranks caveat classes by how strongly they
// warn against trusting an empty result — higher is more cautious.
// Coverage-incomplete carries positive (if weak) evidence of use, so it
// out-ranks the evidence-free classes; likely_unused is the most
// permissive claim and must never displace either. Merging layers
// (federation) use this to pick the surviving caveat when sources
// disagree on a zero-row answer.
func ZeroEdgeClassConservatism(c ZeroEdgeClass) int {
	switch c {
	case ZeroEdgeCoverageIncomplete:
		return 3
	case ZeroEdgePossibleExtractionGap:
		return 2
	case ZeroEdgeLikelyUnused:
		return 1
	default:
		return 0
	}
}

// ZeroEdgeClassRefutedByTierFilter reports whether a tier_filtered
// marker contradicts this class outright. The marker names edges that
// EXIST below the requested tier, which refutes the likely_unused
// CONCLUSION — the symbol is not unused, its evidence is filtered —
// while coverage_incomplete and possible_extraction_gap are
// UNCERTAINTY about coverage that a filter explains nothing about.
// Lives beside the class producer so layers merging caveats
// (federation) never re-derive class semantics by enum comparison.
func ZeroEdgeClassRefutedByTierFilter(c ZeroEdgeClass) bool {
	return c == ZeroEdgeLikelyUnused
}

// ZeroEdgeClassDescribesOwnGraphOnly reports whether a caveat of this
// class from a source that did NOT resolve the queried symbol speaks
// only for that source's graph: an extraction-gap caveat is "MY graph
// may have failed to extract this", which is evidence about the
// answering graph, never about the union.
func ZeroEdgeClassDescribesOwnGraphOnly(c ZeroEdgeClass) bool {
	return c == ZeroEdgePossibleExtractionGap
}

// ZeroEdgeReader is the minimal read surface zero-edge classification
// needs. Both graph backends and request-scoped overlay views satisfy
// it, so callers serving an overlay session pass the same view the
// query ran on — classifying against the base graph misreports symbols
// the overlay added, removed, or re-wired.
type ZeroEdgeReader interface {
	GetNode(id string) *Node
	GetInEdges(nodeID string) []*Edge
	GetOutEdges(nodeID string) []*Edge
}

// ZeroImpactCaveat is the per-symbol caveat attached to an empty impact
// analysis result, which is computed over a list of symbols. It carries
// the symbol ID alongside the same machine-checkable classification.
type ZeroImpactCaveat struct {
	ID      string        `json:"id" toon:"id"`
	Class   ZeroEdgeClass `json:"class" toon:"class"`
	Message string        `json:"message" toon:"message"`
}

// TierFilteredClass is the machine-checkable class of a min_tier-emptied
// result — distinct from ZeroEdgeClass so a safety gate never confuses
// "filtered out by min_tier" with "genuinely has no usages".
const TierFilteredClass = "tier_filtered"

// TierFilteredCaveat is attached to a find_usages / get_* result whose edge
// list was emptied (or thinned) by a min_tier filter while lower-tier edges
// still exist. Without it a min_tier that filters every edge is
// indistinguishable from "no usages" — the honesty bug it fixes. Class is
// always TierFilteredClass; EdgesBelowMinTier counts the edges dropped by the
// filter and MaxAvailableTier names the best tier actually present.
type TierFilteredCaveat struct {
	Class             string `json:"class" toon:"class"`
	EdgesBelowMinTier int    `json:"edges_below_min_tier" toon:"edges_below_min_tier"`
	MaxAvailableTier  string `json:"max_available_tier" toon:"max_available_tier"`
}

// usageEdgeKinds are the incoming edge kinds that count as a symbol
// being "used" — calls, references, and the type/instantiation edges
// that find_usages itself treats as usages. An incoming edge of any of
// these kinds means the symbol is not dead code.
var usageEdgeKinds = map[EdgeKind]bool{
	EdgeCalls:        true,
	EdgeReferences:   true,
	EdgeInstantiates: true,
	EdgeImplements:   true,
	EdgeExtends:      true,
	EdgeReads:        true,
	EdgeWrites:       true,
	EdgeTests:        true,
}

// UsageInboundEdgeKinds returns the canonical list of incoming edge
// kinds that classify a symbol as "used" by ClassifyZeroEdge. Exposed
// for capability callers (NodeDegreeAggregator) that need to mirror
// the in-graph usage filter server-side. Order is stable so the slice
// is safe to pass directly to a query parameter binding.
func UsageInboundEdgeKinds() []EdgeKind {
	return []EdgeKind{
		EdgeCalls,
		EdgeReferences,
		EdgeInstantiates,
		EdgeImplements,
		EdgeExtends,
		EdgeReads,
		EdgeWrites,
		EdgeTests,
	}
}

// IsWeakUsageEvidence reports whether an incoming usage edge is name-only
// evidence rather than proof that the symbol is used.
//
// An edge is weak when it is speculative (a best-guess dynamic-dispatch
// edge, default-excluded from every edge-returning surface) or when its
// effective provenance is the text_matched tier — a bare-name match that
// no type system, LSP provider, or unambiguous AST binding corroborates.
// Everything from ast_inferred upward counts as real evidence: it is
// keyed on an extracted type, not on a name collision.
//
// Provenance is read through Edge.EffectiveOrigin, so an edge the
// producer never stamped is judged by the tier it is *displayed* with
// rather than treated as untagged.
func IsWeakUsageEvidence(e *Edge) bool {
	if e == nil {
		return true
	}
	if e.IsSpeculative() {
		return true
	}
	switch e.EffectiveOrigin() {
	case OriginTextMatched, OriginSpeculative:
		return true
	}
	return false
}

// ClassifyZeroEdge inspects a symbol's incoming and outgoing edges and
// returns how an empty usage/caller/impact query for it should be read.
//
//   - ZeroEdgeNone — the symbol has at least one incoming usage edge
//     that is more than a name match.
//   - ZeroEdgeCoverageIncomplete — the only incoming evidence is weak:
//     import-level consumer edges, unresolved same-name call candidates,
//     or usage edges resolved by name alone. Unverified, not proof.
//   - ZeroEdgeLikelyUnused — no incoming usage edge of any strength, but
//     the symbol has other graph edges (structural defines/member_of, or
//     any outgoing edge). Consistent with genuine dead code.
//   - ZeroEdgePossibleExtractionGap — the symbol has no edges at all,
//     which a normally indexed symbol never has; the extractor most
//     likely missed it.
//
// Classification weighs incoming usage edges by provenance, not by kind
// alone. A bare-name text match against a common method name (`Get`,
// `Run`, `Close`) is the single most likely edge to be a false positive,
// and counting one as proof of use disarms every safety gate downstream:
// the symbol reads as live, so genuine dead code hides behind the noise.
// Speculative edges are worse still — they are hidden from the result an
// agent sees, so trusting them turns an empty answer into an *uncaveated*
// empty answer, the exact failure this file exists to prevent.
//
// An unknown symbol ID is reported as an extraction gap: a query whose
// target is not even in the graph is exactly as untrustworthy as one
// whose target was never wired up.
func ClassifyZeroEdge(g ZeroEdgeReader, symbolID string) ZeroEdgeClass {
	if g == nil || symbolID == "" {
		return ZeroEdgePossibleExtractionGap
	}
	if g.GetNode(symbolID) == nil {
		return ZeroEdgePossibleExtractionGap
	}

	in := g.GetInEdges(symbolID)
	out := g.GetOutEdges(symbolID)

	if len(in) == 0 && len(out) == 0 {
		return ZeroEdgePossibleExtractionGap
	}
	weakUsage := false
	for _, e := range in {
		if !usageEdgeKinds[e.Kind] {
			continue
		}
		if !IsWeakUsageEvidence(e) {
			return ZeroEdgeNone
		}
		weakUsage = true
	}
	if importConsumerCount(g, symbolID) > 0 {
		return ZeroEdgeCoverageIncomplete
	}
	// The graph still carries unresolved call candidates that name this
	// symbol — `unresolved::*.<name>` call edges the resolver / enrichment
	// pass never bound to it. Their existence is direct evidence that call
	// sites reference this name; an empty usage set is a resolution/coverage
	// gap, not proof the symbol is unused.
	if hasUnresolvedSameNameCandidates(g, symbolID) {
		return ZeroEdgeCoverageIncomplete
	}
	// Name-only usage edges are evidence of a consumer, just not
	// trustworthy evidence. They rule out likely_unused (something does
	// name this symbol) without earning ZeroEdgeNone.
	if weakUsage {
		return ZeroEdgeCoverageIncomplete
	}
	return ZeroEdgeLikelyUnused
}

// hasUnresolvedSameNameCandidates reports whether the graph holds any
// unresolved call edge naming this callable symbol. All four bare-name
// placeholder shapes count (see UnresolvedNameCandidateIDs): checking only
// the member form missed every free-function call, and every call at all in
// a multi-repo graph, so a symbol whose call sites failed to resolve was
// classified likely_unused. Unresolved call stubs are indexed by their
// target string, so each check is a reverse-edge lookup — no scan.
// Non-callable symbols never match.
//
// Only existence matters here, never the edges themselves. A placeholder for a
// common method name (`unresolved::*.Get`) is a hub with thousands of inbound
// call edges, and materialising all of them with their Meta blobs to answer a
// yes/no question is the expensive way to ask it. Backends that can count
// server-side answer every candidate in one round-trip; the rest fall back to
// the direct edge read.
func hasUnresolvedSameNameCandidates(g ZeroEdgeReader, symbolID string) bool {
	n := g.GetNode(symbolID)
	if n == nil {
		return false
	}
	if n.Kind != KindFunction && n.Kind != KindMethod {
		return false
	}
	candidates := UnresolvedNameCandidateIDs(n)
	if len(candidates) == 0 {
		return false
	}
	if counter, ok := g.(InDegreeForNodes); ok {
		for _, count := range counter.InDegreeForNodes(candidates) {
			if count > 0 {
				return true
			}
		}
		return false
	}
	for _, id := range candidates {
		if len(g.GetInEdges(id)) > 0 {
			return true
		}
	}
	return false
}

// importConsumerCount counts the import-level consumer evidence for a
// symbol: inbound `imports` / `re_exports` edges on the symbol itself
// (any language — a per-binding import that resolved onto the symbol is
// direct proof a consumer names it), plus, for a public JS/TS symbol,
// inbound module-level import edges on its file node (a module-level
// `import ... from './file'` lands on the file, but still proves the
// file's exports have consumers).
func importConsumerCount(g ZeroEdgeReader, symbolID string) int {
	count := 0
	for _, e := range g.GetInEdges(symbolID) {
		if e.Kind == EdgeImports || e.Kind == EdgeReExports {
			count++
		}
	}
	if count > 0 {
		return count
	}
	n := g.GetNode(symbolID)
	if n == nil || n.FilePath == "" || n.FilePath == symbolID {
		return 0
	}
	switch n.Language {
	case "typescript", "tsx", "javascript", "jsx":
	default:
		return 0 // module-level file imports imply consumers only for JS/TS
	}
	if vis, _ := n.Meta["visibility"].(string); vis != "public" {
		return 0
	}
	for _, e := range g.GetInEdges(n.FilePath) {
		if e.Kind == EdgeImports || e.Kind == EdgeReExports {
			count++
		}
	}
	return count
}

// zeroEdgeMessages maps each classification to its human-readable
// caveat text.
var zeroEdgeMessages = map[ZeroEdgeClass]string{
	ZeroEdgeLikelyUnused: "no incoming call or reference edges, but the symbol is " +
		"indexed (it has structural or outgoing edges) — consistent with genuine " +
		"unused code. It is also what an import specifier the resolver could not " +
		"expand looks like, so this is evidence of no callers, not proof: confirm " +
		"with a text search for the symbol name before removing it.",
	ZeroEdgePossibleExtractionGap: "the symbol has no graph edges of any kind. A " +
		"normally indexed symbol always has at least a structural edge, so the " +
		"extractor most likely did not process it — treat this empty result as " +
		"unverified, not as proof the symbol is unused.",
	ZeroEdgeCoverageIncomplete: "no resolved call or reference edges, but the graph " +
		"still carries consumer evidence — import/re-export edges pointing at this " +
		"symbol or its file, unresolved same-name call candidates the resolver / " +
		"enrichment pass never bound to it, or usage edges resolved by name alone. " +
		"Reference-level resolution is incomplete, so treat this empty result as " +
		"UNVERIFIED coverage, not as proof the symbol is unused or safe to remove.",
}

// weakUsageEvidenceMessage is the caveat text for a NON-empty result whose
// every row is a name-only match. The result is not empty, so the other
// messages' "this empty result" framing would misdescribe it; what the
// agent needs to know is that the rows it can see are the untrustworthy
// tier and may all be false positives.
const weakUsageEvidenceMessage = "every usage below is a name-only match (the " +
	"text_matched tier) — no type system, LSP provider, or unambiguous AST binding " +
	"corroborates any of them, so a symbol with a common name collects unrelated " +
	"call sites here. Treat this as UNVERIFIED coverage in both directions: it is " +
	"not proof the symbol is used, and the listed callers may not be real. Confirm " +
	"with a text search for the symbol name before relying on either reading."

// WeakUsageEvidenceOnly reports whether a materialised result consists
// entirely of weak usage evidence — at least one usage edge, every one of
// them name-only (see IsWeakUsageEvidence). Non-usage edge kinds are
// ignored so a structural edge riding along in the subgraph cannot mask
// the verdict.
//
// This is the non-empty counterpart to ClassifyZeroEdge, for callers that
// already hold the edge list: it reaches the same conclusion from edges in
// hand instead of re-reading the graph.
func WeakUsageEvidenceOnly(edges []*Edge) bool {
	weak := false
	for _, e := range edges {
		if e == nil || !usageEdgeKinds[e.Kind] {
			continue
		}
		if !IsWeakUsageEvidence(e) {
			return false
		}
		weak = true
	}
	return weak
}

// CaveatForWeakUsageEvidence is the caveat for a non-empty result whose
// every usage row is a name-only match. It carries
// ZeroEdgeCoverageIncomplete so a safety gate already branching on that
// class trips here too, with wording that fits a populated result.
func CaveatForWeakUsageEvidence() *ZeroEdgeCaveat {
	return &ZeroEdgeCaveat{
		Class:   ZeroEdgeCoverageIncomplete,
		Message: weakUsageEvidenceMessage,
	}
}

// zeroEdgeNotFoundMessage is the caveat text when the queried id is not in
// the graph at all — almost always a mistyped id or one missing its repo
// prefix, rather than a true extraction gap.
const zeroEdgeNotFoundMessage = "no symbol with this id is in the graph — the id is " +
	"probably mistyped or missing its repo prefix (ids look like " +
	"<repo>/<path>::<symbol>, e.g. gortex/internal/x.go::Foo). Run a symbol search to " +
	"get the exact id; treat this empty result as unverified, not as proof of no usages."

// CaveatForZeroEdge builds the structured caveat for an empty graph
// query result on symbolID. It returns nil when the symbol has
// incoming usage edges (ZeroEdgeNone) — a non-empty result carries no
// caveat — so callers can attach the return value unconditionally.
func CaveatForZeroEdge(g ZeroEdgeReader, symbolID string) *ZeroEdgeCaveat {
	// A target that is not even in the graph is the most common cause of a
	// "0 usages" surprise — usually a mistyped id or one missing its repo
	// prefix. Keep the untrustworthy extraction-gap class so safety gates
	// still trip, but point the message at the id rather than the extractor.
	if g != nil && symbolID != "" && g.GetNode(symbolID) == nil {
		return &ZeroEdgeCaveat{Class: ZeroEdgePossibleExtractionGap, Message: zeroEdgeNotFoundMessage}
	}
	return CaveatForZeroEdgeResolved(g, symbolID)
}

// CaveatForZeroEdgeResolved is CaveatForZeroEdge for callers whose
// request-scoped view already resolved symbolID: an overlay-served
// symbol is absent from the base graph, so the mistyped-id fast path
// would mislabel it. Classification goes straight to the deeper path.
func CaveatForZeroEdgeResolved(g ZeroEdgeReader, symbolID string) *ZeroEdgeCaveat {
	class := ClassifyZeroEdge(g, symbolID)
	if class == ZeroEdgeNone {
		return nil
	}
	return &ZeroEdgeCaveat{Class: class, Message: zeroEdgeMessages[class]}
}
