package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/search/trigram"
)

func TestPendingGrepExcludesFilesBeforeLimitAndKeepsNativeCache(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		maxLive := 0
		if warm {
			name = "native warm"
			maxLive = 3
		}
		t.Run(name, func(t *testing.T) {
			root, idx := toolWriteTestIndexer(t, maxLive)
			writeTestFile(t, filepath.Join(root, "main.go"), "needle pending first\nneedle pending second\n")
			writeTestFile(t, filepath.Join(root, "other.go"), "needle uncovered\n")
			if warm {
				require.NotNil(t, idx.warmTrigramSearcher())
			}
			cached := idx.trigramSearcher
			excluded := map[string]struct{}{"main.go": {}}
			want := []trigram.Match{{Path: "other.go", Line: 1, Text: "needle uncovered"}}
			require.Equal(t, want, idx.GrepTextPathsExcept("needle", nil, excluded, 1), "post-limit filtering would lose the uncovered file")
			regexp, err := idx.GrepRegexpPathsExcept("needle.*", nil, excluded, 1)
			require.NoError(t, err)
			require.Equal(t, want, regexp)
			require.Equal(t, idx.GrepTextPaths("needle", nil, 1), idx.GrepTextPathsExcept("needle", nil, nil, 1), "empty exclusions preserve the legacy path")
			require.Equal(t, want, idx.GrepTextPathsExcept("needle", []string{"other.go"}, excluded, 1))
			require.Same(t, cached, idx.trigramSearcher, "no second cache or rebuild is introduced")
		})
	}
}
