package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

func TestFoldedNameSnapshotKeepsSelectionHydrationAndLateFieldsTogether(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "folded-name-snapshot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	stopStoreBackgroundReaders(s)
	s.AddBatch([]*graph.Node{
		{ID: "early", Name: "Other", Kind: graph.KindFunction, FilePath: "early.go", Language: "go"},
		{ID: "late", Name: "Needle", Kind: graph.KindFunction, FilePath: "late.go", Language: "go", Meta: map[string]any{"is_test": false}},
	}, nil)
	// A single reader proves hydration reuses the selected transaction rather
	// than borrowing a second pooled connection while its snapshot is pinned.
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	keys, more, selectErr := selectFoldedNameCandidates(ctx, tx, s.viewGen, "Needle", 1, graph.NameSearchFilter{})
	var writeErr error
	func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		write, err := s.beginWriteContext(ctx)
		if err != nil {
			writeErr = err
			return
		}
		changed, _, _, err := insertNodeChunksTx(write, s.viewGen, []*graph.Node{
			{ID: "early", Name: "Needle", Kind: graph.KindFunction, FilePath: "early.go", Language: "rust", Meta: map[string]any{"is_test": true}},
			{ID: "late", Name: "Other", Kind: graph.KindFunction, FilePath: "late.go", Language: "rust", Meta: map[string]any{"is_test": true}},
		}, false)
		if err == nil {
			err = write.Commit()
		}
		if err != nil {
			_ = write.Rollback()
			writeErr = err
			return
		}
		s.finishAnalysisMutationLocked(changed > 0)
	}()
	ids := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	nodes, hydrateErr := hydrateNameCandidateIDs(ctx, tx, s.viewGen, ids)
	rollbackErr := tx.Rollback()
	current, currentErr := s.GetNodesByIDsContext(ctx, []string{"early", "late"})
	closeErr := s.Close()
	closed = true
	t.Logf("coherent selected_keys=%v more=%v retained=%v current=%v selection_err=%v writer_err=%v hydration_err=%v rollback_err=%v current_err=%v close_err=%v", keys, more, nodes, current, selectErr, writeErr, hydrateErr, rollbackErr, currentErr, closeErr)
	if selectErr != nil || writeErr != nil || hydrateErr != nil || rollbackErr != nil || currentErr != nil || closeErr != nil {
		t.Fatal("real snapshot/write/hydration did not complete")
	}
	if more || len(keys) != 1 || keys[0].ID != "late" || len(nodes) != 1 {
		t.Fatal("selected name head changed or was not complete")
	}
	if nodes[0].ID != "late" || nodes[0].Name != "Needle" || nodes[0].Language != "go" || nodes[0].Meta["is_test"] != false {
		t.Fatal("hydration/late language or metadata predicate mixed versions")
	}
	if current["early"].Name != "Needle" || current["late"].Name != "Other" || current["late"].Language != "rust" {
		t.Fatal("the real independent write was not visible after snapshot release")
	}
}

func TestFoldedNameReadPoolWaitHonorsOriginalContext(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "folded-name-cancel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	stopStoreBackgroundReaders(s)
	s.AddNode(&graph.Node{ID: "target", Name: "Needle", Kind: graph.KindFunction})
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	held, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	type answer struct {
		nodes []*graph.Node
		more  bool
		err   error
	}
	done := make(chan answer, 1)
	before := s.db.Stats().WaitCount
	go func() {
		nodes, more, err := s.FindFoldedNameCandidatesContext(ctx, "Needle", 1, graph.NameSearchFilter{})
		done <- answer{nodes, more, err}
	}()
	admitUntil := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == before && time.Now().Before(admitUntil) {
		runtime.Gosched()
	}
	waitObserved := s.db.Stats().WaitCount > before
	cancel()
	var result answer
	returnedBeforeRelease := false
	select {
	case result = <-done:
		returnedBeforeRelease = true
	case <-time.After(time.Second):
	}
	heldErr := held.Rollback()
	if !returnedBeforeRelease {
		result = <-done
	}
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	_, _, expiredErr := s.FindFoldedNameCandidatesContext(expired, "Needle", 1, graph.NameSearchFilter{})
	expire()
	closeErr := s.Close()
	if !waitObserved || !returnedBeforeRelease || !errors.Is(result.err, context.Canceled) || len(result.nodes) != 0 || result.more || !errors.Is(expiredErr, context.DeadlineExceeded) || heldErr != nil || closeErr != nil {
		t.Fatalf("pool_wait=%v returned_before_release=%v result=%+v expired=%v held=%v close=%v", waitObserved, returnedBeforeRelease, result, expiredErr, heldErr, closeErr)
	}
}

