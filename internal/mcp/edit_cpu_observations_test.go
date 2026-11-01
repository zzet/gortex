package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func editCPUProjectionFixture() (*editCPUObservation, editCPUSample) {
	return &editCPUObservation{epoch: "test-server-epoch", baseline: editCPUSample{
		boundsKnown: true, finished: time.Millisecond, cpu: time.Second, available: true,
	}}, editCPUSample{
		boundsKnown: true, started: 2 * time.Millisecond, finished: 3 * time.Millisecond,
		cpu: 3 * time.Second, available: true,
		before: editOperationCountSnapshot{Completed: 2}, after: editOperationCountSnapshot{Completed: 2},
	}
}

func TestEditCPUProjectionStatesAndRawEvidence(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*editCPUObservation, *editCPUSample)
		reason string
		zero   bool
	}{
		{name: "clean process window"},
		{name: "known zero CPU", zero: true, change: func(b *editCPUObservation, c *editCPUSample) { b.baseline.cpu, c.cpu = 0, 0 }},
		{name: "no completed edits", reason: "no_completed_edits", change: func(_ *editCPUObservation, c *editCPUSample) {
			c.before, c.after = editOperationCountSnapshot{}, editOperationCountSnapshot{}
		}},
		{name: "baseline unsupported", reason: "baseline_cpu_unavailable", change: func(b *editCPUObservation, _ *editCPUSample) {
			b.baseline.available, b.baseline.reason = false, "unsupported"
		}},
		{name: "sample unavailable", reason: "cpu_unavailable", change: func(_ *editCPUObservation, c *editCPUSample) { c.available, c.reason = false, "sampling failed" }},
		{name: "unstable count", reason: "unstable_count_bracket", change: func(_ *editCPUObservation, c *editCPUSample) { c.before.Completed = 1 }},
		{name: "reversed sample clock", reason: "invalid_order", change: func(_ *editCPUObservation, c *editCPUSample) { c.finished = c.started - 1 }},
		{name: "CPU regression", reason: "invalid_order", change: func(_ *editCPUObservation, c *editCPUSample) { c.cpu = time.Millisecond }},
		{name: "count regression", reason: "invalid_count_order", change: func(_ *editCPUObservation, c *editCPUSample) { c.before.Completed = 3 }},
		{name: "baseline already counted", reason: "baseline_counts_not_zero", change: func(b *editCPUObservation, _ *editCPUSample) { b.baseline.after.Completed = 1 }},
		{name: "committed warning", reason: "non_clean_window", change: func(_ *editCPUObservation, c *editCPUSample) {
			c.before.CommittedWithError, c.after.CommittedWithError = 1, 1
		}},
		{name: "rollback is a subset", reason: "non_clean_window", change: func(_ *editCPUObservation, c *editCPUSample) {
			c.before.FailedAfterSourceEffect, c.after.FailedAfterSourceEffect = 1, 1
			c.before.RolledBack, c.after.RolledBack = 1, 1
		}},
		{name: "unknown comparison", reason: "non_clean_window", change: func(_ *editCPUObservation, c *editCPUSample) {
			c.before.UnknownSourceComparison, c.after.UnknownSourceComparison = 1, 1
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			baseline, current := editCPUProjectionFixture()
			if tt.change != nil {
				tt.change(baseline, &current)
			}
			got := projectEditProcessWindow(baseline, current)
			require.Equal(t, baseline.epoch, got.Epoch)
			require.Equal(t, current.before, got.Sample.CountsBefore)
			require.Equal(t, current.after, got.Sample.CountsAfter)
			require.Equal(t, baseline.baseline.after, got.Baseline.CountsAfter)
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(encoded, &wire))
			require.Contains(t, wire, "cpu_seconds_per_edit", "missing must be explicit null, never an omitted/zero average")
			if tt.reason != "" {
				require.Nil(t, got.CPUSecondsPerEdit)
				require.Nil(t, wire["cpu_seconds_per_edit"])
				require.Equal(t, "unavailable", got.AverageState)
				require.Equal(t, tt.reason, got.AverageReason)
			} else {
				require.NotNil(t, got.CPUSecondsPerEdit)
				want := 1.0 // (3s process CPU - 1s baseline) / 2 clean completed edits
				if tt.zero {
					want = 0
				}
				require.Equal(t, want, *got.CPUSecondsPerEdit)
				require.Equal(t, "measured_process_window", got.AverageState)
			}
			if !current.available {
				require.Nil(t, got.Sample.CPUSeconds)
				require.Equal(t, "sampling failed", got.Sample.CPUReason)
			}
		})
	}
}

