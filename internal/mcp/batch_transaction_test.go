package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

func newAtomicBatchTestServer(t *testing.T, watcher watcherHistory) *Server {
	t.Helper()
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	return &Server{
		watcher:             watcher,
		session:             newSessionState(),
		mutationReindexWait: 100 * time.Millisecond,
	}
}

func writeAtomicBatchFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readAtomicBatchFixture(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func atomicFileEdit(path, oldText, newText string) batchEditItem {
	return batchEditItem{Op: "edit_file", Path: path, OldString: oldText, NewString: newText}
}

func TestBatchEditSchemaAdvertisesAtomicStatusProtocol(t *testing.T) {
	g := graph.New()
	s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	legacy, ok := s.facades.legacy("batch_edit")
	if !ok {
		t.Fatal("batch_edit is not registered in the facade registry")
	}
	properties := legacy.tool.InputSchema.Properties
	for _, name := range []string{"edits", "transaction_id", "status_only", "dry_run"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("batch_edit schema is missing %q", name)
		}
	}
	for _, name := range legacy.tool.InputSchema.Required {
		if name == "edits" {
			t.Fatal("edits must be optional at schema level so status-only calls validate")
		}
	}
	if !strings.Contains(legacy.tool.Description, "restores all touched files") || !strings.Contains(legacy.tool.Description, "daemon restart") {
		t.Fatalf("batch_edit description does not state atomic/durable behavior: %q", legacy.tool.Description)
	}
	for _, operation := range []string{"edit_symbol", "edit_file", "move_file", "delete_file"} {
		if !strings.Contains(legacy.tool.Description, operation) {
			t.Fatalf("batch_edit description does not advertise %s: %q", operation, legacy.tool.Description)
		}
	}
}

func TestAtomicBatchEditFileThroughSymlinkPreservesTargetPermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	target := writeAtomicBatchFixture(t, dir, "target.txt", "before\n")
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileEdit(link, "before", "after"),
	}, "edit-file-symlink-permissions")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "committed" {
		t.Fatalf("receipt = %+v", receipt)
	}
	info, err := os.Stat(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("replacement permissions = %04o, want 0600", got)
	}
	if got := readAtomicBatchFixture(t, link); got != "after\n" {
		t.Fatalf("replacement content = %q", got)
	}
}

func TestBatchTransactionDefaultIDsAreUnique(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	first, err := normalizeBatchTransactionID("", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	second, err := normalizeBatchTransactionID("", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("default transaction IDs collided: %q", first)
	}
	explicit, err := normalizeBatchTransactionID("caller-key", fingerprint)
	if err != nil || explicit != "caller-key" {
		t.Fatalf("explicit id = %q, err=%v", explicit, err)
	}
}

func TestAtomicBatchSameFileIsSequentialAndIdempotent(t *testing.T) {
	var scheduled atomic.Int64
	s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
	dir := t.TempDir()
	path := writeAtomicBatchFixture(t, dir, "same.txt", "alpha beta\n")
	edits := []batchEditItem{
		atomicFileEdit(path, "alpha", "ALPHA"),
		atomicFileEdit(path, "beta", "BETA"),
	}
	var writes atomic.Int64
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		writes.Add(1)
		return agents.AtomicWriteFile(path, content, mode)
	}

	receipt, err := s.runBatchTransaction(context.Background(), edits, "same-file")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "committed" || receipt.DiskStatus != "committed" || receipt.GraphStatus != "fresh" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got := readAtomicBatchFixture(t, path); got != "ALPHA BETA\n" {
		t.Fatalf("content = %q", got)
	}
	if len(receipt.Files) != 1 || receipt.Summary["applied"] != 2 {
		t.Fatalf("files/summary = %d %+v", len(receipt.Files), receipt.Summary)
	}
	if writes.Load() != 1 || scheduled.Load() != 1 {
		t.Fatalf("writes=%d scheduled=%d, want one physical commit and one reindex", writes.Load(), scheduled.Load())
	}

	retry, err := s.runBatchTransaction(context.Background(), edits, "same-file")
	if err != nil {
		t.Fatal(err)
	}
	if retry.TransactionID != receipt.TransactionID || writes.Load() != 1 || scheduled.Load() != 1 {
		t.Fatalf("idempotent retry wrote or reindexed again: receipt=%+v writes=%d scheduled=%d", retry, writes.Load(), scheduled.Load())
	}
	_, err = s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileEdit(path, "ALPHA", "different"),
	}, "same-file")
	if err == nil || !strings.Contains(err.Error(), "different edit payload") {
		t.Fatalf("payload conflict error = %v", err)
	}

	// Idempotency is durable, not just daemon-local.
	restarted := &Server{watcher: mutationTestWatcher{}, session: newSessionState()}
	restarted.batchWriteOverride = func(string, []byte, os.FileMode) error {
		t.Fatal("durable retry attempted a write")
		return nil
	}
	persisted, err := restarted.runBatchTransaction(context.Background(), edits, "same-file")
	if err != nil || persisted.Status != "committed" || persisted.GraphStatus != "fresh" {
		t.Fatalf("persisted retry = %+v, err=%v", persisted, err)
	}
}

