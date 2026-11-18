package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
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
	// first possible catalog observation of this unknown checkout; the proof
	// preparation below always denies permission to register or activate it.
	checkoutMutationGit(t, foreignPrimary, "worktree", "add", "-b", "foreign-agent", foreignWorktree)
	// Metadata discovery is read-only until this caller authorizes it. A
	// first request may legitimately return pending after 250 ms on cold Git
	// startup; establish that prerequisite without authorizing any catalog
	// observation. The MCP denial below is still one actual request.
	proofDenied := errors.New("fixture declines read-only checkout proof")
	for {
		_, found, proofErr := f.srv.lifecycle.ObserveCheckoutPath(ctx, foreignWorktree, func(prefix string) error {
			require.Equal(t, "foreign", prefix)
			return proofDenied
		})
		require.False(t, found, "a denied proof cannot return a checkout")
		if errors.Is(proofErr, proofDenied) {
			break
		}
		require.ErrorIs(t, proofErr, indexer.ErrCheckoutMutationBusy)
		require.NoError(t, ctx.Err())
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	before, err := f.srv.lifecycle.FamiliesOverview(ctx, registered.FamilyID)
	require.NoError(t, err)
	require.Len(t, before.Families, 1)
	require.Len(t, before.Families[0].Checkouts, 1, "foreign linked checkout must be unseen before the request")
	primaryBefore := before.Families[0].Checkouts[0]
	require.True(t, pathkey.EqualPaths(primaryBefore.RootPath, foreignPrimary))

	// Authorization needs the Git-proven primary of the selected checkout, and
	// a request waits only 250ms for that proof while the shared discovery job
	// keeps running. Slow Git (Windows CI) legitimately answers "pending;
	// retry" first. Retry like an agent would: every attempt must refuse, and
	// once discovery completes the refusal must be the scope denial.
	var result *mcplib.CallToolResult
	deadline := time.Now().Add(20 * time.Second)
	for {
		result = f.edit(t, f.primary, map[string]any{
			"path": "foreign.go", "old_string": "Foreign", "new_string": "Changed", "dry_run": true,
			"view": map[string]any{"kind": "worktree", "path": foreignWorktree},
		})
		require.True(t, result.IsError, "a selector in another workspace must be denied")
		text := viewResultText(t, result)
		if !strings.Contains(text, indexer.ErrCheckoutMutationBusy.Error()) {
			break
		}
		require.True(t, time.Now().Before(deadline), "checkout discovery did not finish: %s", text)
	}
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
