package codex

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

// subAgentKeys is the complete key set a Codex agent file may carry from
// us. Codex parses the file into a `#[serde(deny_unknown_fields)]`
// struct, so a fifth key of any spelling voids the WHOLE file — silently,
// with no warning and no partial load. Pinning the set exactly is the
// only cheap defence against that.
var subAgentKeys = []string{"description", "developer_instructions", "name", "sandbox_mode"}

// TestCodexGlobalModeInstallsSubAgents pins the discovery-critical
// filename: lowercase `.toml`, flat under ~/.codex/agents.
func TestCodexGlobalModeInstallsSubAgents(t *testing.T) {
	env := codexGlobalEnv(t)
	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	names := SubAgentNames()
	if len(names) != len(agents.SubAgents) {
		t.Fatalf("rendered %d sub-agents, want %d", len(names), len(agents.SubAgents))
	}
	if len(names) == 0 {
		t.Fatal("sub-agent set is empty")
	}
	for _, id := range names {
		path := filepath.Join(env.Home, ".codex", "agents", id+".toml")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sub-agent not installed at %s: %v", path, err)
		}
		if !reportsPath(res, path) {
			t.Fatalf("apply did not report %s in its file actions", path)
		}
	}

	// The extension is matched case-sensitively lowercase, and the write
	// must stay flat even though the scan recurses.
	assertFlatLowercaseTOML(t, filepath.Join(env.Home, ".codex", "agents"))
}

// TestCodexSubAgentsAreUserLevelOnly: a project-scoped
// `<repo>/.codex/agents/` is only scanned when the project is TRUSTED —
// an untrusted project's whole config layer is skipped — so writing there
// is a gamble on state the installer cannot see. It would also put Gortex
// files in the user's working tree, which `gortex init` never does.
func TestCodexSubAgentsAreUserLevelOnly(t *testing.T) {
	env := codexProjectEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(env.Home, ".codex", "agents"),
		filepath.Join(env.Root, ".codex", "agents"),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("project mode wrote %s (stat err=%v)", dir, err)
		}
	}
}

// TestCodexSubAgentOmitsMcpServersBecauseTheArrayFormVoidsTheFile is the
// negative test this whole file exists for.
//
// `mcp_servers = ["gortex"]` — the intuitive shape — is not an array of
// server names. The key is a TABLE of full server definitions, so the
// array form fails deserialization with `invalid type: sequence, expected
// a map`, and under deny_unknown_fields that rejects the ENTIRE agent
// file: no agent, no warning. Omitting the key inherits the parent
// session's MCP servers, which is what we want, and Codex has no
// allowlist semantics — an agent cannot be restricted to a subset of
// servers — so there is nothing to gain by writing it even correctly.
func TestCodexSubAgentOmitsMcpServersBecauseTheArrayFormVoidsTheFile(t *testing.T) {
	for id, body := range SubAgents() {
		if strings.Contains(body, "mcp_servers") {
			t.Errorf("sub-agent %s names mcp_servers; the array form voids the whole file and there is no allowlist to gain:\n%s", id, body)
		}
		var decoded map[string]any
		if err := toml.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("sub-agent %s does not parse: %v\n%s", id, err, body)
		}
		if _, exists := decoded["mcp_servers"]; exists {
			t.Errorf("sub-agent %s decoded an mcp_servers key: %#v", id, decoded)
		}
	}
}

// TestCodexSubAgentTOMLRoundTrips parses every rendered agent with the
// same library Codex's config path uses here and asserts the three
// required keys are present and non-blank (whitespace-only counts as
// missing and is rejected), plus that the key set is EXACTLY the four we
// can name — the deny_unknown_fields guard.
func TestCodexSubAgentTOMLRoundTrips(t *testing.T) {
	for id, body := range SubAgents() {
		var decoded map[string]any
		if err := toml.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("sub-agent %s does not parse as TOML: %v\n%s", id, err, body)
		}

		got := make([]string, 0, len(decoded))
		for key := range decoded {
			got = append(got, key)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(subAgentKeys, ",") {
			t.Fatalf("sub-agent %s keys=%v want exactly %v", id, got, subAgentKeys)
		}

		for _, key := range []string{"name", "description", "developer_instructions"} {
			value, _ := decoded[key].(string)
			if strings.TrimSpace(value) == "" {
				t.Errorf("sub-agent %s: %s is blank; Codex rejects the file", id, key)
			}
		}
		if decoded["name"] != id {
			t.Errorf("sub-agent %s: name=%v — the agent's name comes from this key, not the filename", id, decoded["name"])
		}
		// Read-only by contract, so read-only by sandbox: gortex-search
		// only navigates and gortex-impact's body says "Do not mutate
		// files".
		if decoded["sandbox_mode"] != "read-only" {
			t.Errorf("sub-agent %s: sandbox_mode=%v want read-only", id, decoded["sandbox_mode"])
		}
		// The user's model preference is not ours to pin.
		for _, key := range []string{"model", "model_reasoning_effort"} {
			if _, exists := decoded[key]; exists {
				t.Errorf("sub-agent %s pins %s, which belongs to the user", id, key)
			}
		}
	}
}

