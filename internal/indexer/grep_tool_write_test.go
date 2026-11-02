package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/search/trigram"
)

// toolWriteTestIndexer indexes main.go and other.go with a trigram budget
// that builds on first demand, or never when maxLive is zero.
func toolWriteTestIndexer(t *testing.T, maxLive int) (string, *Indexer) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Original() int { return 0 }\n")
	writeTestFile(t, filepath.Join(dir, "other.go"), "package main\n\nfunc Other() {}\n")
	return dir, &Indexer{
		rootPath:              dir,
		fileMtimes:            map[string]int64{"main.go": 1, "other.go": 1},
		trigramBudgetOverride: newTestTrigramBudget(time.Hour, maxLive, &now),
	}
}

func requireOneMatch(t *testing.T, matches []trigram.Match, path string, line int) {
	t.Helper()
	require.Len(t, matches, 1)
	require.Equal(t, path, matches[0].Path)
	require.Equal(t, line, matches[0].Line)
}

// TestTextSearchSeesToolWriteBeforeReindex covers a file a tool has written
// and no index pass has read yet: every text search answers from its new
// bytes, whether a live searcher serves it or a streaming scan does.
func TestTextSearchSeesToolWriteBeforeReindex(t *testing.T) {
	ctx := context.Background()

	t.Run("a live searcher patches an edited file in every search kind", func(t *testing.T) {
		dir, idx := toolWriteTestIndexer(t, 3)
		require.NotNil(t, idx.warmTrigramSearcher())
		built, gen := idx.trigramSearcher, idx.indexGen.Load()
		// No literal contains another, so each old line is gone, not shadowed.
		searches := []struct {
			literal string
			search  func(literal string) []trigram.Match
		}{
			{"EditSeenByGrepText", func(literal string) []trigram.Match { return idx.GrepText(literal, 10) }},
			{"EditSeenByBoundedText", func(literal string) []trigram.Match {
				m, _ := idx.GrepTextBounded(ctx, literal, 10, 0)
				return m
			}},
			{"EditSeenByBoundedLiteral", func(literal string) []trigram.Match {
				m, _ := idx.GrepLiteralBounded(ctx, literal, 10, 0)
				return m
			}},
			{"EditSeenByRegexp", func(literal string) []trigram.Match {
				m, err := idx.GrepRegexp(literal+`\(\)`, "", 10)
				require.NoError(t, err)
				return m
			}},
		}
		previous := "Original"
		for _, tc := range searches {
			writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc "+tc.literal+"() int { return 0 }\n")
			idx.noteToolWrite("main.go")
			requireOneMatch(t, tc.search(tc.literal), "main.go", 3)
			require.Empty(t, tc.search(previous), tc.literal+": the replaced line is gone")
			require.Empty(t, idx.toolWrites.snapshot(), tc.literal+": a tracked file's note retires once applied")
			previous = tc.literal
		}
		require.Same(t, built, idx.trigramSearcher, "a tool write patches, it never rebuilds")
		require.Equal(t, gen, idx.indexGen.Load())
	})

	t.Run("a new file is found before any pass tracks it, then handed to the pass", func(t *testing.T) {
		dir, idx := toolWriteTestIndexer(t, 3)
		require.NotNil(t, idx.warmTrigramSearcher())
		writeTestFile(t, filepath.Join(dir, "added.go"), "package main\n\nfunc AddedByTool() {}\n")
		idx.noteToolWrite("added.go")

		requireOneMatch(t, idx.GrepText("AddedByTool", 10), "added.go", 3)
		// A path-scoped search always streams the known files.
		requireOneMatch(t, idx.GrepTextPaths("AddedByTool", []string{"added.go"}, 10), "added.go", 3)
		require.Contains(t, idx.toolWrites.snapshot(), "added.go", "an untracked file keeps its note")

		// The pass tracks the file: the next search retires its note.
		idx.mtimeMu.Lock()
		idx.fileMtimes["added.go"] = 2
		idx.mtimeMu.Unlock()
		idx.noteTrigramDirty("added.go")
		idx.indexGen.Add(1)
		requireOneMatch(t, idx.GrepText("AddedByTool", 10), "added.go", 3)
		require.Empty(t, idx.toolWrites.snapshot())
	})

	t.Run("an edit the pass has read is found before the pass moves the generation", func(t *testing.T) {
		dir, idx := toolWriteTestIndexer(t, 3)
		require.NotNil(t, idx.warmTrigramSearcher())
		writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc EditedByTool() int { return 0 }\n")
		idx.noteToolWrite("main.go")
		// The pass has marked the file dirty, but the dirty set applies only
		// once the generation moves, so this search has the note alone.
		idx.noteTrigramDirty("main.go")
		requireOneMatch(t, idx.GrepText("EditedByTool", 10), "main.go", 3)
		require.Empty(t, idx.GrepText("Original", 10))
		idx.indexGen.Add(1)
		requireOneMatch(t, idx.GrepText("EditedByTool", 10), "main.go", 3)
		require.Empty(t, idx.toolWrites.snapshot())
	})

	t.Run("a streaming search walks a new file", func(t *testing.T) {
		dir, idx := toolWriteTestIndexer(t, 0)
		writeTestFile(t, filepath.Join(dir, "added.go"), "package main\n\nfunc AddedByTool() {}\n")
		idx.noteToolWrite("added.go")
		requireOneMatch(t, idx.GrepText("AddedByTool", 10), "added.go", 3)
		require.Nil(t, idx.trigramSearcher, "the budget forbids a build")

		// With no searcher to apply it, the note retires once the pass tracks
		// the file.
		idx.mtimeMu.Lock()
		idx.fileMtimes["added.go"] = 2
		idx.mtimeMu.Unlock()
		idx.noteTrigramDirty("added.go")
		idx.indexGen.Add(1)
		require.Empty(t, idx.toolWrites.snapshot())
		requireOneMatch(t, idx.GrepText("AddedByTool", 10), "added.go", 3)
	})

	t.Run("a deleted file drops out", func(t *testing.T) {
		dir, idx := toolWriteTestIndexer(t, 3)
		requireOneMatch(t, idx.GrepText("func Other", 10), "other.go", 3)
		require.NoError(t, os.Remove(filepath.Join(dir, "other.go")))
		idx.noteToolWrite("other.go")
		require.Empty(t, idx.GrepText("func Other", 10))
		require.Empty(t, idx.toolWrites.snapshot(), "a missing file's note retires")
	})

	t.Run("a note made again while an older one is applied survives it", func(t *testing.T) {
		var set toolWriteSet
		set.note("main.go")
		applied := set.snapshot()
		set.note("main.go")
		set.retire(applied, func(string) bool { return true })
		require.Contains(t, set.snapshot(), "main.go")
	})
}
