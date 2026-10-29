package mcp

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/search"
)

func newReadRepairWitnessFixture(t *testing.T) (*Server, *mutationCommitRecord, mutationReadRepairWitness, *indexer.PublicationPhaseRecorder) {
	t.Helper()
	s := newMutationServer()
	phases := indexer.NewPublicationPhaseRecorder(4, 1)
	now := time.Now()
	publication := phases.Begin("checkout", "publication", indexer.PublicationSourceMCPEdit, now)
	record := &mutationCommitRecord{
		owner: s, id: "commit", fingerprint: "edit fingerprint", absPath: "/repo/file.go",
		disk: mutationDiskCommitted, graph: mutationGraphPending, graphRecorded: true,
		commitSeq: 1, newSHA: gitBlobSHA([]byte("committed bytes")), startedAt: now, committedAt: now,
		reindexReceipt: "publication", reindexGeneration: 1, checkoutID: "checkout", checkoutIncarnation: "incarnation",
		publication: publication,
	}
	s.mutationCommits.put(record)
	admission := &mutationReceipt{id: record.reindexReceipt, generation: record.reindexGeneration, path: record.absPath}
	s.mutationReceipts.Store(admission.id, admission)
	record.readRepairAdmission = weak.Make(admission)
	witness := mutationReadRepairWitness{
		server: weak.Make(s), record: weak.Make(record), publication: weak.Make(publication),
		id: record.id, fingerprint: record.fingerprint, absPath: record.absPath,
		blobSHA: record.newSHA, receipt: record.reindexReceipt, checkout: record.checkoutID,
		incarnation: record.checkoutIncarnation, commitSeq: record.commitSeq,
		generation: record.reindexGeneration, committedAt: now,
		admission: weak.Make(admission),
	}
	return s, record, witness, phases
}

// Reuse the existing canonical-ticket adapter, but expose its actual enqueue
// boundary so the test can hold Done through reader admission and acceptance.
type readRepairHeldWatcher struct {
	canonicalPublicationWatcher
	enqueued chan struct{}
}

func (w *readRepairHeldWatcher) EnqueueFileMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	ticket, err := w.canonicalPublicationWatcher.EnqueueFileMutation(ctx, path)
	close(w.enqueued)
	return ticket, err
}

func TestReadRepairCapturedBeforeWatcherAdmissionObservesActualExecutor(t *testing.T) {
	root := setupMiniRepo(t, "reader-admission")
	path := filepath.Join(root, "main.go")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: root, Name: "fixture"}}}
	gc.SetConfigPath(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(gc.ConfigPath())
	require.NoError(t, err)
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, testRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err = mi.IndexAll()
	require.NoError(t, err)
	idx := mi.GetIndexer("fixture")
	require.NotNil(t, idx)
	s := newMutationServer()
	w := &readRepairHeldWatcher{canonicalPublicationWatcher: canonicalPublicationWatcher{hold: true}, enqueued: make(chan struct{})}
	s.watcher = w
	ctx, cancel := context.WithCancel(canonicalTimingContext())
	t.Cleanup(cancel)
	after := []byte("package main\nfunc Hello() {}\nfunc AddedByReader() {}\n")
	record, err := s.commitFileMutation(ctx, "write_file", "logical-reader-admission", "reader admission fixture", "main.go", path, after, 0o600)
	require.NoError(t, err)
	require.Nil(t, record.publication)
	require.Nil(t, record.readRepairAdmission.Value())
	observer := s.readRepairObserver(ctx, path, idx)
	require.NotNil(t, observer, "committed bytes can become stale before publication/ticket binding")
	finished := make(chan mutationReindexOutcome, 1)
	go func() { finished <- s.mutationReindexState(ctx, path) }()
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			select {
			case <-w.enqueued:
				w.results[0] <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
				close(w.results[0])
			case <-time.After(5 * time.Second):
				cancel()
			}
		})
	}
	t.Cleanup(func() {
		release()
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("held watcher waiter was not joined")
		}
	})
	require.Eventually(t, func() bool {
		record.mu.RLock()
		defer record.mu.RUnlock()
		return record.readRepairAdmission.Value() != nil
	}, 5*time.Second, time.Millisecond)
	// The watcher may finish its no-op before the independent executor's
	// callback. Terminal receipt state must not erase that historical owner.
	record.recordGraph(mutationReindexOutcome{Generation: 1, Reindexed: true})
	result, err := mi.IncrementalReindexRepoObserved("fixture", []string{path}, observer)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotEmpty(t, g.FindNodesByName("AddedByReader"), "real executor applied the committed bytes")
	accepted := record.snapshot()
	require.NotNil(t, accepted.readRepair, "actual executor callback must resolve the same newly bound admission")
	require.Equal(t, gitBlobSHA(after), accepted.readRepair.acceptedBlobSHA)
	require.Equal(t, true, accepted.readRepair.accounting.Payload()["observed_partition_complete"])
	release()
	runtime.KeepAlive(idx)
}

