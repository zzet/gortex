package hooks

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"

	"github.com/zzet/gortex/internal/modelhint"
)

func TestParseCodexModeDefaultsAdvisory(t *testing.T) {
	tests := map[string]CodexMode{
		"": CodexModeEnrich, "unknown": CodexModeEnrich, "enrich": CodexModeEnrich,
		"deny": CodexModeDeny, "hard-deny": CodexModeDeny,
		"rewrite": CodexModeRewrite, "input-rewrite": CodexModeRewrite,
		"suppress": CodexModeSuppress, "replace-output": CodexModeSuppress,
	}
	for input, want := range tests {
		if got := ParseCodexMode(input); got != want {
			t.Errorf("ParseCodexMode(%q)=%v want %v", input, got, want)
		}
	}
}

func TestRunCodexMalformedJSONNoop(t *testing.T) {
	out := captureStdout(t, func() { runCodex([]byte(`{`), 0) })
	if out != "" {
		t.Fatalf("malformed JSON should be silent, got %q", out)
	}
}

func TestRunCodexSessionStartUsesManagedOrientationHook(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return nil, errDaemonUnreachable
	})
	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/gortex","source":"startup"}`)
	out := captureStdout(t, func() { runCodex(data, 0) })
	if out == "" {
		t.Fatal("expected managed SessionStart orientation")
	}
	hso := decodeHookOutput(t, out).HookSpecificOutput
	if hso == nil || hso.HookEventName != "SessionStart" {
		t.Fatalf("invalid SessionStart hook output: %s", out)
	}
	if !strings.Contains(hso.AdditionalContext, "choose by requested output") ||
		!strings.Contains(hso.AdditionalContext, "localize task may be concise") ||
		!strings.Contains(hso.AdditionalContext, "faithfully preserve the issue title") {
		t.Fatalf("mandatory localization routing orientation missing: %q", hso.AdditionalContext)
	}
}

func TestRunCodexPostToolUseWithoutParseableOutputSilent(t *testing.T) {
	data := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"rg Foo"}}`)
	out := captureStdout(t, func() { runCodex(data, 0) })
	if out != "" {
		t.Fatalf("PostToolUse without parseable output should be silent, got %q", out)
	}
}

func TestRunCodexIgnoresNonBash(t *testing.T) {
	data := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"internal/x.go"}}`)
	out := captureStdout(t, func() { runCodex(data, 0) })
	if out != "" {
		t.Fatalf("non-Bash PreToolUse should be silent, got %q", out)
	}
}

func TestRunCodexPreToolUseTerminalGateCoversHookObservableLocalTools(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "codex-terminal-families", "prompt-1", t.TempDir())
	const finalResponse = "Use the retained localization evidence."
	if !markLocalizationTerminalWithStrength(identity, localizationTerminalContractV2, false, finalResponse) {
		t.Fatal("mark enforceable terminal localization")
	}

	tests := []struct {
		name string
		tool string
		mode CodexMode
	}{
		{name: "apply patch mutation", tool: "apply_patch"},
		{name: "local plan update", tool: "update_plan"},
		{name: "subagent", tool: "spawn_agent"},
		{name: "image read", tool: "view_image"},
		{name: "bash deny posture", tool: "Bash", mode: CodexModeDeny},
		{name: "bash rewrite posture", tool: "Bash", mode: CodexModeRewrite},
		{name: "gortex navigation", tool: gortexMCPToolPrefix + "search"},
		{name: "gortex change contract", tool: gortexMCPToolPrefix + "change"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := preToolPayload(t, tt.tool, "tool-1", identity, map[string]any{})
			out := captureHookStdout(t, func() { runCodex(data, 0, tt.mode) })
			hso := decodeHookOutput(t, out).HookSpecificOutput
			if hso == nil || hso.PermissionDecision != "deny" {
				t.Fatalf("terminal %s output=%q want deny", tt.tool, out)
			}
			if !strings.HasPrefix(hso.PermissionDecisionReason, localizationTerminalDenyReason) {
				t.Fatalf("terminal reason=%q want prefix %q", hso.PermissionDecisionReason, localizationTerminalDenyReason)
			}
			if !strings.Contains(hso.PermissionDecisionReason, finalResponse) {
				t.Fatalf("terminal reason omitted retained response: %q", hso.PermissionDecisionReason)
			}
		})
	}
}

