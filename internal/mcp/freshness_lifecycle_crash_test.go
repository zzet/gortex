//go:build !windows

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	toon "github.com/toon-format/toon-go"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

const freshnessCrashWorkerRoot = "GORTEX_TEST_FRESHNESS_CRASH_ROOT"

type freshnessCrashReady struct {
	PID         int     `json:"pid"`
	Receipt     string  `json:"receipt"`
	Generation  float64 `json:"generation"`
	GraphStatus string  `json:"graph_status"`
}

// This is the actual process killed by the parent. The owned test repository
// and SQLite file survive because the parent, not this worker, owns them.
func TestFreshnessLifecycleCrashWorker(t *testing.T) {
	base := os.Getenv(freshnessCrashWorkerRoot)
	if base == "" {
		return
	}
	root := filepath.Join(base, "repo")
	fmt.Fprintln(os.Stdout, "FRESHNESS_WORKER_STAGE before_open")
	store, err := store_sqlite.Open(filepath.Join(base, "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	fmt.Fprintln(os.Stdout, "FRESHNESS_WORKER_STAGE before_tracked_server")
	s, _ := freshnessLifecycleTrackedServer(t, store, root, 5)
	fmt.Fprintln(os.Stdout, "FRESHNESS_WORKER_STAGE tracked_server_ready")
	s.editAckMode = config.EditAckLocal
	s.mutationReindexWait = time.Nanosecond
	ctx := freshnessLifecycleSession(t, s)
	fmt.Fprintln(os.Stdout, "FRESHNESS_WORKER_STAGE session_ready")
	// Existing publication barrier: physical ACK and file projection can
	// complete, but the real watcher cannot publish the mutation to SQLite.
	store.ResolveMutex().Lock()
	defer store.ResolveMutex().Unlock()
	fmt.Fprintln(os.Stdout, "FRESHNESS_WORKER_STAGE before_registered_write")
	body := freshnessLifecycleCall(t, s, ctx, 20, "write_file", map[string]any{
		"path": filepath.Join(root, "caller.go"), "format": "json",
		"content": "package p\nfunc Caller() int { return Target() }\nfunc AfterCrash() int { return Target() }\n",
	})
	fmt.Fprintln(os.Stdout, "FRESHNESS_WORKER_STAGE registered_write_returned")
	receipt, ok := body["reindex_receipt"].(string)
	require.True(t, ok, "real pending receipt missing: %v", body)
	require.NotEmpty(t, receipt)
	generation, ok := body["reindex_generation"].(float64)
	require.True(t, ok)
	require.Positive(t, generation)
	status := freshnessLifecycleCall(t, s, ctx, 21, "mutation_status", map[string]any{"receipt": receipt, "format": "json"})
	fmt.Fprintln(os.Stdout, "FRESHNESS_WORKER_STAGE mutation_status_returned")
	require.Equal(t, "pending", status["graph_status"])
	ready, err := json.Marshal(freshnessCrashReady{os.Getpid(), receipt, generation, status["graph_status"].(string)})
	require.NoError(t, err)
	tmp := filepath.Join(base, "ready.tmp")
	require.NoError(t, os.WriteFile(tmp, ready, 0600))
	require.NoError(t, os.Rename(tmp, filepath.Join(base, "ready.json")))
	fmt.Fprintln(os.Stdout, "FRESHNESS_WORKER_STAGE pending_ready_written")
	// No successful receipt, graceful Stop or SQLite Close can be fabricated
	// by the worker. The parent must observe a real SIGKILL and reap it.
	select {}
}

func freshnessLifecycleWorkerTail(t *testing.T, path string) string {
	t.Helper()
	// A separate file description avoids moving the live child's stdout offset.
	file, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("worker log unavailable: %v", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return fmt.Sprintf("worker log stat unavailable: %v", err)
	}
	buf := make([]byte, min(stat.Size(), 16<<10))
	n, err := file.ReadAt(buf, max(0, stat.Size()-int64(len(buf))))
	if err != nil && err != io.EOF {
		return fmt.Sprintf("worker log tail read failed: %v", err)
	}
	return fmt.Sprintf("whole_bytes=%d retained_tail_bytes=%d\n%s", stat.Size(), n, buf[:n])
}

func TestFreshnessLifecycleSIGKILLRestartPendingReceipt(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	freshnessLifecycleRepo(t, root)
	entry := config.RepoEntry{Name: "repo", Path: root}
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
	cfgPath := filepath.Join(base, "config.yaml")
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	log, err := os.Create(filepath.Join(base, "crashed-worker.log"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, log.Close()) })
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("ACTUAL_WORKER_FAILURE_TAIL %s", freshnessLifecycleWorkerTail(t, log.Name()))
		}
	})
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFreshnessLifecycleCrashWorker$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), freshnessCrashWorkerRoot+"="+base)
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var waited bool
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-exited
			waited = true
		}
	})
	var ready freshnessCrashReady
	readyDeadline := time.NewTimer(15 * time.Second)
	defer readyDeadline.Stop()
	readyPoll := time.NewTicker(10 * time.Millisecond)
	defer readyPoll.Stop()
