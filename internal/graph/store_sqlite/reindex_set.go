package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	// Reindex transactions are simulated as one ordered set so intermediate
	// writes that cancel do not invalidate analysis or inflate receipts. SQL is
	// still issued in bounded VALUES relations below SQLite's conservative
	// 999-variable limit.
	reindexSetChunkSize = 70  // 70*14 = 980 parameters for edge inserts.
	reindexKeyChunkSize = 140 // 140*5 = 700 parameters for identity relations.
)

type sqliteReindexKey struct {
	fromID   string
	toID     string
	kind     string
	filePath string
	line     int
}

type sqliteReindexRow struct {
	key                   sqliteReindexKey
	confidence            float64
	confidenceLabel       string
	origin                string
	tier                  string
	crossRepo             int64
	meta                  []byte
	resolveTerminal       sql.NullBool
	resolveTerminalReason sql.NullString
	semanticSource        sql.NullString
	receiptEdge           *graph.Edge
}

type sqliteReindexMutation struct {
	oldKey sqliteReindexKey
	newRow sqliteReindexRow
}

type sqliteReindexSetStats struct {
	selectStatements int
	deleteStatements int
	insertStatements int
	deletedRows      int
	insertedRows     int
}

func (s sqliteReindexSetStats) writeStatements() int {
	return s.deleteStatements + s.insertStatements
}

func (s *sqliteReindexSetStats) add(other sqliteReindexSetStats) {
	s.selectStatements += other.selectStatements
	s.deleteStatements += other.deleteStatements
	s.insertStatements += other.insertStatements
	s.deletedRows += other.deletedRows
	s.insertedRows += other.insertedRows
}

func (s *Store) reindexEdgesSetOriented(batch []graph.EdgeReindex) (sqliteReindexSetStats, error) {
	var stats sqliteReindexSetStats
	if len(batch) == 0 {
		return stats, nil
	}

	// A resolver reindex is a mandatory graph mutation: the graph.Store
	// interface has no error channel (it mirrors the in-memory store's
	// "everything succeeds" contract), so ReindexEdges routes any failure here
	// through panicOnFatal. Acquire the writer gate with an UNBOUNDED Lock,
	// exactly like the single-edge ReindexEdge and the sibling
	// setEdgeProvenanceBatchSetOriented. A bounded LockContext(sqliteBusyRetryWindow)
	// abandons the batch when a sibling writer -- a warmup checkpoint, or a
	// deferred-enrich mutation -- holds writeMu past the window; that surfaces as
	// context.DeadlineExceeded, which is NOT one of panicOnFatal's swallowed
	// teardown races, so it crashes the whole daemon over a recoverable
	// lock-wait. The gate is only ever held by short sibling writers plus
	// context-bounded maintenance (Compact's unbounded VACUUM is a dedicated
	// daemon path, never concurrent with warmup resolve), so waiting cannot
	// hang. Per-transaction SQLITE_BUSY contention is still bounded by
	// withSQLiteBusyRetry below.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	for txStart := 0; txStart < len(batch); txStart += reindexChunkSize {
		txEnd := minInt(txStart+reindexChunkSize, len(batch))
		var (
			txStats             sqliteReindexSetStats
			changed             bool
			invalidatedAnalysis bool
			receipt             *sqliteReindexReceipt
		)
		err := s.withSQLiteBusyRetry(context.Background(), "reindex_edges", func(ctx context.Context) error {
			var txErr error
			txStats, changed, invalidatedAnalysis, receipt, txErr = s.reindexEdgesSetTransactionLocked(ctx, batch[txStart:txEnd])
			return txErr
		})
		if err != nil {
			return stats, err
		}
		stats.add(txStats)
		if invalidatedAnalysis {
			s.analysisGenerationPresent = false
		}
		s.finishAnalysisMutationLocked(changed)
		if changed {
			s.publishSQLiteReindexReceiptLocked(receipt)
		}
	}
	return stats, nil
}

func (s *Store) reindexEdgesSetTransactionLocked(ctx context.Context, batch []graph.EdgeReindex) (
	stats sqliteReindexSetStats,
	changed bool,
	invalidatedAnalysis bool,
	receipt *sqliteReindexReceipt,
	err error,
) {
	mutations, keys, err := sqliteReindexMutations(batch)
	if err != nil || len(mutations) == 0 {
		return stats, false, false, nil, err
	}

	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return stats, false, false, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	receipt = s.prepareSQLiteReindexReceiptTx(tx, batch)

	initial, selectStatements, err := sqliteReindexRowsTx(tx, keys)
	if err != nil {
		return stats, false, false, nil, err
	}
	stats.selectStatements = selectStatements
	deletes, inserts := simulateSQLiteReindexSet(initial, keys, mutations)

	stats.deletedRows, stats.deleteStatements, err = deleteSQLiteReindexRowsTx(tx, deletes)
	if err != nil {
		return stats, false, false, nil, err
	}
	stats.insertedRows, stats.insertStatements, err = insertSQLiteReindexRowsTx(tx, inserts)
	if err != nil {
		return stats, false, false, nil, err
	}
	if stats.insertedRows != len(inserts) {
		return stats, false, false, nil, fmt.Errorf(
			"store_sqlite: set reindex inserted %d of %d simulated rows",
			stats.insertedRows, len(inserts),
		)
	}
	changed = stats.deletedRows > 0 || stats.insertedRows > 0
	if changed && s.analysisGenerationPresent {
		if err := invalidateAnalysisGenerationTx(tx); err != nil {
			return stats, false, false, nil, err
		}
		invalidatedAnalysis = true
	}
	for _, row := range inserts {
		receipt.recordInserted(row.receiptEdge, true)
	}
	if err := tx.Commit(); err != nil {
		return stats, false, false, nil, err
	}
	committed = true
	return stats, changed, invalidatedAnalysis, receipt, nil
}

