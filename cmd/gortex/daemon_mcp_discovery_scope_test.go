package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

func discoveryScopeRepo(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fixture.go"), []byte("package fixture\n\nfunc Value() int { return 1 }\n"), 0o600))
	guidanceGit(t, dir, "init", "-q", "-b", "main")
	guidanceGit(t, dir, "add", ".")
	guidanceGit(t, dir, "commit", "-q", "-m", "fixture")
}

// A checkout discovery that hangs refuses only its own worktree. The primary
// checkout, its directories outside a pending nested worktree, an unrelated
// tracked repository and another family's new worktree keep answering.
func TestDispatcher_PendingDiscoveryStaysInItsWorktree(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	primary, other := filepath.Join(base, "primary"), filepath.Join(base, "other")
	discoveryScopeRepo(t, primary)
	discoveryScopeRepo(t, other)
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(filepath.Join(base, "config.yaml"))
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(gc.ConfigPath())
	require.NoError(t, err)
	store, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	mi := indexer.NewMultiIndexer(store, registry, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{MultiIndexer: mi, ConfigManager: cm, Graph: store, Logger: zap.NewNop()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Close() })
	// The primary family's new worktrees hang in Git metadata and ignore
	// cancellation, like Git on a hung mount.
	stuck, nested := filepath.Join(base, "stuck"), filepath.Join(primary, "nested")
	release := make(chan struct{})
	selectedEntered := make(chan struct{})
	var selectedOnce sync.Once
	t.Cleanup(func() { close(release) })
	reconcile.WithHEADSampler(func(ctx context.Context, root string) (gitstate.HEADState, error) {
		if root = pathkey.CanonicalExistingRoot(root); pathkey.HasPathPrefix(root, stuck) || pathkey.HasPathPrefix(root, nested) {
			if pathkey.EqualPaths(root, stuck) {
				selectedOnce.Do(func() { close(selectedEntered) })
			}
			<-release
			return gitstate.HEADState{}, errors.New("released hung HEAD sample")
		}
		return gitstate.SampleHEAD(ctx, root)
	})(lifecycle.Reconciler())

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	families := map[string]string{}
	for name, root := range map[string]string{"primary": primary, "other": other} {
		registered, err := lifecycle.Register(ctx, config.RepoEntry{Path: root, Name: name}, indexer.TrackSourceCLI)
		require.NoError(t, err)
		require.NoError(t, registered.CatalogErr)
		families[name] = registered.FamilyID
	}
	srv := gortexmcp.NewServer(query.NewEngine(store), store, nil, nil, zap.NewNop(), nil, gortexmcp.MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm, CheckoutLifecycle: lifecycle})
	srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog()})
	d := newMCPDispatcher(srv, mi, zap.NewNop())

	late := filepath.Join(base, "late")
	guidanceGit(t, primary, "worktree", "add", "-q", "-b", "stuck", stuck)
	guidanceGit(t, primary, "worktree", "add", "-q", "-b", "nested", nested)
	guidanceGit(t, other, "worktree", "add", "-q", "-b", "late", late)
	const initializeFrame = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	const searchFrame = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"Value"}}}`
	searchFrom := func(id, cwd string) map[string]any {
		sess := &daemon.Session{ID: id, CWD: cwd}
		dispatchJSON(t, d, sess, initializeFrame)
		return dispatchJSON(t, d, sess, searchFrame)
	}

	// Sessions in many directories of the hung worktree. The second round
	// joins the first round's discoveries, so their workers hang.
	stuckSessions := make([]*daemon.Session, 40)
	for i := range stuckSessions {
		cwd := filepath.Join(stuck, fmt.Sprintf("session-%d", i))
		require.NoError(t, os.Mkdir(cwd, 0o700))
		stuckSessions[i] = &daemon.Session{ID: fmt.Sprintf("stuck-%d", i), CWD: cwd}
	}
	// Admit one dedicated session before the burst and observe its actual
	// hung HEAD sampler. A historical in_progress response alone can outlive
	// its worker or precede admission, and is not this ownership witness.
	selectedPath := filepath.Join(stuck, "selected-worker")
	require.NoError(t, os.Mkdir(selectedPath, 0o700))
	selectedSession := &daemon.Session{ID: "selected-stuck-worker", CWD: selectedPath}
	for {
		data := errorData(t, dispatchJSON(t, d, selectedSession, searchFrame))
		select {
		case <-selectedEntered:
			goto selectedWorkerObserved
		default:
		}
		require.Equal(t, "in_progress", data["reason"], "selected fixture worker did not reach its actual HEAD sampler: %v", data)
		require.NoError(t, ctx.Err())
	}
selectedWorkerObserved:
	stuckReplies := make([][]byte, len(stuckSessions))
	stuckErrs := make([]error, len(stuckSessions))
	for range 2 {
		var wg sync.WaitGroup
		for i, sess := range stuckSessions {
			wg.Go(func() { stuckReplies[i], stuckErrs[i] = d.Dispatch(context.Background(), sess, []byte(searchFrame)) })
		}
		wg.Wait()
	}

	// The checks below run once the hung discovery is past its work deadline
	// and still running: a session waiting on it is then refused as draining.
	drainBy := time.Now().Add(6 * time.Second)
	for {
		data := errorData(t, dispatchJSON(t, d, selectedSession, searchFrame))
		if data["reason"] == "draining" {
			break
		}
		require.Equal(t, "in_progress", data["reason"], "%v", data)
		require.True(t, time.Now().Before(drainBy), "the hung discovery did not pass its work deadline: %v", data)
		time.Sleep(50 * time.Millisecond)
	}

	// Another family's new worktree gets its identity; it may wait for its own
	// discovery, never for the hung one.
	deadline := time.Now().Add(30 * time.Second)
	for {
		reply := searchFrom("late", late)
		refusal, refused := reply["error"].(map[string]any)
		if !refused {
			break
		}
		data, _ := refusal["data"].(map[string]any)
		require.Equal(t, "in_progress", data["reason"], "another family's discovery refused this worktree: %v", reply)
		require.True(t, time.Now().Before(deadline), "the other family's worktree discovery did not finish: %v", reply)
		time.Sleep(10 * time.Millisecond)
	}
	overview, err := lifecycle.FamiliesOverview(ctx, families["other"])
	require.NoError(t, err)
	allocated := false
	for _, family := range overview.Families {
		for _, checkout := range family.Checkouts {
			allocated = allocated || pathkey.EqualPaths(checkout.RootPath, late)
		}
	}
	require.True(t, allocated, "the other family's new worktree has no identity: %+v", overview.Families)

	for _, cwd := range []string{primary, filepath.Join(primary, "internal"), other} {
		reply := searchFrom("answer-"+cwd, cwd)
		require.Contains(t, toolResultText(t, reply), "Value", "%s must answer while another checkout's discovery hangs: %v", cwd, reply)
		require.NotEqual(t, true, reply["result"].(map[string]any)["isError"], "%s: %v", cwd, reply)
	}

	// Only a path inside the unregistered nested worktree waits for discovery.
	reply := searchFrom("nested", nested)
	text := toolResultText(t, reply)
	require.Equal(t, true, reply["result"].(map[string]any)["isError"], "nested worktree answered from its parent: %v", reply)
	require.Contains(t, text, "view_building")
	require.Contains(t, text, nested, "the refusal names the pending nested worktree")

	familyCapacity := 0
	for i, raw := range stuckReplies {
		require.NoError(t, stuckErrs[i])
		var parsed map[string]any
		require.NoError(t, json.Unmarshal(raw, &parsed))
		data := errorData(t, parsed)
		require.Equal(t, "view_building", data["error_code"], "%v", parsed)
		require.Equal(t, true, data["retryable"], "%v", parsed)
		path, _ := data["path"].(string)
		require.True(t, pathkey.EqualPaths(stuck, path), "the refusal names the pending checkout root, not the session directory: %v", parsed)
		require.Equal(t, families["primary"], data["family"], "%v", parsed)
		require.Contains(t, []any{"in_progress", "family_capacity"}, data["reason"], "%v", parsed)
		if data["reason"] == "family_capacity" {
			familyCapacity++
		}
	}
	require.NotZero(t, familyCapacity, "one family's discoveries are bounded")
}
