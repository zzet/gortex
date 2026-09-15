package store_sqlite

import (
	"database/sql"
)

// AnalysisMutationRevision is a process-local graph mutation clock. Durable
// restart correctness comes from clearing the active generation pointer before
// a committed graph mutation can become visible.
func (s *Store) AnalysisMutationRevision() uint64 {
	return s.analysisMutationRevision.Load()
}

// initAnalysisGenerationState makes interrupted builders collectible and
// initializes the mutation hot-path latch from the active singleton.
func (s *Store) initAnalysisGenerationState() error {
	if _, err := s.writerDB.Exec(`UPDATE analysis_generations SET state = ? WHERE state = ?`, analysisGenerationStale, analysisGenerationBuilding); err != nil {
		return err
	}
	var present int
	if err := s.writerDB.QueryRow(`SELECT EXISTS(SELECT 1 FROM analysis_active_generation LIMIT 1)`).Scan(&present); err != nil {
		return err
	}
	s.analysisGenerationPresent = present != 0
	return nil
}

// CommitAnalysisSnapshot closes the revision-check-to-install race by holding
// the graph mutation gate across both operations. install must only publish
// in-memory pointers/tokens and must not re-enter graph mutation methods.
func (s *Store) CommitAnalysisSnapshot(expectedRevision uint64, install func()) bool {
	if install == nil {
		return false
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.analysisMutationRevision.Load() != expectedRevision {
		return false
	}
	install()
	return true
}

// invalidateAnalysisGenerationLocked commits durable invalidation before its
// caller mutates nodes or edges. Building generations are made collectible;
// the active singleton is cleared and its generation marked stale. A crash can
// therefore only lose an optimization, never resurrect stale analysis.
// writeMu must be held.
func (s *Store) invalidateAnalysisGenerationLocked() error {
	if !s.analysisGenerationPresent {
		return nil
	}
	tx, err := s.beginAnalysisWrite()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := invalidateAnalysisGenerationTx(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	s.analysisGenerationPresent = false
	return nil
}

// invalidateAnalysisGenerationTx performs the durable half of analysis
// invalidation inside a caller-owned transaction. Keeping this separate lets
// SQLite-native topology rewrites invalidate the active snapshot and mutate
// graph rows atomically on one pinned connection. That is required when the
// pool has MaxOpenConns=1: checking out a second transaction while a pinned
// connection is held would otherwise deadlock.
//
// Deliberately NOT scoped to the mutating handle's view generation, even
// though the cache now carries one. The revision this fires from,
// analysisMutationRevision, is one counter for the whole storeCore, so an
// in-flight build in any view is already rejected by a mutation in any other;
// clearing every view's pointer keeps the durable state matching that counter.
// Scoping it would be the unsound direction — it can only leave a pointer
// standing that the revision check has already invalidated in memory.
func invalidateAnalysisGenerationTx(tx *sql.Tx) error {
	if _, err := tx.Exec(`UPDATE analysis_generations SET state = ? WHERE state = ? OR generation_id IN (SELECT generation_id FROM analysis_active_generation)`, analysisGenerationStale, analysisGenerationBuilding); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM analysis_active_generation`)
	return err
}

// finishAnalysisMutationLocked advances the in-process race detector only
// after a graph mutation committed. writeMu must be held.
func (s *Store) finishAnalysisMutationLocked(changed bool) {
	if changed {
		s.analysisMutationRevision.Add(1)
		// Resolver liveness snapshots need one cheap process-local token for
		// every committed edge-state change. This hook is intentionally coarse:
		// node-only changes are safe false positives, while centralising the
		// bump here covers reindex, attribute, eviction, contract replacement,
		// and other edge mutation families without N per-row atomics.
		s.edgeMutationRevision.Add(1)
	}
}

// invalidateAnalysisBeforeMutationLocked is the common fail-closed gate for
// graph writes. If durable invalidation fails, callers must not apply the
// mutation: doing so could make stale analysis look valid after restart.
func (s *Store) invalidateAnalysisBeforeMutationLocked() bool {
	if err := s.invalidateAnalysisGenerationLocked(); err != nil {
		panicOnFatal(err)
		return false
	}
	return true
}
