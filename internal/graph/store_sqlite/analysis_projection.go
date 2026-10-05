package store_sqlite

import (
	"context"
	"database/sql"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// BindAnalysisPages returns a non-owning handle over the same generation and
// context. Only callers with an analysis revision publication fence opt in.
func (s *Store) BindAnalysisPages() graph.Store {
	if s == nil {
		return s
	}
	bound := *s
	bound.ownsCore = false
	bound.analysisPaged = true
	return &bound
}

var _ graph.AnalysisPageBinder = (*Store)(nil)

// analysisProjectionPageSize bounds the metadata-free rows held in Go and the
// lifetime of each SQLite snapshot. No cursor survives a consumer callback.
var analysisProjectionPageSize = 4096

var nodesLightOrderedSQL = `SELECT ` + lookupNodeSummaryCols + ` FROM nodes INDEXED BY nodes_by_generation WHERE view_gen = ? ORDER BY id`

// NodesLightSeq retains its single-statement identity/location snapshot unless
// a revision-validated analysis explicitly binds bounded page reads.
func (s *Store) NodesLightSeq() iter.Seq[*graph.Node] {
	if s.analysisPaged {
		return s.nodesLightSeqPages()
	}
	return func(yield func(*graph.Node) bool) {
		rows, err := s.db.Query(nodesLightOrderedSQL, s.viewGen)
		if err != nil {
			panicOnFatal(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			node, scanErr := scanNodeSummary(rows)
			if scanErr != nil {
				panicOnFatal(scanErr)
				return
			}
			if node != nil && !yield(node) {
				return
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			panicOnFatal(rowsErr)
		}
	}
}

func (s *Store) nodesLightSeqPages() iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		ctx := s.readContext()
		indexed, err := s.analysisProjectionIndexPresent(ctx, nodesByGenerationIndexName)
		if err != nil {
			analysisProjectionReadError(ctx, err)
			return
		}
		var high string
		err = s.db.QueryRowContext(ctx, nodesLightHighWaterSQL(indexed), s.viewGen).Scan(&high)
		if err != nil {
			analysisProjectionReadError(ctx, err)
			return
		}
		after, first := "", true
		for ctx.Err() == nil {
			page, err := s.nodesLightPage(ctx, nodesLightPageSQL(indexed, first), s.viewGen, after, high, analysisProjectionPageSize)
			if err != nil {
				analysisProjectionReadError(ctx, err)
				return
			}
			for _, node := range page {
				if ctx.Err() != nil || !yield(node) {
					return
				}
			}
			if len(page) < analysisProjectionPageSize || page[len(page)-1].ID == high {
				return
			}
			after, first = page[len(page)-1].ID, false
		}
	}
}

func nodesLightHighWaterSQL(indexed bool) string {
	return `SELECT id FROM nodes` + analysisProjectionIndexHint(indexed, nodesByGenerationIndexName) + ` WHERE view_gen = ? ORDER BY id DESC LIMIT 1`
}

func nodesLightPageSQL(indexed, first bool) string {
	op := ">"
	if first {
		op = ">=" // retain even an empty identity in a legacy physical payload
	}
	return `SELECT ` + lookupNodeSummaryCols + ` FROM nodes` + analysisProjectionIndexHint(indexed, nodesByGenerationIndexName) +
		` WHERE view_gen = ? AND id ` + op + ` ? AND id <= ? ORDER BY id LIMIT ?`
}

func (s *Store) nodesLightPage(ctx context.Context, query string, args ...any) ([]*graph.Node, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := make([]*graph.Node, 0, analysisProjectionPageSize)
	for rows.Next() {
		node, err := scanNodeSummary(rows)
		if err != nil {
			return nil, err
		}
		page = append(page, node)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return page, rows.Close()
}

// EdgesLightSeq retains its single-statement, kind-indexed snapshot unless
// a revision-validated analysis explicitly binds bounded page reads.
func (s *Store) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	if s.analysisPaged {
		return s.edgesLightSeqPages(kinds...)
	}
	_, args := aggDedupeEdgeKinds(kinds)
	return func(yield func(*graph.Edge) bool) {
		if len(args) == 0 {
			return
		}
		query := `SELECT ` + edgeColsLight + ` FROM edges WHERE kind IN (` +
			inPlaceholders(len(args)) + `) AND view_gen = ?`
		rows, err := s.db.Query(query, append(args, s.viewGen)...)
		if err != nil {
			panicOnFatal(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			edge, scanErr := s.scanEdgeLight(rows)
			if scanErr != nil {
				panicOnFatal(scanErr)
				return
			}
			if edge != nil && !yield(edge) {
				return
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			panicOnFatal(rowsErr)
		}
	}
}

func (s *Store) edgesLightSeqPages(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	_, kindsArgs := aggDedupeEdgeKinds(kinds)
	return func(yield func(*graph.Edge) bool) {
		if len(kindsArgs) == 0 {
			return
		}
		ctx := s.readContext()
		indexed, err := s.analysisProjectionIndexPresent(ctx, edgesByGenerationIndexName)
		if err != nil {
			analysisProjectionReadError(ctx, err)
			return
		}
		var high int64
		err = s.db.QueryRowContext(ctx, edgeGenerationHighWaterSQL(indexed), s.viewGen).Scan(&high)
		if err != nil {
			analysisProjectionReadError(ctx, err)
			return
		}
		var after int64
		query := edgesLightPageSQL(indexed, len(kindsArgs))
		for after < high && ctx.Err() == nil {
			args := []any{s.viewGen, after, high}
			args = append(args, kindsArgs...)
			args = append(args, analysisProjectionPageSize)
			page, scanned, last, err := s.edgesLightPage(ctx, query, args...)
			if err != nil {
				analysisProjectionReadError(ctx, err)
				return
			}
			for _, edge := range page {
				if ctx.Err() != nil || !yield(edge) {
					return
				}
			}
			if scanned < analysisProjectionPageSize {
				return
			}
			after = last
		}
	}
}

func edgesLightPageSQL(indexed bool, kinds int) string {
	return `SELECT id, ` + edgeColsLight + ` FROM edges` + analysisProjectionIndexHint(indexed, edgesByGenerationIndexName) +
		` WHERE view_gen = ? AND id > ? AND id <= ? AND kind IN (` + inPlaceholders(kinds) + `) ORDER BY id LIMIT ?`
}

// The physical id is a keyset cursor, not part of graph.Edge's projection.
type analysisEdgePageScanner struct {
	rows *sql.Rows
	id   int64
}

func (r *analysisEdgePageScanner) Scan(dest ...any) error {
	return r.rows.Scan(append([]any{&r.id}, dest...)...)
}

func (s *Store) edgesLightPage(ctx context.Context, query string, args ...any) ([]*graph.Edge, int, int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	page := make([]*graph.Edge, 0, analysisProjectionPageSize)
	scanner := analysisEdgePageScanner{rows: rows}
	scanned := 0
	for rows.Next() {
		edge, err := s.scanEdgeLight(&scanner)
		if err != nil {
			return nil, 0, 0, err
		}
		scanned++
		if edge != nil {
			page = append(page, edge)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	return page, scanned, scanner.id, rows.Close()
}

func analysisProjectionIndexHint(indexed bool, name string) string {
	if indexed {
		return " INDEXED BY " + name
	}
	return ""
}

func (s *Store) analysisProjectionIndexPresent(ctx context.Context, name string) (bool, error) {
	var present bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?)`, name).Scan(&present)
	return present, err
}

func analysisProjectionReadError(ctx context.Context, err error) {
	if ctx.Err() == nil {
		panicOnFatal(err)
	}
}

// NodesByKindsSeq streams full node rows for a compact fixed kind set. Process
// scoring needs three Meta keys, but only function/method rows pay that decode;
// the rest of the node corpus is never scanned or materialized.
func (s *Store) NodesByKindsSeq(kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	_, args := aggDedupeNodeKinds(kinds)
	return func(yield func(*graph.Node) bool) {
		if len(args) == 0 {
			return
		}
		query := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE kind IN (` +
			inPlaceholders(len(args)) + `) AND view_gen = ?`
		rows, err := s.db.Query(query, append(args, s.viewGen)...)
		if err != nil {
			panicOnFatal(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			node, scanErr := scanNodeCursor(rows)
			if scanErr != nil {
				panicOnFatal(scanErr)
				return
			}
			if node != nil && !yield(node) {
				return
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			panicOnFatal(rowsErr)
		}
	}
}

// NodeIDNamesByKindsSeq keeps symbol-name indexing disk-resident and projects
// only the two consumed columns. Empty repoPrefix is the global view used by
// cross-repository content linking; a non-empty prefix is pushed into SQL.
func (s *Store) NodeIDNamesByKindsSeq(repoPrefix string, kinds ...graph.NodeKind) iter.Seq[graph.NodeIDName] {
	_, kindArgs := aggDedupeNodeKinds(kinds)
	return func(yield func(graph.NodeIDName) bool) {
		if len(kindArgs) == 0 {
			return
		}
		query := `SELECT id, name FROM nodes WHERE kind IN (` + inPlaceholders(len(kindArgs)) + `)`
		args := append([]any(nil), kindArgs...)
		if repoPrefix != "" {
			query += ` AND repo_prefix = ?`
			args = append(args, repoPrefix)
		}
		query += ` AND view_gen = ? ORDER BY name, id`
		args = append(args, s.viewGen)
		rows, err := s.db.Query(query, args...)
		if err != nil {
			panicOnFatal(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var row graph.NodeIDName
			if err := rows.Scan(&row.ID, &row.Name); err != nil {
				panicOnFatal(err)
				return
			}
			if !yield(row) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			panicOnFatal(err)
		}
	}
}

var (
	_ graph.NodeLightSequencer          = (*Store)(nil)
	_ graph.LightEdgeSequencer          = (*Store)(nil)
	_ graph.NodesByKindsSequencer       = (*Store)(nil)
	_ graph.NodeIDNamesByKindsSequencer = (*Store)(nil)
)
