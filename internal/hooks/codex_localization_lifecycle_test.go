package hooks

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/localizationauth"
)

func TestCodexLocalizationLifecycleArmsClaimCheckFromReceipt(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	oldProbe := userPromptProbe
	userPromptProbe = func(string, string, time.Duration) ([]grepSymbolHit, error) { return nil, nil }
	t.Cleanup(func() { userPromptProbe = oldProbe })

	sessionID := "codex-lifecycle"
	promptID := "prompt-1"
	cwd := t.TempDir()
	prompt := codexLocalizationPayload(t, map[string]any{
		"hook_event_name": "UserPromptSubmit", "session_id": sessionID,
		"prompt_id": promptID, "cwd": cwd, "prompt": "Locate the writer implementation",
	})
	_ = captureStdout(t, func() { runCodex(prompt, 0) })

	pre := codexLocalizationPayload(t, map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": gortexMCPToolPrefix + "explore",
		"tool_use_id": "call-1", "session_id": sessionID, "prompt_id": promptID, "cwd": cwd,
		"tool_input": map[string]any{"operation": "localize", "task": "Locate the writer implementation"},
	})
	preOut := captureStdout(t, func() { runCodex(pre, 0) })
	var hookOutput HookOutput
	if err := json.Unmarshal([]byte(preOut), &hookOutput); err != nil {
		t.Fatalf("decode Codex PreToolUse output %q: %v", preOut, err)
	}
	if hookOutput.HookSpecificOutput == nil || hookOutput.HookSpecificOutput.UpdatedInput == nil {
		t.Fatalf("Codex explore did not receive authenticated updated input: %#v", hookOutput)
	}
	token, _ := hookOutput.HookSpecificOutput.UpdatedInput[localizationauth.ArgumentKey].(string)
	if token == "" {
		t.Fatalf("Codex explore updated input omitted terminal auth: %#v", hookOutput.HookSpecificOutput.UpdatedInput)
	}
	identity, ok := currentLocalizationTurn(sessionID, promptID, "", cwd)
	if !ok {
		t.Fatal("Codex UserPromptSubmit did not begin a localization turn")
	}
	if hasLocalizationTerminal(identity) {
		t.Fatal("PreToolUse armed terminal state before an authenticated MCP receipt")
	}

	primary := []string{"repo/a.go::Writer.write"}
	if !localizationauth.Publish(token, localizationauth.Receipt{
		FinalResponse: "Use Writer.write.", PrimaryIDs: primary, EvidenceIDs: primary,
		ContractVersion: localizationTerminalContractV2, Enforceable: true,
	}) {
		t.Fatal("MCP answer_ready receipt was not published")
	}
	post := codexLocalizationPayload(t, map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": gortexMCPToolPrefix + "explore",
		"tool_use_id": "call-1", "session_id": sessionID, "prompt_id": promptID, "cwd": cwd,
		"tool_input":    hookOutput.HookSpecificOutput.UpdatedInput,
		"tool_response": map[string]any{"content": []any{}},
	})
	postOut := captureStdout(t, func() { runCodex(post, 0) })
	if !strings.Contains(postOut, "Localization for this task is complete") {
		t.Fatalf("Codex PostToolUse did not consume the receipt: %q", postOut)
	}
	if !hasLocalizationTerminal(identity) {
		t.Fatal("Codex PostToolUse did not arm terminal state")
	}

	stop := codexLocalizationPayload(t, map[string]any{
		"hook_event_name": "Stop", "session_id": sessionID, "prompt_id": promptID,
		"cwd": cwd, "last_assistant_message": "SYMBOLS:\n- flush",
	})
	stopOut := captureStdout(t, func() { runCodex(stop, 0) })
	var decision HookOutput
	if err := json.Unmarshal([]byte(stopOut), &decision); err != nil {
		t.Fatalf("decode Codex Stop output %q: %v", stopOut, err)
	}
	if decision.Decision != "block" || !strings.Contains(decision.Reason, "claim_check") {
		t.Fatalf("wrong final claim was not blocked from authenticated lifecycle: %#v", decision)
	}
}

func codexLocalizationPayload(t *testing.T, value map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