func TestReadRepairCompletionOnlyFillsPreviouslyMissingIdentity(t *testing.T) {
	for _, field := range []string{"publication", "admission", "receipt", "generation"} {
		t.Run(field, func(t *testing.T) {
			s, record, witness, phases := newReadRepairWitnessFixture(t)
			idx := &indexer.Indexer{}
			record.mu.Lock()
			switch field {
			case "publication":
				record.publication = phases.Begin("checkout", "foreign-publication", indexer.PublicationSourceMCPEdit, time.Now())
			case "admission":
				other := &mutationReceipt{id: witness.receipt, generation: witness.generation, path: witness.absPath}
				s.mutationReceipts.Store("replacement", other)
				record.readRepairAdmission = weak.Make(other)
			case "receipt":
				witness.receipt = "previously-witnessed-other-receipt"
			case "generation":
				witness.generation++
			}
			record.mu.Unlock()
			_, ok := witness.completionWitness(s, record, idx)
			require.False(t, ok, "completion cannot replace an already witnessed %s", field)
			require.Nil(t, record.snapshot().readRepair)
			runtime.KeepAlive(phases)
		})
	}
}

func TestReadRepairSeesCommittedAdmissionBeforeACKAndSurvivesSuccessClear(t *testing.T) {
	s := newMutationServer()
	s.mutationReindexWait = 30 * time.Second
	w := &readRepairHeldWatcher{canonicalPublicationWatcher: canonicalPublicationWatcher{hold: true}, enqueued: make(chan struct{})}
	s.watcher = w
	ctx, cancel := context.WithCancel(canonicalTimingContext())
	t.Cleanup(cancel)
	path := filepath.Join(t.TempDir(), "edit.go")
	record, err := s.commitFileMutation(ctx, "write_file", "caller-logical-id", "distinct-id fixture", filepath.Base(path), path, []byte("package edited\n"), 0o600)
	require.NoError(t, err)
	idx := &indexer.Indexer{}
	outcome := make(chan mutationReindexOutcome, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		outcome <- s.mutationReindexState(ctx, path)
	}()
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			select {
			case <-w.enqueued:
				w.results[0] <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
				close(w.results[0])
			case <-time.After(5 * time.Second):
				cancel()
			}
		})
	}
	t.Cleanup(func() {
		release()
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("held mutation waiter was not reaped")
		}
	})
	require.Eventually(t, func() bool {
		witness, ok := s.captureReadRepairWitness(path, idx)
		return ok && witness.generation != 0
	}, 5*time.Second, time.Millisecond,
		"a pre-ACK reader must see the real admitted ticket, not post-wait metadata")
	pending := record.snapshot()
	require.Empty(t, pending.ReindexReceipt)
	require.Zero(t, pending.ReindexGeneration, "05ab's required nonzero post-wait generation is absent at this real boundary")
	require.False(t, pending.GraphRecorded)
	select {
	case <-outcome:
		t.Fatal("Done was not held through the reader observation")
	default:
	}
	witness, ok := s.captureReadRepairWitness(path, idx)
	require.True(t, ok)
	witness.bind(mutationReadRepairObservation{generation: 2, windowNS: 10})
	accepted := record.snapshot()
	require.NotNil(t, accepted.readRepair)
	release()
	var result mutationReindexOutcome
	select {
	case result = <-outcome:
	case <-time.After(5 * time.Second):
		t.Fatal("actual ticket completion did not release the original wait")
	}
	require.True(t, result.Reindexed)
	require.False(t, result.Pending)
	require.Empty(t, result.Receipt, "immediate-success public output must still clear its receipt")
	record.recordGraph(result)
	late := record.snapshot()
	require.Empty(t, late.ReindexReceipt)
	require.EqualValues(t, 1, late.ReindexGeneration)
	require.NotNil(t, late.readRepair, "expected initial metadata fill/Receipt clearing must not erase accepted R")
	require.Equal(t, accepted.readRepair, late.readRepair)
	// Exercise the production status serialization after success clears the
	// transient receipt: the three identifier domains must remain distinct.
	status := mutationStatusFor(t, s, map[string]any{"mutation_receipt": record.id})
	original := status["publication_phases"].(map[string]any)
	repair := status["read_assisted_publication"].(map[string]any)
	require.Equal(t, record.id, status["receipt"])
	require.Equal(t, "caller-logical-id", status["mutation_id"])
	require.NotEqual(t, status["receipt"], original["key"])
	require.NotEqual(t, status["mutation_id"], original["key"])
	require.Equal(t, original["key"], repair["requested_receipt"])
	require.Equal(t, "mcp_edit", original["source"])
	require.Equal(t, "monotonic", original["clock"])
	require.Equal(t, float64(1), original["ticket"])
	require.Equal(t, original["ticket"], repair["requested_generation"])
	record.recordGraph(mutationReindexOutcome{Receipt: "foreign", Generation: 99, Pending: true})
	require.Nil(t, record.snapshot().readRepair, "actual foreign rebind must still clear R")
	require.EqualValues(t, 2, accepted.readRepair.generation, "prior snapshots remain immutable")
	runtime.KeepAlive(idx)
}

