package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

func newObservedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.WarnLevel)
	return zap.New(core), logs
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
	}
}

// A repo whose ignore rules swallowed every file used to index silently:
// zero files, no error, no warning, and every downstream query answering
// "not found" with full confidence (#624). The walk must now say so, and
// name the pattern that did it.
func TestIndex_WarnsWhenEveryCandidateFileIsExcluded(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"pkg/frame.go": "package pkg\n\nfunc DataFrame() {}\n",
		"main.go":      "package main\n\nfunc main() {}\n",
	})

	logger, logs := newObservedLogger()
	idx := New(graph.New(), newTestRegistry(), config.IndexConfig{
		Workers: 1,
		Exclude: []string{"node_modules/", "everything-eater/"},
	}, logger)
	idx.SetRootPath(dir)
	// An exclude that swallows the whole tree, in the shape that caused
	// the report: one pattern, no negation, applied at the root.
	idx.SetExcludePatterns([]string{"*"})

	res, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 0, res.FileCount, "precondition: the walk admits nothing")

	entries := logs.FilterMessageSnippet("no source files were indexed").All()
	require.Len(t, entries, 1, "an empty index must warn exactly once")
	fields := entries[0].ContextMap()
	require.Equal(t, "*", fields["pattern"], "the warning must name the responsible pattern")
	require.Contains(t, fields, "example_file")
	require.Contains(t, fields["excluded_by"], "exclude list")
}

// A per-directory ignore file is the other door into the same failure, so
// it has to be attributed too — naming the directory, since the pattern
// alone would not say which file it came from.
func TestIndex_WarnsAndNamesPerDirectoryIgnoreFile(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"pkg/frame.go":  "package pkg\n\nfunc DataFrame() {}\n",
		".gortexignore": "*.go\n",
	})

	logger, logs := newObservedLogger()
	idx := New(graph.New(), newTestRegistry(), config.IndexConfig{Workers: 1}, logger)
	idx.SetRootPath(dir)

	res, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 0, res.FileCount)

	entries := logs.FilterMessageSnippet("no source files were indexed").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "*.go", fields["pattern"])
	require.Contains(t, fields["excluded_by"], "per-directory ignore file")
}

// A repo that genuinely holds no source is not a failure, and must not
// cry wolf — otherwise the warning stops meaning anything.
func TestIndex_DoesNotWarnWhenRepoHasNoSource(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"notes.unknownext": "nothing to parse\n"})

	logger, logs := newObservedLogger()
	idx := New(graph.New(), newTestRegistry(), config.IndexConfig{Workers: 1}, logger)
	idx.SetRootPath(dir)

	res, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 0, res.FileCount)
	require.Empty(t, logs.FilterMessageSnippet("no source files were indexed").All())
}

// The happy path pays nothing and says nothing.
func TestIndex_DoesNotWarnWhenFilesAreIndexed(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})

	logger, logs := newObservedLogger()
	idx := New(graph.New(), newTestRegistry(), config.IndexConfig{Workers: 1}, logger)
	idx.SetRootPath(dir)

	res, err := idx.Index(dir)
	require.NoError(t, err)
	require.Positive(t, res.FileCount)
	require.Empty(t, logs.FilterMessageSnippet("no source files were indexed").All())
}
