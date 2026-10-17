package store_sqlite

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// walIndexSnapshot is a best-effort read of SQLite's shared wal-index (-shm)
// header and the WAL file header. It takes no lock, so a concurrent writer can
// make it momentarily stale; it is telemetry and budget sizing, never a
// correctness input.
type walIndexSnapshot struct {
	MxFrame            uint32 // frames in the log (wal-index header)
	NBackfill          uint32 // frames already copied into the main file
	NBackfillAttempted uint32
	// CheckpointSeq is the WAL header's checkpoint sequence. It is written from
	// the WRITING connection's own reset counter, so a reset performed by a
	// different connection (the reclaim's) does not move it; mxFrame moving
	// backwards is the connection-independent reset signal.
	CheckpointSeq uint32
	WALBytes      int64
}

// PendingFrames is the backfill SQLite still owes: frames a checkpoint must
// copy before the log can be reset.
func (w walIndexSnapshot) PendingFrames() int64 {
	if w.MxFrame <= w.NBackfill {
		return 0
	}
	return int64(w.MxFrame - w.NBackfill)
}

// readWALIndexSnapshot reads dbPath-shm and dbPath-wal. The wal-index is in
// native byte order: WalIndexHdr is 48 bytes (mxFrame at offset 16) stored
// twice, followed by WalCkptInfo at offset 96 (nBackfill at 96, the five read
// marks, the eight lock bytes, nBackfillAttempted at 128). The WAL header
// stores its checkpoint sequence big-endian at offset 12.
func readWALIndexSnapshot(dbPath string) (walIndexSnapshot, bool) {
	var snap walIndexSnapshot
	// The platform reader preserves SQLite's locks: POSIX retains the
	// observation descriptor; Windows closes only its separate reader handle.
	var hdr [136]byte
	if err := readWALIndexHeader(dbPath, hdr[:]); err != nil {
		return snap, false
	}
	snap.MxFrame = binary.NativeEndian.Uint32(hdr[16:20])
	snap.NBackfill = binary.NativeEndian.Uint32(hdr[96:100])
	snap.NBackfillAttempted = binary.NativeEndian.Uint32(hdr[128:132])
	// The -wal carries no SQLite locks (they all live on the -shm and the
	// main file), so a short-lived descriptor of it is harmless.
	if wal, err := os.Open(dbPath + "-wal"); err == nil {
		var wh [16]byte
		if _, err := io.ReadFull(wal, wh[:]); err == nil {
			snap.CheckpointSeq = binary.BigEndian.Uint32(wh[12:16])
		}
		if info, err := wal.Stat(); err == nil {
			snap.WALBytes = info.Size()
		}
		_ = wal.Close()
	}
	return snap, true
}

const (
	// closeCheckpointBaseBudget covers lock acquisition, the fsyncs and a
	// small copy. It is also the whole estimate when the backlog is unknown.
	closeCheckpointBaseBudget = 15 * time.Second
	// closeCheckpointPerFrame is the copy cost per pending frame when this
	// store has no close of its own measured yet (closeRate). Measured on the
	// live store: the 2026-09-25 08:16:07 close copied 264,431 pending frames
	// (of 524,077; a 2.16 GB -wal against a 19 GB database) in 50.8 s, 192 µs
	// per frame. The earlier 10 µs predicted 17.6 s for it. Frames land on
	// scattered pages of a large file, so the copy is I/O-bound, not memcpy.
	closeCheckpointPerFrame = 200 * time.Microsecond
	// closeCheckpointEstimateCap caps the estimate published for the daemon's
	// stop path. The stop path no longer gives up on the estimate alone — it
	// waits while the close makes progress (close_progress.go) — so the cap
	// only keeps a pathological backlog from publishing nonsense.
	closeCheckpointEstimateCap = time.Hour
	// closeReadDrainDeadline bounds Close's wait for in-flight pool reads
	// before the final TRUNCATE.
	closeReadDrainDeadline = time.Second
)

// closeCheckpointEstimate is how long the final checkpoint should be allowed
// for a backlog of pendingFrames: base + pending × per-frame cost, capped at
// two minutes; an unknown backlog (-1) gets the base.
func closeCheckpointEstimate(pendingFrames int64) time.Duration {
	return closeCheckpointEstimateAt(pendingFrames, closeCheckpointPerFrame)
}

