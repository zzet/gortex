package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/zzet/gortex/internal/graph"
)

// ScanUnresolvedEdgeIdentitiesBatched keyset-pages only the logical identities
// of unresolved edges in the requested kinds. Each page cursor and the active
// writer connection gate are released before yield runs, allowing the callback
// to exact-refetch and rewrite the same store safely.
func (s *Store) ScanUnresolvedEdgeIdentitiesBatched(
	kinds []graph.EdgeKind,
	batchSize int,
	yield func([]graph.EdgeIdentity) bool,
) {
	if yield == nil {
		return
	}
	kindValues := sqliteEdgeBatchKinds(kinds)
	if len(kindValues) == 0 {
		return
	}
	if batchSize <= 0 {
		batchSize = 1
	}
	if batchSize > maxEdgeKindScanBatch {
		batchSize = maxEdgeKindScanBatch
	}
	highWater, ok := s.edgeKindHighWater(kindValues)
	if !ok || highWater == 0 {
		return
	}

	var after int64
	for after < highWater {
		identities, next, ok := s.unresolvedEdgeIdentityPage(
			kindValues, after, highWater, batchSize,
		)
		if !ok || len(identities) == 0 || next <= after {
			return
		}
		after = next
		if !yield(identities) {
			return
		}
	}
}

// unresolvedIdentityPageSQL is one keyset page of the attribution pass's
// enumeration. The generation binds first so a pinned handle seeks into its own
// rows; the base corpus keeps the raw rowid walk it owns.
func unresolvedIdentityPageSQL(source, generation string, kinds int) string {
	return `SELECT id, from_id, to_id, kind, file_path, line
	FROM ` + source + `
	WHERE ` + generation + ` AND id > ? AND id <= ? AND kind IN (` + inPlaceholders(kinds) + `)
	  AND (substr(to_id, 1, 12) = 'unresolved::'
	       OR instr(to_id, '::unresolved::') > 0)
	ORDER BY id
	LIMIT ?`
}