func TestReadRepairAcceptedAdmissionSurvivesReceiptExpiryButRejectsForeignRebind(t *testing.T) {
	for _, test := range []struct {
		name    string
		outcome mutationReindexOutcome
		keep    bool
	}{
		{"success_clear", mutationReindexOutcome{Generation: 1, Reindexed: true}, true},
		{"pending_original", mutationReindexOutcome{Generation: 1, Receipt: "publication", Pending: true}, true},
		{"foreign_receipt", mutationReindexOutcome{Generation: 1, Receipt: "foreign", Pending: true}, false},
		{"foreign_generation", mutationReindexOutcome{Generation: 2, Receipt: "publication", Pending: true}, false},
		{"unknown_admission", mutationReindexOutcome{Reindexed: true}, false},
		{"foreign_incarnation", mutationReindexOutcome{Generation: 1, Receipt: "publication", Pending: true, checkoutID: "checkout", checkoutIncarnation: "other"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, record, witness, phases := newReadRepairWitnessFixture(t)
			witness.bind(mutationReadRepairObservation{generation: 2, windowNS: 10})
			early := record.snapshot()
			require.NotNil(t, early.readRepair)
			// Model disappearance of the weak admission after its existing
			// map retention, without depending on GC or adding lifecycle work.
			s.mutationReceipts.Delete("publication")
			record.mu.Lock()
			record.readRepairAdmission = weak.Pointer[mutationReceipt]{}
			record.mu.Unlock()
			record.recordGraph(test.outcome)
			require.Equal(t, test.keep, record.snapshot().readRepair != nil)
			require.EqualValues(t, 2, early.readRepair.generation)
			runtime.KeepAlive(s)
			runtime.KeepAlive(phases)
		})
	}
}

func TestReadRepairBindingRejectsEvictionRebindAndNewerCommit(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Server, *mutationCommitRecord, *indexer.PublicationPhaseRecorder)
	}{
		{"evicted", func(s *Server, _ *mutationCommitRecord, _ *indexer.PublicationPhaseRecorder) {
			s.mutationCommits.mu.Lock()
			s.mutationCommits.evictLocked(time.Now().Add(mutationCommitRetention + time.Second))
			s.mutationCommits.mu.Unlock()
		}},
		{"receipt_rebound", func(_ *Server, r *mutationCommitRecord, _ *indexer.PublicationPhaseRecorder) {
			r.reindexReceipt = "replacement"
		}},
		{"generation_rebound", func(_ *Server, r *mutationCommitRecord, _ *indexer.PublicationPhaseRecorder) { r.reindexGeneration++ }},
		{"incarnation_rebound", func(_ *Server, r *mutationCommitRecord, _ *indexer.PublicationPhaseRecorder) {
			r.checkoutIncarnation = "replacement"
		}},
		{"newer_bytes", func(_ *Server, r *mutationCommitRecord, _ *indexer.PublicationPhaseRecorder) {
			r.newSHA = gitBlobSHA([]byte("newer bytes"))
		}},
		{"newer_commit", func(s *Server, r *mutationCommitRecord, _ *indexer.PublicationPhaseRecorder) {
			s.mutationCommits.put(&mutationCommitRecord{owner: s, id: "newer", absPath: r.absPath, startedAt: time.Now()})
		}},
		{"publication_rebound", func(_ *Server, r *mutationCommitRecord, phases *indexer.PublicationPhaseRecorder) {
			r.publication = phases.Begin("checkout", "replacement", indexer.PublicationSourceMCPEdit, time.Now())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, record, witness, phases := newReadRepairWitnessFixture(t)
			test.change(s, record, phases)
			witness.bind(mutationReadRepairObservation{generation: 2, windowNS: 10})
			if record.snapshot().readRepair != nil {
				t.Fatal("unowned/rebound mutation gained repair evidence")
			}
			runtime.KeepAlive(s)
			runtime.KeepAlive(phases)
		})
	}
}

