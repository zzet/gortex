package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestWarmupPrefixCollisionPrefersPublishedCatalogOwner(t *testing.T) {
	for _, includeOwner := range []bool{false, true} {
		t.Run(fmt.Sprint(includeOwner), func(t *testing.T) {
			dir := t.TempDir()
			owner, rejected := filepath.Join(dir, "owner"), filepath.Join(dir, "rejected")
			for _, path := range []string{owner, rejected} {
				require.NoError(t, os.MkdirAll(path, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(path, "main.go"), []byte("package a\nfunc A() {}\n"), 0o644))
			}
			g, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, g.Close()) })
			catalog := g.Catalog()
			require.NoError(t, catalog.UpsertRepositoryFamily(t.Context(), store_sqlite.RepositoryFamily{
				FamilyID: "family", CommonDirIdentity: filepath.Join(owner, ".git"), State: "family_ready",
			}))
			require.NoError(t, catalog.UpsertCheckout(t.Context(), store_sqlite.Checkout{
				CheckoutID: "owner", Incarnation: "one", FamilyID: "family", RootPath: owner,
				GitDir: filepath.Join(owner, ".git"), AdminName: "main", State: store_sqlite.CheckoutStateReady,
				DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated,
			}))
			binding := store_sqlite.DedicatedGraph{GraphID: indexer.GraphIDFor("src"), OwnerCheckoutID: "owner",
				FamilyID: "family", RepoPrefix: "src", IsPrimaryBase: true, State: "graph_ready"}
			require.NoError(t, catalog.UpsertDedicatedGraph(t.Context(), binding))
			_, err = catalog.AcquireDedicatedBaseAuthority(t.Context(), store_sqlite.AcquireDedicatedBaseAuthorityRequest{
				GraphID: binding.GraphID,
				Owner:   store_sqlite.DedicatedBaseOwner{CheckoutID: "owner", Incarnation: "one"},
				Token:   "warmup-owner",
			})
			require.NoError(t, err)
			cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
			require.NoError(t, err)
			// A published owner must win even when a colliding entry precedes it.
			cm.Global().Repos = []config.RepoEntry{{Path: rejected, Name: "src"}}
			if includeOwner {
				cm.Global().Repos = append(cm.Global().Repos, config.RepoEntry{Path: owner, Name: "src"})
			}
			g.AddNode(&graph.Node{ID: "src/old.go::Owner", Name: "Owner", Kind: graph.KindFunction,
				FilePath: "src/old.go", RepoPrefix: "src", Language: "go"})
			require.NotEmpty(t, g.GetRepoNodes("src"))
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			var mu sync.Mutex
			var attempted []string
			mi.SetOnRepoTracked(func(_, path string) { mu.Lock(); attempted = append(attempted, path); mu.Unlock() })
			state := &daemonState{graph: g, multiIndexer: mi, configManager: cm}
			watcher, _ := warmupDaemonState(state, zap.NewNop(), func() {})
			if watcher != nil {
				t.Cleanup(func() { _ = watcher.Stop() })
			}
			mu.Lock()
			if includeOwner {
				assert.Equal(t, []string{owner}, attempted, "reject collisions before parallel dispatch")
			} else {
				assert.Equal(t, []string{rejected}, attempted, "an unconfigured catalog owner must not hide the configured root")
			}
			mu.Unlock()
			if includeOwner {
				require.NotNil(t, mi.GetMetadata("src"))
				assert.Equal(t, owner, mi.GetMetadata("src").RootPath)
				load, ok := mi.RepoLoadFor(rejected)
				require.True(t, ok, "a skipped collision must stay in the load table, not read as untracked")
				assert.Equal(t, indexer.RepoLoadFailed, load.State)
				assert.Contains(t, load.Reason, "prefix_collision")
				assert.Equal(t, "prefix_collision", load.ErrorCode)
				assert.Equal(t, owner, load.OwnerPath)
			}
			assert.NotEmpty(t, g.GetRepoNodes("src"), "skipping a collision must not make its owner an orphan")
			after, found, err := catalog.GetDedicatedGraph(t.Context(), binding.GraphID)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, binding, after)
		})
	}
}
