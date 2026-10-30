package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
)

type pairedState struct {
	p            *privateRun
	clients      []*socketClient
	sweepClients []*socketClient
	phase        *Phase
}

type pairedCoordinator struct {
	ctx        context.Context
	cancel     context.CancelFunc
	options    runOptions
	labels     []string
	group      string
	mu         sync.Mutex
	states     []*pairedState
	registered int
	started    bool
	finished   bool
	err        error
	firstReady chan struct{}
	done       chan struct{}
	orders     map[string]uint64
	records    []pairedRecord
	window     measurementWindow
	metrics    map[string]Metric
}

func newPairedCoordinator(ctx context.Context, o runOptions, labels []string, group string) (*pairedCoordinator, error) {
	if err := validateBuildLabels(labels); err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	c := &pairedCoordinator{ctx: child, cancel: cancel, options: o, labels: append([]string(nil), labels...), group: group, states: make([]*pairedState, len(labels)), firstReady: make(chan struct{}), done: make(chan struct{}), orders: make(map[string]uint64), metrics: make(map[string]Metric)}
	context.AfterFunc(child, func() { c.abort(child.Err()) })
	return c, nil
}

func (c *pairedCoordinator) abort(err error) {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	if c.err == nil {
		c.err = err
	} else if !errors.Is(c.err, err) {
		c.err = errors.Join(c.err, err)
	}
	if !c.started {
		c.finished = true
		close(c.done)
	}
	c.mu.Unlock()
	c.cancel()
}

func (c *pairedCoordinator) driver(index int) func(context.Context, *privateRun, []*socketClient, []*socketClient, *Phase) error {
	return func(ctx context.Context, p *privateRun, clients, sweepClients []*socketClient, phase *Phase) error {
		c.mu.Lock()
		if c.finished {
			err := c.err
			c.mu.Unlock()
			return err
		}
		if index < 0 || index >= len(c.states) || c.states[index] != nil {
			c.mu.Unlock()
			c.abort(errors.New("duplicate/invalid paired readiness registration"))
			<-c.done
			return c.resultError()
		}
		c.states[index] = &pairedState{p: p, clients: clients, sweepClients: sweepClients, phase: phase}
		c.registered++
		if c.registered == 1 {
			close(c.firstReady)
		}
		if c.registered == len(c.states) {
			c.started = true
			go c.execute()
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			c.abort(ctx.Err())
			<-c.done
		case <-c.done:
		}
		return c.resultError()
	}
}

func (c *pairedCoordinator) resultError() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

func (c *pairedCoordinator) execute() {
	var result error
	defer func() {
		c.mu.Lock()
		c.err = errors.Join(c.err, result)
		c.finished = true
		close(c.done)
		c.mu.Unlock()
	}()
	base := c.states[0]
	for _, state := range c.states[1:] {
		if !reflect.DeepEqual(base.phase.Operations, state.phase.Operations) || base.phase.SessionModel != state.phase.SessionModel || len(base.p.Repos) != len(state.p.Repos) {
			result = errors.New("paired builds have different normalized operation/session/repo plans")
			return
		}
		for i, repo := range base.p.Repos {
			a, b := repo.Corpus, state.p.Repos[i].Corpus
			a.Source, b.Source = "", ""
			if !reflect.DeepEqual(a, b) {
				result = errors.New("paired builds have different corpus commits/targets")
				return
			}
		}
	}
	samplers := make([]processSampler, len(c.states))
	monitors := make([]*phaseMonitor, len(c.states))
	for i, state := range c.states {
		samplers[i] = newProcessSampler()
		monitors[i] = startMonitor(state.p, samplers[i], "")
	}
	clientSampler := newProcessSampler()
	clientBefore := clientSampler.sample(os.Getpid())
	var groupBefore map[string]Metric
	if c.group != "" {
		groupBefore = cgroupMetrics(c.group, os.Getpagesize())
	}
	c.window = measurementWindow{Started: time.Now(), Method: "coupled native back-to-back requests; process counters bracket the common group window; per-build sample endpoints retained"}
	budget := c.options.Duration
	if base.phase.Name == "sweep" {
		budget = c.options.SweepBudget
	}
	ctx, cancel := context.WithTimeout(c.ctx, budget)
	defer cancel()
	if base.phase.Name == "sweep" {
		result = c.sweep(ctx)
	} else {
		result = c.agents(ctx)
	}
	c.window.Ended = time.Now()
	clientAfter := clientSampler.sample(os.Getpid())
	seconds := c.window.Ended.Sub(c.window.Started).Seconds()
	cpu := counterDelta(clientBefore["cpu_seconds"], clientAfter["cpu_seconds"])
	c.metrics["client_cpu_seconds"] = cpu
	c.metrics["client_busy_cpu_upper_bound_cores"] = unavailable("cores", cpu.Reason)
	if cpu.Value != nil && seconds > 0 {
		c.metrics["client_busy_cpu_upper_bound_cores"] = measured(*cpu.Value/seconds, "cores")
	}
	if c.group != "" {
		after := cgroupMetrics(c.group, os.Getpagesize())
		for _, key := range []string{"swap_in_bytes", "swap_out_bytes", "oom_kills", "oom_events"} {
			c.metrics["cgroup_"+key] = counterDelta(groupBefore[key], after[key])
		}
	}
	for i, state := range c.states {
		monitors[i].finish(state.phase)
		state.phase.Started, state.phase.DurationSec = c.window.Started, seconds
		for key, value := range state.phase.Metrics {
			if strings.HasPrefix(key, "client_") || strings.HasPrefix(key, "cgroup_") {
				state.phase.Metrics[key] = unavailable(value.Unit, "shared paired client/cgroup; see group_metrics")
			}
		}
		data, err := json.Marshal(struct {
			Group        measurementWindow `json:"group"`
			CounterStart time.Time         `json:"counter_start"`
			CounterEnd   time.Time         `json:"counter_end"`
		}{c.window, monitors[i].started, time.Now().UTC()})
		if err != nil {
			result = errors.Join(result, err)
		} else {
			state.phase.Gate["paired_measurement_window"] = data
		}
		finishEditMetrics(state.phase)
	}
}

