package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

const unresolvedEdgePredicate = `is_unresolved = 1
  AND NOT (to_id >= 'unresolved::fnvalue::' AND to_id < 'unresolved::fnvalue:;')
  AND to_id NOT LIKE '%::unresolved::fnvalue::%'`

// The unresolved frontier lives in one table for every payload generation, and
// nothing the base corpus's own indexes key on distinguishes them: a scan that
// filters `view_gen = ?` after the fact still fetches and discards every
// unresolved row the corpus holds. That is the whole cost of a sparse
// generation's resolve pass — its payload is one change's closure, and it
// reached those few rows through the workspace's entire backlog.
//
// edges_by_generation is keyed (view_gen, id) over exactly the derived rows, so
// a pinned handle seeks straight into its own range. The base corpus is not in
// that partial index and keeps the source it was tuned for, which is what
// leaves generation 0's scope exactly as wide as it has always been.
const (
	// unresolvedHighWaterBaseSource is the base corpus's frontier walk: the
	// partial is_unresolved index, which for generation 0 is the whole
	// frontier and nothing else.
	unresolvedHighWaterBaseSource = `edges INDEXED BY edges_by_unresolved`
	// unresolvedPageBaseSource leaves the base corpus's page statement on the
	// planner's own choice, which is the rowid range it already used.
	unresolvedPageBaseSource = `edges`
	// unresolvedIdentityBaseSource keeps the attribution pass's base scan on
	// the raw rowid order: it walks the corpus end to end, and an index would
	// only add a hop per row.
	unresolvedIdentityBaseSource = `edges NOT INDEXED`
	// unresolvedFrontierBaseSource matches unresolvedHighWaterBaseSource; the
	// census aggregates the same frontier.
	unresolvedFrontierBaseSource = unresolvedHighWaterBaseSource
)

// unresolvedScanSource returns the FROM clause and the leading generation
// predicate one enumeration must use on this handle, given the source the base
// corpus's own scan is tuned for.
//
// The redundant-looking `view_gen > 0` is load-bearing: SQLite uses a partial
// index only when the query's WHERE clause implies the index's, and a bound
// parameter proves nothing at planning time.
func (s *Store) unresolvedScanSource(baseSource string) (source, generation string) {
	if s.viewGen > baseViewGeneration {
		return `edges INDEXED BY ` + edgesByGenerationIndexName, `view_gen > 0 AND view_gen = ?`
	}
	return baseSource, `view_gen = ?`
}

// unresolvedWaterMarkSQL walks the handle's own frontier to one end. direction
// is `DESC` for the high-water mark and `ASC` for the low.
func unresolvedWaterMarkSQL(source, generation, direction string) string {
	return `SELECT id
FROM ` + source + `
WHERE ` + generation + `
  AND ` + unresolvedEdgePredicate + `
ORDER BY id ` + direction + `
LIMIT 1`
}

// unresolvedEdgePageSQL is one bounded keyset page. extra carries the optional
// terminal-skip and scope predicates, each already prefixed with ` AND `; its
// parameters bind after the rowid bounds and before the row limit.
func unresolvedEdgePageSQL(source, generation, extra string) string {
	return `SELECT id, ` + lookupEdgeCols + `
FROM ` + source + `
WHERE ` + generation + ` AND id > ? AND id <= ? AND ` + unresolvedEdgePredicate + extra + `
ORDER BY id
LIMIT ?`
}

var _ graph.UnresolvedEdgePager = (*Store)(nil)

// BeginUnresolvedEdgeScan captures a stable rowid window. Reindexing an edge
// may delete and insert its row; the replacement receives a larger id and
// therefore cannot be visited twice by the same resolver pass.
//
// A derived generation also captures the LOW end, so its pass is bounded below
// by its own first row rather than by the base corpus's. Generation 0 owns the
// whole frontier and leaves it at 0.
//
// PendingBefore is deliberately unknown for a non-empty frontier. Computing
// it with COUNT(*) made every warm scoped resolve walk the entire unresolved
// partial index before the first scoped page could run. The descending id
// probe stops after one indexed entry; the resolver derives the exact
// diagnostic count while consuming pages.
func (s *Store) BeginUnresolvedEdgeScan(ctx context.Context) (graph.UnresolvedEdgeScan, error) {
	scan := graph.UnresolvedEdgeScan{PendingBefore: -1}
	source, generation := s.unresolvedScanSource(unresolvedHighWaterBaseSource)
	err := s.db.QueryRowContext(ctx,
		unresolvedWaterMarkSQL(source, generation, `DESC`), s.viewGen).Scan(&scan.HighWaterID)
	if errors.Is(err, sql.ErrNoRows) {
		scan.PendingBefore = 0
		return scan, nil
	}
	if err != nil || s.viewGen == baseViewGeneration {
		return scan, err
	}
	err = s.db.QueryRowContext(ctx,
		unresolvedWaterMarkSQL(source, generation, `ASC`), s.viewGen).Scan(&scan.LowWaterID)
	if errors.Is(err, sql.ErrNoRows) {
		return scan, nil
	}
	return scan, err
}

