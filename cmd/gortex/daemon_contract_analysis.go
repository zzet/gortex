package main

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"go.uber.org/zap"
)

// daemonContractAnalysis owns the same selected view leases as MCP and drains
// all independent producer handoffs before the shared SQLite owner closes.
type daemonContractAnalysis struct {
	coordinator         *indexer.ContractAnalysisCoordinator
	repair              *indexer.ContractStuckRepair
	cancel              context.CancelFunc
	baselineAdmissionMu sync.Mutex
	baselineJobs        sync.WaitGroup
	closeOnce           sync.Once
	closeDone           chan struct{}
	closeErr            error
}

// installDaemonContractAnalysis installs the production runtime before warmup.
// The explicit opt-out keeps the diagnostic comparison on its legacy path.
func installDaemonContractAnalysis(state *daemonState, logger *zap.Logger) (*daemonContractAnalysis, error) {
	return installDaemonContractAnalysisWith(state, logger, nil)
}

// installDaemonContractAnalysisWith is installDaemonContractAnalysis with an
// adjustment of the scheduler's options before it starts, which only tests
// pass.
func installDaemonContractAnalysisWith(state *daemonState, logger *zap.Logger, adjust func(*indexer.ContractAnalysisCoordinatorOptions)) (*daemonContractAnalysis, error) {
	if os.Getenv("GORTEX_ASYNC_CONTRACTS") == "0" {
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
	// Captures and jobs report suspect stuck inputs to the repair once it
	// exists; a report before then is dropped.
	var repair atomic.Pointer[indexer.ContractStuckRepair]
	captureOptions.SuspectStuck = func(repo string, paths []string) { repair.Load().Suspect(repo, paths) }
	options := indexer.ContractAnalysisCoordinatorOptions{MemoryLimitBytes: debug.SetMemoryLimit(-1), RequireRepoConfigs: true, Store: store, Leases: materializer.Leases, Registry: state.indexer.Registry(), Config: cfg, Logger: logger, Capture: indexer.NewContractFollowupSourceCapture(captureOptions), Inputs: indexer.NewContractPrimaryInputCapture(materializer), LinkedInputs: indexer.NewContractLinkedInputCapture(materializer), ReconcileBaseline: indexer.NewContractBaselineReconciler(captureOptions), Yield: contractAnalysisYield(state.mcpServer.ContractAnalysisShouldYield)}
	options.RetireReservation = state.lifecycle.DeferContractReservationRetirement
	if adjust != nil {
		adjust(&options)
	}
	coordinator, err := indexer.NewContractAnalysisCoordinator(options)
	if err != nil {
		cancel()
		return nil, err
	}
	runtime.coordinator = coordinator
	runner, err := indexer.NewContractStuckRepair(indexer.ContractStuckRepairOptions{Store: store, MultiIndexer: state.multiIndexer, Baseline: captureOptions, Coordinator: coordinator, Logger: logger})
	if err != nil {
		cancel()
		_ = coordinator.CloseContext(context.Background())
		return nil, err
	}
	repair.Store(runner)
	runtime.repair = runner
	hooks := indexer.ContractCoreRuntimeHooks{Published: coordinator.Published}
	if err := state.lifecycle.SetContractCoreRuntime(hooks); err != nil {
		cancel()
		runner.Close()
		if closeErr := coordinator.CloseContext(context.Background()); closeErr != nil {
			return nil, fmt.Errorf("contract analysis: installation refused: %v; close coordinator: %w", err, closeErr)
		}
		return nil, fmt.Errorf("contract analysis: installation refused: %w", err)
	}
	state.indexer.SetContractCoreRuntime(hooks)
	state.multiIndexer.SetContractCoreRuntime(hooks)
	state.mcpServer.SetContractAnalysisRuntime(&gortexmcp.ContractAnalysisRuntime{AcquireLastPublishedCohort: coordinator.AcquireLastPublishedCohort, Schedule: coordinator.Schedule, WaitActor: coordinator.WaitActor, WaitTarget: coordinator.WaitTarget,
		WaitCapacity: coordinator.WaitCapacity, CapacityEpoch: coordinator.CapacityEpoch, InputEpoch: coordinator.InputEpoch, WaitInputs: coordinator.WaitInputs})
	return runtime, nil
}

// contractAnalysisYield gives active ordinary tools and edits priority without
// holding a core build lane. The producer gets a bounded progress opportunity
// after sustained demand; strict contract waiters are excluded by the server.
func contractAnalysisYield(shouldYield func() bool) func(context.Context) error {
	return indexer.ContractAnalysisYield(shouldYield)
}

// Cancel revokes all contract producers before any owner is joined.
func (r *daemonContractAnalysis) Cancel() {
	if r == nil {
		return
	}
	r.cancel()
	r.repair.Cancel()
	r.coordinator.Cancel()
}

// beginClose owns one drain for the lifetime of this runtime. Cancellation is
// synchronous and precedes the admission fence; joins run outside that fence.
func (r *daemonContractAnalysis) beginClose() {
	r.closeOnce.Do(func() {
		r.closeDone = make(chan struct{})
		r.Cancel()
		go func() {
			defer close(r.closeDone)
			r.baselineAdmissionMu.Lock()
			r.baselineAdmissionMu.Unlock() //nolint:staticcheck // Actual completion barrier for a previously admitted baseline handoff, not an empty critical section.
			// All contract owners were canceled above; every physical handoff
			// still has to finish before this barrier permits backend close.
			r.repair.Close()
			r.closeErr = r.coordinator.Close()
			r.baselineJobs.Wait()
		}()
	})
}

// Close refuses to declare drained ownership on timeout. The caller must leave
// the store open until this runtime's same completion barrier has been joined.
func (r *daemonContractAnalysis) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.beginClose()
	select {
	case <-r.closeDone:
		return r.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *daemonContractAnalysis) waitClosed() error {
	if r == nil {
		return nil
	}
	r.beginClose()
	<-r.closeDone
	return r.closeErr
}
