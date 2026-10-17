package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// daemonContractCall makes one strict contracts call through the daemon's
// MCP server and returns the tool result's text and whether it is an error.
func daemonContractCall(t *testing.T, ctx context.Context, state *daemonState, id int, wait time.Duration) (string, bool) {
	t.Helper()
	args := map[string]any{"action": "list", "type": "http", "limit": 1000, "format": "json", "wait_deadline": time.Now().Add(wait).Format(time.RFC3339Nano)}
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": "contracts", "arguments": args}})
	require.NoError(t, err)
	reply := state.mcpServer.MCPServer().HandleMessage(ctx, frame)
	encoded, err := json.Marshal(reply)
	require.NoError(t, err)
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(encoded, &response), string(encoded))
	require.Nil(t, response.Error, string(encoded))
	var text strings.Builder
	for _, content := range response.Result.Content {
		text.WriteString(content.Text)
	}
	return text.String(), response.Result.IsError
}

// TestDaemonContractJobsStartFromPublicationAndRepairAStuckReceipt runs the
// daemon's contract runtime in process. The first job's capture panics: the
// actor stays schedulable and the retry publishes. A route edit accepted
// while a strict request's interest lasts starts its job from the accept,
// before any request asks. A receipt left in flight on another file is
// repaired by the background worker, and the next strict call is complete
// and fresh.
func TestDaemonContractJobsStartFromPublicationAndRepairAStuckReceipt(t *testing.T) {
	daemonContractPublicationAndRepair(t, nil)
}

func daemonContractPublicationAndRepair(t *testing.T, observeRepair func(time.Duration)) {
	t.Helper()
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	// The contracts tool is outside the default preset.
	t.Setenv("GORTEX_TOOLS", "full")
	base := startupPublicationEnv(t)
	root := startupPublicationRepo(t, base, "tracked")
	routes := func(route string) []byte {
		return []byte("package fixture\nfunc register(r Router) { r.GET(\"" + route + "\", users) }\nfunc users() {}\n")
	}
	mainPath, helperPath := filepath.Join(root, "main.go"), filepath.Join(root, "helper.go")
	require.NoError(t, os.WriteFile(mainPath, routes("/before"), 0o600))
	require.NoError(t, os.WriteFile(helperPath, []byte("package fixture\nfunc Helper() int { return 1 }\n"), 0o600))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	state, err := buildDaemonState(zap.NewNop())
	require.NoError(t, err)
	core, logs := observer.New(zapcore.InfoLevel)
	var panicked atomic.Bool
	runtime, err := installDaemonContractAnalysisWith(state, zap.New(core), func(options *indexer.ContractAnalysisCoordinatorOptions) {
		require.NotNil(t, options.RetireReservation, "the daemon uses its existing lifecycle retirement owner")
		capture := options.Capture
		options.Capture = func(ctx context.Context, source *indexer.ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (indexer.ContractFollowupSnapshot, []indexer.ContractFollowupTarget, error) {
			if panicked.CompareAndSwap(false, true) {
				panic("injected capture panic")
			}
			return capture(ctx, source, inputs, repo, checkout)
		}
	})
	require.NoError(t, err)
	var closeOnce sync.Once
	closeState := func() {
		closeOnce.Do(func() {
			require.NoError(t, runtime.Close(ctx))
			require.NoError(t, state.shared.Close())
		})
	}
	t.Cleanup(closeState)
	defer closeState() // Join both owners before the work context's deferred cancellation, including on Fatal.
	_, err = state.lifecycle.Register(ctx, config.RepoEntry{Name: "tracked", Path: root}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	store := state.graph.(*store_sqlite.Store)
	counter := func(key string) int64 { return viewmetrics.Default().Snapshot().Counters[key] }
	call := 0
	strictUntil := func(route string, limit time.Duration) {
		t.Helper()
		start := time.Now()
		for time.Since(start) < limit {
			call++
			text, isError := daemonContractCall(t, ctx, state, call, 10*time.Second)
			if !isError && strings.Contains(text, route) {
				require.Contains(t, text, `"state":"complete"`)
				return
			}
		}
		t.Fatalf("no complete answer with %s within %s", route, limit)
	}
	panics := counter("views_contract_analysis_jobs_total{mode=cold,outcome=panic}")
	strictUntil("/before", time.Minute)
	require.Equal(t, panics+1, counter("views_contract_analysis_jobs_total{mode=cold,outcome=panic}"), "the panicking capture was counted and its actor stayed schedulable")

	// The request's interest lasts: the accept of a route edit starts the
	// job, before any request asks for the new inputs.
	started := len(logs.FilterMessage("contract analysis job started").All())
	require.NoError(t, os.WriteFile(mainPath, routes("/after"), 0o600))
	_, err = state.multiIndexer.IncrementalReindexRepo("tracked", []string{mainPath})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		lines := logs.FilterMessage("contract analysis job started").All()
		return len(lines) > started && lines[len(lines)-1].ContextMap()["trigger"] == "published"
	}, 30*time.Second, 10*time.Millisecond, "the accept started the job, before any request asked")
	strictUntil("/after", time.Minute)

	// A receipt staged outside any mutation, never accepted or ended: its
	// core-write window stays open, and no failed mutation marks it.
	helper, _, err := store.ContractBoundaryReceiptContext(ctx, "tracked", "", "tracked/helper.go")
	require.NoError(t, err)
	require.NotNil(t, helper)
	staged := *helper
	staged.Accepted, staged.Previous = false, nil
	repaired := counter("views_contract_repair_total{kind=stuck_receipt}")
	repairLogs := len(logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_receipt")).All())
	require.NoError(t, store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{staged}))
	var stuckAt time.Time
	if observeRepair != nil {
		stuckAt = time.Now()
	}
	require.NoError(t, os.WriteFile(mainPath, routes("/repaired"), 0o600))
	_, err = state.multiIndexer.IncrementalReindexRepo("tracked", []string{mainPath})
	require.NoError(t, err)
	strictUntil("/repaired", time.Minute)
	// A current route can publish before the independent repair detects the
	// helper's open window. Its completion log follows the forced reindex;
	// the counter alone is only evidence that the repair started.
	require.Eventually(t, func() bool {
		return len(logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_receipt")).All()) > repairLogs
	}, time.Minute, 10*time.Millisecond, "the background worker completed the staged receipt repair")
	require.Equal(t, repaired+1, counter("views_contract_repair_total{kind=stuck_receipt}"))
	lines := logs.FilterMessage("contract stuck repair").FilterField(zap.String("kind", "stuck_receipt")).All()[repairLogs:]
	require.NotEmpty(t, lines)
	require.Equal(t, "tracked", lines[0].ContextMap()["repo"])
	require.Equal(t, "repaired", lines[0].ContextMap()["outcome"])
	require.Contains(t, lines[0].ContextMap()["paths"], "tracked/helper.go")
	if observeRepair != nil {
		observeRepair(lines[0].Time.Sub(stuckAt))
	}
	require.Empty(t, store.OpenContractCoreWindows("tracked"))
	closeState()
}
