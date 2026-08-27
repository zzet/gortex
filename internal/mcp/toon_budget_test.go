package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFindUsages_TOONHonorsByteBudget pins that an explicit
// format:"toon" stays inside the response budget every other format
// enforces: the schema's max_bytes is a contract on the response, not
// on one renderer.
func TestFindUsages_TOONHonorsByteBudget(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 60)

	out := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "toon", "limit": 0, "max_bytes": 120})
	require.LessOrEqual(t, len(out), 120,
		"format:toon must honor max_bytes; got %d bytes", len(out))
}

// TestFindUsages_TOONHonorsTokenBudget pins the max_tokens axis on the
// TOON path: the token cap converts to the same byte ceiling the other
// formats apply.
func TestFindUsages_TOONHonorsTokenBudget(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 60)

	out := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "toon", "limit": 0, "max_tokens": 30})
	require.LessOrEqual(t, len(out), tokensToBytes(30),
		"format:toon must honor max_tokens; got %d bytes over a %d-byte ceiling", len(out), tokensToBytes(30))
}

// TestFindUsages_TOONTinyBudgetBoundary pins the boundary the budget
// helper guarantees everywhere else: a budget at or below the trim
// marker's own length still never yields a payload above the budget.
func TestFindUsages_TOONTinyBudgetBoundary(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 60)

	for _, budget := range []int{1, len(trimBudgetMarker), len(trimBudgetMarker) + 1} {
		out := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "toon", "limit": 0, "max_bytes": budget})
		require.LessOrEqual(t, len(out), budget,
			"budget %d is a hard ceiling on the toon path", budget)
	}
}
