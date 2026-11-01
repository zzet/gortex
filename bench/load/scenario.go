package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func runPhase(ctx context.Context, o runOptions, scenario string, corpora []Corpus, group, outputRoot string) (phase Phase, sweep []SweepEntry, artifacts map[string]string, crashes map[string]int, version string, resultErr error) {
	if scenario == startupScenario {
		return runStartupPhase(ctx, o, corpora, outputRoot)
	}
	phase = Phase{Name: scenario, SessionModel: "per_agent", Started: time.Now().UTC(), Gate: make(map[string]json.RawMessage), Metrics: make(map[string]Metric), EditLatency: distribution(nil, "ms"), Visibility: distribution(nil, "ms")}
	artifacts, crashes = make(map[string]string), make(map[string]int)
	var scenarioGroupStart map[string]Metric
	if group != "" {
		scenarioGroupStart = cgroupMetrics(group, os.Getpagesize())
	}
	defer func() {
		if group != "" {
			after := cgroupMetrics(group, os.Getpagesize())
			for _, key := range []string{"swap_in_bytes", "swap_out_bytes", "oom_kills", "oom_events"} {
				phase.Metrics["scenario_cgroup_"+key] = counterDelta(scenarioGroupStart[key], after[key])
			}
			if metric := phase.Metrics["scenario_cgroup_oom_kills"]; metric.Value != nil {
				crashes["oom_kills"] = int(*metric.Value)
			}
		}
		if phase.DurationSec <= 0 {
			phase.DurationSec = time.Since(phase.Started).Seconds()
		}
		phase.Tools = summarize(phase.Attempts, phase.DurationSec, "workload")
		phase.Probes = summarize(phase.Attempts, phase.DurationSec, "probe")
		for tool, row := range phase.Tools {
			errors := row.ProductExpired
			for _, count := range row.Errors {
				errors += count
			}
			if errors > 0 {
				phase.Errors = append(phase.Errors, fmt.Sprintf("%s: %d workload errors/product expiries (retained in metrics and attempts)", tool, errors))
			}
		}
		if phase.NeverSeen > 0 {
			phase.Errors = append(phase.Errors, fmt.Sprintf("%d acknowledged edits never visible before product deadline", phase.NeverSeen))
		}
		nativeWork := scenario == editProbeScenario && editProbeNativeWorkObserved(phase.EditProbe)
		if scenario != "cold" && scenario != "restart" && scenario != "idle-stop" && len(phase.Tools) == 0 && !nativeWork {
			phase.Errors = append(phase.Errors, "no workload calls observed before phase end")
		}
		if resultErr != nil {
			phase.Errors = append(phase.Errors, resultErr.Error())
		}
		if resultErr == nil && len(phase.Errors) > 0 {
			resultErr = errors.New(strings.Join(phase.Errors, "; "))
		}
	}()
	selected := corpora[:1]
	if scenario == "many" || scenario == "many-edit" || scenario == checkoutDiscoveryScenario {
		if len(corpora) < 2 {
			return phase, nil, artifacts, crashes, "", errors.New("many-repository scenarios require at least two pinned repositories")
		}
		selected = corpora
		if scenario == checkoutDiscoveryScenario {
			selected = corpora[:2]
		}
	}
	editing := scenario == "edit" || scenario == "many-edit" || scenario == editProbeScenario || scenario == checkoutDiscoveryScenario && o.Editors > 0
	for _, corpus := range selected {
		operations, planErr := readingPlan(corpus)
		if planErr != nil {
			return phase, nil, artifacts, crashes, "", planErr
		}
		phase.Operations = append(phase.Operations, operations...)
		if editing {
			phase.Operations = append(phase.Operations, editRequest(corpus, "{{marker}}"))
		}
	}
	if editing {
		for _, corpus := range selected {
			if corpus.EditAnchor == "" || strings.Count(corpus.EditTemplate, "{{marker}}") != 1 {
				return phase, nil, artifacts, crashes, "", errors.New("edit scenarios require a stable unique edit_anchor and language-valid edit_template with one {{marker}} declaration")
			}
		}
	}
	if scenario == editProbeScenario {
		plan, err := planEditProbe(selected[0], o)
		if err != nil {
			return phase, nil, artifacts, crashes, "", err
		}
		phase.Operations = editProbeOperations(plan, selected[0])
		phase.EditProbe = newEditProbeReport(plan, selected[0], !o.editProbeBaseline)
		phase.SessionModel = plan.SessionModel
	}
	p := o.smokeState
	var err error
	if p == nil {
		if scenario == checkoutDiscoveryScenario && o.Discovery.Resume != "" {
			p, err = resumeDiscoveryState(ctx, o, selected)
		} else {
			p, err = newPrivateRun(ctx, outputRoot, o.Binary, selected)
		}
	} else {
		err = validateSmokeOwner(p, o.Binary, selected)
	}
	if err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	artifacts["private_root"] = p.Root
	if scenario == checkoutDiscoveryScenario && o.Discovery.Resume != "" {
		raw, err := os.ReadFile(o.Discovery.Resume)
		if err != nil {
			return phase, nil, artifacts, crashes, "", err
		}
		phase.Gate["discovery_seed_lineage"] = json.RawMessage(raw)
	}
	firstStart := p.startCount + 1
	if o.smokeState != nil {
		info, statErr := os.Stat(p.Store)
		present := statErr == nil
		if p.startCount == 0 && !errors.Is(statErr, os.ErrNotExist) || p.startCount > 0 && (statErr != nil || info.Size() == 0) {
			return phase, nil, artifacts, crashes, "", errors.New("smoke needs one initially absent store, then the same nonempty retained store")
		}
		data, err := json.Marshal(map[string]any{"owned_root": p.Root, "binary": p.Binary, "starts_before_phase": p.startCount, "store_present": present})
		if err != nil {
			return phase, nil, artifacts, crashes, "", err
		}
		phase.Gate["pr_smoke_store"] = data
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), o.StopTimeout)
		elapsed, forced, err := p.stop(stopCtx)
		cancel()
		phase.Metrics["stop_seconds"] = measured(elapsed.Seconds(), "s")
		forcedValue := float64(0)
		if forced {
			forcedValue = 1
		}
		phase.Metrics["force_killed"] = measured(forcedValue, "boolean")
		resultErr = errors.Join(resultErr, err)
		var logArtifacts map[string]string
		var logCrashes map[string]int
		if o.smokeState == nil {
			logArtifacts, logCrashes, err = collectLogs(p.Root)
		} else {
			logArtifacts, logCrashes, err = collectRunLogs(p.Root, outputRoot, firstStart)
		}
		for name, path := range logArtifacts {
			artifacts[name] = path
		}
		for name, count := range logCrashes {
			crashes[name] += count
		}
		resultErr = errors.Join(resultErr, err, collectPatchWrites(logArtifacts, &phase))
		if o.smokeState == nil {
			resultErr = errors.Join(resultErr, os.RemoveAll(p.SocketDir))
		}
		if !o.KeepState && o.smokeState == nil {
			resultErr = errors.Join(resultErr, p.removeState())
		}
	}()
	if scenario == "worktrees" {
		if err := p.addWorktrees(ctx, bootWorktrees(o)); err != nil {
			return phase, nil, artifacts, crashes, "", err
		}
	}
	if o.Host == "floor" {
		files, err := p.git(ctx, p.Repos[0].Path, "ls-files", "-z")
		if err != nil || bytes.Count(files, []byte{0}) > 1000 {
			return phase, nil, artifacts, crashes, "", errors.New("floor corpus exceeds 1000 tracked files or cannot be counted")
		}
	}
	warmCtx, cancelWarm := context.WithTimeout(ctx, o.WarmTimeout)
	defer cancelWarm()
	var configured []privateRepo
	if scenario == checkoutDiscoveryScenario {
		configured, err = p.writeDiscoveryBootRepos()
	} else {
		configured, err = p.writeBootRepos(scenario == "worktrees")
	}
	if err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	if scenario == checkoutDiscoveryScenario && o.Discovery.StallFirst {
		release, files, err := p.installDiscoveryFault()
		if err != nil {
			return phase, nil, artifacts, crashes, "", err
		}
		for name, path := range files {
			artifacts[name] = path
		}
		defer func() {
			resultErr = errors.Join(resultErr, release())
			raw, err := os.ReadFile(files["discovery_git_events"])
			observed := err == nil && len(bytes.TrimSpace(raw)) > 0
			phase.Gate["discovery_git_fault"], _ = json.Marshal(map[string]any{"configured": true, "observed": observed, "raw_events": string(raw), "absolute_child_bound_seconds": 60})
			if !observed {
				resultErr = errors.Join(resultErr, errors.New("Git stall never entered the intended new checkout"), err)
			}
		}()
	}
	coldStart := time.Now()
	if err := p.start(); err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	startupMonitor := startMonitor(p, newProcessSampler(), group)
	startupFinished := false
	finishStartup := func() {
		if startupFinished {
			return
		}
		startupFinished = true
		startup := Phase{Metrics: make(map[string]Metric)}
		startupMonitor.finish(&startup)
		for key, value := range startup.Metrics {
			phase.Metrics["startup_"+key] = value
		}
	}
	defer finishStartup()
	if err := p.awaitSocket(warmCtx); err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	gate, metrics, err := p.awaitReady(warmCtx, coldStart, configured)
	for name, value := range gate {
		phase.Gate[name] = value
	}
	for name, value := range metrics {
		phase.Metrics[name] = value
	}
	if err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	// The gauge series starts as soon as the measured daemon is ready, so it
	// also covers the warmup gates and the level after enrichment, before any
	// load. A restart replaces the process, so there it starts after the
	// restart instead.
	var series *gaugeSeries
	startSeries := func() (err error) {
		if wantsGaugeSeries(o) {
			series, err = startGaugeSeries(ctx, p, newProcessSampler(), group)
		}
		return err
	}
	defer func() {
		if series != nil {
			resultErr = errors.Join(resultErr, series.stopInto(&phase))
		}
	}()
	if scenario != "restart" {
		if err := startSeries(); err != nil {
			return phase, nil, artifacts, crashes, "", err
		}
	}
	var readinessState readiness
	if err := json.Unmarshal(gate["readiness"], &readinessState); err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	for i := range p.Repos {
		for _, tracked := range readinessState.TrackedRepos {
			if filepath.Clean(tracked.Path) == filepath.Clean(p.Repos[i].Path) {
				if scenario == checkoutDiscoveryScenario && o.Discovery.Resume != "" && p.Repos[i].Prefix != tracked.Prefix {
					return phase, nil, artifacts, crashes, "", errors.New("resumed discovery changed a prepared repository identity prefix")
				}
				p.Repos[i].Prefix = tracked.Prefix
			}
		}
		if i > 0 && scenario == "worktrees" || i >= 2 && scenario == checkoutDiscoveryScenario {
			p.Repos[i].Prefix = p.Repos[0].Prefix
		}
		if p.Repos[i].Prefix == "" {
			return phase, nil, artifacts, crashes, "", errors.New("readiness has no actual repo prefix")
		}
	}
	var clients []*socketClient
	defer func() {
		for _, client := range clients {
			_ = client.Close()
		}
	}()
	count := o.Agents
	if editing {
		count += o.Editors
	}
	if scenario == "sweep" || scenario == "cold" || scenario == "restart" {
		count = 1
	}
	for i := range max(count, len(p.Repos)) {
		repo := p.Repos[i%len(p.Repos)]
		client, err := openMCP(warmCtx, p.Socket, repo.Path)
		if err != nil {
			return phase, nil, artifacts, crashes, "", err
		}
		clients = append(clients, client)
		version = client.version
		if i < len(p.Repos) {
			subscribeInvalidations(warmCtx, client, repo.Corpus.Label, o.Deadline, &phase)
			if err := preflightRoot(warmCtx, p, client, repo, scenario == "worktrees" && i > 0 || scenario == checkoutDiscoveryScenario && i >= 2, o.Deadline, &phase); err != nil {
				return phase, nil, artifacts, crashes, version, err
			}
			gate, rows, err := warmAnalysis(warmCtx, client, o.Deadline)
			phase.Attempts = append(phase.Attempts, rows...)
			for name, value := range gate {
				phase.Gate[repo.Corpus.Label+"."+name] = value
			}
			if err != nil {
				return phase, nil, artifacts, crashes, version, err
			}
		}
	}
	finishStartup()
	if len(o.smokeMarkers) > 0 {
		if err := awaitSmokeRestoration(warmCtx, o, p, clients[0], &phase); err != nil {
			return phase, nil, artifacts, crashes, version, err
		}
	}
	if scenario == "restart" {
		for _, client := range clients {
			_ = client.Close()
		}
		clients = nil
		stopCtx, cancel := context.WithTimeout(ctx, o.StopTimeout)
		elapsed, forced, err := p.stop(stopCtx)
		cancel()
		phase.Metrics["restart_stop_seconds"] = measured(elapsed.Seconds(), "s")
		if err != nil || forced {
			return phase, nil, artifacts, crashes, version, fmt.Errorf("restart stop failed or forced: %v", err)
		}
		restartStart := time.Now()
		if err := p.start(); err != nil {
			return phase, nil, artifacts, crashes, version, err
		}
		if err := p.awaitSocket(warmCtx); err != nil {
			return phase, nil, artifacts, crashes, version, err
		}
		_, readyMetrics, err := p.awaitReady(warmCtx, restartStart, configured)
		for name, metric := range readyMetrics {
			phase.Metrics["restart_"+name] = metric
		}
		if err != nil {
			return phase, nil, artifacts, crashes, version, err
		}
		if err := startSeries(); err != nil {
			return phase, nil, artifacts, crashes, version, err
		}
		client, err := openMCP(warmCtx, p.Socket, p.Repos[0].Path)
		if err != nil {
			return phase, nil, artifacts, crashes, version, err
		}
		clients = append(clients, client)
		subscribeInvalidations(warmCtx, client, "restart", o.Deadline, &phase)
		gate, rows, err := warmAnalysis(warmCtx, client, o.Deadline)
		phase.Attempts = append(phase.Attempts, rows...)
		for name, value := range gate {
			phase.Gate["restart."+name] = value
		}
		if err != nil {
			return phase, nil, artifacts, crashes, version, err
		}
	}
	var sweepRequests []request
	var sweepClients []*socketClient
	if scenario == "sweep" {
		phase.SessionModel = "per_operation_isolated"
		sweepRequests, sweep, err = discoverSweep(warmCtx, clients[0], p.Repos[0].Corpus)
		if err != nil {
			return phase, sweep, artifacts, crashes, version, err
		}
		phase.Operations = sweepRequests
		for range sweepRequests {
			client, err := openMCP(warmCtx, p.Socket, p.Repos[0].Path)
			if err != nil {
				return phase, sweep, artifacts, crashes, version, err
			}
			clients = append(clients, client)
			sweepClients = append(sweepClients, client)
		}
		phase.Metrics["sweep_operation_sessions"] = measured(float64(len(sweepClients)), "sessions")
	}
	phase.Metrics["mcp_sessions"] = measured(float64(len(clients)), "sessions")
	if scenario == editProbeScenario {
		runtime, probers, prepareErr := prepareEditProbe(warmCtx, o, p, clients[:o.Agents], clients[o.Agents:o.Agents+o.Editors], &phase)
		clients = append(clients, probers...)
		if runtime != nil {
			defer func() { resultErr = errors.Join(resultErr, runtime.close()) }()
		}
		if prepareErr != nil {
			return phase, sweep, artifacts, crashes, version, prepareErr
		}
		phase.Metrics["mcp_sessions"] = measured(float64(len(clients)), "sessions")
		artifacts["edit_probe_raw"] = filepath.Join(p.Root, "edit-probe")
	}
	captureReceipt(ctx, p, clients[0], &phase)
	sampler := newProcessSampler()
	if scenario == "idle-stop" {
		err = measureIdleStop(ctx, p, clients, &phase, sampler, group)
	} else if o.pairedDriver != nil {
		err = o.pairedDriver(ctx, p, clients, sweepClients, &phase)
	} else {
		err = measureStandalone(ctx, o, p, clients, sweepClients, sweepRequests, sweep, &phase, artifacts, series, sampler, group)
	}
	if err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	if phase.editProbeRuntime != nil {
		if ctx.Err() != nil {
			resultErr = errors.Join(resultErr, errors.New("restore batch unavailable after canceled workload"))
		} else {
			restoreCtx, cancel := context.WithTimeout(ctx, o.Deadline)
			resultErr = errors.Join(resultErr, phase.editProbeRuntime.restore(restoreCtx))
			cancel()
		}
	}
	if scenario == "sweep" {
		finalizeSweepInventory(sweep, phase.Attempts, o.SweepRepetitions, &phase)
	}
	if scenario != "idle-stop" {
		captureReceipt(ctx, p, editProbeReceiptClient(&phase, clients), &phase)
	}
	for i, client := range clients {
		client.mu.Lock()
		data, marshalErr := json.Marshal(struct {
			Received []notificationObservation `json:"received"`
			Dropped  int                       `json:"dropped"`
		}{client.notifies, client.notifyDropped})
		client.mu.Unlock()
		if marshalErr == nil {
			phase.Gate[fmt.Sprintf("notifications.%d", i)] = data
		}
	}
	if err := waitContext(ctx, 2*time.Second); err == nil {
		for key, metric := range sampler.sample(p.process.Process.Pid) {
			if key == "rss_bytes" || key == "phys_footprint_bytes" || key == "pss_bytes" || key == "rss_anon_bytes" {
				phase.Metrics["idle_after_2s_"+key] = metric
			}
		}
	}
	return phase, sweep, artifacts, crashes, version, resultErr
}

