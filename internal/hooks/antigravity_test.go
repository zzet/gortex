package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/localizationauth"
	"github.com/zzet/gortex/internal/modelhint"
)

// agyPayload builds an agy hook payload for conversation conv in /repo.
func agyPayload(t *testing.T, conv string, fields map[string]any) []byte {
	t.Helper()
	merged := map[string]any{"conversationId": conv, "workspacePaths": []string{"/repo"}}
	for k, v := range fields {
		merged[k] = v
	}
	return copilotPayload(t, merged)
}

func agyGortexCall(tool string, args map[string]any) map[string]any {
	return map[string]any{
		"name": "call_mcp_tool",
		"args": map[string]any{"ServerName": "gortex", "ToolName": tool, "Arguments": args},
	}
}

func agyNativeRead(path string) map[string]any {
	return map[string]any{"name": "read_file", "args": map[string]any{"Path": path}}
}

func agyDecision(t *testing.T, out string) antigravityDecisionResponse {
	t.Helper()
	var dec antigravityDecisionResponse
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("unmarshal decision: %v\n%s", err, out)
	}
	return dec
}

func withAgyFixtureRepo(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "1.0.0", Ready: true,
			TrackedRepos: []daemon.TrackedRepoStatus{{Name: "repo", Path: repoTmpFixtureRoot, Workspace: "repo", Nodes: 10}},
			Workspaces:   []daemon.WorkspaceSummary{{Slug: "repo"}},
		}, nil
	})
}

func TestUnwrapAntigravityTool(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		wantRaw    string
		wantMapped string
		wantArgs   map[string]any
	}{
		{"lazy gortex call", "call_mcp_tool",
			map[string]any{"ServerName": "gortex", "ToolName": "explore", "Arguments": map[string]any{"task": "trace auth"}},
			"mcp__gortex__explore", "mcp__gortex__explore", map[string]any{"task": "trace auth"}},
		{"eager gortex tool", "mcp_gortex_search", map[string]any{"query": "foo"},
			"mcp__gortex__search", "mcp__gortex__search", map[string]any{"query": "foo"}},
		{"other MCP server, JSON-string Arguments", "call_mcp_tool",
			map[string]any{"ServerName": "sqlite", "ToolName": "query", "Arguments": `{"sql":"SELECT 1"}`},
			"mcp__sqlite__query", "mcp__sqlite__query", map[string]any{"sql": "SELECT 1"}},
		{"read_file", "read_file", map[string]any{"Path": "internal/auth/token.go", "StartLine": 10, "EndLine": 40},
			"read_file", "Read", map[string]any{"file_path": "internal/auth/token.go", "offset": 10, "limit": 40}},
		{"write", "write_to_file", map[string]any{"TargetFile": "pkg/b.go"},
			"write_to_file", "Write", map[string]any{"file_path": "pkg/b.go"}},
		{"grep_search", "grep_search", map[string]any{"Query": "MyClass", "SearchPath": "src"},
			"grep_search", "Grep", map[string]any{"pattern": "MyClass", "path": "src"}},
		{"find_by_name", "find_by_name", map[string]any{"Pattern": "*.go", "SearchDirectory": "cmd"},
			"find_by_name", "Glob", map[string]any{"pattern": "*.go", "path": "cmd"}},
		{"run_command", "run_command", map[string]any{"CommandLine": "go test ./..."},
			"run_command", "Bash", map[string]any{"command": "go test ./..."}},
		{"JSON-quoted argument", "run_command", map[string]any{"CommandLine": `"grep \"foo\" pkg/a.go"`},
			"run_command", "Bash", map[string]any{"command": `grep "foo" pkg/a.go`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, mapped, args := unwrapAntigravityTool(tc.tool, tc.args)
			if raw != tc.wantRaw || mapped != tc.wantMapped {
				t.Fatalf("unwrap(%s) = (%q, %q), want (%q, %q)", tc.tool, raw, mapped, tc.wantRaw, tc.wantMapped)
			}
			for k, v := range tc.wantArgs {
				if args[k] != v {
					t.Errorf("args[%q] = %v, want %v (args %v)", k, args[k], v, args)
				}
			}
		})
	}

	// Rewritten call_mcp_tool arguments go back under the host's own key.
	overwrite := antigravityWrapOverwrite("call_mcp_tool", map[string]any{"arguments": map[string]any{}}, map[string]any{"task": "new"})
	if _, ok := overwrite["arguments"]; !ok {
		t.Errorf("overwrite lost the lowercase arguments key: %v", overwrite)
	}
	// Unparseable Arguments unwrap to an empty map rather than failing.
	if _, _, args := unwrapAntigravityTool("call_mcp_tool", map[string]any{"ServerName": "gortex", "ToolName": "explore", "Arguments": "not-json"}); len(args) != 0 {
		t.Errorf("unparseable Arguments = %v, want empty", args)
	}
}

