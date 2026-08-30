package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

type mutationTestWatcher struct {
	reject      bool
	scheduleErr error
	scheduled   *atomic.Int64
	done        <-chan indexer.MutationResult
	generation  uint64
}

func (mutationTestWatcher) History() []indexer.GraphChangeEvent { return nil }
func (mutationTestWatcher) HistorySince(time.Time) []indexer.GraphChangeEvent {
	return nil
}
func (mutationTestWatcher) OnSymbolChange(indexer.SymbolChangeCallback) {}
func (w mutationTestWatcher) EnqueueFileMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w.scheduled != nil {
		w.scheduled.Add(1)
	}
	if w.scheduleErr != nil {
		return nil, w.scheduleErr
	}
	if w.reject {
		return nil, nil
	}
	generation := w.generation
	if generation == 0 {
		generation = 1
	}
	done := w.done
	if done == nil {
		completed := make(chan indexer.MutationResult, 1)
		completed <- indexer.MutationResult{
			RequestedGeneration: generation,
			AppliedGeneration:   generation,
			Reindexed:           true,
		}
		close(completed)
		done = completed
	}
	return &indexer.MutationTicket{Path: path, Generation: generation, Done: done}, nil
}

type degradedMutationTestWatcher struct {
	mutationTestWatcher
	reason string
}

func (w degradedMutationTestWatcher) DegradedReason() string { return w.reason }

func mutationRefs(path string) int {
	path = filepath.Clean(path)
	mutationPathLocks.Lock()
	defer mutationPathLocks.Unlock()
	if entry := mutationPathLocks.byPath[path]; entry != nil {
		return entry.refs
	}
	return 0
}

func waitForMutationRefs(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mutationRefs(path) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("mutation refs for %q = %d, want %d", path, mutationRefs(path), want)
}

func TestAcquireMutationPathSerializesAndCleansUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "same.go")
	firstRelease, err := acquireMutationPath(context.Background(), path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	acquired := make(chan func(), 1)
	errs := make(chan error, 1)
	go func() {
		release, acquireErr := acquireMutationPath(context.Background(), path)
		if acquireErr != nil {
			errs <- acquireErr
			return
		}
		acquired <- release
	}()
	waitForMutationRefs(t, path, 2)
	select {
	case <-acquired:
		t.Fatal("second acquire entered before first release")
	default:
	}

	firstRelease()
	var secondRelease func()
	select {
	case err := <-errs:
		t.Fatalf("second acquire: %v", err)
	case secondRelease = <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire did not proceed after release")
	}
	secondRelease()
	waitForMutationRefs(t, path, 0)
}

func TestAcquireMutationPathCancellationReleasesQueueReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel.go")
	ownerRelease, err := acquireMutationPath(context.Background(), path)
	if err != nil {
		t.Fatalf("owner acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, acquireErr := acquireMutationPath(ctx, path)
		errs <- acquireErr
	}()
	waitForMutationRefs(t, path, 2)
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled acquire error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled acquire did not leave the queue")
	}
	waitForMutationRefs(t, path, 1)
	ownerRelease()
	waitForMutationRefs(t, path, 0)
}

func TestAcquireMutationPathsCancellationReleasesEarlierLocks(t *testing.T) {
	first := filepath.Join(t.TempDir(), "a.go")
	second := filepath.Join(t.TempDir(), "b.go")
	if second < first {
		first, second = second, first
	}

	releaseSecond, err := acquireMutationPath(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		release, lockErr := acquireMutationPaths(ctx, []string{second, first, first})
		if release != nil {
			release()
		}
		result <- lockErr
	}()

	waitForMutationRefs(t, first, 1)
	waitForMutationRefs(t, second, 2)
	cancel()
	if lockErr := <-result; !errors.Is(lockErr, context.Canceled) {
		t.Fatalf("acquireMutationPaths error = %v, want context.Canceled", lockErr)
	}
	waitForMutationRefs(t, first, 0)
	waitForMutationRefs(t, second, 1)
}

