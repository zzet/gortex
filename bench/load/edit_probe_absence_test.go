package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func absentSourceWire(message string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": message}}})
	return raw
}

func TestEditProbeExpectedAbsenceMatchesExactProducerRefusal(t *testing.T) {
	const id = "owned/file.go::Old"
	for _, test := range []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{"exact", absentSourceWire("symbol not found: " + id), true},
		{"suggestions", absentSourceWire("symbol not found: " + id + "\nDid you mean another symbol?"), true},
		{"foreign", absentSourceWire("symbol not found: other/file.go::Old"), false},
		{"prefix", absentSourceWire("symbol not found: " + id + "Extra"), false},
		{"freshness", absentSourceWire("repo is still loading"), false},
		{"denied", absentSourceWire("permission denied: " + id), false},
		{"empty", json.RawMessage(`{"isError":false,"content":[]}`), false},
		{"structured", json.RawMessage(`{"isError":true,"structuredContent":{"error":"not found"},"content":[{"type":"text","text":"symbol not found: owned/file.go::Old"}]}`), false},
		{"malformed", json.RawMessage(`{`), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, got := editProbeExactNotFound(test.raw, id)
			if got != test.want {
				t.Fatalf("matched=%v want=%v", got, test.want)
			}
		})
	}
}

func TestEditProbeExpectedAbsenceRequiresPriorPresenceAndPostEditWire(t *testing.T) {
	const logical = "file.go::Old"
	now := time.Now()
	for _, name := range []string{"valid", "no_presence", "before_edit", "no_edit", "no_wire", "wrong_target", "wrong_role", "foreign_refusal"} {
		t.Run(name, func(t *testing.T) {
			state := &editProbeState{absenceBefore: map[string]bool{logical: true}, editWrite: now}
			caller := &editProbeCaller{runtime: &editProbeRuntime{p: &privateRun{Repos: []privateRepo{{Prefix: "owned"}}}}, state: state, role: "probe", request: request{Tool: "read", Operation: "source", ExpectedAbsentSymbol: logical, Arguments: map[string]any{"target": map[string]any{"symbol": logical}}}}
			identity := wireCallIdentity{WriteStarted: now.Add(time.Millisecond), Finished: now.Add(2 * time.Millisecond)}
			raw := absentSourceWire("symbol not found: owned/" + logical)
			switch name {
			case "no_presence":
				state.absenceBefore = nil
			case "before_edit":
				identity.WriteStarted = now.Add(-time.Millisecond)
			case "no_edit":
				state.editWrite = time.Time{}
			case "no_wire":
				identity.WriteStarted = time.Time{}
			case "wrong_target":
				caller.request.Arguments["target"] = map[string]any{"symbol": "other.go::Old"}
			case "wrong_role":
				caller.role = "status"
			case "foreign_refusal":
				raw = absentSourceWire("symbol not found: foreign/" + logical)
			}
			err := caller.expectedAbsence(raw, identity)
			var absent *editProbeExpectedAbsence
			if got := errors.As(err, &absent); got != (name == "valid") {
				t.Fatalf("expected refusal=%v err=%v", got, err)
			}
		})
	}
}

func TestEditProbeExpectedRefusalIsNotAnOrdinarySuccess(t *testing.T) {
	metrics := summarize([]Attempt{{Tool: "read.source", Role: "probe", Outcome: "expected_refusal", ExpectedOutcome: "symbol_absent", LogicalMS: 12}}, 1, "probe")["read.source"]
	if metrics.ExpectedRefusals != 1 || metrics.Successes != 0 || len(metrics.Errors) != 0 {
		t.Fatalf("absence became ordinary success/error: %+v", metrics)
	}
}

func TestEditProbeAbsenceRequiresActualOwnedDeclarationBeforeEdit(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeSymbolCorpus(t, language)
			logical := c.Symbol
			name := logical[strings.LastIndex(logical, "::")+2:]
			source := c.EditAnchor
			for _, variant := range []string{"actual", "comment_only", "wrong_id", "wrong_file", "wrong_name", "empty", "not_found"} {
				t.Run(variant, func(t *testing.T) {
					body := map[string]any{"id": "owned/" + logical, "file_path": "owned/" + c.File, "name": name, "source": source}
					switch variant {
					case "comment_only":
						body["source"] = "// " + source
						if language == "python" {
							body["source"] = "# " + strings.ReplaceAll(source, "\n", "\n# ")
						}
					case "wrong_id":
						body["id"] = "owned/other.go::" + name
					case "wrong_file":
						body["file_path"] = "owned/other.go"
					case "wrong_name":
						body["name"] = "Other"
					case "empty":
						body["source"] = ""
					}
					payload, _ := json.Marshal(body)
					raw, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(payload)}}})
					if variant == "not_found" {
						raw = absentSourceWire("symbol not found: owned/" + logical)
					}
					if got := editProbePresenceProof(raw, c, logical); got != (variant == "actual") {
						t.Fatalf("presence proof=%v", got)
					}
				})
			}
		})
	}
}
