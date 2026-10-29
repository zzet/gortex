package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestEmitPluginBundle_Layout(t *testing.T) {
	dir := t.TempDir()

	written, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	})
	if err != nil {
		t.Fatalf("EmitPluginBundle: %v", err)
	}

	// The emitter should write exactly: 1 plugin.json + 1 README +
	// 1 LICENSE + 1 .mcp.json + N skills + N sub-agents + 1 hooks.json
	// + 1 hook handler script. No commands/: plugin skills are already
	// slash commands.
	wantPaths := []string{
		".claude-plugin/plugin.json",
		".mcp.json",
		"LICENSE",
		"README.md",
		"hooks-handlers/gortex-hook.sh",
		"hooks/hooks.json",
	}
	for _, id := range sortedKeys(GlobalSkills) {
		wantPaths = append(wantPaths, filepath.Join("skills", pluginSkillName(id), "SKILL.md"))
	}
	for _, name := range sortedKeys(SubAgents) {
		wantPaths = append(wantPaths, filepath.Join("agents", name))
	}
	sort.Strings(wantPaths)

	got := append([]string(nil), written...)
	sort.Strings(got)

	if len(got) != len(wantPaths) {
		t.Fatalf("file count mismatch: got %d, want %d.\ngot=%v\nwant=%v", len(got), len(wantPaths), got, wantPaths)
	}
	for i := range got {
		if got[i] != wantPaths[i] {
			t.Errorf("path[%d] mismatch: got %q, want %q", i, got[i], wantPaths[i])
		}
	}

	// Every path the emitter claimed to write must exist on disk.
	for _, rel := range written {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing file %s: %v", rel, err)
		}
	}
}

func TestEmitPluginBundle_PluginManifestShape(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("plugin.json: invalid JSON: %v", err)
	}
	// Required keys per the marketplace schema we observed in
	// claude-plugins-official: name, description, author. We add
	// version + homepage for our own metadata.
	for _, key := range []string{"name", "description", "author", "version", "homepage"} {
		if _, ok := manifest[key]; !ok {
			t.Errorf("plugin.json missing required key %q", key)
		}
	}
	if got := manifest["name"]; got != "gortex" {
		t.Errorf("plugin.json name = %v, want gortex", got)
	}
	if got := manifest["version"]; got != "0.18.2" {
		t.Errorf("plugin.json version = %v, want 0.18.2", got)
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Errorf("plugin.json should end with a newline (got %q)", string(body[len(body)-3:]))
	}
}

func TestEmitPluginBundle_MCPJSONShape(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mcp map[string]any
	if err := json.Unmarshal(body, &mcp); err != nil {
		t.Fatalf(".mcp.json: invalid JSON: %v", err)
	}
	gx, ok := mcp["gortex"].(map[string]any)
	if !ok {
		t.Fatalf(".mcp.json: missing 'gortex' entry, got %v", mcp)
	}
	if got := gx["command"]; got != "gortex" {
		t.Errorf(".mcp.json command = %v, want gortex", got)
	}
	args, ok := gx["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "mcp" {
		t.Errorf(".mcp.json args = %v, want [mcp]", args)
	}
}

func TestPluginReadme_DescribesPublicAgentSurface(t *testing.T) {
	for _, want := range []string{
		"21 tools",
		"`explore`",
		"`search` / `read` / `relations` / `trace`",
		"`edit` / `refactor`",
		"`capabilities`",
		"`gortex call`",
		"`/gortex:explore`",
	} {
		if !strings.Contains(pluginReadmeBody, want) {
			t.Errorf("plugin README must contain %q", want)
		}
	}
	for _, forbidden := range []string{
		"52 tools", "search_symbols", "find_usages", "get_call_chain",
		"rename_symbol", "preview_edit", "simulate_chain", "facade-v1",
		"gortex daemon start",
		"`/gortex-",
	} {
		if strings.Contains(pluginReadmeBody, forbidden) {
			t.Errorf("plugin README must not expose %q", forbidden)
		}
	}
}

