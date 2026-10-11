package store_sqlite_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestCompactReclaimsFreelist pins the boot-compaction capability end to end
// on a real file: shedding most rows leaves the file full of freelist pages
// (CompactStats must say so), Compact() must return them to the filesystem
// (page_count shrinks), and the surviving rows must come through untouched.
// Fractional assertions only — page size and per-row overhead are backend
// details this test must not encode.
func TestCompactReclaimsFreelist(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "test.sqlite")
	s, err := store_sqlite.Open(storePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Seed 60 files × 50 nodes with ~2 KiB of meta each (~6 MiB of pages),
	// then checkpoint so the rows land in the main file rather than the WAL —
	// CompactStats deliberately measures only the main file.
	const files, perFile = 60, 50
	pad := strings.Repeat("x", 2048)
	nodes := make([]*graph.Node, 0, files*perFile)
	for f := 0; f < files; f++ {
		path := fmt.Sprintf("p/f%03d.go", f)
		for n := 0; n < perFile; n++ {
			nodes = append(nodes, &graph.Node{
				ID:       fmt.Sprintf("%s::N%d", path, n),
				Kind:     graph.KindFunction,
				Name:     fmt.Sprintf("N%d", n),
				FilePath: path,
				Meta:     map[string]any{"pad": pad},
			})
		}
	}
	// One setup transaction keeps all fixture rows while avoiding repeated
	// versions of the same index pages in the initial WAL. CheckpointWAL
	// still uses its ordinary production deadline.
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	require.Equal(t, files*perFile, s.NodeCount(), "all setup rows must be present")
	seedWAL, err := os.Stat(storePath + "-wal")
	require.NoError(t, err)
	t.Logf("compaction setup: nodes=%d WAL_seed_bytes=%d", files*perFile, seedWAL.Size())
	require.NoError(t, s.CheckpointWAL())
	checkpointedWAL, err := os.Stat(storePath + "-wal")
	require.NoError(t, err)
	t.Logf("compaction setup: WAL_checkpointed_bytes=%d", checkpointedWAL.Size())
	_, totalSeeded := s.CompactStats()
	require.Greater(t, totalSeeded, int64(3<<20), "sanity: seeding must produce a multi-MiB main file")

	// Shed all but one file, checkpoint again so the deletions reach the main
	// file's freelist.
	for f := 1; f < files; f++ {
		s.EvictFile(fmt.Sprintf("p/f%03d.go", f))
	}
	require.NoError(t, s.CheckpointWAL())

	freeBefore, totalBefore := s.CompactStats()
	assert.Greater(t, freeBefore*3, totalBefore,
		"after shedding ~98%% of rows the freelist must dominate the file (free=%d total=%d)", freeBefore, totalBefore)
	assert.Greater(t, freeBefore, int64(1<<20), "freelist must be at least MiB-scale to make the shrink observable")

	require.NoError(t, s.Compact())

	freeAfter, totalAfter := s.CompactStats()
	assert.Less(t, totalAfter, totalBefore/2,
		"VACUUM must return the dead majority to the filesystem (before=%d after=%d)", totalBefore, totalAfter)
	assert.Less(t, freeAfter, totalBefore/10,
		"post-VACUUM freelist must be near empty (free=%d)", freeAfter)

	// Survivors intact, evicted rows gone.
	assert.Equal(t, perFile, s.NodeCount(), "compaction must not change the row count")
	kept := s.GetNode("p/f000.go::N0")
	require.NotNil(t, kept, "kept row must survive VACUUM")
	assert.Equal(t, pad, kept.Meta["pad"], "kept row's meta blob must round-trip through VACUUM")
	assert.Nil(t, s.GetNode("p/f001.go::N0"), "evicted row must stay gone")

	// The store stays fully writable after the rewrite.
	s.AddNode(&graph.Node{ID: "p/new.go::After", Kind: graph.KindFunction, Name: "After", FilePath: "p/new.go"})
	assert.NotNil(t, s.GetNode("p/new.go::After"))
}
