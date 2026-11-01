package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestFreshnessRefusalHintLists(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Nanosecond}
	newID := func() string { return fmt.Sprintf("mutation-%d", mutationReceiptSequence.Add(1)) }
	failed := pendingFreshnessReceipt(s, newID(), "fixture", "/fixture/failed.go", 1)
	completeFreshnessReceipt(failed, indexer.MutationResult{Err: errors.New("patch failed")})
	unkeyed := pendingFreshnessReceipt(s, newID(), "fixture", "/fixture/other.go", 2)
	completeFreshnessReceipt(unkeyed, indexer.MutationResult{}) // No confirmed reindex.
	pending := pendingFreshnessReceipt(s, newID(), "fixture", "/fixture/pending.go", 3)
	for _, tc := range []struct {
		receipt *mutationReceipt
		key     string
	}{{failed, "original-edit"}, {unkeyed, ""}} {
		record := s.beginMutationCommit(context.Background(), "edit_file", tc.key, "", tc.receipt.path, tc.receipt.path)
		record.recordGraph(mutationReindexOutcome{Receipt: tc.receipt.id, Err: errors.New("patch failed")})
	}

	err := s.awaitMutationFreshnessForRepos(context.Background(), "fixture")
	require.Error(t, err)
	message := err.Error()
	require.True(t, strings.HasPrefix(message, "graph freshness unavailable"), message)
	entries := strings.Split(message, "; ")
	require.Len(t, entries, 4, "three receipt entries and one shared hint: %s", message)
	for i, receipt := range []*mutationReceipt{failed, unkeyed, pending} {
		require.Contains(t, entries[i], "receipt="+receipt.id+" ")
		require.Equal(t, 1, strings.Count(message, "receipt="+receipt.id+" "))
		require.Contains(t, entries[i], "(query: change.receipt options.receipt="+receipt.id+")")
	}
	require.Contains(t, entries[0], " commit=commit-")
	require.Contains(t, entries[0], ` mutation_id="original-edit"`)
	require.Contains(t, entries[1], " commit=commit-")
	require.NotContains(t, entries[1], "mutation_id=")
	require.NotContains(t, entries[2], "commit=")
	for _, want := range []string{"later successful edit of the same path", "workspace_admin operation=reindex", "reindex_repository", "paths=[P]", "exactly that single file", "retention", "last resort"} {
		require.Contains(t, entries[3], want)
	}
	for _, unavailable := range []string{"reconcile", "retry_exhausted", "retry-exhausted", "ETA"} {
		require.NotContains(t, message, unavailable)
	}
}

func TestFreshnessRefusalPublicationPhases(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Nanosecond}
	id := fmt.Sprintf("mutation-%d", mutationReceiptSequence.Add(1))
	receipt := pendingFreshnessReceipt(s, id, "fixture", "/fixture/phases.go", 7)
	origin := time.Now()
	record := indexer.DefaultPublicationPhases().Begin("hint:"+t.Name(), id, indexer.PublicationSourceMCPEdit, origin)
	for i, phase := range []indexer.PublicationPhase{
		indexer.PublicationHandlerStarted, indexer.PublicationReceiptCommitted,
		indexer.PublicationTicketEnqueued, indexer.PublicationAdmitted, indexer.PublicationTicketFailed,
	} {
		record.MarkAt(phase, origin.Add(time.Duration(i+1)*time.Millisecond))
	}
	completeFreshnessReceipt(receipt, indexer.MutationResult{Err: errors.New("failed publication")})
	err := s.awaitMutationFreshnessForRepos(context.Background(), "fixture")
	require.Error(t, err)
	require.Contains(t, err.Error(), `publication_phases=[...+3,"ticket_enqueued"@3.0ms,"admitted"@4.0ms,"ticket_failed"@5.0ms]`)
	require.NotContains(t, err.Error(), `"handler_started"`)
}

