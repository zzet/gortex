package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func syntheticSmokeSequence(t *testing.T) (smokeSequenceReceipt, lanePlan, dataset) {
	t.Helper()
	started := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	s := smokeSequenceReceipt{Version: 1, Started: started, Finished: started.Add(time.Minute), FreshStores: 2, ProcessStarts: map[string]int{"main": 18, "candidate": 18}, Complete: true}
	plan := lanePlan{Lane: "smoke", Execution: "two_owned_smoke_stores_v1"}
	var d dataset
	for repeat := 1; repeat <= 3; repeat++ {
		roles := []string{"main", "candidate"}
		if repeat%2 == 0 {
			roles[0], roles[1] = roles[1], roles[0]
		}
		for _, role := range roles {
			for step, name := range smokeSteps {
				task := laneTask{Scenario: name, Role: role, Repeat: repeat, Seed: int64(repeat)}
				plan.Tasks = append(plan.Tasks, task)
				begin := started.Add(time.Duration(len(s.Tasks)*2) * time.Second)
				s.Tasks = append(s.Tasks, taskReceipt{laneTask: task, Started: begin, Finished: begin.Add(time.Second), Report: filepath.Join(fmt.Sprintf("%s-%03d-%s", name, repeat, role), "report.json")})
				starts := (repeat-1)*6 + step
				if step == 4 {
					starts++
				}
				store, err := json.Marshal(map[string]any{"owned_root": "/owned/" + role, "binary": "/build/" + role, "starts_before_phase": starts, "store_present": starts > 0})
				if err != nil {
					t.Fatal(err)
				}
				d.runs = append(d.runs, observation{input: runInput{Scenario: name, Role: role, Repeat: repeat}, data: report{Binary: "/build/" + role, Phases: []phase{{Gate: map[string]json.RawMessage{"pr_smoke_store": store, "pr_smoke_restoration": json.RawMessage(`{"restored_markers":1,"ready_and_enriched":true}`)}}}}})
			}
		}
	}
	return s, plan, d
}

func TestSmokeSequenceRefusesIncompleteForgedOrUnrotatedEvidence(t *testing.T) {
	for _, mutation := range []string{"", "fresh-every-time", "omitted-stop", "wrong-start-count", "role-not-rotated", "overlap", "forged-path", "failed-cleanup"} {
		t.Run(map[string]string{"": "complete"}[mutation]+mutation, func(t *testing.T) {
			s, plan, _ := syntheticSmokeSequence(t)
			switch mutation {
			case "fresh-every-time":
				s.FreshStores = 30
			case "omitted-stop":
				s.Tasks = s.Tasks[:29]
			case "wrong-start-count":
				s.ProcessStarts["main"] = 17
			case "role-not-rotated":
				s.Tasks[10], s.Tasks[15] = s.Tasks[15], s.Tasks[10]
			case "overlap":
				s.Tasks[1].Started = s.Tasks[0].Started
			case "forged-path":
				s.Tasks[0].Report = "../old-report.json"
			case "failed-cleanup":
				s.Complete = false
				s.Error = "forced cleanup"
			}
			err := validateSmokeSequence(s, plan, s.Started.Add(-time.Second), s.Finished.Add(time.Second))
			if mutation == "" && err != nil || mutation != "" && err == nil {
				t.Fatalf("wrong sequence admission: %v", err)
			}
		})
	}
}

func TestSmokeStoreProofRequiresIsolationAndRestoration(t *testing.T) {
	for _, mutation := range []string{"", "shared-store", "changed-store", "missing-proof", "fresh-restart", "pending-marker", "wrong-binary"} {
		t.Run(map[string]string{"": "complete"}[mutation]+mutation, func(t *testing.T) {
			_, _, d := syntheticSmokeSequence(t)
			gate := d.runs[2].data.Phases[0].Gate
			switch mutation {
			case "shared-store":
				for _, run := range d.runs {
					var value map[string]any
					if err := json.Unmarshal(run.data.Phases[0].Gate["pr_smoke_store"], &value); err != nil {
						t.Fatal(err)
					}
					value["owned_root"] = "/same/store"
					data, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					run.data.Phases[0].Gate["pr_smoke_store"] = data
				}
			case "changed-store":
				gate["pr_smoke_store"] = json.RawMessage(`{"owned_root":"/another/store","binary":"/build/main","starts_before_phase":2,"store_present":true}`)
			case "missing-proof":
				delete(gate, "pr_smoke_store")
			case "fresh-restart":
				gate["pr_smoke_store"] = json.RawMessage(`{"owned_root":"/owned/main","binary":"/build/main","starts_before_phase":2,"store_present":false}`)
			case "pending-marker":
				gate["pr_smoke_restoration"] = json.RawMessage(`{"restored_markers":0,"ready_and_enriched":false}`)
			case "wrong-binary":
				d.runs[2].data.Binary = "/other/build"
			}
			err := validateSmokeStores(d)
			if mutation == "" && err != nil || mutation != "" && err == nil {
				t.Fatalf("wrong retained-store admission: %v", err)
			}
		})
	}
}
