package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// Only the bounded CSR consumes this adapter. Edges retain the independently
// request-bound full-row path; node rows answer existence, regardless of Kind.
type centralityPresenceReader struct {
	graph.Reader
	ctx      context.Context
	checked  graph.NodeKindsByIDsReader
	ids      graph.NodePresenceByIDsReader
	err      error
	observer func(graph.NodeKindReadTiming)
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
	ctx = graph.WithNodeKindReadObserver(ctx, r.observer)
	var present map[string]struct{}
	var kinds map[string]graph.NodeKindRow
	var err error
	if r.ids != nil {
		present, err = r.ids.GetNodePresenceByIDsContext(ctx, ids)
	} else {
		kinds, err = r.checked.GetNodeKindsByIDsContext(ctx, ids)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		r.err = err
		return nil
	}
	out := make(map[string]*graph.Node, len(present)+len(kinds))
	for id := range present {
		out[id] = answerPresentNode
	}
	for id := range kinds {
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

// The CSR alone may use physical ID presence through a core wrapper. This
// private hook binds its exact selected reader; it neither unwraps scoped views
// nor advertises the optional physical capability to general core callers.
func (r *contractCoreEdges) centralityNodePresence() graph.NodePresenceByIDsReader {
	read, _ := r.Reader.(graph.NodePresenceByIDsReader)
	return read
}

func centralityCheckedIDPresence(reader graph.Reader) graph.NodePresenceByIDsReader {
	if core, ok := reader.(interface {
		centralityNodePresence() graph.NodePresenceByIDsReader
	}); ok {
		return core.centralityNodePresence()
	}
	read, _ := reader.(graph.NodePresenceByIDsReader)
	return read
}
