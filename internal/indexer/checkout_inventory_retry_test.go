package indexer

import (
	"testing"
	"time"

	"github.com/zzet/gortex/internal/reconcile"
	"go.uber.org/zap"
)

func TestInventoryUnknownRetryBackoffAndRestartReset(t *testing.T) {
	now := time.Now().Add(time.Hour)
	create := func() *CheckoutLifecycle {
		lc := &CheckoutLifecycle{now: func() time.Time { return now }, logger: zap.NewNop(), familyRetries: map[string]familyRetry{}, coordinators: map[string]*CheckoutCoordinator{}}
		t.Cleanup(func() {
			if err := lc.Close(); err != nil {
				t.Error(err)
			}
		})
		return lc
	}
	lc := create()
	report := reconcile.FamilyReport{FamilyID: "family"}
	for _, delay := range []time.Duration{30 * time.Second, 2 * time.Minute, 8 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		lc.scheduleFamilyRetry(report)
		lc.retryMu.Lock()
		retry, ok := lc.familyRetries[report.FamilyID]
		lc.retryMu.Unlock()
		if !ok || retry.deadline != now.Add(delay).Unix() {
			t.Fatalf("unknown retry=%+v armed=%v want=%s", retry, ok, now.Add(delay))
		}
		lc.scheduleFamilyRetry(report)
		lc.retryMu.Lock()
		again := lc.familyRetries[report.FamilyID]
		lc.retryMu.Unlock()
		if again.deadline != retry.deadline {
			t.Fatal("event inside backoff advanced its deadline")
		}
		now = now.Add(delay)
	}
	healthy := report
	healthy.InventoryUsable = true
	lc.scheduleFamilyRetry(healthy)
	lc.scheduleFamilyRetry(report)
	lc.retryMu.Lock()
	reset := lc.familyRetries[report.FamilyID]
	lc.retryMu.Unlock()
	if reset.deadline != now.Add(30*time.Second).Unix() {
		t.Fatal("healthy inventory did not reset backoff")
	}
	restarted := create()
	restarted.scheduleFamilyRetry(report)
	restarted.retryMu.Lock()
	fresh := restarted.familyRetries[report.FamilyID]
	restarted.retryMu.Unlock()
	if fresh.deadline != now.Add(30*time.Second).Unix() {
		t.Fatal("runtime retry backoff unexpectedly survived restart")
	}
}
