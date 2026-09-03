package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

func atomicFileMove(source, destination, expectedSHA256 string) batchEditItem {
	return batchEditItem{
		Op:              "move_file",
		SourcePath:      source,
		DestinationPath: destination,
		ExpectedSHA256:  expectedSHA256,
	}
}

func atomicFileDelete(path, expectedSHA256 string) batchEditItem {
	return batchEditItem{Op: "delete_file", Path: path, ExpectedSHA256: expectedSHA256}
}

func testSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestAtomicBatchMovesAndDeletesWholeFiles(t *testing.T) {
	var scheduled atomic.Int64
	s := newAtomicBatchTestServer(t, mutationTestWatcher{scheduled: &scheduled})
	dir := t.TempDir()
	source := writeAtomicBatchFixture(t, dir, "source.txt", "move me\n")
	destination := filepath.Join(dir, "nested", "destination.txt")
	deleted := writeAtomicBatchFixture(t, dir, "delete.txt", "delete me\n")

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileMove(source, destination, testSHA256("move me\n")),
		atomicFileDelete(deleted, testSHA256("delete me\n")),
	}, "file-lifecycle-success")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "committed" || receipt.DiskStatus != "committed" || receipt.GraphStatus != "fresh" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists or stat failed: %v", err)
	}
	if got := readAtomicBatchFixture(t, destination); got != "move me\n" {
		t.Fatalf("destination = %q", got)
	}
	if _, err := os.Stat(deleted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file still exists or stat failed: %v", err)
	}
	if scheduled.Load() != 3 {
		t.Fatalf("scheduled = %d, want source + destination + deleted", scheduled.Load())
	}

	states := make(map[string]batchTransactionFile, len(receipt.Files))
	for _, file := range receipt.Files {
		states[file.Path] = file
	}
	if !states[source].AfterAbsent || states[source].BeforeAbsent {
		t.Fatalf("source state = %+v", states[source])
	}
	if !states[destination].BeforeAbsent || states[destination].AfterAbsent {
		t.Fatalf("destination state = %+v", states[destination])
	}
	if !states[deleted].AfterAbsent || states[deleted].BeforeAbsent {
		t.Fatalf("deleted state = %+v", states[deleted])
	}
}

func TestAtomicBatchLifecyclePreconditionsWriteNothing(t *testing.T) {
	t.Run("digest mismatch", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		source := writeAtomicBatchFixture(t, dir, "source.txt", "original\n")
		destination := filepath.Join(dir, "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, strings.Repeat("0", 64)),
		}, "file-lifecycle-digest")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "original\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("destination exists", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		source := writeAtomicBatchFixture(t, dir, "source.txt", "source\n")
		destination := writeAtomicBatchFixture(t, dir, "destination.txt", "destination\n")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-destination-exists")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" {
			t.Fatalf("receipt = %+v", receipt)
		}
		if readAtomicBatchFixture(t, source) != "source\n" || readAtomicBatchFixture(t, destination) != "destination\n" {
			t.Fatal("precondition failure changed disk")
		}
	})
}

func TestAtomicBatchLifecycleRollbackRestoresSources(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	moveSource := writeAtomicBatchFixture(t, dir, "move-source.txt", "move\n")
	moveDestination := filepath.Join(dir, "move-destination.txt")
	deleted := writeAtomicBatchFixture(t, dir, "delete.txt", "delete\n")

	var removes atomic.Int64
	s.batchRemoveOverride = func(path string) error {
		if removes.Add(1) == 2 {
			return errors.New("injected remove failure")
		}
		return os.Remove(path)
	}

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileMove(moveSource, moveDestination, ""),
		atomicFileDelete(deleted, ""),
	}, "file-lifecycle-rollback")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "aborted" || receipt.DiskStatus != "rolled_back" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if readAtomicBatchFixture(t, moveSource) != "move\n" || readAtomicBatchFixture(t, deleted) != "delete\n" {
		t.Fatal("rollback did not restore source files")
	}
	if _, err := os.Stat(moveDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback did not remove destination: %v", err)
	}
}

func TestAtomicBatchLifecycleRejectsSymlinkSource(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	target := writeAtomicBatchFixture(t, dir, "target.txt", "target\n")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileDelete(link, ""),
	}, "file-lifecycle-symlink")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "aborted" || !strings.Contains(receipt.Results[0].Error, "symlink") {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got := readAtomicBatchFixture(t, target); got != "target\n" {
		t.Fatalf("target = %q", got)
	}
}

func TestAtomicBatchLifecycleIdempotentRetry(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	source := writeAtomicBatchFixture(t, dir, "source.txt", "source\n")
	destination := filepath.Join(dir, "destination.txt")
	edits := []batchEditItem{atomicFileMove(source, destination, "")}

	first, err := s.runBatchTransaction(context.Background(), edits, "file-lifecycle-idempotent")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.runBatchTransaction(context.Background(), edits, "file-lifecycle-idempotent")
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "committed" || second.Status != "committed" || second.Fingerprint != first.Fingerprint {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if got := readAtomicBatchFixture(t, destination); got != "source\n" {
		t.Fatalf("destination = %q", got)
	}
}

func TestAtomicBatchLifecycleUsesDurableWriterForCreatedDestination(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	source := writeAtomicBatchFixture(t, dir, "source.txt", "source\n")
	destination := filepath.Join(dir, "destination.txt")
	var writes atomic.Int64
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		writes.Add(1)
		return agents.AtomicWriteFile(path, content, mode)
	}

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileMove(source, destination, ""),
	}, "file-lifecycle-writer")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "committed" || writes.Load() != 1 {
		t.Fatalf("receipt=%+v writes=%d", receipt, writes.Load())
	}
}

func TestAtomicBatchLifecycleCommitsCreationsBeforeRemovals(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	source := writeAtomicBatchFixture(t, dir, "aaa-source.txt", "source\n")
	destination := filepath.Join(dir, "zzz-destination.txt")
	operations := make([]string, 0, 2)
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		operations = append(operations, "write:"+filepath.Base(path))
		return agents.AtomicWriteFile(path, content, mode)
	}
	s.batchRemoveOverride = func(path string) error {
		operations = append(operations, "remove:"+filepath.Base(path))
		return os.Remove(path)
	}

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileMove(source, destination, ""),
	}, "file-lifecycle-create-before-remove")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "committed" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got, want := strings.Join(operations, ","), "write:zzz-destination.txt,remove:aaa-source.txt"; got != want {
		t.Fatalf("commit order = %q, want %q", got, want)
	}
}

func TestValidateBatchCreateTargetRejectsLateDestination(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	destination := filepath.Join(dir, "destination.txt")
	if err := s.validateBatchCreateTarget(context.Background(), destination, "destination.txt"); err != nil {
		t.Fatalf("absent destination rejected: %v", err)
	}
	writeAtomicBatchFixture(t, dir, "destination.txt", "racer\n")
	if err := s.validateBatchCreateTarget(context.Background(), destination, "destination.txt"); err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("late destination error = %v", err)
	}
}
