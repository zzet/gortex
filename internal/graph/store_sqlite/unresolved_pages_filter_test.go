package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// SourceOnly keeps one repository's own unresolved rows (bare targets from
// other repositories included in ScopeFilter are dropped), and Kinds keeps
// only the requested edge kinds.
func TestUnresolvedEdgePagesFilterBySourceRepoAndKind(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "unresolved-filter.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	nodes := []*graph.Node{
		{ID: "a/x.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/x.js", RepoPrefix: "a"},
		{ID: "b/y.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "b/y.js", RepoPrefix: "b"},
	}
	edges := []*graph.Edge{
		{From: "a/x.js::run", To: "unresolved::work", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 1},
		{From: "a/x.js::run", To: "unresolved::Type", Kind: graph.EdgeReferences, FilePath: "a/x.js", Line: 2},
		{From: "b/y.js::run", To: "unresolved::work", Kind: graph.EdgeCalls, FilePath: "b/y.js", Line: 1},
		{From: "b/y.js::run", To: "a::unresolved::helper", Kind: graph.EdgeCalls, FilePath: "b/y.js", Line: 2},
	}
	store.AddBatch(nodes, edges)

	read := func(mutate func(*graph.UnresolvedEdgeScan)) []string {
		t.Helper()
		scan, err := store.BeginUnresolvedEdgeScan(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		mutate(&scan)
		page, err := store.ReadUnresolvedEdgePage(t.Context(), scan, 0, 64, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range page.Edges {
			got = append(got, e.From+" "+string(e.Kind)+" "+e.To)
		}
		sort.Strings(got)
		return got
	}
	equal := func(name string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", name, got, want)
			}
		}
	}

	equal("scope filter keeps bare targets of every repo", read(func(s *graph.UnresolvedEdgeScan) {
		s.ScopeAnchors, s.ScopeFilter = []string{"a"}, true
	}), []string{
		"a/x.js::run calls unresolved::work",
		"a/x.js::run references unresolved::Type",
		"b/y.js::run calls a::unresolved::helper",
		"b/y.js::run calls unresolved::work",
	})
	equal("source only", read(func(s *graph.UnresolvedEdgeScan) {
		s.ScopeAnchors, s.SourceOnly = []string{"a"}, true
	}), []string{
		"a/x.js::run calls unresolved::work",
		"a/x.js::run references unresolved::Type",
	})
	equal("source only and kind", read(func(s *graph.UnresolvedEdgeScan) {
		s.ScopeAnchors, s.SourceOnly = []string{"a"}, true
		s.Kinds = []graph.EdgeKind{graph.EdgeCalls}
	}), []string{"a/x.js::run calls unresolved::work"})
	equal("kind only", read(func(s *graph.UnresolvedEdgeScan) {
		s.Kinds = []graph.EdgeKind{graph.EdgeReferences}
	}), []string{"a/x.js::run references unresolved::Type"})
}

