package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Worktree step mode measures what each linked worktree costs a daemon.
//
// One daemon starts with the primary and the first step's linked worktrees,
// then grows through each later count on the same process. At every count it
// serves the reading agents for --duration from fresh sessions spread over
// every checkout, then sits idle for --duration. Each idle window follows a
// load window of its own count and opens with one garbage collection, so
// every idle level holds what the checkouts retain after use rather than the
// load's garbage or a first load's one-off growth. The gauge series runs
// throughout; each load window keeps a heap profile from its start and from
// its highest sampled footprint, and each idle window one taken right after
// its collection. The change in load peak and in idle level between counts,
// divided by the worktrees added, is the cost of one checkout coordinator.

// stepIdleOverride, when set, replaces --duration as the length of each
// step's idle window. Tests set it: an idle window needs no workload calls,
// so it need not last as long as a load window.
var stepIdleOverride time.Duration

type worktreeStep struct {
	LinkedWorktrees         int                    `json:"linked_worktrees"`
	LoadStarted             time.Time              `json:"load_started"`
	LoadEnded               time.Time              `json:"load_ended"`
	IdleStarted             time.Time              `json:"idle_started"`
	IdleEnded               time.Time              `json:"idle_ended"`
	Load                    map[string]Metric      `json:"load_metrics"`
	Tools                   map[string]ToolMetrics `json:"tools"`
	HeapLoadStart           string                 `json:"heap_profile_load_start"`
	HeapLoadPeak            string                 `json:"heap_profile_load_peak"`
	HeapLoadPeakUnavailable string                 `json:"heap_profile_load_peak_unavailable,omitempty"`
	HeapIdle                string                 `json:"heap_profile_idle"`
}

// bindMemoryFlags registers the standalone memory-diagnosis options. Paired
// runs do not bind them: a coupled comparison measures latency between builds.
func bindMemoryFlags(f *flag.FlagSet, o *runOptions) {
	f.BoolVar(&o.Gauges, "gauges", false, "record a 10s series of daemon_health gauges with external process memory (always on with --worktree-steps)")
	f.BoolVar(&o.HeapProfiles, "heap-profiles", false, "with --gauges, also save daemon heap profiles at load start and at the highest sampled footprint; peak captures run inside the measured load window (always on with --worktree-steps)")
	f.Func("worktree-steps", "worktrees scenario: ascending linked-worktree counts ending at --worktrees, e.g. 0,1,2,4; one daemon is loaded then idle for --duration at each", func(value string) error {
		o.WorktreeSteps = nil
		for field := range strings.SplitSeq(value, ",") {
			count, err := strconv.Atoi(strings.TrimSpace(field))
			if err != nil || count < 0 || len(o.WorktreeSteps) > 0 && count <= o.WorktreeSteps[len(o.WorktreeSteps)-1] {
				return fmt.Errorf("worktree steps must be ascending nonnegative counts: %q", value)
			}
			o.WorktreeSteps = append(o.WorktreeSteps, count)
		}
		return nil
	})
}

func validateMemoryOptions(o runOptions) error {
	if o.Scenario == startupScenario && wantsGaugeSeries(o) {
		return errors.New("--gauges and --heap-profiles are unsupported for startup-probe")
	}
	if o.Scenario == "idle-stop" && wantsGaugeSeries(o) {
		return errors.New("live memory gauges and profiles are unsupported for idle-stop: the extra health session prevents an idle interval")
	}
	if len(o.WorktreeSteps) == 0 {
		if o.HeapProfiles && !o.Gauges {
			return errors.New("--heap-profiles needs --gauges: the peak profile is chosen from the gauge series")
		}
		return nil
	}
	if o.Scenario != "worktrees" || o.WorktreeSteps[len(o.WorktreeSteps)-1] != o.Worktrees {
		return errors.New("--worktree-steps applies to the worktrees scenario and must end at --worktrees")
	}
	if o.CPUProfile != 0 {
		return errors.New("worktree step mode takes heap profiles per step, not --cpu-profile")
	}
	return nil
}

// bootWorktrees is how many linked worktrees exist before the daemon starts.
func bootWorktrees(o runOptions) int {
	if len(o.WorktreeSteps) > 0 {
		return o.WorktreeSteps[0]
	}
	return o.Worktrees
}

func stepArtifact(step int, name string) string {
	return fmt.Sprintf("step%d_%s", step, name)
}

// worktreeStepSessionModel names a step run's sessions by the counts it walks,
// so delta compares step runs only when they stepped through the same counts.
func worktreeStepSessionModel(steps []int) string {
	counts := make([]string, len(steps))
	for i, count := range steps {
		counts[i] = strconv.Itoa(count)
	}
	return "per_agent_per_worktree_step:" + strings.Join(counts, ",")
}

