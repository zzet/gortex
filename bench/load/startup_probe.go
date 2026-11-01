package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The startup scenario observes repository admission from before the
// daemon's socket exists until every configured repository is ready and
// enriched, on an initial boot and on each optional restart. The probes keep
// sampling across the gap between boots, so a restart is seen exactly as an
// agent sees it. A seeded store makes the initial boot an upgrade: its
// migration runs before the socket listens.

const (
	startupScenario          = "startup"
	startupRosterWorkspace   = "probe-roster"
	startupWatchInterval     = 100 * time.Millisecond
	startupCoordinatorWindow = 2 * time.Second
)

type startupOptions struct {
	Restarts  int
	SeedStore string
}

func bindStartupFlags(f *flag.FlagSet, o *startupOptions) {
	f.IntVar(&o.Restarts, "restarts", 0, "startup: warm restarts after the initial boot, each probed from before its socket exists")
	f.StringVar(&o.SeedStore, "seed-store", "", "startup: store file copied into the private run before the initial boot, e.g. one written by an older schema; the source is only read")
}

func (o startupOptions) validate(scenario string) error {
	if o.Restarts < 0 {
		return errors.New("--restarts must be nonnegative")
	}
	if scenario != startupScenario && (o.Restarts != 0 || o.SeedStore != "") {
		return errors.New("--restarts and --seed-store apply to the startup scenario only")
	}
	return nil
}

// bootTiming holds what the readiness watcher observed for one daemon start.
// Every instant is an observed upper bound at the watcher's poll interval.
type bootTiming struct {
	Boot       int                  `json:"boot"`
	Started    time.Time            `json:"started"`
	Stopping   time.Time            `json:"stopping"`
	SocketAt   time.Time            `json:"socket_at"`
	ReadyAt    map[string]time.Time `json:"ready_at"`
	EnrichedAt time.Time            `json:"enriched_at"`
	// Coordinators is the largest checkout coordinator count the status
	// census read in the 2 s after the primary registered.
	Coordinators *int            `json:"coordinators_within_2s"`
	CensusReads  int             `json:"census_reads"`
	CensusErrors []string        `json:"census_errors"`
	Views        json.RawMessage `json:"views"`
}

