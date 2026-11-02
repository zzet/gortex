package indexer

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
)

func TestMutationSetPublicationMembersRequireExactLiveServingOwner(t *testing.T) {
	for _, condition := range []string{"exact", "newer-frontier", "newer-pending", "newer-observed", "wrong-ticket-record", "wrong-ticket-path", "terminal", "evicted", "stopping"} {
		t.Run(condition, func(t *testing.T) {
			owner := NewPublicationPhaseRecorder(2, 1)
			defer runtime.KeepAlive(owner)
			record := owner.Begin("checkout", "original", PublicationSourceMCPEdit, time.Now())
			require.True(t, record.bindCanonicalTicket(2, time.Now()))
			ticket := &MutationTicket{Path: "/owned/a.go", Generation: 2, PublicationRecord: record}
			w := &Watcher{pendingGeneration: map[string]uint64{"/owned/a.go": 2}, observedMutationGeneration: map[string]uint64{"/owned/a.go": 2}, mutationWaiters: map[string]map[uint64]mutationWaiter{"/owned/a.go": {2: {publication: record, ticket: ticket}}}}
			frontier := map[string]uint64{"/owned/a.go": 2}
			switch condition {
			case "newer-frontier":
				frontier["/owned/a.go"] = 3
			case "newer-pending":
				w.pendingGeneration["/owned/a.go"] = 3
			case "newer-observed":
				w.observedMutationGeneration["/owned/a.go"] = 3
			case "wrong-ticket-record":
				ticket.PublicationRecord = owner.Begin("checkout", "replacement", PublicationSourceMCPEdit, time.Now())
			case "wrong-ticket-path":
				ticket.Path = "/foreign/a.go"
			case "terminal":
				record.Mark(PublicationTicketFailed)
			case "evicted":
				owner.Begin("checkout", "replacement", PublicationSourceMCPEdit, time.Now())
				owner.Begin("checkout", "evict-original", PublicationSourceMCPEdit, time.Now())
			case "stopping":
				w.stopping = true
			}
			members := w.mutationSetPublicationMembers(frontier)
			if condition != "exact" {
				require.Empty(t, members)
				return
			}
			require.Len(t, members, 1)
			require.Same(t, record, members[0].record)
			require.Equal(t, uint64(2), members[0].generation)
			require.Equal(t, "/owned/a.go", members[0].path)
		})
	}
}

// Selection is not authority: ownership may change while the real executor is
// running, including after a snapshot was taken but before Done is delivered.
func TestMutationSetPublicationRevalidatesMembersAtActualReturn(t *testing.T) {
	for _, change := range []string{"none", "newer-generation", "wrong-path", "different-record", "terminal", "evicted", "failed-file", "failed-batch", "no-result", "stopping"} {
		t.Run(change, func(t *testing.T) {
			owner := NewPublicationPhaseRecorder(2, 1)
			defer runtime.KeepAlive(owner)
			record := owner.Begin("checkout", "original", PublicationSourceMCPEdit, time.Now())
			require.True(t, record.bindCanonicalTicket(2, time.Now()))
			path := "/owned/a.go"
			ticket := &MutationTicket{Path: path, Generation: 2, PublicationRecord: record}
			w := &Watcher{pendingGeneration: map[string]uint64{path: 2}, observedMutationGeneration: map[string]uint64{path: 2}, mutationWaiters: map[string]map[uint64]mutationWaiter{path: {2: {publication: record, ticket: ticket}}}}
			attempt := w.beginMutationSetPublication(map[string]uint64{path: 2}, time.Now(), 0)
			require.NotNil(t, attempt)
			result := &IndexResult{}
			var batchErr error
			switch change {
			case "newer-generation":
				w.pendingGeneration[path] = 3
			case "wrong-path":
				ticket.Path = "/foreign/a.go"
			case "different-record":
				ticket.PublicationRecord = owner.Begin("checkout", "replacement", PublicationSourceMCPEdit, time.Now())
			case "terminal":
				record.Mark(PublicationTicketFailed)
			case "evicted":
				owner.Begin("checkout", "replacement", PublicationSourceMCPEdit, time.Now())
				owner.Begin("checkout", "evict-original", PublicationSourceMCPEdit, time.Now())
			case "failed-file":
				result.FailedFiles = []string{path}
			case "failed-batch":
				batchErr = errWatcherStopped
			case "no-result":
				result = nil
			case "stopping":
				w.stopping = true
			}
			early := record.Snapshot()
			w.finishMutationSetPublication(attempt, result, batchErr, time.Now())
			shared := record.Snapshot().SharedSetExecution()
			if change != "none" {
				require.Nil(t, shared)
				return
			}
			require.NotNil(t, shared)
			require.Equal(t, "shared_mutation_set_executor", shared.Accounting.Scope)
			require.Equal(t, uint64(0), shared.Accounting.AttemptTicket)
			require.Nil(t, shared.Accounting.PhasesMS["patch_parse"], "an unentered parser cannot become a zero or measured lap")
			require.False(t, shared.Accounting.PartitionComplete)
			require.Nil(t, early.SharedSetExecution())
		})
	}
}

