package indexer

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

func TestContractLinkedCaptureStaleReopensWithoutAnotherPublication(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.captureErr = func(_ string, n int) error {
		if n == 1 {
			return graph.ErrContractProjectionStale
		}
		return nil
	}
	var fresh, released atomic.Int64
	p.c.options.LinkedInputs = func(ctx context.Context, _ ContractActor, _ string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		fresh.Add(1)
		return linkedSchedulerInputs([]string{"r"}, "k"), &ContractCaptureSource{onRelease: func() { released.Add(1) }}, ctx.Err()
	}
	epoch := p.c.InputEpoch()
	first := scheduleLinkedProbe(t, p, "k", &ContractCaptureSource{onRelease: func() { released.Add(1) }})
	status := p.wait(first)
	require.Equal(t, ContractActorPublished, status.Result, "the consumed stale view must not make the waiter poll the same ancestry")
	require.Equal(t, epoch, p.c.InputEpoch(), "reopening requires no unrelated publication event")
	require.EqualValues(t, 1, fresh.Load())
	require.EqualValues(t, 2, released.Load(), "initial and rematerialized handoffs close exactly once")
	require.Equal(t, []string{"r/wt@k"}, p.started(), "no body runs from the refused capture")
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	a := p.c.actors[first.Actor]
	require.Nil(t, a.want.source)
	require.Zero(t, a.running.seq)
	require.Empty(t, a.failedKey)
}

func TestContractLinkedCaptureStaleReopeningKeepsKeyMove(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.captureErr = func(key string, _ int) error {
		if key == "r/wt@old" {
			return graph.ErrContractProjectionStale
		}
		return nil
	}
	p.hold("r/wt@new")
	p.c.options.LinkedInputs = func(ctx context.Context, _ ContractActor, _ string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		return linkedSchedulerInputs([]string{"r"}, "new"), &ContractCaptureSource{}, ctx.Err()
	}
	first := scheduleLinkedProbe(t, p, "old", &ContractCaptureSource{})
	status := p.wait(first)
	require.Equal(t, ContractActorKeyMoved, status.Result, "new inputs must never publish as the old arrival")
	p.waitStarts(1)
	p.c.mu.Lock()
	seq := p.c.actors[first.Actor].want.seq
	require.Greater(t, seq, first.ArrivalSeq)
	require.Empty(t, p.c.actors[first.Actor].lastPublished.key)
	p.c.mu.Unlock()
	p.release("r/wt@new")
	first.ArrivalSeq = seq
	require.Equal(t, ContractActorPublished, p.wait(first).Result)
	require.Equal(t, []string{"r/wt@new"}, p.started())
}

func TestContractLinkedCaptureStaleWithoutAdapterStillResolvesStale(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.captureErr = func(string, int) error { return graph.ErrContractProjectionStale }
	var released atomic.Int64
	first := scheduleLinkedProbe(t, p, "k", &ContractCaptureSource{onRelease: func() { released.Add(1) }})
	require.Equal(t, ContractActorInputsStale, p.wait(first).Result)
	require.Empty(t, p.started())
	require.EqualValues(t, 1, released.Load())
}

func TestContractLinkedCaptureStaleReopenFailureDoesNotLoop(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.captureErr = func(string, int) error { return graph.ErrContractProjectionStale }
	var fresh, released atomic.Int64
	p.c.options.LinkedInputs = func(context.Context, ContractActor, string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		fresh.Add(1)
		return nil, &ContractCaptureSource{onRelease: func() { released.Add(1) }}, graph.ErrContractProjectionStale
	}
	first := scheduleLinkedProbe(t, p, "k", &ContractCaptureSource{onRelease: func() { released.Add(1) }})
	require.Equal(t, ContractActorInputsStale, p.wait(first).Result)
	require.EqualValues(t, 1, fresh.Load(), "a refused new selection parks; no retry loop")
	require.EqualValues(t, 2, released.Load())
	require.Empty(t, p.started())
}