func TestRunCodexPreToolUseTerminalGateAcceptsAnyToolInputShape(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "codex-terminal-input-shapes", "prompt-1", t.TempDir())
	if !markLocalizationTerminalWithStrength(identity, localizationTerminalContractV2, false, "Use retained evidence.") {
		t.Fatal("mark enforceable terminal localization")
	}

	for _, tt := range []struct {
		name  string
		input any
	}{
		{name: "null", input: nil},
		{name: "scalar", input: "freeform input"},
		{name: "array", input: []any{"one", 2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := mustJSON(t, map[string]any{
				"hook_event_name": "PreToolUse",
				"tool_name":       "custom_local_tool",
				"tool_input":      tt.input,
				"session_id":      identity.SessionID,
				"prompt_id":       identity.PromptID,
				"agent_id":        identity.AgentID,
				"cwd":             identity.CWD,
			})
			out := captureHookStdout(t, func() { runCodex(data, 0) })
			hso := decodeHookOutput(t, out).HookSpecificOutput
			if hso == nil || hso.PermissionDecision != "deny" {
				t.Fatalf("terminal input %T output=%q want deny", tt.input, out)
			}
		})
	}
}

func TestRunCodexPreToolUseAdvisoryTerminalPreservesContractOperations(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "codex-advisory-contract", "prompt-1", t.TempDir())
	if !markLocalizationTerminalWithStrength(identity, localizationTerminalContractV2, true, "Advisory localization.") {
		t.Fatal("mark advisory terminal localization")
	}

	for _, tool := range []string{gortexMCPToolPrefix + "change", gortexCodexMCPToolPrefix + "change"} {
		t.Run(tool, func(t *testing.T) {
			data := preToolPayload(t, tool, "tool-1", identity, map[string]any{"operation": "impact"})
			if out := captureHookStdout(t, func() { runCodex(data, 0) }); out != "" {
				t.Fatalf("advisory terminal blocked non-navigation contract operation %s: %q", tool, out)
			}
		})
	}
}

func TestRunCodexPreToolUseWithoutTerminalPreservesBuiltins(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "codex-no-terminal", "prompt-1", t.TempDir())
	for _, tool := range []string{"apply_patch", "update_plan", "view_image", gortexMCPToolPrefix + "change"} {
		t.Run(tool, func(t *testing.T) {
			data := preToolPayload(t, tool, "tool-1", identity, map[string]any{})
			if out := captureHookStdout(t, func() { runCodex(data, 0) }); out != "" {
				t.Fatalf("non-terminal operation %s emitted %q", tool, out)
			}
		})
	}
}

func TestRunCodexPreToolUseBashSoftAdditionalContext(t *testing.T) {
	oldProbe := grepProbe
	grepProbe = func(string, string, time.Duration) ([]grepSymbolHit, error) {
		return nil, errDaemonUnreachable
	}
	t.Cleanup(func() { grepProbe = oldProbe })

	data := codexBashPayload("rg Foo")
	out := captureStdout(t, func() {
		withStdin(t, data, func() { RunCodex(0) })
	})
	if out == "" {
		t.Fatal("expected Codex Bash PreToolUse guidance, got empty output")
	}
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil {
		t.Fatalf("missing hookSpecificOutput: %s", out)
	}
	hso := dec.HookSpecificOutput
	if hso.HookEventName != "PreToolUse" {
		t.Fatalf("hookEventName=%q want PreToolUse", hso.HookEventName)
	}
	if !strings.Contains(hso.AdditionalContext, "Do not Grep indexed source") ||
		!strings.Contains(hso.AdditionalContext, `search(operation:"symbols"`) {
		t.Fatalf("additionalContext missing graph guidance: %q", hso.AdditionalContext)
	}
	if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
		t.Fatalf("Codex soft nudge must not deny: %#v", hso)
	}
}

