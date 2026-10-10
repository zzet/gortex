package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

// A checkout whose cycles publish nothing reaches `daemon status` through
// viewsStatusFromHealth with every field of its run, in the census's order;
// a census with no stall renders no publication_stalls key.
func TestViewsStatusCarriesEveryPublicationStall(t *testing.T) {
	health := indexer.ViewsHealth{
		Families:     1,
		Coordinators: 2,
		PublicationStalls: []indexer.CheckoutPublicationStall{
			{CheckoutID: "co-a", ConsecutiveNonpublishingCycles: 5, Since: 1_800_000_000,
				LastPublicationAgeSeconds: 61, StallReason: "failed:catalog_guard", ChangeSetSize: 3},
			{CheckoutID: "co-b", ConsecutiveNonpublishingCycles: 3, Since: 1_800_000_030,
				LastPublicationAgeSeconds: -1, StallReason: "torn_by_motion", ChangeSetSize: 0},
		},
	}

	status := viewsStatusFromHealth(health)
	require.Equal(t, []daemon.PublicationStall{
		{CheckoutID: "co-a", ConsecutiveNonpublishingCycles: 5, Since: 1_800_000_000,
			LastPublicationAgeSeconds: 61, StallReason: "failed:catalog_guard", ChangeSetSize: 3},
		{CheckoutID: "co-b", ConsecutiveNonpublishingCycles: 3, Since: 1_800_000_030,
			LastPublicationAgeSeconds: -1, StallReason: "torn_by_motion", ChangeSetSize: 0},
	}, status.PublicationStalls, "the status payload dropped or changed a publication stall")
	require.Equal(t, 1, status.Families)
	require.Equal(t, 2, status.Coordinators)

	body, err := json.Marshal(status)
	require.NoError(t, err)
	for _, key := range []string{`"publication_stalls"`, `"consecutive_nonpublishing_cycles":5`,
		`"last_publication_age_s":-1`, `"stall_reason":"torn_by_motion"`, `"change_set_size":3`, `"since":1800000030`} {
		require.Contains(t, string(body), key)
	}

	healthy := viewsStatusFromHealth(indexer.ViewsHealth{Families: 1, Coordinators: 1})
	require.Nil(t, healthy.PublicationStalls)
	body, err = json.Marshal(healthy)
	require.NoError(t, err)
	require.NotContains(t, string(body), "publication_stalls", "a census with no stall renders the key")
}
