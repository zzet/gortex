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
	// Synchronous test seam after the final slice releases the writer and
	// before restoration reacquires it. Nil outside deterministic gap tests.
	walShrinkBeforeRestore func()
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
	return s.resetWALForReclaimWithBusyTimeout(ctx, int64(walResetBusyMillis))
}

// A pressure reset is already under a short writer credit. Its first SQLite
// lock probe must return Busy without spending that same credit waiting: only
// an actual Busy result admits the existing reader drain outside the writer.
func (s *Store) resetWALForPressure(ctx context.Context) (walCheckpointResult, error) {
	return s.resetWALForReclaimWithBusyTimeout(ctx, 0)
}

func (s *Store) resetWALForReclaimWithBusyTimeout(ctx context.Context, busyMillis int64) (walCheckpointResult, error) {
	if err := ctx.Err(); err != nil {
		return walCheckpointResult{}, err
	}
	walPath := s.dbPath + "-wal"
	if s.observedWALFileSize(walPath) < walShrinkInPlaceBytes {
		var result walCheckpointResult
		attempted := false
		err := s.withWriterResetConnWithBusyTimeout(ctx, busyMillis, func(conn *sql.Conn) error {
			attempted = true
			var err error
			result, err = checkpointWALOnceOn(ctx, conn, "TRUNCATE")
			return err
		})
		if hook := walResetResultHook; hook != nil {
			err = hook(err)
		}
		// modernc may return ctx.Err after SQLite completed the checkpoint.
		// Every caller observes that physical result before releasing writeMu.
		if info, statErr := os.Stat(walPath); attempted && walResetInterrupted(err) && statErr == nil && info.Size() == 0 {
			err = nil
		}
		return result, err
	}
	return s.resetWALInPlaceLockedWithBusyTimeout(ctx, busyMillis)
}

// walResetInterrupted reports a reset whose error is only its context ending.
// Once a statement's interrupt fired during the call, the driver returns
// ctx.Err() in place of its result even when SQLite finished the work, and
// SQLite's wait for readers to leave the log never checks the interrupt, so a
// reset can complete just after its deadline. The reset then reads from the
// log whether it happened, while the writer is still held.
func walResetInterrupted(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// walResetBusyMillis bounds how long a reset on the writer connection waits on
// SQLite locks (readers holding the log): the writer connection's own busy
// timeout is 5 s, which a reset under the write gate must never take.
var walResetBusyMillis = sqliteCheckpointBusyTimeoutMillis

// walResetCheckpointConnObserver is a scoped test seam at the actual reset
// connection, after its Busy policy is installed and before the checkpoint.
// Nil in production; install before owned work starts and restore after join.
// The callback must not retain the connection or start asynchronous work.
var walResetCheckpointConnObserver func(context.Context, *sql.Conn)

// walResetResultHook injects an error after SQL for reset outcome tests.
// Nil in production; recognition still runs while the write gate is held.
var walResetResultHook func(error) error

// withWriterResetConn runs fn on the writer connection with its busy timeout
// lowered to walResetBusyMillis (or what is left of ctx's deadline), restored
// afterwards. The caller holds
// writeMu. A bulk window reuses its pinned connection with the same bounded
// busy policy, then restores that connection's previous timeout.
func (s *Store) withWriterResetConn(ctx context.Context, fn func(conn *sql.Conn) error) error {
	return s.withWriterResetConnWithBusyTimeout(ctx, int64(walResetBusyMillis), fn)
}

func (s *Store) withWriterResetConnWithBusyTimeout(ctx context.Context, busyMillis int64, fn func(conn *sql.Conn) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return err
	}
	defer release()
	var previous int64
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&previous); err != nil {
		return err
	}
	busy := max(busyMillis, 0)
	if deadline, ok := ctx.Deadline(); ok {
		busy = min(busy, max(time.Until(deadline).Milliseconds(), 0))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Prepare may apply a PRAGMA even when ExecContext reports cancellation.
	// Own restoration before issuing it, including on a pinned bulk connection.
	defer func() {
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf(`PRAGMA busy_timeout = %d`, previous))
	}()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, busy)); err != nil {
		return err
	}
	if observe := walResetCheckpointConnObserver; observe != nil {
		observe(ctx, conn)
	}
	return fn(conn)
}

