package store_sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	modernsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// builtinSeenKey namespaces a materialised builtin stub by the generation it
// was written into.
func builtinSeenKey(viewGen int64, id string) string {
	return strconv.FormatInt(viewGen, 10) + "\x00" + id
}

// view_gen leads both insert column lists: it is part of the nodes primary key
// and of the edges dedup key, and every writer binds the handle's pinned
// generation for it. It is NOT part of the read column lists — reads scan a row
// into a graph.Node / graph.Edge, which carry no generation.
const (
	nodeInsertColumns = viewGenColumnName + ", " + lookupNodeCols
	nodeInsertParams  = 36
	// The package-private compatibility writer retains the conservative shape.
	nodeInsertChunkSize = 35
	// AddBatch uses the runtime SQLite variable limit, but never lets one node
	// statement retain more than 256 rows or the shared byte cap below.
	nodeInsertMaxChunkSize = 256

	edgeInsertColumns = viewGenColumnName + ", " + lookupEdgeCols
	edgeInsertParams  = 15
	// The package-private compatibility writer retains the conservative shape.
	// Edges carry half as many parameters as nodes, so the same variable and
	// byte budgets permit twice as many rows per statement.
	edgeInsertMaxChunkSize = 512

	// SQLite historically defaulted to 999 variables. Modern builds allow far
	// more, but row and byte caps keep statement preparation and driver argument
	// retention bounded even when SQLITE_LIMIT_VARIABLE_NUMBER is very large.
	sqliteFallbackVariableLimit = 999
	sqliteBatchVariableHeadroom = 16
	sqliteBatchVariableHardCap  = 8192
	sqliteBatchMaxBoundBytes    = 4 << 20
)

// nodeUpsertClause is shared by the single-row prepared statement and the
// bounded multi-row AddBatch writer. Keeping one conflict predicate preserves
// exact no-op/change semantics across both paths. The conflict target names the
// whole nodes primary key: a write for one generation must update that
// generation's row, never another's.
const nodeUpsertClause = ` ON CONFLICT(id, view_gen) DO UPDATE SET
kind=excluded.kind, name=excluded.name, qual_name=excluded.qual_name,
file_path=excluded.file_path, start_line=excluded.start_line, end_line=excluded.end_line,
start_column=excluded.start_column, end_column=excluded.end_column,
language=excluded.language, repo_prefix=excluded.repo_prefix,
workspace_id=excluded.workspace_id, project_id=excluded.project_id,
signature=excluded.signature, visibility=excluded.visibility, doc=excluded.doc,
external=excluded.external, return_type=excluded.return_type,
is_async=excluded.is_async, is_static=excluded.is_static,
is_abstract=excluded.is_abstract, is_exported=excluded.is_exported,
updated_at=excluded.updated_at, data_class=excluded.data_class,
semantic_type=excluded.semantic_type, semantic_source=excluded.semantic_source,
clone_sig=COALESCE(excluded.clone_sig, nodes.clone_sig),
entry_point=excluded.entry_point, entry_point_kind=excluded.entry_point_kind, meta=excluded.meta,
search_signature=excluded.search_signature, search_qual_name=excluded.search_qual_name,
search_doc=excluded.search_doc, search_metadata_suppressed=excluded.search_metadata_suppressed,
section_text=excluded.section_text
WHERE nodes.kind IS NOT excluded.kind OR nodes.name IS NOT excluded.name OR
      nodes.qual_name IS NOT excluded.qual_name OR nodes.file_path IS NOT excluded.file_path OR
      nodes.start_line IS NOT excluded.start_line OR nodes.end_line IS NOT excluded.end_line OR
      nodes.start_column IS NOT excluded.start_column OR nodes.end_column IS NOT excluded.end_column OR
      nodes.language IS NOT excluded.language OR nodes.repo_prefix IS NOT excluded.repo_prefix OR
      nodes.workspace_id IS NOT excluded.workspace_id OR nodes.project_id IS NOT excluded.project_id OR
      nodes.signature IS NOT excluded.signature OR nodes.visibility IS NOT excluded.visibility OR
      nodes.doc IS NOT excluded.doc OR nodes.external IS NOT excluded.external OR
      nodes.return_type IS NOT excluded.return_type OR nodes.is_async IS NOT excluded.is_async OR
      nodes.is_static IS NOT excluded.is_static OR nodes.is_abstract IS NOT excluded.is_abstract OR
      nodes.is_exported IS NOT excluded.is_exported OR nodes.updated_at IS NOT excluded.updated_at OR
      nodes.data_class IS NOT excluded.data_class OR nodes.semantic_type IS NOT excluded.semantic_type OR
      nodes.semantic_source IS NOT excluded.semantic_source OR
      (excluded.clone_sig IS NOT NULL AND nodes.clone_sig IS NOT excluded.clone_sig) OR
      nodes.entry_point IS NOT excluded.entry_point OR
      nodes.entry_point_kind IS NOT excluded.entry_point_kind OR
      nodes.search_signature IS NOT excluded.search_signature OR
      nodes.search_qual_name IS NOT excluded.search_qual_name OR
      nodes.search_doc IS NOT excluded.search_doc OR
      nodes.search_metadata_suppressed IS NOT excluded.search_metadata_suppressed OR
      nodes.section_text IS NOT excluded.section_text OR
      nodes.meta IS NOT excluded.meta`

