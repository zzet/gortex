package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func requireSingleBoundedIdentity(t *testing.T, identities []EdgeIdentity, want EdgeIdentity) {
	t.Helper()
	if !reflect.DeepEqual(identities, []EdgeIdentity{want}) {
		t.Fatalf("identities = %#v, want %#v", identities, []EdgeIdentity{want})
	}
}

func TestOverlaidViewBoundedAdjacencyReplacementAndTombstoneParity(t *testing.T) {
	const (
		sourceFile = "repo/source.go"
		targetFile = "repo/target.go"
		sourceID   = sourceFile + "::source"
		targetID   = targetFile + "::target"
		otherID    = "repo/other.go::caller"
	)
	baseEdge := &Edge{From: sourceID, To: targetID, Kind: EdgeCalls, FilePath: sourceFile, Line: 10}
	otherEdge := &Edge{From: otherID, To: targetID, Kind: EdgeCalls, FilePath: "repo/other.go", Line: 20}
	base := New()
	base.AddEdge(baseEdge)
	base.AddEdge(otherEdge)

	t.Run("source tombstone suppresses base and stray overlay edges", func(t *testing.T) {
		layer := NewOverlayLayer()
		layer.MarkFile(sourceFile, false)
		layer.MarkRemoved("source", sourceID)
		layer.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeCalls, Line: 99})
		view := NewOverlaidView(base, layer)
		out, err := view.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{sourceID}, []EdgeKind{EdgeCalls}, 1)
		if err != nil || len(out.ByEndpoint) != 0 || len(out.Truncated) != 0 {
			t.Fatalf("source tombstone outgoing = %#v, %v", out, err)
		}
		in, err := view.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{targetID}, []EdgeKind{EdgeCalls}, 1)
		if err != nil {
			t.Fatalf("source tombstone incoming: %v", err)
		}
		requireSingleBoundedIdentity(t, in.ByEndpoint[targetID], EdgeIdentityFor(otherEdge))
	})

	t.Run("same ID source replacement uses only overlay adjacency", func(t *testing.T) {
		layer := NewOverlayLayer()
		layer.MarkFile(sourceFile, false)
		layer.MarkRemoved("source", sourceID)
		layer.AddNode(sourceFile, &Node{ID: sourceID, Name: "source", Kind: KindMethod, FilePath: sourceFile})
		replacementEdge := &Edge{From: sourceID, To: targetID, Kind: EdgeCalls, FilePath: sourceFile, Line: 30}
		layer.AddEdge(replacementEdge)
		view := NewOverlaidView(base, layer)
		out, err := view.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{sourceID}, []EdgeKind{EdgeCalls}, 1)
		if err != nil {
			t.Fatalf("source replacement outgoing: %v", err)
		}
		requireSingleBoundedIdentity(t, out.ByEndpoint[sourceID], EdgeIdentityFor(replacementEdge))
		in, err := view.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{targetID}, []EdgeKind{EdgeCalls}, 2)
		if err != nil {
			t.Fatalf("source replacement incoming: %v", err)
		}
		want := []EdgeIdentity{EdgeIdentityFor(replacementEdge), EdgeIdentityFor(otherEdge)}
		sortEdgeIdentities(want)
		if !reflect.DeepEqual(in.ByEndpoint[targetID], want) {
			t.Fatalf("source replacement incoming = %#v, want %#v", in, want)
		}
	})

	t.Run("target tombstone and replacement", func(t *testing.T) {
		tombstone := NewOverlayLayer()
		tombstone.MarkFile(targetFile, false)
		tombstone.MarkRemoved("target", targetID)
		view := NewOverlaidView(base, tombstone)
		out, err := view.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{sourceID}, []EdgeKind{EdgeCalls}, 1)
		if err != nil || len(out.ByEndpoint) != 0 {
			t.Fatalf("target tombstone outgoing = %#v, %v", out, err)
		}
		in, err := view.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{targetID}, []EdgeKind{EdgeCalls}, 2)
		if err != nil || len(in.ByEndpoint) != 0 {
			t.Fatalf("target tombstone incoming = %#v, %v", in, err)
		}

		replacement := NewOverlayLayer()
		replacement.MarkFile(targetFile, false)
		replacement.MarkRemoved("target", targetID)
		replacement.AddNode(targetFile, &Node{ID: targetID, Name: "target", Kind: KindType, FilePath: targetFile})
		view = NewOverlaidView(base, replacement)
		out, err = view.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{sourceID}, []EdgeKind{EdgeCalls}, 1)
		if err != nil {
			t.Fatalf("target replacement outgoing: %v", err)
		}
		requireSingleBoundedIdentity(t, out.ByEndpoint[sourceID], EdgeIdentityFor(baseEdge))
		in, err = view.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{targetID}, []EdgeKind{EdgeCalls}, 2)
		if err != nil || len(in.ByEndpoint[targetID]) != 2 {
			t.Fatalf("target replacement incoming = %#v, %v", in, err)
		}
	})
}

