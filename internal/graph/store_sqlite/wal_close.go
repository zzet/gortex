package store_sqlite

import (
	"encoding/binary"
	"io"
	"os"
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
	// Never os.Open + Close the -shm: closing any descriptor of it releases
	// every SQLite lock this process holds on it (wal_index_file.go).
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
