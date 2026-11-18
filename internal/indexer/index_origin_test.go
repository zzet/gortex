package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

func TestFullIndexLogsOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin string
		shadow bool
		first  bool
	}{
		{"unknown", false, true},
		{"test_caller", true, true},
		{"test_caller", false, false},
	} {
		t.Run(fmt.Sprintf("%s/shadow=%t/first=%t", tc.origin, tc.shadow, tc.first), func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			var store graph.Store = graph.New()
			if tc.shadow {
				store = newFTSStore(t)
			}
			idx := newTestIndexer(store)
			if !tc.first {
				idx.indexCount.Store(1)
			}
			idx.logger = zap.New(core)
			idx.SetRepoPrefix("origin-repo")
			ctx := context.Background()
			if tc.origin != "unknown" {
				ctx = WithIndexOrigin(ctx, tc.origin)
			}
			_, err := idx.IndexCtx(ctx, setupTestDir(t))
			require.NoError(t, err)
			entries := logs.FilterMessage("indexer: full index started").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			assert.Equal(t, "origin-repo", fields["repo"])
			assert.Equal(t, tc.origin, fields["origin"])
			assert.Equal(t, tc.first, fields["first_index"])
			assert.Equal(t, tc.shadow, fields["shadow"])
		})
	}
}

func TestFullIndexDoesNotLogStartWhenMemoryAdmissionIsCancelled(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	idx := newTestIndexer(graph.New())
	idx.logger = zap.New(core)
	budget := newIndexMemoryAdmissionBudget(1)
	held, err := budget.acquire(t.Context(), 1)
	require.NoError(t, err)
	defer held.Release()
	idx.indexMemoryAdmission = budget

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dir := setupTestDir(t)
	result := make(chan error, 1)
	go func() {
		_, indexErr := idx.IndexCtx(ctx, dir)
		result <- indexErr
	}()
	waitForIndexMemoryWaiters(t, budget, 1)
	cancel()
	select {
	case err := <-result:
		require.True(t, errors.Is(err, context.Canceled), "index error = %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled index did not return")
	}
	assert.Zero(t, logs.FilterMessage("indexer: full index started").Len(), "a cancelled admission must not log a started index")
}

func TestCheckoutRegisterLogsFullIndexOrigin(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	core, logs := observer.New(zap.InfoLevel)
	f.mi.logger = zap.New(core)
	root := f.gitRepo("register-origin")

	registered, err := f.lc.Register(context.Background(), config.RepoEntry{Path: root, Name: "register-origin"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	entries := logs.FilterMessage("indexer: full index started").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "register-origin", entries[0].ContextMap()["repo"])
	assert.Equal(t, "checkout_register_cli_track", entries[0].ContextMap()["origin"])
}

func TestMultiIndexerLogsFullIndexEntryPoints(t *testing.T) {
	cm := newTestConfigManager(t)
	root := setupRepoDir(t, "multi-origin")
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: root, Name: "multi-origin"}))
	core, logs := observer.New(zap.InfoLevel)
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })

	_, err := mi.IndexAll()
	require.NoError(t, err)
	entries := logs.FilterMessage("indexer: full index started").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "index_all", entries[0].ContextMap()["origin"])
	logs.TakeAll()

	_, err = mi.IndexRepo("multi-origin")
	require.NoError(t, err)
	entries = logs.FilterMessage("indexer: full index started").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "index_repo", entries[0].ContextMap()["origin"])
}
