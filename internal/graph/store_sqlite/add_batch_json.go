package store_sqlite

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
)

// JSONB bulk ingest.
//
// The placeholder AddBatch writer binds every column of every row as its own
// SQL variable; on the modernc driver each bind copies its argument into the
// VM (conn.bind + memmove dominate cold-load ingest CPU). This path replaces
// thousands of binds per statement with exactly two bounded payloads — a
// JSONB array of scalar rows plus a raw metadata-BLOB arena the statement
// slices with substr() — while sharing the production encoders, conflict
// clauses, and skip predicates, so the resulting rows are byte-identical to
// the placeholder writer's (asserted by the reopen-parity test).
//
// When mutation receipts are active, bounded RETURNING clauses collect the
// exact changed identities from the same JSONB statements. The runtime must
// expose jsonb(); GORTEX_SQLITE_JSONB_INGEST=0 forces the placeholder path.
const (
	jsonbIngestMaxPayload       = sqliteBatchMaxBoundBytes
	jsonbIngestNodeRows         = 4096
	jsonbIngestEdgeRows         = 8192
	jsonbIngestRetainedCapacity = 2 * jsonbIngestMaxPayload
)

// jsonbIngestBuffers reuses the bounded payload arena and row scratch across
// AddBatch calls. Store.writeMu protects the production instance; tests and
// compatibility helpers may use a stack-local zero value.
type jsonbIngestBuffers struct {
	payload bytes.Buffer
	blobs   bytes.Buffer
	args    []any
	encoder *json.Encoder
}

func (buffers *jsonbIngestBuffers) reset(argsCapacity int) {
	buffers.trim()
	buffers.payload.Reset()
	buffers.blobs.Reset()
	buffers.payload.Grow(256 << 10)
	buffers.blobs.Grow(128 << 10)
	if cap(buffers.args) < argsCapacity {
		buffers.args = make([]any, 0, argsCapacity)
	} else {
		buffers.args = buffers.args[:0]
	}
	if buffers.encoder == nil {
		buffers.encoder = json.NewEncoder(&buffers.payload)
	}
	buffers.payload.WriteByte('[')
	// Keep the raw-BLOB bind non-NULL even for a valid zero-length blob. Row
	// offsets are zero-based into this buffer; SQLite substr is one-based.
	buffers.blobs.WriteByte(0)
}

// trim prevents one exceptional first row from becoming a retained arena and
// releases references held by the interface scratch. Normal bounded payload
// growth is kept so later cold-load chunks avoid reallocating.
func (buffers *jsonbIngestBuffers) trim() {
	if cap(buffers.args) > 0 {
		clear(buffers.args[:cap(buffers.args)])
		buffers.args = buffers.args[:0]
	}
	if buffers.payload.Cap() > jsonbIngestRetainedCapacity {
		buffers.payload = bytes.Buffer{}
		buffers.encoder = nil
	}
	if buffers.blobs.Cap() > jsonbIngestRetainedCapacity {
		buffers.blobs = bytes.Buffer{}
	}
}

// release drops reusable arenas at an idle boundary. Keeping them during a
// coordinated cold load saves allocation churn; retaining them after the load
// would turn a transient optimization into permanent daemon RSS.
func (buffers *jsonbIngestBuffers) release() {
	if cap(buffers.args) > 0 {
		clear(buffers.args[:cap(buffers.args)])
	}
	*buffers = jsonbIngestBuffers{}
}

// The $[i] positions are the encoded row's element indices, not the target
// column ordinals: the single metadata BLOB argument is encoded as an
// (offset, length) PAIR into the raw arena, so every element after it sits one
// position later than its column. Element 0 is view_gen, the first column of
// both insert lists — the whole chunk was produced by one handle, so the same
// generation rides on every row rather than a third bound scalar the two
// writers would then have to keep in step.
const jsonbNodeIngestSQL = `INSERT INTO nodes (` + nodeInsertColumns + `)
SELECT
	json_extract(row.value, '$[0]'), json_extract(row.value, '$[1]'),
	json_extract(row.value, '$[2]'), json_extract(row.value, '$[3]'),
	json_extract(row.value, '$[4]'), json_extract(row.value, '$[5]'),
	json_extract(row.value, '$[6]'), json_extract(row.value, '$[7]'),
	json_extract(row.value, '$[8]'), json_extract(row.value, '$[9]'),
	json_extract(row.value, '$[10]'), json_extract(row.value, '$[11]'),
	json_extract(row.value, '$[12]'), json_extract(row.value, '$[13]'),
	json_extract(row.value, '$[14]'), json_extract(row.value, '$[15]'),
	json_extract(row.value, '$[16]'), json_extract(row.value, '$[17]'),
	json_extract(row.value, '$[18]'), json_extract(row.value, '$[19]'),
	json_extract(row.value, '$[20]'), json_extract(row.value, '$[21]'),
	json_extract(row.value, '$[22]'), json_extract(row.value, '$[23]'),
	json_extract(row.value, '$[24]'), json_extract(row.value, '$[25]'),
	json_extract(row.value, '$[26]'), json_extract(row.value, '$[27]'),
	json_extract(row.value, '$[28]'), json_extract(row.value, '$[29]'),
	CASE WHEN json_type(row.value, '$[30]') = 'null' THEN NULL
		ELSE substr(?2, json_extract(row.value, '$[30]') + 1, json_extract(row.value, '$[31]')) END,
	json_extract(row.value, '$[32]'), json_extract(row.value, '$[33]'),
	json_extract(row.value, '$[34]'), json_extract(row.value, '$[35]'),
	json_extract(row.value, '$[36]')
FROM jsonb_each(jsonb(?1)) AS row
WHERE true` + nodeUpsertClause

