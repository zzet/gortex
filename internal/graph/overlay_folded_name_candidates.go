package graph

import (
	"context"
	"fmt"
)

func (v *OverlaidView) FindFoldedNameCandidatesContext(ctx context.Context, needle string, limit int, filter NameSearchFilter) ([]*Node, bool, error) {
	ctx = nodeSearchContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if needle == "" || limit <= 0 {
		return nil, false, fmt.Errorf("graph: invalid folded-name candidate head")
	}
	set := NewFoldedNameCandidateSet(needle, limit)
	nodes := make(map[string]*Node)
	if v.layer != nil {
		for node := range v.layer.Nodes() {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			if node != nil && filter.Allows(compactNameNode(nodeSearchKey(node))) {
				set.Add(nodeSearchKey(node))
			}
		}
		// Retain only the overlay head's payloads, rather than every match.
		for _, key := range set.Keys() {
			nodes[key.ID] = v.layer.NodeByID(key.ID)
		}
	}
	baseMore := false
	if v.base != nil {
		base, ok := v.base.(FoldedNameCandidateReader)
		if !ok {
			return nil, false, fmt.Errorf("graph: selected base lacks coherent folded-name candidates")
		}
		baseFilter := filter
		baseFilter.Accept = func(node *Node) bool {
			return v.baseNodeVisible(node) && (v.layer == nil || !v.layer.IsNameRemoved(node.Name, node.ID)) && filter.Allows(node)
		}
		baseNodes, more, err := base.FindFoldedNameCandidatesContext(ctx, needle, limit, baseFilter)
		if err != nil {
			return nil, false, err
		}
		baseMore = more
		for _, node := range baseNodes {
			set.Add(nodeSearchKey(node))
			nodes[node.ID] = node
		}
	}
	out := make([]*Node, 0, limit)
	for _, key := range set.Keys() {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		node := nodes[key.ID]
		if node == nil || node.Name != key.Name {
			return nil, false, fmt.Errorf("graph: inconsistent folded-name overlay")
		}
		out = append(out, cloneDeltaNode(node))
	}
	return out, baseMore || set.More(), ctx.Err()
}

var _ FoldedNameCandidateReader = (*OverlaidView)(nil)
