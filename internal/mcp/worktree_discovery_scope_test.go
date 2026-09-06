package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestWorktreeDiscoveryDeniesForeignSelectorBeforeCatalogSideEffects(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	foreignPrimary := filepath.Join(filepath.Dir(f.primary), "foreign")
	foreignWorktree := filepath.Join(filepath.Dir(f.primary), "foreign-agent")
	require.NoError(t, os.Mkdir(foreignPrimary, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(foreignPrimary, "foreign.go"), []byte("package foreign\n\nfunc Foreign() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(foreignPrimary, ".gortex.yaml"), []byte("workspace: foreign-workspace\n"), 0o644))
	checkoutMutationGit(t, foreignPrimary, "init", "--initial-branch=main")
	checkoutMutationGit(t, foreignPrimary, "add", "-A")
	checkoutMutationGit(t, foreignPrimary, "commit", "-m", "foreign base")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	registered, err := f.srv.lifecycle.Register(ctx, config.RepoEntry{Path: foreignPrimary, Name: "foreign"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)

	// Register only discovers the worktrees present at that instant. This
	// fixture never starts the daemon's topology watcher or family sweep.
	// Creating the linked worktree afterwards makes the explicit selector the
	// first possible observation of this previously unknown checkout.
	checkoutMutationGit(t, foreignPrimary, "worktree", "add", "-b", "foreign-agent", foreignWorktree)
	before, err := f.srv.lifecycle.FamiliesOverview(ctx, registered.FamilyID)
	require.NoError(t, err)
	require.Len(t, before.Families, 1)
	require.Len(t, before.Families[0].Checkouts, 1, "foreign linked checkout must be unseen before the request")
	primaryBefore := before.Families[0].Checkouts[0]
	require.True(t, pathkey.EqualPaths(primaryBefore.RootPath, foreignPrimary))

	result := f.edit(t, f.primary, map[string]any{
		"path": "foreign.go", "old_string": "Foreign", "new_string": "Changed", "dry_run": true,
		"view": map[string]any{"kind": "worktree", "path": foreignWorktree},
	})
	require.True(t, result.IsError, "a selector in another workspace must be denied")
	require.Contains(t, fmt.Sprint(result.Content), "selector_out_of_scope", "deny authorization, not merely unavailable graph readiness")

	after, err := f.srv.lifecycle.FamiliesOverview(ctx, registered.FamilyID)
	require.NoError(t, err)
	require.Len(t, after.Families, 1)
	require.Len(t, after.Families[0].Checkouts, 1, "authorization must precede checkout registration and activation")
	primaryAfter := after.Families[0].Checkouts[0]
	require.Equal(t, primaryBefore.CheckoutID, primaryAfter.CheckoutID)
	require.True(t, pathkey.EqualPaths(primaryAfter.RootPath, foreignPrimary))
	require.Equal(t, primaryBefore.CoordinatorLive, primaryAfter.CoordinatorLive,
		"denied observation must not change the foreign family's coordinator state")
	// A coordinator requires a catalog checkout ID. No linked checkout row,
	// plus unchanged family coordinator state, proves no foreign activation
	// was admitted through this real MCP request; the indexer-level regression
	// separately checks its private pending-activation registry.
	contents, err := os.ReadFile(filepath.Join(foreignWorktree, "foreign.go"))
	require.NoError(t, err)
	require.Equal(t, "package foreign\n\nfunc Foreign() {}\n", string(contents))
}
