package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"time"
)

// Incremental WAL shrink.
//
// A TRUNCATE checkpoint resets the log and then ftruncate()s the -wal to zero
// bytes while it holds SQLite's WAL write lock (and, in the reclaim's writer
// step, the store's write gate). An ftruncate cannot be interrupted, and its
// cost is the extents it frees: the live reclaim's TRUNCATE of a 57.6 GB log
// held the writer 37.3 s, far past the reclaim's 2 s cap. journal_size_limit
// has the same shape one step later: the first commit after a log restart
// truncates the file to the limit in one call, on whichever writer commits.
//
// A big log is therefore reset in place and shrunk in slices:
//
//  1. reset (in the reclaim's writer step, the writer held): the main writer's
//     journal_size_limit is lifted (-1) so no later commit truncates in one
//     call; a RESTART checkpoint on the reclaim's own connection (also at
//     journal_size_limit -1) waits, like TRUNCATE, for every reader to leave
//     WAL read marks 1..n; then one no-op write on that connection
//     (user_version rewritten to its own value, one page) restarts the log at
//     its first frame. The file keeps its size; everything past the new
//     frames is dead.
//  2. shrink (after the attempt, writer released between slices): each slice
//     takes the write gate and SQLite's WAL write lock (BEGIN IMMEDIATE on the
//     reclaim's connection, so no connection can append a frame meanwhile),
//     reads mxFrame from the wal-index, and truncates the file by at most one
//     slice, never below the end of the last frame nor below
//     journal_size_limit. The slice adapts so one ftruncate takes about
//     walShrinkSliceHold. Once the file is back at journal_size_limit the
//     writer's limit is restored.
//
// Logs up to walShrinkInPlaceBytes keep the plain TRUNCATE: their ftruncate is
// short, and the step-2 writer-free reset (logs under twice the reclaim
// threshold) keeps working exactly as before.

const (
	// walShrinkKeepBytes is the size the shrink stops at: journal_size_limit.
	walShrinkKeepBytes int64 = 64 << 20
	// walShrinkMaxRestartFrames: after the in-place reset's marker write the
	// log holds the marker's frames only. More means the log did not restart.
	walShrinkMaxRestartFrames       = 16
	walShrinkMinSlice         int64 = 16 << 20
	walShrinkMaxSlice         int64 = 4 << 30
	walShrinkFirstSlice       int64 = 256 << 20
	// walShrinkGap separates two slices so a queued write takes the writer.
	walShrinkGap = 20 * time.Millisecond
)

// Vars only so the in-package cases can scale them; production never assigns.
var (
	// walShrinkInPlaceBytes is the -wal size from which the reclaim resets in
	// place and shrinks in slices instead of a TRUNCATE checkpoint.
	walShrinkInPlaceBytes int64 = 512 << 20
	// walShrinkSliceHold is the per-slice ftruncate time the slice adapts to.
	walShrinkSliceHold = 250 * time.Millisecond
	// walShrinkSliceCap caps a slice regardless of the measured rate (tests
	// set it to exercise many slices on a small file).
	walShrinkSliceCap = walShrinkMaxSlice
	// walShrinkTruncate is the file truncation (a seam for the cases that
	// record every call).
	walShrinkTruncate = os.Truncate
)

var errWALResetNotRestarted = errors.New("store_sqlite: wal in-place reset: the log did not restart")

// resetWALForReclaim is the reclaim's reset: a TRUNCATE checkpoint for a small
// log, the in-place reset (and a pending shrink) for a big one. The caller
// holds the write gate and has backfilled the log.
// The reset of the log runs on the writer connection (the caller holds
// writeMu), never on the reclaim's checkpoint connection: a connection
// discards its page cache when another connection changes the log's header,
// so a reset from the checkpoint connection left the writer's cache cold for
// the next write (thousands of page reads per edit). The copies stay on the
// checkpoint connection; a PASSIVE backfill does not change the header.
func (s *Store) resetWALForReclaim(ctx context.Context) (walCheckpointResult, error) {
	walPath := s.dbPath + "-wal"
	if walFileSize(walPath) < walShrinkInPlaceBytes {
		var result walCheckpointResult
		err := s.withWriterResetConn(ctx, func(conn *sql.Conn) error {
			var err error
			result, err = checkpointWALOnceOn(ctx, conn, "TRUNCATE")
			return err
		})
		return result, err
	}
	return s.resetWALInPlaceLocked(ctx)
}

