package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func scatterRepoMembershipRow(t *testing.T, s *Store, id string) {
	t.Helper()
	tx, err := s.writerDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`UPDATE symbol_fts SET rowid = 20000 WHERE node_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE symbol_fts_rowid SET fts_rowid = 20000 WHERE view_gen = ? AND node_id = ?`, s.viewGen, id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSymbolRepoSparseMembershipScoresTiesAndScope(t *testing.T) {
	s := viewBatchTestStore(t)
	seedRepoRange(t, s, "repo", "owned", 7)
	seedRepoRange(t, s, "", "unowned", 3)
	seedRepoRange(t, s, "foreign", "foreign", 11)
	seedRepoRange(t, viewBatchHandle(s, 12), "repo", "other-generation", 4)
	scatterRepoMembershipRow(t, s, "owned-0006")
	for _, tc := range []struct {
		repo, query string
		limit       int
	}{{"repo", "needle shared", 1}, {"repo", "needle shared", 4}, {"repo", "needle shared", 9}, {"repo", "needle shared", 100}, {"repo", "absentRareTerm", 100}, {"missing", "needle shared", 100}, {"", "needle shared", 100}} {
		want := unboundedSingleGeneration(t, s, 0, tc.query, []string{tc.repo}, tc.limit)
		got, handled, err := s.searchSymbolRepoSpanPlan(t.Context(), s.buildFTSMatch(tc.query, true), tc.repo, tc.limit)
		if err != nil || !handled || !reflect.DeepEqual(got, want) {
			t.Fatalf("repo=%q query=%s limit=%d handled=%v err=%v got=%v want=%v", tc.repo, tc.query, tc.limit, handled, err, got, want)
		}
		for i := range got {
			if math.Float64bits(got[i].Score) != math.Float64bits(want[i].Score) {
				t.Fatalf("raw score bits differ at %d", i)
			}
		}
		public, err := s.SearchSymbolsRepoScopedContext(t.Context(), tc.query, []string{tc.repo}, tc.limit)
		if err != nil || !reflect.DeepEqual(public, want) {
			t.Fatalf("public err=%v got=%v want=%v", err, public, want)
		}
	}
	// Broader scopes retain their existing public plans and shared ranking.
	for _, repos := range [][]string{nil, {"repo", "foreign"}} {
		want := unboundedSingleGeneration(t, s, 0, "needle shared", repos, 100)
		got, err := s.SearchSymbolsRepoScopedContext(t.Context(), "needle shared", repos, 100)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("repos=%v err=%v got=%v want=%v", repos, err, got, want)
		}
	}
	positive := viewBatchHandle(s, 12)
	want := unboundedSingleGeneration(t, s, 12, "needle shared", []string{"repo"}, 100)
	got, err := positive.SearchSymbolsRepoScopedContext(t.Context(), "needle shared", []string{"repo"}, 100)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("positive generation err=%v got=%v want=%v", err, got, want)
	}
}