func (s *Store) unresolvedEdgeIdentityPage(
	kinds []string,
	after, highWater int64,
	limit int,
) ([]graph.EdgeIdentity, int64, bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	source, generation := s.unresolvedScanSource(unresolvedIdentityBaseSource)
	args := make([]any, 0, len(kinds)+4)
	args = append(args, s.viewGen, after, highWater)
	args = append(args, toAnyArgs(kinds)...)
	args = append(args, limit)
	rows, err := s.queryActiveWriteLocked(
		context.Background(), unresolvedIdentityPageSQL(source, generation, len(kinds)), args...)
	if err != nil {
		panicOnFatal(err)
		return nil, after, false
	}
	defer rows.Close()

	identities := make([]graph.EdgeIdentity, 0, limit)
	next := after
	for rows.Next() {
		var (
			rowID    int64
			identity graph.EdgeIdentity
		)
		if err := rows.Scan(
			&rowID,
			&identity.From,
			&identity.To,
			&identity.Kind,
			&identity.FilePath,
			&identity.Line,
		); err != nil {
			panicOnFatal(err)
			return nil, after, false
		}
		if rowID > next {
			next = rowID
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
		return nil, after, false
	}
	return identities, next, true
}

const unresolvedTargetUpdateParamsPerRow = 6

type sqliteUnresolvedTargetMutation struct {
	oldKey sqliteReindexKey
	newTo  string
}

type sqliteUnresolvedTargetReindexStats struct {
	updateStatements int
	deleteStatements int
	updatedRows      int
	deletedRows      int
}

func (s *sqliteUnresolvedTargetReindexStats) add(other sqliteUnresolvedTargetReindexStats) {
	s.updateStatements += other.updateStatements
	s.deleteStatements += other.deleteStatements
	s.updatedRows += other.updatedRows
	s.deletedRows += other.deletedRows
}

func (s sqliteUnresolvedTargetReindexStats) writeStatements() int {
	return s.updateStatements + s.deleteStatements
}

// ReindexUnresolvedEdgeTargets updates only the target identity column for a
// compact unresolved-edge census. Full confidence, provenance, and metadata
// payloads remain in SQLite and are never decoded by this path.
func (s *Store) ReindexUnresolvedEdgeTargets(batch []graph.UnresolvedEdgeTargetReindex) {
	if _, err := s.reindexUnresolvedEdgeTargetsOriented(batch); err != nil {
		if errors.Is(err, errReindexWriterGateContended) {
			log.Printf("store_sqlite: unresolved target writer gate contended, dropping %d rebind(s) — a later resolve pass rebinds them: %v", len(batch), err)
			return
		}
		panicOnFatal(err)
	}
}

func (s *Store) reindexUnresolvedEdgeTargetsOriented(
	batch []graph.UnresolvedEdgeTargetReindex,
) (sqliteUnresolvedTargetReindexStats, error) {
	var stats sqliteUnresolvedTargetReindexStats
	mutations, err := sqliteUnresolvedTargetMutations(batch)
	if err != nil || len(mutations) == 0 {
		return stats, err
	}

	gateCtx, cancelGate := context.WithTimeout(context.Background(), s.sqliteBusyRetryWindow())
	gateErr := s.writeMu.LockContext(gateCtx)
	cancelGate()
	if gateErr != nil {
		return stats, fmt.Errorf("%w: %w", errReindexWriterGateContended, gateErr)
	}
	defer s.writeMu.Unlock()

	variableLimit := s.sqliteBatchVariableLimitLocked()
	defer func() {
		s.batchVariableLimit = variableLimit
	}()
	for txStart := 0; txStart < len(mutations); txStart += reindexChunkSize {
		txEnd := minInt(txStart+reindexChunkSize, len(mutations))
		var (
			txStats             sqliteUnresolvedTargetReindexStats
			changed             bool
			invalidatedAnalysis bool
		)
		err = s.withSQLiteBusyRetry(context.Background(), "reindex_unresolved_targets", func(ctx context.Context) error {
			var txErr error
			txStats, changed, invalidatedAnalysis, txErr = s.reindexUnresolvedEdgeTargetsTransactionLocked(
				ctx, mutations[txStart:txEnd], &variableLimit,
			)
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
	}
	return stats, nil
}

func (s *Store) reindexUnresolvedEdgeTargetsTransactionLocked(
	ctx context.Context,
	mutations []sqliteUnresolvedTargetMutation,
	variableLimit *int,
) (
	stats sqliteUnresolvedTargetReindexStats,
	changed bool,
	invalidatedAnalysis bool,
	err error,
) {
	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return stats, false, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var repairDeletes []sqliteReindexKey
	stats.updatedRows, stats.updateStatements, repairDeletes, err =
		updateSQLiteUnresolvedTargetsTxLimited(tx, s.viewGen, mutations, variableLimit)
	if err != nil {
		return stats, false, false, err
	}
	stats.deletedRows, stats.deleteStatements, err =
		deleteSQLiteReindexRowsTxLimited(tx, s.viewGen, repairDeletes, variableLimit)
	if err != nil {
		return stats, false, false, err
	}
	changed = stats.updatedRows > 0 || stats.deletedRows > 0
	if changed && s.analysisGenerationPresent {
		if err := invalidateAnalysisGenerationTx(tx); err != nil {
			return stats, false, false, err
		}
		invalidatedAnalysis = true
	}
	if err := tx.Commit(); err != nil {
		return stats, false, false, err
	}
	committed = true
	return stats, changed, invalidatedAnalysis, nil
}

func sqliteUnresolvedTargetMutations(
	batch []graph.UnresolvedEdgeTargetReindex,
) ([]sqliteUnresolvedTargetMutation, error) {
	mutations := make([]sqliteUnresolvedTargetMutation, 0, len(batch))
	seenOld := make(map[graph.EdgeIdentity]struct{}, len(batch))
	seenNew := make(map[graph.EdgeIdentity]struct{}, len(batch))
	for _, reindex := range batch {
		if !graph.IsUnresolvedTarget(reindex.Old.To) ||
			reindex.NewTo == "" || graph.IsUnresolvedTarget(reindex.NewTo) {
			return nil, fmt.Errorf(
				"store_sqlite: compact target reindex requires unresolved old and resolved new targets: %q -> %q",
				reindex.Old.To, reindex.NewTo,
			)
		}
		if _, duplicate := seenOld[reindex.Old]; duplicate {
			return nil, fmt.Errorf("store_sqlite: duplicate compact target source: %+v", reindex.Old)
		}
		seenOld[reindex.Old] = struct{}{}
		newIdentity := reindex.Old
		newIdentity.To = reindex.NewTo
		if _, duplicate := seenNew[newIdentity]; duplicate {
			return nil, fmt.Errorf("store_sqlite: duplicate compact target destination: %+v", newIdentity)
		}
		seenNew[newIdentity] = struct{}{}
		mutations = append(mutations, sqliteUnresolvedTargetMutation{
			oldKey: sqliteReindexKey{
				fromID: reindex.Old.From, toID: reindex.Old.To, kind: string(reindex.Old.Kind),
				filePath: reindex.Old.FilePath, line: reindex.Old.Line,
			},
			newTo: reindex.NewTo,
		})
	}
	return mutations, nil
}

func updateSQLiteUnresolvedTargetsTxLimited(
	tx *sql.Tx,
	viewGen int64,
	mutations []sqliteUnresolvedTargetMutation,
	variableLimit *int,
) (updatedRows, statements int, repairDeletes []sqliteReindexKey, err error) {
	if len(mutations) == 0 {
		return 0, 0, nil, nil
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	rowLimit := batchRowsForVariableLimit(
		*variableLimit, unresolvedTargetUpdateParamsPerRow, reindexRowMaxChunkSize,
	)
	for pos := 0; pos < len(mutations); {
		chunkStart := pos
		args := make([]any, 0, rowLimit*unresolvedTargetUpdateParamsPerRow+1)
		argBytes := 0
		rowCount := 0
		for pos < len(mutations) && rowCount < rowLimit {
			mutation := mutations[pos]
			argStart := len(args)
			args = append(args,
				mutation.oldKey.fromID, mutation.oldKey.toID, mutation.oldKey.kind,
				mutation.oldKey.filePath, mutation.oldKey.line, mutation.newTo,
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
		args = append(args, viewGen)

		query := `WITH patch(old_from_id, old_to_id, kind, file_path, line, new_to_id) AS (VALUES ` +
			multiValues(rowCount, unresolvedTargetUpdateParamsPerRow) + `)
		UPDATE OR IGNORE edges AS e
		SET to_id = p.new_to_id
		FROM patch AS p
		WHERE e.from_id = p.old_from_id
			AND e.to_id = p.old_to_id
			AND e.kind = p.kind
			AND e.file_path = p.file_path
			AND e.line = p.line
			AND e.view_gen = ?`
		result, execErr := tx.Exec(query, args...)
		if tooManySQLVariables(execErr) && rowCount > 1 {
			rowLimit = lowerBatchVariableLimit(variableLimit, unresolvedTargetUpdateParamsPerRow, rowCount)
			pos = chunkStart
			continue
		}
		if execErr != nil {
			return updatedRows, statements, repairDeletes, execErr
		}
		statements++
		affected64, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return updatedRows, statements, repairDeletes, affectedErr
		}
		affected := int(affected64)
		if affected < 0 || affected > rowCount {
			return updatedRows, statements, repairDeletes, fmt.Errorf(
				"store_sqlite: compact target update affected %d of %d rows", affected, rowCount,
			)
		}
		updatedRows += affected
		if affected != rowCount {
			// Successful updates no longer match their old identity, so deleting
			// the whole short chunk removes only collision losers. Missing source
			// rows remain missing and are never resurrected.
			for _, mutation := range mutations[chunkStart:pos] {
				repairDeletes = append(repairDeletes, mutation.oldKey)
			}
		}
	}
	return updatedRows, statements, repairDeletes, nil
}

var _ graph.UnresolvedEdgeIdentityBatchScanner = (*Store)(nil)
var _ graph.UnresolvedEdgeTargetBatchReindexer = (*Store)(nil)
