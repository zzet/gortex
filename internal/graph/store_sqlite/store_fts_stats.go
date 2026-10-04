package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Symbol FTS statistics and rows for a caller that ranks on its own side.
//
// FTS5's bm25 reads the corpus statistics of the ONE symbol_fts table every
// generation shares, so every publication changes every generation's scores
// and the corpus stamp that keys the per-generation page memos: the first
// search after a publication re-ranks a fresh generation over the whole table
// (59 % of that search on the live daemon). These calls give the ranking inputs
// without bm25 — the statistics, per-prefix document counts, and one
// generation's matching rows with their token counts — plus a stamp scoped to
// one generation, so a caller can rank a sealed lower generation once against
// statistics it chooses and keep that ranking when a new top generation is
// published.

// SymbolFTSStats are the corpus statistics bm25 reads: the number of
// documents, the total number of tokens, and a stamp naming the state they
// were read from.
type SymbolFTSStats struct {
	Rows   int64
	Tokens int64
	Stamp  string
	// Lo and Hi are a generation's first and last document rowid
	// (SymbolFTSGenerationStats only; 0 when it has none).
	Lo, Hi int64
}

// SymbolFTSStats reads the whole shared table's statistics from its averages
// record (one row read), stamped with the table's corpus stamp.
func (s *Store) SymbolFTSStats(ctx context.Context) (SymbolFTSStats, error) {
	var out SymbolFTSStats
	if s.coreless() {
		return out, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var block []byte
	err := s.db.QueryRowContext(ctx, `SELECT block FROM symbol_fts_data WHERE id = 1`).Scan(&block)
	if errors.Is(err, sql.ErrNoRows) {
		out.Stamp = s.symbolFTSCorpusStamp()
		return out, nil
	}
	if err != nil {
		return out, err
	}
	values := sqliteVarints(block)
	if len(values) > 0 {
		out.Rows = int64(values[0])
		for _, v := range values[1:] {
			out.Tokens += int64(v)
		}
	}
	out.Stamp = s.symbolFTSCorpusStamp()
	return out, nil
}

// generationFTSStats memoizes a sealed generation's statistics: its rows
// never change after publication.
var generationFTSStats sync.Map // key: generationFTSStatsKey

type generationFTSStatsKey struct {
	core       *storeCore
	generation int64
}

// SymbolFTSGenerationStats is one generation's own statistics (its documents
// and their tokens, from the per-document sizes) and its scoped stamp: for a
// published generation, a stamp that names the generation and its rowid run
// and so never changes while it exists — a new top generation does not move
// it; for the base or a generation still building, the whole table's corpus
// stamp (their rows can still change).
func (s *Store) SymbolFTSGenerationStats(ctx context.Context, generation int64) (SymbolFTSStats, error) {
	var out SymbolFTSStats
	if s.coreless() {
		return out, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, sealed := s.sealedGeneration(generation)
	key := generationFTSStatsKey{s.storeCore, generation}
	if sealed {
		if v, ok := generationFTSStats.Load(key); ok {
			return v.(SymbolFTSStats), nil
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.fts_rowid, d.sz FROM symbol_fts_rowid m
  LEFT JOIN symbol_fts_docsize d ON d.id = m.fts_rowid
 WHERE m.view_gen = ?`, generation)
	if err != nil {
		return out, err
	}
	lo, hi := int64(-1), int64(-1)
	for rows.Next() {
		var rowid int64
		var sz []byte
		if err := rows.Scan(&rowid, &sz); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.Rows++
		for _, v := range sqliteVarints(sz) {
			out.Tokens += int64(v)
		}
		if lo < 0 || rowid < lo {
			lo = rowid
		}
		if rowid > hi {
			hi = rowid
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if lo >= 0 {
		out.Lo, out.Hi = lo, hi
	}
	if !sealed {
		out.Stamp = "corpus:" + s.symbolFTSCorpusStamp()
		return out, nil
	}
	out.Stamp = "gen:" + strconv.FormatInt(generation, 10) + ":" + strconv.FormatInt(lo, 10) + "-" +
		strconv.FormatInt(hi, 10) + ":" + strconv.FormatInt(out.Rows, 10) + ":" + strconv.FormatInt(out.Tokens, 10)
	generationFTSStats.Store(key, out)
	return out, nil
}

// SymbolFTSPrefixHits returns, per prefix, the number of documents whose
// tokens contain a term starting with it (the document frequency of the prefix
// query `prefix*`), over the whole table when generation < 0 or over one
// generation's documents otherwise. The count is FTS5's own doclist walk — the
// index is maintained incrementally by every write, so the count is always
// current — and is memoized per (generation stamp, prefix) so a repeat is free.
func (s *Store) SymbolFTSPrefixHits(ctx context.Context, generation int64, prefixes []string) (map[string]int64, error) {
	out := make(map[string]int64, len(prefixes))
	if s.coreless() {
		return out, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if generation < 0 && s.rowCountersReady.Load() {
		if hits, err := s.symbolFTSPrefixHitsIncremental(ctx, prefixes); err == nil {
			return hits, nil
		}
		// Any failure of the incremental path falls back to the counts below.
	}
	var stamp string
	if generation < 0 {
		stamp = "corpus:" + s.symbolFTSCorpusStamp()
	} else {
		st, err := s.SymbolFTSGenerationStats(ctx, generation)
		if err != nil {
			return nil, err
		}
		stamp = st.Stamp
	}
	for _, prefix := range prefixes {
		term := ftsPrefixTerm(prefix)
		if term == "" {
			out[prefix] = 0
			continue
		}
		memo := stamp + "\x00" + strconv.FormatInt(generation, 10) + "\x00" + term
		if v, ok := symbolFTSPrefixMemo.Load(memoKey{s.storeCore, memo}); ok {
			out[prefix] = v.(int64)
			continue
		}
		var n int64
		var err error
		noteSymbolFTSPrefixWalk()
		if generation < 0 {
			err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, term).Scan(&n)
		} else {
			err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts
  CROSS JOIN symbol_fts_rowid m ON m.fts_rowid = symbol_fts.rowid AND m.view_gen = ?
 WHERE symbol_fts MATCH ?`, generation, term).Scan(&n)
		}
		if err != nil {
			return nil, fmt.Errorf("symbol fts prefix hits %q: %w", prefix, err)
		}
		symbolFTSPrefixMemo.Store(memoKey{s.storeCore, memo}, n)
		out[prefix] = n
	}
	return out, nil
}

type memoKey struct {
	core *storeCore
	key  string
}

// symbolFTSPrefixMemo holds prefix counts by (stamp, generation, term). A
// stamp that moves makes the old keys unreachable; the memo is bounded by the
// distinct terms asked, which a search surface keeps small.
var symbolFTSPrefixMemo sync.Map

// ftsPrefixTerm renders a user prefix as an FTS5 prefix query on the tokens
// column: the word quoted (so FTS5 syntax in it is literal) with `*` after.
func ftsPrefixTerm(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return ""
	}
	return `tokens : "` + strings.ReplaceAll(prefix, `"`, `""`) + `" *`
}