// Synthetic scalar/order controls exercise the private acceptance boundary;
// these are not runtime timing evidence or a strict P/B acceptance campaign.
func TestMutationSetBackgroundRequiresOriginalTimerAndFreshMember(t *testing.T) {
	for _, change := range []string{"none", "foreign-timer", "old-timer-frontier", "failed-stop", "predecessor", "retry", "newer-pending", "evicted", "failed-ticket", "stopping", "overlapping-arm", "reversed-return"} {
		t.Run(change, func(t *testing.T) {
			owner := NewPublicationPhaseRecorder(2, 1)
			defer runtime.KeepAlive(owner)
			entry := time.Now()
			record := owner.Begin("checkout", "member", PublicationSourceMCPEdit, entry)
			require.True(t, record.bindCanonicalTicket(2, entry.Add(time.Millisecond)))
			path := "/owned/a.go"
			ticket := &MutationTicket{Path: path, Generation: 2, PublicationRecord: record}
			w := &Watcher{pendingGeneration: map[string]uint64{path: 2}, observedMutationGeneration: map[string]uint64{path: 2}, mutationWaiters: map[string]map[uint64]mutationWaiter{path: {2: {publication: record, ticket: ticket, schedulingEntered: entry, backgroundEligible: true}}}}
			attempt := w.beginMutationSetPublication(map[string]uint64{path: 2}, entry.Add(4*time.Millisecond), 0)
			require.NotNil(t, attempt)
			w.finishMutationSetPublication(attempt, &IndexResult{}, nil, entry.Add(8*time.Millisecond))
			early := record.Snapshot()
			require.NotNil(t, early.SharedSetExecution())
			delete(w.pendingGeneration, path) // existing Done completion has consumed the exact frontier
			timer := &mutationSetTimerObservation{watcher: weak.Make(w), frontier: 2, armStarted: entry.Add(2 * time.Millisecond), armFinished: entry.Add(3 * time.Millisecond)}
			returned := entry.Add(10 * time.Millisecond)
			switch change {
			case "foreign-timer":
				timer.watcher = weak.Make(&Watcher{})
			case "old-timer-frontier":
				timer.frontier = 1
			case "failed-stop":
				timer.invalid.Store(true)
			case "predecessor":
				attempt.members[0].backgroundEligible = false
			case "retry":
				attempt.retry = 1
			case "newer-pending":
				w.pendingGeneration[path] = 3
			case "evicted":
				owner.Begin("checkout", "other", PublicationSourceMCPEdit, entry)
				owner.Begin("checkout", "eviction", PublicationSourceMCPEdit, entry)
			case "failed-ticket":
				record.Mark(PublicationTicketFailed)
			case "stopping":
				w.stopping = true
			case "overlapping-arm":
				timer.armFinished = entry.Add(5 * time.Millisecond)
			case "reversed-return":
				returned = entry.Add(3 * time.Millisecond)
			}
			w.finishMutationSetBackground(attempt, timer, returned)
			shared := record.Snapshot().SharedSetExecution()
			require.NotNil(t, shared, "rejection must not alter prior shared executor evidence")
			require.Nil(t, early.SharedSetExecution().Background, "earlier snapshots must remain unchanged")
			if change != "none" {
				require.Nil(t, shared.Background)
				return
			}
			background := shared.Background
			require.NotNil(t, background)
			require.Equal(t, "original_member_shared_set_background", background.Scope)
			require.True(t, background.NonadditiveAcrossMembers)
			require.True(t, background.CallbackIncludesSharedExecution)
			require.True(t, background.RequestedWallIncludesCallback)
			require.InDelta(t, 10.0, background.RequestedWallMS, 0.000001)
			require.InDelta(t, background.RequestedWallMS, background.SchedulingMS+background.TimerArmMS+background.QuietMS+background.CallbackMS, 0.000001)
			require.Nil(t, record.Snapshot().attemptWindow)
			background.RequestedWallMS = -1
			background.Scope = "mutated-caller-copy"
			fresh := record.Snapshot().SharedSetExecution().Background
			require.Equal(t, "original_member_shared_set_background", fresh.Scope)
			require.InDelta(t, 10.0, fresh.RequestedWallMS, 0.000001)
		})
	}
}