func TestReadRepairAcceptedHistoryIsImmutableAfterLaterEdit(t *testing.T) {
	s, record, witness, phases := newReadRepairWitnessFixture(t)
	witness.bind(mutationReadRepairObservation{
		generation: 2, windowNS: 10, causeCount: 1,
		causes: [8]mutationReadRepairCause{{Session: "session", RequestIDKey: "n:7", AdmissionGeneration: 2}},
	})
	early := record.snapshot()
	if early.readRepair == nil || early.readRepair.generation != 2 {
		t.Fatal("accepted actual execution unavailable")
	}
	s.mutationCommits.put(&mutationCommitRecord{owner: s, id: "later", absPath: record.absPath, startedAt: time.Now()})
	witness.bind(mutationReadRepairObservation{generation: 3, windowNS: 20})
	late := record.snapshot()
	if late.readRepair == nil || late.readRepair.generation != 2 || early.readRepair.windowNS != 10 {
		t.Fatal("later event erased or rebound accepted historical evidence")
	}
	late.readRepair.causes[0].Session = "snapshot mutation"
	if record.snapshot().readRepair.causes[0].Session != "session" || early.readRepair.causes[0].Session != "session" {
		t.Fatal("mutable snapshot escaped into retained evidence")
	}
	runtime.KeepAlive(s)
	runtime.KeepAlive(phases)
}

func TestReadRepairUnobservedLedgerDoesNotAllocate(t *testing.T) {
	s := newMutationServer()
	idx := &indexer.Indexer{}
	ctx := t.Context()
	if allocations := testing.AllocsPerRun(100, func() {
		if s.readRepairObserver(ctx, "/repo/file.go", idx) != nil {
			panic("unobserved path acquired a witness")
		}
	}); allocations != 0 {
		t.Fatalf("unobserved allocations: %v", allocations)
	}
}

func TestReadRepairLedgerRetentionDoesNotRestoreCanonicalEvidence(t *testing.T) {
	s, record, witness, phases := newReadRepairWitnessFixture(t)
	witness.bind(mutationReadRepairObservation{generation: 2, windowNS: 10})
	early := record.snapshot()
	for i := range 5 {
		phases.Begin("checkout", "later-"+strconv.Itoa(i), indexer.PublicationSourceMCPEdit, time.Now())
	}
	if _, retained := phases.Lookup("publication"); retained {
		t.Fatal("fixture did not evict the original recorder entry")
	}
	if record.snapshot().readRepair == nil || record.publication.CoalescedPublication() != nil ||
		record.publication.Snapshot().EditTimings().Window.PatchWallMS != nil {
		t.Fatal("recorder eviction changed R retention or R fabricated canonical P/B")
	}
	s.mutationCommits.mu.Lock()
	s.mutationCommits.evictLocked(time.Now().Add(mutationCommitRetention + time.Second))
	s.mutationCommits.mu.Unlock()
	if record.snapshot().readRepair != nil || early.readRepair == nil || early.readRepair.windowNS != 10 {
		t.Fatal("ledger eviction retained live R or mutated an earlier immutable snapshot")
	}
	witness.bind(mutationReadRepairObservation{generation: 3, windowNS: 20})
	if record.snapshot().readRepair != nil {
		t.Fatal("late completion resurrected ledger-evicted repair evidence")
	}
	runtime.KeepAlive(s)
	runtime.KeepAlive(phases)
}
