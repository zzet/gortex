package antigravity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
	"github.com/zzet/gortex/internal/toolref"
)

// TestAntigravityRegistersMCPAndWritesKI is the acceptance test for
// the 2026 audit fix: we must now write *both* the native MCP
// config (new) and the Knowledge Item (existing). A regression to
// KI-only would silently remove the runtime tool access.
func TestAntigravityRegistersMCPAndWritesKI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	// 1. Native MCP stanzas under ~/.gemini/antigravity/mcp_config.json (IDE)
	//    and ~/.gemini/config/mcp_config.json (agy CLI / Antigravity 2.0).
	for _, mcpPath := range mcpConfigPaths(env.Home) {
		if _, err := os.Stat(mcpPath); err != nil {
			t.Fatalf("mcp_config.json missing at %s: %v", mcpPath, err)
		}
		cfg := agentstest.ReadJSON(t, mcpPath)
		servers, ok := cfg["mcpServers"].(map[string]any)
		if !ok {
			t.Fatalf("mcpServers missing in %s: %v", mcpPath, cfg)
		}
		if _, ok := servers["gortex"]; !ok {
			t.Fatalf("gortex server missing from mcpServers in %s: %v", mcpPath, servers)
		}
	}

	// 2. Knowledge Item artifacts.
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	for _, p := range []string{
		filepath.Join(kiBase, "metadata.json"),
		filepath.Join(kiBase, "artifacts", "gortex-instructions.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("KI artifact missing: %s (%v)", p, err)
		}
	}
	instructionsPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")
	instructions, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatalf("read instructions: %v", err)
	}
	text := string(instructions)
	for _, want := range []string{
		agents.InstructionsSentinel,
		"Call `explore` first",
		"change(operation:\"impact\")",
		toolref.TransportClause(),
		toolref.DegradedModeLine(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Antigravity instructions missing %q", want)
		}
	}
	for _, forbidden := range []string{"./gortex query", "facade-v1", "tools_search", "gortex call ", "daemon start"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("Antigravity instructions contain obsolete agent vocabulary %q", forbidden)
		}
	}
	agentstest.AssertIdempotent(t, a, env)
}

func TestAntigravityMigratesExactV060KI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	metadataPath := filepath.Join(kiBase, "metadata.json")
	instructionsPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")
	for path, content := range map[string]string{
		metadataPath:     v060Metadata,
		instructionsPath: v060Instructions,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for path, want := range map[string]string{
		metadataPath:     Metadata,
		instructionsPath: Instructions,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("v0.60.0 artifact %s was not migrated", path)
		}
	}
}

func TestAntigravityPreservesCustomizedKI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	metadataPath := filepath.Join(kiBase, "metadata.json")
	instructionsPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")
	custom := map[string]string{
		metadataPath:     `{"summary":"my policy"}`,
		instructionsPath: "# Keep my custom workflow\n",
	}
	for path, content := range custom {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := New().Apply(env, agents.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for path, want := range custom {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("customized artifact %s was overwritten", path)
		}
	}
}
