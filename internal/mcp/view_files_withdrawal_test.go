package mcp

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/testutil/gitpromisor"
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

func newMCPWithdrawalGeneration(t testing.TB, store *store_sqlite.Store) int64 {
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

func TestRefViewPromisorObjectsNeverFetchAndWithdrawSource(t *testing.T) {
	fixture := gitpromisor.New(t)
	treeMissing := fixture.Clone(t, "tree:1")
	blobMissing := fixture.Clone(t, "blob:none")
	if treeMissing.ObjectPresent(t, fixture.NestedTreeOID) {
		t.Fatalf("tree:1 fixture unexpectedly contains %s", fixture.NestedTreeOID)
	}
	if blobMissing.ObjectPresent(t, fixture.NestedBlobOID) {
		t.Fatalf("blob:none fixture unexpectedly contains %s", fixture.NestedBlobOID)
	}
	treeControl := fixture.Clone(t, "tree:1")
	treeControl.FetchAndRequireRequest(t, fixture.NestedTreeOID)
	blobControl := fixture.Clone(t, "blob:none")
	blobControl.FetchAndRequireRequest(t, fixture.NestedBlobOID)

	t.Run("missing descendant tree", func(t *testing.T) {
		assertPromisorRefViewWithdrawal(t, treeMissing, fixture.RootTreeOID, "nested/missing.go", fixture.NestedTreeOID)
	})
	t.Run("missing blob", func(t *testing.T) {
		assertPromisorRefViewWithdrawal(t, blobMissing, fixture.RootTreeOID, "nested/missing.go", fixture.NestedBlobOID)
	})
}

func assertPromisorRefViewWithdrawal(
	t *testing.T,
	client *gitpromisor.Client,
	treeOID, path, missingOID string,
) {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	generationID := newMCPWithdrawalGeneration(t, store)
	files := &refViewFiles{
		store:        store,
		repoDir:      client.Dir,
		treeOID:      treeOID,
		generationID: generationID,
	}
	_, err = files.read(context.Background(), path)
	files.close()
	if !errors.Is(err, graphview.ErrSourceObjectMissing) {
		t.Fatalf("read() error = %v, want ErrSourceObjectMissing", err)
	}
	if got := client.RequestCount(t); got != 0 {
		t.Fatalf("upload-pack requests = %d, want 0", got)
	}
	if client.ObjectPresent(t, missingOID) {
		t.Fatalf("read() materialized promised object %s", missingOID)
	}
	if stats := store.ProducerWithdrawalStats(); stats.Scheduled == 0 {
		t.Fatalf("missing-object read did not schedule withdrawal: %+v", stats)
	}
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

func BenchmarkRefViewFilesNoLazyFetch(b *testing.B) {
	fixture := gitpromisor.New(b)
	complete := fixture.Clone(b, "blob:limit=1m")
	missing := fixture.Clone(b, "blob:none")
	benchmarkRefViewFilesRead(b, "complete", complete, fixture.RootTreeOID, false)
	benchmarkRefViewFilesRead(b, "missing-blob", missing, fixture.RootTreeOID, true)
}

func benchmarkRefViewFilesRead(b *testing.B, name string, client *gitpromisor.Client, treeOID string, wantMissing bool) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "graph.sqlite"))
		if err != nil {
			b.Fatalf("open store: %v", err)
		}
		generationID := newMCPWithdrawalGeneration(b, store)
		files := &refViewFiles{store: store, repoDir: client.Dir, treeOID: treeOID, generationID: generationID}
		_, err = files.read(context.Background(), "nested/missing.go")
		if errors.Is(err, graphview.ErrSourceObjectMissing) != wantMissing || (!wantMissing && err != nil) {
			b.Fatalf("warm read() error = %v, want missing=%t", err, wantMissing)
		}
		client.ResetRequests(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			_, err := files.read(context.Background(), "nested/missing.go")
			if errors.Is(err, graphview.ErrSourceObjectMissing) != wantMissing || (!wantMissing && err != nil) {
				b.Fatalf("read() error = %v, want missing=%t", err, wantMissing)
			}
		}
		b.StopTimer()
		requests := client.RequestCount(b)
		b.ReportMetric(float64(requests)/float64(b.N), "upload-pack/op")
		if requests != 0 {
			b.Fatalf("upload-pack requests = %d, want 0", requests)
		}
		files.close()
		if err := store.Close(); err != nil {
			b.Fatalf("close store: %v", err)
		}
	})
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
