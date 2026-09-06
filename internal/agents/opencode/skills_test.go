package opencode

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

// recognisedFrontmatterKeys is the whole set OpenCode reads from a
// SKILL.md. Anything else is ignored, so emitting one is dead weight the
// next reader has to research.
var recognisedFrontmatterKeys = map[string]bool{
	"name": true, "description": true, "license": true,
	"compatibility": true, "metadata": true,
}

// TestSkillNameEqualsDirectoryForWholeCorpus is the load-bearing
// assertion of this package. OpenCode drops — silently — any skill whose
// frontmatter `name` differs from its containing directory, so a single
// sampled skill proves nothing: the whole corpus has to hold.
func TestSkillNameEqualsDirectoryForWholeCorpus(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	if _, err := installCuratedPack(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("install curated pack: %v", err)
	}

	root := globalSkillsDir(env.Home)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	if len(entries) != len(agents.GlobalSkills) {
		t.Fatalf("wrote %d skill dirs, want %d", len(entries), len(agents.GlobalSkills))
	}
	for _, entry := range entries {
		dir := entry.Name()
		if !entry.IsDir() {
			t.Fatalf("%s is not a skill directory", dir)
		}
		if !skillpack.ValidID(dir) {
			t.Fatalf("skill directory %q is not kebab-case; OpenCode rejects it", dir)
		}
		front := readFrontmatter(t, filepath.Join(root, dir, skillFileName))
		if front["name"] != dir {
			t.Fatalf("skill %s: frontmatter name %q != directory %q — OpenCode drops it", dir, front["name"], dir)
		}
		if !skillpack.ValidID(front["name"]) {
			t.Fatalf("skill %s: name %q is not kebab-case", dir, front["name"])
		}
		if front["description"] == "" {
			t.Fatalf("skill %s: empty description leaves a blank entry in the picker", dir)
		}
		assertOnlyRecognisedKeys(t, dir, front)
	}
}

// TestCommandsAreNamedByFilename pins the two facts a command depends on:
// the file's base name is the command name, and the only frontmatter key
// we set is `description`.
func TestCommandsAreNamedByFilename(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	if _, err := installCuratedPack(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("install curated pack: %v", err)
	}

	root := globalCommandsDir(env.Home)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	if len(entries) != len(agents.SlashCommands) {
		t.Fatalf("wrote %d commands, want %d", len(entries), len(agents.SlashCommands))
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != commandFileExt {
			t.Fatalf("command %s: OpenCode only globs %s", name, commandFileExt)
		}
		stem := strings.TrimSuffix(name, commandFileExt)
		if !skillpack.ValidID(stem) {
			t.Fatalf("command %q would be invoked as /%s, which is not kebab-case", name, stem)
		}
		if _, ok := agents.SlashCommands[name]; !ok {
			t.Fatalf("command %s has no source in agents.SlashCommands", name)
		}
		front := readFrontmatter(t, filepath.Join(root, name))
		if front["description"] == "" {
			t.Fatalf("command %s: no description — the picker shows a blank entry", name)
		}
		assertOnlyRecognisedKeys(t, name, front)
		if front["name"] != "" {
			t.Fatalf("command %s: emitted a `name` key; OpenCode names a command by its filename", name)
		}
	}
}

// TestCuratedPackIsGlobalOnly guards the split that keeps `gortex init`
// from dropping 41 markdown files into somebody's repo.
func TestCuratedPackIsGlobalOnly(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply project: %v", err)
	}

	if _, err := os.Stat(globalSkillsDir(env.Home)); err == nil {
		t.Fatalf("project mode wrote the curated skills pack to %s", globalSkillsDir(env.Home))
	}
	if _, err := os.Stat(globalCommandsDir(env.Home)); err == nil {
		t.Fatalf("project mode wrote the curated commands pack to %s", globalCommandsDir(env.Home))
	}
	// The generated community skill IS repo-local, and flat.
	generated := filepath.Join(projectSkillsDir(env.Root), "gortex-stub", skillFileName)
	if _, err := os.Stat(generated); err != nil {
		t.Fatalf("project mode did not write the generated skill at %s: %v", generated, err)
	}
	if _, err := os.Stat(filepath.Join(projectSkillsDir(env.Root), "generated")); err == nil {
		t.Fatalf("generated skills must be flat — a `generated/` level breaks name==directory")
	}
	agentstest.AssertIdempotent(t, a, env)
}