func TestSymbolRepoSparseMembershipAdmissionAndSingleCursor(t *testing.T) {
	for _, count := range []int{1024, 1025} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s := viewBatchTestStore(t)
			seedRepoRange(t, s, "repo", "owned", count-1)
			seedRepoRange(t, s, "", "unowned", 1)
			scatterRepoMembershipRow(t, s, fmt.Sprintf("owned-%04d", count-2))
			match := s.buildFTSMatch("needle", true)
			got, handled, err := s.searchSymbolRepoSpanPlan(t.Context(), match, "repo", 5)
			if err != nil || handled != (count == 1024) || (handled && len(got) != 5) || (!handled && got != nil) {
				t.Fatalf("count=%d handled=%v err=%v got=%v", count, handled, err, got)
			}
			want := unboundedSingleGeneration(t, s, 0, "needle", []string{"repo"}, 5)
			public, err := s.SearchSymbolsRepoScopedContext(t.Context(), "needle", []string{"repo"}, 5)
			if err != nil || !reflect.DeepEqual(public, want) {
				t.Fatalf("overflow fallback err=%v got=%v want=%v", err, public, want)
			}
			if count != 1024 {
				return
			}
			tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			ids, complete, err := symbolRepoMembershipIDs(t.Context(), tx, 0, "repo")
			if err != nil || !complete || len(ids) != count {
				t.Fatalf("census complete=%v err=%v ids=%d", complete, err, len(ids))
			}
			args := []any{0, match}
			for _, id := range ids {
				args = append(args, id)
			}
			args = append(args, "repo", 5)
			query := symbolRepoMembershipQuery(len(ids))
			rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				t.Fatal(err)
			}
			var ftsCursors, externalSort int
			for rows.Next() {
				var a, b, c int
				var detail string
				if err = rows.Scan(&a, &b, &c, &detail); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(detail, "symbol_fts VIRTUAL TABLE INDEX") {
					ftsCursors++
					if detail != "SCAN symbol_fts VIRTUAL TABLE INDEX 0:rM3" {
						t.Fatalf("unexpected point/rank cursor: %s", detail)
					}
				}
				if detail == "USE TEMP B-TREE FOR ORDER BY" {
					externalSort++
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil || ftsCursors != 1 || externalSort != 1 {
				t.Fatalf("cursor=%d sort=%d err=%v", ftsCursors, externalSort, err)
			}
		})
	}
	t.Run("sparse-positive-declines", func(t *testing.T) {
		s := viewBatchTestStore(t)
		positive := viewBatchHandle(s, 12)
		seedRepoRange(t, positive, "repo", "positive", 2)
		scatterRepoMembershipRow(t, positive, "positive-0001")
		got, handled, err := positive.searchSymbolRepoSpanPlan(t.Context(), positive.buildFTSMatch("needle", true), "repo", 10)
		if err != nil || handled || got != nil {
			t.Fatalf("positive sparse door handled=%v err=%v got=%v", handled, err, got)
		}
		want := unboundedSingleGeneration(t, s, 12, "needle", []string{"repo"}, 10)
		public, err := positive.SearchSymbolsRepoScopedContext(t.Context(), "needle", []string{"repo"}, 10)
		if err != nil || !reflect.DeepEqual(public, want) {
			t.Fatalf("positive public err=%v got=%v want=%v", err, public, want)
		}
	})
}

func TestSymbolRepoSparseMembershipPinsSnapshotAndErrors(t *testing.T) {
	t.Run("writer", func(t *testing.T) {
		s := viewBatchTestStore(t)
		seedRepoRange(t, s, "repo", "old", 3)
		scatterRepoMembershipRow(t, s, "old-0002")
		before := unboundedSingleGeneration(t, s, 0, "needle", []string{"repo"}, 100)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		got, handled, err := s.searchSymbolRepoSpanSnapshot(ctx, s.buildFTSMatch("needle", true), "repo", 100, func(*sql.Tx) error {
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
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil || !handled || !reflect.DeepEqual(got, before) {
			t.Fatalf("snapshot handled=%v err=%v got=%v want=%v", handled, err, got, before)
		}
		want := unboundedSingleGeneration(t, s, 0, "needle", []string{"repo"}, 100)
		next, err := s.SearchSymbolsRepoScopedContext(t.Context(), "needle", []string{"repo"}, 100)
		if err != nil || len(next) != 4 || !reflect.DeepEqual(next, want) {
			t.Fatalf("next snapshot err=%v got=%v want=%v", err, next, want)
		}
	})
	t.Run("cancel-after-census", func(t *testing.T) {
		s := viewBatchTestStore(t)
		seedRepoRange(t, s, "repo", "cancel", 2)
		scatterRepoMembershipRow(t, s, "cancel-0001")
		ctx, cancel := context.WithCancel(t.Context())
		got, handled, err := s.searchSymbolRepoSpanSnapshot(ctx, s.buildFTSMatch("needle", true), "repo", 10, func(*sql.Tx) error { cancel(); return nil })
		if !handled || !errors.Is(err, context.Canceled) || got != nil {
			t.Fatalf("handled=%v err=%v got=%v", handled, err, got)
		}
	})
	t.Run("closed-census", func(t *testing.T) {
		s := viewBatchTestStore(t)
		tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		ids, complete, err := symbolRepoMembershipIDs(t.Context(), tx, 0, "repo")
		if !errors.Is(err, sql.ErrTxDone) || ids != nil || complete {
			t.Fatalf("ids=%v complete=%v err=%v", ids, complete, err)
		}
	})
}
