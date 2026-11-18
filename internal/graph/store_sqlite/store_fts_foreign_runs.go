package store_sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

// symbolFTSHoleShare and symbolFTSRangesMinShare decide when a generation that
// is not one narrow rowid run is ranked range by range instead of by one
// unbounded MATCH.
//
// The unbounded MATCH makes FTS5 score every matching document of every
// generation before the ownership join drops the others' (see
// symbolFTSSpanFraction). The base corpus is the usual such generation, and
// the other generations' documents are not scattered through it: a derived
// generation is written by one build, so its documents form a run above the
// base's, and an incremental base write that adds a document allocates it
// above every run. The generation's own documents therefore lie in its rowid
// span minus the stretches of those runs it owns nothing in (the runs are read
// from the ownership sidecar in the ranking snapshot, so a document written
// into a run is never cut out). Each remaining range is ranked by its own
// bounded query and the ranked streams are merged on (score, rowid), which is
// exactly the unbounded rank stream: bm25 reads the whole table's statistics
// whatever the bound, and FTS5's rank cursor orders equal scores by ascending
// rowid.
//
// Every bounded query that finds a match pays FTS5's fixed cost again: each
// prefix term's doclists are merged, and each phrase's hits counted for bm25,
// over the whole table whatever the bound. Measured on a 163k-document table
// (an 80k-document base below an 82k-document derived copy), one more such
// query cost about a quarter of ranking the whole table. So a run is cut out
// of the span (adding one query) only when it covers at least
// 1/symbolFTSHoleShare of the table's rowids: above that quarter, with margin
// for rowids that are deletion gaps rather than documents. At most two runs
// can qualify, so a search is at most three queries. The plan also has to
// leave out at least 1/symbolFTSRangesMinShare of the table's rowids in all,
// or the saving does not repay the run discovery and the read transaction.
const (
	symbolFTSHoleShare      = 3
	symbolFTSRangesMinShare = 16
)

// symbolFTSRunGenerations bounds the generations the run discovery reads; a
// table holding more keeps the unbounded plan.
const symbolFTSRunGenerations = 64

// symbolFTSRunsSQL reads, with index seeks only, every generation's rowid run
// (its first and last rowid) and, for each run, the searched generation ?1's
// first rowid at or after the run's start and last rowid at or before its end:
// the searched generation owns nothing inside a run when the first is absent
// or past the run's end. The distinct generations are walked by seeking past
// the previous one on symbol_fts_rowid_by_generation (view_gen, fts_rowid),
// which also serves every extremum, so the cost is a few seeks per generation
// whatever the table's size.
const symbolFTSRunsSQL = `
WITH RECURSIVE gens(g) AS (
  SELECT (SELECT view_gen FROM symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_generation
          ORDER BY view_gen LIMIT 1)
  UNION ALL
  SELECT (SELECT view_gen FROM symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_generation
          WHERE view_gen > gens.g ORDER BY view_gen LIMIT 1)
  FROM gens WHERE gens.g IS NOT NULL
  LIMIT ?2
),
runs(g, lo, hi) AS MATERIALIZED (
  SELECT g,
    (SELECT fts_rowid FROM symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_generation
     WHERE view_gen = gens.g ORDER BY fts_rowid LIMIT 1),
    (SELECT fts_rowid FROM symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_generation
     WHERE view_gen = gens.g ORDER BY fts_rowid DESC LIMIT 1)
  FROM gens WHERE gens.g IS NOT NULL
)
SELECT g, lo, hi,
  (SELECT fts_rowid FROM symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_generation
   WHERE view_gen = ?1 AND fts_rowid >= runs.lo ORDER BY fts_rowid LIMIT 1),
  (SELECT fts_rowid FROM symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_generation
   WHERE view_gen = ?1 AND fts_rowid <= runs.hi ORDER BY fts_rowid DESC LIMIT 1)
FROM runs`

// symbolFTSOtherGenerationSQL asks, with two seeks on
// symbol_fts_rowid_by_generation, whether any generation other than ?1 owns a
// document. Only other generations' runs are ever cut out of a span, so
// without one the range plan has nothing to skip.
const symbolFTSOtherGenerationSQL = `
SELECT EXISTS (SELECT 1 FROM symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_generation WHERE view_gen < ?1)
    OR EXISTS (SELECT 1 FROM symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_generation WHERE view_gen > ?1)`

// symbolFTSRowidRange is an inclusive rowid range.
type symbolFTSRowidRange struct{ lo, hi int64 }

