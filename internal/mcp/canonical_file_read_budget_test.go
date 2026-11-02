package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type remainingFileReadBudget struct {
	graph.Store
	observed time.Time
}

func (r *remainingFileReadBudget) WithFileEditingContextReader(ctx context.Context, _ func(context.Context, graph.FileEditingContextRows) error) error {
	r.observed, _ = ctx.Deadline()
	<-ctx.Done()
	return ctx.Err()
}

func TestFileStoreScopeCannotRenewOriginalProjectionDeadline(t *testing.T) {
	deadline := time.Now().Add(10 * time.Millisecond)
	ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, deadline)
	reader := &remainingFileReadBudget{Store: graph.New()}
	consumed := false
	err := withFileStoreRows(ctx, reader, func(graph.FileEditingContextRows) error {
		consumed = true
		return nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, deadline, reader.observed, "a second 30ms store scope cannot extend the original remaining allowance")
	require.False(t, consumed, "an unavailable scope cannot supply partial canonical rows")
}
