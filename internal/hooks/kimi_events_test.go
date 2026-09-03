package hooks

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubDaemonTool routes callServerTool's daemon-socket fallback to a canned
// response map, so hook handlers that fan out to graph tools are exercised over
// the production transport without touching a real daemon. A missing tool name
// yields "" (unreachable/no-signal), matching how the daemon degrades.
func stubDaemonTool(t *testing.T, responses map[string]string) {
	t.Helper()
	old := callServerToolDaemonFn
	callServerToolDaemonFn = func(_, name string, _ map[string]any) string {
		return responses[name]
	}
	t.Cleanup(func() { callServerToolDaemonFn = old })
}

func TestRunKimiPreToolUseReadIndexedDenies(t *testing.T) {
	cwd := writeGortexProjectMarker(t, t.TempDir())
	old := fileIndexedFn
	fileIndexedFn = func(_, _ string) (bool, int) { return true, 7 }
	defer func() { fileIndexedFn = old }()

	out := captureStdout(t, func() {
		runKimi(kimiPreToolPayload(cwd, "Read", `{"file_path":"internal/auth/token.go"}`), 0, ModeDeny)
	})
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("expected a Kimi permission-decision deny, got: %q", out)
	}
	if !strings.Contains(out, "read(operation") {
		t.Fatalf("deny reason should name graph tools, got: %q", out)
	}
	if strings.Contains(out, "additionalContext") {
		t.Fatalf("a hard deny should not ride additionalContext, got: %q", out)
	}
}

func TestRunKimiPreToolUseReadUnindexedSoftStdout(t *testing.T) {
	cwd := writeGortexProjectMarker(t, t.TempDir())
	old := fileIndexedFn
	fileIndexedFn = func(_, _ string) (bool, int) { return false, 0 }
	defer func() { fileIndexedFn = old }()

	out := captureStdout(t, func() {
		runKimi(kimiPreToolPayload(cwd, "Read", `{"file_path":"internal/new.go"}`), 0, ModeDeny)
	})
	if strings.Contains(out, "permissionDecision") || strings.Contains(out, "hookSpecificOutput") {
		t.Fatalf("an unindexed Read should be soft plain-stdout guidance, not a deny: %q", out)
	}
	if !strings.Contains(out, "read(operation") {
		t.Fatalf("expected soft graph guidance, got: %q", out)
	}
}

func TestRunKimiPreToolUseGrepSymbolDenies(t *testing.T) {
	cwd := writeGortexProjectMarker(t, t.TempDir())
	old := grepProbe
	grepProbe = func(string, string, time.Duration) ([]grepSymbolHit, error) {
		return []grepSymbolHit{
			{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42},
		}, nil
	}
	defer func() { grepProbe = old }()

	out := captureStdout(t, func() {
		runKimi(kimiPreToolPayload(cwd, "Grep", `{"pattern":"ValidateToken"}`), 0, ModeDeny)
	})
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("expected a Grep symbol-match deny, got: %q", out)
	}
	if !strings.Contains(out, "ValidateToken") {
		t.Fatalf("deny reason should name the matched symbol, got: %q", out)
	}
}

func TestRunKimiStopEmitsDiagnostics(t *testing.T) {
	cwd := writeGortexProjectMarker(t, t.TempDir())
	stubDaemonTool(t, map[string]string{
		"detect_changes":   `{"changed_files":["internal/foo.go"],"changed_symbols":[{"id":"internal/foo.go::Foo","name":"Foo","kind":"function"}],"risk":"MEDIUM","summary":"1 symbol"}`,
		"get_test_targets": "internal/foo_test.go::TestFoo",
	})

	out := captureStdout(t, func() {
		runKimi([]byte(`{"hook_event_name":"Stop","cwd":`+strconv.Quote(cwd)+`,"stop_hook_active":false}`), 0, ModeDeny)
	})
	if strings.Contains(out, "hookSpecificOutput") {
		t.Fatalf("Kimi Stop should append plain-stdout context, got JSON: %q", out)
	}
	if !strings.Contains(out, "Working-Tree Diagnostics") || !strings.Contains(out, "TestFoo") {
		t.Fatalf("expected the diagnostics briefing with test targets, got: %q", out)
	}
}

func TestRunKimiStopSkipsWhenActive(t *testing.T) {
	cwd := writeGortexProjectMarker(t, t.TempDir())
	stubDaemonTool(t, map[string]string{
		"detect_changes": `{"changed_files":["internal/foo.go"],"changed_symbols":[{"id":"internal/foo.go::Foo","name":"Foo","kind":"function"}],"risk":"LOW","summary":"1"}`,
	})
	out := captureStdout(t, func() {
		runKimi([]byte(`{"hook_event_name":"Stop","cwd":`+strconv.Quote(cwd)+`,"stop_hook_active":true}`), 0, ModeDeny)
	})
	if out != "" {
		t.Fatalf("expected silent no-op while a Stop hook is already rerunning, got: %q", out)
	}
}

func TestRunKimiSubagentStartBriefsWithTask(t *testing.T) {
	cwd := writeGortexProjectMarker(t, t.TempDir())
	stubDaemonTool(t, map[string]string{
		"graph_stats":   `{"total_nodes":100}`,
		"smart_context": "FooHandler internal/foo.go:10",
	})

	out := captureStdout(t, func() {
		runKimi([]byte(`{"hook_event_name":"SubagentStart","cwd":`+strconv.Quote(cwd)+`,"prompt":"refactor the auth handler"}`), 0, ModeDeny)
	})
	if strings.Contains(out, "hookSpecificOutput") {
		t.Fatalf("subagent briefing must be plain stdout, got JSON: %q", out)
	}
	if !strings.Contains(out, "MUST use Gortex MCP tools") || !strings.Contains(out, "`explore`") {
		t.Fatalf("expected the compact mandatory workflow, got: %q", out)
	}
	if !strings.Contains(out, "FooHandler") {
		t.Fatalf("expected smart_context symbols in the briefing, got: %q", out)
	}
}

func TestRunKimiSubagentStartFallbackWithoutTask(t *testing.T) {
	cwd := writeGortexProjectMarker(t, t.TempDir())
	stubDaemonTool(t, map[string]string{}) // bridge answers nothing

	out := captureStdout(t, func() {
		runKimi([]byte(`{"hook_event_name":"SubagentStart","cwd":`+strconv.Quote(cwd)+`}`), 0, ModeDeny)
	})
	if !strings.Contains(out, "MUST use Gortex MCP tools") || !strings.Contains(out, "`change`") {
		t.Fatalf("expected the static compact fallback briefing, got: %q", out)
	}
}

func TestRunKimiSubagentStartOutsideProjectSilent(t *testing.T) {
	stubDaemonTool(t, map[string]string{"graph_stats": `{"total_nodes":100}`})
	out := captureStdout(t, func() {
		runKimi([]byte(`{"hook_event_name":"SubagentStart","cwd":`+strconv.Quote(t.TempDir())+`,"prompt":"do work"}`), 0, ModeDeny)
	})
	if out != "" {
		t.Fatalf("expected silent no-op outside a Gortex-enabled project, got: %q", out)
	}
}
