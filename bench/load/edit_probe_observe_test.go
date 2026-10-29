package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEditProbeDelayedObservationUsesActualReplyNotObserverOrder(t *testing.T) {
	for _, beforeEdit := range []bool{false, true} {
		t.Run(map[bool]string{false: "after_edit", true: "before_edit"}[beforeEdit], func(t *testing.T) {
			corpus := editProbeCorpus()
			plan, err := makeEditProbePlan(corpus, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Steps[0]
			sink := newEditProbeHeldSink(t)
			sink.unblock()
			state := startHeldEditProbeSpool(t, sink)
			state.step = step
			state.record = newEditProbeRecord(corpus, step)
			state.record.Errors = nil
			origin := time.Now()
			state.started = origin.Add(10 * time.Millisecond)
			run := &editProbeRuntime{origin: origin, plan: plan, p: &privateRun{Repos: []privateRepo{{Corpus: corpus}}}}
			body, err := json.Marshal(map[string]any{"name": step.Marker, "source": strings.ReplaceAll(corpus.EditTemplate, "{{marker}}", step.Marker)})
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(body)}}})
			if err != nil {
				t.Fatal(err)
			}
			finished := origin.Add(15 * time.Millisecond)
			if beforeEdit {
				finished = origin.Add(5 * time.Millisecond)
			}
			identity := wireCallIdentity{WriteStarted: origin.Add(time.Millisecond), Finished: finished}
			if err := run.observeProbe(state, editProbeOracle{name: "source"}, origin, wire, identity, nil, false); err != nil {
				t.Fatal(err)
			}
			first := state.record.FirstCorrect["source"]
			if beforeEdit {
				if first.Value != nil || len(state.record.Errors) != 1 {
					t.Fatalf("late observer hid pre-edit reply: first%+v errors%v", first, state.record.Errors)
				}
			} else {
				if first.Value == nil || *first.Value != 5 {
					t.Fatalf("observer timing replaced actual reply: %+v", first)
				}
				identity.Finished = origin.Add(20 * time.Millisecond)
				if err := run.observeProbe(state, editProbeOracle{name: "source"}, origin, wire, identity, nil, false); err != nil {
					t.Fatal(err)
				}
				if *state.record.FirstCorrect["source"].Value != 5 {
					t.Fatal("later paired probe replaced first success")
				}
			}
		})
	}
}
