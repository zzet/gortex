package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer/source"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The real source refresh mismatch must still publish the final file through
// the original point ticket. A combined fallback is successful work, but its
// duration cannot be relabeled as parse or hidden in executor control.
func TestCanonicalSuccessfulFallbackHasCompleteAcceptedPointAccounting(t *testing.T) {
	path, idx, w := pointPublicationFixture(t)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	fs, err := source.NewFilesystemSource(filepath.Dir(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fs.Close()) })
	changing := &refreshMismatchSource{ContentSource: fs}
	idx.SetContentSource(changing)
	t.Cleanup(func() { idx.SetContentSource(nil) })
	core, logs := observer.New(zap.InfoLevel)
	idx.logger = zap.New(core)
	record := newPointPublication(t, "successful-fallback-accounting")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	result := awaitMutationRetryResult(t, ticket)
	require.NoError(t, result.Err)
	require.Equal(t, ticket.Generation, result.AppliedGeneration, "the accepted physical attempt must serve this exact original ticket")
	require.NotNil(t, nodeByName(t, idx, "edit.go", "After"))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "measurement must not rewrite the physical source")
	fallbacks := logs.FilterMessage("indexer: reconcile phase complete").FilterField(zap.String("phase", "chunk_fallback_parse_apply")).All()
	require.Len(t, fallbacks, 1, "the actual combined fallback, not an ordinary prepared path, must complete")
	require.EqualValues(t, 1, fallbacks[0].ContextMap()["files"])
	require.EqualValues(t, 0, fallbacks[0].ContextMap()["failed_files"])
	require.GreaterOrEqual(t, changing.reads.Load(), int32(3))
	// Receipt completion can precede the immutable late accepted-attempt
	// snapshot. Wait for that actual return, never synthesize a window here.
	require.Eventually(t, func() bool { return record.Snapshot().attemptWindow != nil }, 5*time.Second, time.Millisecond)
	snapshot := record.Snapshot()
	require.True(t, snapshot.Terminal)
	require.Equal(t, ticket.Generation, snapshot.Ticket)
	require.Nil(t, snapshot.SharedSetExecution(), "this witness is one real point executor")
	window := snapshot.EditTimings().Window
	require.NotNil(t, window.PatchWallMS)
	p := window.PatchAccounting
	require.NotNil(t, p)
	require.Equal(t, "accepted_winning_physical_attempt", p.Scope)
	require.Equal(t, ticket.Generation, p.AttemptTicket)
	require.Zero(t, p.AttemptRetry)
	require.True(t, p.PartitionComplete, "successful fallback needs real complete disjoint P; raw non-null laps alone are insufficient")
	for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "attempt_setup", "patch_inspection", "executor_control", "attempt_finalize", "cohort_admission", "lane_wait"} {
		require.Equal(t, "measured", p.PhaseStates[name], name)
		require.NotNil(t, p.PhasesMS[name], name)
		require.GreaterOrEqual(t, *p.PhasesMS[name], float64(0), name)
	}
	require.NotNil(t, p.SubtotalMS)
	require.NotNil(t, p.UnattributedMS)
	require.GreaterOrEqual(t, *window.PatchWallMS, *p.SubtotalMS)
	require.LessOrEqual(t, *p.UnattributedMS, *window.PatchWallMS/20, "strict P residual is checked against its own wall, never ACK/background")
}
