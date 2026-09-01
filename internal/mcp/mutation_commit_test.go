package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// resultJSON parses a successful tool payload.
func resultJSON(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.NotNil(t, res)
	require.False(t, res.IsError, "expected success, got: %s", resultText(res))
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(res)), &payload))
	return payload
}

// newMutationServer builds the smallest Server the single-file mutating tools
// need: session bookkeeping, no indexer. Without an indexer the graph half of
// every receipt reports "stale", which is exactly the separation under test.
func newMutationServer() *Server {
	return &Server{session: &sessionState{}}
}

func writeFileRequest(args map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Name = "write_file"
	req.Params.Arguments = args
	return req
}

func editFileRequest(args map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = args
	return req
}

// --- Criterion: timeout BEFORE the write applies nothing -------------------

// A cancelled request must not reach the disk. The handler takes its
// cancellation gate between registering the commit and calling AtomicWriteFile,
// so the refusal is a guarantee rather than a race the caller has to reason
// about.
func TestWriteFileCancelledBeforeCommitAppliesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "never_written.go")

	s := newMutationServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := s.handleWriteFile(ctx, writeFileRequest(map[string]any{
		"path": target, "content": "package p\n",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)

	_, statErr := os.Stat(target)
	require.True(t, os.IsNotExist(statErr), "a cancelled write must not create the file")
}

// The gate that matters sits AFTER the mutation lock, not only in front of it.
// A deadline that lands while the handler is reading, matching, or parse-gating
// leaves it holding the lock with a dead context — exactly the state
// boundHandler leaves a detached handler in — and it must still refuse rather
// than commit. mutationPreCommitHook is the only way into that window.
func TestEditFileCancelledInsideCommitWindowWritesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "guarded.txt")
	require.NoError(t, os.WriteFile(target, []byte("alpha\n"), 0o644))

	s := newMutationServer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reached bool
	s.mutationPreCommitHook = func(record *mutationCommitRecord) {
		reached = true
		// The record exists before the gate, so a client can always ask what
		// happened even for a mutation that never starts.
		require.Equal(t, mutationDiskInFlight, record.diskStatus())
		cancel()
	}

	res, err := s.handleEditFile(ctx, editFileRequest(map[string]any{
		"path": target, "old_string": "alpha", "new_string": "beta",
	}))
	require.NoError(t, err)
	require.True(t, reached, "the handler must reach the commit window")
	require.True(t, res.IsError)
	require.Contains(t, resultText(res), "disk_status=not_applied")
	require.Contains(t, resultText(res), "Retrying is safe")

	content, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, "alpha\n", string(content), "the file must be untouched")

	record, ok := s.mutationCommits.recentForPath(target)
	require.True(t, ok, "the refusal must still be recorded")
	require.Equal(t, mutationDiskNotApplied, record.diskStatus())
}

// The same window for write_file, where the target does not exist yet: a
// cancelled create must leave no file behind at all.
func TestWriteFileCancelledInsideCommitWindowCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "not_created.go")

	s := newMutationServer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.mutationPreCommitHook = func(*mutationCommitRecord) { cancel() }

	res, err := s.handleWriteFile(ctx, writeFileRequest(map[string]any{
		"path": target, "content": "package p\n",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)

	_, statErr := os.Stat(target)
	require.True(t, os.IsNotExist(statErr), "a cancelled create must not leave a file")
}

// --- Criterion: timeout AFTER the write reports the commit -----------------

// The regression from the issue. boundHandler answers at the deadline while the
// handler keeps running; once that handler has committed, the answer must say
// so instead of "treat any side effect as unknown".
func TestAbandonedCallReportsCommittedDiskWrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "landed.go")

	s := newMutationServer()
	s.ToolCallTimeout = 150 * time.Millisecond

	committed := make(chan struct{})
	handler := func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		record, err := s.commitFileMutation(ctx, "write_file", "", "fp", "landed.go", target,
			[]byte("package p\n"), 0o644)
		require.NoError(t, err)
		require.Equal(t, mutationDiskCommitted, record.diskStatus())
		close(committed)
		// Stand in for the post-commit graph refresh that outran the budget:
		// mutationReindexState can fall through to a synchronous, contextless
		// IncrementalReindexRepo, which is where the reporter's 59s went.
		time.Sleep(500 * time.Millisecond)
		return mcp.NewToolResultText("applied"), nil
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = "write_file"
	res, err := s.boundToolHandler(handler)(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError)

	<-committed
	text := resultText(res)
	require.Contains(t, text, "WAS COMMITTED")
	require.Contains(t, text, "Do NOT retry")
	require.NotContains(t, text, "treat any side effect as unknown")

	// The structured tail is what a client parses.
	_, encoded, found := strings.Cut(text, "mutation_commit=")
	require.True(t, found, "abandoned response must carry a machine-readable receipt")
	var verdict mutationCommitVerdict
	require.NoError(t, json.Unmarshal([]byte(encoded), &verdict))
	require.Equal(t, mutationDiskCommitted, verdict.Status)
	require.Len(t, verdict.Snapshots, 1)
	require.Equal(t, "landed.go", verdict.Snapshots[0].Path)
	require.NotEmpty(t, verdict.Snapshots[0].NewSHA)

	// And the receipt it names must resolve.
	record, ok := s.mutationCommits.byReceipt(verdict.Snapshots[0].Receipt)
	require.True(t, ok)
	require.Equal(t, mutationDiskCommitted, record.diskStatus())
}

