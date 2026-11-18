package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func repoMemoEntries(s *Store) map[string]*symbolFTSPageEntry {
	s.baseFTSPages.mu.Lock()
	defer s.baseFTSPages.mu.Unlock()
	out := make(map[string]*symbolFTSPageEntry, len(s.baseFTSPages.entries))
	for key, entry := range s.baseFTSPages.entries {
		out[key] = entry
	}
	return out
}

func repoMemoSearch(t *testing.T, s *Store, query string, limit int) []graph.SymbolHit {
	t.Helper()
	got, handled, err := s.searchSymbolRepoSpanPlan(t.Context(), s.buildFTSMatch(query, true), "repo", limit)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	want := unboundedSingleGeneration(t, s, 0, query, []string{"repo"}, limit)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range got {
		if math.Float64bits(got[i].Score) != math.Float64bits(want[i].Score) {
			t.Fatalf("score bits differ at %d", i)
		}
	}
	return got
}

func TestSymbolRepoMemoNativeRankAndRefills(t *testing.T) {
	s := viewBatchTestStore(t)
	seedRepoRange(t, s, "repo", "owned", 8)
	seedRepoRange(t, s, "", "unowned", 2)
	seedRepoRange(t, s, "foreign", "foreign", 3)
	seedRepoRange(t, viewBatchHandle(s, 12), "repo", "derived", 2)
	// Exercise the handled sparse-membership plan as well as the dense plans
	// used below. Move document and owner together without changing its rank.
	tx, err := s.writerDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`UPDATE symbol_fts SET rowid=10000 WHERE node_id='owned-0007'`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE symbol_fts_rowid SET fts_rowid=10000 WHERE view_gen=0 AND node_id='owned-0007'`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureRowCounters(t.Context()); err != nil {
		t.Fatal(err)
	}
	repoMemoSearch(t, s, "needle", 3)
	first := repoMemoEntries(s)
	if len(first) != 1 {
		t.Fatalf("expected one real ranked page: %v", first)
	}
	repoMemoSearch(t, s, "needle", 3)
	for key, entry := range first {
		if repoMemoEntries(s)[key] != entry {
			t.Fatal("identical snapshot did not reuse ranked page")
		}
	}
	repoMemoSearch(t, s, "needle", 9)
	for key, entry := range first {
		if next := repoMemoEntries(s)[key]; next == entry || len(next.rows) != 9 || next.complete {
			t.Fatal("larger SQL limit must refill incomplete raw page")
		}
	}
	repoMemoSearch(t, s, "needle", 100)
	complete := repoMemoEntries(s)
	repoMemoSearch(t, s, "needle", 100)
	for key, entry := range complete {
		if !entry.complete || repoMemoEntries(s)[key] != entry {
			t.Fatal("complete page was not reused")
		}
	}
}

func TestSymbolRepoMemoGlobalAndOwnershipInvalidation(t *testing.T) {
	s := viewBatchTestStore(t)
	seedRepoRange(t, s, "repo", "owned", 3)
	if err := s.EnsureRowCounters(t.Context()); err != nil {
		t.Fatal(err)
	}
	repoMemoSearch(t, s, "needle", 20)
	before := repoMemoEntries(s)
	// Metadata/token no-ops keep the exact physical scoring and ownership cut.
	if err := s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "owned-0000", Tokens: "needle shared"}}); err != nil {
		t.Fatal(err)
	}
	repoMemoSearch(t, s, "needle", 20)
	for key, entry := range before {
		if repoMemoEntries(s)[key] != entry {
			t.Fatal("identical FTS document invalidated ranked rows")
		}
	}
	foreign := viewBatchHandle(s, 17)
	seedRepoRange(t, foreign, "foreign", "foreign", 4)
	repoMemoSearch(t, s, "needle", 20)
	if len(repoMemoEntries(s)) != len(before)+1 {
		t.Fatal("global FTS write reused old BM25 snapshot")
	}
	if err := foreign.BatchDeleteSymbolFTS([]string{"foreign-0000"}); err != nil {
		t.Fatal(err)
	}
	repoMemoSearch(t, s, "needle", 20)
	if len(repoMemoEntries(s)) != len(before)+2 {
		t.Fatal("foreign document retirement reused old BM25 snapshot")
	}
	if err := s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "owned-0000", Tokens: "needle needle changed"}}); err != nil {
		t.Fatal(err)
	}
	repoMemoSearch(t, s, "needle", 20)
	prior := repoMemoEntries(s)
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	statsBefore, err := symbolFTSStatsTx(t.Context(), tx)
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.Exec(`UPDATE symbol_fts_rowid SET view_gen = 99 WHERE view_gen = 0 AND node_id = 'owned-0001'`); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	statsAfter, err := symbolFTSStatsTx(t.Context(), tx)
	_ = tx.Rollback()
	if err != nil || statsBefore != statsAfter {
		t.Fatalf("ownership-only move changed FTS blocks: %v", err)
	}
	if got := repoMemoSearch(t, s, "needle", 20); len(got) != 2 {
		t.Fatalf("moved owner still returned: %v", got)
	}
	if len(repoMemoEntries(s)) != len(prior)+1 {
		t.Fatal("ownership-only revision did not invalidate")
	}
}

func TestSymbolRepoMemoMissingAuthorityAndErrorsBypass(t *testing.T) {
	for _, state := range []string{"unready", "unseeded", "revision", "trigger"} {
		t.Run(state, func(t *testing.T) {
			s := viewBatchTestStore(t)
			seedRepoRange(t, s, "repo", "owned", 2)
			if err := s.EnsureRowCounters(t.Context()); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "unready":
				s.rowCountersReady.Store(false)
			case "unseeded":
				if _, err := s.writerDB.Exec(`UPDATE generation_row_counts_state SET seeded=0`); err != nil {
					t.Fatal(err)
				}
			case "revision":
				if _, err := s.writerDB.Exec(`DELETE FROM symbol_fts_generation_revisions WHERE view_gen=0`); err != nil {
					t.Fatal(err)
				}
			case "trigger":
				if _, err := s.writerDB.Exec(`DROP TRIGGER symbol_fts_generation_revisions_update`); err != nil {
					t.Fatal(err)
				}
			}
			if got := repoMemoSearch(t, s, "needle", 20); len(got) != 2 {
				t.Fatal(got)
			}
			if len(repoMemoEntries(s)) != 0 {
				t.Fatal("unknown authority memoized")
			}
		})
	}
	s := viewBatchTestStore(t)
	seedRepoRange(t, s, "repo", "owned", 2)
	if err := s.EnsureRowCounters(t.Context()); err != nil {
		t.Fatal(err)
	}
	repoMemoSearch(t, s, "needle", 20)
	before := repoMemoEntries(s)
	ctx, cancel := context.WithCancel(t.Context())
	_, handled, err := s.searchSymbolRepoSpanSnapshot(ctx, s.buildFTSMatch("needle", true), "repo", 20, func(*sql.Tx) error { cancel(); return nil })
	if !handled || !errors.Is(err, context.Canceled) {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	_, _, err = s.searchSymbolRepoSpanPlan(t.Context(), `"`, "repo", 20)
	if err == nil {
		t.Fatal("malformed MATCH must fail")
	}
	if !reflect.DeepEqual(before, repoMemoEntries(s)) {
		t.Fatal("canceled/failed cursor changed memo")
	}
}

