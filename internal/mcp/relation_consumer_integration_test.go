package mcp

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestVerifyChangeRequireFreshDeadlineStillReturnsLabelledVerdict(t *testing.T) {
	s, root := newRelationFixture(t)
	s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
		filepath.Join(root, "caller.go"): {Generation: 1, AckAt: time.Now()},
	}})
	req := mcp.CallToolRequest{}
	req.Params.Name = "verify_change"
	req.Params.Arguments = map[string]any{
		"changes":       fmt.Sprintf(`[{"symbol_id":%q,"new_signature":"func Target()"}]`, targetID(t, s)),
		"require_fresh": true, "freshness_scope": "relations", "wait_deadline": time.Now().Add(250 * time.Millisecond).Format(time.RFC3339Nano), "format": "json",
	}
	result, err := s.wrapToolHandler(s.handleVerifyChange)(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "a bounded relation verdict must remain an answer when its freshness deadline expires")
	require.Contains(t, freshnessToolResultText(result), "pending")
	require.Contains(t, freshnessToolResultText(result), "checked_callers", "the real verdict handler must run")
}

func TestDeadCodeRequireFreshWaitsForScopedDerivedDeadline(t *testing.T) {
	s, root := newRelationFixture(t)
	unused := writeUnindexed(t, root, "unused.go", "package p\nfunc unused() {}\n")
	require.NoError(t, s.indexer.IndexFile(unused))
	now := time.Now()
	s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
		unused: {Generation: 1, AckAt: now, FileGraphAt: now, RelationsAt: now},
	}})
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_dead_code"
	req.Params.Arguments = map[string]any{"require_fresh": true, "wait_deadline": time.Now().Add(250 * time.Millisecond).Format(time.RFC3339Nano), "format": "json"}
	started := time.Now()
	result, err := s.wrapToolHandler(s.handleFindDeadCode)(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)
	require.GreaterOrEqual(t, time.Since(started), 200*time.Millisecond, "relations publication must not bypass the requested per-candidate derived wait")
	require.Contains(t, freshnessToolResultText(result), "pending_unknown")
}
