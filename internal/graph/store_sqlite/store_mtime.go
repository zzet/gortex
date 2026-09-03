package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions that the SQLite Store satisfies the optional
// per-file mtime persistence capabilities. Lifting this state into the
// same backend the graph lives in means warm restarts read it through
// one persistence surface instead of a second gob snapshot.
var (
	_ graph.FileMtimeWriter   = (*Store)(nil)
	_ graph.FileMtimeReader   = (*Store)(nil)
	_ graph.FileMtimeReplacer = (*Store)(nil)
	_ graph.FileMtimeDeleter  = (*Store)(nil)
	_ graph.FileReceiptPager  = (*Store)(nil)
)

const (
	fileReceiptHighWaterQuery = `SELECT file_path
FROM file_mtimes
WHERE view_gen = ?
  AND repo_prefix = ?
ORDER BY file_path DESC
LIMIT 1`

	fileReceiptPageQuery = `SELECT file_path, mtime_ns
FROM file_mtimes
WHERE view_gen = ?
  AND repo_prefix = ?
  AND file_path > ?
  AND file_path <= ?
ORDER BY file_path
LIMIT ?`
)

// mtimeChunk bounds how many (view_gen, repo_prefix, file_path, mtime_ns)
// tuples ride in a single multi-row INSERT. SQLite's default compiled-in host
// parameter limit is 999; at 4 params per row that caps a statement at 249
// rows, so 240 leaves headroom.
const mtimeChunk = 240

// SetFileMtime records one file's modification time (nanoseconds since
// the epoch) for a repo prefix, replacing any prior value. It is a
// convenience single-row form of BulkSetFileMtimes.
func (s *Store) SetFileMtime(repoPrefix, filePath string, mtimeNs int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(context.Background(),
		`INSERT OR REPLACE INTO file_mtimes (view_gen, repo_prefix, file_path, mtime_ns) VALUES (?, ?, ?, ?)`,
		s.viewGen, repoPrefix, filePath, mtimeNs,
	)
	return err
}

// BulkSetFileMtimes persists every (filePath -> mtimeNs) entry for one
// repo prefix in a single transaction, chunked so no statement exceeds
// SQLite's host-parameter limit. Idempotent on (repoPrefix, filePath):
// re-running with overlapping keys replaces in place. Empty input is a
// no-op.
func (s *Store) BulkSetFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if len(mtimes) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	if err := insertMtimesTx(tx, s.viewGen, repoPrefix, mtimes); err != nil {
		return err
	}

	return tx.Commit()
}