// An abandoned handler that refused before its write must be reported as such,
// so the client knows a retry is safe.
func TestAbandonedCallReportsNotAppliedWrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "refused.go")

	s := newMutationServer()
	s.ToolCallTimeout = 100 * time.Millisecond

	handler := func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		<-ctx.Done() // the deadline fires first
		_, err := s.commitFileMutation(ctx, "write_file", "", "fp", "refused.go", target,
			[]byte("package p\n"), 0o644)
		require.Error(t, err)
		return mcp.NewToolResultText("unreachable"), nil
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = "write_file"
	res, err := s.boundToolHandler(handler)(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError)

	// The handler races the response; wait for it to reach its gate.
	require.Eventually(t, func() bool {
		record, ok := s.mutationCommits.recentForPath("refused.go")
		return ok && record.diskStatus() == mutationDiskNotApplied
	}, 2*time.Second, 10*time.Millisecond)

	_, statErr := os.Stat(target)
	require.True(t, os.IsNotExist(statErr), "the refused write must not exist")
}

// A read tool must keep the original liveness wording — the commit machinery
// adds nothing when nothing was mutated.
func TestAbandonedReadToolKeepsGenericMessage(t *testing.T) {
	s := newMutationServer()
	s.ToolCallTimeout = 80 * time.Millisecond
	handler := func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		<-ctx.Done()
		return mcp.NewToolResultText("late"), nil
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "search_symbols"

	res, err := s.boundToolHandler(handler)(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	text := resultText(res)
	require.Contains(t, text, "treat any side effect as unknown")
	require.NotContains(t, text, "mutation_commit=")
}

// --- Criterion: the response separates disk state from graph state ---------

func TestWriteFileResponseSeparatesDiskAndGraphStatus(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "fresh.txt")

	s := newMutationServer()
	res, err := s.handleWriteFile(context.Background(), writeFileRequest(map[string]any{
		"path": target, "content": "hello\n",
	}))
	require.NoError(t, err)

	payload := resultJSON(t, res)
	require.Equal(t, mutationDiskCommitted, payload["disk_status"])
	require.NotEmpty(t, payload["mutation_receipt"])
	// No indexer is attached, so the graph genuinely cannot be refreshed. That
	// is reported as stale — separately from the committed disk write, which is
	// the distinction the issue asks for.
	require.Equal(t, mutationGraphStale, payload["graph_status"])
	require.NotEmpty(t, payload["new_sha"])
}

// --- Criterion: a queryable receipt ----------------------------------------

func TestMutationStatusResolvesReceiptPathAndListing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "queried.txt")

	s := newMutationServer()
	writeRes, err := s.handleWriteFile(context.Background(), writeFileRequest(map[string]any{
		"path": target, "content": "content\n",
	}))
	require.NoError(t, err)
	receipt, _ := resultJSON(t, writeRes)["mutation_receipt"].(string)
	require.NotEmpty(t, receipt)

	status := func(args map[string]any) map[string]any {
		req := mcp.CallToolRequest{}
		req.Params.Name = "mutation_status"
		req.Params.Arguments = args
		res, statusErr := s.handleMutationStatus(context.Background(), req)
		require.NoError(t, statusErr)
		return resultJSON(t, res)
	}

	byReceipt := status(map[string]any{"receipt": receipt})
	require.Equal(t, true, byReceipt["found"])
	require.Equal(t, mutationDiskCommitted, byReceipt["disk_status"])
	require.Equal(t, false, byReceipt["retry_safe"])
	require.Contains(t, byReceipt["guidance"], "do not re-apply")

	byPath := status(map[string]any{"path": target})
	require.Equal(t, receipt, byPath["receipt"])

	listing := status(map[string]any{})
	require.Equal(t, float64(1), listing["count"])

	unknown := status(map[string]any{"path": filepath.Join(dir, "never.txt")})
	require.Equal(t, false, unknown["found"])
	require.Equal(t, "unrecorded", unknown["disk_status"])
}