type sqliteAddBatchStats struct {
	nodeStatements         int
	edgeStatements         int
	nodeRowsChanged        int
	edgeRowsInserted       int
	analysisNodeStatements int
	analysisEdgeStatements int
}

// sqliteBatchVariableLimitLocked reads the actual runtime limit from the
// writer connection. Coordinated cold loading has a pinned connection, so the
// probe and every subsequent AddBatch transaction observe the same limit.
// The hard cap is a memory policy, not a SQLite assumption.
func (s *Store) sqliteBatchVariableLimitLocked() int {
	if s.batchVariableLimit > 0 {
		return s.batchVariableLimit
	}

	limit := sqliteFallbackVariableLimit
	conn := s.bulkConn
	release := false
	if conn == nil {
		var err error
		conn, err = s.writerDB.Conn(context.Background())
		if err == nil {
			release = true
		}
	}
	if conn != nil {
		if observed, err := modernsqlite.Limit(conn, int(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER), -1); err == nil && observed > 0 {
			limit = observed
		}
	}
	if release {
		_ = conn.Close()
	}
	if limit > sqliteBatchVariableHardCap {
		limit = sqliteBatchVariableHardCap
	}
	s.batchVariableLimit = limit
	return limit
}

func batchRowsForVariableLimit(variableLimit, paramsPerRow, maxRows int) int {
	usable := variableLimit - sqliteBatchVariableHeadroom
	if usable < paramsPerRow {
		return 1
	}
	rows := usable / paramsPerRow
	if rows > maxRows {
		rows = maxRows
	}
	if rows < 1 {
		return 1
	}
	return rows
}

func sqliteBoundArgsBytes(args []any) int {
	bytes := 0
	for _, arg := range args {
		// Count driver/interface retention as well as variable payload. This is
		// deliberately conservative; the cap protects memory, not wire format.
		bytes += 16
		switch value := arg.(type) {
		case string:
			bytes += len(value)
		case []byte:
			bytes += len(value)
		case nil:
		case bool:
			bytes++
		default:
			bytes += 8
		}
	}
	return bytes
}

func tooManySQLVariables(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "too many sql variables")
}

// lowerBatchVariableLimit halves the failed statement shape and persists a
// conservative inferred limit. The next AddBatch therefore avoids repeating
// an oversized prepare even if a connection-specific runtime limit was lower
// than the initial writer probe.
func lowerBatchVariableLimit(variableLimit *int, paramsPerRow, failedRows int) int {
	rows := failedRows / 2
	if rows < 1 {
		rows = 1
	}
	inferred := rows*paramsPerRow + sqliteBatchVariableHeadroom
	if inferred < *variableLimit {
		*variableLimit = inferred
	}
	return rows
}