// Test-only selector from the parent revision; no production toggle.
func selectFoldedNameCandidatesBeforePrefilter(ctx context.Context, tx *sql.Tx, generation int64, needle string, limit int, filter graph.NameSearchFilter) ([]graph.NodeSearchKey, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+nameCandidateCols+` FROM nodes WHERE view_gen = ? ORDER BY id`, generation)
	if err != nil {
		return nil, false, err
	}
	set := graph.NewFoldedNameCandidateSet(needle, limit)
	for rows.Next() {
		var key graph.NodeSearchKey
		if err := rows.Scan(&key.ID, &key.Name, &key.Kind, &key.FilePath, &key.RepoPrefix, &key.WorkspaceID, &key.ProjectID); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		if err := ctx.Err(); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		compact := &graph.Node{ID: key.ID, Name: key.Name, Kind: key.Kind, FilePath: key.FilePath, RepoPrefix: key.RepoPrefix, WorkspaceID: key.WorkspaceID, ProjectID: key.ProjectID}
		if filter.Allows(compact) {
			set.Add(key)
		}
	}
	rowsErr, closeErr := rows.Err(), rows.Close()
	if rowsErr != nil {
		return nil, false, rowsErr
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	return set.Keys(), set.More(), ctx.Err()
}

func foldedPrefilterFixture(tb testing.TB) (*Store, []string) {
	tb.Helper()
	s := openNodeSearchKeyTestStore(tb)
	stopStoreBackgroundReaders(s)
	names := []string{"", "Needle", "NEEDLE", "prefixNeedleSuffix", "other", "k", "K", "K",
		"i", "I", "İ", "ı", "ſ", "s", "S", "ß", "ẞ", "Σ", "σ", "ς", "école", "ÉCOLE", "日本語", "😀", "\uFFFD",
		"foo%bar", "foo_bar", "foo\x5cbar", "*?[]^-", "a\x00Needle", "Needle\x00a", "\x00Needle", "\x00", "\x00\x00", "a\nb", "a\tb", "a\x7fb"}
	for b := range 256 {
		value := string([]byte{byte(b)})
		names = append(names, value, "x"+value+"NeEdLe", "NeEdLe"+value+"x")
	}
	// Overlong sequences can appear ASCII to SQLite; length mismatch must
	// retain them. Single malformed high bytes need the fixed GLOB fallback.
	for _, value := range []string{"\xc0\x80", "\xc1\x81", "\xe0\x81\x81", "\xf0\x80\x81\x81",
		"\x80\x80", "\xff\x80\x80\x80", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xc3", "\xc3\xa9", "\xe9", "\xe2\x84\xaa", "\xf0\x9f\x98\x80"} {
		names = append(names, value, "x"+value+"NeEdLe", "NeEdLe"+value+"x")
	}
	blobStart := len(names)
	names = append(names, "KBlob", "Before\x00BLOB", "\xffBlob", "")
	for _, generation := range []int64{0, 41} {
		nodes := make([]*graph.Node, 0, len(names))
		kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindFile, graph.KindImport}
		for i, name := range names {
			kind := kinds[i%len(kinds)]
			if i >= blobStart {
				kind = graph.KindFunction
			}
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("node-%04d", i), Name: name, Kind: kind,
				FilePath: fmt.Sprintf("dir%d/file.go", i%3), RepoPrefix: []string{"", "repo", "other"}[i%3],
				WorkspaceID: fmt.Sprintf("workspace%d", i%2), ProjectID: fmt.Sprintf("project%d", i%3),
				Language: "go", Meta: map[string]any{"marker": i}})
		}
		if err := s.AtGeneration(generation).AddBatchChecked(nodes, nil); err != nil {
			tb.Fatal(err)
		}
		for i, node := range nodes {
			query := "UPDATE nodes SET name=CAST(? AS TEXT) WHERE view_gen=? AND id=?"
			if i >= blobStart {
				query = "UPDATE nodes SET name=? WHERE view_gen=? AND id=?"
			} else if utf8.ValidString(node.Name) {
				continue
			}
			// Preserve actual legacy TEXT/BLOB bytes: normal JSON writes repair
			// invalid UTF-8, so API-only setup would not exercise this contract.
			if _, err := s.writerDB.Exec(query, []byte(node.Name), generation, node.ID); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return s, names
}

