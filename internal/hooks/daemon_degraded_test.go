package hooks

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/localizationauth"
)

// The tests below pin the #486 contract: while the daemon is unreachable,
// every per-call enforcement surface stands down — no denies, no
// MCP-mandating advisories, no "do not re-Read / re-Grep" follow-ups —
// because that guidance points at tools the daemon cannot serve. Each test
// first proves the enforcement fires with the daemon up (so the outage
// assertion tests the gate, not a broken fixture), then flips reachability.

func preToolReadPayload(t *testing.T, sessionID string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Read",
		"tool_input":      map[string]any{"file_path": filepath.Join(repoFixtureRoot, "internal", "server.go")},
		"cwd":             repoFixtureRoot,
		"session_id":      sessionID,
	})
}

func TestPreToolUseDaemonDownStandsDownWithOneNotice(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	stubIndexedFile(t, true, 9) // would hard-deny if enforcement were live
	withDaemonReachable(t, true)

	control := captureHookStdout(t, func() { runPreToolUse(preToolReadPayload(t, "sess-outage"), 0, ModeDeny) })
	if !strings.Contains(control, "deny") {
		t.Fatalf("control: indexed Read should deny while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	first := captureHookStdout(t, func() { runPreToolUse(preToolReadPayload(t, "sess-outage"), 0, ModeDeny) })
	if strings.Contains(first, "deny") || strings.Contains(first, "BLOCKED") {
		t.Fatalf("daemon down must not deny: %q", first)
	}
	if !strings.Contains(first, "standing down to rule-only guidance") {
		t.Fatalf("first degraded call should carry the once-per-session notice: %q", first)
	}
	if !strings.Contains(first, daemonUnreachableStance) {
		t.Fatalf("notice must state the briefing's outage stance: %q", first)
	}

	second := captureHookStdout(t, func() { runPreToolUse(preToolReadPayload(t, "sess-outage"), 0, ModeDeny) })
	if second != "" {
		t.Fatalf("degraded PreToolUse must go quiet after the one notice: %q", second)
	}
}

func TestPreToolUseDaemonDownWithoutSessionStaysSilent(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	stubIndexedFile(t, true, 3)
	withDaemonReachable(t, false)

	out := captureHookStdout(t, func() { runPreToolUse(preToolReadPayload(t, ""), 0, ModeDeny) })
	if out != "" {
		t.Fatalf("no session id means no dedupe, so the degraded call must stay silent: %q", out)
	}
}

func TestPreToolUseDaemonDownGatesForceCompressDeny(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	t.Setenv(gortexForceCompressEnvVar, "1")
	payload := mustJSON(t, map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       gortexCompactReadTool,
		"tool_input": map[string]any{
			"operation": "file",
			"target":    map[string]any{"file": filepath.Join(repoFixtureRoot, "internal", "server.go")},
		},
		"cwd": repoFixtureRoot,
	})

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if !strings.Contains(control, "deny") {
		t.Fatalf("control: force-compress should deny a whole-file read while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if out != "" {
		t.Fatalf("the force-compress deny is local, but enforcing it during an outage blocks a call that already fails on transport: %q", out)
	}
}

func TestPreToolUseTerminalDenyStaysLiveDuringOutage(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	withDaemonReachable(t, false)
	cwd := t.TempDir()
	identity := beginTestLocalizationSubagentTurn(t, "sess-terminal-outage", "agent-1", cwd)
	if !markLocalizationTerminal(identity, localizationTerminalContractV2) {
		t.Fatal("markLocalizationTerminal failed")
	}

	payload := mustJSON(t, map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Read",
		"tool_input":      map[string]any{"file_path": filepath.Join(repoFixtureRoot, "internal", "server.go")},
		"cwd":             cwd,
		"session_id":      "sess-terminal-outage",
		"agent_id":        "agent-1",
	})
	out := captureHookStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if !strings.Contains(out, "Localization for this task is complete") {
		t.Fatalf("terminal enforcement is daemon-independent and must survive an outage: %q", out)
	}
}

func TestPostToolUseDaemonDownSuppressesFollowUps(t *testing.T) {
	oldSummary := fileSummaryFn
	fileSummaryFn = func(_, _ string) (*hookFileSummary, bool) {
		return &hookFileSummary{
			Symbols:    []summaryNode{{Name: "Foo", Kind: "function", StartLine: 1, EndLine: 20}},
			Dependents: 2,
		}, true
	}
	t.Cleanup(func() { fileSummaryFn = oldSummary })

	payload := mustJSON(t, map[string]any{
		"hook_event_name": "PostToolUse",
		"tool_name":       "Read",
		"tool_input":      map[string]any{"file_path": filepath.Join(repoFixtureRoot, "internal", "server.go")},
		"tool_response":   "package server",
		"cwd":             repoFixtureRoot,
	})

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runPostToolUse(payload) })
	if !strings.Contains(control, "Do not re-Read") {
		t.Fatalf("control: expected the follow-up while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runPostToolUse(payload) })
	if out != "" {
		t.Fatalf("PostToolUse follow-ups must not land during an outage: %q", out)
	}
}

func TestCodexBashHardDenyDaemonDownStandsDown(t *testing.T) {
	stubIndexedFile(t, true, 4)

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runCodexBashHardDeny(codexBashPayload("cat internal/server.go"), 0) })
	if !strings.Contains(control, "deny") {
		t.Fatalf("control: indexed cat should deny while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runCodexBashHardDeny(codexBashPayload("cat internal/server.go"), 0) })
	if out != "" {
		t.Fatalf("Codex deny posture must stand down during an outage: %q", out)
	}
}

func TestCodexBashRewriteDaemonDownStandsDown(t *testing.T) {
	stubIndexedFile(t, true, 4)

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runCodexBashRewrite(codexBashPayload("cat internal/server.go"), 0) })
	if !strings.Contains(control, "gortex call read") {
		t.Fatalf("control: indexed cat should be rewritten while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runCodexBashRewrite(codexBashPayload("cat internal/server.go"), 0) })
	if out != "" {
		t.Fatalf("rewriting a working cat into an unserveable gortex call would break the command: %q", out)
	}
}

func TestCodexMCPReadDaemonDownStaysSilent(t *testing.T) {
	payload := mustJSON(t, map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       gortexCompactReadTool,
		"tool_input": map[string]any{
			"operation": "file",
			"target":    map[string]any{"file": filepath.Join(repoFixtureRoot, "internal", "server.go")},
		},
		"cwd": repoFixtureRoot,
	})

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runCodexMCPReadPreToolUse(payload, CodexModeDeny) })
	if !strings.Contains(control, "deny") {
		t.Fatalf("control: expected the compress-bodies deny while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runCodexMCPReadPreToolUse(payload, CodexModeDeny) })
	if out != "" {
		t.Fatalf("a Gortex read the daemon cannot serve must not be blocked on a nudge: %q", out)
	}
}

func TestKimiPreToolUseDaemonDownStaysSilent(t *testing.T) {
	stubIndexedFile(t, true, 4)
	input := map[string]any{"file_path": filepath.Join(repoFixtureRoot, "internal", "server.go")}

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runKimiPreToolUse("Read", input, "sess", 0, ModeDeny) })
	if !strings.Contains(control, "deny") {
		t.Fatalf("control: indexed Read should deny while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runKimiPreToolUse("Read", input, "sess", 0, ModeDeny) })
	if out != "" {
		t.Fatalf("Kimi's documented contract is a silent no-op when the daemon is unreachable: %q", out)
	}
}

func TestKimiSubagentStartDaemonDownStaysSilent(t *testing.T) {
	payload := mustJSON(t, map[string]any{"hook_event_name": "SubagentStart"})

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runKimiSubagentStart(payload, 0) })
	if !strings.Contains(control, "MUST use Gortex MCP tools") {
		t.Fatalf("control: expected the fallback briefing while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runKimiSubagentStart(payload, 0) })
	if out != "" {
		t.Fatalf("a subagent must not be told MCP is mandatory while the daemon cannot serve it: %q", out)
	}
}

func TestPiToolCallDaemonDownEmptyDecision(t *testing.T) {
	stubIndexedFile(t, true, 4)
	ev := PiEvent{
		Event:     "tool_call",
		ToolName:  "Read",
		ToolInput: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "internal", "server.go")},
		CWD:       repoFixtureRoot,
		SessionID: "sess",
	}

	withDaemonReachable(t, true)
	if control := piToolCall(ev, 0, ModeDeny); !control.Block {
		t.Fatalf("control: indexed Read should block while the daemon is up: %#v", control)
	}

	daemonReachableFn = func() bool { return false }
	if d := piToolCall(ev, 0, ModeDeny); d.Block || d.Reason != "" || d.AdditionalContext != "" {
		t.Fatalf("Pi must return an empty decision during an outage: %#v", d)
	}
}

