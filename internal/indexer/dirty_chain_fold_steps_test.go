package indexer

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graphview"
)

// errFakeYielded stands in for the store giving a step back to an edit.
var errFakeYielded = errors.New("fake: step yielded to an edit")

// fakeChainFold is a fold of rows rows in the store, stepped by stepRows; the
// steps listed in yieldAt are given back to an edit once each.
type fakeChainFold struct {
	rows, copied, stepRows, calls int
	yieldAt                       map[int]bool
	released                      bool
	abandoned                     bool
}

func (f *fakeChainFold) Step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.calls++
	if f.yieldAt[f.calls] {
		return false, errFakeYielded
	}
	f.copied += min(f.stepRows, f.rows-f.copied)
	return f.copied >= f.rows, nil
}
func (f *fakeChainFold) Release(context.Context) error { f.released = true; return nil }
func (f *fakeChainFold) Abandon(context.Context) error { f.abandoned = true; return nil }

// fakeFoldBackend records rebases.
type fakeFoldBackend struct {
	rebases [][3]int64
	fail    error
}

func (b *fakeFoldBackend) BeginChainFold(context.Context, []int64, int64, string) (chainFoldSteps, error) {
	return &fakeChainFold{}, nil
}
func (b *fakeFoldBackend) StepRetryable(err error) bool { return errors.Is(err, errFakeYielded) }

func (b *fakeFoldBackend) RebaseViewGeneration(_ context.Context, id, from, to int64) error {
	if b.fail != nil {
		return b.fail
	}
	b.rebases = append(b.rebases, [3]int64{id, from, to})
	return nil
}