func TestSymbolRepoMemoEmptyIDsLimitsAndBound(t *testing.T) {
	s := viewBatchTestStore(t)
	// A SQL LIMIT consumes this row even though response hydration skips its ID.
	if _, err := s.writerDB.Exec(`INSERT INTO symbol_fts(node_id, tokens, repo_prefix) VALUES ('', 'needle shared', 'repo')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.Exec(`INSERT INTO symbol_fts_rowid(view_gen,node_id,fts_rowid,repo_prefix) SELECT 0,'',rowid,'repo' FROM symbol_fts WHERE node_id=''`); err != nil {
		t.Fatal(err)
	}
	seedRepoRange(t, s, "repo", "owned", symbolFTSPageCacheRows+1)
	if err := s.EnsureRowCounters(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := repoMemoSearch(t, s, "needle", 1); len(got) != 0 {
		t.Fatalf("empty ID did not consume SQL limit: %v", got)
	}
	repoMemoSearch(t, s, "needle", 1)
	if got := repoMemoSearch(t, s, "needle", 2); len(got) != 1 {
		t.Fatal(got)
	}
	before := repoMemoEntries(s)
	if got := repoMemoSearch(t, s, "needle", symbolFTSPageCacheRows+2); len(got) != symbolFTSPageCacheRows+1 {
		t.Fatal(len(got))
	}
	if !reflect.DeepEqual(before, repoMemoEntries(s)) {
		t.Fatal("oversize request changed bounded memo")
	}
	for i := 0; i < symbolFTSPageCacheEntries+1; i++ {
		repoMemoSearch(t, s, fmt.Sprintf("absent%d", i), 20)
	}
	if len(repoMemoEntries(s)) != symbolFTSPageCacheEntries {
		t.Fatal("entry bound not retained")
	}
	for _, entry := range repoMemoEntries(s) {
		if len(entry.rows) > symbolFTSPageCacheRows {
			t.Fatal("row bound exceeded")
		}
	}
}

func TestSymbolRepoMemoPinsAuthorityAndPageTogether(t *testing.T) {
	s := viewBatchTestStore(t)
	seedRepoRange(t, s, "repo", "old", 2)
	if err := s.EnsureRowCounters(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := unboundedSingleGeneration(t, s, 0, "needle", []string{"repo"}, 20)
	got, handled, err := s.searchSymbolRepoSpanSnapshot(t.Context(), s.buildFTSMatch("needle", true), "repo", 20, func(*sql.Tx) error {
		if err := s.AddBatchChecked([]*graph.Node{{ID: "new", Kind: graph.KindFunction, RepoPrefix: "repo"}}, nil); err != nil {
			return err
		}
		return s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "new", Tokens: "needle shared"}})
	})
	if err != nil || !handled || !reflect.DeepEqual(got, before) {
		t.Fatalf("old snapshot got=%v handled=%v err=%v", got, handled, err)
	}
	if len(repoMemoEntries(s)) != 1 {
		t.Fatal("old snapshot not recorded")
	}
	if next := repoMemoSearch(t, s, "needle", 20); len(next) != 3 {
		t.Fatal(next)
	}
	if len(repoMemoEntries(s)) != 2 {
		t.Fatal("next snapshot reused old authority")
	}
}
