package indexer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func resetPrewarmTestState(t *testing.T) {
	t.Helper()
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	editDeltaPrewarmLowPriorityPause = 0
	t.Cleanup(func() { editDeltaPrewarmLowPriorityPause = 50 * time.Millisecond })
}

// prewarmObserver counts key-computing opens and finished warms.
type prewarmObserver struct {
	opens atomic.Int32
	done  chan string
}

// keyedPrewarmStack makes a coordinator's pre-warm read a keyed stack over
// the fixture's routed chain: the fixture's own view composes the base
// corpus and so has no per-stack key. The stack is the commit generation
// itself, so correcting it moves the key.
func keyedPrewarmStack(t *testing.T, f *coordinatorFixture) {
	t.Helper()
	editDeltaPrewarmOpenHook = func(_ context.Context, commit int64) (LayerBase, func(), error) {
		view := chainMaterialize(t, f)
		return commitLayerBase{Reader: view.Reader, stack: []int64{commit}}, view.Close, nil
	}
	t.Cleanup(func() { editDeltaPrewarmOpenHook = nil })
}

func observePrewarms(t *testing.T) *prewarmObserver {
	t.Helper()
	o := &prewarmObserver{done: make(chan string, 8)}
	editDeltaPrewarmKeyOpen = func() { o.opens.Add(1) }
	editDeltaPrewarmDone = func(key string) { o.done <- key }
	t.Cleanup(func() { editDeltaPrewarmKeyOpen, editDeltaPrewarmDone = nil, nil })
	return o
}

func (o *prewarmObserver) wait(t *testing.T, what string) string {
	t.Helper()
	select {
	case key := <-o.done:
		return key
	case <-time.After(90 * time.Second):
		t.Fatalf("%s: the pre-warm never finished", what)
		return ""
	}
}

func (o *prewarmObserver) none(t *testing.T, what string) {
	t.Helper()
	select {
	case key := <-o.done:
		t.Fatalf("%s: a pre-warm ran (%s)", what, key)
	case <-time.After(time.Second):
	}
}

// correctOneRow runs a real derived-row correction of generation: one
// derived edge changes, and the generation's correction epoch moves.
func correctOneRow(t *testing.T, store *store_sqlite.Store, generation int64) {
	t.Helper()
	ctx := context.Background()
	// A derived edge from a synthetic source: the correction's contract is
	// about the pass's rows, not about which nodes the generation holds.
	source := &graph.Node{ID: "rewarm_test.go::Source", FilePath: "rewarm_test.go", StartLine: 1}
	correction, err := store.BeginDerivedCorrection(ctx, store_sqlite.DerivedCorrectionRequest{
		GenerationID: generation, Pass: "rewarm_test", FromVersion: 0, ToVersion: 1,
		EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField},
	})
	if err != nil {
		t.Fatalf("BeginDerivedCorrection: %v", err)
	}
	if err := correction.ReplaceSourceEdges(ctx, []string{source.ID}, []*graph.Edge{
		{From: source.ID, To: source.ID, Kind: graph.EdgeAccessesField, FilePath: source.FilePath, Line: source.StartLine},
	}, nil); err != nil {
		t.Fatalf("ReplaceSourceEdges: %v", err)
	}
	if _, err := correction.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// The pre-warm gives way to the whole edit cycle, not only to a running
// delta: while the builder's edit-cycle predicate holds, the decision does
// not even open the stack to compute its key, and the warm makes no step; it
// completes once the cycle is done, and unregisters its predicate.
func TestPrewarmStandsDownForAnEditCycle(t *testing.T) {
	resetPrewarmTestState(t)
	o := observePrewarms(t)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	keyedPrewarmStack(t, f)
	var cycle atomic.Bool
	cycle.Store(true)
	c.builder.EditCycleActive = cycle.Load
	t.Cleanup(func() { c.builder.EditCycleActive = nil })
	var mu sync.Mutex
	steps := 0
	editDeltaPrewarmStep = func(string) {
		mu.Lock()
		steps++
		mu.Unlock()
	}
	t.Cleanup(func() { editDeltaPrewarmStep = nil })

	c.prewarmEditDeltaStackOnce(coordinatorReconcile(t, c).CommitGenerationID)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	during := steps
	mu.Unlock()
	if during != 0 || o.opens.Load() != 0 {
		t.Fatalf("while an edit cycle held the lane the pre-warm made %d steps and %d key opens", during, o.opens.Load())
	}
	cycle.Store(false)
	o.wait(t, "after the edit cycle")
	time.Sleep(100 * time.Millisecond)
	held := 0
	editCycleSources.Range(func(any, any) bool { held++; return true })
	if held != 0 {
		t.Fatalf("finished pre-warms left %d edit-cycle predicates registered", held)
	}
}

// Two pre-warm runs on one builder (a re-warm starting while the first warm
// is still going) keep their own registration: when the first ends, the
// second still stands down for an edit cycle.
func TestOverlappingPrewarmsKeepTheirStandDown(t *testing.T) {
	b := &SparseGenerationBuilder{EditCycleActive: func() bool { return true }}
	endFirst := registerEditCycleSource(b)
	endSecond := registerEditCycleSource(b)
	endFirst()
	if !prewarmStandDown() {
		t.Fatal("the second pre-warm lost its stand-down when the first ended")
	}
	endSecond()
	if prewarmStandDown() {
		t.Fatal("a stand-down outlived both runs")
	}
}

// While the startup correction is pending, a stack holding a generation it
// will change is not warmed (its caches would be orphaned); the decision is
// kept, so repeated calls cost nothing; once the deferral ends, the next
// call warms it.
func TestPrewarmIsDeferredForAStackThePendingCorrectionWillChange(t *testing.T) {
	resetPrewarmTestState(t)
	o := observePrewarms(t)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	keyedPrewarmStack(t, f)
	var pending atomic.Bool
	pending.Store(true)
	c.builder.PrewarmDeferred = func([]int64) bool { return pending.Load() }
	t.Cleanup(func() { c.builder.PrewarmDeferred = nil })

	commit := coordinatorReconcile(t, c).CommitGenerationID
	c.prewarmEditDeltaStackOnce(commit)
	o.none(t, "a stack the correction will change")
	opens := o.opens.Load()
	if opens != 1 {
		t.Fatalf("the deferred decision opened the stack %d times, want once", opens)
	}
	for range 10 {
		c.prewarmEditDeltaStackOnce(commit)
	}
	time.Sleep(200 * time.Millisecond)
	if got := o.opens.Load(); got != opens {
		t.Fatalf("repeated calls during the deferral opened the stack %d more times", got-opens)
	}
	pending.Store(false)
	c.prewarmEditDeltaStackOnce(commit)
	o.wait(t, "after the deferral ended")
}

// A call that arrives while a pre-warm is in flight is not lost: when the
// running one ends, the coordinator looks again.
func TestAPrewarmCallDuringARunningOneIsRunAfterIt(t *testing.T) {
	var s prewarmState
	if !s.begin() {
		t.Fatal("the first call did not begin")
	}
	if s.begin() {
		t.Fatal("a second call began while one was in flight")
	}
	if !s.end() {
		t.Fatal("the call that arrived meanwhile was dropped")
	}
	if !s.begin() || s.end() {
		t.Fatal("the state did not reset after the rerun")
	}
}
