package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// This file implements graph.ContentSearcher on the SQLite backend using
// the content_fts FTS5 virtual table declared in schema.go — the
// dedicated, on-disk full-text index for CONTENT (data_class="content")
// section bodies, kept physically separate from symbol_fts so content
// text never reaches the symbol search or the code-oriented graph passes.
//
// Streamed build: WipeContent(repoPrefix) once at the start of a full
// index, AppendContent each content file's sections as they are parsed
// (no per-file wipe), then BuildContentIndex to merge segments.
// Incremental reindex of one content file is WipeContentFile +
// AppendContent.

// Compile-time assertion: *Store satisfies the content-search capability.
var (
	_ graph.ContentSearcher         = (*Store)(nil)
	_ graph.ContentFTSBatchReplacer = (*Store)(nil)
)

// contentInsertChunkRows bounds rows per multi-row INSERT. Each FTS row binds
// 6 host params (explicit rowid plus the five payload columns); 160 rows is
// 960 params, below SQLite's conservative 999-variable limit.
const contentInsertChunkRows = 160

// Replacement calls from the indexer are already bounded, but keep the store
// safe for other callers too. Each group is one transaction: at most 64 files,
// 2,048 sections, or 8 MiB of section text. A file is never split across
// groups, preserving authoritative replacement semantics.
const (
	contentReplaceMaxFiles = 64
	contentReplaceMaxItems = 2048
	contentReplaceMaxBytes = 8 << 20
)

type contentFTSReplaceStats struct {
	allocatorQueries          int
	ftsDeleteStatements       int
	ownershipDeleteStatements int
	insertStatements          int
	ownershipInsertStatements int
	commits                   int
}

// Every ownership predicate leads with view_gen: the FTS5 virtual table has no
// rewritable primary key, so the docid map carries the generation and scopes
// each delete to the caller's view.
const (
	contentOwnerByRepo     = `view_gen = ? AND repo_prefix = ?`
	contentOwnerByFile     = `view_gen = ? AND file_path = ?`
	contentOwnerByRepoFile = `view_gen = ? AND repo_prefix = ? AND file_path = ?`
)