func assertFoldedPrefilterMatchesBefore(t *testing.T, s *Store, generation int64, needle string, limit int, filter graph.NameSearchFilter) {
	t.Helper()
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	want, wantMore, wantErr := selectFoldedNameCandidatesBeforePrefilter(t.Context(), tx, generation, needle, limit, filter)
	got, gotMore, gotErr := selectFoldedNameCandidates(t.Context(), tx, generation, needle, limit, filter)
	closeErr := tx.Rollback()
	if wantErr != nil || gotErr != nil || closeErr != nil {
		t.Fatalf("needle=%q generation=%d before=%v after=%v rollback=%v", needle, generation, wantErr, gotErr, closeErr)
	}
	if !reflect.DeepEqual(got, want) || gotMore != wantMore {
		t.Fatalf("needle=%q generation=%d limit=%d got=%#v more=%v want=%#v more=%v", needle, generation, limit, got, gotMore, want, wantMore)
	}
}

func TestFoldedNamePrefilterAllByteAndUnicodeEquivalence(t *testing.T) {
	s, names := foldedPrefilterFixture(t)
	queries := []string{"", "Needle", "NEEDLE", "needle", "k", "K", "İ", "i", "ſ", "s", "ẞ", "ß", "Σ", "σ", "ς",
		"É", "école", "日本", "😀", "\uFFFD", "%", "_", "\x5c", "*", "?", "[", "]", "^", "-", "\x00", "a\x00",
		"\xc1\x81", "\xe0\x81\x81", "\xf0\x80\x81\x81", "\xff\x80\x80", "absent-long-name"}
	for b := range 256 {
		queries = append(queries, string([]byte{byte(b)}))
	}
	for _, needle := range queries {
		assertFoldedPrefilterMatchesBefore(t, s, 0, needle, 3, graph.NameSearchFilter{})
	}
	filters := []graph.NameSearchFilter{{}, {RepoAllow: map[string]bool{"repo": true}},
		{RepoAllow: map[string]bool{"other": false}}, {Accept: func(n *graph.Node) bool {
			return n.Kind == graph.KindMethod && n.WorkspaceID == "workspace1" &&
				n.ProjectID != "project0" && strings.HasPrefix(n.FilePath, "dir")
		}}}
	for _, generation := range []int64{0, 41, 7} {
		for _, needle := range []string{"needle", "k", "İ", "\uFFFD", "\x00", "%", "_", "\x5c", "absent-long-name"} {
			for _, limit := range []int{1, 4, len(names) + 1} {
				for _, filter := range filters {
					assertFoldedPrefilterMatchesBefore(t, s, generation, needle, limit, filter)
				}
			}
		}
	}
	t.Logf("all 256 byte needles plus Unicode/literal adversaries against %d names, raw TEXT/BLOB and scope/generation/head matrix", len(names))
}

