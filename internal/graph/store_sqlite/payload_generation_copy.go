package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// GenerationCopyCounts is what one row-level generation copy moved.
//
// Nodes and Edges are the two payload tables a caller reports on; Rows is the
// total across every generation-keyed table the copy touched, including the
// sidecars and the FTS projections. The authoritative description of what
// landed is the destination generation's own payload, which a caller reads
// back rather than trusting these numbers — they exist for the build report
// and the log line.
//
// The indexer's GenerationPayloadCopier interface aliases this type, so this
// declaration is what makes *Store satisfy it.
type GenerationCopyCounts struct {
	Nodes int64
	Edges int64
	Rows  int64
}

// generationCopyFTSChunk bounds the rows one FTS copy statement carries.
// content_fts is the widest map/vtable pair (fts_rowid + repo_prefix +
// file_path on the map, node_id + repo_prefix + file_path + ordinal + body on
// the vtable), so 100 rows stays under SQLite's conservative 999-variable
// limit with room for a wider projection landing later.
const generationCopyFTSChunk = 100

// CopyPayloadGeneration materialises one payload generation's rows as another
// generation's rows, without reading the tree they describe.
//
// # What it copies
//
// Every generation-keyed table, enumerated from the schema rather than listed
// here: `nodes`, `edges`, the sidecar registry and the ownership-mask registry
// (both through payloadSweepTables, which is the same enumeration the
// retirement sweep walks, so a sidecar added later is copied without a second
// edit), and the two FTS5 projections through their docid maps. Every table's
// column list is read back from the schema itself (pragma_table_xinfo, hidden
// = 0), so a promoted column added by a migration rides along and a GENERATED
// column — which cannot be written — is skipped.
//
// The copy is repository-scoped, using exactly the scope the ordinary readers
// use: `nodes.repo_prefix = ?` (GetRepoNodes) and, for edges, the join onto the
// SOURCE node's repo_prefix within the same generation (GetRepoEdges). A
// sidecar carrying a repo_prefix column is scoped by it; the ownership masks
// carry none and are copied whole — they cannot exist at generation zero at all
// (the mask write path refuses the base generation, ErrMasksAtBaseGeneration),
// which is the only source a committed base is ever copied from.
//
// # What it refuses
//
//   - the base corpus as a DESTINATION. Generation zero is the mutable
//     working-copy view; a copy writes only the new generation's rows and never
//     relabels or modifies generation zero.
//   - a destination that already holds payload. This is the seal on a second
//     copy: the whole licence for writing a generation wholesale is that
//     nothing is there to collide with, and completing a partial generation is
//     not what this primitive does. Refused with ErrGenerationBulkLoadPopulated
//     — the same error the generation bulk window raises for the same reason,
//     so a caller matches one vocabulary.
//   - a destination that is not open to payload writes. The transaction is
//     opened through the ordinary managed-generation seam
//     (AtManagedGeneration + beginWriteContext), so a published, retiring or
//     row-less-in-the-catalog generation is refused exactly as its first
//     ordinary write would be, with ErrPayloadGenerationSealed.
//   - a copy onto itself, a negative source, and an empty repository prefix
//     (the empty prefix names shared global externals and, in solo mode, the
//     whole store — neither is a repository this may claim).
//
// The newer-schema refusal is not restated here: a store whose schema this
// binary does not understand never opens (store.go, the schema-version gate),
// so there is no connection for a copy to run on.
//
// # Where it runs
//
// One transaction on the store's write gate. Inside an open generation bulk
// window (BeginGenerationBulkLoad) that transaction rides the pinned writer
// connection — beginWriteContext routes it there — so the copy takes the
// window's shape: the enlarged page cache and no automatic checkpoint
// mid-payload. Outside a window it is an ordinary write, correct but without
// the shape.
//
// One transaction, not chunked: a half-copied generation would be a base that
// is neither generation zero's payload nor a re-parse of the tree, and the
// write gate is held for the length of a payload the caller has already
// decided to write in one piece.
func (s *Store) CopyPayloadGeneration(ctx context.Context, from, to int64, repoPrefix string) (GenerationCopyCounts, error) {
	var counts GenerationCopyCounts
	if ctx == nil {
		return counts, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	if s.coreless() {
		return counts, fmt.Errorf("%w: a generation copy needs an open store", ErrCatalogInvalidValue)
	}
	if to <= baseViewGeneration {
		return counts, fmt.Errorf("%w: a generation copy may not write the base corpus, got destination %d", ErrCatalogInvalidValue, to)
	}
	if from < baseViewGeneration {
		return counts, fmt.Errorf("%w: a generation copy needs a real source, got %d", ErrCatalogInvalidValue, from)
	}
	if from == to {
		return counts, fmt.Errorf("%w: a generation cannot be copied onto itself (%d)", ErrCatalogInvalidValue, to)
	}
	if repoPrefix == "" {
		return counts, fmt.Errorf("%w: a generation copy needs a repository prefix", ErrCatalogInvalidValue)
	}

	destination, err := s.AtManagedGeneration(to)
	if err != nil {
		return counts, err
	}
	// Asked before the write gate for the same reason BeginGenerationBulkLoad
	// asks it there: resolving the seal reads the catalog on the read pool, and
	// the pinned writer this transaction may be about to take is the connection
	// it would otherwise contend with.
	if err := destination.refuseSealedPayloadWrite(); err != nil {
		return counts, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := destination.beginWriteContext(ctx)
	if err != nil {
		return counts, err
	}
	defer func() { _ = tx.Rollback() }()

	empty, err := generationPayloadEmptyTx(ctx, tx, to)
	if err != nil {
		return counts, err
	}
	if !empty {
		return counts, fmt.Errorf("%w: generation %d", ErrGenerationBulkLoadPopulated, to)
	}

	counts, err = copyGenerationPayloadTx(ctx, tx, from, to, repoPrefix)
	if err != nil {
		return GenerationCopyCounts{}, err
	}
	if err := tx.Commit(); err != nil {
		return GenerationCopyCounts{}, err
	}
	return counts, nil
}

// copyGenerationPayloadTx is the whole move, in the order a reader of the
// destination needs it: nodes first (edges are scoped by their source node),
// then edges, then every sidecar and mask, then the FTS projections.
func copyGenerationPayloadTx(ctx context.Context, tx *sql.Tx, from, to int64, repoPrefix string) (GenerationCopyCounts, error) {
	var counts GenerationCopyCounts
	nodes, err := copyGenerationTableTx(ctx, tx, "nodes", from, to, repoPrefix)
	if err != nil {
		return counts, err
	}
	counts.Nodes, counts.Rows = nodes, nodes

	edges, err := copyGenerationEdgesTx(ctx, tx, from, to, repoPrefix)
	if err != nil {
		return counts, err
	}
	counts.Edges, counts.Rows = edges, counts.Rows+edges

	// The docid maps are driven by the FTS pass below, which has to mint new
	// docids rather than carry generation zero's: an FTS5 docid names one row
	// of the single shared virtual table, so two generations claiming the same
	// one is a corruption and not a per-generation duplicate.
	skip := make(map[string]struct{}, len(generationFTSDocidMaps))
	for _, docidMap := range generationFTSDocidMaps {
		skip[docidMap.ids] = struct{}{}
	}
	for _, table := range payloadSweepTables() {
		if _, deferred := skip[table]; deferred {
			continue
		}
		moved, err := copyGenerationTableTx(ctx, tx, table, from, to, repoPrefix)
		if err != nil {
			return counts, err
		}
		counts.Rows += moved
	}

	for _, docidMap := range generationFTSDocidMaps {
		moved, err := copyGenerationFTSTx(ctx, tx, docidMap, from, to, repoPrefix)
		if err != nil {
			return counts, err
		}
		counts.Rows += moved
	}
	return counts, nil
}

// generationCopyColumns reads a table's writable columns from the schema.
//
// hidden = 0 excludes both a virtual table's hidden columns and every GENERATED
// column, which is what makes this safe on `edges` — its is_unresolved /
// member_receiver / from_repo / to_repo_unresolved projections are VIRTUAL and
// refuse to be written — and on the FTS5 vtables, whose `rank` and
// table-named columns are hidden. Reading the list rather than naming it is
// what keeps a column added by a later migration inside the copy without a
// second edit here.
func generationCopyColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT name FROM pragma_table_xinfo(?) WHERE hidden = 0 ORDER BY cid`, table)
	if err != nil {
		return nil, fmt.Errorf("store_sqlite: read %s columns for a generation copy: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("store_sqlite: table %s reports no writable column, so a generation copy cannot describe it", table)
	}
	return columns, nil
}

// copyGenerationTableTx copies one generation-keyed table's rows for one
// repository. The destination generation is substituted for view_gen in the
// projection; every other column is carried verbatim, so a copied row is the
// source row with a different generation stamp.
func copyGenerationTableTx(ctx context.Context, tx *sql.Tx, table string, from, to int64, repoPrefix string) (int64, error) {
	columns, err := generationCopyColumns(ctx, tx, table)
	if err != nil {
		return 0, err
	}
	projection := make([]string, 0, len(columns))
	args := make([]any, 0, 3)
	scoped := false
	generationKeyed := false
	for _, column := range columns {
		switch column {
		case viewGenColumnName:
			generationKeyed = true
			projection = append(projection, "?")
			args = append(args, to)
		default:
			if column == "repo_prefix" {
				scoped = true
			}
			projection = append(projection, column)
		}
	}
	if !generationKeyed {
		return 0, fmt.Errorf("store_sqlite: table %s is not generation-keyed, so a generation copy cannot scope it", table)
	}
	query := `INSERT INTO ` + table + ` (` + strings.Join(columns, ", ") + `)
SELECT ` + strings.Join(projection, ", ") + ` FROM ` + table + ` WHERE ` + viewGenColumnName + ` = ?`
	args = append(args, from)
	if scoped {
		query += ` AND repo_prefix = ?`
		args = append(args, repoPrefix)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store_sqlite: copy %s from generation %d to %d: %w", table, from, to, err)
	}
	moved, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return moved, nil
}

// copyGenerationEdgesTx copies the edges one repository owns.
//
// Two things separate it from the generic table copy. `edges.id` is an
// INTEGER PRIMARY KEY AUTOINCREMENT naming a physical row rather than a logical
// edge, so it is left out and the destination mints its own; carrying it would
// collide with the source row on the very first edge. And edges carry no
// repo_prefix of their own — the repository that owns an edge is the repository
// that owns its SOURCE node — so the scope is the same join GetRepoEdges uses,
// including its generation equality, which keeps a cross-generation node from
// admitting an edge.
func copyGenerationEdgesTx(ctx context.Context, tx *sql.Tx, from, to int64, repoPrefix string) (int64, error) {
	columns, err := generationCopyColumns(ctx, tx, "edges")
	if err != nil {
		return 0, err
	}
	insert := make([]string, 0, len(columns))
	projection := make([]string, 0, len(columns))
	args := make([]any, 0, 3)
	generationKeyed := false
	for _, column := range columns {
		if column == "id" {
			continue
		}
		insert = append(insert, column)
		if column == viewGenColumnName {
			generationKeyed = true
			projection = append(projection, "?")
			args = append(args, to)
			continue
		}
		projection = append(projection, "e."+column)
	}
	if !generationKeyed {
		return 0, fmt.Errorf("store_sqlite: edges is not generation-keyed, so a generation copy cannot scope it")
	}
	query := `INSERT INTO edges (` + strings.Join(insert, ", ") + `)
SELECT ` + strings.Join(projection, ", ") + `
  FROM edges e
  JOIN nodes n ON n.id = e.from_id AND n.` + viewGenColumnName + ` = e.` + viewGenColumnName + `
 WHERE e.` + viewGenColumnName + ` = ? AND n.repo_prefix = ?`
	args = append(args, from, repoPrefix)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store_sqlite: copy edges from generation %d to %d: %w", from, to, err)
	}
	moved, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return moved, nil
}

// copyGenerationFTSTx copies one FTS5 projection and its docid map.
//
// The virtual table carries no generation column, so the map is the only
// address the copy has for its rows — and the docids themselves cannot be
// carried: symbol_fts_rowid_by_rowid is GLOBALLY unique on purpose, because a
// docid names one row of one shared virtual table. So the copy mints a fresh
// contiguous docid range through the same allocator the incremental FTS writer
// uses (nextFTSRowIDTx) and writes the new docid into both the vtable row and
// the map row.
//
// It runs in bounded chunks read through the map rather than as one
// INSERT … SELECT for two reasons: the statement would be reading the very
// virtual table it inserts into, and an FTS5 body column is unbounded, so a
// whole-generation projection is not a row set to materialise at once.
func copyGenerationFTSTx(ctx context.Context, tx *sql.Tx, docidMap ftsDocidMap, from, to int64, repoPrefix string) (int64, error) {
	mapColumns, err := generationCopyColumns(ctx, tx, docidMap.ids)
	if err != nil {
		return 0, err
	}
	carried := make([]string, 0, len(mapColumns))
	for _, column := range mapColumns {
		if column == viewGenColumnName || column == "fts_rowid" {
			continue
		}
		carried = append(carried, column)
	}
	ftsColumns, err := generationCopyColumns(ctx, tx, docidMap.fts)
	if err != nil {
		return 0, err
	}

	docids, err := generationCopySourceDocids(ctx, tx, docidMap.ids, from, repoPrefix)
	if err != nil {
		return 0, err
	}
	if len(docids) == 0 {
		return 0, nil
	}
	next, err := nextFTSRowIDTx(tx, docidMap.fts)
	if err != nil {
		return 0, fmt.Errorf("store_sqlite: allocate %s docids for a generation copy: %w", docidMap.fts, err)
	}

	var moved int64
	for start := 0; start < len(docids); start += generationCopyFTSChunk {
		end := start + generationCopyFTSChunk
		if end > len(docids) {
			end = len(docids)
		}
		chunk := docids[start:end]
		rows, err := generationCopyFTSChunkRows(ctx, tx, docidMap, carried, ftsColumns, chunk)
		if err != nil {
			return moved, err
		}
		if len(rows) == 0 {
			continue
		}
		written, err := generationCopyWriteFTSChunk(ctx, tx, docidMap, carried, ftsColumns, rows, to, &next)
		if err != nil {
			return moved, err
		}
		moved += written
	}
	return moved, nil
}

// generationCopySourceDocids lists the docids one generation's repository owns,
// in a stable order. It is read whole before any insert so the chunked reads
// below never walk a cursor over a table this copy is writing into.
func generationCopySourceDocids(ctx context.Context, tx *sql.Tx, table string, from int64, repoPrefix string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT fts_rowid FROM `+table+` WHERE `+viewGenColumnName+` = ? AND repo_prefix = ? ORDER BY fts_rowid`,
		from, repoPrefix)
	if err != nil {
		return nil, fmt.Errorf("store_sqlite: read %s docids for a generation copy: %w", table, err)
	}
	defer rows.Close()
	var docids []int64
	for rows.Next() {
		var docid int64
		if err := rows.Scan(&docid); err != nil {
			return nil, err
		}
		docids = append(docids, docid)
	}
	return docids, rows.Err()
}