func TestUnresolvedEdgePagesFilterByMetaContains(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "unresolved-meta-filter.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	nodes := []*graph.Node{
		{ID: "a/x.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/x.js", RepoPrefix: "a"},
		{ID: "b/y.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "b/y.js", RepoPrefix: "b"},
	}
	edges := []*graph.Edge{
		{
			From: "a/x.js::run", To: "unresolved::flatReq", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 1,
			Meta: map[string]any{
				graph.JSLexicalBindingMeta:     graph.JSLexicalRequire,
				graph.JSLexicalRequireSpecMeta: "pkg",
			},
		},
		{
			From: "a/x.js::run", To: "unresolved::plain", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 2,
		},
		{
			From: "b/y.js::run", To: "unresolved::otherRepoReq", Kind: graph.EdgeCalls, FilePath: "b/y.js", Line: 1,
			Meta: map[string]any{
				graph.JSLexicalBindingMeta:     graph.JSLexicalRequire,
				graph.JSLexicalRequireSpecMeta: "pkg",
			},
		},
		{
			From: "b/y.js::run", To: "unresolved::jsonReq", Kind: graph.EdgeCalls, FilePath: "b/y.js", Line: 2,
			// int32 is unsupported by encodeMetaFast, forcing JSON meta encoding on disk.
			Meta: map[string]any{
				graph.JSLexicalBindingMeta:     graph.JSLexicalRequire,
				graph.JSLexicalRequireSpecMeta: "pkg",
				"json_only":                    int32(1),
			},
		},
	}
	store.AddBatch(nodes, edges)

	// Verify on-disk codec encodings so flat-binary and JSON paths are genuinely exercised.
	checkMetaLeadByte := func(toID string, want byte) {
		t.Helper()
		var lead []byte
		if err := store.db.QueryRow(`SELECT substr(meta, 1, 1) FROM edges WHERE to_id = ?`, toID).Scan(&lead); err != nil {
			t.Fatalf("query lead byte for %s: %v", toID, err)
		}
		if len(lead) != 1 || lead[0] != want {
			t.Fatalf("%s meta lead byte: got %x, want %x", toID, lead, want)
		}
	}
	checkMetaLeadByte("unresolved::flatReq", metaFlatMagic0)
	checkMetaLeadByte("unresolved::jsonReq", '{')

	read := func(mutate func(*graph.UnresolvedEdgeScan)) []string {
		t.Helper()
		scan, err := store.BeginUnresolvedEdgeScan(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		mutate(&scan)
		page, err := store.ReadUnresolvedEdgePage(t.Context(), scan, 0, 64, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range page.Edges {
			got = append(got, e.From+" "+e.To)
		}
		sort.Strings(got)
		return got
	}

	gotAll := read(func(s *graph.UnresolvedEdgeScan) {
		s.Kinds = []graph.EdgeKind{graph.EdgeCalls}
		s.MetaContains = graph.JSLexicalRequireSpecMeta
	})
	wantAll := []string{
		"a/x.js::run unresolved::flatReq",
		"b/y.js::run unresolved::jsonReq",
		"b/y.js::run unresolved::otherRepoReq",
	}
	if strings.Join(gotAll, ",") != strings.Join(wantAll, ",") {
		t.Fatalf("MetaContains all: got %v, want %v", gotAll, wantAll)
	}

	gotScoped := read(func(s *graph.UnresolvedEdgeScan) {
		s.Kinds = []graph.EdgeKind{graph.EdgeCalls}
		s.ScopeAnchors = []string{"a"}
		s.SourceOnly = true
		s.MetaContains = graph.JSLexicalRequireSpecMeta
	})
	wantScoped := []string{"a/x.js::run unresolved::flatReq"}
	if strings.Join(gotScoped, ",") != strings.Join(wantScoped, ",") {
		t.Fatalf("MetaContains scoped: got %v, want %v", gotScoped, wantScoped)
	}
}

func TestUnresolvedEdgePagesMetaContainsPlanLock(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "unresolved-plan-lock.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	nodes := []*graph.Node{
		{ID: "a/x.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/x.js", RepoPrefix: "a"},
	}
	edges := []*graph.Edge{
		{
			From: "a/x.js::run", To: "unresolved::req", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 1,
			Meta: map[string]any{graph.JSLexicalRequireSpecMeta: "pkg"},
		},
		{
			From: "a/x.js::run", To: "unresolved::plain", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 2,
		},
	}
	s.AddBatch(nodes, edges)

	type planQuery struct {
		name string
		sql  string
		args []any
	}
	source, generation := s.unresolvedScanSource(unresolvedHighWaterBaseSource)
	queries := []planQuery{
		{
			name: "meta_contains_and_kind",
			sql: unresolvedEdgePageSQL(
				source, generation,
				` AND kind IN (?) AND meta IS NOT NULL AND instr(CAST(meta AS BLOB), CAST(? AS BLOB)) > 0`,
			),
			args: []any{int64(0), int64(0), int64(1000), string(graph.EdgeCalls), []byte(graph.JSLexicalRequireSpecMeta), 2048},
		},
		{
			name: "meta_contains_source_only_and_kind",
			sql: unresolvedEdgePageSQL(
				source, generation,
				` AND (from_repo IS NULL OR from_repo IN (?)) AND kind IN (?) AND meta IS NOT NULL AND instr(CAST(meta AS BLOB), CAST(? AS BLOB)) > 0`,
			),
			args: []any{int64(0), int64(0), int64(1000), "a", string(graph.EdgeCalls), []byte(graph.JSLexicalRequireSpecMeta), 2048},
		},
		{
			name: "kind_only",
			sql:  unresolvedEdgePageSQL(source, generation, ` AND kind IN (?)`),
			args: []any{int64(0), int64(0), int64(1000), string(graph.EdgeCalls), 2048},
		},
	}

	assertLocked := func(t *testing.T, ctx context.Context, conn *sql.Conn) {
		t.Helper()
		for _, q := range queries {
			plan := strings.Join(explainOnConn(t, ctx, conn, q.sql, q.args...), "\n")
			if !strings.Contains(plan, "USING INDEX edges_by_unresolved") {
				t.Errorf("%s: expected USING INDEX edges_by_unresolved:\n%s", q.name, plan)
			}
			if strings.Contains(plan, "TEMP B-TREE") {
				t.Errorf("%s: unexpected TEMP B-TREE:\n%s", q.name, plan)
			}
		}
	}

	t.Run("no_stats", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			assertLocked(t, ctx, conn)
		})
	})
	t.Run("real_store_repair_stats", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("ANALYZE: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear sqlite_stat1: %v", err)
			}
			for _, row := range receiverMutationCallStatRows {
				if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`, row[0], row[1], row[2]); err != nil {
					t.Fatalf("insert stat row %v: %v", row, err)
				}
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
				t.Fatalf("reload stats: %v", err)
			}
			assertLocked(t, ctx, conn)
		})
	})
	t.Run("large_store_stats", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear sqlite_stat1: %v", err)
			}
			largeRows := [][3]string{
				{"edges", "edges_by_unresolved", "50000 50000 1"},
				{"edges", "edges_by_kind", "2000000 400000"},
				{"edges", "edges_by_from_repo", "2000000 200000"},
			}
			for _, row := range largeRows {
				if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`, row[0], row[1], row[2]); err != nil {
					t.Fatalf("insert large stat row %v: %v", row, err)
				}
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
				t.Fatalf("reload stats: %v", err)
			}
			assertLocked(t, ctx, conn)
		})
	})
	t.Run("fresh_analyze", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear sqlite_stat1: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("ANALYZE: %v", err)
			}
			assertLocked(t, ctx, conn)
		})
	})
}