const jsonbEdgeIngestSQL = `INSERT OR IGNORE INTO edges (` + edgeInsertColumns + `)
SELECT
	json_extract(row.value, '$[0]'), json_extract(row.value, '$[1]'),
	json_extract(row.value, '$[2]'), json_extract(row.value, '$[3]'),
	json_extract(row.value, '$[4]'), json_extract(row.value, '$[5]'),
	json_extract(row.value, '$[6]'), json_extract(row.value, '$[7]'),
	json_extract(row.value, '$[8]'), json_extract(row.value, '$[9]'),
	json_extract(row.value, '$[10]'),
	CASE WHEN json_type(row.value, '$[11]') = 'null' THEN NULL
		ELSE substr(?2, json_extract(row.value, '$[11]') + 1, json_extract(row.value, '$[12]')) END,
	json_extract(row.value, '$[13]'), json_extract(row.value, '$[14]'),
	json_extract(row.value, '$[15]')
FROM jsonb_each(jsonb(?1)) AS row`

// jsonbNodeMetaIndex / jsonbEdgeMetaIndex are the offsets of the metadata BLOB
// argument inside the rows appendNodeInsertArgs / appendEdgeInsertArgs build —
// the element the encoder splits into the (offset, length) pair above. Named
// rather than inlined at the call sites so the two SQL statements and the
// encoder cannot drift apart silently.
const (
	jsonbNodeMetaIndex = 30
	jsonbEdgeMetaIndex = 11
)

// jsonbIngestEnabled is the operator kill switch: GORTEX_SQLITE_JSONB_INGEST=0
// (or "false") forces the placeholder writer everywhere.
func jsonbIngestEnabled() bool {
	v := os.Getenv("GORTEX_SQLITE_JSONB_INGEST")
	return v != "0" && !strings.EqualFold(v, "false")
}

// jsonbIngestSupport caches the process-wide jsonb() availability probe:
// 0 = unprobed, 1 = supported, -1 = unsupported. The bundled SQLite build is
// a process constant, so one probe answers for every store.
var jsonbIngestSupport atomic.Int32

func jsonbIngestSupported(tx *sql.Tx) bool {
	switch jsonbIngestSupport.Load() {
	case 1:
		return true
	case -1:
		return false
	}
	var kind string
	if err := tx.QueryRow(`SELECT typeof(jsonb('[]'))`).Scan(&kind); err != nil || kind == "" {
		jsonbIngestSupport.Store(-1)
		return false
	}
	jsonbIngestSupport.Store(1)
	return true
}

// jsonbIngestValue normalizes the driver-facing argument types produced by
// appendNodeInsertArgs / appendEdgeInsertArgs into their JSON equivalents so
// json_extract yields the same SQLite storage classes the placeholder binds
// produce.
func jsonbIngestValue(value any) any {
	switch value := value.(type) {
	case sql.NullString:
		if !value.Valid {
			return nil
		}
		return value.String
	case sql.NullBool:
		if !value.Valid {
			return nil
		}
		return value.Bool
	case sql.NullInt64:
		if !value.Valid {
			return nil
		}
		return value.Int64
	default:
		return value
	}
}

