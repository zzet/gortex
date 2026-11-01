package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Repo-scoped callers read only the scoped repository: the caller walk, the
// concurrency classification of each caller, the interface-dispatch
// boundaries and the call sites parked on the target's name placeholders.
// Other repositories sharing those names and placeholders change neither the
// callers nor what was read to build them.
func TestGetCallersScopedNeverScansAndRowsStayFlat(t *testing.T) {
	srv, store, counter := scopedRowServer(t)
	target := scopedRowID("a", scopedRowTarget)
	callers := func() string {
		return scopedRowCall(t, srv, srv.handleGetCallers, t.Context(), "get_callers", map[string]any{"id": target, "repo": "a", "format": "json"})
	}
	var payload struct {
		Edges              []json.RawMessage `json:"edges"`
		CallerNotes        map[string]any    `json:"caller_notes"`
		Boundaries         []json.RawMessage `json:"boundaries"`
		LowerBound         bool              `json:"lower_bound"`
		NameOnlyCandidates int               `json:"name_only_candidates"`
	}
	require.NoError(t, json.Unmarshal([]byte(callers()), &payload))
	require.NotEmpty(t, payload.Edges)
	require.Contains(t, payload.CallerNotes, scopedRowID("a", scopedRowRefresh), "the lock-holding method caller is classified")
	require.NotEmpty(t, payload.Boundaries, "the interface method it overrides is a dispatch boundary")
	require.True(t, payload.LowerBound)
	require.Equal(t, 3, payload.NameOnlyCandidates)
	requireScopedReadsStayFlat(t, counter, func() { addUnrelatedScopedRowRepos(t, store, 5) }, callers)
	require.Positive(t, counter.rows()["GetInEdgesByNodeIDsFromRepos"], "the placeholder read is narrowed to the scoped repository")
}
