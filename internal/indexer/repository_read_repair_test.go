package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/zzet/gortex/internal/graph"
)

func TestReadRepairAcceptanceRequiresConcreteOwnerAndBytes(t *testing.T) {
	owner := &Indexer{}
	replacement := &Indexer{}
	now := time.Now()
	observation := ReadRepairObservation{
		generation: 2, started: now, acceptedAt: now, finished: now,
		causesComplete: true, executionSucceeded: true,
		acceptedBlobSHA: readRepairBlobSHA([]byte("accepted")), owner: weak.Make(owner),
	}
	if !observation.AcceptedFor(owner, readRepairBlobSHA([]byte("accepted"))) {
		t.Fatal("matching actual owner/content unavailable")
	}
	if observation.AcceptedFor(replacement, observation.acceptedBlobSHA) || observation.AcceptedFor(owner, readRepairBlobSHA([]byte("newer"))) {
		t.Fatal("replacement owner or newer content borrowed earlier evidence")
	}
	for _, invalid := range []time.Time{{}, now.Add(-time.Nanosecond), now.Add(time.Nanosecond)} {
		copy := observation
		copy.acceptedAt = invalid
		if copy.AcceptedFor(owner, observation.acceptedBlobSHA) {
			t.Fatal("missing or out-of-window acceptance fabricated publication")
		}
	}
	observation.owner = weak.Pointer[Indexer]{}
	if observation.AcceptedFor(owner, observation.acceptedBlobSHA) {
		t.Fatal("missing owner fabricated an accepted execution")
	}
	runtime.KeepAlive(owner)
	runtime.KeepAlive(replacement)
}

