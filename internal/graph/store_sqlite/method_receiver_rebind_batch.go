package store_sqlite

import (
	"context"
	"fmt"
	"strings"
)

const goMethodReceiverFileTableSQL = `CREATE TEMP TABLE IF NOT EXISTS go_receiver_rebind_files (
    file_path TEXT PRIMARY KEY
) WITHOUT ROWID`

// The batch candidate query pairs generations the same way its single-file
// sibling does (see method_receiver_rebind.go): the edge, the owning method,
// the current target and the canonical replacement are all held to the writing
// handle's generation, and the target probe stays in the LEFT JOIN's ON clause.
const goMethodReceiverCandidatesForFilesSQL = `
SELECT e.id, MIN(c.id)
FROM temp.go_receiver_rebind_files AS f
CROSS JOIN nodes AS m INDEXED BY nodes_by_file
CROSS JOIN edges AS e INDEXED BY edges_by_from
LEFT JOIN nodes AS t ON t.id = e.to_id AND t.view_gen = ?
JOIN nodes AS c INDEXED BY nodes_go_receiver_type
  ON c.repo_prefix = m.repo_prefix
 AND c.file_dir = e.member_receiver_dir
 AND c.name = e.member_receiver
 AND c.view_gen = ?
WHERE m.file_path = f.file_path
  AND m.view_gen = ?
  AND e.from_id = m.id
  AND e.kind = 'member_of'
  AND e.view_gen = ?
  AND m.language = 'go'
  AND m.kind = 'method'
  AND e.member_receiver IS NOT NULL
  AND e.member_receiver_dir IS NOT NULL
  AND (t.id IS NULL OR t.kind NOT IN ('type', 'interface'))
  AND c.language = 'go'
  AND c.kind IN ('type', 'interface')
  AND c.name <> ''
  AND c.file_path <> ''
GROUP BY e.id
HAVING COUNT(*) = 1 AND MIN(c.id) <> e.to_id`

// RebindGoMethodReceiversForFiles repairs a deduped changed-file frontier with
// one indexed candidate query and one mutation transaction. File paths live in
// a TEMP table on the pinned connection, so the SQL stays bounded at SQLite's
// host-parameter limit without scanning the global member_of corpus.
func (s *Store) RebindGoMethodReceiversForFiles(filePaths []string) (changed int, err error) {
	seen := make(map[string]struct{}, len(filePaths))
	paths := make([]string, 0, len(filePaths))
	for _, filePath := range filePaths {
		if filePath == "" {
			continue
		}
		if _, duplicate := seen[filePath]; duplicate {
			continue
		}
		seen[filePath] = struct{}{}
		paths = append(paths, filePath)
	}
	if len(paths) == 0 {
		return 0, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx := context.Background()
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return 0, fmt.Errorf("sqlite receiver batch acquire writer connection: %w", err)
	}
	defer release()
	if _, err = conn.ExecContext(ctx, goMethodReceiverCandidateTableSQL); err != nil {
		return 0, fmt.Errorf("sqlite receiver batch create candidate table: %w", err)
	}
	if _, err = conn.ExecContext(ctx, goMethodReceiverFileTableSQL); err != nil {
		return 0, fmt.Errorf("sqlite receiver batch create file table: %w", err)
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM temp.go_receiver_rebind_candidates`); err != nil {
		return 0, fmt.Errorf("sqlite receiver batch clear candidate table: %w", err)
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM temp.go_receiver_rebind_files`); err != nil {
		return 0, fmt.Errorf("sqlite receiver batch clear file table: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `DELETE FROM temp.go_receiver_rebind_candidates`)
		_, _ = conn.ExecContext(context.Background(), `DELETE FROM temp.go_receiver_rebind_files`)
	}()

	for start := 0; start < len(paths); start += lookupChunkSize {
		end := minInt(start+lookupChunkSize, len(paths))
		chunk := paths[start:end]
		query := `INSERT OR IGNORE INTO temp.go_receiver_rebind_files (file_path) VALUES ` +
			strings.TrimSuffix(strings.Repeat("(?),", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, path := range chunk {
			args[i] = path
		}
		if _, err = conn.ExecContext(ctx, query, args...); err != nil {
			return 0, fmt.Errorf("sqlite receiver batch load file frontier: %w", err)
		}
	}

	result, err := conn.ExecContext(ctx,
		`INSERT INTO temp.go_receiver_rebind_candidates (edge_id, new_to) `+goMethodReceiverCandidatesForFilesSQL,
		s.viewGen, s.viewGen, s.viewGen, s.viewGen)
	if err != nil {
		return 0, fmt.Errorf("sqlite receiver batch collect candidates: %w", err)
	}
	candidates, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite receiver batch count candidates: %w", err)
	}
	if candidates == 0 {
		return 0, nil
	}

	tx, err := s.beginWriteOnConnContext(ctx, conn)
	if err != nil {
		return 0, fmt.Errorf("sqlite receiver batch begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	analysisInvalidated := s.analysisGenerationPresent
	if analysisInvalidated {
		if err = invalidateAnalysisGenerationTx(tx); err != nil {
			return 0, fmt.Errorf("sqlite receiver batch invalidate analysis: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `
DELETE FROM edges
WHERE id IN (
    SELECT old.id
    FROM edges AS old
    JOIN temp.go_receiver_rebind_candidates AS r ON r.edge_id = old.id
    WHERE EXISTS (
        SELECT 1
        FROM edges AS existing
        WHERE existing.id <> old.id
          AND existing.from_id = old.from_id
          AND existing.to_id = r.new_to
          AND existing.kind = old.kind
          AND existing.file_path = old.file_path
          AND existing.line = old.line
          AND existing.view_gen = old.view_gen
    )
)`); err != nil {
		return 0, fmt.Errorf("sqlite receiver batch remove existing-key conflicts: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
DELETE FROM edges
WHERE id IN (
    SELECT edge_id
    FROM (
        SELECT old.id AS edge_id,
               ROW_NUMBER() OVER (
                   PARTITION BY old.from_id, r.new_to, old.kind, old.file_path, old.line
                   ORDER BY old.id
               ) AS duplicate_rank
        FROM edges AS old
        JOIN temp.go_receiver_rebind_candidates AS r ON r.edge_id = old.id
    )
    WHERE duplicate_rank > 1
)`); err != nil {
		return 0, fmt.Errorf("sqlite receiver batch deduplicate canonical keys: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
UPDATE edges
SET to_id = (
    SELECT r.new_to
    FROM temp.go_receiver_rebind_candidates AS r
    WHERE r.edge_id = edges.id
)
WHERE id IN (SELECT edge_id FROM temp.go_receiver_rebind_candidates)`); err != nil {
		return 0, fmt.Errorf("sqlite receiver batch update targets: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite receiver batch commit: %w", err)
	}
	committed = true
	if analysisInvalidated {
		s.analysisGenerationPresent = false
	}
	s.finishAnalysisMutationLocked(true)
	return int(candidates), nil
}