func runStartupPhase(ctx context.Context, o runOptions, corpora []Corpus, outputRoot string) (phase Phase, _ []SweepEntry, artifacts map[string]string, crashes map[string]int, version string, resultErr error) {
	phase = Phase{Name: startupScenario, SessionModel: "fresh_session_per_probe_tick", Started: time.Now().UTC(), Gate: make(map[string]json.RawMessage), Metrics: make(map[string]Metric), EditLatency: distribution(nil, "ms"), Visibility: distribution(nil, "ms")}
	artifacts, crashes = make(map[string]string), make(map[string]int)
	phase.Operations = startupOperations(corpora[0])
	defer func() {
		phase.DurationSec = time.Since(phase.Started).Seconds()
		if resultErr != nil {
			phase.Errors = append(phase.Errors, resultErr.Error())
		} else if len(phase.Errors) > 0 {
			resultErr = errors.New(strings.Join(phase.Errors, "; "))
		}
	}()
	p, err := newPrivateRun(ctx, outputRoot, o.Binary, corpora)
	if err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	artifacts["private_root"] = p.Root
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), o.StopTimeout)
		_, forced, err := p.stop(stopCtx)
		cancel()
		if forced {
			err = errors.Join(err, errors.New("final stop was forced"))
		}
		logArtifacts, logCrashes, logErr := collectLogs(p.Root)
		for name, path := range logArtifacts {
			artifacts[name] = path
		}
		for name, count := range logCrashes {
			crashes[name] += count
		}
		resultErr = errors.Join(resultErr, err, logErr, os.RemoveAll(p.SocketDir))
		if !o.KeepState {
			resultErr = errors.Join(resultErr, p.removeState())
		}
	}()
	linked, err := addLinkedCheckouts(ctx, p, o.Worktrees)
	if err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	routerDir, err := writeRouterRoster(p)
	if err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	configured, err := p.writeBootRepos(false)
	if err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	if o.Startup.SeedStore != "" {
		if err := copySeedStore(o.Startup.SeedStore, p.Store); err != nil {
			return phase, nil, artifacts, crashes, "", err
		}
	}
	cwds := []probeCWD{{Role: "primary", Dir: p.Repos[0].Path, File: p.Repos[0].File}}
	cwds = append(cwds, linked[:min(1, len(linked))]...)
	cwds = append(cwds, probeCWD{Role: "router", Dir: routerDir, File: p.Repos[0].File})
	logPath := filepath.Join(p.Root, "startup-probe.jsonl")
	probe, err := newStartupProbe(p, cwds, corpora[0].Query, logPath)
	if err != nil {
		return phase, nil, artifacts, crashes, "", err
	}
	artifacts["startup_probe_log"] = logPath
	probeCtx, stopProbe := context.WithCancel(ctx)
	probe.start(probeCtx)
	var boots []bootTiming
	for index := 0; index <= o.Startup.Restarts; index++ {
		if index > 0 {
			probe.stopping()
			boots[index-1].Stopping = time.Now()
			stopCtx, cancel := context.WithTimeout(ctx, o.StopTimeout)
			_, forced, err := p.stop(stopCtx)
			cancel()
			if err != nil || forced {
				resultErr = fmt.Errorf("restart %d: stop failed or was forced: %v", index, err)
				break
			}
		}
		timing, err := runStartupBoot(ctx, o, p, probe, configured, index)
		boots = append(boots, timing)
		if err != nil {
			resultErr = err
			break
		}
	}
	if resultErr == nil {
		receiptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		client, err := openMCP(receiptCtx, p.Socket, p.Repos[0].Path)
		if err == nil {
			captureReceipt(receiptCtx, p, client, &phase)
			err = client.Close()
		}
		cancel()
		resultErr = err
	}
	if len(boots) > 0 && boots[len(boots)-1].Stopping.IsZero() {
		boots[len(boots)-1].Stopping = time.Now()
	}
	stopProbe()
	samples, probeErr := probe.finish()
	resultErr = errors.Join(resultErr, probeErr)
	summary := summarizeStartup(samples, boots, p.Repos[0].Corpus.Label, len(linked))
	summary.CLITTY = probe.ttyStatus
	for name, metric := range summary.metrics() {
		phase.Metrics[name] = metric
	}
	if data, err := json.Marshal(summary); err == nil {
		phase.Gate["startup_probe"] = data
	} else {
		resultErr = errors.Join(resultErr, err)
	}
	return phase, nil, artifacts, crashes, probe.daemonVersion(), resultErr
}

// runStartupBoot starts the daemon and follows it until every configured
// repository is ready, the daemon reports enrichment, and the measured tail
// (--duration) has passed, so the next restart begins from a warm store.
func runStartupBoot(ctx context.Context, o runOptions, p *privateRun, probe *startupProbe, configured []privateRepo, index int) (bootTiming, error) {
	timing := bootTiming{Boot: index, ReadyAt: make(map[string]time.Time)}
	timing.Started = time.Now()
	probe.beginBoot(index)
	if err := p.start(); err != nil {
		return timing, err
	}
	primary := configured[0].Corpus.Label
	for {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		data, err := control(probeCtx, p.Socket, "probe", nil)
		cancel()
		now := time.Now() // after the answer: an upper bound, never early
		if err == nil {
			if timing.SocketAt.IsZero() {
				timing.SocketAt = now
				probe.socketUp(index)
			}
			var state readiness
			if err := json.Unmarshal(data, &state); err != nil {
				return timing, err
			}
			if err := observeReadiness(&timing, state, configured, now); err != nil {
				return timing, err
			}
		}
		ready, registered := timing.ReadyAt[primary]
		if registered && now.Sub(ready) <= startupCoordinatorWindow {
			readCoordinators(ctx, p, &timing)
		}
		if !timing.EnrichedAt.IsZero() && now.Sub(timing.EnrichedAt) >= o.Duration && now.Sub(ready) > startupCoordinatorWindow {
			return timing, nil
		}
		select {
		case <-p.done:
			return timing, fmt.Errorf("boot %d: daemon exited: %v", index, p.exitErr)
		default:
		}
		if now.Sub(timing.Started) > o.WarmTimeout {
			return timing, fmt.Errorf("boot %d: configured repositories not ready and enriched within %s", index, o.WarmTimeout)
		}
		if err := waitContext(ctx, startupWatchInterval); err != nil {
			return timing, err
		}
	}
}

