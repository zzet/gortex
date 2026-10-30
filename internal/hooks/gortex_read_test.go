package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/toolref"
)

// withForceCompress flips GORTEX_HOOK_FORCE_COMPRESS for one test and
// restores the previous value (including unset) on cleanup.
func withForceCompress(t *testing.T, on bool) {
	t.Helper()
	t.Setenv(gortexForceCompressEnvVar, map[bool]string{true: "1", false: ""}[on])
}

func TestGortexReadNudge_NudgesFullSourceRead(t *testing.T) {
	msg := gortexReadNudge(gortexReadFileTool, map[string]any{"path": "internal/resolver/resolver.go"})
	if msg == "" {
		t.Fatal("expected a nudge for a full-body source read")
	}
	for _, want := range []string{"compress_bodies", `search(operation:"text", query:`, "keep", "read_file"} {
		if !strings.Contains(msg, want) {
			t.Errorf("nudge missing %q:\n%s", want, msg)
		}
	}
}

func TestGortexReadNudge_CompactReadShape(t *testing.T) {
	input := map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": "internal/resolver/resolver.go"},
	}
	msg := gortexReadNudge(gortexCompactReadTool, input)
	for _, want := range []string{"compress_bodies", `search(operation:"text", query:`, `read(target:{file:`, toolref.TransportClause(), toolref.DegradedModeLine()} {
		if !strings.Contains(msg, want) {
			t.Errorf("compact read nudge missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "gortex call ") {
		t.Errorf("compact read nudge advertised CLI fallback:\n%s", msg)
	}
	for _, legacy := range []string{"search_text", "read_file", "get_editing_context"} {
		if strings.Contains(msg, legacy) {
			t.Errorf("compact read nudge leaked legacy tool %q:\n%s", legacy, msg)
		}
	}
}

func TestGortexReadNudge_CompactNarrowAndBoundedShapesAreSilent(t *testing.T) {
	tests := []map[string]any{
		{"operation": "source", "target": map[string]any{"symbol": "a.go::A"}},
		{"operation": "summary", "target": map[string]any{"file": "a.go"}},
		{"operation": "file", "target": map[string]any{"file": "a.go"}, "options": map[string]any{"compress_bodies": true}},
		{"operation": "editing_context", "target": map[string]any{"file": "a.go"}, "context": map[string]any{"max_tokens": 500}},
		{"operation": "file", "target": map[string]any{"file": "a.go"}, "output": map[string]any{"max_bytes": 4000}},
	}
	for _, input := range tests {
		if msg := gortexReadNudge(gortexCompactReadTool, input); msg != "" {
			t.Errorf("economical compact read emitted nudge:\n%s\ninput=%#v", msg, input)
		}
	}
}

func TestGortexReadNudge_SilentWhenAlreadyEconomical(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
	}{
		{"compress_bodies true", map[string]any{"path": "a.go", "compress_bodies": true}},
		{"compress_bodies string true", map[string]any{"path": "a.go", "compress_bodies": "true"}},
		{"non-source file", map[string]any{"path": "README.md"}},
		{"config file", map[string]any{"path": "config.yaml"}},
		{"capped by max_lines", map[string]any{"path": "a.go", "max_lines": float64(80)}},
		{"capped by max_bytes", map[string]any{"path": "a.go", "max_bytes": float64(4000)}},
		{"capped by max_tokens", map[string]any{"path": "a.go", "max_tokens": float64(2000)}},
		{"no path", map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if msg := gortexReadNudge(gortexReadFileTool, c.input); msg != "" {
				t.Errorf("expected silence, got nudge:\n%s", msg)
			}
		})
	}
}

func TestGortexReadNudge_ZeroCapStillNudges(t *testing.T) {
	// An explicit max_tokens:0 is an opt-out of the cap, not a cap — the
	// read is still unbounded, so the nudge should fire.
	if msg := gortexReadNudge(gortexReadFileTool, map[string]any{"path": "a.go", "max_tokens": float64(0)}); msg == "" {
		t.Error("max_tokens:0 is not a cap; expected the nudge to fire")
	}
}

func TestEnrichGortexRead_DefaultIsSoftContext(t *testing.T) {
	withForceCompress(t, false)
	result := enrichGortexRead(gortexReadFileTool, map[string]any{"path": "a.go"})
	if result.deny {
		t.Fatal("default posture must not deny")
	}
	if result.context == "" {
		t.Error("expected soft advisory context")
	}
}

