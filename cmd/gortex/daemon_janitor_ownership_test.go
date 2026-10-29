package main

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Observe the actual admitted call stack rather than sleep and hope a tick
// reached its lane. This is test-only observation, not a production hook.
func janitorOwnedStackContains(parts ...string) bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
		all := true
		for _, part := range parts {
			if !strings.Contains(stack, part) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func TestJanitorCanceledCallerRetainsAdmittedWorkerUntilRealStackClose(t *testing.T) {
	base := startupPublicationEnv(t)
	t.Setenv("GORTEX_GO_TYPES", "0")
	t.Setenv("GORTEX_TELEMETRY_ENDPOINT", "")
	root := startupPublicationRepo(t, base, "tracked")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: root, Name: "tracked"}}}
	gc.SetConfigPath(config.DefaultGlobalConfigPath())
	require.NoError(t, gc.Save())
	core, events := observer.New(zap.InfoLevel)
	state, err := buildDaemonState(zap.New(core)) // Actual shared cleanup registration.
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.shared.Close() })
	_, err = state.multiIndexer.IndexAll()
	require.NoError(t, err)
	nodes := state.graph.GetFileNodes("tracked/main.go")
	require.NotEmpty(t, nodes)
	store := state.graph.(*store_sqlite.Store)
	var expectedID string
	for _, node := range nodes {
		if node.Name == "A" {
			expectedID = node.ID
		}
	}
	require.NotEmpty(t, expectedID)
	release, ok := state.multiIndexer.TryRepositoryMutationLanes([]string{"tracked"})
	require.True(t, ok)
	var once sync.Once
	unlock := func() { once.Do(release) }
	t.Cleanup(unlock)
	cancel, stop := startReconcileJanitor(state.multiIndexer, nil, time.Millisecond, zap.NewNop())
	t.Cleanup(func() { unlock(); cancel(); stop() })
	require.Eventually(t, func() bool {
		return janitorOwnedStackContains("startReconcileJanitor", "repositoryMutationCoordinator).reconcile")
	}, 5*time.Second, time.Millisecond,
		"janitor never admitted reconciliation behind the actual held repository lane")
	cancel()
	janitorDone := make(chan struct{})
	go func() { defer close(janitorDone); stop() }()
	select {
	case <-janitorDone:
	case <-time.After(3 * time.Second):
		unlock()
		t.Fatal("janitor ignored its caller context")
	}
	closed := make(chan error, 1)
	go func() { closed <- state.shared.Close() }()
	require.Eventually(t, func() bool { return janitorOwnedStackContains("SharedServer).Close", "MultiIndexer).Close") }, 5*time.Second, time.Millisecond,
		"actual shared stack did not retain the admitted mutation worker")
	select {
	case <-closed:
		t.Fatal("shared close certified completion while its admitted mutation lane remained held")
	default:
	}
	for _, e := range events.FilterMessage("serverstack: shutdown phase start").All() {
		require.NotEqual(t, "backend", e.ContextMap()["phase"], "backend close preceded actual admitted worker completion")
	}
	node, err := store.GetNodeContext(t.Context(), expectedID)
	require.NoError(t, err)
	require.NotNil(t, node, "backend was not live while the actual worker remained owned")
	unlock()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("actual stack did not finish after releasing its owned lane")
	}
	reopened, err := store_sqlite.Open(state.backendPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NotNil(t, reopened.GetNode(expectedID), "committed fixture was lost across actual worker/store completion")
}

func TestJanitorNonjoiningCancelBeforeIndependentOwnerJoins(t *testing.T) {
	ctx, done, cancel, stop := newCancelableReconcileJanitorLifetime()
	finish := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	terminal := make(chan struct{})
	go func() { defer close(done); <-ctx.Done(); <-finish; close(terminal) }()
	t.Cleanup(func() { cancel(); release(); stop() })
	cancel() // Must return while its owner is still finishing terminal work.
	select {
	case <-ctx.Done():
	default:
		t.Fatal("janitor cancel did not revoke admission")
	}
	select {
	case <-terminal:
		t.Fatal("fixture terminal work unexpectedly finished")
	default:
	}
	joined := make(chan struct{})
	go func() { defer close(joined); stop() }()
	select {
	case <-joined:
		t.Fatal("janitor stop returned before actual terminal work")
	default:
	}
	release()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("actual janitor completion did not join")
	}
	stop()
}
