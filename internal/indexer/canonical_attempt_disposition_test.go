package indexer

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func controlledDuplicateAttempt(t *testing.T) canonicalAttemptWindow {
	t.Helper()
	origin := time.Now()
	at := func(n int) time.Time { return origin.Add(time.Duration(n) * time.Millisecond) }
	s := &PublicationStamps{patchLaps: &canonicalPatchLaps{}}
	s.startAttemptControl(at(1))
	cohort := s.beginCanonicalAdmissionAt(canonicalCohortAdmission, at(2))
	cohort.endAt(at(3))
	s.recordCanonicalLaneWaitAt(false, at(4))
	s.recordCanonicalLaneWaitAt(true, at(5))
	s.switchAttemptControlAt(patchInspection, at(6))
	s.recordNativeMtimeDuplicate()
	s.switchAttemptControlAt(patchAttemptFinalize, at(8))
	s.mu.Lock()
	s.patchLaps.finishAttemptControlLocked(at(10))
	evidence := s.freezeAttemptEvidence(origin)
	s.mu.Unlock()
	return canonicalAttemptWindow{startNS: 1e6, endNS: 10e6, ticket: 1, accounting: evidence}
}

func TestCanonicalAttemptObservedDuplicateKeepsUnexecutedLapsUnavailable(t *testing.T) {
	w := controlledDuplicateAttempt(t)
	p := w.projectAccounting()
	require.True(t, p.PartitionComplete)
	require.Equal(t, "native_mtime_duplicate", p.ExecutionDisposition)
	require.Equal(t, "not_established", p.BackgroundCoverage)
	require.Equal(t, 9.0, *p.SubtotalMS)
	require.Zero(t, *p.UnattributedMS)
	for _, name := range append(canonicalPatchLapNames[:], "patch_analysis", "executor_control") {
		require.Equal(t, "not_applicable", p.PhaseStates[name], name)
		require.Nil(t, p.PhasesMS[name], "not executed is never a measured zero")
	}
	w.accounting.nativeMtimeDuplicate = false
	require.Equal(t, "missing_laps", w.projectAccounting().SumState, "unknown execution must retain the existing refusal")
	require.True(t, p.PartitionComplete, "a later projection cannot mutate an earlier snapshot")
}

func TestCanonicalAttemptDuplicateRejectsContradictoryExecution(t *testing.T) {
	for _, name := range []string{"executor_start", "executor_end", "derived_start", "derived_end", "deep", "executor_control", "missing_control"} {
		t.Run(name, func(t *testing.T) {
			w := controlledDuplicateAttempt(t)
			want := "invalid_order"
			switch name {
			case "executor_start":
				w.accounting.hasExecutorStart = true
			case "executor_end":
				w.accounting.hasExecutorEnd = true
			case "derived_start":
				w.accounting.hasDerivedStart = true
			case "derived_end":
				w.accounting.hasDerivedEnd = true
			case "deep":
				w.accounting.laps[patchParse].begun = 1
			case "executor_control":
				w.accounting.laps[patchExecutorControl].done = 1
			case "missing_control":
				w.accounting.laps[patchInspection] = canonicalLapEvidence{}
				want = "missing_laps"
			}
			p := w.projectAccounting()
			require.False(t, p.PartitionComplete)
			require.Equal(t, want, p.SumState)
		})
	}
}

func TestWatcherNativeDuplicateDispositionRequiresActualEarlyReturn(t *testing.T) {
	for _, name := range []string{"native_duplicate", "receipt_checked", "changed_bytes"} {
		t.Run(name, func(t *testing.T) {
			root, idx, w := inertTestWatcher(t, "main.go", "package p\nfunc Stable() {}\n")
			w.degradedNoFsnotify = true // this direct-call fixture never starts the native loop
			t.Cleanup(func() { require.NoError(t, w.Stop()) })
			path := filepath.Join(root, "main.go")
			if name == "changed_bytes" {
				bumpMtime(t, path, "package p\nfunc Changed() {}\n")
			}
			called := false
			stop := errors.New("observed executor entry")
			w.pointReindexRaw = func(string, *PublicationStamps) (*IndexResult, error) {
				called = true
				return nil, stop
			}
			ctx := WithPublicationStamps(t.Context())
			s := PublicationStampsFrom(ctx)
			s.patchLaps = &canonicalPatchLaps{}
			err := w.patchGraphWithReceiptStateRawModern(ctx, idx, path, ChangeModified, 0, name == "receipt_checked", nil, nil)
			if name == "native_duplicate" {
				require.NoError(t, err)
				require.False(t, called)
				require.True(t, s.patchLaps.nativeMtimeDuplicate)
			} else {
				require.ErrorIs(t, err, stop)
				require.True(t, called)
				require.False(t, s.patchLaps.nativeMtimeDuplicate)
			}
		})
	}
}

func TestCanonicalDuplicateDispositionNilAndUnobservedDoesNotAllocate(t *testing.T) {
	for _, s := range []*PublicationStamps{nil, {}} {
		require.Zero(t, testing.AllocsPerRun(100, func() { s.recordNativeMtimeDuplicate() }))
	}
}