func measureStandalone(ctx context.Context, o runOptions, p *privateRun, clients, sweepClients []*socketClient, sweepRequests []request, sweep []SweepEntry, phase *Phase, artifacts map[string]string, series *gaugeSeries, sampler processSampler, group string) error {
	if len(o.WorktreeSteps) > 0 {
		return measureWorktreeSteps(ctx, o, p, clients[0], phase, artifacts, series, sampler, group)
	}
	heap := series.watchLoadProfiles(ctx, o, p, phase)
	phase.Started = time.Now()
	monitor := startMonitor(p, sampler, group)
	var profileDone chan error
	if o.CPUProfile > 0 && ((phase.Name == "read" && o.Agents == 8) || (phase.Name == "worktrees" && o.Worktrees == 4)) {
		profileDone = make(chan error, 1)
		path := filepath.Join(p.Root, phase.Name+".cpu.pprof")
		artifacts["cpu_profile"] = path
		if len(phase.Status) == 0 {
			profileDone <- errors.New("pprof requires a daemon status receipt")
		} else {
			status := phase.Status[len(phase.Status)-1]
			go func() { profileDone <- captureCPUProfile(ctx, status, o.CPUProfile, path) }()
		}
	}
	var err error
	if phase.Name == editProbeScenario {
		err = phase.editProbeRuntime.run(ctx)
	} else if phase.Name == "sweep" {
		sweepCtx, cancelSweep := context.WithTimeout(ctx, o.SweepBudget)
		defer cancelSweep()
		err = runSweep(sweepCtx, o, sweepClients, p.Repos[0], sweepRequests, sweep, phase, waitContext)
	} else if phase.Name == checkoutDiscoveryScenario {
		err = measureCheckoutDiscovery(ctx, o, p, clients, phase)
	} else if phase.Name != "cold" && phase.Name != "restart" {
		err = runAgents(ctx, o, p, clients, phase.Name == "edit" || phase.Name == "many-edit", phase)
	}
	phase.DurationSec = time.Since(phase.Started).Seconds()
	monitor.finish(phase)
	err = errors.Join(err, series.endLoadProfiles(heap, phase, artifacts))
	if profileDone != nil {
		err = errors.Join(err, <-profileDone)
	}
	return err
}

