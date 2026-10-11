package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The census half of the disk-full surface.
//
// A store that cannot delete payload looks, from every count in the census,
// exactly like a store that has nothing to delete: Generations says how many
// generations are in each state, and a generation stuck in retiring because
// the volume is full is counted there beside one that is simply mid-sweep.
//
// Retirement has no caller to fail — the coordinator backlog and the
// lifecycle's owed set retry it forever in the background — so until this the
// reason lived only in a daemon log line, which is a moment rather than the
// daemon's present. store_sqlite.SafeStorageFailureReason had no caller at all.

// TestAViewCensusStatesWhyAGenerationCannotBeCollected is the production trace
// for the reader half: the census a status poll assembles carries the store's
// stated storage failures.
//
// The store half is proven separately and end to end — see
// TestPrivateRetirementSweepStopsCleanlyOnAFullVolume in
// internal/graph/store_sqlite, which pins a real SQLITE_FULL raised inside the
// retirement sweep onto exactly the register this reads. Here the register is
// written through the same production method that sweep failure calls
// (Store.RecordStorageFailure) so the assertion is about the census, not about
// the classification.
//
// Revert-red: delete the `out.StorageFailures = l.store.StorageFailures()`
// line from ViewsHealth and the first length assertion fails.
func TestAViewCensusStatesWhyAGenerationCannotBeCollected(t *testing.T) {
	f := newFamilyFixture(t, "storage-failure-census")
	defer f.close()
	ctx := context.Background()

	health, err := f.lc.ViewsHealth(ctx)
	require.NoError(t, err)
	require.Empty(t, health.StorageFailures,
		"a daemon whose storage is healthy still reports a storage failure")

	// A real generation of this daemon's own store, so the census names
	// something a person can find in the rest of the payload.
	generationID := readinessGeneration(t, f)

	// The write gate's own classification: panicOnFatal wraps a full volume
	// into exactly this type before any recovery sees it.
	require.True(t, f.store.RecordStorageFailure(generationID, &store_sqlite.StorageError{}),
		"a storage failure was not recorded against the generation it happened to")

	health, err = f.lc.ViewsHealth(ctx)
	require.NoError(t, err)
	require.Len(t, health.StorageFailures, 1,
		"the census counts the generations this store holds and says nothing about the one it "+
			"cannot collect; the count reads as ordinary retirement and explains nothing")
	require.Equal(t, generationID, health.StorageFailures[0].GenerationID)
	require.NotEmpty(t, health.StorageFailures[0].Reason,
		"the census carries a failure with no stated reason")
	require.Contains(t, health.StorageFailures[0].Reason, "graph storage",
		"the stated reason does not name what failed")

	// And the census is about the daemon's present: a generation whose
	// storage recovered stops being reported.
	f.store.ClearStorageFailure(generationID)
	health, err = f.lc.ViewsHealth(ctx)
	require.NoError(t, err)
	require.Empty(t, health.StorageFailures,
		"a generation whose storage recovered is still reported as failing")
	require.NotEmpty(t, health.Generations,
		"the rest of the census was displaced by the field that was added")
}

// TestOnlyStorageFailuresReachTheCensus keeps the surface worth reading. Every
// background pass fails routinely — a cancelled context, a lost retirement
// fence, a lease that had not dropped yet — and a census that reported those
// as storage problems would put "check the store volume" in front of a person
// whose volume is fine.
func TestOnlyStorageFailuresReachTheCensus(t *testing.T) {
	f := newFamilyFixture(t, "storage-failure-classification")
	defer f.close()
	ctx := context.Background()

	generationID := readinessGeneration(t, f)

	for _, ordinary := range []error{
		context.Canceled,
		errors.New("payload generation gc: generation left the retiring state"),
		store_sqlite.ErrPayloadGenerationInUse,
	} {
		require.False(t, f.store.RecordStorageFailure(generationID, ordinary),
			"%v was recorded as a storage failure", ordinary)
	}

	health, err := f.lc.ViewsHealth(ctx)
	require.NoError(t, err)
	require.Empty(t, health.StorageFailures,
		"ordinary background-pass failures reached the storage-failure surface")
}

// readinessGeneration mints one payload generation in the fixture's own store
// through the production lifecycle entry point, opens the handle a builder
// would write it through, and reports its id.
//
// The census names generations by id, so a test that recorded a failure
// against an id the catalog never minted would assert on a surface no operator
// could follow back into the rest of the payload. The handle matters for the
// same reason from the other side: the register records only against
// generations the store is actually holding, and holding one is what opening a
// handle on it means (store_sqlite.Store.AtGeneration).
func readinessGeneration(t *testing.T, f *familyFixture) int64 {
	t.Helper()
	ctx := context.Background()
	generationID, _, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        f.primaryGraph,
		CheckoutID:     f.automatic.CheckoutID,
		LayerID:        "layer-readiness",
		GenerationKind: "dirty",
		TreeOID:        "tree-readiness",
		ConfigHash:     "config-readiness",
		CreatedAt:      1000,
	})
	require.NoError(t, err, "the fixture could not mint a payload generation for the census to name")
	require.Positive(t, generationID)
	require.NotNil(t, f.store.AtGeneration(generationID),
		"the store would not open a handle on the generation it just minted")
	return generationID
}