// SymbolFTSRow is one document of a generation: its node, repository, token
// text and token count (FTS5's own per-document size).
type SymbolFTSRow struct {
	RowID      int64
	NodeID     string
	RepoPrefix string
	Tokens     string
	TokenCount int
}

// SymbolFTSGenerationRows returns one generation's documents in rowid order,
// after afterRowID, up to limit (≤ 0: 1,000): every document when match is
// empty, else those matching the FTS5 query match. No bm25 is computed, so the
// cost is the generation's own rows (bounded by its rowid run when it has
// one), never the whole table's ranking. next is the rowid to resume after, 0
// when the generation is exhausted.
func (s *Store) SymbolFTSGenerationRows(ctx context.Context, generation int64, match string, afterRowID int64, limit int) (rows []SymbolFTSRow, next int64, err error) {
	if s.coreless() {
		return nil, 0, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		limit = 1000
	}
	var q string
	var args []any
	if match == "" {
		// No MATCH: the sidecar's (view_gen, fts_rowid) range drives the read
		// and each document is one rowid seek, so the cost is the
		// generation's own documents wherever its rowids lie.
		q = symbolFTSGenerationSeekSQL
		args = []any{generation, afterRowID, limit + 1}
	} else {
		q = `SELECT symbol_fts.rowid, symbol_fts.node_id, symbol_fts.repo_prefix, symbol_fts.tokens, d.sz
  FROM symbol_fts
  CROSS JOIN symbol_fts_rowid m ON m.fts_rowid = symbol_fts.rowid AND m.view_gen = ?
  LEFT JOIN symbol_fts_docsize d ON d.id = symbol_fts.rowid
 WHERE symbol_fts.rowid > ? AND symbol_fts MATCH ?`
		args = []any{generation, afterRowID, match}
		if span, ok, serr := s.symbolFTSGenerationSpan(ctx, generation); serr == nil && ok && span.dense() {
			q += ` AND symbol_fts.rowid BETWEEN ? AND ?`
			args = append(args, span.lo, span.hi)
		}
		q += ` ORDER BY symbol_fts.rowid LIMIT ?`
		args = append(args, limit+1)
	}
	cur, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close()
	for cur.Next() {
		var r SymbolFTSRow
		var sz []byte
		if err := cur.Scan(&r.RowID, &r.NodeID, &r.RepoPrefix, &r.Tokens, &sz); err != nil {
			return nil, 0, err
		}
		for _, v := range sqliteVarints(sz) {
			r.TokenCount += int(v)
		}
		rows = append(rows, r)
	}
	if err := cur.Err(); err != nil {
		return nil, 0, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].RowID
	}
	return rows, next, ctx.Err()
}

