package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"time"
)

// Callback-local state for the existing paired dispatcher, not another queue,
// scheduler or request registry. Only edits and the seven oracles are paired.
type pairedEditProbeCall struct {
	runtime          *editProbeRuntime
	state            *editProbeState
	ctx              context.Context
	phaseContext     context.Context
	cancel           context.CancelFunc
	started          time.Time
	purpose          string
	editing, pending bool
}

// The existing two completion callbacks own this one per-edit cancellation.
// The first completed build cannot cancel its partner's outstanding probes.
type pairedEditProbeLifetime struct {
	mu           sync.Mutex
	finished     int
	cancel       context.CancelFunc
	evidenceOnce sync.Once
}

func (r *editProbeRuntime) preparePairedCall(ctx context.Context, role string, ordinal, slot int) (pairedEditProbeCall, error) {
	if ordinal < 0 || ordinal >= len(r.states) {
		return pairedEditProbeCall{}, errors.New("paired edit ordinal outside offered plan")
	}
	out := pairedEditProbeCall{runtime: r, state: r.states[ordinal], ctx: ctx}
	out.state.mu.Lock()
	if out.state.pairedContext != nil {
		ctx = out.state.pairedContext
	}
	out.state.mu.Unlock()
	out.ctx, out.phaseContext = ctx, ctx
	switch {
	case role == "workload" && slot == 0:
		out.editing, out.purpose = true, "edit"
		out.state, out.ctx, out.cancel, out.started = r.beginEdit(ctx, r.plan.Steps[ordinal])
	case role == "probe" && slot >= 10 && slot < 10+len(editProbeOracles(r.p.Repos[0].Corpus, r.plan.Steps[ordinal])):
		oracle := editProbeOracles(r.p.Repos[0].Corpus, r.plan.Steps[ordinal])[slot-10]
		out.purpose = "oracle:" + oracle.name
		out.pending = out.state.noteProbeStart()
	default:
		return pairedEditProbeCall{}, errors.New("unsupported paired edit-probe operation")
	}
	return out, nil
}
func (call pairedEditProbeCall) perform(req request, role string, stream int) (json.RawMessage, wireCallIdentity, []Attempt, error) {
	client := call.state.client
	if call.editing {
		client = call.runtime.editor
	}
	return call.runtime.performObserved(call.ctx, call.state, client, req, call.purpose, role, stream)
}
func (call pairedEditProbeCall) continueAfterACK(c *pairedCoordinator, data json.RawMessage, identity wireCallIdentity, err error) {
	if call.runtime == nil || !call.editing {
		return
	}
	c.editContinuations.Go(func() {
		defer call.state.stopProbes()
		defer call.cancel()
		call.runtime.finishEditAfterACK(call.phaseContext, call.ctx, call.state, call.state.step, call.started, data, identity, err)
	})
}

func (c *pairedCoordinator) preparePairedEditProbe() error {
	if len(c.states) != 2 {
		return errors.New("paired edit-and-probe needs baseline and candidate")
	}
	base := c.states[0].phase.editProbeRuntime
	if base == nil {
		return errors.New("paired baseline edit runtime unavailable")
	}
	for i, state := range c.states {
		r := state.phase.editProbeRuntime
		if r == nil || !reflect.DeepEqual(r.plan, base.plan) || r.options.Deadline != c.options.Deadline || r.requireAccounting != (i != 0) {
			return errors.New("paired edit plan/budget/accounting roles differ")
		}
		if r.p.Root == "" || r.p.Store == "" || r.p.Socket == "" || !inside(r.p.Root, r.p.Store) ||
			i > 0 && (r.p == base.p || r.p.Root == base.p.Root || r.p.Store == base.p.Store || r.p.Socket == base.p.Socket) {
			return errors.New("paired edit builds do not own independent roots/stores/sockets")
		}
		before, err := r.stats(c.ctx)
		state.phase.EditProbe.StatsBefore = before
		if err != nil {
			return err
		}
		state.phase.EditProbe.Limitations = append(state.phase.EditProbe.Limitations,
			"paired first-correct times are coupled probe observations, not independent 25ms freshness acceptance",
			"immediate own-ACK readbacks and receipt/status controls are unpaired and excluded from matched latency ratios",
			"absent baseline phases remain unavailable; candidate accounting stays required")
	}
	return nil
}

