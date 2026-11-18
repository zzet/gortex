package store_sqlite

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// seedForeignRunDocs writes n documents of one generation in one FTS write,
// so they take n consecutive rowids above every existing one. Many documents
// carry identical tokens, so the rank order is full of ties, including ties
// between documents written before and after another generation's run.
func seedForeignRunDocs(t *testing.T, store *Store, generation int64, prefix string, n int) {
	t.Helper()
	nodes := make([]*graph.Node, 0, n)
	items := make([]graph.SymbolFTSItem, 0, n)
	for i := range n {
		id := fmt.Sprintf("%s-%03d", prefix, i)
		repo := "repo"
		if i%7 == 3 {
			repo = "other"
		}
		tokens := "match skip rule"
		switch i % 5 {
		case 1:
			tokens = "match skip rule ren"
		case 2:
			tokens = fmt.Sprintf("matches other%d", i)
		case 3:
			tokens = "unrelated words only"
		}
		nodes = append(nodes, &graph.Node{ID: id, RepoPrefix: repo})
		items = append(items, graph.SymbolFTSItem{NodeID: id, Tokens: tokens})
	}
	if err := viewBatchHandle(store, generation).AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	seedViewBatchFTS(t, store, generation, items)
}

// seedEmptyNodeIDBaseDoc appends a base document with an empty node id at the
// top of the table. Its single token makes it the best match for "match", so
// a SQL LIMIT that counts it returns one hit fewer.
func seedEmptyNodeIDBaseDoc(t *testing.T, store *Store) {
	t.Helper()
	tx, err := store.writerDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var next int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(rowid), 0) + 1 FROM symbol_fts`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO symbol_fts (rowid, node_id, repo_prefix, tokens) VALUES (?, '', '', 'match')`, next); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO symbol_fts_rowid (view_gen, node_id, repo_prefix, fts_rowid) VALUES (0, '', '', ?)`, next); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// seedBaseAroundForeignRuns builds the shape incremental base writes leave
// behind: base documents, a large derived generation whose build is split by
// one more base document, a small derived generation, then base documents
// appended above every run, the last with an empty node id.
func seedBaseAroundForeignRuns(t *testing.T) *Store {
	t.Helper()
	store := viewBatchTestStore(t)
	seedForeignRunDocs(t, store, 0, "base", 30)
	seedForeignRunDocs(t, store, 7, "g7a", 100)
	seedForeignRunDocs(t, store, 0, "inside", 1)
	seedForeignRunDocs(t, store, 7, "g7b", 100)
	seedForeignRunDocs(t, store, 9, "g9", 10)
	seedForeignRunDocs(t, store, 0, "appended", 20)
	seedEmptyNodeIDBaseDoc(t, store)
	return store
}

func generationRowids(t *testing.T, store *Store, generation int64) []int64 {
	t.Helper()
	rows, err := store.db.Query(`SELECT fts_rowid FROM symbol_fts_rowid WHERE view_gen = ? ORDER BY fts_rowid`, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			t.Fatal(err)
		}
		out = append(out, rowid)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func inSymbolFTSRanges(ranges []symbolFTSRowidRange, rowid int64) bool {
	for _, r := range ranges {
		if r.lo <= rowid && rowid <= r.hi {
			return true
		}
	}
	return false
}

func TestSymbolFTSGenerationRangesCutForeignRunsAroundOwnDocuments(t *testing.T) {
	store := seedBaseAroundForeignRuns(t)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ranges, ok, err := symbolFTSGenerationRanges(context.Background(), tx, 0)
	if err != nil || !ok {
		t.Fatalf("ranges = %v ok=%v err=%v, want the plan to apply", ranges, ok, err)
	}
	// The base document written inside generation 7's build splits its run:
	// the base's documents sit before the run, at the split, and above it.
	if len(ranges) != 3 {
		t.Fatalf("ranges = %v, want three", ranges)
	}
	for _, rowid := range generationRowids(t, store, 0) {
		if !inSymbolFTSRanges(ranges, rowid) {
			t.Fatalf("base rowid %d outside ranges %v", rowid, ranges)
		}
	}
	for _, generation := range []int64{7, 9} {
		for _, rowid := range generationRowids(t, store, generation) {
			if inSymbolFTSRanges(ranges, rowid) {
				t.Fatalf("generation %d rowid %d inside ranges %v", generation, rowid, ranges)
			}
		}
	}

	if ranges, ok, err := symbolFTSGenerationRanges(context.Background(), tx, 99); err != nil || !ok || len(ranges) != 0 {
		t.Fatalf("generation without documents: ranges = %v ok=%v err=%v", ranges, ok, err)
	}
}

func TestSymbolFTSGenerationRangesKeepUnboundedPlanWhenLittleIsSkipped(t *testing.T) {
	store := viewBatchTestStore(t)
	seedForeignRunDocs(t, store, 0, "base", 200)
	seedForeignRunDocs(t, store, 7, "g7", 5)
	seedForeignRunDocs(t, store, 0, "appended", 5)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if ranges, ok, err := symbolFTSGenerationRanges(context.Background(), tx, 0); err != nil || ok {
		t.Fatalf("a run of 5 in 210 rowids: ranges = %v ok=%v err=%v, want the unbounded plan", ranges, ok, err)
	}
	// Generation 7's own search would skip nearly the whole table.
	if ranges, ok, err := symbolFTSGenerationRanges(context.Background(), tx, 7); err != nil || !ok || len(ranges) != 1 {
		t.Fatalf("generation 7: ranges = %v ok=%v err=%v, want its one run", ranges, ok, err)
	}
}

func TestSearchSymbolsBaseRangePlanMatchesTheUnboundedQuery(t *testing.T) {
	store := seedBaseAroundForeignRuns(t)
	sawShortPage := false
	for _, query := range []string{"match", "match skip rule", "matches", "rule ren", "absentName"} {
		for _, repos := range [][]string{nil, {"repo"}, {"other"}, {"repo", "other"}, {"missing"}} {
			for _, limit := range []int{1, 2, 5, 17, 40, 300} {
				name := fmt.Sprintf("%s/%v/%d", query, repos, limit)
				want := unboundedSingleGeneration(t, store, 0, query, repos, limit)
				got, handled, err := store.searchSymbolFTSRangePlan(context.Background(), store.buildFTSMatch(query, true), 0, repos, limit)
				if err != nil || !handled {
					t.Fatalf("%s: handled=%v err=%v", name, handled, err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: range plan differs\n got=%v\nwant=%v", name, got, want)
				}
				for i := range got {
					if math.Float64bits(got[i].Score) != math.Float64bits(want[i].Score) {
						t.Fatalf("%s: score bits differ at %d", name, i)
					}
				}
				public, err := store.SearchSymbolsRepoScopedContext(context.Background(), query, repos, limit)
				if err != nil || !reflect.DeepEqual(public, want) {
					t.Fatalf("%s: public search err=%v\n got=%v\nwant=%v", name, err, public, want)
				}
				if query == "match" && repos == nil && limit == 1 && len(want) == 0 {
					sawShortPage = true
				}
			}
		}
	}
	if !sawShortPage {
		t.Fatal("the empty-id document never took a slot of the SQL limit; the fixture lost its purpose")
	}
}

// TestSearchSymbolsBaseRanksOnlyItsOwnRanges pins the cost contract: a base
// search whose span holds large foreign runs ranks only the ranges around
// them, one bounded query per range, and never runs the unbounded MATCH.
func TestSearchSymbolsBaseRanksOnlyItsOwnRanges(t *testing.T) {
	store := seedBaseAroundForeignRuns(t)
	// Keep the global observers away from background reads.
	store.stopCheckpointLoop()
	store.stopMaintenanceLane()
	var (
		mu       sync.Mutex
		observed [][]symbolFTSRowidRange
		matches  []string
	)
	symbolFTSRangeQueryObserver = func(generation int64, ranges []symbolFTSRowidRange) {
		mu.Lock()
		defer mu.Unlock()
		if generation == 0 {
			observed = append(observed, append([]symbolFTSRowidRange(nil), ranges...))
		}
	}
	readStatementObserver = func(q string) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(q, "symbol_fts MATCH") {
			matches = append(matches, q)
		}
	}
	t.Cleanup(func() {
		symbolFTSRangeQueryObserver = nil
		readStatementObserver = nil
	})

	hits, err := store.SearchSymbolsContext(context.Background(), "match skip rule", 10)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	readStatementObserver = nil
	if want := unboundedSingleGeneration(t, store, 0, "match skip rule", nil, 10); !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits differ\n got=%v\nwant=%v", hits, want)
	}
	if len(observed) != 1 || len(observed[0]) != 3 {
		t.Fatalf("range plan observations = %v, want one search in three ranges", observed)
	}
	if len(matches) != 3 {
		t.Fatalf("MATCH statements = %d, want one per range:\n%s", len(matches), strings.Join(matches, "\n---\n"))
	}
	for _, q := range matches {
		if !strings.Contains(q, "symbol_fts.rowid BETWEEN ? AND ?") {
			t.Fatalf("an unbounded MATCH ran:\n%s", q)
		}
	}
}

// TestSearchSymbolsSingleGenerationSkipsRunDiscovery pins the cheap decline:
// a table holding only the searched generation's documents has no run to cut
// out, so the search runs no discovery statement and no range plan, and
// returns the unbounded query's page.
func TestSearchSymbolsSingleGenerationSkipsRunDiscovery(t *testing.T) {
	store := viewBatchTestStore(t)
	seedForeignRunDocs(t, store, 0, "base", 40)
	seedEmptyNodeIDBaseDoc(t, store)
	// Keep the global observers away from background reads.
	store.stopCheckpointLoop()
	store.stopMaintenanceLane()
	var (
		mu        sync.Mutex
		discovery int
		ranked    int
	)
	symbolFTSRangeQueryObserver = func(int64, []symbolFTSRowidRange) {
		mu.Lock()
		defer mu.Unlock()
		ranked++
	}
	readStatementObserver = func(q string) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(q, "WITH RECURSIVE gens(g)") {
			discovery++
		}
	}
	t.Cleanup(func() {
		symbolFTSRangeQueryObserver = nil
		readStatementObserver = nil
	})

	for _, limit := range []int{1, 5, 40} {
		got, handled, err := store.searchSymbolFTSRangePlan(context.Background(), store.buildFTSMatch("match skip rule", true), 0, nil, limit)
		if err != nil || handled || got != nil {
			t.Fatalf("limit %d: hits=%v handled=%v err=%v, want the plan to decline", limit, got, handled, err)
		}
		hits, err := store.SearchSymbolsContext(context.Background(), "match skip rule", limit)
		if err != nil {
			t.Fatal(err)
		}
		if want := unboundedSingleGeneration(t, store, 0, "match skip rule", nil, limit); !reflect.DeepEqual(hits, want) {
			t.Fatalf("limit %d: hits differ\n got=%v\nwant=%v", limit, hits, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if discovery != 0 || ranked != 0 {
		t.Fatalf("run discovery statements = %d, range plans = %d, want none", discovery, ranked)
	}
}

// symbolFTSPlanDetails returns the EXPLAIN QUERY PLAN lines of query.
func symbolFTSPlanDetails(t *testing.T, store *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
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
	return plan
}

func TestSymbolFTSRunsSQLSeeksOnly(t *testing.T) {
	store := seedBaseAroundForeignRuns(t)
	plan := symbolFTSPlanDetails(t, store, symbolFTSRunsSQL, int64(0), symbolFTSRunGenerations+1)
	joined := strings.Join(plan, "\n")
	// The first generation is the first entry of the index: exactly one scan,
	// which stops at one row. Every other read must be a seek.
	const firstEntryScan = "SCAN symbol_fts_rowid USING COVERING INDEX symbol_fts_rowid_by_generation"
	scans := 0
	for _, line := range plan {
		if !strings.Contains(line, "symbol_fts_rowid") || strings.Contains(line, "SEARCH") {
			continue
		}
		if !strings.Contains(line, firstEntryScan) {
			t.Fatalf("run discovery reads the sidecar without a seek:\n%s", joined)
		}
		scans++
	}
	if scans != 1 {
		t.Fatalf("run discovery scans the sidecar %d times, want only the first-entry read:\n%s", scans, joined)
	}
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("run discovery sorts:\n%s", joined)
	}

	plan = symbolFTSPlanDetails(t, store, symbolFTSOtherGenerationSQL, int64(0))
	joined = strings.Join(plan, "\n")
	seeks := 0
	for _, line := range plan {
		if !strings.Contains(line, "symbol_fts_rowid") {
			continue
		}
		if !strings.Contains(line, "SEARCH symbol_fts_rowid USING COVERING INDEX symbol_fts_rowid_by_generation") {
			t.Fatalf("the other-generation check reads the sidecar without a seek:\n%s", joined)
		}
		seeks++
	}
	if seeks != 2 {
		t.Fatalf("the other-generation check seeks %d times, want two:\n%s", seeks, joined)
	}
}
