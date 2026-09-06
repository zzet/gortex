package copilotcli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// globalEnv is newCopilotEnv switched to the user-level mode `gortex
// install` runs in — the only mode that writes skills, sub-agents and
// hooks.
func globalEnv(t *testing.T) (agents.Env, *strings.Builder) {
	t.Helper()
	env, _ := newCopilotEnv(t)
	env.Mode = agents.ModeGlobal
	log := &strings.Builder{}
	env.Stderr = log
	return env, log
}

// frontmatterKeys returns the top-level keys of a document's leading
// YAML block, in order.
func frontmatterKeys(t *testing.T, body string) []string {
	t.Helper()
	if !strings.HasPrefix(body, "---\n") {
		t.Fatalf("document has no frontmatter fence:\n%s", body)
	}
	rest := body[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("unterminated frontmatter:\n%s", body)
	}
	var keys []string
	for _, line := range strings.Split(rest[:end], "\n") {
		if line == "" {
			continue
		}
		key, _, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("frontmatter line is not a key: %q", line)
		}
		keys = append(keys, strings.TrimSpace(key))
	}
	return keys
}

// TestGlobalModeInstallsCuratedSkills pins the one user-level skills root
// the CLI still honours, and the Anthropic-spec envelope it expects.
func TestGlobalModeInstallsCuratedSkills(t *testing.T) {
	env, _ := globalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	names := CuratedSkillNames()
	if len(names) != len(agents.GlobalSkills) {
		t.Fatalf("curated pack has %d skills, want %d", len(names), len(agents.GlobalSkills))
	}
	for _, id := range names {
		path := filepath.Join(env.Home, copilotConfigDirName, "skills", id, "SKILL.md")
		body := readFile(t, path)

		keys := frontmatterKeys(t, body)
		want := []string{"name", "description"}
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Fatalf("%s frontmatter keys = %v, want %v", path, keys, want)
		}
		if !strings.Contains(body, "name: "+id+"\n") {
			t.Fatalf("%s: frontmatter name must equal the directory name", path)
		}
		// A body that still carries Claude's own fence would stack two
		// frontmatter blocks and the CLI would drop the skill.
		if n := strings.Count(body, "\n---\n"); n != 1 {
			t.Fatalf("%s: expected exactly one closing fence, got %d", path, n)
		}
	}
}

// TestProjectModeWritesNoCuratedSkills: the curated pack is
// codebase-agnostic, so `gortex init` must leave the repo (and the user's
// config home) untouched by it.
func TestProjectModeWritesNoCuratedSkills(t *testing.T) {
	env, _ := newCopilotEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(env.Home, copilotConfigDirName, "skills"),
		filepath.Join(env.Home, copilotConfigDirName, "agents"),
		filepath.Join(env.Home, copilotConfigDirName, "hooks"),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s must not be written in project mode (stat err = %v)", dir, err)
		}
	}
}

// TestGeneratedSkillsLandFlatUnderGithub pins the repo-level layout:
// .github/skills/<DirName>/SKILL.md with no `generated/` level. DirName
// already carries the gortex- prefix, and the curated pack lives
// elsewhere, so there is nothing here to disambiguate against.
func TestGeneratedSkillsLandFlatUnderGithub(t *testing.T) {
	env, _ := newCopilotEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	path := filepath.Join(env.Root, ".github", "skills", "gortex-stub", "SKILL.md")
	if got := readFile(t, path); got != env.GeneratedSkills[0].Content {
		t.Fatalf("%s content = %q, want the generated body verbatim", path, got)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".github", "skills", "generated")); !os.IsNotExist(err) {
		t.Errorf("generated skills must be flat, no generated/ level (stat err = %v)", err)
	}
}

// TestGeneratedSkillWithInvalidNameIsSkipped: the CLI rejects a
// non-kebab-case skill name silently, so a bad DirName must be refused
// loudly here rather than installed and never seen.
func TestGeneratedSkillWithInvalidNameIsSkipped(t *testing.T) {
	env, _ := newCopilotEnv(t)
	log := &strings.Builder{}
	env.Stderr = log
	env.GeneratedSkills = append(env.GeneratedSkills, agents.GeneratedSkill{
		CommunityID: "bad", Label: "Bad_Name", DirName: "Gortex_Bad_Name",
		Content: "---\nname: bad\n---\n",
	})

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".github", "skills", "Gortex_Bad_Name")); !os.IsNotExist(err) {
		t.Fatalf("invalid skill name was installed (stat err = %v)", err)
	}
	if !strings.Contains(log.String(), "Gortex_Bad_Name") {
		t.Fatalf("expected a warning naming the skipped skill, got:\n%s", log)
	}
	for _, f := range res.Files {
		if strings.Contains(f.Path, "Gortex_Bad_Name") {
			t.Fatalf("Apply reported a write for a skipped skill: %+v", f)
		}
	}
}

