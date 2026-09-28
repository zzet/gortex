package store_sqlite

import (
	"database/sql"
	"sync/atomic"
)

// AnalysisMutationRevision is a process-local graph mutation clock. Durable
// restart correctness comes from clearing the active generation pointer before
// a committed graph mutation can become visible.
//
// On an ordinary handle it is the whole store's clock: every committed write,
// in any generation, moves it. On a handle from ViewScopedAnalysis it is the
// clock of the handle's view generation (AnalysisViewRevision), and every
// revision check of the analysis protocol on that handle (begin, append, seal,
// activate, CommitAnalysisSnapshot, the loaded header's GraphRevision) uses the
// same clock.
func (s *Store) AnalysisMutationRevision() uint64 {
	return s.analysisRevision()
}

// AnalysisStoreRevision is the whole store's mutation clock, whatever the
// handle.
func (s *Store) AnalysisStoreRevision() uint64 {
	return s.analysisMutationRevision.Load()
}

// AnalysisViewRevision is the mutation clock of this handle's view
// generation: it moves on a committed write through a handle on that
// generation and on every write to the base generation (0), which every view
// composes over; a write to any other generation leaves it unchanged. A
// worktree's edit writes only its own derived generations, so the base view's
// analysis stays current through it.
func (s *Store) AnalysisViewRevision() uint64 {
	if s.coreless() {
		return 0
	}
	r := s.analysisViewCounter(s.viewGen).Load()
	if s.viewGen != baseViewGeneration {
		r += s.analysisViewCounter(baseViewGeneration).Load()
	}
	return r
}

// ViewScopedAnalysis returns a handle on the same view whose analysis
// protocol is keyed on AnalysisViewRevision instead of the whole store's
// clock (see AnalysisMutationRevision). The analysis pass of a view uses it as
// its writer so a write to another generation neither supersedes a pass in
// flight nor makes the installed snapshot stale.
func (s *Store) ViewScopedAnalysis() *Store {
	h := *s
	h.analysisViewScoped = true
	h.ownsCore = false // a derived handle never tears the core down
	return &h
}

func (s *Store) analysisRevision() uint64 {
	if s.analysisViewScoped {
		return s.AnalysisViewRevision()
	}
	return s.analysisMutationRevision.Load()
}

func (s *Store) analysisViewCounter(g int64) *atomic.Uint64 {
	if v, ok := s.analysisViewRevisions.Load(g); ok {
		return v.(*atomic.Uint64)
	}
	v, _ := s.analysisViewRevisions.LoadOrStore(g, &atomic.Uint64{})
	return v.(*atomic.Uint64)
}

// bumpAnalysisViewRevision advances generation g's view clock (and the whole
// store's) for a committed write made outside a handle on g. writeMu must be
// held.
func (s *Store) bumpAnalysisViewRevision(g int64) {
	s.analysisMutationRevision.Add(1)
	s.analysisViewCounter(g).Add(1)
	s.edgeMutationRevision.Add(1)
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
	if s.analysisRevision() != expectedRevision {
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
	if err := s.invalidateAnalysisViewTx(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	s.analysisGenerationPresent = s.analysisLatchRemaining
	return nil
}

// invalidateAnalysisViewTx is the durable invalidation for a write through
// this handle, scoped like AnalysisViewRevision: a write at the base
// generation stales every view's analysis (every view composes over the
// base); a write at a derived generation stales only that generation's
// analyses — its active pointer and its building rows — and leaves every
// other view's standing, since none of their rows moved. It records in
// analysisLatchRemaining whether any pointer or building analysis remains, the
// value the caller stores in the mutation latch after its commit (clearing the
// latch while another view still holds a pointer would let the next write skip
// its invalidation). writeMu must be held.
func (s *Store) invalidateAnalysisViewTx(tx *sql.Tx) error {
	if s.viewGen <= baseViewGeneration {
		s.analysisLatchRemaining = false
		return invalidateAnalysisGenerationTx(tx)
	}
	if _, err := tx.Exec(`UPDATE analysis_generations SET state = ? WHERE view_gen = ? AND (state = ? OR generation_id IN (SELECT generation_id FROM analysis_active_generation WHERE view_gen = ?))`,
		analysisGenerationStale, s.viewGen, analysisGenerationBuilding, s.viewGen); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM analysis_active_generation WHERE view_gen = ?`, s.viewGen); err != nil {
		return err
	}
	var remaining int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM analysis_active_generation LIMIT 1)
		OR EXISTS(SELECT 1 FROM analysis_generations WHERE state = ? LIMIT 1)`, analysisGenerationBuilding).Scan(&remaining); err != nil {
		return err
	}
	s.analysisLatchRemaining = remaining != 0
	return nil
}

// invalidateAnalysisGenerationTx performs the durable half of analysis
// invalidation inside a caller-owned transaction. Keeping this separate lets
// SQLite-native topology rewrites invalidate the active snapshot and mutate
// graph rows atomically on one pinned connection. That is required when the
// pool has MaxOpenConns=1: checking out a second transaction while a pinned
// connection is held would otherwise deadlock.
//
// This is the unscoped form: every view's pointer and building rows. Writes
// through a store handle use invalidateAnalysisViewTx, which is this for the
// base generation and the view's own rows otherwise; the in-memory clocks
// follow the same scope (AnalysisViewRevision), and the whole-store clock still
// moves on every write for callers that keep it.
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
		s.analysisViewCounter(s.viewGen).Add(1)
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