func TestEmitPluginBundle_HooksShape(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("hooks.json: invalid JSON: %v", err)
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks.json: missing top-level 'hooks' object: %v", doc)
	}

	// All six lifecycle/tool event kinds must be present; each must reference the
	// hooks-handlers script via ${CLAUDE_PLUGIN_ROOT}.
	for _, event := range []string{"PreToolUse", "UserPromptSubmit", "SubagentStart", "SubagentStop", "Stop", "SessionStart"} {
		entries, ok := hooks[event].([]any)
		if !ok || len(entries) == 0 {
			t.Errorf("hooks.json: event %s missing or empty", event)
			continue
		}
		entry := entries[0].(map[string]any)
		inner, ok := entry["hooks"].([]any)
		if !ok || len(inner) == 0 {
			t.Errorf("hooks.json: %s has no inner hooks", event)
			continue
		}
		first := inner[0].(map[string]any)
		cmd, _ := first["command"].(string)
		if !strings.Contains(cmd, "${CLAUDE_PLUGIN_ROOT}") {
			t.Errorf("hooks.json: %s command does not reference ${CLAUDE_PLUGIN_ROOT}: %q", event, cmd)
		}
		if !strings.Contains(cmd, "gortex-hook.sh") {
			t.Errorf("hooks.json: %s command does not invoke gortex-hook.sh: %q", event, cmd)
		}
	}

	// PreCompact output is discarded by Claude Code; the post-compaction
	// briefing rides on SessionStart(source="compact") instead (#814).
	if _, ok := hooks["PreCompact"]; ok {
		t.Error("hooks.json: PreCompact must not be bound")
	}

	// PreToolUse should carry the canonical matcher.
	pre := hooks["PreToolUse"].([]any)
	matcher, _ := pre[0].(map[string]any)["matcher"].(string)
	if matcher != CurrentPreToolUseMatcher {
		t.Errorf("hooks.json: PreToolUse matcher = %q, want %q", matcher, CurrentPreToolUseMatcher)
	}
	post := hooks["PostToolUse"].([]any)
	postMatcher, _ := post[0].(map[string]any)["matcher"].(string)
	for _, want := range []string{"mcp__gortex__explore", "mcp__plugin_gortex_gortex__explore"} {
		if !strings.Contains(postMatcher, want) {
			t.Errorf("hooks.json: PostToolUse matcher %q missing %q", postMatcher, want)
		}
	}
}

func TestEmitPluginBundle_HookHandlerExecBit(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "hooks-handlers", "gortex-hook.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// NTFS carries no POSIX permission bits: os.Chmod there only
		// toggles the read-only attribute and every file reports 0666.
		// The bundle still has to be emitted, which the Stat above pins.
		return
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("hooks-handlers/gortex-hook.sh should be executable (mode=%v)", info.Mode())
	}
}

func TestEmitPluginBundle_SkillBodiesMatchSource(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	// Single-source-of-truth check: the bytes written under
	// skills/<name>/SKILL.md must equal GlobalSkills[<id>] except for
	// the frontmatter name line. Drift means content.go was edited
	// without re-running the emitter; the CI guard catches that case
	// but we double-check here in unit tests.
	for id, source := range GlobalSkills {
		name := pluginSkillName(id)
		got, err := os.ReadFile(filepath.Join(dir, "skills", name, "SKILL.md"))
		if err != nil {
			t.Errorf("read skill %s: %v", name, err)
			continue
		}
		want := strings.Replace(source, "\nname: "+id+"\n", "\nname: "+name+"\n", 1)
		if string(got) != want {
			t.Errorf("skill %s body drift: bytes do not match GlobalSkills[%q] with the plugin name", name, id)
		}
	}
}