// walResetBusyMillis bounds how long a reset on the writer connection waits on
// SQLite locks (readers holding the log): the writer connection's own busy
// timeout is 5 s, which a reset under the write gate must never take.
var walResetBusyMillis = sqliteCheckpointBusyTimeoutMillis

// withWriterResetConn runs fn on the writer connection with its busy timeout
// lowered to walResetBusyMillis (or what is left of ctx's deadline), restored
// afterwards. The caller holds
// writeMu. A store without a separate writer pool (in-memory) or inside a
// bulk window uses the active write connection as it is.
func (s *Store) withWriterResetConn(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return err
	}
	defer release()
	if s.bulkConn == nil {
		busy := int64(walResetBusyMillis)
		if deadline, ok := ctx.Deadline(); ok {
			if left := time.Until(deadline).Milliseconds(); left < busy {
				busy = max(left, 0)
			}
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, busy)); err != nil {
			return err
		}
		defer func() {
			_, _ = conn.ExecContext(context.Background(), fmt.Sprintf(`PRAGMA busy_timeout = %d`, sqliteBusyTimeoutMillis))
		}()
	}
	return fn(conn)
}

// resetWALInPlaceLocked restarts the log at its first frame without shrinking
// the file (see the file comment). The caller holds writeMu.
func (s *Store) resetWALInPlaceLocked(ctx context.Context) (result walCheckpointResult, err error) {
	if err := s.setWriterJournalLimitLocked(ctx, -1); err != nil {
		return result, fmt.Errorf("wal in-place reset: lift the writer's journal_size_limit: %w", err)
	}
	defer func() {
		// A reset that did not happen leaves nothing to shrink: give the
		// writer its limit back (unless an earlier reset's shrink is still
		// pending and will restore it).
		if err != nil && !s.walShrinkPending.Load() {
			_ = s.setWriterJournalLimitLocked(context.Background(), walShrinkKeepBytes)
		}
	}()
	err = s.withWriterResetConn(ctx, func(conn *sql.Conn) error {
		return s.restartWALInPlaceOn(ctx, conn, &result)
	})
	if err != nil {
		return result, err
	}
	snap, ok := readWALIndexSnapshot(s.dbPath)
	if !ok || snap.MxFrame > walShrinkMaxRestartFrames {
		return result, fmt.Errorf("%w: mx_frame=%d", errWALResetNotRestarted, snap.MxFrame)
	}
	s.walShrinkPending.Store(true)
	s.walReclaim.update(func(st *WALReclaimStats) { st.ShrinkInPlaceResets++ })
	return result, nil
}

