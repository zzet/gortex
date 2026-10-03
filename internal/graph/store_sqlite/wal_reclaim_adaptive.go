package store_sqlite

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"os"
	"time"
)

// These observations select a budget; SQLite remains the authority for reset.
// Match both wal-index header copies and the WAL salts, rather than treating
// CheckpointSeq (a per-connection counter) as a WAL generation identity.
type walReclaimFrontier struct {
	mx, backfill uint32
	salt         [8]byte
}

func readWALReclaimFrontier(path string) (walReclaimFrontier, bool) {
	var result walReclaimFrontier
	var index [136]byte
	if readWALIndexHeader(path, index[:]) != nil || !bytes.Equal(index[:48], index[48:96]) || index[12] == 0 {
		return result, false
	}
	wal, err := os.Open(path + "-wal")
	if err != nil {
		return result, false
	}
	defer func() { _ = wal.Close() }()
	var header [32]byte
	if _, err := io.ReadFull(wal, header[:]); err != nil || !bytes.Equal(index[32:40], header[16:24]) {
		return result, false
	}
	result.mx = binary.NativeEndian.Uint32(index[16:20])
	result.backfill = binary.NativeEndian.Uint32(index[96:100])
	copy(result.salt[:], header[16:24])
	return result, true
}

type walReclaimSlowTail struct {
	salt        [8]byte
	copyElapsed time.Duration
	covered     uint32
}

func (r *walReclaimResult) recordWriterCredit(w *walReclaimWriterCredit, adaptive bool) {
	r.writerHold = max(r.writerHold, w.longest)
	r.writerSpent += w.spent
	r.writerHolds++
	if adaptive {
		r.adaptiveHold = max(r.adaptiveHold, w.longest)
	}
	if w.slowTail != nil {
		r.slowTail = w.slowTail
	}
}

// A fast store retains its short reset holds. When an urgent log has a proven
// new tail after a completed slow copy, one longer hold allows that final copy
// and sync to finish before more writes append. All holds charged to this
// attempt, including the previous short holds, share the existing two-second
// ceiling. The caller's operation cancellation and edit-lane policy still apply.
// takeAdaptiveWriterBudget proposes credit without consuming it. The caller
// consumes the opportunity only when a qualified adaptive copy actually starts.
func (r *walReclaimResult) takeAdaptiveWriterBudget() time.Duration {
	if !r.urgent || r.lastResort || r.adaptiveUsed || r.slowTail == nil {
		return 0
	}
	remaining := walReclaimMaxWriterHold - r.writerSpent
	budget := min(2*r.slowTail.copyElapsed+walReclaimResetHold, remaining)
	if budget <= walReclaimResetHold {
		return 0
	}
	return budget
}

func (r *walReclaimResult) adaptiveFrontierCurrent(s *Store) bool {
	current, ok := readWALReclaimFrontier(s.dbPath)
	return ok && r.slowTail != nil && current.salt == r.slowTail.salt && current.backfill >= r.slowTail.covered
}

// beginAdaptiveWriterCopy consumes the single completion opportunity only
// after writer admission and the current lane/bulk/WAL identity guards.
func (r *walReclaimResult) beginAdaptiveWriterCopy(budget time.Duration) bool {
	if r.adaptiveUsed || !r.urgent || r.lastResort || r.slowTail == nil || budget <= walReclaimResetHold || budget > walReclaimMaxWriterHold-r.writerSpent {
		return false
	}
	r.adaptiveUsed, r.adaptiveBudget = true, budget
	log.Printf("store_sqlite: wal reclaim adaptive urgent writer credit budget=%s prior_hold=%s slow_copy=%s", budget, r.writerSpent, r.slowTail.copyElapsed)
	return true
}
