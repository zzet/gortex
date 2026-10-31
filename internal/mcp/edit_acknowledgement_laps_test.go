package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestEditAcknowledgementRepeatedDisjointChunksAndUnclassifiedGap(t *testing.T) {
	start := time.Now()
	a := &editAcknowledgement{entered: start}
	for _, chunk := range []struct {
		kind       editAcknowledgementLapKind
		begin, end time.Duration
	}{
		{editAckParseGate, time.Millisecond, 3 * time.Millisecond},
		{editAckDiskWrite, 4 * time.Millisecond, 7 * time.Millisecond},
		{editAckParseGate, 8 * time.Millisecond, 12 * time.Millisecond},
		{editAckTicketAdmission, 13 * time.Millisecond, 18 * time.Millisecond},
	} {
		lap := a.beginLapAt(chunk.kind, start.Add(chunk.begin))
		lap.finishAt(start.Add(chunk.end))
		lap.finishAt(start.Add(50 * time.Millisecond)) // finished token is a no-op
	}
	a.finishActualAt(start.Add(20 * time.Millisecond))
	a.finishReplyAt(start.Add(22*time.Millisecond), "result")
	snapshot := a.snapshot()
	require.True(t, snapshot.LapsDisjoint)
	require.EqualValues(t, 2, snapshot.ParseGate.Started)
	require.EqualValues(t, 2, snapshot.ParseGate.Completed)
	require.Equal(t, 6.0, *snapshot.ParseGate.AckMS)
	require.Equal(t, 3.0, *snapshot.DiskWrite.AckMS)
	require.Equal(t, 5.0, *snapshot.TicketAdmission.AckMS)
	measured := *snapshot.ParseGate.AckMS + *snapshot.DiskWrite.AckMS + *snapshot.TicketAdmission.AckMS
	require.Equal(t, 14.0, measured)
	require.Equal(t, 22.0, *snapshot.ReplyWallMS)
	require.Greater(t, *snapshot.ReplyWallMS-measured, 0.05*(*snapshot.ReplyWallMS),
		"missing middleware/wait/reply work remains unclassified, not assigned to a lap to force the sum")
	*snapshot.ParseGate.AckMS = 1000
	require.Equal(t, 6.0, *a.snapshot().ParseGate.AckMS)
}

func TestEditAcknowledgementLapsRejectOverlapMissingAndLateIntervals(t *testing.T) {
	start := time.Now()
	t.Run("overlap", func(t *testing.T) {
		a := &editAcknowledgement{entered: start}
		parse := a.beginLapAt(editAckParseGate, start.Add(time.Millisecond))
		disk := a.beginLapAt(editAckDiskWrite, start.Add(2*time.Millisecond))
		disk.finishAt(start.Add(3 * time.Millisecond))
		parse.finishAt(start.Add(4 * time.Millisecond))
		a.finishReplyAt(start.Add(5*time.Millisecond), "result")
		got := a.snapshot()
		require.False(t, got.LapsDisjoint)
		require.Equal(t, "invalid_interval", got.ParseGate.State)
		require.Nil(t, got.ParseGate.AckMS)
		require.Nil(t, got.DiskWrite.AckMS)
	})
	t.Run("missing end", func(t *testing.T) {
		a := &editAcknowledgement{entered: start}
		a.beginLapAt(editAckParseGate, start.Add(time.Millisecond))
		a.finishReplyAt(start.Add(5*time.Millisecond), "go_error")
		require.Equal(t, "incomplete", a.snapshot().ParseGate.State)
		require.Nil(t, a.snapshot().ParseGate.ObservedMS)
		require.Nil(t, a.snapshot().ParseGate.AckMS)
		require.Equal(t, "unobserved", a.snapshot().DiskWrite.State)
	})
	t.Run("negative interval", func(t *testing.T) {
		a := &editAcknowledgement{entered: start}
		lap := a.beginLapAt(editAckParseGate, start.Add(3*time.Millisecond))
		lap.finishAt(start.Add(2 * time.Millisecond))
		a.finishReplyAt(start.Add(5*time.Millisecond), "result")
		require.Equal(t, "invalid_interval", a.snapshot().ParseGate.State)
		require.Nil(t, a.snapshot().ParseGate.ObservedMS)
	})
	t.Run("actual execution containment", func(t *testing.T) {
		a := &editAcknowledgement{entered: start}
		lap := a.beginLapAt(editAckDiskWrite, start.Add(time.Millisecond))
		lap.finishAt(start.Add(4 * time.Millisecond))
		a.finishActualAt(start.Add(3 * time.Millisecond))
		a.finishReplyAt(start.Add(5*time.Millisecond), "result")
		require.Equal(t, "outside_execution_window", a.snapshot().DiskWrite.State)
		require.Nil(t, a.snapshot().DiskWrite.ObservedMS)
	})
	t.Run("late actual execution is not clipped into ack", func(t *testing.T) {
		a := &editAcknowledgement{entered: start}
		lap := a.beginLapAt(editAckDiskWrite, start.Add(time.Millisecond))
		a.finishReplyAt(start.Add(2*time.Millisecond), "go_error")
		lap.finishAt(start.Add(4 * time.Millisecond))
		a.finishActualAt(start.Add(5 * time.Millisecond))
		require.Equal(t, "outside_reply_window", a.snapshot().DiskWrite.State)
		require.Equal(t, 3.0, *a.snapshot().DiskWrite.ObservedMS)
		require.Nil(t, a.snapshot().DiskWrite.AckMS, "do not turn three actual milliseconds into one clipped millisecond")
	})
}