// appendJSONBIngestRow encodes one row's args into the JSON payload, swapping
// the single BLOB argument at metaIndex for an (offset, length) pair into the
// raw blob arena. Returns false (without consuming the row) when adding it
// would exceed the bounded payload; a first row is always admitted so cursor
// progress is guaranteed.
func appendJSONBIngestRow(buffers *jsonbIngestBuffers, row []any, metaIndex, rows int) (bool, error) {
	meta, ok := row[metaIndex].([]byte)
	if row[metaIndex] != nil && !ok {
		return false, fmt.Errorf("metadata argument %d has type %T, want []byte", metaIndex, row[metaIndex])
	}
	metaPresent := meta != nil
	metaOffset := buffers.blobs.Len()

	row = append(row, nil)
	copy(row[metaIndex+2:], row[metaIndex+1:len(row)-1])
	if metaPresent {
		row[metaIndex] = metaOffset
		row[metaIndex+1] = len(meta)
	} else {
		row[metaIndex] = nil
		row[metaIndex+1] = nil
	}
	for i := range row {
		if i == metaIndex || i == metaIndex+1 {
			continue
		}
		row[i] = jsonbIngestValue(row[i])
	}

	rowStart := buffers.payload.Len()
	if rows > 0 {
		buffers.payload.WriteByte(',')
	}
	if err := buffers.encoder.Encode(row); err != nil {
		buffers.payload.Truncate(rowStart)
		return false, err
	}
	encodedEnd := buffers.payload.Len()
	if encodedEnd <= rowStart || buffers.payload.Bytes()[encodedEnd-1] != '\n' {
		buffers.payload.Truncate(rowStart)
		return false, fmt.Errorf("JSONB row encoder omitted trailing newline")
	}
	buffers.payload.Truncate(encodedEnd - 1)

	boundBytes := buffers.payload.Len() + 1 + buffers.blobs.Len() + len(meta)
	if rows > 0 && boundBytes > jsonbIngestMaxPayload {
		buffers.payload.Truncate(rowStart)
		return false, nil
	}
	if metaPresent {
		buffers.blobs.Write(meta)
	}
	return true, nil
}

func nextJSONBNodePayload(buffers *jsonbIngestBuffers, viewGen int64, nodes []*graph.Node, start int) (jsonPayload, blobPayload []byte, next, rows int, err error) {
	buffers.reset(nodeInsertParams + 1)
	pos := start
	for pos < len(nodes) && rows < jsonbIngestNodeRows {
		node := nodes[pos]
		if node == nil || node.ID == "" || graph.IsProxyNode(node) {
			pos++
			continue
		}
		args, appendErr := appendNodeInsertArgs(buffers.args[:0], viewGen, node)
		if appendErr != nil {
			return nil, nil, start, 0, appendErr
		}
		buffers.args = args
		added, appendErr := appendJSONBIngestRow(buffers, args, jsonbNodeMetaIndex, rows)
		if appendErr != nil {
			return nil, nil, start, 0, appendErr
		}
		if !added {
			break
		}
		pos++
		rows++
	}
	buffers.payload.WriteByte(']')
	return buffers.payload.Bytes(), buffers.blobs.Bytes(), pos, rows, nil
}

func nextJSONBEdgePayload(buffers *jsonbIngestBuffers, viewGen int64, edges []*graph.Edge, start int) (jsonPayload, blobPayload []byte, next, rows int, err error) {
	buffers.reset(edgeInsertParams + 1)
	pos := start
	for pos < len(edges) && rows < jsonbIngestEdgeRows {
		edge := edges[pos]
		if edge == nil || graph.IsProxyID(edge.From) || graph.IsProxyID(edge.To) {
			pos++
			continue
		}
		args, appendErr := appendEdgeInsertArgs(buffers.args[:0], viewGen, edge)
		if appendErr != nil {
			return nil, nil, start, 0, appendErr
		}
		buffers.args = args
		added, appendErr := appendJSONBIngestRow(buffers, args, jsonbEdgeMetaIndex, rows)
		if appendErr != nil {
			return nil, nil, start, 0, appendErr
		}
		if !added {
			break
		}
		pos++
		rows++
	}
	buffers.payload.WriteByte(']')
	return buffers.payload.Bytes(), buffers.blobs.Bytes(), pos, rows, nil
}

// insertNodeChunksJSONBTx is the JSONB counterpart of
// insertNodeChunksTxLimited. When returnChanged is true, RETURNING supplies the
// exact per-ID multiplicities needed by mutation receipts without abandoning
// the bounded two-bind JSONB statements.
func insertNodeChunksJSONBTx(
	tx *sql.Tx,
	viewGen int64,
	nodes []*graph.Node,
	returnChanged bool,
) (rowsChanged, statements int, changedIDs map[string]int, err error) {
	var buffers jsonbIngestBuffers
	return insertNodeChunksJSONBTxWithBuffers(tx, viewGen, nodes, returnChanged, &buffers)
}

