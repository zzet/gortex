package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// SetRepoIndexState upserts the freshness-provenance row for one repo —
// written at the end of every (re)index. One row per repo_prefix.
func (s *Store) SetRepoIndexState(st graph.RepoIndexState) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	dirty := 0
	if st.Dirty {
		dirty = 1
	}
	_, err := s.execActiveWriteLocked(context.Background(), `
INSERT OR REPLACE INTO repo_index_state
  (view_gen, repo_prefix, indexed_sha, dirty, indexed_at, workspace_fp, node_count, edge_count, extractor_versions)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.viewGen, st.RepoPrefix, st.IndexedSHA, dirty, st.IndexedAt, st.WorkspaceFP,
		st.NodeCount, st.EdgeCount, st.ExtractorVersions)
	return err
}

// GetRepoIndexState returns the recorded freshness provenance for a repo.
// The bool is false when no row exists yet (never-indexed / pre-feature).
func (s *Store) GetRepoIndexState(repoPrefix string) (graph.RepoIndexState, bool, error) {
	row := s.db.QueryRow(`
SELECT indexed_sha, dirty, indexed_at, workspace_fp, node_count, edge_count, extractor_versions
  FROM repo_index_state WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix)
	st := graph.RepoIndexState{RepoPrefix: repoPrefix}
	var dirty int
	err := row.Scan(&st.IndexedSHA, &dirty, &st.IndexedAt, &st.WorkspaceFP,
		&st.NodeCount, &st.EdgeCount, &st.ExtractorVersions)
	if err == sql.ErrNoRows {
		return graph.RepoIndexState{RepoPrefix: repoPrefix}, false, nil
	}
	if err != nil {
		return graph.RepoIndexState{RepoPrefix: repoPrefix}, false, err
	}
	st.Dirty = dirty != 0
	return st, true, nil
}
