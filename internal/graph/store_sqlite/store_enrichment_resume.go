package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// The resume point lives beside the graph rows it describes, so it is
// committed by the same writer, after the page writes it vouches for.
var _ graph.EnrichmentResumeStore = (*Store)(nil)

// SetEnrichmentResume upserts the resume point for one (repo, provider).
func (s *Store) SetEnrichmentResume(p graph.EnrichmentResumePoint) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(context.Background(), `
INSERT OR REPLACE INTO enrichment_resume
  (view_gen, repo_prefix, provider, indexed_sha, version, phase, after_path, covered, ledger_digest, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.viewGen, p.RepoPrefix, p.Provider, p.IndexedSHA, p.Version, p.Phase, p.After, p.Covered, p.LedgerDigest, p.UpdatedAt)
	return err
}

// GetEnrichmentResume returns the recorded resume point for a (repo,
// provider). The bool is false when none is recorded.
func (s *Store) GetEnrichmentResume(repoPrefix, provider string) (graph.EnrichmentResumePoint, bool, error) {
	row := s.db.QueryRow(`
SELECT indexed_sha, version, phase, after_path, covered, ledger_digest, updated_at
  FROM enrichment_resume WHERE view_gen = ? AND repo_prefix = ? AND provider = ?`, s.viewGen, repoPrefix, provider)
	p := graph.EnrichmentResumePoint{RepoPrefix: repoPrefix, Provider: provider}
	err := row.Scan(&p.IndexedSHA, &p.Version, &p.Phase, &p.After, &p.Covered, &p.LedgerDigest, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return graph.EnrichmentResumePoint{RepoPrefix: repoPrefix, Provider: provider}, false, nil
	}
	if err != nil {
		return graph.EnrichmentResumePoint{RepoPrefix: repoPrefix, Provider: provider}, false, err
	}
	return p, true, nil
}

// ClearEnrichmentResume deletes the resume point for a (repo, provider). A
// missing row is not an error.
func (s *Store) ClearEnrichmentResume(repoPrefix, provider string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(context.Background(), `
DELETE FROM enrichment_resume WHERE view_gen = ? AND repo_prefix = ? AND provider = ?`,
		s.viewGen, repoPrefix, provider)
	return err
}
