package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// halfIncomingSourceCandidateBudget splits the one shared physical-inspection
// budget exactly in two so a lower and an upper layer can each consume half.
const halfIncomingSourceCandidateBudget = MaxIncomingSourceCandidateRows / 2

// sharedBudgetFixture builds one target whose incoming candidates are split
// between a base Graph and an overlay layer. Every candidate is a distinct
// physical edge identity, and exactly one source survives both layers'
// ownership rules:
//
//   - lower rows are recorded IN the covered file, so overlayOwnsBaseEdge
//     supersedes them even though their sources live outside the overlay;
//   - upper rows are sources inside the covered file, and only the one the
//     layer re-emitted a node for is still visible.
func sharedBudgetFixture(t *testing.T, lowerRows, upperRows int) (*Graph, *OverlayLayer, string, string) {
	t.Helper()
	const (
		targetID    = "repo/target.go::target"
		coveredFile = "repo/edited.go"
	)
	base := New()
	base.AddNode(&Node{ID: targetID, Name: "target", Kind: KindFunction, FilePath: "repo/target.go"})
	for index := 0; index < lowerRows; index++ {
		sourceID := fmt.Sprintf("repo/lower.go::lower-%05d", index)
		base.AddNode(&Node{ID: sourceID, Name: "lower", Kind: KindFunction, FilePath: "repo/lower.go"})
		base.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeCalls, FilePath: coveredFile})
	}
	layer := NewOverlayLayer()
	layer.MarkFile(coveredFile, false)
	visibleID := ""
	for index := 0; index < upperRows; index++ {
		sourceID := fmt.Sprintf("%s::upper-%05d", coveredFile, index)
		if index == 0 {
			visibleID = sourceID
			// Only this identity is re-emitted; every other upper source is a
			// covered-file id the layer kept no node for, so it is invisible.
			layer.AddNode(coveredFile, &Node{ID: sourceID, Name: "upper", Kind: KindFunction, FilePath: coveredFile})
		}
		layer.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeCalls, FilePath: coveredFile})
	}
	return base, layer, targetID, visibleID
}