func (s *Store) resetWALInPlaceLockedWithBusyTimeout(ctx context.Context, busyMillis int64) (result walCheckpointResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	before := readWALWriteMark(s.dbPath)
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return result, err
	}
	var previousLimit int64
	err = conn.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&previousLimit)
	release()
	if err != nil {
		return result, err
	}
	defer func() {
		if err == nil {
			return
		}
		// A fully copied log can restart on the next commit even when its
		// RESTART was refused. Remember that incarnation until it restarts or
		// is emptied; its live-frame floor alone cannot authorize restoration.
		if after, ok := readWALReclaimFrontier(s.dbPath); ok && after.mx > 0 && after.backfill == after.mx {
			s.walLimitKept.Store(&after)
			return
		}
		if !s.walShrinkPending.Load() && s.walLimitKept.Load() == nil && previousLimit != -1 {
			_ = s.setWriterJournalLimitLocked(context.Background(), previousLimit)
		}
	}()
	// Own cleanup before the PRAGMA, whose preparation may apply the limit
	// even if the driver subsequently reports cancellation.
	if err := s.setWriterJournalLimitLocked(ctx, -1); err != nil {
		return result, fmt.Errorf("wal in-place reset: lift the writer's journal_size_limit: %w", err)
	}
	err = s.withWriterResetConnWithBusyTimeout(ctx, busyMillis, func(conn *sql.Conn) error {
		return s.restartWALInPlaceOn(ctx, conn, &result)
	})
	if hook := walResetResultHook; hook != nil {
		err = hook(err)
	}
	after := readWALWriteMark(s.dbPath)
	if walInPlaceResetCompleted(before, after) {
		// A physical restart owes bounded cleanup even when an unrelated
		// driver error must still be returned. Restoring the limit here would
		// let a later commit truncate the restarted file in one call.
		s.walShrinkPending.Store(true)
		if err == nil || walResetInterrupted(err) {
			s.walReclaim.update(func(st *WALReclaimStats) { st.ShrinkInPlaceResets++ })
			return result, nil
		}
	}
	if err != nil {
		return result, err
	}
	return result, fmt.Errorf("%w: mx_frame=%d", errWALResetNotRestarted, after.MxFrame)
}

func walInPlaceResetCompleted(before, after WALWriteMark) bool {
	// A small mxFrame alone may predate this attempt. The changed salt proves
	// a new incarnation; its bounded marker frames prove the log restarted.
	return before.Valid && after.Valid && before.Salt != after.Salt && after.MxFrame <= walShrinkMaxRestartFrames
}