func TestMutationSetTimerObservationRequiresOwnedPublication(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unobserved", true: "owned"}[observed], func(t *testing.T) {
			w := &Watcher{stormExplicit: true, stormPublicationObserved: observed, stormGenerations: map[string]uint64{"/owned/a.go": 2}}
			w.stormMu.Lock()
			w.armStormTimerLocked(time.Hour)
			observation := w.stormTimerObservation
			w.stopStormTimerLocked()
			w.stormMu.Unlock()
			w.stormWork.Wait()
			if !observed {
				require.Nil(t, observation, "unobserved sets must not allocate a timer observer")
				return
			}
			require.NotNil(t, observation)
			require.Same(t, w, observation.watcher.Value())
			require.Equal(t, uint64(2), observation.frontier)
			require.False(t, observation.armStarted.IsZero())
			require.False(t, observation.armFinished.Before(observation.armStarted))
		})
	}
}

// The acceptance boundary compares failed-file identity in the same native
// spelling used by the result producer, including slash spellings on Windows.
// This is a scalar boundary control, not a claim of runtime path reachability.
func TestMutationSetPublicationRejectsFailedPathSpellings(t *testing.T) {
	native := filepath.Join(t.TempDir(), "a.go")
	slash := filepath.ToSlash(native)
	for _, spelling := range []struct {
		name, member, failed string
	}{
		{"native-native", native, native},
		{"native-slash", native, slash},
		{"slash-native", slash, native},
		{"slash-slash", slash, slash},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			owner := NewPublicationPhaseRecorder(2, 1)
			defer runtime.KeepAlive(owner)
			record := owner.Begin("checkout", "failed-spelling", PublicationSourceMCPEdit, time.Now())
			require.True(t, record.bindCanonicalTicket(2, time.Now()))
			ticket := &MutationTicket{Path: spelling.member, Generation: 2, PublicationRecord: record}
			w := &Watcher{
				pendingGeneration:          map[string]uint64{spelling.member: 2},
				observedMutationGeneration: map[string]uint64{spelling.member: 2},
				mutationWaiters:            map[string]map[uint64]mutationWaiter{spelling.member: {2: {publication: record, ticket: ticket}}},
			}
			attempt := w.beginMutationSetPublication(map[string]uint64{spelling.member: 2}, time.Now(), 0)
			require.NotNil(t, attempt)
			w.finishMutationSetPublication(attempt, &IndexResult{FailedFiles: []string{spelling.failed}}, nil, time.Now())
			require.Nil(t, record.Snapshot().SharedSetExecution(), "a failed member cannot acquire successful execution evidence")
		})
	}
}