func insertNodeChunksJSONBTxWithBuffers(
	tx *sql.Tx,
	viewGen int64,
	nodes []*graph.Node,
	returnChanged bool,
	buffers *jsonbIngestBuffers,
) (rowsChanged, statements int, changedIDs map[string]int, err error) {
	query := jsonbNodeIngestSQL
	if returnChanged {
		query += " RETURNING id"
		changedIDs = make(map[string]int)
	}
	stmt, err := tx.Prepare(query)
	if err != nil {
		return 0, 0, changedIDs, err
	}
	defer stmt.Close()
	for pos := 0; pos < len(nodes); {
		payload, blobs, next, rows, encodeErr := nextJSONBNodePayload(buffers, viewGen, nodes, pos)
		if encodeErr != nil {
			return rowsChanged, statements, changedIDs, encodeErr
		}
		pos = next
		if rows == 0 {
			continue
		}
		if returnChanged {
			returned, queryErr := stmt.Query(payload, blobs)
			statements++
			if queryErr != nil {
				return rowsChanged, statements, changedIDs, queryErr
			}
			for returned.Next() {
				var id string
				if scanErr := returned.Scan(&id); scanErr != nil {
					_ = returned.Close()
					return rowsChanged, statements, changedIDs, scanErr
				}
				rowsChanged++
				changedIDs[id]++
			}
			if rowsErr := returned.Err(); rowsErr != nil {
				_ = returned.Close()
				return rowsChanged, statements, changedIDs, rowsErr
			}
			if closeErr := returned.Close(); closeErr != nil {
				return rowsChanged, statements, changedIDs, closeErr
			}
			continue
		}
		result, execErr := stmt.Exec(payload, blobs)
		statements++
		if execErr != nil {
			return rowsChanged, statements, changedIDs, execErr
		}
		changed, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsChanged, statements, changedIDs, rowsErr
		}
		rowsChanged += int(changed)
	}
	return rowsChanged, statements, changedIDs, nil
}

// insertEdgeChunksJSONBTx is the JSONB counterpart of
// insertEdgeChunksTxLimited. When returnInserted is true, RETURNING supplies
// exact edge identities for mutation receipts while retaining bounded JSONB
// payloads and INSERT OR IGNORE semantics.
func insertEdgeChunksJSONBTx(
	tx *sql.Tx,
	viewGen int64,
	edges []*graph.Edge,
	returnInserted bool,
) (rowsInserted, statements int, insertedKeys map[sqliteEdgeIdentity]int, err error) {
	var buffers jsonbIngestBuffers
	return insertEdgeChunksJSONBTxWithBuffers(tx, viewGen, edges, returnInserted, &buffers)
}

func insertEdgeChunksJSONBTxWithBuffers(
	tx *sql.Tx,
	viewGen int64,
	edges []*graph.Edge,
	returnInserted bool,
	buffers *jsonbIngestBuffers,
) (rowsInserted, statements int, insertedKeys map[sqliteEdgeIdentity]int, err error) {
	query := jsonbEdgeIngestSQL
	if returnInserted {
		query += " RETURNING from_id, to_id, kind, file_path, line"
		insertedKeys = make(map[sqliteEdgeIdentity]int)
	}
	stmt, err := tx.Prepare(query)
	if err != nil {
		return 0, 0, insertedKeys, err
	}
	defer stmt.Close()
	for pos := 0; pos < len(edges); {
		payload, blobs, next, rows, encodeErr := nextJSONBEdgePayload(buffers, viewGen, edges, pos)
		if encodeErr != nil {
			return rowsInserted, statements, insertedKeys, encodeErr
		}
		pos = next
		if rows == 0 {
			continue
		}
		if returnInserted {
			returned, queryErr := stmt.Query(payload, blobs)
			statements++
			if queryErr != nil {
				return rowsInserted, statements, insertedKeys, queryErr
			}
			for returned.Next() {
				var key sqliteEdgeIdentity
				if scanErr := returned.Scan(&key.from, &key.to, &key.kind, &key.filePath, &key.line); scanErr != nil {
					_ = returned.Close()
					return rowsInserted, statements, insertedKeys, scanErr
				}
				rowsInserted++
				insertedKeys[key]++
			}
			if rowsErr := returned.Err(); rowsErr != nil {
				_ = returned.Close()
				return rowsInserted, statements, insertedKeys, rowsErr
			}
			if closeErr := returned.Close(); closeErr != nil {
				return rowsInserted, statements, insertedKeys, closeErr
			}
			continue
		}
		result, execErr := stmt.Exec(payload, blobs)
		statements++
		if execErr != nil {
			return rowsInserted, statements, insertedKeys, execErr
		}
		inserted, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsInserted, statements, insertedKeys, rowsErr
		}
		rowsInserted += int(inserted)
	}
	return rowsInserted, statements, insertedKeys, nil
}
