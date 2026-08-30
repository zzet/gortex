package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Per-profile instruction-body byte ceilings. The body is re-read into
// the model on every API call of every session (via the @-include), so
// a ballooning body is a per-turn tax — blowing a ceiling fails loudly
// instead of silently re-inflating the ambient prefix.
//
// core inherits the pre-profiles global-section ceiling (6.5 KiB); full
// gets slack for its longer surface description only; localization is
// the diet profile and must stay under 2 KiB (~0.45k tokens).
var bodyByteCeilings = map[string]int{
	"core":         3584,
	"full":         6912,
	"localization": 2688,
}

func TestProfileBodyByteCeilings(t *testing.T) {
	for _, p := range Table() {
		ceiling, ok := bodyByteCeilings[p.Name]
		if !ok {
			t.Errorf("profile %q has no body byte ceiling — add one to bodyByteCeilings", p.Name)
			continue
		}
		got := len(p.Body())
		t.Logf("profile %-13s body bytes=%d (ceiling %d)", p.Name, got, ceiling)
		if got > ceiling {
			t.Errorf("profile %q body is %d bytes, over the %d ceiling", p.Name, got, ceiling)
		}
		if got == 0 {
			t.Errorf("profile %q renders an empty body", p.Name)
		}
	}
}

// positioningCues are the load-bearing fragments EVERY profile must
// keep, lean ones included: the mandatory-rule sentinel, the hook-posture
// warning, the one-shot opener, the memory triggers, the discovery
// path, the configurable hook posture, and the switch-back line. Trimming any of
// these is what costs tool adoption — the diet only ever removes
// elaboration around them.
var positioningCues = []string{
	"## MANDATORY: Use Gortex MCP tools", // idempotency sentinel + the rule itself
	"MUST prefer graph queries",
	"Hook posture is configurable",
	"explore",
	"Gortex MCP integration failure",
	"Do not start a daemon",
	"Implicit linked worktrees become overlays automatically",
	"user-requested dedicated graph",
	"view_building",
	"Never restart the daemon, delete its store, or re-track",
	"gortex instructions switch",
	"NEW sessions only",
}

func TestEveryProfileKeepsPositioningCues(t *testing.T) {
	for _, p := range Table() {
		body := p.Body()
		for _, cue := range positioningCues {
			if !strings.Contains(body, cue) {
				t.Errorf("profile %q body lost the positioning cue %q", p.Name, cue)
			}
		}
	}
}

// TestLocalizationPresetRowsStayInSync keeps the optional legacy
// localization preset table internally consistent. The active localization
// instruction profile now describes the compact public surface instead.
func TestLocalizationPresetRowsStayInSync(t *testing.T) {
	p, ok := ByName("localization")
	if !ok {
		t.Fatal("localization profile missing from the table")
	}
	rowTools := map[string]bool{}
	for _, r := range localizationRows {
		rowTools[r.tool] = true
	}
	for _, tool := range p.EagerTools {
		if !rowTools[tool] && !localizationNonTableTools[tool] {
			t.Errorf("eager tool %q has neither a table row nor a prose cue — add one", tool)
		}
	}
	eager := map[string]bool{}
	for _, tool := range p.EagerTools {
		eager[tool] = true
	}
	for _, r := range localizationRows {
		if !eager[r.tool] {
			t.Errorf("table row for %q has no matching eager tool — remove the row or add the tool", r.tool)
		}
	}
	for tool := range localizationNonTableTools {
		if !eager[tool] {
			t.Errorf("prose cue for %q has no matching eager tool", tool)
		}
	}
}

func TestGenerateAndSwitch(t *testing.T) {
	dir := t.TempDir()

	if err := Generate(dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, p := range Table() {
		raw, err := os.ReadFile(filepath.Join(dir, p.Name+".md"))
		if err != nil {
			t.Fatalf("profile file %s: %v", p.Name, err)
		}
		if string(raw) != p.Body() {
			t.Errorf("profile file %s.md is not the rendered body", p.Name)
		}
	}

	// Before any switch, the active copy is the default profile.
	active, err := os.ReadFile(filepath.Join(dir, ActiveFileName))
	if err != nil {
		t.Fatalf("active.md: %v", err)
	}
	def, _ := ByName(DefaultName)
	if string(active) != def.Body() {
		t.Error("active.md is not a byte copy of the default profile before any switch")
	}
	if got := ActiveName(dir); got != DefaultName {
		t.Errorf("ActiveName = %q before any switch, want %q", got, DefaultName)
	}

	// Switch to localization: active.md becomes a byte copy, state
	// records the name, and it is never a symlink.
	p, err := Switch(dir, "localization")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if p.Name != "localization" {
		t.Errorf("switch returned profile %q", p.Name)
	}
	active, err = os.ReadFile(filepath.Join(dir, ActiveFileName))
	if err != nil {
		t.Fatalf("active.md after switch: %v", err)
	}
	loc, _ := ByName("localization")
	if string(active) != loc.Body() {
		t.Error("active.md is not a byte copy of the switched profile")
	}
	if fi, err := os.Lstat(filepath.Join(dir, ActiveFileName)); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("active.md must be a regular file, got mode %v err %v", fi.Mode(), err)
	}
	if got := ActiveName(dir); got != "localization" {
		t.Errorf("ActiveName = %q after switch, want localization", got)
	}
	if Active(dir).HookTier != HookTierStandard {
		t.Error("localization ships the standard hook tier (the lean tier is opt-in machinery)")
	}

	// Re-generate keeps the switched selection.
	if err := Generate(dir); err != nil {
		t.Fatalf("re-generate: %v", err)
	}
	active, _ = os.ReadFile(filepath.Join(dir, ActiveFileName))
	if string(active) != loc.Body() {
		t.Error("re-generate clobbered the active selection")
	}

	// Unknown profile is refused and changes nothing.
	if _, err := Switch(dir, "nope"); err == nil {
		t.Error("switch to unknown profile must error")
	}
	if got := ActiveName(dir); got != "localization" {
		t.Errorf("failed switch mutated state: ActiveName = %q", got)
	}
}