func dispatchArgs(r request, repo privateRepo) map[string]any {
	args := copyJSON(r.Arguments)
	args["output"] = editProbeDispatchOutput(r)
	if target, ok := args["target"].(map[string]any); ok {
		for _, key := range []string{"file", "symbol"} {
			if value, ok := target[key].(string); ok {
				target[key] = repo.Prefix + "/" + value
			}
		}
		if symbols, ok := target["symbols"].([]any); ok {
			for i, value := range symbols {
				if symbol, ok := value.(string); ok {
					symbols[i] = repo.Prefix + "/" + symbol
				}
			}
		}
	}
	if r.name() == "refactor.move" {
		if destination, ok := args["destination"].(string); ok {
			args["destination"] = repo.Prefix + "/" + destination
		}
	}
	if r.name() == "edit.batch" {
		if changes, ok := args["changes"].([]any); ok {
			for _, item := range changes {
				if change, ok := item.(map[string]any); ok && change["op"] == "edit_file" {
					if path, ok := change["path"].(string); ok {
						change["path"] = repo.Prefix + "/" + path
					}
				}
			}
		}
	}

	if r.name() == "change.verify" {
		if source, ok := args["source"].(map[string]any); ok {
			if changes, ok := source["changes"].([]any); ok {
				for _, item := range changes {
					if change, ok := item.(map[string]any); ok {
						if symbol, ok := change["symbol_id"].(string); ok {
							change["symbol_id"] = repo.Prefix + "/" + symbol
						}
					}
				}
			}
		}
	}
	return args
}

