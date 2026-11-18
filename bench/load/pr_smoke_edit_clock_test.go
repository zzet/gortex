package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// Exact Windows run41612127031 values: mono span70.4689ms, wall
// span70.4677ms. The actual next probe begins at the phase endpoint.
func windowsPREditClockCase(t *testing.T) (Phase, prDrainObservation) {
	t.Helper()
	start, err := time.Parse(time.RFC3339Nano, "2026-11-03T10:09:49.0343837Z")
	if err != nil {
		t.Fatal(err)
	}
	finished := start.Add(70467700 * time.Nanosecond)
	phase := Phase{Attempts: []Attempt{
		{Agent: 0, Step: 0, Slot: 0, Tool: "edit.file", Role: "workload", Started: start, DurationMS: 70.4689},
		{Agent: 0, Step: 0, Slot: 0, Tool: "search.symbols", Role: "probe", Started: finished, DurationMS: 0, Outcome: "success"},
	}, Edits: []EditObservation{{Marker: "GortexLoadMarkerA0S0", Started: start, Outcome: "visible", VisibilityMS: measured(70.4689, "ms")}}}
	raw := map[string]any{"schema_version": 2, "started": start, "finished": finished, "offer_deadline": start.Add(30 * time.Millisecond), "drain_deadline": start.Add(time.Second), "elapsed_seconds": .0704689, "admitted_calls": 1, "admitted_edits": 1, "visibility_probe_attempts": 1, "admissions": []prDrainAdmission{{Agent: 0, Step: 0, Slot: 0, Tool: "edit.file", OfferedAt: start}}, "edit_clock": map[string]any{"semantics": "live_monotonic_offsets_nanoseconds", "elapsed_ns": 70468900, "offered_ns": 30000000, "drain_ns": 1000000000, "phase_window_start_ns": 0, "admissions": []map[string]any{{"agent": 0, "step": 0, "slot": 0, "retry": 0, "role": "admission", "tool": "edit.file", "at": start, "offset_ns": 0}}, "attempts": []map[string]any{{"agent": 0, "step": 0, "slot": 0, "retry": 0, "role": "workload", "tool": "edit.file", "at": start, "offset_ns": 0}, {"agent": 0, "step": 0, "slot": 0, "retry": 0, "role": "probe", "tool": "search.symbols", "at": finished, "offset_ns": 70468900}}, "edits": []map[string]any{{"agent": 0, "step": 0, "slot": 0, "retry": 0, "role": "edit", "tool": "GortexLoadMarkerA0S0", "at": start, "offset_ns": 0}}}}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var observed prDrainObservation
	if err := json.Unmarshal(data, &observed); err != nil {
		t.Fatal(err)
	}
	// Round-trip the attempts too: all timestamps are wall trace after JSON.
	data, err = json.Marshal(phase)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &phase); err != nil {
		t.Fatal(err)
	}
	return phase, observed
}

func TestPREditClockWindowsJSONCounterexample(t *testing.T) {
	phase, o := windowsPREditClockCase(t)
	if err := validatePREditDrainObservation(phase, o, 1); err != nil {
		t.Fatalf("actual Windows completed mono interval rejected after JSON: %v", err)
	}
}
func TestPREditClockKeepsStrictFailures(t *testing.T) {
	for _, name := range []string{"late-attempt", "negative-duration", "nan-duration", "infinite-duration", "negative-offset", "missing-clock", "missing-attempt", "mismatched-attempt", "replayed-attempt", "mismatched-admission", "late-admission", "missing-edit", "mismatched-edit", "elapsed-mismatch"} {
		t.Run(name, func(t *testing.T) {
			phase, o := windowsPREditClockCase(t)
			switch name {
			case "late-attempt":
				phase.Attempts[0].DurationMS += .001
			case "negative-duration":
				phase.Attempts[0].DurationMS = -1
			case "nan-duration":
				phase.Attempts[0].DurationMS = math.NaN()
			case "infinite-duration":
				phase.Attempts[0].DurationMS = math.Inf(1)
			case "negative-offset":
				o.EditClock.Attempts[0].OffsetNS = -1
			case "missing-clock":
				o.EditClock = nil
			case "missing-attempt":
				o.EditClock.Attempts = o.EditClock.Attempts[:1]
			case "mismatched-attempt":
				o.EditClock.Attempts[0].Tool = "search.symbols"
			case "replayed-attempt":
				phase.Attempts = append(phase.Attempts, phase.Attempts[0])
				o.EditClock.Attempts = append(o.EditClock.Attempts, o.EditClock.Attempts[0])
			case "mismatched-admission":
				o.EditClock.Admissions[0].Agent = 1
			case "late-admission":
				o.EditClock.Admissions[0].OffsetNS = o.EditClock.OfferedNS
			case "missing-edit":
				o.EditClock.Edits = nil
			case "mismatched-edit":
				o.EditClock.Edits[0].Tool = "foreign"
			case "elapsed-mismatch":
				o.ElapsedSeconds += .001
			}
			if err := validatePREditDrainObservation(phase, o, 1); err == nil {
				t.Fatal("invalid same-clock receipt accepted")
			}
		})
	}
}
func TestPREditClockOuterPhaseUsesSameClock(t *testing.T) {
	phase, o := windowsPREditClockCase(t)
	phase.Name = "edit"
	phase.Started = o.Started
	phase.DurationSec = o.ElapsedSeconds
	o.AfterOfferDeadlineSeconds = .0404689
	r := Report{Complete: true, Phases: []Phase{phase}}
	r.Workload.Scenario = "edit"
	r.Workload.DeadlineSec = 1
	r.Workload.PRSmokeDrain = &PRSmokeDrain{Version: 2, OfferedSeconds: .03, MaxDrainSeconds: .97}
	check := func() error {
		data, err := json.Marshal(o)
		if err != nil {
			return err
		}
		r.Phases[0].Gate = map[string]json.RawMessage{"pr_smoke_drain": data}
		return validatePRDrainObservation(r)
	}
	if err := check(); err != nil {
		t.Fatalf("same clock valid outer phase rejected: %v", err)
	}
	r.Phases[0].DurationSec -= .001
	if err := check(); err == nil {
		t.Fatal("completion beyond real same-clock phase accepted")
	}
	r.Phases[0].DurationSec = phase.DurationSec
	o.EditClock.PhaseWindowStartNS = 1
	if err := check(); err == nil {
		t.Fatal("late window completion accepted")
	}
}
func TestPREditClockMissingJSONFieldsAreNotMeasuredZero(t *testing.T) {
	_, o := windowsPREditClockCase(t)
	for _, field := range []string{"elapsed_ns", "phase_window_start_ns", "admissions", "attempts", "edits"} {
		t.Run(field, func(t *testing.T) {
			raw, err := json.Marshal(o.EditClock)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if err = json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			delete(m, field)
			raw, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			var c prEditClock
			if err = json.Unmarshal(raw, &c); err == nil {
				t.Fatal("missing clock field accepted")
			}
		})
	}
	raw, err := json.Marshal(o.EditClock)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err = json.Unmarshal(m["attempts"], &rows); err != nil {
		t.Fatal(err)
	}
	delete(rows[0], "offset_ns")
	m["attempts"], err = json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var c prEditClock
	if err = json.Unmarshal(raw, &c); err == nil {
		t.Fatal("missing zero start offset accepted")
	}
}
func TestPRReadClockJSONRemainsAbsent(t *testing.T) {
	raw, err := json.Marshal(prDrainObservation{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, exists := m["edit_clock"]; exists {
		t.Fatal("v1 JSON changed")
	}
}
