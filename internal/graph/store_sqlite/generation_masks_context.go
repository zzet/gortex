package store_sqlite

import (
	"context"
	"slices"
)

// FileMasksContext returns every file-level claim this generation makes,
// ordered by the primary key. Cancellation interrupts both connection-pool
// admission and an in-flight SQLite scan.
func (s *Store) FileMasksContext(ctx context.Context) ([]FileMask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT repo_prefix, file_path, ownership_mode
  FROM generation_file_masks WHERE view_gen = ?
 ORDER BY repo_prefix, file_path`, s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileMask{}
	for rows.Next() {
		var mask FileMask
		var mode string
		if err := rows.Scan(&mask.RepoPrefix, &mask.FilePath, &mode); err != nil {
			return nil, err
		}
		mask.Mode = OwnershipMode(mode)
		out = append(out, mask)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// FileMasksForContext reads only the requested repository/path keys in this
// physical generation. Chunked primary-key probes preserve point-scan scope,
// while cancellation also interrupts connection admission and SQLite work.
// A duplicate or absent key adds no row; any failure discards all partial rows.
func (s *Store) FileMasksForContext(ctx context.Context, repo string, paths []string) ([]FileMask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wanted := slices.Clone(paths)
	slices.Sort(wanted)
	wanted = slices.Compact(wanted)
	var out []FileMask
	for start := 0; start < len(wanted); start += lookupChunkSize {
		chunk := wanted[start:min(start+lookupChunkSize, len(wanted))]
		args := []any{s.viewGen, repo}
		args = append(args, toAnyArgs(chunk)...)
		part, err := func() ([]FileMask, error) {
			rows, err := s.db.QueryContext(ctx, `SELECT file_path,ownership_mode FROM generation_file_masks WHERE view_gen=? AND repo_prefix=? AND file_path IN (`+inPlaceholders(len(chunk))+`) ORDER BY file_path`, args...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var found []FileMask
			for rows.Next() {
				row := FileMask{RepoPrefix: repo}
				var mode string
				if err := rows.Scan(&row.FilePath, &mode); err != nil {
					return nil, err
				}
				row.Mode = OwnershipMode(mode)
				found = append(found, row)
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			return found, nil
		}()
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// EdgeSourceMasksContext returns every edge-set replacement marker this
// generation carries. Cancellation interrupts both connection-pool admission
// and an in-flight SQLite scan.
func (s *Store) EdgeSourceMasksContext(ctx context.Context) ([]EdgeSourceMask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT source_id, ownership_mode FROM generation_edge_sources WHERE view_gen = ? ORDER BY source_id`,
		s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EdgeSourceMask{}
	for rows.Next() {
		var mask EdgeSourceMask
		var mode string
		if err := rows.Scan(&mask.SourceID, &mode); err != nil {
			return nil, err
		}
		mask.Mode = OwnershipMode(mode)
		out = append(out, mask)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