// generationCopyFTSRow is one source row: the map's carried columns followed by
// the virtual table's own columns, both read as driver values so a column added
// later needs no type here.
type generationCopyFTSRow struct {
	carried []any
	body    []any
}

// generationCopyFTSChunkRows reads one bounded batch of source rows. The join
// is the map's, so a map row whose virtual-table row is missing is dropped
// rather than copied as a row of NULLs.
func generationCopyFTSChunkRows(
	ctx context.Context, tx *sql.Tx, docidMap ftsDocidMap,
	carried, ftsColumns []string, docids []int64,
) ([]generationCopyFTSRow, error) {
	projection := make([]string, 0, len(carried)+len(ftsColumns))
	for _, column := range carried {
		projection = append(projection, "m."+column)
	}
	for _, column := range ftsColumns {
		projection = append(projection, "f."+column)
	}
	query := `SELECT ` + strings.Join(projection, ", ") + `
  FROM ` + docidMap.ids + ` m
  JOIN ` + docidMap.fts + ` f ON f.rowid = m.fts_rowid
 WHERE m.fts_rowid IN (` + sqlInt64List(docids) + `)
 ORDER BY m.fts_rowid`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store_sqlite: read %s rows for a generation copy: %w", docidMap.fts, err)
	}
	defer rows.Close()
	out := make([]generationCopyFTSRow, 0, len(docids))
	for rows.Next() {
		values := make([]any, len(carried)+len(ftsColumns))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		out = append(out, generationCopyFTSRow{
			carried: values[:len(carried)],
			body:    values[len(carried):],
		})
	}
	return out, rows.Err()
}