func appendNodeInsertArgs(args []any, viewGen int64, n *graph.Node) ([]any, error) {
	promoted, blobMeta := extractPromotedMeta(stripCloneShingles(n.Meta))
	metaBlob, err := encodeMeta(blobMeta)
	if err != nil {
		return nil, err
	}
	return append(args,
		viewGen,
		n.ID, string(n.Kind), n.Name, n.QualName, n.FilePath,
		n.StartLine, n.EndLine, n.StartColumn, n.EndColumn, n.Language,
		n.RepoPrefix, n.WorkspaceID, n.ProjectID,
		promoted.sig, promoted.vis, promoted.doc, promoted.external, promoted.returnType,
		promoted.isAsync, promoted.isStatic, promoted.isAbstract, promoted.isExported, promoted.updatedAt,
		promoted.dataClass, promoted.semanticType, promoted.semanticSource, promoted.cloneSig,
		promoted.entryPoint, promoted.entryPointKind, metaBlob,
		promoted.searchSig, promoted.searchQualName, promoted.searchDoc,
		promoted.searchSuppressed, promoted.sectionText,
	), nil
}

func appendEdgeInsertArgs(args []any, viewGen int64, e *graph.Edge) ([]any, error) {
	promoted, blobMeta := extractPromotedEdgeMeta(e.Meta)
	metaBlob, err := encodeMeta(blobMeta)
	if err != nil {
		return nil, err
	}
	var crossRepo int64
	if e.CrossRepo {
		crossRepo = 1
	}
	return append(args,
		viewGen,
		e.From, e.To, string(e.Kind), e.FilePath, e.Line,
		e.Confidence, e.ConfidenceLabel, e.Origin, e.Tier,
		crossRepo, metaBlob, promoted.resolveTerminal, promoted.resolveTerminalReason, promoted.semanticSource,
	), nil
}

func multiValues(rows, params int) string {
	var values strings.Builder
	values.Grow(rows * (params*2 + 2))
	for row := 0; row < rows; row++ {
		if row > 0 {
			values.WriteByte(',')
		}
		values.WriteByte('(')
		for param := 0; param < params; param++ {
			if param > 0 {
				values.WriteByte(',')
			}
			values.WriteByte('?')
		}
		values.WriteByte(')')
	}
	return values.String()
}

func insertNodeChunksTx(tx *sql.Tx, viewGen int64, nodes []*graph.Node, returnChanged bool) (rowsChanged, statements int, changedIDs map[string]int, err error) {
	variableLimit := sqliteFallbackVariableLimit
	return insertNodeChunksTxLimited(tx, viewGen, nodes, returnChanged, &variableLimit)
}

