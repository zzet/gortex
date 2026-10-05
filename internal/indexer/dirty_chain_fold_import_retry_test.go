package indexer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Each interrupted slice is rolled back; only the following successful call
// advances its durable cursor. This exercises the driver, not the delay helper.
type interruptedImportFold struct {
	fakeChainFold
	refusal error
}

func (f *interruptedImportFold) Step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.calls++
	if f.calls%2 == 1 {
		return false, f.refusal
	}
	f.copied++
	return f.copied == f.rows, nil
}

func TestImportFoldRetryUsesWriterGapsAndRetainsOtherBackoff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		imported bool
		refusal  error
		wait     time.Duration
	}{
		{"import_writer", true, store_sqlite.ErrChainFoldYielded, 30 * time.Millisecond},
		{"import_wal", true, store_sqlite.ErrChainFoldWALMark, 150 * time.Millisecond},
		{"ordinary_writer", false, store_sqlite.ErrChainFoldYielded, 30 * time.Millisecond},
		{"ordinary_wal", false, store_sqlite.ErrChainFoldWALMark, 150 * time.Millisecond},
		{"ordinary_other_retryable", false, errFakeYielded, 150 * time.Millisecond},
		{"other_retryable", true, errFakeYielded, 150 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tc.imported {
					ctx = context.WithValue(ctx, importFoldPublicationKey{}, &importFoldPublication{})
				}
				fold := &interruptedImportFold{fakeChainFold: fakeChainFold{rows: 3}, refusal: tc.refusal}
				started := time.Now()
				steps, retries, err := runChainFoldSteps(ctx, fold, func(err error) bool { return errors.Is(err, tc.refusal) }, nil)
				if err != nil || steps != 3 || retries != 3 || fold.copied != 3 || fold.calls != 6 {
					t.Fatalf("durable progress: steps=%d retries=%d copied=%d calls=%d err=%v", steps, retries, fold.copied, fold.calls, err)
				}
				if elapsed := time.Since(started); elapsed != tc.wait {
					t.Fatalf("driver retry time=%s, want %s", elapsed, tc.wait)
				}
			})
		})
	}
}

func TestImportFoldBeginRetryAndDemandCancellation(t *testing.T) {
	for _, mode := range []string{"import", "ordinary"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if mode == "import" {
					ctx = context.WithValue(ctx, importFoldPublicationKey{}, &importFoldPublication{})
				}
				backend := &yieldingBeginBackend{failures: 1, failure: store_sqlite.ErrChainFoldYielded, fold: &fakeChainFold{rows: 1, stepRows: 1}}
				started := time.Now()
				fold, retries, err := beginChainFoldWatched(ctx, backend, []int64{1}, 2, "import", nil)
				if err != nil || fold == nil || retries != 1 || backend.calls != 2 || time.Since(started) != 10*time.Millisecond {
					t.Fatalf("retried begin: retries=%d calls=%d elapsed=%s err=%v", retries, backend.calls, time.Since(started), err)
				}

				// Persistent demand must still stop at the original context deadline,
				// without pretending that a refused begin or step committed anything.
				bounded, cancel := context.WithTimeout(ctx, 35*time.Millisecond)
				defer cancel()
				blocked := &yieldingBeginBackend{failures: -1, failure: store_sqlite.ErrChainFoldYielded}
				if fold, retries, err := beginChainFoldWatched(bounded, blocked, []int64{1}, 2, "import", nil); fold != nil || !errors.Is(err, context.DeadlineExceeded) || retries != 4 || blocked.calls != 4 {
					t.Fatalf("cancelled begin: fold=%v retries=%d calls=%d err=%v", fold, retries, blocked.calls, err)
				}
				bounded, cancelStep := context.WithTimeout(ctx, 35*time.Millisecond)
				defer cancelStep()
				blockedStep := refusingFold{chainFoldSteps: &fakeChainFold{}, err: store_sqlite.ErrChainFoldYielded}
				steps, retries, err := runChainFoldSteps(bounded, blockedStep, backend.StepRetryable, nil)
				if !errors.Is(err, context.DeadlineExceeded) || steps != 0 || retries != 4 {
					t.Fatalf("cancelled steps: steps=%d retries=%d err=%v", steps, retries, err)
				}
			})
		})
	}
}