// TestCuratedSkillIsNeverOverwritten: a user who tuned a playbook keeps
// that work across re-installs.
func TestCuratedSkillIsNeverOverwritten(t *testing.T) {
	env, _ := globalEnv(t)
	path := filepath.Join(env.Home, copilotConfigDirName, "skills", "gortex-explore", "SKILL.md")
	const custom = "---\nname: gortex-explore\ndescription: mine\n---\nmy own steps\n"
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
		t.Fatalf("customised skill was overwritten:\n%s", got)
	}
}

// profileSubset is the three-skill shape an instruction profile like
// `localization` narrows the pack to.
var profileSubset = []string{"gortex-explore", "gortex-guide", "gortex-debug"}

// installedSkillIDs lists the skill directories that still hold a
// SKILL.md. Directory presence alone is not the question a profile
// switch asks — what the CLI loads is.
func installedSkillIDs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read dir %s: %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), skillFileName)); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// TestSyncSkillsNarrowsToProfileSubset is the point of the whole
// exercise: `gortex instructions switch localization` must leave the
// Copilot CLI holding three playbooks, not twenty-one. Before this
// existed, only Claude Code's picker shrank.
func TestSyncSkillsNarrowsToProfileSubset(t *testing.T) {
	env, _ := globalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	root := filepath.Join(env.Home, copilotConfigDirName, skillsDirName)
	if got := len(installedSkillIDs(t, root)); got != len(CuratedSkillNames()) {
		t.Fatalf("install left %d skills, want the full pack (%d)", got, len(CuratedSkillNames()))
	}

	if _, err := SyncSkills(env.Stderr, env.Home, profileSubset, agents.ApplyOpts{}); err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	want := append([]string(nil), profileSubset...)
	sort.Strings(want)
	if got := installedSkillIDs(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after the switch %v remain, want %v", got, want)
	}
}

// TestSyncSkillsKeepsCustomisedSkill: a profile is not worth a user's
// edits. An out-of-profile playbook the user rewrote stays byte for
// byte, and the switch warns instead of silently widening the surface.
func TestSyncSkillsKeepsCustomisedSkill(t *testing.T) {
	env, _ := globalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	path := filepath.Join(env.Home, copilotConfigDirName, skillsDirName, "gortex-rename", skillFileName)
	const mine = "---\nname: gortex-rename\ndescription: mine\n---\nmy own steps\n"
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	log := &strings.Builder{}
	if _, err := SyncSkills(log, env.Home, profileSubset, agents.ApplyOpts{}); err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	if got := readFile(t, path); got != mine {
		t.Fatalf("customised skill was deleted or rewritten:\n%s", got)
	}
	if !strings.Contains(log.String(), "gortex-rename") {
		t.Fatalf("expected a warning naming the kept skill, got:\n%s", log.String())
	}
}

// TestSyncSkillsWithoutAnInstalledRootIsANoOp: most machines run one or
// two of the supported hosts. Switching a profile must not conjure a
// Copilot skills tree on a machine that never ran `gortex install`, and
// must not fail because the tree is absent.
func TestSyncSkillsWithoutAnInstalledRootIsANoOp(t *testing.T) {
	env, _ := globalEnv(t)
	actions, err := SyncSkills(env.Stderr, env.Home, profileSubset, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("SyncSkills on an uninstalled machine: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %v, want none", actions)
	}
	if _, err := os.Stat(filepath.Join(env.Home, copilotConfigDirName, skillsDirName)); !os.IsNotExist(err) {
		t.Fatalf("a switch created the skills root (stat err = %v)", err)
	}
}

// TestGlobalApplyIsIdempotent covers every user-level surface at once:
// skills, sub-agents and the hook config must all report skips on a
// re-run.
func TestGlobalApplyIsIdempotent(t *testing.T) {
	env, _ := globalEnv(t)
	env.InstallGlobalInstructions = true
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	agentstest.AssertIdempotent(t, a, env)
}

// TestGlobalPlanEnumeratesEveryAppliedFile keeps `gortex doctor` and
// --print-config honest in the mode that writes the most files: they read
// Plan, not Apply.
func TestGlobalPlanEnumeratesEveryAppliedFile(t *testing.T) {
	env, _ := globalEnv(t)
	env.InstallGlobalInstructions = true

	plan, err := New().Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	planned := make(map[string]bool, len(plan.Files))
	for _, f := range plan.Files {
		planned[f.Path] = true
	}

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, f := range res.Files {
		if !planned[f.Path] {
			t.Errorf("Apply touched %s but Plan did not list it", f.Path)
		}
	}
	if !planned[HookConfigPath(env)] {
		t.Error("Plan must list the hook config")
	}
}
