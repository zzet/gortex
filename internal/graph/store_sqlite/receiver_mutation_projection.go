package store_sqlite

import (
	"database/sql"
	"encoding/json"

	"github.com/zzet/gortex/internal/graph"
)

const defaultReceiverMutationPageSize = 4096

func receiverMutationPageSize(pageSize int) int {
	if pageSize <= 0 {
		return defaultReceiverMutationPageSize
	}
	return pageSize
}

func (s *Store) receiverMutationNodeHighWater(kind graph.NodeKind) (string, bool) {
	var highWater string
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(id), '') FROM nodes WHERE kind = ? AND view_gen = ?`, string(kind), s.viewGen,
	).Scan(&highWater); err != nil {
		panicOnFatal(err)
		return "", false
	}
	return highWater, highWater != ""
}

func (s *Store) receiverMutationEdgeHighWater(kind graph.EdgeKind) (int64, bool) {
	var highWater int64
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM edges WHERE kind = ? AND view_gen = ?`, string(kind), s.viewGen,
	).Scan(&highWater); err != nil {
		panicOnFatal(err)
		return 0, false
	}
	return highWater, highWater != 0
}

// ScanReceiverMutationMethods keyset-pages only method IDs and receiver
// metadata. sql.RawBytes is decoded before Rows.Next so database/sql does not
// retain a second metadata copy.
func (s *Store) ScanReceiverMutationMethods(
	pageSize int,
	yield func([]graph.ReceiverMutationMethod) bool,
) {
	if yield == nil {
		return
	}
	pageSize = receiverMutationPageSize(pageSize)
	highWater, ok := s.receiverMutationNodeHighWater(graph.KindMethod)
	if !ok {
		return
	}
	stmt, err := s.db.Prepare(`
SELECT id, meta
FROM nodes
WHERE kind = ? AND id > ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`)
	if err != nil {
		panicOnFatal(err)
		return
	}
	defer stmt.Close()

	lastID := ""
	for lastID < highWater {
		rows, err := stmt.Query(string(graph.KindMethod), lastID, highWater, s.viewGen, pageSize)
		if err != nil {
			panicOnFatal(err)
			return
		}
		page := make([]graph.ReceiverMutationMethod, 0, pageSize)
		scanned := 0
		for rows.Next() {
			var id string
			var meta sql.RawBytes
			if err := rows.Scan(&id, &meta); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return
			}
			scanned++
			lastID = id
			receiver, _, _, err := decodeReceiverMutationMeta(meta)
			if err != nil {
				continue
			}
			page = append(page, graph.ReceiverMutationMethod{ID: id, Receiver: receiver})
		}
		if !closeReceiverMutationRows(rows) {
			return
		}
		if len(page) > 0 && !yield(page) {
			return
		}
		if scanned < pageSize {
			return
		}
	}
}

// ScanReceiverMutationFields keyset-pages only field identity, name, and
// receiver metadata.
func (s *Store) ScanReceiverMutationFields(
	pageSize int,
	yield func([]graph.ReceiverMutationField) bool,
) {
	if yield == nil {
		return
	}
	pageSize = receiverMutationPageSize(pageSize)
	highWater, ok := s.receiverMutationNodeHighWater(graph.KindField)
	if !ok {
		return
	}
	stmt, err := s.db.Prepare(`
SELECT id, name, meta
FROM nodes
WHERE kind = ? AND id > ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`)
	if err != nil {
		panicOnFatal(err)
		return
	}
	defer stmt.Close()

	lastID := ""
	for lastID < highWater {
		rows, err := stmt.Query(string(graph.KindField), lastID, highWater, s.viewGen, pageSize)
		if err != nil {
			panicOnFatal(err)
			return
		}
		page := make([]graph.ReceiverMutationField, 0, pageSize)
		scanned := 0
		for rows.Next() {
			var id, name string
			var meta sql.RawBytes
			if err := rows.Scan(&id, &name, &meta); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return
			}
			scanned++
			lastID = id
			receiver, _, _, err := decodeReceiverMutationMeta(meta)
			if err != nil {
				continue
			}
			page = append(page, graph.ReceiverMutationField{ID: id, Name: name, Receiver: receiver})
		}
		if !closeReceiverMutationRows(rows) {
			return
		}
		if len(page) > 0 && !yield(page) {
			return
		}
		if scanned < pageSize {
			return
		}
	}
}

