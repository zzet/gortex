package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestHasLanguageUsesCoveringLanguageProbe(t *testing.T) {
	// A small store can choose this index even without the pin. Require the
	// explicit choice as well: large-store statistics can favor a competing
	// index, which is the regression this query must remain insulated from.
	if !strings.Contains(hasLanguageSQL, "INDEXED BY nodes_by_repo_language_name") {
		t.Fatal("language gate must seek the covering language index: explicit index pin missing")
	}
	s := openScopedProjectionTestStore(t)
	// Exercise the production query against the complete schema, including the
	// competing nodes_name_candidates index. Avoid wall-clock assertions.
	plan := scopedProjectionPlan(t, s, hasLanguageSQL, "missing", s.viewGen)
	if !strings.Contains(plan, "COVERING INDEX nodes_by_repo_language_name") ||
		!strings.Contains(plan, "language=?") || strings.Contains(plan, "nodes_name_candidates") {
		t.Fatalf("language gate must seek the covering language index:\n%s", plan)
	}
}

func TestHasLanguageHonorsGenerationAndMutation(t *testing.T) {
	s := openScopedProjectionTestStore(t)
	s.AddNode(&graph.Node{ID: "solo", Name: "Solo", Kind: graph.KindFunction, Language: "go"})
	// Prefix discovery spans generations, but presence must not. Seed an inactive
	// generation with its own prefix, plus the same ID as the active generation.
	if _, err := s.writerDB.Exec(`INSERT INTO nodes (id, kind, name, language, repo_prefix, view_gen, file_path) VALUES
		('solo', 'function', 'Other', 'ruby', '', 41, ''),
		('other', 'function', 'Other', 'python', 'other', 41, '')`); err != nil {
		t.Fatal(err)
	}
	if !s.HasLanguage("go") || s.HasLanguage("ruby") || s.HasLanguage("python") {
		t.Fatal("language presence crossed the active generation boundary")
	}
	if _, err := s.writerDB.Exec(`UPDATE nodes SET language = 'rust' WHERE id = 'solo' AND view_gen = ?`, s.viewGen); err != nil {
		t.Fatal(err)
	}
	if s.HasLanguage("go") || !s.HasLanguage("rust") {
		t.Fatal("language presence did not observe a changed definition")
	}
	if _, err := s.writerDB.Exec(`DELETE FROM nodes WHERE id = 'solo' AND view_gen = ?`, s.viewGen); err != nil {
		t.Fatal(err)
	}
	if s.HasLanguage("rust") || s.HasLanguage("python") {
		t.Fatal("deleted or inactive language remained present")
	}
}

func TestHasLanguageWhileLanguageIndexIsDeferred(t *testing.T) {
	s := openScopedProjectionTestStore(t)
	s.AddNode(&graph.Node{ID: "solo", Name: "Solo", Kind: graph.KindFunction, Language: "go"})
	s.AddNode(&graph.Node{ID: "unnamed", Kind: graph.KindFile, Language: "ruby"})
	if _, err := s.writerDB.Exec(`DROP INDEX nodes_by_repo_language_name`); err != nil {
		t.Fatal(err)
	}
	if !s.HasLanguage("go") || s.HasLanguage("ruby") || s.HasLanguage("missing") {
		t.Fatal("deferred-index fallback changed language-presence semantics")
	}
}

func BenchmarkHasLanguageRepositoryProbe(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "language.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	var nodes []*graph.Node
	for repo := range 4 {
		for i := range 3000 {
			nodes = append(nodes, &graph.Node{
				ID: fmt.Sprintf("repo%d::symbol%d", repo, i), Name: fmt.Sprintf("symbol%d", i),
				Kind: graph.KindFunction, Language: "go", RepoPrefix: fmt.Sprintf("repo%d", repo),
			})
		}
	}
	s.AddBatch(nodes, nil)
	for _, language := range []string{"go", "missing"} {
		b.Run(language, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if got := s.HasLanguage(language); got != (language == "go") {
					b.Fatalf("HasLanguage(%q) = %v", language, got)
				}
			}
		})
	}
}
