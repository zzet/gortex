package indexer

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
)

type countingSidecarGraph struct {
	*graph.Graph
	mu           sync.Mutex
	fileDeletes  int
	fileSets     int
	constDeletes int
	constSets    int
}

func (s *countingSidecarGraph) DeleteFileMetasByFiles(repoPrefix string, files []string) error {
	s.mu.Lock()
	s.fileDeletes++
	s.mu.Unlock()
	return s.Graph.DeleteFileMetasByFiles(repoPrefix, files)
}

func (s *countingSidecarGraph) SetFileMetas(repoPrefix string, rows []graph.FileMetaRow) error {
	s.mu.Lock()
	s.fileSets++
	s.mu.Unlock()
	return s.Graph.SetFileMetas(repoPrefix, rows)
}

func (s *countingSidecarGraph) DeleteConstantValuesByFiles(repoPrefix string, files []string) error {
	s.mu.Lock()
	s.constDeletes++
	s.mu.Unlock()
	return s.Graph.DeleteConstantValuesByFiles(repoPrefix, files)
}

func (s *countingSidecarGraph) BulkSetConstantValues(repoPrefix string, rows []graph.ConstantValueRow) error {
	s.mu.Lock()
	s.constSets++
	s.mu.Unlock()
	return s.Graph.BulkSetConstantValues(repoPrefix, rows)
}

func (s *countingSidecarGraph) writeCounts() (int, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fileDeletes, s.fileSets, s.constDeletes, s.constSets
}

func TestParseSidecarBatchBoundsWritesAndPreservesRows(t *testing.T) {
	store := &countingSidecarGraph{Graph: graph.New()}
	idx := &Indexer{graph: store, repoPrefix: "repo"}
	batch := newParseSidecarBatch(idx)

	const fileCount = 600
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := worker; i < fileCount; i += 8 {
				rel := fmt.Sprintf("pkg/f%03d.go", i)
				batch.add(rel, []byte(fmt.Sprintf("const C%d = %d", i, i)), &parser.ExtractionResult{
					Nodes: []*graph.Node{{ID: rel + "::C", Kind: graph.KindConstant}},
					ConstValues: []parser.ConstValue{{
						NodeID: rel + "::C", FilePath: rel, Value: fmt.Sprint(i),
					}},
				})
			}
		}()
	}
	wg.Wait()
	batch.flush()

	fileDeletes, fileSets, constDeletes, constSets := store.writeCounts()
	require.Equal(t, 3, fileDeletes)
	require.Equal(t, 3, fileSets)
	require.Equal(t, 3, constDeletes)
	require.Equal(t, 3, constSets)

	fileRows, err := store.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Len(t, fileRows, fileCount)
	ids := make([]string, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		ids = append(ids, fmt.Sprintf("repo/pkg/f%03d.go::C", i))
	}
	values, err := store.ConstantValuesByNodeIDs(ids)
	require.NoError(t, err)
	require.Len(t, values, fileCount)
	require.Equal(t, "42", values["repo/pkg/f042.go::C"])
}