func TestActiveStateDegradesToDefault(t *testing.T) {
	dir := t.TempDir()
	// Missing state file.
	if got := ActiveName(dir); got != DefaultName {
		t.Errorf("missing state: ActiveName = %q, want %q", got, DefaultName)
	}
	// Corrupt state file.
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ActiveName(dir); got != DefaultName {
		t.Errorf("corrupt state: ActiveName = %q, want %q", got, DefaultName)
	}
	// Unknown name in state file.
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(`{"name":"gone"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ActiveName(dir); got != DefaultName {
		t.Errorf("unknown state name: ActiveName = %q, want %q", got, DefaultName)
	}
}

func TestActiveEnvOverride(t *testing.T) {
	dir := t.TempDir()
	if _, err := Switch(dir, "full"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ActiveEnv, "localization")
	if got := ActiveName(dir); got != "localization" {
		t.Errorf("env override ignored: ActiveName = %q", got)
	}
	// Unknown env value falls through to the recorded state.
	t.Setenv(ActiveEnv, "not-a-profile")
	if got := ActiveName(dir); got != "full" {
		t.Errorf("unknown env value must fall through to state, got %q", got)
	}
}

// Single-home markers: content that relocated to gortex://guide and the
// schema resources must never re-inflate an instruction body. Mirrors
// the pre-profiles guard on the CLAUDE.md rule block (the profile file
// is that block's new home).
var relocatedContentMarkers = []string{
	"`local` / `anthropic` / `openai` / `azure` / `ollama` / `claudecli` / `codex` / `copilot` / `cursor` / `opencode` / `gemini` / `bedrock` / `deepseek`", // provider matrix
	"Tarjan's SCC",                // analyze catalog deep-dive
	"compact tabular text, lossy", // wire-format deep-dive
	"k8s_resources",               // analyze kind catalog
	"error-not-wrapped",           // search_ast detector catalog
	"gortex://report",             // analyzer rollup roster
}

// fullPolicyTokens is the policy core the standard-depth profiles must
// carry — the machine-level single home for the full memory-workflow
// triggers. The localization profile intentionally keeps only the
// positioning cues (TestEveryProfileKeepsPositioningCues).
var preCompactFullPolicyTokens = []string{
	"search_symbols", "find_usages", "get_callers", "get_call_chain",
	"get_symbol_source", "get_editing_context", "read_file",
	"smart_context", "edit_file", "rename_symbol", "compress_bodies",
	"distill_session", "surface_memories", "save_note", "store_memory",
	"query_notes", "query_memories",
	"tools_search", "gortex://guide", "MCP startup manages daemon availability",
}

func TestBodies_PolicyCoreAndSingleHome(t *testing.T) {
	full, ok := ByName("full")
	if !ok {
		t.Fatal("full profile missing")
	}
	for _, token := range preCompactFullPolicyTokens {
		if !strings.Contains(full.Body(), token) {
			t.Errorf("full body no longer mentions %q — pre-compact opt-in policy regression", token)
		}
	}

	for _, name := range []string{"core", "localization"} {
		p, found := ByName(name)
		if !found {
			t.Fatalf("profile %q missing", name)
		}
		for _, token := range []string{"explore", "search", "read", "relations", "trace", "change", "edit", "refactor", "capabilities", "recall", "remember"} {
			if !strings.Contains(p.Body(), token) {
				t.Errorf("%s body no longer mentions compact tool %q", name, token)
			}
		}
		for _, banned := range []string{"facade-v1", "search_symbols", "get_symbol_source", "tools_search", "while these tools are available"} {
			if strings.Contains(p.Body(), banned) {
				t.Errorf("%s body exposes implementation detail %q", name, banned)
			}
		}
	}
	for _, p := range Table() {
		body := p.Body()
		for _, banned := range relocatedContentMarkers {
			if strings.Contains(body, banned) {
				t.Errorf("%s body re-carries relocated content %q — single-home violation", p.Name, banned)
			}
		}
	}
}
