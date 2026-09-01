package graph

import (
	"strings"

	"github.com/zzet/gortex/internal/testpath"
)

// NodeGetter is the single-node lookup NodeIsTest needs to resolve a
// child node's owner. Both graph stores and the overlay view satisfy it.
type NodeGetter interface {
	GetNode(id string) *Node
}

// NodeIsTest is the one test classifier shared by result filtering
// (QueryOptions.ExcludeTests) and output metadata (from_is_test,
// usage_summary.n_test_refs), so a row is never filtered as a test yet
// labeled as production. Authority order:
//
//  1. The node's own is_test stamp — set by the indexer's test-edge
//     pass on function/method symbols, whether discovered by file
//     location or by runner annotation (#[test], @Test).
//  2. For child nodes (params, locals, closures, type params — the
//     `<owner-id>#...` ID convention), the owner's stamp: an
//     annotation-discovered test in a production-looking path stamps
//     only the function, and its children inherit that identity.
//  3. The canonical path predicate — file-level nodes and any other
//     kind the stamping pass never visits.
//
// A nil getter skips the owner hop; the stamp and path checks still
// apply.
func NodeIsTest(g NodeGetter, n *Node) bool {
	if n == nil {
		return false
	}
	if v, _ := n.Meta["is_test"].(bool); v {
		return true
	}
	if i := strings.IndexByte(n.ID, '#'); i > 0 && g != nil {
		if owner := g.GetNode(n.ID[:i]); owner != nil {
			if v, _ := owner.Meta["is_test"].(bool); v {
				return true
			}
		}
	}
	return testpath.IsTestFile(n.FilePath)
}
