package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func seedRepoRange(t *testing.T, s *Store, repo, prefix string, count int) {
	t.Helper()
	nodes := make([]*graph.Node, count)
	items := make([]graph.SymbolFTSItem, count)
	for i := range nodes {
		id := fmt.Sprintf("%s-%04d", prefix, i)
		nodes[i] = &graph.Node{ID: id, Name: id, Kind: graph.KindFunction, RepoPrefix: repo}
		items[i] = graph.SymbolFTSItem{NodeID: id, Tokens: "needle shared"}
	}
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.BatchUpsertSymbolFTS(items); err != nil {
		t.Fatal(err)
	}
}

func TestSymbolRepoRangeMatchesSharedScoresTiesAndLimits(t *testing.T) {
	s := viewBatchTestStore(t)
	seedRepoRange(t, s, "repo", "owned-first", 3)
	seedRepoRange(t, s, "other", "foreign-inside-range", 4)
	seedRepoRange(t, viewBatchHandle(s, 12), "repo", "foreign-generation", 2)
	seedRepoRange(t, s, "", "unowned", 2)
	seedRepoRange(t, s, "repo", "owned-last", 3)
	for _, tc := range []struct {
		query string
		limit int
	}{{"needle shared", 1}, {"needle shared", 3}, {"needle shared", 7}, {"needle shared", 100}, {"absentRareTerm", 100}} {
		want := unboundedSingleGeneration(t, s, 0, tc.query, []string{"repo"}, tc.limit)
		if tc.query == "needle shared" && tc.limit == 100 && len(want) != 8 {
			t.Fatalf("reference must retain six repo and two unowned rows, got %v", want)
		}
		if tc.query == "absentRareTerm" && len(want) != 0 {
			t.Fatalf("reference must have no matches, got %v", want)
		}
		got, handled, err := s.searchSymbolRepoSpanPlan(t.Context(), s.buildFTSMatch(tc.query, true), "repo", tc.limit)
		if err != nil || !handled || !reflect.DeepEqual(got, want) {
			t.Fatalf("query %s limit %d handled=%v err=%v got=%v want=%v", tc.query, tc.limit, handled, err, got, want)
		}
		for i := range got {
			if math.Float64bits(got[i].Score) != math.Float64bits(want[i].Score) {
				t.Fatalf("query %s limit %d score bits differ at %d", tc.query, tc.limit, i)
			}
		}
		public, err := s.SearchSymbolsRepoScopedContext(t.Context(), tc.query, []string{"repo"}, tc.limit)
		if err != nil || !reflect.DeepEqual(public, want) {
			t.Fatalf("public query %s limit %d err=%v got=%v want=%v", tc.query, tc.limit, err, public, want)
		}
	}
}