// agy, Gemini CLI and Claude-style payloads spell the same fields differently.
func TestAntigravityInputWireAliases(t *testing.T) {
	var in antigravityInput
	raw := `{"workspacePaths":[" ","/ws"],"cwd":"/other","model":"gemini-pro","tool_use_id":"call-123","final_model_output":"done"}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	if in.cwd() != "/ws" || in.model() != "gemini-pro" || in.toolUseID() != "call-123" || in.finalModelOutput() != "done" {
		t.Errorf("aliases = cwd %q model %q toolUseID %q final %q", in.cwd(), in.model(), in.toolUseID(), in.finalModelOutput())
	}
	if got := (antigravityInput{}).finalModelOutput(); got != "" {
		t.Errorf("empty finalModelOutput = %q", got)
	}
}

func TestNormalizeAntigravityEvents(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		in   antigravityInput
		want string
	}{
		{"sessionstart", antigravityInput{}, "SessionStart"},
		{"preinvocation", antigravityInput{}, "PreInvocation"},
		{"userpromptsubmit", antigravityInput{}, "PreInvocation"},
		{"pretooluse", antigravityInput{}, "PreToolUse"},
		{"posttooluse", antigravityInput{}, "PostToolUse"},
		{"aftertool", antigravityInput{}, "AfterTool"},
		{"stop", antigravityInput{}, "Stop"},
		// No event name: inferred from the payload shape.
		{"", antigravityInput{ToolName: "read_file", Result: json.RawMessage(`{"bytes":10}`)}, "PostToolUse"},
		{"", antigravityInput{ToolName: "read_file"}, "PreToolUse"},
		{"", antigravityInput{TerminationReason: "COMPLETED"}, "Stop"},
		{"", antigravityInput{Prompt: "hello"}, "PreInvocation"},
		{"", antigravityInput{}, "SessionStart"},
	} {
		if got := normalizeAntigravityEvent(tc.raw, tc.in); got != tc.want {
			t.Errorf("normalizeAntigravityEvent(%q, %+v) = %q, want %q", tc.raw, tc.in, got, tc.want)
		}
	}
}

func TestHandleAntigravitySessionStartInjectsSystemMessageAndWritesModelHint(t *testing.T) {
	t.Setenv("GORTEX_MODEL_HINT_DIR", t.TempDir())
	withAgyFixtureRepo(t)

	payload := copilotPayload(t, map[string]any{
		"conversationId": "conv-123",
		"workspacePaths": []string{repoTmpFixtureRoot},
		"modelName":      "gemini-3.1-pro",
	})
	out, _ := handleAntigravity(payload, 0, ModeDeny, "SessionStart")
	var resp antigravityInjectResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal SessionStart response: %v\n%s", err, out)
	}
	if len(resp.InjectSteps) != 1 || resp.InjectSteps[0].SystemMessage == nil ||
		!strings.Contains(resp.InjectSteps[0].SystemMessage.SystemMessage, "Gortex Session Orientation") {
		t.Fatalf("expected one orientation systemMessage step, got %s", out)
	}
	if hint, ok := modelhint.Read(repoTmpFixtureRoot); !ok || hint.Model != "gemini-3.1-pro" || hint.Client != "antigravity" {
		t.Errorf("model hint = %+v (ok=%v)", hint, ok)
	}
}

func TestHandleAntigravityPreInvocationInjectsEphemeralMessageFromTranscript(t *testing.T) {
	withDaemonReachable(t, true)
	stubPromptProbe(t, []grepSymbolHit{{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42}})

	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	line := `{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>\nfix ValidateToken in token.go\n</USER_REQUEST>"}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := handleAntigravity(agyPayload(t, "conv-456", map[string]any{"transcriptPath": transcript}), 0, ModeDeny, "PreInvocation")
	var resp antigravityInjectResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal PreInvocation response: %v\n%s", err, out)
	}
	if len(resp.InjectSteps) != 1 || !strings.Contains(resp.InjectSteps[0].EphemeralMessage, "ValidateToken") {
		t.Fatalf("expected an ephemeralMessage naming ValidateToken, got %s", out)
	}
}

