package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.FileImporters      = (*baseGraphReader)(nil)
	_ graph.InEdgeCounter      = (*baseGraphReader)(nil)
	_ graph.BFSCapable         = (*baseGraphReader)(nil)
	_ graph.BFSAdmissionReader = (*baseGraphReader)(nil)
)

// The edge capabilities below are answered by this view, never by the corpus.
// The corpus's import join, kind count and recursive walk answer without the
// reference site the narrowing decides each edge by (a walk has also already
// passed through whatever it reached), so their answers cannot be narrowed
// afterwards. Each is instead recomputed over this reader's own lanes —
// GetInEdges, EdgesByKind, GetOutEdges and the node lookups — which apply
// exactly the predicate adjacency applies. That needs nothing from the corpus
// beyond the plain Reader, so a narrowed view serves them whatever its corpus
// implements, with the answer a capability-less caller would compute from the
// same lanes.

// FileImporters follows incoming imports from this file's identities through
// the narrowed adjacency: an importer written in, or resolving to, another
// repository is not one of this view's.
func (r *baseGraphReader) FileImporters(path string) []graph.FileImporterRow {
	// A background read is never cancelled, and the plain lanes it follows
	// report no error of their own.
	rows, _ := fileImportersFromReader(context.Background(), r, path)
	return rows
}

// InEdgeCountsByKind counts the incoming edges this view admits, per target,
// for the requested kinds. A count by kind has no anchor to start from, so it
// streams every edge of each requested kind from the corpus, every
// repository's included, and decides each through this view's predicate. That
// is the capability-less fallback's cost, not the corpus's single grouped
// query. Scoped consumers must instead count incoming rows of their bounded
// candidate identities.
func (r *baseGraphReader) InEdgeCountsByKind(kinds []graph.EdgeKind) map[string]int {
	return inEdgeCountsFromReader(r, kinds)
}

// BFS is the canonical hop-set walk over this view's adjacency, so no hop
// passes through another repository's node or a reference written in its
// files.
func (r *baseGraphReader) BFS(seeds []string, dir graph.Direction, kinds []graph.EdgeKind, depth, limit int) ([]graph.BFSHop, error) {
	return r.BFSContext(context.Background(), seeds, dir, kinds, depth, limit)
}

func (r *baseGraphReader) BFSContext(ctx context.Context, seeds []string, dir graph.Direction, kinds []graph.EdgeKind, depth, limit int) ([]graph.BFSHop, error) {
	return graph.BFSOf(ctx, r, seeds, dir, kinds, depth, limit)
}

// BFSAdmissionContext always declines, and advertising it is the point. A
// reader that offers BFSCapable alone moves the query engine's unscoped walks
// onto the hop-set path, which reports discovery edges only; declining
// admission keeps them on the Go walk they took before this view offered BFS,
// with its backedges, evidence order and caps. Nothing in the corpus can answer
// admission instead: its one-statement projection takes an edge filter with no
// term for this view's predicate.
func (r *baseGraphReader) BFSAdmissionContext(ctx context.Context, _ graph.BFSAdmissionRequest) (graph.BFSAdmissionSnapshot, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return graph.BFSAdmissionSnapshot{}, false, ctx.Err()
}
