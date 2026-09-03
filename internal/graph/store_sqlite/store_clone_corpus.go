package store_sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.CloneCorpusWriter          = (*Store)(nil)
	_ graph.CloneCorpusSignatureWriter = (*Store)(nil)
	_ graph.CloneCorpusPager           = (*Store)(nil)
	_ graph.CloneCorpusRepoReplacer    = (*Store)(nil)
	_ graph.CloneCorpusInitialization  = (*Store)(nil)
)

const cloneCorpusPageSize = 1024

// cloneCorpusRowsFromNodes extracts the parser's transient raw shingle stamp
// before node Meta is encoded. The returned rows are bounded by the caller's
// AddBatch input and are persisted in the same SQLite transaction as the node.
func cloneCorpusRowsFromNodes(nodes []*graph.Node) []graph.CloneCorpusRow {
	rows := make([]graph.CloneCorpusRow, 0)
	for _, node := range nodes {
		if node == nil || node.ID == "" || node.Meta == nil || graph.IsProxyNode(node) {
			continue
		}
		if node.Kind != graph.KindFunction && node.Kind != graph.KindMethod {
			continue
		}
		shingles, ok := node.Meta["clone_shingles"].([]uint64)
		if !ok {
			continue
		}
		row := graph.CloneCorpusRow{
			NodeID: node.ID, RepoPrefix: node.RepoPrefix, Shingles: shingles,
			TokenCount: cloneTokenCount(node.Meta["clone_tokens"]),
		}
		if sig, ok := node.Meta["clone_sig"].(string); ok {
			row.Signature = sig
			row.Finalized = true
		}
		rows = append(rows, row)
	}
	return rows
}