// agy fires PreInvocation before every model invocation; only invocationNum 0
// (a new user prompt) opens a turn and injects context.
func TestHandleAntigravityPreInvocationOnlyStartsTurnOnNewPrompt(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	withDaemonReachable(t, true)
	stubPromptProbe(t, []grepSymbolHit{{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42}})

	preInv := func(invocation int) string {
		out, _ := handleAntigravity(agyPayload(t, "conv-steps", map[string]any{
			"lastUserInput": "fix ValidateToken in token.go",
			"invocationNum": invocation,
		}), 0, ModeDeny, "PreInvocation")
		return out
	}

	if out := preInv(0); !strings.Contains(out, "ValidateToken") {
		t.Fatalf("first invocation of a prompt should inject context, got %s", out)
	}
	turn, ready := currentLocalizationTurnState("conv-steps", "", "", "/repo")
	if !ready {
		t.Fatal("expected a current turn after the first invocation")
	}
	if out := preInv(1); out != "{}" {
		t.Fatalf("continuation invocation should stay silent, got %s", out)
	}
	if same, ok := currentLocalizationTurnState("conv-steps", "", "", "/repo"); !ok || same.Identity != turn.Identity {
		t.Fatalf("continuation must keep the turn: before %+v, after %+v", turn.Identity, same.Identity)
	}
	if out := preInv(0); !strings.Contains(out, "ValidateToken") {
		t.Fatalf("a new prompt should inject context again, got %s", out)
	}
	if next, ok := currentLocalizationTurnState("conv-steps", "", "", "/repo"); !ok || next.Identity == turn.Identity {
		t.Fatalf("a new prompt must open a new turn, still %+v", next.Identity)
	}
}

func TestHandleAntigravityPreToolUseDeniesWholeFileReadAndUnlocksAfterCallMCPTool(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 9)

	read := agyPayload(t, "conv-unlock", map[string]any{"toolCall": agyNativeRead("internal/auth/token.go")})
	if dec := agyDecision(t, mustAgy(t, read, ModeConsultUnlock)); dec.Decision != "deny" || !strings.Contains(dec.Reason, "9 symbols indexed") {
		t.Fatalf("expected deny before Gortex consultation, got %+v", dec)
	}
	gortex := agyPayload(t, "conv-unlock", map[string]any{"toolCall": agyGortexCall("explore", map[string]any{"task": "check token"})})
	if out := mustAgy(t, gortex, ModeConsultUnlock); out != `{"decision":"allow"}` {
		t.Fatalf("call_mcp_tool explore = %s, want allow", out)
	}
	if out := mustAgy(t, read, ModeConsultUnlock); out != `{"decision":"allow"}` {
		t.Fatalf("read after Gortex consultation = %s, want allow", out)
	}
}

// mustAgy runs one PreToolUse payload through handleAntigravity.
func mustAgy(t *testing.T, payload []byte, mode Mode) string {
	t.Helper()
	out, code := handleAntigravity(payload, 0, mode, "PreToolUse")
	if code != 0 {
		t.Fatalf("PreToolUse exit code = %d", code)
	}
	return out
}