// observeReadiness stamps each configured repository the first time the probe
// lists it as ready. Daemons that predate per-repository state list a
// registered repository with its prefix and no state.
func observeReadiness(timing *bootTiming, state readiness, configured []privateRepo, now time.Time) error {
	for _, repo := range configured {
		if _, seen := timing.ReadyAt[repo.Corpus.Label]; seen {
			continue
		}
		for _, tracked := range state.TrackedRepos {
			if filepath.Clean(tracked.Path) == filepath.Clean(repo.Path) && tracked.Prefix != "" && (tracked.State == "ready" || tracked.State == "") {
				timing.ReadyAt[repo.Corpus.Label] = now
			}
		}
	}
	covered, err := readyCoverage(state, configured)
	if err == nil && covered && state.Ready && state.Enriched && timing.EnrichedAt.IsZero() && len(timing.ReadyAt) == len(configured) {
		timing.EnrichedAt = now
	}
	return err
}

func readCoordinators(ctx context.Context, p *privateRun, timing *bootTiming) {
	statusCtx, cancel := context.WithTimeout(ctx, time.Second)
	data, err := control(statusCtx, p.Socket, "status", nil)
	cancel()
	timing.CensusReads++
	var status struct {
		Views json.RawMessage `json:"views"`
	}
	var views struct {
		Coordinators *int `json:"coordinators"`
	}
	if err == nil {
		err = json.Unmarshal(data, &status)
	}
	if err == nil && len(status.Views) > 0 {
		err = json.Unmarshal(status.Views, &views)
	}
	if err == nil && views.Coordinators == nil {
		err = errors.New("status has no checkout coordinator census")
	}
	if err != nil {
		if len(timing.CensusErrors) < 5 {
			timing.CensusErrors = append(timing.CensusErrors, err.Error())
		}
		return
	}
	timing.Views = status.Views
	if timing.Coordinators == nil || *views.Coordinators > *timing.Coordinators {
		timing.Coordinators = views.Coordinators
	}
}

// addLinkedCheckouts adds linked worktrees of the first manifest repository.
// They are not configured: the daemon discovers them through the primary.
func addLinkedCheckouts(ctx context.Context, p *privateRun, count int) ([]probeCWD, error) {
	primary := p.Repos[0]
	var linked []probeCWD
	for i := range count {
		path := filepath.Join(p.Root, "repos", fmt.Sprintf("linked-%d", i+1))
		if _, err := p.git(ctx, primary.Path, "worktree", "add", "--detach", path, primary.Corpus.Commit); err != nil {
			return nil, err
		}
		file, err := ownedFile(path, primary.Corpus.File)
		if err != nil {
			return nil, err
		}
		linked = append(linked, probeCWD{Role: "worktree", Dir: path, File: file})
	}
	return linked, nil
}

// writeRouterRoster gives one cwd inside the primary a workspace that a
// servers.toml roster claims. The roster's only server is a socket that never
// exists, so a routed call fails to dial and the daemon answers locally; the
// primary and worktree cwds keep their ordinary path.
func writeRouterRoster(p *privateRun) (string, error) {
	dir := filepath.Join(p.Repos[0].Path, ".gortex-probe-router")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte("workspace: "+startupRosterWorkspace+"\n"), 0o600); err != nil {
		return "", err
	}
	configDir := filepath.Join(p.Root, "config", "gortex")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", err
	}
	remote := "unix://" + filepath.ToSlash(filepath.Join(p.Root, "absent-remote.sock"))
	roster := fmt.Sprintf("[[server]]\nslug = \"probe-remote\"\nurl = %q\nworkspaces = [%q]\n", remote, startupRosterWorkspace)
	return dir, os.WriteFile(filepath.Join(configDir, "servers.toml"), []byte(roster), 0o600)
}

// copySeedStore copies a quiescent store, and its write-ahead log when there
// is one, into the private run. The source is opened read-only.
func copySeedStore(source, dest string) error {
	for _, suffix := range []string{"", "-wal"} {
		in, err := os.Open(source + suffix)
		if suffix != "" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("seed store: %w", err)
		}
		out, err := os.OpenFile(dest+suffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = in.Close()
			return fmt.Errorf("seed store: %w", err)
		}
		_, err = io.Copy(out, in)
		if err = errors.Join(err, in.Close(), out.Close()); err != nil {
			return fmt.Errorf("seed store: %w", err)
		}
	}
	return nil
}
