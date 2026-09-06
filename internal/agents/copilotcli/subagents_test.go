package copilotcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// TestGlobalModeInstallsSubAgents pins the discovery-critical filename
// (NAME.agent.md, not NAME.md) and the envelope.
func TestGlobalModeInstallsSubAgents(t *testing.T) {
	env, _ := globalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	names := SubAgentNames()
	if len(names) != len(agents.SubAgents) {
		t.Fatalf("rendered %d sub-agents, want %d", len(names), len(agents.SubAgents))
	}
	for _, id := range names {
		path := filepath.Join(env.Home, copilotConfigDirName, "agents", id+".agent.md")
		body := readFile(t, path)

		keys := frontmatterKeys(t, body)
		want := []string{"name", "description"}
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Fatalf("%s frontmatter keys = %v, want %v", path, keys, want)
		}
		// Claude's allowlist is spelled in mcp__gortex__* — a vocabulary
		// the CLI does not resolve. An unrecognised tools entry grants
		// nothing, so the key must be absent rather than mistranslated.
		if strings.Contains(body, "mcp__gortex__") {
			t.Fatalf("%s leaked Claude's mcp__gortex__ tool names:\n%s", path, body)
		}
		if !strings.Contains(body, "name: "+id+"\n") {
			t.Fatalf("%s: frontmatter name must equal the file's base name", path)
		}
	}

	// A plain .md in the agents directory is invisible to the CLI.
	if _, err := os.Stat(filepath.Join(env.Home, copilotConfigDirName, "agents", "gortex-search.md")); !os.IsNotExist(err) {
		t.Errorf("wrote a bare .md sub-agent; the CLI only loads *.agent.md (stat err = %v)", err)
	}
}

// TestProjectModeWritesNoRepoSubAgents: a home agent shadows a repo agent
// of the same name, so a committed .github/agents copy would be
// permanently dead and pushed at every teammate.
func TestProjectModeWritesNoRepoSubAgents(t *testing.T) {
	env, _ := newCopilotEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".github", "agents")); !os.IsNotExist(err) {
		t.Errorf(".github/agents must not be written (stat err = %v)", err)
	}
}

// TestSubAgentIsNeverOverwritten: a tuned description or a hand-added
// tools allowlist survives re-installs.
func TestSubAgentIsNeverOverwritten(t *testing.T) {
	env, _ := globalEnv(t)
	path := filepath.Join(env.Home, copilotConfigDirName, "agents", "gortex-search.agent.md")
	const custom = "---\nname: gortex-search\ndescription: mine\n---\nmy own instructions\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readFile(t, path); got != custom {
		t.Fatalf("customised sub-agent was overwritten:\n%s", got)
	}
}
