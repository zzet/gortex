package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRetirementBoundedBudgetStillStopsForWriterAndDeadline(t *testing.T) {
	for _, reason := range []string{"writer", "deadline"} {
		t.Run(reason, func(t *testing.T) {
			l, gate, id := agedRetirementFixture(t)
			l.owed[id] = struct{}{}
			calls := 0
			l.retireQuantum = func(ctx context.Context, generationID int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
				calls++
				if calls == 1 {
					return store_sqlite.PayloadRetirementProgress{ChunksCommitted: 1, RowsDeleted: 16}, nil
				}
				if reason == "writer" {
					return store_sqlite.PayloadRetirementProgress{}, store_sqlite.ErrPayloadRetirementWriteWanted
				}
				<-ctx.Done()
				return store_sqlite.PayloadRetirementProgress{}, ctx.Err()
			}
			began := time.Now()
			_, pending, err := l.serveDeferredRetirementBurst(t.Context(), gate, []int64{id}, nil, nil)
			require.NoError(t, err)
			require.True(t, pending)
			require.Equal(t, 2, calls, "actual preemption must stop before a third quantum")
			require.False(t, gate.Stats().Active)
			require.Less(t, time.Since(began), time.Second)
		})
	}
}