func TestEnrichGortexRead_GateOnDenies(t *testing.T) {
	withForceCompress(t, true)
	result := enrichGortexRead(gortexEditingContextTool, map[string]any{"path": "a.go"})
	if !result.deny {
		t.Fatal("GORTEX_HOOK_FORCE_COMPRESS=1 must deny a full-body read")
	}
	if !strings.Contains(result.reason, "compress_bodies") {
		t.Errorf("deny reason should name compress_bodies:\n%s", result.reason)
	}
}

func TestEnrichGortexRead_EconomicalCallPassesThroughEvenWhenGated(t *testing.T) {
	withForceCompress(t, true)
	result := enrichGortexRead(gortexReadFileTool, map[string]any{"path": "a.go", "compress_bodies": true})
	if result.deny || result.context != "" {
		t.Errorf("already-compressed read must pass silently even with the gate on; got deny=%v ctx=%q", result.deny, result.context)
	}
}

func TestEnrich_DispatchesGortexReadTools(t *testing.T) {
	withForceCompress(t, false)
	tests := []struct {
		tool  string
		input map[string]any
	}{
		{gortexReadFileTool, map[string]any{"path": "a.go"}},
		{gortexEditingContextTool, map[string]any{"path": "a.go"}},
		{gortexCompactReadTool, map[string]any{"operation": "file", "target": map[string]any{"file": "a.go"}}},
	}
	for _, tt := range tests {
		input := HookInput{ToolName: tt.tool, ToolInput: tt.input}
		if result := enrich(input, 0); result.context == "" {
			t.Errorf("enrich must route %s to enrichGortexRead", tt.tool)
		}
	}
}

func TestRunPreToolUse_CompactRead_EmitsAdditionalContext(t *testing.T) {
	withForceCompress(t, false)
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__read","tool_input":{"operation":"file","target":{"file":"internal/x.go"}}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })

	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if dec.HookSpecificOutput == nil || !strings.Contains(dec.HookSpecificOutput.AdditionalContext, "compress_bodies") {
		t.Fatalf("expected compact read guidance, got: %s", out)
	}
}

func TestRunPreToolUse_GortexRead_EmitsAdditionalContext(t *testing.T) {
	withForceCompress(t, false)
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__read_file","tool_input":{"path":"internal/x.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })

	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if dec.HookSpecificOutput.PermissionDecision == "deny" {
		t.Error("default posture must not hard-deny a Gortex read")
	}
	if !strings.Contains(dec.HookSpecificOutput.AdditionalContext, "compress_bodies") {
		t.Errorf("expected compress_bodies advice in additionalContext:\n%s", out)
	}
}

func TestRunPreToolUse_GortexRead_CompressedIsSilent(t *testing.T) {
	withForceCompress(t, false)
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__read_file","tool_input":{"path":"internal/x.go","compress_bodies":true}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if strings.TrimSpace(out) != "" {
		t.Errorf("a compressed read should emit nothing, got:\n%s", out)
	}
}

func TestRunPreToolUse_GortexRead_PermissiveCarriesNudge(t *testing.T) {
	withForceCompress(t, false)
	// Under a permissive permission mode the call is auto-approved, but
	// the read-discipline nudge must still ride along as soft context.
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__read_file","tool_input":{"path":"internal/x.go"},"permission_mode":"acceptEdits"}`)
	out := captureStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })

	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if dec.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("permissive mode must keep the allow decision, got %q", dec.HookSpecificOutput.PermissionDecision)
	}
	if !strings.Contains(dec.HookSpecificOutput.AdditionalContext, "compress_bodies") {
		t.Errorf("permissive auto-approve should still carry the nudge:\n%s", out)
	}
}

func TestRunPreToolUse_GortexRead_PermissiveNeverDeniesEvenWhenGated(t *testing.T) {
	withForceCompress(t, true)
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__read_file","tool_input":{"path":"internal/x.go"},"permission_mode":"acceptEdits"}`)
	out := captureStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })

	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if dec.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("auto-approve must hold even with the gate on, got %q", dec.HookSpecificOutput.PermissionDecision)
	}
}

func TestAsBool(t *testing.T) {
	truthy := []any{true, "true", "TRUE", "1", "yes", "on"}
	for _, v := range truthy {
		if !asBool(v) {
			t.Errorf("asBool(%#v) = false, want true", v)
		}
	}
	falsy := []any{false, "false", "0", "no", "off", "", nil, 1, "maybe"}
	for _, v := range falsy {
		if asBool(v) {
			t.Errorf("asBool(%#v) = true, want false", v)
		}
	}
}
