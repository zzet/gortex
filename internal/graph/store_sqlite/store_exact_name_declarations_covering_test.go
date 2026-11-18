package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestExactNameDeclarationCoveringMigrationPreservesOrderedFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy37.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := string([]byte{'r', 0xff})
	for _, gen := range []int64{0, 41} {
		if err := s.AtGeneration(gen).AddBatchChecked([]*graph.Node{
			{ID: "z", Name: "Shared", Kind: graph.KindConstant, RepoPrefix: "a", FilePath: "a/z.go", Meta: map[string]any{"value": "one"}},
			{ID: "a", Name: "Shared", Kind: graph.KindParam, RepoPrefix: "z", FilePath: "z/a.go"},
			{ID: "m", Name: "Shared", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/m.go"},
			{ID: "raw", Name: raw, Kind: graph.KindConstant, RepoPrefix: "a", FilePath: "a/raw.go"},
			{ID: "replacement", Name: "r�", Kind: graph.KindConstant, RepoPrefix: "a"},
			{ID: "empty", Name: "", Kind: graph.KindFile, RepoPrefix: "a"},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"DROP INDEX nodes_by_name", "CREATE INDEX nodes_by_name ON nodes(name,view_gen)", "PRAGMA user_version=37"} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	read := func(db *sql.DB, q string, args ...any) []graph.Node {
		t.Helper()
		rows, e := db.Query(q, args...)
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		var out []graph.Node
		for rows.Next() {
			var n graph.Node
			if e = rows.Scan(&n.ID, &n.Name, &n.Kind, &n.FilePath, &n.RepoPrefix); e != nil {
				t.Fatal(e)
			}
			out = append(out, n)
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		return out
	}
	type question struct {
		q    string
		args []any
		want []graph.Node
	}
	var questions []question
	for _, gen := range []int64{0, 41} {
		for _, repo := range []string{"", "a", "z"} {
			for _, rawCase := range []bool{false, true} {
				q := exactNameDeclarationSeekSQL
				args := []any{`["Shared","Missing","","Shared"]`, gen}
				if rawCase {
					q = exactRawNameDeclarationSeekSQL
					args = []any{raw, gen}
				}
				if repo != "" {
					q += " AND +n.repo_prefix = ?"
					args = append(args, repo)
				}
				questions = append(questions, question{q, args, read(db, q, args...)})
			}
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	for _, q := range questions {
		if got := read(s.db, q.q, q.args...); !reflect.DeepEqual(got, q.want) {
			t.Fatalf("ordered declaration facts changed: got%v want%v", got, q.want)
		}
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+q.q, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		covered := false
		for rows.Next() {
			var a, b, c int
			var detail string
			if err = rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			covered = covered || strings.Contains(detail, "COVERING INDEX nodes_by_name")
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !covered {
			t.Fatal("narrow declaration query reads wide node payload")
		}
	}
	tx, err := s.writerDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = coverNodeDeclarationIndex(tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	requireGenerationLookupIndexShapes(t, s.db)
}

// An upgrade from v25 or older reaches v38 with nodes_by_name already rebuilt
// by v26; the step must not build that index a second time.
func TestExactNameDeclarationCoveringMigrationKeepsCurrentIndex(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "current.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.writerDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// Every DROP or CREATE bumps the schema cookie; a freed root page can be
	// reused by the rebuilt index, so the page number alone proves nothing.
	cookie := func() int64 {
		t.Helper()
		var v int64
		if err := tx.QueryRow(`PRAGMA schema_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := cookie()
	if err = coverNodeDeclarationIndex(tx); err != nil {
		t.Fatal(err)
	}
	if after := cookie(); after != before {
		t.Fatalf("current nodes_by_name was rebuilt: schema cookie %d -> %d", before, after)
	}
}
