package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/platform"
)

// A coordinated cold load deliberately disables SQLite's automatic WAL
// checkpointing on its pinned writer. These limits bound the amount of active
// WAL a reader-limited PASSIVE checkpoint may leave behind. The limit grows
// with the database so a large corpus is not forced through an artificially
// tiny window. The 8 GiB ceiling is an intentional product safety limit on
// *new, uncheckpointable growth*, not total database or reusable WAL size: a
// reader pinning more active change than that fails its unpublished generation
// instead of recreating the observed 40+ GiB volume-exhaustion failure.
const (
	bulkWALHardLimitFloor   = int64(4 << 30) // 4 GiB
	bulkWALHardLimitCeiling = int64(8 << 30) // 8 GiB
	bulkDiskReserveFloor    = int64(1 << 30) // 1 GiB
	bulkDiskReserveCeiling  = int64(8 << 30) // 8 GiB
	// A cheap WAL-file stat after each committed bulk batch closes the gap where
	// a small number of unusually large rows crosses the byte budget long before
	// a row-count checkpoint is due. Full DB/statfs inspection starts here.
	bulkWALInspectionFloor = bulkWALHardLimitFloor / 2
	bulkWALInspectionMin   = int64(64 << 20)
	bulkWALInspectionStep  = int64(256 << 20)

	// At most eight base row intervals separate automatic retries. Pressure
	// inspection remains at the base interval and can force an earlier attempt.
	bulkCheckpointMaxBackoffShift = uint8(3)
)

// ErrBulkLoadWALPressure is returned through AddBatchChecked when a cold
// generation cannot keep its active WAL bounded. Use errors.As with
// *BulkLoadWALPressureError and inspect Committed(): a false value is an
// admission refusal before the first transaction; a true value means the
// triggering batch is durable in an unpublished generation and must not be
// retried.
var ErrBulkLoadWALPressure = errors.New("store_sqlite: bulk WAL pressure")

type bulkWALPressureSnapshot struct {
	DBBytes            int64
	WALBytes           int64
	ReusableWALBytes   int64
	WALProbeFailed     bool
	DiskAvailableBytes uint64
	DiskAvailableKnown bool
}

// BulkLoadWALPressureError describes a safety stop in a cold bulk generation.
// Committed distinguishes an initial admission refusal from a post-commit
// stop; only the latter has a durable triggering batch that must not be retried.
type BulkLoadWALPressureError struct {
	Reason             string
	DBBytes            int64
	WALBytes           int64
	ReusableWALBytes   int64
	WALProbeFailed     bool
	DiskAvailableBytes uint64
	DiskAvailableKnown bool
	Busy               int
	WALFrames          int
	CheckpointedFrames int
	BatchCommitted     bool
}

func (e *BulkLoadWALPressureError) Error() string {
	if e == nil {
		return "bulk WAL pressure limit reached"
	}
	available := "unknown"
	if e.DiskAvailableKnown {
		available = fmt.Sprintf("%d", e.DiskAvailableBytes)
	}
	return fmt.Sprintf(
		"bulk WAL pressure limit reached: reason=%s batch_committed=%t db_bytes=%d wal_bytes=%d reusable_wal_bytes=%d wal_probe_failed=%t disk_available_bytes=%s busy=%d wal_frames=%d checkpointed_frames=%d",
		e.Reason, e.BatchCommitted, e.DBBytes, e.WALBytes, e.ReusableWALBytes, e.WALProbeFailed, available, e.Busy, e.WALFrames, e.CheckpointedFrames,
	)
}

func (e *BulkLoadWALPressureError) Unwrap() error { return ErrBulkLoadWALPressure }

// Committed reports the partial-success contract carried by this error.
func (e *BulkLoadWALPressureError) Committed() bool {
	return e != nil && e.BatchCommitted
}

func clampBulkPressureLimit(value, floor, ceiling int64) int64 {
	if value < floor {
		return floor
	}
	if value > ceiling {
		return ceiling
	}
	return value
}

func bulkWALPressureLimits(dbBytes int64) (softWAL, hardWAL int64, diskReserve uint64) {
	if dbBytes < 0 {
		dbBytes = 0
	}
	hardWAL = clampBulkPressureLimit(dbBytes, bulkWALHardLimitFloor, bulkWALHardLimitCeiling)
	softWAL = hardWAL / 2
	reserve := clampBulkPressureLimit(dbBytes/4, bulkDiskReserveFloor, bulkDiskReserveCeiling)
	return softWAL, hardWAL, uint64(reserve)
}

