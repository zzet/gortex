package store_sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRepoStatsContextCancellationAndParity(t *testing.T) {
	s := openScopedProjectionTestStore(t)
	s.AddNode(&graph.Node{ID: "fixture::main", RepoPrefix: "fixture", Kind: graph.KindFunction, Language: "go"})
	s.AddEdge(&graph.Edge{From: "fixture::main", To: "unresolved::callee", Kind: graph.EdgeCalls})
	stats, err := s.RepoStatsContext(context.Background())
	if err != nil || !reflect.DeepEqual(stats, s.RepoStats()) {
		t.Fatalf("stats parity: %v %v", stats, err)
	}
	if st := stats["fixture"]; st.TotalNodes != 1 || st.TotalEdges != 1 || st.ByKind["function"] != 1 || st.ByLanguage["go"] != 1 {
		t.Fatalf("authoritative row counts: %+v", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats, err = s.RepoStatsContext(ctx)
	if !errors.Is(err, context.Canceled) || stats != nil {
		t.Fatalf("canceled query published stats=%v err=%v", stats, err)
	}
}
