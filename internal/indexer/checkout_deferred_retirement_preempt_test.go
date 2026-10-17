package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// failedRetirementFixture builds one failed generation a fresh lifecycle
// discovers as owed retirement work.
func failedRetirementFixture(t *testing.T) (*store_sqlite.Store, int64, time.Time) {
	t.Helper()
	fixture := newSparseBuildFlightFixture(t)
	started := time.Now()
	request := fixture.request
	request.Identity.CreatedAt = started.Add(-time.Hour).Unix()
	wantErr := errors.New("leave a failed generation to retire")
	request.PrePublish = func(context.Context, int64) error { return wantErr }
	generationID, _, err := fixture.builder.Build(context.Background(), request)
	if !errors.Is(err, wantErr) {
		t.Fatalf("fixture build error = %v, want %v", err, wantErr)
	}
	return fixture.store, generationID, started
}

// blockingChunk stands in for a retirement chunk that takes far longer than
// an edit may wait: it holds until its context ends (as an interrupted
// statement does) or the test ends.
func blockingChunk(t *testing.T, inChunk chan<- struct{}) func(context.Context, int64) error {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(ctx context.Context, generationID int64) error {
		inChunk <- struct{}{}
		select {
		case <-ctx.Done():
			return fmt.Errorf("payload generation gc: generation %d: %w", generationID, ctx.Err())
		case <-release:
			return errors.New("chunk outlived the test")
		}
	}
}

func TestDeferredRetirementSliceYieldsMidChunkToAnInteractiveWriter(t *testing.T) {
	store, generationID, started := failedRetirementFixture(t)
	lifecycle := newGenerationRetirementLifecycle(store, started)
	var demand atomic.Bool
	lifecycle.interactiveDemand = demand.Load
	inChunk := make(chan struct{}, 1)
	lifecycle.retireOwedSlice = blockingChunk(t, inChunk)
	before := DeferredRetirementPreemptions()

	type result struct {
		retired int
		pending bool
		err     error
		at      time.Time
	}
	done := make(chan result, 1)
	go func() {
		retired, pending, err := lifecycle.SweepDeferredRetirements(context.Background())
		done <- result{retired, pending, err, time.Now()}
	}()
	select {
	case <-inChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("the slice never started its chunk")
	}
	waitFrom := time.Now()
	demand.Store(true)
	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the running chunk was not given up to the interactive writer")
	}
	if waited := got.at.Sub(waitFrom); waited > 100*time.Millisecond {
		t.Fatalf("interactive writer waited %s for the chunk, want <= 100ms", waited)
	}
	if got.retired != 0 || !got.pending || got.err != nil {
		t.Fatalf("preempted slice = (%d, %v, %v), want (0, true, <nil>)", got.retired, got.pending, got.err)
	}
	if DeferredRetirementPreemptions() <= before {
		t.Fatal("preemption was not counted")
	}

	// While the writer still waits, no slice starts.
	lifecycle.retireOwedSlice = func(context.Context, int64) error {
		t.Fatal("a slice started while an interactive writer waited")
		return nil
	}
	if retired, pending, err := lifecycle.SweepDeferredRetirements(context.Background()); retired != 0 || !pending || err != nil {
		t.Fatalf("slice under demand = (%d, %v, %v), want (0, true, <nil>)", retired, pending, err)
	}

	// Once the writer is served, the same generation resumes and drains.
	demand.Store(false)
	lifecycle.retireOwedSlice = nil
	for attempt := 0; attempt < 10; attempt++ {
		_, pending, err := lifecycle.SweepDeferredRetirements(context.Background())
		if err != nil {
			t.Fatalf("resumed sweep attempt %d: %v", attempt, err)
		}
		if !pending {
			requireGenerationRetired(t, store, generationID)
			return
		}
	}
	t.Fatal("the preempted retirement did not resume and drain")
}

