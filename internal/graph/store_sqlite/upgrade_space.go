package store_sqlite

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/platform"
)

// Free-space precheck for an in-place schema upgrade.
//
// Open runs every pending in-place step in one transaction with automatic
// checkpoints off, then rebuilds the core and sidecar indexes, so the -wal
// file holds every page the upgrade touched until the first checkpoint after
// Open. Running out of disk partway through leaves the old version in place
// (the stamp is the last write), but it also leaves a WAL the size of
// whatever fit and a daemon that fails the same way on every restart. The
// precheck refuses before the writer connection exists instead, so a store
// the disk cannot upgrade is never touched.
//
// The estimate is deliberately version-agnostic: it reads only the store's
// page counts and whether every pending step is an explicitly additive one,
// so new registry entries need no sizing data of their own.

const (
	// upgradeRewritePercent is the share of live pages a transition is
	// assumed to rewrite when any pending step may reshape graph data (a
	// table rebuild, a re-scoped hot index, a backfill).
	upgradeRewritePercent = 100
	// upgradeAdditiveStepPercent is the share of live pages each explicitly
	// additive step (one that preserves analysis — a single new or reshaped
	// index) is assumed to write. The total is capped at
	// upgradeRewritePercent.
	upgradeAdditiveStepPercent = 25
	// upgradeSpaceHeadroom covers what the estimate does not model: the
	// tables and columns schemaSQL adds, the FTS backfills, the analysis
	// invalidation, and the -shm file. It equals journal_size_limit, the
	// WAL size a store keeps between checkpoints anyway.
	upgradeSpaceHeadroom = 64 << 20

	// walFileHeaderBytes is the fixed header at the start of a -wal file;
	// each frame after it carries walFrameHeaderBytes of its own.
	walFileHeaderBytes = 32
)

// ErrInsufficientUpgradeSpace means Open refused a schema upgrade because
// the volume holding the store has less free space than the upgrade needs.
// The store is left exactly as it was.
var ErrInsufficientUpgradeSpace = errors.New("not enough free disk space to upgrade the sqlite store schema")

// InsufficientUpgradeSpaceError names the store, the transition, and both
// byte counts. Callers can match ErrInsufficientUpgradeSpace with errors.Is.
type InsufficientUpgradeSpaceError struct {
	Path      string
	From      int
	To        int
	Need      uint64
	Available uint64
}

func (e *InsufficientUpgradeSpaceError) Error() string {
	return fmt.Sprintf("%v: upgrading %s from v%d to v%d needs %d bytes (%s) free, %d bytes (%s) available; "+
		"the store was not modified — free disk space on that volume or move the store to a larger one, then start again",
		ErrInsufficientUpgradeSpace, e.Path, e.From, e.To, e.Need, formatMiB(e.Need), e.Available, formatMiB(e.Available))
}

func (e *InsufficientUpgradeSpaceError) Unwrap() error { return ErrInsufficientUpgradeSpace }

