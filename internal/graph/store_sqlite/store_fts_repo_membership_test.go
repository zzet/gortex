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

func assertSparseMembershipCursor(t *testing.T, tx *sql.Tx, query string, args []any) {
	t.Helper()
	rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The CROSS JOIN must keep one unranked MATCH cursor outermost, even
	// after ANALYZE. Ownership remains an indexed seek for each admitted row.
	if len(details) != 3 || details[0] != "SCAN symbol_fts VIRTUAL TABLE INDEX 0:rM3" ||
		!strings.HasPrefix(details[1], "SEARCH symbol_fts_rowid USING COVERING INDEX ") ||
		!strings.Contains(details[1], "view_gen=?") || !strings.Contains(details[1], "fts_rowid=?") ||
		details[2] != "USE TEMP B-TREE FOR ORDER BY" {
		t.Fatalf("unexpected MATCH/ownership/sort plan: %v", details)
	}

	rows, err = tx.QueryContext(t.Context(), "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	type instruction struct {
		addr, p1, p2, p3, p5 int
		op                   string
		p4, comment          sql.NullString
	}
	var ops []instruction
	for rows.Next() {
		var op instruction
		if err = rows.Scan(&op.addr, &op.op, &op.p1, &op.p2, &op.p3, &op.p4, &op.p5, &op.comment); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		ops = append(ops, op)
		if len(ops) > 4096 {
			_ = rows.Close()
			t.Fatal("unexpectedly unbounded membership bytecode")
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	ftsCursor, membershipCursor := -1, -1
	filterAddr, nextAddr, nextTarget, filters := -1, -1, -1, 0
	for _, op := range ops {
		if op.op == "VFilter" {
			filters++
			ftsCursor, filterAddr = op.p1, op.addr
			if op.p4.String != "rM3" {
				t.Fatalf("unexpected MATCH constraint: %+v", op)
			}
		}
		if op.op == "OpenEphemeral" && op.p4.String == "k(1,)" {
			membershipCursor = op.p1
		}
	}
	firstMetadata, membershipAddr, membershipMiss, decisionAddr, decisionSkip := len(ops), -1, -1, -1, -1
	rowidAddr := -1
	for _, op := range ops {
		if op.op == "VNext" && op.p1 == ftsCursor {
			nextAddr, nextTarget = op.addr, op.p2
		}
		if op.op == "Rowid" && op.p1 == ftsCursor && rowidAddr < 0 {
			rowidAddr = op.addr
		}
		if op.op == "NotFound" && op.p1 == membershipCursor {
			membershipAddr, membershipMiss = op.addr, op.p2
		}
		if (op.op == "VColumn" && op.p1 == ftsCursor) || op.op == "SeekGE" {
			firstMetadata = min(firstMetadata, op.addr)
		}
	}
	falseRegister, decisionRegister := -1, -1
	for _, op := range ops {
		if op.addr == membershipMiss && op.op == "Integer" && op.p1 == 0 {
			falseRegister = op.p2
		}
		if op.op == "Ne" && op.addr >= membershipMiss && op.addr < firstMetadata {
			decisionAddr, decisionSkip = op.addr, op.p2
			decisionRegister = op.p3
		}
	}
	// Assert actual execution structure, not SQL spelling: one MATCH setup;
	// rejected membership takes CASE's false arm and skips both FTS content
	// and ownership metadata to VNext, which never replays VFilter.
	if filters != 1 || membershipCursor < 0 || nextTarget <= filterAddr ||
		rowidAddr <= filterAddr || membershipAddr <= rowidAddr ||
		membershipMiss <= membershipAddr || decisionAddr < membershipMiss ||
		decisionAddr >= firstMetadata || decisionSkip != nextAddr ||
		falseRegister < 0 || decisionRegister != falseRegister {
		t.Fatalf("membership is not before metadata in one MATCH loop: filters=%d rowid=%d membership=%d miss=%d decision=%d skip=%d metadata=%d filter=%d next=%d target=%d", filters, rowidAddr, membershipAddr, membershipMiss, decisionAddr, decisionSkip, firstMetadata, filterAddr, nextAddr, nextTarget)
	}
}

func TestSymbolRepoSparseMembershipScoresTiesAndScope(t *testing.T) {
	s := viewBatchTestStore(t)
	seedRepoRange(t, s, "repo", "owned", 7)
	seedRepoRange(t, s, "", "unowned", 3)
	seedRepoRange(t, s, "foreign", "foreign", 11)
	seedRepoRange(t, viewBatchHandle(s, 12), "repo", "other-generation", 4)
	// Different TF/lengths exercise shared BM25, including a cutoff through
	// equal scores. Strong foreign/generation matches must affect statistics,
	// but cannot enter this scope's results.
	if err := s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{
		{NodeID: "owned-0000", Tokens: "needle needle needle shared"},
		{NodeID: "owned-0001", Tokens: "needle needle needle shared"},
		{NodeID: "owned-0002", Tokens: "needle shared " + strings.Repeat("padding ", 40)},
		{NodeID: "foreign-0000", Tokens: strings.Repeat("needle ", 20) + "shared"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := viewBatchHandle(s, 12).BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "other-generation-0000", Tokens: strings.Repeat("needle ", 30) + "shared"}}); err != nil {
		t.Fatal(err)
	}
	scatterRepoMembershipRow(t, s, "owned-0006")
	unequal := unboundedSingleGeneration(t, s, 0, "needle shared", []string{"repo"}, 100)
	if len(unequal) != 10 || unequal[0].Score == unequal[len(unequal)-1].Score {
		t.Fatalf("heterogeneous rank fixture is inactive: %v", unequal)
	}
	tieCutoff := 0
	for i := 0; i+1 < len(unequal); i++ {
		if unequal[i].NodeID == "owned-0000" && unequal[i+1].NodeID == "owned-0001" &&
			math.Float64bits(unequal[i].Score) == math.Float64bits(unequal[i+1].Score) {
			tieCutoff = i + 1
		}
	}
	if tieCutoff == 0 {
		t.Fatalf("equal-score rowid tie fixture is inactive: %v", unequal)
	}
	if _, err := s.writerDB.Exec(`INSERT INTO symbol_fts(symbol_fts, rank) VALUES('rank', 'bm25(0.0, 0.0, 0.0)')`); err != nil {
		t.Fatal(err)
	}
	var customRank, explicitBM25 float64
	if err := s.db.QueryRow(`SELECT rank, bm25(symbol_fts) FROM symbol_fts WHERE symbol_fts MATCH 'needle' LIMIT 1`).Scan(&customRank, &explicitBM25); err != nil {
		t.Fatal(err)
	}
	if customRank == explicitBM25 {
		t.Fatalf("persistent custom rank fixture is inactive: rank=%g bm25=%g", customRank, explicitBM25)
	}
	if after := unboundedSingleGeneration(t, s, 0, "needle shared", []string{"repo"}, 100); !reflect.DeepEqual(after, unequal) {
		t.Fatalf("explicit BM25 changed under persistent custom rank: got=%v want=%v", after, unequal)
	}
	for _, tc := range []struct {
		repo, query string
		limit       int
	}{{"repo", "needle shared", 1}, {"repo", "needle shared", tieCutoff}, {"repo", "needle shared", 4}, {"repo", "needle shared", 9}, {"repo", "needle shared", 100}, {"repo", "absentRareTerm", 100}, {"missing", "needle shared", 100}, {"", "needle shared", 100}} {
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
			assertSparseMembershipCursor(t, tx, query, args)
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if _, err = s.writerDB.Exec(`ANALYZE`); err != nil {
				t.Fatal(err)
			}
			tx, err = s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			assertSparseMembershipCursor(t, tx, query, args)
		})
	}
	t.Run("missing-without-unowned", func(t *testing.T) {
		s := viewBatchTestStore(t)
		seedRepoRange(t, s, "foreign", "foreign", 3)
		for _, repo := range []string{"missing", ""} {
			want := unboundedSingleGeneration(t, s, 0, "needle", []string{repo}, 10)
			got, handled, err := s.searchSymbolRepoSpanPlan(t.Context(), s.buildFTSMatch("needle", true), repo, 10)
			if err != nil || !handled || got != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("repo=%q handled=%v err=%v got=%v want=%v", repo, handled, err, got, want)
			}
			public, err := s.SearchSymbolsRepoScopedContext(t.Context(), "needle", []string{repo}, 10)
			if err != nil || !reflect.DeepEqual(public, want) {
				t.Fatalf("empty public repo=%q err=%v got=%v want=%v", repo, err, public, want)
			}
		}
	})
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
