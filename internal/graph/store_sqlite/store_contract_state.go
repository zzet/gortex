package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion that the SQLite Store persists the contract-tier
// completion marker. It lives in the same backend as the graph it describes,
// so a warm restart can tell an unbuilt contract tier (the tail that commits
// it was lost, and per-file mtime admission will never re-extract it) from a
// repo that genuinely declares no contracts.
var _ graph.ContractStateStore = (*Store)(nil)

// SetContractState upserts the completion marker for one repo — written when
// a whole-repo contract pass finishes committing. One row per repo_prefix.
func (s *Store) SetContractState(st graph.ContractState) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(context.Background(), `
INSERT OR REPLACE INTO contract_state
  (view_gen, repo_prefix, indexed_sha, completed_at, contract_count)
VALUES (?, ?, ?, ?, ?)`,
		s.viewGen, st.RepoPrefix, st.IndexedSHA, st.CompletedAt, st.ContractCount)
	return err
}

// GetContractState returns the recorded completion marker for a repo. The
// bool is false when no row exists yet — the repo's contract pass never
// completed against this store, or the store predates the marker.
func (s *Store) GetContractState(repoPrefix string) (graph.ContractState, bool, error) {
	row := s.db.QueryRow(`
SELECT indexed_sha, completed_at, contract_count
  FROM contract_state WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix)
	st := graph.ContractState{RepoPrefix: repoPrefix}
	err := row.Scan(&st.IndexedSHA, &st.CompletedAt, &st.ContractCount)
	if err == sql.ErrNoRows {
		return graph.ContractState{RepoPrefix: repoPrefix}, false, nil
	}
	if err != nil {
		return graph.ContractState{RepoPrefix: repoPrefix}, false, err
	}
	return st, true, nil
}
