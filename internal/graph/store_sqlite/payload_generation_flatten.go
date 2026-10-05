package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Copy-based generation primitives for the per-file delta model.
//
// A checkout's working-tree state is a chain of small generations, each a delta
// over the one below. Two operations on such a chain need no parse at all,
// because every row they write is already in the store:
//
//   - carrying a generation over to another parent (a primary base advance:
//     the rows do not depend on which base the committed tree was composed
//     from) — CopyGenerationPayloadWhole;
//   - folding a chain into one generation over the chain's root parent (the
//     phase-1 compactor, and the fold of a large overlay) —
//     FlattenGenerationChain.
//
// Both copy EVERY row of the source generations, whatever repository prefix
// a row carries: a checkout generation belongs to one checkout, and a pathless
// external stub it wrote under another prefix is as much its payload as a
// symbol of its own files. That is the difference from CopyPayloadGeneration,
// which copies one repository out of a shared base corpus.

// payloadTableShape is what the copy needs to know about one generation-keyed
// table: its writable columns and which of them scope a row.
type payloadTableShape struct {
	table   string
	columns []string
	file    bool // a file_path column: the row belongs to a path
	node    bool // a node_id column: the row belongs to a node
	from    bool // a from_id column: the row belongs to a source's adjacency
	to      bool // a to_id column: the row names a target identity
}

// payloadRowTables lists the generation-keyed row tables a flatten composes,
// nodes and edges first, masks, manifests and the FTS docid maps excluded (each
// has its own rule).
func payloadRowTables(ctx context.Context, tx *sql.Tx) ([]payloadTableShape, error) {
	skip := map[string]struct{}{}
	for _, m := range generationMaskTables {
		skip[m.table] = struct{}{}
	}
	for _, m := range generationInputManifestTables {
		skip[m.table] = struct{}{}
	}
	for _, m := range generationFTSDocidMaps {
		skip[m.ids] = struct{}{}
	}
	names := []string{"nodes", "edges"}
	for _, table := range payloadSweepTables() {
		if _, skipped := skip[table]; skipped {
			continue
		}
		names = append(names, table)
	}
	out := make([]payloadTableShape, 0, len(names))
	for _, table := range names {
		columns, err := generationCopyColumns(ctx, tx, table)
		if err != nil {
			return nil, err
		}
		shape := payloadTableShape{table: table}
		for _, column := range columns {
			if table == "edges" && column == "id" {
				continue // a physical row id; the destination mints its own
			}
			shape.columns = append(shape.columns, column)
			switch column {
			case "file_path":
				shape.file = true
			case "node_id":
				shape.node = true
			case "from_id":
				shape.from = true
			case "to_id":
				shape.to = true
			}
		}
		if table == "nodes" {
			shape.node = false // nodes are addressed by id, handled directly
		}
		out = append(out, shape)
	}
	return out, nil
}

// beginGenerationCopy opens the write transaction a copy into `to` runs in,
// after the refusals every generation copy shares.
func (s *Store) beginGenerationCopy(ctx context.Context, to int64) (*sql.Tx, func(), error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if s.coreless() {
		return nil, nil, fmt.Errorf("%w: a generation copy needs an open store", ErrCatalogInvalidValue)
	}
	if to <= baseViewGeneration {
		return nil, nil, fmt.Errorf("%w: a generation copy may not write the base corpus, got destination %d", ErrCatalogInvalidValue, to)
	}
	destination, err := s.AtManagedGeneration(to)
	if err != nil {
		return nil, nil, err
	}
	if err := destination.refuseSealedPayloadWrite(); err != nil {
		return nil, nil, err
	}
	s.writeMu.Lock()
	tx, err := destination.beginWriteContext(ctx)
	if err != nil {
		s.writeMu.Unlock()
		return nil, nil, err
	}
	release := func() {
		_ = tx.Rollback()
		s.writeMu.Unlock()
	}
	empty, err := generationPayloadEmptyTx(ctx, tx, to)
	if err != nil {
		release()
		return nil, nil, err
	}
	if !empty {
		release()
		return nil, nil, fmt.Errorf("%w: generation %d", ErrGenerationBulkLoadPopulated, to)
	}
	return tx, release, nil
}

