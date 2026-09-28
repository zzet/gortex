package store_sqlite

import "context"

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
