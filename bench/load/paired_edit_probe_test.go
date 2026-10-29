package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func pairedEditProbeFixture(t *testing.T, ctx context.Context, count int, handle func(string, stubFrame) (any, *rpcError)) *pairedCoordinator {
	t.Helper()
	options := runOptions{Seed: 1, Deadline: time.Second}
	c, err := newPairedCoordinator(ctx, options, []string{"baseline", "candidate"}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.cancel)
	corpus := editProbeCorpus()
	plan, err := makeEditProbePlan(corpus, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	plan.Steps = plan.Steps[:count]
	plan.Count = count
	for i, label := range c.labels {
		socket := startStub(t, func(frame stubFrame) (any, *rpcError) { return handle(label, frame) })
		editor, err := openMCP(ctx, socket, "/owned/"+label)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = editor.Close() })
		root := t.TempDir()
		p := &privateRun{Root: root, Store: filepath.Join(root, "store.db"), Socket: socket, Repos: []privateRepo{{Corpus: corpus, Prefix: "sample"}}}
		phase := &Phase{Metrics: make(map[string]Metric), EditProbe: newEditProbeReport(plan, corpus, i != 0)}
		r := &editProbeRuntime{options: options, p: p, phase: phase, plan: plan, editor: editor, origin: time.Now(), requireAccounting: i != 0}
		phase.editProbeRuntime = r
		clients := []*socketClient{editor}
		for _, step := range plan.Steps {
			prober, err := openMCP(ctx, socket, "/owned/"+label)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = prober.Close() })
			sink := newEditProbeHeldSink(t)
			sink.unblock()
			state := startHeldEditProbeSpool(t, sink)
			state.step = step
			state.record = newEditProbeRecord(corpus, step)
			state.record.Errors = nil
			state.client = prober
			r.states = append(r.states, state)
			clients = append(clients, prober)
		}
		c.states[i] = &pairedState{p: p, clients: clients, phase: phase}
	}
	return c
}

func TestPairedEditOwnACKStartsReadbackBeforePartnerACK(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	readback, partner := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce, readOnce, partnerOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	c := pairedEditProbeFixture(t, ctx, 1, func(label string, frame stubFrame) (any, *rpcError) {
		if frame.Params["name"] == "edit" {
			if label == "candidate" {
				partnerOnce.Do(func() { close(partner) })
				<-release
			}
			return stubContent(map[string]any{"mutation_receipt": label + "-receipt"}), nil
		}
		if label == "baseline" && frame.Params["name"] == "read" {
			readOnce.Do(func() { close(readback) })
		}
		return stubContent(map[string]any{}), nil
	})
	done := make(chan struct{})
	t.Cleanup(func() { unblock(); cancel(); <-done; c.editContinuations.Wait() })
	go func() {
		defer close(done)
		step := c.states[0].phase.editProbeRuntime.plan.Steps[0]
		c.callAcross(ctx, step.Request, 0, 0, 0, 0, "workload", time.Second, -1)
	}()
	awaitEditProbeSpool(t, partner)
	awaitEditProbeSpool(t, readback) // Partner edit still cannot return its ACK.
	unblock()
	cancel()
	awaitEditProbeSpool(t, done)
	c.editContinuations.Wait()
	first := c.states[0].phase.editProbeRuntime.states[0].record
	if first.Receipt != "baseline-receipt" || len(first.Readbacks) == 0 {
		t.Fatalf("own ACK/readback lost: %+v", first)
	}
	if len(c.records) != 1 || len(c.records[0].Samples) != 2 {
		t.Fatal("partner failure/cancel erased pair denominator")
	}
}

