package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This is fixture setup readiness, not the C5 timed first-response oracle.
// The existing baselineCtx (created before write/status) is shared unchanged
// across every reply and wait. Only the documented late related-file partial
// may progress; the original transport helper already fails RPC/tool errors.
func daemonBaselineReadiness(ctx context.Context, call func(context.Context) map[string]any, observe func(int, string)) (map[string]any, error) {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body := call(ctx)
		tier, ok := body["freshness"].(map[string]any)
		if !ok {
			return body, fmt.Errorf("baseline reply lacks actual freshness")
		}
		if tier["graph_freshness"] == "fresh" {
			if tier["complete"] == false || tier["freshness_scope"] != "relations" || tier["fresh"] != true || tier["pending_file_count"] != nil && tier["pending_file_count"] != float64(0) {
				return body, fmt.Errorf("baseline fresh reply is incomplete")
			}
			if err := ctx.Err(); err != nil {
				return body, err
			}
			if observe != nil {
				observe(attempt, "FRESH")
			}
			return body, nil
		}
		if !daemonBaselineRelatedPartial(body, tier) {
			return body, fmt.Errorf("unexpected baseline freshness reply: %v", tier)
		}
		if observe != nil {
			observe(attempt, "PARTIAL")
		}
		// Match the existing watched-freshness poll cadence. This timer does
		// not create or reset a caller deadline; cancellation always wins.
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return body, ctx.Err()
		case <-timer.C:
		}
	}
}

func daemonBaselineRelatedPartial(body, tier map[string]any) bool {
	if tier["graph_freshness"] != "pending" || tier["freshness_scope"] != "relations" || tier["fresh"] != true || tier["complete"] != false || tier["pending_file_count"] != float64(1) {
		return false
	}
	for _, field := range []string{"file_reason", "reason", "recovery_hint", "background_admission"} {
		if value, exists := tier[field]; exists && value != "" {
			return false
		}
	}
	paths, ok := tier["pending_files"].([]any)
	if !ok || len(paths) != 1 || paths[0] != "repo/target.go" {
		return false
	}
	edges, ok := body["edges"].([]any)
	if !ok {
		return false
	}
	for _, value := range edges {
		edge, ok := value.(map[string]any)
		if ok && edge["from"] == "repo/caller.go::Caller" && edge["to"] == "repo/target.go::Target" && edge["kind"] == "calls" && edge["file_path"] == "repo/caller.go" {
			return true
		}
	}
	return false
}

func daemonBaselineReply(state string) map[string]any {
	tier := map[string]any{"graph_freshness": state, "freshness_scope": "relations", "fresh": true}
	if state == "pending" {
		tier["complete"] = false
		tier["pending_file_count"] = float64(1)
		tier["pending_files"] = []any{"repo/target.go"}
	}
	return map[string]any{
		"freshness": tier,
		"edges": []any{map[string]any{
			"from": "repo/caller.go::Caller", "to": "repo/target.go::Target", "kind": "calls", "file_path": "repo/caller.go",
		}},
	}
}

func TestDaemonBaselineReadinessKeepsSharedCallerAndPartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var states []string
	calls := 0
	body, err := daemonBaselineReadiness(ctx, func(actual context.Context) map[string]any {
		require.Same(t, ctx, actual, "no new/reset context per reply")
		actualDeadline, _ := actual.Deadline()
		require.Equal(t, deadline, actualDeadline)
		calls++
		if calls == 1 {
			return daemonBaselineReply("pending")
		}
		return daemonBaselineReply("fresh")
	}, func(_ int, state string) { states = append(states, state) })
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, []string{"PARTIAL", "FRESH"}, states, "the first reply stays partial, never first-response PASS")
	require.Equal(t, "fresh", body["freshness"].(map[string]any)["graph_freshness"])
}

func TestDaemonBaselineReadinessRejectsUnexpectedReplies(t *testing.T) {
	for _, name := range []string{"unknown", "failed", "wrong_pending_path", "missing_partial", "wrong_caller", "file_reason", "store_busy", "fresh_with_pending"} {
		t.Run(name, func(t *testing.T) {
			body := daemonBaselineReply("pending")
			tier := body["freshness"].(map[string]any)
			switch name {
			case "unknown", "failed":
				tier["graph_freshness"] = name
			case "wrong_pending_path":
				tier["pending_files"] = []any{"repo/foreign.go"}
			case "missing_partial":
				delete(tier, "complete")
			case "wrong_caller":
				body["edges"].([]any)[0].(map[string]any)["to"] = "repo/foreign.go::Other"
			case "file_reason":
				tier["file_reason"] = "external_write"
			case "store_busy":
				tier["reason"] = "store_busy"
			case "fresh_with_pending":
				tier["graph_freshness"] = "fresh"
			}
			calls := 0
			_, err := daemonBaselineReadiness(t.Context(), func(context.Context) map[string]any { calls++; return body }, nil)
			require.Error(t, err)
			require.Equal(t, 1, calls, "unexpected replies are fatal, not retried")
		})
	}
}

func TestDaemonBaselineReadinessStopsOnOriginalContext(t *testing.T) {
	t.Run("cancel_after_partial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		var states []string
		_, err := daemonBaselineReadiness(ctx, func(context.Context) map[string]any {
			calls++
			cancel()
			return daemonBaselineReply("pending")
		}, func(_ int, state string) { states = append(states, state) })
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, calls)
		require.Equal(t, []string{"PARTIAL"}, states)
	})
	t.Run("already_expired", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
		defer cancel()
		calls := 0
		_, err := daemonBaselineReadiness(ctx, func(context.Context) map[string]any { calls++; return daemonBaselineReply("fresh") }, nil)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Zero(t, calls)
	})
}