// Claude Code namespaces plugin skills as "gortex:", so a skill's
// directory and frontmatter name must both drop the "gortex-" prefix.
// The frontmatter name wins over the directory, so checking only one
// would let /gortex:gortex-<name> slip back in.
func TestEmitPluginBundle_SkillNamesDropPrefix(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(GlobalSkills) {
		t.Fatalf("skills/ has %d entries, want %d (one per workflow)", len(entries), len(GlobalSkills))
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "gortex-") {
			t.Errorf("skill directory %q keeps the gortex- prefix", e.Name())
		}
		body, err := os.ReadFile(filepath.Join(dir, "skills", e.Name(), "SKILL.md"))
		if err != nil {
			t.Errorf("read skill %s: %v", e.Name(), err)
			continue
		}
		if !strings.Contains(string(body), "\nname: "+e.Name()+"\n") {
			t.Errorf("skill %s: frontmatter name does not match its directory", e.Name())
		}
	}
}

// A plugin skill is already a slash command, so a commands/ directory
// would list every workflow twice. The bodies there also had no
// frontmatter, which fails `claude plugin validate --strict`.
func TestEmitPluginBundle_NoCommandsDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "commands")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plugin bundle must not contain commands/ (stat err = %v)", err)
	}
}

func TestEmitPluginBundle_RemovesLegacyPaths(t *testing.T) {
	dir := t.TempDir()
	cmdName := sortedKeys(SlashCommands)[0]
	skillID := sortedKeys(GlobalSkills)[0]
	legacy := []string{
		filepath.Join("commands", cmdName),
		filepath.Join("skills", skillID, "SKILL.md"),
	}
	for _, rel := range legacy {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Files the emitter never wrote, including one inside a legacy
	// skill directory, must survive the cleanup.
	userFiles := []string{
		filepath.Join(dir, "skills", "my-own", "SKILL.md"),
		filepath.Join(dir, "skills", sortedKeys(GlobalSkills)[1], "references", "team.md"),
	}
	for _, full := range userFiles {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "commands")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy commands/ should be removed (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", skillID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy skills/%s should be removed (stat err = %v)", skillID, err)
	}
	for _, full := range userFiles {
		if _, err := os.Stat(full); err != nil {
			t.Errorf("files the emitter did not write must survive: %v", err)
		}
	}
	legacySkill := filepath.Join(dir, "skills", sortedKeys(GlobalSkills)[1], "SKILL.md")
	if _, err := os.Stat(legacySkill); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy %s should be removed (stat err = %v)", legacySkill, err)
	}
}

