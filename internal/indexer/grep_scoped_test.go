package indexer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/search/trigram"
)

func TestGrepTextPathsRepeatedScopeDoesNotWarmOrEvictTrigramCache(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root+"/outside.txt", "needle outside\n")
	writeTestFile(t, root+"/scope/a.txt", "needle first\nneedle second\n")
	writeTestFile(t, root+"/scope/b.txt", "unmatched\nneedle third\n")
	writeTestFile(t, root+"/scope-other.txt", "needle prefix lookalike\n")
	paths := []string{"outside.txt", "scope-other.txt", "scope/a.txt", "scope/b.txt"}
	known := make(map[string]int64, len(paths))
	for _, path := range paths {
		known[path] = 1
	}
	budget := newTrigramBudget(time.Hour, 1, -1)
	budget.afterFunc = nil
	other := &Indexer{}
	released := false
	budget.touch(other, func() { released = true }, 0)
	idx := &Indexer{rootPath: root, fileMtimes: known, trigramBudgetOverride: budget}

	// Compare the complete scoped result and its ordered limit against the
	// actual indexed search, while exercising the third-demand promotion seam.
	reference := trigram.Build(root, paths)
	want := reference.GrepPaths("needle", []string{"scope"}, 0)
	require.Len(t, want, 3)
	for request := 0; request < 4; request++ {
		require.Equal(t, want, idx.GrepTextPaths("needle", []string{"scope"}, 0))
		require.Nil(t, idx.trigramSearcher, "scoped request %d must not build the repository cache", request+1)
	}
	require.Equal(t, want[:2], idx.GrepTextPaths("needle", []string{"scope"}, 2))
	require.False(t, released, "a scoped search must not evict another repository's cache")
	require.Equal(t, 1, budget.live())
	require.NotContains(t, budget.demand, idx, "scoped requests must not promote whole-repository demand")

	writeTestFile(t, root+"/scope/a.txt", "needle changed\n")
	require.Equal(t, []trigram.Match{
		{Path: "scope/a.txt", Line: 1, Text: "needle changed"},
		{Path: "scope/b.txt", Line: 2, Text: "needle third"},
	}, idx.GrepTextPaths("needle", []string{"scope"}, 0))
	require.Nil(t, idx.trigramSearcher)
}
