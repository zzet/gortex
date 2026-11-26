package indexer

import (
	"errors"
	"testing"
)

// A committed checkout write's ticket carries the tier evidence a watcher
// tool ticket carries: the acknowledgement at admission, the graph tiers when
// the checkout publishes a generation holding the bytes, and failed tiers when
// the ticket ends without one.
func TestCheckoutRefreshTicketStampsPublicationTiers(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc TieredHelper() {}\n")
	admitted := ticket.Ticket.TierState()
	if admitted.AckAt.IsZero() || admitted.Generation != ticket.Ticket.Generation ||
		!admitted.FileGraphAt.IsZero() || !admitted.RelationsAt.IsZero() || !admitted.DerivedAt.IsZero() ||
		admitted.Failed || admitted.Partial {
		t.Fatalf("an admitted checkout ticket carries only its acknowledgement: %+v (generation %d)", admitted, ticket.Ticket.Generation)
	}

	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("refresh did not publish: %+v", result)
	}
	published := ticket.Ticket.TierState()
	if published.Failed || published.FileGraphAt.IsZero() || published.RelationsAt.IsZero() ||
		published.FileGraphAt.Before(published.AckAt) || published.RelationsAt.Before(published.FileGraphAt) {
		t.Fatalf("a published checkout generation holds the file graph and its relations: %+v", published)
	}
	debt, top, err := c.PendingDerived(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owed := false
	for _, d := range debt {
		for _, p := range d.Paths {
			owed = owed || p == builderRepoPrefix+"/helper.go"
		}
	}
	switch {
	case owed && (!published.Partial || !published.DerivedAt.IsZero()):
		t.Fatalf("route %d owes helper.go derived data %+v, yet the ticket reads complete: %+v", top, debt, published)
	case !owed && (published.Partial || published.DerivedAt.Before(published.RelationsAt)):
		t.Fatalf("route %d owes helper.go nothing, yet derived is not stamped: %+v", top, published)
	}

	stopped := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc StoppedTieredHelper() {}\n")
	_ = c.Close()
	if result := awaitCheckoutRefresh(t, stopped); !errors.Is(result.Err, ErrCheckoutRefreshStopped) {
		t.Fatalf("stopped ticket: %+v", result)
	}
	failed := stopped.Ticket.TierState()
	if !failed.Failed || failed.AckAt.IsZero() || !failed.FileGraphAt.IsZero() || !failed.RelationsAt.IsZero() || !failed.DerivedAt.IsZero() {
		t.Fatalf("a ticket ended without publication reports failed tiers and no stamps: %+v", failed)
	}
}