func TestOverlaidViewBoundedIncomingRejectsStrayAndTombstonedOverlaySources(t *testing.T) {
	const target = "repo/target.go::target"
	for _, test := range []struct {
		name       string
		source     string
		configure  func(*OverlayLayer, string)
		wantSource bool
	}{
		{name: "untouched stray", source: "repo/untouched.go::source", configure: func(layer *OverlayLayer, source string) {}},
		{name: "standard tombstone", source: "repo/source.go::source", configure: func(layer *OverlayLayer, source string) {
			layer.MarkFile("repo/source.go", false)
			layer.MarkRemoved("source", source)
		}},
		{name: "standard current", source: "repo/source.go::source", configure: func(layer *OverlayLayer, source string) {
			layer.MarkFile("repo/source.go", false)
			layer.AddNode("repo/source.go", &Node{ID: source, Name: "source", Kind: KindMethod, FilePath: "repo/source.go"})
		}, wantSource: true},
		{name: "detached tombstone", source: "legacy-source", configure: func(layer *OverlayLayer, source string) {
			layer.MarkRemoved("legacy-source", source)
		}},
		{name: "detached current", source: "legacy-source", configure: func(layer *OverlayLayer, source string) {
			layer.MarkRemoved("legacy-source", source)
			layer.AddNode("overlay.go", &Node{ID: source, Name: "legacy-source", Kind: KindMethod, FilePath: "overlay.go"})
		}, wantSource: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			layer := NewOverlayLayer()
			test.configure(layer, test.source)
			edge := &Edge{From: test.source, To: target, Kind: EdgeCalls, FilePath: "overlay.go", Line: 5}
			layer.AddEdge(edge)
			projection, err := NewOverlaidView(New(), layer).FindIncomingEdgeIdentitiesBounded(
				context.Background(), []string{target}, []EdgeKind{EdgeCalls}, 1,
			)
			if err != nil {
				t.Fatalf("incoming projection: %v", err)
			}
			if test.wantSource {
				requireSingleBoundedIdentity(t, projection.ByEndpoint[target], EdgeIdentityFor(edge))
			} else if len(projection.ByEndpoint) != 0 || len(projection.Truncated) != 0 {
				t.Fatalf("stray/tombstoned source authenticated: %#v", projection)
			}
		})
	}
}

// TestOverlaidViewDetachedIdentityOutEdgeReadersAgree pins the ownership
// the plain out-edge reader applies to a detached identity: an ID whose
// file the layer does not cover but whose node it re-emitted. The layer
// speaks for such a node's whole outgoing edge set, so GetOutEdges serves
// the layer's adjacency — the same relation the bounded projection and
// the bulk read expose. A reader that asked only whether the source's
// file is covered would answer "no edges", because base's own edge out of
// the source is already hidden by the same ownership.
func TestOverlaidViewDetachedIdentityOutEdgeReadersAgree(t *testing.T) {
	const (
		source  = "legacy-source"
		target  = "repo/target.go::Target"
		overlay = "repo/overlay.go"
	)
	base := New()
	base.AddNode(&Node{ID: source, Name: "legacy-source", Kind: KindMethod, FilePath: "legacy.go", RepoPrefix: "repo"})
	base.AddNode(&Node{ID: target, Name: "Target", Kind: KindFunction, FilePath: "repo/target.go", RepoPrefix: "repo"})
	base.AddEdge(&Edge{From: source, To: target, Kind: EdgeCalls, FilePath: "legacy.go", Line: 1})

	layer := NewOverlayLayer()
	layer.AddNode(overlay, &Node{ID: source, Name: "legacy-source", Kind: KindMethod, FilePath: overlay, RepoPrefix: "repo"})
	replacement := &Edge{From: source, To: target, Kind: EdgeCalls, FilePath: overlay, Line: 2}
	layer.AddEdge(replacement)
	view := NewOverlaidView(base, layer)

	if got := view.GetOutEdges(source); len(got) != 1 || got[0] != replacement {
		t.Fatalf("GetOutEdges(%q) = %#v, want the layer's edge", source, got)
	}
	if got := view.AllEdges(); len(got) != 1 || got[0] != replacement {
		t.Fatalf("AllEdges = %#v, want only the layer's edge", got)
	}
	if got := view.GetOutEdgesByNodeIDs([]string{source})[source]; len(got) != 1 || got[0] != replacement {
		t.Fatalf("GetOutEdgesByNodeIDs[%q] = %#v, want the layer's edge", source, got)
	}
	projection, err := view.FindOutgoingEdgeIdentitiesBounded(
		context.Background(), []string{source}, []EdgeKind{EdgeCalls}, 4,
	)
	if err != nil {
		t.Fatalf("bounded outgoing projection: %v", err)
	}
	requireSingleBoundedIdentity(t, projection.ByEndpoint[source], EdgeIdentityFor(replacement))
}

