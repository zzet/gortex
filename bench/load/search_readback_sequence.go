package main

import (
	"context"
	"sync"
	"time"
)

type searchReadbackTurn struct {
	previous <-chan struct{}
	done     chan struct{}
	once     sync.Once
}

// The offered plan remains absolute/open-loop. Only this acceptance profile's
// actual editor dispatch queues behind that editor's preceding four searches.
// Independent editors and post-search diagnostic work never share this turn.
func (r *editProbeRuntime) waitSearchReadbackTurn(ctx context.Context, step editProbeStep) (func(), bool) {
	if !r.plan.SearchReadbackAcceptance {
		return func() {}, true
	}
	r.searchTurnsOnce.Do(func() {
		r.searchTurns = make(map[int]*searchReadbackTurn, len(r.plan.Steps))
		ready := make(chan struct{})
		close(ready)
		last := make(map[int]<-chan struct{})
		for _, offered := range r.plan.Steps {
			previous := last[offered.Editor]
			if previous == nil {
				previous = ready
			}
			turn := &searchReadbackTurn{previous: previous, done: make(chan struct{})}
			r.searchTurns[offered.Ordinal] = turn
			last[offered.Editor] = turn.done
		}
	})
	state := r.states[step.Ordinal]
	queued := time.Now()
	ns := queued.Sub(r.origin).Nanoseconds()
	state.mu.Lock()
	state.record.SearchQueuedNS = &ns
	state.mu.Unlock()
	release := func() { r.releaseSearchReadbackTurn(step) }
	turn := r.searchTurns[step.Ordinal]
	select {
	case <-turn.previous:
	case <-ctx.Done():
	}
	waited := time.Since(queued).Nanoseconds()
	state.mu.Lock()
	state.record.SearchQueueWaitNS = &waited
	if err := ctx.Err(); err != nil {
		state.record.Outcome = "phase_censored"
		state.record.Errors = append(state.record.Errors, "search editor queue: "+err.Error())
		state.mu.Unlock()
		release()
		state.signalAck()
		state.stopProbes()
		return func() {}, false
	}
	state.mu.Unlock()
	return release, true
}

func (r *editProbeRuntime) releaseSearchReadbackTurn(step editProbeStep) {
	if !r.plan.SearchReadbackAcceptance {
		return
	}
	r.searchTurns[step.Ordinal].once.Do(func() {
		ns := time.Since(r.origin).Nanoseconds()
		state := r.states[step.Ordinal]
		state.mu.Lock()
		state.record.SearchTurnReleasedNS = &ns
		state.mu.Unlock()
		close(r.searchTurns[step.Ordinal].done)
	})
}