func cloneTokenCount(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

// stripCloneShingles removes the large parser-only shingle slice from the
// SQLite Meta payload without mutating the caller's map. clone_sig remains a
// promoted flat column and is hydrated back into Meta on every node read.
func stripCloneShingles(meta map[string]any) map[string]any {
	_, hasShingles := meta["clone_shingles"]
	_, hasTokens := meta["clone_tokens"]
	if !hasShingles && !hasTokens {
		return meta
	}
	out := make(map[string]any, len(meta)-1)
	for key, value := range meta {
		// Both keys are parser->corpus transport: their durable home is the
		// clone_shingles sidecar (shingles blob + token_count), which the
		// AddBatch corpus hook populates from the SAME original map before
		// this stripped copy is encoded. Persisting them again in the node
		// blob duplicated the corpus per node.
		if key != "clone_shingles" && key != "clone_tokens" {
			out[key] = value
		}
	}
	return out
}

// BulkSetCloneCorpus persists a bounded projection batch and mirrors finalized
// signatures into nodes.clone_sig in the same transaction. A nil SQL signature
// means "pending"; the empty string means "finalized but filtered out".
func (s *Store) BulkSetCloneCorpus(repoPrefix string, rows []graph.CloneCorpusRow) error {
	if len(rows) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := upsertCloneCorpusTx(tx, s.viewGen, repoPrefix, rows); err != nil {
		return err
	}
	if err := markCloneCorpusInitializedTx(tx, s.viewGen, repoPrefix); err != nil {
		return err
	}
	return tx.Commit()
}

// BulkSetCloneSignatures finalizes existing clone-corpus rows without encoding
// or rewriting their unchanged shingle BLOBs. Both sidecar and promoted node
// signatures are updated in the same transaction.
func (s *Store) BulkSetCloneSignatures(repoPrefix string, updates []graph.CloneCorpusSignatureUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := updateCloneSignaturesTx(tx, s.viewGen, repoPrefix, updates); err != nil {
		return err
	}
	if err := markCloneCorpusInitializedTx(tx, s.viewGen, repoPrefix); err != nil {
		return err
	}
	return tx.Commit()
}

func updateCloneSignaturesTx(tx *sql.Tx, viewGen int64, repoPrefix string, updates []graph.CloneCorpusSignatureUpdate) error {
	for start := 0; start < len(updates); start += shingleChunk {
		end := min(start+shingleChunk, len(updates))
		batch := updates[start:end]
		ids := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*3+1)
		var cases strings.Builder
		cases.WriteString("CASE node_id")
		for _, update := range batch {
			if update.NodeID == "" {
				continue
			}
			cases.WriteString(" WHEN ? THEN ?")
			args = append(args, update.NodeID, update.Signature)
			ids = append(ids, update.NodeID)
		}
		if len(ids) == 0 {
			continue
		}
		cases.WriteString(" ELSE signature END")
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args = append(args, viewGen, repoPrefix)
		for _, id := range ids {
			args = append(args, id)
		}
		query := `UPDATE clone_shingles SET signature = ` + cases.String() + `
WHERE view_gen = ? AND repo_prefix = ? AND node_id IN (` + placeholders + `)`
		if _, err := tx.Exec(query, args...); err != nil {
			return err
		}

		// The nodes.clone_sig mirror reads back the signature this handle just
		// wrote, so its subqueries carry the same generation scope.
		nodeArgs := make([]any, 0, len(ids)+4)
		nodeArgs = append(nodeArgs, viewGen)
		for _, id := range ids {
			nodeArgs = append(nodeArgs, id)
		}
		nodeArgs = append(nodeArgs, repoPrefix, viewGen, viewGen)
		updateNodes := `UPDATE nodes
SET clone_sig = NULLIF((SELECT signature FROM clone_shingles WHERE node_id = nodes.id AND view_gen = ?), '')
WHERE id IN (` + placeholders + `) AND repo_prefix = ? AND view_gen = ?
  AND clone_sig IS NOT NULLIF((SELECT signature FROM clone_shingles WHERE node_id = nodes.id AND view_gen = ?), '')`
		if _, err := tx.Exec(updateNodes, nodeArgs...); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceCloneCorpus atomically resets one repository's projection. It is the
// full-shadow-drain sibling of BulkSetCloneCorpus; subsequent bounded chunks
// append through BulkSetCloneCorpus.
func (s *Store) ReplaceCloneCorpus(repoPrefix string, rows []graph.CloneCorpusRow) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`DELETE FROM clone_shingles WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE nodes SET clone_sig = NULL WHERE repo_prefix = ? AND view_gen = ? AND clone_sig IS NOT NULL`, repoPrefix, s.viewGen); err != nil {
		return err
	}
	if err := upsertCloneCorpusTx(tx, s.viewGen, repoPrefix, rows); err != nil {
		return err
	}
	if err := markCloneCorpusInitializedTx(tx, s.viewGen, repoPrefix); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertCloneCorpusTx(tx *sql.Tx, viewGen int64, repoPrefix string, rows []graph.CloneCorpusRow) error {
	for start := 0; start < len(rows); start += shingleChunk {
		end := start + shingleChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*6)
		var values strings.Builder
		for _, row := range batch {
			if row.NodeID == "" {
				continue
			}
			if values.Len() > 0 {
				values.WriteByte(',')
			}
			values.WriteString("(?, ?, ?, ?, ?, ?)")
			var signature any
			if row.Finalized {
				signature = row.Signature
			}
			prefix := repoPrefix
			if row.RepoPrefix != "" {
				prefix = row.RepoPrefix
			}
			args = append(args, viewGen, row.NodeID, prefix, encodeShingles(row.Shingles), signature, row.TokenCount)
		}
		if values.Len() == 0 {
			continue
		}
		query := `INSERT INTO clone_shingles (view_gen, node_id, repo_prefix, shingles, signature, token_count) VALUES ` + values.String() + `
ON CONFLICT(view_gen, node_id) DO UPDATE SET
 repo_prefix=excluded.repo_prefix,
 shingles=excluded.shingles,
 token_count=excluded.token_count,
 signature=CASE
   WHEN excluded.signature IS NULL
    AND clone_shingles.repo_prefix IS excluded.repo_prefix
    AND clone_shingles.shingles IS excluded.shingles
    AND clone_shingles.token_count IS excluded.token_count
   THEN clone_shingles.signature
   ELSE excluded.signature END
WHERE clone_shingles.repo_prefix IS NOT excluded.repo_prefix
   OR clone_shingles.shingles IS NOT excluded.shingles
   OR clone_shingles.token_count IS NOT excluded.token_count
   OR (excluded.signature IS NOT NULL AND clone_shingles.signature IS NOT excluded.signature)`
		if _, err := tx.Exec(query, args...); err != nil {
			return err
		}

		ids := make([]string, 0, len(batch))
		for _, row := range batch {
			if row.NodeID != "" {
				ids = append(ids, row.NodeID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		updateArgs := make([]any, 0, len(ids)+3)
		updateArgs = append(updateArgs, viewGen)
		for i := range ids {
			updateArgs = append(updateArgs, ids[i])
		}
		updateArgs = append(updateArgs, viewGen, viewGen)
		update := `UPDATE nodes
SET clone_sig = NULLIF((SELECT signature FROM clone_shingles WHERE node_id = nodes.id AND view_gen = ?), '')
WHERE id IN (` + placeholders + `) AND view_gen = ?
  AND clone_sig IS NOT NULLIF((SELECT signature FROM clone_shingles WHERE node_id = nodes.id AND view_gen = ?), '')`
		if _, err := tx.Exec(update, updateArgs...); err != nil {
			return err
		}
	}
	return nil
}

// CloneCorpusInitialized reports whether this repository's clone sidecar has
// been authoritatively initialized. The single-row primary-key probe remains
// cheap for a genuinely empty corpus.
func (s *Store) CloneCorpusInitialized(repoPrefix string) (bool, error) {
	var initialized bool
	if err := s.db.QueryRow(`SELECT EXISTS(
SELECT 1 FROM clone_corpus_state WHERE view_gen = ? AND repo_prefix = ?)`, s.viewGen, repoPrefix).Scan(&initialized); err != nil {
		return false, err
	}
	return initialized, nil
}

// MarkCloneCorpusInitialized records a successful legacy-empty compatibility
// scan so future restarts can trust the empty sidecar.
func (s *Store) MarkCloneCorpusInitialized(repoPrefix string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := markCloneCorpusInitializedTx(tx, s.viewGen, repoPrefix); err != nil {
		return err
	}
	return tx.Commit()
}

func markCloneCorpusInitializedTx(tx *sql.Tx, viewGen int64, repoPrefix string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO clone_corpus_state (view_gen, repo_prefix) VALUES (?, ?)`, viewGen, repoPrefix)
	return err
}

// CloneCorpusPage returns a stable, bounded projection page. The composite
// repo index makes both cold two-pass finalisation and warm LSH rebuilds seek
// instead of scanning other repositories.
func (s *Store) CloneCorpusPage(repoPrefix, afterNodeID string, limit int) ([]graph.CloneCorpusRow, error) {
	if limit <= 0 || limit > cloneCorpusPageSize {
		limit = cloneCorpusPageSize
	}
	rows, err := s.db.Query(`
SELECT node_id, shingles, signature, token_count
FROM clone_shingles
WHERE view_gen = ? AND repo_prefix = ? AND node_id > ?
ORDER BY node_id
LIMIT ?`, s.viewGen, repoPrefix, afterNodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]graph.CloneCorpusRow, 0, limit)
	for rows.Next() {
		var (
			row  graph.CloneCorpusRow
			blob sql.RawBytes
			sig  sql.NullString
		)
		if err := rows.Scan(&row.NodeID, &blob, &sig, &row.TokenCount); err != nil {
			return nil, err
		}
		row.Shingles = decodeShingles(blob)
		row.Finalized = sig.Valid
		if sig.Valid {
			row.Signature = sig.String
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type cloneCorpusSchemaDB interface {
	Query(query string, args ...any) (*sql.Rows, error)
	Exec(query string, args ...any) (sql.Result, error)
}

func ensureCloneCorpusColumns(db cloneCorpusSchemaDB) error {
	rows, err := db.Query(`PRAGMA table_info(clone_shingles)`)
	if err != nil {
		return err
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !existing["signature"] {
		if _, err := db.Exec(`ALTER TABLE clone_shingles ADD COLUMN signature TEXT`); err != nil {
			return fmt.Errorf("add clone signature: %w", err)
		}
	}
	if !existing["token_count"] {
		if _, err := db.Exec(`ALTER TABLE clone_shingles ADD COLUMN token_count INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add clone token count: %w", err)
		}
	}
	// clone_shingles_by_repo leads with view_gen, a column the v15 step adds,
	// so it is created from the shared sidecar registry after the migration
	// steps run rather than here — this function runs before them.
	return nil
}
