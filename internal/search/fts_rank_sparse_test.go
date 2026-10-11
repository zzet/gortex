package search_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// ownedFTSTable is a store double whose generations own arbitrary rowids, as
// the live store's do once generations interleave: a generation's documents
// need not form one rowid run. Its reads cost what the store's do, counted in
// table rows walked:
//
//   - a read with no MATCH is the store's sidecar seek: one rowid seek per
//     document of the generation, wherever its rowids lie;
//   - a MATCH read walks the documents its MATCH hits over the whole table.
type ownedFTSTable struct {
	*ftsTable
	owner      map[int64]int64 // rowid -> generation
	unknownRun map[int64]bool  // generations reported without a run
	walked     []walk
}

type walk struct {
	generation int64
	match      string
	rows       int64
}

func newOwnedFTSTable(t *testing.T) *ownedFTSTable {
	return &ownedFTSTable{ftsTable: newFTSTable(t), owner: map[int64]int64{}}
}

func (o *ownedFTSTable) insert(t *testing.T, generation int64, row [3]string) {
	t.Helper()
	res, err := o.db.Exec(`INSERT INTO symbol_fts(node_id, repo_prefix, tokens) VALUES (?, ?, ?)`, row[0], row[1], row[2])
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	o.owner[id] = generation
}

func (o *ownedFTSTable) run(generation int64) (docs, lo, hi int64) {
	lo, hi = -1, -1
	for rowid, g := range o.owner {
		if g != generation {
			continue
		}
		docs++
		if lo < 0 || rowid < lo {
			lo = rowid
		}
		if rowid > hi {
			hi = rowid
		}
	}
	return docs, lo, hi
}

func (o *ownedFTSTable) SymbolFTSGenerationRun(_ context.Context, generation int64) (search.FTSGenerationRun, error) {
	docs, lo, hi := o.run(generation)
	if o.unknownRun[generation] {
		return search.FTSGenerationRun{Docs: docs, DocsKnown: true}, nil
	}
	return search.FTSGenerationRun{Docs: docs, Lo: lo, Hi: hi, Known: docs > 0, DocsKnown: true}, nil
}

func (o *ownedFTSTable) SymbolFTSGenerationRows(ctx context.Context, generation int64, match string, _ []string) ([]search.FTSRankRow, error) {
	q := `SELECT f.rowid, f.node_id, f.repo_prefix, f.tokens, d.sz FROM symbol_fts f JOIN symbol_fts_docsize d ON d.id = f.rowid`
	var args []any
	if match != "" {
		q += ` WHERE symbol_fts MATCH ?`
		args = append(args, match)
	}
	rows, err := o.db.QueryContext(ctx, q+` ORDER BY f.rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.FTSRankRow
	var hitRows int64
	for rows.Next() {
		var r search.FTSRankRow
		var sz []byte
		if err := rows.Scan(&r.Rowid, &r.NodeID, &r.RepoPrefix, &r.Tokens, &sz); err != nil {
			return nil, err
		}
		hitRows++
		if o.owner[r.Rowid] != generation {
			continue
		}
		for off := 0; off < len(sz); {
			v, m := varint(sz[off:])
			r.TokenCount += int(v)
			off += m
		}
		out = append(out, r)
	}
	walked := hitRows
	if match == "" {
		walked, _, _ = o.run(generation) // one seek per document
	}
	o.walked = append(o.walked, walk{generation: generation, match: match, rows: walked})
	return out, rows.Err()
}

// storeRankOwned is the store's own ranking of one generation: FTS5's bm25
// order over the MATCH, restricted to the generation's documents.
func (o *ownedFTSTable) storeRankOwned(t *testing.T, generation int64, query string, limit int) []graph.SymbolHit {
	t.Helper()
	terms, ok := search.FTSRankTerms(query)
	if !ok {
		t.Fatalf("query %q has no exact terms", query)
	}
	var ids []string
	for rowid, g := range o.owner {
		if g == generation {
			ids = append(ids, fmt.Sprint(rowid))
		}
	}
	sort.Strings(ids)
	rows, err := o.db.Query(`SELECT node_id, bm25(symbol_fts) FROM symbol_fts WHERE symbol_fts MATCH ? AND rowid IN (`+strings.Join(ids, ",")+`)
 AND symbol_fts.rank MATCH 'bm25()' ORDER BY symbol_fts.rank LIMIT ?`, search.FTSRankMatch(terms), limit)
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
		hits = append(hits, graph.SymbolHit{NodeID: id, Score: -score})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return hits
}

// Every generation of known size is read whole once, whatever its rowid run —
// interleaved with another generation's rows, spread over a run far wider than
// its documents, dense, or large — and the read costs its own documents (the
// store's sidecar seek). Every later query, new names included, is answered
// from the kept documents with no read, and every ranking equals FTS5's.
func TestEveryGenerationIsReadWholeOnceAtItsOwnCost(t *testing.T) {
	ctx := context.Background()
	o := newOwnedFTSTable(t)
	rows := fixtureRows("row", 6200, 3)
	next := 0
	take := func() [3]string { r := rows[next%len(rows)]; next++; r[0] = fmt.Sprintf("n%d", next); return r }
	// 30: 40 documents interleaved with 150 rows of 31 each.
	for i := 0; i < 40; i++ {
		o.insert(t, 30, take())
		for j := 0; j < 150; j++ {
			o.insert(t, 31, take())
		}
	}
	// 32: 10 documents over a run of about 3,000 rowids.
	for i := 0; i < 10; i++ {
		o.insert(t, 32, take())
		for j := 0; j < 300; j++ {
			o.insert(t, 31, take())
		}
	}
	// 33: 50 documents in one run; 34: 3,000 documents over about 7,000.
	for i := 0; i < 50; i++ {
		o.insert(t, 33, take())
	}
	for i := 0; i < 3000; i++ {
		o.insert(t, 34, take())
		if i%3 == 0 {
			o.insert(t, 31, take())
		}
	}
	ranker := search.NewFTSRanker(o)
	for _, q := range []string{"config", "checkout route", "flip coordinator", "dirty sampler refresh", "newDirtySamplerRen9"} {
		for _, generation := range []int64{30, 32, 33, 34} {
			got, ok, err := ranker.Rank(ctx, generation, true, q, nil, 20)
			if err != nil || !ok {
				t.Fatalf("gen %d %q: ok=%v err=%v", generation, q, ok, err)
			}
			if want := o.storeRankOwned(t, generation, q, 20); renderHits(got) != renderHits(want) {
				t.Fatalf("gen %d %q:\n memory %s\n fts5   %s", generation, q, renderHits(got), renderHits(want))
			}
		}
	}
	whole := map[int64]int{}
	for _, w := range o.walked {
		if w.match != "" {
			t.Fatalf("generation %d took a MATCH read", w.generation)
		}
		whole[w.generation]++
		if docs, _, _ := o.run(w.generation); w.rows != docs {
			t.Fatalf("a read of generation %d walked %d rows for its %d documents", w.generation, w.rows, docs)
		}
	}
	for _, generation := range []int64{30, 32, 33, 34} {
		if whole[generation] != 1 {
			t.Fatalf("generation %d was read whole %d time(s), want 1", generation, whole[generation])
		}
	}
}