func TestFoldedNamePrefilterRetainsEveryUnsafeTextName(t *testing.T) {
	s, names := foldedPrefilterFixture(t)
	seen := make(map[string]bool)
	filter := graph.NameSearchFilter{Accept: func(n *graph.Node) bool { seen[n.ID] = true; return true }}
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _, readErr := selectFoldedNameCandidates(t.Context(), tx, 0, "absent-long-name", 2, filter)
	closeErr := tx.Rollback()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read=%v rollback=%v", readErr, closeErr)
	}
	for i, name := range names {
		unsafe := false
		for j := range len(name) {
			unsafe = unsafe || name[j] < 0x20 || name[j] > 0x7e
		}
		if unsafe && !seen[fmt.Sprintf("node-%04d", i)] {
			t.Fatalf("non-ASCII/control/NUL/malformed name %q excluded before Go matching", name)
		}
	}
}

func TestFoldedNamePrefilterRejectsUnrelatedASCIIRowsBeforeDecode(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	stopStoreBackgroundReaders(s)
	nodes := make([]*graph.Node, 0, 513)
	for i := range 512 {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("ascii-%04d", i), Name: fmt.Sprintf("OrdinarySymbol%04d", i), Kind: graph.KindFunction})
	}
	nodes = append(nodes, &graph.Node{ID: "match", Name: "NarrowTarget", Kind: graph.KindFunction})
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	// Empty RepoAllow and nonnil compact keys: one Accept call per successful
	// Rows.Scan, before authoritative set.Add. Both arms get the same counter.
	beforeRows, afterRows := 0, 0
	want, wantMore, beforeErr := selectFoldedNameCandidatesBeforePrefilter(t.Context(), tx, s.viewGen, "target", 2,
		graph.NameSearchFilter{Accept: func(*graph.Node) bool { beforeRows++; return true }})
	got, gotMore, afterErr := selectFoldedNameCandidates(t.Context(), tx, s.viewGen, "target", 2,
		graph.NameSearchFilter{Accept: func(*graph.Node) bool { afterRows++; return true }})
	closeErr := tx.Rollback()
	if beforeErr != nil || afterErr != nil || closeErr != nil {
		t.Fatalf("before=%v after=%v close=%v", beforeErr, afterErr, closeErr)
	}
	if !reflect.DeepEqual(got, want) || gotMore != wantMore || len(got) != 1 {
		t.Fatal("candidate parity changed")
	}
	if beforeRows != len(nodes) || afterRows != 1 {
		t.Fatalf("decoded work not reduced: before=%d after=%d want before=%d after=1", beforeRows, afterRows, len(nodes))
	}
}

func TestFoldedNamePrefilterPublicBoundaryErrorsAndNilContext(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "folded-errors.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	stopStoreBackgroundReaders(s)
	if err := s.AddBatchChecked([]*graph.Node{{ID: "x", Name: "Needle", Kind: graph.KindFunction}}, nil); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	for _, args := range []struct {
		needle string
		limit  int
	}{{"", 1}, {"needle", 0}, {"needle", -1}} {
		if _, _, err := s.FindFoldedNameCandidatesContext(t.Context(), args.needle, args.limit, graph.NameSearchFilter{}); err == nil {
			t.Errorf("accepted invalid args %+v", args)
		}
	}
	//nolint:staticcheck // Deliberately exercise the existing nil-context normalization boundary.
	nodes, more, nilErr := s.FindFoldedNameCandidatesContext(nil, "needle", 1, graph.NameSearchFilter{})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, cancelErr := s.FindFoldedNameCandidatesContext(canceled, "needle", 1, graph.NameSearchFilter{})
	closeErr := s.Close()
	_, _, closedErr := s.FindFoldedNameCandidatesContext(t.Context(), "needle", 1, graph.NameSearchFilter{})
	if nilErr != nil || more || len(nodes) != 1 || !errors.Is(cancelErr, context.Canceled) || closeErr != nil || closedErr == nil {
		t.Fatalf("nil=%v nodes=%v more=%v cancel=%v close=%v closed=%v", nilErr, nodes, more, cancelErr, closeErr, closedErr)
	}
}