func insertNodeChunksTxLimited(tx *sql.Tx, viewGen int64, nodes []*graph.Node, returnChanged bool, variableLimit *int) (rowsChanged, statements int, changedIDs map[string]int, err error) {
	if returnChanged {
		changedIDs = make(map[string]int)
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	rowLimit := batchRowsForVariableLimit(*variableLimit, nodeInsertParams, nodeInsertMaxChunkSize)
	var fullStmt *sql.Stmt
	defer func() {
		if fullStmt != nil {
			_ = fullStmt.Close()
		}
	}()

	for pos := 0; pos < len(nodes); {
		chunkStart := pos
		chunk := make([]*graph.Node, 0, rowLimit)
		args := make([]any, 0, rowLimit*nodeInsertParams)
		argBytes := 0
		for pos < len(nodes) && len(chunk) < rowLimit {
			node := nodes[pos]
			if node == nil || node.ID == "" || graph.IsProxyNode(node) {
				pos++
				continue
			}
			candidate, appendErr := appendNodeInsertArgs(args, viewGen, node)
			if appendErr != nil {
				return rowsChanged, statements, changedIDs, appendErr
			}
			rowBytes := sqliteBoundArgsBytes(candidate[len(args):])
			if len(chunk) > 0 && argBytes+rowBytes > sqliteBatchMaxBoundBytes {
				break
			}
			pos++
			args = candidate
			argBytes += rowBytes
			chunk = append(chunk, node)
		}
		if len(chunk) == 0 {
			continue
		}

		query := `INSERT INTO nodes (` + nodeInsertColumns + `) VALUES ` + multiValues(len(chunk), nodeInsertParams) + nodeUpsertClause
		if returnChanged {
			query += ` RETURNING id`
		}
		statements++

		retryAtLowerLimit := func(queryErr error) bool {
			if !tooManySQLVariables(queryErr) || len(chunk) <= 1 {
				return false
			}
			rowLimit = lowerBatchVariableLimit(variableLimit, nodeInsertParams, len(chunk))
			pos = chunkStart
			if fullStmt != nil {
				_ = fullStmt.Close()
				fullStmt = nil
			}
			return true
		}

		var stmt *sql.Stmt
		if len(chunk) == rowLimit {
			if fullStmt == nil {
				fullStmt, err = tx.Prepare(query)
				if err != nil {
					if retryAtLowerLimit(err) {
						continue
					}
					return rowsChanged, statements, changedIDs, err
				}
			}
			stmt = fullStmt
		}

		if returnChanged {
			var rows *sql.Rows
			if stmt != nil {
				rows, err = stmt.Query(args...)
			} else {
				rows, err = tx.Query(query, args...)
			}
			if err != nil {
				if retryAtLowerLimit(err) {
					continue
				}
				return rowsChanged, statements, changedIDs, err
			}
			for rows.Next() {
				var id string
				if scanErr := rows.Scan(&id); scanErr != nil {
					_ = rows.Close()
					return rowsChanged, statements, changedIDs, scanErr
				}
				rowsChanged++
				changedIDs[id]++
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				_ = rows.Close()
				return rowsChanged, statements, changedIDs, rowsErr
			}
			if closeErr := rows.Close(); closeErr != nil {
				return rowsChanged, statements, changedIDs, closeErr
			}
			continue
		}

		var result sql.Result
		if stmt != nil {
			result, err = stmt.Exec(args...)
		} else {
			result, err = tx.Exec(query, args...)
		}
		if err != nil {
			if retryAtLowerLimit(err) {
				continue
			}
			return rowsChanged, statements, changedIDs, err
		}
		changed, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsChanged, statements, changedIDs, rowsErr
		}
		rowsChanged += int(changed)
	}
	return rowsChanged, statements, changedIDs, nil
}

func insertEdgeChunksTx(tx *sql.Tx, viewGen int64, edges []*graph.Edge, returnInserted bool) (rowsInserted, statements int, insertedKeys map[sqliteEdgeIdentity]int, err error) {
	variableLimit := sqliteFallbackVariableLimit
	return insertEdgeChunksTxLimited(tx, viewGen, edges, returnInserted, &variableLimit)
}

