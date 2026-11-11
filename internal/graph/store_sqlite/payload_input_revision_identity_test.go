package store_sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPayloadInputRevisionLazyOwnersRemainDistinctAndConcurrent(t *testing.T) {
	// Alternate constructors and fixture cores must receive identities too.
	a := &Store{storeCore: &storeCore{}}
	b := &Store{storeCore: &storeCore{}}
	const workers = 32
	revisions := make([]PayloadInputRevision, workers)
	var group sync.WaitGroup
	for i := range revisions {
		group.Go(func() { revisions[i] = a.PayloadInputRevision() })
	}
	group.Wait()
	require.NotEqual(t, PayloadInputRevision{}, revisions[0])
	for _, revision := range revisions {
		require.Equal(t, revisions[0], revision)
	}
	require.Equal(t, revisions[0], a.AtGeneration(0).PayloadInputRevision())
	require.NotEqual(t, revisions[0], b.PayloadInputRevision(), "separate uninitialized cores must never impersonate each other")
	require.NotEqual(t, revisions[0], a.AtGeneration(1).PayloadInputRevision())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := a.PayloadInputRevisionContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPayloadInputRevisionReopenedOwnerCannotReuseOldValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	first, err := Open(path)
	require.NoError(t, err)
	before := first.PayloadInputRevision()
	require.NoError(t, first.Close())
	current, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = current.Close() })
	require.NotEqual(t, before, current.PayloadInputRevision(), "a reopened database is a new process-local input owner")
}
