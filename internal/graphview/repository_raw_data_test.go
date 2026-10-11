package graphview

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

type privateRawQueuedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *privateRawQueuedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func privateRawWait(t testing.TB, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out joining %s", label)
	}
}

func privateRawSource(t testing.TB, m *LeaseManager, reg *RawRepositoryRegistration, fingerprint string) uint64 {
	t.Helper()
	write, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Release()
	if err := write.Complete(fingerprint); err != nil {
		t.Fatal(err)
	}
	return write.Revision()
}

func TestRawDataSnapshotHoldsMutationAdmissionUntilConsumerCloses(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	revision := privateRawSource(t, m, reg, "source-a")
	read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, revision)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	queued := &privateRawQueuedContext{Context: ctx, entered: make(chan struct{})}
	exited := make(chan struct{})
	acquired := make(chan *RawRepositoryMutationLease, 1)
	failed := make(chan error, 1)
	t.Cleanup(func() { cancel(); read.Release(); privateRawWait(t, exited, "blocked raw mutation") })
	go func() {
		defer close(exited)
		write, err := m.AcquireRawRepositoryMutation(queued, reg)
		if err != nil {
			failed <- err
			return
		}
		acquired <- write
		write.Release()
	}()
	privateRawWait(t, queued.entered, "mutation admission")
	select {
	case <-acquired:
		t.Fatal("mutation crossed held source read admission")
	case err := <-failed:
		t.Fatal(err)
	default:
	}
	if got := read.Witness(); got.Revision != revision || got.Fingerprint != "source-a" {
		t.Fatalf("held witness changed: %+v", got)
	}
	read.Release()
	privateRawWait(t, exited, "released raw mutation")
	select {
	case write := <-acquired:
		if write.Revision() <= revision {
			t.Fatal("source revision did not advance")
		}
	case err := <-failed:
		t.Fatal(err)
	default:
		t.Fatal("mutation did not acquire after read release")
	}
}

func TestRawDataFailedMutationCannotCertifyPartialSource(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	old := privateRawSource(t, m, reg, "source-a")
	write, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	failedRevision := write.Revision()
	write.Release()
	if failedRevision <= old {
		t.Fatal("failed attempt did not revoke prior current revision")
	}
	if _, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, 0); !errors.Is(err, ErrRawRepositorySourceChanged) {
		t.Fatalf("partial source became available=%v", err)
	}
	if err := write.Complete("late-completion"); !errors.Is(err, ErrRawRepositorySourceChanged) {
		t.Fatalf("released writer changed source=%v", err)
	}
	current := privateRawSource(t, m, reg, "source-b")
	if current <= failedRevision {
		t.Fatal("retry reused failed revision")
	}
	if _, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, old); !errors.Is(err, ErrRawRepositorySourceChanged) {
		t.Fatalf("old derived upper borrowed new lower=%v", err)
	}
	read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, current)
	if err != nil {
		t.Fatal(err)
	}
	read.Release()
}

func TestRawDataQueuedCancellationAndCloseDoNotLeakOwnerPins(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	privateRawSource(t, m, reg, "source-a")
	write, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	queued := &privateRawQueuedContext{Context: ctx, entered: make(chan struct{})}
	exited := make(chan struct{})
	result := make(chan error, 1)
	t.Cleanup(func() { cancel(); write.Release(); privateRawWait(t, exited, "canceled raw snapshot") })
	go func() {
		defer close(exited)
		read, err := m.AcquireRawRepositorySnapshot(queued, reg, 0)
		if read != nil {
			read.Release()
		}
		result <- err
	}()
	privateRawWait(t, queued.entered, "raw snapshot wait")
	drain, err := m.CloseRawRepositoryAdmission(reg)
	if err != nil {
		t.Fatal(err)
	}
	privateRawStillDraining(t, drain)
	cancel()
	privateRawWait(t, exited, "canceled raw snapshot")
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued read cancellation=%v", err)
	}
	privateRawStillDraining(t, drain)
	write.Release()
	privateRawDrained(t, drain)
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
}

func TestRawDataSnapshotReleaseAllowsReentrantDrainNotification(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	privateRawSource(t, m, reg, "source-a")
	read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, 0)
	if err != nil {
		t.Fatal(err)
	}
	drain, err := m.CloseRawRepositoryAdmission(reg)
	if err != nil {
		t.Fatal(err)
	}
	notified := make(chan struct{})
	exited := make(chan struct{})
	t.Cleanup(func() { privateRawWait(t, exited, "reentrant raw release") })
	drain.Notify(func() { read.Release(); close(notified) })
	go func() { defer close(exited); read.Release() }()
	privateRawWait(t, exited, "reentrant raw release")
	select {
	case <-notified:
	default:
		t.Fatal("drain notification did not run synchronously")
	}
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
}

