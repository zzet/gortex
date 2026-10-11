package store_sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func resumeConcurrentRetirementQuantum(parent context.Context, err error) bool {
	if parent.Err() != nil {
		return false
	}
	return errors.Is(err, ErrPayloadSweepBudgetExhausted) ||
		errors.Is(err, ErrPayloadRetirementWriteWanted) || errors.Is(err, context.DeadlineExceeded)
}

func TestConcurrentRetirementDriverResumesOnlyInternalDeadline(t *testing.T) {
	s, id := quantumPayloadFixture(t)
	parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	before := countAtGeneration(t, s, "nodes", id)
	s.writeMu.Lock()
	progress, err := s.RetirePayloadGenerationQuantum(parent, id, nil)
	s.writeMu.Unlock()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, parent.Err(), "only the internal quantum expired")
	require.Equal(t, PayloadRetirementProgress{}, progress)
	require.True(t, resumeConcurrentRetirementQuantum(parent, err))
	require.Equal(t, before, countAtGeneration(t, s, "nodes", id))
	require.False(t, resumeConcurrentRetirementQuantum(parent, errors.New("corruption")))
	require.False(t, resumeConcurrentRetirementQuantum(parent, context.Canceled))
	cancel()
	require.False(t, resumeConcurrentRetirementQuantum(parent, err), "parent cancellation must stop the driver")
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	require.False(t, resumeConcurrentRetirementQuantum(expired, context.DeadlineExceeded))
}