// CopyGenerationPayloadWhole materialises every row of generation from — all
// repositories, masks, input manifest and FTS projections included — as the
// payload of the empty building generation to. It is the carry-over of a
// working-tree generation to another parent: the destination's catalog row
// names the new parent, the rows are the source's.
func (s *Store) CopyGenerationPayloadWhole(ctx context.Context, from, to int64) (counts GenerationCopyCounts, err error) {
	if !s.coreless() {
		mark, started := readWALWriteMark(s.dbPath), time.Now()
		defer func() { s.logGenerationCopy("copy", []int64{from}, to, &counts, mark, started, err) }()
	}
	if from <= baseViewGeneration || from == to {
		return counts, fmt.Errorf("%w: a whole-generation copy needs a derived source other than its destination (%d -> %d)",
			ErrCatalogInvalidValue, from, to)
	}
	tx, release, err := s.beginGenerationCopy(ctx, to)
	if err != nil {
		return counts, err
	}
	defer release()
	shapes, err := payloadRowTables(ctx, tx)
	if err != nil {
		return counts, err
	}
	for _, shape := range shapes {
		moved, err := insertGenerationRowsTx(ctx, tx, shape, from, to, nil, false)
		if err != nil {
			return counts, err
		}
		counts.Rows += moved
		switch shape.table {
		case "nodes":
			counts.Nodes = moved
		case "edges":
			counts.Edges = moved
		}
	}
	for _, table := range append(maskTableNames(), manifestTableNames()...) {
		moved, err := insertWholeTableTx(ctx, tx, table, from, to)
		if err != nil {
			return counts, err
		}
		counts.Rows += moved
	}
	for _, docidMap := range generationFTSDocidMaps {
		moved, err := copyFTSRowsFilteredTx(ctx, tx, docidMap, from, to, "")
		if err != nil {
			return counts, err
		}
		counts.Rows += moved
	}
	if err := tx.Commit(); err != nil {
		return GenerationCopyCounts{}, err
	}
	if counts.Rows > 0 {
		s.constantInputCounter(to).Add(1)
	}
	return counts, nil
}

func maskTableNames() []string {
	out := make([]string, 0, len(generationMaskTables))
	for _, m := range generationMaskTables {
		out = append(out, m.table)
	}
	return out
}

func manifestTableNames() []string {
	out := make([]string, 0, len(generationInputManifestTables))
	for _, m := range generationInputManifestTables {
		out = append(out, m.table)
	}
	return out
}

