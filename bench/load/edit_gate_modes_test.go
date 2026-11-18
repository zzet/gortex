package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEditGateModesKeepFourEditorScheduleAndJSONACK(t *testing.T) {
	args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", "4", "--worktrees", "0"}
	ordinary, err := parseRun(args)
	if err != nil {
		t.Fatal(err)
	}
	c := editProbeCorpus()
	baseline, err := planEditProbe(c, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	options, err := parseRun(append(args, "--edit-gate-modes"))
	if err != nil {
		t.Fatalf("gate-specific five-mode coverage cannot be planned: %v", err)
	}
	plan, err := planEditProbe(c, options)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Count != 100 || plan.Editors != 4 || plan.Mode != "json" || len(plan.Steps) != 100 {
		t.Fatalf("gate changed the ACK workload: %+v", plan)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Steps []struct {
			Mode string `json:"probe_mode"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	modes := []string{"json", "toon", "compact", "facade", "cli"}
	counts := map[string]int{}
	for i, step := range plan.Steps {
		if got := wire.Steps[i].Mode; got != modes[i%len(modes)] {
			t.Fatalf("ordinal%d planned mode%q, want%q", i, got, modes[i%len(modes)])
		}
		counts[wire.Steps[i].Mode]++
		old := baseline.Steps[i]
		if step.DueNS != old.DueNS || step.ActivateNS != old.ActivateNS || step.Editor != old.Editor || step.Marker != old.Marker || step.MutationID != old.MutationID || step.RequestHash != old.RequestHash || !reflect.DeepEqual(step.Request, old.Request) {
			t.Fatalf("mode coverage changed offered edit/ACK at ordinal%d", i)
		}
	}
	for _, mode := range modes {
		if counts[mode] != 20 {
			t.Fatalf("planned denominator %s=%d, want20 of100 total", mode, counts[mode])
		}
	}
	recordData, err := json.Marshal(newEditProbeReport(plan, c, true))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Records []struct {
			Mode string `json:"probe_mode"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(recordData, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Records) != 100 {
		t.Fatal("censored setup lost offered edits")
	}
	for i, record := range report.Records {
		if record.Mode != modes[i%len(modes)] {
			t.Fatalf("censored ordinal%d lost explicit mode", i)
		}
	}
}

func TestOrdinaryEditPlanHasNoGateModeAssignments(t *testing.T) {
	c := editProbeCorpus()
	old, err := makeEditProbeTrafficPlan(c, 42, 100, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, err := planEditProbe(c, runOptions{Seed: 42, EditCount: 100, Editors: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, got) {
		t.Fatal("ordinary plan changed without gate opt-in")
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if _, exists := wire["gate_modes"]; exists {
		t.Fatal("default hash gained gate metadata")
	}
}

func TestEditGateFacadeRequiresMatchingDispatchEnvelopeAndActualJSON(t *testing.T) {
	makeWire := func(change string) json.RawMessage {
		identity := map[string]any{"surface_version": "facade-v1", "facade": "read", "operation": "source", "canonical_tool": "get_symbol_source"}
		wire := map[string]any{"content": []any{map[string]any{"type": "text", "text": "{\"source\":\"marker\"}"}}, "_meta": map[string]any{"gortex_facade": identity}}
		switch change {
		case "missing":
			delete(wire, "_meta")
		case "wrong_surface":
			identity["surface_version"] = "legacy"
		case "wrong_facade":
			identity["facade"] = "search"
		case "wrong_operation":
			identity["operation"] = "summary"
		case "wrong_canonical":
			identity["canonical_tool"] = "read_file"
		case "structured_only":
			wire["content"] = []any{}
			wire["structuredContent"] = map[string]any{"source": "marker"}
		case "non_json":
			wire["content"] = []any{map[string]any{"type": "text", "text": "source: marker"}}
			wire["structuredContent"] = map[string]any{"source": "marker"}
		}
		data, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	body, err := decodeEditProbeOutput(makeWire(""), "facade", "read.source")
	if err != nil {
		t.Fatalf("actual facade-v1 result cannot be observed: %v", err)
	}
	if body["source"] != "marker" {
		t.Fatal("facade proof replaced actual payload semantics")
	}
	for _, change := range []string{"missing", "wrong_surface", "wrong_facade", "wrong_operation", "wrong_canonical", "structured_only", "non_json"} {
		t.Run(change, func(t *testing.T) {
			if _, err := decodeEditProbeOutput(makeWire(change), "facade", "read.source"); err == nil {
				t.Fatal("dispatch or payload evidence was fabricated")
			}
		})
	}
}

func TestEditGateJSONAndFacadeRefuseNullPayloadWithoutPanic(t *testing.T) {
	for _, mode := range []string{"json", "facade"} {
		t.Run(mode, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Errorf("invalid actual response panicked instead of refusing evidence: %v", value)
				}
			}()
			raw := outputWire("null", nil)
			if mode == "facade" {
				var envelope map[string]any
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
				envelope["_meta"] = map[string]any{"gortex_facade": map[string]any{"surface_version": "facade-v1", "facade": "read", "operation": "summary", "canonical_tool": "get_file_summary"}}
				var err error
				raw, err = json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := decodeEditProbeOutput(raw, mode, "read.summary"); err == nil {
				t.Error("null payload counted as a decoded answer")
			}
		})
	}
}
