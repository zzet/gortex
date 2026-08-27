package mcp

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func runWithdrawalGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func missingBlobRefViewRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runWithdrawalGit(t, repo, "init", "-q")
	runWithdrawalGit(t, repo, "config", "user.email", "withdrawal@example.invalid")
	runWithdrawalGit(t, repo, "config", "user.name", "Withdrawal Test")
	if err := os.WriteFile(filepath.Join(repo, "missing.go"), []byte("package missing\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runWithdrawalGit(t, repo, "add", "missing.go")
	runWithdrawalGit(t, repo, "commit", "-q", "-m", "fixture")
	treeOID := runWithdrawalGit(t, repo, "rev-parse", "HEAD^{tree}")
	blobOID := runWithdrawalGit(t, repo, "rev-parse", "HEAD:missing.go")
	objectPath := filepath.Join(repo, ".git", "objects", blobOID[:2], blobOID[2:])
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("remove loose blob %s: %v", blobOID, err)
	}
	return repo, treeOID
}

func newMCPWithdrawalGeneration(t *testing.T, store *store_sqlite.Store) int64 {
	t.Helper()
	generationID, generation, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "mcp_withdrawal_test",
		GenerationKind: "ref",
		TreeOID:        "missing-object-tree",
		ConfigHash:     "mcp-withdrawal-test",
		CreatedAt:      time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("begin generation: %v", err)
	}
	if err := generation.SetProducerStates([]store_sqlite.ProducerCompleteness{
		{Producer: string(graphview.CapSourceSnapshot), State: store_sqlite.ProducerStateComplete},
		{Producer: string(graphview.CapSyntaxGraph), State: store_sqlite.ProducerStateComplete},
	}); err != nil {
		t.Fatalf("seed producer states: %v", err)
	}
	return generationID
}

func TestRefViewMissingObjectSchedulesWithdrawalWithoutWriterWait(t *testing.T) {
	repo, treeOID := missingBlobRefViewRepo(t)
	storePath := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	generationID := newMCPWithdrawalGeneration(t, store)

	locker, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("open writer locker: %v", err)
	}
	defer locker.Close()
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("writer locker conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin writer lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	files := &refViewFiles{
		store:        store,
		repoDir:      repo,
		treeOID:      treeOID,
		generationID: generationID,
	}
	defer files.close()
	readDone := make(chan error, 1)
	go func() {
		_, readErr := files.read(context.Background(), "missing.go")
		readDone <- readErr
	}()
	select {
	case readErr := <-readDone:
		if readErr == nil {
			t.Fatal("missing blob read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("missing-object read waited for the contended SQLite writer")
	}
	if stats := store.ProducerWithdrawalStats(); stats.Scheduled == 0 {
		t.Fatalf("missing-object read did not schedule withdrawal: %+v", stats)
	}

	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release writer lock: %v", err)
	}
	locked = false
	if err := store.Close(); err != nil {
		t.Fatalf("close and drain store: %v", err)
	}

	reopened, err := store_sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	sourceState, err := reopened.Catalog().ReadProducerAvailability(context.Background(), generationID, string(graphview.CapSourceSnapshot))
	if err != nil {
		t.Fatalf("read source state: %v", err)
	}
	if !sourceState.Declared || sourceState.State != store_sqlite.ProducerStateUnavailable {
		t.Fatalf("source state after restart = %+v, want unavailable", sourceState)
	}
	structural, err := reopened.Catalog().ReadProducerAvailability(context.Background(), generationID, string(graphview.CapSyntaxGraph))
	if err != nil {
		t.Fatalf("read structural state: %v", err)
	}
	if !structural.Declared || structural.State != store_sqlite.ProducerStateComplete {
		t.Fatalf("structural state after restart = %+v, want complete", structural)
	}
}
