package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestObserveLocalizationTerminalAcceptsDirectAndPluginNavigationFacades(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	prefixes := []string{gortexMCPToolPrefix, gortexPluginMCPToolPrefix}
	operations := []string{"explore", "search", "read", "relations", "trace", "analyze"}
	for _, prefix := range prefixes {
		for _, operation := range operations {
			tool := prefix + operation
			t.Run(tool, func(t *testing.T) {
				sessionID := strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())
				cwd := t.TempDir()
				identity := beginTestLocalizationTurn(t, sessionID, "prompt-1", cwd)
				toolUseID := "tool-1"
				snapshotTestLocalizationTool(t, identity, tool, toolUseID)
				data := localizationPostToolPayload(t, tool, toolUseID, identity, terminalToolResponse(t, terminalContractMap(), true, false))
				if _, observed := observeLocalizationTerminal(data); !observed {
					t.Fatalf("expected %s terminal contract to be observed", tool)
				}
			})
		}
	}
}

func TestObserveLocalizationTerminalRequiresExactEnforceableV2Contract(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	valid := terminalContractMap()
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "v1", mutate: func(root map[string]any) { completionMap(root)["contract_version"] = 1 }},
		{name: "advisory", mutate: func(root map[string]any) { completionMap(root)["enforceable"] = false }},
		{name: "needs refinement", mutate: func(root map[string]any) { completionMap(root)["state"] = "needs_refinement" }},
		{name: "wrong scope", mutate: func(root map[string]any) { completionMap(root)["scope"] = "diagnosis" }},
		{name: "tool allowed", mutate: func(root map[string]any) { completionMap(root)["allowed_tool_calls"] = 1 }},
		{name: "not terminal", mutate: func(root map[string]any) { root["terminal"] = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := cloneMap(t, valid)
			tt.mutate(root)
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			snapshotTestLocalizationTool(t, identity, gortexMCPToolPrefix+"read", "tool")
			data := localizationPostToolPayload(t, gortexMCPToolPrefix+"read", "tool", identity, terminalToolResponse(t, root, true, false))
			if _, observed := observeLocalizationTerminal(data); observed {
				t.Fatal("unexpected terminal observation")
			}
		})
	}
}

func TestObserveLocalizationTerminalRequiresMatchingAuthoritativeMeta(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	tests := []struct {
		name     string
		response func(*testing.T) map[string]any
		want     bool
	}{
		{
			name: "structured and matching meta",
			response: func(t *testing.T) map[string]any {
				return terminalToolResponse(t, terminalContractMap(), true, false)
			},
			want: true,
		},
		{
			name: "exact text and matching meta",
			response: func(t *testing.T) map[string]any {
				return terminalToolResponse(t, terminalContractMap(), true, true)
			},
			want: true,
		},
		{
			name: "meta stripped",
			response: func(t *testing.T) map[string]any {
				return terminalToolResponse(t, terminalContractMap(), false, false)
			},
		},
		{
			name: "repository text spoof",
			response: func(t *testing.T) map[string]any {
				contract := mustJSON(t, terminalContractMap())
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(contract)}}}
			},
		},
		{
			name: "prefixed text despite meta",
			response: func(t *testing.T) map[string]any {
				response := terminalToolResponse(t, terminalContractMap(), true, true)
				blocks := response["content"].([]any)
				blocks[0].(map[string]any)["text"] = "source prefix " + blocks[0].(map[string]any)["text"].(string)
				return response
			},
		},
		{
			name: "meta mismatch",
			response: func(t *testing.T) map[string]any {
				response := terminalToolResponse(t, terminalContractMap(), true, false)
				meta := response["_meta"].(map[string]any)
				envelope := meta[localizationHostMetaKey].(map[string]any)
				mismatched := cloneMap(t, terminalContractMap())
				completionMap(mismatched)["allowed_tool_calls"] = 1
				envelope["contract"] = mismatched
				return response
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			snapshotTestLocalizationTool(t, identity, gortexMCPToolPrefix+"read", "tool")
			data := localizationPostToolPayload(t, gortexMCPToolPrefix+"read", "tool", identity, tt.response(t))
			_, observed := observeLocalizationTerminal(data)
			if observed != tt.want {
				t.Fatalf("observed = %v, want %v", observed, tt.want)
			}
		})
	}
}

