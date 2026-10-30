package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
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
			for _, variant := range []string{"actual", "comment_only", "wrong_id", "wrong_file", "wrong_name", "empty", "not_found", "is_error_json"} {
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

func TestEditProbeExpectedAbsenceSurvivesPerformWithoutEvidenceFailure(t *testing.T) {
	const logical = "file.go::Old"
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "symbol not found: owned/" + logical}}}, nil
	})
	client, err := openMCP(t.Context(), socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sink := newEditProbeHeldSink(t)
	sink.unblock()
	state := startHeldEditProbeSpool(t, sink)
	state.absenceBefore = map[string]bool{logical: true}
	state.editWrite = time.Now().Add(-time.Millisecond)
	run := &editProbeRuntime{p: &privateRun{Repos: []privateRepo{{Prefix: "owned"}}}, phase: &Phase{}, origin: time.Now(), options: runOptions{Deadline: time.Second}}
	req := request{Tool: "read", Operation: "source", ExpectedAbsentSymbol: logical, Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": logical}}}
	_, _, attempts, err := run.performObserved(t.Context(), state, client, req, "oracle:old_source_absent", "probe", 0)
	if _, ok := err.(*editProbeExpectedAbsence); !ok {
		t.Fatalf("clean expected refusal wrapped/lost: %T %v", err, err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != "expected_refusal" {
		t.Fatalf("attempts=%+v", attempts)
	}
}

type canceledExpectedAbsenceCaller struct{ cancel context.CancelFunc }

func (c canceledExpectedAbsenceCaller) call(context.Context, string, any) (json.RawMessage, error) {
	c.cancel()
	return absentSourceWire("symbol not found: owned/file.go::Old"), &editProbeExpectedAbsence{symbol: "owned/file.go::Old", message: "symbol not found: owned/file.go::Old"}
}
func TestEditProbeCensoredExpectedAbsenceCannotBeCorrect(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, attempts, err := callEditProbeTool(ctx, canceledExpectedAbsenceCaller{cancel}, request{Tool: "read", Operation: "source"}, nil, "probe", 0, 0, 0, time.Second, waitContext)
	if _, ok := err.(*editProbeExpectedAbsence); ok {
		t.Fatal("censored result retained eligible absence type")
	}
	if !errors.Is(err, context.Canceled) || len(attempts) != 1 || attempts[0].Outcome != "phase_censored" {
		t.Fatalf("attempts=%+v err=%v", attempts, err)
	}
}

func TestEditProbeDisappearanceBeforeAckRetainsActualFirstCorrect(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	const logical = "file.go::Old"
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Params["name"] == "edit" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return stubContent(map[string]any{"mutation_receipt": "committed"}), nil
		}
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "symbol not found: owned/" + logical}}}, nil
	})
	editor, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer editor.Close()
	prober, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer prober.Close()
	sink := newEditProbeHeldSink(t)
	sink.unblock()
	state := startHeldEditProbeSpool(t, sink)
	state.absenceBefore = map[string]bool{logical: true}
	state.started = time.Now()
	state.record.FirstCorrect = map[string]Metric{"old_source_absent": unavailable("ms", "not observed")}
	run := &editProbeRuntime{p: &privateRun{Repos: []privateRepo{{Prefix: "owned"}}}, phase: &Phase{}, origin: state.started, options: runOptions{Deadline: time.Second}}
	done := make(chan error, 1)
	go func() {
		_, _, err := run.perform(ctx, state, editor, request{Tool: "edit", Operation: "symbol", Arguments: map[string]any{"operation": "symbol", "target": map[string]any{"symbol": logical}, "match": "old", "replacement": ""}}, "edit", "edit", 0)
		done <- err
	}()
	defer func() {
		unblock()
		select {
		case <-done:
		case <-ctx.Done():
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("edit was not sent")
	}
	req := request{Tool: "read", Operation: "source", ExpectedAbsentSymbol: logical, Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": logical}}}
	raw, identity, err := run.perform(ctx, state, prober, req, "oracle:old_source_absent", "probe", 1)
	if _, ok := err.(*editProbeExpectedAbsence); !ok {
		t.Fatalf("pre-ACK disappearance lost: %v", err)
	}
	if err := run.observeProbe(state, editProbeOracle{name: "old_source_absent", call: req}, identity.WriteStarted, raw, identity, err, true); err != nil {
		t.Fatal(err)
	}
	if state.record.FirstCorrect["old_source_absent"].Value == nil {
		t.Fatal("actual early disappearance was gated behind ACK")
	}
	select {
	case <-done:
		t.Fatal("ACK was not held")
	default:
	}
}
