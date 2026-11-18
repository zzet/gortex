package store_sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SQLite finishes the real reset before its observer waits out the deadline.
// The subsequent driver call returns the expired context with no result; the
// reset itself must recognise the physical state for every reclaim caller.
func TestWALResetCompletedAfterDeadlineIsRecognised(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		t.Run(map[bool]string{false: "truncate", true: "in_place"}[inPlace], func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "0")
			s, _ := finalBackfillFixture(t)
			previous := walShrinkInPlaceBytes
			if inPlace {
				walShrinkInPlaceBytes = 1
			}
			t.Cleanup(func() { walShrinkInPlaceBytes = previous; walResetCheckpointConnObserver = nil })
			before := readWALWriteMark(s.dbPath)
			require.True(t, before.Valid)
			bytesBefore := walFileSize(s.dbPath + "-wal")
			require.Positive(t, bytesBefore)
			observed := false
			walResetCheckpointConnObserver = func(ctx context.Context, conn *sql.Conn) {
				observed = true
				if inPlace {
					var result walCheckpointResult
					require.NoError(t, s.restartWALInPlaceOn(context.Background(), conn, &result))
				} else {
					_, err := checkpointWALOnceOn(context.Background(), conn, "TRUNCATE")
					require.NoError(t, err)
				}
				<-ctx.Done()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			var err error
			func() {
				s.writeMu.Lock()
				defer s.writeMu.Unlock()
				_, err = s.resetWALForReclaim(ctx)
			}()
			require.True(t, observed, "the test must reach the real reset connection")
			require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
			require.NoError(t, err, "a completed reset must not be counted as deferred")
			require.Equal(t, inPlace, s.walShrinkPending.Load())
			if inPlace {
				after := readWALWriteMark(s.dbPath)
				require.NotEqual(t, before.Salt, after.Salt)
				require.LessOrEqual(t, after.MxFrame, uint32(walShrinkMaxRestartFrames))
				require.Equal(t, bytesBefore, walFileSize(s.dbPath+"-wal"))
				require.EqualValues(t, 1, s.WALReclaimStats().ShrinkInPlaceResets)
				require.EqualValues(t, -1, writerJournalLimit(t, s))
			} else {
				require.Zero(t, walFileSize(s.dbPath+"-wal"))
				require.Zero(t, s.WALReclaimStats().ShrinkInPlaceResets)
				require.EqualValues(t, walShrinkKeepBytes, writerJournalLimit(t, s))
			}
		})
	}
}

func TestWALInPlaceResetRequiresChangedSaltAndBoundedFrames(t *testing.T) {
	before := WALWriteMark{Valid: true, MxFrame: 2, Salt: [8]byte{1}}
	fresh := WALWriteMark{Valid: true, MxFrame: 1, Salt: [8]byte{2}}
	cases := []struct {
		name          string
		before, after WALWriteMark
		completed     bool
	}{
		{"new incarnation", before, fresh, true},
		{"unchanged small log", before, before, false},
		{"unreadable before", WALWriteMark{}, fresh, false},
		{"unreadable after", before, WALWriteMark{}, false},
		{"too many frames", before, WALWriteMark{Valid: true, MxFrame: walShrinkMaxRestartFrames + 1, Salt: fresh.Salt}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.completed, walInPlaceResetCompleted(tc.before, tc.after))
		})
	}
}
