package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

type coreEdgeTimingReads struct{ raw, full, checkedNodes, kinds int }

type coreEdgeTimingReader struct {
	graph.Store
	reads coreEdgeTimingReads
	err   error
}

func (r *coreEdgeTimingReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	r.reads.raw++
	return r.Store.GetOutEdgesByNodeIDs(ids)
}
func (r *coreEdgeTimingReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	r.reads.full++
	return r.Store.GetNodesByIDs(ids)
}
func (r *coreEdgeTimingReader) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.reads.checkedNodes++
	return r.Store.GetNodesByIDs(ids), nil
}
func (r *coreEdgeTimingReader) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	r.reads.kinds++
	if r.err != nil {
		return nil, r.err
	}
	return graph.GetNodeKindsByIDsContext(ctx, r.Store, ids)
}

func TestCoreEdgeTimingPreservesScoresReadsFilteringAndErrors(t *testing.T) {
	base := walkTestGraph(t)
	base.AddBatch([]*graph.Node{
		{ID: "contract", Kind: graph.KindContract}, {ID: "table", Kind: graph.KindTable},
	}, []*graph.Edge{
		{From: "a.go::A", To: "c.go::C", Kind: graph.EdgeReferences, Origin: graph.OriginLSPResolved, Confidence: 0.8, Meta: map[string]any{"evidence": "retained"}},
		{From: "a.go::A", To: "c.go::C", Kind: graph.EdgeCalls, Line: 9, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a.go::A", To: "contract", Kind: graph.EdgeProvides},
		{From: "a.go::A", To: "table", Kind: graph.EdgeProvides},
	})
	sentinel := errors.New("checked endpoint projection failed")
	for _, mode := range []string{"checked", "legacy", "checked_error"} {
		t.Run(mode, func(t *testing.T) {
			run := func(observed bool) (rerank.CentralityResult, map[string][]*graph.Edge, coreEdgeTimingReads, rerank.CentralityTiming, error) {
				ctx := t.Context()
				if mode != "legacy" {
					ctx = withContractCoreReadErrors(ctx)
				}
				reader := &coreEdgeTimingReader{Store: base}
				if mode == "checked_error" {
					reader.err = sentinel
				}
				srv := &Server{graph: reader}
				installContractCoreKindTestRuntime(t, srv)
				var timing rerank.CentralityTiming
				var observer func(rerank.CentralityTiming)
				if observed {
					observer = func(v rerank.CentralityTiming) { timing = v }
				}
				result := srv.boundedCentralityForRequestObserved(ctx, []string{"a.go::A"}, []string{"a.go::A", "b.go::B", "c.go::C"}, observer)
				// Separately exercise the metadata-bearing result: CSR construction
				// consumes only call/reference edges, not the full core adjacency.
				var batchTiming rerank.CoreEdgeTiming
				if observed {
					ctx = withCoreEdgeTiming(ctx, &batchTiming)
				}
				rows := srv.readerFor(ctx).GetOutEdgesByNodeIDs([]string{"a.go::A"})
				return result, rows, reader.reads, timing, contractCoreReadError(ctx)
			}
			plain, plainRows, reads, _, plainErr := run(false)
			got, rows, gotReads, timing, gotErr := run(true)
			require.Equal(t, plain, got)
			require.Equal(t, plainRows, rows)
			require.Equal(t, reads, gotReads, "observation must not add or change graph reads")
			require.Equal(t, timing.EdgeReads, timing.CoreEdges.RawReads, "the real readerFor wrapper must receive the request hook")
			require.Positive(t, timing.CoreEdges.RawReads)
			require.Equal(t, fmt.Sprintf("%T", &coreEdgeTimingReader{}), timing.CoreEdges.ReaderType)
			require.GreaterOrEqual(t, timing.EdgeRead, timing.CoreEdges.RawRead+timing.CoreEdges.Filter)
			require.GreaterOrEqual(t, timing.CoreEdges.Filter, timing.CoreEdges.EndpointLookup)
			require.Greater(t, timing.CoreEdges.EndpointIDs, timing.CoreEdges.EndpointDistinctIDs, "shared endpoints must be counted distinctly without changing the lookup input")
			if mode == "legacy" {
				require.Positive(t, timing.CoreEdges.LegacyClassifiers)
				require.Zero(t, timing.CoreEdges.CheckedClassifiers)
				require.Zero(t, gotReads.kinds)
			} else {
				require.Positive(t, timing.CoreEdges.CheckedClassifiers)
				require.Zero(t, timing.CoreEdges.LegacyClassifiers)
				require.Zero(t, gotReads.full)
			}
			if mode == "checked_error" {
				require.ErrorIs(t, plainErr, sentinel)
				require.ErrorIs(t, gotErr, sentinel, "derived observer context must retain the original sticky error holder")
				require.Empty(t, rows)
				require.Zero(t, timing.CoreEdges.KeptRows)
			} else {
				require.NoError(t, plainErr)
				require.NoError(t, gotErr)
				require.Len(t, rows["a.go::A"], 3, "calls/references and SQL provides remain; DI and contract ownership do not")
				for _, edge := range rows["a.go::A"] {
					if edge.Kind == graph.EdgeReferences {
						require.Equal(t, graph.OriginLSPResolved, edge.Origin)
						require.Equal(t, 0.8, edge.Confidence)
						require.Equal(t, "retained", edge.Meta["evidence"])
					}
				}
			}
		})
	}
	require.Nil(t, withCoreEdgeTiming(nil, &rerank.CoreEdgeTiming{}))
	ctx := t.Context()
	require.Same(t, ctx, withCoreEdgeTiming(ctx, nil))
	require.Nil(t, coreEdgeTimingFromContext(t.Context()))
	var combined rerank.CentralityTiming
	combined.Add(rerank.CentralityTiming{CoreEdges: rerank.CoreEdgeTiming{RawRead: 2500 * time.Microsecond, Filter: 3 * time.Millisecond, EndpointLookup: time.Millisecond, RawReads: 1}})
	fields := symbolCentralityTimingFields(combined)["core_edge_read"].(map[string]any)
	require.Equal(t, 2.5, fields["raw_read_ms"])
	require.Equal(t, 2.0, fields["filter_remaining_ms"])
	require.Equal(t, 1, fields["raw_reads"])
}
