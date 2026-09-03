package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestFileMetasByPathsReturnsOnlyRequestedPrimaryKeys(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "files.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const rowCount = 170
	repoRows := make([]graph.FileMetaRow, 0, rowCount)
	otherRows := make([]graph.FileMetaRow, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		filePath := fmt.Sprintf("shared/file-%03d.go", i)
		repoRows = append(repoRows, graph.FileMetaRow{
			FilePath: filePath, ContentHash: fmt.Sprintf("repo-%03d", i), Size: i + 1, NodeCount: i + 2,
		})
		otherRows = append(otherRows, graph.FileMetaRow{
			FilePath: filePath, ContentHash: fmt.Sprintf("other-%03d", i), Size: i + 3, NodeCount: i + 4,
		})
	}
	require.NoError(t, store.SetFileMetas("repo", repoRows))
	require.NoError(t, store.SetFileMetas("other", otherRows))

	// 125 keys force two bounded queries (fileMetaChunk is 80). Both repos own
	// the same paths, so every returned hash also proves repo-prefix isolation.
	requested := make([]string, 0, 126)
	expected := make(map[string]graph.FileMetaRow, 125)
	for i := 0; i < 125; i++ {
		requested = append(requested, repoRows[i].FilePath)
		expected[repoRows[i].FilePath] = repoRows[i]
	}
	requested = append(requested, "shared/missing.go")
	got, err := store.FileMetasByPaths("repo", requested)
	require.NoError(t, err)
	require.Equal(t, expected, got)
	for _, row := range got {
		require.True(t, strings.HasPrefix(row.ContentHash, "repo-"))
	}

	empty, err := store.FileMetasByPaths("repo", nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	planRows, err := store.db.Query(
		`EXPLAIN QUERY PLAN SELECT file_path, content_hash, size, node_count, errors
		 FROM files WHERE view_gen = ? AND repo_prefix = ? AND file_path IN (?, ?, ?)`,
		store.viewGen, "repo", requested[0], requested[80], requested[124],
	)
	require.NoError(t, err)
	defer planRows.Close()
	var plan []string
	for planRows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, planRows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, planRows.Err())
	require.Contains(t, strings.ToUpper(strings.Join(plan, "\n")), "PRIMARY KEY")
}