func TestRepositoryReadRepairUsesGitBlobIdentity(t *testing.T) {
	for _, test := range []struct{ content, oid string }{
		{"", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{"hello\n", "ce013625030ba8dba906f756967f9e9ca394464a"},
	} {
		if got := readRepairBlobSHA([]byte(test.content)); got != test.oid {
			t.Fatalf("Git blob identity = %q, want %q", got, test.oid)
		}
	}
}

func TestReadRepairInertSameBytesCannotClaimEarlierPublication(t *testing.T) {
	owner := &Indexer{}
	now := time.Now()
	observation := ReadRepairObservation{
		path: "file.go", generation: 2, started: now, finished: now,
		causesComplete: true, executionSucceeded: true, owner: weak.Make(owner),
	}
	// The second reader can validate the exact bytes an earlier unobserved
	// operation published. That fresh receipt is still not an apply receipt.
	receipt := fileReadReceipt{readRepairBlobSHA: readRepairBlobSHA([]byte("already published"))}
	observation.acceptFreshBlob("file.go", receipt.readRepairBlobSHA, receipt.readRepairApplied)
	if observation.acceptedBlobSHA != "" || observation.AcceptedFor(owner, receipt.readRepairBlobSHA) {
		t.Fatal("inert same-byte validation claimed an earlier publication")
	}
	receipt.readRepairApplied = true
	observation.acceptFreshBlob("file.go", receipt.readRepairBlobSHA, receipt.readRepairApplied)
	observation.finished = time.Now()
	if !observation.AcceptedFor(owner, receipt.readRepairBlobSHA) {
		t.Fatal("actual accepted apply lost its content proof")
	}
	runtime.KeepAlive(owner)
}

func TestReadRepairRealBatchDistinguishesInertFromApply(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.go")
	before := "package fixture\nfunc Value() int { return 1 }\n"
	writeFile(t, path, before)
	idx := newTestIndexer(graph.New())
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	if _, err := idx.Index(root); err != nil {
		t.Fatal(err)
	}
	// Exercise the real probe/inert/fresh-receipt path rather than merely
	// checking an aggregate result count after a no-op mtime fast path.
	inert := &ReadRepairObservation{path: "value.go"}
	plan, _, failed, raced := idx.reindexIncrementalFilesBatched([]string{path}, nil, &reparsePendingEnrichmentBatch{readRepair: inert}, false)
	if plan.InertFiles != 1 || len(failed) != 0 || len(raced) != 0 || inert.acceptedBlobSHA != "" {
		t.Fatalf("inert batch claimed publication: plan=%+v failed=%v raced=%v proof=%q", plan, failed, raced, inert.acceptedBlobSHA)
	}
	after := before + "func Added() int { return Value() }\n"
	writeFile(t, path, after)
	applied := &ReadRepairObservation{path: "value.go"}
	plan, _, failed, raced = idx.reindexIncrementalFilesBatched([]string{path}, nil, &reparsePendingEnrichmentBatch{readRepair: applied}, false)
	if plan.InertFiles != 0 || len(failed) != 0 || len(raced) != 0 || applied.acceptedBlobSHA != readRepairBlobSHA([]byte(after)) {
		t.Fatalf("accepted apply lost its own bytes: plan=%+v failed=%v raced=%v proof=%q", plan, failed, raced, applied.acceptedBlobSHA)
	}
}

func TestRepositoryReadRepairAcceptedContentCannotChangeOwners(t *testing.T) {
	observation := &ReadRepairObservation{path: "target.go"}
	older := readRepairBlobSHA([]byte("old bytes"))
	newer := readRepairBlobSHA([]byte("new bytes"))
	observation.acceptFreshBlob("other.go", older, true)
	if observation.acceptedBlobSHA != "" {
		t.Fatal("another path supplied target content")
	}
	observation.acceptFreshBlob("target.go", older, true)
	immutable := *observation
	observation.acceptFreshBlob("target.go", older, true)
	if !observation.acceptedAt.Equal(immutable.acceptedAt) {
		t.Fatal("same bytes replaced their actual first publication time")
	}
	observation.acceptFreshBlob("target.go", newer, true)
	observation.acceptFreshBlob("target.go", older, true)
	if observation.acceptedBlobSHA != "" || !observation.contentAmbiguous || immutable.acceptedBlobSHA != older {
		t.Fatal("ambiguous content regained authority or changed an earlier snapshot")
	}
}

func TestRepositoryReadRepairCausesKeepReusedWireIDsDistinct(t *testing.T) {
	request := &repositoryReadRepairRequest{session: "session", requestIDKey: "n:7", complete: func(ReadRepairObservation) {}}
	waiters := []*repositoryMutationWaiter{
		{generation: 4, readRepair: request},
		{generation: 5, readRepair: request},
	}
	observation := newRepositoryReadRepairObservation(5, []string{"file.go"}, waiters)
	if observation == nil || observation.generation != 5 || observation.causeCount != 2 || !observation.causesComplete {
		t.Fatalf("missing drain/admission observations: %+v", observation)
	}
	if observation.causes[0].admissionGeneration != 4 || observation.causes[1].admissionGeneration != 5 {
		t.Fatalf("reused wire IDs aliased admissions: %+v", observation.causes)
	}
}

func TestReadRepairPathUsesCanonicalRepositoryKey(t *testing.T) {
	observation := newRepositoryReadRepairObservation(1, []string{filepath.FromSlash("dir/value.go")}, []*repositoryMutationWaiter{
		{generation: 1, readRepair: &repositoryReadRepairRequest{complete: func(ReadRepairObservation) {}}},
	})
	if observation == nil || observation.path != "dir/value.go" {
		t.Fatalf("native scope spelling diverged from receipt key: %+v", observation)
	}
}

func TestRepositoryReadRepairUnobservedPathDoesNotAllocate(t *testing.T) {
	paths := []string{"file.go"}
	waiters := []*repositoryMutationWaiter{{generation: 1}}
	if allocations := testing.AllocsPerRun(100, func() {
		if newRepositoryReadRepairObservation(1, paths, waiters) != nil {
			panic("unobserved request acquired evidence")
		}
	}); allocations != 0 {
		t.Fatalf("unobserved allocations: %v", allocations)
	}
}

func TestRepositoryReadRepairCauseOverflowAndBroadScopesRemainUnavailable(t *testing.T) {
	request := &repositoryReadRepairRequest{complete: func(ReadRepairObservation) {}}
	waiters := make([]*repositoryMutationWaiter, repositoryReadRepairCauseCap+1)
	for i := range waiters {
		waiters[i] = &repositoryMutationWaiter{generation: uint64(i + 1), readRepair: request}
	}
	observation := newRepositoryReadRepairObservation(uint64(len(waiters)), []string{"file.go"}, waiters)
	if observation == nil || observation.causesComplete || observation.causeCount != repositoryReadRepairCauseCap {
		t.Fatalf("overflow incorrectly complete: %+v", observation)
	}
	for _, paths := range [][]string{nil, {"a.go", "b.go"}} {
		if newRepositoryReadRepairObservation(1, paths, waiters) != nil {
			t.Fatalf("broad scope acquired single-file evidence: %v", paths)
		}
	}
}

func TestRepositoryReadRepairCompletionIsOutsideCoordinatorLocks(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			coordinator := newRepositoryMutationCoordinator(func([]string) (*IndexResult, error) {
				if failed {
					return nil, errors.New("failed execution")
				}
				return &IndexResult{StaleFileCount: 1}, nil
			})
			var batchGate sync.RWMutex
			coordinator.batchMutationGate = &batchGate
			var snapshot ReadRepairObservation
			_, err := coordinator.reconcileObserved(t.Context(), []string{"file.go"}, &repositoryReadRepairRequest{
				complete: func(observation ReadRepairObservation) {
					// Both operations would deadlock if completion still owned
					// coordinator.mu or the repository mutation lane.
					if !coordinator.mu.TryLock() {
						t.Error("completion still owns coordinator lock")
						return
					}
					coordinator.mu.Unlock()
					if !batchGate.TryLock() {
						t.Error("completion still owns batch lock")
						return
					}
					batchGate.Unlock()
					if laneErr := coordinator.runExclusive(context.Background(), func() error { return nil }); laneErr != nil {
						panic(laneErr)
					}
					snapshot = observation
					observation.causes[0].session = "caller mutation"
				},
			})
			coordinator.work.Wait()
			if (err != nil) != failed || snapshot.executionSucceeded == failed || snapshot.started.IsZero() || snapshot.finished.Before(snapshot.started) {
				t.Fatalf("execution window mismatch: failed=%v err=%v observation=%+v", failed, err, snapshot)
			}
			if snapshot.causes[0].session != "" {
				t.Fatal("mutable completion value escaped")
			}
		})
	}
}

