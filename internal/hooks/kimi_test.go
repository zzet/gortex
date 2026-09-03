package hooks

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunKimiUserPromptSubmitPlainStdout(t *testing.T) {
	cwd := writeGortexProjectMarker(t, t.TempDir())
	restore := stubUserPromptProbe(t, []grepSymbolHit{
		{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42},
	}, nil)
	defer restore()

	out := captureStdout(t, func() {
		runKimi(kimiPromptPayload(cwd, "fix auth token validation"), 0, ModeDeny)
	})
	if out == "" {
		t.Fatal("expected Kimi prompt context, got empty output")
	}
	if strings.Contains(out, "hookSpecificOutput") || strings.Contains(out, "additionalContext") {
		t.Fatalf("Kimi dispatcher should emit plain stdout, got JSON-shaped output:\n%s", out)
	}
	if !strings.Contains(out, "ValidateToken") {
		t.Fatalf("missing symbol context:\n%s", out)
	}
}

func TestRunKimiUserPromptSubmitContentParts(t *testing.T) {
	root := writeKimiProjectMCP(t, t.TempDir())
	cwd := filepath.Join(root, "nested", "worktree")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	restore := stubUserPromptProbe(t, []grepSymbolHit{
		{Name: "AuthMiddleware", Kind: "type", FilePath: "internal/auth/mw.go"},
	}, nil)
	defer restore()

	out := captureStdout(t, func() {
		runKimi([]byte(`{"hook_event_name":"UserPromptSubmit","cwd":`+strconv.Quote(cwd)+`,"prompt":[{"type":"text","text":"trace auth middleware"},{"type":"image","source":"ignored"}]}`), 0, ModeDeny)
	})
	if !strings.Contains(out, "AuthMiddleware") {
		t.Fatalf("content-parts prompt did not produce context:\n%s", out)
	}
}

func TestRunKimiPreToolUseGortexReadPlainStdout(t *testing.T) {
	withForceCompress(t, true)
	cwd := writeGortexProjectMarker(t, t.TempDir())
	tests := []struct {
		name  string
		tool  string
		input string
	}{
		{
			name:  "compact read",
			tool:  gortexCompactReadTool,
			input: `{"operation":"file","target":{"file":"internal/a.go"}}`,
		},
		{
			name:  "kimi plain read_file",
			tool:  "read_file",
			input: `{"path":"internal/a.go"}`,
		},
		{
			name:  "kimi plain get_editing_context",
			tool:  "get_editing_context",
			input: `{"path":"internal/a.go"}`,
		},
		{
			name:  "prefixed read_file",
			tool:  gortexReadFileTool,
			input: `{"path":"internal/a.go"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				runKimi(kimiPreToolPayload(cwd, tt.tool, tt.input), 0, ModeDeny)
			})
			if out == "" {
				t.Fatal("expected Kimi MCP read PreToolUse guidance, got empty output")
			}
			for _, want := range []string{"compress_bodies", `search(operation:"text", query:`, "keep", "Native Gortex MCP is mandatory"} {
				if !strings.Contains(out, want) {
					t.Fatalf("stdout guidance missing %q: %q", want, out)
				}
			}
			if strings.Contains(out, "gortex call ") {
				t.Fatalf("Kimi MCP read guidance advertised CLI fallback: %q", out)
			}
			for _, notWant := range []string{"hookSpecificOutput", "additionalContext", "permissionDecision"} {
				if strings.Contains(out, notWant) {
					t.Fatalf("Kimi PreToolUse nudge must stay plain stdout, got %q", out)
				}
			}
		})
	}
}

func TestRunKimiPreToolUseGortexReadSilentShapes(t *testing.T) {
	withForceCompress(t, false)
	cwd := writeGortexProjectMarker(t, t.TempDir())
	tests := []struct {
		name  string
		cwd   string
		tool  string
		input string
	}{
		{
			name:  "compressed",
			cwd:   cwd,
			tool:  "read_file",
			input: `{"path":"internal/a.go","compress_bodies":true}`,
		},
		{
			name:  "capped",
			cwd:   cwd,
			tool:  "read_file",
			input: `{"path":"internal/a.go","max_lines":80}`,
		},
		{
			name:  "non-source file",
			cwd:   cwd,
			tool:  "read_file",
			input: `{"path":"README.md"}`,
		},
		{
			name:  "other tool",
			cwd:   cwd,
			tool:  "search_text",
			input: `{"query":"Foo"}`,
		},
		{
			name:  "outside project",
			cwd:   t.TempDir(),
			tool:  "read_file",
			input: `{"path":"internal/a.go"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				runKimi(kimiPreToolPayload(tt.cwd, tt.tool, tt.input), 0, ModeDeny)
			})
			if out != "" {
				t.Fatalf("expected silent no-op, got %q", out)
			}
		})
	}
}

