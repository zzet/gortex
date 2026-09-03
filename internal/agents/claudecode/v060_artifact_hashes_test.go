package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/profiles"
)

func TestV060ArtifactHashCatalogCoversShippedArtifacts(t *testing.T) {
	for name := range GlobalSkills {
		if v060GlobalSkillHashes[name] == "" {
			t.Errorf("missing v0.60.0 skill fingerprint for %s", name)
		}
	}
	for name := range SlashCommands {
		if v060SlashCommandHashes[name] == "" {
			t.Errorf("missing v0.60.0 command fingerprint for %s", name)
		}
	}
	for name := range SubAgents {
		if v060SubAgentHashes[name] == "" {
			t.Errorf("missing v0.60.0 sub-agent fingerprint for %s", name)
		}
	}
}

func TestPreWorktreeArtifactHashCatalogMatchesShippedBodies(t *testing.T) {
	policy := "\n## Worktree and branch routing\n\n" + profiles.WorktreeBranchRoutingPolicy
	check := func(kind string, current, hashes map[string]string) {
		t.Helper()
		if len(current) != len(hashes) {
			t.Errorf("%s hashes = %d, want %d", kind, len(hashes), len(current))
		}
		for name, body := range current {
			old := strings.Replace(body, policy, "", 1)
			if old == body {
				t.Errorf("%s/%s: policy block absent", kind, name)
				continue
			}
			if got := artifactHash([]byte(old)); hashes[name] != got {
				t.Errorf("%s/%s hash = %q, want %q", kind, name, hashes[name], got)
			}
		}
	}
	check("commands", SlashCommands, preWorktreeSlashCommandHashes)
	check("subagents", SubAgents, preWorktreeSubAgentHashes)
}

func TestWriteAgentArtifactMigratesRealPreWorktreeBodiesSafely(t *testing.T) {
	policy := "\n## Worktree and branch routing\n\n" + profiles.WorktreeBranchRoutingPolicy
	for _, tc := range []struct {
		name, current string
		hashes        []string
	}{
		{"command", SlashCommands["gortex-explore.md"], []string{v060SlashCommandHashes["gortex-explore.md"], preWorktreeSlashCommandHashes["gortex-explore.md"]}},
		{"subagent", SubAgents["gortex-search.md"], []string{v060SubAgentHashes["gortex-search.md"], preWorktreeSubAgentHashes["gortex-search.md"]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := strings.Replace(tc.current, policy, "", 1)
			path := filepath.Join(t.TempDir(), tc.name+".md")
			if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
				t.Fatal(err)
			}
			action, err := writeAgentArtifact(nil, path, tc.current, tc.hashes, agents.ApplyOpts{DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			if action.Action != agents.ActionWouldMerge {
				t.Fatalf("dry-run action = %s, want %s: %+v", action.Action, agents.ActionWouldMerge, action)
			}
			if got, _ := os.ReadFile(path); string(got) != old {
				t.Fatal("dry-run changed historical artifact")
			}
			action, err = writeAgentArtifact(nil, path, tc.current, tc.hashes, agents.ApplyOpts{})
			if err != nil || action.Action != agents.ActionMerge {
				t.Fatalf("migration = %+v, %v", action, err)
			}
			custom := old + "x"
			if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
				t.Fatal(err)
			}
			action, err = writeAgentArtifact(nil, path, tc.current, tc.hashes, agents.ApplyOpts{Force: true})
			if err != nil || action.Action != agents.ActionSkip || action.Reason != "customised" {
				t.Fatalf("custom = %+v, %v", action, err)
			}
			if got, _ := os.ReadFile(path); string(got) != custom {
				t.Fatalf("custom overwritten: %q", got)
			}
		})
	}
}

func TestInstallGlobalClaudeArtifactsMigratesEveryPreWorktreeBody(t *testing.T) {
	home := t.TempDir()
	policy := "\n## Worktree and branch routing\n\n" + profiles.WorktreeBranchRoutingPolicy
	seed := func(dir string, current map[string]string) {
		t.Helper()
		for name, body := range current {
			path := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.Replace(body, policy, "", 1)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	root := userClaudeConfigDir(home)
	seed(filepath.Join(root, "commands"), SlashCommands)
	seed(filepath.Join(root, "agents"), SubAgents)
	commands, err := installGlobalSlashCommands(nil, home, agents.ApplyOpts{})
	if err != nil {
		t.Fatal(err)
	}
	subagents, err := installGlobalSubAgents(nil, home, agents.ApplyOpts{})
	if err != nil {
		t.Fatal(err)
	}
	assert := func(dir string, current map[string]string, actions []agents.FileAction) {
		t.Helper()
		if len(actions) != len(current) {
			t.Fatalf("actions = %d, want %d", len(actions), len(current))
		}
		for _, action := range actions {
			if action.Action != agents.ActionMerge {
				t.Errorf("%s action = %s, want merge", action.Path, action.Action)
			}
		}
		for name, want := range current {
			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("%s did not migrate", name)
			}
		}
	}
	assert(filepath.Join(root, "commands"), SlashCommands, commands)
	assert(filepath.Join(root, "agents"), SubAgents, subagents)
}

func BenchmarkInstallGlobalClaudeArtifactsTwentyTwoUnchanged(b *testing.B) {
	home := b.TempDir()
	if _, err := installGlobalSlashCommands(nil, home, agents.ApplyOpts{}); err != nil {
		b.Fatal(err)
	}
	if _, err := installGlobalSubAgents(nil, home, agents.ApplyOpts{}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := installGlobalSlashCommands(nil, home, agents.ApplyOpts{}); err != nil {
			b.Fatal(err)
		}
		if _, err := installGlobalSubAgents(nil, home, agents.ApplyOpts{}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestWriteAgentArtifactMigratesOnlyExactV060Bytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	v060 := []byte("exact old Gortex artifact\n")
	current := "new public-tool artifact\n"
	if err := os.WriteFile(path, v060, 0o644); err != nil {
		t.Fatal(err)
	}

	action, err := writeAgentArtifact(nil, path, current, []string{"unused", artifactHash(v060)}, agents.ApplyOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if action.Action != agents.ActionMerge {
		t.Fatalf("v0.60.0 action = %s, want merge", action.Action)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != current {
		t.Fatalf("v0.60.0 artifact was not migrated: %q", got)
	}

	custom := []byte("user customized policy\n")
	if err := os.WriteFile(path, custom, 0o644); err != nil {
		t.Fatal(err)
	}
	action, err = writeAgentArtifact(nil, path, current, []string{artifactHash(v060)}, agents.ApplyOpts{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if action.Action != agents.ActionSkip || action.Reason != "customised" {
		t.Fatalf("custom action = %+v, want customized skip", action)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(custom) {
		t.Fatalf("custom artifact was overwritten: %q", got)
	}
}
