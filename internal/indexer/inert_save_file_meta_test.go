package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// An inert save moves a file's bytes without changing its graph, so the
// incremental reindex records the read and restates no rows. The file's
// metadata row must still describe the bytes on disk: every reader that
// proves "these are the indexed bytes" compares it with the file, and a stale
// row makes a contract baseline skip the file as moved for good, since the
// equal mtime means the census never reads it again.
func TestInertSaveRecordsTheSavedBytesInFileMeta(t *testing.T) {
	for _, mode := range []string{"core", "contract_inputs", "sqlite"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "value.go")
			writeFile(t, path, "package fixture\nfunc Value() int { return 1 }\n")
			var idx *Indexer
			if mode == "sqlite" {
				idx, _ = newSQLiteIndexer(t)
			} else {
				idx = newTestIndexer(graph.New())
			}
			idx.SetRepoPrefix("fixture")
			t.Cleanup(idx.Close)
			if mode == "contract_inputs" {
				backend := &contractCoreBackendProbe{known: true, receipts: make(map[string]*contractBoundaryReceipt), pendingReceipts: make(map[string]*contractBoundaryReceipt)}
				journal, err := newContractCoreInputJournal(context.Background(), backend)
				require.NoError(t, err)
				idx.contractCoreInputs = journal
			}
			_, err := idx.Index(root)
			require.NoError(t, err)

			saved := "package fixture\nfunc Value() int { return 1 }   \n"
			writeFile(t, path, saved)
			result, err := idx.IncrementalReindexPaths(root, []string{path})
			require.NoError(t, err)
			require.Equal(t, 1, result.DerivedInvalidation.InertFiles, "trailing spaces leave the graph unchanged")

			reader, ok := idx.graph.(graph.FileMetaPathReader)
			require.True(t, ok)
			rows, err := reader.FileMetasByPaths("fixture", []string{"fixture/value.go"})
			require.NoError(t, err)
			row := rows["fixture/value.go"]
			require.Equal(t, contentHashForSource([]byte(saved)), row.ContentHash, "the row describes the saved bytes")
			require.Equal(t, len(saved), row.Size)
			require.Equal(t, 2, row.NodeCount, "an inert save keeps the file's node count")
			require.True(t, idx.indexedFileReceiptMatches(path), "an equal-mtime replay of the saved bytes is recognised")
		})
	}
}