func formatMiB(n uint64) string {
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// storeSpaceStats is the on-disk shape of a store as the read-only probe sees
// it, WAL contents included.
type storeSpaceStats struct {
	pageSize      uint64
	pageCount     uint64
	freelistPages uint64
	mainBytes     uint64 // main database file size
	walBytes      uint64 // -wal file size; 0 when absent
}

func (s storeSpaceStats) livePages() uint64 {
	if s.freelistPages >= s.pageCount {
		return 0
	}
	return s.pageCount - s.freelistPages
}

// readStoreSpaceStats reads page counts through the probe's read-only URI, so
// nothing about the store changes.
func readStoreSpaceStats(probe storeProbe) (storeSpaceStats, error) {
	var stats storeSpaceStats
	db, err := openSQLiteStoreDB(probe.uri)
	if err != nil {
		return stats, err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, p := range []struct {
		pragma string
		out    *uint64
	}{
		{"page_size", &stats.pageSize},
		{"page_count", &stats.pageCount},
		{"freelist_count", &stats.freelistPages},
	} {
		if err := db.QueryRow("PRAGMA " + p.pragma).Scan(p.out); err != nil {
			return stats, fmt.Errorf("PRAGMA %s: %w", p.pragma, err)
		}
	}
	info, err := os.Stat(probe.filename)
	if err != nil {
		return stats, err
	}
	stats.mainBytes = uint64(info.Size())
	if info, err := os.Stat(probe.filename + "-wal"); err == nil {
		stats.walBytes = uint64(info.Size())
	} else if !os.IsNotExist(err) {
		return stats, err
	}
	return stats, nil
}

// upgradeSpaceEstimate is how many bytes an in-place upgrade may claim on the
// store's volume before its first checkpoint completes.
type upgradeSpaceEstimate struct {
	// WALPeak is the -wal file the transition can grow: one frame per page
	// it writes, since automatic checkpoints are off during Open.
	WALPeak uint64
	// Growth is how far the main file may extend once those frames are
	// checkpointed, after the free list has absorbed what it can.
	Growth uint64
	// Headroom is the fixed allowance for everything the estimate does not
	// model.
	Headroom uint64
	// Need is the sum, and the figure the precheck compares with the free
	// space on the volume.
	Need uint64
}

// estimateUpgradeSpace sizes a pending transition from the store's live
// pages. An additive-only transition (every step preserves analysis) writes
// a fixed share of the live pages per step; anything else is assumed to
// rewrite the whole live store.
func estimateUpgradeSpace(stats storeSpaceStats, pending []schemaMigration) upgradeSpaceEstimate {
	percent := uint64(upgradeRewritePercent)
	if additiveOnly(pending) {
		percent = min(uint64(len(pending))*upgradeAdditiveStepPercent, upgradeRewritePercent)
	}
	written := (stats.livePages()*percent + 99) / 100
	est := upgradeSpaceEstimate{Headroom: upgradeSpaceHeadroom}
	if written > 0 {
		est.WALPeak = walFileHeaderBytes + written*(stats.pageSize+walFrameHeaderBytes)
	}
	if free := stats.freelistPages; written > free {
		est.Growth = (written - free) * stats.pageSize
	}
	est.Need = est.WALPeak + est.Growth + est.Headroom
	return est
}

func additiveOnly(pending []schemaMigration) bool {
	if len(pending) == 0 {
		return false
	}
	for _, step := range pending {
		if step.inPlace == nil || step.rebuild || !step.preserveAnalysis {
			return false
		}
	}
	return true
}

// upgradeSpace is the free-space picture of one pending in-place upgrade.
type upgradeSpace struct {
	path      string
	from, to  int
	stats     storeSpaceStats
	estimate  upgradeSpaceEstimate
	available uint64
	// measured is false when the volume's free space could not be read; the
	// upgrade then proceeds unchecked rather than bricking startup on a
	// filesystem statfs does not understand.
	measured bool
}

// measureUpgradeSpace sizes the in-place upgrade plan would run on the probed
// store. It reports false when there is nothing to size: an in-memory or new
// store, a current or pre-versioning one, a refused version, or a rebuild
// (which deletes the file instead of growing it).
func measureUpgradeSpace(probe storeProbe, current int, plan schemaPlan, diskAvail func(string) (uint64, error)) (upgradeSpace, bool) {
	if !probe.exists || probe.filename == "" || probe.stored <= 0 || plan.err != nil || plan.wipe || len(plan.inPlace) == 0 {
		return upgradeSpace{}, false
	}
	space := upgradeSpace{path: probe.filename, from: probe.stored, to: current}
	stats, err := readStoreSpaceStats(probe)
	if err != nil {
		log.Printf("store_sqlite: upgrade space check skipped path=%s from=v%d to=v%d reason=%q", space.path, space.from, space.to, err)
		return space, true
	}
	space.stats = stats
	space.estimate = estimateUpgradeSpace(stats, plan.inPlace)
	if diskAvail == nil {
		diskAvail = platform.DiskAvailBytes
	}
	available, err := diskAvail(filepath.Dir(probe.filename))
	if err != nil {
		log.Printf("store_sqlite: upgrade space check skipped path=%s from=v%d to=v%d need=%d reason=%q", space.path, space.from, space.to, space.estimate.Need, err)
		return space, true
	}
	space.available, space.measured = available, true
	return space, true
}

// refuseShort returns the typed refusal when the volume measurably cannot
// hold the upgrade.
func (s upgradeSpace) refuseShort() error {
	if !s.measured || s.available >= s.estimate.Need {
		return nil
	}
	return &InsufficientUpgradeSpaceError{Path: s.path, From: s.from, To: s.to, Need: s.estimate.Need, Available: s.available}
}
