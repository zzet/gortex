package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

func copilotPayload(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal copilot payload: %v", err)
	}
	return data
}

// stubPromptProbe feeds the per-turn prompt injection canned hits so the
// prompt lowering can be asserted without a daemon.
func stubPromptProbe(t *testing.T, hits []grepSymbolHit) {
	t.Helper()
	prev := userPromptProbe
	userPromptProbe = func(string, string, time.Duration) ([]grepSymbolHit, error) { return hits, nil }
	t.Cleanup(func() { userPromptProbe = prev })
}

// (d) Every Copilot built-in that maps onto a policy tool must be translated
// before classification. `powershell` is the row that matters most: it is the
// shell tool on Windows, so a missing entry silently unenforces the whole
// Windows shell surface.
func TestCopilotToolNameNormalisation(t *testing.T) {
	cases := map[string]string{
		"view":       "Read",
		"grep":       "Grep",
		"rg":         "Grep",
		"glob":       "Glob",
		"bash":       "Bash",
		"powershell": "Bash",
		"create":     "Write",
		"edit":       "Edit",
		"task":       "Task",
		// Unmapped Copilot built-ins pass through and fall out of enrich's
		// switch as a no-op rather than being misclassified.
		"web_fetch":   "web_fetch",
		"ask_user":    "ask_user",
		"update_todo": "update_todo",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			if got := copilotToolName(input); got != want {
				t.Fatalf("copilotToolName(%q)=%q want %q", input, got, want)
			}
		})
	}
}

// (d, end to end) A PowerShell command that reads indexed source must reach
// the same deny a Bash command does — proof the table entry is actually wired
// into the classification path, not just present in the map.
func TestHandleCopilotPowerShellReadOfIndexedSourceDenies(t *testing.T) {
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 7)

	out, code := handleCopilot(copilotPayload(t, map[string]any{
		"hookEventName": "preToolUse",
		"cwd":           "/repo",
		"toolName":      "powershell",
		"toolArgs":      map[string]any{"command": "cat internal/auth/token.go"},
	}), 0, ModeDeny)

	if code != 0 {
		t.Fatalf("exit code=%d want 0", code)
	}
	decision := decodeCopilotPreToolUse(t, out)
	if decision.PermissionDecision != "deny" {
		t.Fatalf("powershell read of indexed source was not denied: %s", out)
	}
	if !strings.Contains(decision.PermissionDecisionReason, "reads indexed source") {
		t.Fatalf("deny reason lost the shell-read redirect: %q", decision.PermissionDecisionReason)
	}
}

// (b) A deny must arrive in Copilot's flat per-event shape and name the
// Gortex tool that replaces the refused call.
func TestHandleCopilotPreToolUseDeniesIndexedRead(t *testing.T) {
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 7)

	// `view` carries its target as `path`; the Claude leaves switch on
	// `file_path`, so the alias fill-in is exercised here too.
	out, code := handleCopilot(copilotPayload(t, map[string]any{
		"hookEventName": "preToolUse",
		"sessionId":     "sess-copilot",
		"cwd":           "/repo",
		"toolName":      "view",
		"toolArgs":      map[string]any{"path": "internal/auth/token.go"},
	}), 0, ModeDeny)

	if code != 0 {
		t.Fatalf("exit code=%d want 0", code)
	}
	if strings.Contains(out, "hookSpecificOutput") {
		t.Fatalf("Copilot answers flat, never in Claude's envelope: %s", out)
	}
	decision := decodeCopilotPreToolUse(t, out)
	if decision.PermissionDecision != "deny" {
		t.Fatalf("indexed Read was not denied: %s", out)
	}
	for _, want := range []string{"7 symbols indexed", `read(target:{symbol:`} {
		if !strings.Contains(decision.PermissionDecisionReason, want) {
			t.Fatalf("deny reason missing %q: %q", want, decision.PermissionDecisionReason)
		}
	}
}

// Copilot has accepted PascalCase event names alongside camelCase since
// v1.0.6, so a real config carries either spelling.
func TestHandleCopilotAcceptsPascalCaseEventNames(t *testing.T) {
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 3)

	out, _ := handleCopilot(copilotPayload(t, map[string]any{
		"hookEventName": "PreToolUse",
		"cwd":           "/repo",
		"toolName":      "view",
		"toolArgs":      map[string]any{"path": "internal/auth/token.go"},
	}), 0, ModeDeny)

	if decodeCopilotPreToolUse(t, out).PermissionDecision != "deny" {
		t.Fatalf("PascalCase event name was not recognised: %s", out)
	}
}