func TestAtomicBatchConcurrentIdempotencyWritesOnce(t *testing.T) {
	var scheduled atomic.Int64
	s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
	path := writeAtomicBatchFixture(t, t.TempDir(), "concurrent.txt", "old\n")
	edits := []batchEditItem{atomicFileEdit(path, "old", "new")}
	var writes atomic.Int64
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		writes.Add(1)
		return agents.AtomicWriteFile(path, content, mode)
	}

	const callers = 12
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			receipt, err := s.runBatchTransaction(context.Background(), edits, "concurrent-key")
			if err == nil && (receipt.Status != "committed" || receipt.GraphStatus != "fresh") {
				err = fmt.Errorf("unexpected receipt: %+v", receipt)
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if writes.Load() != 1 || scheduled.Load() != 1 {
		t.Fatalf("concurrent retries wrote=%d scheduled=%d, want 1/1", writes.Load(), scheduled.Load())
	}
	if got := readAtomicBatchFixture(t, path); got != "new\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestBatchTransactionPublishCopiesReceiptStorage(t *testing.T) {
	state := &batchTransactionState{done: make(chan struct{})}
	completedAt := time.Now().UTC()
	original := completedAt
	published := batchTransactionReceipt{
		Status:      "prepared",
		Results:     []batchEditResult{{FilePath: "a.txt", Status: "validated"}},
		Files:       []batchTransactionFile{{Path: "/tmp/a.txt"}},
		Summary:     map[string]int{"total": 1},
		CompletedAt: &completedAt,
	}
	state.publish(published, false)

	// A publisher goes on mutating its own receipt after publishing; none of
	// that may reach the state, which concurrent readers snapshot under RLock.
	published.Results[0].Status = "applied"
	published.Files[0].ReindexReceipt = "receipt-1"
	published.Summary["total"] = 99
	*published.CompletedAt = completedAt.Add(time.Hour) // also rewrites completedAt

	stored := state.snapshot()
	if stored.Results[0].Status != "validated" {
		t.Errorf("Results storage is shared with the publisher: %q", stored.Results[0].Status)
	}
	if stored.Files[0].ReindexReceipt != "" {
		t.Errorf("Files storage is shared with the publisher: %q", stored.Files[0].ReindexReceipt)
	}
	if stored.Summary["total"] != 1 {
		t.Errorf("Summary map is shared with the publisher: %d", stored.Summary["total"])
	}
	if !stored.CompletedAt.Equal(original) {
		t.Errorf("CompletedAt is shared with the publisher: %s", stored.CompletedAt)
	}

	// The snapshot side owes the same isolation in the other direction.
	stored.Results[0].Status = "failed"
	stored.Summary["total"] = 7
	if again := state.snapshot(); again.Results[0].Status != "validated" || again.Summary["total"] != 1 {
		t.Errorf("snapshot storage is shared with the state: %+v", again)
	}
}

