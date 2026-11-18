package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

// These are private proof/fence controls, not claims of public relation
// publication. The two-ticket integration test supplies the real whole path.
func TestPointRelationsReadFrontierVersionAndOwnerGuards(t *testing.T) {
	for _, test := range []string{"validated_os_read", "changed_version", "related_path", "snapshot", "different_call", "new_read_invalidates"} {
		t.Run(test, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			other := filepath.Join(root, "other.go")
			require.NoError(t, os.WriteFile(path, []byte("package main\nfunc Target() {}\n"), 0600))
			require.NoError(t, os.WriteFile(other, []byte("package main\nfunc Other() {}\n"), 0600))
			db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "read-fence.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			registry := parser.NewRegistry()
			registry.Register(languages.NewGoExtractor())
			idx := New(db, registry, config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			_, err = idx.Index(root)
			require.NoError(t, err)
			frontier := &relationsReadFrontier{path: path, capture: func() uint64 { return 2 }}
			readPath := path
			if test == "related_path" {
				readPath = other
			}
			if test == "snapshot" {
				src, err := source.NewFilesystemSource(root)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, src.Close()) })
				idx.SetContentSource(src)
			}
			_, version, err := idx.readFileWithVersion(readPath, frontier)
			require.NoError(t, err)
			require.Zero(t, frontier.covered, "read alone never authorizes relations")
			if test == "changed_version" {
				require.NoError(t, os.WriteFile(path, []byte("package main\nfunc TargetChangedAfterRead() {}\n"), 0600))
			}
			fresh, stale := idx.recordFileReadVersionsBatched([]fileReadReceipt{{absPath: readPath, mtimeKey: idx.relKey(readPath), readVersion: version}})
			switch test {
			case "changed_version":
				require.Empty(t, fresh)
				require.Equal(t, []string{path}, stale)
				require.Zero(t, frontier.covered, "a real version-fence failure must refuse the captured bound")
			case "related_path", "snapshot":
				require.Equal(t, []string{readPath}, fresh)
				require.Zero(t, frontier.covered, "another path or snapshot cannot prove this tool's disk read")
			default:
				require.Empty(t, stale)
				require.Equal(t, uint64(2), frontier.covered)
				if test == "different_call" {
					next := &relationsReadFrontier{path: path, capture: func() uint64 { return 9 }}
					next.accept(path, version)
					require.Zero(t, next.covered, "prepared/read proof from an earlier point call cannot be borrowed")
				}
				if test == "new_read_invalidates" {
					_, _, err := idx.readFileWithVersion(path, frontier)
					require.NoError(t, err)
					require.Zero(t, frontier.covered, "another read must pass its own final fence")
				}
			}
		})
	}
}

func TestSetBatchDoesNotAcquirePointRelationsReadAuthority(t *testing.T) {
	root := t.TempDir()
	paths := []string{filepath.Join(root, "a.go"), filepath.Join(root, "b.go")}
	for _, path := range paths {
		require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0600))
	}
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "set.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(db, registry, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	mode := incrementalPathMode{detectDeletions: true, forceExplicitFiles: true}
	result, receipt, batch, err := idx.incrementalReindexPathsWithReceiptMode(root, paths, mode)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, receipt)
	require.NotNil(t, batch)
	require.Nil(t, batch.relationsRead, "ordinary set/reconcile mode keeps its existing frozen frontier; no point proof")
}
