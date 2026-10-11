package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractInputRetryRebuildsAncestryAndRetiresRefusal(t *testing.T) {
	for _, mode := range []string{"unchanged_records", "new_file_absence", "runtime_dependency_fallback", "reader_stale_before_capture"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoordinatorFixtureWithTree(t, map[string]string{
				"value.go": "package fixture\nfunc Value() int { return 1 }\n",
			})
			c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
			initial := coordinatorReconcile(t, c)
			require.Positive(t, initial.CommitGenerationID)
			switch mode {
			case "new_file_absence":
				builderWriteFile(t, f.worktree, "added.go", "package fixture\nfunc Added() int { return 2 }\n")
			case "runtime_dependency_fallback":
				builderWriteFile(t, f.worktree, "value.go", "package fixture\nfunc Value() int { external(); return 1 }\n")
			default:
				builderWriteFile(t, f.worktree, "value.go", "package fixture\nfunc Value() int { return 2 }\n")
			}
			sourceID := builderRepoPrefix + "/value.go::Value"
			marker := &graph.Edge{From: sourceID, To: sourceID, Kind: graph.EdgeAccessesField, FilePath: builderRepoPrefix + "/value.go", Line: 2}
			var correction *store_sqlite.DerivedCorrection
			var firstReader graph.Reader
			attempts := 0
			built, err := c.buildDirtyLayerAttempts(context.Background(), f.graphID, initial.CommitGenerationID,
				dirtyParentSelection{}, nil, "", func(req *DirtyLayerRequest) {
					attempts++
					base := req.Base.(commitLayerBase)
					if attempts == 1 {
						firstReader = base.Reader
						require.NotContains(t, firstReader.GetOutEdges(sourceID), marker)
						correct := func() {
							var err error
							correction, err = f.store.BeginDerivedCorrection(context.Background(), store_sqlite.DerivedCorrectionRequest{GenerationID: initial.CommitGenerationID, Pass: "capability", FromVersion: 1, ToVersion: 2, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
							require.NoError(t, err)
							require.NoError(t, correction.ReplaceSourceEdges(context.Background(), []string{sourceID}, []*graph.Edge{marker}, nil))
						}
						if mode == "reader_stale_before_capture" {
							correct()
						} else {
							req.buildBarrier = correct
						}
					} else {
						require.NotSame(t, firstReader, base.Reader, "retry must open a new ancestry reader")
						require.Contains(t, base.GetOutEdges(sourceID), marker, "new masks must expose the committed correction before Finish")
					}
				})
			require.NoError(t, err)
			require.Positive(t, built.GenerationID)
			require.Equal(t, 2, attempts, "the input guard must refuse the first payload, then the bounded retry succeeds")
			_, err = correction.Finish(context.Background())
			require.NoError(t, err)
			require.NotEmpty(t, c.retirementBacklog(), "refused plain stale-input errors must still schedule failed-payload cleanup")
		})
	}
}

func TestIndexFileNewDeltaConstantHasAcceptedInventory(t *testing.T) {
	root := t.TempDir()
	dw := graph.NewDeltaWriter(graph.New(), graph.New())
	idx := newTestIndexer(dw)
	idx.SetRepoPrefix("fixture")
	idx.storeRootPath(root)
	t.Cleanup(idx.Close)
	for _, test := range []struct {
		name   string
		direct bool
	}{{"batched", false}, {"legacy_direct", true}} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, test.name+".go")
			writeFile(t, path, "package fixture\nconst Path = \"/new\"\n")
			var err error
			if test.direct {
				err = idx.indexFile(path, false, nil)
			} else {
				err = idx.IndexFile(path)
			}
			require.NoError(t, err)
			id := "fixture/" + test.name + ".go::Path"
			values, err := graph.ConstantValuesByNodeIDsContext(context.Background(), dw, []string{id})
			require.NoError(t, err)
			require.Equal(t, "/new", values[id], "new-file constant ownership must follow accepted inventory")
			require.NoError(t, dw.ConstantValueReadError())
		})
	}
}

func TestContractInputReadErrorsNeverBecomeEmptySuccess(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	idx.rememberContractInputError(graph.ErrConstantProjectionStale)
	require.ErrorIs(t, idx.contractInputError(), ErrDirtySnapshotChanged)
	require.ErrorIs(t, idx.contractInputError(), errContractInputsChanged)
	require.ErrorIs(t, idx.contractInputError(), graph.ErrConstantProjectionStale)
	idx.clearContractInputError()
	idx.rememberContractInputError(context.Canceled)
	require.ErrorIs(t, idx.contractInputError(), context.Canceled)
	require.NotErrorIs(t, idx.contractInputError(), errContractInputsChanged, "ordinary failures must not become rebuild requests")
}