func TestCheckedReadsCorelessClosedAndPagedCancel(t *testing.T) {
	// Coreless / nil-db store must return sql.ErrConnDone even on empty inputs.
	coreless := &Store{}
	seq, errFn := coreless.EdgesLightSeqErr(t.Context())
	for range seq {
	}
	if !errors.Is(errFn(), sql.ErrConnDone) {
		t.Fatalf("coreless EdgesLightSeqErr(empty): got %v, want sql.ErrConnDone", errFn())
	}
	if _, err := coreless.GetEdgeCandidatesErr(t.Context(), nil, nil); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("coreless GetEdgeCandidatesErr(empty): got %v, want sql.ErrConnDone", err)
	}

	s, err := Open(filepath.Join(t.TempDir(), "checked-reads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	nodes := []*graph.Node{
		{ID: "a/x.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/x.js", RepoPrefix: "a"},
		{ID: "b/y.js::work", Kind: graph.KindFunction, Name: "work", FilePath: "b/y.js", RepoPrefix: "b"},
	}
	edges := []*graph.Edge{
		{From: "a/x.js::run", To: "b/y.js::work", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 1, CrossRepo: true},
		{From: "a/x.js::run", To: "b/y.js::work", Kind: graph.EdgeCrossRepoCalls, FilePath: "a/x.js", Line: 1, CrossRepo: true},
		{From: "a/x.js::run", To: "b/y.js::work", Kind: graph.EdgeCrossRepoCalls, FilePath: "a/x.js", Line: 2, CrossRepo: true},
	}
	s.AddBatch(nodes, edges)

	// Early break on healthy context must report nil error in both modes.
	for _, paged := range []bool{false, true} {
		target := s
		if paged {
			target = s.BindAnalysisPages().(*Store)
		}
		seq, errFn := target.EdgesLightSeqErr(t.Context(), graph.EdgeCrossRepoCalls)
		seen := 0
		for range seq {
			seen++
			break
		}
		if seen != 1 || errFn() != nil {
			t.Fatalf("paged=%v early-break: seen=%d err=%v", paged, seen, errFn())
		}

		// Cancelled context must surface context.Canceled in both modes.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		seq2, errFn2 := target.EdgesLightSeqErr(ctx, graph.EdgeCrossRepoCalls)
		for range seq2 {
		}
		if !errors.Is(errFn2(), context.Canceled) {
			t.Fatalf("paged=%v cancelled EdgesLightSeqErr: got %v, want context.Canceled", paged, errFn2())
		}
		if _, err := target.GetEdgeCandidatesErr(ctx, nil, []graph.EdgeSite{{From: "a/x.js::run", Line: 1, Kind: graph.EdgeCalls}}); !errors.Is(err, context.Canceled) {
			t.Fatalf("paged=%v cancelled GetEdgeCandidatesErr: got %v, want context.Canceled", paged, err)
		}
	}

	// Closed store must return an error on non-empty reads instead of panicking.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	seqClosed, errClosed := s.EdgesLightSeqErr(t.Context(), graph.EdgeCrossRepoCalls)
	for range seqClosed {
	}
	if errClosed() == nil {
		t.Fatal("closed EdgesLightSeqErr: expected non-nil error")
	}
	if _, err := s.GetEdgeCandidatesErr(t.Context(), nil, []graph.EdgeSite{{From: "a/x.js::run", Line: 1, Kind: graph.EdgeCalls}}); err == nil {
		t.Fatal("closed GetEdgeCandidatesErr: expected non-nil error")
	}
}