// TestCodexSubAgentInstructionsFitALiteralString guards the other way an
// agent file breaks silently.
//
// `developer_instructions` is a multi-line LITERAL string — the form
// delimited by three apostrophes — chosen because it processes no escape
// sequences. The bodies are Go raw-string constants full of backticks and
// quotes, and a future backslash would be an escape inside the basic
// (three double quotes) form. What a literal string cannot hold is a run
// of three apostrophes, or a trailing apostrophe adjacent to the closing
// delimiter (that would make four in a row, which TOML forbids). The
// renderer's own newline currently
// separates the body from the delimiter, so the second half of this check
// is conservative on purpose: it stays correct if that newline is ever
// removed, and a loud build failure beats a silently mangled agent.
func TestCodexSubAgentInstructionsFitALiteralString(t *testing.T) {
	skills := subAgentSkills(t)
	if len(skills) == 0 {
		t.Fatal("sub-agent set is empty")
	}
	for _, skill := range skills {
		body := subAgentInstructions(skill)
		if strings.Contains(body, "'''") {
			t.Errorf("sub-agent %s body contains ''' and cannot be embedded in a TOML literal string", skill.ID)
		}
		if strings.HasSuffix(body, "'") {
			t.Errorf("sub-agent %s body ends with an apostrophe; adjacent to ''' that is four in a row, which TOML forbids", skill.ID)
		}
		// The apostrophes that DO appear ("an operation's arguments") are
		// exactly why the guard above is not vacuous.
		rendered := renderSubAgent(skill)
		if !strings.Contains(rendered, "developer_instructions = '''\n") {
			t.Errorf("sub-agent %s does not use a multi-line literal string:\n%s", skill.ID, rendered)
		}
	}
}