func TestRunKimiNoopShapes(t *testing.T) {
	cwd := writeGortexProjectMarker(t, t.TempDir())
	restore := stubUserPromptProbe(t, nil, errors.New("should not be called"))
	defer restore()

	cases := [][]byte{
		[]byte(`{`),
		[]byte(`{"hook_event_name":"PreToolUse","cwd":` + strconv.Quote(cwd) + `,"prompt":"fix auth"}`),
		kimiPromptPayload(cwd, "/clear"),
	}
	for _, tc := range cases {
		out := captureStdout(t, func() { runKimi(tc, 0, ModeDeny) })
		if out != "" {
			t.Fatalf("expected no output for %s, got %q", tc, out)
		}
	}
}

func TestRunKimiNoopOutsideGortexProject(t *testing.T) {
	var calls int
	restore := stubUserPromptProbeFunc(t, func(string, string, time.Duration) ([]grepSymbolHit, error) {
		calls++
		return []grepSymbolHit{
			{Name: "ShouldNotAppear", Kind: "function", FilePath: "internal/nope.go"},
		}, nil
	})
	defer restore()

	out := captureStdout(t, func() {
		runKimi(kimiPromptPayload(t.TempDir(), "fix auth token validation"), 0, ModeDeny)
	})
	if out != "" {
		t.Fatalf("expected no output outside a Gortex-enabled project, got %q", out)
	}
	if calls != 0 {
		t.Fatalf("expected no daemon probe outside a Gortex-enabled project, got %d calls", calls)
	}
}

func TestKimiGortexEnabledProjectMarkers(t *testing.T) {
	root := t.TempDir()
	if kimiGortexEnabledProject(root) {
		t.Fatal("bare temp dir should not be treated as a Gortex-enabled project")
	}
	if err := os.Mkdir(filepath.Join(root, ".gortex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if kimiGortexEnabledProject(root) {
		t.Fatal("bare .gortex directory should not be enough to enable the global Kimi hook")
	}
	if err := os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte("project: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !kimiGortexEnabledProject(root) {
		t.Fatal(".gortex.yaml should mark a Gortex-enabled project")
	}
}

func kimiPromptPayload(cwd, prompt string) []byte {
	return []byte(`{"hook_event_name":"UserPromptSubmit","cwd":` + strconv.Quote(cwd) + `,"prompt":` + strconv.Quote(prompt) + `}`)
}

func kimiPreToolPayload(cwd, toolName, input string) []byte {
	return []byte(`{"hook_event_name":"PreToolUse","cwd":` + strconv.Quote(cwd) + `,"tool_name":` + strconv.Quote(toolName) + `,"tool_input":` + input + `}`)
}

func writeGortexProjectMarker(t *testing.T, dir string) string {
	t.Helper()
	gortexDir := filepath.Join(dir, ".gortex")
	if err := os.MkdirAll(gortexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gortexDir, ".gitignore"), []byte("# Gortex-managed: local index state, do not commit\n*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeKimiProjectMCP(t *testing.T, dir string) string {
	t.Helper()
	kimiDir := filepath.Join(dir, ".kimi-code")
	if err := os.MkdirAll(kimiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"mcpServers":{"gortex":{"command":"gortex","args":["mcp"]}}}`)
	if err := os.WriteFile(filepath.Join(kimiDir, "mcp.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func stubUserPromptProbe(t *testing.T, hits []grepSymbolHit, err error) func() {
	t.Helper()
	return stubUserPromptProbeFunc(t, func(string, string, time.Duration) ([]grepSymbolHit, error) {
		return hits, err
	})
}

func stubUserPromptProbeFunc(t *testing.T, fn grepProbeFn) func() {
	t.Helper()
	old := userPromptProbe
	userPromptProbe = fn
	return func() { userPromptProbe = old }
}
