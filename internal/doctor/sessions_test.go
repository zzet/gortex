package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rollout writes a Codex-shaped transcript: one JSON object per line, tool
// calls carried as function_call payloads with a namespace for MCP tools.
func rollout(t *testing.T, home, day, name string, lines []map[string]any, mod time.Time) {
	t.Helper()
	dir := filepath.Join(home, "sessions", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	var buf strings.Builder
	for _, line := range lines {
		blob, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(blob)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func call(namespace, name, args string) map[string]any {
	return map[string]any{"type": "response_item", "payload": map[string]any{
		"type": "function_call", "namespace": namespace, "name": name, "arguments": args,
	}}
}

func TestScanCodexSessionsClassifiesToolCalls(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	rollout(t, home, "2026/07/26", "rollout-a.jsonl", []map[string]any{
		{"type": "session_meta", "payload": map[string]any{
			"cwd": "/repo", "cli_version": "0.145.0", "timestamp": "2026-07-26T09:11:55Z",
			"git": map[string]any{"branch": "main"},
		}},
		{"type": "turn_context", "payload": map[string]any{"model": "gpt-5.6-sol"}},
		call("mcp__gortex", "explore", `{"operation":"task"}`),
		call("mcp__gortex", "search", `{"operation":"symbols"}`),
		call("mcp__other", "lookup", `{}`),
		call("", "exec_command", `{"command":"rg TODO ."}`),
		call("", "exec_command", `{"command":"go build ./..."}`),
	}, now)

	got := ScanCodexSessions(home, now.Add(-24*time.Hour), 10)

	if got.FilesFound != 1 || got.InWindow != 1 {
		t.Fatalf("files=%d inWindow=%d want 1/1", got.FilesFound, got.InWindow)
	}
	if got.GortexCalls != 2 {
		t.Errorf("gortex=%d want 2", got.GortexCalls)
	}
	if got.OtherMCP != 1 {
		t.Errorf("otherMCP=%d want 1 (a non-Gortex MCP call is not a shell call)", got.OtherMCP)
	}
	if got.ShellCalls != 2 {
		t.Errorf("shell=%d want 2", got.ShellCalls)
	}
	if got.ShellReads != 1 {
		t.Errorf("shellReads=%d want 1 — only the rg is a read/search", got.ShellReads)
	}
	if got.Tools["explore"] != 1 || got.Tools["search"] != 1 {
		t.Errorf("tools=%v", got.Tools)
	}
	if got.Models["gpt-5.6-sol"] != 1 {
		t.Errorf("models=%v", got.Models)
	}
	if share := got.GortexShare(); share < 49 || share > 51 {
		t.Errorf("share=%.1f want ~50", share)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].Branch != "main" || got.Sessions[0].CWD != "/repo" {
		t.Errorf("session row=%+v", got.Sessions)
	}
}

// TestScanCodexSessionsWindowAndEmpties: sessions outside the window and
// sessions with no tool calls must not enter the ratio — a week of idle
// transcripts would otherwise read as an adoption collapse.
func TestScanCodexSessionsWindowAndEmpties(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	rollout(t, home, "2026/07/26", "recent.jsonl", []map[string]any{
		call("mcp__gortex", "read", `{}`),
	}, now)
	rollout(t, home, "2026/06/01", "old.jsonl", []map[string]any{
		call("", "exec_command", `{"command":"grep -r x ."}`),
	}, now.Add(-30*24*time.Hour))
	rollout(t, home, "2026/07/26", "chatonly.jsonl", []map[string]any{
		{"type": "response_item", "payload": map[string]any{"type": "message"}},
	}, now)

	got := ScanCodexSessions(home, now.Add(-24*time.Hour), 10)

	if got.FilesFound != 3 {
		t.Errorf("filesFound=%d want 3 (every transcript on disk is counted)", got.FilesFound)
	}
	if got.InWindow != 1 {
		t.Errorf("inWindow=%d want 1", got.InWindow)
	}
	if got.ShellCalls != 0 {
		t.Errorf("shell=%d — the out-of-window session leaked in", got.ShellCalls)
	}
	if got.GortexCalls != 1 {
		t.Errorf("gortex=%d want 1", got.GortexCalls)
	}
}

// TestScanCodexSessionsSurvivesGarbage — a half-written final line is normal
// for an append-only transcript from a killed process, and must not blind the
// scan to the good lines before it.
func TestScanCodexSessionsSurvivesGarbage(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	dir := filepath.Join(home, "sessions", "2026/07/26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "torn.jsonl")
	body := `{"type":"response_item","payload":{"type":"function_call","namespace":"mcp__gortex","name":"read"}}` + "\n" +
		"not json at all\n" + `{"type":"response_item","payload":{"type":"functi`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}

	got := ScanCodexSessions(home, now.Add(-24*time.Hour), 10)
	if got.GortexCalls != 1 {
		t.Errorf("gortex=%d want 1 — the good line before the garbage still counts", got.GortexCalls)
	}
}

func TestScanCodexSessionsMissingHome(t *testing.T) {
	if got := ScanCodexSessions("", time.Now().Add(-time.Hour), 10); got.FilesFound != 0 {
		t.Errorf("empty home should yield nothing, got %+v", got)
	}
	got := ScanCodexSessions(filepath.Join(t.TempDir(), "absent"), time.Now().Add(-time.Hour), 10)
	if got.FilesFound != 0 || got.GortexShare() != -1 {
		t.Errorf("missing sessions dir should yield nothing, got %+v", got)
	}
}

// TestScanCodexSessionsLimitsListedRows keeps the report readable while still
// aggregating every session in the window.
func TestScanCodexSessionsLimitsListedRows(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	for i := 0; i < 5; i++ {
		rollout(t, home, "2026/07/26", "s"+string(rune('a'+i))+".jsonl", []map[string]any{
			call("mcp__gortex", "read", `{}`),
		}, now)
	}

	got := ScanCodexSessions(home, now.Add(-24*time.Hour), 2)
	if got.InWindow != 5 || got.GortexCalls != 5 {
		t.Errorf("aggregate should cover every session: %+v", got)
	}
	if len(got.Sessions) != 2 {
		t.Errorf("listed rows=%d want 2", len(got.Sessions))
	}
}

// claudeTranscript writes a Claude Code transcript: assistant messages whose
// content array carries tool_use parts, which is where the tool name lives.
func claudeTranscript(t *testing.T, home, slug, name string, tools []string, mod time.Time) {
	t.Helper()
	dir := filepath.Join(home, "projects", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	content := []map[string]any{}
	for _, tool := range tools {
		content = append(content, map[string]any{"type": "tool_use", "name": tool, "input": map[string]any{"command": "rg TODO ."}})
	}
	rec := map[string]any{
		"type": "assistant", "cwd": "/repo", "gitBranch": "main", "version": "2.0.0",
		"message": map[string]any{"model": "claude-sonnet-5", "content": content},
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	buf.Write(blob)
	buf.WriteByte('\n')
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestScanClaudeSessionsClassifiesToolCalls(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	claudeTranscript(t, home, "-repo", "a.jsonl", []string{
		"mcp__gortex__explore", "mcp__gortex__read",
		"mcp__other__lookup",
		"Read", "Grep", // native reads compete with graph queries
		"Bash",      // its input is a rg command, so read-shaped
		"TodoWrite", // neither
	}, now)

	got := ScanClaudeSessions(home, now.Add(-24*time.Hour), 10)

	if got.GortexCalls != 2 {
		t.Errorf("gortex=%d want 2", got.GortexCalls)
	}
	if got.OtherMCP != 1 {
		t.Errorf("otherMCP=%d want 1", got.OtherMCP)
	}
	// Read + Grep + Bash: a native Read is the same competing behaviour as a
	// shell read and belongs in the same denominator.
	if got.ShellCalls != 3 {
		t.Errorf("shell=%d want 3", got.ShellCalls)
	}
	if got.ShellReads != 3 {
		t.Errorf("shellReads=%d want 3", got.ShellReads)
	}
	if got.Tools["explore"] != 1 || got.Tools["read"] != 1 {
		t.Errorf("tools=%v — the mcp__gortex__ prefix should be stripped", got.Tools)
	}
	if got.Models["claude-sonnet-5"] != 1 {
		t.Errorf("models=%v", got.Models)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].CWD != "/repo" || got.Sessions[0].Branch != "main" {
		t.Errorf("session row=%+v", got.Sessions)
	}
}

func TestScanClaudeSessionsWindowAndEmpties(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	claudeTranscript(t, home, "-repo", "recent.jsonl", []string{"mcp__gortex__read"}, now)
	claudeTranscript(t, home, "-repo", "old.jsonl", []string{"Read"}, now.Add(-30*24*time.Hour))
	claudeTranscript(t, home, "-repo", "chat.jsonl", nil, now)

	got := ScanClaudeSessions(home, now.Add(-24*time.Hour), 10)

	if got.InWindow != 1 {
		t.Errorf("inWindow=%d want 1", got.InWindow)
	}
	if got.ShellCalls != 0 {
		t.Errorf("shell=%d — the out-of-window transcript leaked in", got.ShellCalls)
	}
	if got.GortexCalls != 1 {
		t.Errorf("gortex=%d want 1", got.GortexCalls)
	}
}

func TestScanClaudeSessionsMissingHome(t *testing.T) {
	if got := ScanClaudeSessions("", time.Now().Add(-time.Hour), 10); got.FilesFound != 0 {
		t.Errorf("empty home should yield nothing, got %+v", got)
	}
	got := ScanClaudeSessions(filepath.Join(t.TempDir(), "absent"), time.Now().Add(-time.Hour), 10)
	if got.FilesFound != 0 || got.GortexShare() != -1 {
		t.Errorf("missing projects dir should yield nothing, got %+v", got)
	}
}

func TestScanAntigravitySessionsClassifiesToolCalls(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", "conv-1", ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "transcript.jsonl")
	rec := map[string]any{
		"type":       "PLANNER_RESPONSE",
		"created_at": "2026-10-21T08:00:00Z",
		"tool_calls": []map[string]any{
			{
				"name": "call_mcp_tool",
				"args": map[string]any{
					"ServerName": `"gortex"`,
					"ToolName":   `"explore"`,
				},
			},
			{
				"name": "mcp_gortex_search",
				"args": map[string]any{},
			},
			{
				"name": "call_mcp_tool",
				"args": map[string]any{
					"ServerName": `"other"`,
					"ToolName":   `"lookup"`,
				},
			},
			{
				"name": "read_file",
				"args": map[string]any{"Path": `"/repo/main.go"`},
			},
			{
				"name": "run_command",
				"args": map[string]any{
					"CommandLine": `"rg TODO ."`,
					"Cwd":         `"/repo"`,
				},
			},
			{
				"name": "run_command",
				"args": map[string]any{
					"CommandLine": `"go test ./..."`,
					"Cwd":         `"/repo"`,
				},
			},
		},
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}

	got := ScanAntigravitySessions(home, now.Add(-24*time.Hour), 10)
	if got.FilesFound != 1 || got.InWindow != 1 {
		t.Fatalf("files=%d inWindow=%d want 1/1", got.FilesFound, got.InWindow)
	}
	if got.GortexCalls != 2 {
		t.Errorf("gortex=%d want 2", got.GortexCalls)
	}
	if got.OtherMCP != 1 {
		t.Errorf("otherMCP=%d want 1", got.OtherMCP)
	}
	if got.ShellCalls != 3 {
		t.Errorf("shell=%d want 3 (read_file + 2 run_command)", got.ShellCalls)
	}
	if got.ShellReads != 2 {
		t.Errorf("shellReads=%d want 2 (read_file + rg run_command)", got.ShellReads)
	}
	if got.Tools["explore"] != 1 || got.Tools["search"] != 1 {
		t.Errorf("tools=%v", got.Tools)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].CWD != "/repo" {
		t.Errorf("session row=%+v", got.Sessions)
	}
}
