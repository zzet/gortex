package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
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

// TSAliasInputs returns the config inputs recorded beside the digest:
// repo-relative slash paths mapped to a hex content hash, empty for a probe
// that was missing. Nil means none were recorded. The set is only a hint: a
// reader must check that it still reproduces TSAliasDigest before trusting it.
func (s *Store) TSAliasInputs(repo string) (map[string]string, error) {
	var raw string
	err := s.db.QueryRow(`SELECT inputs FROM ts_alias_inputs WHERE view_gen=? AND repo_prefix=?`, s.viewGen, repo).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var inputs map[string]string
	if err := json.Unmarshal([]byte(raw), &inputs); err != nil {
		return nil, nil // an unreadable hint only costs a rediscovery
	}
	return inputs, nil
}

// SetTSAliasInputs records the config inputs of the digest about to be
// certified. Callers write it before SetTSAliasDigest; a crash in between
// leaves the digest cleared, so the stale set is never trusted.
func (s *Store) SetTSAliasInputs(repo string, inputs map[string]string) error {
	raw, err := json.Marshal(inputs)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.execActiveWriteLocked(context.Background(), `INSERT OR REPLACE INTO ts_alias_inputs (view_gen, repo_prefix, inputs) VALUES (?, ?, ?)`, s.viewGen, repo, string(raw))
	return err
}

// MayHaveTSAliasCallers includes every generation: a hit in a masked or
// unrelated layer only causes extra config discovery. A miss proves no
// named JS/TS caller in this repository can inherit a different binding.
//
// Keep this SQL language list in sync with indexer.closureJSTSPath
// (internal/indexer/builder_closure.go) and resolver.isJSTSPath
// (internal/resolver/jsts_imports.go); drift is guarded by
// TestTSAliasDiscovery_MayHaveCallersMatchesClosureJSTSPath.
func (s *Store) MayHaveTSAliasCallers(ctx context.Context, repo string) (bool, error) {
	var present bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes
		WHERE repo_prefix=? AND name<>'' AND language IN
		('javascript','typescript','jsx','tsx','vue','svelte','astro'))`, repo).Scan(&present)
	return present, err
}
