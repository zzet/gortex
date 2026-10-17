package store_sqlite

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func reparseWindowFixture(t *testing.T) (*Store, *Store, int64) {
	t.Helper()
	store, _ := openTempStore(t)
	id, handle, err := store.BeginPayloadGeneration(t.Context(), PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: "reparse-graph",
		GenerationKind: "dedicated", TreeOID: "tree", ConfigHash: "config",
		ExtractorVersions: `{"go":"1"}`, ResolverVersion: "resolver", CreatedAt: 1,
	})
	require.NoError(t, err)
	return store, handle, id
}

func TestManagedDedicatedReparseWindowPreservesPopulatedRowsAndWriteShape(t *testing.T) {
	store, handle, id := reparseWindowFixture(t)
	base := &graph.Node{ID: "base::Stable", Name: "Stable", Kind: graph.KindFunction, RepoPrefix: "base"}
	store.AddNode(base)
	base = store.GetNode(base.ID)
	other := store.AtGeneration(91)
	other.AddNode(base)
	otherBase := other.GetNode(base.ID)
	// Real external attribution deliberately keeps dep:: symbols unowned.
	residual := &graph.Node{ID: "dep::example/dep::Call", Name: "Call", Kind: graph.KindFunction, Language: "go"}
	require.NoError(t, handle.AddBatchChecked([]*graph.Node{residual}, nil))
	residual = handle.GetNode(residual.ID)
	opened, err := store.BeginGenerationBulkLoad(id)
	require.False(t, opened)
	require.ErrorIs(t, err, ErrGenerationBulkLoadPopulated)
	beforeCache := pragmaIntDB(t, store.writerDB, "cache_size")
	beforeSync := pragmaIntDB(t, store.writerDB, "synchronous")
	beforeAuto := pragmaIntDB(t, store.writerDB, "wal_autocheckpoint")
	opened, err = handle.BeginManagedDedicatedReparseBulkLoad(t.Context(), id)
	require.NoError(t, err)
	require.True(t, opened)
	t.Cleanup(func() { require.NoError(t, handle.EndGenerationBulkLoadFor(id)) })
	cache, auto, active := store.GenerationBulkLoadShape()
	require.True(t, active)
	require.Equal(t, int64(-262144), cache)
	require.Zero(t, auto)
	syncMode, err := pragmaInt(t.Context(), store.bulkConn, "synchronous")
	require.NoError(t, err)
	require.Equal(t, beforeSync, syncMode)
	fresh := &graph.Node{ID: "repo::Fresh", Name: "Fresh", Kind: graph.KindFunction, RepoPrefix: "repo"}
	require.NoError(t, handle.AddBatchChecked([]*graph.Node{fresh}, nil))
	require.Equal(t, residual, handle.GetNode(residual.ID))
	require.Equal(t, "Fresh", handle.GetNode(fresh.ID).Name)
	require.Equal(t, base, store.GetNode(base.ID))
	require.Equal(t, otherBase, other.GetNode(base.ID))
	require.NoError(t, handle.EndGenerationBulkLoadFor(id))
	_, active = store.InGenerationBulkLoad()
	require.False(t, active)
	require.Equal(t, beforeCache, pragmaIntDB(t, store.writerDB, "cache_size"))
	require.Equal(t, beforeSync, pragmaIntDB(t, store.writerDB, "synchronous"))
	require.Equal(t, beforeAuto, pragmaIntDB(t, store.writerDB, "wal_autocheckpoint"))
	store.backgroundCheckpoint.mu.Lock()
	lease, bound := store.backgroundCheckpoint.generationLease, store.backgroundCheckpoint.boundGeneration
	store.backgroundCheckpoint.mu.Unlock()
	require.Zero(t, lease, "closing the window must release its checkpoint suppression")
	require.Zero(t, bound)
}