// A fold runs to its last step whatever edits do meanwhile: a step the store
// gives back to an edit is asked for again, only its context stops it, and a
// stopped fold resumes from its cursor within the process.
func TestChainFoldRunsInStepsAndAnEditDoesNotStopIt(t *testing.T) {
	backend := &fakeFoldBackend{}
	fold := &fakeChainFold{rows: 9_000, stepRows: 1_700, yieldAt: map[int]bool{2: true, 4: true}}
	steps, retries, err := runChainFoldSteps(context.Background(), fold, backend.StepRetryable, nil)
	if err != nil || steps != 6 || retries != 2 || fold.copied != 9_000 {
		t.Fatalf("fold: steps=%d retries=%d copied=%d err=%v; want 6 steps, the 2 given-back steps retried, every row copied", steps, retries, fold.copied, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := &fakeChainFold{rows: 9_000, stepRows: 1_700}
	_, _, err = runChainFoldSteps(ctx, stopped, backend.StepRetryable, func(step int) {
		if step == 2 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || stopped.copied != 2*1_700 {
		t.Fatalf("stopped fold: copied=%d err=%v; want it to stop at the step boundary", stopped.copied, err)
	}
	resumed, _, err := runChainFoldSteps(context.Background(), stopped, backend.StepRetryable, nil)
	if err != nil || stopped.copied != 9_000 || resumed != 4 {
		t.Fatalf("resumed fold: steps=%d copied=%d err=%v; want the 4 remaining steps", resumed, stopped.copied, err)
	}
	failing := &fakeChainFold{rows: 9_000, stepRows: 1_700}
	bounded, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, retries, err := runChainFoldSteps(bounded, &erringFold{failing}, backend.StepRetryable, nil); err == nil || retries != 0 || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a fold whose step failed for good: retries=%d err=%v; want the failure returned at once", retries, err)
	}
}

// erringFold fails every step with an error the backend does not retry.
type erringFold struct{ *fakeChainFold }

func (e *erringFold) Step(context.Context) (bool, error) { return false, errors.New("disk full") }

// A finished fold lands by the routed chain: a flip when nothing was published
// above it, a rebase of the lowest layer above it otherwise, and a preferred
// parent when the route left the folded chain.
func TestChainFoldLandingPlans(t *testing.T) {
	folded := []int64{11, 12, 13, 14}
	cases := []struct {
		routed []int64
		want   foldLanding
	}{
		{[]int64{11, 12, 13, 14}, foldLanding{kind: foldLandFlip}},
		{[]int64{11, 12, 13, 14, 20, 21}, foldLanding{kind: foldLandRebase, rebase: 20, from: 14}},
		{[]int64{30, 31}, foldLanding{kind: foldLandMoved}},
		{[]int64{11, 12}, foldLanding{kind: foldLandMoved}},
	}
	for _, tc := range cases {
		if got := planFoldLanding(tc.routed, folded); got != tc.want {
			t.Fatalf("routed %v: landing %+v, want %+v", tc.routed, got, tc.want)
		}
	}
}

// Landing a fold rebases exactly the lowest layer above the folded chain onto
// the fold, and nothing else.
func TestLandingAFoldRebasesOnlyTheLowestLayerAbove(t *testing.T) {
	c := &CheckoutCoordinator{logger: zap.NewNop()}
	backend := &fakeFoldBackend{}
	flipped := int64(0)
	flip := func(_ context.Context, to int64) error { flipped = to; return nil }
	var released []int64
	release := func(_ context.Context, ids []int64) { released = append(released, ids...) }
	landing, err := c.landChainFold(context.Background(), backend, []int64{11, 12, 13, 20, 21, 22}, []int64{11, 12, 13}, 99, flip, release)
	if err != nil || landing.kind != foldLandRebase {
		t.Fatalf("landing %+v err %v", landing, err)
	}
	if want := [][3]int64{{20, 13, 99}}; !reflect.DeepEqual(backend.rebases, want) || flipped != 0 {
		t.Fatalf("rebases %v flipped %d; want only %v", backend.rebases, flipped, want)
	}
	if !reflect.DeepEqual(released, []int64{11, 12, 13}) {
		t.Fatalf("released %v, want the folded members", released)
	}
	backend.rebases = nil
	if _, err := c.landChainFold(context.Background(), backend, []int64{11, 12, 13}, []int64{11, 12, 13}, 99, flip, release); err != nil || flipped != 99 || len(backend.rebases) != 0 {
		t.Fatalf("nothing above the chain: flipped %d rebases %v err %v; want a flip only", flipped, backend.rebases, err)
	}
}

// The depth an edit is held to while a fold runs counts the folding prefix as
// one layer; at the bound it folds what it can without waiting.
func TestChainBoundWhileAFoldRuns(t *testing.T) {
	folding := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	above := func(n int) []int64 {
		routed := append([]int64(nil), folding...)
		for i := 0; i < n; i++ {
			routed = append(routed, int64(100+i))
		}
		return routed
	}
	if got := effectiveChainDepth(above(3), folding); got != 4 {
		t.Fatalf("effective depth with 3 layers above an 8-layer fold = %d, want 4", got)
	}
	if got := chainBoundActionWithin(above(3), folding, maxPhysicalChainDepth); got != chainActionChain {
		t.Fatalf("3 layers above a running fold: %s, want %s", got, chainActionChain)
	}
	if got := chainBoundActionWithin(above(7), folding, maxPhysicalChainDepth); got != chainActionFoldUpper {
		t.Fatalf("7 layers above a running fold (effective 8): %s, want %s", got, chainActionFoldUpper)
	}
	if got := chainBoundActionWithin(folding, nil, maxPhysicalChainDepth); got != chainActionFoldAll {
		t.Fatalf("8 layers, no fold: %s, want %s", got, chainActionFoldAll)
	}
	if got := chainBoundActionWithin(folding[:5], nil, maxPhysicalChainDepth); got != chainActionChain {
		t.Fatalf("5 layers, no fold: %s, want %s", got, chainActionChain)
	}
	// While the materializer composes no more than maxDirtyChainDepth layers the
	// physical limit equals the effective one: an edit above a running fold folds
	// the layers above it, and one over a full chain folds it all, as before.
	if got := chainBoundActionWithin(above(1), folding, maxDirtyChainDepth); got != chainActionFoldUpper {
		t.Fatalf("1 layer above an 8-layer fold, physical limit 8: %s, want %s", got, chainActionFoldUpper)
	}
	if got := chainBoundActionWithin(folding, folding, maxDirtyChainDepth); got != chainActionFoldAll {
		t.Fatalf("an 8-layer fold with nothing above, physical limit 8: %s, want %s", got, chainActionFoldAll)
	}
	// In production a chain as deep as the materializer allows never chains further.
	full := above(maxChainWalkDepth - len(folding))
	if got := chainBoundAction(full, folding); got == chainActionChain {
		t.Fatalf("a %d-layer chain at the materializer bound chained further", len(full))
	}
	// The production limit never exceeds what the materializer composes.
	if maxChainWalkDepth > graphview.MaxDirtyChainDepth || maxChainWalkDepth < maxDirtyChainDepth {
		t.Fatalf("maxChainWalkDepth = %d, want within [%d, %d]", maxChainWalkDepth, maxDirtyChainDepth, graphview.MaxDirtyChainDepth)
	}
}