func TestAtomicBatchSnapshotIsolatedFromCommitInProgress(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	path := writeAtomicBatchFixture(t, t.TempDir(), "isolated.txt", "old\n")
	edits := []batchEditItem{atomicFileEdit(path, "old", "new")}
	fingerprint := batchEditFingerprint(edits)

	committing := make(chan struct{})
	s.batchWriteOverride = func(target string, content []byte, mode os.FileMode) error {
		// Signalled without waiting on the reader: a back-edge would order the
		// commit loop's writes behind the reader's loads and hide the race.
		close(committing)
		return agents.AtomicWriteFile(target, content, mode)
	}

	stop := make(chan struct{})
	torn := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-committing
		for {
			select {
			case <-stop:
				return
			default:
			}
			// The call path the race detector reported: an idempotent retry
			// classifies the in-flight transaction by snapshotting its receipt.
			state, _, err := s.loadOrCreateBatchTransaction(context.Background(), "isolated-key", fingerprint)
			if err != nil {
				continue
			}
			observed := state.snapshot()
			if observed.Status == "committed" {
				continue
			}
			for _, result := range observed.Results {
				if result.Status == "applied" {
					select {
					case torn <- fmt.Sprintf("status %q carried an applied result", observed.Status):
					default:
					}
					return
				}
			}
		}
	}()

	receipt, err := s.runBatchTransaction(context.Background(), edits, "isolated-key")
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "committed" || receipt.GraphStatus != "fresh" {
		t.Fatalf("receipt = %+v", receipt)
	}
	select {
	case message := <-torn:
		t.Fatalf("reader observed a receipt the committer was still mutating: %s", message)
	default:
	}
}

func TestAtomicBatchPreflightFailureWritesNothing(t *testing.T) {
	var scheduled atomic.Int64
	s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
	dir := t.TempDir()
	a := writeAtomicBatchFixture(t, dir, "a.txt", "one\n")
	b := writeAtomicBatchFixture(t, dir, "b.txt", "two\n")
	var writes atomic.Int64
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		writes.Add(1)
		return agents.AtomicWriteFile(path, content, mode)
	}

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileEdit(a, "one", "ONE"),
		atomicFileEdit(b, "missing", "TWO"),
	}, "preflight")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || receipt.GraphStatus != "not_started" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if writes.Load() != 0 || scheduled.Load() != 0 {
		t.Fatalf("writes=%d scheduled=%d", writes.Load(), scheduled.Load())
	}
	if readAtomicBatchFixture(t, a) != "one\n" || readAtomicBatchFixture(t, b) != "two\n" {
		t.Fatal("preflight failure changed disk")
	}
}

func TestAtomicBatchCommitFailureRollsBackEveryFile(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failAt     int64
		afterWrite bool
	}{
		{name: "second-before-write", failAt: 2},
		{name: "second-after-write", failAt: 2, afterWrite: true},
		{name: "third-before-write", failAt: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var scheduled atomic.Int64
			s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
			dir := t.TempDir()
			paths := []string{
				writeAtomicBatchFixture(t, dir, "a.txt", "a0\n"),
				writeAtomicBatchFixture(t, dir, "b.txt", "b0\n"),
				writeAtomicBatchFixture(t, dir, "c.txt", "c0\n"),
			}
			edits := make([]batchEditItem, 0, len(paths))
			for i, path := range paths {
				edits = append(edits, atomicFileEdit(path, fmt.Sprintf("%c0", 'a'+i), fmt.Sprintf("%c1", 'a'+i)))
			}
			var calls atomic.Int64
			s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
				call := calls.Add(1)
				if call == tc.failAt && !tc.afterWrite {
					return errors.New("injected commit failure")
				}
				if err := agents.AtomicWriteFile(path, content, mode); err != nil {
					return err
				}
				if call == tc.failAt {
					return errors.New("injected post-write failure")
				}
				return nil
			}

			receipt, err := s.runBatchTransaction(context.Background(), edits, "rollback-"+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Status != "aborted" || receipt.DiskStatus != "rolled_back" || receipt.GraphStatus != "not_started" {
				t.Fatalf("receipt = %+v", receipt)
			}
			if receipt.Summary["failed"] != 1 || receipt.Summary["applied"] != 0 {
				t.Fatalf("summary = %+v", receipt.Summary)
			}
			for i, path := range paths {
				want := fmt.Sprintf("%c0\n", 'a'+i)
				if got := readAtomicBatchFixture(t, path); got != want {
					t.Fatalf("%s = %q, want rollback %q", path, got, want)
				}
			}
			if scheduled.Load() != 0 {
				t.Fatalf("rolled-back batch scheduled %d reindexes", scheduled.Load())
			}
		})
	}
}

