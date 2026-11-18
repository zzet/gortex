package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

// This uses the production selected capture over a real sparse checkout, not
// a snapshot with a test Seeds callback. Ordinary Go files are not DI seeds.
func TestContractComposedCaptureSeedsStaySparseAndMatchCold(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper, "unrelated.go": "package fixture\nfunc unrelated() int { return 7 }\n"}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "consts.go")
	view := w.view(t)
	ctx := t.Context()
	inputs, err := f.options.Materializer.CaptureContractInputs(ctx, view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	snapshot, targets, err := NewContractFollowupCapture(f.options)(ctx, view, inputs, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	defer snapshot.Release()
	nodes, edges, supported, err := snapshot.Seeds(ctx, "fixture")
	require.NoError(t, err)
	require.True(t, supported, "a selected sparse checkout must not fall back to importing every live file")
	require.Empty(t, nodes)
	require.Empty(t, edges)
	loaded := map[string]int{}
	var guard *contractReadGuard
	read := snapshot.ReadCoreFile
	snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		if guard != nil && guard.open {
			loaded[file.Path]++
		}
		return read(ctx, file)
	}
	snapshot.Work = targets[0].Work
	_, payload, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "contract-linked-seeds", LayerID: "seed-job", GenerationKind: "contract_analysis", ConfigHash: "job-config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "contract-only", CreatedAt: time.Now().UnixNano()})
	require.NoError(t, err)
	report, err := RunContractFollowup(ctx, ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: f.options.Materializer.Leases, Logger: zap.NewNop(), Yield: f.options.Yield, PublishedEpoch: f.epoch.Load, WaitPublished: f.waitPublished, convergenceHooks: &contractConvergenceHooks{guard: func(value *contractReadGuard) { guard = value }}})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Zero(t, loaded["fixture/unrelated.go"], "an unrelated file must not become enrichment evidence merely because the core is composed")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(w.contracts(t, view)))
}