func insertEdgeChunksTxLimited(tx *sql.Tx, viewGen int64, edges []*graph.Edge, returnInserted bool, variableLimit *int) (rowsInserted, statements int, insertedKeys map[sqliteEdgeIdentity]int, err error) {
	if returnInserted {
		insertedKeys = make(map[sqliteEdgeIdentity]int)
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	rowLimit := batchRowsForVariableLimit(*variableLimit, edgeInsertParams, edgeInsertMaxChunkSize)
	var fullStmt *sql.Stmt
	defer func() {
		if fullStmt != nil {
			_ = fullStmt.Close()
		}
	}()

	for pos := 0; pos < len(edges); {
		chunkStart := pos
		chunk := make([]*graph.Edge, 0, rowLimit)
		args := make([]any, 0, rowLimit*edgeInsertParams)
		argBytes := 0
		for pos < len(edges) && len(chunk) < rowLimit {
			edge := edges[pos]
			if edge == nil || graph.IsProxyID(edge.From) || graph.IsProxyID(edge.To) {
				pos++
				continue
			}
			candidate, appendErr := appendEdgeInsertArgs(args, viewGen, edge)
			if appendErr != nil {
				return rowsInserted, statements, insertedKeys, appendErr
			}
			rowBytes := sqliteBoundArgsBytes(candidate[len(args):])
			if len(chunk) > 0 && argBytes+rowBytes > sqliteBatchMaxBoundBytes {
				break
			}
			pos++
			args = candidate
			argBytes += rowBytes
			chunk = append(chunk, edge)
		}
		if len(chunk) == 0 {
			continue
		}

		query := `INSERT OR IGNORE INTO edges (` + edgeInsertColumns + `) VALUES ` + multiValues(len(chunk), edgeInsertParams)
		if returnInserted {
			query += ` RETURNING from_id, to_id, kind, file_path, line`
		}
		statements++

		retryAtLowerLimit := func(queryErr error) bool {
			if !tooManySQLVariables(queryErr) || len(chunk) <= 1 {
				return false
			}
			rowLimit = lowerBatchVariableLimit(variableLimit, edgeInsertParams, len(chunk))
			pos = chunkStart
			if fullStmt != nil {
				_ = fullStmt.Close()
				fullStmt = nil
			}
			return true
		}

		var stmt *sql.Stmt
		if len(chunk) == rowLimit {
			if fullStmt == nil {
				fullStmt, err = tx.Prepare(query)
				if err != nil {
					if retryAtLowerLimit(err) {
						continue
					}
					return rowsInserted, statements, insertedKeys, err
				}
			}
			stmt = fullStmt
		}

		if returnInserted {
			var rows *sql.Rows
			if stmt != nil {
				rows, err = stmt.Query(args...)
			} else {
				rows, err = tx.Query(query, args...)
			}
			if err != nil {
				if retryAtLowerLimit(err) {
					continue
				}
				return rowsInserted, statements, insertedKeys, err
			}
			for rows.Next() {
				var key sqliteEdgeIdentity
				if scanErr := rows.Scan(&key.from, &key.to, &key.kind, &key.filePath, &key.line); scanErr != nil {
					_ = rows.Close()
					return rowsInserted, statements, insertedKeys, scanErr
				}
				rowsInserted++
				insertedKeys[key]++
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				_ = rows.Close()
				return rowsInserted, statements, insertedKeys, rowsErr
			}
			if closeErr := rows.Close(); closeErr != nil {
				return rowsInserted, statements, insertedKeys, closeErr
			}
			continue
		}

		var result sql.Result
		if stmt != nil {
			result, err = stmt.Exec(args...)
		} else {
			result, err = tx.Exec(query, args...)
		}
		if err != nil {
			if retryAtLowerLimit(err) {
				continue
			}
			return rowsInserted, statements, insertedKeys, err
		}
		inserted, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsInserted, statements, insertedKeys, rowsErr
		}
		rowsInserted += int(inserted)
	}
	return rowsInserted, statements, insertedKeys, nil
}

