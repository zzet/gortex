package graph

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// The two guards pinned here are the negative space of the scoped
// incoming-source reader: both are reached only when a layer or base
// implementation declines a capability, so no fixture built from the
// in-memory *Graph / *OverlayLayer pair (which implement everything) can
// exercise them. Each fake below implements EXACTLY the surface the
// composition is entitled to, with the capability under test withheld, so
// flipping the guard to fail open turns the assertion red.

// incomingSourceScopedBaseReader is a base that answers the scoped contract
// and nothing else. Its rows are what a fail-open layer guard would project.
type incomingSourceScopedBaseReader struct {
	Reader // nil: any read beyond the scoped contract panics
	rows   map[string][]string
	calls  int
}

func (r *incomingSourceScopedBaseReader) FindIncomingSourcesScoped(_ context.Context, ids []string, _ EdgeKind, _ int, _ IncomingSourceScope, _ *IncomingSourceBudget) (BoundedIncomingSourceProjection, error) {
	r.calls++
	out := emptyIncomingSourceProjection()
	for _, id := range ids {
		if sources := r.rows[id]; len(sources) > 0 {
			out.Sources[id] = append([]string(nil), sources...)
		}
	}
	return out, nil
}

// incomingSourceLayerWithoutScopedReader is a covering layer that does NOT
// implement ScopedIncomingSourceReader. Every promoted method is nil, so any
// consultation of the layer past the refusal panics rather than silently
// returning a zero value.
type incomingSourceLayerWithoutScopedReader struct {
	OverlayLayerReader
}

// TestScopedIncomingSourcesRefuseLayerWithoutScopedReader pins the layer-side
// fail-closed guard in OverlaidView.FindIncomingSourcesScoped: a layer that
// cannot answer the scoped contract must refuse with
// ErrBoundedLocalizationUnavailable and a zero projection. Projecting the
// lower rows alone would publish base's answer as if the overlay had been
// applied to it — the exact fail-open the base-side guard already forbids.
func TestScopedIncomingSourcesRefuseLayerWithoutScopedReader(t *testing.T) {
	const targetID = "repo/target.go::target"
	base := &incomingSourceScopedBaseReader{rows: map[string][]string{
		targetID: {"repo/base.go::caller"},
	}}
	view := NewOverlaidViewWithLayer(base, &incomingSourceLayerWithoutScopedReader{})

	t.Run("scoped entrypoint", func(t *testing.T) {
		page, err := view.FindIncomingSourcesScoped(
			context.Background(), []string{targetID}, EdgeCalls, 8, IncomingSourceScope{}, nil)
		if !errors.Is(err, ErrBoundedLocalizationUnavailable) {
			t.Fatalf("layer without a scoped reader did not fail closed: err=%v page=%#v", err, page)
		}
		if page.Sources != nil || page.Truncated != nil {
			t.Fatalf("refusal returned a partial projection: %#v", page)
		}
	})

	t.Run("public bounded entrypoint reaches the same guard", func(t *testing.T) {
		// FindIncomingSourcesBounded delegates to the scoped path whenever a
		// layer is composed (bounded_incoming_sources.go:231), so production
		// callers inherit this refusal rather than only the scoped API.
		base.calls = 0
		page, err := view.FindIncomingSourcesBounded(
			context.Background(), []string{targetID}, EdgeCalls, 8)
		if !errors.Is(err, ErrBoundedLocalizationUnavailable) {
			t.Fatalf("public wrapper did not fail closed on a non-scoped layer: err=%v page=%#v", err, page)
		}
		if page.Sources != nil || page.Truncated != nil {
			t.Fatalf("public wrapper returned a partial projection: %#v", page)
		}
		// The base WAS read and its rows discarded: the refusal is the reason
		// nothing is published, not an absence of lower rows to publish.
		if base.calls != 1 {
			t.Fatalf("lower reader calls = %d, want 1 lower read whose rows %v are discarded",
				base.calls, base.rows[targetID])
		}
	})
}

// incomingSourceLayerWithoutNodeChecker answers the scoped contract and the
// ownership predicates, but is NOT an IncomingSourceNodeChecker: it cannot
// report checked node presence for the identities it covers. It carries no
// node for the covered target, so treating "cannot answer" as "visible"
// resurrects an identity the overlay dropped.
type incomingSourceLayerWithoutNodeChecker struct {
	OverlayLayerReader // nil: unexpected consultation panics
	coveredFile        string
}

func (l *incomingSourceLayerWithoutNodeChecker) HasFile(graphPath string) bool {
	return graphPath == l.coveredFile
}

func (l *incomingSourceLayerWithoutNodeChecker) CoversNodeID(id string) bool {
	if file := IDFile(id); file != "" {
		return l.HasFile(file)
	}
	return l.HasFile(id)
}

// The layer kept no node under any id and marked none removed.
func (l *incomingSourceLayerWithoutNodeChecker) OwnsNodeIdentity(string) bool { return false }

// The layer replaced no source's adjacency outright.
func (l *incomingSourceLayerWithoutNodeChecker) OwnsOutEdges(string) bool { return false }

func (l *incomingSourceLayerWithoutNodeChecker) FindIncomingSourcesScoped(_ context.Context, _ []string, _ EdgeKind, _ int, _ IncomingSourceScope, _ *IncomingSourceBudget) (BoundedIncomingSourceProjection, error) {
	return emptyIncomingSourceProjection(), nil
}

