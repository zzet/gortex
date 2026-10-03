package analysis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type cancelFileJoinReader struct {
	graph.Reader
	cancel context.CancelFunc
	calls  int
}

func (r *cancelFileJoinReader) GetFileNodesContext(ctx context.Context, file string) []*graph.Node {
	r.calls++
	r.cancel()
	return r.GetFileNodes(file)
}

func TestMapGitDiffContextCancelledBeforeGit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := MapGitDiffContext(ctx, nil, t.TempDir(), "", "unstaged", "main")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
}

func TestJoinHunksToSymbolsContextStopsAfterFileLookup(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "A", Name: "A", Kind: graph.KindFunction, FilePath: "a.go", StartLine: 1, EndLine: 4})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &cancelFileJoinReader{Reader: g, cancel: cancel}
	result, err := joinHunksToSymbolsContext(ctx, reader, "", []DiffHunk{
		{FilePath: "a.go", StartLine: 2, EndLine: 2},
		{FilePath: "b.go", StartLine: 2, EndLine: 2},
	}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.Equal(t, 1, reader.calls)
}

func TestJoinHunksToSymbolsContextMatchesCompatibilityPath(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "A", Name: "A", Kind: graph.KindFunction, FilePath: "a.go", StartLine: 1, EndLine: 4})
	hunks := []DiffHunk{{FilePath: "a.go", StartLine: 2, EndLine: 2}}
	files := []FileChange{{Path: "a.go", Kind: FileModified}, {Path: "docs.md", Kind: FileDeleted}}
	want := joinHunksToSymbols(g, "", hunks, files)
	got, err := joinHunksToSymbolsContext(t.Context(), g, "", hunks, files)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