func TestPairedEditBlockedProbeCannotDelayOfferedEdits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	probe, edited := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce, probeOnce sync.Once
	var mu sync.Mutex
	editCount := 0
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	c := pairedEditProbeFixture(t, ctx, 1, func(label string, frame stubFrame) (any, *rpcError) {
		if frame.Connection > 1 {
			probeOnce.Do(func() { close(probe) })
			<-release
		}
		if frame.Params["name"] == "edit" {
			mu.Lock()
			editCount++
			if editCount == 2 {
				close(edited)
			}
			mu.Unlock()
			return stubContent(map[string]any{"mutation_receipt": label + "-receipt"}), nil
		}
		return stubContent(map[string]any{}), nil
	})
	origin := time.Now()
	plan := c.states[0].phase.editProbeRuntime.plan
	plan.Steps[0].ActivateNS = 0
	plan.Steps[0].DueNS = int64(25 * time.Millisecond)
	for _, state := range c.states {
		state.phase.editProbeRuntime.origin = origin
		state.phase.editProbeRuntime.plan = plan
	}
	done := make(chan struct{})
	t.Cleanup(func() { unblock(); cancel(); <-done; c.editContinuations.Wait() })
	go func() {
		defer close(done)
		_ = runEditProbeArrivals(ctx, plan, origin, c.activatePairedEditProbe, func(ctx context.Context, step editProbeStep) {
			c.callAcross(ctx, step.Request, 0, 0, step.Ordinal, 0, "workload", time.Second, -1)
		}, waitContext)
	}()
	awaitEditProbeSpool(t, probe)
	awaitEditProbeSpool(t, edited)
	cancel()
	unblock()
	awaitEditProbeSpool(t, done)
	c.editContinuations.Wait()
}

func TestPairedEditBlockedACKCannotShiftNextOfferedPair(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	held, next := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce, heldOnce, nextOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	c := pairedEditProbeFixture(t, ctx, 2, func(label string, frame stubFrame) (any, *rpcError) {
		if frame.Params["name"] == "edit" {
			args, _ := frame.Params["arguments"].(map[string]any)
			opts, _ := args["options"].(map[string]any)
			if opts["mutation_id"] == "load-edit-0000000000000001-000" && label == "baseline" {
				heldOnce.Do(func() { close(held) })
				<-release
			}
			if opts["mutation_id"] == "load-edit-0000000000000001-001" {
				nextOnce.Do(func() { close(next) })
			}
			return stubContent(map[string]any{"mutation_receipt": label + "-receipt"}), nil
		}
		return stubContent(map[string]any{}), nil
	})
	origin := time.Now()
	plan := c.states[0].phase.editProbeRuntime.plan
	for i := range plan.Steps {
		plan.Steps[i].ActivateNS = 0
		plan.Steps[i].DueNS = int64(time.Duration(i+1) * 25 * time.Millisecond)
	}
	for _, state := range c.states {
		state.phase.editProbeRuntime.origin = origin
		state.phase.editProbeRuntime.plan = plan
	}
	done := make(chan struct{})
	t.Cleanup(func() { unblock(); cancel(); <-done; c.editContinuations.Wait() })
	go func() {
		defer close(done)
		_ = runEditProbeArrivals(ctx, plan, origin, func(context.Context, editProbeStep) {}, func(ctx context.Context, step editProbeStep) {
			c.callAcross(ctx, step.Request, 0, 0, step.Ordinal, 0, "workload", time.Second, -1)
		}, waitContext)
	}()
	awaitEditProbeSpool(t, held)
	awaitEditProbeSpool(t, next)
	cancel()
	unblock()
	awaitEditProbeSpool(t, done)
	c.editContinuations.Wait()
}

