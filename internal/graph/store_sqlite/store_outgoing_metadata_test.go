package store_sqlite

import (
	"context"
	"errors"
	"github.com/zzet/gortex/internal/graph"
	"path/filepath"
	"testing"
)

func TestOutgoingMetadataProjectionPreservesDIAndGeneration(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	edge := &graph.Edge{From: "repo/config.ts::Service", To: "unresolved::TOKEN", Kind: graph.EdgeConsumes, FilePath: "repo/config.ts", Line: 9, Confidence: .9, Meta: map[string]any{"via": "@Inject", graph.MetaDIToken: "TOKEN"}}
	if err := s.AddBatchChecked(nil, []*graph.Edge{edge}); err != nil {
		t.Fatal(err)
	}
	other := s.AtGeneration(7)
	otherEdge := *edge
	otherEdge.Meta = map[string]any{"via": "@Inject", graph.MetaDIToken: "OTHER"}
	if err := other.AddBatchChecked(nil, []*graph.Edge{&otherEdge}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		store *Store
		token string
	}{{s, "TOKEN"}, {other, "OTHER"}} {
		rows, truncated, err := test.store.GetOutEdgesByNodeIDsWithMetadataContext(context.Background(), []string{edge.From}, 1)
		if err != nil || truncated || len(rows[edge.From]) != 1 {
			t.Fatalf("full outgoing=%#v truncated=%v err=%v", rows, truncated, err)
		}
		got := rows[edge.From][0]
		if got.Meta["via"] != "@Inject" || got.Meta[graph.MetaDIToken] != test.token || got.Confidence != edge.Confidence {
			t.Fatalf("metadata/generation lost=%#v", got)
		}
	}
	rows, truncated, err := s.GetOutEdgesByNodeIDsWithMetadataContext(context.Background(), []string{edge.From}, 0)
	if err != nil || !truncated || rows != nil {
		t.Fatalf("zero bound accepted=%#v %v %v", rows, truncated, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows, _, err = s.GetOutEdgesByNodeIDsWithMetadataContext(ctx, []string{edge.From}, 1)
	if !errors.Is(err, context.Canceled) || rows != nil {
		t.Fatalf("canceled projection returned rows=%#v err=%v", rows, err)
	}
}