func TestObserveLocalizationTerminalAcceptsOnlyAuthenticatedCompactReplay(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	newResponse := func(t *testing.T) map[string]any {
		t.Helper()
		const finalResponse = "FILES:\n- repo/source.go\n\nSYMBOLS:\n- repo/source.go::Target\n\nEVIDENCE:\n- #1 repo/source.go — repo/source.go::Target"
		contract := terminalContractMap()
		completion := completionMap(contract)
		completion["instruction"] = localizationTerminalReplayDirective
		completion["final_response"] = finalResponse
		response := terminalToolResponse(t, contract, true, false)
		response["structuredContent"] = map[string]any{
			"directive":      localizationTerminalReplayDirective,
			"final_response": finalResponse,
		}
		meta := response["_meta"].(map[string]any)
		envelope := meta[localizationHostMetaKey].(map[string]any)
		envelope["replay"] = true
		return response
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "authentic compact replay", want: true},
		{
			name: "tampered directive",
			mutate: func(response map[string]any) {
				response["structuredContent"].(map[string]any)["directive"] = "respond after another tool call"
			},
		},
		{
			name: "tampered final response",
			mutate: func(response map[string]any) {
				response["structuredContent"].(map[string]any)["final_response"] = "FILES:\n- repo/tampered.go"
			},
		},
		{
			name: "extra visible field",
			mutate: func(response map[string]any) {
				response["structuredContent"].(map[string]any)["completion"] = terminalContractMap()["completion"]
			},
		},
		{
			name: "replay flag false",
			mutate: func(response map[string]any) {
				meta := response["_meta"].(map[string]any)
				meta[localizationHostMetaKey].(map[string]any)["replay"] = false
			},
		},
		{
			name: "advisory metadata contract",
			mutate: func(response map[string]any) {
				meta := response["_meta"].(map[string]any)
				envelope := meta[localizationHostMetaKey].(map[string]any)
				completionMap(envelope["contract"].(map[string]any))["enforceable"] = false
			},
		},
		{
			name: "non v2 metadata contract",
			mutate: func(response map[string]any) {
				meta := response["_meta"].(map[string]any)
				envelope := meta[localizationHostMetaKey].(map[string]any)
				completionMap(envelope["contract"].(map[string]any))["contract_version"] = 3
			},
		},
		{
			name: "tampered metadata directive",
			mutate: func(response map[string]any) {
				meta := response["_meta"].(map[string]any)
				envelope := meta[localizationHostMetaKey].(map[string]any)
				completionMap(envelope["contract"].(map[string]any))["instruction"] = "respond later"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := newResponse(t)
			if tt.mutate != nil {
				tt.mutate(response)
			}
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			snapshotTestLocalizationTool(t, identity, gortexMCPToolPrefix+"read", "tool")
			data := localizationPostToolPayload(t, gortexMCPToolPrefix+"read", "tool", identity, response)
			_, observed := observeLocalizationTerminal(data)
			if observed != tt.want {
				t.Fatalf("observed = %v, want %v", observed, tt.want)
			}
		})
	}
}