func TestDeferredRetirementStarvedSweepIgnoresInteractiveDemand(t *testing.T) {
	store, generationID, started := failedRetirementFixture(t)
	lifecycle := newGenerationRetirementLifecycle(store, started)
	lifecycle.interactiveDemand = func() bool { return true }
	lifecycle.retirementStarvationLimit = time.Minute
	// The last progress is older than the limit: this slice must run to the
	// end of its chunks even though an interactive writer waits.
	lifecycle.deferredRetirementProgress.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	for attempt := 0; attempt < 10; attempt++ {
		_, pending, err := lifecycle.SweepDeferredRetirements(context.Background())
		if err != nil {
			t.Fatalf("starved sweep attempt %d: %v", attempt, err)
		}
		if !pending {
			requireGenerationRetired(t, store, generationID)
			return
		}
	}
	t.Fatal("a starved sweep kept yielding to interactive demand")
}

// An admitted edit cycle holds the build lane and writes its payload before it
// lets the lane go, with no writer parked on the store's gate in between: the
// sweep stands down for it as it does for a waiting writer — no slice starts,
// and a running chunk is given up.
func TestDeferredRetirementStandsDownWhileAnEditCycleHoldsTheLane(t *testing.T) {
	store, generationID, started := failedRetirementFixture(t)
	lifecycle := newGenerationRetirementLifecycle(store, started)
	lifecycle.interactiveDemand = func() bool { return false }
	var editCycle atomic.Bool
	lifecycle.analysisEditCycle = editCycle.Load
	inChunk := make(chan struct{}, 1)
	lifecycle.retireOwedSlice = blockingChunk(t, inChunk)

	done := make(chan error, 1)
	var stoppedAt atomic.Int64
	go func() {
		retired, pending, err := lifecycle.SweepDeferredRetirements(context.Background())
		stoppedAt.Store(time.Now().UnixNano())
		if err == nil && (retired != 0 || !pending) {
			err = fmt.Errorf("slice = (%d, %v), want (0, true)", retired, pending)
		}
		done <- err
	}()
	select {
	case <-inChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("the slice never started its chunk")
	}
	cycleFrom := time.Now()
	editCycle.Store(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("preempted slice: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the running chunk kept going through an edit cycle")
	}
	if waited := time.Unix(0, stoppedAt.Load()).Sub(cycleFrom); waited > 100*time.Millisecond {
		t.Fatalf("the edit cycle waited %s for the chunk, want <= 100ms", waited)
	}

	lifecycle.retireOwedSlice = func(context.Context, int64) error {
		t.Fatal("a slice started while an edit cycle held the lane")
		return nil
	}
	if retired, pending, err := lifecycle.SweepDeferredRetirements(context.Background()); retired != 0 || !pending || err != nil {
		t.Fatalf("slice during an edit cycle = (%d, %v, %v), want (0, true, <nil>)", retired, pending, err)
	}

	editCycle.Store(false)
	lifecycle.retireOwedSlice = nil
	for attempt := 0; attempt < 10; attempt++ {
		_, pending, err := lifecycle.SweepDeferredRetirements(context.Background())
		if err != nil {
			t.Fatalf("sweep after the cycle, attempt %d: %v", attempt, err)
		}
		if !pending {
			requireGenerationRetired(t, store, generationID)
			return
		}
	}
	t.Fatal("the retirement did not drain after the edit cycle")
}

// While interactive work wants the writer the sweep does nothing at all: no
// catalog scan for candidates, no slice.
func TestDeferredRetirementDoesNotScanWhileAnEditCycleHoldsTheLane(t *testing.T) {
	store, _, started := failedRetirementFixture(t)
	lifecycle := newGenerationRetirementLifecycle(store, started)
	lifecycle.interactiveDemand = func() bool { return false }
	lifecycle.analysisEditCycle = func() bool { return true }
	lifecycle.retireOwedSlice = func(context.Context, int64) error {
		t.Fatal("a slice started while an edit cycle held the lane")
		return nil
	}
	scans := deferredRetirementScans.Load()
	for i := 0; i < 3; i++ {
		if retired, pending, err := lifecycle.SweepDeferredRetirements(context.Background()); retired != 0 || !pending || err != nil {
			t.Fatalf("sweep during an edit cycle = (%d, %v, %v), want (0, true, <nil>)", retired, pending, err)
		}
	}
	if got := deferredRetirementScans.Load() - scans; got != 0 {
		t.Fatalf("the sweep scanned the catalog %d times during an edit cycle", got)
	}
}

