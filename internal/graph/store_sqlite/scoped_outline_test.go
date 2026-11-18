package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/storetest"
)

func TestScopedOutlineParity(t *testing.T) {
	storetest.RunScopedOutline(t, func(t *testing.T) graph.Store {
		s, err := Open(filepath.Join(t.TempDir(), "outline.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestScopedOutlineQueryPlans(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	seedGenerationLookupRows(t, db, 0, 4, 256)
	for _, analyze := range []bool{false, true} {
		if analyze {
			if _, err := db.Exec("ANALYZE"); err != nil {
				t.Fatal(err)
			}
		}
		for _, generation := range []int64{0, 3} {
			scope := graph.NodeScope{WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"repo": true, "other03": true}}
			for name, filter := range map[string]graph.OutlineEdgeFilter{
				"unfiltered": {},
				"contract":   {ExcludeKinds: []graph.EdgeKind{graph.EdgeMatches}, ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}, ExcludeVia: "spring.Bean"},
				"identities": {ExcludeNodeIDs: map[string]bool{"hidden": true}},
			} {
				query := outlineAggregateQuery(scope, generation, filter)
				t.Run(name, func(t *testing.T) {
					rows, err := db.Query("EXPLAIN QUERY PLAN "+query.sql, query.args...)
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
					t.Log(joined)
					if !strings.Contains(joined, "nodes_by_repo (repo_prefix=? AND view_gen=?)") {
						t.Fatalf("missing repo seek: %s", joined)
					}
					for _, forbidden := range []string{"SCAN n", "SCAN e", "SCAN source", "SCAN nodes", "SCAN edges"} {
						if strings.Contains(joined, forbidden) {
							t.Fatalf("table scan: %s", joined)
						}
					}
					if !strings.Contains(joined, "edges_by_from (view_gen=? AND from_id=?)") {
						t.Fatalf("missing outgoing seek: %s", joined)
					}
					if !strings.Contains(joined, "edges_by_to (view_gen=? AND to_id=? AND kind=?)") {
						t.Fatalf("missing incoming seek: %s", joined)
					}
				})
			}
		}
	}
}

func TestScopedOutlineGenerationAndMissingIndexes(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	for _, row := range []struct {
		id, repo, lang, path string
		generation           int
	}{
		{"from", "a", "go", "a/main.go", 0},
		{"target", "a", "go", "a/lib.go", 0},
		{"from", "outside", "python", "outside/main.py", 3},
		{"target", "a", "rust", "a/lib.rs", 3},
		{"derived-only", "a", "rust", "a/derived.rs", 3},
	} {
		if _, err := db.Exec(`INSERT INTO nodes(id,view_gen,repo_prefix,language,file_path,kind,name,meta) VALUES(?,?,?,?,?,'function','',?)`, row.id, row.generation, row.repo, row.lang, row.path, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		from, to   string
		generation int
	}{
		{"from", "target", 0}, {"from", "target", 3}, {"target", "derived-only", 3},
		// An endpoint existing only in another generation cannot authorize
		// the edge count or attribute imports to that generation's file.
		{"from", "derived-only", 0},
	} {
		if _, err := db.Exec(`INSERT INTO edges(from_id,to_id,kind,view_gen,meta) VALUES(?,?,'imports',?,?)`, row.from, row.to, row.generation, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		for _, gen := range []int64{0, 3} {
			s := &Store{storeCore: &storeCore{db: db}, viewGen: gen}
			got, err := s.ScopedOutlineCounts(graph.NodeScope{RepoAllow: map[string]bool{"a": true}})
			if err != nil {
				t.Fatal(err)
			}
			if got.TotalNodes != 2 || got.TotalEdges != 1 {
				t.Fatalf("generation %d counts: %+v", gen, got)
			}
			if gen == 0 && (got.ByLanguage["go"] != 2 || len(got.FileImports) != 1 || got.FileImports[0].Count != 1 || got.FileImports[0].FilePath != "a/lib.go") {
				t.Fatalf("base counts: %+v", got)
			}
			if gen == 3 && (got.ByLanguage["rust"] != 2 || len(got.FileImports) != 2) {
				t.Fatalf("derived counts: %+v", got)
			}
		}
	}
	check()
	for _, index := range []string{"nodes_by_repo", "edges_by_from", "edges_by_to"} {
		if _, err := db.Exec("DROP INDEX " + index); err != nil {
			t.Fatal(err)
		}
		check()
	}
}

func TestScopedOutlineFilteredParity(t *testing.T) {
	storetest.RunFilteredScopedOutline(t, func(t *testing.T) graph.Store {
		s, err := Open(filepath.Join(t.TempDir(), "outline.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestScopedOutlineMetadataEncodings(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/a.go"},
		{ID: "b", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/b.go"},
	}, nil)
	flat, err := encodeMeta(map[string]any{"via": "spring.Bean"})
	if err != nil {
		t.Fatal(err)
	}
	falsePositive, err := encodeMeta(map[string]any{"other": "spring.Bean", "via": "spring.BeanFactory"})
	if err != nil {
		t.Fatal(err)
	}
	var legacy bytes.Buffer
	if err := gob.NewEncoder(&legacy).Encode(map[string]any{"via": "spring.Bean"}); err != nil {
		t.Fatal(err)
	}
	for i, blob := range [][]byte{flat, []byte(`{"via":"spring.Bean"}`), []byte(`{"via":"\u0073pring.Bean"}`), legacy.Bytes(), falsePositive, nil} {
		for _, kind := range []string{"calls", "imports"} {
			if _, err := s.writerDB.Exec(`INSERT INTO edges(from_id,to_id,kind,line,view_gen,meta) VALUES('a','b',?,?,0,?)`, kind, i, blob); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, text := range []string{`{"via":"spring.Bean"}`, string(flat)} {
		// SQLite TEXT may contain the flat codec's leading NUL; length(TEXT)
		// would report zero, while the decoder still sees the complete bytes.
		for _, kind := range []string{"calls", "imports"} {
			if _, err := s.writerDB.Exec(`INSERT INTO edges(from_id,to_id,kind,line,view_gen,meta) VALUES('a','b',?,?,0,?)`, kind, 100+i, text); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := s.ScopedOutlineCountsFiltered(graph.NodeScope{RepoAllow: map[string]bool{"repo": true}}, graph.OutlineEdgeFilter{ExcludeVia: "spring.Bean"})
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalEdges != 4 || len(got.FileImports) != 1 || got.FileImports[0].Count != 2 {
		t.Fatalf("mixed metadata counts = %+v, want 4 edges and 2 imports", got)
	}
}

func TestScopedOutlineUsesOneSnapshot(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "snapshot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/a.go"},
		{ID: "b", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/b.go"},
	}, []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeImports}})
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	scope := graph.NodeScope{RepoAllow: map[string]bool{"repo": true}}
	filter := graph.OutlineEdgeFilter{ExcludeVia: "spring.Bean"}
	var edges, imports int
	written := false
	if err := scanOutlineQuery(t.Context(), tx, outlineAggregateQuery(scope, 0, filter), func(rows *sql.Rows) error {
		var section, key string
		var count int
		if err := rows.Scan(&section, &key, &count); err != nil {
			return err
		}
		if !written {
			// The language row pins the snapshot before either edge section.
			written = true
			for line := 1; line <= 4; line++ {
				s.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeImports, Line: line, Meta: map[string]any{"via": "spring.Bean"}})
			}
			s.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeImports, Line: 5})
		}
		if section == "edge" {
			edges += count
		}
		if section == "import" {
			imports += count
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !written || edges != 1 || imports != 1 {
		t.Fatalf("writer changed pinned snapshot: written=%v edges=%d imports=%d", written, edges, imports)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ScopedOutlineCountsFiltered(scope, filter)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.TotalEdges != 2 || len(fresh.FileImports) != 1 || fresh.FileImports[0].Count != 2 {
		t.Fatalf("fresh filtered counts: %+v", fresh)
	}
	all, err := s.ScopedOutlineCounts(scope)
	if err != nil {
		t.Fatal(err)
	}
	if all.TotalEdges != 6 {
		t.Fatalf("writer did not insert concurrent edges: %+v", all)
	}
}

func TestScopedOutlineCancellationAndClosedStore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "canceled.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.WithReadContext(ctx).ScopedOutlineCounts(graph.NodeScope{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled outline error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScopedOutlineCounts(graph.NodeScope{}); err == nil {
		t.Fatal("closed store returned a successful outline")
	}
}

func TestScopedOutlineMetadataErrorsPropagate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "invalid-meta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/a.go"},
		{ID: "b", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/b.go"},
	}, nil)
	if _, err := s.writerDB.Exec(`INSERT INTO edges(from_id,to_id,kind,view_gen,meta) VALUES('a','b','calls',0,?)`, []byte("invalid-gob")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScopedOutlineCountsFiltered(graph.NodeScope{RepoAllow: map[string]bool{"repo": true}}, graph.OutlineEdgeFilter{ExcludeVia: "spring.Bean"}); err == nil {
		t.Fatal("invalid metadata returned a successful filtered outline")
	}
}

func TestScopedOutlineMetadataViaContainsNUL(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "nul-via.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const via = "custom\x00via"
	s.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/a.go"},
		{ID: "b", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/b.go"},
	}, []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeImports, Meta: map[string]any{"via": via}}})
	counts, err := s.ScopedOutlineCountsFiltered(graph.NodeScope{RepoAllow: map[string]bool{"repo": true}}, graph.OutlineEdgeFilter{ExcludeVia: via})
	if err != nil {
		t.Fatal(err)
	}
	if counts.TotalEdges != 0 || len(counts.FileImports) != 0 {
		t.Fatalf("NUL via value was truncated: %+v", counts)
	}
}