func TestRunCodexPreToolUseGortexMCPReadSoftAdditionalContext(t *testing.T) {
	withForceCompress(t, false)
	tests := []struct {
		name  string
		tool  string
		input string
	}{
		{
			name:  "compact read file",
			tool:  gortexCompactReadTool,
			input: `{"operation":"file","target":{"file":"internal/a.go"}}`,
		},
		{
			name:  "read file compatibility",
			tool:  gortexReadFileTool,
			input: `{"path":"internal/a.go"}`,
		},
		{
			name:  "editing context compatibility",
			tool:  gortexEditingContextTool,
			input: `{"path":"internal/a.go"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := codexPreToolPayload(tt.tool, tt.input)
			out := captureStdout(t, func() { runCodex(data, 0) })
			if out == "" {
				t.Fatal("expected Codex MCP read PreToolUse guidance, got empty output")
			}
			hso := decodeHookOutput(t, out).HookSpecificOutput
			if hso == nil {
				t.Fatalf("missing hookSpecificOutput: %s", out)
			}
			if hso.HookEventName != "PreToolUse" {
				t.Fatalf("hookEventName=%q want PreToolUse", hso.HookEventName)
			}
			for _, want := range []string{"compress_bodies", `search(operation:"text", query:`, "keep", "Native Gortex MCP is mandatory"} {
				if !strings.Contains(hso.AdditionalContext, want) {
					t.Fatalf("additionalContext missing %q: %q", want, hso.AdditionalContext)
				}
			}
			if strings.Contains(hso.AdditionalContext, "gortex call ") {
				t.Fatalf("MCP read advisory must not advertise CLI fallback: %q", hso.AdditionalContext)
			}
			if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
				t.Fatalf("Codex MCP read nudge must not deny: %#v", hso)
			}
		})
	}
}

func TestRunCodexPreToolUseHardDenyAndRewrite(t *testing.T) {
	data := codexPreToolPayload(gortexCompactReadTool, `{"target":{"file":"internal/a.go"}}`)

	denied := captureStdout(t, func() { runCodex(data, 0, CodexModeDeny) })
	deny := decodeHookOutput(t, denied).HookSpecificOutput
	if deny == nil || deny.PermissionDecision != "deny" || !strings.Contains(deny.PermissionDecisionReason, "compress_bodies") {
		t.Fatalf("hard-deny output=%s", denied)
	}
	if deny.UpdatedInput != nil {
		t.Fatalf("deny must not include updatedInput: %#v", deny)
	}

	rewritten := captureStdout(t, func() { runCodex(data, 0, CodexModeRewrite) })
	rewrite := decodeHookOutput(t, rewritten).HookSpecificOutput
	if rewrite == nil || rewrite.PermissionDecision != "allow" {
		t.Fatalf("rewrite output=%s", rewritten)
	}
	options, ok := rewrite.UpdatedInput["options"].(map[string]any)
	if !ok || options["compress_bodies"] != true {
		t.Fatalf("rewrite updatedInput=%#v", rewrite.UpdatedInput)
	}
	target := rewrite.UpdatedInput["target"].(map[string]any)
	if target["file"] != "internal/a.go" {
		t.Fatalf("rewrite lost target: %#v", rewrite.UpdatedInput)
	}
}

func TestRunCodexHardDenyCoversPureTextAlternation(t *testing.T) {
	oldReachable := daemonReachableFn
	daemonReachableFn = func() bool { return true }
	t.Cleanup(func() { daemonReachableFn = oldReachable })
	oldTextSearch := codexTextSearchHitFn
	codexTextSearchHitFn = func(int, string) bool { return true }
	t.Cleanup(func() { codexTextSearchHitFn = oldTextSearch })
	data := codexBashPayload(`rg 'Phase 5|world-map' .`)
	out := captureStdout(t, func() { runCodex(data, 0, CodexModeDeny) })
	hso := decodeHookOutput(t, out).HookSpecificOutput
	if hso == nil || hso.PermissionDecision != "deny" {
		t.Fatalf("pure-text alternation was not denied: %s", out)
	}
	if !strings.Contains(hso.PermissionDecisionReason, "operation `text`") {
		t.Fatalf("deny did not route to public text search: %s", hso.PermissionDecisionReason)
	}
}

func TestRunCodexHardDenyRequiresIndexedWorkspaceMatch(t *testing.T) {
	oldReachable := daemonReachableFn
	daemonReachableFn = func() bool { return true }
	t.Cleanup(func() { daemonReachableFn = oldReachable })
	oldTextSearch := codexTextSearchHitFn
	codexTextSearchHitFn = func(int, string) bool { return false }
	t.Cleanup(func() { codexTextSearchHitFn = oldTextSearch })

	noHit := captureStdout(t, func() {
		runCodex(codexBashPayload(`rg 'Phase 5|world-map' .`), 0, CodexModeDeny)
	})
	noHitHSO := decodeHookOutput(t, noHit).HookSpecificOutput
	if noHitHSO == nil || noHitHSO.PermissionDecision != "" || noHitHSO.AdditionalContext == "" {
		t.Fatalf("unmatched text search must remain advisory: %s", noHit)
	}

	oldProbe := grepProbe
	grepProbe = func(string, string, time.Duration) ([]grepSymbolHit, error) {
		return []grepSymbolHit{{Name: "Foo", FilePath: "internal/a.go", Line: 1}}, nil
	}
	t.Cleanup(func() { grepProbe = oldProbe })
	external := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","cwd":"/repo","tool_input":{"command":"rg Foo /tmp/external"}}`)
	externalOut := captureStdout(t, func() { runCodex(external, 0, CodexModeDeny) })
	externalHSO := decodeHookOutput(t, externalOut).HookSpecificOutput
	if externalHSO == nil || externalHSO.PermissionDecision != "" || externalHSO.AdditionalContext == "" {
		t.Fatalf("external search target must never be hard-denied: %s", externalOut)
	}
}

func TestRunCodexBashRewriteOnlyForSimpleIndexedCat(t *testing.T) {
	stubFileIndexScopeBy(t, func(_, path string) fileIndexStatus {
		if path == "internal/a.go" {
			return indexedStatus(3)
		}
		return indexedStatus(0)
	})

	data := codexBashPayload("cat internal/a.go")
	out := captureStdout(t, func() { runCodex(data, 0, CodexModeRewrite) })
	hso := decodeHookOutput(t, out).HookSpecificOutput
	if hso == nil || hso.PermissionDecision != "allow" {
		t.Fatalf("rewrite output=%s", out)
	}
	command, _ := hso.UpdatedInput["command"].(string)
	if !strings.Contains(command, "gortex call read --json") || strings.Contains(command, "cat internal/a.go") {
		t.Fatalf("rewritten command=%q", command)
	}

	compound := codexBashPayload("cat internal/a.go | head")
	compoundOut := captureStdout(t, func() { runCodex(compound, 0, CodexModeRewrite) })
	compoundHSO := decodeHookOutput(t, compoundOut).HookSpecificOutput
	if compoundHSO == nil || compoundHSO.UpdatedInput != nil || compoundHSO.PermissionDecision != "" {
		t.Fatalf("compound command must remain advisory: %s", compoundOut)
	}
}

func TestRunCodexPreToolUseGortexMCPReadPermissiveModeStaysAdditionalContext(t *testing.T) {
	withForceCompress(t, true)
	tests := []struct {
		name           string
		permissionMode string
	}{
		{
			name:           "auto",
			permissionMode: "auto",
		},
		{
			name:           "accept edits",
			permissionMode: "acceptEdits",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := codexPreToolPayloadWithPermission(gortexReadFileTool, `{"path":"internal/a.go"}`, tt.permissionMode)
			out := captureStdout(t, func() { runCodex(data, 0) })
			if out == "" {
				t.Fatal("expected Codex MCP read PreToolUse guidance, got empty output")
			}
			hso := decodeHookOutput(t, out).HookSpecificOutput
			if hso == nil {
				t.Fatalf("missing hookSpecificOutput: %s", out)
			}
			if !strings.Contains(hso.AdditionalContext, "compress_bodies") {
				t.Fatalf("additionalContext missing compress_bodies guidance: %q", hso.AdditionalContext)
			}
			if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
				t.Fatalf("Codex MCP read nudge must not emit permission decisions: %#v", hso)
			}
		})
	}
}

