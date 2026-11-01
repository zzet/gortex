package main

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestPREditDrainRequiresExplicitVersionTwo(t *testing.T) {
	base := []string{"--binary", "binary", "--manifest", "manifest", "--scratch", "scratch", "--scenario", "edit", "--agents", "0", "--edit-agents", "4", "--worktrees", "0", "--duration", "20s", "--pr-smoke-drain", "2m"}
	if _, err := parseRun(base); err == nil {
		t.Fatal("legacy read drain silently accepted edits")
	}
	if _, err := parseRun(append(base, "--pr-smoke-drain-version", "2")); err != nil {
		t.Fatalf("explicit version2 edit offer/drain refused: %v", err)
	}
	for _, extra := range [][]string{{"--scenario", "read"}, {"--scenario", "edit-and-probe"}, {"--scenario", "many-edit"}, {"--host", "reference"}, {"--duration", "30s"}, {"--deadline", "119s"}, {"--profile", "saturation"}, {"--worktrees", "1"}, {"--pr-smoke-drain-version", "3"}, {"--pr-smoke-drain", "0"}} {
		args := append(slices.Clone(base), "--pr-smoke-drain-version", "2")
		if _, err := parseRun(append(args, extra...)); err == nil {
			t.Fatalf("unsupported edit drain accepted: %v", extra)
		}
	}
}

