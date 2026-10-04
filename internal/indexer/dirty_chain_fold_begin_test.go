package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type yieldingBeginBackend struct {
	fakeFoldBackend
	failures int
	failure  error
	calls    int
	fold     *fakeChainFold
}

func (b *yieldingBeginBackend) BeginChainFold(context.Context, []int64, int64, string) (chainFoldSteps, error) {
	b.calls++
	if b.failures < 0 || b.calls <= b.failures {
		return nil, b.failure
	}
	return b.fold, nil
}
func (*yieldingBeginBackend) StepRetryable(err error) bool {
	return errors.Is(err, store_sqlite.ErrChainFoldYielded) || errors.Is(err, store_sqlite.ErrChainFoldWALMark)
}

func TestChainFoldBeginRetriesWriterYieldBeforeCopying(t *testing.T) {
	backend := &yieldingBeginBackend{failures: 1, failure: store_sqlite.ErrChainFoldYielded, fold: &fakeChainFold{rows: 9, stepRows: 3}}
	fold, retries, err := beginChainFoldWatched(t.Context(), backend, []int64{1, 2}, 3, "test", nil)
	if err != nil || retries != 1 || backend.calls != 2 {
		t.Fatalf("begin retries=%d calls=%d err=%v", retries, backend.calls, err)
	}
	steps, _, err := runChainFoldSteps(t.Context(), fold, backend.StepRetryable, nil)
	if err != nil || steps != 3 || backend.fold.copied != 9 {
		t.Fatalf("copy after retried begin: steps=%d copied=%d err=%v", steps, backend.fold.copied, err)
	}
	if err := fold.Release(t.Context()); err != nil || !backend.fold.released {
		t.Fatalf("reservation not released: %v", err)
	}
}

func TestChainFoldBeginRetryStopsAtCancellationAndStarvation(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		backend := &yieldingBeginBackend{failures: -1, failure: store_sqlite.ErrChainFoldYielded}
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		fold, _, err := beginChainFoldWatched(ctx, backend, []int64{1}, 2, "test", nil)
		if fold != nil || !errors.Is(err, context.DeadlineExceeded) || backend.calls != 1 {
			t.Fatalf("cancelled begin: fold=%v calls=%d err=%v", fold, backend.calls, err)
		}
	})
	t.Run("starve", func(t *testing.T) {
		backend := &yieldingBeginBackend{failures: -1, failure: store_sqlite.ErrChainFoldWALMark}
		start := time.Unix(100, 0)
		watch := &foldStepWatch{starve: time.Second, progress: start, started: start, now: func() time.Time { return start.Add(time.Second) }}
		fold, _, err := beginChainFoldWatched(t.Context(), backend, []int64{1}, 2, "test", watch)
		if fold != nil || !errors.Is(err, errChainFoldStarved) || backend.calls != 1 {
			t.Fatalf("starved begin: fold=%v calls=%d err=%v", fold, backend.calls, err)
		}
	})
}

func TestChainFoldBeginDoesNotRetryBusyOrStaleTarget(t *testing.T) {
	for _, refusal := range []error{store_sqlite.ErrChainFoldBusy, store_sqlite.ErrChainFoldStale} {
		backend := &yieldingBeginBackend{failures: -1, failure: refusal}
		fold, retries, err := beginChainFoldWatched(t.Context(), backend, []int64{1}, 2, "test", nil)
		if fold != nil || retries != 0 || backend.calls != 1 || !errors.Is(err, refusal) {
			t.Fatalf("nonretryable begin: retries=%d calls=%d err=%v", retries, backend.calls, err)
		}
	}
}
