package store_sqlite

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRepoLanguageFileCountsKeepRepoSeeksAcrossStatistics(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	seedGenerationLookupRows(t, db, 0, 4, 256)
	check := func(t *testing.T) {
		t.Helper()
		for _, generation := range []int{0, 3} {
			for _, roots := range []string{`["repo"]`, `["","repo"]`, `["repo","other03"]`} {
				requireGenerationLookupPlan(t, db, repoLanguageFileCountsSQL, "nodes_by_repo", true,
					[]any{roots, string(graph.KindModule), string(graph.KindDoc), generation}, "repo_prefix=?", "view_gen=?")
			}
		}
	}
	t.Run("no_statistics", func(t *testing.T) { check(t) })
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	t.Run("fresh_statistics", func(t *testing.T) { check(t) })
	// This is the native full29 cold snapshot's skew: generation and repo/gen
	// averages tie, letting the original reorderable join scan the generation.
	for index, stats := range map[string]string{
		"nodes_by_generation": "596886 1001 1",
		"nodes_by_repo":       "596886 1001 1001",
	} {
		if _, err := db.Exec(`UPDATE sqlite_stat1 SET stat = ? WHERE idx = ?`, stats, index); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`ANALYZE sqlite_schema`); err != nil {
		t.Fatal(err)
	}
	t.Run("native_skewed_statistics", func(t *testing.T) { check(t) })
	seedGenerationLookupRows(t, db, 4, 4, 256)
	t.Run("stale_statistics", func(t *testing.T) { check(t) })
}

func TestRepoLanguageFileCountsPreserveUnnamedAndGenerationFilters(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	for _, generation := range []int64{0, 3} {
		for i, row := range []struct{ repo, file, language, kind, class string }{
			{"", "solo.go", "go", "function", ""},
			{"a", "a.go", "go", "file", ""},
			{"a", "a.go", "go", "function", ""},
			{"a", "a.go", "go", "module", ""},
			{"a", "doc.go", "go", "doc", "symbol"},
			{"a", "content.md", "markdown", "doc", "content"},
			{"a", "empty.go", "", "function", ""},
			{"b", "b.py", "python", "type", ""},
			{"outside", "other.rs", "rust", "function", ""},
		} {
			if _, err := db.Exec(`INSERT INTO nodes(id,view_gen,repo_prefix,file_path,language,kind,name,data_class,meta) VALUES (?,?,?,?,?,?,?,?,?)`,
				fmt.Sprintf("node-%d", i), generation, row.repo, row.file, row.language, row.kind, "", row.class, []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`INSERT INTO nodes(id,view_gen,repo_prefix,file_path,language,kind,name,meta) VALUES ('derived-only',3,'a','a.go','go','type','',?)`, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	want := []graph.RepoLanguageFileCount{
		{RepoPrefix: "", FilePath: "solo.go", Language: "go", Count: 1},
		{RepoPrefix: "a", FilePath: "a.go", Language: "go", Count: 2},
		{RepoPrefix: "a", FilePath: "doc.go", Language: "go", Count: 1},
		{RepoPrefix: "b", FilePath: "b.py", Language: "python", Count: 1},
	}
	check := func(t *testing.T) {
		t.Helper()
		for _, generation := range []int64{0, 3} {
			store := &Store{storeCore: &storeCore{db: db}, viewGen: generation}
			expected := append([]graph.RepoLanguageFileCount(nil), want...)
			if generation == 3 {
				expected[1].Count = 3
			}
			if got := store.RepoLanguageFileCounts([]string{"b", "", "a", "a"}); !reflect.DeepEqual(got, expected) {
				t.Fatalf("generation %d: got %#v want %#v", generation, got, expected)
			}
			if got := store.RepoLanguageFileCounts(nil); len(got) != 0 {
				t.Fatalf("empty request = %#v", got)
			}
			if got := store.RepoLanguageFileCounts([]string{"missing"}); len(got) != 0 {
				t.Fatalf("missing repo = %#v", got)
			}
		}
	}
	check(t)
	// This secondary index is legitimately absent inside bulk-load windows.
	// CROSS JOIN must retain correctness rather than fail with no such index.
	if _, err := db.Exec(`DROP INDEX nodes_by_repo`); err != nil {
		t.Fatal(err)
	}
	check(t)
}