func TestOverlaidViewBoundedAdjacencyDetachedReplacementAndTombstoneParity(t *testing.T) {
	const (
		source = "legacy-source"
		target = "legacy-target"
	)
	baseEdge := &Edge{From: source, To: target, Kind: EdgeCalls, FilePath: "legacy.go", Line: 1}
	base := New()
	base.AddEdge(baseEdge)

	sourceTombstone := NewOverlayLayer()
	sourceTombstone.MarkRemoved("legacy-source", source)
	sourceTombstone.AddEdge(&Edge{From: source, To: target, Kind: EdgeCalls, Line: 99})
	out, err := NewOverlaidView(base, sourceTombstone).FindOutgoingEdgeIdentitiesBounded(
		context.Background(), []string{source}, []EdgeKind{EdgeCalls}, 1,
	)
	if err != nil || len(out.ByEndpoint) != 0 || len(out.Truncated) != 0 {
		t.Fatalf("detached source tombstone = %#v, %v", out, err)
	}

	sourceReplacement := NewOverlayLayer()
	sourceReplacement.MarkRemoved("legacy-source", source)
	sourceReplacement.AddNode("overlay.go", &Node{ID: source, Name: "legacy-source", Kind: KindMethod, FilePath: "overlay.go"})
	replacementEdge := &Edge{From: source, To: target, Kind: EdgeCalls, FilePath: "overlay.go", Line: 2}
	sourceReplacement.AddEdge(replacementEdge)
	out, err = NewOverlaidView(base, sourceReplacement).FindOutgoingEdgeIdentitiesBounded(
		context.Background(), []string{source}, []EdgeKind{EdgeCalls}, 1,
	)
	if err != nil {
		t.Fatalf("detached source replacement: %v", err)
	}
	requireSingleBoundedIdentity(t, out.ByEndpoint[source], EdgeIdentityFor(replacementEdge))

	targetTombstone := NewOverlayLayer()
	targetTombstone.MarkRemoved("legacy-target", target)
	view := NewOverlaidView(base, targetTombstone)
	out, err = view.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{source}, []EdgeKind{EdgeCalls}, 1)
	if err != nil || len(out.ByEndpoint) != 0 {
		t.Fatalf("detached target tombstone outgoing = %#v, %v", out, err)
	}
	in, err := view.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{target}, []EdgeKind{EdgeCalls}, 1)
	if err != nil || len(in.ByEndpoint) != 0 || len(in.Truncated) != 0 {
		t.Fatalf("detached target tombstone incoming = %#v, %v", in, err)
	}

	targetReplacement := NewOverlayLayer()
	targetReplacement.MarkRemoved("legacy-target", target)
	targetReplacement.AddNode("overlay.go", &Node{ID: target, Name: "legacy-target", Kind: KindType, FilePath: "overlay.go"})
	view = NewOverlaidView(base, targetReplacement)
	out, err = view.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{source}, []EdgeKind{EdgeCalls}, 1)
	if err != nil {
		t.Fatalf("detached target replacement outgoing: %v", err)
	}
	requireSingleBoundedIdentity(t, out.ByEndpoint[source], EdgeIdentityFor(baseEdge))
	in, err = view.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{target}, []EdgeKind{EdgeCalls}, 1)
	if err != nil {
		t.Fatalf("detached target replacement incoming: %v", err)
	}
	requireSingleBoundedIdentity(t, in.ByEndpoint[target], EdgeIdentityFor(baseEdge))
}