func TestHermesPreToolCallDaemonDownNeverBlocks(t *testing.T) {
	stubIndexedFile(t, true, 4)
	payload := mustJSON(t, map[string]any{
		"hook_event_name": hermesEventPreToolCall,
		"tool_name":       hermesReadFileTool,
		"tool_input":      map[string]any{"path": filepath.Join(repoFixtureRoot, "internal", "server.go")},
		"cwd":             repoFixtureRoot,
		"session_id":      "sess",
	})

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runHermesPreToolCall(payload, 0, ModeDeny) })
	if !strings.Contains(control, "block") {
		t.Fatalf("control: indexed read_file should block while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runHermesPreToolCall(payload, 0, ModeDeny) })
	if out != "" {
		t.Fatalf("Hermes' only channel is a block, which must never fire during an outage: %q", out)
	}
}

func TestDaemonDownNoticeSharesBriefingStance(t *testing.T) {
	if !strings.Contains(daemonDownNotice, daemonUnreachableStance) {
		t.Fatal("the per-call notice must carry the same stance sentence as the SessionStart briefing")
	}

	old := sessionStartStatusFn
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) { return nil, errDaemonUnreachable }
	t.Cleanup(func() { sessionStartStatusFn = old })
	briefing := buildSessionStartBriefing("")
	if !strings.Contains(briefing, daemonUnreachableStance) {
		t.Fatalf("the unreachable briefing must carry the shared stance sentence: %q", briefing)
	}
}