// TestScopedIncomingSourcesShareOneInspectionBudgetAcrossLayers pins the shared
// per-query candidate budget: the lower and the upper physical read charge the
// same IncomingSourceBudget, so half the cap on each side lands exactly on
// MaxIncomingSourceCandidateRows and one more row refuses with the typed limit
// error and no partial projection.
func TestScopedIncomingSourcesShareOneInspectionBudgetAcrossLayers(t *testing.T) {
	t.Run("exact shared budget succeeds", func(t *testing.T) {
		base, layer, targetID, visibleID := sharedBudgetFixture(t,
			halfIncomingSourceCandidateBudget, halfIncomingSourceCandidateBudget)
		recording := &recordingBoundedIncomingReader{Reader: base, bounded: base}
		page, err := NewOverlaidView(recording, layer).FindIncomingSourcesBounded(
			context.Background(), []string{targetID}, EdgeCalls, 8,
		)
		if err != nil {
			t.Fatalf("exactly %d shared inspections refused: %v", MaxIncomingSourceCandidateRows, err)
		}
		if page.Truncated[targetID] || !reflect.DeepEqual(page.Sources[targetID], []string{visibleID}) {
			t.Fatalf("distinct visible source = %#v, want only %q untruncated", page, visibleID)
		}
		// The scoped delegation path must be the one exercised: a legacy
		// FindIncomingSourcesBounded fallback carries no budget at all.
		if recording.calls != 1 || recording.scopedCalls != 1 || recording.legacyCalls != 0 ||
			!reflect.DeepEqual(recording.limits, []int{8}) ||
			!reflect.DeepEqual(recording.inspections, []int{halfIncomingSourceCandidateBudget}) {
			t.Fatalf("scoped=%d legacy=%d limits=%v inspections=%v; want one scoped limit-8 query inspecting %d lower rows",
				recording.scopedCalls, recording.legacyCalls, recording.limits, recording.inspections,
				halfIncomingSourceCandidateBudget)
		}
		if len(recording.budgets) != 1 || recording.budgets[0] == nil {
			t.Fatalf("lower reader received budgets %v; want the query's one shared budget", recording.budgets)
		}
		// The upper read charged the SAME object the lower read was handed.
		if remaining := recording.budgets[0].Remaining(); remaining != 0 {
			t.Fatalf("shared budget remaining = %d after %d lower + %d upper rows, want 0 (upper charged a private budget)",
				remaining, halfIncomingSourceCandidateBudget, halfIncomingSourceCandidateBudget)
		}
	})

	t.Run("one row past the shared budget refuses", func(t *testing.T) {
		base, layer, targetID, _ := sharedBudgetFixture(t,
			halfIncomingSourceCandidateBudget, halfIncomingSourceCandidateBudget+1)
		recording := &recordingBoundedIncomingReader{Reader: base, bounded: base}
		page, err := NewOverlaidView(recording, layer).FindIncomingSourcesBounded(
			context.Background(), []string{targetID}, EdgeCalls, 8,
		)
		var limitErr *BoundedLocalizationLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("row %d error = %v, want *BoundedLocalizationLimitError",
				MaxIncomingSourceCandidateRows+1, err)
		}
		if limitErr.Resource != "incoming-source candidate inspections" ||
			limitErr.Limit != MaxIncomingSourceCandidateRows {
			t.Fatalf("typed limit error = %+v, want the incoming-source candidate inspection cap %d",
				limitErr, MaxIncomingSourceCandidateRows)
		}
		if page.Sources != nil || page.Truncated != nil {
			t.Fatalf("refused query returned a partial projection: %#v", page)
		}
		if recording.scopedCalls != 1 || recording.legacyCalls != 0 {
			t.Fatalf("scoped=%d legacy=%d; want the refusal to come from the scoped path",
				recording.scopedCalls, recording.legacyCalls)
		}
	})

	t.Run("upper rows alone stay inside the budget", func(t *testing.T) {
		// Control for the refusal above: the same upper layer over an empty
		// base succeeds, so the refusal is the SHARED charge, not the upper
		// read exceeding the cap on its own.
		_, layer, targetID, visibleID := sharedBudgetFixture(t, 0, halfIncomingSourceCandidateBudget+1)
		empty := New()
		recording := &recordingBoundedIncomingReader{Reader: empty, bounded: empty}
		page, err := NewOverlaidView(recording, layer).FindIncomingSourcesBounded(
			context.Background(), []string{targetID}, EdgeCalls, 8,
		)
		if err != nil {
			t.Fatalf("%d upper rows alone refused: %v", halfIncomingSourceCandidateBudget+1, err)
		}
		if page.Truncated[targetID] || !reflect.DeepEqual(page.Sources[targetID], []string{visibleID}) {
			t.Fatalf("upper-only projection = %#v, want only %q", page, visibleID)
		}
		if len(recording.budgets) != 1 || recording.budgets[0] == nil {
			t.Fatalf("lower reader received budgets %v; want one shared budget", recording.budgets)
		}
		if remaining := recording.budgets[0].Remaining(); remaining != MaxIncomingSourceCandidateRows-(halfIncomingSourceCandidateBudget+1) {
			t.Fatalf("shared budget remaining = %d, want %d",
				remaining, MaxIncomingSourceCandidateRows-(halfIncomingSourceCandidateBudget+1))
		}
	})
}

// legacyOnlyIncomingSourceReader implements ONLY the pre-scope bounded reader.
// Its answer is deliberately truncated: a layer that filtered it after the fact
// would silently turn "more than limit sources exist" into a short exact list.
type legacyOnlyIncomingSourceReader struct {
	Reader
	boundedCalls int
	limits       []int
}

func (r *legacyOnlyIncomingSourceReader) FindIncomingSourcesBounded(
	_ context.Context,
	targetIDs []string,
	_ EdgeKind,
	limit int,
) (BoundedIncomingSourceProjection, error) {
	r.boundedCalls++
	r.limits = append(r.limits, limit)
	out := emptyIncomingSourceProjection()
	for _, id := range targetIDs {
		out.Sources[id] = []string{"legacy-visible", "legacy-tombstoned"}
		out.Truncated[id] = true
	}
	return out, nil
}

