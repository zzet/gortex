package hooks

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOutsideConfigRecoveryClaudeDispatchAllowsBothBlockEditModes(t *testing.T) {
	f := newOutsideConfigFixture(t)
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	for _, input := range outsideConfigDoors() {
		for _, block := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v/block%v", input.ToolName, input.ToolInput, block), func(t *testing.T) {
				withEditBlocking(t, block)
				calls := 0
				stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus {
					calls++
					return fileIndexStatus{Indexed: true, Count: 2, Tracked: true, ProbeOK: true, MutationKnown: true, ReadOnly: true}
				})
				input.HookEventName, input.CWD, input.SessionID = "PreToolUse", f.target, "outside-claude"
				input.PermissionMode = "default"
				output := captureHookStdout(t, func() { runPreToolUse(mustJSON(t, input), 0, ModeDeny) })
				var got HookOutput
				require.NoError(t, json.Unmarshal([]byte(output), &got))
				require.NotNil(t, got.HookSpecificOutput)
				require.NotEqual(t, "deny", got.HookSpecificOutput.PermissionDecision)
				require.Contains(t, got.HookSpecificOutput.AdditionalContext, "native reads and edits are allowed")
				require.Equal(t, 1, calls)
			})
		}
	}
}

func TestOutsideConfigRecoveryCodexRewriteReusesUnderlyingObservation(t *testing.T) {
	f := newOutsideConfigFixture(t)
	for name, status := range map[string]fileIndexStatus{
		"read_only": {Indexed: true, Count: 2, Tracked: true, ProbeOK: true, MutationKnown: true, ReadOnly: true},
		"ready":     {Indexed: true, Count: 2, Tracked: true, ProbeOK: true, MutationKnown: true},
		"unreached": {Unreached: true},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			stubFileIndexScopeBy(t, func(cwd, path string) fileIndexStatus {
				calls++
				require.Equal(t, f.target, cwd)
				require.Equal(t, "a.go", path)
				return status
			})
			data := mustJSON(t, HookInput{HookEventName: "PreToolUse", ToolName: "Bash", SessionID: "outside-codex", CWD: f.target, ToolInput: map[string]any{"command": "cat a.go"}})
			output := captureHookStdout(t, func() { runCodex(data, 0, CodexModeRewrite) })
			require.Equal(t, 1, calls, "rewrite and scope-recovery must share one real query")
			if name == "unreached" {
				require.Empty(t, output, "outside-config failure retains its previous silence")
				return
			}
			var got HookOutput
			require.NoError(t, json.Unmarshal([]byte(output), &got))
			require.NotNil(t, got.HookSpecificOutput)
			require.NotEqual(t, "deny", got.HookSpecificOutput.PermissionDecision)
			if name == "ready" {
				require.NotNil(t, got.HookSpecificOutput.UpdatedInput, "preserve the existing indexed-ready rewrite")
				require.NotContains(t, got.HookSpecificOutput.AdditionalContext, "writes are refused")
			} else {
				require.Nil(t, got.HookSpecificOutput.UpdatedInput, "a refused edit must retain its native read door")
				require.Contains(t, got.HookSpecificOutput.AdditionalContext, "native reads and edits are allowed")
			}
		})
	}
}

func TestOutsideConfigRecoveryCodexEnrichAndDenyKeepNativeRead(t *testing.T) {
	f := newOutsideConfigFixture(t)
	for _, mode := range []CodexMode{CodexModeEnrich, CodexModeDeny} {
		t.Run(mode.String(), func(t *testing.T) {
			calls := 0
			stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus {
				calls++
				return fileIndexStatus{Indexed: true, Count: 2, Tracked: true, ProbeOK: true, MutationKnown: true, ReadOnly: true}
			})
			data := mustJSON(t, HookInput{HookEventName: "PreToolUse", ToolName: "Bash", SessionID: "outside-codex-mode", CWD: f.target, ToolInput: map[string]any{"command": "cat a.go"}})
			output := captureHookStdout(t, func() { runCodex(data, 0, mode) })
			var got HookOutput
			require.NoError(t, json.Unmarshal([]byte(output), &got))
			require.NotNil(t, got.HookSpecificOutput)
			require.NotEqual(t, "deny", got.HookSpecificOutput.PermissionDecision)
			require.Contains(t, got.HookSpecificOutput.AdditionalContext, "native reads and edits are allowed")
			require.Equal(t, 1, calls)
		})
	}
}