// generationCopyWriteFTSChunk writes one batch into the virtual table and its
// map under freshly minted docids. Both statements consume the same docid for
// the same row, which is what keeps the map an exact address of what landed.
func generationCopyWriteFTSChunk(
	ctx context.Context, tx *sql.Tx, docidMap ftsDocidMap,
	carried, ftsColumns []string, rows []generationCopyFTSRow, to int64, next *int64,
) (int64, error) {
	var documents strings.Builder
	documents.WriteString(`INSERT INTO ` + docidMap.fts + ` (rowid, ` + strings.Join(ftsColumns, ", ") + `) VALUES `)
	documentArgs := make([]any, 0, len(rows)*(len(ftsColumns)+1))

	var owners strings.Builder
	owners.WriteString(`INSERT INTO ` + docidMap.ids + ` (` + viewGenColumnName + `, fts_rowid`)
	for _, column := range carried {
		owners.WriteString(", ")
		owners.WriteString(column)
	}
	owners.WriteString(`) VALUES `)
	ownerArgs := make([]any, 0, len(rows)*(len(carried)+2))

	for i, row := range rows {
		docid := *next
		*next++
		if i > 0 {
			documents.WriteByte(',')
			owners.WriteByte(',')
		}
		documents.WriteString("(?" + strings.Repeat(", ?", len(ftsColumns)) + ")")
		documentArgs = append(documentArgs, docid)
		documentArgs = append(documentArgs, row.body...)

		owners.WriteString("(?, ?" + strings.Repeat(", ?", len(carried)) + ")")
		ownerArgs = append(ownerArgs, to, docid)
		ownerArgs = append(ownerArgs, row.carried...)
	}
	if _, err := tx.ExecContext(ctx, documents.String(), documentArgs...); err != nil {
		return 0, fmt.Errorf("store_sqlite: write %s rows for a generation copy: %w", docidMap.fts, err)
	}
	if _, err := tx.ExecContext(ctx, owners.String(), ownerArgs...); err != nil {
		return 0, fmt.Errorf("store_sqlite: write %s rows for a generation copy: %w", docidMap.ids, err)
	}
	return int64(len(rows)), nil
}

// generationPayloadEmptyTx is generationPayloadEmpty inside the caller's own
// transaction: the emptiness a copy depends on has to be proved by the same
// snapshot that then writes, or a writer that committed in between would make
// the proof stale before the first INSERT.
func generationPayloadEmptyTx(ctx context.Context, tx *sql.Tx, generationID int64) (bool, error) {
	var empty int
	err := tx.QueryRowContext(ctx, `
SELECT NOT EXISTS(SELECT 1 FROM nodes WHERE view_gen > 0 AND view_gen = ?)
   AND NOT EXISTS(SELECT 1 FROM edges WHERE view_gen > 0 AND view_gen = ?)`,
		generationID, generationID).Scan(&empty)
	if err != nil {
		return false, err
	}
	return empty == 1, nil
}