func TestMutationReindexStateRoutesByPathAdmission(t *testing.T) {
	var scheduled atomic.Int64
	active := &Server{watcher: mutationTestWatcher{scheduled: &scheduled}}
	outcome := active.mutationReindexState(context.Background(), "/tmp/active.go")
	if outcome.Err != nil || !outcome.Reindexed || outcome.Pending || outcome.Generation != 1 || outcome.AppliedGeneration != 1 || outcome.Receipt != "" || scheduled.Load() != 1 {
		t.Fatalf("active watcher outcome = %+v scheduled:%d", outcome, scheduled.Load())
	}

	// Aggregate degradation is informational. Direct path admission decides
	// whether the owning watcher can accept the mutation.
	degraded := &Server{watcher: degradedMutationTestWatcher{
		mutationTestWatcher: mutationTestWatcher{scheduled: &scheduled},
		reason:              "other-repo overflow",
	}}
	outcome = degraded.mutationReindexState(context.Background(), "/tmp/degraded.go")
	if outcome.Err != nil || !outcome.Reindexed || outcome.Pending || scheduled.Load() != 2 {
		t.Fatalf("degraded watcher outcome = %+v scheduled:%d", outcome, scheduled.Load())
	}

	rejected := &Server{watcher: mutationTestWatcher{reject: true}}
	outcome = rejected.mutationReindexState(context.Background(), "/tmp/uncovered.go")
	if outcome.Err != nil || outcome.Reindexed || outcome.Pending || outcome.Generation != 0 {
		t.Fatalf("uncovered watcher outcome = %+v", outcome)
	}

	embedded := &Server{}
	outcome = embedded.mutationReindexState(context.Background(), "/tmp/embedded.go")
	if outcome.Err != nil || outcome.Reindexed || outcome.Pending || outcome.Generation != 0 {
		t.Fatalf("embedded outcome = %+v", outcome)
	}

	scheduleFailure := errors.New("schedule failed")
	failed := &Server{watcher: mutationTestWatcher{scheduleErr: scheduleFailure}}
	outcome = failed.mutationReindexState(context.Background(), "/tmp/failed.go")
	if !errors.Is(outcome.Err, scheduleFailure) || outcome.Reindexed || outcome.Pending {
		t.Fatalf("schedule failure outcome = %+v", outcome)
	}
}