func TestLocalizationTerminalHookForwardsGortexReplayDeniesNativeThenPromptRotatesTurn(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	sessionID := "terminal-flow"
	cwd := t.TempDir()
	identity := beginTestLocalizationTurn(t, sessionID, "prompt-1", cwd)
	snapshotTestLocalizationTool(t, identity, gortexMCPToolPrefix+"read", "tool-1")

	post := localizationPostToolPayload(t, gortexMCPToolPrefix+"read", "tool-1", identity, terminalToolResponse(t, terminalContractMap(), true, false))
	postOutput := captureHookStdout(t, func() { runPostToolUse(post) })
	if !strings.Contains(postOutput, localizationTerminalContext) {
		t.Fatalf("PostToolUse output %q does not contain fixed terminal context", postOutput)
	}

	for _, tool := range []string{
		gortexMCPToolPrefix + "search",
		gortexPluginMCPToolPrefix + "search",
	} {
		pre := preToolPayload(t, tool, "replay-tool", identity, map[string]any{
			"operation": "symbols",
			"query":     "Target",
		})
		if got := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeDeny) }); got != "" {
			t.Fatalf("%s should pass through to the MCP replay, got %q", tool, got)
		}
	}

	native := []struct {
		tool  string
		input map[string]any
	}{
		{tool: "Read", input: map[string]any{"file_path": "/repo/source.go"}},
		{tool: "Grep", input: map[string]any{"pattern": "Target", "path": "/repo"}},
		{tool: "Glob", input: map[string]any{"pattern": "**/*.go", "path": "/repo"}},
		{tool: gortexMCPToolPrefix + "edit", input: map[string]any{"operation": "file"}},
	}
	for _, test := range native {
		pre := preToolPayload(t, test.tool, "", identity, test.input)
		preOutput := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeDeny) })
		var output HookOutput
		if err := json.Unmarshal([]byte(preOutput), &output); err != nil {
			t.Fatalf("decode %s PreToolUse output %q: %v", test.tool, preOutput, err)
		}
		if output.HookSpecificOutput == nil ||
			output.HookSpecificOutput.PermissionDecision != "deny" ||
			output.HookSpecificOutput.PermissionDecisionReason != localizationTerminalDenyReason {
			t.Fatalf("expected terminal deny for %s, got %#v", test.tool, output)
		}
	}

	beginTestLocalizationTurn(t, sessionID, "prompt-2", cwd)
	newTurn := currentTestLocalizationTurn(t, sessionID, "prompt-2", "", cwd)
	newPre := preToolPayload(t, "WebSearch", "", newTurn, nil)
	if got := captureHookStdout(t, func() { runPreToolUse(newPre, 0, ModeDeny) }); got != "" {
		t.Fatalf("new prompt inherited terminal deny: %q", got)
	}
}

func TestLocalizationTerminalDelayedPostCannotPoisonNewTurn(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	sessionID := "delayed-post"
	cwd := t.TempDir()
	oldTurn := beginTestLocalizationTurn(t, sessionID, "prompt-old", cwd)
	snapshotTestLocalizationTool(t, oldTurn, gortexMCPToolPrefix+"explore", "old-tool")
	newTurn := beginTestLocalizationTurn(t, sessionID, "prompt-new", cwd)

	oldPost := localizationPostToolPayload(t, gortexMCPToolPrefix+"explore", "old-tool", oldTurn, terminalToolResponse(t, terminalContractMap(), true, true))
	if _, observed := observeLocalizationTerminal(oldPost); observed {
		t.Fatal("a snapshot cleared at the next UserPromptSubmit must not be consumed")
	}
	// Even if a PostToolUse process had already loaded its old snapshot before
	// the prompt rotated, the marker key remains bound to the old turn token.
	if !markLocalizationTerminal(oldTurn, localizationTerminalContractV2) {
		t.Fatal("mark old turn")
	}
	if hasLocalizationTerminal(newTurn) {
		t.Fatal("old-turn marker poisoned the new turn")
	}
	if got := captureHookStdout(t, func() {
		runPreToolUse(preToolPayload(t, "WebSearch", "", newTurn, nil), 0, ModeDeny)
	}); got != "" {
		t.Fatalf("new turn was denied by delayed old PostToolUse: %q", got)
	}
}

func TestLocalizationTerminalSeparatesParentAndSubagent(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	sessionID := "shared-session"
	cwd := t.TempDir()
	parent := beginTestLocalizationTurn(t, sessionID, "parent-prompt", cwd)
	start := subagentLifecyclePayload(t, "SubagentStart", sessionID, "agent-7", cwd)
	runSubagentStart(start)
	subagent := currentTestLocalizationTurn(t, sessionID, "", "agent-7", cwd)
	if !markLocalizationTerminal(parent, localizationTerminalContractV2) {
		t.Fatal("mark parent")
	}
	if hasLocalizationTerminal(subagent) {
		t.Fatal("parent marker leaked into subagent")
	}
	if got := captureHookStdout(t, func() {
		runPreToolUse(preToolPayload(t, "WebSearch", "", subagent, nil), 0, ModeDeny)
	}); got != "" {
		t.Fatalf("subagent was denied by parent marker: %q", got)
	}
	if got := captureHookStdout(t, func() {
		runPreToolUse(preToolPayload(t, "WebSearch", "", parent, nil), 0, ModeDeny)
	}); !strings.Contains(got, `"permissionDecision":"deny"`) {
		t.Fatalf("parent marker did not deny parent tool: %q", got)
	}
}

