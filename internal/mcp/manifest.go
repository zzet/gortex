package mcp

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/elide"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
)

// defaultManifestBudget is the token ceiling a graded smart_context
// manifest fills when the caller does not pass token_budget.
const defaultManifestBudget = 8000

// smartCtxMaxSource caps how many focus functions/methods a flat
// smart_context response embeds full source for; the rest ship as
// signatures. The estimate path mirrors this to size the flat shape.
const smartCtxMaxSource = 3

// skeletonizeSiblingThreshold is the polymorphic-family size above
// which a focus type / interface / method is skeletonized (embedded as
// a signature/structure-only stub) instead of full source. When a
// symbol has more than this many interchangeable siblings — interface
// implementors, method overriders, or co-implementors of a shared
// interface — one representative shipping full detail is enough; the
// rest of the family is redundant and ships compressed. The threshold
// is deliberately conservative so a sole or near-sole implementation
// (the symbol the agent actually needs) is never skeletonized.
const skeletonizeSiblingThreshold = 3

// manifestSourceKinds are the node kinds whose source is worth
// embedding in a manifest; one-liners (vars, consts, fields) carry
// their whole meaning in the signature already.
var manifestSourceKinds = map[graph.NodeKind]bool{
	graph.KindFunction:  true,
	graph.KindMethod:    true,
	graph.KindType:      true,
	graph.KindInterface: true,
}

// ringMember is one symbol on the graph-distance-1 adjacency ring of
// the focus set, tagged with how it relates to the focus.
type ringMember struct {
	node     *graph.Node
	relation string
}

