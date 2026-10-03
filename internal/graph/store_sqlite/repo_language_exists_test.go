package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRepoHasLanguageMatchesFileCountsAndCancellation(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	for _, gen := range []int64{0, 3} {
		for i, row := range []struct{ repo, lang, kind, class string }{
			{"", "go", "function", ""}, {"go", "go", "function", ""}, {"module", "go", "module", ""},
			{"content", "go", "doc", "content"}, {"symbol", "go", "doc", "symbol"}, {"python", "python", "function", ""},
			{"empty", "", "function", ""}, {"nullable", "go", "doc", ""},
		} {
			if _, err := db.Exec(`INSERT INTO nodes(id,view_gen,repo_prefix,file_path,language,kind,name,data_class,meta) VALUES(?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("n-%d", i), gen, row.repo, "file", row.lang, row.kind, "", row.class, []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`INSERT INTO nodes(id,view_gen,repo_prefix,file_path,language,kind,name,meta) VALUES('derived',3,'derived','file','go','function','',?)`, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	check := func() {
		for _, gen := range []int64{0, 3, 9} {
			s := &Store{storeCore: &storeCore{db: db}, viewGen: gen}
			for _, repo := range []string{"", "go", "module", "content", "symbol", "python", "empty", "nullable", "derived", "missing"} {
				for _, lang := range []string{"go", "python", ""} {
					want := false
					for _, row := range s.RepoLanguageFileCounts([]string{repo}) {
						if row.Language == lang && row.Count > 0 {
							want = true
						}
					}
					got, err := s.RepoHasLanguageContext(nil, repo, lang)
					if err != nil || got != want {
						t.Fatalf("gen%d repo%q lang%q got%t/%v want%t", gen, repo, lang, got, err, want)
					}
				}
			}
		}
	}
	check()
	if _, err := db.Exec(`DROP INDEX nodes_by_repo`); err != nil {
		t.Fatal(err)
	}
	check()
	s := &Store{storeCore: &storeCore{db: db}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := s.RepoHasLanguageContext(ctx, "go", "go"); got || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query got%t/%v", got, err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if got, err := s.RepoHasLanguageContext(ctx, "go", "go"); got || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting query got%t/%v", got, err)
	}
}