// ReadUnresolvedEdgePage returns one row- and byte-bounded keyset page. The
// byte bound is measured from the encoded row plus scalar/string fields before
// Meta is decoded; one individually oversized row is admitted to guarantee
// cursor progress.
func (s *Store) ReadUnresolvedEdgePage(ctx context.Context, scan graph.UnresolvedEdgeScan, afterID int64, maxRows, maxBytes int) (graph.UnresolvedEdgePage, error) {
	if maxRows <= 0 {
		maxRows = 2048
	}
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	page := graph.UnresolvedEdgePage{NextID: afterID}
	if afterID >= scan.HighWaterID || scan.HighWaterID == 0 {
		page.Exhausted = true
		return page, nil
	}
	// The cursor the caller keeps stays its own; only the row the statement
	// starts after is lifted to the scan's floor, so a pass that begins at 0
	// never looks below the generation it is pinned to.
	lowerBound := afterID
	if floor := scan.LowWaterID - 1; floor > lowerBound {
		lowerBound = floor
	}

	source, generation := s.unresolvedScanSource(unresolvedPageBaseSource)
	extra := ""
	args := []any{s.viewGen, lowerBound, scan.HighWaterID}
	anchorIn := func(column string) (string, []any) {
		if len(scan.ScopeAnchors) == 0 {
			return "", nil
		}
		clause := column + ` IN (`
		anchorArgs := make([]any, 0, len(scan.ScopeAnchors))
		for i, anchor := range scan.ScopeAnchors {
			if i > 0 {
				clause += `,`
			}
			clause += `?`
			anchorArgs = append(anchorArgs, anchor)
		}
		return clause + `)`, anchorArgs
	}
	if scan.SkipTerminal {
		// The durable stamp lives in the promoted resolve_terminal column,
		// so a scoped pass's terminal skip runs at the store instead of
		// loading + decoding the row first. NULL (never-stamped /
		// pre-promotion) always passes. A stamped row anchored to a scope
		// repo in either endpoint stays included, tested on the generated
		// from_repo / to_repo_unresolved columns (SQL mirrors of the Go
		// prefix helpers, parity-asserted); stub-qualified targets read as
		// NULL there and fail open to the exact in-memory rule.
		cond := `(resolve_terminal IS NOT 1 OR to_repo_unresolved IS NULL`
		if fromIn, fromArgs := anchorIn(`from_repo`); fromIn != "" {
			cond += ` OR ` + fromIn
			args = append(args, fromArgs...)
		}
		if toIn, toArgs := anchorIn(`to_repo_unresolved`); toIn != "" {
			cond += ` OR ` + toIn
			args = append(args, toArgs...)
		}
		cond += `)`
		extra += ` AND ` + cond
	}
	if scan.ScopeFilter && len(scan.ScopeAnchors) > 0 {
		// Scoped-pass pushdown of edgeInResolveScope: keep when the source
		// repo is in scope, the target is bare (''), the target's unresolved
		// repo qualifier is in scope, or the shape is one only Go can parse
		// (NULL — fail open). Rows dropped here are exactly the rows the
		// consumer's filterPendingByScope would drop; the in-memory filter
		// still runs and remains the authority.
		cond := `(to_repo_unresolved IS NULL OR to_repo_unresolved = ''`
		if fromIn, fromArgs := anchorIn(`from_repo`); fromIn != "" {
			cond += ` OR ` + fromIn
			args = append(args, fromArgs...)
		}
		if toIn, toArgs := anchorIn(`to_repo_unresolved`); toIn != "" {
			cond += ` OR ` + toIn
			args = append(args, toArgs...)
		}
		cond += `)`
		extra += ` AND ` + cond
	}
	args = append(args, maxRows)
	rows, err := s.db.QueryContext(ctx, unresolvedEdgePageSQL(source, generation, extra), args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()

	bytesUsed := 0
	rowsRead := 0
	byteStopped := false
	for rows.Next() {
		id, edge, encodedBytes, scanErr := scanUnresolvedEdge(rows)
		if scanErr != nil {
			return page, scanErr
		}
		rowsRead++
		page.NextID = id
		page.Edges = append(page.Edges, edge)
		bytesUsed += encodedBytes
		if bytesUsed >= maxBytes {
			byteStopped = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	page.Exhausted = page.NextID >= scan.HighWaterID || (!byteStopped && rowsRead < maxRows)
	return page, nil
}

func scanUnresolvedEdge(scanner interface{ Scan(...any) error }) (int64, *graph.Edge, int, error) {
	var (
		id        int64
		edge      graph.Edge
		metaBlob  sql.RawBytes
		crossRepo int64
		promoted  promotedEdgeMeta
	)
	if err := scanner.Scan(
		&id, &edge.From, &edge.To, &edge.Kind, &edge.FilePath, &edge.Line,
		&edge.Confidence, &edge.ConfidenceLabel, &edge.Origin, &edge.Tier,
		&crossRepo, &metaBlob, &promoted.resolveTerminal,
		&promoted.resolveTerminalReason, &promoted.semanticSource,
	); err != nil {
		return 0, nil, 0, err
	}
	edge.CrossRepo = crossRepo != 0
	if len(metaBlob) > 0 {
		meta, err := decodeMeta(metaBlob)
		if err != nil {
			return 0, nil, 0, fmt.Errorf("decode unresolved edge %d meta: %w", id, err)
		}
		edge.Meta = meta
	}
	restorePromotedEdgeMeta(&edge, promoted)
	encodedBytes := 192 + len(edge.From) + len(edge.To) + len(edge.Kind) +
		len(edge.FilePath) + len(edge.ConfidenceLabel) + len(edge.Origin) +
		len(edge.Tier) + len(metaBlob)
	return id, &edge, encodedBytes, nil
}