func TestEditAcknowledgementRealParseGatePreservesVerdictsAndApplicability(t *testing.T) {
	s := newMutationServer()
	before := []byte("package p\nfunc A() {}\n")
	for _, tt := range []struct {
		name, path, after string
		checked, blocked  bool
	}{
		{"allow", "a.go", "package p\nfunc A() { println(1) }\n", true, false},
		{"refuse", "a.go", "package p\nfunc A( {\n", true, true},
		{"unsupported", "a.unknown-language", "anything", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, ack, _ := s.withEditAcknowledgement(t.Context())
			got := s.checkAcknowledgedParseGate(ctx, tt.path, before, []byte(tt.after))
			require.Equal(t, checkParseGate(tt.path, before, []byte(tt.after)), got, "only observe the existing parser verdict")
			require.Equal(t, tt.checked, got.Checked)
			require.Equal(t, tt.blocked, got.Blocked)
			ack.finishActual()
			ack.finishReply(mcp.NewToolResultText("fixture gate returned"), nil)
			snapshot := ack.snapshot().ParseGate
			require.EqualValues(t, 1, snapshot.Started)
			require.EqualValues(t, 1, snapshot.Completed)
			if tt.checked {
				require.Equal(t, "measured", snapshot.State)
				require.NotNil(t, snapshot.AckMS)
			} else {
				require.Equal(t, "not_applicable", snapshot.State)
				require.Nil(t, snapshot.ObservedMS)
				require.Nil(t, snapshot.AckMS)
			}
		})
	}
}

func TestEditAcknowledgementRealDiskAndCanonicalTicketBoundaries(t *testing.T) {
	for _, scheduleErr := range []error{nil, errors.New("fixture refused enqueue")} {
		s := newMutationServer()
		s.watcher = &canonicalPublicationWatcher{mutationTestWatcher: mutationTestWatcher{scheduleErr: scheduleErr}}
		path := filepath.Join(t.TempDir(), "written.go")
		var record *mutationCommitRecord
		h := s.boundToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var err error
			record, err = s.commitFileMutation(ctx, "write_file", "", "", "written.go", path, []byte("package p\n"), 0o600)
			if err != nil {
				return nil, err
			}
			outcome := s.mutationReindexState(ctx, path)
			record.recordGraph(outcome)
			if outcome.Err != nil {
				return mcp.NewToolResultError(outcome.Err.Error()), nil
			}
			return mcp.NewToolResultText("source and ticket returned"), nil
		})
		result, err := h(canonicalTimingContext(), editOperationRequest("write_file", ""))
		require.NoError(t, err)
		require.Equal(t, scheduleErr != nil, result.IsError)
		bytes, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "package p\n", string(bytes))
		ack := retainedMutationAcknowledgement(record).snapshot()
		require.EqualValues(t, 1, ack.DiskWrite.Completed)
		require.EqualValues(t, 1, ack.TicketAdmission.Completed)
		require.Equal(t, "measured", ack.DiskWrite.State)
		require.Equal(t, "measured", ack.TicketAdmission.State, "failed admission is an observed call, not fake missing work")
		if scheduleErr == nil {
			require.EqualValues(t, 1, ack.ReindexWait.Completed)
			require.Equal(t, "measured", ack.ReindexWait.State, "immediate ticket completion still reaches the actual wait")
		} else {
			require.Equal(t, "unobserved", ack.ReindexWait.State)
		}
		require.Equal(t, "unobserved", ack.ParseGate.State, "a raw commit did not call the gate")
		require.True(t, ack.LapsDisjoint)
	}
}