// deleteContentFTSByOwnershipTx resolves ownership through the ordinary
// indexed sidecar, then deletes FTS rows by docid. Filtering the FTS5 table's
// UNINDEXED repo/file columns directly would scan the entire virtual table.
func deleteContentFTSByOwnershipTx(tx *sql.Tx, predicate string, args ...any) (int64, error) {
	result, err := tx.Exec(`DELETE FROM content_fts
WHERE rowid IN (
    SELECT fts_rowid FROM content_fts_rowid WHERE `+predicate+`
)`, args...)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM content_fts_rowid WHERE `+predicate, args...); err != nil {
		return 0, err
	}
	return changed, nil
}

// WipeContent removes a repo's content rows before a full rebuild. Empty
// repoPrefix wipes the whole table (single-repo / conformance behaviour).
func (s *Store) WipeContent(repoPrefix string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if _, err := deleteContentFTSByOwnershipTx(tx, contentOwnerByRepo, s.viewGen, repoPrefix); err != nil {
		return err
	}
	return tx.Commit()
}

// WipeContentFile removes one file's content rows — the incremental
// reindex path when a single content file changes.
func (s *Store) WipeContentFile(filePath string) error {
	if filePath == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if _, err := deleteContentFTSByOwnershipTx(tx, contentOwnerByFile, s.viewGen, filePath); err != nil {
		return err
	}
	return tx.Commit()
}

// ContentRepoHasRows reports whether a repository owns any persisted content
// rows. The repo-qualified form is one indexed EXISTS probe over
// content_fts_rowid_by_repo_file; it never scans or materializes FTS bodies.
//
// An empty prefix is a WILDCARD: it asks whether the sidecar holds ANY content
// rows at all, across every repo. That is deliberate — the probe's callers use
// it as a "is there content to search" gate — and it is not a single-repo
// special case, so it must survive unchanged even once every repo carries a
// prefix.
func (s *Store) ContentRepoHasRows(repoPrefix string) (bool, error) {
	var exists bool
	if repoPrefix == "" {
		err := s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM content_fts_rowid WHERE view_gen = ? LIMIT 1
	)`, s.viewGen).Scan(&exists)
		return exists, err
	}
	err := s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM content_fts_rowid WHERE view_gen = ? AND repo_prefix = ? LIMIT 1
	)`, s.viewGen, repoPrefix).Scan(&exists)
	return exists, err
}

// WipeContentFileInRepo removes ONE file's content rows scoped to a repo —
// the crash-safe full-index sibling of WipeContentFile (which keys on
// file_path alone and so would clobber a same-named file in another repo).
// A full index streams content per file: delete this file's prior rows,
// then AppendContent its fresh sections — so a mid-parse kill leaves a mix
// of old+new content per file rather than the empty table a repo-wide
// pre-wipe would leave behind. Empty filePath is a no-op.
func (s *Store) WipeContentFileInRepo(repoPrefix, filePath string) error {
	if filePath == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if _, err := deleteContentFTSByOwnershipTx(tx, contentOwnerByRepoFile, s.viewGen, repoPrefix, filePath); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteContentFilesForRepoNotIn sweeps a repo's content rows down to keep —
// every content row whose file_path is absent from keep is deleted. keep is
// the set of files that actually STREAMED content sections in the walk just
// completed (each recorded as the same file_path form AppendContent wrote),
// NOT the set of files that merely survive on disk: a file can still exist
// yet stop producing content (doc emptied, classification changed), and a
// disk-based keep would protect its stale rows forever. Run once at the end
// of a successful full index (right after the authoritative mtime replace),
// it reaps vanished files and content->no-content transitions in one scan;
// the per-file WipeContentFileInRepo + AppendContent streaming build
// refreshes the files that still produce content. Together they replace the
// old repo-wide pre-wipe: a mid-parse kill no longer empties the content
// index, and stale rows are reaped only on the next clean completion.
// Empty keep is a deliberate no-op safety net — never wipe a whole repo from
// an empty set; a caller that legitimately ends a walk with zero content
// files calls WipeContent explicitly instead.
func (s *Store) DeleteContentFilesForRepoNotIn(repoPrefix string, keep map[string]struct{}) error {
	if len(keep) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	kept := make([]string, 0, len(keep))
	for filePath := range keep {
		kept = append(kept, filePath)
	}
	keepJSON, ok := projectionJSON(kept)
	if !ok {
		return nil
	}
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	predicate := `view_gen = ?
AND repo_prefix = ?
AND file_path NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	if _, err := deleteContentFTSByOwnershipTx(tx, predicate, s.viewGen, repoPrefix, keepJSON); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceContentFiles atomically replaces the complete section set for many
// files. It is the cold-index fast path: one indexed ownership delete pair and
// one transaction per bounded group replace the old wipe+append transaction
// pair for every individual file. An empty Items slice is authoritative and
// deletes stale rows without inserting replacements.
func (s *Store) ReplaceContentFiles(repoPrefix string, files []graph.ContentFTSFileReplacement) error {
	_, err := s.replaceContentFiles(files, repoPrefix)
	return err
}

func normalizeContentReplacements(files []graph.ContentFTSFileReplacement) []graph.ContentFTSFileReplacement {
	positions := make(map[string]int, len(files))
	normalized := make([]graph.ContentFTSFileReplacement, 0, len(files))
	for _, file := range files {
		if file.FilePath == "" {
			continue
		}

		// A graph node ID identifies one section. Keep the first position so
		// output order remains stable, but replace its payload with the last
		// occurrence to mirror graph AddBatch's last-write-wins semantics.
		itemPositions := make(map[string]int, len(file.Items))
		items := make([]graph.ContentFTSItem, 0, len(file.Items))
		for _, item := range file.Items {
			if item.NodeID == "" {
				continue
			}
			item.FilePath = file.FilePath
			if pos, ok := itemPositions[item.NodeID]; ok {
				items[pos] = item
				continue
			}
			itemPositions[item.NodeID] = len(items)
			items = append(items, item)
		}
		file.Items = items
		if pos, ok := positions[file.FilePath]; ok {
			normalized[pos] = file
			continue
		}
		positions[file.FilePath] = len(normalized)
		normalized = append(normalized, file)
	}
	return normalized
}