func (c *pairedCoordinator) editProbes(ctx context.Context) error {
	base := c.states[0].phase.editProbeRuntime
	for _, state := range c.states {
		r := state.phase.editProbeRuntime
		r.origin = c.window.Started
		state.phase.Started = r.origin
	}
	err := runEditProbeArrivals(ctx, base.plan, c.window.Started, c.activatePairedEditProbe, func(ctx context.Context, step editProbeStep) {
		c.callAcross(ctx, step.Request, 0, 0, step.Ordinal, 0, "workload", c.options.Deadline, -1)
	}, waitContext)
	c.editContinuations.Wait() // Each continuation joins its owned paired probes.
	var failures []error
	failures = append(failures, err)
	for _, state := range c.states {
		r := state.phase.editProbeRuntime
		failures = append(failures, r.finish())
		after, statsErr := r.stats(ctx)
		state.phase.EditProbe.StatsAfter = after
		failures = append(failures, statsErr)
	}
	return errors.Join(failures...)
}

// A shared oracle pair lives until both own-ACK continuations are done. Fast
// builds keep matching probes, but their first-correct endpoints never change.
// Activation only starts tasks; it never waits for a network response.
func (c *pairedCoordinator) activatePairedEditProbe(ctx context.Context, step editProbeStep) {
	lifetime := &pairedEditProbeLifetime{}
	var probeCtx context.Context
	probeCtx, lifetime.cancel = context.WithCancel(ctx)
	for _, state := range c.states {
		var once sync.Once
		done := func() {
			once.Do(func() {
				lifetime.mu.Lock()
				lifetime.finished++
				all := lifetime.finished == len(c.states)
				lifetime.mu.Unlock()
				if all {
					lifetime.cancel()
				}
			})
		}
		s := state.phase.editProbeRuntime.activateProbeState(step, done)
		s.mu.Lock()
		s.pairedContext = probeCtx
		s.mu.Unlock()
	}
	oracles := editProbeOracles(c.states[0].p.Repos[0].Corpus, step)
	for index, oracle := range oracles {
		for _, state := range c.states {
			state.phase.editProbeRuntime.states[step.Ordinal].probes.Add(1)
		}
		go func() {
			defer func() {
				for _, state := range c.states {
					state.phase.editProbeRuntime.states[step.Ordinal].probes.Done()
				}
			}()
			ticker := time.NewTicker(editProbePeriod)
			defer ticker.Stop()
			for probeCtx.Err() == nil {
				results := c.callAcross(probeCtx, oracle.call, 0, 0, step.Ordinal, 10+index, "probe", c.options.Deadline, -1)
				evidenceFailed := false
				for i, label := range c.labels {
					result := results[label]
					r := c.states[i].phase.editProbeRuntime
					if err := r.observeProbe(r.states[step.Ordinal], oracle, result.sample.Started, result.data, result.identity, result.err, result.pending); err != nil {
						evidenceFailed = true
					}
				}
				if evidenceFailed {
					lifetime.evidenceOnce.Do(func() {
						c.cancelPairedEditEvidence(step.Ordinal, lifetime.cancel)
					})
					return
				}
				select {
				case <-probeCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
}

func (c *pairedCoordinator) cancelPairedEditEvidence(ordinal int, cancel context.CancelFunc) {
	for _, state := range c.states {
		s := state.phase.editProbeRuntime.states[ordinal]
		s.mu.Lock()
		s.record.Errors = append(s.record.Errors, "paired probes canceled after partner evidence failure")
		s.mu.Unlock()
	}
	cancel()
}