// TestGeneratedSkillsAreProjectOnly is the mirror: `gortex install` must
// not scatter repo-derived skills into the user's config.
func TestGeneratedSkillsAreProjectOnly(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply global: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".opencode", "skills")); err == nil {
		t.Fatalf("global mode wrote repo-local skills into %s", env.Root)
	}
	if _, err := os.Stat(filepath.Join(globalSkillsDir(env.Home), "gortex-stub")); err == nil {
		t.Fatalf("global mode installed a generated community skill into the user pack")
	}
	agentstest.AssertIdempotent(t, a, env)
}

// TestGeneratedSkillWithBadDirNameIsSkipped: OpenCode rejects a non-
// kebab-case name silently, so the installer has to be the loud one.
func TestGeneratedSkillWithBadDirNameIsSkipped(t *testing.T) {
	env, buf := agentstest.NewEnv(t)
	env.GeneratedSkills = append(env.GeneratedSkills, agents.GeneratedSkill{
		CommunityID: "bad", Label: "Bad_Name", DirName: "Gortex_Bad Name",
		Content: "---\nname: whatever\n---\nbody\n",
	})
	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectSkillsDir(env.Root), "Gortex_Bad Name")); err == nil {
		t.Fatalf("wrote a skill directory OpenCode will refuse to load")
	}
	if !strings.Contains(buf.String(), "kebab-case") {
		t.Fatalf("skipping a bad skill must warn; stderr was:\n%s", buf.String())
	}
}

// TestPlanEnumeratesEveryWrittenPath: Plan is what --dry-run and doctor
// read, and neither ever calls Apply. A path Apply writes but Plan omits
// is invisible to both.
func TestPlanEnumeratesEveryWrittenPath(t *testing.T) {
	for _, mode := range []agents.Mode{agents.ModeProject, agents.ModeGlobal} {
		env, _ := agentstest.NewEnv(t)
		env.Mode = mode
		a := New()

		plan, err := a.Plan(env)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		planned := map[string]bool{}
		for _, f := range plan.Files {
			planned[f.Path] = true
		}

		res, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		var missing []string
		for _, f := range res.Files {
			if !planned[f.Path] {
				missing = append(missing, f.Path)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Fatalf("mode %v: Apply wrote paths Plan never mentioned:\n%s", mode, strings.Join(missing, "\n"))
		}
	}
}

// TestDryRunWritesNothing: --dry-run has to be safe in both modes, and
// the global mode is the one that writes an executable.
func TestDryRunWritesNothing(t *testing.T) {
	for _, mode := range []agents.Mode{agents.ModeProject, agents.ModeGlobal} {
		env, _ := agentstest.NewEnv(t)
		env.Mode = mode
		res, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true, DryRun: true})
		if err != nil {
			t.Fatalf("apply dry-run: %v", err)
		}
		if len(res.Files) == 0 {
			t.Fatalf("mode %v: dry-run reported no planned files", mode)
		}
		for _, f := range res.Files {
			if _, err := os.Stat(f.Path); err == nil {
				t.Fatalf("mode %v: dry-run wrote %s", mode, f.Path)
			}
		}
	}
}