func TestPreToolUseDaemonDownPreservesLocalizationPassthrough(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	withDaemonReachable(t, false)
	problemStatement := "Title: Neutral storage failure\n\nDiskStorage.load returns an empty value."
	identity, ok := beginLocalizationTurnWithProblem(t.Name(), "prompt", "", t.TempDir(), problemStatement)
	if !ok {
		t.Fatal("beginLocalizationTurnWithProblem failed")
	}
	payload := mustJSON(t, map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       gortexMCPToolPrefix + "explore",
		"tool_use_id":     "tool-use",
		"tool_input": map[string]any{
			"operation": "localize",
			"task":      "literal symptom",
			"options":   map[string]any{"new_user_task": true},
		},
		"session_id": identity.SessionID,
		"prompt_id":  identity.PromptID,
		"cwd":        identity.CWD,
	})

	output := captureHookStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	var decoded HookOutput
	if err := json.Unmarshal([]byte(output), &decoded); err != nil || decoded.HookSpecificOutput == nil {
		t.Fatalf("invalid degraded PreToolUse output: %v\n%s", err, output)
	}
	hso := decoded.HookSpecificOutput
	if hso.PermissionDecision == "deny" {
		t.Fatalf("degraded branch must not deny: %#v", hso)
	}
	// The auth nonce and the problem-statement rewrite are what correlate a
	// still-live MCP transport's answer_ready receipt with this turn; the
	// stand-down must not strip them.
	if _, ok := hso.UpdatedInput[localizationauth.ArgumentKey].(string); !ok {
		t.Fatalf("degraded branch lost the terminal auth passthrough: %#v", hso)
	}
	if got := hso.UpdatedInput["task"]; got != problemStatement {
		t.Fatalf("degraded branch lost the problem-statement rewrite: %#v", got)
	}
}

func TestDaemonDownNoticeScopedToTrackedRepos(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	withDaemonReachable(t, false)
	oldRepos := hookTrackedReposFn
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus {
		return []daemon.TrackedRepoStatus{{Path: repoFixtureRoot, Name: "repo"}}
	}
	t.Cleanup(func() { hookTrackedReposFn = oldRepos })

	untracked := mustJSON(t, map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Read",
		"tool_input":      map[string]any{"file_path": filepath.Join(elsewhereFixtureRoot, "scratch.go")},
		"cwd":             elsewhereFixtureRoot,
		"session_id":      "sess-scoped",
	})
	if out := captureHookStdout(t, func() { runPreToolUse(untracked, 0, ModeDeny) }); out != "" {
		t.Fatalf("enforcement never applied outside tracked repos, so the notice must not fire there: %q", out)
	}

	// The untracked call must not have burned the session's single notice.
	tracked := preToolReadPayload(t, "sess-scoped")
	out := captureHookStdout(t, func() { runPreToolUse(tracked, 0, ModeDeny) })
	if !strings.Contains(out, "standing down to rule-only guidance") {
		t.Fatalf("first tracked-repo call should still carry the notice: %q", out)
	}
}