func (p bulkWALPressureSnapshot) walGrowthBytes() int64 {
	if p.WALBytes <= p.ReusableWALBytes {
		return 0
	}
	return p.WALBytes - p.ReusableWALBytes
}

func (p bulkWALPressureSnapshot) nextInspectionBytes() int64 {
	step := p.inspectionStep()
	growth := p.walGrowthBytes()
	if growth > int64(^uint64(0)>>1)-step {
		return int64(^uint64(0) >> 1)
	}
	return growth + step
}

func (p bulkWALPressureSnapshot) inspectionStep() int64 {
	step := bulkWALInspectionStep
	if p.DiskAvailableKnown {
		_, _, reserve := bulkWALPressureLimits(p.DBBytes)
		if p.DiskAvailableBytes > reserve {
			usable := int64(p.DiskAvailableBytes - reserve)
			step = clampBulkPressureLimit(usable/4, bulkWALInspectionMin, bulkWALInspectionStep)
		}
	}
	return step
}

func (p bulkWALPressureSnapshot) needsCheckpoint() bool {
	if p.WALProbeFailed {
		return true
	}
	softWAL, _, reserve := bulkWALPressureLimits(p.DBBytes)
	if p.walGrowthBytes() >= softWAL {
		return true
	}
	// When headroom is smaller than one bounded WAL window plus the reserve,
	// try to make the existing WAL reusable before accepting another cadence.
	return p.DiskAvailableKnown && p.DiskAvailableBytes <= reserve+uint64(p.inspectionStep())
}

func (p bulkWALPressureSnapshot) failureBeforeCheckpoint() error {
	if p.WALProbeFailed {
		return p.newPressureError("wal_probe_unavailable", walCheckpointResult{}, true)
	}
	if !p.DiskAvailableKnown {
		return nil
	}
	_, _, reserve := bulkWALPressureLimits(p.DBBytes)
	if p.DiskAvailableBytes <= reserve {
		return p.newPressureError("disk_reserve", walCheckpointResult{}, true)
	}
	usable := p.DiskAvailableBytes - reserve
	required := uint64(p.inspectionStep())
	// Charge only growth beyond the last completely checkpointed allocation.
	// The writer connection's 64 MiB journal_size_limit truncates hidden reuse
	// after a WAL reset; without a reset SQLite appends and physical growth is
	// visible here. Charging the full reusable high-water mark would reject a
	// tiny active cycle merely because PASSIVE had left a large file allocated.
	copyExposure := p.walGrowthBytes()
	if copyExposure > 0 {
		if uint64(copyExposure) > ^uint64(0)-required {
			required = ^uint64(0)
		} else {
			required += uint64(copyExposure)
		}
	}
	// PASSIVE may need to copy every active WAL page into a growing main DB
	// while the WAL allocation remains live. Refuse that copy when it could
	// consume the reserve; waiting for its SQLITE_FULL result is too late.
	if usable <= required {
		return p.newPressureError("checkpoint_headroom", walCheckpointResult{}, true)
	}
	return nil
}

func (p bulkWALPressureSnapshot) failureBeforeFirstWrite() error {
	if p.WALProbeFailed {
		return p.newPressureError("wal_probe_unavailable", walCheckpointResult{}, false)
	}
	if !p.DiskAvailableKnown {
		return nil
	}
	_, _, reserve := bulkWALPressureLimits(p.DBBytes)
	if p.DiskAvailableBytes <= reserve+uint64(p.inspectionStep()) {
		return p.newPressureError("initial_headroom", walCheckpointResult{}, false)
	}
	return nil
}

func (p bulkWALPressureSnapshot) failureAfterCheckpoint(result walCheckpointResult, checkpointErr error) error {
	if p.WALProbeFailed {
		return p.newPressureError("wal_probe_unavailable", result, true)
	}
	_, hardWAL, reserve := bulkWALPressureLimits(p.DBBytes)
	if p.DiskAvailableKnown && p.DiskAvailableBytes <= reserve {
		return p.newPressureError("disk_reserve", result, true)
	}
	// A complete PASSIVE checkpoint makes its physical WAL file reusable even
	// though PASSIVE need not truncate that file. File size alone must therefore
	// never reject a healthy, fully checkpointed writer.
	if checkpointErr == nil {
		if p.DiskAvailableKnown &&
			p.DiskAvailableBytes <= reserve+uint64(p.inspectionStep()) {
			return p.newPressureError("next_write_headroom", result, true)
		}
		return nil
	}
	walGrowth := p.walGrowthBytes()
	if walGrowth >= hardWAL {
		return p.newPressureError("active_wal_limit", result, true)
	}
	// Once a checkpoint is known to be reader-limited, preserve a concrete next
	// growth window even when the current WAL is still small. This stops before
	// reserve exhaustion without imposing a multi-GiB false floor on every
	// legitimate large-store reader.
	if p.DiskAvailableKnown && p.DiskAvailableBytes <= reserve+uint64(p.inspectionStep()) {
		return p.newPressureError("wal_headroom", result, true)
	}
	return nil
}

