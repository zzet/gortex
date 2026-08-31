package store_sqlite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireGraphTopologyFenceInUse(t *testing.T, catalog *Catalog, graphID string, want bool) {
	t.Helper()
	inUse, err := catalog.GraphTopologyFenceInUse(context.Background(), graphID)
	require.NoError(t, err)
	require.Equal(t, want, inUse)
}

func TestCatalogGraphTopologyFenceInUseTracksEveryDurableOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("absent", func(t *testing.T) {
		catalog := openCatalogStore(t).Catalog()
		requireGraphTopologyFenceInUse(t, catalog, "graph-absent", false)
	})

	t.Run("dedicated graph and route", func(t *testing.T) {
		catalog := openCatalogStore(t).Catalog()
		const (
			familyID    = "family-fence-route"
			checkoutID  = "checkout-fence-route"
			incarnation = "incarnation-fence-route"
			graphID     = "graph-fence-route"
		)
		seedFamilyAndCheckout(t, catalog, familyID, checkoutID, incarnation)
		require.NoError(t, catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
			GraphID: graphID, OwnerCheckoutID: checkoutID, RepoPrefix: "repo-fence-route",
			FamilyID: familyID, State: "graph_ready",
		}))
		require.NoError(t, catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
			CheckoutID: checkoutID, GraphID: graphID, RouteEpoch: 1, State: RouteActive,
		}))
		requireGraphTopologyFenceInUse(t, catalog, graphID, true)

		require.NoError(t, catalog.DeleteCheckoutRoute(ctx, checkoutID))
		requireGraphTopologyFenceInUse(t, catalog, graphID, true)
		deleted, err := catalog.DeleteDedicatedGraphForIncarnation(
			ctx, graphID, checkoutID, incarnation,
		)
		require.NoError(t, err)
		require.True(t, deleted)
		requireGraphTopologyFenceInUse(t, catalog, graphID, false)
	})

	t.Run("orphaned generation", func(t *testing.T) {
		catalog := openCatalogStore(t).Catalog()
		const graphID = "graph-fence-generation"
		generationID, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
			OwnerKind: "dedicated_base", GraphID: graphID,
			LayerID: graphID + ":base", GenerationKind: "dedicated_base",
			TreeOID: "tree-fence-generation", State: ViewGenerationSuperseded,
		})
		require.NoError(t, err)
		requireGraphTopologyFenceInUse(t, catalog, graphID, true)
		require.NoError(t, catalog.DeleteViewGeneration(ctx, generationID))
		requireGraphTopologyFenceInUse(t, catalog, graphID, false)
	})

	t.Run("ref view", func(t *testing.T) {
		catalog := openCatalogStore(t).Catalog()
		const graphID = "graph-fence-ref-view"
		require.NoError(t, catalog.UpsertRefView(ctx, RefView{
			RefViewID: "ref-view-fence", GraphID: graphID,
			SelectorKind: "git_ref", SelectorValue: "refs/heads/feature",
			DesiredRef: "refs/heads/feature", EnrichmentProfile: "structural",
			State: RefViewPending,
		}))
		requireGraphTopologyFenceInUse(t, catalog, graphID, true)
		require.NoError(t, catalog.DeleteRefView(ctx, "ref-view-fence"))
		requireGraphTopologyFenceInUse(t, catalog, graphID, false)
	})
}