func runAgents(parent context.Context, o runOptions, p *privateRun, clients []*socketClient, editing bool, phase *Phase) error {
	if o.PRSmokeDrain > 0 {
		if editing {
			return errors.New("offered-window/drain for edit/probe work is unsupported")
		}
		return runPRSmokeReads(parent, o, p, clients, phase)
	}
	ctx, cancel := context.WithTimeout(parent, o.Duration)
	defer cancel()
	var mu sync.Mutex
	var workers sync.WaitGroup
	count := o.Agents
	if editing {
		count += o.Editors
	}
	for agent := range count {
		workers.Go(func() {
			repo := p.Repos[agent%len(p.Repos)]
			client := clients[agent]
			plan, _ := readingPlan(repo.Corpus)
			pace := newPacing(profile(o.Profile), uint64(o.Seed)+uint64(agent))
			for step := 0; ctx.Err() == nil; step++ {
				if waitContext(ctx, pace.think()) != nil {
					return
				}
				if editing && agent >= o.Agents {
					edit, rows := editAndObserve(ctx, o, client, repo, agent, step)
					mu.Lock()
					phase.Edits = append(phase.Edits, edit)
					phase.Attempts = append(phase.Attempts, rows...)
					mu.Unlock()
				}
				if ctx.Err() != nil {
					return
				}
				var burst sync.WaitGroup
				for slot := range pace.burst() {
					burst.Go(func() {
						r := plan[(agent+step*5+slot)%len(plan)]
						_, rows, _ := callTool(ctx, client, r, dispatchArgs(r, repo), "workload", agent, step, slot, o.Deadline, waitContext)
						mu.Lock()
						phase.Attempts = append(phase.Attempts, rows...)
						mu.Unlock()
					})
				}
				burst.Wait()
			}
		})
	}
	workers.Wait()
	finishEditMetrics(phase)
	return parent.Err()
}

