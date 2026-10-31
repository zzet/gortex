package store_sqlite

import (
	"context"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// Dense-index repair: the maintenance-lane job that rebuilds the dense indexes
// a cold bulk window closed without (denseIndexesMissing). The usual cause is
// the volume filling up during the seal, so the job is built around not
// making that worse:
//
//   - it never runs inline. The exit that recorded the missing indexes posts
//     one request after it has released the write gate, and the lane worker
//     runs it as a quiescing whole-database job, like VACUUM.
//   - it does not try while the disk is still full. An attempt is skipped
//     while the free space on the store's volume is below the store's used
//     bytes (with a floor of denseIndexRepairMinFreeBytes). The dense indexes
//     are a fraction of the used pages, and a CREATE INDEX needs about twice
//     the index size at its peak (the sort plus the WAL frames), so a volume
//     with the store's own size free can always take the rebuild while a full
//     one is never asked to.
//   - it backs off. A skipped or failed attempt re-posts itself after
//     denseIndexRepairBaseDelay, doubling up to denseIndexRepairMaxDelay,
//     so a volume that stays full costs one statfs per backoff step and one
//     log line, not a retry loop.
//   - it holds the write gate one index at a time. The DDL is exactly what
//     createGraphCoreIndexes runs (bulkDroppableIndexes is the single source
//     for both), but each CREATE INDEX takes and releases writeMu on its own,
//     so a writer waits behind at most one index build rather than the set.
//
// It is deliberately not part of the lane's settle point (waitMaintenanceIdle):
// a long rebuild must not hold AwaitMaintenanceIdle callers, which sit on
// publication paths. A completed repair asks the lane for a statistics pass,
// because the rebuilt indexes have no sqlite_stat1 rows yet.

// Vars rather than constants only so the in-package cases can shorten them.
var (
	denseIndexRepairBaseDelay    = 30 * time.Second
	denseIndexRepairMaxDelay     = 15 * time.Minute
	denseIndexRepairMinFreeBytes = int64(64 << 20)
)

// scheduleDenseIndexRepair posts one immediate repair request, resetting any
// backoff an earlier record had built up. The caller has just recorded the
// missing set and must not hold writeMu; like scheduleWALDrain it takes only
// maintenanceSched and never blocks.
func (s *Store) scheduleDenseIndexRepair() {
	if s.coreless() {
		return
	}
	s.maintenanceSched.Lock()
	defer s.maintenanceSched.Unlock()
	if s.maintenanceClosed || s.maintenanceSignal == nil {
		return
	}
	s.stopDenseIndexRepairTimerLocked()
	s.denseRepairDelay = 0
	if s.denseRepairOwed {
		return
	}
	s.denseRepairOwed = true
	select {
	case s.maintenanceSignal <- struct{}{}:
	default:
	}
}

// takeDenseIndexRepair claims an owed repair for the lane worker.
func (s *Store) takeDenseIndexRepair() bool {
	s.maintenanceSched.Lock()
	defer s.maintenanceSched.Unlock()
	if !s.denseRepairOwed || s.maintenanceClosed {
		return false
	}
	s.denseRepairOwed = false
	return true
}

// retryDenseIndexRepairLater re-posts the repair once the current backoff
// elapses and doubles the backoff for the attempt after it. It returns the
// delay it armed, or 0 when the lane is closed.
func (s *Store) retryDenseIndexRepairLater() time.Duration {
	s.maintenanceSched.Lock()
	defer s.maintenanceSched.Unlock()
	if s.maintenanceClosed || s.maintenanceSignal == nil {
		return 0
	}
	delay := s.denseRepairDelay
	if delay <= 0 {
		delay = denseIndexRepairBaseDelay
	}
	s.denseRepairDelay = min(2*delay, denseIndexRepairMaxDelay)
	s.stopDenseIndexRepairTimerLocked()
	signal := s.maintenanceSignal
	s.denseRepairTimer = time.AfterFunc(delay, func() {
		s.maintenanceSched.Lock()
		defer s.maintenanceSched.Unlock()
		if s.maintenanceClosed || s.maintenanceSignal != signal || s.denseRepairOwed {
			return
		}
		s.denseRepairOwed = true
		select {
		case signal <- struct{}{}:
		default:
		}
	})
	return delay
}

// stopDenseIndexRepairTimerLocked cancels a pending retry. The caller holds
// maintenanceSched.
func (s *Store) stopDenseIndexRepairTimerLocked() {
	if s.denseRepairTimer != nil {
		s.denseRepairTimer.Stop()
		s.denseRepairTimer = nil
	}
}

// runDenseIndexRepair is one attempt, run by the lane worker. Nothing it hits
// is an error to anyone: the record stays, the attempt is logged, and the
// backoff decides when the next one comes.
func (s *Store) runDenseIndexRepair(ctx context.Context) {
	missing := s.denseIndexesMissingNames()
	if len(missing) == 0 {
		// A later seal already rebuilt them.
		s.maintenanceSched.Lock()
		s.stopDenseIndexRepairTimerLocked()
		s.denseRepairDelay = 0
		s.maintenanceSched.Unlock()
		return
	}
	avail, need, ok := s.denseIndexRepairSpace()
	if !ok {
		s.denseIndexRepairSkips.Add(1)
		delay := s.retryDenseIndexRepairLater()
		log.Printf("store_sqlite: dense index repair skipped: low free space avail_bytes=%d need_bytes=%d indexes=%s retry_in=%s", avail, need, strings.Join(missing, ","), delay)
		return
	}
	started := time.Now()
	err := s.runMaintenance(ctx, maintenanceDenseIndexRepair, true, func(ctx context.Context) error {
		s.denseIndexRepairAttempts.Add(1)
		return s.repairDenseIndexes(ctx)
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		delay := s.retryDenseIndexRepairLater()
		log.Printf("store_sqlite: dense index repair deferred indexes=%s retry_in=%s error=%q", strings.Join(s.denseIndexesMissingNames(), ","), delay, err)
		return
	}
	s.maintenanceSched.Lock()
	s.stopDenseIndexRepairTimerLocked()
	s.denseRepairDelay = 0
	s.maintenanceSched.Unlock()
	s.denseIndexRepairs.Add(1)
	log.Printf("store_sqlite: dense index repair complete indexes=%s elapsed=%s", strings.Join(missing, ","), time.Since(started).Round(time.Millisecond))
	// The rebuilt indexes carry no statistics yet; the lane's next pass
	// notices that and analyzes them.
	s.schedulePublishMaintenance()
}

// denseIndexRepairSpace reports the free space on the store's volume, the
// amount an attempt needs, and whether the attempt may run. An unreadable
// free-space figure is a skip: the gate exists for the full volume, which is
// exactly where a probe may fail too.
func (s *Store) denseIndexRepairSpace() (avail, need int64, ok bool) {
	free, total := s.CompactStats()
	need = max(total-free, denseIndexRepairMinFreeBytes)
	probe := platform.DiskAvailBytes
	if override := s.denseRepairDiskAvail.Load(); override != nil && *override != nil {
		probe = *override
	}
	bytes, err := probe(filepath.Dir(s.dbPath))
	if err != nil {
		return -1, need, false
	}
	avail = int64(min(bytes, uint64(math.MaxInt64)))
	return avail, need, avail >= need
}

// repairDenseIndexes rebuilds every index in the missing record, one write-gate
// hold per index. It refuses, like VACUUM, while a bulk window pins the writer:
// that window drops and seals the same indexes itself.
func (s *Store) repairDenseIndexes(ctx context.Context) error {
	for _, name := range s.denseIndexesMissingNames() {
		if err := s.repairDenseIndex(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) repairDenseIndex(ctx context.Context, name string) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return fmt.Errorf("%w: %s: write gate: %w", ErrMaintenanceBusy, maintenanceDenseIndexRepair, err)
	}
	defer s.writeMu.Unlock()
	if s.bulkConn != nil {
		return fmt.Errorf("%w: %s: bulk writer pinned", ErrMaintenanceBusy, maintenanceDenseIndexRepair)
	}
	for _, idx := range bulkDroppableIndexes {
		if idx.name != name {
			continue
		}
		if _, err := s.writerDB.ExecContext(ctx, idx.ddl); err != nil {
			return fmt.Errorf("store_sqlite: repair index %s: %w", name, err)
		}
		break
	}
	// Under the hold that built it, so whichever window recorded the name, the
	// index it names is present now.
	var remaining []string
	for _, missing := range s.denseIndexesMissingNames() {
		if missing != name {
			remaining = append(remaining, missing)
		}
	}
	s.setDenseIndexesMissingLocked(remaining)
	return nil
}
