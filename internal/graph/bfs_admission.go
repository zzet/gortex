package graph

import "context"

// BFSAdmissionRequest describes the ordinary one-direction walk whose
// admission remains in the query engine. Dispatch and test-exclusion hooks
// keep their selected-reader walk instead of using this projection. The
// engine admits the seed against Scope before asking, so a projection keeps
// visible edges back to the seed without checking its scope again.
type BFSAdmissionRequest struct {
	Seed         string
	Direction    Direction
	Kinds        []EdgeKind
	Depth, Limit int
	Scope        NodeScope
}

// BFSAdmissionSnapshot carries the selected adjacency needed by Go's admission
// oracle. Nodes holds walked neighbors only. The caller supplies the seed, and
// unresolved or external targets, which admission never reads, are absent like
// missing neighbors; their edges remain evidence, including unresolved-target
// boundaries. Nodes may also hold in-repository neighbors outside the
// workspace or project scope, which admission rejects itself.
//
// Adjacency.Total is either the visible count saturated at Request.Limit, or
// an upper bound below Request.Limit-1-len(Nodes). Admission over the snapshot
// holds at most 1+len(Nodes) nodes and sorts before its cap once held nodes
// plus Total reach Limit, so both forms decide that sort the same way.
type BFSAdmissionSnapshot struct {
	Nodes     map[string]*Node
	Adjacency map[string]KindAdjacency
}

// BFSAdmissionReader is a selected-reader projection. Its presence also keeps
// the engine on the legacy Go admission contract if the projection declines:
// advertising BFSCapable must not replace its backedges with discovery edges.
type BFSAdmissionReader interface {
	BFSAdmissionContext(context.Context, BFSAdmissionRequest) (BFSAdmissionSnapshot, bool, error)
}

// FilteredBFSAdmissionReader is a physical indexed projection. An adjacency
// wrapper must supply its exact exclusions rather than expose this interface.
type FilteredBFSAdmissionReader interface {
	BFSAdmissionFilteredContext(context.Context, BFSAdmissionRequest, OutlineEdgeFilter) (BFSAdmissionSnapshot, bool, error)
}

func ReadBFSAdmissionContext(ctx context.Context, reader Reader, request BFSAdmissionRequest) (BFSAdmissionSnapshot, bool, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return BFSAdmissionSnapshot{}, false, err
	}
	if selected, ok := reader.(BFSAdmissionReader); ok {
		return selected.BFSAdmissionContext(ctx, request)
	}
	if physical, ok := reader.(FilteredBFSAdmissionReader); ok {
		return physical.BFSAdmissionFilteredContext(ctx, request, OutlineEdgeFilter{})
	}
	return BFSAdmissionSnapshot{}, false, nil
}

// BFSAdmissionBudgets bound materialized recursive states and emitted edges.
// An overflow is a declined optimization, never a complete partial graph.
// Endpoint/kind index plans and clean latency remain separate evidence of the
// internal SQL work; a result-row limit alone does not establish that cost.
func BFSAdmissionBudgets(request BFSAdmissionRequest) (states, edges int, supported bool) {
	if request.Seed == "" || request.Depth <= 0 || request.Depth > 16 || request.Limit <= 0 || request.Limit > 256 || len(request.Kinds) == 0 {
		return 0, 0, false
	}
	return max(256, request.Limit*4), max(512, request.Limit*16), true
}

// The embedded Reader supplies empty answers for methods outside a walk's
// identity/adjacency contract. It is immutable and holds no request data.
var emptyBFSAdmissionReader Reader = New()

type bfsAdmissionSnapshotReader struct {
	Reader
	snapshot  BFSAdmissionSnapshot
	seed      *Node
	direction Direction
}

func NewBFSAdmissionReader(snapshot BFSAdmissionSnapshot, seed *Node, dir Direction) Reader {
	return &bfsAdmissionSnapshotReader{Reader: emptyBFSAdmissionReader, snapshot: snapshot, seed: seed, direction: dir}
}
func (r *bfsAdmissionSnapshotReader) GetNode(id string) *Node {
	if r.seed != nil && id == r.seed.ID {
		return r.seed
	}
	return r.snapshot.Nodes[id]
}
func (r *bfsAdmissionSnapshotReader) GetNodesByIDs(ids []string) map[string]*Node {
	out := make(map[string]*Node, len(ids))
	for _, id := range ids {
		if node := r.GetNode(id); node != nil {
			out[id] = node
		}
	}
	return out
}
func (r *bfsAdmissionSnapshotReader) GetOutEdges(id string) []*Edge {
	if r.direction != DirectionBackward {
		return append([]*Edge(nil), r.snapshot.Adjacency[id].Edges...)
	}
	return nil
}
func (r *bfsAdmissionSnapshotReader) GetInEdges(id string) []*Edge {
	if r.direction == DirectionBackward {
		return append([]*Edge(nil), r.snapshot.Adjacency[id].Edges...)
	}
	return nil
}
func (r *bfsAdmissionSnapshotReader) ReadKindAdjacency(ctx context.Context, id string, dir Direction, kinds []EdgeKind) (KindAdjacency, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return KindAdjacency{}, err
	}
	if dir != r.direction {
		return KindAdjacency{}, nil
	}
	allowed := make(map[EdgeKind]bool, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = true
	}
	stored := r.snapshot.Adjacency[id]
	out := KindAdjacency{Total: stored.Total}
	for _, edge := range stored.Edges {
		if allowed[edge.Kind] {
			out.Edges = append(out.Edges, edge)
		}
	}
	return out, nil
}