func TestMutationStatusUnknownReceiptIsAnError(t *testing.T) {
	s := newMutationServer()
	req := mcp.CallToolRequest{}
	req.Params.Name = "mutation_status"
	req.Params.Arguments = map[string]any{"receipt": "commit-does-not-exist"}
	res, err := s.handleMutationStatus(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(res), "no mutation receipt")
}

// --- Criterion: retrying after a timeout is idempotent ---------------------

// The concrete failure from the issue: a call is abandoned, the client retries,
// and the same logical change lands twice. With a mutation_id the retry replays
// the original result and writes nothing.
func TestEditFileRetryWithMutationIDDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "fields.go")
	require.NoError(t, os.WriteFile(target, []byte("type T struct {\n\ta int\n}\n"), 0o644))

	s := newMutationServer()
	args := map[string]any{
		"path":        target,
		"old_string":  "\ta int\n",
		"new_string":  "\ta int\n\texistenceSet map[string]bool\n",
		"mutation_id": "add-existence-set",
	}

	first, err := s.handleEditFile(context.Background(), editFileRequest(args))
	require.NoError(t, err)
	firstPayload := resultJSON(t, first)
	require.Equal(t, "applied", firstPayload["status"])
	require.Nil(t, firstPayload["replayed"])

	afterFirst, err := os.ReadFile(target)
	require.NoError(t, err)

	// The client never saw the response and retries the identical edit.
	second, err := s.handleEditFile(context.Background(), editFileRequest(args))
	require.NoError(t, err)
	secondPayload := resultJSON(t, second)
	require.Equal(t, true, secondPayload["replayed"])
	require.Equal(t, firstPayload["new_sha"], secondPayload["new_sha"])
	require.Equal(t, firstPayload["mutation_receipt"], secondPayload["mutation_receipt"])

	afterSecond, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, string(afterFirst), string(afterSecond))
	require.Equal(t, 1, strings.Count(string(afterSecond), "existenceSet"),
		"the retry must not duplicate the field")
}

// Without a key the tools stay as they were — the retry re-applies. This is the
// documented boundary of the guarantee, asserted so it cannot drift silently.
func TestWriteFileRetryWithoutMutationIDStillWrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "unkeyed.txt")

	s := newMutationServer()
	req := writeFileRequest(map[string]any{"path": target, "content": "one\n"})
	_, err := s.handleWriteFile(context.Background(), req)
	require.NoError(t, err)

	second, err := s.handleWriteFile(context.Background(), writeFileRequest(map[string]any{
		"path": target, "content": "two\n",
	}))
	require.NoError(t, err)
	require.Nil(t, resultJSON(t, second)["replayed"])

	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "two\n", string(content))
}

// A key reused for a different edit is a caller bug, not a retry. Treating it
// as a new mutation would silently drop one of the two edits.
func TestMutationIDReusedForDifferentEditIsRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "conflict.txt")
	require.NoError(t, os.WriteFile(target, []byte("a\nb\n"), 0o644))

	s := newMutationServer()
	_, err := s.handleEditFile(context.Background(), editFileRequest(map[string]any{
		"path": target, "old_string": "a", "new_string": "A", "mutation_id": "k1",
	}))
	require.NoError(t, err)

	res, err := s.handleEditFile(context.Background(), editFileRequest(map[string]any{
		"path": target, "old_string": "b", "new_string": "B", "mutation_id": "k1",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(res), "already used for a different edit")

	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "A\nb\n", string(content), "the refused edit must not have been applied")
}

// Concurrent retries of one keyed mutation must write once. The path lock
// serialises them; the replay check runs inside it.
func TestConcurrentKeyedRetriesWriteOnce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "race.txt")
	require.NoError(t, os.WriteFile(target, []byte("x\n"), 0o644))

	s := newMutationServer()
	args := map[string]any{
		"path": target, "old_string": "x", "new_string": "x\ny", "mutation_id": "once",
	}

	const callers = 8
	var wg sync.WaitGroup
	results := make([]map[string]any, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, callErr := s.handleEditFile(context.Background(), editFileRequest(args))
			if callErr == nil && res != nil && !res.IsError {
				var payload map[string]any
				if json.Unmarshal([]byte(resultText(res)), &payload) == nil {
					results[i] = payload
				}
			}
		}()
	}
	wg.Wait()

	applied := 0
	for _, payload := range results {
		require.NotNil(t, payload)
		if payload["replayed"] == nil {
			applied++
		}
	}
	require.Equal(t, 1, applied, "exactly one caller may perform the write")

	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "x\ny\n", string(content))
}