func TestHandleAntigravityLocalizationTerminalLifecycle(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	withDaemonReachable(t, true)
	stubPromptProbe(t, nil)
	const conv = "conv-term"

	handleAntigravity(agyPayload(t, conv, map[string]any{
		"lastUserInput": "<USER_REQUEST>\n--- problem statement ---\nwhere is ValidateToken defined in the auth package?\n-------------------------\n</USER_REQUEST>",
		"invocationNum": 0,
	}), 0, ModeDeny, "PreInvocation")

	// The localize call is allowed with its arguments rewritten in place:
	// auth token added, task replaced by the turn's problem statement.
	localize := agyGortexCall("explore", map[string]any{
		"operation": "localize",
		"task":      "short query",
		"options":   map[string]any{"new_user_task": true},
	})
	pre := agyDecision(t, mustAgy(t, agyPayload(t, conv, map[string]any{"stepIdx": 3, "toolCall": localize}), ModeDeny))
	rewritten, _ := pre.Overwrite["Arguments"].(map[string]any)
	authToken, _ := rewritten[localizationauth.ArgumentKey].(string)
	if pre.Decision != "allow" || authToken == "" || rewritten["task"] != "where is ValidateToken defined in the auth package?" {
		t.Fatalf("expected allow with auth token and rewritten task, got %+v", pre)
	}

	if !localizationauth.Publish(authToken, localizationauth.Receipt{
		FinalResponse:   "ValidateToken is defined in internal/auth/token.go:42.",
		ContractVersion: 2,
		Enforceable:     true,
		PrimaryIDs:      []string{"internal/auth/token.go::ValidateToken#function"},
		EvidenceIDs:     []string{"internal/auth/token.go::ValidateToken#function"},
	}) {
		t.Fatal("failed to publish localizationauth receipt")
	}
	if out, _ := handleAntigravity(agyPayload(t, conv, map[string]any{
		"stepIdx": 3, "toolCall": localize, "result": `{"terminal":true}`,
	}), 0, ModeDeny, "PostToolUse"); out != "{}" {
		t.Fatalf("PostToolUse = %s, want {}", out)
	}

	// answer_ready: further navigation in the turn is denied with the answer.
	follow := agyDecision(t, mustAgy(t, agyPayload(t, conv, map[string]any{"stepIdx": 4, "toolCall": agyNativeRead("internal/auth/token.go")}), ModeDeny))
	if follow.Decision != "deny" || !strings.Contains(follow.Reason, "ValidateToken is defined in internal/auth/token.go:42.") {
		t.Fatalf("expected terminal deny carrying the final response, got %+v", follow)
	}

	// Stop sends an answer that ignores the localized symbol back to the model.
	out, _ := handleAntigravity(agyPayload(t, conv, map[string]any{
		"executionNum":      1,
		"terminationReason": "NO_TOOL_CALL",
		"finalModelOutput":  "The handler is in `UnrelatedHandler` inside `other/pkg/handler.go`.",
	}), 0, ModeDeny, "Stop")
	if stop := agyDecision(t, out); stop.Decision != "continue" || !strings.Contains(stop.Reason, "ValidateToken") {
		t.Fatalf("expected Stop decision=continue for an ungrounded claim, got %+v", stop)
	}
}

func TestRunAntigravityStdinStdout(t *testing.T) {
	origStdin, origStdout := os.Stdin, os.Stdout
	defer func() { os.Stdin, os.Stdout = origStdin, origStdout }()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = inW.Write([]byte(`{"conversationId":"conv-run","workspacePaths":["/tmp"],"toolCall":{"name":"read_file","args":{"Path":"foo.go"}}}`))
		_ = inW.Close()
	}()
	os.Stdin, os.Stdout = inR, outW

	RunAntigravity(0, ModeDeny, "PreToolUse")
	_ = outW.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, outR)
	if !strings.Contains(buf.String(), `"decision":"allow"`) {
		t.Errorf("RunAntigravity stdout = %q, want decision:allow", buf.String())
	}
}