// buildContextManifest assembles a graded-fidelity context pack: the
// focus symbols at full source, their caller/callee adjacency ring as
// elided signature stubs, and an outline-only remainder — all packed
// under one token budget. Entries are returned as a single flat list
// tagged with `tier` so the shape stays friendly to every wire
// format; budget pressure demotes an entry to a cheaper tier (full →
// compressed → outline) rather than dropping it outright.
func (s *Server) buildContextManifest(ctx context.Context, focus, outlineCandidates []*graph.Node, budget int) map[string]any {
	if budget <= 0 {
		nodes := 0
		if s.graph != nil {
			nodes = s.graph.NodeCount()
		}
		budget = manifestBudgetForNodeCount(nodes)
	}
	used := 0
	placed := make(map[string]bool)
	entries := make([]map[string]any, 0, len(focus)+len(outlineCandidates))
	omitted := 0
	// siblingCache memoises the polymorphic-family size per node ID for
	// the duration of this call — a type that recurs across focus and
	// ring is counted once.
	siblingCache := make(map[string]int)
	siblingCountOf := func(n *graph.Node) int {
		if c, ok := siblingCache[n.ID]; ok {
			return c
		}
		c := s.manifestSiblingCount(ctx, n)
		siblingCache[n.ID] = c
		return c
	}

	// Flow-spine-aware sizing: a symbol on the forward flow spine from the
	// focus is on the answer path and stays full; off-spine polymorphic
	// siblings (and family supertypes) skeletonize to free budget. Computed
	// once from the primary focus symbol.
	onSpine := make(map[string]bool)
	if len(focus) > 0 && focus[0] != nil {
		if spine, _ := s.flowSpine(ctx, focus[0].ID, manifestSpineDepth); len(spine) > 0 {
			for _, id := range spine {
				onSpine[id] = true
			}
		}
	}
	// Process the symbols that will skeletonize first so their freed budget is
	// available to the unique, full-source symbols after them.
	focus = manifestOrderFocus(focus, onSpine, siblingCountOf)

	base := func(n *graph.Node) map[string]any {
		e := map[string]any{
			"id":         n.ID,
			"kind":       string(n.Kind),
			"name":       n.Name,
			"file_path":  n.FilePath,
			"language":   n.Language,
			"start_line": n.StartLine,
			"relation":      "",
			"distance":      0,
			"compressed":    false,
			"source":        "",
			"sibling_count": 0,
		}
		if sig, ok := n.Meta["signature"].(string); ok {
			e["signature"] = sig
		}
		return e
	}
	sigCost := func(e map[string]any) int {
		c := 16
		if sig, ok := e["signature"].(string); ok {
			c += int(tokens.CachedCountInt64(sig))
		}
		return c
	}

	// Tier 0 — focus: full source, demoted to a compressed stub or to
	// an outline entry when the budget tightens.
	for _, n := range focus {
		if n == nil || placed[n.ID] {
			continue
		}
		placed[n.ID] = true
		e := base(n)
		src := s.manifestSymbolSource(ctx, n)
		if src != "" {
			// Flow-spine-aware polymorphic-sibling skeletonization: a focus
			// type / interface / method in a large interchangeable family
			// (more than skeletonizeSiblingThreshold siblings) ships compressed
			// when it is OFF the answer-path flow spine, or when it is the
			// family supertype (the family-file override) — one representative
			// is enough and the freed budget goes to the symbols/files the
			// agent actually needs. An on-spine concrete symbol stays full.
			if sc := siblingCountOf(n); manifestShouldSkeletonize(n.Kind, onSpine[n.ID], sc) {
				if comp, err := elide.CompressString(src, n.Language); err == nil {
					if cc := int(tokens.CachedCountInt64(comp)); used+cc <= budget {
						e["tier"] = "focus"
						e["source"] = comp
						e["compressed"] = true
						e["sibling_count"] = sc
						e["off_spine"] = !onSpine[n.ID]
						used += cc
						entries = append(entries, e)
						continue
					}
				}
			}
			if full := int(tokens.CachedCountInt64(src)); used+full <= budget {
				e["tier"] = "focus"
				e["source"] = src
				used += full
				entries = append(entries, e)
				continue
			}
			if comp, err := elide.CompressString(src, n.Language); err == nil {
				if cc := int(tokens.CachedCountInt64(comp)); used+cc <= budget {
					e["tier"] = "ring"
					e["source"] = comp
					e["compressed"] = true
					used += cc
					entries = append(entries, e)
					continue
				}
			}
		}
		if c := sigCost(e); used+c <= budget {
			e["tier"] = "outline"
			used += c
			entries = append(entries, e)
			continue
		}
		omitted++
	}

	// Tier 1 — ring: the caller/callee adjacency of the focus set,
	// embedded as elided signature stubs.
	for _, rm := range s.manifestRing(ctx, focus, placed) {
		n := rm.node
		placed[n.ID] = true
		e := base(n)
		e["relation"] = rm.relation
		e["distance"] = 1
		src := s.manifestSymbolSource(ctx, n)
		if src != "" {
			comp := src
			if c, err := elide.CompressString(src, n.Language); err == nil {
				comp = c
			}
			if cc := int(tokens.CachedCountInt64(comp)); used+cc <= budget {
				e["tier"] = "ring"
				e["source"] = comp
				e["compressed"] = true
				used += cc
				entries = append(entries, e)
				continue
			}
		}
		if c := sigCost(e); used+c <= budget {
			e["tier"] = "outline"
			used += c
			entries = append(entries, e)
			continue
		}
		omitted++
	}

	// Tier 2 — outline: keyword matches past the focus cap, signature
	// only.
	for _, n := range outlineCandidates {
		if n == nil || placed[n.ID] {
			continue
		}
		placed[n.ID] = true
		e := base(n)
		e["distance"] = 2
		if c := sigCost(e); used+c <= budget {
			e["tier"] = "outline"
			used += c
			entries = append(entries, e)
			continue
		}
		omitted++
	}

	return map[string]any{
		"token_budget": budget,
		"tokens_used":  used,
		"omitted":      omitted,
		"entries":      entries,
	}
}

// manifestSymbolSource reads the on-disk source of a symbol worth
// embedding (functions, methods, types, interfaces). Returns "" when
// the symbol has no usable kind, range, or path.
func (s *Server) manifestSymbolSource(ctx context.Context, n *graph.Node) string {
	if n == nil || !manifestSourceKinds[n.Kind] {
		return ""
	}
	if n.StartLine <= 0 || n.EndLine <= 0 {
		return ""
	}
	absPath, err := s.resolveNodePath(ctx, n)
	if err != nil {
		return ""
	}
	src, _, _, err := s.readLinesForCtx(ctx, absPath, n.StartLine, n.EndLine, 0)
	if err != nil {
		return ""
	}
	// smart_context is an assembly surface, so config-leaf secrets are always
	// withheld here — the explicit-read override lives on read_file /
	// get_symbol_source. A no-op for ordinary code symbols.
	if red, did := s.maybeRedactConfigLeaf(n.Language, n.FilePath, false, src); did {
		src = red
	}
	return src
}

