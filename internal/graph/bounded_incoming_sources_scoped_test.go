package graph

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type incomingSourcesLegacyOnlyReader struct {
	Reader
	called bool
}

func (r *incomingSourcesLegacyOnlyReader) FindIncomingSourcesBounded(_ context.Context, ids []string, _ EdgeKind, _ int) (BoundedIncomingSourceProjection, error) {
	r.called = true
	out := emptyIncomingSourceProjection()
	for _, id := range ids {
		out.Sources[id] = []string{"legacy-source"}
	}
	return out, nil
}

func TestIncomingSourcePublicWrapperKeepsLegacyGuardAndDelegationOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	keys := make([]string, MaxBoundedAdjacencyKeys+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("target-%04d", i)
	}
	var absent *OverlaidView
	page, err := absent.FindIncomingSourcesBounded(ctx, keys, EdgeKind("calls"), maxBoundedIncomingSourceLimit)
	if err != nil || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("nil receiver no longer wins guards: %+v %v", page, err)
	}
	view := &OverlaidView{layer: &OverlayLayer{}}
	page, err = view.FindIncomingSourcesBounded(ctx, keys, EdgeKind("calls"), 0)
	if err != nil || len(page.Sources) != 0 || len(page.Truncated) != len(keys) {
		t.Fatalf("zero limit no longer wins cancellation/key budget: %+v %v", page, err)
	}
	base := &incomingSourcesLegacyOnlyReader{}
	view = &OverlaidView{base: base}
	page, err = view.FindIncomingSourcesBounded(context.Background(), []string{"target"}, EdgeKind("calls"), 1)
	if err != nil || !base.called || len(page.Sources["target"]) != 1 || page.Sources["target"][0] != "legacy-source" {
		t.Fatalf("unfiltered legacy-only delegation failed: %+v %v", page, err)
	}
	base.called = false
	page, err = view.FindIncomingSourcesBounded(ctx, []string{"target"}, EdgeKind("calls"), 1)
	if !errors.Is(err, context.Canceled) || base.called || len(page.Sources) != 0 {
		t.Fatalf("cancellation no longer precedes delegation: %+v %v", page, err)
	}
}
