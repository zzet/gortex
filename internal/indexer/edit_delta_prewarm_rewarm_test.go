package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// correctOneRow runs a real derived-row correction of generation: one
// derived edge changes, and the generation's correction epoch moves.
func correctOneRow(t *testing.T, store *store_sqlite.Store, generation int64) {
	t.Helper()
	ctx := context.Background()
	// A derived edge from a synthetic source: the correction's contract is
	// about the pass's rows, not about which nodes the generation holds.
	source := &graph.Node{ID: "rewarm_test.go::Source", FilePath: "rewarm_test.go", StartLine: 1}
	correction, err := store.BeginDerivedCorrection(ctx, store_sqlite.DerivedCorrectionRequest{
		GenerationID: generation, Pass: "rewarm_test", FromVersion: 0, ToVersion: 1,
		EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField},
	})
	if err != nil {
		t.Fatalf("BeginDerivedCorrection: %v", err)
	}
	if err := correction.ReplaceSourceEdges(ctx, []string{source.ID}, []*graph.Edge{
		{From: source.ID, To: source.ID, Kind: graph.EdgeAccessesField, FilePath: source.FilePath, Line: source.StartLine},
	}, nil); err != nil {
		t.Fatalf("ReplaceSourceEdges: %v", err)
	}
	if _, err := correction.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}
