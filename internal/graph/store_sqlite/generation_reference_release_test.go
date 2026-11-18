package store_sqlite

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// A route re-installed or repointed, and a dedicated graph whose active
// pointer moves, report the generations they stopped naming — the releases a
// parked retirement waits for. A write that keeps what it named reports
// nothing.
func TestRouteAndDedicatedWritesNameTheGenerationsTheyRelease(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")
	published := func() int64 {
		id := seedBuildingGeneration(t, catalog, "graph-1")
		require.NoError(t, catalog.PublishViewGeneration(ctx, id, 600))
		return id
	}
	first, second := published(), published()

	var mu sync.Mutex
	var releases []GenerationReferenceRelease
	OnGenerationReferencesReleased(func(release GenerationReferenceRelease) {
		mu.Lock()
		releases = append(releases, release)
		mu.Unlock()
	})
	t.Cleanup(func() { OnGenerationReferencesReleased(nil) })
	take := func() []GenerationReferenceRelease {
		mu.Lock()
		defer mu.Unlock()
		out := releases
		releases = nil
		return out
	}

	route := CheckoutRoute{CheckoutID: "wt", GraphID: "graph-1", CommitGenerationID: first, State: RouteActive}
	require.NoError(t, catalog.UpsertCheckoutRoute(ctx, route))
	require.Empty(t, take(), "a new route released something")

	require.NoError(t, catalog.FlipCheckoutRoute(ctx, FlipCheckoutRouteRequest{
		CheckoutID: "wt", ExpectedRouteEpoch: 0, GraphID: "graph-1", CommitGenerationID: second, State: RouteActive,
	}))
	require.Equal(t, []GenerationReferenceRelease{{Released: []int64{first}}}, take())

	route.CommitGenerationID, route.RouteEpoch = second, 1
	require.NoError(t, catalog.UpsertCheckoutRoute(ctx, route))
	require.Empty(t, take(), "re-installing the same route released something")
	route.CommitGenerationID = first
	require.NoError(t, catalog.UpsertCheckoutRoute(ctx, route))
	require.Equal(t, []GenerationReferenceRelease{{Released: []int64{second}}}, take())

	dedicated := DedicatedGraph{
		GraphID: "graph-1", OwnerCheckoutID: "wt", RepoPrefix: "wt-prefix", FamilyID: "fam",
		ActiveGenerationID: second, State: "graph_ready",
	}
	require.NoError(t, catalog.UpsertDedicatedGraph(ctx, dedicated))
	require.Empty(t, take(), "a new dedicated graph released something")
	dedicated.ActiveGenerationID = 0
	require.NoError(t, catalog.UpsertDedicatedGraph(ctx, dedicated))
	require.Equal(t, []GenerationReferenceRelease{{Released: []int64{second}}}, take())
}

// A removal names the base its row held when it was deleted. Retirement reads
// the row before it fences it, and a rebase may move the layer onto another
// base in between: the release then named the old base, and the true one —
// parked on its "based" reference alone, waiting for a release naming it —
// waited for the recheck floor.
func TestARemovalNamesTheBaseItsRowHeldWhenDeleted(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")
	published := func() int64 {
		id := seedBuildingGeneration(t, catalog, "graph-1")
		require.NoError(t, catalog.PublishViewGeneration(ctx, id, 600))
		return id
	}
	oldBase, newBase := published(), published()
	layer, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind: "checkout", GraphID: "graph-1", CheckoutID: "wt", GenerationKind: "dirty",
		BaseGenerationID: oldBase, State: ViewGenerationBuilding, CreatedAt: 601,
	})
	require.NoError(t, err)
	require.NoError(t, catalog.PublishViewGeneration(ctx, layer, 602))

	var mu sync.Mutex
	var removals []GenerationReferenceRelease
	OnGenerationReferencesReleased(func(release GenerationReferenceRelease) {
		mu.Lock()
		defer mu.Unlock()
		if release.Removed != 0 {
			removals = append(removals, release)
		}
	})
	t.Cleanup(func() { OnGenerationReferencesReleased(nil) })

	// The in-use check runs after retirement read the row and before it
	// fences it: the rebase lands there.
	var rebased sync.Once
	inUse := func(int64) bool {
		rebased.Do(func() {
			require.NoError(t, catalog.RebaseViewGeneration(ctx, RebaseViewGenerationRequest{
				GenerationID: layer, FromBase: oldBase, ToBase: newBase,
			}))
		})
		return false
	}
	require.NoError(t, store.RetirePayloadGeneration(ctx, layer, inUse))
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []GenerationReferenceRelease{{Removed: layer, Released: []int64{newBase}}}, removals)
}

// A refusal knows which references held the generation: only one held by
// generations built on it alone is waiting for a release that names it.
func TestARetirementRefusalNamesItsReferences(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")
	base := seedBuildingGeneration(t, catalog, "graph-1")
	require.NoError(t, catalog.PublishViewGeneration(ctx, base, 600))
	_, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind: "checkout", GraphID: "graph-1", CheckoutID: "wt", GenerationKind: "dirty",
		BaseGenerationID: base, State: ViewGenerationBuilding, CreatedAt: 601,
	})
	require.NoError(t, err)

	err = store.RetirePayloadGeneration(ctx, base, nil)
	require.ErrorIs(t, err, ErrCatalogGenerationReferenced)
	var referenced *GenerationReferencedError
	require.ErrorAs(t, err, &referenced)
	require.True(t, referenced.Refs.OnlyBased(), "%+v", referenced.Refs)

	require.NoError(t, catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: "wt", GraphID: "graph-1", CommitGenerationID: base, State: RouteActive,
	}))
	err = store.RetirePayloadGeneration(ctx, base, nil)
	require.ErrorAs(t, err, &referenced)
	require.False(t, referenced.Refs.OnlyBased(), "a routed generation waits on a release no write may name")
}