// sqliteVarints decodes a run of SQLite varints (big-endian 7-bit groups,
// the ninth byte whole), the encoding of FTS5's averages record and
// per-document sizes.
func sqliteVarints(b []byte) []uint64 {
	var out []uint64
	for len(b) > 0 {
		var v uint64
		n := 0
		for n < 9 && n < len(b) {
			c := b[n]
			n++
			if n == 9 {
				v = v<<8 | uint64(c)
				break
			}
			v = v<<7 | uint64(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
		out = append(out, v)
		b = b[n:]
	}
	return out
}

// symbolFTSPrefixWalkObserver, when set by a test, counts every FTS5 prefix
// count this file runs (each one walks the prefix's doclists over the whole
// shared table). nil in production.
var symbolFTSPrefixWalkObserver func()

func noteSymbolFTSPrefixWalk() {
	if observe := symbolFTSPrefixWalkObserver; observe != nil {
		observe()
	}
}

// symbolFTSPrefixBaseline is one term's whole-table document frequency and
// the table state it was taken at: every generation's revision
// (symbol_fts_generation_revisions) and the table's document count.
type symbolFTSPrefixBaseline struct {
	count int64
	rows  int64
	revs  map[int64]int64
}

type symbolFTSPrefixState struct {
	mu    sync.Mutex
	terms map[string]*symbolFTSPrefixBaseline
}

var symbolFTSPrefixStates sync.Map // *storeCore → *symbolFTSPrefixState

// symbolFTSSmallGeneration bounds the generations whose prefix counts are
// taken from their rows in Go instead of an FTS5 count.
const symbolFTSSmallGeneration = 5000

// symbolFTSPrefixHitsIncremental keeps each term's whole-table count current
// without walking the shared index again after a publication. In one read
// snapshot it reads every generation's revision and the table's document
// count, and then per term:
//   - no baseline, a generation whose revision moved or vanished (a write to
//     it, a retirement), or a document count the added generations do not
//     explain: one FTS5 count, the new baseline;
//   - otherwise: the baseline plus the counts of the generations added since,
//     taken from their rows (tokenized as unicode61 does for ASCII) when they
//     are small and ASCII, else by an FTS5 count scoped to the generation.
//
// A new top generation therefore costs its own rows, never a walk of the
// table. Every count is taken inside the snapshot the revisions were read in.
func (s *Store) symbolFTSPrefixHitsIncremental(ctx context.Context, prefixes []string) (map[string]int64, error) {
	v, _ := symbolFTSPrefixStates.LoadOrStore(s.storeCore, &symbolFTSPrefixState{terms: map[string]*symbolFTSPrefixBaseline{}})
	state := v.(*symbolFTSPrefixState)
	state.mu.Lock()
	defer state.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only snapshot
	revs := map[int64]int64{}
	rrows, err := tx.QueryContext(ctx, `SELECT view_gen, rev FROM `+symbolFTSRevisionsTable)
	if err != nil {
		return nil, err
	}
	for rrows.Next() {
		var g, r int64
		if err := rrows.Scan(&g, &r); err != nil {
			_ = rrows.Close()
			return nil, err
		}
		revs[g] = r
	}
	if err := rrows.Err(); err != nil {
		_ = rrows.Close()
		return nil, err
	}
	if err := rrows.Close(); err != nil {
		return nil, err
	}
	var nRow int64
	var block []byte
	if err := tx.QueryRowContext(ctx, `SELECT block FROM symbol_fts_data WHERE id = 1`).Scan(&block); err == nil {
		if vals := sqliteVarints(block); len(vals) > 0 {
			nRow = int64(vals[0])
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// The added generations' rows, read once for every term.
	addedRows := map[int64][]SymbolFTSRow{}
	addedCounted := map[int64]int64{}
	out := make(map[string]int64, len(prefixes))
	for _, prefix := range prefixes {
		term := ftsPrefixTerm(prefix)
		if term == "" {
			out[prefix] = 0
			continue
		}
		folded := strings.ToLower(strings.TrimSpace(prefix))
		base := state.terms[folded]
		fresh := base == nil
		var added []int64
		if !fresh {
			for g, r := range base.revs {
				if revs[g] != r {
					fresh = true
					break
				}
			}
		}
		if !fresh {
			for g := range revs {
				if _, known := base.revs[g]; !known {
					added = append(added, g)
				}
			}
			var addedDocs int64
			for _, g := range added {
				n, ok := addedCounted[g]
				if !ok {
					if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts_rowid WHERE view_gen = ?`, g).Scan(&n); err != nil {
						return nil, err
					}
					addedCounted[g] = n
				}
				addedDocs += n
			}
			if base.rows+addedDocs != nRow {
				fresh = true
			}
		}
		if fresh {
			var n int64
			noteSymbolFTSPrefixWalk()
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, term).Scan(&n); err != nil {
				return nil, err
			}
			state.terms[folded] = &symbolFTSPrefixBaseline{count: n, rows: nRow, revs: cloneRevs(revs)}
			out[prefix] = n
			continue
		}
		count := base.count
		for _, g := range added {
			n, err := s.generationPrefixCountTx(ctx, tx, g, folded, term, addedRows)
			if err != nil {
				return nil, err
			}
			count += n
		}
		state.terms[folded] = &symbolFTSPrefixBaseline{count: count, rows: nRow, revs: cloneRevs(revs)}
		out[prefix] = count
	}
	return out, nil
}

// generationPrefixCountTx counts one generation's documents with a token
// starting with folded: from its rows in Go when it is small and ASCII
// (unicode61 splits ASCII text into runs of letters and digits and folds
// case), else by an FTS5 count scoped to the generation.
func (s *Store) generationPrefixCountTx(ctx context.Context, tx *sql.Tx, generation int64, folded, term string, cache map[int64][]SymbolFTSRow) (int64, error) {
	rows, ok := cache[generation]
	if !ok {
		cur, err := tx.QueryContext(ctx, `SELECT symbol_fts.tokens FROM symbol_fts_rowid m
  JOIN symbol_fts ON symbol_fts.rowid = m.fts_rowid
 WHERE m.view_gen = ? LIMIT ?`, generation, symbolFTSSmallGeneration+1)
		if err != nil {
			return 0, err
		}
		for cur.Next() {
			var r SymbolFTSRow
			if err := cur.Scan(&r.Tokens); err != nil {
				_ = cur.Close()
				return 0, err
			}
			rows = append(rows, r)
		}
		if err := cur.Err(); err != nil {
			_ = cur.Close()
			return 0, err
		}
		if err := cur.Close(); err != nil {
			return 0, err
		}
		cache[generation] = rows
	}
	if len(rows) <= symbolFTSSmallGeneration && asciiFoldable(folded) {
		var n int64
		inGo := true
		for _, r := range rows {
			has, ascii := asciiTokensHavePrefix(r.Tokens, folded)
			if !ascii {
				inGo = false
				break
			}
			if has {
				n++
			}
		}
		if inGo {
			return n, nil
		}
	}
	var n int64
	noteSymbolFTSPrefixWalk()
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts
  CROSS JOIN symbol_fts_rowid m ON m.fts_rowid = symbol_fts.rowid AND m.view_gen = ?
 WHERE symbol_fts MATCH ?`, generation, term).Scan(&n)
	return n, err
}

func asciiFoldable(term string) bool {
	if term == "" {
		return false
	}
	for i := 0; i < len(term); i++ {
		c := term[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// asciiTokensHavePrefix reports whether text holds a unicode61 token (a run of
// ASCII letters and digits, lower-cased) starting with prefix; ascii is false
// when text holds a non-ASCII byte.
func asciiTokensHavePrefix(text, prefix string) (has, ascii bool) {
	start := -1
	for i := 0; i <= len(text); i++ {
		alnum := false
		if i < len(text) {
			c := text[i]
			if c >= 0x80 {
				return false, false
			}
			alnum = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		}
		if alnum && start < 0 {
			start = i
		}
		if !alnum && start >= 0 {
			if !has && i-start >= len(prefix) && strings.EqualFold(text[start:start+len(prefix)], prefix) {
				has = true
			}
			start = -1
		}
	}
	return has, true
}

func cloneRevs(in map[int64]int64) map[int64]int64 {
	out := make(map[int64]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SymbolFTSViewGenerationRows runs match once over the shared symbol_fts
// table and returns, per requested generation, its matching documents in
// rowid order with their token text and token count (no bm25): the rows each
// generation's own SymbolFTSGenerationRows(match) would return, read with one
// walk of the index instead of one per generation. A generation with more
// than perGeneration matching documents (perGeneration <= 0: no bound) is
// reported in over and its rows are not returned. Every requested generation
// is a key of rows (nil when it has no match).
//
// When every requested generation has a measured rowid run (derived
// generations), the MATCH is bounded by the union of their runs; the base
// generation, whose rowids scatter over the table, leaves it unbounded. The
// ownership sidecar decides membership either way.
func (s *Store) SymbolFTSViewGenerationRows(ctx context.Context, generations []int64, match string, perGeneration int) (map[int64][]SymbolFTSRow, map[int64]bool, error) {
	if s.coreless() {
		return nil, nil, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows := make(map[int64][]SymbolFTSRow, len(generations))
	over := map[int64]bool{}
	var gens []int64
	for _, g := range generations {
		if _, dup := rows[g]; dup {
			continue
		}
		rows[g] = nil
		gens = append(gens, g)
	}
	if len(gens) == 0 || match == "" {
		return rows, over, ctx.Err()
	}
	bounded := true
	lo, hi := int64(-1), int64(-1)
	for _, g := range gens {
		span, measured, err := s.symbolFTSGenerationSpan(ctx, g)
		if err != nil {
			return nil, nil, err
		}
		if !measured {
			bounded = false
			break
		}
		if !span.present {
			continue
		}
		if lo < 0 || span.lo < lo {
			lo = span.lo
		}
		if span.hi > hi {
			hi = span.hi
		}
	}
	q := `SELECT symbol_fts.rowid, m.view_gen, symbol_fts.node_id, symbol_fts.repo_prefix, symbol_fts.tokens, d.sz
  FROM symbol_fts
  CROSS JOIN symbol_fts_rowid m ON m.fts_rowid = symbol_fts.rowid AND m.view_gen IN (?` + strings.Repeat(`,?`, len(gens)-1) + `)
  LEFT JOIN symbol_fts_docsize d ON d.id = symbol_fts.rowid
 WHERE symbol_fts MATCH ?`
	args := make([]any, 0, len(gens)+3)
	for _, g := range gens {
		args = append(args, g)
	}
	args = append(args, match)
	if bounded {
		if lo < 0 {
			return rows, over, ctx.Err() // no requested generation owns a document
		}
		q += ` AND symbol_fts.rowid BETWEEN ? AND ?`
		args = append(args, lo, hi)
	}
	q += ` ORDER BY symbol_fts.rowid`
	cur, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer cur.Close()
	for cur.Next() {
		var r SymbolFTSRow
		var g int64
		var sz []byte
		if err := cur.Scan(&r.RowID, &g, &r.NodeID, &r.RepoPrefix, &r.Tokens, &sz); err != nil {
			return nil, nil, err
		}
		if over[g] {
			continue
		}
		if perGeneration > 0 && len(rows[g]) >= perGeneration {
			over[g] = true
			rows[g] = nil
			continue
		}
		for _, v := range sqliteVarints(sz) {
			r.TokenCount += int(v)
		}
		rows[g] = append(rows[g], r)
	}
	if err := cur.Err(); err != nil {
		return nil, nil, err
	}
	return rows, over, ctx.Err()
}

// symbolFTSGenerationSeekSQL reads a generation's documents from the ownership
// sidecar in rowid order and seeks each one in symbol_fts (FTS5's rowid
// lookup) and its docsize row. CROSS JOIN keeps the sidecar the outer loop.
const symbolFTSGenerationSeekSQL = `SELECT f.rowid, f.node_id, f.repo_prefix, f.tokens, d.sz
  FROM symbol_fts_rowid AS m INDEXED BY symbol_fts_rowid_by_generation
  CROSS JOIN symbol_fts AS f
  LEFT JOIN symbol_fts_docsize AS d ON d.id = m.fts_rowid
 WHERE m.view_gen = ? AND m.fts_rowid > ? AND f.rowid = m.fts_rowid
 ORDER BY m.fts_rowid
 LIMIT ?`
