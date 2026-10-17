package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

// Exercise the actual daemon constructor, including a restart over accepted
// owner/catalog rows. The worker stays idle until a contract consumer requests
// it; pending analysis must not prevent ordinary source and call publication.
func TestDaemonDefaultContractRuntimeInstallsBeforeColdAndRestoredOwners(t *testing.T) {
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	base := startupPublicationEnv(t)
	root := startupPublicationRepo(t, base, "tracked")
	sourcePath := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(sourcePath, []byte("package fixture\nfunc register(r Router) { r.GET(\"/before\", users) }\nfunc users() { before() }\nfunc before() {}\nfunc after() {}\n"), 0o600))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var previous graph.ContractInputState
	for _, phase := range []string{"cold", "restored"} {
		state, err := buildDaemonState(zap.NewNop())
		require.NoError(t, err)
		store, ok := state.graph.(*store_sqlite.Store)
		require.True(t, ok)
		if phase == "restored" {
			persisted, found, readErr := store.ContractInputStateContext(ctx, "tracked", "")
			require.NoError(t, readErr)
			require.True(t, found, "restart must contain the prior actual owner inputs")
			require.Equal(t, previous.InputFingerprint, persisted.InputFingerprint)
		}
		runtime, err := installDaemonContractAnalysis(state, zap.NewNop())
		require.NoError(t, err, "actual pre-warmup state must permit installation")
		require.NotNil(t, runtime)
		var closeOnce sync.Once
		closeState := func() {
			closeOnce.Do(func() {
				require.NoError(t, runtime.Close(ctx))
				require.NoError(t, state.shared.Close())
			})
		}
		t.Cleanup(closeState)
		registered, err := state.lifecycle.Register(ctx, config.RepoEntry{Name: "tracked", Path: root}, indexer.TrackSourceCLI)
		require.NoError(t, err)
		require.Equal(t, "tracked", registered.Prefix)
		require.NoError(t, os.WriteFile(sourcePath, []byte("package fixture\nfunc register(r Router) { r.GET(\"/"+phase+"\", users) }\nfunc users() { after() }\nfunc before() {}\nfunc after() {}\n"), 0o600))
		result, err := state.multiIndexer.IncrementalReindexRepo("tracked", []string{sourcePath})
		require.NoError(t, err)
		require.Empty(t, result.FailedFiles)
		users := store.FindNodesByNameInRepo("users", "tracked")
		after := store.FindNodesByNameInRepo("after", "tracked")
		require.Len(t, users, 1)
		require.Len(t, after, 1)
		foundCall := false
		for _, edge := range store.GetOutEdges(users[0].ID) {
			foundCall = foundCall || edge.Kind == graph.EdgeCalls && edge.To == after[0].ID
		}
		require.True(t, foundCall, "ordinary callers must publish before contract analysis")
		require.Empty(t, store.NodesByKinds([]graph.NodeKind{graph.KindContract}))
		pending, err := store.PendingContractWorkForScopeContext(ctx, "tracked", "")
		require.NoError(t, err)
		require.NotEmpty(t, pending)
		previous, _, err = store.ContractInputStateContext(ctx, "tracked", "")
		require.NoError(t, err)
		closeState()
	}
}

func TestDaemonContractRuntimeRefusalDrainsFreshCoordinator(t *testing.T) {
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	startupPublicationEnv(t)
	state, err := buildDaemonState(zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, state.shared.Close()) })
	// The real lifecycle has closed admission. Installation creates an idle
	// coordinator, then must cancel/join it on the refusal without installing
	// any foreground hooks or delaying the caller indefinitely.
	require.NoError(t, state.lifecycle.Close())
	done := make(chan error, 1)
	go func() {
		runtime, installErr := installDaemonContractAnalysis(state, zap.NewNop())
		if runtime != nil {
			done <- runtime.Close(context.Background())
			return
		}
		done <- installErr
	}()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "installation refused")
	case <-time.After(5 * time.Second):
		t.Fatal("refused installation did not cancel and join its fresh coordinator")
	}
}
