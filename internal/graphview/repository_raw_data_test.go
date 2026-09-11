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

// --- W5.3: initial capture and witness validation -----------------------

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