// A generation the catalog still references is refused once and parked: it
// is not offered again, and the sweep reports nothing pending, until a
// reference may have been released.
func TestDeferredRetirementParksAStillReferencedGeneration(t *testing.T) {
	store, generationID, started := failedRetirementFixture(t)
	lifecycle := newGenerationRetirementLifecycle(store, started)
	lifecycle.interactiveDemand = func() bool { return false }
	var offers int
	lifecycle.retireOwedSlice = func(_ context.Context, id int64) error {
		offers++
		return fmt.Errorf("%w: generation %d", store_sqlite.ErrCatalogGenerationReferenced, id)
	}
	retired, pending, err := lifecycle.SweepDeferredRetirements(context.Background())
	if retired != 0 || pending || err != nil || offers != 1 {
		t.Fatalf("first sweep = (%d, %v, %v) after %d offers; want (0, false, <nil>) after one", retired, pending, err, offers)
	}
	for i := 0; i < 3; i++ {
		if _, pending, err := lifecycle.SweepDeferredRetirements(context.Background()); pending || err != nil {
			t.Fatalf("sweep %d over a parked generation = (%v, %v), want nothing pending", i, pending, err)
		}
	}
	if offers != 1 {
		t.Fatalf("a still-referenced generation was offered %d times, want once", offers)
	}
	noteRetirementReferenceReleased()
	lifecycle.retireOwedSlice = nil
	for attempt := 0; attempt < 10; attempt++ {
		_, pending, err := lifecycle.SweepDeferredRetirements(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !pending {
			requireGenerationRetired(t, store, generationID)
			return
		}
	}
	t.Fatal("the released generation was not retired")
}

// A starved sweep stops yielding to waiting writers, never to an edit cycle:
// no slice starts while one holds the lane, and a chunk in flight when one
// starts is given up.
func TestStarvedDeferredRetirementStillStandsDownForAnEditCycle(t *testing.T) {
	store, _, started := failedRetirementFixture(t)
	lifecycle := newGenerationRetirementLifecycle(store, started)
	lifecycle.interactiveDemand = func() bool { return true }
	lifecycle.retirementStarvationLimit = time.Minute
	lifecycle.deferredRetirementProgress.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	var editCycle atomic.Bool
	editCycle.Store(true)
	lifecycle.analysisEditCycle = editCycle.Load
	lifecycle.retireOwedSlice = func(context.Context, int64) error {
		t.Fatal("a starved slice started while an edit cycle held the lane")
		return nil
	}
	if retired, pending, err := lifecycle.SweepDeferredRetirements(context.Background()); retired != 0 || !pending || err != nil {
		t.Fatalf("starved sweep during an edit cycle = (%d, %v, %v), want (0, true, <nil>)", retired, pending, err)
	}

	editCycle.Store(false)
	inChunk := make(chan struct{}, 1)
	lifecycle.retireOwedSlice = blockingChunk(t, inChunk)
	done := make(chan time.Time, 1)
	go func() {
		_, _, _ = lifecycle.SweepDeferredRetirements(context.Background())
		done <- time.Now()
	}()
	select {
	case <-inChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("the starved slice never started")
	}
	cycleFrom := time.Now()
	editCycle.Store(true)
	select {
	case at := <-done:
		if waited := at.Sub(cycleFrom); waited > 100*time.Millisecond {
			t.Fatalf("the edit cycle waited %s for a starved chunk, want <= 100ms", waited)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a starved chunk kept the writer through an edit cycle")
	}
}
