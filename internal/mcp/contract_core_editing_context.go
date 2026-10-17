package mcp

import "github.com/zzet/gortex/internal/graph"

// contractCoreEditingContext serves the get_editing_context fast path through
// a contract-core wrapper. The selected reader's own FileEditingContext reads
// raw imports and calls, so the projection is recomputed over the wrapper's
// filtered adjacency instead: the same batched reads, with contract, bridge
// and config-key endpoints and every other edge the filter hides left out.
type contractCoreEditingContext struct{ reader graph.Reader }

func (c contractCoreEditingContext) FileEditingContext(filePath string, kinds []graph.NodeKind) *graph.FileEditingContextResult {
	return graph.FileEditingContextOf(c.reader, filePath, kinds)
}

// fileEditingContextFor resolves the editing-context fast path for a request
// reader. Through a contract-core wrapper it is offered only when the selected
// reader has one, so each request shape keeps the path it takes without the
// runtime.
func fileEditingContextFor(reader graph.Reader) (graph.FileEditingContext, bool) {
	if wrapped, ok := reader.(contractCoreWrapped); ok {
		if _, fast := wrapped.contractCore().Reader.(graph.FileEditingContext); !fast {
			return nil, false
		}
		return contractCoreEditingContext{reader: reader}, true
	}
	fc, ok := reader.(graph.FileEditingContext)
	return fc, ok
}