func TestPluginSkillBody_OnlyRewritesLeadingFrontmatter(t *testing.T) {
	cases := map[string]string{
		"---\nname: gortex-x\ndescription: d\n---\nbody\n": "---\nname: x\ndescription: d\n---\nbody\n",
		"# Skill\n---\nname: body\n":                       "# Skill\n---\nname: body\n",
		"---\nmeta:\n  name: nested\n---\nname: after\n":   "---\nmeta:\n  name: nested\n---\nname: after\n",
	}
	for in, want := range cases {
		if got := pluginSkillBody(in, "x"); got != want {
			t.Errorf("pluginSkillBody(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmitPluginBundle_Idempotent(t *testing.T) {
	dir := t.TempDir()

	first, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Fatalf("second emit: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("file count drift between runs: %d vs %d", len(first), len(second))
	}
	// Every file's bytes must be identical across the two runs.
	for _, rel := range first {
		path := filepath.Join(dir, rel)
		body1, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Re-emit and re-read: same path, must equal first body.
		body2, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body1) != string(body2) {
			t.Errorf("non-idempotent output at %s", rel)
		}
	}
}

func TestEmitPluginBundle_RejectsBadInputs(t *testing.T) {
	cases := map[string]PluginBundleSpec{
		"missing-target":  {Version: "0.18.2"},
		"missing-version": {TargetDir: t.TempDir()},
		"bad-variant":     {TargetDir: t.TempDir(), Version: "0.18.2", LayoutVariant: "weird"},
	}
	for name, spec := range cases {
		_, err := EmitPluginBundle(spec)
		if err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestEmitPluginBundle_VariantDefaultsToAnthropic(t *testing.T) {
	dir := t.TempDir()
	// Empty LayoutVariant should fall through to LayoutVariantAnthropic.
	_, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Fatalf("empty variant: %v", err)
	}
	// Verify by checking that the Anthropic-shaped manifest exists.
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("default variant did not produce .claude-plugin/plugin.json: %v", err)
	}
}

func TestEmitPluginBundle_OverwritesExistingFiles(t *testing.T) {
	dir := t.TempDir()

	// Pre-seed an old README to ensure overwrite works.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "OLD\n" {
		t.Errorf("emitter did not overwrite existing README.md")
	}
}

func TestPluginPathHelpers(t *testing.T) {
	if PluginManifestPath() != filepath.Join(".claude-plugin", "plugin.json") {
		t.Errorf("PluginManifestPath wrong: %s", PluginManifestPath())
	}
	if PluginMCPPath() != ".mcp.json" {
		t.Errorf("PluginMCPPath wrong: %s", PluginMCPPath())
	}
	if PluginHooksPath() != filepath.Join("hooks", "hooks.json") {
		t.Errorf("PluginHooksPath wrong: %s", PluginHooksPath())
	}

	skills := PluginSkillPaths()
	if len(skills) != len(GlobalSkills) {
		t.Errorf("PluginSkillPaths count: got %d, want %d", len(skills), len(GlobalSkills))
	}
	subagents := PluginSubAgentPaths()
	if len(subagents) != len(SubAgents) {
		t.Errorf("PluginSubAgentPaths count: got %d, want %d", len(subagents), len(SubAgents))
	}
}

func TestEmitPluginBundle_SubAgentBodiesMatchSource(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	for name, source := range SubAgents {
		got, err := os.ReadFile(filepath.Join(dir, "agents", name))
		if err != nil {
			t.Errorf("read sub-agent %s: %v", name, err)
			continue
		}
		want := pluginSubAgentBody(source)
		if string(got) != want {
			t.Errorf("sub-agent %s body drift: bytes do not match the plugin-transformed SubAgents[%q]", name, name)
		}
	}
}

func TestEmitPluginBundle_SubAgentToolNamespaces(t *testing.T) {
	// Pin Claude Code's external names independently of the emitter constants:
	// a misspelled plugin namespace must fail even if every tool uses it.
	const wantUserPrefix = "mcp__gortex__"
	const wantPluginPrefix = "mcp__plugin_gortex_gortex__"
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"}); err != nil {
		t.Fatal(err)
	}

	for name, source := range SubAgents {
		userTools := SubAgentTools(source)
		if len(userTools) == 0 {
			t.Fatalf("user sub-agent %s has no tools", name)
		}
		for _, tool := range userTools {
			if !strings.HasPrefix(tool, wantUserPrefix) {
				t.Errorf("user sub-agent %s tool %q does not use %q", name, tool, wantUserPrefix)
			}
		}

		body, err := os.ReadFile(filepath.Join(dir, "agents", name))
		if err != nil {
			t.Fatalf("read plugin sub-agent %s: %v", name, err)
		}
		pluginTools := SubAgentTools(string(body))
		if len(pluginTools) != len(userTools) {
			t.Fatalf("plugin sub-agent %s tool count = %d, want %d", name, len(pluginTools), len(userTools))
		}
		for i, tool := range pluginTools {
			want := wantPluginPrefix + strings.TrimPrefix(userTools[i], wantUserPrefix)
			if tool != want {
				t.Errorf("plugin sub-agent %s tool[%d] = %q, want %q", name, i, tool, want)
			}
		}
	}
}

func TestEmitPluginBundle_MissingTargetParent(t *testing.T) {
	// MkdirAll handles non-existent parents — verify that.
	dir := filepath.Join(t.TempDir(), "deep", "nested", "path")
	_, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Errorf("EmitPluginBundle should create parent dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Errorf("emitter did not create plugin.json at deep target")
		}
	}
}
