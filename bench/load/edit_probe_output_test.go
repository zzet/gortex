package main

import (
	"encoding/json"
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
