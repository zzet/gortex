package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// Only the bounded CSR consumes this adapter. Edges retain the independently
// request-bound full-row path; node rows answer existence, regardless of Kind.
type centralityPresenceReader struct {
	graph.Reader
	ctx     context.Context
	checked graph.NodeKindsByIDsReader
	err     error
}

func (r *centralityPresenceReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	if r.err != nil {
		return nil
	}
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		r.err = err
		return nil
	}
	rows, err := r.checked.GetNodeKindsByIDsContext(ctx, ids)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		r.err = err
		return nil
	}
	out := make(map[string]*graph.Node, len(rows))
	for id := range rows {
		out[id] = answerPresentNode
	}
	return out
}

// Core wrappers retain legacy checked fallback methods for general callers.
// This optimization requires an actual compact selected-reader capability.
func (r *contractCoreEdges) centralityNodeKindsAvailable() bool {
	_, ok := r.Reader.(graph.NodeKindsByIDsReader)
	return ok
}
func centralityCheckedPresence(reader graph.Reader) (graph.NodeKindsByIDsReader, bool) {
	if core, ok := reader.(interface{ centralityNodeKindsAvailable() bool }); ok && !core.centralityNodeKindsAvailable() {
		return nil, false
	}
	checked, ok := reader.(graph.NodeKindsByIDsReader)
	return checked, ok
}
