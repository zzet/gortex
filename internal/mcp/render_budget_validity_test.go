package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBudgetedDiagramsStaySyntacticallyValid pins that a budget cut
// never hands back a broken document: a diagram (or TOON) response is
// re-rendered over fewer rows, not byte-sliced mid-grammar — a dot
// fragment without its closing brace, or a TOON table whose header
// declares more rows than remain, fails every downstream parser while
// looking like a successful response.
func TestBudgetedDiagramsStaySyntacticallyValid(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 80)

	t.Run("dot keeps its closing brace", func(t *testing.T) {
		out := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "dot", "limit": 0, "max_bytes": 1200})
		require.LessOrEqual(t, len(out), 1200)
		require.Contains(t, out, "digraph")
		require.Contains(t, out, "\n}\n",
			"a budgeted dot document must still close its digraph block, got tail %q", out[max(0, len(out)-80):])
		require.NotContains(t, out, "trimmed to byte budget",
			"the plain-text marker is not dot syntax; the cut must re-render, not slice")
	})

	t.Run("mermaid carries no prose marker", func(t *testing.T) {
		out := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "mermaid", "limit": 0, "max_bytes": 1200})
		require.LessOrEqual(t, len(out), 1200)
		require.NotContains(t, out, "trimmed to byte budget",
			"the plain-text marker is not mermaid syntax; the cut must re-render, not slice")
	})

	t.Run("toon re-renders instead of slicing", func(t *testing.T) {
		out := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "toon", "limit": 0, "max_bytes": 1200})
		require.LessOrEqual(t, len(out), 1200)
		require.NotContains(t, out, "trimmed to byte budget",
			"a byte-sliced TOON table contradicts its own row-count headers; the cut must re-render")
	})
}

// TestTinyTokenBudgetDropsTheDecoration pins the boundary where the
// token-budget decoration is bigger than the budget itself: the
// decoration must be dropped — it can never be the bytes that push the
// payload past the cap it documents. (The JSON structural trim keeps
// its documented scalar-skeleton floor; the decoration must not add
// to it.) On the text renderers the same tiny budget stays a hard
// ceiling outright.
func TestTinyTokenBudgetDropsTheDecoration(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 40)

	for _, tokens := range []int{5, 10, 14} {
		out := findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 0, "max_tokens": tokens})
		require.NotContains(t, out, `"_truncated_by_tokens"`,
			"max_tokens:%d cannot fit the decoration, so the decoration must be dropped", tokens)
		require.NotContains(t, out, `"_max_tokens"`,
			"max_tokens:%d cannot fit the decoration, so the decoration must be dropped", tokens)

		toonOut := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "toon", "limit": 0, "max_tokens": tokens})
		require.LessOrEqual(t, len(toonOut), tokensToBytes(tokens),
			"the text renderers keep max_tokens:%d as a hard ceiling", tokens)
	}
}
