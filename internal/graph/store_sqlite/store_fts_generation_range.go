package store_sqlite

import (
	"context"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// symbolFTSSpanFraction and symbolFTSSpanFloor decide when a generation's
// rowid run is narrow enough to bound its search with: a run no wider than
// symbolFTSSpanFloor rowids, or no wider than 1/symbolFTSSpanFraction of the
// whole table's rowid range.
//
// symbol_fts is one table shared by every generation, and FTS5 ranks a MATCH
// by scoring every matching document of the whole table before it returns the
// first row, whatever the outer join later keeps. A derived generation's
// documents are written by one build, so their rowids form one narrow run (on
// the live store every derived generation's run holds exactly as many rowids
// as documents); bounding the MATCH with that run makes FTS5 seek every
// doclist to the run's first rowid and stop at its last, so the cost is the
// generation's own matches instead of every generation's. The base corpus is
// re-indexed in place, its rowids scatter over the whole table, and it keeps
// the unbounded plan. The bound never decides membership (the sidecar join
// does), so a run that happens to be wide only costs speed, never rows.
const (
	symbolFTSSpanFraction = 4
	symbolFTSSpanFloor    = 4096
)

// symbolFTSSpan is one generation's rowid run in symbol_fts.
type symbolFTSSpan struct {
	generation int64
	present    bool // the generation owns at least one document
	lo, hi     int64
	tableMax   int64
}

// empty reports a generation that carries no symbol documents at all: every
// search of it answers nothing, and no FTS query needs to run.
func (s symbolFTSSpan) empty() bool { return !s.present }

// dense reports whether the generation's rowid run is narrow enough to bound
// the MATCH with it.
func (s symbolFTSSpan) dense() bool {
	if !s.present || s.hi < s.lo {
		return false
	}
	width := s.hi - s.lo + 1
	return width <= symbolFTSSpanFloor || width*symbolFTSSpanFraction <= s.tableMax
}

// symbolFTSSpanSQL finds one generation's rowid run with index seeks only:
// it walks the generation's distinct repo prefixes in
// symbol_fts_rowid_by_repo (view_gen, repo_prefix, fts_rowid) and takes the
// first and last rowid under each, so its cost is a few seeks per repo prefix
// whatever the generation's size. A COUNT or MIN/MAX over view_gen alone would
// scan every entry of the generation (0.46 s for 80k documents on the live
// store). The table's largest rowid is one seek on symbol_fts_rowid_by_rowid.
const symbolFTSSpanSQL = `
WITH RECURSIVE prefixes(repo) AS (
  SELECT (SELECT repo_prefix FROM symbol_fts_rowid WHERE view_gen = ?1 ORDER BY repo_prefix LIMIT 1)
  UNION ALL
  SELECT (SELECT repo_prefix FROM symbol_fts_rowid
          WHERE view_gen = ?1 AND repo_prefix > prefixes.repo ORDER BY repo_prefix LIMIT 1)
  FROM prefixes WHERE prefixes.repo IS NOT NULL
)
SELECT
  (SELECT MIN(lo) FROM (SELECT (SELECT fts_rowid FROM symbol_fts_rowid
          WHERE view_gen = ?1 AND repo_prefix = prefixes.repo ORDER BY fts_rowid LIMIT 1) AS lo
        FROM prefixes WHERE prefixes.repo IS NOT NULL)),
  (SELECT MAX(hi) FROM (SELECT (SELECT fts_rowid FROM symbol_fts_rowid
          WHERE view_gen = ?1 AND repo_prefix = prefixes.repo ORDER BY fts_rowid DESC LIMIT 1) AS hi
        FROM prefixes WHERE prefixes.repo IS NOT NULL)),
  (SELECT MAX(fts_rowid) FROM symbol_fts_rowid)`

// symbolFTSGenerationSpan reads one derived generation's rowid run from the
// ownership sidecar. The base generation is never bounded and is not measured.
func (s *Store) symbolFTSGenerationSpan(ctx context.Context, generation int64) (symbolFTSSpan, bool, error) {
	span := symbolFTSSpan{generation: generation}
	if generation <= baseViewGeneration {
		return span, false, nil
	}
	var lo, hi, tableMax *int64
	if err := s.db.QueryRowContext(ctx, symbolFTSSpanSQL, generation).Scan(&lo, &hi, &tableMax); err != nil {
		return span, false, err
	}
	if lo != nil && hi != nil {
		span.present = true
		span.lo, span.hi = *lo, *hi
	}
	if tableMax != nil {
		span.tableMax = *tableMax
	}
	return span, true, nil
}

// symbolFTSSpanQuery ranks one generation's documents inside its rowid run.
// The sidecar join still decides membership, so a foreign row that happens to
// sit inside the run is excluded exactly as the unbounded plan excludes it;
// the run only decides how much of the shared index FTS5 reads.
//
// withLimit appends the SQL LIMIT the single-generation search has always
// used (it counts every row); the batch reads without it and stops after limit
// non-empty ids, which is how the shared stream counts.
func symbolFTSSpanQuery(repos int, withLimit bool) string {
	q := `SELECT symbol_fts.node_id, bm25(symbol_fts)
FROM symbol_fts
CROSS JOIN symbol_fts_rowid
WHERE symbol_fts MATCH ?
  AND symbol_fts.rowid BETWEEN ? AND ?
  AND symbol_fts_rowid.fts_rowid = symbol_fts.rowid
  AND symbol_fts_rowid.view_gen = ?`
	if repos > 0 {
		q += ` AND symbol_fts.repo_prefix IN ('', ?` + strings.Repeat(`,?`, repos-1) + `)`
	}
	q += ` AND symbol_fts.rank MATCH 'bm25()' ORDER BY symbol_fts.rank`
	if withLimit {
		q += ` LIMIT ?`
	}
	return q
}

// searchSymbolFTSSpan returns one generation's ranked page, the same rows in
// the same order the unbounded rank stream yields for that generation.
//
// A published generation's ranked rows for one MATCH never change (the
// payload is sealed, and bm25's corpus statistics are the shared table's, so
// a new generation can only change scores — see symbolFTSPageCache), so the
// head of the ranking is memoized per generation and a later page — a refill
// of the same request, or the same query from the next request — is cut from
// it without a query.
func (s *Store) searchSymbolFTSSpan(
	ctx context.Context,
	match string,
	span symbolFTSSpan,
	repoAllow []string,
	limit int,
	sqlLimit bool,
) ([]graph.SymbolHit, error) {
	if cache := s.symbolFTSPageCacheFor(span.generation); cache != nil {
		key := symbolFTSPageKey(match, repoAllow, s.symbolFTSCorpusStamp())
		if page, ok := cache.page(key, limit, sqlLimit); ok {
			return page, ctx.Err()
		}
		rows, complete, err := s.querySymbolFTSSpanRows(ctx, match, span, repoAllow, symbolFTSPageCacheRows)
		if err != nil {
			return nil, err
		}
		cache.store(key, rows, complete)
		if page, ok := cache.cut(rows, complete, limit, sqlLimit); ok {
			return page, ctx.Err()
		}
	}
	args := make([]any, 0, 5+len(repoAllow))
	args = append(args, match, span.lo, span.hi, span.generation)
	for _, repo := range repoAllow {
		args = append(args, repo)
	}
	if sqlLimit {
		args = append(args, limit)
	}
	if observe := symbolFTSSpanQueryObserver; observe != nil {
		observe(span.generation)
	}
	rows, err := s.db.QueryContext(ctx, symbolFTSSpanQuery(len(repoAllow), sqlLimit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []graph.SymbolHit
	for rows.Next() {
		var (
			id    string
			score float64
		)
		if err := rows.Scan(&id, &score); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		hits = append(hits, graph.SymbolHit{NodeID: id, Score: -score})
		if !sqlLimit && len(hits) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, ctx.Err()
}

// querySymbolFTSSpanRows reads the first max ranked rows of one generation's
// run, empty ids included (they count toward a SQL LIMIT). complete reports
// that the ranking had no more rows.
func (s *Store) querySymbolFTSSpanRows(
	ctx context.Context,
	match string,
	span symbolFTSSpan,
	repoAllow []string,
	max int,
) ([]symbolFTSRow, bool, error) {
	args := make([]any, 0, 5+len(repoAllow))
	args = append(args, match, span.lo, span.hi, span.generation)
	for _, repo := range repoAllow {
		args = append(args, repo)
	}
	args = append(args, max+1)
	if observe := symbolFTSSpanQueryObserver; observe != nil {
		observe(span.generation)
	}
	rows, err := s.db.QueryContext(ctx, symbolFTSSpanQuery(len(repoAllow), true), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []symbolFTSRow
	for rows.Next() {
		var row symbolFTSRow
		if err := rows.Scan(&row.id, &row.score); err != nil {
			return nil, false, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > max {
		return out[:max], false, nil
	}
	return out, true, nil
}

// symbolFTSSpanQueryObserver, when set by a test, is told every bounded FTS
// query a generation runs. nil in production.
var symbolFTSSpanQueryObserver func(generation int64)

// symbolFTSRow is one ranked row of a generation's run.
type symbolFTSRow struct {
	id    string
	score float64
}

// symbolFTSPageCacheRows is how many ranked rows one memoized query keeps: far
// more than any first page or refill a composed-view search asks for, and small
// enough that a generation's whole memo stays a few megabytes.
const symbolFTSPageCacheRows = 1024

// symbolFTSPageCacheEntries bounds the memoized queries per generation.
const symbolFTSPageCacheEntries = 32

// symbolFTSPageCache memoizes the head of one published generation's ranking
// per (MATCH, repo scope, corpus stamp). It hangs off the generation's shared
// payloadSeal, so every handle shares it and it is dropped at retirement.
//
// bm25 reads corpus-wide statistics (row count, average length, per-term
// document frequency) of the ONE symbol_fts table every generation shares, so
// any FTS write anywhere changes scores and can reorder a generation's rows.
// The corpus stamp (symbolFTSCorpusStamp) is part of the key for exactly that
// reason: a memo is only ever served against the table state it was ranked on.
type symbolFTSPageCache struct {
	mu      sync.Mutex
	entries map[string]*symbolFTSPageEntry
	order   []string // insertion order, oldest first
}

type symbolFTSPageEntry struct {
	rows     []symbolFTSRow
	complete bool
}

func symbolFTSPageKey(match string, repoAllow []string, stamp string) string {
	return stamp + "\x00" + match + "\x00" + strings.Join(repoAllow, "\x01")
}

func (c *symbolFTSPageCache) page(key string, limit int, sqlLimit bool) ([]graph.SymbolHit, bool) {
	c.mu.Lock()
	entry := c.entries[key]
	c.mu.Unlock()
	if entry == nil {
		return nil, false
	}
	return c.cut(entry.rows, entry.complete, limit, sqlLimit)
}

// cut takes a page off memoized rows with the same counting rule the query
// path uses: a SQL LIMIT counts every row, the batch counts non-empty ids.
// ok=false means the memo does not reach far enough to decide the page.
func (c *symbolFTSPageCache) cut(rows []symbolFTSRow, complete bool, limit int, sqlLimit bool) ([]graph.SymbolHit, bool) {
	var hits []graph.SymbolHit
	if sqlLimit {
		if limit > len(rows) && !complete {
			return nil, false
		}
		for i := 0; i < len(rows) && i < limit; i++ {
			if rows[i].id != "" {
				hits = append(hits, graph.SymbolHit{NodeID: rows[i].id, Score: -rows[i].score})
			}
		}
		return hits, true
	}
	for _, row := range rows {
		if row.id == "" {
			continue
		}
		hits = append(hits, graph.SymbolHit{NodeID: row.id, Score: -row.score})
		if len(hits) >= limit {
			return hits, true
		}
	}
	if !complete {
		return nil, false
	}
	return hits, true
}

func (c *symbolFTSPageCache) store(key string, rows []symbolFTSRow, complete bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*symbolFTSPageEntry)
	}
	if _, exists := c.entries[key]; !exists {
		c.order = append(c.order, key)
	}
	c.entries[key] = &symbolFTSPageEntry{rows: rows, complete: complete}
	for len(c.order) > symbolFTSPageCacheEntries {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
}

// symbolFTSPageCacheFor returns the page memo of generation when it is a
// published derived generation, nil otherwise.
func (s *Store) symbolFTSPageCacheFor(generation int64) *symbolFTSPageCache {
	seal, ok := s.sealedGeneration(generation)
	if !ok {
		return nil
	}
	return &seal.ftsPages
}

// symbolFTSCorpusStamp names the current state of the shared symbol_fts table:
// its averages record (row count and total token count) and its structure
// record, whose leading cookie FTS5 increments on every change to the index's
// segment structure — which every write transaction to the table makes. An
// unreadable stamp is a fresh random-free sentinel that never matches a stored
// key, so the memo is simply bypassed.
func (s *Store) symbolFTSCorpusStamp() string {
	rows, err := s.db.Query(`SELECT id, block FROM symbol_fts_data WHERE id IN (1, 10) ORDER BY id`)
	if err != nil {
		return "unstamped:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	defer rows.Close()
	h := fnv.New64a()
	found := 0
	for rows.Next() {
		var (
			id    int64
			block []byte
		)
		if err := rows.Scan(&id, &block); err != nil {
			return "unstamped:" + strconv.FormatInt(time.Now().UnixNano(), 10)
		}
		_, _ = h.Write([]byte(strconv.FormatInt(id, 10)))
		_, _ = h.Write(block)
		found++
	}
	if rows.Err() != nil || found == 0 {
		return "unstamped:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}