// FlattenGenerationChain writes the composition of a chain of generations
// (oldest first, each one a layer over the one before) as the payload of the
// empty building generation to, which is to be published over the chain's own
// root parent. No input manifest is written: the caller records the chain's
// resolved manifest as a full one.
//
// The result carries what a reader of the chain would see above the root
// parent, by the layer contract (graphview.GenerationLayer):
//
//   - a path a member claims (replace or delete) shows that member's rows and
//     nothing from below it; the claim itself is kept with the top-most mode;
//   - a node identity a member masks (a legacy tombstone or an identity
//     replacement) hides the same identity below it, and the mask is kept
//     for the root parent; an identity replacement whose row a member above
//     hid is dropped;
//   - an edge-source marker (and a legacy tombstone) of a member replaces its
//     source's outgoing edges recorded below it outside the source's own
//     file;
//   - an edge below a member that speaks for one of its endpoints (covers the
//     endpoint's file, or masks its identity) without carrying a node for it
//     is hidden: the endpoint is gone from the view, and so is every edge
//     recorded elsewhere that names it;
//   - rows at a member's read-only context paths are never served, so they
//     are not carried, and context marks are not carried either (they claim
//     nothing).
//
// Sidecar rows follow the row they describe: a row with a file_path belongs to
// its path, one with a node_id to its node, one with a from_id additionally to
// its source's adjacency; a row with none of them (per-repository state) is
// taken from the top-most member that carries it.
//
// It refuses rather than guess: a member carrying a mask kind the flatten does
// not know is an error, and so is a row a composition rule would have to
// overwrite. The caller verifies the result against the chain's composed view
// before it publishes anything.
func (s *Store) FlattenGenerationChain(ctx context.Context, chain []int64, to int64) (counts GenerationCopyCounts, err error) {
	if !s.coreless() {
		mark, started := readWALWriteMark(s.dbPath), time.Now()
		defer func() { s.logGenerationCopy("fold", chain, to, &counts, mark, started, err) }()
	}
	if len(chain) == 0 {
		return counts, fmt.Errorf("%w: an empty chain has nothing to flatten", ErrCatalogInvalidValue)
	}
	for _, id := range chain {
		if id <= baseViewGeneration || id == to {
			return counts, fmt.Errorf("%w: chain member %d cannot be flattened into %d", ErrCatalogInvalidValue, id, to)
		}
	}
	tx, release, err := s.beginGenerationCopy(ctx, to)
	if err != nil {
		return counts, err
	}
	defer release()
	shapes, err := payloadRowTables(ctx, tx)
	if err != nil {
		return counts, err
	}

	// Composed top-down: a member's rows are carried unless a member above it
	// hides them. hidden* accumulate the claims of every member already
	// carried (the ones above the member being read).
	hiddenPaths := map[string]struct{}{}
	hiddenIDs := map[string]struct{}{}
	hiddenSources := map[string]struct{}{}
	var speakers []flattenSpeaker
	for i := len(chain) - 1; i >= 0; i-- {
		member := chain[i]
		masks, err := readGenerationMasksTx(ctx, tx, member)
		if err != nil {
			return counts, err
		}
		endpoints, err := flattenHiddenEndpointsTx(ctx, tx, member, speakers)
		if err != nil {
			return counts, err
		}
		exclude := &flattenExclusion{
			paths:     hiddenPaths,
			context:   masks.context,
			ids:       hiddenIDs,
			sources:   hiddenSources,
			endpoints: endpoints,
		}
		if err := flattenScratch(ctx, tx, exclude); err != nil {
			return counts, err
		}
		for _, shape := range shapes {
			moved, err := insertGenerationRowsTx(ctx, tx, shape, member, to, exclude, true)
			if err != nil {
				return counts, fmt.Errorf("store_sqlite: flatten %s of generation %d: %w", shape.table, member, err)
			}
			counts.Rows += moved
			switch shape.table {
			case "nodes":
				counts.Nodes += moved
			case "edges":
				counts.Edges += moved
			}
		}
		moved, err := flattenFTSTx(ctx, tx, member, to, exclude)
		if err != nil {
			return counts, err
		}
		counts.Rows += moved
		if err := flattenMasksTx(ctx, tx, member, to, masks); err != nil {
			return counts, err
		}

		// What this member claims hides everything below it.
		memberIDs, err := generationNodeIDsTx(ctx, tx, member, masks.context)
		if err != nil {
			return counts, err
		}
		for p := range masks.covered {
			hiddenPaths[p] = struct{}{}
		}
		for id := range masks.identity {
			hiddenIDs[id] = struct{}{}
		}
		for id := range memberIDs {
			hiddenIDs[id] = struct{}{}
		}
		speakers = append(speakers, flattenSpeaker{covered: masks.covered, identity: masks.identity, carried: memberIDs})
		for id := range masks.sources {
			if !idInPaths(id, masks.covered) {
				hiddenSources[id] = struct{}{}
			}
		}
		for id, kind := range masks.identity {
			if kind == string(NodeIdentityMaskLegacy) && !idInPaths(id, masks.covered) {
				hiddenSources[id] = struct{}{}
			}
		}
	}
	// An identity replacement whose row a member above hid replaces nothing
	// any more (publish validation refuses one without its row). A legacy
	// tombstone is kept whether or not the fold carries a row for its
	// identity: with a row it is how a layer speaks for a pathless identity
	// (the stub rows a working-tree build carries), without one it removes
	// the identity below.
	if _, err := tx.ExecContext(ctx, `
DELETE FROM generation_node_tombstones
 WHERE view_gen = ?1 AND claim_kind = 'identity_replace'
   AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.view_gen = ?1 AND n.id = node_id)`,
		to); err != nil {
		return counts, fmt.Errorf("store_sqlite: settle flattened identity masks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return GenerationCopyCounts{}, err
	}
	if counts.Rows > 0 {
		s.constantInputCounter(to).Add(1)
	}
	return counts, nil
}

// flattenExclusion is what hides a member's rows during a flatten.
type flattenExclusion struct {
	// scratch prefixes the temp tables the exclusion is loaded into
	// ("flatten" when empty); the stepped fold uses its own.
	scratch   string
	paths     map[string]struct{} // claimed by a member above
	context   map[string]struct{} // this member's read-only context paths
	ids       map[string]struct{} // identities a member above masks or re-emits
	sources   map[string]struct{} // sources whose outgoing edges a member above replaces
	endpoints map[string]struct{} // identities a member above speaks for without carrying them
}

func (e *flattenExclusion) scratchName() string {
	if e == nil || e.scratch == "" {
		return "flatten"
	}
	return e.scratch
}

// flattenSpeaker is what one member already carried speaks for: the paths it
// claims, the identities it masks and the node identities it carries.
type flattenSpeaker struct {
	covered  map[string]string
	identity map[string]string
	carried  map[string]struct{}
}

// flattenHiddenEndpointsTx lists the edge endpoints of one member that a
// member above speaks for without carrying a node for them — an identity the
// layer contract hides (graph.OverlaidView's identity visibility), so every
// edge of the member naming it is hidden too, wherever it was recorded.
func flattenHiddenEndpointsTx(ctx context.Context, tx *sql.Tx, member int64, speakers []flattenSpeaker) (map[string]struct{}, error) {
	if len(speakers) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT from_id FROM edges WHERE view_gen = ?1 UNION SELECT to_id FROM edges WHERE view_gen = ?1`, member)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hidden := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		for _, s := range speakers {
			_, masked := s.identity[id]
			if !masked && !idInPaths(id, s.covered) {
				continue
			}
			if _, carried := s.carried[id]; !carried {
				hidden[id] = struct{}{}
				break
			}
		}
	}
	return hidden, rows.Err()
}

// generationMaskSet is one generation's masks, read inside the transaction.
type generationMaskSet struct {
	covered  map[string]string // path -> replace/delete
	context  map[string]struct{}
	identity map[string]string // node id -> claim kind
	sources  map[string]struct{}
}

func readGenerationMasksTx(ctx context.Context, tx *sql.Tx, generation int64) (generationMaskSet, error) {
	set := generationMaskSet{
		covered:  map[string]string{},
		context:  map[string]struct{}{},
		identity: map[string]string{},
		sources:  map[string]struct{}{},
	}
	err := scanPairsTx(ctx, tx, `SELECT file_path, ownership_mode FROM generation_file_masks WHERE view_gen = ?`, generation,
		func(p, mode string) error {
			switch OwnershipMode(mode) {
			case OwnershipReplace, OwnershipDelete:
				set.covered[p] = mode
			case OwnershipContext:
				set.context[p] = struct{}{}
			default:
				return fmt.Errorf("store_sqlite: generation %d claims %q on %q, a mode a flatten cannot compose", generation, mode, p)
			}
			return nil
		})
	if err != nil {
		return set, err
	}
	err = scanPairsTx(ctx, tx, `SELECT node_id, claim_kind FROM generation_node_tombstones WHERE view_gen = ?`, generation,
		func(id, kind string) error {
			if kind != string(NodeIdentityMaskLegacy) && kind != string(NodeIdentityMaskReplace) {
				return fmt.Errorf("store_sqlite: generation %d masks %q with kind %q, a kind a flatten cannot compose", generation, id, kind)
			}
			set.identity[id] = kind
			return nil
		})
	if err != nil {
		return set, err
	}
	err = scanPairsTx(ctx, tx, `SELECT source_id, ownership_mode FROM generation_edge_sources WHERE view_gen = ?`, generation,
		func(id, mode string) error {
			if OwnershipMode(mode) != OwnershipReplace {
				return fmt.Errorf("store_sqlite: generation %d marks source %q with mode %q, a mode a flatten cannot compose", generation, id, mode)
			}
			set.sources[id] = struct{}{}
			return nil
		})
	return set, err
}

// scanPairsTx runs a two-column string query for one generation and hands
// every row to visit.
func scanPairsTx(ctx context.Context, tx *sql.Tx, query string, generation int64, visit func(a, b string) error) error {
	rows, err := tx.QueryContext(ctx, query, generation)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return err
		}
		if err := visit(a, b); err != nil {
			return err
		}
	}
	return rows.Err()
}

// generationNodeIDsTx lists the node ids a generation carries outside its
// context paths: the identities it speaks for.
func generationNodeIDsTx(ctx context.Context, tx *sql.Tx, generation int64, context map[string]struct{}) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, file_path FROM nodes WHERE view_gen = ?`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id, p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		if _, ctxPath := context[p]; ctxPath {
			continue
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// idInPaths reports whether a node id's file (the part before "::", or the
// whole id for a file node) is one of paths.
func idInPaths(id string, paths map[string]string) bool {
	file := id
	if i := strings.Index(id, "::"); i >= 0 {
		file = id[:i]
	}
	_, ok := paths[file]
	return ok
}

// flattenScratch fills the temporary exclusion tables one member's insert
// statements join against. They are connection-local temp tables, created on
// first use and emptied per member.
func flattenScratch(ctx context.Context, tx *sql.Tx, exclude *flattenExclusion) error {
	x := exclude.scratchName()
	for _, ddl := range []string{
		`CREATE TEMP TABLE IF NOT EXISTS ` + x + `_hidden_paths (p TEXT PRIMARY KEY) WITHOUT ROWID`,
		`CREATE TEMP TABLE IF NOT EXISTS ` + x + `_hidden_ids (id TEXT PRIMARY KEY) WITHOUT ROWID`,
		`CREATE TEMP TABLE IF NOT EXISTS ` + x + `_hidden_sources (id TEXT PRIMARY KEY) WITHOUT ROWID`,
		`CREATE TEMP TABLE IF NOT EXISTS ` + x + `_hidden_endpoints (id TEXT PRIMARY KEY) WITHOUT ROWID`,
		`DELETE FROM ` + x + `_hidden_paths`,
		`DELETE FROM ` + x + `_hidden_ids`,
		`DELETE FROM ` + x + `_hidden_sources`,
		`DELETE FROM ` + x + `_hidden_endpoints`,
	} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	fill := func(table, column string, values map[string]struct{}) error {
		if len(values) == 0 {
			return nil
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO `+table+` (`+column+`) VALUES (?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for v := range values {
			if _, err := stmt.ExecContext(ctx, v); err != nil {
				return err
			}
		}
		return nil
	}
	if err := fill(x+"_hidden_paths", "p", exclude.paths); err != nil {
		return err
	}
	if err := fill(x+"_hidden_paths", "p", exclude.context); err != nil {
		return err
	}
	if err := fill(x+"_hidden_ids", "id", exclude.ids); err != nil {
		return err
	}
	if err := fill(x+"_hidden_sources", "id", exclude.sources); err != nil {
		return err
	}
	return fill(x+"_hidden_endpoints", "id", exclude.endpoints)
}

// insertGenerationRowsTx copies one table's rows of generation from into to.
// With exclude set (a flatten), rows the exclusion hides are skipped and a row
// the destination already holds under the same key is kept (the destination
// holds the member above's row, which wins); without it every row is copied
// and a conflict is an error.
func insertGenerationRowsTx(
	ctx context.Context, tx *sql.Tx, shape payloadTableShape, from, to int64,
	exclude *flattenExclusion, flatten bool,
) (int64, error) {
	return insertGenerationRowsBoundedTx(ctx, tx, shape, from, to, exclude, flatten, nil, nil)
}

// insertGenerationRowsBoundedTx is insertGenerationRowsTx with extra
// predicates over the source rows (alias t), for a page of them.
func insertGenerationRowsBoundedTx(
	ctx context.Context, tx *sql.Tx, shape payloadTableShape, from, to int64,
	exclude *flattenExclusion, flatten bool, bounds []string, boundArgs []any,
) (int64, error) {
	projection := make([]string, 0, len(shape.columns))
	args := []any{}
	for _, column := range shape.columns {
		if column == viewGenColumnName {
			projection = append(projection, "?")
			args = append(args, to)
			continue
		}
		projection = append(projection, "t."+column)
	}
	where := []string{"t." + viewGenColumnName + " = ?"}
	args = append(args, from)
	if exclude != nil {
		// The exclusion tables were filled for this member by the caller.
		x := exclude.scratchName()
		if shape.file {
			where = append(where, "t.file_path NOT IN (SELECT p FROM "+x+"_hidden_paths)")
		}
		switch {
		case shape.table == "nodes":
			where = append(where, "t.id NOT IN (SELECT id FROM "+x+"_hidden_ids)")
		case shape.node:
			// A node's sidecar follows the node: carried when the member
			// carries the node itself, or describes a node it does not
			// carry that nothing above hides.
			where = append(where, `t.node_id NOT IN (SELECT id FROM `+x+`_hidden_ids)`,
				`NOT EXISTS (SELECT 1 FROM nodes n WHERE n.view_gen = t.view_gen AND n.id = t.node_id
				   AND n.file_path IN (SELECT p FROM `+x+`_hidden_paths))`)
		}
		if shape.from {
			where = append(where, "t.from_id NOT IN (SELECT id FROM "+x+"_hidden_sources)",
				"t.from_id NOT IN (SELECT id FROM "+x+"_hidden_endpoints)")
		}
		if shape.to {
			where = append(where, "t.to_id NOT IN (SELECT id FROM "+x+"_hidden_endpoints)")
		}
	}
	where = append(where, bounds...)
	args = append(args, boundArgs...)
	verb := "INSERT INTO "
	if flatten && shape.table != "nodes" {
		// The member above was carried first, so a key both hold keeps the
		// upper row: per-repository state, a sidecar row of the same node,
		// an identical pathless edge. A node is never carried twice — the
		// exclusion hides every identity a member above speaks for.
		verb = "INSERT OR IGNORE INTO "
	}
	query := verb + shape.table + ` (` + strings.Join(shape.columns, ", ") + `)
SELECT ` + strings.Join(projection, ", ") + ` FROM ` + shape.table + ` t WHERE ` + strings.Join(where, " AND ")
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store_sqlite: copy %s from generation %d to %d: %w", shape.table, from, to, err)
	}
	return result.RowsAffected()
}

// insertWholeTableTx copies every row of one generation-keyed table.
func insertWholeTableTx(ctx context.Context, tx *sql.Tx, table string, from, to int64) (int64, error) {
	columns, err := generationCopyColumns(ctx, tx, table)
	if err != nil {
		return 0, err
	}
	projection := make([]string, 0, len(columns))
	args := []any{}
	for _, column := range columns {
		if column == viewGenColumnName {
			projection = append(projection, "?")
			args = append(args, to)
			continue
		}
		projection = append(projection, column)
	}
	args = append(args, from)
	result, err := tx.ExecContext(ctx, `INSERT INTO `+table+` (`+strings.Join(columns, ", ")+`)
SELECT `+strings.Join(projection, ", ")+` FROM `+table+` WHERE `+viewGenColumnName+` = ?`, args...)
	if err != nil {
		return 0, fmt.Errorf("store_sqlite: copy %s from generation %d to %d: %w", table, from, to, err)
	}
	return result.RowsAffected()
}

// flattenMasksTx carries one member's masks into the flattened generation,
// the member above (already carried) winning every key both hold. Context
// marks are not carried: they claim nothing.
func flattenMasksTx(ctx context.Context, tx *sql.Tx, member, to int64, _ generationMaskSet) error {
	for _, op := range foldMaskOperations {
		if _, err := tx.ExecContext(ctx, op.query, op.args(member, to)...); err != nil {
			return fmt.Errorf("store_sqlite: flatten %s of generation %d: %w", op.table, member, err)
		}
	}
	return nil
}

// flattenFTSTx carries one member's FTS documents: symbol documents follow
// their node, content documents their path.
func flattenFTSTx(ctx context.Context, tx *sql.Tx, member, to int64, exclude *flattenExclusion) (int64, error) {
	var moved int64
	for _, docidMap := range generationFTSDocidMaps {
		filter, args := flattenFTSFilter(docidMap, member, to, exclude)
		n, err := copyFTSRowsFilteredTx(ctx, tx, docidMap, member, to, filter, args...)
		if err != nil {
			return moved, err
		}
		moved += n
	}
	return moved, nil
}

// copyFTSRowsFilteredTx is the FTS copy with an extra predicate over the
// source map's rows.
func copyFTSRowsFilteredTx(ctx context.Context, tx *sql.Tx, docidMap ftsDocidMap, from, to int64, filter string, filterArgs ...any) (int64, error) {
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
	docids, err := readDocidsTx(ctx, tx,
		`SELECT fts_rowid FROM `+docidMap.ids+` WHERE `+viewGenColumnName+` = ?`+filter+` ORDER BY fts_rowid`,
		append([]any{from}, filterArgs...)...)
	if err != nil {
		return 0, fmt.Errorf("store_sqlite: read %s docids for a copy: %w", docidMap.ids, err)
	}
	if len(docids) == 0 {
		return 0, nil
	}
	next, err := nextFTSRowIDTx(tx, docidMap.fts)
	if err != nil {
		return 0, err
	}
	var moved int64
	for start := 0; start < len(docids); start += generationCopyFTSChunk {
		end := min(start+generationCopyFTSChunk, len(docids))
		chunk, err := generationCopyFTSChunkRows(ctx, tx, docidMap, carried, ftsColumns, docids[start:end])
		if err != nil {
			return moved, err
		}
		if len(chunk) == 0 {
			continue
		}
		written, err := generationCopyWriteFTSChunk(ctx, tx, docidMap, carried, ftsColumns, chunk, to, &next)
		if err != nil {
			return moved, err
		}
		moved += written
	}
	return moved, nil
}

// readDocidsTx reads one column of FTS docids, whole, before any write.
func readDocidsTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
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

func flattenContractBoundaryReceiptsTx(ctx context.Context, tx *sql.Tx, member, to int64) error {
	// Upper file receipts replace every key membership of that exact owner,
	// including explicit empty/deleted negative membership. Pending union keys
	// remain visible until outer acceptance, independent of whole-file masks.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO generation_contract_boundary_keys(view_gen,key_kind,lookup_key,repo_prefix,checkout_id,file_path) SELECT ?,key_kind,lookup_key,repo_prefix,checkout_id,file_path FROM generation_contract_boundary_keys k WHERE k.view_gen=? AND NOT EXISTS(SELECT 1 FROM generation_contract_boundary_receipt r WHERE r.view_gen=? AND r.repo_prefix=k.repo_prefix AND r.checkout_id=k.checkout_id AND r.file_path=k.file_path)`, to, member, to); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO generation_contract_boundary_receipt(view_gen,repo_prefix,checkout_id,file_path,version,fingerprint,source_fingerprint,accepted,receipt) SELECT ?,repo_prefix,checkout_id,file_path,version,fingerprint,source_fingerprint,accepted,receipt FROM generation_contract_boundary_receipt WHERE view_gen=?`, to, member); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO generation_contract_boundary_baseline(view_gen,repo_prefix,checkout_id,version,fingerprint) SELECT ?,repo_prefix,checkout_id,version,fingerprint FROM generation_contract_boundary_baseline WHERE view_gen=?`, to, member)
	return err
}
