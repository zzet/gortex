package store_sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type callAncestorOracle struct {
	graph.Reader
	filter graph.OutlineEdgeFilter
}

func (r callAncestorOracle) GetInEdges(id string) []*graph.Edge {
	var out []*graph.Edge
	for _, edge := range r.Reader.GetInEdges(id) {
		if r.filter.Allows(edge, r.Reader.GetNode(edge.From), r.Reader.GetNode(edge.To)) {
			out = append(out, edge)
		}
	}
	return out
}

func TestCallAncestorsMatchGoOracleAndGeneration(t *testing.T) {
	base, derived := openGenerationReadPair(t)
	for _, s := range []*Store{base, derived} {
		for _, filter := range []graph.OutlineEdgeFilter{
			{},
			{ExcludeKinds: []graph.EdgeKind{graph.EdgeReferences}},
			{ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}},
			{ExcludeNodeIDs: map[string]bool{genReadShared: true}},
			{ExcludeVia: "spring.Bean"},
		} {
			ids := append(genReadProbeIDs(), genReadProbeIDs()...)
			got, err := s.CallAncestorsFilteredContext(t.Context(), ids, filter)
			require.NoError(t, err)
			want, err := graph.CallAncestorsOf(t.Context(), callAncestorOracle{Reader: s, filter: filter}, ids)
			require.NoError(t, err)
			require.ElementsMatch(t, want, got, "generation=%d filter=%+v", s.viewGen, filter)
		}
	}
}

func TestCallAncestorsUseIndexedClosureAndCancel(t *testing.T) {
	s := openCatalogStore(t)
	query := callAncestorsQuery(0, []string{"target"}, graph.OutlineEdgeFilter{ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}, ExcludeVia: "spring.Bean"})
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.sql, query.args...)
	require.NoError(t, err)
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "SEARCH e USING INDEX edges_by_to")
	require.Contains(t, joined, "kind=?")
	require.NotContains(t, joined, "SCAN e ")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.CallAncestorsContext(ctx, []string{"target"})
	require.True(t, errors.Is(err, context.Canceled))
}
