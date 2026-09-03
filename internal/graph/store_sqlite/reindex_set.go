package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

const (
	// Reindex transactions are simulated as one ordered set so intermediate
	// writes that cancel do not invalidate analysis or inflate receipts. SQL is
	// issued in VALUES relations bounded by the probed connection variable
	// limit and the shared argument-byte budget.
	reindexKeyParamsPerRow = 5
	reindexRowParamsPerRow = edgeInsertParams
	reindexRowMaxChunkSize = edgeInsertMaxChunkSize
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
	oldKey             sqliteReindexKey
	newRow             sqliteReindexRow
	resolvedConversion bool
}

type sqliteResolvedConversionPlan struct {
	oldUnresolved   bool
	oldCounts       map[sqliteReindexKey]int
	newCounts       map[sqliteReindexKey]int
	fallbackDeletes []sqliteReindexKey
	fallbackInserts []sqliteReindexRow
}

func (p sqliteResolvedConversionPlan) updateCandidate(mutation sqliteReindexMutation, updateKind bool) bool {
	if p.oldCounts[mutation.oldKey] != 1 || p.newCounts[mutation.newRow.key] != 1 {
		return false
	}
	return (mutation.oldKey.kind != mutation.newRow.key.kind) == updateKind
}

type sqliteReindexSetStats struct {
	selectStatements int
	updateStatements int
	deleteStatements int
	insertStatements int
	updatedRows      int
	deletedRows      int
	insertedRows     int
}

func (s sqliteReindexSetStats) writeStatements() int {
	return s.updateStatements + s.deleteStatements + s.insertStatements
}

func (s *sqliteReindexSetStats) add(other sqliteReindexSetStats) {
	s.selectStatements += other.selectStatements
	s.updateStatements += other.updateStatements
	s.deleteStatements += other.deleteStatements
	s.insertStatements += other.insertStatements
	s.updatedRows += other.updatedRows
	s.deletedRows += other.deletedRows
	s.insertedRows += other.insertedRows
}