// TestCustomisedCuratedFileIsKept: the curated pack is never clobbered,
// so a user's edit survives the next `gortex install`.
func TestCustomisedCuratedFileIsKept(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	path := filepath.Join(globalSkillsDir(env.Home), "gortex-explore", skillFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const mine = "---\nname: gortex-explore\ndescription: mine\n---\nmine\n"
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != mine {
		t.Fatalf("clobbered a customised skill:\n%s", got)
	}
}

// profileSubset is the three-skill shape an instruction profile like
// `localization` narrows the pack to.
var profileSubset = []string{"gortex-explore", "gortex-guide", "gortex-debug"}

// skillDirNames lists the skill directories that still hold a SKILL.md.
// Directory presence alone is not the question a profile switch asks —
// what the host loads is.
func skillDirNames(t *testing.T, root string) []string {
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
// exercise: `gortex instructions switch localization` must leave
// OpenCode holding three playbooks, not twenty-one. Before this existed,
// only Claude Code's picker shrank.
func TestSyncSkillsNarrowsToProfileSubset(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	root := globalSkillsDir(env.Home)
	if len(skillDirNames(t, root)) < len(profileSubset)+1 {
		t.Fatalf("install left %v — too few skills for a subset to mean anything", skillDirNames(t, root))
	}

	if _, err := SyncSkills(env.Stderr, env.Home, profileSubset, agents.ApplyOpts{}); err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	want := append([]string(nil), profileSubset...)
	sort.Strings(want)
	if got := skillDirNames(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after the switch %v remain, want %v", got, want)
	}

	// Commands are deliberately untouched: a slash command is invoked by
	// name, so it costs nothing until it is asked for. Claude Code draws
	// the same line between ~/.claude/skills and ~/.claude/commands.
	if _, err := os.Stat(filepath.Join(globalCommandsDir(env.Home), "gortex-rename"+commandFileExt)); err != nil {
		t.Fatalf("the profile switch pruned a slash command: %v", err)
	}
}

// TestSyncSkillsKeepsCustomisedSkill: a profile is not worth a user's
// edits. An out-of-profile playbook the user rewrote stays byte for
// byte, and the switch warns instead of silently widening the surface.
func TestSyncSkillsKeepsCustomisedSkill(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	path := filepath.Join(globalSkillsDir(env.Home), "gortex-rename", skillFileName)
	const mine = "---\nname: gortex-rename\ndescription: mine\n---\nmy own steps\n"
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	log := &strings.Builder{}
	if _, err := SyncSkills(log, env.Home, profileSubset, agents.ApplyOpts{}); err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("customised skill was deleted to satisfy a profile: %v", err)
	}
	if string(got) != mine {
		t.Fatalf("customised skill was rewritten:\n%s", got)
	}
	if !strings.Contains(log.String(), "gortex-rename") {
		t.Fatalf("expected a warning naming the kept skill, got:\n%s", log.String())
	}
}

// TestSyncSkillsWithoutAnInstalledRootIsANoOp: most machines run one or
// two of the supported hosts. Switching a profile must not conjure an
// OpenCode skills tree on a machine that never ran `gortex install`, and
// must not fail because the tree is absent.
func TestSyncSkillsWithoutAnInstalledRootIsANoOp(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	actions, err := SyncSkills(env.Stderr, env.Home, profileSubset, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("SyncSkills on an uninstalled machine: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %v, want none", actions)
	}
	if _, err := os.Stat(globalSkillsDir(env.Home)); !os.IsNotExist(err) {
		t.Fatalf("a switch created the skills root (stat err = %v)", err)
	}
}

// readFrontmatter parses the leading YAML block of a rendered artifact
// into a flat key/value map. Deliberately naive — the renderer only ever
// emits flat scalars, and a parser that accepted more would stop being a
// check on what we emit.
func readFrontmatter(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return out
	}
	for _, line := range lines[1:] {
		if line == "---" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("%s: frontmatter line %q is not a scalar mapping", path, line)
		}
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return out
}

func assertOnlyRecognisedKeys(t *testing.T, name string, front map[string]string) {
	t.Helper()
	for key := range front {
		if !recognisedFrontmatterKeys[key] {
			t.Fatalf("%s: frontmatter key %q is not one OpenCode reads", name, key)
		}
	}
}
