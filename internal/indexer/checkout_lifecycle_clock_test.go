package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A lifecycle built without its clock (a struct literal) reads time.Now at
// every place it reads the clock, and never calls the nil func.
func TestIdleReleaseClockOnALifecycleWithoutOne(t *testing.T) {
	t.Run("NoteCheckoutUse", func(t *testing.T) {
		l := &CheckoutLifecycle{}
		before := time.Now()
		l.NoteCheckoutUse("c", "test")
		if at := l.idle.lastUse["c"]; at.Before(before) {
			t.Fatalf("use recorded at %v, want time.Now (after %v)", at, before)
		}
	})
	t.Run("ActivateCheckout", func(t *testing.T) {
		// Closing, so the call ends after recording the use: a cold
		// activation needs the rest of a built lifecycle (its logger and
		// transition context), which is not what this test is about.
		// TestCheckoutSelectionPromotesExistingQueuedCoordinator covers the
		// live-coordinator return on a struct literal.
		l := &CheckoutLifecycle{coordinatorClosing: true}
		before := time.Now()
		l.ActivateCheckout("c", "test")
		if at := l.idle.lastUse["c"]; at.Before(before) {
			t.Fatalf("use recorded at %v, want time.Now (after %v)", at, before)
		}
	})
	t.Run("ReleaseIdleCheckouts", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		l := newGenerationRetirementLifecycle(f.store, time.Now())
		l.now = nil
		before := time.Now()
		if _, err := l.ReleaseIdleCheckouts(context.Background()); err != nil {
			t.Fatal(err)
		}
		if l.idle.scanned.Before(before) {
			t.Fatalf("scan recorded at %v, want time.Now (after %v)", l.idle.scanned, before)
		}
	})
	t.Run("recordCoordinatorStartFailure", func(t *testing.T) {
		l := &CheckoutLifecycle{}
		before := time.Now().Unix()
		l.recordCoordinatorStartFailure(store_sqlite.Checkout{CheckoutID: "c"}, errors.New("refused"))
		if at := l.coordinatorStartFailures["c"].At; at < before {
			t.Fatalf("failure recorded at %d, want time.Now (at or after %d)", at, before)
		}
	})
	t.Run("scheduleFamilyRetryAt and runFamilyRetry", func(t *testing.T) {
		// The reconcile fails (no catalog), so the retry reads the clock to
		// schedule the next one (TestCheckoutLifecycleFamilyRetryTimerFires,
		// on a lifecycle without its clock).
		l := &CheckoutLifecycle{
			logger:        zap.NewNop(),
			familyRetries: map[string]familyRetry{},
			coordinators:  map[string]*CheckoutCoordinator{},
		}
		t.Cleanup(func() { _ = l.Close() })
		initial := time.Now().Unix()
		l.scheduleFamilyRetryAt("family", initial)
		deadline := time.Now().Add(5 * time.Second)
		for {
			l.retryMu.Lock()
			retry, ok := l.familyRetries["family"]
			l.retryMu.Unlock()
			if ok && retry.deadline > initial {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the failed retry scheduled no next retry")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	t.Run("recordCheckout", func(t *testing.T) {
		builderIsolateGit(t)
		store := builderOpenStore(t, "clock-record-checkout")
		repoDir := builderTempDir(t, "clock-record-checkout")
		builderGit(t, repoDir, "init", "--initial-branch=main")
		builderWriteTree(t, repoDir, map[string]string{"go.mod": "module example.com/c\n\ngo 1.22\n"})
		builderGit(t, repoDir, "add", "-A")
		builderGit(t, repoDir, "commit", "-q", "-m", "base")
		l := newGenerationRetirementLifecycle(store, time.Now())
		l.now = nil
		if _, err := l.recordCheckout(context.Background(), builderRepoPrefix, repoDir, store_sqlite.IntentSourceManualConfig, false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("orphanedGenerations", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		l := newGenerationRetirementLifecycle(f.store, time.Now())
		l.now = nil
		_ = l.orphanedGenerations(context.Background(), map[string]struct{}{}, nil)
	})
}
