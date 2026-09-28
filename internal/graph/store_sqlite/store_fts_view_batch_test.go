package store_sqlite

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func viewBatchTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir() + "/graph.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func viewBatchHandle(store *Store, generation int64) *Store {
	handle := *store
	handle.viewGen = generation
	return &handle
}

func seedViewBatchFTS(t *testing.T, store *Store, generation int64, items []graph.SymbolFTSItem) {
	t.Helper()
	if err := viewBatchHandle(store, generation).BatchUpsertSymbolFTS(items); err != nil {
		t.Fatal(err)
	}
}

func TestSearchSymbolsViewGenerationsMatchesIndividualPages(t *testing.T) {
	store := viewBatchTestStore(t)
	if err := viewBatchHandle(store, 0).AddBatchChecked([]*graph.Node{
		{ID: "base-a", RepoPrefix: "repo"},
		{ID: "base-b", RepoPrefix: "repo"},
		{ID: "base-other", RepoPrefix: "other"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := viewBatchHandle(store, 869).AddBatchChecked([]*graph.Node{
		{ID: "derived-a", RepoPrefix: "repo"},
		{ID: "derived-b", RepoPrefix: "repo"},
		{ID: "derived-other", RepoPrefix: "other"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	seedViewBatchFTS(t, store, 0, []graph.SymbolFTSItem{
		{NodeID: "base-a", Tokens: "alpha shared"},
		{NodeID: "base-b", Tokens: "alpha shared"},
		{NodeID: "base-other", Tokens: "alpha shared"},
	})
	seedViewBatchFTS(t, store, 869, []graph.SymbolFTSItem{
		{NodeID: "derived-a", Tokens: "alpha shared"},
		{NodeID: "derived-b", Tokens: "alpha shared"},
		{NodeID: "derived-other", Tokens: "alpha shared"},
	})

	for _, tc := range []struct {
		name  string
		repos []string
		limit int
	}{
		{name: "unscoped", limit: 2},
		{name: "repo", repos: []string{"repo"}, limit: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantBase, err := viewBatchHandle(store, 0).SearchSymbolsRepoScopedContext(context.Background(), "alpha shared", tc.repos, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			wantDerived, err := viewBatchHandle(store, 869).SearchSymbolsRepoScopedContext(context.Background(), "alpha shared", tc.repos, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.SearchSymbolsViewGenerationsRepoScopedContext(
				context.Background(), "alpha shared", tc.repos, []int64{0, 869, 869, 777}, tc.limit,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got[0], wantBase) {
				t.Fatalf("base page differs\n got=%#v\nwant=%#v", got[0], wantBase)
			}
			if !reflect.DeepEqual(got[869], wantDerived) {
				t.Fatalf("derived page differs\n got=%#v\nwant=%#v", got[869], wantDerived)
			}
			if empty, present := got[777]; !present || empty != nil {
				t.Fatalf("empty generation = %#v, present=%v", empty, present)
			}
			if len(got) != 3 {
				t.Fatalf("duplicate generation created extra output: %#v", got)
			}
		})
	}
}

func TestSearchSymbolsViewGenerationsPreservesPerGenerationExactTier(t *testing.T) {
	store := viewBatchTestStore(t)
	base := viewBatchHandle(store, 0)
	derived := viewBatchHandle(store, 869)
	if err := base.AddBatchChecked([]*graph.Node{
		{ID: "base-exact", Name: "Needle", Kind: graph.KindFunction, RepoPrefix: "repo"},
		{ID: "base-fts", RepoPrefix: "repo"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := derived.AddBatchChecked([]*graph.Node{{ID: "derived-fts", RepoPrefix: "repo"}}, nil); err != nil {
		t.Fatal(err)
	}
	seedViewBatchFTS(t, store, 0, []graph.SymbolFTSItem{{NodeID: "base-fts", Tokens: "needle"}})
	seedViewBatchFTS(t, store, 869, []graph.SymbolFTSItem{{NodeID: "derived-fts", Tokens: "needle"}})

	wantBase, err := base.SearchSymbolsRepoScopedContext(context.Background(), "Needle", []string{"repo"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantDerived, err := derived.SearchSymbolsRepoScopedContext(context.Background(), "Needle", []string{"repo"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.SearchSymbolsViewGenerationsRepoScopedContext(
		context.Background(), "Needle", []string{"repo"}, []int64{0, 869}, 3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[0], wantBase) || !reflect.DeepEqual(got[869], wantDerived) {
		t.Fatalf("exact-tier parity differs\n got=%#v\nbase=%#v\nderived=%#v", got, wantBase, wantDerived)
	}
}

func TestSearchSymbolsViewGenerationsPlanUsesOneRankStream(t *testing.T) {
	store := viewBatchTestStore(t)
	match := store.buildFTSMatch("alpha shared", true)
	rows, err := store.db.Query(
		"EXPLAIN QUERY PLAN "+symbolFTSViewBatchQuery(2, 1),
		match, int64(0), int64(869), "repo",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	if strings.Count(plan, "SCAN symbol_fts VIRTUAL TABLE") != 1 {
		t.Fatalf("expected one FTS stream:\n%s", plan)
	}
	if !strings.Contains(plan, "symbol_fts_rowid_by_rowid") {
		t.Fatalf("missing rowid sidecar point lookup:\n%s", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("rank stream spilled to external order:\n%s", plan)
	}
}

func TestSearchSymbolsViewGenerationsCancellationReturnsNoPartial(t *testing.T) {
	store := viewBatchTestStore(t)
	seedViewBatchFTS(t, store, 0, []graph.SymbolFTSItem{{NodeID: "base", Tokens: "alpha shared"}})
	store.db.SetMaxOpenConns(1)
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	type answer struct {
		hits map[int64][]graph.SymbolHit
		err  error
	}
	done := make(chan answer, 1)
	waitsBefore := store.db.Stats().WaitCount
	go func() {
		hits, err := store.SearchSymbolsViewGenerationsRepoScopedContext(ctx, "alpha shared", nil, []int64{0}, 1)
		done <- answer{hits: hits, err: err}
	}()
	deadline := time.Now().Add(time.Second)
	for store.db.Stats().WaitCount == waitsBefore && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if store.db.Stats().WaitCount == waitsBefore {
		cancel()
		t.Fatal("query did not block in reader pool")
	}
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) || got.hits != nil {
		t.Fatalf("canceled batch returned hits=%#v err=%v", got.hits, got.err)
	}
}

func TestSearchSymbolsViewGenerationsEarlyCloseReusesConnection(t *testing.T) {
	store := viewBatchTestStore(t)
	seedViewBatchFTS(t, store, 0, []graph.SymbolFTSItem{
		{NodeID: "a", Tokens: "alpha shared"},
		{NodeID: "b", Tokens: "alpha shared"},
	})
	store.db.SetMaxOpenConns(1)
	if _, err := store.SearchSymbolsViewGenerationsRepoScopedContext(
		context.Background(), "alpha shared", nil, []int64{0}, 1,
	); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := store.SearchSymbolsRepoScopedContext(ctx, "alpha shared", nil, 1); err != nil {
		t.Fatalf("reader connection was not reusable after early close: %v", err)
	}
}