func TestRawDataProvisionalMutationDoesNotOpenReadAdmission(t *testing.T) {
	m := NewLeaseManager()
	reg, err := m.PrepareRawRepositoryOwner(RawRepositoryOwner{RepoPrefix: "raw", RootIdentity: "root", Incarnation: "one"})
	if err != nil {
		t.Fatal(err)
	}
	privateRawSource(t, m, reg, "source-a")
	if _, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, 0); !errors.Is(err, ErrRawRepositoryNotReady) {
		t.Fatalf("source completion bypassed MI installation=%v", err)
	}
	if err := m.CommitRawRepositoryOwner(reg); err != nil {
		t.Fatal(err)
	}
	read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, 0)
	if err != nil {
		t.Fatal(err)
	}
	read.Release()
}

func BenchmarkRawDataSnapshotAdmission(b *testing.B) {
	m := NewLeaseManager()
	reg := privateRawRegistration(b, m, "raw", "one")
	revision := privateRawSource(b, m, reg, "source-a")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, revision)
		if err != nil {
			b.Fatal(err)
		}
		read.Release()
	}
}

func TestRawDataDurableRevisionFloorAndOverflowDoNotReuseOrPoison(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	write, err := m.AcquireRawRepositoryMutationAfter(context.Background(), reg, 100)
	if err != nil {
		t.Fatal(err)
	}
	if write.Revision() != 101 {
		write.Release()
		t.Fatalf("durable revision was reused: %d", write.Revision())
	}
	if err := write.Complete("source-101"); err != nil {
		write.Release()
		t.Fatal(err)
	}
	write.Release()
	for _, invalid := range []uint64{math.MaxInt64, math.MaxUint64} {
		if invalidWrite, err := m.AcquireRawRepositoryMutationAfter(context.Background(), reg, invalid); !errors.Is(err, ErrRawRepositorySourceChanged) {
			if invalidWrite != nil {
				invalidWrite.Release()
			}
			t.Fatalf("invalid floor=%d err=%v", invalid, err)
		}
	}
	read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, 101)
	if err != nil {
		t.Fatalf("invalid request poisoned stable source: %v", err)
	}
	read.Release()
	write, err = m.AcquireRawRepositoryMutationAfter(context.Background(), reg, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Release()
	if write.Revision() != 102 {
		t.Fatalf("stale durable floor moved local revision backwards: %d", write.Revision())
	}
}

// --- initial capture and witness validation -----------------------------

// TestRawDataInitialCaptureMakesANeverMutatedOwnerPinnable is the hole the
// capture closes: before it, an owner that was registered and only ever read
// had no data authority at all, so the read-side acquisition refused it as
// "changed" and it stayed unpinnable until somebody wrote to it.
func TestRawDataInitialCaptureMakesANeverMutatedOwnerPinnable(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	if _, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, 0); !errors.Is(err, ErrRawRepositorySourceChanged) {
		t.Fatalf("snapshot of a never-captured owner = %v, want %v", err, ErrRawRepositorySourceChanged)
	}
	revision, err := m.CaptureInitialRawRepositorySource(context.Background(), reg, "source-a")
	if err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Fatalf("initial capture revision = %d, want 1", revision)
	}
	read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, revision)
	if err != nil {
		t.Fatalf("snapshot after the initial capture: %v", err)
	}
	defer read.Release()
	if got := read.Witness(); got.Revision != revision || got.Fingerprint != "source-a" {
		t.Fatalf("witness = %+v, want revision %d fingerprint %q", got, revision, "source-a")
	}
	if err := read.ValidateCurrent(); err != nil {
		t.Fatalf("ValidateCurrent() on an unchanged source = %v, want nil", err)
	}
}