func TestRunCodexPreToolUseGortexMCPReadSilentShapes(t *testing.T) {
	withForceCompress(t, false)
	tests := []struct {
		name  string
		tool  string
		input string
	}{
		{
			name:  "compact source is already narrow",
			tool:  gortexCompactReadTool,
			input: `{"operation":"source","target":{"symbol":"internal/a.go::A"}}`,
		},
		{
			name:  "compact read compressed",
			tool:  gortexCompactReadTool,
			input: `{"operation":"file","target":{"file":"internal/a.go"},"options":{"compress_bodies":true}}`,
		},
		{
			name:  "read_file compressed",
			tool:  gortexReadFileTool,
			input: `{"path":"internal/a.go","compress_bodies":true}`,
		},
		{
			name:  "get_editing_context compressed",
			tool:  gortexEditingContextTool,
			input: `{"path":"internal/a.go","compress_bodies":true}`,
		},
		{
			name:  "read_file max lines",
			tool:  gortexReadFileTool,
			input: `{"path":"internal/a.go","max_lines":80}`,
		},
		{
			name:  "read_file max bytes",
			tool:  gortexReadFileTool,
			input: `{"path":"internal/a.go","max_bytes":4000}`,
		},
		{
			name:  "get_editing_context max tokens",
			tool:  gortexEditingContextTool,
			input: `{"path":"internal/a.go","max_tokens":2000}`,
		},
		{
			name:  "non-source file",
			tool:  gortexReadFileTool,
			input: `{"path":"README.md"}`,
		},
		{
			name:  "other Gortex MCP tool",
			tool:  gortexMCPToolPrefix + "search_symbols",
			input: `{"path":"internal/a.go"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := codexPreToolPayload(tt.tool, tt.input)
			out := captureStdout(t, func() { runCodex(data, 0) })
			if out != "" {
				t.Fatalf("expected silent no-op, got %q", out)
			}
		})
	}
}

func TestRunCodexPostToolUseBashGrepOutputAdditionalContext(t *testing.T) {
	port := stubBridge(t, nil,
		map[string]struct{ ID, Name, Kind string }{
			"internal/a.go:7": {ID: "internal/a.go::MyType", Name: "MyType", Kind: "type"},
		}, nil)

	data := codexPostBashPayload("rg -n MyType", "internal/a.go:7:type MyType struct{}\n")
	out := captureStdout(t, func() { runCodex(data, port) })
	if out == "" {
		t.Fatal("expected Codex Bash PostToolUse graph context, got empty output")
	}
	hso := decodeHookOutput(t, out).HookSpecificOutput
	if hso == nil {
		t.Fatalf("missing hookSpecificOutput: %s", out)
	}
	if hso.HookEventName != "PostToolUse" {
		t.Fatalf("hookEventName=%q want PostToolUse", hso.HookEventName)
	}
	if !strings.Contains(hso.AdditionalContext, "type MyType") {
		t.Fatalf("additionalContext missing enclosing symbol: %q", hso.AdditionalContext)
	}
	if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
		t.Fatalf("Codex PostToolUse enrichment must not deny: %#v", hso)
	}
}

func TestRunCodexPostToolUseSuppressUsesSupportedReplacement(t *testing.T) {
	port := stubBridge(t, nil,
		map[string]struct{ ID, Name, Kind string }{
			"internal/a.go:7": {ID: "internal/a.go::MyType", Name: "MyType", Kind: "type"},
		}, nil)
	data := codexPostBashPayload("rg -n MyType", "internal/a.go:7:type MyType struct{}\n")
	out := captureStdout(t, func() { runCodex(data, port, CodexModeSuppress) })
	decision := decodeHookOutput(t, out)
	if decision.Decision != "block" || !strings.Contains(decision.Reason, "type MyType") {
		t.Fatalf("suppress replacement output=%s", out)
	}
	if strings.Contains(out, "suppressOutput") || strings.Contains(out, "updatedMCPToolOutput") {
		t.Fatalf("must not emit Codex fields that are parsed but unsupported: %s", out)
	}
}

func TestRunCodexPostToolUseApplyPatchMutationPipeline(t *testing.T) {
	changed := `{"changed_files":["internal/a.go"],"changed_symbols":[{"id":"internal/a.go::A"}],"risk":"MEDIUM"}`
	srv := newFakeServer(map[string]string{
		"detect_changes":   changed,
		"get_test_targets": "internal/a_test.go::TestA",
		"check_guards":     "boundary layering violated",
		"contracts":        "matched: 0 pairs\norphan providers: 1\n  [repo] GET /a a.go:1\norphan consumers: 0\n",
	})
	defer srv.Close()
	payload := []byte(`{"hook_event_name":"PostToolUse","tool_name":"apply_patch","cwd":"/repo","tool_input":{"command":"*** Begin Patch"},"tool_response":"Done!"}`)
	out := captureStdout(t, func() { runCodex(payload, portFromURL(t, srv.URL), CodexModeEnrich) })
	hso := decodeHookOutput(t, out).HookSpecificOutput
	if hso == nil {
		t.Fatalf("missing apply_patch context: %s", out)
	}
	for _, want := range []string{"mutation follow-up", "internal/a.go::A", "Tests", "TestA", "Guards", "layering", "Contracts", "orphan provider"} {
		if !strings.Contains(hso.AdditionalContext, want) {
			t.Fatalf("apply_patch context missing %q: %s", want, hso.AdditionalContext)
		}
	}
	if hso.PermissionDecision != "" {
		t.Fatalf("post-mutation advisory must not deny: %#v", hso)
	}
}

func TestRunCodexPostToolUseBashReadSourceAdditionalContext(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{
			name:    "cat",
			command: "cat internal/a.go",
		},
		{
			name:    "head",
			command: "head -20 internal/a.go",
		},
		{
			name:    "tail",
			command: "tail -n 50 internal/a.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := stubBridge(t, map[string]int{"internal/a.go": 3}, nil, map[string]int{"internal/a.go": 2})

			data := codexPostBashPayload(tt.command, "package internal\n")
			out := captureStdout(t, func() { runCodex(data, port) })
			if out == "" {
				t.Fatal("expected Codex Bash PostToolUse Read graph context, got empty output")
			}
			hso := decodeHookOutput(t, out).HookSpecificOutput
			if hso == nil {
				t.Fatalf("missing hookSpecificOutput: %s", out)
			}
			if hso.HookEventName != "PostToolUse" {
				t.Fatalf("hookEventName=%q want PostToolUse", hso.HookEventName)
			}
			if !strings.Contains(hso.AdditionalContext, "Graph footprint for internal/a.go") {
				t.Fatalf("additionalContext missing file footprint: %q", hso.AdditionalContext)
			}
			if !strings.Contains(hso.AdditionalContext, "3 indexed symbol(s)") {
				t.Fatalf("additionalContext missing symbol count: %q", hso.AdditionalContext)
			}
			if !strings.Contains(hso.AdditionalContext, "2 file(s) import this one") {
				t.Fatalf("additionalContext missing importer count: %q", hso.AdditionalContext)
			}
			if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
				t.Fatalf("Codex PostToolUse enrichment must not deny: %#v", hso)
			}
		})
	}
}

func TestRunCodexPostToolUseBashFindNameFileListAdditionalContext(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		response string
		indexed  map[string]int
		want     []string
	}{
		{
			name:     "find name",
			command:  `find . -name "*.go"`,
			response: "internal/hooks/codex.go\ninternal/hooks/codex_test.go\nREADME.md\n",
			indexed: map[string]int{
				"internal/hooks/codex.go":      4,
				"internal/hooks/codex_test.go": 9,
			},
			want: []string{
				"Indexed 2/3 Glob match(es)",
				"internal/hooks/codex_test.go",
				"9 symbol(s)",
				"internal/hooks/codex.go",
				"4 symbol(s)",
			},
		},
		{
			name:     "find iname",
			command:  `find internal -iname "*hook*.go"`,
			response: "internal/hooks/codex.go\ninternal/hooks/posttooluse.go\n",
			indexed: map[string]int{
				"internal/hooks/codex.go":       4,
				"internal/hooks/posttooluse.go": 7,
			},
			want: []string{
				"Indexed 2/2 Glob match(es)",
				"internal/hooks/posttooluse.go",
				"7 symbol(s)",
				"internal/hooks/codex.go",
				"4 symbol(s)",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := stubBridge(t, tt.indexed, nil, nil)

			data := codexPostBashPayload(tt.command, tt.response)
			out := captureStdout(t, func() { runCodex(data, port) })
			if out == "" {
				t.Fatal("expected Codex Bash PostToolUse Glob graph context, got empty output")
			}
			hso := decodeHookOutput(t, out).HookSpecificOutput
			if hso == nil {
				t.Fatalf("missing hookSpecificOutput: %s", out)
			}
			if hso.HookEventName != "PostToolUse" {
				t.Fatalf("hookEventName=%q want PostToolUse", hso.HookEventName)
			}
			for _, want := range tt.want {
				if !strings.Contains(hso.AdditionalContext, want) {
					t.Fatalf("additionalContext missing %q: %q", want, hso.AdditionalContext)
				}
			}
			if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
				t.Fatalf("Codex PostToolUse enrichment must not deny: %#v", hso)
			}
		})
	}
}

func TestRunCodexPostToolUseBashCommandShapes(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		response string
		want     string
	}{
		{
			name:     "grep with no path line output stays quiet",
			command:  "grep -rn handleFoo .",
			response: "no matches\n",
		},
		{
			name:     "piped grep is filter not search",
			command:  "go test ./... | grep FAIL",
			response: "pkg/x_test.go:12: FAIL\n",
		},
		{
			name:     "unindexed find name list stays quiet",
			command:  `find . -name "Handler*"`,
			response: "unindexed/handler.go\n",
		},
		{
			name:     "unindexed cat source output stays quiet",
			command:  "cat /repo/handler.go",
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:     "unsupported sed range read stays quiet",
			command:  `sed -n '1,80p' /repo/handler.go`,
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:     "sed source read is enriched",
			command:  `sed -n '1,20p' internal/a.go`,
			response: "package hooks\n",
			want:     "Graph footprint for internal/a.go",
		},
		{
			name:     "unsupported awk source scan stays quiet",
			command:  `awk 'NR>=1 && NR<=80 {print}' /repo/handler.go`,
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:     "awk unbounded source read stays quiet",
			command:  `awk '{print}' internal/a.go`,
			response: "package hooks\n",
		},
		{
			name:     "awk bounded source read is enriched",
			command:  `awk 'NR>=1 && NR<=20 {print}' internal/a.go`,
			response: "package hooks\n",
			want:     "Graph footprint for internal/a.go",
		},
		{
			name:     "ls file list is enriched",
			command:  "ls /repo",
			response: "internal/a.go\n",
			want:     "Glob match(es)",
		},
		{
			name:     "fd file list is enriched",
			command:  `fd '\.go$' internal`,
			response: "internal/a.go\n",
			want:     "Glob match(es)",
		},
		{
			name:     "tree stays quiet",
			command:  "tree internal",
			response: "internal/a.go\n",
		},
		{
			name:     "tree full-path list is enriched",
			command:  "tree -fi internal",
			response: "internal/a.go\n",
			want:     "Glob match(es)",
		},
		{
			name:     "git ls-files is enriched",
			command:  "git ls-files '*.go'",
			response: "internal/a.go\n",
			want:     "Glob match(es)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := stubBridge(t, map[string]int{"internal/a.go": 3},
				map[string]struct{ ID, Name, Kind string }{
					"internal/a.go:7": {ID: "internal/a.go::MyType", Name: "MyType", Kind: "type"},
				}, nil)

			data := codexPostBashPayload(tt.command, tt.response)
			out := captureStdout(t, func() { runCodex(data, port) })
			if tt.want == "" {
				if out != "" {
					t.Fatalf("expected silent no-op, got %q", out)
				}
				return
			}
			if !strings.Contains(out, tt.want) {
				t.Fatalf("expected enrichment containing %q, got %q", tt.want, out)
			}
		})
	}
}

func TestRunCodexPostToolUseIgnoresNonBash(t *testing.T) {
	data := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":"internal/a.go"},"tool_response":"internal/a.go:7:type MyType struct{}\n"}`)
	out := captureStdout(t, func() { runCodex(data, 0) })
	if out != "" {
		t.Fatalf("non-Bash PostToolUse should be silent, got %q", out)
	}
}

func TestRunCodexPostToolUseMalformedJSONNoop(t *testing.T) {
	out := captureStdout(t, func() { runCodexPostToolUse([]byte(`{`), 0, CodexModeEnrich) })
	if out != "" {
		t.Fatalf("malformed JSON should be silent, got %q", out)
	}
}

func TestRunCodexUserPromptSubmitInjectsGraphContext(t *testing.T) {
	prev := userPromptProbe
	userPromptProbe = func(string, string, time.Duration) ([]grepSymbolHit, error) {
		return []grepSymbolHit{
			{Name: "AuthMiddleware", Kind: "function", FilePath: "internal/auth.go", Line: 12},
		}, nil
	}
	t.Cleanup(func() { userPromptProbe = prev })

	data := []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"codex-shape","prompt":"where is the auth middleware wired"}`)
	out := captureStdout(t, func() { runCodex(data, 0) })
	if out == "" {
		t.Fatal("expected Codex UserPromptSubmit injection, got empty output")
	}
	hso := decodeHookOutput(t, out).HookSpecificOutput
	if hso == nil {
		t.Fatalf("missing hookSpecificOutput: %s", out)
	}
	if hso.HookEventName != "UserPromptSubmit" {
		t.Fatalf("hookEventName=%q want UserPromptSubmit", hso.HookEventName)
	}
	if !strings.Contains(hso.AdditionalContext, "AuthMiddleware") {
		t.Fatalf("additionalContext missing probed symbol: %q", hso.AdditionalContext)
	}
	if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
		t.Fatalf("Codex UserPromptSubmit injection must not deny: %#v", hso)
	}
}

func TestRunCodexUserPromptSubmitSilentWhenNoHits(t *testing.T) {
	prev := userPromptProbe
	userPromptProbe = func(string, string, time.Duration) ([]grepSymbolHit, error) {
		return nil, nil
	}
	t.Cleanup(func() { userPromptProbe = prev })

	data := []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"codex-shape","prompt":"refactor the parser"}`)
	out := captureStdout(t, func() { runCodex(data, 0) })
	if out != "" {
		t.Fatalf("expected silent no-op when probe returns no hits, got %q", out)
	}
}

