package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// javaOverrideDispatchCap bounds how many overrides a single ambiguous call
// may fan out to. A name shared by more definitions than this is too generic
// to attribute confidently, so the call is left ambiguous rather than sprayed
// across the graph.
const javaOverrideDispatchCap = 8

// resolveJavaOverrideDispatchCandidates fans out an ambiguous Java member call
// whose same-name candidates are overrides related through the class hierarchy.
// Because no receiver type disambiguated the legal runtime targets, emitted
// edges remain speculative and are hidden from default usage queries. The
// caller supplies the already-scoped candidate census so full and warm passes
// share the same dispatch semantics without repeating a corpus scan.
func (r *Resolver) resolveJavaOverrideDispatchCandidates(candidates overrideDispatchCandidates) int {
	g := r.graph
	if g == nil {
		return 0
	}
	ancestors := javaTypeAncestors(g)
	if len(ancestors) == 0 {
		return 0
	}

	type fanout struct {
		edge   *graph.Edge
		base   *graph.Node
		others []*graph.Node
	}
	var jobs []fanout
	for e := range candidates.javaEdges(g) {
		if e == nil || e.Kind != graph.EdgeCalls || e.IsSpeculative() {
			continue
		}
		// Scoped warm pass: an unchanged repo's calls were already dispatched (or
		// left ambiguous) by a prior full pass over the same hierarchy, so only
		// reconsider the changed repos' calls.
		if !r.edgeFromInScope(e.From) {
			continue
		}
		name := javaUnresolvedMemberName(e.To)
		if name == "" || strings.HasSuffix(name, ".<init>") {
			continue
		}
		// SQLite joined caller language in the compact census. Adapters without
		// exact identity lookup retain the historical hydration gate.
		if !candidates.exact {
			caller := r.cachedGetNode(e.From)
			if caller == nil || caller.Language != "java" {
				continue
			}
		}
		cands := javaOverrideCandidates(r.cachedFindNodesByNameInRepo(name, r.callerRepoPrefix(e)))
		if len(cands) < 2 || len(cands) > javaOverrideDispatchCap {
			continue
		}
		if !javaOverridesRelated(cands, ancestors) {
			continue
		}
		jobs = append(jobs, fanout{edge: e, base: cands[0], others: cands[1:]})
	}

	n := 0
	for _, j := range jobs {
		oldTo := j.edge.To
		j.edge.To = j.base.ID
		j.edge.Origin = graph.OriginSpeculative
		j.edge.Confidence = 0.3
		if j.edge.Meta == nil {
			j.edge.Meta = map[string]any{}
		}
		j.edge.Meta[graph.MetaSpeculative] = true
		j.edge.Meta["dispatch"] = "override"
		if !r.applyReindexEdges([]graph.EdgeReindex{{Edge: j.edge, OldTo: oldTo}}) {
			continue
		}
		n++
		for _, o := range j.others {
			g.AddEdge(&graph.Edge{
				From: j.edge.From, To: o.ID, Kind: graph.EdgeCalls,
				FilePath: j.edge.FilePath, Line: j.edge.Line,
				Origin:     graph.OriginSpeculative,
				Confidence: 0.3,
				Meta:       map[string]any{graph.MetaSpeculative: true, "dispatch": "override"},
			})
			n++
		}
	}
	return n
}

// javaUnresolvedMemberName returns the method name of an `unresolved::*.<name>`
// member-call target, or "" for any other target shape.
func javaUnresolvedMemberName(to string) string {
	name := graph.UnresolvedName(to)
	if name == "" {
		return ""
	}
	rest, ok := strings.CutPrefix(name, "*.")
	if !ok || strings.Contains(rest, "::") {
		return ""
	}
	return rest
}

// javaOverrideCandidates filters name-matched nodes to the in-repo Java method
// definitions, one per declaring type (deduped by receiver), excluding stubs
// and definitions with no declaring type.
func javaOverrideCandidates(raw []*graph.Node) []*graph.Node {
	var out []*graph.Node
	seen := map[string]bool{}
	for _, n := range raw {
		if n == nil || n.Language != "java" || n.Kind != graph.KindMethod {
			continue
		}
		if graph.IsStub(n.ID) || graph.IsUnresolvedTarget(n.ID) {
			continue
		}
		recv := nodeReceiverType(n)
		if recv == "" || seen[recv] {
			continue
		}
		seen[recv] = true
		out = append(out, n)
	}
	return out
}

// javaOverridesRelated reports whether the candidate methods are overrides of a
// common supertype: their declaring types share at least one common ancestor
// (or one is an ancestor of another). This is the precision gate — same-name
// methods on unrelated types (no shared ancestor) are never sprayed together,
// while genuine overrides of a common base (two entities overriding
// BaseEntity's toString) fan out to every override the way a language server's
// call hierarchy attributes them.
func javaOverridesRelated(cands []*graph.Node, ancestors map[string]map[string]bool) bool {
	var common map[string]bool
	for i, c := range cands {
		rc := nodeReceiverType(c)
		if rc == "" {
			return false
		}
		// Ancestor-or-self set of this candidate's declaring type.
		set := map[string]bool{rc: true}
		for a := range ancestors[rc] {
			set[a] = true
		}
		if i == 0 {
			common = set
			continue
		}
		for k := range common {
			if !set[k] {
				delete(common, k)
			}
		}
		if len(common) == 0 {
			return false
		}
	}
	return len(common) > 0
}

// javaBaseTypeName reduces a possibly package-qualified, generic type reference
// to its simple class name (`model.Person<X>` → `Person`).
func javaBaseTypeName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// javaTypeAncestors builds, for each Java type simple name, the transitive set
// of its superclass simple names. The direct superclass is read from each type
// node's scope_parent meta — the same source the scope resolver's super-method
// walk uses — because regular Java `extends` is recorded there, not as a graph
// EdgeExtends (only anonymous classes emit that). So a cross-package
// inheritance chain (`owner.Owner extends model.Person extends model.BaseEntity`)
// contributes to the hierarchy even though its supertype references never
// resolved to a type node. Empty when the graph indexes no Java hierarchy.
func javaTypeAncestors(g graph.Store) map[string]map[string]bool {
	direct := map[string]map[string]bool{}
	add := func(childName, parentName string) {
		if childName == "" || parentName == "" || childName == parentName {
			return
		}
		set := direct[childName]
		if set == nil {
			set = map[string]bool{}
			direct[childName] = set
		}
		set[parentName] = true
	}
	for _, kind := range []graph.NodeKind{graph.KindType, graph.KindInterface} {
		for n := range g.NodesByKind(kind) {
			if n == nil || n.Language != "java" || n.Name == "" || n.Meta == nil {
				continue
			}
			if p, ok := n.Meta[MetaScopeParentClass].(string); ok {
				add(n.Name, javaBaseTypeName(p))
			}
		}
	}
	if len(direct) == 0 {
		return nil
	}
	// Transitive closure via DFS from each type.
	closure := make(map[string]map[string]bool, len(direct))
	var visit func(t string, acc map[string]bool, seen map[string]bool)
	visit = func(t string, acc, seen map[string]bool) {
		for p := range direct[t] {
			if seen[p] {
				continue
			}
			seen[p] = true
			acc[p] = true
			visit(p, acc, seen)
		}
	}
	for t := range direct {
		acc := map[string]bool{}
		visit(t, acc, map[string]bool{t: true})
		closure[t] = acc
	}
	return closure
}