// TestRawDataInitialCaptureFailsClosed: a capture may never overwrite a
// witness a reader could already be holding.
func TestRawDataInitialCaptureFailsClosed(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	if _, err := m.CaptureInitialRawRepositorySource(context.Background(), reg, ""); !errors.Is(err, ErrRawRepositorySourceChanged) {
		t.Fatalf("capture with no fingerprint = %v, want %v", err, ErrRawRepositorySourceChanged)
	}
	if _, err := m.CaptureInitialRawRepositorySource(context.Background(), reg, "source-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CaptureInitialRawRepositorySource(context.Background(), reg, "source-b"); !errors.Is(err, ErrRawRepositorySourceChanged) {
		t.Fatalf("second capture = %v, want %v", err, ErrRawRepositorySourceChanged)
	}
	// A mutated owner is not a never-mutated one either.
	mutated := privateRawRegistration(t, m, "raw2", "one")
	privateRawSource(t, m, mutated, "source-x")
	if _, err := m.CaptureInitialRawRepositorySource(context.Background(), mutated, "source-y"); !errors.Is(err, ErrRawRepositorySourceChanged) {
		t.Fatalf("capture over a mutated owner = %v, want %v", err, ErrRawRepositorySourceChanged)
	}
}

// TestRawDataSnapshotValidateCurrentSeesAMutationAfterHandoff states the
// lease's real contract: it protects lifetime, not bytes. Once the gate is
// released the source can move, and a holder that kept the witness has to be
// able to find that out instead of assuming its capture still stands.
func TestRawDataSnapshotValidateCurrentSeesAMutationAfterHandoff(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	revision := privateRawSource(t, m, reg, "source-a")
	read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.ValidateCurrent(); err != nil {
		t.Fatalf("ValidateCurrent() while nothing moved = %v, want nil", err)
	}
	// The gate goes; the witness stays. This is the shape a handed-off
	// consumer is in, and the one a request-lifetime holder is in.
	read.Release()
	if err := read.ValidateCurrent(); err != nil {
		t.Fatalf("ValidateCurrent() after release with no mutation = %v, want nil", err)
	}
	privateRawSource(t, m, reg, "source-b")
	if err := read.ValidateCurrent(); !errors.Is(err, ErrRawRepositorySourceChanged) {
		t.Fatalf("ValidateCurrent() after the source moved = %v, want %v", err, ErrRawRepositorySourceChanged)
	}
}

// --- the prefix-keyed base-corpus source door ---------------------------

// TestBaseCorpusMutationServesADedicatedOwner is the reachability claim.
//
// The lifecycle registers exactly one DEDICATED owner per tracked repository
// prefix, and a dedicated registration has no RawRepositoryRegistration handle,
// so the registration-keyed mutation door cannot address it. AcquireBaseCorpus
// nevertheless pins a request against that same owner state and observes its
// source, so without a prefix-keyed mutation door the witness of every
// repository a daemon actually tracks is unreachable and every pin can only
// answer "unwitnessed".
func TestBaseCorpusMutationServesADedicatedOwner(t *testing.T) {
	m := NewLeaseManager()
	owner := RepositoryOwner{GraphID: "g1", CheckoutID: "c1", Incarnation: "i1", RepoPrefix: "repo"}
	if err := m.RegisterRepositoryOwner(owner); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LookupRawRepositoryRegistration("repo", "/tmp/repo"); err == nil {
		t.Fatal("a dedicated owner must not resolve to a raw registration; the door under test would be redundant")
	}

	pin := m.AcquireBaseCorpus("repo")
	defer pin.Release()
	if !pin.OwnerPinned() {
		t.Fatal("the dedicated owner was not pinned")
	}

	write, err := m.AcquireBaseCorpusMutation(context.Background(), "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("AcquireBaseCorpusMutation over a dedicated owner: %v", err)
	}
	if _, err := write.CompleteUnchanged("content-a"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	write.Release()

	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusChanged) {
		t.Fatalf("pin after a base-corpus mutation = %v, want ErrBaseCorpusChanged", err)
	}
	fresh := m.AcquireBaseCorpus("repo")
	defer fresh.Release()
	if !fresh.Witnessed() {
		t.Fatal("the mutation left no witness for the next request")
	}
	if err := fresh.ValidateCurrent(); err != nil {
		t.Fatalf("a pin taken after the mutation = %v, want nil", err)
	}
}