// skeletonizableKind reports whether a node kind participates in a
// polymorphic family worth skeletonizing: interfaces, concrete types,
// and methods. Functions, vars, consts, and fields are never
// skeletonized — they have no interchangeable sibling family.
func skeletonizableKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindInterface, graph.KindType, graph.KindMethod:
		return true
	default:
		return false
	}
}

// manifestSpineDepth is the forward flow-spine walk depth the graded manifest
// uses to decide which focus symbols are "on the answer path".
const manifestSpineDepth = 6

// manifestIsFamilySupertype reports whether a node kind is the *supertype* of a
// polymorphic family — an interface, or a parent method that subclasses
// override. (A concrete KindType is a family *member*, not its supertype.) The
// supertype's full body is redundant once the family's signatures are present.
func manifestIsFamilySupertype(k graph.NodeKind) bool {
	return k == graph.KindInterface || k == graph.KindMethod
}

// manifestShouldSkeletonize decides whether an embeddable focus symbol ships
// skeletonized (a compressed signature/structure stub) rather than at full
// source, given the flow spine and its polymorphic-family size:
//
//   - not a polymorphic-family member → never skeletonized;
//   - the family *supertype* (interface / overridden method) → ALWAYS
//     skeletonized, even on-spine: its body is redundant once the impl
//     signatures are present, and the freed budget goes to the sibling
//     implementation files the agent actually needs (the family-file override);
//   - an off-spine concrete sibling → skeletonized (one representative is
//     enough; thin the rest to free budget before unique symbols);
//   - an on-spine concrete symbol → kept full (it is on the answer path).
func manifestShouldSkeletonize(kind graph.NodeKind, onSpine bool, siblingCount int) bool {
	if !skeletonizableKind(kind) || siblingCount <= skeletonizeSiblingThreshold {
		return false
	}
	if manifestIsFamilySupertype(kind) {
		return true
	}
	return !onSpine
}

// manifestOrderFocus stably reorders the focus set so the symbols that will
// skeletonize (and therefore ship cheap) come first — the budget they free is
// then available to the unique, full-source symbols processed after them.
func manifestOrderFocus(focus []*graph.Node, onSpine map[string]bool, siblingCount func(*graph.Node) int) []*graph.Node {
	skel := make([]*graph.Node, 0, len(focus))
	uniq := make([]*graph.Node, 0, len(focus))
	for _, n := range focus {
		if n != nil && manifestShouldSkeletonize(n.Kind, onSpine[n.ID], siblingCount(n)) {
			skel = append(skel, n)
		} else {
			uniq = append(uniq, n)
		}
	}
	return append(skel, uniq...)
}

// manifestSiblingCount returns the size of a node's interchangeable
// polymorphic family — the signal that decides skeletonization:
//
//   - interface  → the number of concrete types that implement it.
//   - method     → the number of methods that override it.
//   - type       → the largest co-implementor count across the
//     interfaces the type implements (excluding the type itself), so a
//     concrete type in a large interface family skeletonizes alongside
//     its peers.
//
// A node with no family (a sole implementation, a non-polymorphic
// type) returns 0. The lookups are graph reads; the caller memoises
// them per manifest call.
func (s *Server) manifestSiblingCount(ctx context.Context, n *graph.Node) int {
	if n == nil {
		return 0
	}
	eng := s.engineFor(ctx)
	switch n.Kind {
	case graph.KindInterface:
		return len(eng.FindImplementations(n.ID))
	case graph.KindMethod:
		return len(eng.FindOverrides(n.ID))
	case graph.KindType:
		best := 0
		for _, edge := range eng.GetOutEdges(n.ID) {
			if edge.Kind != graph.EdgeImplements {
				continue
			}
			impls := eng.FindImplementations(edge.To)
			// Exclude the type itself from its own co-implementor count.
			co := 0
			for _, impl := range impls {
				if impl != nil && impl.ID != n.ID {
					co++
				}
			}
			if co > best {
				best = co
			}
		}
		return best
	default:
		return 0
	}
}

