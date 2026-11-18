package goanalysis

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/runtimeactivity"
)

// toolCallActivity is the process-wide activity kind an MCP tool call holds
// while it runs (see internal/mcp's beginMCPToolCall).
const toolCallActivity = "mcp"

// answerPathBusy names the answer-path work a module batch's listing or
// merge waits for, "" when none: foreground work of the daemon's view (an
// edit cycle holding the lane, an interactive build, a refresh ticket) or a
// tool call in flight. Without a foreground view the warm-up only yields to
// this provider's compiler loads, as before.
func (p *Provider) answerPathBusy() string {
	r := &p.warm
	r.mu.Lock()
	activity, toolCalls := r.activity, r.toolCalls
	r.mu.Unlock()
	if activity == nil {
		return ""
	}
	if busy, _ := activity.ForegroundWork(); busy != "" {
		return busy
	}
	if toolCalls == nil {
		toolCalls = func() int64 { return runtimeactivity.Current().ByKind[toolCallActivity] }
	}
	if toolCalls() > 0 {
		return "tool_call"
	}
	return ""
}

// pauseForAnswerPath holds a module batch's listing or merge (before names
// which) while answer-path work is in flight, polling at the warm-up's
// interval, and reports false once the provider closed. It runs outside
// the warm-up slot, so it holds nothing another warm-up or a pass waits for.
// Forced warm-ups pause too: forcing only stops a warm-up waiting for an
// idle daemon, never lets it list over an edit or a query. Targeted batches
// never pause (a pass may be waiting for them), and a pause never restarts
// anything: the queue, the plan and the merged packages are kept.
//
// One pause lasts at most maxPausePeriod (the maximum deferral unless set):
// an edit cycle or a query ends well within it, while a daemon that is never
// idle (other checkouts' builds back to back), a tool call waiting on the
// warm-up itself or a client keeping one call open must not stall the
// warm-up for good. It still yields at every batch boundary.
func (p *Provider) pauseForAnswerPath(e *checkoutWarmup, before string) bool {
	r := &p.warm
	var (
		started time.Time
		reason  string
	)
	for {
		r.mu.Lock()
		parent := r.ctx
		r.mu.Unlock()
		if parent.Err() != nil {
			return false
		}
		busy := p.answerPathBusy()
		if busy == "" || (!started.IsZero() && time.Since(started) >= r.maxPausePeriod()) {
			break
		}
		if started.IsZero() {
			started = time.Now()
			r.mu.Lock()
			e.status.Pauses++
			r.mu.Unlock()
		}
		if busy != reason {
			reason = busy
			r.mu.Lock()
			e.status.DeferredBy = busy
			r.notifyLocked()
			r.mu.Unlock()
		}
		timer := time.NewTimer(r.pollInterval())
		select {
		case <-parent.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	if started.IsZero() {
		return true
	}
	waited := time.Since(started)
	r.mu.Lock()
	e.status.PausedMs += waited.Milliseconds()
	e.status.DeferredBy = ""
	r.mu.Unlock()
	if p.logger != nil {
		p.logger.Info("go-types: checkout warm-up paused for answer-path work",
			zap.String("module_dir", e.loadDir),
			zap.String("before", before),
			zap.String("reason", reason),
			zap.Duration("paused", waited))
	}
	return true
}

// nextBatchTargeted reports whether e's next batch lists a targeted package
// (runWarmup lists those first, one at a time).
func (p *Provider) nextBatchTargeted(e *checkoutWarmup) bool {
	r := &p.warm
	r.mu.Lock()
	defer r.mu.Unlock()
	return e.targeted > 0
}

// maxPausePeriod bounds one pauseForAnswerPath wait.
func (r *warmupRegistry) maxPausePeriod() time.Duration {
	if r.maxPause > 0 {
		return r.maxPause
	}
	return r.maxDeferralPeriod()
}

// errWarmupListingStalled is the cause a background listing's context is
// canceled with when it outlives the stall deadline.
var errWarmupListingStalled = errors.New("goanalysis: background warm-up listing stalled")

// stallPeriod is how long a background listing may run (defaultWarmupStall
// unless set).
func (r *warmupRegistry) stallPeriod() time.Duration {
	if r.stall > 0 {
		return r.stall
	}
	return defaultWarmupStall
}

// backgroundListingDeadline returns the context a module batch's listing runs
// under and, for a background listing (armed), a report of whether it was
// canceled for outliving the stall deadline.
func (p *Provider) backgroundListingDeadline(ctx context.Context, armed bool) (context.Context, func() bool) {
	if !armed {
		return ctx, func() bool { return false }
	}
	listCtx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(p.warm.stallPeriod(), func() { cancel(errWarmupListingStalled) })
	return listCtx, func() bool {
		timer.Stop()
		stalled := errors.Is(context.Cause(listCtx), errWarmupListingStalled)
		cancel(nil)
		return stalled
	}
}
