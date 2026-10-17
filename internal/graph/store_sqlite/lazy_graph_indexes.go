package store_sqlite

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// edges_by_file_generation serves the by-recording-file reads of one
// generation (EdgeEndpointsRecordedAt, RecordedEdgesAt). edges_by_file
// (file_path, kind) carries no generation, so on a store that keeps many
// generations those reads visit a file's whole history: on the live store
// config.go's rows span 41 generations (53,336 rows) for the ~1,400 one
// generation holds, and a composed view pays that once per level.
//
// The index is additive, optional and existence-detected; it is not part of
// the schema version (it stays 29). An older binary opening a store that has
// it simply carries an extra index SQLite maintains on every write, so a
// binary rollback stays possible. It is NOT built inside Open: on a store of
// millions of edges a CREATE INDEX takes minutes, and Open gates the daemon's
// readiness. A store without it gets it from the lazy builder below, on the
// maintenance lane, after open; until then the readers use the legacy
// edges_by_file plan. A cold
// bulk window drops it with the other dense indexes and the builder restores
// it after the window closes.
const (
	edgesByFileGenerationIndexName        = "edges_by_file_generation"
	edgesByFileGenerationIndexDDL         = `CREATE INDEX IF NOT EXISTS edges_by_file_generation ON edges(file_path, view_gen)`
	edgesByFileGenerationIndexPresenceSQL = `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?)`
)

// maintenanceLazyIndex is the lane job kind of the lazy index build.
const maintenanceLazyIndex maintenanceJob = "lazy_index"

// lazyIndexState values for storeCore.fileGenerationIndex.
const (
	lazyIndexUnknown int32 = iota
	lazyIndexPresent
)

// Vars, not consts, so the in-package cases can shorten the cadence.
var (
	lazyIndexInitialDelay = 30 * time.Second
	lazyIndexPollInterval = 60 * time.Second
	lazyIndexHeartbeat    = 15 * time.Second
)

// lazyGraphIndexesEnabled is the kill switch: GORTEX_SQLITE_LAZY_INDEXES=off
// (or 0/false/no) never builds the index; the readers keep the legacy plan.
func lazyGraphIndexesEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_SQLITE_LAZY_INDEXES"))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// fileGenerationIndexPresent reports whether edges_by_file_generation exists.
// Presence is cached once seen (only a cold bulk window drops the index, and
// it clears the cache first); absence is re-probed per call (one catalog
// lookup) so readers switch to the index as soon as the builder commits it.
func (s *Store) fileGenerationIndexPresent() bool {
	if s.coreless() {
		return false
	}
	if s.fileGenerationIndex.Load() == lazyIndexPresent {
		return true
	}
	var present bool
	if err := s.db.QueryRow(edgesByFileGenerationIndexPresenceSQL,
		edgesByFileGenerationIndexName).Scan(&present); err != nil || !present {
		return false
	}
	s.fileGenerationIndex.Store(lazyIndexPresent)
	return true
}

// The builder already owns writeMu. Its metadata probe must use the writer
// pool too: long readers can occupy every s.db connection and otherwise keep
// the application's writer parked before any index work starts.
func (s *Store) fileGenerationIndexPresentLocked(ctx context.Context) (bool, error) {
	if s.fileGenerationIndex.Load() == lazyIndexPresent {
		return true, nil
	}
	var present bool
	if err := s.writerDB.QueryRowContext(ctx, edgesByFileGenerationIndexPresenceSQL,
		edgesByFileGenerationIndexName).Scan(&present); err != nil {
		return false, fmt.Errorf("probe lazy index %s: %w", edgesByFileGenerationIndexName, err)
	}
	if present {
		s.fileGenerationIndex.Store(lazyIndexPresent)
	}
	return present, nil
}

// forgetFileGenerationIndex clears the presence cache before the index is
// dropped, and after a reader found it missing.
func (s *Store) forgetFileGenerationIndex() {
	if !s.coreless() {
		s.fileGenerationIndex.Store(lazyIndexUnknown)
	}
}

// isNoSuchIndexErr reports an INDEXED BY naming an index that is gone (a cold
// bulk window dropped it between a reader's probe and its statement).
func isNoSuchIndexErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such index")
}

// LazyIndexStats is the lazy builder's telemetry.
type LazyIndexStats struct {
	Present   bool
	Builds    int64
	Deferrals int64
	Failures  int64
	LastBuild time.Duration
	LastError string
}

type lazyIndexCounters struct {
	builds, deferrals, failures atomic.Int64
	lastBuildNanos              atomic.Int64
	lastError                   atomic.Value // string
}

