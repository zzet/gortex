package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/testenv"
)

// Real Git, catalog, lifecycle and control transport: no activation stub or
// synthetic intent rows. The primary is registered only after all 24 loading
// requests return; its normal registration callback must wake their loops.
func TestStartupCoverageDemandsActivateNoCheckoutBeforeReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	primary := startupPublicationRepo(t, base, "primary")
	roots := make([]string, 24)
	for i := range roots {
		roots[i] = filepath.Join(base, fmt.Sprintf("linked-%02d", i))
		wtGit(t, primary, "worktree", "add", "--detach", "--no-checkout", roots[i], "HEAD")
		_, err := os.Lstat(filepath.Join(roots[i], "main.go"))
		require.True(t, os.IsNotExist(err), "no-checkout source must remain absent")
	}
	store, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cm, err := config.NewConfigManager(filepath.Join(base, "config.yaml"))
	require.NoError(t, err)
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	newLifecycle := func() (*indexer.MultiIndexer, *indexer.CheckoutLifecycle) {
		mi := indexer.NewMultiIndexer(store, registry, search.NewNull(), cm, zap.NewNop())
		lc, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{Graph: store, MultiIndexer: mi, ConfigManager: cm, Logger: zap.NewNop()})
		require.NoError(t, err)
		return mi, lc
	}
	entry := config.RepoEntry{Path: primary, Name: "startup-demand"}
	oldMI, oldLifecycle := newLifecycle()
	tracked, err := oldLifecycle.Register(ctx, entry, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.NoError(t, oldLifecycle.Close())
	require.NoError(t, oldMI.Close(ctx))
	mi, lifecycle := newLifecycle()
	t.Cleanup(func() {
		require.NoError(t, lifecycle.Close())
		require.NoError(t, mi.Close(context.Background()))
	})
	// This is the normal warmup gate, not a test-only indexing barrier.
	lifecycle.SetBuildGate(indexer.NewViewBuildGate())
	mi.ExpectRepos([]config.RepoEntry{entry})
	require.NoError(t, lifecycle.Seed(ctx))
	require.Nil(t, mi.GetMetadata(tracked.Prefix))
	c := &realController{graph: store, multiIndexer: mi, configManager: cm, lifecycle: lifecycle, logger: zap.NewNop()}
	c.viewMaterializer = &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: lifecycle.ViewLeases()}
	require.Nil(t, c.probeActivateCheckout, "use the production ActivateCheckout path")
	socketRoot := testenv.ShortTempDir(t) // Only the socket needs a short path.
	socket := filepath.Join(socketRoot, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(socketRoot, "p"))
	server := daemon.New(socket, "startup-native-demand-test", zap.NewNop())
	server.Controller = c
	server.Ready = func() (bool, string) { return c.IsReady(), "warmup" }
	mcpServer := gortexmcp.NewServer(query.NewEngine(store), store, indexer.New(store, registry, config.Default().Index, zap.NewNop()), nil, zap.NewNop(), nil,
		gortexmcp.MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm, CheckoutLifecycle: lifecycle})
	mcpServer.SetMaterializer(c.viewMaterializer)
	server.MCPDispatcher = newMCPDispatcher(mcpServer, mi, zap.NewNop())
	require.NoError(t, server.Listen())
	served := make(chan error, 1)
	go func() { served <- server.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, server.Shutdown())
		require.NoError(t, <-served)
	})
	// The public administrative MCP catalog is distinct from a cwd-scoped
	// graph probe. There is no list_checkouts control operation.
	snapshot := func() indexer.FamiliesOverview {
		t.Helper()
		client, err := daemon.DialToWithDeadline(socket, daemon.Handshake{Mode: daemon.ModeMCP, Tools: "core", ToolsMode: "defer"}, time.Now().Add(time.Second))
		require.NoError(t, err)
		defer client.Close()
		call := func(id int, method string, params any) json.RawMessage {
			t.Helper()
			request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
			require.NoError(t, err)
			require.NoError(t, client.WriteMCPFrame(request))
			for {
				raw, err := client.ReadMCPFrame()
				require.NoError(t, err)
				var response struct {
					ID     int             `json:"id"`
					Error  json.RawMessage `json:"error"`
					Result json.RawMessage `json:"result"`
				}
				require.NoError(t, json.Unmarshal(raw, &response))
				if response.ID == id {
					require.Empty(t, response.Error, "actual native MCP error: %s", raw)
					return response.Result
				}
			}
		}
		call(1, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "startup-native-catalog", "version": "1"}})
		require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
		raw := call(2, "tools/call", map[string]any{"name": "list_checkouts", "arguments": map[string]any{"family": primary, "format": "json", "max_bytes": 8 << 20}})
		var result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.False(t, result.IsError, "actual native tool result: %s", raw)
		require.NotEmpty(t, result.Content)
		var overview indexer.FamiliesOverview
		require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &overview))
		require.Len(t, overview.Families, 1)
		require.Equal(t, tracked.FamilyID, overview.Families[0].FamilyID)
		return overview
	}
	before := snapshot()
	require.False(t, c.IsReady())
	owner, err := platform.CaptureProcessIdentity(os.Getpid())
	require.NoError(t, err)
	birth, err := owner.Token()
	require.NoError(t, err)
	type observedMember struct {
		ID          string `json:"checkout_id"`
		Incarnation string `json:"incarnation"`
		Root        string `json:"root_path"`
		Live        bool   `json:"coordinator_live"`
	}
	project := func(row indexer.CheckoutOverview) observedMember {
		return observedMember{ID: row.CheckoutID, Incarnation: row.Incarnation, Root: row.RootPath, Live: row.CoordinatorLive}
	}
	record := func(stage string, value any) {
		t.Helper()
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), 64<<10, "bounded native fixture evidence")
		t.Logf("startup_native24_%s %s", stage, encoded)
	}
	members := make(map[string]indexer.CheckoutOverview)
	var beforeRows []observedMember
	for _, row := range before.Families[0].Checkouts {
		if filepath.Clean(row.RootPath) != filepath.Clean(primary) {
			require.False(t, row.CoordinatorLive)
			require.Empty(t, row.Transition)
			members[filepath.Clean(row.RootPath)] = row
			beforeRows = append(beforeRows, project(row))
		}
	}
	require.Len(t, members, 24)
	var requests sync.WaitGroup
	errors := make(chan error, len(roots))
	type observedRequest struct {
		Root      string          `json:"root_path"`
		Started   time.Time       `json:"started"`
		Completed time.Time       `json:"completed"`
		Result    json.RawMessage `json:"native_coverage_result,omitempty"`
		Error     string          `json:"error,omitempty"`
	}
	observed := make([]observedRequest, len(roots))
	for i, root := range roots {
		requests.Go(func() {
			row := &observed[i]
			row.Root, row.Started = root, time.Now()
			defer func() { row.Completed = time.Now() }()
			client, err := daemon.DialToWithDeadline(socket, daemon.Handshake{Mode: daemon.ModeControl, CWD: root}, time.Now().Add(time.Second))
			if err != nil {
				row.Error = err.Error()
				errors <- err
				return
			}
			defer client.Close()
			response, err := client.ControlWithTimeout(daemon.ControlFileCoverage, daemon.FileCoverageParams{Path: filepath.Join(root, "main.go")}, time.Second)
			row.Result = append(json.RawMessage(nil), response.Result...)
			if err != nil {
				row.Error = err.Error()
				errors <- err
				return
			}
			var coverage daemon.FileCoverageResult
			if err := json.Unmarshal(response.Result, &coverage); err != nil {
				row.Error = err.Error()
				errors <- err
				return
			}
			if !response.OK || coverage.RepoState != daemon.RepoStateLoading || coverage.View == nil || coverage.View.CheckoutID != members[filepath.Clean(root)].CheckoutID || coverage.View.Kind != daemon.ProbeViewUnrouted || coverage.View.Exact || c.IsReady() {
				err := fmt.Errorf("wrong pre-ready native coverage: %+v, %+v", response, coverage)
				row.Error = err.Error()
				errors <- err
			}
		})
	}
	requests.Wait()
	goOverrides := make(map[string]string)
	for _, key := range []string{"GOMAXPROCS", "GOMEMLIMIT", "GOGC", "GODEBUG"} {
		if value, present := os.LookupEnv(key); present {
			goOverrides[key] = value
		}
	}
	record("before_registration", map[string]any{"pid": os.Getpid(), "process_birth_token": birth, "family_id": tracked.FamilyID, "primary_root": primary, "members": beforeRows, "requests": observed, "go_process_overrides": goOverrides, "runtime_gomaxprocs": runtime.GOMAXPROCS(0), "reference_profile_requires_outer_qualification": true})
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	registered := make(chan indexer.RepoLoad, 1)
	mi.OnRepoRegistered(func(rec indexer.RepoLoad) {
		lifecycle.PrimaryRegistered(rec.Prefix)
		registered <- rec
	})
	_, err = mi.TrackRepoCtx(ctx, entry)
	require.NoError(t, err)
	origin := <-registered
	require.False(t, origin.Since.IsZero())
	require.Eventually(t, func() bool { return lifecycle.LiveCoordinators(tracked.FamilyID) == 24 }, time.Until(origin.Since.Add(2*time.Second)), time.Millisecond)
	after := snapshot()
	afterWire := time.Now()
	var afterRows []observedMember
	for _, row := range after.Families[0].Checkouts {
		if _, expected := members[filepath.Clean(row.RootPath)]; expected {
			afterRows = append(afterRows, project(row))
		}
	}
	record("after_registration", map[string]any{"pid": os.Getpid(), "process_birth_token": birth, "family_id": tracked.FamilyID, "native_registration": origin, "after_wire_at": afterWire, "members": afterRows})
	require.Len(t, afterRows, 24)
	require.True(t, afterWire.Before(origin.Since.Add(2*time.Second)), "the public per-ID snapshot must also arrive within the native registration window")
	for _, row := range after.Families[0].Checkouts {
		if expected, ok := members[filepath.Clean(row.RootPath)]; ok {
			require.Equal(t, expected.CheckoutID, row.CheckoutID)
			require.Equal(t, expected.Incarnation, row.Incarnation)
			require.True(t, row.CoordinatorLive)
			require.Empty(t, row.Transition, "activation is not a fabricated mode-change intent")
		}
	}
}
