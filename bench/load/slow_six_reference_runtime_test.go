//go:build acceptance

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var slowSixPlanPath = flag.String("slow-six-plan", "", "explicit six-repository slow3 plan; omission performs no work")
var slowSixExecute = flag.Bool("slow-six-execute", false, "execute only the externally granted six-repository cell")

func TestSlowSixReferenceCampaign(t *testing.T) {
	if !*slowSixExecute {
		t.Skip("requires explicit selected six-repository plan and external runtime grant")
	}
	if !filepath.IsAbs(*slowSixPlanPath) {
		t.Fatal("absolute frozen plan required")
	}
	raw, err := os.ReadFile(*slowSixPlanPath)
	if err != nil {
		t.Fatal(err)
	}
	var p slowSixPlan
	if err = json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if err = slowSixCollect(t.Context(), p); err != nil {
		t.Fatal(err)
	}
}

// Exactly one persistent serial slow call per connection. A phase deadline
// stops new offers; the admitted call keeps the original run/product deadline.
func slowSixPhase(ctx context.Context, p *privateRun, fast, slow []*fairnessNoisy50Connection, plans [][]request, name string, seed uint64, sink *fairnessNoisy50Sink) (map[string]any, error) {
	if len(fast) != 8 || len(slow) != 3 || len(p.Repos) != 6 {
		return nil, errors.New("literal six/three/eight population required")
	}
	if err := fairnessNoisy50Idle(ctx, p.Socket); err != nil {
		return nil, err
	}
	origin := time.Now()
	offerEnd := origin.Add(120 * time.Second)
	offerCtx, stopOffers := context.WithDeadline(ctx, offerEnd)
	defer stopOffers()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var resultErr error
	add := func(e error) {
		if e != nil {
			mu.Lock()
			resultErr = errors.Join(resultErr, e)
			mu.Unlock()
		}
	}
	sampleCtx, stopSample := context.WithCancel(ctx)
	samplesDone := make(chan struct{})
	var samples []slowSixSample
	go func() {
		defer close(samplesDone)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			s := slowSixSampleStatus(sampleCtx, p.Socket, origin)
			samples = append(samples, s)
			add(sink.write(sink.status, map[string]any{"phase": name, "sample": s}))
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	if name == "treatment" {
		for i, c := range slow {
			wg.Add(1)
			go func(i int, c *fairnessNoisy50Connection) {
				defer wg.Done()
				add(slowSixPersistent(ctx, offerCtx.Done(), offerEnd, func(run context.Context, step int) error {
					req := slowSixRequests()[(i+step)%2]
					return fairnessNoisy50Call(run, c, p.Repos[i%6], req, name, i, step, origin, sink)
				}))
			}(i, c)
		}
	}
	var readers sync.WaitGroup
	for i, c := range fast {
		readers.Add(1)
		go func(i int, c *fairnessNoisy50Connection) {
			defer readers.Done()
			pace := newPacing(agentProfile, seed+uint64(i))
			for step := 0; offerCtx.Err() == nil; step++ {
				if waitContext(offerCtx, pace.think()) != nil {
					return
				}
				var burst sync.WaitGroup
				for slot := range pace.burst() {
					if offerCtx.Err() != nil {
						break
					}
					burst.Add(1)
					go func(slot int) {
						defer burst.Done()
						req := plans[i%6][(i+step*5+slot)%len(plans[i%6])]
						add(fairnessNoisy50Call(offerCtx, c, p.Repos[i%6], req, name, slot, step, origin, sink))
					}(slot)
				}
				burst.Wait()
			}
		}(i, c)
	}
	readers.Wait()
	<-offerCtx.Done()
	// Keep the sampler until admitted slow calls join, but measured windows are
	// exactly [0,120s). Drain rows/samples stay raw and are labelled separately.
	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()
	timer := time.NewTimer(120 * time.Second)
	defer timer.Stop()
	select {
	case <-joined:
	case <-timer.C:
		add(errors.New("slow calls exceeded bounded product drain; waiting for actual join"))
		add(sink.write(sink.status, map[string]any{"phase": name, "state": "FAILED_DRAIN_QUARANTINE_WAIT_ACTUAL_JOIN"}))
		<-joined
	case <-ctx.Done():
		add(ctx.Err())
		<-joined
	}
	stopSample()
	<-samplesDone
	if err := fairnessNoisy50Idle(ctx, p.Socket); err != nil {
		add(err)
	}
	add(slowSixSampleOracle(samples, int64(120*time.Second)))
	return map[string]any{"phase": name, "seed": seed, "measurement_seconds": 120, "measured_end_ns": int64(120 * time.Second), "drain_end_ns": time.Since(origin).Nanoseconds(), "samples": samples, "sample_interval_ns": int64(200 * time.Millisecond), "sample_oracle_error": fmt.Sprint(slowSixSampleOracle(samples, int64(120*time.Second))), "error": fmt.Sprint(resultErr)}, resultErr
}

func slowSixCollect(parent context.Context, p slowSixPlan) (resultErr error) {
	// Input validation occurs before any private clone, daemon, or output exists.
	corpora, err := loadManifest(p.Manifest)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(p.Authority)
	if err != nil {
		return err
	}
	var authority slowSixAuthority
	if err = json.Unmarshal(raw, &authority); err != nil {
		return err
	}
	if err = p.validateTopology(authority, corpora); err != nil {
		return err
	}
	globalRaw, err := os.ReadFile(authority.GlobalNativeConfig.Path)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(globalRaw)) != authority.GlobalNativeConfig.SHA256 {
		return errors.New("original global config changed")
	}
	if err = slowSixGlobalProfile(globalRaw, authority.OriginalSources); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	for path, key := range map[string]string{p.Binary: "product_sha256", exe: "collector_sha256", p.Manifest: "fixture_sha256", p.HostReceipt: "host_receipt_sha256", p.Authority: "topology_authority_sha256"} {
		h, e := fairnessNoisy50SHA(path)
		if e != nil || h != p.Bindings[key] {
			return errors.Join(fmt.Errorf("changed bound %s", key), e)
		}
	}
	if err = validateOutputRoots(corpora, p.Output); err != nil {
		return err
	}
	if err = os.Mkdir(p.Output, 0700); err != nil {
		return err
	}
	attempts, err := os.OpenFile(filepath.Join(p.Output, "attempts.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	status, err := os.OpenFile(filepath.Join(p.Output, "status.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = attempts.Close()
		return err
	}
	sink := &fairnessNoisy50Sink{attempts: attempts, status: status}
	report := map[string]any{"protocol": slowSixProtocol, "plan": p, "topology_authority": authority, "complete": false, "acceptance": "UNAVAILABLE until exact two-repetition raw evaluation"}
	var run *privateRun
	var connections []*fairnessNoisy50Connection
	defer func() {
		for _, c := range connections {
			resultErr = errors.Join(resultErr, c.client.Close())
		}
		if run != nil {
			stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, forced, e := run.stop(stop)
			cancel()
			resultErr = errors.Join(resultErr, slowSixStopOutcome(forced, e))
			report["cleanup"] = map[string]any{"forced": forced, "stop_error": fmt.Sprint(e), "native_handle_cleared": run.process == nil}
			if run.process == nil {
				resultErr = errors.Join(resultErr, run.removeState(), removeOwnedState(run.SocketDir))
			} else {
				resultErr = errors.Join(resultErr, errors.New("native lifetime unresolved; outer must retain lease"))
			}
		}
		resultErr = errors.Join(resultErr, sink.attempts.Sync(), sink.status.Sync(), sink.attempts.Close(), sink.status.Close(), sink.err)
		report["complete"], report["error"] = resultErr == nil, fmt.Sprint(resultErr)
		data, e := json.MarshalIndent(report, "", "  ")
		resultErr = errors.Join(resultErr, e)
		if e == nil {
			resultErr = errors.Join(resultErr, os.WriteFile(filepath.Join(p.Output, "report.json"), append(data, '\n'), 0600))
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 2880*time.Second)
	defer cancel()
	warm, warmCancel := context.WithTimeout(ctx, 1800*time.Second)
	defer warmCancel()
	before, e := inspectReadbackReferenceHost("reference", p.Scratch)
	report["host_before"] = before
	if e != nil {
		return e
	}
	run, err = newPrivateRun(warm, p.Scratch, p.Binary, corpora)
	if err != nil {
		return err
	}
	report["owned_root"], report["owned_socket_dir"] = run.Root, run.SocketDir
	// Native profile overlays are private authority inputs, never public fixtures.
	for _, repo := range run.Repos {
		cfg := authority.NativeConfigs[repo.Corpus.Label]
		target := filepath.Join(repo.Path, ".gortex.yaml")
		info, e := os.Lstat(target)
		if cfg.Absent {
			if !errors.Is(e, os.ErrNotExist) {
				return errors.New("expected absent native config differs in owned clone")
			}
			continue
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("owned native config is not a regular file")
		}
		raw, e := os.ReadFile(cfg.Path)
		if e != nil {
			return e
		}
		if fmt.Sprintf("%x", sha256.Sum256(raw)) != cfg.SHA256 {
			return errors.New("bound native config changed")
		}
		if e = os.WriteFile(target, raw, 0600); e != nil {
			return e
		}
	}

	// Fixed before startup: setup must never accidentally index an empty global list.
	if err = os.WriteFile(run.Config, []byte(`{"embedding":{"enabled":false}}`+"\n"), 0600); err != nil {
		return err
	}
	report["applied_nonpath_global_config"] = map[string]any{"embedding.enabled": false, "original_config_sha256": authority.GlobalNativeConfig.SHA256, "original_repo_paths_copied": false}
	configured, err := run.writeBootRepos(false)
	if err != nil {
		return err
	}
	start := time.Now()
	if err = run.start(); err != nil {
		return err
	}
	report["daemon_pid"] = run.process.Process.Pid
	if err = run.awaitSocket(warm); err != nil {
		return err
	}
	gate, _, err := run.awaitReady(warm, start, configured)
	report["readiness"] = gate
	if err != nil {
		return err
	}
	var ready readiness
	if err = json.Unmarshal(gate["readiness"], &ready); err != nil {
		return err
	}
	if len(ready.TrackedRepos) != 6 {
		return errors.New("ready catalog is not exactly six repositories")
	}
	for i := range run.Repos {
		for _, rr := range ready.TrackedRepos {
			if filepath.Clean(rr.Path) == filepath.Clean(run.Repos[i].Path) {
				run.Repos[i].Prefix = rr.Prefix
			}
		}
		if run.Repos[i].Prefix == "" {
			return errors.New("six actual ready repository identities required")
		}
	}
	report["actual_repositories"] = run.Repos
	var fast, slow []*fairnessNoisy50Connection
	instance := ""
	sessions := map[string]bool{}
	open := func(repo privateRepo, role string) (*fairnessNoisy50Connection, error) {
		c, e := openFairnessNoisy50Connection(warm, run.Socket, repo.Path, role)
		if e != nil {
			return nil, e
		}
		connections = append(connections, c)
		if sessions[c.SessionID] || instance != "" && c.Instance != instance {
			return nil, errors.New("foreign/shared session or daemon instance")
		}
		sessions[c.SessionID] = true
		instance = c.Instance
		return c, nil
	}
	for i := range 8 {
		c, e := open(run.Repos[i%6], "fast")
		if e != nil {
			return e
		}
		fast = append(fast, c)
	}
	for i := range 3 {
		c, e := open(run.Repos[i%6], "slow")
		if e != nil {
			return e
		}
		slow = append(slow, c)
	}
	report["connections"] = connections
	plans := make([][]request, 6)
	for i, repo := range run.Repos {
		plans[i], err = readingPlan(repo.Corpus)
		if err != nil || len(plans[i]) == 0 {
			return errors.New("frozen fast reading plan unavailable")
		}
	}
	report["fast_request_plans"] = plans
	preflight := &Phase{Gate: map[string]json.RawMessage{}}
	for i, repo := range run.Repos {
		if err = preflightRoot(warm, run, fast[i].client, repo, false, 120*time.Second, preflight); err != nil {
			report["source_preflight"] = preflight
			return err
		}
	}
	report["source_preflight"] = preflight
	warmCancel()
	expectedSet := map[string]bool{}
	var expected []string
	for _, plan := range plans {
		for _, req := range plan {
			if !expectedSet[req.name()] {
				expectedSet[req.name()] = true
				expected = append(expected, req.name())
			}
		}
	}
	report["expected_fast_operations"] = expected
	order := []string{"control", "treatment"}
	if p.Repetition == 2 {
		order = []string{"treatment", "control"}
	}
	var phases []map[string]any
	for _, name := range order {
		phase, e := slowSixPhase(ctx, run, fast, slow, plans, name, p.Seed, sink)
		phases = append(phases, phase)
		report["phases"] = phases
		if e != nil {
			return e
		}
	}
	if err = sink.attempts.Sync(); err != nil {
		return err
	}
	slowIDs := []string{slow[0].SessionID, slow[1].SessionID, slow[2].SessionID}
	var fastIDs []string
	for _, c := range fast {
		fastIDs = append(fastIDs, c.SessionID)
	}
	analysis, e := slowSixAnalyze(filepath.Join(p.Output, "attempts.jsonl"), slowIDs, fastIDs, instance, expected)
	report["analysis"] = analysis
	if e != nil {
		return e
	}
	after, e := inspectReadbackReferenceHost("reference", before.ProbePath)
	report["host_after"] = after
	if e != nil {
		return e
	}
	return nil
}