// LazyIndexStats reports whether edges_by_file_generation is present and what
// the builder did.
func (s *Store) LazyIndexStats() LazyIndexStats {
	if s.coreless() {
		return LazyIndexStats{}
	}
	st := LazyIndexStats{
		Present:   s.fileGenerationIndexPresent(),
		Builds:    s.lazyIndex.builds.Load(),
		Deferrals: s.lazyIndex.deferrals.Load(),
		Failures:  s.lazyIndex.failures.Load(),
		LastBuild: time.Duration(s.lazyIndex.lastBuildNanos.Load()),
	}
	if msg, ok := s.lazyIndex.lastError.Load().(string); ok {
		st.LastError = msg
	}
	return st
}

// startLazyIndexBuilder runs the builder beside the checkpoint loop; the
// returned join waits for it after stopCheckpoint closes.
func (s *Store) startLazyIndexBuilder() (join func()) {
	if s.coreless() || s.stopCheckpoint == nil || s.db == s.writerDB || !lazyGraphIndexesEnabled() {
		return func() {}
	}
	done := make(chan struct{})
	go s.runLazyIndexBuilder(done)
	return func() { <-done }
}

func (s *Store) runLazyIndexBuilder(done chan<- struct{}) {
	defer close(done)
	wait := lazyIndexInitialDelay
	for {
		timer := time.NewTimer(wait)
		select {
		case <-s.stopCheckpoint:
			timer.Stop()
			return
		case <-timer.C:
		}
		wait = lazyIndexPollInterval
		if rowCountersEnabled() && !s.rowCountersReady.Load() {
			// The writer-maintained row counts ride the same loop: installed
			// and seeded once, then verified at each open.
			if err := s.ensureRowCountersUntilStopped(); err != nil {
				log.Printf("store_sqlite: row counters deferred error=%q next_attempt_in=%s", err, wait)
			}
		}
		if s.fileGenerationIndexPresent() {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		stop := make(chan struct{})
		go func() {
			select {
			case <-s.stopCheckpoint:
				cancel()
			case <-stop:
			}
		}()
		err := s.buildLazyIndexOnce(ctx)
		close(stop)
		cancel()
		if err != nil {
			log.Printf("store_sqlite: lazy index %s deferred error=%q next_attempt_in=%s", edgesByFileGenerationIndexName, err, wait)
		}
	}
}

// buildLazyIndexOnce builds edges_by_file_generation once, on the maintenance
// lane (after the in-flight publishes and builds, so an interactive build is
// not stalled behind it mid-write), holding the application writer. It skips
// while a bulk window is open. The CREATE INDEX holds SQLite's write lock for
// its whole duration; that is the one-time cost, logged with a heartbeat.
func (s *Store) buildLazyIndexOnce(ctx context.Context) error {
	err := s.runMaintenance(ctx, maintenanceLazyIndex, true, func(ctx context.Context) error {
		if err := s.writeMu.LockContext(ctx); err != nil {
			return err
		}
		defer s.writeMu.Unlock()
		if s.bulkConn != nil {
			return fmt.Errorf("bulk window open")
		}
		present, err := s.fileGenerationIndexPresentLocked(ctx)
		if err != nil {
			return err
		}
		if present {
			return nil
		}
		started := time.Now()
		log.Printf("store_sqlite: lazy index %s build started", edgesByFileGenerationIndexName)
		beat := make(chan struct{})
		go func() {
			ticker := time.NewTicker(lazyIndexHeartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-beat:
					return
				case <-ticker.C:
					log.Printf("store_sqlite: lazy index %s building elapsed=%s", edgesByFileGenerationIndexName, time.Since(started).Round(time.Second))
				}
			}
		}()
		_, err = s.writerDB.ExecContext(ctx, edgesByFileGenerationIndexDDL)
		close(beat)
		elapsed := time.Since(started)
		if err != nil {
			s.lazyIndex.failures.Add(1)
			s.lazyIndex.lastError.Store(err.Error())
			log.Printf("store_sqlite: lazy index %s build failed elapsed=%s error=%q", edgesByFileGenerationIndexName, elapsed, err)
			return err
		}
		s.lazyIndex.builds.Add(1)
		s.lazyIndex.lastBuildNanos.Store(int64(elapsed))
		s.fileGenerationIndex.Store(lazyIndexPresent)
		log.Printf("store_sqlite: lazy index %s built elapsed=%s", edgesByFileGenerationIndexName, elapsed)
		return nil
	})
	if err != nil {
		s.lazyIndex.deferrals.Add(1)
	}
	return err
}
