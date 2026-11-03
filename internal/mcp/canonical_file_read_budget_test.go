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

func TestFileStoreScopeCannotRenewOriginalCallerDeadline(t *testing.T) {
	deadline := time.Now().Add(10 * time.Millisecond)
	caller, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	projectionDeadline := time.Now().Add(-time.Second)
	ctx := context.WithValue(caller, projectedSourceDeadlineKey{}, projectionDeadline)
	allowance := &projectedSourceAllowance{tool: "get_file_summary"}
	allowance.attempts.Store(2)
	ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
	reader := &remainingFileReadBudget{Store: graph.New()}
	consumed := false
	err := withFileStoreRows(ctx, reader, func(graph.FileEditingContextRows) error {
		consumed = true
		return nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, deadline, reader.observed, "a 30ms canonical SQL scope cannot extend the caller's original hard deadline")
	require.Equal(t, projectionDeadline, ctx.Value(projectedSourceDeadlineKey{}))
	require.False(t, allowance.claim(), "canonical SQL cannot revive an exhausted projection allowance")
	require.EqualValues(t, 2, allowance.attempts.Load())
	require.False(t, consumed, "an unavailable scope cannot supply partial canonical rows")
}