// TestBoundedIncomingSourcesLegacyOnlyReaderRefusesBehindOverlay pins the
// fail-closed rule for a lower reader that predates the scoped contract: with
// no overlay layer the view is a pass-through and may delegate once, but any
// real layer — including an empty one and one that only tombstones a source —
// must refuse with ErrBoundedLocalizationUnavailable instead of filtering an
// answer the lower reader already bounded.
func TestBoundedIncomingSourcesLegacyOnlyReaderRefusesBehindOverlay(t *testing.T) {
	const targetID = "repo/target.go::target"

	t.Run("no overlay delegates once verbatim", func(t *testing.T) {
		legacy := &legacyOnlyIncomingSourceReader{}
		page, err := NewOverlaidView(legacy, nil).FindIncomingSourcesBounded(
			context.Background(), []string{targetID}, EdgeCalls, 8,
		)
		if err != nil {
			t.Fatalf("pass-through legacy delegation refused: %v", err)
		}
		if legacy.boundedCalls != 1 || !reflect.DeepEqual(legacy.limits, []int{8}) {
			t.Fatalf("legacy bounded calls=%d limits=%v; want exactly one limit-8 delegation",
				legacy.boundedCalls, legacy.limits)
		}
		if !page.Truncated[targetID] ||
			!reflect.DeepEqual(page.Sources[targetID], []string{"legacy-visible", "legacy-tombstoned"}) {
			t.Fatalf("pass-through projection = %#v, want the lower answer verbatim", page)
		}
	})

	for _, test := range []struct {
		name  string
		layer func() *OverlayLayer
	}{
		{name: "empty overlay layer", layer: NewOverlayLayer},
		{
			name: "overlay that only tombstones a source",
			layer: func() *OverlayLayer {
				layer := NewOverlayLayer()
				layer.MarkRemoved("legacy", "legacy-tombstoned")
				return layer
			},
		},
		{
			name: "overlay that only tombstones a file",
			layer: func() *OverlayLayer {
				layer := NewOverlayLayer()
				layer.MarkFile("repo/deleted.go", true)
				return layer
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			legacy := &legacyOnlyIncomingSourceReader{}
			page, err := NewOverlaidView(legacy, test.layer()).FindIncomingSourcesBounded(
				context.Background(), []string{targetID}, EdgeCalls, 8,
			)
			if !errors.Is(err, ErrBoundedLocalizationUnavailable) {
				t.Fatalf("overlay over a legacy-only reader err = %v, want ErrBoundedLocalizationUnavailable", err)
			}
			if page.Sources != nil || page.Truncated != nil {
				t.Fatalf("refused query returned a projection: %#v", page)
			}
			// The refusal must precede the lower read: a bounded/truncated
			// lower answer can never be filtered without losing truncation.
			if legacy.boundedCalls != 0 {
				t.Fatalf("legacy bounded reader was called %d time(s) behind an overlay; want 0",
					legacy.boundedCalls)
			}
		})
	}

	t.Run("scoped path is what an overlay requires", func(t *testing.T) {
		// The same fixture with a scope-aware lower reader succeeds, so the
		// refusal above is the missing capability and not the overlay itself.
		base := New()
		base.AddEdge(&Edge{From: "repo/caller.go::caller", To: targetID, Kind: EdgeCalls, FilePath: "repo/caller.go"})
		page, err := NewOverlaidView(base, NewOverlayLayer()).FindIncomingSourcesBounded(
			context.Background(), []string{targetID}, EdgeCalls, 8,
		)
		if err != nil || page.Truncated[targetID] ||
			!reflect.DeepEqual(page.Sources[targetID], []string{"repo/caller.go::caller"}) {
			t.Fatalf("scope-aware lower reader behind the same overlay = %#v, %v", page, err)
		}
	})
}
