package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

// The filesystem route previously streamed text nodes while omitting their
// metadata and boundary receipts. The same committed bytes then gained files
// in the accepted census when a Git-tree build used the ordinary byte route.
func TestContractColdStreamingTextCensusMatchesGitTree(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		label := "inline"
		if deferred {
			label = "deferred"
		}
		t.Run(label, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			root := t.TempDir()
			git := gitInFixture(t, root)
			git("init", "-q")
			require.NoError(t, os.MkdirAll(filepath.Join(root, "testdata"), 0700))
			text := "Accepted text document.\nThis file belongs to the contract input census.\n"
			writeColdReceiptFile(t, root, "testdata/document.txt", text)
			writeColdReceiptFile(t, root, "main.go", "package app\nfunc Main() {}\n")
			git("add", ".")
			git("commit", "-qm", "text census fixture")
			tree := git("rev-parse", "HEAD^{tree}")
			var inventories [][]string
			for _, gitTree := range []bool{false, true} {
				store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				idx := New(store, newAssetTestRegistry(), config.Default().Index, zap.NewNop())
				t.Cleanup(idx.Close)
				idx.SetRepoPrefix("repo")
				idx.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
				idx.SetDeferResolve(deferred)
				idx.SetDeferGlobalPasses(deferred)
				if gitTree {
					src, err := source.NewGitTreeSource(ctx, root, tree)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, src.Close()) })
					idx.SetContentSource(src)
				}
				result, err := idx.IndexCtx(ctx, root)
				require.NoError(t, err)
				require.Empty(t, result.FailedFiles)
				if deferred {
					idx.RunDeferredPasses(ctx)
				}
				metas, err := store.FileMetasForRepoContext(ctx, "repo")
				require.NoError(t, err)
				var paths []string
				for _, m := range metas {
					paths = append(paths, m.FilePath)
					if m.FilePath == "repo/testdata/document.txt" {
						require.Equal(t, contentHashForSource([]byte(text)), m.ContentHash)
						require.Equal(t, len(text), m.Size)
					}
				}
				require.ElementsMatch(t, []string{"repo/main.go", "repo/testdata/document.txt"}, paths, "text nodes must not escape the accepted-file inventory")
				row, found, err := store.ContractBoundaryReceiptForVersionContext(ctx, "repo", "", "repo/testdata/document.txt", contractBoundaryReceiptVersion)
				require.NoError(t, err)
				require.True(t, found)
				require.NotNil(t, row)
				require.Equal(t, contractInputHash([]byte(text)), row.SourceFingerprint)
				mi := NewMultiIndexer(store, idx.registry, search.NewNull(), nil, zap.NewNop())
				mi.repos["repo"] = &RepoMetadata{RepoPrefix: "repo", RootPath: root}
				mi.indexers["repo"] = idx
				leases := graphview.NewLeaseManager()
				registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "repo", RootIdentity: root, Incarnation: t.Name()}, nil)
				require.NoError(t, err)
				_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "accepted-streaming-census")
				require.NoError(t, err)
				mi.SetOutputGenerationAuthority(NewOutputGenerationAuthority(leases))
				materializer := &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases}
				options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: materializer, MultiIndexer: mi, Registry: idx.registry, Config: idx.config, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
				require.NoError(t, reconcilePrimaryContractBaseline(ctx, options, "repo"))
				inputs, err := materializer.CaptureContractInputs(ctx, nil, "repo", "")
				require.NoError(t, err)
				require.True(t, inputs.State.Accepted)
				snapshot, _, err := NewContractFollowupCapture(options)(ctx, nil, inputs, "repo", "")
				require.NoError(t, err)
				var census []string
				for _, f := range snapshot.Files {
					census = append(census, f.Path)
				}
				snapshot.Release()
				sort.Strings(census)
				require.Equal(t, []string{"repo/main.go", "repo/testdata/document.txt"}, census)
				inventories = append(inventories, census)
				row, found, err = store.ContractBoundaryReceiptForVersionContext(ctx, "repo", "", "repo/testdata/document.txt", contractBoundaryReceiptVersion)
				require.NoError(t, err)
				require.True(t, found)
				require.True(t, row.Accepted)
			}
			require.Equal(t, inventories[0], inventories[1], "switching to a committed source must not add historical text files")
		})
	}
}