// ScanReceiverMutationWrites validates persisted metadata exactly like the
// full edge iterator, but retains only the two endpoint IDs.
func (s *Store) ScanReceiverMutationWrites(
	pageSize int,
	yield func([]graph.ReceiverMutationWrite) bool,
) {
	if yield == nil {
		return
	}
	pageSize = receiverMutationPageSize(pageSize)
	highWater, ok := s.receiverMutationEdgeHighWater(graph.EdgeWrites)
	if !ok {
		return
	}
	stmt, err := s.db.Prepare(`
SELECT id, from_id, to_id, meta
FROM edges
WHERE kind = ? AND id > ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`)
	if err != nil {
		panicOnFatal(err)
		return
	}
	defer stmt.Close()

	lastID := int64(0)
	for lastID < highWater {
		rows, err := stmt.Query(string(graph.EdgeWrites), lastID, highWater, s.viewGen, pageSize)
		if err != nil {
			panicOnFatal(err)
			return
		}
		page := make([]graph.ReceiverMutationWrite, 0, pageSize)
		scanned := 0
		for rows.Next() {
			var id int64
			var from, to string
			var meta sql.RawBytes
			if err := rows.Scan(&id, &from, &to, &meta); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return
			}
			scanned++
			lastID = id
			if err := validateReceiverMutationMeta(meta); err != nil {
				continue
			}
			page = append(page, graph.ReceiverMutationWrite{From: from, To: to})
		}
		if !closeReceiverMutationRows(rows) {
			return
		}
		if len(page) > 0 && !yield(page) {
			return
		}
		if scanned < pageSize {
			return
		}
	}
}

// ScanReceiverMutationCalls retains only calls stamped as own-receiver field
// or sibling-method calls. The target join replaces the legacy bulk node
// refetch; malformed target metadata is treated as a missing target, exactly as
// GetNodesByIDs did after its full-node decoder rejected that row.
func (s *Store) ScanReceiverMutationCalls(
	pageSize int,
	yield func([]graph.ReceiverMutationCall) bool,
) {
	if yield == nil {
		return
	}
	pageSize = receiverMutationPageSize(pageSize)
	highWater, ok := s.receiverMutationEdgeHighWater(graph.EdgeCalls)
	if !ok {
		return
	}
	candidateStmt, err := s.db.Prepare(`
SELECT id, meta
FROM edges
WHERE kind = ? AND id > ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`)
	if err != nil {
		panicOnFatal(err)
		return
	}
	defer candidateStmt.Close()
	exactStmt, err := s.db.Prepare(`
WITH requested(id) AS (
    SELECT CAST(value AS INTEGER) FROM json_each(?)
)
SELECT e.id, e.from_id, e.to_id, e.file_path, e.line,
       target.kind, target.name, target.meta
FROM requested AS r
JOIN edges AS e ON e.id = r.id AND e.view_gen = ?
LEFT JOIN nodes AS target ON target.id = e.to_id AND target.view_gen = e.view_gen
ORDER BY e.id`)
	if err != nil {
		panicOnFatal(err)
		return
	}
	defer exactStmt.Close()

	type callMeta struct {
		recvField string
		recvSelf  bool
	}
	lastID := int64(0)
	for lastID < highWater {
		rows, err := candidateStmt.Query(string(graph.EdgeCalls), lastID, highWater, s.viewGen, pageSize)
		if err != nil {
			panicOnFatal(err)
			return
		}
		candidateIDs := make([]int64, 0, 64)
		candidateMeta := make(map[int64]callMeta, 64)
		scanned := 0
		for rows.Next() {
			var id int64
			var edgeMeta sql.RawBytes
			if err := rows.Scan(&id, &edgeMeta); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return
			}
			scanned++
			lastID = id
			_, recvField, recvSelf, err := decodeReceiverMutationMeta(edgeMeta)
			if err != nil || (recvField == "" && !recvSelf) {
				continue
			}
			candidateIDs = append(candidateIDs, id)
			candidateMeta[id] = callMeta{recvField: recvField, recvSelf: recvSelf}
		}
		if !closeReceiverMutationRows(rows) {
			return
		}

		if len(candidateIDs) > 0 {
			encodedIDs, err := json.Marshal(candidateIDs)
			if err != nil {
				panicOnFatal(err)
				return
			}
			exactRows, err := exactStmt.Query(string(encodedIDs), s.viewGen)
			if err != nil {
				panicOnFatal(err)
				return
			}
			page := make([]graph.ReceiverMutationCall, 0, len(candidateIDs))
			for exactRows.Next() {
				var id int64
				var from, to, filePath string
				var line int
				var targetMeta sql.RawBytes
				var targetKind, targetName sql.NullString
				if err := exactRows.Scan(
					&id, &from, &to, &filePath, &line,
					&targetKind, &targetName, &targetMeta,
				); err != nil {
					_ = exactRows.Close()
					panicOnFatal(err)
					return
				}
				meta, present := candidateMeta[id]
				if !present {
					continue
				}
				methodName := ""
				if targetKind.Valid && targetKind.String == string(graph.KindMethod) &&
					targetName.Valid && targetName.String != "" &&
					validateReceiverMutationMeta(targetMeta) == nil {
					methodName = targetName.String
				}
				page = append(page, graph.ReceiverMutationCall{
					From: from, To: to, TargetMethodName: methodName,
					RecvField: meta.recvField, RecvSelf: meta.recvSelf,
					FilePath: filePath, Line: line,
				})
			}
			if !closeReceiverMutationRows(exactRows) {
				return
			}
			if len(page) > 0 && !yield(page) {
				return
			}
		}
		if scanned < pageSize {
			return
		}
	}
}

func closeReceiverMutationRows(rows *sql.Rows) bool {
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil {
		panicOnFatal(rowsErr)
		return false
	}
	if closeErr != nil {
		panicOnFatal(closeErr)
		return false
	}
	return true
}

var _ graph.ReceiverMutationScanner = (*Store)(nil)
