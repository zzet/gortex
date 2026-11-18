package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

func loadRefFactsByTargetsContextQuery(targetCount int) string {
	const cols = `from_id, to_id, kind, ref_name, line, origin, tier, candidates, file_path, lang`
	stmt := make([]byte, 0, 192+targetCount*4)
	stmt = append(stmt, "SELECT "+cols+" FROM ref_facts INDEXED BY ref_facts_by_target WHERE view_gen = ? AND repo_prefix = ? AND to_id IN ("...)
	for i := 0; i < targetCount; i++ {
		if i > 0 {
			stmt = append(stmt, ',')
		}
		stmt = append(stmt, '?')
	}
	stmt = append(stmt, ')')
	return string(stmt)
}

// LoadRefFactsByTargetsContext is the cancellable sibling of
// LoadRefFactsByTargets. It preserves the legacy query, chunking, grouping,
// duplicate-target and generation semantics. Result ordering remains
// unspecified, as in the legacy query, which has no ORDER BY clause.
func (s *Store) LoadRefFactsByTargetsContext(
	ctx context.Context, repoPrefix string, targetIDs []string,
) (map[string][]graph.RefFact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := map[string][]graph.RefFact{}
	if len(targetIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(targetIDs); start += refFactChunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + refFactChunk
		if end > len(targetIDs) {
			end = len(targetIDs)
		}
		chunk := targetIDs[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.viewGen, repoPrefix)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, loadRefFactsByTargetsContextQuery(len(chunk)), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				_ = rows.Close()
				return nil, err
			}
			var f graph.RefFact
			var cand string
			if err := rows.Scan(&f.FromID, &f.ToID, &f.Kind, &f.RefName, &f.Line, &f.Origin, &f.Tier, &cand, &f.FilePath, &f.Lang); err != nil {
				_ = rows.Close()
				return nil, err
			}
			f.RepoPrefix = repoPrefix
			f.Candidates = decodeCandidates(cand)
			out[f.FilePath] = append(out[f.FilePath], f)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