func TestFoldedNamePrefilterLiteralGlobPragmaAndPatternBounds(t *testing.T) {
	s, _ := foldedPrefilterFixture(t)
	queries := []string{"needle", "NEEDLE", "K", "İ", "Σ", "\uFFFD", "\x00", "\xff",
		"%", "_", "\x5c", "*", "?", "[", "]", "[^", "[]", "-", "^",
		strings.Repeat("a", (sqliteLikePatternLimit-2)/4),
		strings.Repeat("a", (sqliteLikePatternLimit-2)/4+1),
		strings.Repeat("*", sqliteLikePatternLimit/3+1),
		strings.Repeat("[", sqliteLikePatternLimit/3+1),
		strings.Repeat("z", sqliteLikePatternLimit+1)}
	for _, setting := range []string{"ON", "OFF"} {
		tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "PRAGMA case_sensitive_like="+setting); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		for _, needle := range queries {
			want, wantMore, beforeErr := selectFoldedNameCandidatesBeforePrefilter(t.Context(), tx, s.viewGen, needle, 3, graph.NameSearchFilter{})
			got, gotMore, afterErr := selectFoldedNameCandidates(t.Context(), tx, s.viewGen, needle, 3, graph.NameSearchFilter{})
			if beforeErr != nil || afterErr != nil || !reflect.DeepEqual(got, want) || gotMore != wantMore {
				_ = tx.Rollback()
				t.Fatalf("pragma=%s needle_bytes=%d prefix=%q before=%v after=%v got=%v more=%v want=%v more=%v", setting, len(needle), needle[:min(len(needle), 20)], beforeErr, afterErr, got, gotMore, want, wantMore)
			}
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}

func foldedPrefilterPlan(tb testing.TB, s *Store, needle string) string {
	tb.Helper()
	tx, err := s.db.BeginTx(tb.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		tb.Fatal(err)
	}
	query, args := foldedNameCandidatesSQL(s.viewGen, needle)
	rows, err := tx.QueryContext(tb.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		_ = tx.Rollback()
		tb.Fatal(err)
	}
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			tb.Fatal(err)
		}
		details = append(details, detail)
	}
	rowsErr, closeErr, rollbackErr := rows.Err(), rows.Close(), tx.Rollback()
	if rowsErr != nil || closeErr != nil || rollbackErr != nil {
		tb.Fatalf("plan rows=%v close=%v rollback=%v", rowsErr, closeErr, rollbackErr)
	}
	return strings.Join(details, "; ")
}

func TestFoldedNamePrefilterExistingCoverAndMissingIndex(t *testing.T) {
	s, names := foldedPrefilterFixture(t)
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	plan := foldedPrefilterPlan(t, s, "needle")
	t.Logf("existing-index plan: %s", plan)
	if !strings.Contains(plan, "USING COVERING INDEX nodes_name_candidates") || strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("compact query did not use existing cover without sort: %s", plan)
	}
	for _, needle := range []string{"", "needle", "K", "\x00", "\xff", "%", "["} {
		assertFoldedPrefilterMatchesBefore(t, s, 0, needle, len(names)+1, graph.NameSearchFilter{})
	}
	// A legacy/missing-index store must retain semantic reads. Production does
	// not force INDEXED BY or create/cache an index-availability certificate.
	if _, err := s.writerDB.Exec("DROP INDEX nodes_name_candidates"); err != nil {
		t.Fatal(err)
	}
	// EXPLAIN alone can describe a connection's cached schema after DDL on
	// the writer. Require actual index absence and execute the fallback read
	// before interpreting the refreshed plan on this same pooled reader.
	var installed int
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE type='index' AND name='nodes_name_candidates'").Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if installed != 0 {
		t.Fatal("DROP did not establish an actual missing-index fixture")
	}
	for _, generation := range []int64{0, 41, 7} {
		for _, needle := range []string{"", "needle", "K", "\x00", "\xff", "%", "["} {
			assertFoldedPrefilterMatchesBefore(t, s, generation, needle, 3, graph.NameSearchFilter{})
		}
	}
	plan = foldedPrefilterPlan(t, s, "needle")
	t.Logf("missing-index fallback plan after actual read: %s", plan)
	if strings.Contains(plan, "nodes_name_candidates") {
		t.Fatal("dropped index still in refreshed plan")
	}
}