// symbolFTSGenerationRanges returns the rowid ranges that hold all of
// generation's documents once every qualifying run it owns nothing in is cut
// out of its span, read in tx. ok=false means the ranges would not pay (see
// symbolFTSHoleShare) or the table holds too many generations to read; a
// generation without documents is ok with no ranges. A range may still hold
// other generations' documents: the ownership join excludes them exactly as
// the unbounded plan does, so the ranges only decide how much FTS5 reads.
func symbolFTSGenerationRanges(ctx context.Context, tx *sql.Tx, generation int64) ([]symbolFTSRowidRange, bool, error) {
	rows, err := tx.QueryContext(ctx, symbolFTSRunsSQL, generation, symbolFTSRunGenerations+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var (
		span     symbolFTSRowidRange
		present  bool
		tableMax int64
		read     int
		holes    []symbolFTSRowidRange
	)
	for rows.Next() {
		var (
			g                 int64
			lo, hi            sql.NullInt64
			ownFirst, ownLast sql.NullInt64
		)
		if err := rows.Scan(&g, &lo, &hi, &ownFirst, &ownLast); err != nil {
			return nil, false, err
		}
		read++
		if !lo.Valid || !hi.Valid {
			continue
		}
		tableMax = max(tableMax, hi.Int64)
		if g == generation {
			span, present = symbolFTSRowidRange{lo.Int64, hi.Int64}, true
			continue
		}
		// The parts of the run the searched generation owns nothing in: all
		// of it, or the stretches before its first and after its last
		// document inside the run.
		if !ownFirst.Valid || ownFirst.Int64 > hi.Int64 {
			holes = append(holes, symbolFTSRowidRange{lo.Int64, hi.Int64})
			continue
		}
		if ownFirst.Int64 > lo.Int64 {
			holes = append(holes, symbolFTSRowidRange{lo.Int64, ownFirst.Int64 - 1})
		}
		if ownLast.Valid && ownLast.Int64 < hi.Int64 {
			holes = append(holes, symbolFTSRowidRange{ownLast.Int64 + 1, hi.Int64})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if read > symbolFTSRunGenerations {
		return nil, false, ctx.Err()
	}
	if !present {
		return nil, true, ctx.Err()
	}

	// Holes are free of the searched generation, so neither end of its span
	// lies in one; clipped to the span and merged, each worthwhile one splits
	// a range in two.
	cut := holes[:0]
	for _, hole := range holes {
		hole.lo, hole.hi = max(hole.lo, span.lo), min(hole.hi, span.hi)
		if hole.lo <= hole.hi {
			cut = append(cut, hole)
		}
	}
	slices.SortFunc(cut, func(a, b symbolFTSRowidRange) int { return cmp.Compare(a.lo, b.lo) })
	var merged []symbolFTSRowidRange
	for _, hole := range cut {
		if n := len(merged); n > 0 && hole.lo <= merged[n-1].hi+1 {
			merged[n-1].hi = max(merged[n-1].hi, hole.hi)
			continue
		}
		merged = append(merged, hole)
	}
	ranges := make([]symbolFTSRowidRange, 0, 3)
	next := span.lo
	for _, hole := range merged {
		if (hole.hi-hole.lo+1)*symbolFTSHoleShare < tableMax {
			continue
		}
		ranges = append(ranges, symbolFTSRowidRange{next, hole.lo - 1})
		next = hole.hi + 1
	}
	ranges = append(ranges, symbolFTSRowidRange{next, span.hi})
	var covered int64
	for _, r := range ranges {
		covered += r.hi - r.lo + 1
	}
	if (tableMax-covered)*symbolFTSRangesMinShare < tableMax {
		return nil, false, ctx.Err()
	}
	return ranges, true, ctx.Err()
}

// symbolFTSRangeQueryObserver, when set by a test, is told every generation
// the range plan ranks and the ranges it ranks it in. nil in production.
var symbolFTSRangeQueryObserver func(generation int64, ranges []symbolFTSRowidRange)

// searchSymbolFTSRangePlan returns the page the unbounded single-generation
// query returns — the same rows in the same order, its SQL LIMIT counting
// empty ids too — by ranking each of the generation's rowid ranges on its own
// (see symbolFTSHoleShare). handled=false means the plan does not apply and
// the caller runs the unbounded query.
//
// The runs and every ranked range are read in one snapshot: base rows are
// mutable, and bounds read outside the ranking transaction could omit a
// document written in between.
func (s *Store) searchSymbolFTSRangePlan(ctx context.Context, match string, generation int64, repoAllow []string, limit int) ([]graph.SymbolHit, bool, error) {
	// A table that holds no other generation's document has no run to cut
	// out, so a single-generation store keeps the unbounded plan without
	// opening the ranking snapshot. The check reads outside that snapshot: a
	// generation written after it only sends this search down the unbounded
	// query, which returns the same page.
	var others bool
	if err := s.db.QueryRowContext(ctx, symbolFTSOtherGenerationSQL, generation).Scan(&others); err != nil {
		return nil, true, err
	}
	if !others {
		return nil, false, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = tx.Rollback() }()
	ranges, ok, err := symbolFTSGenerationRanges(ctx, tx, generation)
	if err != nil {
		return nil, true, err
	}
	if !ok {
		return nil, false, nil
	}
	if len(ranges) == 0 {
		return nil, true, nil
	}
	if observe := symbolFTSRangeQueryObserver; observe != nil {
		observe(generation, ranges)
	}

	type rankedRow struct {
		id    string
		score float64
		rowid int64
	}
	// Each range's first limit rows hold every row of the merged first limit.
	var ranked []rankedRow
	query := symbolFTSBoundedQuery(`symbol_fts.node_id, bm25(symbol_fts), symbol_fts.rowid`, len(repoAllow), true)
	for _, r := range ranges {
		args := make([]any, 0, 5+len(repoAllow))
		args = append(args, match, r.lo, r.hi, generation)
		for _, repo := range repoAllow {
			args = append(args, repo)
		}
		args = append(args, limit)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, true, err
		}
		for rows.Next() {
			var row rankedRow
			if err := rows.Scan(&row.id, &row.score, &row.rowid); err != nil {
				_ = rows.Close()
				return nil, true, err
			}
			ranked = append(ranked, row)
		}
		err = rows.Err()
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, true, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	slices.SortFunc(ranked, func(a, b rankedRow) int {
		if c := cmp.Compare(a.score, b.score); c != 0 {
			return c
		}
		return cmp.Compare(a.rowid, b.rowid)
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	var hits []graph.SymbolHit
	for _, row := range ranked {
		if row.id != "" {
			hits = append(hits, graph.SymbolHit{NodeID: row.id, Score: -row.score})
		}
	}
	return hits, true, nil
}
