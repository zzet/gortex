package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestSourceMutationWriteIntentRestoresAdmissionAndCancelsWait(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "intent.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	start := func(ctx context.Context) (context.Context, *sourceMutationWriteIntent) {
		intent := &sourceMutationWriteIntent{announce: store.AnnounceWrite}
		intent.release = intent.announce()
		return context.WithValue(ctx, sourceMutationWriteIntentKey{}, intent), intent
	}

	ctx, intent := start(context.Background())
	require.True(t, store.WriteWanted(), "selection and mutation admission preempt background writers")
	outer := suspendSourceMutationWriteIntent(ctx)
	require.False(t, store.WriteWanted(), "the prior publication can finish while this request waits")
	inner := suspendSourceMutationWriteIntent(ctx)
	inner()
	inner()
	require.False(t, store.WriteWanted(), "a nested wait cannot restore an enclosing wait's announcement")
	outer()
	outer()
	require.True(t, store.WriteWanted(), "mutation admission regains its original writer preemption")
	intent.close()
	intent.close()
	require.False(t, store.WriteWanted())

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelCtx, canceledIntent := start(cancelCtx)
	resume := suspendSourceMutationWriteIntent(cancelCtx)
	cancel()
	resume()
	require.False(t, store.WriteWanted(), "canceling an unwritten waiting request announces no new write")
	canceledIntent.close()

	closedCtx, closedIntent := start(context.Background())
	resume = suspendSourceMutationWriteIntent(closedCtx)
	closedIntent.close()
	resume()
	require.False(t, store.WriteWanted(), "cleanup cannot be undone by a late wait return")
}

func TestSelectedCheckoutSourceMutationStillAnnouncesStoreIntent(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "checkout-intent.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	srv, root := setupTestServer(t)
	srv.materializer = &graphview.Materializer{Store: store}
	selected := withCheckoutMutation(t.Context(), &fakeCheckoutMutationLifecycle{}, root)
	_, release := srv.withSourceMutationWriteIntent(selected, "edit_file")
	require.True(t, store.WriteWanted(), "a selected store-writing checkout retains foreground preemption")
	release()
	require.False(t, store.WriteWanted())
	_, releaseRead := srv.withSourceMutationWriteIntent(selected, "get_symbol")
	defer releaseRead()
	require.False(t, store.WriteWanted(), "read requests never acquire a writer intent")
}