func sqliteReindexMutations(batch []graph.EdgeReindex) ([]sqliteReindexMutation, []sqliteReindexKey, error) {
	mutations := make([]sqliteReindexMutation, 0, len(batch))
	keys := make([]sqliteReindexKey, 0, len(batch)*2)
	seen := make(map[sqliteReindexKey]struct{}, len(batch)*2)
	addKey := func(key sqliteReindexKey) {
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	for _, reindex := range batch {
		edge := reindex.Edge
		if edge == nil {
			continue
		}
		oldFrom := edge.From
		oldKind := reindex.OldKind
		if oldKind == "" {
			oldKind = edge.Kind
		}
		oldFilePath, oldLine := edge.FilePath, edge.Line
		if reindex.RefreshIdentity {
			if reindex.OldFrom != "" {
				oldFrom = reindex.OldFrom
			}
			oldFilePath, oldLine = reindex.OldFilePath, reindex.OldLine
		} else if reindex.OldTo == edge.To && oldFrom == edge.From && oldKind == edge.Kind {
			continue
		}
		newRow, err := sqliteReindexRowForEdge(edge)
		if err != nil {
			return nil, nil, err
		}
		oldKey := sqliteReindexKey{
			fromID: oldFrom, toID: reindex.OldTo, kind: string(oldKind),
			filePath: oldFilePath, line: oldLine,
		}
		mutations = append(mutations, sqliteReindexMutation{oldKey: oldKey, newRow: newRow})
		addKey(oldKey)
		addKey(newRow.key)
	}
	return mutations, keys, nil
}

func sqliteReindexRowForEdge(edge *graph.Edge) (sqliteReindexRow, error) {
	promoted, blobMeta := extractPromotedEdgeMeta(edge.Meta)
	meta, err := encodeMeta(blobMeta)
	if err != nil {
		return sqliteReindexRow{}, err
	}
	var crossRepo int64
	if edge.CrossRepo {
		crossRepo = 1
	}
	return sqliteReindexRow{
		key: sqliteReindexKey{
			fromID: edge.From, toID: edge.To, kind: string(edge.Kind),
			filePath: edge.FilePath, line: edge.Line,
		},
		confidence:            edge.Confidence,
		confidenceLabel:       edge.ConfidenceLabel,
		origin:                edge.Origin,
		tier:                  edge.Tier,
		crossRepo:             crossRepo,
		meta:                  meta,
		resolveTerminal:       promoted.resolveTerminal,
		resolveTerminalReason: promoted.resolveTerminalReason,
		semanticSource:        promoted.semanticSource,
		receiptEdge:           edge,
	}, nil
}

func sqliteReindexRowsTx(tx *sql.Tx, keys []sqliteReindexKey) (map[sqliteReindexKey]sqliteReindexRow, int, error) {
	out := make(map[sqliteReindexKey]sqliteReindexRow, len(keys))
	statements := 0
	for start := 0; start < len(keys); start += reindexKeyChunkSize {
		end := minInt(start+reindexKeyChunkSize, len(keys))
		chunk := keys[start:end]
		var values strings.Builder
		values.Grow(len(chunk) * len("(?,?,?,?,?),"))
		args := make([]any, 0, len(chunk)*5)
		for i, key := range chunk {
			if i > 0 {
				values.WriteByte(',')
			}
			values.WriteString("(?,?,?,?,?)")
			args = append(args, key.fromID, key.toID, key.kind, key.filePath, key.line)
		}
		query := `WITH wanted(from_id, to_id, kind, file_path, line) AS (VALUES ` + values.String() + `)
		SELECT e.from_id, e.to_id, e.kind, e.file_path, e.line,
			e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo,
			e.meta, e.resolve_terminal, e.resolve_terminal_reason, e.semantic_source
		FROM wanted AS w
		JOIN edges AS e
		  ON e.from_id = w.from_id
		 AND e.to_id = w.to_id
		 AND e.kind = w.kind
		 AND e.file_path = w.file_path
		 AND e.line = w.line`
		rows, err := tx.Query(query, args...)
		if err != nil {
			return nil, statements, err
		}
		statements++
		for rows.Next() {
			var row sqliteReindexRow
			if err := rows.Scan(
				&row.key.fromID, &row.key.toID, &row.key.kind, &row.key.filePath, &row.key.line,
				&row.confidence, &row.confidenceLabel, &row.origin, &row.tier, &row.crossRepo,
				&row.meta, &row.resolveTerminal, &row.resolveTerminalReason, &row.semanticSource,
			); err != nil {
				_ = rows.Close()
				return nil, statements, err
			}
			out[row.key] = row
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, statements, err
		}
		if err := rows.Close(); err != nil {
			return nil, statements, err
		}
	}
	return out, statements, nil
}

func simulateSQLiteReindexSet(
	initial map[sqliteReindexKey]sqliteReindexRow,
	keys []sqliteReindexKey,
	mutations []sqliteReindexMutation,
) (deletes []sqliteReindexKey, inserts []sqliteReindexRow) {
	state := make(map[sqliteReindexKey]sqliteReindexRow, len(initial)+len(mutations))
	for key, row := range initial {
		state[key] = row
	}
	for _, mutation := range mutations {
		delete(state, mutation.oldKey)
		if _, exists := state[mutation.newRow.key]; !exists {
			state[mutation.newRow.key] = mutation.newRow
		}
	}

	for _, key := range keys {
		before, existed := initial[key]
		after, remains := state[key]
		switch {
		case existed && !remains:
			deletes = append(deletes, key)
		case !existed && remains:
			inserts = append(inserts, after)
		case existed && remains && !equalSQLiteReindexRows(before, after):
			deletes = append(deletes, key)
			inserts = append(inserts, after)
		}
	}
	return deletes, inserts
}

func equalSQLiteReindexRows(left, right sqliteReindexRow) bool {
	return left.key == right.key &&
		left.confidence == right.confidence &&
		left.confidenceLabel == right.confidenceLabel &&
		left.origin == right.origin &&
		left.tier == right.tier &&
		left.crossRepo == right.crossRepo &&
		(left.meta == nil) == (right.meta == nil) && bytes.Equal(left.meta, right.meta) &&
		left.resolveTerminal == right.resolveTerminal &&
		left.resolveTerminalReason == right.resolveTerminalReason &&
		left.semanticSource == right.semanticSource
}

func deleteSQLiteReindexRowsTx(tx *sql.Tx, keys []sqliteReindexKey) (int, int, error) {
	changed := 0
	statements := 0
	for start := 0; start < len(keys); start += reindexKeyChunkSize {
		end := minInt(start+reindexKeyChunkSize, len(keys))
		chunk := keys[start:end]
		var values strings.Builder
		values.Grow(len(chunk) * len("(?,?,?,?,?),"))
		args := make([]any, 0, len(chunk)*5)
		for i, key := range chunk {
			if i > 0 {
				values.WriteByte(',')
			}
			values.WriteString("(?,?,?,?,?)")
			args = append(args, key.fromID, key.toID, key.kind, key.filePath, key.line)
		}
		query := `WITH doomed(from_id, to_id, kind, file_path, line) AS (VALUES ` + values.String() + `)
		DELETE FROM edges
		WHERE id IN (
			SELECT e.id
			FROM edges AS e
			JOIN doomed AS d
			  ON e.from_id = d.from_id
			 AND e.to_id = d.to_id
			 AND e.kind = d.kind
			 AND e.file_path = d.file_path
			 AND e.line = d.line
		)`
		result, err := tx.Exec(query, args...)
		if err != nil {
			return changed, statements, err
		}
		statements++
		rows, err := result.RowsAffected()
		if err != nil {
			return changed, statements, err
		}
		changed += int(rows)
	}
	return changed, statements, nil
}

func insertSQLiteReindexRowsTx(tx *sql.Tx, rows []sqliteReindexRow) (int, int, error) {
	changed := 0
	statements := 0
	for start := 0; start < len(rows); start += reindexSetChunkSize {
		end := minInt(start+reindexSetChunkSize, len(rows))
		chunk := rows[start:end]
		var values strings.Builder
		values.Grow(len(chunk) * len("(?,?,?,?,?,?,?,?,?,?,?,?,?,?,),"))
		args := make([]any, 0, len(chunk)*14)
		for i, row := range chunk {
			if i > 0 {
				values.WriteByte(',')
			}
			values.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args,
				row.key.fromID, row.key.toID, row.key.kind, row.key.filePath, row.key.line,
				row.confidence, row.confidenceLabel, row.origin, row.tier,
				row.crossRepo, row.meta, row.resolveTerminal, row.resolveTerminalReason, row.semanticSource,
			)
		}
		query := `INSERT OR IGNORE INTO edges (
			from_id, to_id, kind, file_path, line,
			confidence, confidence_label, origin, tier, cross_repo, meta,
			resolve_terminal, resolve_terminal_reason, semantic_source
		) VALUES ` + values.String()
		result, err := tx.Exec(query, args...)
		if err != nil {
			return changed, statements, err
		}
		statements++
		inserted, err := result.RowsAffected()
		if err != nil {
			return changed, statements, err
		}
		changed += int(inserted)
	}
	return changed, statements, nil
}
