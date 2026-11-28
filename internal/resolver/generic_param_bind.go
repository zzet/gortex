package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// bindGenericParamRefs rewrites `unresolved::<name>` edges where the
// name is a generic type parameter declared by the source's
// enclosing function. The Go extractor already materialises
// KindGenericParam nodes with IDs `<func>#tparam:<name>` and an
// EdgeMemberOf back to the owner — the resolver just hasn't been
// consulting them when an in-body reference (`var x T`, return type
// `T`, etc.) lands as `unresolved::T`.
//
// Side benefit beyond stub reduction: `find_usages` on a generic
// type parameter starts working — *"where in this generic function
// is T used?"* — which is a real refactoring query.
//
// Scope is per-function: a function's tparams are visible only
// inside its body. The owner-keyed index built here lets each edge
// resolve in O(1) without re-walking the graph.
func (r *Resolver) bindGenericParamRefs() {
	owned := r.buildGenericParamOwnerIndex()
	if len(owned) == 0 {
		return
	}

	var batch []graph.EdgeReindex
	// We don't know up front which edge kinds carry type-param refs:
	// EdgeReferences for `var x T`, EdgeTypedAs for parameters typed
	// as T, EdgeReturns for return signature, EdgeInstantiates for
	// generic instantiation expressions. Walk the union.
	for _, k := range []graph.EdgeKind{
		graph.EdgeReferences,
		graph.EdgeTypedAs,
		graph.EdgeReturns,
		graph.EdgeInstantiates,
	} {
		for e := range r.graph.EdgesByKind(k) {
			if old := r.tryBindGenericParam(e, owned); old != "" {
				batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: old})
			}
		}
	}
	if len(batch) > 0 {
		r.applyReindexEdges(batch)
	}
}

func (r *Resolver) buildGenericParamOwnerIndex() map[string]map[string]string {
	// owner-function ID → set of tparam-name → tparam-node-id.
	owned := map[string]map[string]string{}
	for n := range graph.NamedLanguageNodesSeq(r.graph, graph.KindGenericParam) {
		if n.Language != "go" || n.Name == "" {
			continue
		}
		owner := enclosingFunctionForBinding(n.ID)
		if owner == "" || owner == n.ID {
			continue
		}
		set, ok := owned[owner]
		if !ok {
			set = map[string]string{}
			owned[owner] = set
		}
		// Don't overwrite — two tparams with the same name in the
		// same function shouldn't happen in valid Go, but be defensive.
		if _, dup := set[n.Name]; dup {
			set[n.Name] = ""
			continue
		}
		set[n.Name] = n.ID
	}
	return owned
}

func (r *Resolver) bindGenericParamRefsFromCensus(census *postBareAttributionCensus) {
	if census == nil {
		return
	}
	owned := r.buildGenericParamOwnerIndex()
	if len(owned) == 0 {
		return
	}
	r.reindexAttributionIdentitiesBatched(
		census.finder,
		census.genericCandidates,
		func(edge *graph.Edge) string {
			return r.tryBindGenericParam(edge, owned)
		},
	)
}

// bindGenericParamRefsForFile is the single-file-resolve form of
// bindGenericParamRefs. A type parameter is visible only inside its own
// function's body, which lives in this file, so both the owner index and the
// edges to rewrite are scoped to the file — no whole-graph EdgesByKind sweep
// (the dominant cost of an incremental edit on a large graph).
func (r *Resolver) bindGenericParamRefsForFile(filePath string) {
	owned := map[string]map[string]string{}
	for _, n := range r.incrementalFileNodes(filePath) {
		if n == nil || n.Kind != graph.KindGenericParam || n.Language != "go" || n.Name == "" {
			continue
		}
		owner := enclosingFunctionForBinding(n.ID)
		if owner == "" || owner == n.ID {
			continue
		}
		set, ok := owned[owner]
		if !ok {
			set = map[string]string{}
			owned[owner] = set
		}
		if _, dup := set[n.Name]; dup {
			set[n.Name] = ""
			continue
		}
		set[n.Name] = n.ID
	}
	if len(owned) == 0 {
		return
	}
	var batch []graph.EdgeReindex
	for _, e := range r.fileOutEdges(filePath) {
		switch e.Kind {
		case graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeReturns, graph.EdgeInstantiates:
			if old := r.tryBindGenericParam(e, owned); old != "" {
				batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: old})
			}
		}
	}
	r.persistAttributionReindexes(batch)
}

// tryBindGenericParam returns the old To value (for batched reindex)
// when the edge was rewritten, or "" when left alone.
func (r *Resolver) tryBindGenericParam(e *graph.Edge, owned map[string]map[string]string) string {
	if e == nil {
		return ""
	}
	name, ok := genericParamTargetName(e.To)
	if !ok {
		return ""
	}
	ownerID := enclosingFunctionForBinding(e.From)
	if ownerID == "" {
		return ""
	}
	set := owned[ownerID]
	if len(set) == 0 {
		return ""
	}
	target, ok := set[name]
	if !ok || target == "" || target == e.To {
		return ""
	}
	oldTo := e.To
	e.To = target
	return oldTo
}

// genericParamTargetName is the exact target-shape gate shared by the light
// census and tryBindGenericParam. Repo-qualified unresolved targets remain
// excluded: type parameters are owned by the source function and use the bare
// unresolved::<name> representation.
func genericParamTargetName(to string) (string, bool) {
	if !strings.HasPrefix(to, "unresolved::") {
		return "", false
	}
	name := strings.TrimPrefix(to, "unresolved::")
	if name == "" || strings.ContainsAny(name, ".*:#") {
		return "", false
	}
	return name, true
}
