package store_sqlite

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// unboundedSymbolPages is the reference: every generation ranked out of the
// one shared, unbounded FTS rank stream, exactly as the batch did before
// generations were bounded by their rowid runs.
func unboundedSymbolPages(t *testing.T, store *Store, query string, repos []string, generations []int64, limit int) map[int64][]graph.SymbolHit {
	t.Helper()
	out := make(map[int64][]graph.SymbolHit, len(generations))
	for _, generation := range generations {
		out[generation] = nil
	}
	args := []any{store.buildFTSMatch(query, true)}
	for _, generation := range generations {
		args = append(args, generation)
	}
	for _, repo := range repos {
		args = append(args, repo)
	}
	rows, err := store.db.Query(symbolFTSViewBatchQuery(len(generations), len(repos)), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			generation int64
			id         string
			score      float64
		)
		if err := rows.Scan(&generation, &id, &score); err != nil {
			t.Fatal(err)
		}
		if id == "" || len(out[generation]) >= limit {
			continue
		}
		out[generation] = append(out[generation], graph.SymbolHit{NodeID: id, Score: -score})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// unboundedSingleGeneration is the reference for the single-generation search.
func unboundedSingleGeneration(t *testing.T, store *Store, generation int64, query string, repos []string, limit int) []graph.SymbolHit {
	t.Helper()
	q := `SELECT symbol_fts.node_id, bm25(symbol_fts)
FROM symbol_fts
JOIN symbol_fts_rowid
  ON symbol_fts_rowid.fts_rowid = symbol_fts.rowid AND symbol_fts_rowid.view_gen = ?
WHERE symbol_fts MATCH ?`
	args := []any{generation, store.buildFTSMatch(query, true)}
	if len(repos) > 0 {
		q += ` AND symbol_fts.repo_prefix IN ('', ?` + strings.Repeat(`,?`, len(repos)-1) + `)`
		for _, repo := range repos {
			args = append(args, repo)
		}
	}
	q += ` AND symbol_fts.rank MATCH 'bm25()' ORDER BY symbol_fts.rank LIMIT ?`
	args = append(args, limit)
	rows, err := store.db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var hits []graph.SymbolHit
	for rows.Next() {
		var (
			id    string
			score float64
		)
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

// seedInterleavedGenerations writes the base corpus and three derived
// generations whose FTS writes interleave, so generation 11's rowid run
// contains generation 12's rows. Many documents carry identical tokens, so
// the rank order is full of ties; others vary so scores differ.
func seedInterleavedGenerations(t *testing.T) *Store {
	t.Helper()
	store := viewBatchTestStore(t)
	items := func(prefix string, n int) []graph.SymbolFTSItem {
		out := make([]graph.SymbolFTSItem, 0, n)
		for i := range n {
			tokens := "match skip rule"
			switch i % 5 {
			case 1:
				tokens = "match skip rule ren"
			case 2:
				tokens = fmt.Sprintf("matches other%d", i)
			case 3:
				tokens = "unrelated words only"
			}
			out = append(out, graph.SymbolFTSItem{NodeID: fmt.Sprintf("%s-%03d", prefix, i), Tokens: tokens})
		}
		return out
	}
	seed := func(generation int64, items []graph.SymbolFTSItem) {
		nodes := make([]*graph.Node, 0, len(items))
		for i, item := range items {
			repo := "repo"
			if i%7 == 3 {
				repo = "other"
			}
			nodes = append(nodes, &graph.Node{ID: item.NodeID, RepoPrefix: repo})
		}
		if err := viewBatchHandle(store, generation).AddBatchChecked(nodes, nil); err != nil {
			t.Fatal(err)
		}
		seedViewBatchFTS(t, store, generation, items)
	}
	seed(0, items("base", 80))
	seed(11, items("g11a", 40))
	seed(12, items("g12", 30))
	seed(11, items("g11b", 40))
	seed(13, items("g13", 25))
	return store
}

func TestSearchSymbolsViewGenerationsBoundedRunsMatchTheUnboundedStream(t *testing.T) {
	store := seedInterleavedGenerations(t)
	for _, generation := range []int64{11, 12, 13} {
		span, measured, err := store.symbolFTSGenerationSpan(context.Background(), generation)
		if err != nil || !measured || !span.dense() {
			t.Fatalf("generation %d span = %+v measured=%v err=%v, want a dense run", generation, span, measured, err)
		}
	}
	generations := []int64{0, 11, 12, 13, 14}
	for _, query := range []string{"matchSkipRuleRen0", "match", "matches", "absentName", "rule ren"} {
		for _, repos := range [][]string{nil, {"repo"}} {
			for _, limit := range []int{1, 5, 17, 200} {
				name := fmt.Sprintf("%s/%v/%d", query, repos, limit)
				want := unboundedSymbolPages(t, store, query, repos, generations, limit)
				got, err := store.SearchSymbolsViewGenerationsRepoScopedContext(context.Background(), query, repos, generations, limit)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: bounded pages differ\n got=%v\nwant=%v", name, got, want)
				}
				for _, generation := range []int64{11, 12, 13} {
					single, err := viewBatchHandle(store, generation).SearchSymbolsRepoScopedContext(context.Background(), query, repos, limit)
					if err != nil {
						t.Fatalf("%s: single %d: %v", name, generation, err)
					}
					if wantSingle := unboundedSingleGeneration(t, store, generation, query, repos, limit); !reflect.DeepEqual(single, wantSingle) {
						t.Fatalf("%s: single generation %d differs\n got=%v\nwant=%v", name, generation, single, wantSingle)
					}
				}
			}
		}
	}
}

func TestSymbolFTSSpanQueryBoundsTheRankStreamByRowid(t *testing.T) {
	store := seedInterleavedGenerations(t)
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+symbolFTSSpanQuery(1, true),
		store.buildFTSMatch("match", true), int64(1), int64(9), int64(11), "repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	// FTS5 encodes the constraints it consumed in its index string: M is the
	// MATCH, r the rank ordering, and < / > the rowid bounds.
	var fts string
	for _, line := range plan {
		if strings.Contains(line, "SCAN symbol_fts VIRTUAL TABLE") {
			fts = line
		}
	}
	if fts == "" || !strings.Contains(fts, "<") || !strings.Contains(fts, ">") {
		t.Fatalf("the FTS scan does not consume the rowid bounds:\n%s", joined)
	}
	if strings.Contains(joined, "USE TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("bounded rank stream spilled to an external sort:\n%s", joined)
	}
}

func TestSymbolFTSGenerationSpanClassifiesRuns(t *testing.T) {
	store := seedInterleavedGenerations(t)
	span, measured, err := store.symbolFTSGenerationSpan(context.Background(), 99)
	if err != nil || !measured || !span.empty() {
		t.Fatalf("span of a generation without documents = %+v measured=%v err=%v", span, measured, err)
	}
	if _, measured, _ := store.symbolFTSGenerationSpan(context.Background(), 0); measured {
		t.Fatal("the base corpus must keep the unbounded plan")
	}
	wide := symbolFTSSpan{present: true, lo: 1, hi: 1_000_000, tableMax: 2_000_000}
	if wide.dense() {
		t.Fatal("a run spanning half the table was classified narrow")
	}
	narrow := symbolFTSSpan{present: true, lo: 1_500_000, hi: 1_600_000, tableMax: 2_000_000}
	if !narrow.dense() {
		t.Fatal("a run spanning a twentieth of the table was not bounded")
	}
}

// TestSearchSymbolsViewGenerationsStreamsOnlyUnboundedGenerations pins the
// cost contract: a derived generation with a narrow rowid run, or with no
// documents at all, never joins the shared unbounded rank stream; only the
// base corpus does.
func TestSearchSymbolsViewGenerationsStreamsOnlyUnboundedGenerations(t *testing.T) {
	store := seedInterleavedGenerations(t)
	var streamed [][]int64
	symbolFTSStreamObserver = func(generations []int64) { streamed = append(streamed, generations) }
	t.Cleanup(func() { symbolFTSStreamObserver = nil })

	if _, err := store.SearchSymbolsViewGenerationsRepoScopedContext(context.Background(), "matchSkipRuleRen0", nil, []int64{11, 12, 13, 14}, 5); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 0 {
		t.Fatalf("derived generations went through the unbounded stream: %v", streamed)
	}
	if _, err := store.SearchSymbolsViewGenerationsRepoScopedContext(context.Background(), "matchSkipRuleRen0", nil, []int64{0, 11}, 5); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 1 || !reflect.DeepEqual(streamed[0], []int64{0}) {
		t.Fatalf("streamed generations = %v, want only the base corpus", streamed)
	}
}
