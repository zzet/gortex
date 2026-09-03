package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/profiles"
)

func TestEnrichTask_NoBridge_Silent(t *testing.T) {
	result := enrichTask(map[string]any{
		"description": "Audit session hook",
		"prompt":      "Look at how PreCompact works",
	}, 1) // port 1: guaranteed closed
	if result.context != "" {
		t.Errorf("expected silent result when bridge unreachable, got: %q", result.context)
	}
	if result.deny {
		t.Error("enrichTask must never deny")
	}
}

func TestEnrichTask_EmptyTask_Silent(t *testing.T) {
	result := enrichTask(map[string]any{}, 0)
	if result.context != "" || result.deny {
		t.Errorf("expected empty result for empty task, got: %+v", result)
	}
}

func TestEnrichTask_Briefing(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"graph_stats":        `{"total_nodes":610,"total_edges":3519,"by_language":{"go":500,"markdown":50,"yaml":10}}`,
		"smart_context":      "function enrichRead internal/hooks/pretooluse.go:106\nfunction runPreCompact internal/hooks/precompact.go:33",
		"get_symbol_history": "method Server.handleHook internal/mcp/tools.go:2000 (edits=2)",
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	result := enrichTask(map[string]any{
		"description": "Subagent briefing hook",
		"prompt":      "Add a PreToolUse Task matcher that injects graph orientation",
	}, port)

	if result.deny {
		t.Fatal("enrichTask must not deny Task invocations")
	}
	if result.context == "" {
		t.Fatal("expected briefing, got empty")
	}
	if !strings.Contains(result.context, "Subagent briefing") {
		t.Errorf("missing briefing header:\n%s", result.context)
	}
	if !strings.Contains(result.context, "610 nodes, 3519 edges") {
		t.Errorf("missing stats summary:\n%s", result.context)
	}
	if !strings.Contains(result.context, "enrichRead") {
		t.Errorf("missing smart_context symbols:\n%s", result.context)
	}
	if !strings.Contains(result.context, "handleHook") {
		t.Errorf("missing recent modifications:\n%s", result.context)
	}

	// Compact workflow guidance must be inlined because subagents don't see CLAUDE.md.
	for _, needle := range []string{
		"`explore`",
		"`search`",
		"`read`",
		"`relations`",
		"`trace`",
		"`change`",
		"`edit`",
		"`refactor`",
		"`capabilities`",
	} {
		if !strings.Contains(result.context, needle) {
			t.Errorf("briefing missing Gortex tool guidance for %q:\n%s", needle, result.context)
		}
	}
}

// TestEnrichTask_AlwaysIncludesToolGuidance ensures the compact workflow is
// present even when smart_context and history return nothing. Subagents must
// not be able to reach a state where they receive stats but no guidance to
// use graph tools over Read/Grep.
func TestEnrichTask_AlwaysIncludesToolGuidance(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"graph_stats": `{"total_nodes":1,"total_edges":0,"by_language":{"go":1}}`,
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	result := enrichTask(map[string]any{
		"description": "x",
		"prompt":      "y",
	}, port)
	if result.context == "" {
		t.Fatal("expected briefing")
	}
	if !strings.Contains(result.context, "MUST use Gortex MCP tools instead of Read/Grep/Glob") {
		t.Errorf("tool guidance header missing:\n%s", result.context)
	}
	if got := strings.Count(result.context, profiles.WorktreeBranchRoutingPolicy); got != 1 {
		t.Errorf("subagent briefing embeds canonical worktree policy %d times, want once", got)
	}
}

func TestEnrichTask_OnlyStats_StillBriefs(t *testing.T) {
	// smart_context and get_symbol_history may return nothing early in a session;
	// as long as graph_stats works we should still emit orientation.
	srv := newFakeServer(map[string]string{
		"graph_stats": `{"total_nodes":100,"total_edges":200,"by_language":{"go":100}}`,
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	result := enrichTask(map[string]any{
		"description": "x",
		"prompt":      "y",
	}, port)
	if result.context == "" {
		t.Fatal("expected briefing with just stats, got empty")
	}
	if !strings.Contains(result.context, "100 nodes, 200 edges") {
		t.Errorf("missing stats summary:\n%s", result.context)
	}
}

func TestPreToolUse_RoutesTask(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"graph_stats":   `{"total_nodes":1,"total_edges":0,"by_language":{"go":1}}`,
		"smart_context": "function foo internal/foo.go:1",
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	payload := map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Task",
		"tool_input": map[string]any{
			"description": "Refactor auth",
			"prompt":      "Rename SessionStore to AuthStore across the codebase",
		},
	}
	data, _ := json.Marshal(payload)

	out := captureStdout(t, func() { runPreToolUse(data, port, ModeDeny) })
	if out == "" {
		t.Fatal("expected hook output for Task")
	}

	var parsed HookOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid HookOutput JSON: %v\n%s", err, out)
	}
	if parsed.HookSpecificOutput == nil {
		t.Fatal("hookSpecificOutput missing")
	}
	if parsed.HookSpecificOutput.PermissionDecision == "deny" {
		t.Error("Task must never be denied")
	}
	if !strings.Contains(parsed.HookSpecificOutput.AdditionalContext, "Subagent briefing") {
		t.Errorf("missing subagent briefing header:\n%s", parsed.HookSpecificOutput.AdditionalContext)
	}
}
