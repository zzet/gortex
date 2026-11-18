package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/platform"
	"go.uber.org/zap"
)

// The daemon hands the scheduler the host's RAM, so a daemon without a Go
// memory limit still sizes its contract checkpoint budget from the host.
func TestDaemonContractAnalysisPassesHostMemory(t *testing.T) {
	want := platform.HostPhysicalMemoryBytes()
	if want == 0 {
		t.Skip("host memory is unknown on this platform")
	}
	startupPublicationEnv(t)
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	state, err := buildDaemonState(zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, state.shared.Close()) })
	var got uint64
	runtime, err := installDaemonContractAnalysisWith(state, zap.NewNop(), func(options *indexer.ContractAnalysisCoordinatorOptions) {
		got = options.HostMemoryBytes
	})
	require.NoError(t, err)
	require.NotNil(t, runtime)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	require.Equal(t, want, got)
}