func TestEditCPUCollectorBracketsExactlyOneSampleWithoutReset(t *testing.T) {
	s := &Server{}
	s.initializeEditCPUObservation(func() (time.Duration, error) { return time.Second, nil })
	epoch := s.editCPU.epoch
	calls := 0
	first := s.editProcessWindowSnapshot(func() (time.Duration, error) {
		calls++
		s.editOperations.completed.Add(1) // actual counter change during the CPU endpoint
		return 3 * time.Second, nil
	})
	require.Equal(t, 1, calls)
	require.Zero(t, first.Sample.CountsBefore.Completed)
	require.EqualValues(t, 1, first.Sample.CountsAfter.Completed)
	require.Equal(t, "unstable_count_bracket", first.AverageReason)
	require.Nil(t, first.CPUSecondsPerEdit)
	second := s.editProcessWindowSnapshot(func() (time.Duration, error) { calls++; return 3 * time.Second, nil })
	require.Equal(t, 2, calls)
	require.Equal(t, epoch, second.Epoch)
	require.EqualValues(t, 1, second.Sample.CountsBefore.Completed)
	require.Equal(t, second.Sample.CountsBefore, second.Sample.CountsAfter)
	require.Equal(t, 2.0, *second.CPUSecondsPerEdit)
	require.Zero(t, second.Baseline.CountsAfter.Completed)
	require.EqualValues(t, 1, s.editOperations.completed.Load())
}

func TestEditCPUWireCannotMutateStoredBaseline(t *testing.T) {
	s := &Server{}
	s.initializeEditCPUObservation(func() (time.Duration, error) { return time.Second, nil })
	s.editOperations.completed.Add(1)
	sample := func() (time.Duration, error) { return 2 * time.Second, nil }
	first := s.editProcessWindowSnapshot(sample)
	*first.Baseline.CPUSeconds = 1000
	*first.Baseline.StartedSeconds = -1000
	first.Baseline.CountsAfter.Completed = 1000
	*first.CPUSecondsPerEdit = 1000
	second := s.editProcessWindowSnapshot(sample)
	require.Equal(t, 1.0, *second.Baseline.CPUSeconds)
	require.GreaterOrEqual(t, *second.Baseline.StartedSeconds, 0.0)
	require.Zero(t, second.Baseline.CountsAfter.Completed)
	require.Equal(t, 1.0, *second.CPUSecondsPerEdit)
}

func TestEditCPUConstructorBaselineAndEpoch(t *testing.T) {
	first, _ := setupTestServer(t)
	second, _ := setupTestServer(t)
	require.NotNil(t, first.editCPU)
	require.NotNil(t, second.editCPU)
	require.NotEmpty(t, first.editCPU.epoch)
	require.NotEqual(t, first.editCPU.epoch, second.editCPU.epoch)
	require.Zero(t, first.editCPU.baseline.before)
	require.Zero(t, first.editCPU.baseline.after)
	require.True(t, first.editCPU.baseline.boundsKnown)
	if first.editCPU.baseline.available {
		require.GreaterOrEqual(t, first.editCPU.baseline.cpu, time.Duration(0))
	} else {
		require.NotEmpty(t, first.editCPU.baseline.reason, "unsupported/error must remain explicit")
	}
}

func TestEditCPULiteralAndUnavailableBaselineStayUnavailable(t *testing.T) {
	s := &Server{}
	got := s.editProcessWindowSnapshot(func() (time.Duration, error) { return time.Second, nil })
	require.Empty(t, got.Epoch)
	require.Nil(t, got.Baseline)
	require.Nil(t, got.Sample.StartedSeconds)
	require.Equal(t, "baseline_uninitialized", got.AverageReason)
	s.initializeEditCPUObservation(func() (time.Duration, error) { return 0, errors.New("test CPU unavailable") })
	s.editOperations.completed.Add(1)
	epoch := s.editCPU.epoch
	got = s.editProcessWindowSnapshot(func() (time.Duration, error) { return time.Second, nil })
	require.Equal(t, epoch, got.Epoch)
	require.Nil(t, got.Baseline.CPUSeconds)
	require.Equal(t, "test CPU unavailable", got.Baseline.CPUReason)
	require.NotNil(t, got.Sample.CPUSeconds, "raw later endpoint remains available")
	require.Nil(t, got.CPUSecondsPerEdit)
	require.Equal(t, "baseline_cpu_unavailable", got.AverageReason, "stats reads never replace a failed baseline")
}

func TestEditCPUStatsProcessScopeSurvivesResourceAndToolViews(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.editOperations.completed.Add(3) // projection scope fixture, not a real edit oracle
	var resource, tool editProcessWindow
	_, err := stack.readBoundResource(t, stack.worktreeRoot, "gortex://stats", func(ctx context.Context) ([]mcp.ResourceContents, error) {
		resource = stack.srv.buildGraphStatsPayload(ctx)["process_edit_window"].(editProcessWindow)
		return nil, nil
	})
	require.NoError(t, err)
	_, err = stack.callWithView(t, "", "graph_stats", nil, func(ctx context.Context) (*mcp.CallToolResult, error) {
		tool = stack.srv.buildGraphStatsPayload(ctx)["process_edit_window"].(editProcessWindow)
		return mcp.NewToolResultText(`{"ok":true}`), nil
	})
	require.NoError(t, err)
	require.Equal(t, "process_window", tool.Scope)
	require.Equal(t, "server_all_sessions_views", tool.CountScope)
	require.Equal(t, "whole_process_including_background", tool.CPUScope)
	require.Equal(t, resource.Epoch, tool.Epoch)
	require.Equal(t, resource.Baseline, tool.Baseline)
	require.EqualValues(t, 3, resource.Sample.CountsAfter.Completed)
	require.EqualValues(t, 3, tool.Sample.CountsAfter.Completed)
	// Live CPU/timestamps differ between sequential reads; graph-view equality
	// remains covered separately by TestResourceStatsEqualsTheGraphStatsTool.
}