func TestHandleAntigravityMalformedJSONFailsOpen(t *testing.T) {
	if out, code := handleAntigravity([]byte("{malformed"), 0, ModeDeny, "PreToolUse"); code != 0 || out != `{"decision":"allow"}` {
		t.Errorf("malformed PreToolUse = (%s, %d), want allow", out, code)
	}
	if out, code := handleAntigravity([]byte("{malformed"), 0, ModeDeny, "SessionStart"); code != 0 || out != "{}" {
		t.Errorf("malformed SessionStart = (%s, %d), want {}", out, code)
	}
}

// A whole-file Gortex read is a nudge on Claude and Kimi, and a deny only
// under GORTEX_HOOK_FORCE_COMPRESS. PreToolUse has no soft channel, so the
// default answer is a plain allow rather than a deny.
func TestHandleAntigravityGortexWholeFileReadDeniesOnlyWhenForced(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	withDaemonReachable(t, true)

	payload := agyPayload(t, "conv-gortex-read", map[string]any{
		"toolCall": agyGortexCall("read_file", map[string]any{"path": "internal/auth/token.go"}),
	})
	if out := mustAgy(t, payload, ModeDeny); out != `{"decision":"allow"}` {
		t.Fatalf("whole-file Gortex read should be allowed by default, got %s", out)
	}
	t.Setenv("GORTEX_HOOK_FORCE_COMPRESS", "1")
	if dec := agyDecision(t, mustAgy(t, payload, ModeDeny)); dec.Decision != "deny" || !strings.Contains(dec.Reason, "compress_bodies") {
		t.Fatalf("expected compress_bodies deny under GORTEX_HOOK_FORCE_COMPRESS, got %+v", dec)
	}
}

// In nudge mode a Gortex call is the symbolic move that resets the
// fallback streak, as it does for Claude and Copilot.
func TestHandleAntigravityNudgeModeResetsStreakOnGortexCall(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 9)

	read := agyPayload(t, "conv-nudge", map[string]any{"toolCall": agyNativeRead("internal/auth/token.go")})
	gortex := agyPayload(t, "conv-nudge", map[string]any{"toolCall": agyGortexCall("explore", map[string]any{"task": "check token"})})
	decision := func(payload []byte) string {
		return agyDecision(t, mustAgy(t, payload, ModeAdaptiveNudge)).Decision
	}

	for i := 0; i < nudgeThreshold-1; i++ {
		decision(read)
	}
	if got := decision(gortex); got != "allow" {
		t.Fatalf("Gortex call = %q, want allow", got)
	}
	for i := 0; i < nudgeThreshold-1; i++ {
		if got := decision(read); got != "allow" {
			t.Fatalf("read %d after the Gortex call = %q, want allow (streak should have reset)", i+1, got)
		}
	}
	if got := decision(read); got != "deny" {
		t.Fatalf("read at the threshold = %q, want the one-shot nudge deny", got)
	}
}

// Before hooks moved to ~/.gemini/config/hooks.json, Gortex wrote
// `gortex hook --agent antigravity` (no --event) into ~/.gemini/settings.json,
// which only Gemini CLI reads. Those entries must keep getting Gemini's
// hookSpecificOutput shape, not agy's injectSteps.
func TestHandleAntigravityLegacyGeminiSettingsEntryAnswersInGeminiShape(t *testing.T) {
	withAgyFixtureRepo(t)

	payload := copilotPayload(t, map[string]any{"hook_event_name": "SessionStart", "cwd": repoTmpFixtureRoot})
	out, _ := handleAntigravity(payload, 0, ModeDeny, "")
	var resp HookOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal legacy response: %v\n%s", err, out)
	}
	if strings.Contains(out, "injectSteps") || resp.HookSpecificOutput == nil ||
		resp.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(resp.HookSpecificOutput.AdditionalContext, "Gortex Session Orientation") {
		t.Fatalf("expected Gemini hookSpecificOutput SessionStart orientation, got %s", out)
	}
}
