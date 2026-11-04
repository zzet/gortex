package graph

import (
	"context"
	"errors"
)

type contextOutgoingEdgesReader interface {
	GetOutEdgesByNodeIDsContext(context.Context, []string, int) (map[string][]*Edge, bool, error)
}

// This adapter changes only the base read of one existing overlay composition.
// The original generic Reader and its complete metadata surface are untouched.
type composedOutgoingContextBase struct {
	Reader
	ctx context.Context
	err error
}

func (r *composedOutgoingContextBase) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return nil
	}
	var rows map[string][]*Edge
	if contextual, ok := r.Reader.(contextOutgoingEdgesReader); ok {
		// A base limit before masking could hide the first surviving edge. Read
		// the complete base partition; the caller's cap is applied only afterward.
		var truncated bool
		rows, truncated, r.err = contextual.GetOutEdgesByNodeIDsContext(r.ctx, ids, int(^uint(0)>>1)-1)
		if r.err != nil {
			return nil
		}
		if truncated {
			r.err = errors.New("composed outgoing base was truncated before visibility filtering")
			return nil
		}
		// Legacy indexes may leave Origin empty and carry semantic_source only in
		// Meta. ProvenanceWeight still needs that field. Discard the entire light
		// answer rather than mixing partitions or silently changing legacy weights.
		legacy := false
		for _, edges := range rows {
			for _, edge := range edges {
				if edge != nil && edge.Origin == "" {
					legacy = true
					break
				}
			}
			if legacy {
				break
			}
		}
		if legacy {
			if err := r.ctx.Err(); err != nil {
				r.err = err
				return nil
			}
			rows = r.Reader.GetOutEdgesByNodeIDs(ids)
		}
	} else {
		rows = r.Reader.GetOutEdgesByNodeIDs(ids)
	}
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return nil
	}
	return rows
}

// GetOutEdgesByNodeIDsContext forwards the existing optional cancellable light
// adjacency capability through the same ordered overlay visibility algorithm.
// Explicit-Origin base rows may omit Meta, as on Store's contextual read;
// ambiguous legacy provenance retains the original complete base batch. The
// generic GetOutEdgesByNodeIDs always retains its full metadata semantics.
// Duplicate/empty IDs are ignored, input order determines the post-mask total
// cap, and no base truncation is mistaken for a complete composed answer.
func (v *OverlaidView) GetOutEdgesByNodeIDsContext(ctx context.Context, ids []string, limit int) (map[string][]*Edge, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return nil, false, nil
	}
	if limit <= 0 {
		return nil, true, nil
	}
	copyView := *v
	var wrapped *composedOutgoingContextBase
	if v.adjacencyBatch != nil {
		batch := *v.adjacencyBatch
		if batch.base != nil {
			wrapped = &composedOutgoingContextBase{Reader: batch.base, ctx: ctx}
			batch.base = wrapped
		}
		copyView.adjacencyBatch = &batch
	} else if v.base != nil {
		wrapped = &composedOutgoingContextBase{Reader: v.base, ctx: ctx}
		copyView.base = wrapped
	}
	rows := copyView.GetOutEdgesByNodeIDs(unique)
	if wrapped != nil && wrapped.err != nil {
		return nil, false, wrapped.err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	out := make(map[string][]*Edge, len(unique))
	total := 0
	for _, id := range unique {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		out[id] = rows[id][:0:0]
		for _, edge := range rows[id] {
			if total&127 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, false, err
				}
			}
			if total >= limit {
				return out, true, nil
			}
			out[id] = append(out[id], edge)
			total++
		}
	}
	return out, false, nil
}