func (s *Store) reindexEdgesSetOriented(batch []graph.EdgeReindex) (sqliteReindexSetStats, error) {
	var stats sqliteReindexSetStats
	if len(batch) == 0 {
		return stats, nil
	}

	gateCtx, cancelGate := context.WithTimeout(context.Background(), s.sqliteBusyRetryWindow())
	gateErr := s.writeMu.LockContext(gateCtx)
	cancelGate()
	if gateErr != nil {
		// Wrap the recoverable sentinel so ReindexEdges can tell a contended
		// gate (drop the batch, rebind later) apart from a fatal store error
		// (panic). gateErr stays wrapped too, so callers still see the
		// underlying context.DeadlineExceeded / Canceled.
		return stats, fmt.Errorf("%w: %w", errReindexWriterGateContended, gateErr)
	}
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
	mutations, err := sqliteReindexMutations(batch)
	if err != nil || len(mutations) == 0 {
		return stats, false, false, nil, err
	}

	// Probe before beginWriteContext checks out the writer connection. A fresh
	// single-connection Store cannot discover its limit while its own
	// transaction is holding that connection.
	variableLimit := s.sqliteBatchVariableLimitLocked()
	defer func() {
		// Persist a connection-specific fallback discovered while preparing any
		// of the three bounded reindex statement shapes.
		s.batchVariableLimit = variableLimit
	}()

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

	conversionPlan, resolvedConversionFastPath := sqliteResolvedConversionUpdatePlan(mutations)
	// A reverse conversion creates a new unresolved edge. While a mutation
	// receipt is active, retain the generic simulator's exact insert accounting:
	// UPDATE OR IGNORE reports only an aggregate row count and cannot distinguish
	// successful updates from destination collisions. Cold guard passes run
	// outside the receipt window and keep the in-place fast path.
	if resolvedConversionFastPath && receipt != nil && !conversionPlan.oldUnresolved {
		resolvedConversionFastPath = false
	}

	var deletes []sqliteReindexKey
	var inserts []sqliteReindexRow
	if resolvedConversionFastPath {
		deletes = conversionPlan.fallbackDeletes
		inserts = conversionPlan.fallbackInserts

		updated, statements, repairDeletes, repairInserts, updateErr :=
			updateSQLiteResolvedConversionsTxLimited(tx, s.viewGen, mutations, conversionPlan, false)
		if updateErr != nil {
			return stats, false, false, nil, updateErr
		}
		stats.updatedRows += updated
		stats.updateStatements += statements
		deletes = append(deletes, repairDeletes...)
		inserts = append(inserts, repairInserts...)

		updated, statements, repairDeletes, repairInserts, updateErr =
			updateSQLiteResolvedConversionsTxLimited(tx, s.viewGen, mutations, conversionPlan, true)
		if updateErr != nil {
			return stats, false, false, nil, updateErr
		}
		stats.updatedRows += updated
		stats.updateStatements += statements
		deletes = append(deletes, repairDeletes...)
		inserts = append(inserts, repairInserts...)
	} else {
		keys := sqliteReindexKeys(mutations)
		initial, selectStatements, selectErr := sqliteReindexRowsTxLimited(tx, s.viewGen, keys, &variableLimit)
		if selectErr != nil {
			return stats, false, false, nil, selectErr
		}
		stats.selectStatements = selectStatements
		deletes, inserts = simulateSQLiteReindexSet(initial, keys, mutations)
	}

	stats.deletedRows, stats.deleteStatements, err = deleteSQLiteReindexRowsTxLimited(tx, s.viewGen, deletes, &variableLimit)
	if err != nil {
		return stats, false, false, nil, err
	}
	stats.insertedRows, stats.insertStatements, err = insertSQLiteReindexRowsTxLimited(tx, s.viewGen, inserts, &variableLimit)
	if err != nil {
		return stats, false, false, nil, err
	}
	if !resolvedConversionFastPath && stats.insertedRows != len(inserts) {
		return stats, false, false, nil, fmt.Errorf(
			"store_sqlite: set reindex inserted %d of %d simulated rows",
			stats.insertedRows, len(inserts),
		)
	}
	// Every in-place candidate is unique across both its old and new logical
	// key. UPDATE OR IGNORE completes the common case in one write; a short
	// RowsAffected count repairs the whole bounded chunk through the existing
	// ordered delete/insert path. Converging and duplicate-old mutations always
	// take that fallback directly, preserving first-candidate-wins semantics.
	changed = stats.updatedRows > 0 || stats.deletedRows > 0 || stats.insertedRows > 0
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

func sqliteReindexMutations(batch []graph.EdgeReindex) ([]sqliteReindexMutation, error) {
	mutations := make([]sqliteReindexMutation, 0, len(batch))
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
		}
		// An entry whose identity did not move is NOT skipped: it still
		// carries payload. The resolver's terminal-clearing pass hands edges
		// back with an unchanged target and a cleared terminal stamp, and
		// skipping those would leave the stale stamp on the row, keeping the
		// edge permanently excluded from later resolution.
		//
		// Such an entry becomes a mutation whose old and new keys are equal.
		// The simulator compares it against the stored row and emits writes
		// only on a real difference, so an idempotent replay still touches
		// nothing.
		newRow, err := sqliteReindexRowForEdge(edge)
		if err != nil {
			return nil, err
		}
		oldKey := sqliteReindexKey{
			fromID: oldFrom, toID: reindex.OldTo, kind: string(oldKind),
			filePath: oldFilePath, line: oldLine,
		}
		mutations = append(mutations, sqliteReindexMutation{
			oldKey: oldKey,
			newRow: newRow,
			resolvedConversion: !reindex.RefreshIdentity &&
				graph.IsUnresolvedTarget(oldKey.toID) != graph.IsUnresolvedTarget(newRow.key.toID),
		})
	}
	return mutations, nil
}

