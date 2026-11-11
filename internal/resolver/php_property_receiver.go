package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// phpPropertyCallTarget follows declaration identities, never short-name
// proximity. The boolean distinguishes a known (possibly conflicting) type
// from missing type information, which must retain the ordinary resolver path.
func (r *Resolver) phpPropertyCallTarget(e *graph.Edge, method string) (*graph.Node, bool) {
	repo := r.callerRepoPrefix(e)
	root := phpEdgeMetaString(e, "php_property_root")
	if e.Meta["php_property_ambiguous"] == true {
		return nil, true
	}
	if root == "" {
		return nil, false
	}
	current := r.phpUniquePropertyType(root, repo)
	if current == nil {
		return nil, true
	}
	for _, name := range strings.Split(phpEdgeMetaString(e, "php_property_path"), ".") {
		field, known := r.phpPropertyMember(current, name, graph.KindField, repo)
		if field == nil {
			return nil, known
		}
		if (scopeMetaString(field.Meta, "visibility") == "private" && field.FilePath != e.FilePath) || strings.ContainsAny(scopeMetaString(field.Meta, "field_type"), "|&") {
			return nil, true
		}
		current = nil
		for _, typed := range r.graph.GetOutEdges(field.ID) {
			if typed.Kind != graph.EdgeTypedAs {
				continue
			}
			if current != nil {
				return nil, true
			}
			current = r.phpPropertyEdgeType(typed, repo)
			if current == nil {
				return nil, true
			}
		}
		if current == nil {
			return nil, scopeMetaString(field.Meta, "field_type") != ""
		}
	}
	target, _ := r.phpPropertyMember(current, method, graph.KindMethod, repo)
	return target, true
}

func (r *Resolver) phpUniquePropertyType(fqn, repo string) *graph.Node {
	var match *graph.Node
	for _, n := range r.phpFindByFQN(strings.TrimPrefix(fqn, `\`), repo) {
		if !isTypeNodeKind(n.Kind) {
			continue
		}
		if match != nil {
			return nil
		}
		match = n
	}
	return match
}

func (r *Resolver) phpPropertyEdgeType(e *graph.Edge, repo string) *graph.Node {
	fqn := phpEdgeMetaString(e, "target_fqn")
	if fqn == "" {
		fqn = graph.UnresolvedName(e.To)
		if n := r.cachedGetNode(e.To); n != nil {
			fqn = n.Name
		}
	}
	return r.phpUniquePropertyType(fqn, repo)
}

// Declaration-local lookup follows the shared class/trait/parent precedence.
// Interfaces supply method contracts but no property storage. An incomplete
// chain must not turn a missing declaration into a name-only fallback.
func (r *Resolver) phpPropertyMember(owner *graph.Node, name string, kind graph.NodeKind, repo string) (*graph.Node, bool) {
	if owner == nil {
		return nil, true
	}
	chain, complete := r.phpAncestorChain(owner.ID)
	members := r.cachedFindNodesByNameInRepo(name, repo)
	for _, level := range chain {
		if level.kind == "interface" && kind == graph.KindField {
			continue
		}
		var match *graph.Node
		for _, id := range level.ids {
			typ := r.cachedGetNode(id)
			for _, n := range members {
				if n.Language != "php" || n.FilePath != typ.FilePath || phpNodeNamespace(n) != phpNodeNamespace(typ) || nodeReceiverType(n) != typ.Name {
					continue
				}
				// A declaration of the wrong kind shadows the inherited member too.
				if n.Kind != kind || (match != nil && match.ID != n.ID) {
					return nil, true
				}
				match = n
			}
		}
		if match != nil {
			return match, true
		}
	}
	return nil, !complete
}
