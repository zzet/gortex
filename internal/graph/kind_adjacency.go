package graph

import "context"

// KindAdjacency retains full edge payloads for the requested kinds and the
// size of the reader's complete adjacency. Total lets capped walkers keep
// their existing decision to sort before admission. A BFS admission snapshot
// may report a bounded upper bound instead, only where it cannot change that
// decision; see BFSAdmissionSnapshot.
type KindAdjacency struct {
	Edges []*Edge
	Total int
}

// KindAdjacencyReader narrows one visible adjacency that the reader already
// holds, such as a detached walk snapshot. It must preserve the ordinary
// GetOutEdges/GetInEdges order, metadata and masks; Total counts those same
// visible rows before the kind predicate.
//
// Stores and masking wrappers do not implement it: their prepared point
// lookup filtered here costs a fraction of a composite typed statement.
type KindAdjacencyReader interface {
	ReadKindAdjacency(context.Context, string, Direction, []EdgeKind) (KindAdjacency, error)
}

// ReadKindAdjacency keeps legacy readers on their own visible point lookup;
// it never unwraps an overlay or an adjacency filter.
func ReadKindAdjacency(ctx context.Context, reader Reader, id string, dir Direction, kinds []EdgeKind) (KindAdjacency, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return KindAdjacency{}, err
	}
	if reader == nil || id == "" {
		return KindAdjacency{}, nil
	}
	if typed, ok := reader.(KindAdjacencyReader); ok {
		rows, err := typed.ReadKindAdjacency(ctx, id, dir, kinds)
		if err != nil {
			return KindAdjacency{}, err
		}
		return rows, ctx.Err()
	}
	var edges []*Edge
	if dir == DirectionBackward {
		edges = reader.GetInEdges(id)
	} else {
		edges = reader.GetOutEdges(id)
	}
	out := KindAdjacency{Total: len(edges)}
	allowed := make(map[EdgeKind]bool, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = true
	}
	for _, edge := range edges {
		if edge != nil && allowed[edge.Kind] {
			out.Edges = append(out.Edges, edge)
		}
	}
	return out, ctx.Err()
}