func contentReplacementWeight(file graph.ContentFTSFileReplacement) (items, bytes int) {
	items = len(file.Items)
	for _, item := range file.Items {
		bytes += len(item.NodeID) + len(item.Body) + len(file.FilePath) + 32
	}
	return items, bytes
}

func (s *Store) replaceContentFiles(
	files []graph.ContentFTSFileReplacement,
	repoPrefix string,
) (contentFTSReplaceStats, error) {
	var stats contentFTSReplaceStats
	files = normalizeContentReplacements(files)
	if len(files) == 0 {
		return stats, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for start := 0; start < len(files); {
		end := start
		itemCount := 0
		bodyBytes := 0
		for end < len(files) {
			fileItems, fileBytes := contentReplacementWeight(files[end])
			wouldOverflow := end > start && (end-start >= contentReplaceMaxFiles ||
				itemCount+fileItems > contentReplaceMaxItems ||
				bodyBytes+fileBytes > contentReplaceMaxBytes)
			if wouldOverflow {
				break
			}
			itemCount += fileItems
			bodyBytes += fileBytes
			end++
		}
		if end == start {
			return stats, fmt.Errorf("content replacement made no progress at file %q", files[start].FilePath)
		}
		if err := s.replaceContentFileGroupLocked(repoPrefix, files[start:end], &stats); err != nil {
			return stats, err
		}
		start = end
	}
	return stats, nil
}

func (s *Store) replaceContentFileGroupLocked(
	repoPrefix string,
	files []graph.ContentFTSFileReplacement,
	stats *contentFTSReplaceStats,
) error {
	paths := make([]string, len(files))
	itemCount := 0
	for i, file := range files {
		paths[i] = file.FilePath
		itemCount += len(file.Items)
	}
	pathsJSON, ok := projectionJSON(paths)
	if !ok {
		return fmt.Errorf("encode %d content replacement paths", len(paths))
	}

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	predicate := `view_gen = ?
AND repo_prefix = ?
AND file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	if _, err := deleteContentFTSByOwnershipTx(tx, predicate, s.viewGen, repoPrefix, pathsJSON); err != nil {
		return err
	}
	stats.ftsDeleteStatements++
	stats.ownershipDeleteStatements++

	if itemCount > 0 {
		nextRowid, err := nextFTSRowIDTx(tx, "content_fts")
		if err != nil {
			return err
		}
		stats.allocatorQueries++

		items := make([]graph.ContentFTSItem, 0, itemCount)
		for _, file := range files {
			items = append(items, file.Items...)
		}
		for start := 0; start < len(items); start += contentInsertChunkRows {
			end := minInt(start+contentInsertChunkRows, len(items))
			chunk := items[start:end]

			var insert strings.Builder
			insert.WriteString(`INSERT INTO content_fts (rowid, node_id, repo_prefix, file_path, ordinal, body) VALUES `)
			args := make([]any, 0, len(chunk)*6)
			ownerArgs := make([]any, 0, len(chunk)*4)
			for _, item := range chunk {
				if len(args) > 0 {
					insert.WriteByte(',')
				}
				insert.WriteString(`(?,?,?,?,?,?)`)
				rowid := nextRowid
				nextRowid++
				args = append(args, rowid, item.NodeID, repoPrefix, item.FilePath, item.Ordinal, item.Body)
				ownerArgs = append(ownerArgs, s.viewGen, rowid, repoPrefix, item.FilePath)
			}
			if _, err := tx.Exec(insert.String(), args...); err != nil {
				return err
			}
			stats.insertStatements++

			var owners strings.Builder
			owners.WriteString(`INSERT INTO content_fts_rowid (view_gen, fts_rowid, repo_prefix, file_path) VALUES `)
			for i := range chunk {
				if i > 0 {
					owners.WriteByte(',')
				}
				owners.WriteString(`(?,?,?,?)`)
			}
			if _, err := tx.Exec(owners.String(), ownerArgs...); err != nil {
				return err
			}
			stats.ownershipInsertStatements++
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	stats.commits++
	return nil
}

// AppendContent inserts content rows for repoPrefix without wiping — the
// streamed per-file build path. Callers wipe (whole repo or one file)
// first. Rows with an empty NodeID are skipped.
func (s *Store) AppendContent(repoPrefix string, items []graph.ContentFTSItem) error {
	if len(items) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	commit := false
	defer func() {
		if !commit {
			_ = tx.Rollback()
		}
	}()
	nextRowid, err := nextFTSRowIDTx(tx, "content_fts")
	if err != nil {
		return err
	}
	validOffset := int64(0)

	for start := 0; start < len(items); start += contentInsertChunkRows {
		end := minInt(start+contentInsertChunkRows, len(items))
		chunk := items[start:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO content_fts (rowid, node_id, repo_prefix, file_path, ordinal, body) VALUES `)
		args := make([]any, 0, len(chunk)*6)
		mapArgs := make([]any, 0, len(chunk)*4)
		for _, it := range chunk {
			if it.NodeID == "" {
				continue
			}
			if len(args) > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?,?,?,?,?,?)`)
			rowid := nextRowid + validOffset
			validOffset++
			args = append(args, rowid, it.NodeID, repoPrefix, it.FilePath, it.Ordinal, it.Body)
			mapArgs = append(mapArgs, s.viewGen, rowid, repoPrefix, it.FilePath)
		}
		if len(args) == 0 {
			continue
		}
		if _, err := tx.Exec(b.String(), args...); err != nil {
			return err
		}
		var owners strings.Builder
		owners.WriteString(`INSERT INTO content_fts_rowid (view_gen, fts_rowid, repo_prefix, file_path) VALUES `)
		for i := 0; i < len(mapArgs)/4; i++ {
			if i > 0 {
				owners.WriteByte(',')
			}
			owners.WriteString(`(?,?,?,?)`)
		}
		if _, err := tx.Exec(owners.String(), mapArgs...); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	commit = true
	return nil
}

// backfillContentFTSRowidMap upgrades a store written before the ownership
// sidecar existed. It scans the FTS virtual table once on that transition;
// every subsequent Open sees a populated map and stays O(1). Like the symbol
// map's backfill it runs before the migration steps and so names no view_gen
// column: the column's DEFAULT 0 puts recovered rows in the base corpus, and
// an older store does not have the column yet.
func backfillContentFTSRowidMap(db *sql.DB) error {
	var mapped bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM content_fts_rowid)`).Scan(&mapped); err != nil {
		return err
	}
	if mapped {
		return nil
	}
	var hasFTS bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM content_fts)`).Scan(&hasFTS); err != nil {
		return err
	}
	if !hasFTS {
		return nil
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO content_fts_rowid (fts_rowid, repo_prefix, file_path)
SELECT rowid, repo_prefix, file_path FROM content_fts`)
	return err
}

