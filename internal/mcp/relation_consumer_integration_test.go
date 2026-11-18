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

func TestDeadCodeWaitKeepsDerivedScopeAndOriginalPassDeadline(t *testing.T) {
	s, root := newRelationFixture(t)
	path := writeUnindexed(t, root, "unused.go", "package p\nfunc unused() {}\nfunc secondUnused() {}\nfunc thirdUnused() {}\n")
	require.NoError(t, s.indexer.IndexFile(path))
	now := time.Now()
	s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{path: {Generation: 1, AckAt: now, FileGraphAt: now, RelationsAt: now}}})
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_dead_code"
	req.Params.Arguments = map[string]any{"require_fresh": true, "freshness_scope": "relations", "wait_deadline": time.Now().Add(250 * time.Millisecond).Format(time.RFC3339Nano), "format": "json"}
	started := time.Now()
	res, err := s.wrapToolHandler(s.handleFindDeadCode)(t.Context(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.GreaterOrEqual(t, time.Since(started), 200*time.Millisecond, "a weaker request tier cannot bypass derived publication")
	require.Less(t, time.Since(started), time.Second, "candidate waits share one original deadline")
	payload := lifecycleResultPayload(t, res)
	require.EqualValues(t, 3, payload["pending_unknown_count"])
	require.Equal(t, "derived", payload["freshness_scope"])
	require.Empty(t, payload["dead_code"])
}

func TestDeadCodeDerivedWaitIncludesPendingInboundAndIgnoresUnrelated(t *testing.T) {
	for _, inbound := range []bool{false, true} {
		t.Run(fmt.Sprint(inbound), func(t *testing.T) {
			s, root := newRelationFixture(t)
			file := writeUnindexed(t, root, "unused.go", "package p\nfunc unused() {}\n")
			require.NoError(t, s.indexer.IndexFile(file))
			text := "package p\nfunc Namer() {}\n"
			if inbound {
				text = "package p\nfunc Namer() { unused() }\n"
			}
			pending := writeUnindexed(t, root, "namer.go", text)
			now := time.Now()
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{pending: {Generation: 1, AckAt: now, FileGraphAt: now, RelationsAt: now}}})
			req := mcp.CallToolRequest{}
			req.Params.Name = "find_dead_code"
			req.Params.Arguments = map[string]any{"require_fresh": true, "wait_deadline": time.Now().Add(500 * time.Millisecond).Format(time.RFC3339Nano), "format": "json"}
			started := time.Now()
			res, err := s.wrapToolHandler(s.handleFindDeadCode)(t.Context(), req)
			require.NoError(t, err)
			require.False(t, res.IsError)
			if inbound {
				require.GreaterOrEqual(t, time.Since(started), 400*time.Millisecond)
				require.Contains(t, freshnessToolResultText(res), "pending_unknown")
			} else {
				require.Less(t, time.Since(started), 300*time.Millisecond, "an unrelated derived gap must not create a workspace wait")
				require.NotContains(t, freshnessToolResultText(res), "pending_unknown")
			}
		})
	}
}

func TestOrdinaryRelationReadStillRefusesExpiredRequireFresh(t *testing.T) {
	s, root := newRelationFixture(t)
	s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{filepath.Join(root, "caller.go"): {Generation: 1, AckAt: time.Now()}}})
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": targetID(t, s), "require_fresh": true, "wait_deadline": time.Now().Add(100 * time.Millisecond).Format(time.RFC3339Nano), "format": "json"}
	res, err := s.wrapToolHandler(s.handleGetCallers)(t.Context(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "ordinary read freshness contract remains a refusal")
}

func TestFacadeVerifyDeadlineReturnsLabelledVerdict(t *testing.T) {
	for _, format := range []string{"json", "toon", "gcx", "compact"} {
		t.Run(format, func(t *testing.T) {
			s, root := newRelationFixture(t)
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{filepath.Join(root, "caller.go"): {Generation: 1, AckAt: time.Now()}}})
			output := map[string]any{"format": format}
			if format == "compact" {
				output["format"] = "json"
				output["compact"] = true
			}
			req := mcp.CallToolRequest{}
			req.Params.Name = "change"
			req.Params.Arguments = map[string]any{"operation": "verify", "source": map[string]any{"changes": []any{map[string]any{"symbol_id": targetID(t, s), "new_signature": "func Target()"}}}, "options": map[string]any{"require_fresh": true, "wait_deadline": time.Now().Add(150 * time.Millisecond).Format(time.RFC3339Nano)}, "output": output}
			res, err := s.handleFacade(t.Context(), "change", req)
			require.NoError(t, err)
			require.False(t, res.IsError, freshnessToolResultText(res))
			require.Contains(t, freshnessToolResultText(res), "pending")
		})
	}
}

func TestRelationVerdictCallerCancellationIsNotFreshnessExpiry(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			s, root := newRelationFixture(t)
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
				filepath.Join(root, "caller.go"): {Generation: 1, AckAt: time.Now()},
			}})
			req := mcp.CallToolRequest{}
			req.Params.Name = "verify_change"
			req.Params.Arguments = map[string]any{
				"changes":       fmt.Sprintf(`[{"symbol_id":%q,"new_signature":"func Target()"}]`, targetID(t, s)),
				"require_fresh": true, "wait_deadline": time.Now().Add(time.Second).Format(time.RFC3339Nano), "format": "json",
			}
			var ctx context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Millisecond))
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(t.Context())
				timer := time.AfterFunc(100*time.Millisecond, cancel)
				defer timer.Stop()
			}
			defer cancel()
			result, err := s.wrapToolHandler(s.handleVerifyChange)(ctx, req)
			require.ErrorIs(t, err, want, "caller cancellation must not become a labelled successful verdict")
			require.Nil(t, result)
		})
	}
}