// TestBaseCorpusMutationKeepsTheRawRootCheck: a RAW registration is still
// matched on its canonical root, so a prefix rebound to a different root is
// refused exactly as LookupRawRepositoryRegistration refuses it. The door is
// not dedicated-to-raw inference.
func TestBaseCorpusMutationKeepsTheRawRootCheck(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	root := reg.Owner().RootIdentity

	if _, err := m.AcquireBaseCorpusMutation(context.Background(), "raw", "other-root"); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("wrong canonical root = %v, want ErrRepositoryOwnerUnknown", err)
	}
	if _, err := m.AcquireBaseCorpusMutation(context.Background(), "raw", ""); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("missing canonical root = %v, want ErrRepositoryOwnerUnknown", err)
	}
	if _, err := m.AcquireBaseCorpusMutation(context.Background(), "absent", root); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("unregistered prefix = %v, want ErrRepositoryOwnerUnknown", err)
	}
	write, err := m.AcquireBaseCorpusMutation(context.Background(), "raw", root)
	if err != nil {
		t.Fatalf("matching canonical root: %v", err)
	}
	if err := write.Complete("source-a"); err != nil {
		t.Fatal(err)
	}
	write.Release()

	// A closing registration is refused rather than mutated.
	if _, err := m.CloseRawRepositoryAdmission(reg); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AcquireBaseCorpusMutation(context.Background(), "raw", root); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("closing owner = %v, want ErrRepositoryAdmissionClosed", err)
	}
}

// TestCompleteUnchangedRestoresTheObservationItDisplaced is the no-op claim.
//
// A revision is allocated at ACQUISITION, before anybody can know whether the
// payload will move a byte, so a mutation that turns out to have written
// nothing would still tell every live pin the source moved. Completing with the
// fingerprint that was already there restores the exact observation readers
// hold; a different fingerprint publishes the new revision as usual.
func TestCompleteUnchangedRestoresTheObservationItDisplaced(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	first := privateRawSource(t, m, reg, "content-a")

	pin := m.AcquireBaseCorpus("raw")
	defer pin.Release()
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("the pin must start clean: %v", err)
	}

	write, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	if got := write.PriorFingerprint(); got != "content-a" {
		t.Fatalf("PriorFingerprint() = %q, want %q", got, "content-a")
	}
	if write.Revision() != first+1 {
		t.Fatalf("acquisition revision = %d, want %d", write.Revision(), first+1)
	}
	restored, err := write.CompleteUnchanged("content-a")
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("a mutation that wrote the same content must restore the observation it displaced")
	}
	write.Release()
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("a no-op mutation told a live pin the source moved: %v", err)
	}

	read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, first)
	if err != nil {
		t.Fatalf("the restored revision must still be the current one: %v", err)
	}
	read.Release()

	// Content that DID move publishes the new revision.
	moved, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	restored, err = moved.CompleteUnchanged("content-b")
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("different content must not be reported as unchanged")
	}
	moved.Release()
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusChanged) {
		t.Fatalf("a mutation that moved content = %v, want ErrBaseCorpusChanged", err)
	}
}

// TestCompleteUnchangedOnAFirstMutationPublishes: an owner with no available
// source to restore takes the ordinary publish path, so the first mutation of a
// never-captured owner is never silently swallowed.
func TestCompleteUnchangedOnAFirstMutationPublishes(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	write, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	if got := write.PriorFingerprint(); got != "" {
		t.Fatalf("PriorFingerprint() on a never-captured owner = %q, want empty", got)
	}
	restored, err := write.CompleteUnchanged("content-a")
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("there was no prior observation to restore")
	}
	write.Release()
	read, err := m.AcquireRawRepositorySnapshot(context.Background(), reg, 0)
	if err != nil {
		t.Fatalf("the first mutation must publish: %v", err)
	}
	defer read.Release()
	if got := read.Witness().Fingerprint; got != "content-a" {
		t.Fatalf("fingerprint = %q, want content-a", got)
	}
}

