package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"go.uber.org/zap"
)

// daemonContractAnalysis owns the same selected view leases as MCP and drains
// all independent producer handoffs before the shared SQLite owner closes.
type daemonContractAnalysis struct {
	coordinator         *indexer.ContractAnalysisCoordinator
	cancel              context.CancelFunc
	baselineAdmissionMu sync.Mutex
	baselineJobs        sync.WaitGroup
}

// installDaemonContractAnalysis is explicitly gated until integrated native
// qualification. It installs the single implementation, never a stub capture.
func installDaemonContractAnalysis(state *daemonState, logger *zap.Logger) (*daemonContractAnalysis, error) {
	if os.Getenv("GORTEX_ASYNC_CONTRACTS") != "1" {
		return nil, nil
	}
	if state == nil || state.mcpServer == nil || state.indexer == nil || state.multiIndexer == nil || state.lifecycle == nil {
		return nil, fmt.Errorf("contract analysis: complete daemon runtime required")
	}
	store, ok := state.graph.(*store_sqlite.Store)
	if !ok {
		return nil, fmt.Errorf("contract analysis: SQLite store required")
	}
	materializer := state.mcpServer.Materializer()
	if materializer == nil || materializer.Leases == nil {
		return nil, fmt.Errorf("contract analysis: shared selected-view leases required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &daemonContractAnalysis{cancel: cancel}
	if state.configManager == nil {
		cancel()
		return nil, fmt.Errorf("contract analysis: accepted configuration authority required")
	}
	primaryConfig := state.configManager.GetRepoConfig("")
	if primaryConfig == nil {
		cancel()
		return nil, fmt.Errorf("contract analysis: primary configuration unavailable")
	}
	cfg := primaryConfig.Index
	captureOptions := indexer.ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: materializer, MultiIndexer: state.multiIndexer, Registry: state.indexer.Registry(), Config: cfg, Logger: logger, BaselineAdmissionMu: &runtime.baselineAdmissionMu, BaselineJobs: &runtime.baselineJobs, Yield: contractAnalysisYield(state.mcpServer.ContractAnalysisShouldYield)}
	coordinator, err := indexer.NewContractAnalysisCoordinator(indexer.ContractAnalysisCoordinatorOptions{RequireRepoConfigs: true, Store: store, Leases: materializer.Leases, Registry: state.indexer.Registry(), Config: cfg, Logger: logger, Capture: indexer.NewContractFollowupCapture(captureOptions), ReconcileBaseline: indexer.NewContractBaselineReconciler(captureOptions), Yield: contractAnalysisYield(state.mcpServer.ContractAnalysisShouldYield)})
	if err != nil {
		cancel()
		return nil, err
	}
	runtime.coordinator = coordinator
	hooks := indexer.ContractCoreRuntimeHooks{Published: coordinator.Published}
	state.indexer.SetContractCoreRuntime(hooks)
	state.multiIndexer.SetContractCoreRuntime(hooks)
	state.lifecycle.SetContractCoreRuntime(hooks)
	state.mcpServer.SetContractAnalysisRuntime(&gortexmcp.ContractAnalysisRuntime{Request: coordinator.Request, WaitChange: coordinator.WaitChange})
	return runtime, nil
}

// contractAnalysisYield gives active ordinary tools and edits priority without
// holding a core build lane. The producer gets a bounded progress opportunity
// after sustained demand; strict contract waiters are excluded by the server.
func contractAnalysisYield(shouldYield func() bool) func(context.Context) error {
	return indexer.ContractAnalysisYield(shouldYield)
}

// Close refuses to declare drained ownership on timeout. The caller must leave
// the store open in that case; committed pending state remains for restart.
func (r *daemonContractAnalysis) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.baselineAdmissionMu.Lock()
	r.cancel()
	r.baselineAdmissionMu.Unlock()
	if err := r.coordinator.CloseContext(ctx); err != nil {
		return err
	}
	drained := make(chan struct{})
	go func() { r.baselineJobs.Wait(); close(drained) }()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