// ReplaceFileMtimes persists the AUTHORITATIVE full mtime set for one repo
// prefix: every prior row for the prefix is dropped and the supplied set is
// written, all in one transaction. The full-index persist path uses this so
// files deleted since the last index are pruned — BulkSetFileMtimes (upsert)
// would leave their rows behind, and warm-restart reconcile would then
// detect them as phantom deletions on every restart, forcing a full
// re-track that never converges.
//
// Empty input is a deliberate no-op: it never wipes a repo's mtimes from an
// empty snapshot (the indexer guards the call with len(snapshot) > 0).
func (s *Store) ReplaceFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if len(mtimes) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	if _, err := tx.Exec(`DELETE FROM file_mtimes WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
		return err
	}
	if err := insertMtimesTx(tx, s.viewGen, repoPrefix, mtimes); err != nil {
		return err
	}

	return tx.Commit()
}

// DeleteFileMtimes drops the rows for a set of repo-relative file paths
// under one repo prefix — the incremental-reindex sibling of
// ReplaceFileMtimes. The watcher / incremental path calls it when a file is
// deleted so the persisted set stays in step with the live graph and the
// next warm restart does not see the path as a phantom deletion. Empty
// input is a no-op.
func (s *Store) DeleteFileMtimes(repoPrefix string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	// Chunk so the IN-list never exceeds SQLite's host-parameter limit:
	// two leading scope args + up to mtimeChunk path args per stmt.
	for start := 0; start < len(paths); start += mtimeChunk {
		end := min(start+mtimeChunk, len(paths))
		batch := paths[start:end]

		args := make([]any, 0, len(batch)+2)
		args = append(args, s.viewGen, repoPrefix)
		stmt := make([]byte, 0, 64+len(batch)*2)
		stmt = append(stmt, "DELETE FROM file_mtimes WHERE view_gen = ? AND repo_prefix = ? AND file_path IN ("...)
		for i := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, batch[i])
		}
		stmt = append(stmt, ')')
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// insertMtimesTx writes every (path -> ns) entry for repoPrefix into the
// given transaction with chunked multi-row INSERT OR REPLACE statements,
// each kept under SQLite's host-parameter limit. The caller owns the tx
// lifecycle (Begin/Commit/Rollback) and the write lock.
func insertMtimesTx(tx *sql.Tx, viewGen int64, repoPrefix string, mtimes map[string]int64) error {
	// Stable ordering is not required for correctness, but iterating the
	// map directly is fine — we only chunk by count.
	type kv struct {
		path string
		ns   int64
	}
	pending := make([]kv, 0, len(mtimes))
	for p, ns := range mtimes {
		pending = append(pending, kv{path: p, ns: ns})
	}

	for start := 0; start < len(pending); start += mtimeChunk {
		end := min(start+mtimeChunk, len(pending))
		batch := pending[start:end]

		// Build a multi-row INSERT OR REPLACE: (?, ?, ?, ?), (?, ?, ?, ?), ...
		args := make([]any, 0, len(batch)*4)
		stmt := make([]byte, 0, 64+len(batch)*16)
		stmt = append(stmt, "INSERT OR REPLACE INTO file_mtimes (view_gen, repo_prefix, file_path, mtime_ns) VALUES "...)
		for i, e := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?)"...)
			args = append(args, viewGen, repoPrefix, e.path, e.ns)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}

	return nil
}

// LoadFileMtimes returns the recorded mtimes for one repo prefix as a
// fresh map. Returns nil when there is no data for the prefix (the
// "no recorded state" signal warmup expects).
func (s *Store) LoadFileMtimes(repoPrefix string) map[string]int64 {
	rows, err := s.db.Query(
		`SELECT file_path, mtime_ns FROM file_mtimes WHERE view_gen = ? AND repo_prefix = ?`,
		s.viewGen, repoPrefix,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out map[string]int64
	for rows.Next() {
		var path string
		var ns int64
		if err := rows.Scan(&path, &ns); err != nil {
			return nil
		}
		if out == nil {
			out = make(map[string]int64)
		}
		out[path] = ns
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return out
}

// FileMtimes is a fallible read form of LoadFileMtimes. It always
// returns a non-nil (possibly empty) map for a known/unknown prefix and
// surfaces any query error. The interface method LoadFileMtimes is the
// daemon's entry point; this variant exists for callers (and tests)
// that want the error and an always-materialised map.
func (s *Store) FileMtimes(repoPrefix string) (map[string]int64, error) {
	rows, err := s.db.Query(
		`SELECT file_path, mtime_ns FROM file_mtimes WHERE view_gen = ? AND repo_prefix = ?`,
		s.viewGen, repoPrefix,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var path string
		var ns int64
		if err := rows.Scan(&path, &ns); err != nil {
			return nil, err
		}
		out[path] = ns
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// FileReceiptHighWater freezes the lexicographically greatest tracked path
// for one bounded polling rotation. Rows added above it wait for the next
// rotation instead of extending the current one indefinitely.
func (s *Store) FileReceiptHighWater(repoPrefix string) (string, error) {
	var highWater string
	err := s.db.QueryRow(fileReceiptHighWaterQuery, s.viewGen, repoPrefix).Scan(&highWater)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return highWater, err
}

// FileReceiptPage returns one compact mtime-keyset page and closes that cursor
// before optionally fetching the page's content identities. No result cursor
// survives this call, so the poller's filesystem reads never pin a SQLite read
// transaction or reader connection.
func (s *Store) FileReceiptPage(
	repoPrefix, afterPath, highWaterPath string,
	limit int,
	includeContent bool,
) ([]graph.FileReceipt, error) {
	if limit <= 0 || highWaterPath == "" || afterPath >= highWaterPath {
		return nil, nil
	}
	rows, err := s.db.Query(
		fileReceiptPageQuery,
		s.viewGen, repoPrefix, afterPath, highWaterPath, limit,
	)
	if err != nil {
		return nil, err
	}
	receipts := make([]graph.FileReceipt, 0, limit)
	for rows.Next() {
		var receipt graph.FileReceipt
		if err := rows.Scan(&receipt.FilePath, &receipt.MtimeNS); err != nil {
			_ = rows.Close()
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if !includeContent || len(receipts) == 0 {
		return receipts, nil
	}
	if err := s.fillFileReceiptContent(repoPrefix, receipts); err != nil {
		return nil, err
	}
	return receipts, nil
}

// fillFileReceiptContent reads only content_hash + size for a bounded mtime
// page. file_mtimes uses slash-form unprefixed keys, while files uses the
// graph's prefixed, OS-native path, so map the keys in Go rather than baking a
// platform-specific path expression into the SQL lookup.
func (s *Store) fillFileReceiptContent(repoPrefix string, receipts []graph.FileReceipt) error {
	byGraphPath := make(map[string]int, len(receipts))
	graphPaths := make([]string, len(receipts))
	for i, receipt := range receipts {
		graphPath := filepath.FromSlash(receipt.FilePath)
		if repoPrefix != "" {
			graphPath = repoPrefix + "/" + graphPath
		}
		graphPaths[i] = graphPath
		byGraphPath[graphPath] = i
	}

	for start := 0; start < len(graphPaths); start += fileMetaChunk {
		end := min(start+fileMetaChunk, len(graphPaths))
		chunk := graphPaths[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.viewGen, repoPrefix)
		stmt := make([]byte, 0, 96+len(chunk)*2)
		stmt = append(stmt, "SELECT file_path, content_hash, size FROM files WHERE view_gen = ? AND repo_prefix = ? AND file_path IN ("...)
		for i, filePath := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, filePath)
		}
		stmt = append(stmt, ')')

		rows, err := s.db.Query(string(stmt), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var filePath, contentHash string
			var size int64
			if err := rows.Scan(&filePath, &contentHash, &size); err != nil {
				_ = rows.Close()
				return err
			}
			if i, ok := byGraphPath[filePath]; ok {
				receipts[i].ContentHash = contentHash
				receipts[i].Size = size
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}
