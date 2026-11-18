package indexer

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

func TestContractFollowupBatchKeepsWorkReportWhenInputKeyMoves(t *testing.T) {
	routes := func(path string) string { return strings.ReplaceAll(jobRoutes, "RoutePath", `"`+path+`"`) }
	f := newContractJobFixture(t, map[string]string{
		"routes.go": routes("/before"), "consts.go": jobConsts, "helper.go": jobHelper,
	})
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	f.save("routes.go", routes("/intermediate"))
	ctx := t.Context()
	held := f.options.Materializer.Leases.Held()
	inputs, err := f.options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	snapshot, targets, err := NewContractFollowupCapture(f.options)(ctx, nil, inputs, "fixture", "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	snapshot.Work = targets[0].Work
	session, err := newContractDeltaSession(cache, snapshot, f.options.Config)
	require.NoError(t, err)
	_, payload, err := f.options.Store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: "contract-failed-batch", LayerID: "job-" + time.Now().Format(time.RFC3339Nano),
		GenerationKind: "contract_analysis", ConfigHash: "job-config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "contract-only",
	})
	require.NoError(t, err)
	targets[0].Payload = payload
	moved := false
	scratch := t.TempDir()
	batch, err := RunContractFollowupBatch(ctx, ContractFollowupBatchRequest{
		RebuildReason: ContractFollowupUnknownInterval,
		Snapshot:      snapshot, Targets: targets, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: snapshot.RepoConfigs,
		Leases: f.options.Materializer.Leases, Logger: zap.NewNop(), Yield: f.options.Yield, ScratchParent: scratch,
		PublishedEpoch: f.epoch.Load, WaitPublished: f.waitPublished, delta: session,
		convergenceHooks: &contractConvergenceHooks{beforeFence: func(int) {
			if !moved {
				moved = true
				f.save("routes.go", routes("/after"))
			}
		}},
	})
	require.Equal(t, viewmetrics.ContractJobSupersededKeyMoved, contractJobOutcomeOf(err), "job error: %v", err)
	require.True(t, moved, "the original captured key must be overtaken after real delta work")
	require.Len(t, batch.Targets, 1)
	require.Error(t, batch.Targets[0].Err)
	require.False(t, batch.Targets[0].Report.Published)
	require.Nil(t, batch.Publication)
	require.Equal(t, batch.Targets[0].Report, batch.Cohort, "the failed batch must retain its actual preparation report")
	require.Equal(t, viewmetrics.ContractModeDelta, batch.Cohort.Mode)
	require.Empty(t, batch.Cohort.CacheReason)
	require.Equal(t, []string{"fixture/routes.go"}, batch.Cohort.ChangedFiles)
	require.Equal(t, 1, batch.Cohort.Files, "an interrupted delta must not be reported as an empty cold job")
	require.Positive(t, batch.Cohort.SourceReads)
	require.LessOrEqual(t, batch.Cohort.SourceReads, 2*len(snapshot.Files), "the interrupted one-file delta remains bounded")
	require.Positive(t, batch.Cohort.ExtractDuration)
	require.Equal(t, held, f.options.Materializer.Leases.Held(), "the captured snapshot and payload lease return on failure")
	entries, readErr := os.ReadDir(scratch)
	require.NoError(t, readErr)
	require.Empty(t, entries, "failed preparation closes scratch")
	header, readErr := f.options.Store.GetContractAttachmentContext(ctx, targets[0].Key)
	require.NoError(t, readErr)
	require.Nil(t, header, "a superseded batch cannot publish its captured key")
}