// (a) The single most likely bug in this adapter: Copilot's
// userPromptSubmitted has NO additionalContext channel, so reusing Claude's
// lowering emits valid JSON and discards every byte of prompt-time context.
func TestHandleCopilotUserPromptSubmittedUsesModifiedPrompt(t *testing.T) {
	withDaemonReachable(t, true)
	stubPromptProbe(t, []grepSymbolHit{
		{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42},
	})

	const prompt = "fix the token validation bug"
	out, code := handleCopilot(copilotPayload(t, map[string]any{
		"hookEventName": "userPromptSubmitted",
		"cwd":           "/repo",
		"prompt":        prompt,
	}), 0, ModeDeny)

	if code != 0 {
		t.Fatalf("exit code=%d want 0", code)
	}
	if strings.Contains(out, "additionalContext") {
		t.Fatalf("userPromptSubmitted has no additionalContext channel: %s", out)
	}
	var decoded copilotPromptResponse
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("invalid userPromptSubmitted response %q: %v", out, err)
	}
	if !strings.HasPrefix(decoded.ModifiedPrompt, prompt) {
		t.Fatalf("modifiedPrompt dropped the user's own text: %q", decoded.ModifiedPrompt)
	}
	for _, want := range []string{"relevant indexed symbols", "ValidateToken"} {
		if !strings.Contains(decoded.ModifiedPrompt, want) {
			t.Fatalf("modifiedPrompt missing %q: %q", want, decoded.ModifiedPrompt)
		}
	}
}

func TestHandleCopilotSessionStartEmitsAdditionalContext(t *testing.T) {
	withDaemonReachable(t, true)
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "1.0.0", Ready: true,
			TrackedRepos: []daemon.TrackedRepoStatus{{Name: "repo", Path: "/tmp/repo", Workspace: "repo", Nodes: 10}},
			Workspaces:   []daemon.WorkspaceSummary{{Slug: "repo"}},
		}, nil
	})

	out, code := handleCopilot(copilotPayload(t, map[string]any{
		"hookEventName": "sessionStart",
		"cwd":           "/tmp/repo",
	}), 0, ModeDeny)

	if code != 0 {
		t.Fatalf("exit code=%d want 0", code)
	}
	var decoded copilotSessionStartResponse
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("invalid sessionStart response %q: %v", out, err)
	}
	if !strings.Contains(decoded.AdditionalContext, "Gortex Session Orientation") {
		t.Fatalf("sessionStart lost the orientation briefing: %q", decoded.AdditionalContext)
	}
}

func TestHandleCopilotPostToolUseEnrichesGrepOutput(t *testing.T) {
	withDaemonReachable(t, true)
	oldSummary := fileSummaryFn
	fileSummaryFn = func(_, _ string) (*hookFileSummary, bool) {
		return &hookFileSummary{Symbols: []summaryNode{
			{Name: "ValidateToken", Kind: "function", StartLine: 1, EndLine: 10},
		}}, true
	}
	t.Cleanup(func() { fileSummaryFn = oldSummary })

	out, code := handleCopilot(copilotPayload(t, map[string]any{
		"hookEventName": "postToolUse",
		"cwd":           "/repo",
		"toolName":      "rg",
		"toolArgs":      map[string]any{"pattern": "ValidateToken"},
		"toolResult":    "internal/auth/token.go:3:func ValidateToken()",
	}), 0, ModeDeny)

	if code != 0 {
		t.Fatalf("exit code=%d want 0", code)
	}
	var decoded copilotPostToolUseResponse
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("invalid postToolUse response %q: %v", out, err)
	}
	if !strings.Contains(decoded.AdditionalContext, "ValidateToken") {
		t.Fatalf("postToolUse enrichment missing graph follow-up: %q", decoded.AdditionalContext)
	}
	if decoded.ModifiedResult != nil {
		t.Fatalf("Gortex must never replace a Copilot tool result: %s", out)
	}
}

// (f) Nine of Copilot's fourteen events map to nothing Gortex emits, and
// agentStop has no context channel at all — all of them answer empty at 0.
func TestHandleCopilotUnhandledEventsAreSilent(t *testing.T) {
	withDaemonReachable(t, true)
	for _, event := range []string{
		"notification", "preCompact", "errorOccurred", "permissionRequest",
		"subagentStart", "subagentStop", "postToolUseFailure",
		"userPromptTransformed", "sessionEnd",
		// agentStop is implemented but deliberately silent: its only
		// documented fields are decision/reason, so a post-task briefing
		// has nowhere to land.
		"agentStop",
	} {
		t.Run(event, func(t *testing.T) {
			out, code := handleCopilot(copilotPayload(t, map[string]any{
				"hookEventName": event,
				"cwd":           "/repo",
			}), 0, ModeDeny)
			if out != "" || code != 0 {
				t.Fatalf("event %q must be a silent no-op, got out=%q code=%d", event, out, code)
			}
		})
	}
}