func TestLocalizationTerminalSubagentLifecycleDelayedPostAndAgentIDReuse(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	sessionID := "subagent-reuse"
	agentID := "agent-7"
	cwd := t.TempDir()
	start := subagentLifecyclePayload(t, "SubagentStart", sessionID, agentID, cwd)
	stop := subagentLifecyclePayload(t, "SubagentStop", sessionID, agentID, cwd)
	runSubagentStart(start)
	oldTurn := currentTestLocalizationTurn(t, sessionID, "", agentID, cwd)
	snapshotTestLocalizationTool(t, oldTurn, gortexMCPToolPrefix+"explore", "old-tool")

	runSubagentStop(stop)
	if _, ok := currentLocalizationTurn(sessionID, "", agentID, cwd); ok {
		t.Fatal("SubagentStop retained the old agent turn")
	}
	runSubagentStart(start)
	newTurn := currentTestLocalizationTurn(t, sessionID, "", agentID, cwd)
	if newTurn.TurnToken == oldTurn.TurnToken {
		t.Fatal("reused agent_id did not receive a fresh turn token")
	}

	oldPost := localizationPostToolPayload(t, gortexMCPToolPrefix+"explore", "old-tool", oldTurn, terminalToolResponse(t, terminalContractMap(), true, false))
	if _, observed := observeLocalizationTerminal(oldPost); observed {
		t.Fatal("delayed PostToolUse consumed state from the prior agent lifetime")
	}
	// A PostToolUse that loaded its snapshot just before Stop can still finish,
	// but its old token must remain inert after agent_id reuse.
	if !markLocalizationTerminal(oldTurn, localizationTerminalContractV2) {
		t.Fatal("simulate delayed old marker")
	}
	if hasLocalizationTerminal(newTurn) {
		t.Fatal("old agent-lifetime marker poisoned reused agent_id")
	}
}

func TestLocalizationTerminalSubagentPromptIDFlowsThroughStartPrePostStop(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	sessionID := "subagent-prompt"
	agentID := "agent-prompt"
	promptID := "prompt-42"
	cwd := t.TempDir()
	start := subagentLifecyclePayloadWithPrompt(t, "SubagentStart", sessionID, agentID, promptID, cwd)
	runSubagentStart(start)
	identity := currentTestLocalizationTurn(t, sessionID, promptID, agentID, cwd)
	if identity.PromptID != promptID {
		t.Fatalf("stored prompt_id = %q, want %q", identity.PromptID, promptID)
	}

	tool := gortexPluginMCPToolPrefix + "read"
	toolUseID := "prompt-tool"
	pre := preToolPayload(t, tool, toolUseID, identity, map[string]any{
		"operation": "summary",
		"target":    map[string]any{"file": "internal/hooks/pretooluse.go"},
	})
	_ = captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeDeny) })
	post := localizationPostToolPayload(t, tool, toolUseID, identity, terminalToolResponse(t, terminalContractMap(), true, false))
	if got := captureHookStdout(t, func() { runPostToolUse(post) }); !strings.Contains(got, localizationTerminalContext) {
		t.Fatalf("PostToolUse did not observe prompt-scoped terminal result: %q", got)
	}
	if !hasLocalizationTerminal(identity) {
		t.Fatal("prompt-scoped terminal marker was not persisted")
	}

	stop := subagentLifecyclePayloadWithPrompt(t, "SubagentStop", sessionID, agentID, promptID, cwd)
	runSubagentStop(stop)
	if _, ok := currentLocalizationTurn(sessionID, promptID, agentID, cwd); ok {
		t.Fatal("SubagentStop retained prompt-scoped state")
	}
}

func TestLocalizationTerminalDelayedSubagentStopCannotDeleteNewPrompt(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	sessionID := "subagent-delayed-stop"
	agentID := "reused-agent"
	cwd := t.TempDir()

	oldPromptID := "prompt-old"
	runSubagentStart(subagentLifecyclePayloadWithPrompt(t, "SubagentStart", sessionID, agentID, oldPromptID, cwd))
	oldIdentity := currentTestLocalizationTurn(t, sessionID, oldPromptID, agentID, cwd)

	newPromptID := "prompt-new"
	runSubagentStart(subagentLifecyclePayloadWithPrompt(t, "SubagentStart", sessionID, agentID, newPromptID, cwd))
	newIdentity := currentTestLocalizationTurn(t, sessionID, newPromptID, agentID, cwd)
	if oldIdentity.TurnToken == newIdentity.TurnToken {
		t.Fatal("reused agent retained the prior prompt turn token")
	}

	runSubagentStop(subagentLifecyclePayloadWithPrompt(t, "SubagentStop", sessionID, agentID, oldPromptID, cwd))
	current := currentTestLocalizationTurn(t, sessionID, newPromptID, agentID, cwd)
	if current.TurnToken != newIdentity.TurnToken {
		t.Fatalf("delayed SubagentStop changed current turn token: got %q, want %q", current.TurnToken, newIdentity.TurnToken)
	}
}