func TestSymbolRepoRangeEmptyLargeDenseAndSparsePlans(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		s := viewBatchTestStore(t)
		got, handled, err := s.searchSymbolRepoSpanPlan(t.Context(), s.buildFTSMatch("needle", true), "missing", 10)
		if err != nil || !handled || len(got) != 0 {
			t.Fatalf("got=%v handled=%v err=%v", got, handled, err)
		}
	})
	t.Run("large-dense", func(t *testing.T) {
		s := viewBatchTestStore(t)
		seedRepoRange(t, s, "repo", "overflow", 2049)
		got, handled, err := s.searchSymbolRepoSpanPlan(t.Context(), s.buildFTSMatch("needle", true), "repo", 10)
		if err != nil || !handled || len(got) != 10 {
			t.Fatalf("got=%v handled=%v err=%v", got, handled, err)
		}
		want := unboundedSingleGeneration(t, s, 0, "needle", []string{"repo"}, 10)
		public, err := s.SearchSymbolsRepoScopedContext(t.Context(), "needle", []string{"repo"}, 10)
		if err != nil || !reflect.DeepEqual(public, want) || !reflect.DeepEqual(got, want) {
			t.Fatalf("public err=%v got=%v want=%v", err, public, want)
		}
	})
	t.Run("missing-repo-retains-unowned", func(t *testing.T) {
		s := viewBatchTestStore(t)
		seedRepoRange(t, s, "", "unowned-only", 3)
		seedRepoRange(t, s, "foreign", "excluded", 2)
		want := unboundedSingleGeneration(t, s, 0, "needle", []string{"missing"}, 10)
		got, handled, err := s.searchSymbolRepoSpanPlan(t.Context(), s.buildFTSMatch("needle", true), "missing", 10)
		if err != nil || !handled || len(got) != 3 || !reflect.DeepEqual(got, want) {
			t.Fatalf("got=%v want=%v handled=%v err=%v", got, want, handled, err)
		}
	})
	t.Run("sparse", func(t *testing.T) {
		s := viewBatchTestStore(t)
		seedRepoRange(t, s, "repo", "sparse", 2)
		// A valid but scattered base ownership bucket: the last document has
		// moved outside the dense-run heuristic, with its sidecar in lockstep.
		tx, err := s.writerDB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err = tx.Exec(`UPDATE symbol_fts SET rowid = 10000 WHERE node_id = ?`, "sparse-0001"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`UPDATE symbol_fts_rowid SET fts_rowid = 10000 WHERE view_gen = 0 AND node_id = ?`, "sparse-0001"); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		got, handled, err := s.searchSymbolRepoSpanPlan(t.Context(), s.buildFTSMatch("needle", true), "repo", 10)
		if err != nil || handled || got != nil {
			t.Fatalf("got=%v handled=%v err=%v", got, handled, err)
		}
		want := unboundedSingleGeneration(t, s, 0, "needle", []string{"repo"}, 10)
		public, err := s.SearchSymbolsRepoScopedContext(t.Context(), "needle", []string{"repo"}, 10)
		if err != nil || !reflect.DeepEqual(public, want) {
			t.Fatalf("fallback err=%v got=%v want=%v", err, public, want)
		}
	})
}

func TestSymbolRepoRangePinsCensusAndRankingAcrossWriterCommit(t *testing.T) {
	s := viewBatchTestStore(t)
	seedRepoRange(t, s, "repo", "old", 2)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	before := unboundedSingleGeneration(t, s, 0, "needle", []string{"repo"}, 100)
	got, handled, err := s.searchSymbolRepoSpanSnapshot(ctx, s.buildFTSMatch("needle", true), "repo", 100, func(tx *sql.Tx) error {
		var oldMax int64
		if err := tx.QueryRow(`SELECT MAX(fts_rowid) FROM symbol_fts_rowid WHERE view_gen = 0`).Scan(&oldMax); err != nil {
			return err
		}
		committed := make(chan error, 1)
		go func() {
			if err := s.AddBatchChecked([]*graph.Node{{ID: "new", Name: "new", Kind: graph.KindFunction, RepoPrefix: "repo"}}, nil); err != nil {
				committed <- err
				return
			}
			committed <- s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "new", Tokens: "needle shared"}})
		}()
		select {
		case err := <-committed:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		var newRow int64
		if err := s.db.QueryRow(`SELECT fts_rowid FROM symbol_fts_rowid WHERE view_gen = 0 AND node_id = 'new'`).Scan(&newRow); err != nil {
			return err
		}
		if newRow <= oldMax {
			return fmt.Errorf("new row %d did not exceed old bound %d", newRow, oldMax)
		}
		return nil
	})
	if err != nil || !handled || !reflect.DeepEqual(got, before) {
		t.Fatalf("pinned snapshot handled=%v err=%v got=%v want=%v", handled, err, got, before)
	}
	want := unboundedSingleGeneration(t, s, 0, "needle", []string{"repo"}, 100)
	next, err := s.SearchSymbolsRepoScopedContext(t.Context(), "needle", []string{"repo"}, 100)
	if err != nil || len(next) != 3 || !reflect.DeepEqual(next, want) {
		t.Fatalf("next snapshot err=%v got=%v want=%v", err, next, want)
	}
}

func TestSymbolRepoRangeCancellationIsAuthoritative(t *testing.T) {
	s := viewBatchTestStore(t)
	seedRepoRange(t, s, "repo", "cancel", 1)
	ctx, cancel := context.WithCancel(t.Context())
	_, handled, err := s.searchSymbolRepoSpanSnapshot(ctx, s.buildFTSMatch("needle", true), "repo", 10, func(*sql.Tx) error {
		cancel()
		return nil
	})
	if !handled || !errors.Is(err, context.Canceled) {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
}
