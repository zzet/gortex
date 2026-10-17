package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func callReferenceRows(rows map[string][]*graph.Edge) map[string][]*graph.Edge {
	out := make(map[string][]*graph.Edge)
	for id, edges := range rows {
		for _, edge := range edges {
			if edge.Kind == graph.EdgeCalls || edge.Kind == graph.EdgeReferences {
				out[id] = append(out[id], edge)
			}
		}
	}
	return out
}

func TestCallReferenceOutgoingFullParityChunksAndGeneration(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "calls.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	var ids []string
	var edges []*graph.Edge
	for i := 0; i < lookupChunkSize+3; i++ {
		id := fmt.Sprintf("repo/f.go::F%05d", i)
		ids = append(ids, id)
		// Line order interleaves kinds and must not become kind-first order.
		for line, kind := range []graph.EdgeKind{graph.EdgeReferences, graph.EdgeReturnsTo, graph.EdgeCalls, graph.EdgeArgOf, graph.EdgeReferences} {
			edges = append(edges, &graph.Edge{From: id, To: fmt.Sprintf("repo/g.go::T%d", line), Kind: kind, FilePath: "repo/f.go", Line: line + 1, Confidence: .8123456789, ConfidenceLabel: "high", Origin: graph.OriginASTResolved, Tier: "ast", CrossRepo: true, Meta: map[string]any{"via": "test", "evidence": "full metadata", "resolve_terminal": true, "resolve_terminal_reason": "accepted", "semantic_source": "test-provider"}})
		}
	}
	// Same-line ties preserve kind and row identity order, including two
	// same-kind destinations inserted in reverse lexical order.
	edges = append(edges,
		&graph.Edge{From: ids[0], To: "repo/g.go::TieCall", Kind: graph.EdgeCalls, FilePath: "repo/f.go", Line: 1, Meta: map[string]any{"via": "tie"}},
		&graph.Edge{From: ids[0], To: "repo/g.go::TieZ", Kind: graph.EdgeReferences, FilePath: "repo/f.go", Line: 1, Meta: map[string]any{"via": "tie-z"}},
		&graph.Edge{From: ids[0], To: "repo/g.go::TieA", Kind: graph.EdgeReferences, FilePath: "repo/f.go", Line: 1, Meta: map[string]any{"via": "tie-a"}},
	)
	require.NoError(t, s.AddBatchChecked(nil, edges))
	positive := s.AtGeneration(7)
	other := *edges[0]
	other.Meta = map[string]any{"via": "positive", "evidence": "selected generation"}
	require.NoError(t, positive.AddBatchChecked(nil, []*graph.Edge{&other}))
	ids = append(ids, "", ids[0], ids[0], "missing")
	for _, selected := range []*Store{s, positive} {
		want := callReferenceRows(selected.GetOutEdgesByNodeIDs(ids))
		if selected == s {
			require.Len(t, want[ids[0]], 6)
		} else {
			require.Len(t, want[ids[0]], 1)
		}
		got, err := selected.GetCallReferenceOutEdgesContext(t.Context(), ids)
		require.NoError(t, err)
		require.Equal(t, want, got, "all fields, promoted metadata, duplicate suppression and per-anchor order must match")
	}
	// A healthy deferred-index window retains the original query/order.
	_, err = s.writerDB.Exec(`DROP INDEX edges_by_from_line_kind`)
	require.NoError(t, err)
	want := callReferenceRows(s.GetOutEdgesByNodeIDs(ids))
	got, err := s.GetCallReferenceOutEdgesContext(t.Context(), ids)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestCallReferenceOutgoingCancellationAndFailureHaveNoPartialRows(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "errors.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.AddBatchChecked(nil, []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 1}, {From: "a", To: "c", Kind: graph.EdgeReferences, Line: 2}}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := s.GetCallReferenceOutEdgesContext(ctx, []string{"a"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, got)
	got, err = s.GetCallReferenceOutEdgesContext(nil, []string{"a"}) //nolint:staticcheck // SA1012: Verify explicit nil-context rejection.
	require.Error(t, err)
	require.Nil(t, got)
	// The second selected row fails after a valid row has been decoded.
	_, err = s.writerDB.Exec(`UPDATE edges SET meta=? WHERE from_id='a' AND line=2`, []byte{0xff, 0xfe})
	require.NoError(t, err)
	got, err = s.GetCallReferenceOutEdgesContext(t.Context(), []string{"a"})
	require.Error(t, err)
	require.Nil(t, got)
	require.NoError(t, s.Close())
	got, err = s.GetCallReferenceOutEdgesContext(t.Context(), []string{"a"})
	require.Error(t, err)
	require.Nil(t, got)
}

func TestCallReferenceOutgoingActualDeferredBulkIndexParity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "deferred.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	s.BeginBulkLoad()
	require.True(t, s.bulkIndexesDeferred, "fixture must enter the actual empty-store bulk window")
	require.NoError(t, s.AddBatchChecked(nil, []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeReferences, Line: 1, Meta: map[string]any{"via": "kept"}},
		{From: "a", To: "c", Kind: graph.EdgeReturnsTo, Line: 2},
		{From: "a", To: "d", Kind: graph.EdgeCalls, Line: 3, Origin: graph.OriginASTResolved},
	}))
	want := callReferenceRows(s.GetOutEdgesByNodeIDs([]string{"a"}))
	require.Len(t, want["a"], 2, "bulk writes must be visible to the independent read connection")
	got, err := s.GetCallReferenceOutEdgesContext(t.Context(), []string{"a"})
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.NoError(t, s.FlushBulk())
}

func TestCallReferenceOutgoingUsesOrderPreservingIndex(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "plan.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+callReferenceOutgoingSQL(1, true), "a", int64(0), string(graph.EdgeCalls), string(graph.EdgeReferences))
	require.NoError(t, err)
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plans = append(plans, detail)
	}
	require.NoError(t, rows.Err())
	require.Contains(t, strings.Join(plans, "\n"), "edges_by_from_line_kind")
	require.NotContains(t, strings.Join(plans, "\n"), "TEMP B-TREE", "projection must retain index order instead of adding a sorter")
}
