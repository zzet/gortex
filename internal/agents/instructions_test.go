package agents

import (
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/profiles"
)

// TestCursorMDCFrontmatter proves the MDC wrapper emits the two keys
// Cursor needs — `description` so users can see the rule in the UI
// and `alwaysApply: true` so it attaches to every chat turn. Without
// alwaysApply Cursor gates rules on keyword heuristics and the
// Gortex-preference block would fire only sporadically.
func TestCursorMDCFrontmatter(t *testing.T) {
	out := CursorMDCFrontmatter("BODY")
	assert.True(t, strings.HasPrefix(out, "---\n"),
		"MDC file must start with YAML frontmatter fence")
	assert.Contains(t, out, "alwaysApply: true",
		"MDC block must opt into always-apply so Cursor attaches it on every turn")
	assert.Contains(t, out, "description:")
	assert.Contains(t, out, "BODY")
}

// Single-home markers: content that must live in exactly one place. These
// strings anchor the reference blocks that were relocated OUT of the CLAUDE.md
// sections into the guide (provider matrix, analyze catalog) or the server
// instructions (wire-format deep-dive). Their absence from the slim bodies is
// the enforcement side of the single-home principle; their presence in the
// guide / server-instructions is asserted in the mcp package.
const (
	providerMatrixMarker = "`local` / `anthropic` / `openai` / `azure` / `ollama` / `claudecli` / `codex` / `copilot` / `cursor` / `opencode` / `gemini` / `bedrock` / `deepseek`"
	analyzeCatalogMarker = "Tarjan's SCC" // from the analyze `cycles` kind doc
	formatDeepDiveMarker = "compact tabular text, lossy"
)

// TestInstructionsBody_PolicyCoreAndSingleHome locks the compact public
// workflow and its native-MCP failure posture. Agent-facing rules must not leak transport
// versions, legacy implementation aliases, or the relocated long reference.
func TestInstructionsBody_PolicyCoreAndSingleHome(t *testing.T) {
	for _, token := range []string{
		"explore", "search", "read", "relations", "trace", "change",
		"edit", "refactor", "capabilities", "recall", "remember",
		"Gortex MCP integration failure", `operation:"verify"`,
	} {
		if !strings.Contains(InstructionsBody, token) {
			t.Errorf("InstructionsBody no longer mentions %q — policy core regression", token)
		}
	}
	for _, banned := range []string{
		providerMatrixMarker, analyzeCatalogMarker, formatDeepDiveMarker,
		"facade-v1", "search_symbols", "get_symbol_source", "tools_search",
		"gortex call", "daemon start", "while these tools are available",
	} {
		if strings.Contains(InstructionsBody, banned) {
			t.Errorf("InstructionsBody re-carries relocated content %q — single-home violation", banned)
		}
	}
	if got := strings.Count(InstructionsBody, profiles.WorktreeBranchRoutingPolicy); got != 1 {
		t.Fatalf("InstructionsBody embeds canonical worktree policy %d times, want once", got)
	}
	if len(InstructionsBody) > 3_584 {
		t.Fatalf("InstructionsBody grew to %d bytes; keep ambient agent guidance lean", len(InstructionsBody))
	}
}

func TestBashInstructionsBodyUsesOnlyExplicitCLIMirror(t *testing.T) {
	for _, want := range []string{"no native MCP transport", "gortex call <tool>", "gortex call explore", "gortex call change"} {
		if !strings.Contains(BashInstructionsBody, want) {
			t.Errorf("BashInstructionsBody missing %q", want)
		}
	}
	if strings.Contains(BashInstructionsBody, "Native Gortex MCP is mandatory") {
		t.Error("Bash-only instructions must not claim a native MCP transport")
	}
	if got := strings.Count(BashInstructionsBody, profiles.WorktreeBranchRoutingPolicy); got != 1 {
		t.Fatalf("BashInstructionsBody embeds canonical worktree policy %d times, want once", got)
	}
}

// TestGlobalPointerBody_ShapeAndSentinel locks in the thin pointer
// block `gortex install` writes into ~/.claude/CLAUDE.md: it must keep
// the mandatory-rule heading (the cross-adapter idempotency sentinel),
// @-include the active profile copy from the given directory, name the
// switch verb with its next-session caveat, and stay tiny — the full
// policy body lives in the profile file, not here.
func TestGlobalPointerBody_ShapeAndSentinel(t *testing.T) {
	const dir = "/home/user/.gortex/instructions"
	body := GlobalPointerBody(dir)
	// path.Join, not filepath.Join: the @-include is document content and
	// stays '/'-spelled on every OS. Building the expectation with
	// filepath.Join would assert the native mangling on Windows and pass
	// there whether or not the renderer normalises.
	activePath := path.Join(dir, "active.md")

	if !strings.Contains(body, InstructionsSentinel) {
		t.Error("pointer block lost the idempotency sentinel heading")
	}
	if !strings.Contains(body, "@"+activePath) {
		t.Errorf("pointer block does not @-include the active profile: %q", body)
	}
	if !strings.Contains(body, "gortex instructions switch") {
		t.Error("pointer block lost the switch verb")
	}
	if !strings.Contains(body, "NEW sessions only") {
		t.Error("pointer block lost the next-session caveat")
	}
	if strings.Contains(body, "@~") {
		t.Error("pointer block must embed a resolved path, not a ~ shorthand")
	}
	const pointerByteCeiling = 512
	if len(body) > pointerByteCeiling {
		t.Errorf("pointer block is %d bytes, over the %d ceiling — the body belongs in the profile file", len(body), pointerByteCeiling)
	}
}
