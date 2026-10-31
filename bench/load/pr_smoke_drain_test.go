package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type prReleasedCaller struct{ reply <-chan struct{} }

func (c prReleasedCaller) call(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.reply:
		return json.RawMessage(`{"content":[{"text":"{}"}]}`), nil
	}
}

func TestPRReadWindowDrainsAdmittedReplyWithoutNewOffers(t *testing.T) {
	var attempts []Attempt
	observation, err := runOfferedWindow(context.Background(), 30*time.Millisecond, time.Second, func(offers, drain context.Context) error {
		if !canOfferPRRead(offers, drain) {
			return errors.New("initial read admission unexpectedly closed")
		}
		reply := make(chan struct{})
		go func() { <-offers.Done(); close(reply) }()
		_, rows, err := callTool(drain, prReleasedCaller{reply}, request{Tool: "search"}, nil, "workload", 0, 0, 0, 200*time.Millisecond, waitContext)
		attempts = rows
		if canOfferPRRead(offers, drain) {
			return errors.New("new read was admissible after offered window")
		}
		return err
	})
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != "success" {
		t.Fatalf("admitted late reply was clipped at offer end: %+v %v", attempts, err)
	}
	if observation.Finished.Before(observation.OfferDeadline) || observation.ElapsedSeconds < .03 {
		t.Fatal("drain time disappeared from observed wall time")
	}
}

func TestPRReadWindowRetainsProductExpiryParentCancellationAndDrainOverflow(t *testing.T) {
	for _, name := range []string{"product-expiry", "parent-cancel", "drain-overflow"} {
		t.Run(name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			offered, drainFor, product := 5*time.Millisecond, 200*time.Millisecond, 20*time.Millisecond
			want := "product_deadline"
			if name == "parent-cancel" {
				timer := time.AfterFunc(2*time.Millisecond, cancel)
				defer timer.Stop()
				want = "phase_censored"
			}
			if name == "drain-overflow" {
				drainFor = 5 * time.Millisecond
				product = time.Second
				want = "phase_censored"
			}
			var attempts []Attempt
			_, err := runOfferedWindow(parent, offered, drainFor, func(_ context.Context, drain context.Context) error {
				_, rows, err := callTool(drain, blockingCaller{}, request{Tool: "search"}, nil, "workload", 0, 0, 0, product, waitContext)
				attempts = rows
				return err
			})
			if err == nil || len(attempts) != 1 || attempts[0].Outcome != want {
				t.Fatalf("%s lost its failure/censor classification: %+v %v", name, attempts, err)
			}
		})
	}
}

func TestPRReadDrainIsExplicitAndRefusesUnsupportedWork(t *testing.T) {
	base := []string{"--binary", "binary", "--manifest", "manifest", "--scratch", "scratch", "--scenario", "read", "--agents", "16", "--edit-agents", "0", "--worktrees", "0", "--duration", "20s", "--pr-smoke-drain", "2m"}
	if _, err := parseRun(base); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{{"--scenario", "edit"}, {"--scenario", "edit-and-probe"}, {"--scenario", "all"}, {"--scenario", "worktrees"}, {"--host", "reference"}, {"--host", "floor"}, {"--profile", "saturation"}, {"--duration", "60s"}, {"--pr-smoke-drain", "1s"}, {"--pr-smoke-drain", "3m"}, {"--cpu-profile", "1s"}, {"--keep-state"}} {
		args := append(slices.Clone(base), extra...)
		if _, err := parseRun(args); err == nil {
			t.Fatalf("unsupported PR drain accepted: %v", extra)
		}
	}
	disabled := []string{"--binary", "binary", "--manifest", "manifest", "--scratch", "scratch"}
	options, err := parseRun(disabled)
	if err != nil || options.PRSmokeDrain != 0 {
		t.Fatal("legacy default acquired drain semantics")
	}
	_, _, err = parsePaired([]string{"--build", "tag=tag", "--build", "main=main", "--manifest", "manifest", "--scratch", "scratch", "--scenario", "read", "--agents", "16", "--edit-agents", "0", "--duration", "20s", "--pr-smoke-drain", "2m"})
	if err == nil || !strings.Contains(err.Error(), "standalone-only") {
		t.Fatal("coupled paired mode silently accepted new drain identity")
	}
}

