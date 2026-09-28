package search_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// ftsTable is a symbol_fts-shaped FTS5 table (same schema and tokenizer as the
// store's) whose generations are rowid runs, served through a Source that
// reads what the store would: the averages record, per-phrase row counts, and
// a generation's rows with their docsize.
type ftsTable struct {
	db    *sql.DB
	spans map[int64][2]int64
	reads int
}

func newFTSTable(t *testing.T) *ftsTable {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fts.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE VIRTUAL TABLE symbol_fts USING fts5(node_id UNINDEXED, repo_prefix UNINDEXED, tokens)`); err != nil {
		t.Fatal(err)
	}
	return &ftsTable{db: db, spans: map[int64][2]int64{}}
}

// addGeneration appends a generation's rows as one rowid run.
func (f *ftsTable) addGeneration(t *testing.T, generation int64, rows [][3]string) {
	t.Helper()
	var lo, hi int64
	for i, r := range rows {
		res, err := f.db.Exec(`INSERT INTO symbol_fts(node_id, repo_prefix, tokens) VALUES (?, ?, ?)`, r[0], r[1], r[2])
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if i == 0 {
			lo = id
		}
		hi = id
	}
	f.spans[generation] = [2]int64{lo, hi}
}

func (f *ftsTable) deleteRow(t *testing.T, nodeID string) {
	t.Helper()
	if _, err := f.db.Exec(`DELETE FROM symbol_fts WHERE node_id = ?`, nodeID); err != nil {
		t.Fatal(err)
	}
}

// varint decodes one SQLite varint.
func varint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	if len(b) >= 9 {
		return v<<8 | uint64(b[8]), 9
	}
	return v, len(b)
}

func (f *ftsTable) SymbolFTSStats(ctx context.Context) (search.FTSRankStats, error) {
	var block []byte
	if err := f.db.QueryRowContext(ctx, `SELECT block FROM symbol_fts_data WHERE id = 1`).Scan(&block); err != nil {
		return search.FTSRankStats{}, err
	}
	rows, n := varint(block)
	var tokens uint64
	for off := n; off < len(block); {
		v, m := varint(block[off:])
		tokens += v
		off += m
	}
	return search.FTSRankStats{Rows: int64(rows), Tokens: int64(tokens), Stamp: fmt.Sprintf("%x", block)}, nil
}

func (f *ftsTable) SymbolFTSPrefixHits(ctx context.Context, terms []string) (map[string]int64, string, error) {
	stats, err := f.SymbolFTSStats(ctx)
	if err != nil {
		return nil, "", err
	}
	out := make(map[string]int64, len(terms))
	for _, term := range terms {
		var n int64
		if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, search.FTSRankMatch([]string{term})).Scan(&n); err != nil {
			return nil, "", err
		}
		out[term] = n
	}
	return out, stats.Stamp, nil
}

func (f *ftsTable) SymbolFTSGenerationRows(ctx context.Context, generation int64, _ string, repoAllow []string) ([]search.FTSRankRow, error) {
	f.reads++
	span := f.spans[generation]
	q := `SELECT f.rowid, f.node_id, f.repo_prefix, f.tokens, d.sz FROM symbol_fts f JOIN symbol_fts_docsize d ON d.id = f.rowid WHERE f.rowid BETWEEN ? AND ?`
	args := []any{span[0], span[1]}
	if len(repoAllow) > 0 {
		q += ` AND f.repo_prefix IN (''` + strings.Repeat(`,?`, len(repoAllow)) + `)`
		for _, r := range repoAllow {
			args = append(args, r)
		}
	}
	rows, err := f.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.FTSRankRow
	for rows.Next() {
		var r search.FTSRankRow
		var sz []byte
		if err := rows.Scan(&r.Rowid, &r.NodeID, &r.RepoPrefix, &r.Tokens, &sz); err != nil {
			return nil, err
		}
		for off := 0; off < len(sz); {
			v, m := varint(sz[off:])
			r.TokenCount += int(v)
			off += m
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// storeRank is the store's own per-generation ranking: the span query.
func (f *ftsTable) storeRank(t *testing.T, generation int64, query string, repoAllow []string, limit int) []graph.SymbolHit {
	t.Helper()
	terms, ok := search.FTSRankTerms(query)
	if !ok {
		t.Fatalf("query %q has no exact terms", query)
	}
	span := f.spans[generation]
	q := `SELECT node_id, bm25(symbol_fts) FROM symbol_fts WHERE symbol_fts MATCH ? AND rowid BETWEEN ? AND ?`
	args := []any{search.FTSRankMatch(terms), span[0], span[1]}
	if len(repoAllow) > 0 {
		q += ` AND repo_prefix IN (''` + strings.Repeat(`,?`, len(repoAllow)) + `)`
		for _, r := range repoAllow {
			args = append(args, r)
		}
	}
	q += ` AND symbol_fts.rank MATCH 'bm25()' ORDER BY symbol_fts.rank LIMIT ?`
	args = append(args, limit)
	rows, err := f.db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var hits []graph.SymbolHit
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			t.Fatal(err)
		}
		if id != "" {
			hits = append(hits, graph.SymbolHit{NodeID: id, Score: -score})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return hits
}

func renderHits(hits []graph.SymbolHit) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%x ", h.NodeID, math.Float64bits(h.Score))
	}
	return b.String()
}

// fixtureRows builds a generation's rows over a small vocabulary so prefixes
// overlap, tokens repeat within a row, rows tie exactly, and repos differ.
func fixtureRows(prefix string, n int, seed int) [][3]string {
	words := []string{"config", "conf", "configure", "load", "loader", "check", "checkout", "coordinator", "route", "flip", "skip", "rule", "match", "matches", "sampler", "dirty", "refresh"}
	out := make([][3]string, 0, n)
	for i := 0; i < n; i++ {
		k := (i*7 + seed) % len(words)
		tokens := []string{words[k], words[(k+3)%len(words)]}
		if i%3 == 0 {
			tokens = append(tokens, words[k]) // a repeated token
		}
		if i%5 == 0 {
			tokens = append(tokens, words[(k+5)%len(words)], words[(k+9)%len(words)])
		}
		repo := []string{"alpha", "beta", ""}[i%3]
		if i%11 == 0 { // exact duplicate text: a score tie broken by rowid
			tokens = []string{"checkout", "route"}
		}
		out = append(out, [3]string{fmt.Sprintf("%s/n%d", prefix, i), repo, strings.Join(tokens, " ")})
	}
	return out
}

// TestFTSRankerRanksLikeFTS5 compares the in-memory ranking of every generation
// with FTS5's own for many queries, repo scopes and limits: the same hits in
// the same order with the same score bits. Then a new generation is published
// and a row deleted — the table statistics move — and the lower generations'
// kept match lists still rank exactly, without being read again.
func TestFTSRankerRanksLikeFTS5(t *testing.T) {
	ctx := context.Background()
	f := newFTSTable(t)
	f.addGeneration(t, 10, fixtureRows("root", 400, 1))
	f.addGeneration(t, 11, fixtureRows("commit", 60, 4))
	f.addGeneration(t, 12, fixtureRows("dirty", 25, 9))
	ranker := search.NewFTSRanker(f)
	// "co" matches more than half the table's rows, so its IDF takes FTS5's
	// 1e-6 floor.
	queries := []string{"config", "checkout route", "matchesSkipRule", "load config loader", "conf", "flip", "dirty sampler refresh", "coordinator checkout", "config config", "co", "co route"}
	repos := [][]string{nil, {"alpha"}, {"beta", "alpha"}}
	limits := []int{1, 5, 17, 200}
	compare := func(label string, generations []int64) {
		for _, generation := range generations {
			for _, q := range queries {
				for _, repo := range repos {
					for _, limit := range limits {
						want := f.storeRank(t, generation, q, repo, limit)
						got, ok, err := ranker.Rank(ctx, generation, true, q, repo, limit)
						if err != nil || !ok {
							t.Fatalf("%s gen %d %q: ok=%v err=%v", label, generation, q, ok, err)
						}
						if renderHits(got) != renderHits(want) {
							t.Fatalf("%s gen %d %q repos %v limit %d:\n memory %s\n fts5   %s", label, generation, q, repo, limit, renderHits(got), renderHits(want))
						}
					}
				}
			}
		}
	}
	compare("before", []int64{10, 11, 12})
	readsBefore := f.reads

	// A publication: a new generation's rows land and an old row goes away.
	f.addGeneration(t, 13, fixtureRows("next", 30, 13))
	f.deleteRow(t, "dirty/n3")
	delete(f.spans, 12)
	compare("after", []int64{10, 11})
	if f.reads != readsBefore {
		t.Fatalf("the kept generations were read again after the publication: %d reads, want %d", f.reads, readsBefore)
	}
	compare("new", []int64{13})
}

// A query term or a row the package cannot tokenize exactly is not ranked in
// memory: the caller keeps the store's query.
func TestFTSRankerDeclinesWhatItCannotRankExactly(t *testing.T) {
	ctx := context.Background()
	f := newFTSTable(t)
	f.addGeneration(t, 1, [][3]string{{"a", "", "café config"}, {"b", "", "config"}})
	ranker := search.NewFTSRanker(f)
	if _, ok, _ := ranker.Rank(ctx, 1, true, "config", nil, 10); ok {
		t.Fatal("a generation holding a non-ASCII row was ranked in memory")
	}
	if _, ok := search.FTSRankTerms("café"); ok {
		t.Fatal("a non-ASCII query term was accepted")
	}
}

// viewStore is a store double over the FTS table: its own per-generation
// search is the store's (FTS5 span query, with an exact-name tier), and it
// serves the ranker's reads.
type viewStore struct {
	*ftsTable
	t     *testing.T
	exact map[int64][]graph.SymbolHit // generation -> exact-tier hits
	sql   int                         // generations the FTS5 path ranked
}

func (v *viewStore) SearchSymbolsViewGenerationsRepoScopedContext(_ context.Context, query string, repos []string, gens []int64, limit int) (map[int64][]graph.SymbolHit, error) {
	out := make(map[int64][]graph.SymbolHit, len(gens))
	for _, g := range gens {
		if hits, ok := v.exact[g]; ok {
			out[g] = hits
			continue
		}
		v.sql++
		out[g] = v.storeRank(v.t, g, query, repos, limit)
	}
	return out, nil
}

func (v *viewStore) SymbolExactHitsViewGenerations(_ context.Context, _ string, _ []string, gens []int64, _ int) (map[int64][]graph.SymbolHit, []int64, error) {
	out := make(map[int64][]graph.SymbolHit)
	var rest []int64
	for _, g := range gens {
		if hits, ok := v.exact[g]; ok {
			out[g] = hits
			continue
		}
		rest = append(rest, g)
	}
	return out, rest, nil
}

func (v *viewStore) SymbolSearchViewGeneration() int64 { return 0 }
func (v *viewStore) SharesSymbolSearchCore(any) bool   { return true }
func (v *viewStore) SymbolSearchCoreKey() any          { return v.ftsTable }

// The view search through the ranker equals the store's own per-generation
// search: the exact tier is kept, every other generation is ranked in memory
// with the store's hits and score bits, and a generation the ranker declines
// (non-ASCII rows) is answered by the store.
func TestViewGenerationsRankInMemoryLikeTheStore(t *testing.T) {
	ctx := context.Background()
	search.ResetFTSRankersForTest()
	f := newFTSTable(t)
	f.addGeneration(t, 1, fixtureRows("root", 300, 2))
	f.addGeneration(t, 2, fixtureRows("commit", 40, 5))
	f.addGeneration(t, 3, [][3]string{{"x/a", "", "config café"}, {"x/b", "", "config"}})
	f.addGeneration(t, 4, fixtureRows("dirty", 20, 7))
	store := &viewStore{ftsTable: f, t: t, exact: map[int64][]graph.SymbolHit{2: {{NodeID: "commit/exact", Score: 100}}}}
	gens := []int64{1, 2, 3, 4}
	for _, q := range []string{"config", "checkout route", "load config"} {
		store.sql = 0
		got, err := search.SearchViewGenerationsForTest(ctx, store, q, gens, 17)
		if err != nil {
			t.Fatal(err)
		}
		if store.sql != 1 {
			t.Fatalf("%q: the store ranked %d generations, want only the declined one", q, store.sql)
		}
		want, _ := store.SearchSymbolsViewGenerationsRepoScopedContext(ctx, q, nil, gens, 17)
		for _, g := range gens {
			if renderHits(got[g]) != renderHits(want[g]) {
				t.Fatalf("%q gen %d:\n view  %s\n store %s", q, g, renderHits(got[g]), renderHits(want[g]))
			}
		}
	}
}

// A term in more than half the table's rows has a negative raw IDF, which
// FTS5 floors at 1e-6; the in-memory ranking floors it the same way.
func TestFTSRankerFloorsTheIDFLikeFTS5(t *testing.T) {
	ctx := context.Background()
	f := newFTSTable(t)
	var rows [][3]string
	for i := 0; i < 40; i++ {
		tokens := "shared filler"
		if i%4 == 0 {
			tokens = "shared shared rare"
		}
		if i%10 == 9 {
			tokens = "other filler"
		}
		rows = append(rows, [3]string{fmt.Sprintf("n%d", i), "", tokens})
	}
	f.addGeneration(t, 1, rows)
	ranker := search.NewFTSRanker(f)
	for _, q := range []string{"shared", "shared rare", "filler shared"} {
		want := f.storeRank(t, 1, q, nil, 50)
		got, ok, err := ranker.Rank(ctx, 1, true, q, nil, 50)
		if err != nil || !ok {
			t.Fatalf("%q: ok=%v err=%v", q, ok, err)
		}
		if renderHits(got) != renderHits(want) {
			t.Fatalf("%q:\n memory %s\n fts5   %s", q, renderHits(got), renderHits(want))
		}
	}
}