// closeCheckpointEstimateAt is the estimate at a given per-frame cost.
func closeCheckpointEstimateAt(pendingFrames int64, perFrame time.Duration) time.Duration {
	if pendingFrames <= 0 {
		return closeCheckpointBaseBudget
	}
	if perFrame <= 0 {
		perFrame = closeCheckpointPerFrame
	}
	if pendingFrames > int64((closeCheckpointEstimateCap-closeCheckpointBaseBudget)/perFrame) {
		return closeCheckpointEstimateCap
	}
	return closeCheckpointBaseBudget + time.Duration(pendingFrames)*perFrame
}

// closeCheckpointDeadline is the deadline Close actually imposes, zero meaning
// none. By default there is none, deliberately: Close is the last durability
// boundary, and a deadline cannot shorten shutdown anyway — an interrupted
// TRUNCATE publishes no progress (nBackfill moves only when a pass completes)
// and SQLite's close of the last connection then runs the same full
// checkpoint itself, uninterruptibly. What bounds a restart is keeping the
// backlog small (the reclaim) and letting the stop path wait the estimate
// (CloseCheckpointEstimate). GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S > 0 opts into
// a deadline of min(that, the estimate).
func closeCheckpointDeadline(pendingFrames int64) time.Duration {
	raw := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S"))
	if raw == "" {
		return 0
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return 0
	}
	if secs > 24*3600 {
		secs = 24 * 3600
	}
	knob := time.Duration(secs) * time.Second
	if est := closeCheckpointEstimate(pendingFrames); est < knob {
		return est
	}
	return knob
}

// CloseCheckpointEstimate reports how long Close's final checkpoint should be
// given for the backlog on disk right now, and that backlog in frames (-1 when
// the wal-index cannot be read). A stop path that force-kills the process
// must wait at least this long: a killed checkpoint records no progress, and
// the next open recovers the whole log with nBackfill back at zero.
func (s *Store) CloseCheckpointEstimate() (time.Duration, int64) {
	if s.coreless() || s.dbPath == "" || isMemoryPath(s.dbPath) {
		return 0, 0
	}
	pending := int64(-1)
	if snap, ok := readWALIndexSnapshot(s.dbPath); ok {
		pending = snap.PendingFrames()
	}
	return closeCheckpointEstimateAt(pending, closePerFrame(s.dbPath)), pending
}

// closeCheckpointWAL is Close's final TRUNCATE. The periodic loop and the
// maintenance lane are already joined, so nothing else competes for the log:
// the store's own pool readers are drained through the read gate (bounded),
// and the outcome is logged with the numbers an operator needs to tell
// "drained" from "gave up" — pending frames before, frames checkpointed, reset
// or not, duration against the backlog's estimate.
func (s *Store) closeCheckpointWAL() error {
	before, known := readWALIndexSnapshot(s.dbPath)
	pending := int64(-1)
	if known {
		pending = before.PendingFrames()
	}
	perFrame := closePerFrame(s.dbPath)
	estimate := closeCheckpointEstimateAt(pending, perFrame)
	deadline := closeCheckpointDeadline(pending)

	started := time.Now()
	// Tell the stop path how the close is going (close_progress.go): a
	// daemon stop that sees the copy advancing waits instead of killing it.
	stopProgress := startCloseProgress(s.dbPath, pending, perFrame, estimate, started)
	reopen := func() {}
	if s.readGate != nil {
		// A drain failure is not fatal: the TRUNCATE busy-waits for any read
		// still in flight.
		if r, _, err := s.readGate.quiesce(context.Background(), started.Add(closeReadDrainDeadline)); err == nil {
			reopen = r
		}
	}
	ctx := context.Background()
	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
	result, err := s.checkpointWALWithContextResult(ctx)
	// Reopen before the pools close so a reader parked at the gate fails on
	// the closed pool instead of waiting forever.
	reopen()
	walAfter := walFileSize(s.dbPath + "-wal")
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	took := time.Since(started)
	stopProgress(err == nil)
	learned := ""
	if err == nil {
		if rate, ok := recordCloseRate(s.dbPath, pending, took); ok {
			learned = fmt.Sprintf(" learned_per_frame=%s", rate)
		}
	}
	log.Printf("store_sqlite: close wal checkpoint mode=TRUNCATE frames_pending_before=%d wal_frames_before=%d wal_bytes_before=%d busy=%d checkpointed_frames=%d reset=%t wal_bytes_after=%d duration=%s estimate=%s per_frame=%s deadline=%s error=%q%s",
		pending, before.MxFrame, before.WALBytes, result.Busy, result.CheckpointedFrames, err == nil,
		walAfter, took.Round(time.Millisecond), estimate, perFrame, deadline, errText, learned)
	return err
}