func TestMutationReindexStateSlowTicketReceiptCompletes(t *testing.T) {
	done := make(chan indexer.MutationResult, 1)
	s := &Server{
		watcher:             mutationTestWatcher{done: done, generation: 7},
		mutationReindexWait: 5 * time.Millisecond,
		mutationSafetyWait:  5 * time.Millisecond,
	}
	outcome := s.mutationReindexState(context.Background(), "/tmp/slow.go")
	if outcome.Reindexed || !outcome.Pending || outcome.Receipt == "" || outcome.Generation != 7 {
		t.Fatalf("slow ticket outcome = %+v", outcome)
	}
	if status, ok := s.mutationReceiptState(outcome.Receipt); !ok || !status.Pending || status.Generation != 7 {
		t.Fatalf("pending receipt state = %+v, ok=%v", status, ok)
	}
	if err := s.awaitMutationFreshness(context.Background()); err == nil || !strings.Contains(err.Error(), "pending receipt=") {
		t.Fatalf("pending safety barrier error = %v", err)
	}

	done <- indexer.MutationResult{
		RequestedGeneration: 7,
		AppliedGeneration:   8,
		Reindexed:           true,
	}
	close(done)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := s.mutationReceiptState(outcome.Receipt)
		if ok && status.Reindexed && !status.Pending && status.AppliedGeneration == 8 {
			if err := s.awaitMutationFreshness(context.Background()); err != nil {
				t.Fatalf("completed safety barrier: %v", err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("mutation receipt did not reach a terminal reindexed state")
}

func TestMutationReindexStateFailureBlocksSafetyReads(t *testing.T) {
	patchFailure := errors.New("patch failed")
	done := make(chan indexer.MutationResult, 1)
	done <- indexer.MutationResult{RequestedGeneration: 3, AppliedGeneration: 3, Err: patchFailure}
	close(done)
	s := &Server{watcher: mutationTestWatcher{done: done, generation: 3}}
	outcome := s.mutationReindexState(context.Background(), "/tmp/broken.go")
	if !errors.Is(outcome.Err, patchFailure) || outcome.Reindexed || outcome.Pending {
		t.Fatalf("failed ticket outcome = %+v", outcome)
	}
	if err := s.awaitMutationFreshness(context.Background()); err == nil || !strings.Contains(err.Error(), "patch failed") {
		t.Fatalf("failed safety barrier error = %v", err)
	}
}

func TestAttachMutationFreshnessPendingOmitsStaleSyntaxHealth(t *testing.T) {
	resp := map[string]any{}
	outcome := mutationReindexOutcome{Pending: true, Receipt: "mutation-7", Generation: 7}
	(&Server{}).attachMutationFreshness(context.Background(), resp, "pending.go", "/tmp/pending.go", outcome)
	if got := resp["reindexed"]; got != false {
		t.Fatalf("reindexed = %#v, want false", got)
	}
	if got := resp["reindex_pending"]; got != true {
		t.Fatalf("reindex_pending = %#v, want true", got)
	}
	if got := resp["reindex_receipt"]; got != "mutation-7" {
		t.Fatalf("reindex_receipt = %#v", got)
	}
	if got := resp["reindex_generation"]; got != uint64(7) {
		t.Fatalf("reindex_generation = %#v", got)
	}
	if _, ok := resp["syntax_health"]; ok {
		t.Fatal("pending response exposed stale syntax_health")
	}
}

func TestApplyBatchFileEditSerializesConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.txt")
	if err := os.WriteFile(path, []byte("alpha beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{watcher: mutationTestWatcher{}, session: newSessionState()}

	start := make(chan struct{})
	results := make(chan batchEditResult, 2)
	for _, edit := range []batchEditItem{
		{Path: path, OldString: "alpha", NewString: "ALPHA"},
		{Path: path, OldString: "beta", NewString: "BETA"},
	} {
		edit := edit
		go func() {
			<-start
			results <- s.applyBatchFileEdit(context.Background(), edit, true)
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.Status != "applied" || !result.Reindexed || result.ReindexPending || result.ReindexError != "" {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ALPHA BETA\n" {
		t.Fatalf("concurrent edit result = %q", got)
	}
}

func TestApplyBatchSymbolEditFallsBackAfterPendingLineShift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shifted.go")
	content := "package sample\n\nfunc target() {\n\tprintln(\"old\")\n}\n\nfunc other() {}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New()
	node := &graph.Node{
		ID:        "sample::target",
		Name:      "target",
		Kind:      graph.KindFunction,
		FilePath:  path,
		StartLine: 7,
		EndLine:   7,
	}
	g.AddNode(node)
	s := &Server{
		engine:  query.NewEngine(g),
		graph:   g,
		watcher: mutationTestWatcher{},
		session: newSessionState(),
	}
	result := s.applyBatchSymbolEdit(context.Background(), batchEditItem{
		SymbolID:  node.ID,
		OldSource: "println(\"old\")",
		NewSource: "println(\"new\")",
	}, true)
	if result.Status != "applied" || !result.Reindexed || result.ReindexPending || result.ReindexError != "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "package sample\n\nfunc target() {\n\tprintln(\"new\")\n}\n\nfunc other() {}\n"
	if string(got) != want {
		t.Fatalf("stale-range fallback result:\n%s\nwant:\n%s", got, want)
	}
}