// restartWALInPlaceOn is the in-place reset's statements on one connection: a
// RESTART checkpoint, then one committed write so the log starts over at its
// first frame.
func (s *Store) restartWALInPlaceOn(ctx context.Context, conn *sql.Conn, result *walCheckpointResult) (err error) {
	*result, err = checkpointWALOnceOn(ctx, conn, "RESTART")
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var version int64
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// setWriterJournalLimitLocked sets journal_size_limit on the writer
// connection. The caller holds writeMu.
func (s *Store) setWriterJournalLimitLocked(ctx context.Context, limit int64) error {
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return err
	}
	defer release()
	_, err = conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA journal_size_limit = %d`, limit))
	return err
}

// walShrinkSlice is one slice's outcome.
type walShrinkSlice struct {
	before, after int64
	hold          time.Duration // writer hold, the ftruncate included
	truncate      time.Duration
	done          bool
}

// shrinkWALSliceOnce truncates the -wal by at most slice bytes under the write
// gate and SQLite's WAL write lock. done reports a file back at its floor.
func (s *Store) shrinkWALSliceOnce(ctx context.Context, ckptDB *sql.DB, slice int64) (out walShrinkSlice, err error) {
	walPath := s.dbPath + "-wal"
	wctx, cancel := context.WithTimeout(ctx, walReclaimWriterWait)
	err = s.writeMu.LockContext(wctx)
	cancel()
	if err != nil {
		return out, fmt.Errorf("%w: wal shrink: writer gate: %w", ErrMaintenanceBusy, err)
	}
	held := time.Now()
	defer func() {
		out.hold = time.Since(held)
		s.writeMu.Unlock()
	}()
	conn, err := ckptDB.Conn(ctx)
	if err != nil {
		return out, err
	}
	defer conn.Close()
	// BEGIN IMMEDIATE takes SQLite's WAL write lock: no connection of any
	// pool can append a frame past the end read below until the ROLLBACK.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return out, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	var pageSize int64
	if err := conn.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return out, err
	}
	snap, ok := readWALIndexSnapshot(s.dbPath)
	if !ok {
		return out, fmt.Errorf("wal shrink: wal-index unreadable")
	}
	used := int64(32) + int64(snap.MxFrame)*(24+pageSize)
	floor := max(used, walShrinkKeepBytes)
	out.before = walFileSize(walPath)
	out.after = out.before
	if out.before <= floor {
		out.done = true
		return out, nil
	}
	target := max(floor, out.before-slice)
	start := time.Now()
	if err := walShrinkTruncate(walPath, target); err != nil {
		return out, err
	}
	out.truncate = time.Since(start)
	out.after = target
	out.done = target <= floor
	return out, nil
}

// walShrinkNeeded reports a -wal file larger than its live frames need by
// more than the smallest slice: a log reset in place and not yet shrunk (or
// one a commit restarted while the writer's journal_size_limit was lifted).
func (s *Store) walShrinkNeeded() bool {
	if s.walShrinkPending.Load() {
		return true
	}
	snap, ok := readWALIndexSnapshot(s.dbPath)
	if !ok {
		return false
	}
	used := int64(32) + int64(snap.MxFrame)*(24+4096)
	return snap.WALBytes > max(used, walShrinkKeepBytes)+walShrinkMinSlice && snap.WALBytes >= walShrinkInPlaceBytes
}

// shrinkWAL shrinks the -wal back to journal_size_limit in slices, releasing
// the writer between them and waiting while a write wants it, then restores
// the writer's journal_size_limit. It stops early on ctx or a slice error
// (the next poll resumes it).
func (s *Store) shrinkWAL(ctx context.Context, ckptDB *sql.DB) (slices int, shrunk int64, err error) {
	walPath := s.dbPath + "-wal"
	startBytes := walFileSize(walPath)
	started := time.Now()
	slice := min(walShrinkFirstSlice, walShrinkSliceCap)
	var maxHold time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return slices, shrunk, err
		}
		// A slice is an ftruncate under the writer: it waits for a queued
		// write and for the gaps between edit cycles.
		for s.writeWanted() || (s.cycleYieldEnabled() && s.buildLaneBusy()) {
			select {
			case <-ctx.Done():
				return slices, shrunk, ctx.Err()
			case <-time.After(walReclaimYieldPoll):
			}
		}
		step, serr := s.shrinkWALSliceOnce(ctx, ckptDB, slice)
		if serr != nil {
			log.Printf("store_sqlite: wal shrink paused after %d slices error=%q wal_bytes=%d", slices, serr, walFileSize(walPath))
			return slices, shrunk, serr
		}
		if step.after < step.before {
			slices++
			shrunk += step.before - step.after
			maxHold = max(maxHold, step.hold)
			s.walReclaim.update(func(st *WALReclaimStats) {
				st.ShrinkSlices++
				st.ShrinkBytes += step.before - step.after
				st.ShrinkSliceHoldMax = max(st.ShrinkSliceHoldMax, step.hold)
			})
			// Adapt the next slice to the measured rate.
			if step.truncate > 0 {
				rate := float64(step.before-step.after) / step.truncate.Seconds()
				slice = int64(rate * walShrinkSliceHold.Seconds())
			}
			slice = min(max(slice, walShrinkMinSlice), walShrinkMaxSlice, walShrinkSliceCap)
		}
		if step.done {
			break
		}
		select {
		case <-ctx.Done():
			return slices, shrunk, ctx.Err()
		case <-time.After(walShrinkGap):
		}
	}
	s.walShrinkPending.Store(false)
	if lerr := s.restoreWriterJournalLimit(ctx); lerr != nil {
		log.Printf("store_sqlite: wal shrink could not restore the writer's journal_size_limit error=%q", lerr)
	}
	if slices > 0 {
		log.Printf("store_sqlite: wal shrunk bytes_before=%d bytes_after=%d slices=%d max_slice_hold=%s elapsed=%s",
			startBytes, walFileSize(walPath), slices, maxHold.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
	}
	return slices, shrunk, nil
}

func (s *Store) restoreWriterJournalLimit(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(ctx, walReclaimWriterWait)
	err := s.writeMu.LockContext(wctx)
	cancel()
	if err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.setWriterJournalLimitLocked(ctx, walShrinkKeepBytes)
}