func TestAtomicBatchCancellationBoundaries(t *testing.T) {
	t.Run("before-commit", func(t *testing.T) {
		var scheduled atomic.Int64
		s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
		path := writeAtomicBatchFixture(t, t.TempDir(), "cancel.txt", "before\n")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		receipt, err := s.runBatchTransaction(ctx, []batchEditItem{atomicFileEdit(path, "before", "after")}, "cancel-before")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || readAtomicBatchFixture(t, path) != "before\n" {
			t.Fatalf("receipt/content = %+v %q", receipt, readAtomicBatchFixture(t, path))
		}
		if scheduled.Load() != 0 {
			t.Fatalf("scheduled = %d", scheduled.Load())
		}
	})

	t.Run("after-first-commit", func(t *testing.T) {
		var scheduled atomic.Int64
		s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
		dir := t.TempDir()
		a := writeAtomicBatchFixture(t, dir, "a.txt", "a0\n")
		b := writeAtomicBatchFixture(t, dir, "b.txt", "b0\n")
		ctx, cancel := context.WithCancel(context.Background())
		var calls atomic.Int64
		s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
			if err := agents.AtomicWriteFile(path, content, mode); err != nil {
				return err
			}
			if calls.Add(1) == 1 {
				cancel()
			}
			return nil
		}
		receipt, err := s.runBatchTransaction(ctx, []batchEditItem{
			atomicFileEdit(a, "a0", "a1"), atomicFileEdit(b, "b0", "b1"),
		}, "cancel-after")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "committed" || receipt.GraphStatus != "fresh" {
			t.Fatalf("receipt = %+v", receipt)
		}
		if readAtomicBatchFixture(t, a) != "a1\n" || readAtomicBatchFixture(t, b) != "b1\n" {
			t.Fatal("cancellation interrupted an in-progress atomic commit")
		}
		if scheduled.Load() != 2 {
			t.Fatalf("scheduled = %d, want both files", scheduled.Load())
		}
	})
}

func TestAtomicBatchPendingGraphReceiptCompletesWithoutDuplicateAdmission(t *testing.T) {
	done := make(chan indexer.MutationResult, 1)
	var scheduled atomic.Int64
	s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled, done: done, generation: 7})
	s.mutationReindexWait = 5 * time.Millisecond
	path := writeAtomicBatchFixture(t, t.TempDir(), "pending.txt", "old\n")
	edits := []batchEditItem{atomicFileEdit(path, "old", "new")}
	receipt, err := s.runBatchTransaction(context.Background(), edits, "pending-graph")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "committed" || receipt.GraphStatus != "pending" || len(receipt.Files) != 1 || receipt.Files[0].ReindexReceipt == "" {
		t.Fatalf("pending receipt = %+v", receipt)
	}

	done <- indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 8, Reindexed: true}
	close(done)
	status, err := s.batchTransactionStatus(context.Background(), "pending-graph")
	if err != nil {
		t.Fatal(err)
	}
	if status.GraphStatus != "fresh" || !status.Results[0].Reindexed || status.Results[0].ReindexAppliedGeneration != 8 {
		t.Fatalf("completed status = %+v", status)
	}
	if scheduled.Load() != 1 {
		t.Fatalf("status retry admitted %d watcher generations, want 1", scheduled.Load())
	}
}

