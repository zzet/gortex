package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// driftContractFile rewrites a primary census file's bytes on disk without
// reindexing it: its metadata row keeps describing the old bytes, the state
// an inert save left behind before it kept that row current.
func driftContractFile(t *testing.T, f *contractJobFixture, name, text string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.root, name), []byte(text), 0o600))
}

// After a policy change, a census file whose bytes no longer match its
// metadata row keeps its outdated receipt for good: the capture refuses it
// as policy-stale, the repair rebuilds the baseline, and the rebuild skips
// the file as moved, so the request never attaches. The repair takes the
// reported file as drifted and reindexes it, and the request attaches with
// the file's receipt at the current policy.
func TestContractPolicyRepairReindexesADriftedFile(t *testing.T) {
	options, _, _, original := currentReceiptBaselineFixture(t, nil)
	require.NoError(t, reconcilePrimaryContractBaseline(t.Context(), options, "fixture"))
	root := options.MultiIndexer.repos["fixture"].RootPath
	drifted := "package fixture\nconst route = \"/reused-current\"\nfunc register(router *Router) { router.GET(route, serve) }\nfunc serve() {}\n// an edit no extractor sees\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "routes.go"), []byte(drifted), 0o600))
	options.Config.HTTPClientAliases = []string{"apiGet"}
	var repair atomic.Pointer[ContractStuckRepair]
	options.SuspectStuck = func(repo string, paths []string) { repair.Load().Suspect(repo, paths) }
	coordinator, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{
		Store: options.Store, Leases: options.Materializer.Leases, Registry: options.Registry,
		Config: options.Config, Capture: NewContractFollowupSourceCapture(options),
		Inputs: NewContractPrimaryInputCapture(options.Materializer), Yield: options.Yield,
	})
	require.NoError(t, err)
	options.MultiIndexer.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: coordinator.Published})
	runner, err := NewContractStuckRepair(ContractStuckRepairOptions{
		Store: options.Store, MultiIndexer: options.MultiIndexer, Baseline: options,
		Coordinator: coordinator, Period: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	repair.Store(runner)
	t.Cleanup(func() { runner.Close(); require.NoError(t, coordinator.Close()) })
	repaired := repairCount("stuck_receipt")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	attached := scheduleUntilAttached(t, ctx, coordinator, options.Materializer, []string{"fixture"}, "fixture")
	require.True(t, attached.State.Accepted)
	require.Equal(t, repaired+1, repairCount("stuck_receipt"), "one forced reindex rewrote the drifted file")
	current, _, err := options.Store.ContractBoundaryReceiptContext(ctx, "fixture", "", original.FilePath)
	require.NoError(t, err)
	require.True(t, current.Accepted)
	require.Equal(t, contractInputHash([]byte(drifted)), current.SourceFingerprint, "the receipt is of the bytes on disk")
	rows, err := options.Store.FileMetasByPaths("fixture", []string{original.FilePath})
	require.NoError(t, err)
	require.Equal(t, contentHashForSource([]byte(drifted)), rows[original.FilePath].ContentHash, "the row describes the bytes on disk")
}

// The repair takes a drifted file only while its bytes on disk differ from
// its row: a file whose row matches, or one gone from disk, is not drifted.
// A forced reindex rewrites the row, so a drifted file is repaired once.
func TestContractRepairTakesADriftedRowOnlyWhileTheBytesDiffer(t *testing.T) {
	const helper = "fixture/helper.go"
	f := newContractRepairFixture(t, nil)
	store := f.store.AtGeneration(0)
	require.False(t, f.r.driftedRow(t.Context(), store, "fixture", helper), "an indexed file is not drifted")
	driftContractFile(t, f.contractJobFixture, "helper.go", jobHelper+"// drifted\n")
	require.True(t, f.r.driftedRow(t.Context(), store, "fixture", helper))
	require.NoError(t, f.r.realReindex(t.Context(), "fixture", []string{helper}))
	require.False(t, f.r.driftedRow(t.Context(), store, "fixture", helper), "a forced reindex rewrites the row")
	driftContractFile(t, f.contractJobFixture, "helper.go", jobHelper+"// drifted again\n")
	require.NoError(t, os.Remove(filepath.Join(f.root, "helper.go")))
	require.False(t, f.r.driftedRow(t.Context(), store, "fixture", helper), "a deleted file is not drifted")
}

// Drift alone, with every receipt at the current policy, is repaired by one
// forced reindex of the reported file, without a baseline rebuild.
func TestContractDriftedFileIsReindexedWithoutABaselineRebuild(t *testing.T) {
	const helper = "fixture/helper.go"
	f := newContractRepairFixture(t, nil)
	f.strict(t, time.Minute)
	repaired, headers := repairCount("stuck_receipt"), repairCount("pending_header")
	drifted := jobHelper + "// an edit no extractor sees\n"
	driftContractFile(t, f.contractJobFixture, "helper.go", drifted)
	f.r.Suspect("fixture", []string{helper})
	store := f.store.AtGeneration(0)
	// The repair is counted as its reindex starts, and a path whose save is
	// writing is not drifted: wait for the rewritten row itself.
	require.Eventually(t, func() bool {
		rows, err := store.FileMetasByPaths("fixture", []string{helper})
		return err == nil && rows[helper].ContentHash == contentHashForSource([]byte(drifted))
	}, 20*time.Second, 10*time.Millisecond, "one forced reindex rewrote the drifted row")
	require.Eventually(t, func() bool {
		return len(f.store.OpenContractCoreWindows("fixture")) == 0
	}, 20*time.Second, 10*time.Millisecond, "the reindex closed its window")
	require.False(t, f.r.driftedRow(t.Context(), store, "fixture", helper))
	require.Equal(t, repaired+1, repairCount("stuck_receipt"), "one forced reindex")
	require.Equal(t, headers, repairCount("pending_header"), "drift alone needs no baseline rebuild")
}