func TestManagedDedicatedReparseWindowRefusesUnqualifiedOrClosedPayloads(t *testing.T) {
	for _, variant := range []string{"base", "unmanaged", "wrong_id", "missing", "wrong_owner", "wrong_kind", "noninitial", "layered", "lower_fingerprint", "ready", "retiring", "canceled"} {
		t.Run(variant, func(t *testing.T) {
			store, handle, id := reparseWindowFixture(t)
			ctx := t.Context()
			update := func(query string, args ...any) {
				store.writeMu.Lock()
				defer store.writeMu.Unlock()
				_, err := store.writerDB.ExecContext(t.Context(), query, args...)
				require.NoError(t, err)
			}
			switch variant {
			case "base":
				handle, id = store, 0
			case "unmanaged":
				handle = store.AtGeneration(id)
			case "wrong_id":
				id++
			case "missing":
				handle, _ = store.AtManagedGeneration(id + 100)
				id += 100
			case "wrong_owner":
				update(`UPDATE view_generations SET owner_kind='checkout' WHERE generation_id=?`, id)
			case "wrong_kind":
				update(`UPDATE view_generations SET generation_kind='dirty' WHERE generation_id=?`, id)
			case "noninitial":
				lowerID, _, err := store.BeginPayloadGeneration(t.Context(), PayloadGenerationRequest{
					OwnerKind: "dedicated_graph", GraphID: "lower-graph", GenerationKind: "dedicated", TreeOID: "lower-tree",
					ConfigHash: "config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "resolver", CreatedAt: 1,
				})
				require.NoError(t, err)
				update(`UPDATE view_generations SET base_generation_id=? WHERE generation_id=?`, lowerID, id)
			case "layered":
				update(`UPDATE view_generations SET layer_id='layer' WHERE generation_id=?`, id)
			case "lower_fingerprint":
				update(`UPDATE view_generations SET lower_view_fingerprint='lower' WHERE generation_id=?`, id)
			case "ready", "retiring":
				update(`UPDATE view_generations SET state=? WHERE generation_id=?`, variant, id)
			case "canceled":
				child, cancel := context.WithCancel(ctx)
				cancel()
				ctx = child
			}
			opened, err := handle.BeginManagedDedicatedReparseBulkLoad(ctx, id)
			require.Error(t, err)
			require.False(t, opened)
			_, active := store.InGenerationBulkLoad()
			require.False(t, active)
		})
	}
}

func TestManagedDedicatedReparseWindowCheckpointBusyAndCancellationReleaseLease(t *testing.T) {
	store, handle, id := reparseWindowFixture(t)
	store.passiveCheckpointTimeout = 25 * time.Millisecond
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); waitCheckpointSignal(t, done) })
	go func() {
		defer close(done)
		store.runBackgroundCheckpointAttempt(func(context.Context) (bool, bool) { close(entered); <-release; return false, true })
	}()
	waitCheckpointSignal(t, entered)
	opened, err := handle.BeginManagedDedicatedReparseBulkLoad(t.Context(), id)
	require.False(t, opened)
	require.ErrorIs(t, err, ErrGenerationBulkCheckpointBusy)
	store.passiveCheckpointTimeout = time.Second
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	actorDone := make(chan struct{})
	t.Cleanup(func() { cancel(); waitCheckpointSignal(t, actorDone) })
	result := make(chan checkpointBeginResult, 1)
	go func() {
		defer close(actorDone)
		opened, err := handle.BeginManagedDedicatedReparseBulkLoad(ctx, id)
		result <- checkpointBeginResult{opened, err}
	}()
	// Wait until the coordination owner exists before asking it to cancel.
	require.Eventually(t, func() bool {
		store.backgroundCheckpoint.mu.Lock()
		defer store.backgroundCheckpoint.mu.Unlock()
		return store.backgroundCheckpoint.generationLease != 0
	}, time.Second, time.Millisecond)
	cancel()
	got := waitCheckpointBeginResult(t, result)
	require.False(t, got.opened)
	require.ErrorIs(t, got.err, context.Canceled)
	unblock()
	waitCheckpointSignal(t, done)
	opened, err = handle.BeginManagedDedicatedReparseBulkLoad(t.Context(), id)
	require.NoError(t, err)
	require.True(t, opened)
	require.NoError(t, handle.EndGenerationBulkLoadFor(id))
}
