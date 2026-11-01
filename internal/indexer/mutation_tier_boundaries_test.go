package indexer

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/semantic"
)

func TestStandaloneRelationsWaitForSemanticCatchup(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	dir, watcher := toolAdmissionFixture(t)
	idx := watcher.indexer
	path := filepath.Join(dir, "main.go")
	manager := semantic.NewManager(semantic.Config{Enabled: true, EnrichOnWatch: true}, zap.NewNop())
	manager.RegisterProvider(&retainingRepoProvider{retained: make(map[string]struct{}), leased: make(map[string]struct{})})
	idx.SetSemanticManager(manager)
	writeTestFile(t, path, "package main\nfunc Changed() {}\n")
	entered, release := make(chan MutationTierState, 1), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	idx.incrementalCatchupHook = func(kind string, _ []string) {
		if kind == "semantic" {
			entered <- watcher.TierState(path)
			<-release
		}
	}
	ticket, err := watcher.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	select {
	case state := <-entered:
		require.True(t, state.RelationsAt.IsZero(), "semantic references have not published yet")
	case <-time.After(2 * time.Second):
		t.Fatal("the real semantic pipeline did not reach its hook")
	}
	unblock()
	select {
	case result := <-ticket.Done:
		require.NoError(t, result.Err)
		require.False(t, ticket.TierState().RelationsAt.IsZero())
	case <-time.After(2 * time.Second):
		t.Fatal("semantic completion did not release the ticket")
	}
}

func TestMixedBatchDoesNotPublishFailedRelationsBeforeDerived(t *testing.T) {
	dir, watcher := toolAdmissionFixture(t)
	idx := watcher.indexer
	good, bad := filepath.Join(dir, "main.go"), filepath.Join(dir, "bad.go")
	writeTestFile(t, bad, "package main\nfunc Bad() {}\n")
	require.NoError(t, idx.IndexFile(bad))
	idx.registry.Register(contractFailingGoExtractor{GoExtractor: languages.NewGoExtractor()})
	writeTestFile(t, good, "package main\nfunc GoodChanged() {}\n")
	writeTestFile(t, bad, "package main\n// EXTRACT_FAIL\nfunc BadChanged() {}\n")
	entered, release := make(chan MutationTierState, 1), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	idx.incrementalCatchupHook = func(kind string, _ []string) {
		if kind == "derived" {
			entered <- watcher.TierState(bad)
			<-release
		}
	}
	tickets, err := watcher.EnqueueFileMutations(t.Context(), []string{good, bad})
	require.NoError(t, err)
	select {
	case state := <-entered:
		require.True(t, state.RelationsAt.IsZero(), "a failed parse cannot certify its old relations")
	case <-time.After(2 * time.Second):
		t.Fatal("mixed-success batch did not reach derived work")
	}
	unblock()
	for path, ticket := range tickets {
		select {
		case result := <-ticket.Done:
			if path == bad {
				require.Error(t, result.Err)
			} else {
				require.NoError(t, result.Err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("batch ticket did not finish")
		}
	}
}