func TestPreToolUseDaemonDownSkipsNudgeUnderAutoApprove(t *testing.T) {
	payload := mustJSON(t, map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       gortexCompactReadTool,
		"tool_input": map[string]any{
			"operation": "file",
			"target":    map[string]any{"file": filepath.Join(repoFixtureRoot, "internal", "server.go")},
		},
		"cwd":             repoFixtureRoot,
		"permission_mode": "acceptEdits",
	})

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if !strings.Contains(control, "auto-approved") || !strings.Contains(control, "compress_bodies") {
		t.Fatalf("control: auto-approve should carry the read nudge while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if !strings.Contains(out, "auto-approved") {
		t.Fatalf("auto-approve itself must survive the outage: %q", out)
	}
	if strings.Contains(out, "compress_bodies") {
		t.Fatalf("coaching the shape of a call about to fail on transport must be skipped: %q", out)
	}
}

func TestCodexPostToolUseSuppressDaemonDownStaysSilent(t *testing.T) {
	oldSummary := fileSummaryFn
	fileSummaryFn = func(_, _ string) (*hookFileSummary, bool) {
		return &hookFileSummary{
			Symbols: []summaryNode{{Name: "Foo", Kind: "function", StartLine: 1, EndLine: 20}},
		}, true
	}
	t.Cleanup(func() { fileSummaryFn = oldSummary })
	payload := codexPostBashPayload("grep -rn Foo internal/", "internal/server.go:3:Foo()")

	withDaemonReachable(t, true)
	control := captureHookStdout(t, func() { runCodexPostToolUse(payload, 0, CodexModeSuppress) })
	if !strings.Contains(control, "Do not re-Grep") {
		t.Fatalf("control: expected the graph follow-up while the daemon is up: %q", control)
	}

	daemonReachableFn = func() bool { return false }
	out := captureHookStdout(t, func() { runCodexPostToolUse(payload, 0, CodexModeSuppress) })
	if out != "" {
		t.Fatalf("Codex suppress-mode follow-ups must not land during an outage: %q", out)
	}
}

func TestPiAdaptiveNudgeStreakFrozenDuringOutage(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	const session = "sess-pi-streak"
	saveSessionState(session, sessionState{NonSymbolicStreak: 2})
	ev := PiEvent{
		Event:        "tool_call",
		ToolName:     "search_symbols",
		CWD:          repoFixtureRoot,
		SessionID:    session,
		IsGortexTool: true,
	}

	withDaemonReachable(t, false)
	if d := piToolCall(ev, 0, ModeAdaptiveNudge); d.Block || d.AdditionalContext != "" {
		t.Fatalf("gortex call must pass through untouched: %#v", d)
	}
	if got := loadSessionState(session).NonSymbolicStreak; got != 2 {
		t.Fatalf("outage must freeze the streak like every other adapter, got %d", got)
	}

	daemonReachableFn = func() bool { return true }
	if d := piToolCall(ev, 0, ModeAdaptiveNudge); d.Block || d.AdditionalContext != "" {
		t.Fatalf("gortex call must pass through untouched: %#v", d)
	}
	if got := loadSessionState(session).NonSymbolicStreak; got != 0 {
		t.Fatalf("a symbolic call with the daemon up should reset the streak, got %d", got)
	}
}

func TestDaemonDownNoticeOnceDeduplicatesPerSession(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	if got := daemonDownNoticeOnce("sess-a"); got != daemonDownNotice {
		t.Fatalf("first call should return the notice, got %q", got)
	}
	if got := daemonDownNoticeOnce("sess-a"); got != "" {
		t.Fatalf("second call for the same session must be silent, got %q", got)
	}
	if got := daemonDownNoticeOnce("sess-b"); got != daemonDownNotice {
		t.Fatalf("a different session gets its own notice, got %q", got)
	}
	if got := daemonDownNoticeOnce(""); got != "" {
		t.Fatalf("a session without a state path must stay silent, got %q", got)
	}
}
