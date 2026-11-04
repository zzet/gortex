package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Exercise the actual activation, FIFO and multiplexed wire path. Queued
// acceptance offers must not create ordinary oracle traffic before their edit;
// ordinary open-loop activation remains independently observable.
func TestSearchAcceptanceQueuedOffersDoNotFanOutOrdinaryOracles(t *testing.T) {
	for _, acceptance := range []bool{true, false} {
		name := "ordinary_open_loop_control"
		if acceptance {
			name = "queued_search_acceptance"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			corpus := editProbeRenameCorpus(t, "go", 1000)
			corpus.EditTool = "edit.file"
			plan, err := makeEditProbeTrafficPlanSized(corpus, 42, 1000, 8, time.Second, true)
			if err != nil {
				t.Fatal(err)
			}
			plan.SearchReadbackAcceptance = acceptance
			plan.Steps = plan.Steps[:3]
			for i := range plan.Steps {
				plan.Steps[i].Editor = 0
			}
			r := &editProbeRuntime{plan: plan, options: runOptions{Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Prefix: "fixture", Corpus: corpus}}}, phase: &Phase{Metrics: make(map[string]Metric)}, origin: time.Now(), modeCounts: make(map[string]editProbeModeCounts)}
			calls := make(chan string, 32)
			for _, step := range plan.Steps {
				sink := newEditProbeHeldSink(t)
				sink.unblock()
				s := startHeldEditProbeSpool(t, sink)
				s.step, s.record = step, newEditProbeRecord(corpus, step)
				s.ackReady = make(chan struct{})
				s.client = searchSequenceClient(t, func(frame stubFrame) any {
					if frame.Method == "tools/call" {
						name, _ := frame.Params["name"].(string)
						calls <- name
						<-ctx.Done()
					}
					return stubContent(map[string]any{})
				})
				r.states = append(r.states, s)
			}
			release, admitted := r.waitSearchReadbackTurn(ctx, plan.Steps[0])
			if !admitted {
				t.Fatal("initial actual editor turn was not admitted")
			}
			defer release()
			for _, step := range plan.Steps[1:] {
				r.activate(ctx, step)
			}
			joined := make(chan struct{})
			go func() {
				for _, s := range r.states[1:] {
					s.probes.Wait()
				}
				close(joined)
			}()
			observed := 0
			if acceptance {
				select {
				case tool := <-calls:
					observed++
					t.Errorf("queued acceptance offer sent an ordinary %s oracle before acquiring its editor turn", tool)
				case <-joined:
				case <-ctx.Done():
					t.Error("queued activation did not finish without ordinary oracle loops")
				}
			} else {
			ordinaryCalls:
				for observed < 16 { // Two real offers, eight unchanged oracles each.
					select {
					case <-calls:
						observed++
					case <-ctx.Done():
						t.Errorf("ordinary activation lost its oracle population: observed=%d", observed)
						break ordinaryCalls
					}
				}
			}
			cancel()
			var workers sync.WaitGroup
			for _, s := range r.states {
				workers.Go(s.stopProbes)
			}
			workers.Wait()
			<-joined
			t.Logf("acceptance=%v actual_prober_clients=3 actual_pre_edit_oracle_calls_at_decision=%d edits_dispatched=0 all_probe_loops_joined=true", acceptance, observed)
			for _, s := range r.states[1:] {
				if s.record.StartedNS != nil {
					t.Error("the activation control dispatched an edit")
				}
			}
		})
	}
}