// (c) Malformed stdin fails open: no output, exit 0, no panic.
func TestHandleCopilotMalformedInputFailsOpen(t *testing.T) {
	withDaemonReachable(t, true)
	for _, payload := range []string{`{not json`, ``, `[]`, `{"hookEventName":42}`} {
		out, code := handleCopilot([]byte(payload), 0, ModeDeny)
		if out != "" || code != 0 {
			t.Fatalf("payload %q must fail open, got out=%q code=%d", payload, out, code)
		}
	}
}

// (e) During a daemon outage the per-call posture stands down: every deny it
// could produce mandates graph tools the daemon cannot serve (#486).
func TestHandleCopilotStandsDownWhenDaemonUnreachable(t *testing.T) {
	withDaemonReachable(t, false)
	stubIndexedFile(t, true, 7)

	for _, event := range []string{"preToolUse", "postToolUse", "userPromptSubmitted"} {
		t.Run(event, func(t *testing.T) {
			out, code := handleCopilot(copilotPayload(t, map[string]any{
				"hookEventName": event,
				"cwd":           "/repo",
				"prompt":        "fix the token validation bug",
				"toolName":      "view",
				"toolArgs":      map[string]any{"path": "internal/auth/token.go"},
				"toolResult":    "internal/auth/token.go:3:func ValidateToken()",
			}), 0, ModeDeny)
			if out != "" || code != 0 {
				t.Fatalf("%s must stand down during an outage, got out=%q code=%d", event, out, code)
			}
		})
	}
}

// (g) Copilot's camelCase event names are not in hookEffectivenessEvents, so
// an unnormalised name is dropped by the writer and `gortex doctor` reports
// "configured but never ran" against a working install forever.
func TestCopilotTelemetryUsesTheClaudeEventVocabulary(t *testing.T) {
	for copilotEvent, claudeEvent := range copilotTelemetryEvents {
		if hookEffectivenessEvents[copilotEvent] {
			t.Fatalf("the allowlist must not gain camelCase duplicates: %q", copilotEvent)
		}
		if !hookEffectivenessEvents[claudeEvent] {
			t.Fatalf("%q normalises to %q, which the allowlist drops", copilotEvent, claudeEvent)
		}
	}

	path := filepath.Join(t.TempDir(), "effectiveness.jsonl")
	t.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", path)
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 7)

	if _, code := handleCopilot(copilotPayload(t, map[string]any{
		"hookEventName": "preToolUse",
		"cwd":           "/repo",
		"toolName":      "view",
		"toolArgs":      map[string]any{"path": "internal/auth/token.go"},
	}), 0, ModeDeny); code != 0 {
		t.Fatalf("exit code=%d want 0", code)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a Copilot preToolUse must leave a hook-effectiveness record: %v", err)
	}
	var record hookEffectiveness
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &record); err != nil {
		t.Fatalf("invalid effectiveness JSONL %q: %v", data, err)
	}
	if !hookEffectivenessEvents[record.Event] {
		t.Fatalf("logged event %q is dropped by the allowlist", record.Event)
	}
	if record.Event != "PreToolUse" || !record.EmittedContext {
		t.Fatalf("record=%+v", record)
	}
}

// RunCopilot is the process entry point: stdin in, one flat response out,
// exit 0.
func TestRunCopilotReadsStdinAndWritesFlatResponse(t *testing.T) {
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 7)

	payload := copilotPayload(t, map[string]any{
		"hookEventName": "preToolUse",
		"cwd":           "/repo",
		"toolName":      "view",
		"toolArgs":      map[string]any{"path": "internal/auth/token.go"},
	})
	out := captureStdout(t, func() {
		withStdin(t, payload, func() { RunCopilot(0, ModeDeny) })
	})
	if decodeCopilotPreToolUse(t, out).PermissionDecision != "deny" {
		t.Fatalf("RunCopilot did not emit the deny: %q", out)
	}
}

func decodeCopilotPreToolUse(t *testing.T, out string) copilotPreToolUseResponse {
	t.Helper()
	var decoded copilotPreToolUseResponse
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("invalid preToolUse response %q: %v", out, err)
	}
	return decoded
}
