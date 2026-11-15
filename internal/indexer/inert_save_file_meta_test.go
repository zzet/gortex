package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
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
				useContractCoreInputProbe(t, idx)
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

func useContractCoreInputProbe(t *testing.T, idx *Indexer) {
	t.Helper()
	backend := &contractCoreBackendProbe{known: true, receipts: make(map[string]*contractBoundaryReceipt), pendingReceipts: make(map[string]*contractBoundaryReceipt)}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	idx.contractCoreInputs = journal
}

// fileMetaWriteFault fails SetFileMetas while armed and leaves deletes
// alone, as a transient upsert failure would.
type fileMetaWriteFault struct {
	mu      sync.Mutex
	armed   bool
	onWrite func(rows []graph.FileMetaRow)
}

func (f *fileMetaWriteFault) set(armed bool, onWrite func(rows []graph.FileMetaRow)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.onWrite = armed, onWrite
}

func (f *fileMetaWriteFault) write(rows []graph.FileMetaRow, next func() error) error {
	f.mu.Lock()
	armed, onWrite := f.armed, f.onWrite
	f.mu.Unlock()
	if armed {
		return errors.New("injected file metadata write failure")
	}
	if onWrite != nil {
		onWrite(rows)
	}
	return next()
}

type fileMetaFaultGraph struct {
	*graph.Graph
	fault *fileMetaWriteFault
}

func (g *fileMetaFaultGraph) SetFileMetas(repoPrefix string, rows []graph.FileMetaRow) error {
	return g.fault.write(rows, func() error { return g.Graph.SetFileMetas(repoPrefix, rows) })
}

type fileMetaFaultSQLite struct {
	*store_sqlite.Store
	fault *fileMetaWriteFault
}

func (s *fileMetaFaultSQLite) SetFileMetas(repoPrefix string, rows []graph.FileMetaRow) error {
	return s.fault.write(rows, func() error { return s.Store.SetFileMetas(repoPrefix, rows) })
}

// A failed metadata write must not cost an inert file its prior row or
// record its read: the save is reported failed and stays stale, and the
// retry writes the row before it advances the mtime.
func TestInertSaveFileMetaWriteFailureWithholdsFreshness(t *testing.T) {
	for _, mode := range []string{"core", "contract_inputs", "sqlite"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "value.go")
			writeFile(t, path, "package fixture\nfunc Value() int { return 1 }\n")
			fault := &fileMetaWriteFault{}
			var store graph.Store = &fileMetaFaultGraph{Graph: graph.New(), fault: fault}
			if mode == "sqlite" {
				sqlite, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = sqlite.Close() })
				store = &fileMetaFaultSQLite{Store: sqlite, fault: fault}
			}
			idx := newTestIndexer(store)
			idx.SetRepoPrefix("fixture")
			t.Cleanup(idx.Close)
			if mode == "contract_inputs" {
				useContractCoreInputProbe(t, idx)
			}
			_, err := idx.Index(root)
			require.NoError(t, err)

			const graphPath = "fixture/value.go"
			reader, ok := idx.graph.(graph.FileMetaPathReader)
			require.True(t, ok)
			readRow := func() (graph.FileMetaRow, bool) {
				rows, err := reader.FileMetasByPaths("fixture", []string{graphPath})
				require.NoError(t, err)
				row, found := rows[graphPath]
				return row, found
			}
			prior, found := readRow()
			require.True(t, found)
			key := idx.relKey(path)
			priorMtime := idx.FileMtimes()[key]

			saved := "package fixture\nfunc Value() int { return 1 }   \n"
			bumpMtime(t, path, saved)
			fault.set(true, nil)
			result, err := idx.IncrementalReindexPaths(root, []string{path})
			require.NoError(t, err)
			// The pass retries a failed file once, inert again.
			require.NotZero(t, result.DerivedInvalidation.InertFiles, "trailing spaces leave the graph unchanged")
			row, found := readRow()
			require.True(t, found, "a failed write keeps the prior row")
			require.Equal(t, prior, row)
			require.Contains(t, result.FailedFiles, path, "the save is reported failed")
			require.Contains(t, idx.fileIndexFailurePaths(), graphPath, "the failure is kept for the retry")
			require.ErrorContains(t, idx.fileIndexFailureError(path), "injected file metadata write failure")
			require.Equal(t, priorMtime, idx.FileMtimes()[key], "the read is not recorded")
			require.True(t, idx.IsStale(key), "the file stays stale for the retry")
			require.False(t, idx.indexedFileReceiptMatches(path))

			var mu sync.Mutex
			var mtimesAtWrite []int64
			fault.set(false, func(rows []graph.FileMetaRow) {
				for _, r := range rows {
					if r.FilePath == graphPath {
						mtime := idx.FileMtimes()[key]
						mu.Lock()
						mtimesAtWrite = append(mtimesAtWrite, mtime)
						mu.Unlock()
					}
				}
			})
			result, err = idx.IncrementalReindexPaths(root, []string{path})
			require.NoError(t, err)
			require.Equal(t, 1, result.DerivedInvalidation.InertFiles)
			require.Empty(t, result.FailedFiles)
			mu.Lock()
			require.Equal(t, []int64{priorMtime}, mtimesAtWrite, "the retry writes the row once, before it records the read")
			mu.Unlock()
			row, found = readRow()
			require.True(t, found)
			require.Equal(t, contentHashForSource([]byte(saved)), row.ContentHash, "the retry's row describes the saved bytes")
			require.Equal(t, len(saved), row.Size)
			require.NotContains(t, idx.fileIndexFailurePaths(), graphPath, "the retry clears the failure")
			require.Greater(t, idx.FileMtimes()[key], priorMtime, "the retry records the read")
			require.False(t, idx.IsStale(key))
			require.True(t, idx.indexedFileReceiptMatches(path))
		})
	}
}
