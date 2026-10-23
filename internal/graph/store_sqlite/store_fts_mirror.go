package store_sqlite

import (
	"database/sql"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ftsMirrorChunkRows bounds the rows per lean mirror read/write statement —
// the shape policy the lean mirror was tuned with, not a variable-limit
// assumption: mirrorChunkSizes derives the actual row counts from the
// store's runtime SQLite variable limit (each write row binds
// mirrorRowParams host params; each read row binds 2 plus the view_gen
// join), so a build with a conservative SQLITE_LIMIT_VARIABLE_NUMBER gets
// smaller statements instead of a prepare failure.
const ftsMirrorChunkRows = 512

// mirrorRowParams is the bind count one mirror write row carries (id,
// view_gen, kind, name, file_path, repo_prefix).
const mirrorRowParams = 6

// mirrorChunkSizes derives the per-statement row counts for the lean mirror
// from the store's probed variable limit (issue #36,
// R3-sql-var-ceiling-mirror-read): the write binds mirrorRowParams per row,
// the diff read binds 2 per id plus one view_gen bind — accounted by
// lowering the limit by one before the shared headroom math. Both stay
// capped by ftsMirrorChunkRows.
func mirrorChunkSizes(variableLimit int) (writeRows, readRows int) {
	writeRows = batchRowsForVariableLimit(variableLimit, mirrorRowParams, ftsMirrorChunkRows)
	readRows = batchRowsForVariableLimit(variableLimit-1, 2, ftsMirrorChunkRows)
	return writeRows, readRows
}

// mirrorRow is the six-column mirror projection a node row carries for the
// engine's FTS readers: the filing join (repo_prefix), tier-0 exact-name
// lookups (name, kind) and the scoped-search orphan predicate (id).
type mirrorRow struct {
	kind       graph.NodeKind
	name       string
	filePath   string
	repoPrefix string
	present    bool
}

// MirrorFTSNodes implements graph.FTSNodeMirror: the lean node-row mirror
// write the FTS filing path needs, without AddBatch's graph-write machinery
// (analysis invalidation, mutation receipts, builtin stubs, JSONB payload
// encoding). The engine's mirror readers touch only the six mirrored
// columns; every other node column keeps its DDL default or NULL, which
// scanNodeWithMeta decodes as zero values (a NULL meta blob skips decodeMeta
// entirely). Tier-0 exact-name lookups and the scoped-search orphan
// predicate see exactly the rows they would see through AddBatch — pinned
// by store_fts_mirror_test.go.
//
// The write is diffed against the mirrored state and is update-only on
// conflict: a row already carrying a full node keeps its non-mirrored
// columns (INSERT OR REPLACE would wipe them — issue #36,
// R3-replace-delete-side-effects): rows whose six-column
// projection is unchanged are skipped, so steady-state rebuilds (the same
// resident graph re-mirrored at every boot) pay one set-oriented read pass
// instead of a full rewrite — rewriting 100k rows through the nodes table's
// secondary indexes measured worse than the JSONB path it replaces. Callers
// outside the FTS mirror contract must NOT use this: the written rows are
// NOT complete nodes and must never be round-tripped through AddNode /
// AddBatch (the graph.LightNodeReader rule).
func (s *Store) MirrorFTSNodes(nodes []*graph.Node) error {
	// Dedupe by ID with last-write-wins, mirroring the AddBatch upsert
	// semantics a caller would otherwise get per chunk.
	type incoming struct {
		id                   string
		kind                 graph.NodeKind
		name, filePath, repo string
	}
	order := make([]string, 0, len(nodes))
	byID := make(map[string]incoming, len(nodes))
	for _, n := range nodes {
		if n == nil || n.ID == "" || graph.IsProxyNode(n) {
			continue
		}
		if _, dup := byID[n.ID]; !dup {
			order = append(order, n.ID)
		}
		byID[n.ID] = incoming{n.ID, n.Kind, n.Name, n.FilePath, n.RepoPrefix}
	}
	if len(order) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	variableLimit := s.sqliteBatchVariableLimitLocked()
	writeRows, readRows := mirrorChunkSizes(variableLimit)

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(order); start += ftsMirrorChunkRows {
		end := minInt(start+ftsMirrorChunkRows, len(order))
		chunk := order[start:end]

		// One set-oriented read of the mirrored projection for the whole
		// chunk, sub-chunked under the store's SQLite variable ceiling (2
		// binds per id plus the view_gen join) — the same accounting the
		// AddBatch write path applies to its statements.
		existing := make(map[string]mirrorRow, len(chunk))
		for rs := 0; rs < len(chunk); rs += readRows {
			re := minInt(rs+readRows, len(chunk))
			part, err := s.mirrorExistingTx(tx, chunk[rs:re])
			if err != nil {
				return err
			}
			for id, row := range part {
				existing[id] = row
			}
		}

		// Collect the rows whose six-column projection actually changed.
		pending := make([]string, 0, len(chunk))
		for _, id := range chunk {
			want := byID[id]
			if have, ok := existing[id]; ok && have.present &&
				have.kind == want.kind && have.name == want.name &&
				have.filePath == want.filePath && have.repoPrefix == want.repo {
				continue
			}
			pending = append(pending, id)
		}

		for ws := 0; ws < len(pending); ws += writeRows {
			we := minInt(ws+writeRows, len(pending))
			var (
				sb   strings.Builder
				args = make([]any, 0, (we-ws)*mirrorRowParams)
			)
			sb.WriteString(`INSERT INTO nodes (id, view_gen, kind, name, file_path, repo_prefix) VALUES `)
			for i, id := range pending[ws:we] {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(`(?, ?, ?, ?, ?, ?)`)
				want := byID[id]
				args = append(args, id, s.viewGen, want.kind, want.name, want.filePath, want.repo)
			}
			// ON CONFLICT UPDATE instead of INSERT OR REPLACE: REPLACE is a
			// delete+insert of the whole row — on a row already carrying a
			// full AddBatch-written node it resets every non-mirrored column
			// to its DDL default and fires any delete-side triggers / FK
			// actions per replaced row (issue #36,
			// R3-replace-delete-side-effects). The update touches only the
			// six mirrored columns; the diff pass above guarantees every row
			// reaching this statement is a real change.
			sb.WriteString(` ON CONFLICT(id, view_gen) DO UPDATE SET ` +
				`kind=excluded.kind, name=excluded.name, file_path=excluded.file_path, repo_prefix=excluded.repo_prefix`)
			if _, err := tx.Exec(sb.String(), args...); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// mirrorExistingTx reads the current six-column mirror projection for the
// chunk's IDs in one VALUES join — the same set-oriented shape the FTS
// filing lookup uses, so the diff pass costs one seek per row instead of
// one query per row.
func (s *Store) mirrorExistingTx(tx *sql.Tx, ids []string) (map[string]mirrorRow, error) {
	var sb strings.Builder
	sb.WriteString(`WITH wanted(ord, node_id) AS (VALUES `)
	args := make([]any, 0, len(ids)*2)
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`(?, ?)`)
		args = append(args, i, id)
	}
	sb.WriteString(`)
SELECT wanted.node_id, n.kind, n.name, n.file_path, n.repo_prefix, n.id IS NOT NULL
FROM wanted
LEFT JOIN nodes n ON n.id = wanted.node_id AND n.view_gen = ?`)
	args = append(args, s.viewGen)

	rows, err := tx.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]mirrorRow, len(ids))
	for rows.Next() {
		var id string
		var row mirrorRow
		var kind, name, filePath, repo sql.NullString
		if err := rows.Scan(&id, &kind, &name, &filePath, &repo, &row.present); err != nil {
			return nil, err
		}
		row.kind = graph.NodeKind(kind.String)
		row.name = name.String
		row.filePath = filePath.String
		row.repoPrefix = repo.String
		out[id] = row
	}
	return out, rows.Err()
}