// sqliteReindexKeys materializes the generic simulator's affected identity set.
// Namespace conversions avoid this allocation entirely; it is built only after
// the conversion planner rejects (or receipt safety disables) the fast path.
func sqliteReindexKeys(mutations []sqliteReindexMutation) []sqliteReindexKey {
	keys := make([]sqliteReindexKey, 0, len(mutations)*2)
	seen := make(map[sqliteReindexKey]struct{}, len(mutations)*2)
	add := func(key sqliteReindexKey) {
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for _, mutation := range mutations {
		add(mutation.oldKey)
		add(mutation.newRow.key)
	}
	return keys
}

// sqliteResolvedConversionUpdatePlan recognizes uniform transitions across the
// unresolved-target namespace boundary. It validates direction before building
// any count maps, then retains only the counts needed to identify one-to-one
// in-place updates. Duplicate old keys and converging destinations preserve the
// ordered delete/insert path; its map and slices stay nil on the common unique
// conversion path.
func sqliteResolvedConversionUpdatePlan(mutations []sqliteReindexMutation) (sqliteResolvedConversionPlan, bool) {
	if len(mutations) == 0 {
		return sqliteResolvedConversionPlan{}, false
	}
	oldUnresolved := graph.IsUnresolvedTarget(mutations[0].oldKey.toID)
	newUnresolved := graph.IsUnresolvedTarget(mutations[0].newRow.key.toID)
	if oldUnresolved == newUnresolved {
		return sqliteResolvedConversionPlan{}, false
	}
	for _, mutation := range mutations {
		if !mutation.resolvedConversion ||
			graph.IsUnresolvedTarget(mutation.oldKey.toID) != oldUnresolved ||
			graph.IsUnresolvedTarget(mutation.newRow.key.toID) != newUnresolved {
			return sqliteResolvedConversionPlan{}, false
		}
	}

	plan := sqliteResolvedConversionPlan{
		oldUnresolved: oldUnresolved,
		oldCounts:     make(map[sqliteReindexKey]int, len(mutations)),
		newCounts:     make(map[sqliteReindexKey]int, len(mutations)),
	}
	for _, mutation := range mutations {
		plan.oldCounts[mutation.oldKey]++
		plan.newCounts[mutation.newRow.key]++
	}

	var fallbackOld map[sqliteReindexKey]struct{}
	for _, mutation := range mutations {
		if plan.updateCandidate(mutation, mutation.oldKey.kind != mutation.newRow.key.kind) {
			continue
		}
		if fallbackOld == nil {
			fallbackOld = make(map[sqliteReindexKey]struct{})
		}
		if _, seen := fallbackOld[mutation.oldKey]; !seen {
			fallbackOld[mutation.oldKey] = struct{}{}
			plan.fallbackDeletes = append(plan.fallbackDeletes, mutation.oldKey)
		}
		// Do not deduplicate destinations: INSERT OR IGNORE preserves the
		// generic simulator's input-order, first-candidate-wins contract.
		plan.fallbackInserts = append(plan.fallbackInserts, mutation.newRow)
	}
	return plan, true
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

func updateSQLiteResolvedConversionsTxLimited(
	tx *sql.Tx,
	viewGen int64,
	mutations []sqliteReindexMutation,
	plan sqliteResolvedConversionPlan,
	updateKind bool,
) (
	updatedRows int,
	statements int,
	repairDeletes []sqliteReindexKey,
	repairInserts []sqliteReindexRow,
	err error,
) {
	if len(mutations) == 0 {
		return 0, 0, nil, nil, nil
	}
	candidates := make([]sqliteReindexMutation, 0, len(mutations))
	for _, mutation := range mutations {
		if !plan.updateCandidate(mutation, updateKind) {
			continue
		}
		if !jsonSafeResolvedConversion(mutation) {
			// json.Marshal would corrupt this row (invalid UTF-8 rewrites to
			// U+FFFD — fatal on a SET column, where the mangled value would
			// commit — and a non-finite confidence fails the whole Marshal).
			// The repair path binds raw bytes and keeps it exact.
			repairDeletes = append(repairDeletes, mutation.oldKey)
			repairInserts = append(repairInserts, mutation.newRow)
			continue
		}
		candidates = append(candidates, mutation)
	}
	query := sqliteResolvedConversionUpdateJSONStatement(updateKind)
	for start := 0; start < len(candidates); {
		payload, rowCount, encodeErr := encodeResolvedConversionRows(candidates[start:], updateKind)
		if encodeErr != nil {
			return updatedRows, statements, repairDeletes, repairInserts, encodeErr
		}
		result, execErr := tx.Exec(query, payload, viewGen)
		if execErr != nil {
			return updatedRows, statements, repairDeletes, repairInserts, execErr
		}
		statements++
		affected64, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return updatedRows, statements, repairDeletes, repairInserts, affectedErr
		}
		affected := int(affected64)
		if affected < 0 || affected > rowCount {
			return updatedRows, statements, repairDeletes, repairInserts, fmt.Errorf(
				"store_sqlite: resolved conversion update affected %d of %d rows",
				affected, rowCount,
			)
		}
		updatedRows += affected
		if affected != rowCount {
			// UPDATE OR IGNORE cannot identify which rows were skipped without
			// materializing RETURNING rows. Replaying the whole bounded chunk is
			// exact: successful rows already occupy their destination, while
			// missing sources and destination collisions are repaired below.
			for _, mutation := range candidates[start : start+rowCount] {
				repairDeletes = append(repairDeletes, mutation.oldKey)
				repairInserts = append(repairInserts, mutation.newRow)
			}
		}
		start += rowCount
	}
	return updatedRows, statements, repairDeletes, repairInserts, nil
}

// jsonSafeResolvedConversion reports whether every value the JSON transport
// would carry survives json.Marshal byte-exact: strings must be valid UTF-8
// (Marshal rewrites invalid bytes to U+FFFD — on a WHERE column that is a
// self-healing miss, but on a SET column the mangled value would commit) and
// confidence must be finite (Marshal rejects NaN/Inf outright). meta is
// exempt — it rides hex-armored.
func jsonSafeResolvedConversion(mutation sqliteReindexMutation) bool {
	row := mutation.newRow
	if math.IsNaN(row.confidence) || math.IsInf(row.confidence, 0) {
		return false
	}
	for _, s := range []string{
		mutation.oldKey.fromID, mutation.oldKey.toID, mutation.oldKey.kind,
		mutation.oldKey.filePath, row.key.toID, row.key.kind,
		row.confidenceLabel, row.origin, row.tier,
		row.resolveTerminalReason.String, row.semanticSource.String,
	} {
		if !utf8.ValidString(s) {
			return false
		}
	}
	return true
}

// encodeResolvedConversionRows marshals a bounded prefix of the candidates as
// one json_each relation: positional arrays in patch-column order. meta rides
// hex-encoded — its compact binary encoding is not UTF-8-safe inside JSON,
// and unhex() restores the exact bytes — while NULL-able columns become JSON
// null. Bounded by reindexRowMaxChunkSize rows and sqliteBatchMaxBoundBytes
// of estimated payload; the first row is always taken, so every call makes
// progress. The estimate counts raw bytes and ignores JSON escaping
// inflation (~2x on backslash-heavy ids) — it is a soft memory bound, far
// below SQLite's bound-length limit, not an exact guard.
func encodeResolvedConversionRows(candidates []sqliteReindexMutation, updateKind bool) (string, int, error) {
	rows := make([]any, 0, minInt(len(candidates), reindexRowMaxChunkSize))
	estimated := 0
	for _, mutation := range candidates {
		if len(rows) >= reindexRowMaxChunkSize {
			break
		}
		size := resolvedConversionRowEstimate(mutation)
		if len(rows) > 0 && estimated+size > sqliteBatchMaxBoundBytes {
			break
		}
		rows = append(rows, resolvedConversionJSONRow(mutation, updateKind))
		estimated += size
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return "", 0, err
	}
	return string(encoded), len(rows), nil
}

func resolvedConversionRowEstimate(mutation sqliteReindexMutation) int {
	row := mutation.newRow
	return len(mutation.oldKey.fromID) + len(mutation.oldKey.toID) +
		len(mutation.oldKey.kind) + len(mutation.oldKey.filePath) +
		len(row.key.toID) + len(row.key.kind) + len(row.confidenceLabel) +
		len(row.origin) + len(row.tier) + 2*len(row.meta) +
		len(row.resolveTerminalReason.String) + len(row.semanticSource.String) + 64
}

func resolvedConversionJSONRow(mutation sqliteReindexMutation, updateKind bool) []any {
	row := mutation.newRow
	out := make([]any, 0, 16)
	out = append(out,
		mutation.oldKey.fromID, mutation.oldKey.toID, mutation.oldKey.kind,
		mutation.oldKey.filePath, mutation.oldKey.line, row.key.toID,
	)
	if updateKind {
		out = append(out, row.key.kind)
	}
	var meta any
	if row.meta != nil {
		meta = hex.EncodeToString(row.meta)
	}
	var terminal any
	if row.resolveTerminal.Valid {
		if row.resolveTerminal.Bool {
			terminal = 1
		} else {
			terminal = 0
		}
	}
	var reason any
	if row.resolveTerminalReason.Valid {
		reason = row.resolveTerminalReason.String
	}
	var semantic any
	if row.semanticSource.Valid {
		semantic = row.semanticSource.String
	}
	out = append(out,
		row.confidence, row.confidenceLabel, row.origin, row.tier,
		row.crossRepo, meta, terminal, reason, semantic,
	)
	return out
}

// sqliteResolvedConversionUpdateJSONStatement is the resolved-conversion
// update arm over a json_each relation: the whole chunk rides ONE bound
// variable, where the per-row VALUES form paid the driver's bind cost for
// 15-16 parameters per row. meta travels hex-encoded (binary; see
// encodeResolvedConversionRows) and unhex() restores the exact bytes; JSON
// numbers and nulls land as INTEGER/REAL/NULL, with column affinity settling
// confidence to REAL.
func sqliteResolvedConversionUpdateJSONStatement(updateKind bool) string {
	if updateKind {
		return `WITH patch AS (SELECT
		value ->> 0 AS old_from_id,
		value ->> 1 AS old_to_id,
		value ->> 2 AS old_kind,
		value ->> 3 AS file_path,
		value ->> 4 AS line,
		value ->> 5 AS new_to_id,
		value ->> 6 AS new_kind,
		CAST(value ->> 7 AS REAL) AS confidence,
		value ->> 8 AS confidence_label,
		value ->> 9 AS origin,
		value ->> 10 AS tier,
		value ->> 11 AS cross_repo,
		unhex(value ->> 12) AS meta,
		value ->> 13 AS resolve_terminal,
		value ->> 14 AS resolve_terminal_reason,
		value ->> 15 AS semantic_source
	FROM json_each(?))
	UPDATE OR IGNORE edges AS e
	SET to_id = p.new_to_id,
		kind = p.new_kind,
		confidence = p.confidence,
		confidence_label = p.confidence_label,
		origin = p.origin,
		tier = p.tier,
		cross_repo = p.cross_repo,
		meta = p.meta,
		resolve_terminal = p.resolve_terminal,
		resolve_terminal_reason = p.resolve_terminal_reason,
		semantic_source = p.semantic_source
	FROM patch AS p
	WHERE e.from_id = p.old_from_id
		AND e.to_id = p.old_to_id
		AND e.kind = p.old_kind
		AND e.file_path = p.file_path
		AND e.line = p.line
		AND e.view_gen = ?`
	}
	return `WITH patch AS (SELECT
		value ->> 0 AS old_from_id,
		value ->> 1 AS old_to_id,
		value ->> 2 AS kind,
		value ->> 3 AS file_path,
		value ->> 4 AS line,
		value ->> 5 AS new_to_id,
		CAST(value ->> 6 AS REAL) AS confidence,
		value ->> 7 AS confidence_label,
		value ->> 8 AS origin,
		value ->> 9 AS tier,
		value ->> 10 AS cross_repo,
		unhex(value ->> 11) AS meta,
		value ->> 12 AS resolve_terminal,
		value ->> 13 AS resolve_terminal_reason,
		value ->> 14 AS semantic_source
	FROM json_each(?))
	UPDATE OR IGNORE edges AS e
	SET to_id = p.new_to_id,
		confidence = p.confidence,
		confidence_label = p.confidence_label,
		origin = p.origin,
		tier = p.tier,
		cross_repo = p.cross_repo,
		meta = p.meta,
		resolve_terminal = p.resolve_terminal,
		resolve_terminal_reason = p.resolve_terminal_reason,
		semantic_source = p.semantic_source
	FROM patch AS p
	WHERE e.from_id = p.old_from_id
		AND e.to_id = p.old_to_id
		AND e.kind = p.kind
		AND e.file_path = p.file_path
		AND e.line = p.line
		AND e.view_gen = ?`
}

// sqliteReindexRowsTxLimited reads the stored rows the simulator compares
// against. It binds the generation the deletes and inserts below use, so the
// simulated before-state and the writes that act on it describe one corpus.
func sqliteReindexRowsTxLimited(tx *sql.Tx, viewGen int64, keys []sqliteReindexKey, variableLimit *int) (map[sqliteReindexKey]sqliteReindexRow, int, error) {
	out := make(map[sqliteReindexKey]sqliteReindexRow, len(keys))
	if len(keys) == 0 {
		return out, 0, nil
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	rowLimit := batchRowsForVariableLimit(*variableLimit, reindexKeyParamsPerRow, len(keys))
	statements := 0
	for pos := 0; pos < len(keys); {
		chunkStart := pos
		args := make([]any, 0, rowLimit*reindexKeyParamsPerRow+1)
		argBytes := 0
		rowCount := 0
		for pos < len(keys) && rowCount < rowLimit {
			key := keys[pos]
			argStart := len(args)
			args = append(args, key.fromID, key.toID, key.kind, key.filePath, key.line)
			rowBytes := sqliteBoundArgsBytes(args[argStart:])
			if rowCount > 0 && argBytes+rowBytes > sqliteBatchMaxBoundBytes {
				args = args[:argStart]
				break
			}
			pos++
			rowCount++
			argBytes += rowBytes
		}
		args = append(args, viewGen)

		query := `WITH wanted(from_id, to_id, kind, file_path, line) AS (VALUES ` + multiValues(rowCount, reindexKeyParamsPerRow) + `)
		SELECT e.from_id, e.to_id, e.kind, e.file_path, e.line,
			e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo,
			e.meta, e.resolve_terminal, e.resolve_terminal_reason, e.semantic_source
		FROM wanted AS w
		JOIN edges AS e
		  ON e.from_id = w.from_id
		 AND e.to_id = w.to_id
		 AND e.kind = w.kind
		 AND e.file_path = w.file_path
		 AND e.line = w.line
		 AND e.view_gen = ?`
		rows, err := tx.Query(query, args...)
		if tooManySQLVariables(err) && rowCount > 1 {
			rowLimit = lowerBatchVariableLimit(variableLimit, reindexKeyParamsPerRow, rowCount)
			pos = chunkStart
			continue
		}
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

func deleteSQLiteReindexRowsTxLimited(tx *sql.Tx, viewGen int64, keys []sqliteReindexKey, variableLimit *int) (int, int, error) {
	if len(keys) == 0 {
		return 0, 0, nil
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	rowLimit := batchRowsForVariableLimit(*variableLimit, reindexKeyParamsPerRow, len(keys))
	changed := 0
	statements := 0
	for pos := 0; pos < len(keys); {
		chunkStart := pos
		args := make([]any, 0, rowLimit*reindexKeyParamsPerRow+1)
		argBytes := 0
		rowCount := 0
		for pos < len(keys) && rowCount < rowLimit {
			key := keys[pos]
			argStart := len(args)
			args = append(args, key.fromID, key.toID, key.kind, key.filePath, key.line)
			rowBytes := sqliteBoundArgsBytes(args[argStart:])
			if rowCount > 0 && argBytes+rowBytes > sqliteBatchMaxBoundBytes {
				args = args[:argStart]
				break
			}
			pos++
			rowCount++
			argBytes += rowBytes
		}
		args = append(args, viewGen)

		query := `WITH doomed(from_id, to_id, kind, file_path, line) AS (VALUES ` + multiValues(rowCount, reindexKeyParamsPerRow) + `)
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
			 AND e.view_gen = ?
		)`
		result, err := tx.Exec(query, args...)
		if tooManySQLVariables(err) && rowCount > 1 {
			rowLimit = lowerBatchVariableLimit(variableLimit, reindexKeyParamsPerRow, rowCount)
			pos = chunkStart
			continue
		}
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

func insertSQLiteReindexRowsTxLimited(tx *sql.Tx, viewGen int64, rows []sqliteReindexRow, variableLimit *int) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	rowLimit := batchRowsForVariableLimit(*variableLimit, reindexRowParamsPerRow, reindexRowMaxChunkSize)
	changed := 0
	statements := 0
	for pos := 0; pos < len(rows); {
		chunkStart := pos
		args := make([]any, 0, rowLimit*reindexRowParamsPerRow)
		argBytes := 0
		rowCount := 0
		for pos < len(rows) && rowCount < rowLimit {
			row := rows[pos]
			argStart := len(args)
			args = append(args,
				viewGen,
				row.key.fromID, row.key.toID, row.key.kind, row.key.filePath, row.key.line,
				row.confidence, row.confidenceLabel, row.origin, row.tier,
				row.crossRepo, row.meta, row.resolveTerminal, row.resolveTerminalReason, row.semanticSource,
			)
			rowBytes := sqliteBoundArgsBytes(args[argStart:])
			if rowCount > 0 && argBytes+rowBytes > sqliteBatchMaxBoundBytes {
				args = args[:argStart]
				break
			}
			pos++
			rowCount++
			argBytes += rowBytes
		}

		query := `INSERT OR IGNORE INTO edges (` + edgeInsertColumns + `) VALUES ` + multiValues(rowCount, reindexRowParamsPerRow)
		result, err := tx.Exec(query, args...)
		if tooManySQLVariables(err) && rowCount > 1 {
			rowLimit = lowerBatchVariableLimit(variableLimit, reindexRowParamsPerRow, rowCount)
			pos = chunkStart
			continue
		}
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
