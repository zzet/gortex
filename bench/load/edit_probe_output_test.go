package main

import (
	"encoding/json"
	"reflect"
	"testing"

	toon "github.com/toon-format/toon-go"
)

func outputWire(text string, structured any) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "structuredContent": structured})
	return raw
}
func TestEditProbeTOONEvaluatesActualTextAndPreservesMissingFields(t *testing.T) {
	producer := map[string]any{"nodes": []any{map[string]any{"id": "fixture/main.go::Added", "file_path": "fixture/main.go", "kind": "function", "name": "Added", "start_line": 7}}, "edges": []any{}, "total": 1, "truncated": false}
	encoded, err := toon.Marshal(producer)
	if err != nil {
		t.Fatal(err)
	}
	body, err := decodeEditProbeOutput(outputWire(string(encoded), map[string]any{"nodes": []any{}}), "toon", "read.summary")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := body["total_nodes"]; exists {
		t.Fatal("invented omitted JSON total_nodes")
	}
	if _, exists := body["total_edges"]; exists {
		t.Fatal("invented omitted edge count")
	}
	node := body["nodes"].([]any)[0].(map[string]any)
	if node["id"] != "fixture/main.go::Added" || node["start_line"] != json.Number("7") || body["total"] != json.Number("1") {
		t.Fatal(body)
	}
	if _, exists := node["meta"]; exists {
		t.Fatal("invented compact-row metadata")
	}
	if proof, ok := body[editProbeTOONWireKey].(editProbeTOONWire); !ok || proof.operation != "read.summary" {
		t.Fatal("requested decoder proof missing")
	}
}
func TestEditProbeTOONRefusesWrongFormatSiblingAndIncompleteWire(t *testing.T) {
	validJSON := map[string]any{"id": "fixture/main.go::Added"}
	for name, text := range map[string]string{"JSON_fallback": `{"id":"fixture/main.go::Added"}`, "scalar": "false", "empty": "", "truncated_table": "nodes[2]{id}:\n  one\n", "unsafe_integer": "owner: 9007199254740993", "malformed_indent": "a:\n   b: 1\n c: 2"} {
		t.Run(name, func(t *testing.T) {
			if body, err := decodeEditProbeOutput(outputWire(text, validJSON), "toon", "read.source"); err == nil {
				t.Fatal("wrong actual wire credited", body)
			}
		})
	}
	duplicate := json.RawMessage(`{"content":[{"type":"text","text":"id: first"},{"type":"text","text":"id: second"}]}`)
	if _, err := decodeEditProbeOutput(duplicate, "toon", "read.source"); err == nil {
		t.Fatal("ambiguous text content accepted")
	}
	failed := json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"id: parseable"}]}`)
	if _, err := decodeEditProbeOutput(failed, "toon", "read.source"); err == nil {
		t.Fatal("tool failure counted as data")
	}
	if _, err := decodeEditProbeOutput(outputWire("id: added", nil), "json", "read.source"); err == nil {
		t.Fatal("TOON body credited to JSON mode")
	}
	if _, err := decodeEditProbeOutput(outputWire("id: added", nil), "gcx", "read.source"); err == nil {
		t.Fatal("unsupported mode accepted")
	}
}

func TestEditProbeOutputModePreservesDefaultPlanAndActualRequestIntent(t *testing.T) {
	c := editProbeCorpus()
	original, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	c.EditOutput = "json"
	explicitJSON, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, explicitJSON) {
		t.Fatal("explicit default JSON changed normalized offered plan")
	}
	c.EditOutput = "toon"
	compact, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	if compact.Mode != "toon" || len(compact.Steps) != 100 {
		t.Fatal(compact)
	}
	for i, step := range compact.Steps {
		baseline := original.Steps[i]
		if step.DueNS != baseline.DueNS || step.ActivateNS != baseline.ActivateNS || step.MutationID != baseline.MutationID || step.Marker != baseline.Marker || step.RequestHash == baseline.RequestHash || editProbeRequestOutput(step.Request) != "toon" {
			t.Fatalf("format altered offered schedule or failed to bind request: %+v", step)
		}
		args := dispatchArgs(step.Request, privateRepo{Prefix: "fixture"})
		if args["output"].(map[string]any)["format"] != "toon" {
			t.Fatal("actual dispatch erased requested output")
		}
		for _, oracle := range editProbeOracles(c, step) {
			if editProbeRequestOutput(oracle.call) != "toon" {
				t.Fatal("probe silently requested JSON", oracle.name)
			}
		}
	}
	if original.Mode != "json" || editProbeRequestOutput(original.Steps[0].Request) != "json" {
		t.Fatal("TOON setup mutated the original JSON plan")
	}
	// Measurement controls remain exact JSON even for a TOON corpus/session.
	control := request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": "commit-1"}}}
	if dispatchArgs(control, privateRepo{Prefix: "fixture", Corpus: c})["output"].(map[string]any)["format"] != "json" {
		t.Fatal("format changed timing-status control")
	}
	for _, unsupported := range []string{"TOON", "text", "gcx", "invalid"} {
		c.EditOutput = unsupported
		if _, err := makeEditProbePlan(c, 42, 100); err == nil {
			t.Fatal("unsupported output mode admitted", unsupported)
		}
	}
}

func TestEditProbeTOONCensusKeepsOmittedCountsUnavailable(t *testing.T) {
	producer := map[string]any{"nodes": []any{map[string]any{"id": "fixture/main.go::Survivor"}}, "edges": nil, "total": 1, "truncated": false}
	raw, err := toon.Marshal(producer)
	if err != nil {
		t.Fatal(err)
	}
	body, err := decodeEditProbeOutput(outputWire(string(raw), nil), "toon", "relations.callers")
	if err != nil {
		t.Fatal(err)
	}
	if rows, ok := editProbeCompleteCount(body, "nodes", "total_nodes"); !ok || len(rows) != 1 {
		t.Fatal("explicit TOON node census not recognized", body)
	}
	if rows, ok := editProbeCompleteCount(body, "edges", "total_edges"); !ok || len(rows) != 0 {
		t.Fatal("explicit null untrimmed edge collection not recognized", body)
	}
	if _, present := body["total_nodes"]; present {
		t.Fatal("fabricated JSON node count")
	}
	if _, present := body["total_edges"]; present {
		t.Fatal("fabricated omitted full edge count")
	}
	for _, name := range []string{"missing_total", "mismatched_total", "truncated", "missing_truncation", "budget_clipped", "missing_edges", "explicit_wrong_edges", "wrong_operation", "forged_JSON_proof"} {
		t.Run(name, func(t *testing.T) {
			candidate, err := decodeEditProbeOutput(outputWire(string(raw), nil), "toon", "relations.callers")
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "missing_total":
				delete(candidate, "total")
			case "mismatched_total":
				candidate["total"] = json.Number("2")
			case "truncated":
				candidate["truncated"] = true
			case "missing_truncation":
				delete(candidate, "truncated")
			case "budget_clipped":
				candidate["_truncated_by_budget"] = true
			case "missing_edges":
				delete(candidate, "edges")
			case "explicit_wrong_edges":
				candidate["total_edges"] = json.Number("1")
			case "wrong_operation":
				candidate[editProbeTOONWireKey] = editProbeTOONWire{operation: "search.symbols"}
			case "forged_JSON_proof":
				candidate[editProbeTOONWireKey] = map[string]any{"operation": "relations.callers"}
			}
			if _, ok := editProbeCompleteCount(candidate, "edges", "total_edges"); ok {
				t.Fatal("incomplete/foreign census accepted", name, candidate)
			}
		})
	}
}

func TestEditProbeTOONSignatureDoesNotBorrowMissingJSONMetadata(t *testing.T) {
	c := editProbeCorpus()
	c.EditOutput, c.EditKind = "toon", "change_signature"
	if _, err := makeEditProbePlan(c, 42, 1); err == nil {
		t.Fatal("missing TOON span/signature proof silently accepted")
	}
}
