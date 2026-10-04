package store_sqlite

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRepoLanguageCountsKeepRepoSeeksAcrossStatistics(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	seedGenerationLookupRows(t, db, 0, 4, 256)
	check := func(t *testing.T) {
		t.Helper()
		for _, gen := range []int64{0, 3} {
			for _, roots := range []string{`["repo"]`, `["","repo"]`, `["repo","other03"]`} {
				requireGenerationLookupPlan(t, db, repoLanguageCountsSQL, "nodes_by_repo", true,
					[]any{roots, string(graph.KindDoc), gen}, "repo_prefix=?", "view_gen=?")
			}
		}
	}
	t.Run("no_statistics", check)
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	t.Run("fresh_statistics", check)
	// Retained v12 chooses the generation-only index for the reorderable join.
	for index, stats := range map[string]string{"nodes_by_generation": "596886 1001 1", "nodes_by_repo": "596886 1001 1001"} {
		if _, err := db.Exec(`UPDATE sqlite_stat1 SET stat=? WHERE idx=?`, stats, index); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`ANALYZE sqlite_schema`); err != nil {
		t.Fatal(err)
	}
	t.Run("native_skewed_statistics", check)
	seedGenerationLookupRows(t, db, 4, 4, 256)
	t.Run("stale_statistics", check)
}

func TestRepoLanguageCountsPreserveExactProjectionWhenIndexIsAbsent(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	for _, gen := range []int64{0, 3} {
		for i, row := range []struct{ repo, language, kind, class string }{
			{"", "go", "function", ""}, {"a", "go", "module", ""}, {"a", "go", "function", ""},
			{"a", "go", "doc", "symbol"}, {"a", "markdown", "doc", "content"}, {"a", "", "function", ""},
			{"b", "python", "type", ""}, {"outside", "rust", "function", ""},
		} {
			if _, err := db.Exec(`INSERT INTO nodes(id,view_gen,repo_prefix,file_path,language,kind,name,data_class,meta) VALUES(?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("node-%d", i), gen, row.repo, "file", row.language, row.kind, "", row.class, []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`INSERT INTO nodes(id,view_gen,repo_prefix,file_path,language,kind,name,meta) VALUES('derived-only',3,'a','file','go','type','',?)`, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		for _, gen := range []int64{0, 3} {
			s := &Store{storeCore: &storeCore{db: db}, viewGen: gen}
			count := 3
			if gen == 3 {
				count++
			}
			want := map[string]map[string]int{"": {"go": 1}, "a": {"go": count}, "b": {"python": 1}}
			if got := s.RepoLanguageCounts([]string{"b", "", "a", "a"}); !reflect.DeepEqual(got, want) {
				t.Fatalf("gen%d: got%v want%v", gen, got, want)
			}
			if got := s.RepoLanguageCounts(nil); len(got) != 0 {
				t.Fatalf("empty roots: %v", got)
			}
			if got := s.RepoLanguageCounts([]string{"missing"}); len(got) != 0 {
				t.Fatalf("missing root: %v", got)
			}
		}
	}
	check()
	if _, err := db.Exec(`DROP INDEX nodes_by_repo`); err != nil {
		t.Fatal(err)
	}
	check()
}