func TestLocalizationSubagentLifecycleRequiresFullIdentity(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	for _, event := range []string{"SubagentStart", "SubagentStop"} {
		for _, missing := range []string{"session_id", "agent_id", "cwd"} {
			t.Run(event+"/missing_"+missing, func(t *testing.T) {
				payload := map[string]any{
					"hook_event_name": event,
					"session_id":      "session",
					"agent_id":        "agent",
					"cwd":             t.TempDir(),
				}
				delete(payload, missing)
				data := mustJSON(t, payload)
				if event == "SubagentStart" && beginLocalizationSubagentFromHook(data) {
					t.Fatal("incomplete SubagentStart identity was accepted")
				}
				if event == "SubagentStop" && endLocalizationSubagentFromHook(data) {
					t.Fatal("incomplete SubagentStop identity was accepted")
				}
			})
		}
	}
}

func TestLocalizationUserPromptSubmitCannotFabricateSubagentTurn(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	cwd := t.TempDir()
	payload := mustJSON(t, map[string]any{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      "session",
		"agent_id":        "agent",
		"cwd":             cwd,
		"prompt":          "not a real subagent lifecycle event",
	})
	if clearLocalizationTerminalFromHook(payload) {
		t.Fatal("UserPromptSubmit with agent_id must not initialize subagent state")
	}
	if _, ok := currentLocalizationTurn("session", "", "agent", cwd); ok {
		t.Fatal("fabricated UserPromptSubmit initialized subagent state")
	}
}

func TestLocalizationStateIsBoundedAndResetPerAgent(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	sessionID := "bounded-state"
	agentID := "agent-a"
	cwd := t.TempDir()
	start := subagentLifecyclePayload(t, "SubagentStart", sessionID, agentID, cwd)
	if !beginLocalizationSubagentFromHook(start) {
		t.Fatal("SubagentStart failed")
	}
	turn := currentTestLocalizationTurn(t, sessionID, "", agentID, cwd)
	base, ok := localizationTerminalBaseFor(sessionID, agentID, cwd)
	if !ok {
		t.Fatal("base")
	}
	snapshotDir := filepath.Join(localizationAgentStateDir(base), "snapshots")
	for i := 0; i < localizationTerminalPruneLimit+32; i++ {
		path := filepath.Join(snapshotDir, fmt.Sprintf("%064x.json", i))
		if !writeLocalizationState(path, localizationToolSnapshot{
			Version: localizationTerminalMarkerVersion, Identity: turn, CreatedUnixNano: time.Now().UnixNano(),
		}) {
			t.Fatalf("seed snapshot %d", i)
		}
	}
	preserved := filepath.Join(snapshotDir, strings.Repeat("f", sha256HexLength)+".json")
	if !writeBoundedLocalizationState(preserved, localizationToolSnapshot{
		Version: localizationTerminalMarkerVersion, Identity: turn, CreatedUnixNano: time.Now().UnixNano(),
	}) {
		t.Fatal("bounded write")
	}
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > localizationTerminalPruneLimit {
		t.Fatalf("snapshot state remained unbounded: %d", len(entries))
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("current snapshot was not preserved: %v", err)
	}

	if !beginLocalizationSubagentFromHook(start) {
		t.Fatal("agent reset failed")
	}
	if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
		t.Fatalf("agent reset did not remove all per-agent snapshots: %v", err)
	}
}