// --- Ledger mechanics ------------------------------------------------------

// A pending graph refresh must not read as pending forever: mutation_status
// re-reads the underlying watcher receipt before answering.
func TestMutationStatusRefreshesPendingGraphStatus(t *testing.T) {
	s := newMutationServer()
	record := s.beginMutationCommit(context.Background(), "edit_file", "", "fp", "pkg/a.go", "/abs/pkg/a.go")
	record.markCommitted("sha", 10)

	ticket := &mutationReceipt{id: "mutation-test", generation: 7, done: make(chan struct{})}
	s.mutationReceipts.Store(ticket.id, ticket)
	record.recordGraph(mutationReindexOutcome{Pending: true, Receipt: ticket.id, Generation: 7})
	require.Equal(t, mutationGraphPending, record.snapshot().GraphStatus)

	// The watcher finishes after the response was already sent.
	ticket.result = indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 7, Reindexed: true}
	ticket.completed = true
	close(ticket.done)

	payload := s.mutationStatusPayload(record)
	require.Equal(t, mutationGraphFresh, payload["graph_status"])
	require.Equal(t, mutationDiskCommitted, payload["disk_status"])
}

func TestMutationLedgerEvictsOldestPastCap(t *testing.T) {
	s := newMutationServer()
	for i := range maxMutationCommits + 10 {
		s.beginMutationCommit(context.Background(), "write_file", "", "fp",
			fmt.Sprintf("f%d.go", i), fmt.Sprintf("/abs/f%d.go", i))
	}
	s.mutationCommits.mu.Lock()
	held := len(s.mutationCommits.ordered)
	indexed := len(s.mutationCommits.byID)
	s.mutationCommits.mu.Unlock()

	require.LessOrEqual(t, held, maxMutationCommits)
	require.Equal(t, held, indexed, "the id index must not outlive the ordered list")

	_, ok := s.mutationCommits.recentForPath("f0.go")
	require.False(t, ok, "the oldest record must have been evicted")
	_, ok = s.mutationCommits.recentForPath(fmt.Sprintf("f%d.go", maxMutationCommits+9))
	require.True(t, ok, "the newest record must survive")
}

func TestGraphStatusForCoversEveryOutcome(t *testing.T) {
	require.Equal(t, mutationGraphFailed, graphStatusFor(mutationReindexOutcome{Err: context.Canceled}))
	require.Equal(t, mutationGraphPending, graphStatusFor(mutationReindexOutcome{Pending: true}))
	require.Equal(t, mutationGraphFresh, graphStatusFor(mutationReindexOutcome{Reindexed: true}))
	require.Equal(t, mutationGraphStale, graphStatusFor(mutationReindexOutcome{}))
}

// --- Multi-file writes -----------------------------------------------------

// A coordinated rename must not keep writing files after its request is dead.
// Grinding on would spread a half-applied rename across more files than
// necessary; stopping leaves receipts that say exactly how far it got.
func TestRenameStopsWritingOnceCancelled(t *testing.T) {
	dir := t.TempDir()
	writes := make([]*renameFileWrite, 0, 3)
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		abs := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(abs, []byte("package p // old\n"), 0o644))
		writes = append(writes, &renameFileWrite{
			RelPath: name, AbsPath: abs, New: []byte("package p // new\n"), NewSHA: "sha-" + name,
		})
	}

	s := newMutationServer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Let the first file commit, then kill the request inside the second.
	var seen int
	s.mutationPreCommitHook = func(*mutationCommitRecord) {
		seen++
		if seen == 2 {
			cancel()
		}
	}

	results := s.commitRenameWrites(ctx, writes)
	require.Len(t, results, 2, "the rename must stop instead of attempting every remaining file")
	require.Equal(t, "applied", results[0]["status"])
	require.Equal(t, mutationDiskCommitted, results[0]["disk_status"])
	require.Equal(t, "failed", results[1]["status"])
	require.Equal(t, mutationDiskNotApplied, results[1]["disk_status"])

	first, err := os.ReadFile(writes[0].AbsPath)
	require.NoError(t, err)
	require.Equal(t, "package p // new\n", string(first))
	for _, w := range writes[1:] {
		content, readErr := os.ReadFile(w.AbsPath)
		require.NoError(t, readErr)
		require.Equal(t, "package p // old\n", string(content), "%s must be untouched", w.RelPath)
	}
}
