package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The route moves between an edit's view selection and its admission (a
// rebuild publishes in between, as a base advance does). Admission is refused
// as a moved route, the edit selects its view once more and is admitted
// against the new route; nothing is refused and the edit lands.
func TestEditAdmissionToleratesARouteMovedAfterSelection(t *testing.T) {
	fixture := newRealCheckoutMutationFixture(t)
	cwd := fixture.worktree
	ctx := context.Background()
	before, _, err := fixture.store.Catalog().GetCheckoutRoute(ctx, fixture.checkoutID)
	require.NoError(t, err)

	moved := false
	previous := mutationBeforeAdmission
	mutationBeforeAdmission = func(context.Context) {
		if moved {
			return
		}
		moved = true
		require.NoError(t, os.WriteFile(filepath.Join(fixture.worktree, "moved.go"), []byte("package repo\n\nfunc Moved() {}\n"), 0o644))
		ticket, err := fixture.srv.lifecycle.RequestCheckoutRefresh(ctx, fixture.checkoutID, fixture.worktree)
		require.NoError(t, err)
		select {
		case result := <-ticket.Ticket.Done:
			require.NoError(t, result.Err)
		case <-time.After(20 * time.Second):
			t.Fatal("the injected route move never published")
		}
	}
	t.Cleanup(func() { mutationBeforeAdmission = previous })

	written := fixture.edit(t, cwd, map[string]any{
		"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func AdmittedAfterAMove() {}",
	})
	require.True(t, moved, "the route move was never injected")
	require.False(t, written.IsError, viewResultText(t, written))
	after, _, err := fixture.store.Catalog().GetCheckoutRoute(ctx, fixture.checkoutID)
	require.NoError(t, err)
	require.NotEqual(t, before.RouteEpoch, after.RouteEpoch)
	data, err := os.ReadFile(filepath.Join(fixture.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(data), "func AdmittedAfterAMove() {}")
	fixture.awaitMutation(t, cwd, written)
}