type acknowledgementWaitWatcher struct {
	mutationTestWatcher
	afterAdmission func()
}

func (w acknowledgementWaitWatcher) EnqueueFileMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	ticket, err := w.mutationTestWatcher.EnqueueFileMutation(ctx, path)
	if w.afterAdmission != nil {
		w.afterAdmission()
	}
	return ticket, err
}

func TestEditAcknowledgementActualWaitTimerAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timer", true: "caller cancellation"}[cancelled], func(t *testing.T) {
			s := newMutationServer()
			s.mutationReindexWait = 5 * time.Millisecond // exercise the existing timer, not a latency assertion
			done := make(chan indexer.MutationResult, 1)
			ctx, cancel := context.WithCancel(canonicalTimingContext())
			defer cancel()
			watcher := acknowledgementWaitWatcher{mutationTestWatcher: mutationTestWatcher{done: done}}
			if cancelled {
				watcher.afterAdmission = cancel
			}
			s.watcher = watcher
			ctx, ack, _ := s.withEditAcknowledgement(ctx)
			path := filepath.Join(t.TempDir(), "wait.go")
			commitTimingFixture(t, s, ctx, path, "package p\n")
			// Release the sole existing ticket consumer on every assertion path.
			t.Cleanup(func() {
				close(done)
				s.mutationReceipts.Range(func(_, value any) bool {
					if receipt, ok := value.(*mutationReceipt); ok {
						select {
						case <-receipt.done:
						case <-time.After(5 * time.Second):
							t.Error("owned ticket consumer did not join")
						}
					}
					return true
				})
			})
			outcome := s.mutationReindexState(ctx, path)
			require.True(t, outcome.Pending)
			require.NotEmpty(t, outcome.Receipt)
			ack.finishActual()
			ack.finishReply(mcp.NewToolResultText("pending"), nil)
			got := ack.snapshot()
			require.Equal(t, "measured", got.ReindexWait.State)
			require.EqualValues(t, 1, got.ReindexWait.Started)
			require.EqualValues(t, 1, got.ReindexWait.Completed)
			require.NotNil(t, got.ReindexWait.AckMS)
			require.True(t, got.LapsDisjoint)
		})
	}
}

func TestEditAcknowledgementUnobservedLapAddsNoAllocationOrIdentity(t *testing.T) {
	s, ctx := &Server{}, t.Context()
	before := editAcknowledgementSequence.Load()
	require.Zero(t, testing.AllocsPerRun(100, func() {
		lap := s.beginAcknowledgementLap(ctx, editAckDiskWrite)
		lap.finish()
	}))
	require.Equal(t, before, editAcknowledgementSequence.Load())
}

func TestEditAcknowledgementActualDiskFailureDoesNotInventAdmission(t *testing.T) {
	s := newMutationServer()
	path := t.TempDir() // an atomic file rename cannot replace this directory
	var record *mutationCommitRecord
	h := s.boundToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var err error
		record, err = s.commitFileMutation(ctx, "write_file", "", "", "owned-directory", path, []byte("after"), 0o600)
		return nil, err
	})
	result, err := h(canonicalTimingContext(), editOperationRequest("write_file", ""))
	require.Error(t, err)
	require.Nil(t, result)
	ack := retainedMutationAcknowledgement(record).snapshot()
	require.EqualValues(t, 1, ack.DiskWrite.Completed)
	require.Equal(t, "measured", ack.DiskWrite.State, "observe the real failed file call's elapsed work")
	require.Equal(t, "unobserved", ack.TicketAdmission.State)
	require.Equal(t, "go_error", ack.ReplyOutcome)
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	require.True(t, info.IsDir(), "failed source replacement must preserve its existing target")
}
