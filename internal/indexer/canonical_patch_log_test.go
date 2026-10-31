package indexer

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestCanonicalPatchLogOwnsItsSnapshotBeforeWinnerPublication(t *testing.T) {
	path, idx, w := pointPublicationFixture(t)
	core, logs := observer.New(zap.InfoLevel)
	w.logger = zap.New(core)
	var local *PublicationStamps
	w.pointReindexRaw = func(path string, s *PublicationStamps) (*IndexResult, error) {
		local = s
		return idx.incrementalPointWatcherPathObserved(idx.rootPath, path, s)
	}
	record := newPointPublication(t, "log")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	entries := logs.FilterMessage("graph patch complete").All()
	require.Len(t, entries, 1, "instrumentation must extend the existing line, not emit another patch line")
	fields := entries[0].ContextMap()
	require.Equal(t, "physical_attempt_before_receipt_validation", fields["patch_lap_scope"])
	require.Equal(t, ticket.Generation, fields["attempt_ticket"])
	require.Eventually(t, func() bool { return record.Snapshot().attemptWindow != nil }, 2*time.Second, time.Millisecond)
	status := record.Snapshot().EditTimings()
	require.Equal(t, "measured", status.States["debounce"])
	require.Equal(t, status.States["debounce"], fields["attempt_debounce_state"])
	require.Equal(t, *status.Milliseconds["debounce"], fields["attempt_debounce_ms"])
	require.Equal(t, status.States["lane_wait"], fields["attempt_lane_wait_state"])
	require.NotNil(t, status.Milliseconds["lane_wait"])
	require.Equal(t, *status.Milliseconds["lane_wait"], fields["attempt_lane_wait_ms"])
	for _, name := range canonicalAdmissionNames {
		require.Equal(t, status.States[name], fields["attempt_"+name+"_state"], name)
		if ms := status.Milliseconds[name]; ms != nil {
			require.Equal(t, *ms, fields["attempt_"+name+"_ms"], name)
		}
	}
	for _, name := range canonicalPatchLapNames {
		require.Equal(t, status.States[name], fields["attempt_"+name+"_state"], name)
		if ms := status.Milliseconds[name]; ms != nil {
			require.Equal(t, *ms, fields["attempt_"+name+"_ms"], name)
		}
	}
	// Actual unrelated graph work and a changed local collector must affect
	// neither the already-emitted log nor the receipt's accepted scalar copy.
	other := filepath.Join(filepath.Dir(path), "other.go")
	writeTestFile(t, other, "package p\nfunc Unrelated() {}\n")
	require.NoError(t, idx.IndexFile(other))
	late := local.beginPatchLap(patchParse)
	late.end()
	require.Equal(t, fields, entries[0].ContextMap(), "encoders may be evaluated later or more than once")
	require.Equal(t, status, record.Snapshot().EditTimings())
}

func TestCanonicalPatchNativeLogKeepsOriginalFieldsOnly(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	core, logs := observer.New(zap.InfoLevel)
	w.logger = zap.New(core)
	require.NoError(t, w.patchGraphAfterReceiptCheck(path, ChangeModified))
	entries := logs.FilterMessage("graph patch complete").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Contains(t, fields, "incident_edges")
	require.Contains(t, fields, "ms")
	for key := range fields {
		require.False(t, strings.HasPrefix(key, "attempt_"), key)
	}
	require.NotContains(t, fields, "patch_lap_scope")
	var s *PublicationStamps
	var observed bool
	allocs := testing.AllocsPerRun(100, func() { _, observed = s.snapshotPatchLog() })
	require.False(t, observed)
	require.Zero(t, allocs, "the nil-observer log branch must not allocate")
}
