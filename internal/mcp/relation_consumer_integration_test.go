package mcp

import (
	"context"
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

func TestRelationVerdictsRequireFreshDeadlineReturnAnswers(t *testing.T) {
	for _, name := range []string{"verify_change", "get_edit_plan", "change_contract", "preview_edit"} {
		t.Run(name, func(t *testing.T) {
			s, root := newRelationFixture(t)
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
				filepath.Join(root, "caller.go"): {Generation: 1, AckAt: time.Now()},
			}})
			id := targetID(t, s)
			args := map[string]any{"require_fresh": true, "wait_deadline": time.Now().Add(250 * time.Millisecond).Format(time.RFC3339Nano), "format": "json"}
			var handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
			var answerKey string
			switch name {
			case "verify_change":
				handler = s.handleVerifyChange
				answerKey = "checked_callers"
				args["changes"] = fmt.Sprintf(`[{"symbol_id":%q,"new_signature":"func Target()"}]`, id)
			case "get_edit_plan":
				handler = s.handleGetEditPlan
				answerKey = "edit_order"
				args["ids"] = id
			case "change_contract":
				handler = s.handleChangeContract
				answerKey = "verdict"
				args["source"] = "symbols"
				args["symbols"] = id
			case "preview_edit":
				handler = s.handlePreviewEdit
				answerKey = "broken_callers"
				args["diagnostics"] = false
				args["workspace_edit"] = fmt.Sprintf(`{"changes":{%q:[{"range":{"start":{"line":2,"character":5},"end":{"line":2,"character":11}},"newText":"Renamed"}]}}`, nativeFileURI(filepath.Join(root, "target.go")))
			}
			req := mcp.CallToolRequest{}
			req.Params.Name = name
			req.Params.Arguments = args
			started := time.Now()
			result, err := s.wrapToolHandler(handler)(t.Context(), req)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.IsError, "deadline is a labelled verdict, never a freshness refusal")
			require.GreaterOrEqual(t, time.Since(started), 200*time.Millisecond, "explicit bounded freshness must not be ignored")
			require.Contains(t, freshnessToolResultText(result), "pending")
			require.Contains(t, freshnessToolResultText(result), answerKey, "real verdict payload must survive deadline")
		})
	}
}