// TestCodexSubAgentQuotingIsTOMLNotYAML pins why skillpack.QuoteYAMLValue
// is not reused: it returns identifier-ish values UNQUOTED, and TOML has
// no bare values, so `name = gortex-search` would be a syntax error.
func TestCodexSubAgentQuotingIsTOMLNotYAML(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"gortex-search", `"gortex-search"`},
		{`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		{"line\nbreak", `"line\nbreak"`},
		{"bell\x07", `"bell\u0007"`},
	} {
		if got := quoteTOMLBasicString(tc.in); got != tc.want {
			t.Errorf("quoteTOMLBasicString(%q)=%s want %s", tc.in, got, tc.want)
		}
	}
	var decoded map[string]any
	if err := toml.Unmarshal([]byte("name = "+quoteTOMLBasicString(`a "quoted" \ value`)+"\n"), &decoded); err != nil {
		t.Fatalf("quoted value does not parse: %v", err)
	}
	if decoded["name"] != `a "quoted" \ value` {
		t.Errorf("round trip lost the value: %#v", decoded["name"])
	}
}

// TestCodexSubAgentIsNeverOverwritten covers the full ownership contract
// for a tuned copy: a second Apply leaves it alone, RemoveGlobal keeps
// it, and the uninstall preview never promises to delete it.
func TestCodexSubAgentIsNeverOverwritten(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	path := subAgentPath(env.Home, "gortex-search")
	const custom = "name = \"gortex-search\"\ndescription = \"mine\"\ndeveloper_instructions = '''\nmy own instructions\n'''\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got := readCodexFile(t, path); got != custom {
		t.Fatalf("customised sub-agent was overwritten:\n%s", got)
	}

	for _, listed := range GlobalArtifacts(env.Home) {
		if listed == path {
			t.Errorf("the preview promises to delete a customised sub-agent: %s", listed)
		}
	}
	if _, failures := a.RemoveGlobal(env, agents.ApplyOpts{}); len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if got := readCodexFile(t, path); got != custom {
		t.Fatalf("the customised sub-agent was changed or deleted:\n%s", got)
	}
}

// TestCodexRemoveGlobalRemovesSubAgentsAndPrunesTheDirectory is the
// round-trip: install then uninstall leaves no Gortex agent file and no
// empty directory to wonder about.
func TestCodexRemoveGlobalRemovesSubAgentsAndPrunesTheDirectory(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	dir := filepath.Join(env.Home, ".codex", "agents")

	listed := make(map[string]bool)
	for _, path := range GlobalArtifacts(env.Home) {
		listed[path] = true
	}
	for _, id := range SubAgentNames() {
		if !listed[subAgentPath(env.Home, id)] {
			t.Fatalf("GlobalArtifacts omits sub-agent %s: %v", id, GlobalArtifacts(env.Home))
		}
	}

	if _, failures := a.RemoveGlobal(env, agents.ApplyOpts{}); len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	for _, id := range SubAgentNames() {
		if _, err := os.Stat(subAgentPath(env.Home, id)); !os.IsNotExist(err) {
			t.Errorf("sub-agent %s survived removal (stat err=%v)", id, err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the emptied agents directory was not pruned (stat err=%v)", err)
	}
	for _, path := range GlobalArtifacts(env.Home) {
		if strings.Contains(path, string(filepath.Separator)+"agents"+string(filepath.Separator)) {
			t.Errorf("GlobalArtifacts still lists %s after removal", path)
		}
	}
}

// TestCodexRemoveGlobalKeepsAUsersOwnAgent: ~/.codex/agents is the user's
// own agent root. A hand-written agent beside ours survives byte-identical
// and keeps the directory alive — the same posture the shared skills root
// gets.
func TestCodexRemoveGlobalKeepsAUsersOwnAgent(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	dir := filepath.Join(env.Home, ".codex", "agents")
	mine := filepath.Join(dir, "mine.toml")
	const body = "name = \"mine\"\ndescription = \"my own agent\"\ndeveloper_instructions = '''\ndo my thing\n'''\n"
	if err := os.WriteFile(mine, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, failures := a.RemoveGlobal(env, agents.ApplyOpts{}); len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if got := readCodexFile(t, mine); got != body {
		t.Fatalf("the user's own agent changed:\n%s", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the agents directory was pruned out from under the user's agent: %v", err)
	}
}

// TestCodexPlanEnumeratesEverySubAgentPath: `gortex doctor` and
// --print-config read Plan(), never Apply(), so a path Apply writes but
// Plan omits is invisible to both.
func TestCodexPlanEnumeratesEverySubAgentPath(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()
	plan, err := a.Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	planned := make(map[string]bool, len(plan.Files))
	for _, f := range plan.Files {
		planned[f.Path] = true
	}
	for _, id := range SubAgentNames() {
		path := subAgentPath(env.Home, id)
		if !planned[path] {
			t.Fatalf("Plan omitted %s: %#v", path, plan.Files)
		}
	}

	// Project mode plans nothing, matching Apply.
	projectPlan, err := a.Plan(codexProjectEnv(t))
	if err != nil {
		t.Fatalf("project plan: %v", err)
	}
	for _, f := range projectPlan.Files {
		if strings.HasSuffix(f.Path, ".toml") && strings.Contains(f.Path, "agents") {
			t.Errorf("project mode planned a sub-agent: %s", f.Path)
		}
	}
}

// TestCodexSubAgentsRefuseAnEmptyHome: filepath.Join onto an empty home
// yields a RELATIVE path, so the agents would land in whatever directory
// the process is standing in — most often the user's repo.
func TestCodexSubAgentsRefuseAnEmptyHome(t *testing.T) {
	env := agents.Env{Mode: agents.ModeGlobal}
	if got := installSubAgents(env, agents.ApplyOpts{}); len(got) != 0 {
		t.Fatalf("installSubAgents with no home returned %#v", got)
	}
	if got := planSubAgents(env); len(got) != 0 {
		t.Fatalf("planSubAgents with no home returned %#v", got)
	}
	if got := subAgentPath("", "gortex-search"); got != "" {
		t.Fatalf("subAgentPath with no home = %q, want empty", got)
	}
}

// subAgentSkills re-parses the shared bodies the way SubAgents does, so a
// test can inspect the neutral Skill rather than the rendered file.
func subAgentSkills(t *testing.T) []skillpack.Skill {
	t.Helper()
	out := make([]skillpack.Skill, 0, len(agents.SubAgents))
	for file, body := range agents.SubAgents {
		skill, err := skillpack.Parse(subAgentID(file), body)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		out = append(out, skill)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// assertFlatLowercaseTOML fails when the agents directory holds a
// subdirectory or a file whose extension is not lowercase `.toml`. The
// scan recurses, but the extension match is case-sensitive: a `.TOML`
// file is ignored with no error at all.
func assertFlatLowercaseTOML(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		if d.IsDir() {
			t.Errorf("sub-agents must be written flat, found directory %s", path)
			return fs.SkipDir
		}
		if filepath.Ext(d.Name()) != ".toml" {
			t.Errorf("%s is not a lowercase .toml file; Codex ignores it silently", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// subAgentCount is how many agent files a ModeGlobal Apply writes.
// Derived, not hardcoded — the same reason curatedSkillCount is — so
// shipping a third sub-agent does not mean editing every count assertion
// in this package.
func subAgentCount() int { return len(SubAgentNames()) }

func readCodexFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
