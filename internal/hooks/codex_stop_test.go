package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRunCodexStopClaimCheckUsesFinalMessageAndStopsAfterRetry(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- flush")
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runCodex(data, 0) })
	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode claim-check output %q: %v", out, err)
	}
	if payload.Decision != "block" || !strings.Contains(payload.Reason, "claim_check") {
		t.Fatalf("Codex Stop was not blocked with a claim check: %#v", payload)
	}

	input.StopHookActive = true
	data, _ = json.Marshal(input)
	if retry := captureStdout(t, func() { runCodex(data, 0) }); retry != "" {
		t.Fatalf("recursive Codex Stop should be silent: %q", retry)
	}
}

func TestRunCodexStopClaimCheckReadsPrimaryHeading(t *testing.T) {
	input := claimCheckTestInput(t, "# PRIMARY\nFabricated")
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runCodex(data, 0) })
	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode heading claim-check output %q: %v", out, err)
	}
	if payload.Decision != "block" || !strings.Contains(payload.Reason, "claim_check") {
		t.Fatalf("Codex Stop did not enforce PRIMARY heading claims: %#v", payload)
	}
}

// TestRunCodexStopBriefingMatchesCodexStopSchema pins #779: Codex's Stop
// output schema is closed and has no hookSpecificOutput, so the diagnostics
// briefing must use only its top-level fields.
func TestRunCodexStopBriefingMatchesCodexStopSchema(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["internal/foo.go"],"changed_symbols":[{"id":"internal/foo.go::Foo","name":"Foo","kind":"function"}],"risk":"LOW","summary":"1 symbol"}`,
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","session_id":"codex-stop-779","stop_hook_active":false,"last_assistant_message":""}`)
	out := captureStdout(t, func() { runCodex(data, portFromURL(t, srv.URL)) })
	if out == "" {
		t.Fatal("expected a Stop briefing for a dirty tree")
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(out), &fields); err != nil {
		t.Fatalf("decode Codex Stop output %q: %v", out, err)
	}
	allowed := map[string]bool{
		"continue": true, "decision": true, "reason": true,
		"stopReason": true, "suppressOutput": true, "systemMessage": true,
	}
	for key := range fields {
		if !allowed[key] {
			t.Errorf("Codex Stop output has field %q outside Codex's Stop schema: %s", key, out)
		}
	}
	msg, _ := fields["systemMessage"].(string)
	if !strings.Contains(msg, "Working-Tree Diagnostics") {
		t.Fatalf("briefing missing from systemMessage: %s", out)
	}
	if _, blocked := fields["decision"]; blocked {
		t.Fatalf("diagnostics briefing must not block the stop: %s", out)
	}
}

func TestRunCodexStopFailsOpenWithoutFinalMessage(t *testing.T) {
	input := claimCheckTestInput(t, "")
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() { runCodex(data, 0) }); out != "" {
		t.Fatalf("Codex Stop without last_assistant_message must fail open: %q", out)
	}
}
