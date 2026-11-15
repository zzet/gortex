package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

func TestDaemonFreshnessPreparationKeepsRelatedAndNewCallerPending(t *testing.T) {
	for _, name := range []string{"related_target", "new_caller", "unknown_pending_bytes"} {
		t.Run(name, func(t *testing.T) { daemonFreshnessDefaultRuntimeCase(t, false, name) })
	}
}

// These are separate negative observations, not extra requests in C5. They use
// its same real default daemon/session/accepted-source prerequisites and real
// held extraction; the new caller is absent from the accepted graph.
func daemonFreshnessPendingPreparationCase(t *testing.T, ctx context.Context, s *gortexmcp.Server, root string, w *indexer.MultiWatcher, gate *daemonFreshnessUnrelatedExtractor, baselineCtx context.Context, name string) {
	t.Helper()
	file, raw := "other_a.go", "package p\nfunc NewCaller() int { return Target() }\n"
	if name == "related_target" {
		file, raw = "target.go", "package p\nfunc Target() int { return 3 }\n"
	}
	path := filepath.Join(root, file)
	require.NoError(t, os.WriteFile(path, []byte(raw), 0600))
	ticket, err := w.EnqueueFileMutation(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.NotZero(t, ticket.Generation)
	select {
	case <-gate.entered:
	case <-baselineCtx.Done():
		t.Fatalf("real negative extractor entry missing: %v", baselineCtx.Err())
	}
	require.True(t, w.TierState(path).RelationsAt.IsZero())
	if name == "unknown_pending_bytes" {
		require.NoError(t, os.Remove(path))
		relative, relErr := filepath.Rel(root, path)
		require.NoError(t, relErr)
		require.Equal(t, "other_a.go", relative)
		_, statErr := os.Stat(path)
		require.ErrorIs(t, statErr, os.ErrNotExist, "actual pending source bytes must be unavailable")
		// These are one-time non-atomic observations of the real watcher, not
		// fabricated tiers or an authority for the answer. The registered refusal
		// below must itself name this exact unknown path.
		canonical, known := w.TierPaths()[filepath.Clean(path)]
		require.True(t, known)
		require.NotZero(t, canonical.Generation)
		require.GreaterOrEqual(t, canonical.Generation, ticket.Generation)
		require.True(t, canonical.RelationsAt.IsZero())
		require.False(t, canonical.Failed)
		t.Logf("ACTUAL_UNKNOWN_PENDING_PATH path=%q ticket_generation=%d observed_canonical_generation=%d relations_pending=true stat_error=%v snapshot_non_atomic=true", path, ticket.Generation, canonical.Generation, statErr)
	}
	client, ok := ctx.Value(daemonFreshnessTransportKey{}).(*daemonFreshnessTransport)
	require.True(t, ok)
	require.Same(t, s, client.dispatcher.srv)
	frame, err := buildToolCallFrameWithDefault(20, "get_callers", map[string]any{"id": "repo/target.go::Target", "format": "json", "require_fresh": true, "freshness_scope": "relations", "wait_deadline": time.Now().Add(2 * time.Second).Format(time.RFC3339Nano)}, true)
	require.NoError(t, err)
	require.Equal(t, client.root, requestCWD(client.session, frame))
	response, err := client.dispatcher.Dispatch(ctx, client.session, frame)
	t.Logf("ACTUAL_PREPARATION_NEGATIVE case=%s original_response=%s", name, response)
	require.NoError(t, err)
	var envelope struct {
		Error  any `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response, &envelope))
	require.Nil(t, envelope.Error)
	require.True(t, envelope.Result.IsError, "related/new-caller/unknown physical scope must never claim fresh")
	require.NotEmpty(t, envelope.Result.Content)
	require.Contains(t, envelope.Result.Content[0].Text, "pending")
	require.Contains(t, envelope.Result.Content[0].Text, file)
	gate.releaseAndJoin()
	require.Positive(t, gate.entries.Load())
	require.Equal(t, gate.entries.Load(), gate.completed.Load())
}