// restartWALInPlaceOn is the in-place reset's statements on one connection: a
// RESTART checkpoint, then one committed write so the log starts over at its
// first frame.
func (s *Store) restartWALInPlaceOn(ctx context.Context, conn *sql.Conn, result *walCheckpointResult) (err error) {
	*result, err = checkpointWALOnceOn(ctx, conn, "RESTART")
	if err != nil {
		return err
	}
	// ExecContext can report cancellation after SQLite applied BEGIN. Own
	// rollback before issuing it, including that ambiguous error boundary.
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
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
// connection. The caller holds writeMu. SQLite applies this pager setting at
// preparation, before the driver can observe cancellation. Reject ended
// contexts first, then detach this lock-free mutation so its result describes
// whether the setting actually changed.
func (s *Store) setWriterJournalLimitLocked(ctx context.Context, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA journal_size_limit = %d`, limit))
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
	// The statement may have applied even when its context reports an error.
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return out, err
	}
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
	out.before = s.observedWALFileSize(walPath)
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
// Kept incarnations also schedule limit restoration after restart/emptying,
// even if their file no longer reaches the ordinary shrink threshold.
func (s *Store) walShrinkNeeded() bool {
	if s.walShrinkPending.Load() {
		return true
	}
	if kept := s.walLimitKept.Load(); kept != nil && s.walKeptLogRestarted(kept) {
		return true
	}
	snap, ok := readWALIndexSnapshot(s.dbPath)
	if !ok {
		return false
	}
	used := int64(32) + int64(snap.MxFrame)*(24+4096)
	return snap.WALBytes > max(used, walShrinkKeepBytes)+walShrinkMinSlice && snap.WALBytes >= walShrinkInPlaceBytes
}

// walKeptLogRestarted reports whether a kept log has restarted (its salt
// changed) or been emptied (a TRUNCATE elsewhere). A wal-index that cannot be
// read consistently over a non-empty file is neither.
func (s *Store) walKeptLogRestarted(kept *walReclaimFrontier) bool {
	if current, ok := readWALReclaimFrontier(s.dbPath); ok {
		return current.salt != kept.salt
	}
	info, err := os.Stat(s.dbPath + "-wal")
	// A missing WAL has no old incarnation left to protect. Other stat
	// errors are not evidence that a non-empty, reader-pinned file vanished.
	return (err == nil && info.Size() == 0) || os.IsNotExist(err)
}

// shrinkWAL shrinks the -wal back to journal_size_limit in slices, releasing
// the writer between them and waiting while a write wants it, then restores
// the writer's journal_size_limit. It stops early on ctx or a slice error
// (the next poll resumes it).
func (s *Store) shrinkWAL(ctx context.Context, ckptDB *sql.DB) (slices int, shrunk int64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	// Even a heuristic shrink must retain cleanup intent if restoring the
	// pager setting fails after the file reaches its floor.
	s.walShrinkPending.Store(true)
	walPath := s.dbPath + "-wal"
	startBytes := s.observedWALFileSize(walPath)
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
			log.Printf("store_sqlite: wal shrink paused after %d slices error=%q wal_bytes=%d", slices, serr, s.observedWALFileSize(walPath))
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
	if hook := walShrinkBeforeRestore; hook != nil {
		hook()
	}
	if lerr := s.restoreWriterJournalLimit(ctx); lerr != nil {
		log.Printf("store_sqlite: wal shrink could not restore the writer's journal_size_limit error=%q", lerr)
		return slices, shrunk, lerr
	}
	if slices > 0 {
		log.Printf("store_sqlite: wal shrunk bytes_before=%d bytes_after=%d slices=%d max_slice_hold=%s elapsed=%s",
			startBytes, s.observedWALFileSize(walPath), slices, maxHold.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
	}
	return slices, shrunk, nil
}

// restoreWriterJournalLimit gives the writer its 64 MiB journal_size_limit
// back once the file is shrunk, and clears a kept log that has restarted. A
// kept log that has not restarted yet keeps the limit lifted: its frames are
// live, so no slice could cut the file, and the commit that restarts it must
// not truncate the file in one call. walShrinkNeeded reports it once it has
// restarted. Decided under the write gate, which a reset that keeps a log
// also holds.
func (s *Store) restoreWriterJournalLimit(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(ctx, walReclaimWriterWait)
	err := s.writeMu.LockContext(wctx)
	cancel()
	if err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	if kept := s.walLimitKept.Load(); kept != nil {
		if !s.walKeptLogRestarted(kept) {
			// The slices reached their live-frame floor. The kept receipt
			// still protects this incarnation and schedules its later restart.
			s.walShrinkPending.Store(false)
			return nil
		}
	}
	// The final slice released the writer. A reset admitted in that gap can
	// leave a new incarnation in the same large file; verify its current
	// floor before restoring a limit that would truncate it on a later commit.
	info, err := os.Stat(s.dbPath + "-wal")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && info.Size() > walShrinkKeepBytes {
		frontier, ok := readWALReclaimFrontier(s.dbPath)
		if !ok {
			return fmt.Errorf("wal shrink: final WAL frontier unreadable")
		}
		conn, release, err := s.activeWriteConnLocked(ctx)
		if err != nil {
			return err
		}
		var pageSize int64
		err = conn.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize)
		release()
		if err != nil {
			return err
		}
		floor := max(walShrinkKeepBytes, int64(32)+int64(frontier.mx)*(24+pageSize))
		if info.Size() > floor {
			// Do not erase the newer reset's pending shrink. The next poll
			// resumes bounded slices instead of allowing a whole-file truncate.
			return nil
		}
	}
	if err := s.setWriterJournalLimitLocked(ctx, walShrinkKeepBytes); err != nil {
		return err
	}
	// Clear intent only after the setting succeeded, while the writer is
	// still held so a later reset cannot have its pending state erased.
	s.walLimitKept.Store(nil)
	s.walShrinkPending.Store(false)
	return nil
}