func TestFreshnessRefusalAnnotationBoundsAndScope(t *testing.T) {
	s := &Server{}
	id := fmt.Sprintf("mutation-%d", mutationReceiptSequence.Add(1))
	record := s.beginMutationCommit(context.Background(), "edit_file", strings.Repeat("\n", 10000), "", "file.go", "/fixture/file.go")
	record.recordGraph(mutationReindexOutcome{Pending: true, Receipt: id})
	hint := s.mutationFreshnessReceiptHint(context.Background(), id)
	require.Less(t, len(hint), 1000)
	require.Contains(t, hint, "(truncated)")
	require.NotContains(t, hint, "\n", "caller keys must not inject diagnostic lines")
	record.mu.Lock()
	record.checkoutID = "other-checkout"
	record.mu.Unlock()
	require.NotContains(t, s.mutationFreshnessReceiptHint(context.Background(), id), "commit=")

	snapshot := indexer.PublicationPhaseSnapshot{}
	for i := range 100 {
		snapshot.Phases = append(snapshot.Phases, indexer.PublicationPhaseOffset{
			Phase: indexer.PublicationPhase(strings.Repeat("\n", 10000)), OffsetMS: float64(i),
		})
	}
	summary := mutationFreshnessPhaseSummary(snapshot)
	require.Less(t, len(summary), 1000)
	require.Contains(t, summary, "...+97")
	require.NotContains(t, summary, "\n")
}

// Exercise the public call shapes against real handlers and an owned working
// tree. The existing watcher seam supplies a failed publication result after
// the edit acknowledges; this does not inject a real store patch failure.
func TestRelationFreshnessRecoveryFlow(t *testing.T) {
	s, root := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"init", "--quiet"}, {"add", "main.go"},
		{"-c", "user.name=Receipt Test", "-c", "user.email=receipt@example.invalid", "-c", "commit.gpgSign=false", "commit", "--quiet", "-m", "fixture"},
	} {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	call := func(facade string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		req := mcp.CallToolRequest{}
		req.Params.Name, req.Params.Arguments = facade, args
		res, err := s.handleFacade(ctx, facade, req)
		require.NoError(t, err)
		return res
	}

	done := make(chan indexer.MutationResult, 1)
	defer close(done)
	s.SetWatcher(mutationTestWatcher{done: done, generation: 7})
	s.mutationReindexWait = time.Nanosecond
	target := filepath.Join(root, "main.go")
	edit := resultJSON(t, call("edit", map[string]any{
		"operation": "file", "target": map[string]any{"file": target},
		"match": "func helper() {}", "replacement": "func helper() { println(42) }",
		"options": map[string]any{"mutation_id": "recovery-flow"},
	}))
	id, ok := edit["reindex_receipt"].(string)
	require.True(t, ok, "the edit must acknowledge the pending publication: %#v", edit)
	require.NotEmpty(t, id)
	done <- indexer.MutationResult{RequestedGeneration: 7, Err: errors.New("injected graph publication failure")}
	waitCheckoutReceipt(t, s, id)

	detectArgs := map[string]any{"operation": "detect", "options": map[string]any{"scope": "unstaged"}}
	partial := resultJSON(t, call("change", detectArgs))
	require.Equal(t, false, partial["complete"])
	require.Equal(t, "failed", partial["graph_freshness"])
	require.NotEmpty(t, partial["recovery_hint"])
	status := resultJSON(t, call("change", map[string]any{
		"operation": "receipt", "options": map[string]any{"mutation_id": id},
	}))
	require.Equal(t, "graph_refresh", status["receipt_kind"])
	require.Equal(t, "this is a graph freshness receipt id, not a caller mutation_id", status["note"])

	resultJSON(t, call("workspace_admin", map[string]any{
		"operation": "reindex", "options": map[string]any{"paths": []any{target}},
	}))
	resultJSON(t, call("change", detectArgs))
	// Recovery did not erase the failed ticket's diagnostic identity.
	status = resultJSON(t, call("change", map[string]any{
		"operation": "receipt", "options": map[string]any{"receipt": id},
	}))
	require.Equal(t, id, status["receipt"])
	require.Equal(t, mutationGraphFresh, status["graph_status"])
}
