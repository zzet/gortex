//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

const relationFreshnessCLIRestartRoot = "GORTEX_RELATION_FRESHNESS_CLI_RESTART_ROOT"

const (
	relationFreshnessCLIRestartTarget = "repo/target.go::Target"
	relationFreshnessCLIRestartCaller = "repo/caller.go::Caller"
	relationFreshnessCLIRestartAfter  = "repo/caller.go::AfterCrash"
)

// relationFreshnessCLIRestartIndexer builds the daemon's multi-repository
// indexer and checkout lifecycle over store for the one repository at root.
func relationFreshnessCLIRestartIndexer(t *testing.T, store *store_sqlite.Store, base, root string) (*indexer.MultiIndexer, *indexer.CheckoutLifecycle, *config.ConfigManager, *graphview.LeaseManager) {
	t.Helper()
	cfgPath := filepath.Join(base, "restart-config.yaml")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "repo", Path: root}}}
		gc.SetConfigPath(cfgPath)
		require.NoError(t, gc.Save())
	}
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	mi := indexer.NewMultiIndexer(store, registry, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	leases := graphview.NewLeaseManager()
	mi.SetOutputGenerationAuthority(indexer.NewOutputGenerationAuthority(leases))
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer: mi, ConfigManager: cm, Graph: store, Logger: zap.NewNop(), ViewLeases: leases,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lifecycle.Close()) })
	return mi, lifecycle, cm, leases
}

// TestRelationFreshnessCLIRestartWorker is the daemon process the parent
// kills. It registers and indexes the repository, then the caller file
// changes on disk while no watcher runs, so the durable store the next daemon
// opens never published the change.
func TestRelationFreshnessCLIRestartWorker(t *testing.T) {
	base := os.Getenv(relationFreshnessCLIRestartRoot)
	if base == "" {
		return
	}
	root := filepath.Join(base, "repo")
	store, err := store_sqlite.Open(filepath.Join(base, "graph.sqlite"))
	require.NoError(t, err)
	_, lifecycle, _, _ := relationFreshnessCLIRestartIndexer(t, store, base, root)
	registered, err := lifecycle.Register(t.Context(), config.RepoEntry{Name: "repo", Path: root}, store_sqlite.IntentSourceMCPTrack)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	require.NotNil(t, store.GetNode(relationFreshnessCLIRestartTarget))
	require.NotNil(t, store.GetNode(relationFreshnessCLIRestartCaller))
	require.Nil(t, store.GetNode(relationFreshnessCLIRestartAfter))
	require.NoError(t, os.WriteFile(filepath.Join(root, "caller.go"),
		[]byte("package a\nfunc Caller() int { return Target() }\nfunc AfterCrash() int { return Target() }\n"), 0o600))
	ready, err := json.Marshal(map[string]int{"pid": os.Getpid()})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(base, "ready.tmp"), ready, 0o600))
	require.NoError(t, os.Rename(filepath.Join(base, "ready.tmp"), filepath.Join(base, "ready.json")))
	// No graceful stop or store close: the parent must observe a real SIGKILL.
	select {}
}

