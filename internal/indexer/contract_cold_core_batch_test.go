package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractColdExtractionBatchesCoreProjections(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("helper%02d.go", i)] = fmt.Sprintf("package fixture\nfunc Helper%d() int { return %d }\n", i, i)
	}
	f := newContractJobFixture(t, files)
	var batches []*contractReplayCoreBatch
	reads := 0
	f.adjust = func(snapshot *ContractFollowupSnapshot) {
		read := snapshot.ReadCoreFile
		snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
			rows, err := read(ctx, file)
			if extraction, _ := ctx.Value(contractExtractionCoreContextKey{}).(bool); extraction {
				reads++
				batch, _ := ctx.Value(contractReplayCoreBatchKey{}).(*contractReplayCoreBatch)
				require.NotNil(t, batch, "cold extraction must use the bounded backend projection inside its proof bracket")
				require.Len(t, batch.rows, len(files), "the installed production callback actually acquired the shared projection")
				require.Contains(t, batch.rows, file)
				batches = append(batches, batch)
			}
			return rows, err
		}
	}
	report, err := f.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, len(files), reads, "every logical extraction still enters the installed callback")
	for _, batch := range batches {
		require.True(t, batch.closed)
		require.Empty(t, batch.rows, "no projection survives its R0/R1 extraction round")
	}
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

func TestContractColdExtractionBatchKeepsWrapperRefusal(t *testing.T) {
	f := newRouteConstantFixture(t)
	sentinel := errors.New("installed extraction wrapper refused")
	calls := 0
	f.adjust = func(snapshot *ContractFollowupSnapshot) {
		read := snapshot.ReadCoreFile
		snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
			rows, err := read(ctx, file)
			if file.Path == "fixture/routes.go" {
				calls++
				return ContractFollowupCoreFile{}, sentinel
			}
			return rows, err
		}
	}
	report, err := f.job(nil, nil)
	require.ErrorIs(t, err, sentinel)
	require.False(t, report.Published)
	require.Equal(t, 1, calls)
}

func TestContractColdExtractionBatchRejectsPrefetchedSourceMove(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	prefetched := false
	f.adjust = func(snapshot *ContractFollowupSnapshot) {
		var routes ContractFollowupFile
		for _, file := range snapshot.Files {
			if file.Path == "fixture/routes.go" {
				routes = file
			}
		}
		require.NotEmpty(t, routes.SourceFingerprint)
		read := snapshot.ReadCoreFile
		snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
			rows, err := read(ctx, file)
			if file.Path == "fixture/consts.go" {
				if batch, _ := ctx.Value(contractReplayCoreBatchKey{}).(*contractReplayCoreBatch); batch != nil {
					_, prefetched = batch.rows[routes]
				}
			}
			return rows, err
		}
	}
	moved := false
	hooks := &contractConvergenceHooks{beforeExtract: func(path string) {
		if path == "fixture/routes.go" && !moved {
			require.True(t, prefetched, "the later file was read by the earlier projection")
			moved = true
			files["routes.go"] = "package fixture\nfunc register(r Router) { r.GET(\"/after-prefetch\", handler) }\nfunc handler() {}\n"
			f.saveKeepingKey("routes.go", files["routes.go"])
		}
	}}
	report, err := f.job(hooks, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, moved)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()), "publication must use the newly proven source, never prefetched old rows")
}
