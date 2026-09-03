package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// providerMatrixMarker is the 13-provider enumeration — the single-home
// marker for the LLM-provider matrix. It must appear in the guide and NOWHERE
// in the installed CLAUDE.md sections (asserted by the single-home gate).
const providerMatrixMarker = "`local` / `anthropic` / `openai` / `azure` / `ollama` / `claudecli` / `codex` / `copilot` / `cursor` / `opencode` / `gemini` / `bedrock` / `deepseek`"

// formatDeepDiveMarker is a phrase unique to the wire-format deep-dive in the
// server instructions (sharedParamLegend). It is the single-home marker for
// "format lives only in the instructions" — asserted absent from the guide
// here and from the CLAUDE.md sections in the agents package.
const formatDeepDiveMarker = "compact tabular text, lossy"

// TestGuideText_TopicsAndContent verifies each relocated reference block is
// reachable through GuideText, both in the full render and section-addressed.
func TestGuideText_TopicsAndContent(t *testing.T) {
	full := GuideText("")
	require.Contains(t, full, "# Gortex Guide")
	require.Contains(t, full, providerMatrixMarker, "provider matrix must live in the guide")
	require.Contains(t, full, "Overlay sessions", "capabilities catalog must live in the guide")
	require.Contains(t, full, "compress_bodies", "token-economy content must live in the guide")
	require.Contains(t, full, "gortex://report", "resources list must live in the guide")
	require.Contains(t, full, "## Worktree and branch views")

	// Section addressing returns just that section.
	require.Contains(t, GuideText("providers"), providerMatrixMarker)
	require.NotContains(t, GuideText("providers"), "Overlay sessions",
		"a section address must return only its section")
	require.Contains(t, GuideText("capabilities"), "Speculative execution")
	require.Contains(t, GuideText("resources"), "gortex://guide")
	views := GuideText("views")
	for _, cue := range []string{"exact:false", "require_exact:true", "require_fresh:true", "wait_deadline", "checkout_id", "refs/heads/release", "coordinator-backed"} {
		require.Contains(t, views, cue)
	}

	// The analyze / search_ast sections point at the single-source catalog
	// (kind:"help" / detector:"help" / gortex://schema) rather than re-inlining
	// it — the grouped summary + a pointer, not the full per-kind text.
	analyze := GuideText("analyze")
	require.Contains(t, analyze, "dead_code")
	require.Contains(t, analyze, `kind:"help"`)
	require.NotContains(t, analyze, analyzeCatalogText(),
		"the guide points at the analyze catalog; it must not re-inline the full per-kind text")
	searchAST := GuideText("search_ast")
	require.Contains(t, searchAST, "detector")
	require.Contains(t, searchAST, `detector:"help"`)

	// Aliases resolve.
	require.Equal(t, GuideText("llm"), GuideText("providers"))
	require.Equal(t, GuideText("features"), GuideText("capabilities"))
	require.Equal(t, GuideText("worktrees"), GuideText("views"))

	// Unknown topic degrades to the full guide (still useful, names topics).
	unknown := GuideText("does-not-exist")
	require.Contains(t, unknown, "# Gortex Guide")
	require.Contains(t, unknown, providerMatrixMarker)
}

// TestGuideText_FormatDeepDiveStaysInInstructions enforces that the wire-
// FORMAT deep-dive is NOT re-inlined into the guide — it lives once in the
// server instructions (sharedParamLegend). The guide only points at it.
func TestGuideText_FormatDeepDiveStaysInInstructions(t *testing.T) {
	require.Contains(t, sharedParamLegend, formatDeepDiveMarker,
		"the wire-format deep-dive must live in the server instructions")
	require.NotContains(t, GuideText(""), formatDeepDiveMarker,
		"the guide must point at the server-instructions format legend, not repeat it")
}

// TestGuideResource_ReturnsContent drives the MCP resource handlers.
func TestGuideResource_ReturnsContent(t *testing.T) {
	s := &Server{}
	got, err := s.handleResourceGuide(context.Background(), mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "gortex://guide"},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	trc, ok := got[0].(mcp.TextResourceContents)
	require.True(t, ok)
	require.Contains(t, trc.Text, providerMatrixMarker)

	sec, err := s.handleResourceGuideSection(context.Background(), mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "gortex://guide/capabilities"},
	})
	require.NoError(t, err)
	require.Len(t, sec, 1)
	secText := sec[0].(mcp.TextResourceContents).Text
	require.Contains(t, secText, "Speculative execution")
	require.False(t, strings.Contains(secText, providerMatrixMarker),
		"the capabilities section must not carry the provider matrix")
}