type pairedCallResult struct {
	data   json.RawMessage
	err    error
	sample pairedSample
}

func (c *pairedCoordinator) callAcross(ctx context.Context, r request, repoIndex, agent, step, slot int, role string, timeout time.Duration, sweepIndex int) map[string]pairedCallResult {
	return c.callAcrossWithEnds(ctx, r, repoIndex, agent, step, slot, role, timeout, sweepIndex, nil)
}

func (c *pairedCoordinator) callAcrossWithEnds(ctx context.Context, r request, repoIndex, agent, step, slot int, role string, timeout time.Duration, sweepIndex int, ends map[string]time.Time) map[string]pairedCallResult {
	baseRepo := c.states[0].p.Repos[repoIndex]
	hash, hashErr := hashJSON(r)
	tuple := requestTuple{Tool: r.name(), Role: role, Repo: baseRepo.Corpus.Label, Commit: baseRepo.Corpus.Commit, Agent: agent, Step: step, Slot: slot, RequestHash: hash}
	c.mu.Lock()
	category := role + "." + r.name()
	tuple.Ordinal = c.orders[category]
	order := rotatingOrder(c.labels, tuple.Ordinal)
	c.orders[category]++
	c.mu.Unlock()
	tupleHash, _ := hashJSON(tuple)
	record := pairedRecord{Tuple: tuple, TupleHash: tupleHash, Order: order, Samples: make(map[string]pairedSample)}
	results := make(map[string]pairedCallResult)
	for _, label := range order {
		index := 0
		for c.labels[index] != label {
			index++
		}
		state := c.states[index]
		repo := state.p.Repos[repoIndex]
		sample := pairedSample{Repo: repo.Corpus.Label, Commit: repo.Corpus.Commit, RequestHash: hash, Started: time.Now(), Latency: unavailable("ms", "request was not dispatched"), Outcome: "phase_censored"}
		var data json.RawMessage
		var rows []Attempt
		err := hashErr
		budget := timeout
		if end, ok := ends[label]; ok {
			budget = time.Until(end)
		}
		if err != nil {
			sample.Outcome = "error"
		}
		if err == nil && budget <= 0 {
			err = context.DeadlineExceeded
			sample.Outcome = "product_deadline"
			if end, ok := ctx.Deadline(); (ok && end.Before(ends[label])) || ctx.Err() == context.Canceled {
				sample.Outcome = "phase_censored"
			}
			sample.Latency = unavailable("ms", "visibility deadline elapsed before probe dispatch")
		} else if err == nil && ctx.Err() == nil {
			client := state.clients[agent]
			if sweepIndex >= 0 {
				client = state.sweepClients[sweepIndex]
			}
			data, rows, err = callTool(ctx, client, r, dispatchArgs(r, repo), role, agent, step, slot, budget, waitContext)
			if len(rows) > 0 {
				last := rows[len(rows)-1]
				sample.Outcome, sample.Code, sample.Error = last.Outcome, last.Code, last.Error
				sample.Latency = measured(last.LogicalMS, "ms")
			}
		} else if err == nil {
			err = ctx.Err()
		}
		if err != nil && sample.Error == "" {
			sample.Error = err.Error()
		}
		sample.Ended = time.Now()
		record.Samples[label] = sample
		results[label] = pairedCallResult{data: data, err: err, sample: sample}
		c.mu.Lock()
		state.phase.Attempts = append(state.phase.Attempts, rows...)
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.records = append(c.records, record)
	c.mu.Unlock()
	return results
}

func (c *pairedCoordinator) agents(ctx context.Context) error {
	editing := c.states[0].phase.Name == "edit" || c.states[0].phase.Name == "many-edit"
	count := c.options.Agents
	if editing {
		count += c.options.Editors
	}
	var agents sync.WaitGroup
	for agent := range count {
		agents.Go(func() {
			repoIndex := agent % len(c.states[0].p.Repos)
			plan, _ := readingPlan(c.states[0].p.Repos[repoIndex].Corpus)
			pace := newPacing(profile(c.options.Profile), uint64(c.options.Seed)+uint64(agent))
			for step := 0; ctx.Err() == nil; step++ {
				if waitContext(ctx, pace.think()) != nil {
					return
				}
				if editing && agent >= c.options.Agents {
					c.edit(ctx, repoIndex, agent, step)
				}
				if ctx.Err() != nil {
					return
				}
				var burst sync.WaitGroup
				for slot := range pace.burst() {
					burst.Go(func() {
						r := plan[(agent+step*5+slot)%len(plan)]
						c.callAcross(ctx, r, repoIndex, agent, step, slot, "workload", c.options.Deadline, -1)
					})
				}
				burst.Wait()
			}
		})
	}
	agents.Wait()
	return c.ctx.Err()
}

func (c *pairedCoordinator) sweep(ctx context.Context) error {
	requests := c.states[0].phase.Operations
	pace := newPacing(profile(c.options.Profile), uint64(c.options.Seed))
	step := 0
	for range c.options.SweepRepetitions {
		for index := 0; index < len(requests); step++ {
			if err := waitContext(ctx, pace.think()); err != nil {
				return err
			}
			size := sweepBurst(len(requests)-index, pace)
			var burst sync.WaitGroup
			for slot := range size {
				operationIndex := index + slot
				burst.Go(func() {
					c.callAcross(ctx, requests[operationIndex], 0, 0, step, slot, "workload", c.options.Deadline, operationIndex)
				})
			}
			burst.Wait()
			index += size
		}
	}
	return nil
}

func (c *pairedCoordinator) edit(ctx context.Context, repoIndex, agent, step int) {
	corpus := c.states[0].p.Repos[repoIndex].Corpus
	marker := fmt.Sprintf("GortexLoadMarkerA%dS%d", agent, step)
	acks := c.callAcross(ctx, editRequest(corpus, marker), repoIndex, agent, step, 0, "workload", c.options.Deadline, -1)
	observations := make(map[string]EditObservation)
	ends := make(map[string]time.Time)
	var lastEnd time.Time
	for _, label := range c.labels {
		ack := acks[label]
		outcome := "never_visible"
		if ack.err != nil {
			outcome = "edit_error"
		}
		writeMS := float64(0)
		if ack.sample.Latency.Value != nil {
			writeMS = *ack.sample.Latency.Value
		}
		observations[label] = EditObservation{Repo: corpus.Label, Marker: marker, Started: ack.sample.Started, WriteMS: writeMS, VisibilityMS: unavailable("ms", "not observed"), Outcome: outcome, Acknowledgement: ack.data}
		ends[label] = ack.sample.Started.Add(c.options.Deadline)
		if ends[label].After(lastEnd) {
			lastEnd = ends[label]
		}
	}
	visibilityCtx, cancel := context.WithDeadline(ctx, lastEnd)
	defer cancel()
	query := request{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": marker}}
	for visibilityCtx.Err() == nil {
		results := c.callAcrossWithEnds(ctx, query, repoIndex, agent, step, 0, "probe", c.options.Deadline, -1, ends)
		allDone := true
		for _, label := range c.labels {
			observation := observations[label]
			if observation.Outcome == "edit_error" {
				continue
			}
			observation = observePairedVisibility(observation, results[label], marker)
			observations[label] = observation
			if observation.VisibilityMS.Value == nil && time.Now().Before(ends[label]) {
				allDone = false
			}
		}
		if allDone {
			break
		}
		if waitContext(visibilityCtx, 250*time.Millisecond) != nil {
			break
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, label := range c.labels {
		observation := observations[label]
		if observation.Outcome == "never_visible" {
			if end, ok := ctx.Deadline(); (ok && end.Before(ends[label])) || ctx.Err() == context.Canceled {
				observation.Outcome = "phase_censored"
			}
		}
		c.states[i].phase.Edits = append(c.states[i].phase.Edits, observation)
	}
}

func finishEditMetrics(phase *Phase) {
	var writes, visible []float64
	for _, edit := range phase.Edits {
		if edit.Outcome != "edit_error" {
			writes = append(writes, edit.WriteMS)
		}
		if edit.VisibilityMS.Value != nil {
			visible = append(visible, *edit.VisibilityMS.Value)
		}
		if edit.Outcome == "never_visible" {
			phase.NeverSeen++
		}
		if edit.Outcome == "phase_censored" {
			phase.Censored++
		}
	}
	phase.EditLatency, phase.Visibility = distribution(writes, "ms"), distribution(visible, "ms")
}

// Probes remain matched across builds even after one build is visible. Those
// later probe receipts are evidence only: they cannot replace its first success.
func observePairedVisibility(observation EditObservation, result pairedCallResult, marker string) EditObservation {
	if observation.Outcome == "edit_error" || observation.VisibilityMS.Value != nil || result.err != nil {
		return observation
	}
	if payload, err := textPayload(result.data, "content"); err == nil && containsIdentity(payload, marker) {
		observation.Outcome = "visible"
		observation.VisibilityMS = measured(float64(result.sample.Ended.Sub(observation.Started))/float64(time.Millisecond), "ms")
	}
	return observation
}