func TestLocalizationStateJanitorCleansAbandonedSessionsAndMissingStops(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	cwd := t.TempDir()

	// Session A leaves agent-1 behind without SubagentStop. A later lifecycle
	// creation in the same session must remove the stale agent tree.
	beginTestLocalizationSubagentTurn(t, "session-a", "agent-1", cwd)
	baseA1, _ := localizationTerminalBaseFor("session-a", "agent-1", cwd)
	agentA1 := localizationAgentStateDir(baseA1)
	stale := time.Now().Add(-localizationTerminalMarkerTTL - time.Minute)
	if err := os.Chtimes(agentA1, stale, stale); err != nil {
		t.Fatal(err)
	}
	beginTestLocalizationSubagentTurn(t, "session-a", "agent-2", cwd)
	if _, err := os.Stat(agentA1); !os.IsNotExist(err) {
		t.Fatalf("missing-Stop agent tree survived TTL janitor: %v", err)
	}

	// Keep an independent session live, then age the whole abandoned session.
	liveB := beginTestLocalizationTurn(t, "session-b", "prompt-b", cwd)
	sessionA := localizationSessionStateDir(baseA1)
	if err := os.Chtimes(sessionA, stale, stale); err != nil {
		t.Fatal(err)
	}
	beginTestLocalizationTurn(t, "session-c", "prompt-c", cwd)
	if _, err := os.Stat(sessionA); !os.IsNotExist(err) {
		t.Fatalf("abandoned session tree survived global TTL janitor: %v", err)
	}
	if current, ok := currentLocalizationTurn(liveB.SessionID, liveB.PromptID, liveB.AgentID, liveB.CWD); !ok || current != liveB {
		t.Fatal("global janitor removed an independent live session")
	}
}

