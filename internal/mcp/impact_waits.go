package mcp

import (
	"context"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/reach"
)

type impactWaitPhase struct {
	observed, attempted, acquired, detached int
	total                                   time.Duration
}

type impactCoordinationWaits struct {
	mu            sync.Mutex
	gate, resolve impactWaitPhase
}

func withImpactCoordinationWaits(ctx context.Context) (context.Context, *impactCoordinationWaits) {
	waits := new(impactCoordinationWaits)
	return reach.WithCoordinationWaitObserver(ctx, waits.observe), waits
}

func (w *impactCoordinationWaits) observe(wait reach.CoordinationWait) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var phase *impactWaitPhase
	switch wait.Phase {
	case "gate_wait":
		phase = &w.gate
	case "resolve_mutex_wait":
		phase = &w.resolve
	default:
		return
	}
	phase.observed++
	if !wait.Attempted {
		phase.detached++
		return
	}
	phase.attempted++
	phase.total += wait.Duration
	if wait.Acquired {
		phase.acquired++
	}
}

func impactWaitFields(phase impactWaitPhase) map[string]any {
	state := "not_observed"
	var ms any
	switch {
	case phase.attempted > phase.acquired:
		state = "censored"
		ms = float64(phase.total) / float64(time.Millisecond)
	case phase.attempted > 0:
		state = "observed"
		ms = float64(phase.total) / float64(time.Millisecond)
	case phase.detached > 0:
		state = "not_applicable_detached"
	}
	return map[string]any{"state": state, "total_ms": ms, "observations": phase.observed, "attempted": phase.attempted, "acquired": phase.acquired, "detached": phase.detached}
}

// Timing travels in the existing format-independent metadata envelope. The
// selected JSON/TOON/GCX payload is unchanged; no status registry is retained.
func attachImpactCoordinationWaits(result *mcp.CallToolResult, w *impactCoordinationWaits) *mcp.CallToolResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	return mergeResultMeta(result, map[string]any{"reach_coordination": map[string]any{"gate_wait": impactWaitFields(w.gate), "resolve_mutex_wait": impactWaitFields(w.resolve)}})
}
