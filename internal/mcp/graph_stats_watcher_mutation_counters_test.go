package mcp

import (
	"github.com/stretchr/testify/require"
	"testing"
)

type watcherMutationCounterFixture struct{ mutationTestWatcher }

func (watcherMutationCounterFixture) MutationCounters() map[string]uint64 {
	return map[string]uint64{"echoes_attached": 7, "generations_superseded": 5, "waiters_completed_by_later_patch": 3}
}

func TestGraphStatsReportsWatcherMutationCounters(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.SetWatcher(watcherMutationCounterFixture{})
	payload := srv.buildGraphStatsPayload(t.Context())
	require.Equal(t, map[string]any{"scope": "active_watchers", "echoes_attached": uint64(7), "generations_superseded": uint64(5), "waiters_completed_by_later_patch": uint64(3)}, payload["watcher_mutation_counters"])
	srv.SetWatcher(mutationTestWatcher{})
	payload = srv.buildGraphStatsPayload(t.Context())
	require.NotContains(t, payload, "watcher_mutation_counters", "an unsupported observation must not invent zero counters")
}