func prepareAtomicRecoveryFixture(t *testing.T, s *Server, id string, paths []string, before, after []string) batchTransactionReceipt {
	t.Helper()
	buffers := make(map[string]*batchFileBuffer, len(paths))
	results := make([]batchEditResult, len(paths))
	for i, path := range paths {
		buffers[path] = &batchFileBuffer{
			absPath: path, relPath: filepath.Base(path), mode: 0o644,
			original: []byte(before[i]), content: []byte(after[i]),
			existsBefore: true, existsAfter: true, existenceSet: true,
		}
		results[i] = batchEditResult{Op: "edit_file", FilePath: filepath.Base(path), Status: "validated"}
	}
	receipt := batchTransactionReceipt{
		Version: batchTransactionVersion, TransactionID: id, Fingerprint: "recovery-fixture",
		Status: "preparing", DiskStatus: "unchanged", GraphStatus: "not_started",
		Results: results, Summary: batchSummary(results), StartedAt: time.Now().UTC(),
	}
	if err := s.prepareBatchJournal(&receipt, buffers, paths); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestAtomicBatchDurableRecovery(t *testing.T) {
	t.Run("all-after-finishes-commit", func(t *testing.T) {
		var scheduled atomic.Int64
		s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
		dir := t.TempDir()
		paths := []string{
			writeAtomicBatchFixture(t, dir, "a.txt", "a0\n"),
			writeAtomicBatchFixture(t, dir, "b.txt", "b0\n"),
		}
		prepareAtomicRecoveryFixture(t, s, "recover-after", paths, []string{"a0\n", "b0\n"}, []string{"a1\n", "b1\n"})
		for i, path := range paths {
			if err := agents.AtomicWriteFile(path, []byte(fmt.Sprintf("%c1\n", 'a'+i)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		restarted := &Server{watcher: mutationTestWatcher{scheduled: &scheduled}, session: newSessionState(), mutationReindexWait: 100 * time.Millisecond}
		receipt, err := restarted.batchTransactionStatus(context.Background(), "recover-after")
		if err != nil {
			t.Fatal(err)
		}
		if !receipt.Recovered || receipt.Status != "committed" || receipt.DiskStatus != "committed" || receipt.GraphStatus != "fresh" {
			t.Fatalf("recovered receipt = %+v", receipt)
		}
		if scheduled.Load() != 2 {
			t.Fatalf("scheduled = %d", scheduled.Load())
		}
	})

	t.Run("mixed-commit-rolls-back", func(t *testing.T) {
		var scheduled atomic.Int64
		s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
		dir := t.TempDir()
		paths := []string{
			writeAtomicBatchFixture(t, dir, "a.txt", "a0\n"),
			writeAtomicBatchFixture(t, dir, "b.txt", "b0\n"),
		}
		prepareAtomicRecoveryFixture(t, s, "recover-mixed", paths, []string{"a0\n", "b0\n"}, []string{"a1\n", "b1\n"})
		if err := agents.AtomicWriteFile(paths[0], []byte("a1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		restarted := &Server{watcher: mutationTestWatcher{scheduled: &scheduled}, session: newSessionState()}
		receipt, err := restarted.batchTransactionStatus(context.Background(), "recover-mixed")
		if err != nil {
			t.Fatal(err)
		}
		if !receipt.Recovered || receipt.Status != "aborted" || receipt.DiskStatus != "rolled_back" || receipt.GraphStatus != "not_started" {
			t.Fatalf("recovered receipt = %+v", receipt)
		}
		if readAtomicBatchFixture(t, paths[0]) != "a0\n" || readAtomicBatchFixture(t, paths[1]) != "b0\n" {
			t.Fatal("mixed recovery did not restore every original file")
		}
		if scheduled.Load() != 0 {
			t.Fatalf("rolled-back recovery scheduled %d reindexes", scheduled.Load())
		}
	})

	t.Run("unknown-disk-state-fails-closed", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		path := writeAtomicBatchFixture(t, t.TempDir(), "unknown.txt", "old\n")
		prepareAtomicRecoveryFixture(t, s, "recover-conflict", []string{path}, []string{"old\n"}, []string{"new\n"})
		if err := agents.AtomicWriteFile(path, []byte("external\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		restarted := &Server{watcher: mutationTestWatcher{}, session: newSessionState()}
		receipt, err := restarted.batchTransactionStatus(context.Background(), "recover-conflict")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "recovery_conflict" || receipt.DiskStatus != "conflict" || !strings.Contains(receipt.Error, "unknown disk state") {
			t.Fatalf("conflict receipt = %+v", receipt)
		}
		if readAtomicBatchFixture(t, path) != "external\n" {
			t.Fatal("conflict recovery overwrote external bytes")
		}
	})
}
