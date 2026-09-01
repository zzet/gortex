package store_sqlite

import (
	"context"
	"errors"
	"testing"
)

func TestCatalogUpdateCheckoutHeadIsNarrowAndGuarded(t *testing.T) {
	catalog := openCatalogStore(t).Catalog()
	ctx := context.Background()
	seedFamilyAndCheckout(t, catalog, "family-head", "checkout-head", "incarnation-head")

	before, found, err := catalog.GetCheckout(ctx, "checkout-head")
	if err != nil || !found {
		t.Fatalf("GetCheckout before update: found=%v err=%v", found, err)
	}
	req := UpdateCheckoutHeadRequest{
		CheckoutID:         before.CheckoutID,
		Incarnation:        before.Incarnation,
		ExpectedRootPath:   before.RootPath,
		ExpectedHeadRef:    before.HeadRef,
		ExpectedHeadCommit: before.HeadCommit,
		ExpectedHeadTree:   before.HeadTree,
		HeadRef:            "refs/heads/feature",
		HeadCommit:         "beef",
		HeadTree:           "cafe",
	}
	if err := catalog.UpdateCheckoutHead(ctx, req); err != nil {
		t.Fatalf("UpdateCheckoutHead: %v", err)
	}
	after, found, err := catalog.GetCheckout(ctx, before.CheckoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckout after update: found=%v err=%v", found, err)
	}
	if after.HeadRef != req.HeadRef || after.HeadCommit != req.HeadCommit || after.HeadTree != req.HeadTree {
		t.Fatalf("HEAD = %q/%q/%q, want %q/%q/%q",
			after.HeadRef, after.HeadCommit, after.HeadTree,
			req.HeadRef, req.HeadCommit, req.HeadTree)
	}
	if after.State != before.State || after.RootPath != before.RootPath ||
		after.DesiredMode != before.DesiredMode || after.EffectiveMode != before.EffectiveMode ||
		after.LastSeen != before.LastSeen || after.LastAccessible != before.LastAccessible {
		t.Fatalf("head-only update changed another lifecycle axis: before=%+v after=%+v", before, after)
	}

	assertStale := func(name string, mutate func(*UpdateCheckoutHeadRequest)) {
		t.Helper()
		stale := UpdateCheckoutHeadRequest{
			CheckoutID:         after.CheckoutID,
			Incarnation:        after.Incarnation,
			ExpectedRootPath:   after.RootPath,
			ExpectedHeadRef:    after.HeadRef,
			ExpectedHeadCommit: after.HeadCommit,
			ExpectedHeadTree:   after.HeadTree,
			HeadRef:            "refs/heads/stale",
			HeadCommit:         "dead",
			HeadTree:           "fade",
		}
		mutate(&stale)
		if err := catalog.UpdateCheckoutHead(ctx, stale); !errors.Is(err, ErrCatalogStaleGuard) {
			t.Fatalf("%s: error = %v, want ErrCatalogStaleGuard", name, err)
		}
		current, _, readErr := catalog.GetCheckout(ctx, after.CheckoutID)
		if readErr != nil {
			t.Fatalf("%s: GetCheckout: %v", name, readErr)
		}
		if current.HeadRef != after.HeadRef || current.HeadCommit != after.HeadCommit || current.HeadTree != after.HeadTree {
			t.Fatalf("%s: stale update changed HEAD: %+v", name, current)
		}
	}
	assertStale("incarnation", func(req *UpdateCheckoutHeadRequest) { req.Incarnation = "older-incarnation" })
	assertStale("root", func(req *UpdateCheckoutHeadRequest) { req.ExpectedRootPath += "-old" })
	assertStale("prior head", func(req *UpdateCheckoutHeadRequest) { req.ExpectedHeadTree = "older-tree" })

	if err := catalog.UpdateCheckoutState(ctx, UpdateCheckoutStateRequest{
		CheckoutID:    after.CheckoutID,
		Incarnation:   after.Incarnation,
		State:         CheckoutStateReconciling,
		DesiredMode:   after.DesiredMode,
		EffectiveMode: after.EffectiveMode,
		LastSeen:      after.LastSeen,
	}); err != nil {
		t.Fatalf("move checkout out of ready state: %v", err)
	}
	assertStale("state", func(*UpdateCheckoutHeadRequest) {})
}

func BenchmarkCheckoutHeadObservationCAS(b *testing.B) {
	catalog := openCatalogStore(b).Catalog()
	ctx := context.Background()
	seedFamilyAndCheckout(b, catalog, "family-head-bench", "checkout-head-bench", "incarnation-head-bench")
	current, found, err := catalog.GetCheckout(ctx, "checkout-head-bench")
	if err != nil || !found {
		b.Fatalf("GetCheckout: found=%v err=%v", found, err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next := UpdateCheckoutHeadRequest{
			CheckoutID:         current.CheckoutID,
			Incarnation:        current.Incarnation,
			ExpectedRootPath:   current.RootPath,
			ExpectedHeadRef:    current.HeadRef,
			ExpectedHeadCommit: current.HeadCommit,
			ExpectedHeadTree:   current.HeadTree,
			HeadRef:            "refs/heads/benchmark-a",
			HeadCommit:         "commit-a",
			HeadTree:           "tree-a",
		}
		if i%2 != 0 {
			next.HeadRef = "refs/heads/benchmark-b"
			next.HeadCommit = "commit-b"
			next.HeadTree = "tree-b"
		}
		if err := catalog.UpdateCheckoutHead(ctx, next); err != nil {
			b.Fatal(err)
		}
		current.HeadRef, current.HeadCommit, current.HeadTree = next.HeadRef, next.HeadCommit, next.HeadTree
	}
}
