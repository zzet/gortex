package graph

// ScopedOutlineNodeReader bounds the two node-only lookups an outline needs:
// ranked analyzer membership and exact-name entry points. Neither method may
// answer from a physical store underneath a different selected view.
type ScopedOutlineNodeReader interface {
	OutlineNodeIDsInScope(NodeScope, []string) ([]string, error)
	OutlineNodesByName(NodeScope, string) ([]*Node, error)
}

var _ ScopedOutlineNodeReader = (*Graph)(nil)

func (g *Graph) OutlineNodeIDsInScope(scope NodeScope, ids []string) ([]string, error) {
	var out []string
	for id, n := range g.GetNodesByIDs(ids) {
		if n != nil && scope.Allows(n) {
			out = append(out, id)
		}
	}
	return out, nil
}
func (g *Graph) OutlineNodesByName(scope NodeScope, name string) ([]*Node, error) {
	var out []*Node
	for _, n := range g.FindNodesByName(name) {
		if n != nil && scope.Allows(n) {
			out = append(out, n)
		}
	}
	return out, nil
}
