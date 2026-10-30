package store_sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestHotspotPreparationReadsCancelWhileWaitingForActualSQLConnection(t *testing.T) {
	reads := map[string]func(*Store, int64) (int, error){
		"candidate_ids": func(s *Store, _ int64) (int, error) {
			return len(s.NodeIDsByKinds([]graph.NodeKind{graph.KindFunction})), nil
		},
		"fan_counts": func(s *Store, _ int64) (int, error) {
			return len(s.NodeFanCounts([]string{"r/A", "r/B"}, []graph.EdgeKind{graph.EdgeCalls}, []graph.EdgeKind{graph.EdgeCalls})), nil
		},
		"crossings": func(s *Store, _ int64) (int, error) {
			return len(s.CommunityCrossingsByKind([]graph.EdgeKind{graph.EdgeCalls}, map[string]string{"r/A": "a", "r/B": "b"})), nil
		},
		"adjacency": func(s *Store, _ int64) (int, error) {
			count := 0
			for range s.EdgeAdjacencyForKinds([]graph.EdgeKind{graph.EdgeCalls}, []graph.NodeKind{graph.KindFunction}) {
				count++
			}
			return count, nil
		},
		"survivor_nodes": func(s *Store, _ int64) (int, error) { return len(s.GetNodesByIDs([]string{"r/A", "r/B"})), nil },
		"community_summaries": func(s *Store, generation int64) (int, error) {
			rows, _, err := s.ListAnalysisCommunitySummaries(generation, 8, "")
			return len(rows), err
		},
		"community_members": func(s *Store, generation int64) (int, error) {
			rows, _, err := s.AnalysisCommunityMembers(generation, "warm-community", 8, "")
			return len(rows), err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			s, _ := openTempStore(t)
			stopStoreBackgroundReaders(s)
			require.NoError(t, s.AddBatchChecked([]*graph.Node{
				{ID: "r/A", Name: "A", RepoPrefix: "r", Kind: graph.KindFunction},
				{ID: "r/B", Name: "B", RepoPrefix: "r", Kind: graph.KindFunction},
			}, []*graph.Edge{{From: "r/A", To: "r/B", Kind: graph.EdgeCalls}}))
			generation := buildMinimalAnalysisGeneration(t, s, "warm", 0, true)
			count, err := read(s, generation)
			require.NoError(t, err)
			require.Positive(t, count, "the control must read a valid nonempty fixture before cancellation")
			s.db.SetMaxOpenConns(1)
			conn, err := s.db.Conn(t.Context())
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				_ = conn.Close()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("actual SQL waiter was not joined")
				}
			})
			before := s.db.Stats().WaitCount
			var actual int
			var readErr error
			go func() { defer close(done); actual, readErr = read(s.WithReadContext(ctx), generation) }()
			require.Eventually(t, func() bool { return s.db.Stats().WaitCount > before }, 3*time.Second, time.Millisecond)
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("cancelled SQL read still waits behind the held connection")
			}
			require.Zero(t, actual)
			if readErr != nil {
				require.True(t, errors.Is(readErr, context.Canceled), "unexpected SQL error: %v", readErr)
			}
			require.NoError(t, conn.Close())
		})
	}
}