func TestPRDrainLeavesFrozenReportAndWorkloadIdentityUnchanged(t *testing.T) {
	data, err := os.ReadFile("testdata/report-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	hash, err := hashJSON(report.Workload)
	if err != nil || hash != report.RequestPlan {
		t.Fatalf("frozen workload hash changed with disabled drain: %s %s %v", hash, report.RequestPlan, err)
	}
	encoded, err := json.Marshal(report.Workload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("pr_smoke_drain")) {
		t.Fatal("disabled optional pointer changed frozen JSON")
	}
	report.Workload.PRSmokeDrain = &PRSmokeDrain{Version: 1, OfferedSeconds: 20, MaxDrainSeconds: 120}
	next, err := hashJSON(report.Workload)
	if err != nil || next == hash {
		t.Fatal("explicit drain semantics are missing from workload identity")
	}
}

func TestPRDrainSchemaAddsOnlyItsOptionalPointer(t *testing.T) {
	data, err := os.ReadFile("testdata/report-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var frozen map[string]any
	if err := json.Unmarshal(data, &frozen); err != nil {
		t.Fatal(err)
	}
	prior := frozen["properties"].(map[string]any)["workload"].(map[string]any)
	current := schemaFor(reflect.TypeFor[Workload]())
	before, _ := json.Marshal(prior["required"])
	after, _ := json.Marshal(current["required"])
	if !bytes.Equal(before, after) {
		t.Fatal("historical Workload required list changed")
	}
	if slices.Contains(current["required"].([]string), "pr_smoke_drain") {
		t.Fatal("new optional pointer became a required frozen field")
	}
	if _, ok := current["properties"].(map[string]any)["pr_smoke_drain"]; !ok {
		t.Fatal("drain identity absent from schema")
	}
}

func TestCompletePRDrainNeedsObservedTimingAndAdmissionReceipts(t *testing.T) {
	build := func(t *testing.T) Report {
		t.Helper()
		r := sampleReport(t)
		r.Workload.DurationSec = 20
		r.Workload.PRSmokeDrain = &PRSmokeDrain{Version: 1, OfferedSeconds: 20, MaxDrainSeconds: 120}
		var err error
		r.RequestPlan, err = hashJSON(r.Workload)
		if err != nil {
			t.Fatal(err)
		}
		started := r.Phases[0].Started
		observed := prDrainObservation{Version: 1, Started: started, OfferDeadline: started.Add(20 * time.Second), DrainDeadline: started.Add(140 * time.Second), Finished: started.Add(21 * time.Second), ElapsedSeconds: 21, AfterOfferDeadlineSeconds: 1, AdmittedCalls: 1}
		data, err := json.Marshal(observed)
		if err != nil {
			t.Fatal(err)
		}
		r.Phases[0].Gate["pr_smoke_drain"] = data
		r.Phases[0].DurationSec = 21
		r.Phases[0].Attempts = []Attempt{{Role: "workload", Tool: "search.symbols", Agent: 0, Step: 0, Slot: 0, Outcome: "success", LogicalMS: 1}}
		r.Phases[0].Tools = summarize(r.Phases[0].Attempts, r.Phases[0].DurationSec, "workload")
		return r
	}
	r := build(t)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReport(data, r); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"missing-receipt", "short-duration", "wrong-admission-count", "drain-overrun"} {
		t.Run(mutation, func(t *testing.T) {
			r := build(t)
			if mutation == "missing-receipt" {
				delete(r.Phases[0].Gate, "pr_smoke_drain")
			} else {
				var observed prDrainObservation
				if err := json.Unmarshal(r.Phases[0].Gate["pr_smoke_drain"], &observed); err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "short-duration":
					r.Phases[0].DurationSec = 20
				case "wrong-admission-count":
					observed.AdmittedCalls = 2
				case "drain-overrun":
					observed.Finished = observed.DrainDeadline.Add(time.Second)
				}
				data, err := json.Marshal(observed)
				if err != nil {
					t.Fatal(err)
				}
				r.Phases[0].Gate["pr_smoke_drain"] = data
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateReport(data, r); err == nil {
				t.Fatalf("%s silently accepted as complete drain evidence", mutation)
			}
		})
	}
}