func TestPREditWindowDrainsEditAndVisibilityWithoutOfferingReads(t *testing.T) {
	for _, held := range []string{"edit", "visibility"} {
		t.Run(held, func(t *testing.T) {
			reply := make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(reply) })
			socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
				if frame.Method == "tools/call" {
					tool, _ := frame.Params["name"].(string)
					if tool == "edit" && held == "edit" || tool == "search" && held == "visibility" {
						<-reply
					}
				}
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": `[{"id":"sample.go::GortexLoadMarkerA0S0"}]`}}}, nil
			})
			client, err := openMCP(context.Background(), socket, "/fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			phase := Phase{}
			o := runOptions{Profile: "saturation", Editors: 1, Duration: 30 * time.Millisecond, PRSmokeDrain: time.Second, PRSmokeDrainVersion: 2, Deadline: time.Second}
			p := &privateRun{Repos: []privateRepo{{Corpus: editProbeCorpus(), Prefix: "sample"}}}
			timer := time.AfterFunc(70*time.Millisecond, func() { release.Do(func() { close(reply) }) })
			defer timer.Stop()
			if err := runPRSmokeWindow(context.Background(), o, p, []*socketClient{client}, true, &phase); err != nil {
				t.Fatalf("admitted %s tail was clipped: %v", held, err)
			}
			var observed prDrainObservation
			if err := json.Unmarshal(phase.Gate["pr_smoke_drain"], &observed); err != nil {
				t.Fatal(err)
			}
			if len(phase.Edits) != 1 || phase.Edits[0].Outcome != "visible" || len(phase.Attempts) != 2 || observed.AdmittedCalls != 1 || observed.AdmittedEdits != 1 || observed.VisibilityProbeAttempts != 1 || observed.Finished.Before(observed.OfferDeadline) {
				t.Fatalf("lost edit/probe census or offered new reads after window: %+v %+v", phase, observed)
			}
			if err := validatePREditDrainObservation(phase, observed, o.Deadline.Seconds()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Exercise the production dispatch and its original twenty-second window, so
// accepting the new flag while retaining phase cancellation cannot pass.
func TestPREditAgentsDrainAcrossOriginalOfferWindow(t *testing.T) {
	reply := make(chan struct{})
	var release sync.Once
	timer := time.AfterFunc(21*time.Second, func() { release.Do(func() { close(reply) }) })
	defer timer.Stop()
	defer release.Do(func() { close(reply) })
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Method == "tools/call" && frame.Params["name"] == "edit" {
			<-reply
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": `[{"id":"sample.go::GortexLoadMarkerA0S0"}]`}}}, nil
	})
	client, err := openMCP(context.Background(), socket, "/fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o := runOptions{Scenario: "edit", Host: "native", Profile: "agent", Editors: 1, Duration: 20 * time.Second, PRSmokeDrain: 120 * time.Second, PRSmokeDrainVersion: 2, Deadline: 120 * time.Second}
	p := &privateRun{Repos: []privateRepo{{Corpus: editProbeCorpus(), Prefix: "sample"}}}
	phase := Phase{}
	if err := runAgents(parent, o, p, []*socketClient{client}, true, &phase); err != nil {
		t.Fatalf("production edit dispatch clipped an admitted tail: %v", err)
	}
	var observed prDrainObservation
	if err := json.Unmarshal(phase.Gate["pr_smoke_drain"], &observed); err != nil {
		t.Fatal(err)
	}
	if observed.AdmittedCalls != 1 || observed.AdmittedEdits != 1 || len(phase.Attempts) != 2 || len(phase.Edits) != 1 || phase.Edits[0].Outcome != "visible" || !observed.Finished.After(observed.OfferDeadline) {
		t.Fatalf("production dispatch lost the tail or offered a new reading burst: %+v %+v", observed, phase)
	}
}

func TestPREditWindowRetainsExpiryCancellationAndFiniteDrainFailures(t *testing.T) {
	for _, name := range []string{"product-expiry", "parent-cancel", "drain-overflow"} {
		t.Run(name, func(t *testing.T) {
			reply := make(chan struct{})
			socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
				if frame.Method == "tools/call" {
					<-reply
				}
				return map[string]any{}, nil
			})
			client, err := openMCP(context.Background(), socket, "/fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			o := runOptions{Profile: "saturation", Editors: 1, Duration: 5 * time.Millisecond, PRSmokeDrain: time.Second, PRSmokeDrainVersion: 2, Deadline: 30 * time.Millisecond}
			want := "product_deadline"
			if name == "parent-cancel" {
				timer := time.AfterFunc(15*time.Millisecond, cancel)
				defer timer.Stop()
				o.Deadline, want = time.Second, "phase_censored"
			}
			if name == "drain-overflow" {
				o.PRSmokeDrain, o.Deadline, want = 10*time.Millisecond, time.Second, "phase_censored"
			}
			phase := Phase{}
			p := &privateRun{Repos: []privateRepo{{Corpus: editProbeCorpus(), Prefix: "sample"}}}
			err = runPRSmokeWindow(parent, o, p, []*socketClient{client}, true, &phase)
			close(reply) // Join every stub request before cleanup, including canceled tails.
			if err == nil || len(phase.Attempts) != 1 || phase.Attempts[0].Outcome != want || len(phase.Edits) != 1 || phase.Edits[0].Outcome == "visible" {
				t.Fatalf("%s lost failure or censor: %+v %v", name, phase, err)
			}
		})
	}
}

func TestPREditDrainChecksDistinctAdmissionsAndProbeLineage(t *testing.T) {
	build := func() (Phase, prDrainObservation) {
		start := time.Now()
		editAt, readAt := start.Add(time.Second), start.Add(2*time.Second)
		phase := Phase{Edits: []EditObservation{{Marker: "GortexLoadMarkerA0S0", Started: editAt, Outcome: "visible", VisibilityMS: measured(21000, "ms")}}, Attempts: []Attempt{
			{Agent: 0, Step: 0, Slot: 0, Tool: "edit.file", Role: "workload", Started: editAt},
			{Agent: 0, Step: 0, Slot: 0, Tool: "search.symbols", Role: "workload", Started: readAt},
			{Agent: 0, Step: 0, Slot: 0, Tool: "search.symbols", Role: "probe", Started: start.Add(21 * time.Second), Outcome: "success"},
		}}
		observed := prDrainObservation{Version: 2, Started: start, OfferDeadline: start.Add(20 * time.Second), DrainDeadline: start.Add(140 * time.Second), Finished: start.Add(22 * time.Second), AdmittedCalls: 2, AdmittedEdits: 1, VisibilityProbeAttempts: 1, Admissions: []prDrainAdmission{
			{Agent: 0, Step: 0, Slot: 0, Tool: "edit.file", OfferedAt: editAt},
			{Agent: 0, Step: 0, Slot: 0, Tool: "search.symbols", OfferedAt: readAt},
		}}
		return phase, observed
	}
	phase, observed := build()
	if err := validatePREditDrainObservation(phase, observed, 120); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"late-offer", "orphan-probe", "missing-edit", "wrong-count", "replayed-call", "missing-first-attempt", "expired-visibility", "unfinished-attempt"} {
		t.Run(mutation, func(t *testing.T) {
			phase, observed := build()
			switch mutation {
			case "late-offer":
				observed.Admissions[1].OfferedAt = observed.OfferDeadline
			case "orphan-probe":
				phase.Attempts[2].Agent = 99
			case "missing-edit":
				phase.Edits = nil
			case "wrong-count":
				observed.VisibilityProbeAttempts++
			case "replayed-call":
				phase.Attempts = append(phase.Attempts, phase.Attempts[0])
			case "missing-first-attempt":
				phase.Attempts[0].Retry = 1
			case "expired-visibility":
				phase.Edits[0].VisibilityMS = measured(120001, "ms")
			case "unfinished-attempt":
				phase.Attempts[2].DurationMS = 2000
			}
			if err := validatePREditDrainObservation(phase, observed, 120); err == nil {
				t.Fatalf("%s was accepted as complete", mutation)
			}
		})
	}
}
