//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// Integration layer: the daemon's existing constructor seam sets actor
// admission capacity two. The shipped binary has capacity eight and no public
// control for it. Sources, SQLite, materializer, capture, analysis, publication
// and MCP middleware/handlers are real; only the worker's Yield is held.
func TestContractCapacityTwoFourActorsMCPRetryAndSuccess(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "1")
	base := startupPublicationEnv(t)
	roots := map[string]string{}
	for i := 0; i < 4; i++ {
		repo := fmt.Sprintf("capacity%d", i)
		roots[repo] = startupPublicationRepo(t, base, repo)
		require.NoError(t, os.WriteFile(filepath.Join(roots[repo], "main.go"), []byte(e2eContractRoute("/"+repo+"/before")), 0o600))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	state, err := buildDaemonState(zap.NewNop())
	require.NoError(t, err)
	var owner *daemonContractAnalysis
	var coordinator *indexer.ContractAnalysisCoordinator
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if coordinator != nil {
			if err := coordinator.CloseContext(closeCtx); err != nil {
				t.Errorf("capacity driver failed to join constructor coordinator: %v", err)
				return
			}
		}
		if err := owner.Close(closeCtx); err != nil {
			t.Errorf("capacity driver failed to join worker: %v", err)
			return
		}
		require.NoError(t, state.shared.Close())
	})
	owner, err = installDaemonContractAnalysis(state, zap.NewNop())
	require.NoError(t, err)
	for i := 0; i < 4; i++ {
		repo := fmt.Sprintf("capacity%d", i)
		_, err := state.lifecycle.Register(ctx, config.RepoEntry{Name: repo, Path: roots[repo], Workspace: repo, Project: repo}, indexer.TrackSourceCLI)
		require.NoError(t, err)
	}

	call := func(callCtx context.Context, repo string, exact bool, wait time.Duration) (map[string]any, bool, string, error) {
		// Repo filters the response; project selects this actor's single-repo cohort.
		args := map[string]any{"action": "list", "type": "http", "repo": repo, "project": repo, "limit": 1000, "format": "json", "require_exact": exact, "wait_deadline": time.Now().Add(wait).Format(time.RFC3339Nano)}
		frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": repo, "method": "tools/call", "params": map[string]any{"name": "contracts", "arguments": args}})
		if err != nil {
			return nil, false, "", err
		}
		reply, err := json.Marshal(state.mcpServer.MCPServer().HandleMessage(callCtx, frame))
		if err != nil {
			return nil, false, "", err
		}
		var envelope struct {
			Error  any `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(reply, &envelope); err != nil {
			return nil, false, string(reply), err
		}
		if envelope.Error != nil {
			return nil, false, string(reply), fmt.Errorf("MCP transport error: %v", envelope.Error)
		}
		var text strings.Builder
		for _, content := range envelope.Result.Content {
			text.WriteString(content.Text)
		}
		var payload map[string]any
		if !envelope.Result.IsError {
			if err := json.Unmarshal([]byte(text.String()), &payload); err != nil {
				return nil, false, text.String(), fmt.Errorf("decode contracts result: %w; response: %s", err, text.String())
			}
		}
		return payload, envelope.Result.IsError, text.String(), nil
	}
	assertRoute := func(repo, suffix string, payload map[string]any) {
		t.Helper()
		require.True(t, e2eContractHasRoute(payload, "/"+repo+"/"+suffix), "%v", payload)
		for i := 0; i < 4; i++ {
			other := fmt.Sprintf("capacity%d", i)
			if other != repo {
				require.False(t, e2eContractHasRoute(payload, "/"+other+"/before"))
				require.False(t, e2eContractHasRoute(payload, "/"+other+"/after"))
			}
		}
	}
	for i := 0; i < 4; i++ {
		repo := fmt.Sprintf("capacity%d", i)
		payload, isError, text, err := call(ctx, repo, false, time.Minute)
		require.NoError(t, err)
		require.False(t, isError, text)
		assertRoute(repo, "before", payload)
		require.Equal(t, "complete", payload["contract_analysis"].(map[string]any)["state"])
	}
	// Join the initial owner before installing a new, empty scheduler. The
	// persisted old payloads remain available as genuine predecessor proof.
	require.NoError(t, owner.Close(ctx))
	owner = nil
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // registered after owner cleanup: unblock first on Fatal
	// Reinstalling daemon hooks after lifecycle coordinators exist is rightly
	// forbidden. This explicit constructor integration borrows the same live
	// state and installs only the real MCP/primary-indexer callbacks it uses;
	// it never changes that lifecycle guard or drives a sparse builder.
	materializer := state.mcpServer.Materializer()
	cfg := state.configManager.GetRepoConfig("").Index
	yield := contractAnalysisYield(state.mcpServer.ContractAnalysisShouldYield)
	capture := indexer.ContractFollowupCaptureOptions{Context: ctx, Store: state.graph.(*store_sqlite.Store), Materializer: materializer, MultiIndexer: state.multiIndexer, Registry: state.indexer.Registry(), Config: cfg, Logger: zap.NewNop(), Yield: yield}
	coordinator, err = indexer.NewContractAnalysisCoordinator(indexer.ContractAnalysisCoordinatorOptions{
		QueueCapacity: 2, JobSlots: 1, RequireRepoConfigs: true,
		Store: state.graph.(*store_sqlite.Store), Leases: materializer.Leases, Registry: state.indexer.Registry(), Config: cfg, Logger: zap.NewNop(),
		Capture: indexer.NewContractFollowupSourceCapture(capture), Inputs: indexer.NewContractPrimaryInputCapture(materializer),
		Yield: func(ctx context.Context) error {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
				return yield(ctx)
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	require.NoError(t, err)
	state.mcpServer.SetContractAnalysisRuntime(&gortexmcp.ContractAnalysisRuntime{AcquireLastPublishedCohort: coordinator.AcquireLastPublishedCohort, Schedule: coordinator.Schedule, WaitActor: coordinator.WaitActor, WaitCapacity: coordinator.WaitCapacity, CapacityEpoch: coordinator.CapacityEpoch, InputEpoch: coordinator.InputEpoch, WaitInputs: coordinator.WaitInputs})
	hooks := indexer.ContractCoreRuntimeHooks{Published: coordinator.Published}
	state.indexer.SetContractCoreRuntime(hooks)
	state.multiIndexer.SetContractCoreRuntime(hooks)
	for i := 0; i < 4; i++ {
		repo := fmt.Sprintf("capacity%d", i)
		path := filepath.Join(roots[repo], "main.go")
		require.NoError(t, os.WriteFile(path, []byte(e2eContractRoute("/"+repo+"/after")), 0o600))
		result, err := state.multiIndexer.IncrementalReindexRepo(repo, []string{path})
		require.NoError(t, err)
		require.Empty(t, result.FailedFiles)
	}
	type answer struct {
		repo    string
		payload map[string]any
		isError bool
		text    string
		err     error
	}
	done := make(chan answer, 2)
	for i := 0; i < 2; i++ {
		repo := fmt.Sprintf("capacity%d", i)
		go func() {
			payload, failed, text, err := call(ctx, repo, false, time.Minute)
			done <- answer{repo, payload, failed, text, err}
		}()
		if i == 0 {
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("real first worker did not reach hold")
			}
		}
		require.Eventually(t, func() bool {
			state := coordinator.Status(indexer.NewContractActor("", []string{repo})).State
			return state != "" && state != "idle"
		}, time.Minute, time.Millisecond)
	}
	queueFullCount := func() int64 {
		return viewmetrics.Default().Snapshot().Counters[viewmetrics.ContractAnalysisRequestsTotal+"{result=queue_full}"]
	}
	queueFullBefore := queueFullCount()
	for i := 2; i < 4; i++ {
		repo := fmt.Sprintf("capacity%d", i)
		payload, failed, text, err := call(ctx, repo, false, time.Second)
		require.NoError(t, err)
		require.False(t, failed, text)
		assertRoute(repo, "before", payload)
		require.False(t, e2eContractHasRoute(payload, "/"+repo+"/after"))
		status := payload["contract_analysis"].(map[string]any)
		require.Equal(t, "building", status["state"])
		require.Equal(t, false, status["fresh"])
		_, failed, text, err = call(ctx, repo, true, time.Second)
		require.NoError(t, err)
		require.True(t, failed)
		require.Contains(t, text, "required_capability_incomplete")
		require.Contains(t, text, "retry after")
		t.Logf("capacity2 actor %s positively served isolated predecessor; exact twin refused: %s", repo, text)
	}
	queueFullAfter := queueFullCount()
	require.EqualValues(t, 4, queueFullAfter-queueFullBefore, "both new actors and their exact twins hit the real admission guard")
	// An admitted actor at the internal deadline must also serve its own
	// predecessor; exact never borrows it. No elapsed-time pass criterion.
	payload, failed, text, err := call(ctx, "capacity0", false, 10*time.Millisecond)
	require.NoError(t, err)
	require.False(t, failed, text)
	assertRoute("capacity0", "before", payload)
	require.Equal(t, false, payload["contract_analysis"].(map[string]any)["fresh"])
	_, failed, text, err = call(ctx, "capacity0", true, 10*time.Millisecond)
	require.NoError(t, err)
	require.True(t, failed)
	require.Contains(t, text, "required_capability_incomplete")
	unblock()
	for range 2 {
		response := <-done
		require.NoError(t, response.err)
		require.False(t, response.isError, response.text)
		assertRoute(response.repo, "after", response.payload)
	}
	store := state.graph.(*store_sqlite.Store)
	for i := 0; i < 4; i++ {
		repo := fmt.Sprintf("capacity%d", i)
		payload, failed, text, err := call(ctx, repo, true, time.Minute)
		require.NoError(t, err)
		require.False(t, failed, text)
		assertRoute(repo, "after", payload)
		require.False(t, e2eContractHasRoute(payload, "/"+repo+"/before"))
		require.Equal(t, "complete", payload["contract_analysis"].(map[string]any)["state"])
		inputs, err := indexer.NewContractPrimaryInputCapture(state.mcpServer.Materializer())(ctx, []string{repo}, repo)
		require.NoError(t, err)
		require.True(t, inputs.State.Accepted)
		require.Eventually(t, func() bool {
			return coordinator.Status(indexer.NewContractActor("", []string{repo})).State == "idle"
		}, time.Minute, time.Millisecond, "publication metadata must be installed before custody is checked")
		proof, release, err := coordinator.AcquireLastPublishedCohort(ctx, indexer.NewContractActor("", []string{repo}))
		require.NoError(t, err)
		defer release()
		require.Len(t, proof.Targets, 1)
		target := proof.Targets[0]
		require.Equal(t, repo, target.Key.RepoPrefix)
		require.Equal(t, "", target.Key.CheckoutID)
		require.Equal(t, inputs.State.InputVersion, target.Key.InputVersion)
		require.Equal(t, inputs.State.InputFingerprint, target.Key.InputFingerprint)
		header, err := store.GetContractAttachmentContext(ctx, target.Key)
		require.NoError(t, err)
		require.NotNil(t, header)
		require.Equal(t, target.PayloadGeneration, header.PayloadGeneration)
		release()
	}
	t.Log("integration QueueCapacity2: four real actors, two actual queue-full predecessor/exact twins, held-worker deadline twins, release then four isolated current publications; shipped-native capacity remains8")
}
