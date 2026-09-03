package hooks

import (
	"encoding/json"
	"testing"

	"github.com/zzet/gortex/internal/localizationauth"
)

func TestEmitPreToolUseNormalizesUpdatedInputByHost(t *testing.T) {
	rewrite := map[string]any{"operation": "localize"}
	for _, tt := range []struct {
		name           string
		host           preToolUseHost
		permissionMode string
		decision       string
		updatedInput   map[string]any
		wantDecision   string
		wantUpdated    bool
	}{
		{name: "Claude default asks", host: preToolUseClaude, permissionMode: "default", updatedInput: rewrite, wantDecision: "ask", wantUpdated: true},
		{name: "Claude plan asks", host: preToolUseClaude, permissionMode: "plan", updatedInput: rewrite, wantDecision: "ask", wantUpdated: true},
		{name: "Claude acceptEdits allows", host: preToolUseClaude, permissionMode: "acceptEdits", updatedInput: rewrite, wantDecision: "allow", wantUpdated: true},
		{name: "Claude auto allows", host: preToolUseClaude, permissionMode: "auto", updatedInput: rewrite, wantDecision: "allow", wantUpdated: true},
		{name: "Claude explicit ask", host: preToolUseClaude, decision: "ask", updatedInput: rewrite, wantDecision: "ask", wantUpdated: true},
		{name: "Codex decision-less rewrite allows", host: preToolUseCodex, updatedInput: rewrite, wantDecision: "allow", wantUpdated: true},
		{name: "Codex explicit allow", host: preToolUseCodex, decision: "allow", updatedInput: rewrite, wantDecision: "allow", wantUpdated: true},
		{name: "Codex ask fails closed", host: preToolUseCodex, decision: "ask", updatedInput: rewrite, wantDecision: "deny"},
		{name: "deny discards rewrite", host: preToolUseClaude, decision: "deny", updatedInput: rewrite, wantDecision: "deny"},
		{name: "defer discards rewrite", host: preToolUseClaude, decision: "defer", updatedInput: rewrite, wantDecision: "defer"},
		{name: "Codex allow without rewrite restores prompt", host: preToolUseCodex, decision: "allow"},
		{name: "advisory without rewrite", host: preToolUseClaude},
	} {
		t.Run(tt.name, func(t *testing.T) {
			encoded := captureHookStdout(t, func() {
				emitPreToolUseForHost(tt.host, tt.permissionMode, HookOutput{HookSpecificOutput: &HookSpecificOutput{
					HookEventName:      "PreToolUse",
					PermissionDecision: tt.decision,
					UpdatedInput:       tt.updatedInput,
				}})
			})
			var output HookOutput
			if err := json.Unmarshal([]byte(encoded), &output); err != nil || output.HookSpecificOutput == nil {
				t.Fatalf("invalid PreToolUse output: %v\n%s", err, encoded)
			}
			hso := output.HookSpecificOutput
			if hso.PermissionDecision != tt.wantDecision {
				t.Fatalf("permission decision = %q, want %q", hso.PermissionDecision, tt.wantDecision)
			}
			if got := hso.UpdatedInput != nil; got != tt.wantUpdated {
				t.Fatalf("updatedInput present = %v, want %v: %#v", got, tt.wantUpdated, hso)
			}
		})
	}
}

func TestRunCodexLocalizationAuthRewriteAllowsInput(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
	input := map[string]any{
		"operation": "localize",
		"task":      "literal symptom",
		"options":   map[string]any{"new_user_task": true, "keep": "value"},
	}
	payload := preToolPayload(t, gortexMCPToolPrefix+"explore", "tool-use", identity, input)
	encoded := captureHookStdout(t, func() { runCodex(payload, 0, CodexModeEnrich) })
	var output HookOutput
	if err := json.Unmarshal([]byte(encoded), &output); err != nil || output.HookSpecificOutput == nil {
		t.Fatalf("invalid Codex PreToolUse output: %v\n%s", err, encoded)
	}
	hso := output.HookSpecificOutput
	if hso.PermissionDecision != "allow" {
		t.Fatalf("Codex localization rewrite decision = %q, want allow", hso.PermissionDecision)
	}
	if token, ok := hso.UpdatedInput[localizationauth.ArgumentKey].(string); !ok || token == "" {
		t.Fatalf("Codex localization rewrite lost auth token: %#v", hso.UpdatedInput)
	}
	for key, want := range input {
		if got := hso.UpdatedInput[key]; !equalJSONValue(got, want) {
			t.Fatalf("updatedInput[%q] = %#v, want %#v", key, got, want)
		}
	}
}

func TestRunCodexLocalizationDenyDropsAuthRewrite(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
	previousReachable := daemonReachableFn
	daemonReachableFn = func() bool { return true }
	t.Cleanup(func() { daemonReachableFn = previousReachable })

	payload := preToolPayload(t, gortexMCPToolPrefix+"read", "tool-use", identity, map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": "internal/hooks/pretooluse.go"},
	})
	encoded := captureHookStdout(t, func() { runCodex(payload, 0, CodexModeDeny) })
	var output HookOutput
	if err := json.Unmarshal([]byte(encoded), &output); err != nil || output.HookSpecificOutput == nil {
		t.Fatalf("invalid Codex PreToolUse output: %v\n%s", err, encoded)
	}
	hso := output.HookSpecificOutput
	if hso.PermissionDecision != "deny" {
		t.Fatalf("Codex localization decision = %q, want deny", hso.PermissionDecision)
	}
	if hso.UpdatedInput != nil {
		t.Fatalf("Codex deny carried incompatible updatedInput: %#v", hso.UpdatedInput)
	}
}