func editAndObserve(ctx context.Context, o runOptions, client *socketClient, repo privateRepo, agent, step int) (EditObservation, []Attempt) {
	marker := fmt.Sprintf("GortexLoadMarkerA%dS%d", agent, step)
	start := time.Now()
	observation := EditObservation{Repo: repo.Corpus.Label, Marker: marker, Started: start, VisibilityMS: unavailable("ms", "not observed"), Outcome: "edit_error"}
	r := editRequest(repo.Corpus, marker)
	ack, rows, err := callTool(ctx, client, r, dispatchArgs(r, repo), "workload", agent, step, 0, o.Deadline, waitContext)
	observation.Acknowledgement = ack
	observation.WriteMS = float64(time.Since(start)) / float64(time.Millisecond)
	if err != nil {
		return observation, rows
	}
	visibilityCtx, cancel := context.WithDeadline(ctx, start.Add(o.Deadline))
	defer cancel()
	query := request{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": marker}}
	for visibilityCtx.Err() == nil {
		result, probeRows, err := callTool(ctx, client, query, dispatchArgs(query, repo), "probe", agent, step, 0, time.Until(start.Add(o.Deadline)), waitContext)
		rows = append(rows, probeRows...)
		if err == nil {
			payload, payloadErr := textPayload(result, "content")
			if payloadErr == nil && containsIdentity(payload, marker) {
				observation.Outcome, observation.VisibilityMS = "visible", measured(float64(time.Since(start))/float64(time.Millisecond), "ms")
				return observation, rows
			}
		}
		if waitContext(visibilityCtx, 250*time.Millisecond) != nil {
			break
		}
	}
	observation.Outcome = "never_visible"
	if end, ok := ctx.Deadline(); (ok && end.Before(start.Add(o.Deadline))) || ctx.Err() == context.Canceled {
		observation.Outcome = "phase_censored"
	}
	return observation, rows
}

func containsIdentity(data json.RawMessage, marker string) bool {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(value any) bool {
		switch row := value.(type) {
		case map[string]any:
			id, _ := row["id"].(string)
			if id != "" && !strings.HasPrefix(id, "ext::") && (strings.HasSuffix(id, "::"+marker) || row["name"] == marker) {
				return true
			}
			for _, child := range row {
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range row {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func editRequest(corpus Corpus, marker string) request {
	return request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file", "target": map[string]any{"file": corpus.File}, "match": corpus.EditAnchor, "replacement": corpus.EditAnchor + strings.ReplaceAll(corpus.EditTemplate, "{{marker}}", marker)}}
}