// TestRelationFreshnessCLIRestart kills a daemon with SIGKILL after a change
// it never published, restarts on the same store and runs `gortex call` in a
// child process while the restarted daemon's startup resolution is held at
// its real queryable boundary. The CLI's machine JSON must disclose pending
// relations and claim nothing fresh; once startup resolution completes, the
// CLI reply holds the exact post-crash caller edge and no relation state.
func TestRelationFreshnessCLIRestart(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	base := startupPublicationEnv(t)
	root, err := filepath.EvalSymlinks(startupPublicationRepo(t, base, "repo"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "target.go"), []byte("package a\nfunc Target() int { return 1 }\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "caller.go"), []byte("package a\nfunc Caller() int { return Target() }\n"), 0o600))
	work := filepath.Dir(root)

	log, err := os.Create(filepath.Join(t.TempDir(), "worker.log"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, log.Close()) })
	tail := func() string {
		data, _ := os.ReadFile(log.Name())
		return string(data[max(0, len(data)-8<<10):])
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRelationFreshnessCLIRestartWorker$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), relationFreshnessCLIRestartRoot+"="+work)
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	deadline := time.After(30 * time.Second)
	for {
		data, err := os.ReadFile(filepath.Join(work, "ready.json"))
		var ready map[string]int
		if err == nil && json.Unmarshal(data, &ready) == nil {
			require.Equal(t, cmd.Process.Pid, ready["pid"])
			break
		}
		select {
		case err := <-exited:
			reaped = true
			t.Fatalf("worker exited before its unpublished change: %v\n%s", err, tail())
		case <-deadline:
			t.Fatalf("worker never became ready\n%s", tail())
		case <-time.After(10 * time.Millisecond):
		}
	}
	before, err := os.Stat(filepath.Join(work, "graph.sqlite"))
	require.NoError(t, err)
	require.NoError(t, cmd.Process.Kill())
	err = <-exited
	reaped = true
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	status, ok := exit.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	require.True(t, status.Signaled())
	require.Equal(t, syscall.SIGKILL, status.Signal(), "a graceful exit is not a crash")

	// Restart in this process on precisely the same durable store.
	store, err := store_sqlite.Open(filepath.Join(work, "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	after, err := os.Stat(filepath.Join(work, "graph.sqlite"))
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "the restart must not substitute a new store")
	require.NotNil(t, store.GetNode(relationFreshnessCLIRestartTarget))
	require.Nil(t, store.GetNode(relationFreshnessCLIRestartAfter), "the killed daemon never published the change")
	prior := store.LoadFileMtimes("repo")
	require.NotEmpty(t, prior)
	mi, lifecycle, cm, leases := relationFreshnessCLIRestartIndexer(t, store, work, root)
	require.NoError(t, lifecycle.Seed(t.Context()), "restore the persisted catalog owner")
	mi.BeginParallelBatch()
	s := gortexmcp.NewServer(query.NewEngine(store), store, nil, nil, zap.NewNop(), nil,
		gortexmcp.MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm, CheckoutLifecycle: lifecycle})
	s.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases})
	t.Cleanup(s.DrainBackground)
	cli := relationFreshnessCLIDaemon(t, root, s)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	probe := func(phase string) {
		t.Helper()
		payload := cli(ctx, "call", "relations", "--index", root, "--format", "json", "--json",
			`{"operation":"callers","target":{"symbol":"`+relationFreshnessCLIRestartTarget+`"},"output":{"format":"json","max_bytes":0}}`)
		require.Equal(t, 1, relationFreshnessCLIEdges(payload, relationFreshnessCLIRestartCaller, relationFreshnessCLIRestartTarget), "%s: surviving caller edge", phase)
		afterCrash := relationFreshnessCLIEdges(payload, relationFreshnessCLIRestartAfter, relationFreshnessCLIRestartTarget)
		state, fields := relationFreshnessCLIState(t, payload)
		switch phase {
		case "pending":
			require.Equal(t, "pending", state, "a restarted daemon answered before its startup resolution without disclosing it")
			require.Equal(t, "relations", fields["freshness_scope"])
			// Reconciliation may have installed the edge before the held
			// resolver boundary; the label is what discloses it.
			require.LessOrEqual(t, afterCrash, 1)
		case "fresh":
			require.Empty(t, state)
			require.Equal(t, 1, afterCrash, "a CLI reply after startup resolution lost the post-crash caller")
		}
	}

	// The daemon becomes queryable while the startup resolver still holds
	// its refinement tail, cross-repository resolve and end of batch. Hold
	// that real boundary.
	entry := config.RepoEntry{Name: "repo", Path: root}
	queryable := make(chan struct{})
	releaseStartup := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(releaseStartup) })
	t.Cleanup(unblock)
	completed := make(chan error, 1)
	startupDone := make(chan struct{})
	onQueryable := sync.OnceFunc(func() { close(queryable); <-releaseStartup })
	go func() {
		defer close(startupDone)
		_, err := mi.ReconcileRepoCtx(t.Context(), entry, prior)
		if err == nil {
			err = mi.RunPreEnrichResolve(t.Context(), nil, onQueryable)
		}
		if err == nil {
			mi.RunDeferredPassesAllResult(t.Context())
			mi.ReconcileContractEdges()
			mi.EndBatchContext(t.Context())
			if err = t.Context().Err(); err == nil {
				mi.CompleteStartupRelations()
			}
		}
		completed <- err
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case <-startupDone:
		case <-time.After(10 * time.Second):
			t.Error("startup reconciliation did not join")
		}
	})
	select {
	case <-queryable:
	case err := <-completed:
		t.Fatalf("startup did not reach its queryable resolve boundary: %v", err)
	case <-ctx.Done():
		t.Fatal("startup queryable resolve boundary not reached")
	}
	require.True(t, mi.RelationsPending())
	probe("pending")
	require.True(t, mi.RelationsPending(), "startup resolution must stay held through the CLI reply")

	unblock()
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("startup reconciliation did not finish")
	}
	require.False(t, mi.RelationsPending())
	require.NotNil(t, store.GetNode(relationFreshnessCLIRestartAfter))
	w, err := indexer.NewConfigManagedMultiWatcher(mi, map[string]config.WatchConfig{"repo": {Enabled: true, DebounceMs: 5}}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.Start())
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	s.SetWatcher(w)
	probe("fresh")
	t.Logf("CLI restart: killed pid=%d same_store=true pending_then_exact=true", cmd.Process.Pid)
}