func TestOutsideConfigRecoveryOtherAdaptersPreserveTheirWireContracts(t *testing.T) {
	f := newOutsideConfigFixture(t)
	calls := 0
	stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus {
		calls++
		return fileIndexStatus{Indexed: true, Count: 2, Tracked: true, ProbeOK: true, MutationKnown: true, ReadOnly: true}
	})
	t.Run("Kimi", func(t *testing.T) {
		before := calls
		setHookCWD(f.target)
		defer setHookCWD("")
		output := captureHookStdout(t, func() { runKimiPreToolUse("Read", map[string]any{"file_path": "a.go"}, "outside-kimi", 0, ModeDeny) })
		require.Contains(t, output, "native reads and edits are allowed")
		require.Equal(t, before+1, calls)
	})
	t.Run("Pi_OpenCode_shared_bridge", func(t *testing.T) {
		before := calls
		got := bridgeToolCall(BridgeEvent{ToolName: "Read", ToolInput: map[string]any{"file_path": "a.go"}, CWD: f.target, SessionID: "outside-bridge"}, 0, ModeDeny)
		require.False(t, got.Block)
		require.Contains(t, got.AdditionalContext, "native reads and edits are allowed")
		require.Equal(t, before+1, calls)
	})
	for _, tool := range []string{hermesReadFileTool, hermesTerminalTool} {
		t.Run("Hermes_"+tool, func(t *testing.T) {
			before := calls
			args := map[string]any{"path": "a.go"}
			if tool == hermesTerminalTool {
				args = map[string]any{"command": "cat a.go"}
			}
			data := mustJSON(t, hermesPreToolInput{HookEventName: hermesEventPreToolCall, ToolName: tool, ToolInput: args, CWD: f.target, SessionID: "outside-hermes"})
			output := captureHookStdout(t, func() { runHermesPreToolCall(data, 0, ModeDeny) })
			require.Empty(t, output, "hard-only Hermes pre-tool hook cannot invent a soft channel")
			require.Equal(t, before+1, calls)
		})
	}
	t.Run("Copilot_hard_only", func(t *testing.T) {
		before := calls
		out, code := copilotPreToolUse(copilotInput{ToolName: "view", ToolArgs: map[string]any{"path": "a.go"}, CWD: f.target}, 0, ModeDeny)
		require.Zero(t, code)
		require.Empty(t, out, "do not add an unsupported pre-tool context channel")
		require.Equal(t, before+1, calls)
	})
}

func TestOutsideConfigRecoveryHermesRetainsItsRawCoverageContract(t *testing.T) {
	f := newOutsideConfigFixture(t)
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	for _, root := range []string{f.primary, f.target} {
		t.Run(root, func(t *testing.T) {
			// Another adapter's compatible legacy marker must not introduce a
			// new session-fallback policy into Hermes' existing raw probe path.
			require.True(t, observeMutationFallback(mutationPostFixture(root, "same-id", true, legacyStaleRefusal), time.Now()))
			calls := 0
			stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus {
				calls++
				return fileIndexStatus{Indexed: true, Count: 2, Tracked: true, ProbeOK: true}
			})
			result := hermesEnrich(hermesPreToolInput{ToolName: hermesReadFileTool, ToolInput: map[string]any{"path": "a.go"}, CWD: root, SessionID: "same-id"}, 0)
			require.Equal(t, root == f.primary, result.deny)
			require.False(t, result.readOnlyAllowance)
			require.Equal(t, 1, calls)
		})
	}
}

func TestOutsideConfigRecoveryCannotBypassLocalizationTerminal(t *testing.T) {
	f := newOutsideConfigFixture(t)
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "outside-terminal", "prompt-1", f.target)
	snapshotTestLocalizationTool(t, identity, gortexMCPToolPrefix+"read", "tool-1")
	post := localizationPostToolPayload(t, gortexMCPToolPrefix+"read", "tool-1", identity, terminalToolResponse(t, terminalContractMap(), true, false))
	require.Contains(t, captureHookStdout(t, func() { runPostToolUse(post) }), localizationTerminalContext)
	calls := 0
	stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus {
		calls++
		return fileIndexStatus{ReadOnly: true, ProbeOK: true}
	})
	for _, host := range []string{"Claude", "Codex"} {
		t.Run(host, func(t *testing.T) {
			tool, args := "Read", map[string]any{"file_path": "a.go"}
			if host == "Codex" {
				tool, args = "Bash", map[string]any{"command": "cat a.go"}
			}
			pre := preToolPayload(t, tool, "", identity, args)
			output := captureHookStdout(t, func() {
				if host == "Codex" {
					runCodex(pre, 0, CodexModeRewrite)
				} else {
					runPreToolUse(pre, 0, ModeDeny)
				}
			})
			var got HookOutput
			require.NoError(t, json.Unmarshal([]byte(output), &got))
			require.NotNil(t, got.HookSpecificOutput)
			require.Equal(t, "deny", got.HookSpecificOutput.PermissionDecision)
			require.Contains(t, got.HookSpecificOutput.PermissionDecisionReason, localizationTerminalDenyReason)
			require.Zero(t, calls, "terminal policy precedes every recovery probe")
		})
	}
}
