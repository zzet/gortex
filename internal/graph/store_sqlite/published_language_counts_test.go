package store_sqlite

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestPublishedRepoLanguageCountsMemoizesAboveTheBaseOnly pins that the
// published-generation count equals RepoLanguageCounts, is shared by every
// handle over the store core (a later row change on that generation is not
// re-read: the caller asserts the generation is immutable), and that the
// mutable base generation is never memoized.
func TestPublishedRepoLanguageCountsMemoizesAboveTheBaseOnly(t *testing.T) {
	s, _ := openTempStore(t)
	write := func(generation int64, ids ...string) {
		t.Helper()
		var nodes []*graph.Node
		for _, id := range ids {
			nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"})
		}
		if err := s.AtGeneration(generation).AddBatchChecked(nodes, nil); err != nil {
			t.Fatalf("write generation %d: %v", generation, err)
		}
	}
	write(0, "repo/a.go::A", "repo/a.go::B")
	write(7, "repo/a.go::C")

	want := s.AtGeneration(7).RepoLanguageCounts([]string{"repo"})["repo"]
	got := s.AtGeneration(7).PublishedRepoLanguageCounts("repo")
	if !reflect.DeepEqual(got, want) || got["go"] != 1 {
		t.Fatalf("published count %v, want %v", got, want)
	}
	write(7, "repo/a.go::D")
	if again := s.AtGeneration(7).PublishedRepoLanguageCounts("repo"); again["go"] != 1 {
		t.Fatalf("a second handle re-read the generation: %v", again)
	}
	if base := s.AtGeneration(0).PublishedRepoLanguageCounts("repo"); base["go"] != 2 {
		t.Fatalf("base count %v, want go=2", base)
	}
	write(0, "repo/a.go::E")
	if base := s.AtGeneration(0).PublishedRepoLanguageCounts("repo"); base["go"] != 3 {
		t.Fatalf("base generation was memoized: %v", base)
	}
}