func codexBashPayload(command string) []byte {
	return []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","session_id":"codex-shape","tool_input":{"command":` + strconv.Quote(command) + `}}`)
}

func codexPreToolPayload(toolName string, input string) []byte {
	return []byte(`{"hook_event_name":"PreToolUse","tool_name":` + strconv.Quote(toolName) + `,"session_id":"codex-shape","tool_input":` + input + `}`)
}

func codexPreToolPayloadWithPermission(toolName string, input string, permissionMode string) []byte {
	return []byte(`{"hook_event_name":"PreToolUse","tool_name":` + strconv.Quote(toolName) + `,"session_id":"codex-shape","permission_mode":` + strconv.Quote(permissionMode) + `,"tool_input":` + input + `}`)
}

func codexPostBashPayload(command string, response string) []byte {
	return []byte(`{"hook_event_name":"PostToolUse","tool_name":"Bash","session_id":"codex-shape","tool_input":{"command":` + strconv.Quote(command) + `},"tool_response":` + strconv.Quote(response) + `}`)
}

// TestCodexCapturesTheModelHint closes the gap that left every Codex call
// either unattributed or — before the reader checked the hint's agent —
// booked against whichever model Claude Code last announced for the same
// working directory. Codex hands us the active model slug on every hook
// event; nothing was reading it.
func TestCodexCapturesTheModelHint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GORTEX_MODEL_HINT_DIR", dir)
	// SessionStart otherwise dials the real daemon socket and waits out its
	// timeout — two seconds of I/O, and enough stdout noise to bury the
	// result line. Pin the status so this stays a unit test.
	origStatus := sessionStartStatusFn
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) { return nil, errDaemonUnreachable }
	t.Cleanup(func() { sessionStartStatusFn = origStatus })

	payload := func(event, model string) []byte {
		return []byte(`{"hook_event_name":"` + event + `","cwd":"/repo","model":"` + model + `","prompt":"hi"}`)
	}

	runCodex(payload("SessionStart", "gpt-5.6-sol"), 0)

	got, ok := modelhint.Read("/repo")
	if !ok {
		t.Fatal("no hint written for a Codex session")
	}
	if got.Model != "gpt-5.6-sol" {
		t.Errorf("model = %q, want gpt-5.6-sol", got.Model)
	}
	// Must be tagged as Codex, or the reader's cross-agent guard cannot tell
	// this hint from one Claude Code wrote for the same directory.
	if !modelhint.SameClient(got.Client, "codex-mcp-client") {
		t.Errorf("client = %q, should match the codex MCP client", got.Client)
	}
	if modelhint.SameClient(got.Client, "claude-code") {
		t.Errorf("client = %q must not be adopted by a Claude session", got.Client)
	}
}

