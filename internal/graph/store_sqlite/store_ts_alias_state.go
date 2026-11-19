package store_sqlite

import (
	"context"
	"database/sql"
)

// TSAliasDigest is the alias resolution policy applied to this generation's
// callers. Empty means unrecorded or an interrupted rebind.
func (s *Store) TSAliasDigest(repo string) (string, error) {
	var digest string
	err := s.db.QueryRow(`SELECT digest FROM ts_alias_state WHERE view_gen=? AND repo_prefix=?`, s.viewGen, repo).Scan(&digest)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return digest, err
}

func (s *Store) SetTSAliasDigest(repo, digest string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(context.Background(), `INSERT OR REPLACE INTO ts_alias_state (view_gen, repo_prefix, digest) VALUES (?, ?, ?)`, s.viewGen, repo, digest)
	return err
}

// MayHaveTSAliasCallers includes every generation: a hit in a masked or
// unrelated layer only causes extra config discovery. A miss proves no
// named JS/TS caller in this repository can inherit a different binding.
func (s *Store) MayHaveTSAliasCallers(ctx context.Context, repo string) (bool, error) {
	var present bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes
		WHERE repo_prefix=? AND name<>'' AND language IN
		('javascript','typescript','jsx','tsx','vue','svelte','astro'))`, repo).Scan(&present)
	return present, err
}