func (p bulkWALPressureSnapshot) newPressureError(reason string, result walCheckpointResult, committed bool) error {
	return &BulkLoadWALPressureError{
		Reason: reason, DBBytes: p.DBBytes, WALBytes: p.WALBytes, ReusableWALBytes: p.ReusableWALBytes,
		WALProbeFailed:     p.WALProbeFailed,
		DiskAvailableBytes: p.DiskAvailableBytes, DiskAvailableKnown: p.DiskAvailableKnown,
		Busy: result.Busy, WALFrames: result.WALFrames, CheckpointedFrames: result.CheckpointedFrames,
		BatchCommitted: committed,
	}
}

func (s *Store) inspectBulkWALPressure() bulkWALPressureSnapshot {
	if s.bulkWALPressureProbe != nil {
		return s.bulkWALPressureProbe()
	}
	dbBytes, _ := s.DBStats()
	walBytes := s.currentBulkWALBytes()
	if walBytes < s.bulkReusableWALBytes {
		// PASSIVE does not truncate, but a later WAL reset, journal-size policy,
		// or external checkpoint can. Lower the high-water baseline immediately
		// so regrowth is never hidden behind a stale reusable allocation.
		// The first observation after a reset may already include substantial
		// newly active WAL. Rebase to zero, not to the observed size, so that
		// shrink-then-regrow in one batch cannot hide that new allocation.
		s.bulkReusableWALBytes = 0
		s.bulkWALInspectionBytes = bulkWALInspectionMin
	}
	snapshot := bulkWALPressureSnapshot{
		DBBytes: dbBytes, WALBytes: walBytes, ReusableWALBytes: s.bulkReusableWALBytes,
		WALProbeFailed: s.bulkWALProbeFailed,
	}
	if s.dbPath == "" {
		return snapshot
	}
	if available, err := platform.DiskAvailBytes(filepath.Dir(s.dbPath)); err == nil {
		snapshot.DiskAvailableBytes = available
		snapshot.DiskAvailableKnown = true
	}
	return snapshot
}

func (s *Store) currentBulkWALBytes() int64 {
	if s.bulkWALSizeProbe != nil {
		bytes, err := s.bulkWALSizeProbe()
		if err == nil {
			s.bulkLastWALBytes = bytes
			s.bulkWALProbeFailed = false
			return bytes
		}
		s.bulkWALProbeFailed = true
		return s.bulkLastWALBytes
	}
	if s.dbPath == "" {
		s.bulkLastWALBytes = 0
		s.bulkWALProbeFailed = false
		return 0
	}
	if info, err := os.Stat(s.dbPath + "-wal"); err == nil {
		s.bulkLastWALBytes = info.Size()
		s.bulkWALProbeFailed = false
		return info.Size()
	} else if os.IsNotExist(err) {
		s.bulkLastWALBytes = 0
		s.bulkWALProbeFailed = false
		return 0
	}
	s.bulkWALProbeFailed = true
	return s.bulkLastWALBytes
}

func (s *Store) bulkWALInspectionThreshold() int64 {
	if s.bulkWALInspectionBytes < bulkWALInspectionMin {
		return bulkWALInspectionFloor
	}
	return s.bulkWALInspectionBytes
}

func (s *Store) bulkWALGrowthSinceReusable(walBytes int64) int64 {
	if walBytes < s.bulkReusableWALBytes {
		s.bulkReusableWALBytes = 0
		s.bulkWALInspectionBytes = bulkWALInspectionMin
	}
	return walBytes - s.bulkReusableWALBytes
}

func (s *Store) runBulkPassiveCheckpoint(ctx context.Context, conn *sql.Conn, mode string) (walCheckpointResult, error) {
	if s.bulkPassiveCheckpoint != nil {
		return s.bulkPassiveCheckpoint(ctx, conn, mode)
	}
	return checkpointWALOnceOn(ctx, conn, mode)
}
