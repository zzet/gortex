package analysis

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func fileOnlyDiffGit(t testing.TB, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, output)
}

func fileOnlyDiffFixture(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	fileOnlyDiffGit(t, root, "init")
	fileOnlyDiffGit(t, root, "config", "user.name", "Gortex Test")
	fileOnlyDiffGit(t, root, "config", "user.email", "test@gortex.invalid")
	for name, content := range map[string]string{
		"modified.go": "package sample\n\nfunc Before() {}\n",
		"deleted.go":  "package sample\n\nfunc Deleted() {}\n",
		"renamed.txt": "an unchanged document that moves to a new name\n",
		"empty.txt":   "",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o644))
	}
	fileOnlyDiffGit(t, root, "add", ".")
	fileOnlyDiffGit(t, root, "commit", "-m", "baseline")
	return root
}

func fileOnlyDiffChanges(t testing.TB, root string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, "modified.go"), []byte("package sample\n\nfunc After() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "added.go"), []byte("package sample\n\nfunc Added() {}\n"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(root, "deleted.go")))
	require.NoError(t, os.Remove(filepath.Join(root, "empty.txt")))
	require.NoError(t, os.Rename(filepath.Join(root, "renamed.txt"), filepath.Join(root, "moved.txt")))
	fileOnlyDiffGit(t, root, "add", "-A")
}

func TestMapGitDiffWithoutGraphPreservesFileMetadata(t *testing.T) {
	root := fileOnlyDiffFixture(t)
	fileOnlyDiffChanges(t, root)
	result, err := MapGitDiff(nil, root, "repo", "staged", "HEAD")
	require.NoError(t, err)
	require.Empty(t, result.ChangedSymbols)
	require.NotEmpty(t, result.Hunks)
	require.Equal(t, []string{"added.go", "deleted.go", "empty.txt", "modified.go", "moved.txt", "renamed.txt"}, result.ChangedFiles)
	changes := make(map[string]FileChange)
	for _, change := range result.FileChanges {
		changes[change.Path] = change
	}
	require.Equal(t, FileAdded, changes["added.go"].Kind)
	require.Equal(t, FileDeleted, changes["deleted.go"].Kind)
	require.Equal(t, FileDeleted, changes["empty.txt"].Kind)
	require.Equal(t, FileModified, changes["modified.go"].Kind)
	require.Equal(t, FileRenamed, changes["moved.txt"].Kind)
	require.Equal(t, "renamed.txt", changes["moved.txt"].PreviousPath)
}

func TestMapGitDiffWithoutGraphPreservesIntentToAdd(t *testing.T) {
	root := fileOnlyDiffFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "intent.go"), []byte("package sample\n\nfunc Intent() {}\n"), 0o644))
	fileOnlyDiffGit(t, root, "add", "-N", "intent.go")
	result, err := MapGitDiff(nil, root, "repo", "unstaged", "HEAD")
	require.NoError(t, err)
	require.Empty(t, result.ChangedSymbols)
	require.Equal(t, []string{"intent.go"}, result.ChangedFiles)
	require.Equal(t, []FileChange{{Path: "intent.go", Kind: FileAdded}}, result.FileChanges)
	require.NotEmpty(t, result.Hunks)
}

type diffFileNodeReader struct {
	graph.Reader
	nodes []*graph.Node
	reads int
}

func (r *diffFileNodeReader) GetFileNodes(string) []*graph.Node {
	r.reads++
	return r.nodes
}

func TestJoinHunksFileOnlyModeDoesNotChangeGraphMapping(t *testing.T) {
	hunks := []DiffHunk{{FilePath: "source.go", StartLine: 3, EndLine: 3}}
	files := []FileChange{{Path: "source.go", Kind: FileModified}}
	reader := &diffFileNodeReader{nodes: []*graph.Node{{
		ID: "repo/source.go::Changed", Name: "Changed", Kind: graph.KindFunction,
		FilePath: "repo/source.go", StartLine: 3, EndLine: 5,
	}}}
	mapped := joinHunksToSymbols(reader, "repo", hunks, files)
	require.Equal(t, 1, reader.reads)
	require.Len(t, mapped.ChangedSymbols, 1)
	require.Equal(t, "Changed", mapped.ChangedSymbols[0].Name)
	fileOnly := joinHunksToSymbols(nil, "repo", hunks, files)
	require.Empty(t, fileOnly.ChangedSymbols)
	require.Equal(t, mapped.Hunks, fileOnly.Hunks)
	require.Equal(t, mapped.ChangedFiles, fileOnly.ChangedFiles)
	require.Equal(t, mapped.FileChanges, fileOnly.FileChanges)
}

func TestJoinVanishedFileOnlyModeDoesNotChangeGraphMapping(t *testing.T) {
	files := []FileChange{
		{Path: "deleted.go", Kind: FileDeleted},
		{Path: "renamed.go", PreviousPath: "original.go", Kind: FileRenamed},
	}
	reader := &diffFileNodeReader{nodes: []*graph.Node{{
		ID: "repo/deleted.go::Deleted", Name: "Deleted", Kind: graph.KindFunction,
		FilePath: "repo/deleted.go", StartLine: 3, EndLine: 5,
	}}}
	mapped := joinHunksToSymbols(reader, "repo", nil, files)
	require.Equal(t, 2, reader.reads)
	require.Len(t, mapped.ChangedSymbols, 1)
	require.Equal(t, "Deleted", mapped.ChangedSymbols[0].Name)
	fileOnly := joinHunksToSymbols(nil, "repo", nil, files)
	require.Empty(t, fileOnly.ChangedSymbols)
	require.Equal(t, []string{"deleted.go", "original.go", "renamed.go"}, fileOnly.ChangedFiles)
	require.Equal(t, mapped.ChangedFiles, fileOnly.ChangedFiles)
	require.Equal(t, mapped.FileChanges, fileOnly.FileChanges)
}

func TestMapGitDiffWithoutGraphPreservesGitErrors(t *testing.T) {
	_, err := MapGitDiff(nil, t.TempDir(), "repo", "staged", "HEAD")
	require.Error(t, err)
}

func BenchmarkMapGitDiffWithoutGraph(b *testing.B) {
	root := fileOnlyDiffFixture(b)
	fileOnlyDiffChanges(b, root)
	for _, mode := range []string{"legacy_empty_graph", "file_only"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var reader graph.Reader
				if mode == "legacy_empty_graph" {
					// Test-only baseline for the former production placeholder.
					reader = graph.New()
				}
				result, err := MapGitDiff(reader, root, "repo", "staged", "HEAD")
				if err != nil || len(result.ChangedFiles) != 6 || len(result.ChangedSymbols) != 0 {
					b.Fatalf("file-only diff failed: %+v, %v", result, err)
				}
			}
		})
	}
}

func BenchmarkJoinHunksFileOnlyMode(b *testing.B) {
	hunks := []DiffHunk{{FilePath: "source.go", StartLine: 3, EndLine: 5}}
	files := []FileChange{
		{Path: "source.go", Kind: FileModified},
		{Path: "deleted.go", Kind: FileDeleted},
		{Path: "new.go", PreviousPath: "old.go", Kind: FileRenamed},
	}
	for _, mode := range []string{"legacy_empty_graph", "file_only"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var reader graph.Reader
				if mode == "legacy_empty_graph" {
					reader = graph.New()
				}
				result := joinHunksToSymbols(reader, "repo", hunks, files)
				if len(result.ChangedFiles) != 4 || len(result.ChangedSymbols) != 0 {
					b.Fatalf("file-only join failed: %+v", result)
				}
			}
		})
	}
}
