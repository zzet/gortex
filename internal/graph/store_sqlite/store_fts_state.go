package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.SymbolFTSNormalizationState = (*Store)(nil)

// GetSymbolFTSNormalization returns the mode that produced repoPrefix's
// durable symbol FTS corpus. A missing row means the historical unnormalised
// mode; callers still receive ok=false so an enabled mode can trigger its
// one-time authoritative rebuild.
func (s *Store) GetSymbolFTSNormalization(repoPrefix string) (string, bool, error) {
	var mode string
	err := s.db.QueryRow(`SELECT normalization FROM symbol_fts_state WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix).Scan(&mode)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return mode, true, nil
}

// SetSymbolFTSNormalization advances repoPrefix's marker after the caller has
// committed an authoritative corpus replacement. Keeping this write separate
// and ordered after replacement is deliberately fail-closed: a crash between
// them leaves the old marker and makes the next warm reconcile repeat the
// idempotent rebuild.
func (s *Store) SetSymbolFTSNormalization(repoPrefix, mode string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(context.Background(), `
INSERT INTO symbol_fts_state (view_gen, repo_prefix, normalization) VALUES (?, ?, ?)
ON CONFLICT(view_gen, repo_prefix) DO UPDATE SET normalization = excluded.normalization`, s.viewGen, repoPrefix, mode)
	return err
}
