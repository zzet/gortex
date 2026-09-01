package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
	"github.com/zzet/gortex/internal/profiles"
)

// Byte ceilings for the installed ambient layer. Approximated at ~4.4 B/token:
//   - 4 KiB global section (pointer block + the @-included default
//     profile body) ≈ 0.9k tokens
//   - 2.5 KiB skills-eager total ≈ 0.57k tokens
//
// Blowing either (a future rule-block or description balloon) fails loudly
// instead of silently re-inflating the per-session tax. Per-profile body
// ceilings live in internal/profiles.
const (
	globalSectionByteCeiling = 4096 // 4 KiB
	skillsEagerByteCeiling   = 2560 // 2.5 KiB
)

// frontmatterOf returns the YAML frontmatter block (--- … ---) of a skill /
// sub-agent definition — the part a client loads eagerly to route. Empty when
// the body has no leading frontmatter.
func frontmatterOf(s string) string {
	s = strings.TrimLeft(s, "\n")
	if !strings.HasPrefix(s, "---\n") {
		return ""
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return ""
	}
	return "---\n" + rest[:end+len("\n---")]
}

// TestInstalledAmbientByteCeilings is the permanent measurement gate: it prints
// the byte cost of the installed ambient layer and asserts the global rule
// block and the skills-eager total stay inside their ceilings.
func TestInstalledAmbientByteCeilings(t *testing.T) {
	// What a session actually pays at the machine level: the pointer
	// block in ~/.claude/CLAUDE.md plus the @-included active profile
	// body (default profile on a fresh install).
	def, _ := profiles.ByName(profiles.DefaultName)
	global := len(agents.GlobalPointerBody("/home/user/.gortex/instructions")) + len(def.Body())

	skillsFM := 0
	for _, body := range GlobalSkills {
		skillsFM += len(frontmatterOf(body))
	}
	subFM := 0
	for _, body := range SubAgents {
		subFM += len(frontmatterOf(body))
	}

	t.Logf("installed ambient byte cost:")
	t.Logf("  global section (pointer+default profile): %d bytes  (ceiling %d)", global, globalSectionByteCeiling)
	t.Logf("  skills eager frontmatter : %d bytes  (ceiling %d, %d skills)", skillsFM, skillsEagerByteCeiling, len(GlobalSkills))
	t.Logf("  sub-agent eager frontmatter: %d bytes  (%d agents)", subFM, len(SubAgents))
	t.Logf("  projected installed eager: %d bytes", global+skillsFM+subFM)

	if global > globalSectionByteCeiling {
		t.Errorf("global section (pointer + default profile body) is %d bytes, over the %d ceiling", global, globalSectionByteCeiling)
	}
	if skillsFM > skillsEagerByteCeiling {
		t.Errorf("skills eager frontmatter is %d bytes, over the %d ceiling", skillsFM, skillsEagerByteCeiling)
	}
}

// TestGlobalInstall_FatToSlimReplacement is the migration gate: a user
// upgrading from a fat pre-diet rule block gets the thin pointer block
// on re-install (the policy body moves to the @-included instruction
// profile), with the user's own prose around the marker block preserved
// and no duplicate block. Exercises the marker-fenced merge on the real
// install path.
func TestGlobalInstall_FatToSlimReplacement(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true

	claudeMd := filepath.Join(env.Home, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claudeMd), 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed a realistic pre-diet file: user prose, then a FAT marker block
	// carrying content the diet relocates (provider matrix, capabilities
	// catalog), then more user prose.
	const userAbove = "# My machine notes\n\nAlways run tests with -race.\n"
	const userBelow = "\n## My other tooling\n\nUse ripgrep for logs.\n"
	fatBlock := agents.GlobalRulesStartMarker + "\n" +
		"## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob\n\n" +
		"### LLM provider\n" +
		"one of `local` / `anthropic` / `openai` / `azure` / `ollama` / `claudecli` / `codex` / `copilot` / `cursor` / `opencode` / `gemini` / `bedrock` / `deepseek`\n\n" +
		"### Non-obvious capabilities worth knowing\n" +
		"- analyze is a 61-kind dispatcher: dead_code, hotspots, cycles via Tarjan's SCC…\n" +
		"- TOON tabular text is a compact tabular text, lossy fallback\n" +
		agents.GlobalRulesEndMarker + "\n"
	seed := userAbove + "\n" + fatBlock + userBelow
	if err := os.WriteFile(claudeMd, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, err := os.ReadFile(claudeMd)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)

	// User prose around the block survives verbatim.
	if !strings.Contains(text, userAbove) {
		t.Error("user content above the block was clobbered")
	}
	if !strings.Contains(text, strings.TrimLeft(userBelow, "\n")) {
		t.Error("user content below the block was clobbered")
	}

	// Exactly one marker block — the fat one was replaced in place, not
	// appended alongside.
	if n := strings.Count(text, agents.GlobalRulesStartMarker); n != 1 {
		t.Errorf("expected exactly 1 rule block, found %d", n)
	}

	// The block is now the thin pointer: it @-includes the active
	// profile copy from the hermetic instructions dir…
	// Two spellings of one file, and the difference is the point: the
	// @-include is document content and stays '/'-spelled on every OS,
	// while the ReadFile below is a real filesystem call and needs the
	// native path.
	activePath := filepath.Join(env.InstructionsDir, profiles.ActiveFileName)
	includedPath := filepath.ToSlash(activePath)
	if !strings.Contains(text, "@"+includedPath) {
		t.Errorf("re-installed block does not @-include the active profile (%s)", includedPath)
	}
	if !strings.Contains(text, "gortex instructions switch") {
		t.Error("re-installed block lost the switch verb")
	}
	// …and the slim policy core moved into the profile file itself.
	activeBody, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatalf("active profile copy was not generated: %v", err)
	}
	for _, token := range []string{
		"gortex://guide", "explore", "search", "read", "change",
		"capabilities", "recall", "remember",
	} {
		if !strings.Contains(string(activeBody), token) {
			t.Errorf("active profile body is missing the policy core token %q", token)
		}
	}
	// …and the relocated fat content is gone.
	for _, gone := range []string{
		"Non-obvious capabilities worth knowing",
		"61-kind dispatcher",
		"compact tabular text, lossy",
		"`local` / `anthropic` / `openai` / `azure` / `ollama` / `claudecli` / `codex` / `copilot` / `cursor` / `opencode` / `gemini` / `bedrock` / `deepseek`",
	} {
		if strings.Contains(text, gone) {
			t.Errorf("re-installed block still carries relocated fat content %q", gone)
		}
	}
}