func TestPairedEditPolicyKeepsBaselinePhasesExplicitAndPlanExact(t *testing.T) {
	args := []string{"--build", "main=/private/main", "--build", "candidate=/private/candidate", "--manifest", "/private/manifest.json", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", "1", "--worktrees", "0", "--edit-count", "100"}
	options, builds, err := parsePaired(args)
	if err != nil || len(builds) != 2 || options.EditCount != 100 {
		t.Fatalf("initial paired policy: %v %v", builds, err)
	}
	if _, _, err := parsePaired(append(args, "--build", "third=/private/third")); err == nil {
		t.Fatal("third build silently widened initial paired cell")
	}
	// Baseline missing instrumentation is a qualification, not a measured zero.
	baseline := newEditProbeReport(editProbePlan{}, Corpus{}, false)
	candidate := newEditProbeReport(editProbePlan{}, Corpus{}, true)
	if baseline.AccountingRequired || !candidate.AccountingRequired {
		t.Fatal("baseline/candidate accounting roles lost")
	}
	missing := checkEditProbeAccounting(map[string]any{"receipt": "r"}, "r")
	if missing.State == "observed_single_window_checks_pass" || missing.A != nil || missing.B != nil {
		t.Fatal("absent baseline phases became zero or pass")
	}
}

func TestPairedEditPreparationRejectsForeignPlanSharedStoreAndWrongAccountingRole(t *testing.T) {
	for _, name := range []string{"valid", "foreign_plan", "shared_store", "wrong_role"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := pairedEditProbeFixture(t, ctx, 1, func(string, stubFrame) (any, *rpcError) {
				return stubContent(map[string]any{}), nil
			})
			candidate := c.states[1].phase.editProbeRuntime
			switch name {
			case "foreign_plan":
				candidate.plan.Steps = append([]editProbeStep(nil), candidate.plan.Steps...)
				candidate.plan.Steps[0].MutationID = "foreign"
			case "shared_store":
				candidate.p.Store = c.states[0].p.Store
			case "wrong_role":
				candidate.requireAccounting = false
			}
			if err := c.preparePairedEditProbe(); (err != nil) != (name != "valid") {
				t.Fatalf("prepared %s: %v", name, err)
			}
		})
	}
}

func TestPairedEvidenceFailureCancelsPartnerAndRefusesEarlierCompleteResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c := pairedEditProbeFixture(t, ctx, 1, func(string, stubFrame) (any, *rpcError) {
		return stubContent(map[string]any{}), nil
	})
	baseline := c.states[0].phase.editProbeRuntime
	candidate := c.states[1].phase.editProbeRuntime
	baseline.states[0].record.Outcome = "complete"
	state := candidate.states[0]
	sink := state.sink.(*editProbeHeldSink)
	sink.mu.Lock()
	sink.writeErr = errors.New("owned evidence disk failure")
	sink.mu.Unlock()
	if err := state.emit(map[string]any{"event": "held-probe"}); err != nil {
		t.Fatal(err)
	}
	state.finishRaw() // Actual failing write is joined before its next observation.
	err := state.emit(map[string]any{"event": "next-probe"})
	if !errors.Is(err, errEditProbeEvidenceIncomplete) {
		t.Fatalf("actual evidence failure was not visible: %v", err)
	}
	shared, stop := context.WithCancel(ctx)
	defer stop()
	c.cancelPairedEditEvidence(0, stop)
	if shared.Err() == nil {
		t.Fatal("partner continued under a product deadline after coupled evidence loss")
	}
	for _, r := range []*editProbeRuntime{baseline, candidate} {
		if err := r.finish(); err == nil {
			t.Fatal("coupled evidence loss became success")
		}
		if len(r.phase.EditProbe.Records) != 1 || len(r.phase.EditProbe.Records[0].Errors) == 0 {
			t.Fatal("evidence loss erased the edit denominator/cause")
		}
	}
	if baseline.phase.EditProbe.Records[0].Outcome != "diagnostic_failure" || candidate.phase.EditProbe.Records[0].Outcome != "harness_evidence_incomplete" {
		t.Fatal("late partner failure left an earlier complete result or hid failed evidence")
	}
}

func TestPairedCanceledPlanRetainsEveryUnstartedEdit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := pairedEditProbeFixture(t, ctx, 2, func(string, stubFrame) (any, *rpcError) {
		return stubContent(map[string]any{}), nil
	})
	c.window.Started = time.Now()
	cancel()
	if err := c.editProbes(ctx); err == nil {
		t.Fatal("canceled offered plan became success")
	}
	for _, state := range c.states {
		if len(state.phase.EditProbe.Records) != 2 {
			t.Fatal("cancellation dropped planned entries")
		}
		for _, record := range state.phase.EditProbe.Records {
			if record.Outcome != "phase_censored" || record.Ack.Value != nil || record.StartedNS != nil {
				t.Fatal("unstarted edit acquired dispatch/ACK evidence or lost censoring")
			}
		}
	}
}
