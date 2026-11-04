package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/indexer"
)

func TestRepoReadinessForwardsNativeStateOriginAcrossAnnouncements(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newReadinessBroadcaster(fake, zap.NewNop())
	b.subscribe("owned-session")
	s := &Server{readinessBroadcaster: b}
	origin := time.Date(2001, 2, 3, 4, 5, 6, 123, time.FixedZone("origin", 3600))
	record := indexer.RepoLoad{Root: "/owned/primary", Prefix: "app", State: indexer.RepoLoadReady, Since: origin}
	before := time.Now()
	s.PublishRepoReadiness(record)
	s.PublishRepoReadiness(record)
	after := time.Now()
	calls := fake.snapshot()
	require.Len(t, calls, 2)
	for _, call := range calls {
		require.Equal(t, "notifications/workspace_readiness", call.method)
		require.Equal(t, origin.UTC().Format(time.RFC3339Nano), call.params["repo_state_since"])
		announced, err := time.Parse(time.RFC3339Nano, call.params["ts"].(string))
		require.NoError(t, err)
		require.False(t, announced.Before(before))
		require.False(t, announced.After(after))
		require.NotEqual(t, call.params["repo_state_since"], call.params["ts"], "announcement time must not become registration time")
	}
}

func TestRepoReadinessOmitsUnknownOriginInsteadOfInheritingSnapshot(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newReadinessBroadcaster(fake, zap.NewNop())
	b.publish(map[string]any{"phase": "warming", "repo_state_since": "stale"})
	b.subscribe("owned-session")
	s := &Server{readinessBroadcaster: b}
	s.PublishRepoReadiness(indexer.RepoLoad{Root: "/owned/primary", Prefix: "app", State: indexer.RepoLoadReady})
	calls := fake.snapshot()
	require.Len(t, calls, 2)
	_, exists := calls[1].params["repo_state_since"]
	require.False(t, exists, "unknown native origin must stay unknown")
	require.NotEmpty(t, calls[1].params["ts"], "normal announcement time remains available")
}