// ringScanLimit bounds how many callers/callees are scanned per focus
// symbol. It is generous on purpose: a complete caller/callee set is
// order-independent, so a value above almost every symbol's real fan
// keeps the ring — and therefore the pack root — deterministic.
const ringScanLimit = 64

// manifestRing collects the distance-1 adjacency ring of the focus
// set — direct callers and callees, skipping anything already placed.
// The ring is returned sorted by node ID so the manifest, its token
// accounting, and its pack root are all deterministic across repeated
// calls (the call-graph traversal itself does not promise an order).
func (s *Server) manifestRing(ctx context.Context, focus []*graph.Node, exclude map[string]bool) []ringMember {
	nodes := make(map[string]*graph.Node)
	relation := make(map[string]string)
	sessWS, _, _ := s.sessionScope(ctx)
	consider := func(n *graph.Node, rel string) {
		if n == nil || n.Kind == graph.KindFile || exclude[n.ID] {
			return
		}
		if _, dup := nodes[n.ID]; dup {
			return // first relation seen wins
		}
		nodes[n.ID] = n
		relation[n.ID] = rel
	}
	for _, f := range focus {
		if f == nil {
			continue
		}
		callers := s.engineFor(ctx).GetCallers(f.ID, query.QueryOptions{Depth: 1, Limit: ringScanLimit, Detail: "brief", WorkspaceID: sessWS})
		for _, cn := range callers.Nodes {
			if cn.ID != f.ID {
				consider(cn, "caller")
			}
		}
		callees := s.engineFor(ctx).GetCallChain(f.ID, query.QueryOptions{Depth: 1, Limit: ringScanLimit, Detail: "brief", WorkspaceID: sessWS})
		for _, cn := range callees.Nodes {
			if cn.ID != f.ID {
				consider(cn, "callee")
			}
		}
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ring := make([]ringMember, 0, len(ids))
	for _, id := range ids {
		ring = append(ring, ringMember{node: nodes[id], relation: relation[id]})
	}
	return ring
}

// buildSmartContextEstimate projects the token cost of a smart_context
// symbol delivery without returning the payload, so an agent can
// budget before fetching. For graded fidelity it sizes the manifest
// at the given budget; for flat fidelity it sizes the legacy
// relevant_symbols shape (full source for the first smartCtxMaxSource
// functions, signatures for the rest).
func (s *Server) buildSmartContextEstimate(ctx context.Context, graded bool, budget int, focus, outline []*graph.Node) map[string]any {
	est := map[string]any{
		"symbol_count": len(focus) + len(outline),
	}
	if graded {
		mani := s.buildContextManifest(ctx, focus, outline, budget)
		tiers := map[string]int{"focus": 0, "ring": 0, "outline": 0}
		if entries, ok := mani["entries"].([]map[string]any); ok {
			for _, e := range entries {
				if t, ok := e["tier"].(string); ok {
					tiers[t]++
				}
			}
		}
		est["fidelity"] = "graded"
		est["token_budget"] = mani["token_budget"]
		est["projected_tokens"] = mani["tokens_used"]
		est["omitted"] = mani["omitted"]
		est["focus"] = tiers["focus"]
		est["ring"] = tiers["ring"]
		est["outline"] = tiers["outline"]
		return est
	}
	est["fidelity"] = "flat"
	est["projected_tokens"] = s.estimateFlatTokens(ctx, focus)
	return est
}

// estimateFlatTokens sizes a flat smart_context symbol payload: a
// per-entry signature cost for every focus symbol plus full source
// for the first smartCtxMaxSource functions/methods.
func (s *Server) estimateFlatTokens(ctx context.Context, syms []*graph.Node) int {
	total, embedded := 0, 0
	for _, n := range syms {
		if n == nil {
			continue
		}
		total += 16
		if sig, ok := n.Meta["signature"].(string); ok {
			total += int(tokens.CachedCountInt64(sig))
		}
		if embedded < smartCtxMaxSource && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
			if src := s.manifestSymbolSource(ctx, n); src != "" {
				total += int(tokens.CachedCountInt64(src))
				embedded++
			}
		}
	}
	return total
}
