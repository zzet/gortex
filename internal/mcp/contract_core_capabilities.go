package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// The contract-core wrapper filters adjacency and nothing else, so the
// optional capabilities a request may use split three ways:
//
//   - node- and content-only capabilities (kind and file scans, the content
//     index, enrichment sidecars, bounded name lookups) are the selected
//     reader's to grant or refuse; consumers probe them through
//     contractCoreSelectedReader, which keeps each request shape's answer
//     exactly what it was without the runtime;
//   - edge-reading capabilities are never resolved past the wrapper: they
//     read raw adjacency and would bypass the contract edge filter;
//   - capabilities the wrapper can honour itself are methods on it
//     (BindReadContext below, the variants in contract_core_scoped_projection.go).
//
// The capability table test pins the status of every optional interface the
// SQLite store implements.

// contractCore exposes the wrapper behind any of its capability variants.
func (r *contractCoreEdges) contractCore() *contractCoreEdges { return r }

type contractCoreWrapped interface{ contractCore() *contractCoreEdges }

// contractCoreSelectedReader returns the reader that answers node- and
// content-only optional capabilities for reader: the reader a contract-core
// wrapper selected, or reader itself. It never answers an edge-reading
// capability: probe those on reader, where the wrapper refuses them.
func contractCoreSelectedReader(reader graph.Reader) graph.Reader {
	if wrapped, ok := reader.(contractCoreWrapped); ok {
		return wrapped.contractCore().Reader
	}
	return reader
}

// SelectedVectorSearcher hands the query engine's cosine refinement the
// selected reader's stored vectors, keyed by node id, or nil when it has
// none. The wrapper never implements graph.VectorSearcher itself: that
// interface also writes and rebuilds the vector index.
func (r *contractCoreEdges) SelectedVectorSearcher() graph.VectorSearcher {
	vectors, _ := contractCoreSelectedReader(r).(graph.VectorSearcher)
	return vectors
}

// BindReadContext binds the selected reader to ctx and wraps the result again
// with the same contract set and edge timing, so an abandoned request stops
// paging while every read still passes the adjacency filter. A selected
// reader that cannot bind is wrapped unchanged; the wrapper's own scans
// still stop with ctx.
func (r *contractCoreEdges) BindReadContext(ctx context.Context) graph.Reader {
	if ctx == nil {
		ctx = r.ctx
	}
	return wrapContractCoreEdges(&contractCoreEdges{
		Reader:      graph.BindReadContext(r.Reader, ctx),
		ctx:         ctx,
		contractIDs: r.contractIDs,
		edgeTiming:  r.edgeTiming,
	})
}