// TestCodexModelHintWriteCadence: PreToolUse fires hundreds of times a
// session, so it must not turn a 12-hour hint into a per-tool-call disk write.
// The turn- and session-scoped events are frequent enough to keep it fresh.
func TestCodexModelHintWriteCadence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GORTEX_MODEL_HINT_DIR", dir)

	captureCodexModelHint("PreToolUse", "/repo", "gpt-5.6-sol")
	if _, ok := modelhint.Read("/repo"); ok {
		t.Error("PreToolUse should not write a hint")
	}

	captureCodexModelHint("UserPromptSubmit", "/repo", "gpt-5.6-sol")
	if _, ok := modelhint.Read("/repo"); !ok {
		t.Error("UserPromptSubmit should write a hint")
	}
}

// TestCodexModelHintIgnoresEmpty keeps an older Codex that omits the field
// from clearing a good hint.
func TestCodexModelHintIgnoresEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GORTEX_MODEL_HINT_DIR", dir)

	captureCodexModelHint("SessionStart", "/repo", "gpt-5.6-sol")
	captureCodexModelHint("SessionStart", "/repo", "")

	got, ok := modelhint.Read("/repo")
	if !ok || got.Model != "gpt-5.6-sol" {
		t.Errorf("an empty model must not clobber a good hint: %+v ok=%v", got, ok)
	}
}
