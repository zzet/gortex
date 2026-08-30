package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
	"github.com/zzet/gortex/internal/profiles"
)

func TestWorkflowRuleUsesCompactMCPTools(t *testing.T) {
	for _, required := range []string{"explore", "search", "read", "relations", "trace", "change", "edit", "refactor", "capabilities"} {
		if !strings.Contains(workflowRuleBody, required) {
			t.Errorf("workflow rule missing compact tool %q", required)
		}
	}
	for _, legacy := range []string{"smart_context", "search_symbols", "get_symbol_source", "find_usages", "get_callers", "verify_change", "read_file"} {
		if strings.Contains(workflowRuleBody, legacy) {
			t.Errorf("workflow rule contains legacy MCP tool %q", legacy)
		}
	}
	if strings.Contains(workflowRuleBody, "facade-v1") {
		t.Error("workflow rule should not expose an implementation version")
	}
	if got := strings.Count(workflowRuleBody, profiles.WorktreeBranchRoutingPolicy); got != 1 {
		t.Fatalf("workflow rule embeds canonical worktree policy %d times, want once", got)
	}
}

// TestCursorCreatesMergesAndSkips covers the three behavioural
// phases every adapter must honour:
//   - initial create on an empty project (via .cursor/ sentinel)
//   - re-run is a no-op (idempotent)
//   - merge into an existing mcp.json preserves user keys
func TestCursorCreatesMergesAndSkips(t *testing.T) {
	env, _ := agentstest.NewEnv(t)

	// Sentinel: create .cursor/ so Detect reports true. Without
	// this the adapter would skip with "not detected".
	if err := os.MkdirAll(filepath.Join(env.Root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := New()

	// Phase 1 — create.
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Detected {
		t.Fatal("expected Detected=true after creating .cursor/")
	}
	if !res.Configured {
		t.Fatal("expected Configured=true after first apply")
	}
	// Three creates: .cursor/mcp.json, gortex-workflow.mdc (always-on MCP
	// guidance), and gortex-communities.mdc (routing from stub SkillsRouting).
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 3})
	mcp := agentstest.ReadJSON(t, filepath.Join(env.Root, ".cursor", "mcp.json"))
	servers := mcp["mcpServers"].(map[string]any)
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex server missing: %v", servers)
	}

	// Phase 2 — idempotent re-run.
	agentstest.AssertIdempotent(t, a, env)
}

// TestCursorGlobalWritesProxyEntry confirms that user-level installs
// (`gortex install`, env.Mode == ModeGlobal) emit the daemon-proxy
// entry — never the legacy `--index . --watch` shape that resolves
// against the launch cwd and fails Cursor's global-config handshake.
// See gortexhq/gortex#19.
func TestCursorGlobalWritesProxyEntry(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	// Sentinel: ~/.cursor exists so Detect succeeds in global mode.
	if err := os.MkdirAll(filepath.Join(env.Home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true for global install")
	}

	mcp := agentstest.ReadJSON(t, filepath.Join(env.Home, ".cursor", "mcp.json"))
	servers := mcp["mcpServers"].(map[string]any)
	entry, ok := servers["gortex"].(map[string]any)
	if !ok {
		t.Fatalf("gortex entry missing in global mcp.json: %v", servers)
	}
	args, _ := entry["args"].([]any)
	if len(args) == 0 || args[0] != "mcp" {
		t.Fatalf("args[0] = %v, want \"mcp\"; full args=%v", args, args)
	}
	// Canonical shape is bare ["mcp"] — no --index, no --proxy.
	if len(args) != 1 {
		t.Errorf("global cursor entry must emit canonical [mcp], got %v", args)
	}
	for _, a := range args {
		if s, _ := a.(string); s == "--index" || s == "--proxy" {
			t.Errorf("global cursor entry must not pass %q: %v", s, args)
		}
	}
}

// TestCursorGlobalMigratesLegacyIndexEntry exercises the migration
// path. A user upgrading from a version that wrote the legacy
// `--index . --watch` shape should get their global config rewritten
// to the daemon-proxy form on the next `gortex install`, without
// having to pass --force.
func TestCursorGlobalMigratesLegacyIndexEntry(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	cursorDir := filepath.Join(env.Home, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed the legacy entry that issue 19 reported in the wild.
	mcpPath := filepath.Join(cursorDir, "mcp.json")
	agentstest.WriteJSON(t, mcpPath, map[string]any{
		"mcpServers": map[string]any{
			"gortex": map[string]any{
				"command": "gortex",
				"args":    []any{"mcp", "--index", ".", "--watch"},
				"env":     map[string]any{"GORTEX_INDEX_WORKERS": "8"},
			},
		},
	})

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true after migration")
	}

	after := agentstest.ReadJSON(t, mcpPath)
	servers := after["mcpServers"].(map[string]any)
	entry := servers["gortex"].(map[string]any)
	args, _ := entry["args"].([]any)
	for _, a := range args {
		if s, _ := a.(string); s == "--index" {
			t.Fatalf("migration left legacy --index in args: %v", args)
		}
	}
}

// TestCursorGlobalPreservesUserCustomization keeps the migration
// honest: a user who pointed their global gortex entry at a custom
// wrapper script must not have it silently overwritten.
func TestCursorGlobalPreservesUserCustomization(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	cursorDir := filepath.Join(env.Home, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mcpPath := filepath.Join(cursorDir, "mcp.json")
	agentstest.WriteJSON(t, mcpPath, map[string]any{
		"mcpServers": map[string]any{
			"gortex": map[string]any{
				"command": "/opt/wrappers/my-gortex.sh",
				"args":    []any{"mcp"},
			},
		},
	})

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := agentstest.ReadJSON(t, mcpPath)
	servers := after["mcpServers"].(map[string]any)
	entry := servers["gortex"].(map[string]any)
	if entry["command"] != "/opt/wrappers/my-gortex.sh" {
		t.Fatalf("user-customized command was overwritten: %v", entry)
	}
}

// TestCursorMergeIntoExistingPreservesUserKeys confirms that the
// adapter writes alongside — not over — a user's pre-existing MCP
// server entry. This is the load-bearing property of MergeJSON.
func TestCursorMergeIntoExistingPreservesUserKeys(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	mcpPath := filepath.Join(env.Root, ".cursor", "mcp.json")
	agentstest.WriteJSON(t, mcpPath, map[string]any{
		"mcpServers": map[string]any{
			"user-server": map[string]any{"command": "user-tool"},
		},
	})

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// mcp.json pre-exists → merge; workflow + communities rules are new.
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionMerge: 1, agents.ActionCreate: 2})
	after := agentstest.ReadJSON(t, mcpPath)
	servers := after["mcpServers"].(map[string]any)
	if _, ok := servers["user-server"]; !ok {
		t.Fatalf("user-server was clobbered: %v", servers)
	}
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex missing after merge: %v", servers)
	}
}