func (s *Store) addBatchSetOriented(nodes []*graph.Node, edges []*graph.Edge) (sqliteAddBatchStats, error) {
	var stats sqliteAddBatchStats
	if len(nodes) == 0 && len(edges) == 0 {
		return stats, nil
	}
	// Structural-shape backstop covering every SQLite ingest path (AddEdge
	// and AddBatch route here; the bulk JSONB chunks are emitted below).
	// See graph.StructuralEdgeTargetInvalid for the mapper-bug class this
	// stops at the door.
	if kept, rejected := graph.FilterStructuralEdgeViolations(edges); len(rejected) > 0 {
		edges = kept
		inputRepos := structuralInputNodeRepos(nodes)
		for _, edge := range rejected {
			repo := s.structuralWriteRepo(edge, inputRepos)
			s.recordStructuralEdge(graph.StructuralDropWrite, graph.StructuralPathSQLiteAddBatch, repo, edge)
		}
	}
	// Lazy builtin-sentinel materialization, mirroring Graph.AddBatch: give
	// every ::builtin:: edge target a real KindBuiltin node so those edges
	// stop reading as orphans. The per-store seen-set keeps warm re-indexes
	// from re-upserting identical stubs on every batch. It is keyed by
	// generation: the set is shared by every handle over one core, so a stub
	// the base corpus already holds must not suppress the same stub in a
	// derived generation that has never had it written.
	// builtinSeen is shared by every generation handle over one store. Pair its
	// admission with the writer lock so another batch cannot observe a key whose
	// transaction later rolls back and omit the corresponding stub.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var freshBuiltinKeys []string
	builtinKeysCommitted := false
	defer func() {
		if builtinKeysCommitted {
			return
		}
		for _, key := range freshBuiltinKeys {
			s.builtinSeen.Delete(key)
		}
	}()
	if stubs := graph.BuiltinStubNodes(edges); len(stubs) > 0 {
		var fresh []*graph.Node
		for _, stub := range stubs {
			key := builtinSeenKey(s.viewGen, stub.ID)
			if _, dup := s.builtinSeen.LoadOrStore(key, struct{}{}); !dup {
				fresh = append(fresh, stub)
			}
		}
		if len(fresh) > 0 {
			nodes = append(append(make([]*graph.Node, 0, len(nodes)+len(fresh)), nodes...), fresh...)
		}
	}

	hasGraphInput := false
	for _, node := range nodes {
		if node != nil && node.ID != "" && !graph.IsProxyNode(node) {
			hasGraphInput = true
			break
		}
	}
	if !hasGraphInput {
		for _, edge := range edges {
			if edge != nil && !graph.IsProxyID(edge.From) && !graph.IsProxyID(edge.To) {
				hasGraphInput = true
				break
			}
		}
	}
	if !hasGraphInput {
		return stats, nil
	}
	for _, edge := range edges {
		if edge != nil && graph.IsUnresolvedTarget(edge.To) &&
			!graph.IsProxyID(edge.From) && !graph.IsProxyID(edge.To) {
			s.unresolvedInserts.Add(1)
		}
	}

	variableLimit := s.sqliteBatchVariableLimitLocked()
	defer func() {
		// Persist any automatic fallback discovered by a connection-specific
		// "too many SQL variables" error for subsequent AddBatch calls.
		s.batchVariableLimit = variableLimit
	}()

	if s.analysisGenerationPresent {
		nodesChanged, statements, err := s.batchContainsAnalysisNodeChangeLocked(nodes)
		stats.analysisNodeStatements = statements
		if err != nil {
			return stats, err
		}
		if nodesChanged && !s.invalidateAnalysisBeforeMutationLocked() {
			return stats, nil
		}
		if s.analysisGenerationPresent {
			edgesChanged, statements, err := s.batchContainsNewEdgeLocked(edges)
			stats.analysisEdgeStatements = statements
			if err != nil {
				return stats, err
			}
			if edgesChanged && !s.invalidateAnalysisBeforeMutationLocked() {
				return stats, nil
			}
		}
	}

	tx, err := s.beginWrite()
	if err != nil {
		return stats, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var (
		receiptDelta  *sqliteMutationReceiptAccumulator
		identities    map[string]sqliteMutationNodeIdentity
		identityExact = true
	)
	if s.hasActiveMutationReceiptsLocked() {
		receiptDelta = newSQLiteMutationReceiptAccumulator()
		ids := make([]string, 0, len(nodes)+len(edges))
		for _, node := range nodes {
			if node != nil && node.ID != "" && !graph.IsProxyNode(node) {
				ids = append(ids, node.ID)
			}
		}
		for _, edge := range edges {
			if edge != nil && edge.FilePath == "" && !graph.IsProxyID(edge.From) && !graph.IsProxyID(edge.To) {
				ids = append(ids, edge.From)
			}
		}
		identities, err = mutationNodeIdentitiesTx(tx, s.viewGen, ids)
		if err != nil {
			identityExact = false
			identities = make(map[string]sqliteMutationNodeIdentity)
		}
	}

	// JSONB bulk fast path: two bounded payload binds per statement instead
	// of thousands of per-row variable binds (see add_batch_json.go). Active
	// receipts collect exact identities through bounded RETURNING result sets;
	// the placeholder writer remains the kill-switch/compatibility fallback.
	useJSONB := jsonbIngestEnabled() && jsonbIngestSupported(tx)
	if useJSONB {
		defer func() {
			if s.bulkConn == nil && !s.coordinatedBulkLoad {
				s.jsonbIngestBuffers.release()
				return
			}
			s.jsonbIngestBuffers.trim()
		}()
	}
	var changedNodeIDs map[string]int
	if useJSONB {
		stats.nodeRowsChanged, stats.nodeStatements, changedNodeIDs, err = insertNodeChunksJSONBTxWithBuffers(tx, s.viewGen, nodes, receiptDelta != nil, &s.jsonbIngestBuffers)
	} else {
		stats.nodeRowsChanged, stats.nodeStatements, changedNodeIDs, err = insertNodeChunksTxLimited(tx, s.viewGen, nodes, receiptDelta != nil, &variableLimit)
	}
	if err != nil {
		return stats, err
	}
	if corpusRows := cloneCorpusRowsFromNodes(nodes); len(corpusRows) > 0 {
		if err := upsertCloneCorpusTx(tx, s.viewGen, "", corpusRows); err != nil {
			return stats, err
		}
	}
	if receiptDelta != nil && stats.nodeRowsChanged > 0 {
		inputCounts := make(map[string]int, len(changedNodeIDs))
		lastNodes := make(map[string]*graph.Node, len(changedNodeIDs))
		for _, node := range nodes {
			if node == nil || node.ID == "" || graph.IsProxyNode(node) {
				continue
			}
			inputCounts[node.ID]++
			lastNodes[node.ID] = node
		}
		for id := range changedNodeIDs {
			node := lastNodes[id]
			oldIdentity, oldFound := identities[id]
			if !identityExact {
				receiptDelta.noteIncomplete("node_identity_preload_failed")
			} else if !oldFound {
				// The transaction exposes only the final duplicate occurrence. A
				// newly created final identity is therefore exact even when earlier
				// occurrences of the same ID carried enrichment-only differences.
				recordSQLiteAddedNode(receiptDelta, node)
			} else if !oldIdentity.equalsNode(node) {
				if inputCounts[id] > 1 &&
					(graph.IsReferenceableSymbol(graph.NodeKind(oldIdentity.kind)) || graph.IsReferenceableSymbol(node.Kind)) {
					receiptDelta.noteIncomplete("duplicate_node_batch")
				} else {
					recordSQLiteChangedNodeIdentity(receiptDelta, oldIdentity, node)
				}
			}
		}
		for id, node := range lastNodes {
			identities[id] = sqliteIdentityForNode(node)
		}
	}

	var insertedEdgeKeys map[sqliteEdgeIdentity]int
	if useJSONB {
		stats.edgeRowsInserted, stats.edgeStatements, insertedEdgeKeys, err = insertEdgeChunksJSONBTxWithBuffers(tx, s.viewGen, edges, receiptDelta != nil, &s.jsonbIngestBuffers)
	} else {
		stats.edgeRowsInserted, stats.edgeStatements, insertedEdgeKeys, err = insertEdgeChunksTxLimited(tx, s.viewGen, edges, receiptDelta != nil, &variableLimit)
	}
	if err != nil {
		return stats, err
	}
	if receiptDelta != nil && stats.edgeRowsInserted > 0 {
		for _, edge := range edges {
			if edge == nil || graph.IsProxyID(edge.From) || graph.IsProxyID(edge.To) {
				continue
			}
			key := sqliteIdentityForEdge(edge)
			if insertedEdgeKeys[key] == 0 {
				continue
			}
			insertedEdgeKeys[key]--
			file := edge.FilePath
			if file == "" {
				if source, found := identities[edge.From]; found {
					file = source.filePath
				} else if !identityExact {
					receiptDelta.noteIncomplete("edge_source_identity_preload_failed")
				}
			}
			recordSQLiteAddedEdge(receiptDelta, edge, file)
		}
	}

	if err := tx.Commit(); err != nil {
		return stats, err
	}
	committed = true
	changed := stats.nodeRowsChanged > 0 || stats.edgeRowsInserted > 0
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.mergeMutationReceiptLocked(receiptDelta)
	}
	// The transaction is durable before index sealing. If the bounded cold
	// threshold was reached, build the dense indexes now on the same pinned
	// connection; a failure remains retryable at the repository/final boundary.
	return stats, s.noteBulkRowsLocked(stats.nodeRowsChanged, stats.edgeRowsInserted)
}
