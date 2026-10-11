package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// RepoHasLanguageContext answers the existence question with the same node
// exclusions as RepoLanguageFileCounts, without grouping every file/language.
// Cancellation and read errors are distinct from an absent language.
func (s *Store) RepoHasLanguageContext(ctx context.Context, repoPrefix, language string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if language == "" {
		return false, nil
	}
	var present bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM nodes
 WHERE repo_prefix = ? AND language = ? AND kind <> ?
   AND (kind <> ? OR data_class IS NOT 'content') AND view_gen = ?
 )`, repoPrefix, language, string(graph.KindModule), string(graph.KindDoc), s.viewGen).Scan(&present)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return present, nil
}
