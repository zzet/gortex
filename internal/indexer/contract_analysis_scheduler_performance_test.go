//go:build performance

package indexer

import (
	"context"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graphview"
)

func TestContractScheduleReturnsWithinItsBudget(t *testing.T) {
	var elapsed []time.Duration
	scheduleWithCaptureBlocked(t, func(d time.Duration) { elapsed = append(elapsed, d) })
	sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
	require.Less(t, elapsed[len(elapsed)/2], 5*time.Millisecond)
}

func TestContractSchedulerServesAPanicWaiterWithinItsBudget(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	run := p.run
	var panicked atomic.Bool
	var panickedAt atomic.Int64
	p.c.run = func(job *contractAnalysisJob) string {
		if panicked.CompareAndSwap(false, true) {
			panickedAt.Store(time.Now().UnixNano())
			panic("injected job panic")
		}
		return run(job)
	}
	recoverPanicPin(t, p, "", nil, func() {
		require.Less(t, time.Since(time.Unix(0, panickedAt.Load())), 200*time.Millisecond)
	})
}

func TestContractSchedulerServesACapturePanicWaiterWithinItsBudget(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	var panicked atomic.Bool
	var panickedAt atomic.Int64
	p.c.options.Capture = func(ctx context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		if panicked.CompareAndSwap(false, true) {
			panickedAt.Store(time.Now().UnixNano())
			panic("injected capture panic")
		}
		return p.capture(ctx, source, inputs, repo, checkout)
	}
	recoverPanicPin(t, p, "wt", &ContractCaptureSource{}, func() {
		require.Less(t, time.Since(time.Unix(0, panickedAt.Load())), 200*time.Millisecond)
	})
}
