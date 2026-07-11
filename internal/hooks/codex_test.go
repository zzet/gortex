package hooks

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunCodexMalformedJSONNoop(t *testing.T) {
	out := captureStdout(t, func() { runCodex([]byte(`{`), 0) })
	if out != "" {
		t.Fatalf("malformed JSON should be silent, got %q", out)
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

func TestRunCodexPreToolUseBashSoftAdditionalContext(t *testing.T) {
	oldProbe := grepProbe
	grepProbe = func(string, time.Duration) ([]grepSymbolHit, error) {
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
	if !strings.Contains(hso.AdditionalContext, "PREFER graph tools over Grep") {
		t.Fatalf("additionalContext missing graph guidance: %q", hso.AdditionalContext)
	}
	if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
		t.Fatalf("Codex soft nudge must not deny: %#v", hso)
	}
}

func TestRunCodexLogsEffectivenessWithoutCommandContent(t *testing.T) {
	logPath := redirectTelemetry(t)
	stubProbe(t, nil, errDaemonUnreachable)

	data := codexBashPayload("rg 'place_edges|location_edge|normalize'")
	_ = captureStdout(t, func() { runCodex(data, 0) })

	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open telemetry: %v", err)
	}
	defer f.Close()
	var found *codexHookEffect
	s := bufio.NewScanner(f)
	for s.Scan() {
		var rec codexHookEffect
		if json.Unmarshal(s.Bytes(), &rec) == nil && rec.Kind == "codex_hook_effectiveness" {
			found = &rec
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scan telemetry: %v", err)
	}
	if found == nil {
		t.Fatal("missing Codex hook effectiveness record")
	}
	if found.Event != "PreToolUse" || found.Tool != "Bash" || !found.EmittedContext {
		t.Fatalf("unexpected effectiveness record: %#v", found)
	}
	if (found.DaemonReachability != "reachable" && found.DaemonReachability != "unreachable") || found.AlternationSegments != 3 {
		t.Fatalf("missing daemon/alternation dimensions: %#v", found)
	}
	if found.DurationMS < 0 {
		t.Fatalf("negative latency: %#v", found)
	}
}

func TestRunCodexPreToolUseGortexMCPReadSoftAdditionalContext(t *testing.T) {
	withForceCompress(t, false)
	tests := []struct {
		name string
		tool string
	}{
		{
			name: "read file",
			tool: gortexReadFileTool,
		},
		{
			name: "editing context",
			tool: gortexEditingContextTool,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := codexPreToolPayload(tt.tool, `{"path":"internal/a.go"}`)
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
			for _, want := range []string{"compress_bodies", "search_text", "keep"} {
				if !strings.Contains(hso.AdditionalContext, want) {
					t.Fatalf("additionalContext missing %q: %q", want, hso.AdditionalContext)
				}
			}
			if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
				t.Fatalf("Codex MCP read nudge must not deny: %#v", hso)
			}
		})
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
		name        string
		command     string
		response    string
		wantContext bool
		indexed     map[string]int
		want        []string
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
		name        string
		command     string
		response    string
		wantContext bool
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
			name:        "sed source read is enriched",
			command:     `sed -n '1,20p' internal/a.go`,
			response:    "package hooks\n",
			wantContext: true,
		},
		{
			name:     "unsupported awk source scan stays quiet",
			command:  `awk 'NR>=1 && NR<=80 {print}' /repo/handler.go`,
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:        "awk source read is enriched",
			command:     `awk '{print}' internal/a.go`,
			response:    "package hooks\n",
			wantContext: true,
		},
		{
			name:        "ls file list is enriched",
			command:     "ls /repo",
			response:    "internal/a.go\n",
			wantContext: true,
		},
		{
			name:        "fd file list is enriched",
			command:     `fd '\.go$' internal`,
			response:    "internal/a.go\n",
			wantContext: true,
		},
		{
			name:        "tree file list is enriched",
			command:     "tree internal",
			response:    "internal/a.go\n",
			wantContext: true,
		},
		{
			name:        "git ls-files is enriched",
			command:     "git ls-files '*.go'",
			response:    "internal/a.go\n",
			wantContext: true,
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
			if tt.wantContext {
				if out == "" {
					t.Fatal("expected graph context, got empty output")
				}
				return
			}
			if out != "" {
				t.Fatalf("expected silent no-op, got %q", out)
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
	out := captureStdout(t, func() { runCodexPostToolUse([]byte(`{`)) })
	if out != "" {
		t.Fatalf("malformed JSON should be silent, got %q", out)
	}
}

func TestRunCodexUserPromptSubmitInjectsGraphContext(t *testing.T) {
	prev := userPromptProbe
	userPromptProbe = func(string, time.Duration) ([]grepSymbolHit, error) {
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
	userPromptProbe = func(string, time.Duration) ([]grepSymbolHit, error) {
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