// BuildContentIndex opportunistically merges FTS5 segments (a read-latency
// improvement). Like BuildSymbolIndex it is a no-op for correctness — the
// FTS index is maintained incrementally on every insert — and idempotent.
func (s *Store) BuildContentIndex() error {
	// Derived generations are immutable and invisible until publication. Their
	// incremental FTS writes are already query-correct; optimizing the global
	// virtual table once per generation only multiplies writer hold time.
	if s.viewGen > 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.coordinatedBulkLoad {
		s.deferredContentFTS = true
		return nil
	}
	_, _ = s.execActiveWriteLocked(context.Background(), `INSERT INTO content_fts(content_fts) VALUES('optimize')`)
	return nil
}

// SearchContent runs a content query scoped to repoPrefix (empty = all
// repos) and returns hits ordered by descending relevance, each carrying a
// short snippet excerpt from the matched body. Reuses the symbol path's
// write-side tokeniser (buildFTSMatch) so the content corpus and queries
// agree on camelCase / path-separator splitting.
func (s *Store) SearchContent(query, repoPrefix string, limit int) ([]graph.ContentHit, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	match := s.buildFTSMatch(query, false)
	if match == "" {
		return nil, nil
	}

	var sb strings.Builder
	// snippet() over the body column (index 4): no highlight marks, an
	// ellipsis for elision, ~16 tokens of context. CAST(ordinal AS INTEGER)
	// forces integer affinity so the FTS5 text column scans cleanly into an
	// int.
	// The rowid map carries the generation content_fts itself cannot: one
	// shared virtual table holds every generation's sections, so the MATCH is
	// joined back through content_fts_rowid (whose primary key IS fts_rowid) to
	// keep hidden generations out of the candidate set. bm25 ordering is
	// unchanged.
	sb.WriteString(`SELECT content_fts.node_id, content_fts.file_path, CAST(content_fts.ordinal AS INTEGER), snippet(content_fts, 4, '', '', '…', 16), bm25(content_fts)
FROM content_fts
JOIN content_fts_rowid
  ON content_fts_rowid.fts_rowid = content_fts.rowid AND content_fts_rowid.view_gen = ?
WHERE content_fts MATCH ?`)
	args := []any{s.viewGen, match}
	if repoPrefix != "" {
		sb.WriteString(` AND content_fts.repo_prefix = ?`)
		args = append(args, repoPrefix)
	}
	sb.WriteString(` ORDER BY bm25(content_fts) LIMIT ?`)
	args = append(args, limit)

	rows, err := s.db.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []graph.ContentHit
	for rows.Next() {
		var (
			id, fp, snip string
			ordinal      int
			score        float64
		)
		if err := rows.Scan(&id, &fp, &ordinal, &snip, &score); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		// bm25() is negative-better in SQLite; negate so higher = better,
		// matching the ContentHit contract. Rows already arrive best-first.
		hits = append(hits, graph.ContentHit{
			NodeID:   id,
			FilePath: fp,
			Ordinal:  ordinal,
			Score:    -score,
			Snippet:  snip,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// ScanContent streams every content row (scoped to repoPrefix; empty = all
// repos) to fn with its FULL body, read incrementally via a cursor so a
// consumer iterating hundreds of thousands of sections stays bounded. fn
// returns false to stop the scan early.
func (s *Store) ScanContent(repoPrefix string, fn func(nodeID, filePath, body string) bool) error {
	var rows *sql.Rows
	var err error
	// Same rowid-map join as SearchContent: the virtual table is shared, so the
	// generation filter has to come from the ownership sidecar.
	const scan = `SELECT content_fts.node_id, content_fts.file_path, content_fts.body
FROM content_fts
JOIN content_fts_rowid
  ON content_fts_rowid.fts_rowid = content_fts.rowid AND content_fts_rowid.view_gen = ?`
	if repoPrefix == "" {
		rows, err = s.db.Query(scan, s.viewGen)
	} else {
		rows, err = s.db.Query(scan+` WHERE content_fts.repo_prefix = ?`, s.viewGen, repoPrefix)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, filePath, body string
		if err := rows.Scan(&nodeID, &filePath, &body); err != nil {
			return err
		}
		if !fn(nodeID, filePath, body) {
			return nil
		}
	}
	return rows.Err()
}