func TestOverlaidViewBoundedSiteReplacementAndTombstoneParity(t *testing.T) {
	for _, test := range []struct {
		name       string
		source     string
		sourceFile string
		target     string
		targetFile string
	}{
		{name: "standard", source: "repo/source.go::source", sourceFile: "repo/source.go", target: "repo/target.go::target", targetFile: "repo/target.go"},
		{name: "detached", source: "legacy-source", target: "legacy-target"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A call site is recorded in the file that holds it, which is
			// how covering the source's file reaches base's row: ownership
			// is per edge, keyed on the path the edge was recorded at. The
			// detached source lives at no file, so its row stays where a
			// path claim cannot reach it and only the identity claim can.
			baseFile := test.sourceFile
			if baseFile == "" {
				baseFile = "base.go"
			}
			baseEdge := &Edge{From: test.source, To: test.target, Kind: EdgeCalls, FilePath: baseFile, Line: 10}
			base := New()
			base.AddEdge(baseEdge)
			site := EdgeSourceSite{From: test.source, Line: 10}
			markRemoved := func(layer *OverlayLayer, file, name, id string) {
				if file != "" {
					layer.MarkFile(file, false)
				}
				layer.MarkRemoved(name, id)
			}
			addCurrent := func(layer *OverlayLayer, file, name, id string, kind NodeKind) {
				if file == "" {
					file = "overlay.go"
				}
				layer.AddNode(file, &Node{ID: id, Name: name, Kind: kind, FilePath: file})
			}

			sourceTombstone := NewOverlayLayer()
			markRemoved(sourceTombstone, test.sourceFile, "source", test.source)
			sourceTombstone.AddEdge(&Edge{From: test.source, To: test.target, Kind: EdgeCalls, FilePath: "stray.go", Line: 10})
			projection, err := NewOverlaidView(base, sourceTombstone).FindOutgoingSiteEdgeIdentitiesBounded(
				context.Background(), []EdgeSourceSite{site}, []EdgeKind{EdgeCalls}, 1,
			)
			if err != nil || len(projection.BySite) != 0 || len(projection.Truncated) != 0 {
				t.Fatalf("source tombstone site = %#v, %v", projection, err)
			}

			sourceReplacement := NewOverlayLayer()
			markRemoved(sourceReplacement, test.sourceFile, "source", test.source)
			addCurrent(sourceReplacement, test.sourceFile, "source", test.source, KindMethod)
			replacementEdge := &Edge{From: test.source, To: test.target, Kind: EdgeCalls, FilePath: "overlay.go", Line: 10}
			sourceReplacement.AddEdge(replacementEdge)
			projection, err = NewOverlaidView(base, sourceReplacement).FindOutgoingSiteEdgeIdentitiesBounded(
				context.Background(), []EdgeSourceSite{site}, []EdgeKind{EdgeCalls}, 1,
			)
			if err != nil {
				t.Fatalf("source replacement site: %v", err)
			}
			requireSingleBoundedIdentity(t, projection.BySite[site], EdgeIdentityFor(replacementEdge))

			targetTombstone := NewOverlayLayer()
			markRemoved(targetTombstone, test.targetFile, "target", test.target)
			projection, err = NewOverlaidView(base, targetTombstone).FindOutgoingSiteEdgeIdentitiesBounded(
				context.Background(), []EdgeSourceSite{site}, []EdgeKind{EdgeCalls}, 1,
			)
			if err != nil || len(projection.BySite) != 0 || len(projection.Truncated) != 0 {
				t.Fatalf("target tombstone site = %#v, %v", projection, err)
			}

			targetReplacement := NewOverlayLayer()
			markRemoved(targetReplacement, test.targetFile, "target", test.target)
			addCurrent(targetReplacement, test.targetFile, "target", test.target, KindType)
			projection, err = NewOverlaidView(base, targetReplacement).FindOutgoingSiteEdgeIdentitiesBounded(
				context.Background(), []EdgeSourceSite{site}, []EdgeKind{EdgeCalls}, 1,
			)
			if err != nil {
				t.Fatalf("target replacement site: %v", err)
			}
			requireSingleBoundedIdentity(t, projection.BySite[site], EdgeIdentityFor(baseEdge))
		})
	}
}