func TestPersistShadowCompactSidecarsReplacesSQLiteProjection(t *testing.T) {
	disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, disk.Close()) })

	require.NoError(t, disk.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "repo/stale.go"}}))
	require.NoError(t, disk.BulkSetConstantValues("repo", []graph.ConstantValueRow{{
		NodeID: "repo/stale.go::C", FilePath: "repo/stale.go", Value: "stale",
	}}))
	require.NoError(t, disk.BulkSetCloneCorpus("repo", []graph.CloneCorpusRow{{
		NodeID: "repo/stale.go::F", Shingles: []uint64{9}, Signature: "stale", Finalized: true,
	}}))

	shadow := graph.New()
	require.NoError(t, shadow.SetFileMetas("repo", []graph.FileMetaRow{
		{FilePath: "repo/a.go", ContentHash: "a", NodeCount: 2},
		{FilePath: "repo/b.go", ContentHash: "b", NodeCount: 3},
	}))
	require.NoError(t, shadow.BulkSetConstantValues("repo", []graph.ConstantValueRow{
		{NodeID: "repo/a.go::A", FilePath: "repo/a.go", Value: "one"},
		{NodeID: "repo/b.go::B", FilePath: "repo/b.go", Value: "two"},
	}))
	require.NoError(t, shadow.BulkSetCloneCorpus("repo", []graph.CloneCorpusRow{
		{NodeID: "repo/a.go::A", Shingles: []uint64{1, 2}, Signature: "sig-a", TokenCount: 12, Finalized: true},
		{NodeID: "repo/b.go::B", Shingles: []uint64{3, 4}, Signature: "sig-b", TokenCount: 14, Finalized: true},
	}))
	require.NoError(t, resetShadowCloneCorpus(disk, "repo"))
	require.NoError(t, persistShadowCompactSidecars(shadow, disk, "repo"))

	fileRows, err := disk.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Equal(t, []graph.FileMetaRow{
		{FilePath: "repo/a.go", ContentHash: "a", NodeCount: 2},
		{FilePath: "repo/b.go", ContentHash: "b", NodeCount: 3},
	}, fileRows)
	values, err := disk.ConstantValuesByNodeIDs([]string{
		"repo/a.go::A", "repo/b.go::B", "repo/stale.go::C",
	})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"repo/a.go::A": "one",
		"repo/b.go::B": "two",
	}, values)
	cloneRows, err := disk.CloneCorpusPage("repo", "", 10)
	require.NoError(t, err)
	require.Len(t, cloneRows, 2)
	require.Equal(t, "repo/a.go::A", cloneRows[0].NodeID)
	require.Equal(t, "sig-a", cloneRows[0].Signature)
	require.Equal(t, "repo/b.go::B", cloneRows[1].NodeID)

	// An authoritative empty shadow must clear, not preserve, old rows.
	require.NoError(t, resetShadowCloneCorpus(disk, "repo"))
	require.NoError(t, persistShadowCompactSidecars(graph.New(), disk, "repo"))
	fileRows, err = disk.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Empty(t, fileRows)
	values, err = disk.ConstantValuesByNodeIDs([]string{"repo/a.go::A", "repo/b.go::B"})
	require.NoError(t, err)
	require.Empty(t, values)
	cloneRows, err = disk.CloneCorpusPage("repo", "", 10)
	require.NoError(t, err)
	require.Empty(t, cloneRows)
}

func TestPersistShadowCompactSidecarChunkAppendsPriorChunks(t *testing.T) {
	disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, disk.Close()) })

	persistChunk := func(filePath, nodeID, value string) {
		shadow := graph.New()
		require.NoError(t, shadow.SetFileMetas("repo", []graph.FileMetaRow{{
			FilePath: filePath, ContentHash: value, NodeCount: 1,
		}}))
		require.NoError(t, shadow.BulkSetConstantValues("repo", []graph.ConstantValueRow{{
			NodeID: nodeID, FilePath: filePath, Value: value,
		}}))
		require.NoError(t, persistShadowCompactSidecarChunk(shadow, disk, "repo"))
	}
	persistChunk("repo/first.go", "repo/first.go::C", "first")
	persistChunk("repo/second.go", "repo/second.go::C", "second")

	fileRows, err := disk.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Len(t, fileRows, 2)
	values, err := disk.ConstantValuesByNodeIDs([]string{
		"repo/first.go::C", "repo/second.go::C",
	})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"repo/first.go::C": "first", "repo/second.go::C": "second",
	}, values)
}

func TestShadowCompactSidecarsKeepNodeDrainedCloneCorpus(t *testing.T) {
	disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, disk.Close()) })
	require.NoError(t, disk.BulkSetCloneCorpus("repo", []graph.CloneCorpusRow{{NodeID: "repo/stale.go::F", Shingles: []uint64{9}}}))
	require.NoError(t, disk.BulkSetCloneCorpus("other", []graph.CloneCorpusRow{{NodeID: "other/kept.go::F", Shingles: []uint64{8}, TokenCount: 7}}))
	require.NoError(t, resetShadowCloneCorpus(disk, "repo"))
	// This is the same pending parser payload the real shadow node drain
	// writes before the deferred global clone pass can finalize it.
	require.NoError(t, disk.AddBatchChecked([]*graph.Node{{
		ID: "repo/current.go::F", RepoPrefix: "repo", Kind: graph.KindFunction,
		Meta: map[string]any{"clone_shingles": []uint64{1, 2, 3}, "clone_tokens": 15},
	}}, nil))
	require.NoError(t, persistShadowCompactSidecars(graph.New(), disk, "repo"))
	rows, err := disk.CloneCorpusPage("repo", "", 10)
	require.NoError(t, err)
	// The SQLite pager scopes by the exact input prefix but does not repeat
	// that known prefix in each returned row's optional RepoPrefix field.
	require.Equal(t, []graph.CloneCorpusRow{{NodeID: "repo/current.go::F", Shingles: []uint64{1, 2, 3}, TokenCount: 15}}, rows)
	other, err := disk.CloneCorpusPage("other", "", 10)
	require.NoError(t, err)
	require.Equal(t, []graph.CloneCorpusRow{{NodeID: "other/kept.go::F", Shingles: []uint64{8}, TokenCount: 7}}, other)
}