func measureWorktreeSteps(ctx context.Context, o runOptions, p *privateRun, receipts *socketClient, phase *Phase, artifacts map[string]string, series *gaugeSeries, sampler processSampler, group string) error {
	phase.SessionModel = worktreeStepSessionModel(o.WorktreeSteps)
	phase.Started = time.Now()
	monitor := startMonitor(p, sampler, group)
	var parked []*socketClient
	var steps []worktreeStep
	loaded := time.Duration(0)
	defer func() {
		closeClients(parked)
		phase.DurationSec = loaded.Seconds()
		monitor.finish(phase)
		phase.Gate["worktree_steps"], _ = json.Marshal(steps)
	}()
	for k, linked := range o.WorktreeSteps {
		for len(p.Repos)-1 < linked {
			client, err := attachWorktree(ctx, o, p, phase)
			if client != nil {
				parked = append(parked, client)
			}
			if err != nil {
				return err
			}
		}
		captureReceipt(ctx, p, receipts, phase)
		step := worktreeStep{LinkedWorktrees: linked}
		agents, err := openAgents(ctx, p, o.Agents)
		if err != nil {
			return err
		}
		base := filepath.Join(p.Root, fmt.Sprintf("%s.step%d", phase.Name, k))
		window := series.watchLoad(ctx, lastStatus(phase), base)
		stepMonitor := startMonitor(p, sampler, group)
		first := len(phase.Attempts)
		step.LoadStarted = time.Now()
		loadErr := runAgents(ctx, o, p, agents, false, phase)
		step.LoadEnded = time.Now()
		closeClients(agents)
		loaded += step.LoadEnded.Sub(step.LoadStarted)
		load := Phase{Metrics: make(map[string]Metric), Attempts: phase.Attempts[first:]}
		stepMonitor.finish(&load)
		step.Load = load.Metrics
		step.Tools = summarize(load.Attempts, step.LoadEnded.Sub(step.LoadStarted).Seconds(), "workload")
		var heapErr error
		step.HeapLoadStart, step.HeapLoadPeak, heapErr = series.endLoad(window)
		step.HeapLoadPeakUnavailable = window.peakUnavailable
		artifacts[stepArtifact(k, "heap_profile_load_start")] = step.HeapLoadStart
		if step.HeapLoadPeak != "" {
			artifacts[stepArtifact(k, "heap_profile_load_peak")] = step.HeapLoadPeak
		}
		stepErr := errors.Join(loadErr, heapErr)
		if stepErr == nil {
			step.HeapIdle = base + ".heap-idle.pprof"
			artifacts[stepArtifact(k, "heap_profile_idle")] = step.HeapIdle
			stepErr = captureCollectedHeapProfile(ctx, lastStatus(phase), step.HeapIdle)
			step.IdleStarted = time.Now()
			if stepErr == nil {
				idle := o.Duration
				if stepIdleOverride > 0 {
					idle = stepIdleOverride
				}
				stepErr = waitContext(ctx, idle)
			}
			step.IdleEnded = time.Now()
		}
		steps = append(steps, step)
		if stepErr != nil {
			return stepErr
		}
	}
	return nil
}

// attachWorktree links one more worktree, opens a session in it and waits as
// startup does for a linked checkout: an exact worktree view, a source-backed
// read and a current analysis publication. The session stays open for the
// rest of the run, standing for an agent parked in that checkout.
func attachWorktree(ctx context.Context, o runOptions, p *privateRun, phase *Phase) (*socketClient, error) {
	warmCtx, cancel := context.WithTimeout(ctx, o.WarmTimeout)
	defer cancel()
	if err := p.addWorktree(warmCtx); err != nil {
		return nil, err
	}
	repo := &p.Repos[len(p.Repos)-1]
	repo.Prefix = p.Repos[0].Prefix
	client, err := openMCP(warmCtx, p.Socket, repo.Path)
	if err != nil {
		return nil, err
	}
	if err := preflightRoot(warmCtx, p, client, *repo, true, o.Deadline, phase); err != nil {
		return client, err
	}
	gate, rows, err := warmAnalysis(warmCtx, client, o.Deadline)
	phase.Attempts = append(phase.Attempts, rows...)
	for name, value := range gate {
		phase.Gate[repo.Corpus.Label+"."+name] = value
	}
	return client, err
}

// openAgents opens one fresh session per reading agent, agent i in checkout
// i modulo the checkouts, which is the checkout runAgents addresses it to.
func openAgents(ctx context.Context, p *privateRun, count int) ([]*socketClient, error) {
	clients := make([]*socketClient, 0, count)
	for i := range count {
		client, err := openMCP(ctx, p.Socket, p.Repos[i%len(p.Repos)].Path)
		if err != nil {
			closeClients(clients)
			return nil, err
		}
		clients = append(clients, client)
	}
	return clients, nil
}

func closeClients(clients []*socketClient) {
	for _, client := range clients {
		_ = client.Close()
	}
}
