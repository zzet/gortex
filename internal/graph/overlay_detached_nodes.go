package graph

import "iter"

// OverlayDetachedNodeReader describes explicitly claimed, carried identities
// outside covered files. This capability alone makes no adjacency claim;
// legacy identity masks and explicit source masks retain independent behavior.
type OverlayDetachedNodeReader interface {
	// Summaries contain complete identity/location fields and may omit Meta.
	// They are owned copies, and enumeration never scans the lower corpus.
	DetachedNodeSummaries() iter.Seq[*Node]
	DetachedFileNodes(filePath string) []*Node
	DetachedRepoNodes(repoPrefix string) []*Node
}

func (v *OverlaidView) detachedNodeSummaries() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if v == nil || v.layer == nil {
			return
		}
		if rows, ok := v.layer.(OverlayDetachedNodeReader); ok {
			for node := range rows.DetachedNodeSummaries() {
				if node != nil && !yield(node) {
					return
				}
			}
		}
	}
}
