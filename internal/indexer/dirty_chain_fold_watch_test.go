package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// refusingFoldBackend is a fold backend whose every step is refused with err.
type refusingFoldBackend struct {
	inner chainFoldBackend
	err   error
}

func (b refusingFoldBackend) BeginChainFold(ctx context.Context, chain []int64, to int64, owner string) (chainFoldSteps, error) {
	fold, err := b.inner.BeginChainFold(ctx, chain, to, owner)
	if err != nil {
		return nil, err
	}
	return refusingFold{chainFoldSteps: fold, err: b.err}, nil
}

func (b refusingFoldBackend) RebaseViewGeneration(ctx context.Context, generationID, fromBase, toBase int64) error {
	return b.inner.RebaseViewGeneration(ctx, generationID, fromBase, toBase)
}

func (b refusingFoldBackend) StepRetryable(err error) bool { return b.inner.StepRetryable(err) }

type refusingFold struct {
	chainFoldSteps
	err error
}

func (f refusingFold) Step(context.Context) (bool, error) { return false, f.err }

// The watch logs the first refusal, a change of cause and a summary per
// interval, and reports starvation once no step committed for its limit.
func TestFoldStepWatchLogsRefusalsAndReportsStarvation(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	clock := time.Unix(1_000_000, 0)
	w := &foldStepWatch{logger: zap.New(core), checkout: "c", to: 7, starve: time.Minute,
		now: func() time.Time { return clock }, started: clock, progress: clock}
	for i := 0; i < 5; i++ {
		clock = clock.Add(time.Second)
		if err := w.refused(store_sqlite.ErrChainFoldWALMark); err != nil {
			t.Fatalf("starved after %d s", i+1)
		}
	}
	if n := logs.FilterMessage("checkout coordinator: chain fold step refused; retrying").Len(); n != 1 {
		t.Fatalf("%d refusal lines for five refusals of one cause within the interval, want 1", n)
	}
	clock = clock.Add(time.Second)
	_ = w.refused(store_sqlite.ErrChainFoldYielded)
	clock = clock.Add(dirtyChainFoldWaitLog)
	_ = w.refused(store_sqlite.ErrChainFoldYielded)
	if n := logs.FilterMessage("checkout coordinator: chain fold step refused; retrying").Len(); n != 3 {
		t.Fatalf("%d refusal lines, want 3 (first, change of cause, interval)", n)
	}
	w.stepped()
	if logs.FilterMessage("checkout coordinator: chain fold stepping again").Len() != 1 {
		t.Fatal("no line when the fold stepped again")
	}
	clock = clock.Add(time.Minute)
	if err := w.refused(store_sqlite.ErrChainFoldWALMark); !errors.Is(err, errChainFoldStarved) {
		t.Fatalf("a fold without a step for the limit: %v, want starvation", err)
	}
	if logs.FilterLevelExact(zapcore.WarnLevel).Len() != 1 {
		t.Fatal("no warning when the fold starved")
	}
}

// A stepped fold every step of which is refused over the log's mark gives up
// at the starvation limit, and the compaction reports it refused. Overrides,
// named: the fold backend refuses every step; the starvation limit is 300 ms
// (production: dirtyChainFoldStarvation); background compactions closed (the
// fold is run by hand).
func TestStarvedSteppedFoldGivesUp(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var trigger CheckoutCycle
	for i := 0; i < dirtyChainCompactionDepth; i++ {
		trigger = mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
	}
	core, logs := observer.New(zapcore.InfoLevel)
	c.logger = zap.New(core)
	c.compaction.mu.Lock()
	c.compaction.backend = refusingFoldBackend{inner: storeChainFoldBackend{store: f.store}, err: store_sqlite.ErrChainFoldWALMark}
	c.compaction.starvation = 300 * time.Millisecond
	c.compaction.mu.Unlock()
	started := time.Now()
	report := c.compactDirtyChain(context.Background(), trigger)
	if report.Outcome != dirtyChainCompactionFoldRefused || !errors.Is(report.Err, errChainFoldStarved) {
		t.Fatalf("the starved fold: %s %v, want refused for starvation", report.Outcome, report.Err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the starved fold took %v to give up", took)
	}
	if logs.FilterMessage("checkout coordinator: chain fold step refused; retrying").Len() == 0 {
		t.Fatal("the refusals were not logged")
	}
	if len(c.foldingChain()) != 0 {
		t.Fatal("the starved fold still holds its chain")
	}
}
