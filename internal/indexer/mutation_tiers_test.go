package indexer

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReceiptTierTimestampsAreOrdered(t *testing.T) {
	dir, idx, watcher := inertTestWatcher(t, "main.go", "package main\nfunc Before() {}\n")
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\nfunc After() {}\n")
	derived, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	idx.incrementalCatchupHook = func(kind string, _ []string) {
		if kind == "derived" {
			close(derived)
			<-release
		}
	}
	ticket, err := watcher.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	select {
	case <-derived:
	case <-time.After(2 * time.Second):
		t.Fatal("the real patch did not reach its derived pass")
	}
	tiers := ticket.TierState()
	require.False(t, tiers.AckAt.IsZero(), "admission must stamp the live ticket")
	require.True(t, tiers.AckAt.Before(tiers.FileGraphAt))
	require.False(t, tiers.RelationsAt.Before(tiers.FileGraphAt))
	require.True(t, tiers.DerivedAt.IsZero(), "relations must publish before blocked derived work")
}

func TestNativeOnlyStormHasFreshnessEvidence(t *testing.T) {
	dir, watcher := toolAdmissionFixture(t)
	path := filepath.Join(dir, "main.go")
	watcher.config.StormQuietPeriodMs = int(time.Hour / time.Millisecond)
	writeTestFile(t, path, "package main\nfunc After() {}\n")
	watcher.recordInStormEvent(path, ChangeModified, true)
	state, found := watcher.TierPaths()[path]
	require.True(t, found, "a native-only storm edit must be visible before its drain")
	require.False(t, state.AckAt.IsZero())
	require.True(t, state.RelationsAt.IsZero())
	watcher.completeStormMutationWaiters(map[string]uint64{path: state.Generation}, nil, errors.New("patch failed"))
	require.True(t, watcher.TierState(path).Failed, "the native path must retain its terminal failure")

	repair, err := watcher.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	select {
	case result := <-repair.Done:
		require.NoError(t, result.Err)
	case <-time.After(2 * time.Second):
		t.Fatal("repair did not finish")
	}
	require.NotContains(t, watcher.TierPaths(), path, "successful newer content repairs the failed path")
}

func TestExpiredReceiptCannotCertifyUnrepairedPath(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "partial"}[partial], func(t *testing.T) {
			dir, _, watcher := inertTestWatcher(t, "main.go", "package main\nfunc Before() {}\n")
			path := filepath.Join(dir, "main.go")
			watcher.mu.Lock()
			watcher.failedPaths = map[string]MutationTierState{path: {
				Generation: 7, Failed: !partial, Partial: partial, failedAt: time.Now().Add(-11 * time.Minute),
			}}
			watcher.mu.Unlock()
			state := watcher.TierState(path)
			require.True(t, state.Failed || state.Partial, "receipt expiry is not content publication")
			require.Contains(t, watcher.TierPaths(), path)
		})
	}
}

func TestIdentityPatchPublishesTicketTiers(t *testing.T) {
	dir, _, watcher := inertTestWatcher(t, "main.go", "package main\nfunc Before() {}\n")
	ticket, err := watcher.EnqueueToolMutation(t.Context(), filepath.Join(dir, "main.go"))
	require.NoError(t, err)
	select {
	case result := <-ticket.Done:
		require.NoError(t, result.Err)
	case <-time.After(2 * time.Second):
		t.Fatal("unchanged mutation did not finish")
	}
	tiers := ticket.TierState()
	require.False(t, tiers.DerivedAt.IsZero(), "a successful identity no-op still certifies the admitted bytes")
	require.False(t, tiers.Failed)
}

func TestExplicitTierRepairIsBoundToFailedGeneration(t *testing.T) {
	for _, scenario := range []string{"same_failure", "newer_failure", "partial", "newer_pending"} {
		t.Run(scenario, func(t *testing.T) {
			dir, _, watcher := inertTestWatcher(t, "main.go", "package main\nfunc Main() {}\n")
			path := filepath.Join(dir, "main.go")
			failure := MutationTierState{Generation: 7, Failed: true, failedAt: time.Now()}
			if scenario == "partial" {
				failure.Failed, failure.Partial = false, true
			}
			watcher.failedPaths = map[string]MutationTierState{path: failure}
			repair := watcher.CaptureTierRepair(path)
			if scenario == "newer_failure" {
				failure.Generation++
				watcher.failedPaths[path] = failure
			}
			if scenario == "newer_pending" {
				ticket := newMutationTierTicket(path, 8, nil)
				watcher.mutationWaiters[path] = map[uint64]mutationWaiter{8: {ticket: ticket}}
			}
			if repair != nil {
				repair()
			}
			switch scenario {
			case "same_failure":
				require.NotContains(t, watcher.TierPaths(), path, "an explicit successful reparse repairs the captured failure")
			case "newer_failure":
				require.Equal(t, failure, watcher.TierState(path), "the pass did not observe this later failure")
			case "partial":
				require.True(t, watcher.TierState(path).Partial, "a file parse alone cannot certify bounded derived fanout")
			case "newer_pending":
				require.Equal(t, uint64(8), watcher.TierState(path).Generation)
				require.NotContains(t, watcher.failedPaths, path)
			}
		})
	}
}

func TestStartupRelationsRemainPendingAfterCancelledBatchReset(t *testing.T) {
	mi := &MultiIndexer{}
	mi.BeginParallelBatch()
	require.True(t, mi.RelationsPending())
	mi.ResetBatch() // also used when warmup fails or is cancelled
	require.True(t, mi.RelationsPending(), "an aborted batch cannot certify startup relations")
	mi.CompleteStartupRelations()
	require.False(t, mi.RelationsPending())
}
