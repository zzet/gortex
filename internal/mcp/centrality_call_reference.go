package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type centralityCallReferenceRead func(context.Context, []string) (map[string][]*graph.Edge, error)

// This request-local adapter is installed only for non-stacked bounded CSR.
// General graph methods, writers, scoped readers and stacked memos keep their
// existing adjacency path. A failed checked batch cannot seed a PPR cache.
type centralityCallReferenceReader struct {
	graph.Reader
	ctx  context.Context
	read centralityCallReferenceRead
	err  error
}

func (r *centralityCallReferenceReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	if r.err != nil {
		return nil
	}
	rows, err := r.read(r.ctx, ids)
	if err == nil {
		err = r.ctx.Err()
	}
	if err != nil {
		r.err = err
		return nil
	}
	return rows
}

// The hook is private: embedding core wrappers does not falsely advertise a
// graph capability when the exact selected reader lacks it.
func (r *contractCoreEdges) centralityCallReferenceRead() centralityCallReferenceRead {
	checked, ok := r.Reader.(graph.CallReferenceOutgoingReader)
	if !ok {
		return nil
	}
	return func(ctx context.Context, ids []string) (map[string][]*graph.Edge, error) {
		var started time.Time
		if r.edgeTiming != nil {
			started = time.Now()
		}
		rows, err := checked.GetCallReferenceOutEdgesContext(ctx, ids)
		if t := r.edgeTiming; t != nil {
			if t.RawReads == 0 {
				t.ReaderType = fmt.Sprintf("%T", r.Reader)
			}
			t.RawRead += time.Since(started)
			t.RawReads++
			for _, edges := range rows {
				t.RawRows += len(edges)
			}
		}
		if err != nil {
			return nil, err
		}
		if r.edgeTiming != nil {
			started = time.Now()
		}
		filtered := r.filterBatch(rows)
		if t := r.edgeTiming; t != nil {
			t.Filter += time.Since(started)
			for _, edges := range filtered {
				t.KeptRows += len(edges)
			}
		}
		if err := contractCoreReadError(ctx); err != nil {
			return nil, err
		}
		return filtered, ctx.Err()
	}
}

func centralityCheckedCallReferences(ctx context.Context, reader graph.Reader) centralityCallReferenceRead {
	// Potential mutators retain the legacy reader/error semantics. Only the
	// checked read-only middleware owns the late-refusal error channel.
	if !hasContractCoreReadErrors(ctx) {
		return nil
	}
	if core, ok := reader.(interface {
		centralityCallReferenceRead() centralityCallReferenceRead
	}); ok {
		return core.centralityCallReferenceRead()
	}
	if checked, ok := reader.(graph.CallReferenceOutgoingReader); ok {
		return checked.GetCallReferenceOutEdgesContext
	}
	return nil
}
