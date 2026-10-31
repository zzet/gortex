package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Repo-scoped usages read only the scoped repository's references and the
// call sites parked on the target's name placeholders. Other repositories
// park their own calls on the same bare placeholders; those must change
// neither the usages nor what was read to build them.
func TestFindUsagesScopedNeverScansAndRowsStayFlat(t *testing.T) {
	srv, store, counter := scopedRowServer(t)
	target := scopedRowID("a", scopedRowTarget)
	usages := func() string {
		defaults := scopedRowCall(t, srv, srv.handleFindUsages, t.Context(), "find_usages", map[string]any{"id": target, "repo": "a", "format": "json"})
		nameOnly := scopedRowCall(t, srv, srv.handleFindUsages, t.Context(), "find_usages", map[string]any{"id": target, "repo": "a", "format": "json", "min_tier": "text_matched"})
		return "[" + defaults + "," + nameOnly + "]"
	}
	var payload []struct {
		NameOnlyCandidates int `json:"name_only_candidates"`
	}
	require.NoError(t, json.Unmarshal([]byte(usages()), &payload))
	require.Equal(t, 3, payload[0].NameOnlyCandidates, "the scoped repository's three unbound call sites are counted")
	requireScopedReadsStayFlat(t, counter, func() { addUnrelatedScopedRowRepos(t, store, 5) }, usages)
}