func TestOverlaidViewBoundedAdjacencyCompensatesMultipleHiddenIdentities(t *testing.T) {
	for _, test := range []struct {
		name       string
		hiddenID   string
		hiddenFile string
	}{
		{name: "standard", hiddenID: "repo/hidden.go::hidden", hiddenFile: "repo/hidden.go"},
		{name: "detached", hiddenID: "legacy-hidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const source = "repo/source.go::source"
			visible := &Edge{From: source, To: "visible-target", Kind: EdgeCalls, FilePath: "visible.go", Line: 10}
			base := New()
			base.AddEdge(&Edge{From: source, To: test.hiddenID, Kind: EdgeCalls, FilePath: "hidden-a.go", Line: 10})
			base.AddEdge(&Edge{From: source, To: test.hiddenID, Kind: EdgeCalls, FilePath: "hidden-b.go", Line: 10})
			base.AddEdge(visible)
			layer := NewOverlayLayer()
			if test.hiddenFile != "" {
				layer.MarkFile(test.hiddenFile, false)
			}
			layer.MarkRemoved("hidden", test.hiddenID)
			view := NewOverlaidView(base, layer)
			out, err := view.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{source}, []EdgeKind{EdgeCalls}, 1)
			if err != nil || out.Truncated[source] {
				t.Fatalf("compensated outgoing = %#v, %v", out, err)
			}
			requireSingleBoundedIdentity(t, out.ByEndpoint[source], EdgeIdentityFor(visible))
			site := EdgeSourceSite{From: source, Line: 10}
			bySite, err := view.FindOutgoingSiteEdgeIdentitiesBounded(context.Background(), []EdgeSourceSite{site}, []EdgeKind{EdgeCalls}, 1)
			if err != nil || bySite.Truncated[site] {
				t.Fatalf("compensated site = %#v, %v", bySite, err)
			}
			requireSingleBoundedIdentity(t, bySite.BySite[site], EdgeIdentityFor(visible))

			const target = "repo/target.go::target"
			visibleSource := &Edge{From: "repo/visible.go::source", To: target, Kind: EdgeCalls, FilePath: "visible.go", Line: 20}
			incomingBase := New()
			incomingBase.AddEdge(&Edge{From: test.hiddenID, To: target, Kind: EdgeCalls, FilePath: "hidden-a.go", Line: 20})
			incomingBase.AddEdge(&Edge{From: test.hiddenID, To: target, Kind: EdgeCalls, FilePath: "hidden-b.go", Line: 21})
			incomingBase.AddEdge(visibleSource)
			incoming, err := NewOverlaidView(incomingBase, layer).FindIncomingEdgeIdentitiesBounded(
				context.Background(), []string{target}, []EdgeKind{EdgeCalls}, 1,
			)
			if err != nil || incoming.Truncated[target] {
				t.Fatalf("compensated incoming = %#v, %v", incoming, err)
			}
			requireSingleBoundedIdentity(t, incoming.ByEndpoint[target], EdgeIdentityFor(visibleSource))
		})
	}
}

type boundedAdjacencyUnsupportedReader struct{ Reader }

func TestOverlaidViewBoundedAdjacencyFailsClosedWithoutBaseCapability(t *testing.T) {
	base := boundedAdjacencyUnsupportedReader{Reader: New()}
	layer := NewOverlayLayer()
	const overlaySource = "overlay.go::source"
	layer.MarkFile("overlay.go", false)
	layer.AddNode("overlay.go", &Node{ID: overlaySource, Name: "source", Kind: KindMethod, FilePath: "overlay.go"})
	layer.AddEdge(&Edge{From: overlaySource, To: "target", Kind: EdgeCalls})
	projection, err := NewOverlaidView(base, layer).FindOutgoingEdgeIdentitiesBounded(
		context.Background(), []string{overlaySource, "durable-source"}, []EdgeKind{EdgeCalls}, 1,
	)
	if !errors.Is(err, ErrBoundedLocalizationUnavailable) || len(projection.ByEndpoint) != 0 || len(projection.Truncated) != 0 {
		t.Fatalf("unsupported base leaked partial overlay evidence: %#v, %v", projection, err)
	}
}

var errBoundedAdjacencyBase = errors.New("bounded adjacency base failure")

type boundedAdjacencyErrorReader struct{ Reader }

func (boundedAdjacencyErrorReader) FindOutgoingEdgeIdentitiesBounded(context.Context, []string, []EdgeKind, int) (BoundedEdgeIdentityProjection, error) {
	return BoundedEdgeIdentityProjection{ByEndpoint: map[string][]EdgeIdentity{"prefilled": {{From: "stale"}}}}, errBoundedAdjacencyBase
}