func TestRepositoryReadRepairContentRequiresSuccessfulAuthorityAndResult(t *testing.T) {
	for _, test := range []struct {
		name      string
		result    *IndexResult
		authority bool
	}{
		{name: "missing_result"},
		{name: "full", result: &IndexResult{FullRetrack: true}},
		{name: "failed", result: &IndexResult{FailedFiles: []string{"file.go"}}},
		{name: "deleted", result: &IndexResult{DeletedFileCount: 1}},
		{name: "quarantined", result: &IndexResult{QuarantinedFiles: 1}},
		{name: "skipped", result: &IndexResult{SkippedFiles: 1}},
		{name: "authority_refusal", result: &IndexResult{StaleFileCount: 1}, authority: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newRepositoryMutationCoordinator(nil)
			coordinator.readRepairExecutor = func(_ []string, observation *ReadRepairObservation) (*IndexResult, error) {
				observation.acceptFreshBlob("file.go", readRepairBlobSHA([]byte("accepted bytes")), true)
				return test.result, nil
			}
			if test.authority {
				coordinator.outputGeneration = func(fn func(func(*OutputSourceContent)) error) error {
					if err := fn(func(*OutputSourceContent) {}); err != nil {
						return err
					}
					return errors.New("superseded authority")
				}
			}
			var snapshot ReadRepairObservation
			_, _ = coordinator.reconcileObserved(t.Context(), []string{"file.go"}, &repositoryReadRepairRequest{
				complete: func(observation ReadRepairObservation) { snapshot = observation },
			})
			coordinator.work.Wait()
			if snapshot.executionSucceeded || snapshot.acceptedBlobSHA != "" {
				t.Fatalf("unsuccessful publisher retained content authority: %+v", snapshot)
			}
		})
	}
}
