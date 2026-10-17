package indexer

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractChangedAnswerExtractionSamplesKnownCoverageFrontier(t *testing.T) {
	files := map[string]string{
		"routes.go": jobRoutes,
		"consts.go": jobConsts,
		"helper.go": "package fixture\nfunc Helper(RoutePath string) {}\n",
	}
	f := newContractJobFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	reader := cache.units["fixture/routes.go"]
	require.NotNil(t, reader)
	require.NotContains(t, reader.owners, "fixture/helper.go", "the parameter is not an endpoint constant candidate")
	require.Contains(t, cache.declarers.lookup("name:RoutePath", cache.units), "fixture/helper.go", "the negative declaration frontier still includes nonconstants")
	debt, _ := advanceContractCoverageInputs(t, f.store, "fixture", "fixture/consts.go", graph.ContractWorkScope{})
	var latestPoint []string
	var routeReplays []bool
	var extractionFrontier []string
	waits := 0
	f.onWait = func() { waits++ }
	hooks := &contractConvergenceHooks{
		roundWait: time.Millisecond,
		beforeExtract: func(path string) {
			if path == "fixture/routes.go" {
				extractionFrontier = slices.Clone(latestPoint)
			}
		},
		revalidated: func(path string, equal bool) {
			if path == "fixture/routes.go" {
				routeReplays = append(routeReplays, equal)
			}
		},
	}
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: &cache, hooks: hooks, epoch: f.epoch.Load, wait: f.waitPublished,
		adjust: func(snapshot *ContractFollowupSnapshot) {
			require.Contains(t, snapshot.Work, debt)
			scan := snapshot.Scan
			snapshot.Scan = func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
				if paths != nil {
					latestPoint = slices.Clone(paths)
				}
				return scan(ctx, paths)
			}
			readCore := snapshot.ReadCoreFile
			snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
				rows, err := readCore(ctx, file)
				if err == nil && file.Path == "fixture/routes.go" {
					// The complete recorded file answer changes, forcing real
					// re-extraction while endpoint constants remain unchanged.
					for i, node := range rows.Nodes {
						if node.ID == "fixture/routes.go::register" {
							copyNode := *node
							copyNode.Meta = map[string]any{}
							for key, value := range node.Meta {
								copyNode.Meta[key] = value
							}
							copyNode.Meta["frontier_test_revision"] = "changed"
							rows.Nodes[i] = &copyNode
						}
					}
				}
				return rows, err
			}
		}})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Contains(t, extractionFrontier, "fixture/helper.go", "the known declarer must be in actual point R0 before extraction reads")
	require.Contains(t, extractionFrontier, "fixture/consts.go")
	require.Equal(t, []bool{false}, routeReplays, "the changed old answer is followed by a complete extraction proof, not a deferred replay of new raw")
	require.Zero(t, waits, "known proof inputs need no external event before they can be read")
	require.Equal(t, 2, report.Files, "the debt producer and changed-answer reader are both freshly extracted")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

func TestContractChangedAnswerRetainsReadOwnersAndDiscoveredFrontier(t *testing.T) {
	unit := &contractUnit{path: "repo/reader.go", owners: map[string]bool{"repo/source.go": true}, keys: map[string]bool{"name:Route": true}, ledger: &contractUnitLedger{reads: []contractRecordedRead{{owners: []string{"repo/read-owner.go"}}}}}
	declarer := &contractUnit{path: "repo/declarer.go", declared: []string{"name:Route"}}
	w := newContractWorkCoverage([]graph.ContractWork{{Token: "debt", RepoPrefix: "repo", FilePath: "repo/producer.go"}})
	w.extra[unit.path] = map[string]bool{"repo/discovered.go": true}
	c := &contractConvergence{units: map[string]*contractUnit{unit.path: unit, declarer.path: declarer}, coverage: w, retryOwners: map[string]map[string]bool{}}
	c.dropForExtraction(unit.path)
	for _, owner := range []string{"repo/source.go", "repo/read-owner.go", "repo/declarer.go", "repo/discovered.go"} {
		require.True(t, c.retryOwners[unit.path][owner], "known frontier %s is retained before the unit is dropped", owner)
	}
	require.NotContains(t, c.units, unit.path)
	require.False(t, w.readSatisfied(unit), "retaining paths never imports an old query proof")
}