func TestLocalizationStateJanitorEnforcesSessionAndAgentHardCaps(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	cwd := t.TempDir()
	identity := beginTestLocalizationTurn(t, "cap-session", "prompt", cwd)
	base, _ := localizationTerminalBaseFor(identity.SessionID, identity.AgentID, identity.CWD)

	sessionsDir := filepath.Join(localizationTerminalRoot(), "sessions")
	for i := 0; i < localizationTerminalSessionHardCap+5; i++ {
		if err := os.MkdirAll(filepath.Join(sessionsDir, fmt.Sprintf("dummy-session-%03d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	maintainLocalizationState(base)
	if got := countStateTreeDirs(t, sessionsDir); got > localizationTerminalSessionHardCap {
		t.Fatalf("session trees = %d, hard cap = %d", got, localizationTerminalSessionHardCap)
	}

	agentsDir := filepath.Join(localizationSessionStateDir(base), "agents")
	for i := 0; i < localizationTerminalAgentHardCap+5; i++ {
		if err := os.MkdirAll(filepath.Join(agentsDir, fmt.Sprintf("dummy-agent-%03d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	maintainLocalizationState(base)
	if got := countStateTreeDirs(t, agentsDir); got > localizationTerminalAgentHardCap {
		t.Fatalf("agent trees = %d, hard cap = %d", got, localizationTerminalAgentHardCap)
	}
}

func TestLocalizationTerminalMissingTurnOrToolUseIDFailsOpen(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	cwd := t.TempDir()
	identity := localizationTerminalIdentity{SessionID: "missing", CWD: cwd, TurnToken: "not-installed"}
	if got := captureHookStdout(t, func() {
		runPreToolUse(preToolPayload(t, "WebSearch", "", identity, nil), 0, ModeDeny)
	}); got != "" {
		t.Fatalf("missing UserPromptSubmit must not deny: %q", got)
	}
	post := localizationPostToolPayload(t, gortexMCPToolPrefix+"read", "", identity, terminalToolResponse(t, terminalContractMap(), true, false))
	if _, observed := observeLocalizationTerminal(post); observed {
		t.Fatal("PostToolUse without tool_use_id snapshot must fail open")
	}
}

func TestRunPreToolUseSnapshotsDocumentedToolUseID(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "pre-snapshot", "", t.TempDir())
	tool := gortexPluginMCPToolPrefix + "read"
	pre := preToolPayload(t, tool, "tool-use-1", identity, map[string]any{
		"operation": "summary",
		"target":    map[string]any{"file": "internal/hooks/pretooluse.go"},
	})
	_ = captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeDeny) })
	post := localizationPostToolPayload(t, tool, "tool-use-1", identity, terminalToolResponse(t, terminalContractMap(), true, false))
	if _, observed := observeLocalizationTerminal(post); !observed {
		t.Fatal("production PreToolUse did not persist the tool_use_id snapshot")
	}
}

func TestPreToolUseUnrelatedToolWithoutTurnIsStrictLocalNoOp(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	originalReachable := daemonReachableFn
	probes := 0
	daemonReachableFn = func() bool {
		probes++
		return true
	}
	t.Cleanup(func() { daemonReachableFn = originalReachable })

	identity := localizationTerminalIdentity{SessionID: "no-marker", CWD: t.TempDir()}
	output := captureHookStdout(t, func() {
		runPreToolUse(preToolPayload(t, "WebSearch", "", identity, nil), 0, ModeDeny)
	})
	if output != "" {
		t.Fatalf("unrelated marker-absent tool emitted %q", output)
	}
	if probes != 0 {
		t.Fatalf("unrelated marker-absent tool made %d daemon reachability probe(s)", probes)
	}
}

func TestLocalizationTerminalMarkerUsesFullHashAndAtomicConcurrentWrites(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationSubagentTurn(t, "terminal-race", "agent", t.TempDir())
	base := strings.TrimSuffix(filepath.Base(localizationTerminalMarkerPath(identity)), ".json")
	if len(base) != sha256HexLength || strings.Trim(base, "0123456789abcdef") != "" {
		t.Fatalf("marker basename is not a full SHA-256 hex digest: %q", base)
	}

	const workers = 32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !markLocalizationTerminal(identity, localizationTerminalContractV2) {
				t.Errorf("markLocalizationTerminal failed")
			}
			_ = hasLocalizationTerminal(identity)
		}()
	}
	wg.Wait()
	if !hasLocalizationTerminal(identity) {
		t.Fatal("expected a complete marker after concurrent writers")
	}
}

func TestLocalizationTerminalMarkerRejectsStaleAndWrongIdentity(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationSubagentTurn(t, "terminal-stale", "agent-a", t.TempDir())
	if !markLocalizationTerminal(identity, localizationTerminalContractV2) {
		t.Fatal("markLocalizationTerminal failed")
	}
	wrong := identity
	wrong.AgentID = "agent-b"
	if hasLocalizationTerminal(wrong) {
		t.Fatal("marker must be scoped to the full identity")
	}

	path := localizationTerminalMarkerPath(identity)
	marker := localizationTerminalMarker{
		Version:          localizationTerminalMarkerVersion,
		ContractVersion:  localizationTerminalContractV2,
		Identity:         identity,
		ObservedUnixNano: time.Now().Add(-localizationTerminalMarkerTTL - time.Minute).UnixNano(),
	}
	if !writeLocalizationState(path, marker) {
		t.Fatal("write stale marker")
	}
	if hasLocalizationTerminal(identity) {
		t.Fatal("stale marker must fail open")
	}
}

func TestLocalizationTerminalTelemetryEventsReachProductionLog(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	path := filepath.Join(t.TempDir(), "effectiveness.jsonl")
	t.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", path)
	want := []string{"observed", "denied", "cleared_prompt", "cleared_session"}
	for _, event := range want {
		localizationTerminalTelemetry(event, true, time.Now())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != len(want) {
		t.Fatalf("telemetry records = %d, want %d: %q", len(lines), len(want), data)
	}
	for i, line := range lines {
		var record hookEffectiveness
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if record.Event != "LocalizationTerminal."+want[i] {
			t.Fatalf("record %d event = %q", i, record.Event)
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["daemon_reachable"]; ok {
			t.Fatalf("record %d falsely reported daemon reachability: %s", i, line)
		}
	}
}

func BenchmarkPreToolUseUnrelatedWithoutTurn(b *testing.B) {
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("XDG_CACHE_HOME", home)
	identity := localizationTerminalIdentity{SessionID: "benchmark", CWD: home}
	data := preToolPayloadB(b, "WebSearch", "", identity, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		runPreToolUse(data, 0, ModeDeny)
	}
}

const sha256HexLength = 64

func beginTestLocalizationTurn(t *testing.T, sessionID, promptID, cwd string) localizationTerminalIdentity {
	t.Helper()
	payload := mustJSON(t, map[string]any{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      sessionID,
		"prompt_id":       promptID,
		"cwd":             cwd,
		"prompt":          "test prompt",
	})
	_ = clearLocalizationTerminalFromHook(payload)
	return currentTestLocalizationTurn(t, sessionID, promptID, "", cwd)
}

func beginTestLocalizationSubagentTurn(t *testing.T, sessionID, agentID, cwd string) localizationTerminalIdentity {
	t.Helper()
	if !beginLocalizationSubagentFromHook(subagentLifecyclePayload(t, "SubagentStart", sessionID, agentID, cwd)) {
		t.Fatal("SubagentStart failed")
	}
	return currentTestLocalizationTurn(t, sessionID, "", agentID, cwd)
}

func subagentLifecyclePayload(t *testing.T, event, sessionID, agentID, cwd string) []byte {
	t.Helper()
	return subagentLifecyclePayloadWithPrompt(t, event, sessionID, agentID, "", cwd)
}

func subagentLifecyclePayloadWithPrompt(t *testing.T, event, sessionID, agentID, promptID, cwd string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"hook_event_name": event,
		"session_id":      sessionID,
		"agent_id":        agentID,
		"prompt_id":       promptID,
		"cwd":             cwd,
	})
}

func countStateTreeDirs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

func currentTestLocalizationTurn(t *testing.T, sessionID, promptID, agentID, cwd string) localizationTerminalIdentity {
	t.Helper()
	identity, ok := currentLocalizationTurn(sessionID, promptID, agentID, cwd)
	if !ok {
		t.Fatal("currentLocalizationTurn failed")
	}
	return identity
}

func snapshotTestLocalizationTool(t *testing.T, identity localizationTerminalIdentity, tool, toolUseID string) {
	t.Helper()
	if !snapshotLocalizationToolUse(HookInput{
		HookEventName: "PreToolUse",
		ToolName:      tool,
		ToolUseID:     toolUseID,
		SessionID:     identity.SessionID,
		PromptID:      identity.PromptID,
		AgentID:       identity.AgentID,
		CWD:           identity.CWD,
	}, identity) {
		t.Fatal("snapshotLocalizationToolUse failed")
	}
}

func localizationPostToolPayload(t *testing.T, tool, toolUseID string, identity localizationTerminalIdentity, response map[string]any) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"hook_event_name": "PostToolUse",
		"tool_name":       tool,
		"tool_use_id":     toolUseID,
		"session_id":      identity.SessionID,
		"prompt_id":       identity.PromptID,
		"agent_id":        identity.AgentID,
		"cwd":             identity.CWD,
		"tool_response":   response,
	})
}