func TestFoldedNamePrefilterHeadIndependentOfPhysicalOrder(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	stopStoreBackgroundReaders(s)
	nodes := make([]*graph.Node, 0, 12)
	for i := range 12 {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("id%02d", i),
			Name: strings.Repeat("x", (11-i)%4) + "Needle", Kind: graph.KindFunction,
			RepoPrefix: []string{"z", "", "a"}[i%3]})
	}
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 3, 20} {
		tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var beforeOrder, afterOrder []string
		want, wantMore, beforeErr := selectFoldedNameCandidatesBeforePrefilter(t.Context(), tx, s.viewGen, "needle", limit,
			graph.NameSearchFilter{Accept: func(n *graph.Node) bool { beforeOrder = append(beforeOrder, n.ID); return true }})
		got, gotMore, afterErr := selectFoldedNameCandidates(t.Context(), tx, s.viewGen, "needle", limit,
			graph.NameSearchFilter{Accept: func(n *graph.Node) bool { afterOrder = append(afterOrder, n.ID); return true }})
		closeErr := tx.Rollback()
		if beforeErr != nil || afterErr != nil || closeErr != nil {
			t.Fatalf("before=%v after=%v close=%v", beforeErr, afterErr, closeErr)
		}
		if !reflect.DeepEqual(got, want) || gotMore != wantMore || reflect.DeepEqual(beforeOrder, afterOrder) {
			t.Fatalf("head/more changed or fixture did not exercise different visitation: got=%v/%v want=%v/%v before=%v after=%v", got, gotMore, want, wantMore, beforeOrder, afterOrder)
		}
	}
}

func TestFoldedNamePrefilterActualBenchmarkFixtureCover(t *testing.T) {
	s, count := foldedNameCandidateBenchmarkFixture(t)
	if count != 20128 {
		t.Fatalf("benchmark fixture population changed: %d", count)
	}
	for _, needle := range []string{"target", "absent-query-xyz"} {
		plan := foldedPrefilterPlan(t, s, needle)
		t.Logf("actual unchanged benchmark fixture query=%s plan=%s", needle, plan)
		if !strings.Contains(plan, "USING COVERING INDEX nodes_name_candidates") {
			t.Fatal("existing cover absent/ineligible in actual benchmark setup")
		}
		assertFoldedPrefilterMatchesBefore(t, s, s.viewGen, needle, 10, graph.NameSearchFilter{})
	}
}

func TestFoldedNamePrefilterWhileOtherBulkIndexesDeferred(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	stopStoreBackgroundReaders(s)
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("fresh deferred bulk window not admitted")
	}
	defer func() {
		if err := s.AbortCoordinatedBulkLoad(); err != nil {
			t.Error(err)
		}
	}()
	if err := s.AddBatchChecked([]*graph.Node{{ID: "x", Name: "Needle", Kind: graph.KindFunction, RepoPrefix: "repo"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushBulk(); err != nil {
		t.Fatal(err)
	}
	if !s.bulkIndexesDeferred || !s.coordinatedBulkLoad {
		t.Fatal("fixture did not preserve actual deferred-index state")
	}
	assertFoldedPrefilterMatchesBefore(t, s, s.viewGen, "needle", 1, graph.NameSearchFilter{})
	nodes, more, err := s.FindFoldedNameCandidatesContext(t.Context(), "needle", 1, graph.NameSearchFilter{})
	if err != nil || more || len(nodes) != 1 {
		t.Fatalf("deferred-state read=%v more=%v err=%v", nodes, more, err)
	}
}