waitingForReady:
	for {
		select {
		case err := <-exited:
			waited = true
			t.Fatalf("worker exited before its actual pending receipt: %v; %s", err, freshnessLifecycleWorkerTail(t, log.Name()))
		case <-readyDeadline.C:
			t.Fatalf("worker never produced its actual pending receipt within15s; %s", freshnessLifecycleWorkerTail(t, log.Name()))
		case <-readyPoll.C:
			data, err := os.ReadFile(filepath.Join(base, "ready.json"))
			if err == nil && len(data) <= 4096 && json.Unmarshal(data, &ready) == nil {
				break waitingForReady
			}
		}
	}
	require.Equal(t, cmd.Process.Pid, ready.PID)
	require.Equal(t, "pending", ready.GraphStatus)
	require.Positive(t, ready.Generation)
	require.NotEmpty(t, ready.Receipt)
	t.Logf("ACTUAL_CRASH_READY pid=%d receipt=%s generation=%.0f graph_status=%s", ready.PID, ready.Receipt, ready.Generation, ready.GraphStatus)
	before, err := os.Stat(filepath.Join(base, "graph.sqlite"))
	require.NoError(t, err)
	require.NoError(t, cmd.Process.Kill())
	err = <-exited
	waited = true
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	status, ok := exit.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	require.True(t, status.Signaled())
	require.Equal(t, syscall.SIGKILL, status.Signal(), "a graceful exit does not establish the crash criterion")

	// Restart in a different process from the killed worker, opening precisely
	// the same durable store and preserving its acknowledged physical write.
	store, err := store_sqlite.Open(filepath.Join(base, "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	after, err := os.Stat(filepath.Join(base, "graph.sqlite"))
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "the restart must not substitute a new empty store")
	require.NotNil(t, store.GetNode("repo/target.go::Target"))
	raw, err := os.ReadFile(filepath.Join(root, "caller.go"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "func AfterCrash()")
	prior := store.LoadFileMtimes("repo")
	require.NotEmpty(t, prior)
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	mi := indexer.NewMultiIndexer(store, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	leases := graphview.NewLeaseManager()
	mi.SetOutputGenerationAuthority(indexer.NewOutputGenerationAuthority(leases))
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer: mi, ConfigManager: cm, Graph: store, Logger: zap.NewNop(), ViewLeases: leases,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lifecycle.Close()) })
	require.NoError(t, lifecycle.Seed(t.Context()), "restore the actual persisted catalog owner")
	mi.BeginParallelBatch()
	s := NewServer(query.NewEngine(store), store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm, CheckoutLifecycle: lifecycle})
	s.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases})
	ctx := freshnessLifecycleSession(t, s)
	// A second client reads the same boundary through the facade surface in
	// TOON, where the relation state rides as an in-band label line.
	const facadeSession = "freshness-lifecycle-facade-toon"
	require.NoError(t, s.ConnectSession(facadeSession, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(facadeSession) })
	facadeCtx := s.WithClientSession(WithSessionID(t.Context(), facadeSession), facadeSession)
	s.NoteSessionToolPolicy(facadeSession, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(facadeCtx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"freshness-lifecycle-facade-toon","version":"1"}}}`)))
	checkLost := func(id int) {
		t.Helper()
		// The request serializes its ceiling and the handler parses a
		// wall-clock deadline; the slack check below must run in that same
		// clock domain rather than against a monotonic-bearing time.Now.
		serialized := time.Now().Add(5 * time.Second).Format(time.RFC3339Nano)
		deadline, err := time.Parse(time.RFC3339, serialized)
		require.NoError(t, err)
		body := freshnessLifecycleCall(t, s, ctx, id, "mutation_status", map[string]any{
			"receipt": ready.Receipt, "format": "json", "require_fresh": true,
			"wait_deadline": serialized,
		})
		require.Equal(t, "lost", body["state"])
		require.Equal(t, false, body["found"])
		require.Equal(t, true, body["graph_status_terminal"])
		require.True(t, time.Now().Before(deadline.Add(-time.Second)),
			"a lost receipt waited close to its serialized deadline %s", serialized)
	}
	checkLost(30)
	// The daemon becomes queryable at onComputeDone, while the actual master
	// resolver still holds this store's ResolveMutex before its refinement
	// tail, cross-repo resolve and end-batch. Hold that real startup boundary,
	// independent of whether the crash already persisted file-core rows.
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
			deferred := mi.RunDeferredPassesAllResult(t.Context())
			mi.ReconcileContractEdges()
			mi.EndBatchContext(t.Context())
			err = t.Context().Err()
			if err == nil {
				t.Logf("ACTUAL_STARTUP_RESOLVE resolve_ok=true deferred=%+v end_batch_returned=true", deferred)
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
		t.Fatalf("startup did not reach its real queryable resolve boundary: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("startup queryable resolve boundary not reached")
	}
	select {
	case err := <-completed:
		t.Fatalf("startup completed through a held queryable resolver boundary: %v", err)
	default:
	}
	checkLost(31)
	require.True(t, mi.RelationsPending())
	pending := freshnessLifecycleCall(t, s, ctx, 32, "get_callers", map[string]any{"id": "repo/target.go::Target", "format": "json"})
	require.Equal(t, "pending", freshnessLifecycleTier(t, pending)["graph_freshness"])
	checkRestartFacadeTOON(t, s, facadeCtx, 2, true)
	unblock()
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("actual startup reconciliation did not finish")
	}
	require.False(t, mi.RelationsPending())
	// The daemon starts its real watchers after successful startup resolution.
	w, err := indexer.NewConfigManagedMultiWatcher(mi, map[string]config.WatchConfig{"repo": {Enabled: true, DebounceMs: 5}}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.Start())
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	s.SetWatcher(w)
	fresh := freshnessLifecycleCall(t, s, ctx, 33, "get_callers", map[string]any{"id": "repo/target.go::Target", "format": "json"})
	// C6's boundary is the real shared producer leaving pending. An idle
	// default response retains the byte-omission contract; it must not acquire
	// an invented explicit-fresh authority merely to make this test pass.
	require.Empty(t, s.freshnessFor(ctx, "relations", []string{"repo/target.go::Target"}, nil).state)
	require.NotContains(t, fresh, "freshness")
	freshnessLifecycleHasCaller(t, fresh, "AfterCrash")
	checkRestartFacadeTOON(t, s, facadeCtx, 3, false)
	checkLost(34)
	t.Logf("ACTUAL_SIGKILL_RESTART old_pid=%d new_pid=%d receipt=%s generation=%.0f same_store=true lost_before_and_during_reconcile=true relations_pending_then_fresh=true", ready.PID, os.Getpid(), ready.Receipt, ready.Generation)
}

// checkRestartFacadeTOON reads callers through the facade in TOON. While the
// restarted daemon's relations are pending, the body must carry the pending
// label line and no fresh claim; once startup resolution completes, the idle
// reply omits the label and holds the exact post-crash caller edge.
func checkRestartFacadeTOON(t *testing.T, s *Server, ctx context.Context, id int, pending bool) {
	t.Helper()
	const target = "repo/target.go::Target"
	result, err := externalVersionRegisteredCall(s, ctx, id, "relations", map[string]any{
		"operation": "callers",
		"target":    map[string]any{"symbol": target},
		"output":    map[string]any{"format": "toon", "max_bytes": 0},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(result))
	require.Nil(t, result.StructuredContent, "only the requested TOON text is evidence")
	text, ok := singleTextContent(result)
	require.True(t, ok)
	require.NotEmpty(t, text)

	body := text
	if pending {
		var label string
		label, body, ok = strings.Cut(text, "\n")
		require.True(t, ok, "pending must label the original TOON body")
		require.Regexp(t, `^relations: pending \([0-9]+ files\)$`, label)
	} else {
		require.False(t, strings.HasPrefix(text, "relations: "), "completed default read must preserve idle label omission")
	}
	require.False(t, json.Valid([]byte(body)), "requested TOON returned JSON text")
	decoded, err := toon.Decode([]byte(body), toon.WithStrictMode(true))
	require.NoError(t, err)
	payload, ok := decoded.(map[string]any)
	require.True(t, ok, "TOON relation response must decode as a record")
	require.Equal(t, false, payload["truncated"])
	freshnessLifecycleHasCaller(t, payload, "Caller")
	if pending {
		rider, _ := payload["freshness"].(map[string]any)
		var meta map[string]any
		if result.Meta != nil {
			meta, _ = result.Meta.AdditionalFields["freshness"].(map[string]any)
		}
		for _, fields := range []map[string]any{payload, rider, meta} {
			require.NotEqual(t, "fresh", fields["graph_freshness"])
			require.NotEqual(t, true, fields["fresh"])
		}
	}
	edges, ok := payload["edges"].([]any)
	require.True(t, ok)
	afterCrashEdges := 0
	for _, row := range edges {
		if edge, ok := row.(map[string]any); ok && edge["from"] == "repo/caller.go::AfterCrash" &&
			edge["to"] == target && edge["kind"] == "calls" {
			afterCrashEdges++
		}
	}
	if pending {
		// The reconcile may already have installed the exact edge before its
		// remaining resolver boundary; the label is what discloses it.
		require.LessOrEqual(t, afterCrashEdges, 1)
	} else {
		require.Equal(t, 1, afterCrashEdges)
	}
}
