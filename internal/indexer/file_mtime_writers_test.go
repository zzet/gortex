package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestBatchedFileMtimeUpsertDetachesPublishedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	idx := newTestIndexer(graph.New())
	idx.fileMtimes = map[string]int64{"a.go": 1}
	published := idx.publishFileMtimes()

	fresh, stale := idx.recordFileReadVersionsBatched([]fileReadReceipt{{
		absPath:  path,
		mtimeKey: "a.go",
		readVersion: fileReadVersion{
			info:  info,
			mtime: info.ModTime().UnixNano(),
			size:  info.Size(),
			valid: true,
		},
	}})

	assert.Equal(t, []string{path}, fresh)
	assert.Empty(t, stale)
	assert.Equal(t, int64(1), published["a.go"])
	assert.Equal(t, info.ModTime().UnixNano(), idx.fileMtimes["a.go"])
}

func TestBatchedFileMtimeEvictionDetachesPublishedSnapshot(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.fileMtimes = map[string]int64{"a.go": 1, "b.go": 2}
	published := idx.publishFileMtimes()

	idx.evictDeletedFilesBatched([]string{"a.go"}, &DerivedInvalidationPlan{}, nil)

	assert.Equal(t, int64(1), published["a.go"])
	assert.NotContains(t, idx.fileMtimes, "a.go")
	assert.Equal(t, int64(2), idx.fileMtimes["b.go"])
}

func TestRefreshFileMtimeDetachesPublishedSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	idx := newTestIndexer(graph.New())
	idx.SetRootPath(root)
	key := idx.relKey(path)
	idx.fileMtimes = map[string]int64{key: 1}
	published := idx.publishFileMtimes()

	idx.RefreshFileMtime(path)

	assert.Equal(t, int64(1), published[key])
	assert.Equal(t, info.ModTime().UnixNano(), idx.fileMtimes[key])
}

func TestMultiIndexerFileMtimesReturnsOwnedCopy(t *testing.T) {
	published := map[string]int64{"a.go": 1}
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"repo": {FileMtimes: published},
	}}

	owned := mi.FileMtimes("repo")
	owned["a.go"] = 99

	assert.Equal(t, int64(1), published["a.go"])
	assert.Equal(t, int64(1), mi.repos["repo"].FileMtimes["a.go"])
}