// TestInvalidateBaseCorpusSourceMovesAPinDrainingBehindAClose is the
// closing-owner half of the source witness.
//
// CloseRepositoryAdmission refuses NEW leases and leaves the ones already out
// valid ("existing leases remain valid"), and finalization cannot run until
// they drain, so a request that pinned the base corpus before the close is
// still reading and still answering. A generation-zero write admitted at that
// moment cannot take a mutation lease — and must not therefore be reported as
// unwitnessed, because that pin would answer "as exact as the route said it
// was" about a corpus that has just moved under it.
func TestInvalidateBaseCorpusSourceMovesAPinDrainingBehindAClose(t *testing.T) {
	m := NewLeaseManager()
	owner := RepositoryOwner{GraphID: "g1", CheckoutID: "c1", Incarnation: "i1", RepoPrefix: "repo"}
	if err := m.RegisterRepositoryOwner(owner); err != nil {
		t.Fatal(err)
	}

	// An owner nothing has written to yet has no source data: there is nothing
	// to move and nothing that could be holding a witness on it.
	if moved, err := m.InvalidateBaseCorpusSource("repo"); moved || err != nil {
		t.Fatalf("InvalidateBaseCorpusSource on a never-written owner = (%v, %v), want (false, nil)", moved, err)
	}
	if moved, err := m.InvalidateBaseCorpusSource("absent"); moved || !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("InvalidateBaseCorpusSource on an unregistered prefix = (%v, %v), want (false, ErrRepositoryOwnerUnknown)", moved, err)
	}

	write, err := m.AcquireBaseCorpusMutation(context.Background(), "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("AcquireBaseCorpusMutation: %v", err)
	}
	if err := write.Complete("content-a"); err != nil {
		t.Fatal(err)
	}
	write.Release()

	pin := m.AcquireBaseCorpus("repo")
	defer pin.Release()
	if !pin.Witnessed() {
		t.Fatal("the pin captured no witness to compare against")
	}
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("the pin must start clean: %v", err)
	}

	drain, err := m.CloseRepositoryAdmission(owner)
	if err != nil {
		t.Fatalf("CloseRepositoryAdmission: %v", err)
	}
	if _, err := m.AcquireBaseCorpusMutation(context.Background(), "repo", "/tmp/repo"); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("a closing owner = %v, want ErrRepositoryAdmissionClosed", err)
	}
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("closing an admission is not itself a source change: %v", err)
	}

	moved, err := m.InvalidateBaseCorpusSource("repo")
	if err != nil {
		t.Fatalf("InvalidateBaseCorpusSource under a closing owner: %v", err)
	}
	if !moved {
		t.Fatal("the write under a closing owner moved no observation")
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusChanged) {
		t.Fatalf("pin after a write admitted under a closing owner = %v, want ErrBaseCorpusChanged", err)
	}

	// The witness moved without taking a reader: a closed admission may have
	// drained already, and a reader taken after that would sit behind a drain
	// whose channel is closed.
	pin.Release()
	select {
	case <-drain.Done():
	default:
		t.Fatal("moving the witness left a reader behind a closed admission")
	}
}

// TestARestoredRevisionIsNeverHandedToANewSourceState pins the high-water mark.
//
// CompleteUnchanged moves a revision DOWN — it restores the observation it
// displaced. Without a high-water floor the next mutation reuses the revision
// number the restore gave back, and a pin taken mid-mutation (revision R+1,
// nothing available) compares EQUAL to a different source state that happens to
// wear the same number.
func TestARestoredRevisionIsNeverHandedToANewSourceState(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	first := privateRawSource(t, m, reg, "content-a")

	write, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	// A request that pins the corpus while that mutation is in flight.
	pin := m.AcquireBaseCorpus("raw")
	defer pin.Release()
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("a pin taken mid-mutation must start clean: %v", err)
	}
	restored, err := write.CompleteUnchanged("content-a")
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("the no-op mutation did not restore the observation it displaced")
	}
	write.Release()
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusChanged) {
		t.Fatalf("the mid-mutation pin must see the restore: %v", err)
	}

	second, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if second.Revision() <= first+1 {
		t.Fatalf("the next mutation took revision %d, which the restore already handed out (first=%d)", second.Revision(), first)
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusChanged) {
		t.Fatalf("mid-mutation pin against a reused revision = %v, want ErrBaseCorpusChanged", err)
	}
}

// TestInvalidateBaseCorpusSourceIgnoresTheCanonicalRoot: the root check on the
// lease door decides whether a caller may WRITE through an owner's gate. It is
// not a reason to leave that owner's readers holding a witness a write has
// already invalidated, so the witness door does not apply it — the write lands
// on the prefix those readers pinned either way.
func TestInvalidateBaseCorpusSourceIgnoresTheCanonicalRoot(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "raw", "one")
	privateRawSource(t, m, reg, "content-a")

	pin := m.AcquireBaseCorpus("raw")
	defer pin.Release()
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("the pin must start clean: %v", err)
	}
	if _, err := m.AcquireBaseCorpusMutation(context.Background(), "raw", "other-root"); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("the lease door must still refuse a mismatched root: %v", err)
	}

	moved, err := m.InvalidateBaseCorpusSource("raw")
	if err != nil || !moved {
		t.Fatalf("InvalidateBaseCorpusSource = (%v, %v), want (true, nil)", moved, err)
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusChanged) {
		t.Fatalf("pin after a write the root check refused = %v, want ErrBaseCorpusChanged", err)
	}
}