// TestScopedIncomingSourcesRefuseLayerWithoutNodeChecker pins the
// identityVisible fail-closed guard: when a covering layer cannot answer
// checked node presence the filter must refuse, never fail open. Failing open
// is worse than an error — it silently republishes a base target the overlay
// covers but carries no node for, i.e. a wrong localization answer with no
// signal that the overlay was never actually consulted.
func TestScopedIncomingSourcesRefuseLayerWithoutNodeChecker(t *testing.T) {
	const (
		coveredFile = "repo/edited.go"
		targetID    = coveredFile + "::target"
		sourceID    = "repo/caller.go::caller"
	)
	base := New()
	base.AddNode(&Node{ID: targetID, Name: "target", Kind: KindFunction, FilePath: coveredFile})
	base.AddNode(&Node{ID: sourceID, Name: "caller", Kind: KindFunction, FilePath: "repo/caller.go"})
	// The edge is RECORDED outside the covered file, so overlayOwnsBaseEdge
	// does not hide it: only the target's identity check stands between this
	// base row and the projection.
	base.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeCalls, FilePath: "repo/caller.go"})

	layer := &incomingSourceLayerWithoutNodeChecker{coveredFile: coveredFile}
	view := NewOverlaidViewWithLayer(base, layer)

	// Control: the same fixture with the in-memory layer — which IS an
	// IncomingSourceNodeChecker and holds no node for the covered target —
	// hides the row rather than erroring. That is the answer the refusal
	// protects, and it proves the fixture would otherwise publish the row.
	checking := NewOverlayLayer()
	checking.MarkFile(coveredFile, false)
	controlPage, err := NewOverlaidView(base, checking).FindIncomingSourcesBounded(
		context.Background(), []string{targetID}, EdgeCalls, 8)
	if err != nil {
		t.Fatalf("control with a checking layer refused: %v", err)
	}
	if len(controlPage.Sources[targetID]) != 0 || controlPage.Truncated[targetID] {
		t.Fatalf("control published a dropped identity: %#v", controlPage)
	}

	for _, tc := range []struct {
		name string
		call func() (BoundedIncomingSourceProjection, error)
	}{
		{"scoped entrypoint", func() (BoundedIncomingSourceProjection, error) {
			return view.FindIncomingSourcesScoped(
				context.Background(), []string{targetID}, EdgeCalls, 8, IncomingSourceScope{}, nil)
		}},
		{"public bounded entrypoint", func() (BoundedIncomingSourceProjection, error) {
			return view.FindIncomingSourcesBounded(
				context.Background(), []string{targetID}, EdgeCalls, 8)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := tc.call()
			if !errors.Is(err, ErrBoundedLocalizationUnavailable) {
				t.Fatalf("layer without a node checker did not fail closed: err=%v sources=%v",
					err, page.Sources[targetID])
			}
			if page.Sources != nil || page.Truncated != nil {
				t.Fatalf("refusal returned a partial projection: %#v", page)
			}
		})
	}

	// The same guard governs candidate rows, not just target identities: a
	// source whose file the layer covers is equally unanswerable.
	t.Run("candidate source identity", func(t *testing.T) {
		coveredSource := coveredFile + "::upper"
		flipped := New()
		flipped.AddNode(&Node{ID: "repo/target.go::plain", Name: "plain", Kind: KindFunction, FilePath: "repo/target.go"})
		flipped.AddNode(&Node{ID: coveredSource, Name: "upper", Kind: KindFunction, FilePath: coveredFile})
		flipped.AddEdge(&Edge{From: coveredSource, To: "repo/target.go::plain", Kind: EdgeCalls, FilePath: "repo/target.go"})
		page, err := NewOverlaidViewWithLayer(flipped, layer).FindIncomingSourcesBounded(
			context.Background(), []string{"repo/target.go::plain"}, EdgeCalls, 8)
		if !errors.Is(err, ErrBoundedLocalizationUnavailable) {
			t.Fatalf("covered candidate source did not fail closed: err=%v sources=%v",
				err, page.Sources["repo/target.go::plain"])
		}
	})
}

// TestIncomingSourceCandidateRowCapPinned pins the numeric anti-amplification
// cap. Every other test in this package derives its fixture size from the
// constant, so they pin the RELATIONSHIP and stay green for any value: only
// this assertion catches a raised cap, which is the regression that would
// destroy the bound this reader exists to enforce.
func TestIncomingSourceCandidateRowCapPinned(t *testing.T) {
	if MaxIncomingSourceCandidateRows != 16_384 {
		t.Fatalf("MaxIncomingSourceCandidateRows = %d, want 16384", MaxIncomingSourceCandidateRows)
	}
	if MaxIncomingSourceCandidateRows != MaxBoundedAdjacencyInspectedEdges {
		t.Fatalf("MaxIncomingSourceCandidateRows = %d, want the shared adjacency inspection cap %d",
			MaxIncomingSourceCandidateRows, MaxBoundedAdjacencyInspectedEdges)
	}

	// The shared budget spends exactly that many rows and refuses one more.
	var budget IncomingSourceBudget
	if err := budget.Charge(MaxIncomingSourceCandidateRows); err != nil {
		t.Fatalf("charging the whole cap refused: %v", err)
	}
	if remaining := budget.Remaining(); remaining != 0 {
		t.Fatalf("remaining after the whole cap = %d, want 0", remaining)
	}
	err := budget.Charge(1)
	var limitErr *BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("one row past the cap = %v, want *BoundedLocalizationLimitError", err)
	}
	if limitErr.Limit != MaxIncomingSourceCandidateRows {
		t.Fatalf("refusal reports limit %d, want %d", limitErr.Limit, MaxIncomingSourceCandidateRows)
	}
	if !reflect.DeepEqual(limitErr.Resource, "incoming-source candidate inspections") {
		t.Fatalf("refusal resource = %q", limitErr.Resource)
	}
}