func (boundedAdjacencyErrorReader) FindIncomingEdgeIdentitiesBounded(context.Context, []string, []EdgeKind, int) (BoundedEdgeIdentityProjection, error) {
	return BoundedEdgeIdentityProjection{ByEndpoint: map[string][]EdgeIdentity{"prefilled": {{From: "stale"}}}}, errBoundedAdjacencyBase
}

func (boundedAdjacencyErrorReader) FindOutgoingSiteEdgeIdentitiesBounded(context.Context, []EdgeSourceSite, []EdgeKind, int) (BoundedSiteEdgeIdentityProjection, error) {
	return BoundedSiteEdgeIdentityProjection{BySite: map[EdgeSourceSite][]EdgeIdentity{{From: "stale", Line: 1}: {{From: "stale"}}}}, errBoundedAdjacencyBase
}

func TestOverlaidViewBoundedAdjacencyDiscardsBasePartialOnError(t *testing.T) {
	base := boundedAdjacencyErrorReader{Reader: New()}
	layer := NewOverlayLayer()
	const overlaySource = "overlay.go::source"
	layer.MarkFile("overlay.go", false)
	layer.AddNode("overlay.go", &Node{ID: overlaySource, Name: "source", Kind: KindMethod, FilePath: "overlay.go"})
	layer.AddEdge(&Edge{From: overlaySource, To: "target", Kind: EdgeCalls, Line: 1})
	view := NewOverlaidView(base, layer)

	out, err := view.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{overlaySource, "base-source"}, []EdgeKind{EdgeCalls}, 1)
	if !errors.Is(err, errBoundedAdjacencyBase) || len(out.ByEndpoint) != 0 || len(out.Truncated) != 0 {
		t.Fatalf("outgoing base error leaked partial: %#v, %v", out, err)
	}
	in, err := view.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{"target", "base-target"}, []EdgeKind{EdgeCalls}, 1)
	if !errors.Is(err, errBoundedAdjacencyBase) || len(in.ByEndpoint) != 0 || len(in.Truncated) != 0 {
		t.Fatalf("incoming base error leaked partial: %#v, %v", in, err)
	}
	site := EdgeSourceSite{From: overlaySource, Line: 1}
	bySite, err := view.FindOutgoingSiteEdgeIdentitiesBounded(context.Background(), []EdgeSourceSite{site, {From: "base-source", Line: 1}}, []EdgeKind{EdgeCalls}, 1)
	if !errors.Is(err, errBoundedAdjacencyBase) || len(bySite.BySite) != 0 || len(bySite.Truncated) != 0 {
		t.Fatalf("site base error leaked partial: %#v, %v", bySite, err)
	}
}

func TestOverlaidViewBoundedAdjacencyInspectionAndCancellationAreFailClosed(t *testing.T) {
	const source = "overlay.go::source"
	layer := NewOverlayLayer()
	layer.MarkFile("overlay.go", false)
	layer.AddNode("overlay.go", &Node{ID: source, Name: "source", Kind: KindMethod, FilePath: "overlay.go"})
	for index := 0; index < MaxBoundedAdjacencyInspectedEdges; index++ {
		layer.AddEdge(&Edge{From: source, To: fmt.Sprintf("noise-%05d", index), Kind: EdgeReferences, Line: index})
	}
	layer.AddEdge(&Edge{From: source, To: "wanted", Kind: EdgeCalls, Line: MaxBoundedAdjacencyInspectedEdges})
	view := NewOverlaidView(New(), layer)
	projection, err := view.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{source}, []EdgeKind{EdgeCalls}, 1)
	var limitErr *BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "adjacency inspected edge rows" ||
		len(projection.ByEndpoint) != 0 || len(projection.Truncated) != 0 {
		t.Fatalf("overlay inspection overflow = %#v, %v", projection, err)
	}

	ctx := &cancelAdjacencyAfterChecksContext{Context: context.Background(), remaining: 5}
	projection, err = view.FindOutgoingEdgeIdentitiesBounded(ctx, []string{source}, []EdgeKind{EdgeCalls}, 1)
	if !errors.Is(err, context.Canceled) || len(projection.ByEndpoint) != 0 || len(projection.Truncated) != 0 {
		t.Fatalf("canceled overlay projection leaked partial rows: %#v, %v", projection, err)
	}
}