func terminalToolResponse(t *testing.T, contract map[string]any, withMeta, text bool) map[string]any {
	t.Helper()
	response := make(map[string]any)
	if text {
		response["content"] = []any{map[string]any{"type": "text", "text": string(mustJSON(t, contract))}}
	} else {
		response["structuredContent"] = contract
	}
	if withMeta {
		response["_meta"] = map[string]any{
			localizationHostMetaKey: map[string]any{
				"version":  localizationTerminalHostMetaVersion,
				"contract": cloneMap(t, contract),
				"evidence": []any{map[string]any{"file": "repo/source.go", "id": "repo/source.go::Target"}},
			},
		}
	}
	return response
}

func preToolPayload(t *testing.T, tool, toolUseID string, identity localizationTerminalIdentity, input map[string]any) []byte {
	t.Helper()
	return preToolPayloadB(t, tool, toolUseID, identity, input)
}

type jsonTestHelper interface {
	Helper()
	Fatal(...any)
}

func preToolPayloadB(tb jsonTestHelper, tool, toolUseID string, identity localizationTerminalIdentity, input map[string]any) []byte {
	tb.Helper()
	if input == nil {
		input = map[string]any{}
	}
	data, err := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       tool,
		"tool_use_id":     toolUseID,
		"tool_input":      input,
		"session_id":      identity.SessionID,
		"prompt_id":       identity.PromptID,
		"agent_id":        identity.AgentID,
		"cwd":             identity.CWD,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return data
}

func terminalContractMap() map[string]any {
	return map[string]any{
		"completion": map[string]any{
			"state":              "answer_ready",
			"scope":              "localization",
			"required_action":    "respond",
			"allowed_tool_calls": 0,
			"contract_version":   localizationTerminalContractV2,
			"enforceable":        true,
		},
		"terminal": true,
	}
}

func completionMap(root map[string]any) map[string]any {
	return root["completion"].(map[string]any)
}

func cloneMap(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	data := mustJSON(t, in)
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func configureLocalizationTerminalTestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
}

func captureHookStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	defer func() { os.Stdout = original }()
	fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
