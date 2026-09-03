// Package overlaytest holds the conformance matrix for the overlay
// composition: the set of rules graph.OverlaidView applies to whatever
// layer it composes.
//
// The matrix lives in its own package so more than one implementation of
// graph.OverlayLayerReader can be held to it. An implementation that
// cannot import package graph's own test files — a persisted layer built
// on top of the storage package, for instance — supplies a builder and
// inherits every case here.
//
// Assertions compare node and edge payloads, never pointers: a layer
// that reads its content back from storage answers with fresh values, and
// the composition rules are about what a reader sees, not about which
// allocation it sees it through.
package overlaytest

import (
	"fmt"
	"iter"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const (
	consistencyRepo    = "repo"
	consistencyKeepFil = "repo/keep.go"
	consistencyEditFil = "repo/edit.go"
	consistencyKeepID  = consistencyKeepFil + "::Keeper"
	consistencyKeptID  = consistencyEditFil + "::Kept"
	consistencyGoneID  = consistencyEditFil + "::Gone"
)

// LayerBuilder stages one layer's content. Every fixture below speaks
// this vocabulary and nothing else, so the whole matrix runs against any
// implementation of the graph.OverlayLayerReader contract.
type LayerBuilder interface {
	MarkFile(graphPath string, deleted bool)
	AddNode(graphPath string, n *graph.Node)
	AddEdge(e *graph.Edge)
	MarkRemoved(baseName, baseID string)
	// Freeze ends construction and hands back the read side.
	Freeze() graph.OverlayLayerReader
}

// LayerFactory opens one empty builder per fixture.
type LayerFactory func() LayerBuilder

// inMemoryLayerBuilder stages content in *graph.OverlayLayer, the
// in-process layer the MCP overlay middleware builds per session.
type inMemoryLayerBuilder struct{ *graph.OverlayLayer }

func (b inMemoryLayerBuilder) Freeze() graph.OverlayLayerReader { return b.OverlayLayer }

// NewInMemoryLayerBuilder is the factory for the in-process layer.
func NewInMemoryLayerBuilder() LayerBuilder {
	return inMemoryLayerBuilder{graph.NewOverlayLayer()}
}

// consistencyFixture builds a base graph with one untouched file and
// one file the overlay covers, plus the layer that replaces one symbol
// in the covered file under the same ID and hides the other.
//
//	base: Keeper -> Kept      (unchanged source, covered target re-emitted)
//	      Keeper -> Gone      (unchanged source, covered target hidden)
//	      Kept   -> Keeper    (covered source: replaced by the layer's edge)
//	layer: Kept' -> Keeper    (the overlay's own version of the call)
func consistencyFixture(newLayer LayerFactory) (*graph.Graph, graph.OverlayLayerReader, *graph.Node) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: consistencyKeepID, Name: "Keeper", Kind: graph.KindFunction, FilePath: consistencyKeepFil, RepoPrefix: consistencyRepo})
	base.AddNode(&graph.Node{ID: consistencyKeptID, Name: "Kept", Kind: graph.KindFunction, FilePath: consistencyEditFil, RepoPrefix: consistencyRepo, StartLine: 10})
	base.AddNode(&graph.Node{ID: consistencyGoneID, Name: "Gone", Kind: graph.KindFunction, FilePath: consistencyEditFil, RepoPrefix: consistencyRepo})
	base.AddEdge(&graph.Edge{From: consistencyKeepID, To: consistencyKeptID, Kind: graph.EdgeCalls, FilePath: consistencyKeepFil, Line: 10})
	base.AddEdge(&graph.Edge{From: consistencyKeepID, To: consistencyGoneID, Kind: graph.EdgeCalls, FilePath: consistencyKeepFil, Line: 11})
	base.AddEdge(&graph.Edge{From: consistencyKeptID, To: consistencyKeepID, Kind: graph.EdgeCalls, FilePath: consistencyEditFil, Line: 20})

	replacement := &graph.Node{
		ID: consistencyKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: consistencyEditFil, RepoPrefix: consistencyRepo,
		StartLine: 40,
	}
	layer := newLayer()
	layer.MarkFile(consistencyEditFil, false)
	layer.AddNode(consistencyEditFil, replacement)
	layer.MarkRemoved("Gone", consistencyGoneID)
	layer.AddEdge(&graph.Edge{From: consistencyKeptID, To: consistencyKeepID, Kind: graph.EdgeCalls, FilePath: consistencyEditFil, Line: 21})
	return base, layer.Freeze(), replacement
}

func nodeIDs(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			out = append(out, n.ID)
		}
	}
	sort.Strings(out)
	return out
}

// overlayEdgeKey renders one edge's identity for set comparison across
// the point, batched and bulk readers.
func overlayEdgeKey(e *graph.Edge) string {
	return fmt.Sprintf("%s->%s|%s|%s:%d", e.From, e.To, e.Kind, e.FilePath, e.Line)
}

func edgeKeys(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			out = append(out, overlayEdgeKey(e))
		}
	}
	sort.Strings(out)
	return out
}

// overlayNodeKey renders one node's identity *and* payload, so a
// comparison catches a reader that returned base's copy where the layer
// re-emitted the ID. It is the equality every node assertion here uses:
// a layer that round-trips its content through storage answers with a
// different pointer carrying the same payload.
func overlayNodeKey(n *graph.Node) string {
	if n == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s:%d-%d",
		n.ID, n.Kind, n.Name, n.QualName, n.FilePath, n.StartLine, n.EndLine)
}

func nodeKeys(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			out = append(out, overlayNodeKey(n))
		}
	}
	sort.Strings(out)
	return out
}

// sameNode reports payload equality between a reader's answer and the
// node a fixture staged.
func sameNode(got, want *graph.Node) bool {
	return got != nil && want != nil && overlayNodeKey(got) == overlayNodeKey(want)
}

// Run executes the composition matrix against one layer implementation.
// Each case pins a rule OverlaidView applies to whatever layer it
// composes, so a new implementation inherits the whole matrix by
// supplying a builder.
func Run(t *testing.T, newLayer LayerFactory) {
	t.Helper()
	cases := []struct {
		name   string
		assert func(*testing.T, LayerFactory)
	}{
		{"re-emitted node surfaces once", assertReEmittedNodeSurfacesOnce},
		{"edge readers agree", assertEdgeReadersAgree},
		{"edge readers agree on a tombstoned file", assertEdgeReadersAgreeOnTombstonedFile},
		{"adding a node twice replaces it", assertAddNodeIsIdempotent},
		{"stats reflect the overlay", assertStatsReflectOverlay},
		{"kind scans match the bulk reads", assertKindScansMatchBulkReads},
		{"kind scans honour the overlay payload", assertKindScansHonourOverlayPayload},
		{"re-emitted file node surfaces once", assertReEmittedFileNodeSurfacesOnce},
		{"covered file node hidden without re-emission", assertCoveredFileNodeHiddenWithoutReEmission},
		{"file node edge visibility", assertFileNodeEdgeVisibility},
		{"edges recorded outside a covered file survive", assertEdgesRecordedElsewhereSurvive},
		{"uncovered tombstone hides the identity", assertUncoveredTombstoneHidesTheIdentity},
		{"kind scans stop early", assertKindScansStopEarly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.assert(t, newLayer) })
	}
}

// assertReEmittedNodeSurfacesOnce pins the duplicate-identity case: when
// the overlay re-emits a node under an ID base already has, every node
// reader must return exactly one copy — the layer's payload, not base's.
func assertReEmittedNodeSurfacesOnce(t *testing.T, newLayer LayerFactory) {
	base, layer, replacement := consistencyFixture(newLayer)
	view := graph.NewOverlaidViewWithLayer(base, layer)

	readers := map[string]func() []*graph.Node{
		"GetRepoNodes": func() []*graph.Node { return view.GetRepoNodes(consistencyRepo) },
		"AllNodes":     view.AllNodes,
		"GetFileNodes": func() []*graph.Node { return view.GetFileNodes(consistencyEditFil) },
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			var hits []*graph.Node
			for _, n := range read() {
				if n != nil && n.ID == consistencyKeptID {
					hits = append(hits, n)
				}
			}
			if len(hits) != 1 {
				t.Fatalf("%s returned %d copies of %s, want exactly 1", name, len(hits), consistencyKeptID)
			}
			if !sameNode(hits[0], replacement) {
				t.Fatalf("%s returned base's payload (line %d), want the layer's (line %d)",
					name, hits[0].StartLine, replacement.StartLine)
			}
		})
	}

	// The bulk readers must also agree with the point lookups over the
	// visible ID set: no hidden symbol resurfaces, nothing is dropped.
	wantVisible := []string{consistencyKeepID, consistencyKeptID}
	sort.Strings(wantVisible)
	if got := nodeIDs(view.GetRepoNodes(consistencyRepo)); !reflect.DeepEqual(got, wantVisible) {
		t.Fatalf("GetRepoNodes = %v, want %v", got, wantVisible)
	}
	if got := nodeIDs(view.AllNodes()); !reflect.DeepEqual(got, wantVisible) {
		t.Fatalf("AllNodes = %v, want %v", got, wantVisible)
	}
	for _, id := range wantVisible {
		if view.GetNode(id) == nil {
			t.Fatalf("GetNode(%q) = nil but the bulk readers list it", id)
		}
	}
	if view.GetNode(consistencyGoneID) != nil {
		t.Fatalf("GetNode(%q) resurfaced a symbol the overlay hid", consistencyGoneID)
	}
}

// assertEdgeReadersAgree pins the point / batched / bulk contract: all
// three expose the same visible-edge relation, including the
// covered-target-re-emitted and covered-target-hidden cases.
func assertEdgeReadersAgree(t *testing.T, newLayer LayerFactory) {
	base, layer, _ := consistencyFixture(newLayer)
	view := graph.NewOverlaidViewWithLayer(base, layer)
	visible := []string{consistencyKeepID, consistencyKeptID}
	hidden := consistencyGoneID

	t.Run("out edges by source", func(t *testing.T) {
		cases := []struct {
			name   string
			source string
			want   []string
		}{
			{
				name:   "unchanged source keeps the edge to a re-emitted target",
				source: consistencyKeepID,
				want: []string{overlayEdgeKey(&graph.Edge{From: consistencyKeepID, To: consistencyKeptID,
					Kind: graph.EdgeCalls, FilePath: consistencyKeepFil, Line: 10})},
			},
			{
				name:   "covered source uses only the layer's edges",
				source: consistencyKeptID,
				want: []string{overlayEdgeKey(&graph.Edge{From: consistencyKeptID, To: consistencyKeepID,
					Kind: graph.EdgeCalls, FilePath: consistencyEditFil, Line: 21})},
			},
			{
				name:   "hidden source has no edges at all",
				source: hidden,
				want:   []string{},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := edgeKeys(view.GetOutEdges(tc.source)); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("GetOutEdges(%q) = %v, want %v", tc.source, got, tc.want)
				}
			})
		}
	})

	t.Run("in edges by target", func(t *testing.T) {
		cases := []struct {
			name   string
			target string
			want   []string
		}{
			{
				name:   "base edge from a covered source is replaced by the layer's",
				target: consistencyKeepID,
				want: []string{overlayEdgeKey(&graph.Edge{From: consistencyKeptID, To: consistencyKeepID,
					Kind: graph.EdgeCalls, FilePath: consistencyEditFil, Line: 21})},
			},
			{
				name:   "re-emitted target keeps its base in-edge",
				target: consistencyKeptID,
				want: []string{overlayEdgeKey(&graph.Edge{From: consistencyKeepID, To: consistencyKeptID,
					Kind: graph.EdgeCalls, FilePath: consistencyKeepFil, Line: 10})},
			},
			{
				name:   "hidden target drops its base in-edge",
				target: hidden,
				want:   []string{},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := edgeKeys(view.GetInEdges(tc.target)); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("GetInEdges(%q) = %v, want %v", tc.target, got, tc.want)
				}
			})
		}
	})

	t.Run("batched matches the point loop", func(t *testing.T) {
		ids := append(append([]string{}, visible...), hidden)
		batchedOut := view.GetOutEdgesByNodeIDs(ids)
		batchedIn := view.GetInEdgesByNodeIDs(ids)
		for _, id := range ids {
			if got, want := edgeKeys(batchedOut[id]), edgeKeys(view.GetOutEdges(id)); !reflect.DeepEqual(got, want) {
				t.Fatalf("GetOutEdgesByNodeIDs[%q] = %v, GetOutEdges = %v", id, got, want)
			}
			if got, want := edgeKeys(batchedIn[id]), edgeKeys(view.GetInEdges(id)); !reflect.DeepEqual(got, want) {
				t.Fatalf("GetInEdgesByNodeIDs[%q] = %v, GetInEdges = %v", id, got, want)
			}
		}
	})

	t.Run("bulk matches the union of the point reads", func(t *testing.T) {
		var union []*graph.Edge
		for _, n := range view.AllNodes() {
			union = append(union, view.GetOutEdges(n.ID)...)
		}
		if got, want := edgeKeys(view.AllEdges()), edgeKeys(union); !reflect.DeepEqual(got, want) {
			t.Fatalf("AllEdges = %v, union of GetOutEdges over visible sources = %v", got, want)
		}
		// The in-edge side must cover the same relation.
		var inUnion []*graph.Edge
		for _, n := range view.AllNodes() {
			inUnion = append(inUnion, view.GetInEdges(n.ID)...)
		}
		if got, want := edgeKeys(inUnion), edgeKeys(view.AllEdges()); !reflect.DeepEqual(got, want) {
			t.Fatalf("union of GetInEdges = %v, AllEdges = %v", got, want)
		}
	})
}

// assertEdgeReadersAgreeOnTombstonedFile is the tombstone variant:
// nothing in the covered file survives, in either direction.
func assertEdgeReadersAgreeOnTombstonedFile(t *testing.T, newLayer LayerFactory) {
	base, _, _ := consistencyFixture(newLayer)
	builder := newLayer()
	builder.MarkFile(consistencyEditFil, true)
	builder.MarkRemoved("Kept", consistencyKeptID)
	builder.MarkRemoved("Gone", consistencyGoneID)
	view := graph.NewOverlaidViewWithLayer(base, builder.Freeze())

	if got := nodeIDs(view.AllNodes()); !reflect.DeepEqual(got, []string{consistencyKeepID}) {
		t.Fatalf("AllNodes over a tombstoned file = %v, want only %v", got, consistencyKeepID)
	}
	if got := edgeKeys(view.AllEdges()); len(got) != 0 {
		t.Fatalf("AllEdges over a tombstoned file = %v, want none", got)
	}
	for _, id := range []string{consistencyKeepID, consistencyKeptID, consistencyGoneID} {
		if got := view.GetOutEdges(id); len(got) != 0 {
			t.Fatalf("GetOutEdges(%q) = %v, want none", id, edgeKeys(got))
		}
		if got := view.GetInEdges(id); len(got) != 0 {
			t.Fatalf("GetInEdges(%q) = %v, want none", id, edgeKeys(got))
		}
	}
	batchedOut := view.GetOutEdgesByNodeIDs([]string{consistencyKeepID, consistencyKeptID})
	batchedIn := view.GetInEdgesByNodeIDs([]string{consistencyKeepID, consistencyKeptID})
	for _, id := range []string{consistencyKeepID, consistencyKeptID} {
		if len(batchedOut[id]) != 0 || len(batchedIn[id]) != 0 {
			t.Fatalf("batched adjacency for %q survived a tombstone: out=%v in=%v",
				id, edgeKeys(batchedOut[id]), edgeKeys(batchedIn[id]))
		}
	}
}

const consistencyStaleID = consistencyKeepFil + "::Stale"

// uncoveredRemovalFixture stages the node tombstone's own shape: the
// layer hides one base identity without claiming the file it lives in,
// so nothing but the removal marker is in the layer's footprint. Both of
// keep.go's symbols stay where they are and only one of them disappears.
//
//	base: Keeper -> Stale    (untouched source into the removed identity)
//	      Stale  -> Keeper   (the removed identity's own call)
func uncoveredRemovalFixture(newLayer LayerFactory) (*graph.Graph, graph.OverlayLayerReader) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: consistencyKeepID, Name: "Keeper", QualName: "repo.Keeper",
		Kind: graph.KindFunction, FilePath: consistencyKeepFil, RepoPrefix: consistencyRepo})
	base.AddNode(&graph.Node{ID: consistencyStaleID, Name: "Stale", QualName: "repo.Stale",
		Kind: graph.KindFunction, FilePath: consistencyKeepFil, RepoPrefix: consistencyRepo})
	base.AddEdge(&graph.Edge{From: consistencyKeepID, To: consistencyStaleID, Kind: graph.EdgeCalls,
		FilePath: consistencyKeepFil, Line: 3})
	base.AddEdge(&graph.Edge{From: consistencyStaleID, To: consistencyKeepID, Kind: graph.EdgeCalls,
		FilePath: consistencyKeepFil, Line: 9})

	layer := newLayer()
	layer.MarkRemoved("Stale", consistencyStaleID)
	return base, layer.Freeze()
}

// assertUncoveredTombstoneHidesTheIdentity pins the removal marker on its
// own: a layer that speaks for one identity and covers no file at all
// still hides it from every reader, and leaves the file's other symbol
// untouched. A reader that only asks whether the ID's file is covered
// answers this case from base and contradicts the readers that ask
// whether the layer speaks for the identity.
func assertUncoveredTombstoneHidesTheIdentity(t *testing.T, newLayer LayerFactory) {
	base, layer := uncoveredRemovalFixture(newLayer)
	view := graph.NewOverlaidViewWithLayer(base, layer)

	if got := view.GetNode(consistencyStaleID); got != nil {
		t.Fatalf("GetNode(%q) = %s, want nil — the layer removed the identity",
			consistencyStaleID, overlayNodeKey(got))
	}
	if got, ok := view.GetNodesByIDs([]string{consistencyStaleID})[consistencyStaleID]; ok {
		t.Fatalf("GetNodesByIDs resurfaced the removed %s", overlayNodeKey(got))
	}
	if got := view.GetNodeByQualName("repo.Stale"); got != nil {
		t.Fatalf("GetNodeByQualName = %s, want nil", overlayNodeKey(got))
	}
	if got := view.GetNodesByQualNames([]string{"repo.Stale", "repo.Keeper"}); len(got["repo.Stale"]) != 0 {
		t.Fatalf("GetNodesByQualNames[repo.Stale] = %v, want none", nodeKeys(got["repo.Stale"]))
	}
	if got := view.FindNodesByName("Stale"); len(got) != 0 {
		t.Fatalf("FindNodesByName(Stale) = %v, want none", nodeKeys(got))
	}
	if got := view.GetNode(consistencyKeepID); got == nil {
		t.Fatalf("GetNode(%q) dropped the untouched symbol", consistencyKeepID)
	}

	for name, nodes := range map[string][]*graph.Node{
		"AllNodes":     view.AllNodes(),
		"GetRepoNodes": view.GetRepoNodes(consistencyRepo),
		"GetFileNodes": view.GetFileNodes(consistencyKeepFil),
		"NodesByKind":  collectNodeSeq(view.NodesByKind(graph.KindFunction)),
	} {
		t.Run(name, func(t *testing.T) {
			if hits := copiesOf(nodes, consistencyStaleID); len(hits) != 0 {
				t.Fatalf("%s resurfaced the removed %s", name, consistencyStaleID)
			}
			if hits := copiesOf(nodes, consistencyKeepID); len(hits) != 1 {
				t.Fatalf("%s returned %d copies of the untouched %s, want 1",
					name, len(hits), consistencyKeepID)
			}
		})
	}

	// The counters price the removal the readers apply. Only the node
	// side is checked against RepoStats: adjacency lost outside a
	// covered file is documented as not charged per repo.
	if got, want := view.NodeCount(), len(view.AllNodes()); got != want {
		t.Fatalf("NodeCount = %d, AllNodes has %d", got, want)
	}
	stats := view.Stats()
	if stats.TotalNodes != view.NodeCount() || stats.TotalEdges != view.EdgeCount() {
		t.Fatalf("Stats = %d nodes / %d edges, want %d / %d",
			stats.TotalNodes, stats.TotalEdges, view.NodeCount(), view.EdgeCount())
	}
	if got := view.RepoStats()[consistencyRepo].TotalNodes; got != view.NodeCount() {
		t.Fatalf("RepoStats.TotalNodes = %d, NodeCount = %d", got, view.NodeCount())
	}

	// Both base edges touch the removed identity — one as source, one as
	// target — so the whole edge relation goes with it.
	if got := edgeKeys(view.AllEdges()); len(got) != 0 {
		t.Fatalf("AllEdges = %v, want none", got)
	}
	for _, id := range []string{consistencyKeepID, consistencyStaleID} {
		if got := view.GetOutEdges(id); len(got) != 0 {
			t.Fatalf("GetOutEdges(%q) = %v, want none", id, edgeKeys(got))
		}
		if got := view.GetInEdges(id); len(got) != 0 {
			t.Fatalf("GetInEdges(%q) = %v, want none", id, edgeKeys(got))
		}
	}
}

// assertAddNodeIsIdempotent pins the builder's promise: re-adding the
// same (path, ID) replaces the node everywhere instead of leaving a
// second entry in the per-file or per-name index.
func assertAddNodeIsIdempotent(t *testing.T, newLayer LayerFactory) {
	const path = "repo/edit.go"
	first := &graph.Node{ID: path + "::Fn", Name: "Fn", QualName: "repo.Fn", Kind: graph.KindFunction, FilePath: path, StartLine: 1}
	second := &graph.Node{ID: first.ID, Name: first.Name, QualName: first.QualName, Kind: graph.KindFunction, FilePath: path, StartLine: 7}

	builder := newLayer()
	builder.MarkFile(path, false)
	builder.AddNode(path, first)
	builder.AddNode(path, second)
	layer := builder.Freeze()

	fileNodes := layer.FileNodes(path)
	if len(fileNodes) != 1 || !sameNode(fileNodes[0], second) {
		t.Fatalf("file index = %v, want exactly the second node", nodeKeys(fileNodes))
	}
	byName := layer.NodesByName("Fn")
	if len(byName) != 1 || !sameNode(byName[0], second) {
		t.Fatalf("name index holds %v, want exactly the second node", nodeKeys(byName))
	}
	if got := layer.NodeByQualName("repo.Fn"); !sameNode(got, second) {
		t.Fatalf("qual index = %s, want the second node", overlayNodeKey(got))
	}
	if got := layer.NodeByID(first.ID); !sameNode(got, second) {
		t.Fatalf("id index = %s, want the second node", overlayNodeKey(got))
	}

	// A view over the idempotent layer still reports one node.
	view := graph.NewOverlaidViewWithLayer(graph.New(), layer)
	if got := nodeIDs(view.GetFileNodes(path)); len(got) != 1 {
		t.Fatalf("GetFileNodes = %v, want a single entry", got)
	}
	if got := view.FindNodesByName("Fn"); len(got) != 1 {
		t.Fatalf("FindNodesByName returned %d entries, want 1", len(got))
	}
}

// assertStatsReflectOverlay pins the counters: Stats and RepoStats
// report the overlay's totals, not base's, and agree with NodeCount /
// EdgeCount.
func assertStatsReflectOverlay(t *testing.T, newLayer LayerFactory) {
	base, layer, _ := consistencyFixture(newLayer)

	baseStats := base.Stats()
	if baseStats.TotalNodes != 3 || baseStats.TotalEdges != 3 {
		t.Fatalf("fixture base stats = %d nodes / %d edges, want 3 / 3", baseStats.TotalNodes, baseStats.TotalEdges)
	}

	view := graph.NewOverlaidViewWithLayer(base, layer)
	// The overlay hides one of the covered file's two symbols and
	// replaces the other, so one node and one edge go: base's
	// Keeper->Gone call loses its target and Kept's own call is
	// re-emitted by the layer.
	if got := view.NodeCount(); got != 2 {
		t.Fatalf("NodeCount = %d, want 2", got)
	}
	if got := view.EdgeCount(); got != 2 {
		t.Fatalf("EdgeCount = %d, want 2", got)
	}
	stats := view.Stats()
	if stats.TotalNodes != view.NodeCount() || stats.TotalEdges != view.EdgeCount() {
		t.Fatalf("Stats = %d nodes / %d edges, want %d / %d",
			stats.TotalNodes, stats.TotalEdges, view.NodeCount(), view.EdgeCount())
	}
	// The breakdowns stay base-derived by design.
	if !reflect.DeepEqual(stats.ByKind, baseStats.ByKind) {
		t.Fatalf("Stats.ByKind = %v, want base's %v", stats.ByKind, baseStats.ByKind)
	}

	repoStats := view.RepoStats()[consistencyRepo]
	if repoStats.TotalNodes != 2 || repoStats.TotalEdges != 2 {
		t.Fatalf("RepoStats = %d nodes / %d edges, want 2 / 2", repoStats.TotalNodes, repoStats.TotalEdges)
	}
	if base.RepoStats()[consistencyRepo].TotalNodes != 3 {
		t.Fatalf("the overlay adjustment leaked into base's own RepoStats")
	}

	t.Run("added symbols raise the totals", func(t *testing.T) {
		added := newLayer()
		added.MarkFile(consistencyEditFil, false)
		for _, n := range base.GetFileNodes(consistencyEditFil) {
			added.AddNode(consistencyEditFil, n)
		}
		fresh := &graph.Node{
			ID: consistencyEditFil + "::Fresh", Name: "Fresh", Kind: graph.KindFunction,
			FilePath: consistencyEditFil, RepoPrefix: consistencyRepo,
		}
		added.AddNode(consistencyEditFil, fresh)
		added.AddEdge(&graph.Edge{From: fresh.ID, To: consistencyKeepID, Kind: graph.EdgeCalls, FilePath: consistencyEditFil, Line: 30})
		addedView := graph.NewOverlaidViewWithLayer(base, added.Freeze())

		if got := addedView.NodeCount(); got != 4 {
			t.Fatalf("NodeCount after an overlay add = %d, want 4", got)
		}
		if got := addedView.Stats().TotalNodes; got != 4 {
			t.Fatalf("Stats.TotalNodes after an overlay add = %d, want 4", got)
		}
		repo := addedView.RepoStats()[consistencyRepo]
		if repo.TotalNodes != 4 {
			t.Fatalf("RepoStats.TotalNodes after an overlay add = %d, want 4", repo.TotalNodes)
		}
		// Base's own out-edge from the covered file is replaced by the
		// layer's single new call, so the repo nets one edge less.
		if repo.TotalEdges != addedView.EdgeCount() {
			t.Fatalf("RepoStats.TotalEdges = %d, EdgeCount = %d", repo.TotalEdges, addedView.EdgeCount())
		}
	})
}

const (
	churnKeepFil  = "repo/handler.go"
	churnEditFil  = "repo/morph.go"
	churnHandleID = churnKeepFil + "::Handler"
	churnConfigID = churnKeepFil + "::Config"
	churnMorphID  = churnEditFil + "::Morph"
	churnDropID   = churnEditFil + "::Dropped"
	churnFreshID  = churnEditFil + "::Fresh"
)

// kindChurnFixture is the kind-bounded readers' hard case: the overlay
// re-emits one covered symbol under the same ID but a *different* Kind,
// introduces a symbol of a third Kind, hides a fourth, and replaces the
// covered file's outgoing edges with edges of two kinds. A kind-bounded
// reader that trusted base's kind index alone would report the stale
// kind for the re-emitted ID.
func kindChurnFixture(newLayer LayerFactory) (*graph.Graph, graph.OverlayLayerReader) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: churnHandleID, Name: "Handler", Kind: graph.KindFunction, FilePath: churnKeepFil, RepoPrefix: consistencyRepo})
	base.AddNode(&graph.Node{ID: churnConfigID, Name: "Config", Kind: graph.KindType, FilePath: churnKeepFil, RepoPrefix: consistencyRepo})
	base.AddNode(&graph.Node{ID: churnMorphID, Name: "Morph", Kind: graph.KindFunction, FilePath: churnEditFil, RepoPrefix: consistencyRepo, StartLine: 5})
	base.AddNode(&graph.Node{ID: churnDropID, Name: "Dropped", Kind: graph.KindType, FilePath: churnEditFil, RepoPrefix: consistencyRepo})
	base.AddEdge(&graph.Edge{From: churnHandleID, To: churnMorphID, Kind: graph.EdgeCalls, FilePath: churnKeepFil, Line: 3})
	base.AddEdge(&graph.Edge{From: churnHandleID, To: churnDropID, Kind: graph.EdgeReferences, FilePath: churnKeepFil, Line: 4})
	base.AddEdge(&graph.Edge{From: churnMorphID, To: churnConfigID, Kind: graph.EdgeReferences, FilePath: churnEditFil, Line: 6})

	layer := newLayer()
	layer.MarkFile(churnEditFil, false)
	layer.AddNode(churnEditFil, &graph.Node{ID: churnMorphID, Name: "Morph", Kind: graph.KindMethod, FilePath: churnEditFil, RepoPrefix: consistencyRepo, StartLine: 50})
	layer.AddNode(churnEditFil, &graph.Node{ID: churnFreshID, Name: "Fresh", Kind: graph.KindType, FilePath: churnEditFil, RepoPrefix: consistencyRepo, StartLine: 60})
	layer.MarkRemoved("Dropped", churnDropID)
	layer.AddEdge(&graph.Edge{From: churnMorphID, To: churnHandleID, Kind: graph.EdgeCalls, FilePath: churnEditFil, Line: 51})
	layer.AddEdge(&graph.Edge{From: churnFreshID, To: churnConfigID, Kind: graph.EdgeReferences, FilePath: churnEditFil, Line: 61})
	return base, layer.Freeze()
}

func collectNodeSeq(seq iter.Seq[*graph.Node]) []*graph.Node {
	var out []*graph.Node
	for n := range seq {
		out = append(out, n)
	}
	return out
}

func collectEdgeSeq(seq iter.Seq[*graph.Edge]) []*graph.Edge {
	var out []*graph.Edge
	for e := range seq {
		out = append(out, e)
	}
	return out
}

func nodesOfKind(nodes []*graph.Node, kind graph.NodeKind) []*graph.Node {
	var out []*graph.Node
	for _, n := range nodes {
		if n != nil && n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func edgesOfKind(edges []*graph.Edge, kind graph.EdgeKind) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range edges {
		if e != nil && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// overlayKindFixtures are the (base, layer) pairs the kind-bounded
// assertions run over: an unlayered pass-through, a covered file whose
// symbols are partly re-emitted and partly hidden, a tombstoned file,
// and the kind-churn case.
func overlayKindFixtures(newLayer LayerFactory) map[string]func() (*graph.Graph, graph.OverlayLayerReader) {
	return map[string]func() (*graph.Graph, graph.OverlayLayerReader){
		"no layer": func() (*graph.Graph, graph.OverlayLayerReader) {
			base, _, _ := consistencyFixture(newLayer)
			return base, nil
		},
		"replaced file": func() (*graph.Graph, graph.OverlayLayerReader) {
			base, layer, _ := consistencyFixture(newLayer)
			return base, layer
		},
		"tombstoned file": func() (*graph.Graph, graph.OverlayLayerReader) {
			base, _, _ := consistencyFixture(newLayer)
			layer := newLayer()
			layer.MarkFile(consistencyEditFil, true)
			layer.MarkRemoved("Kept", consistencyKeptID)
			layer.MarkRemoved("Gone", consistencyGoneID)
			return base, layer.Freeze()
		},
		"kind churn": func() (*graph.Graph, graph.OverlayLayerReader) { return kindChurnFixture(newLayer) },
	}
}

// assertKindScansMatchBulkReads is the equivalence contract for the
// kind-bounded readers: NodesByKind(k) must be exactly AllNodes filtered
// to k, and EdgesByKind(k) exactly AllEdges filtered to k, for every
// kind either side carries — plus a kind nobody carries, which must come
// back empty rather than leaking base rows.
func assertKindScansMatchBulkReads(t *testing.T, newLayer LayerFactory) {
	for name, build := range overlayKindFixtures(newLayer) {
		t.Run(name, func(t *testing.T) {
			base, layer := build()
			view := graph.NewOverlaidViewWithLayer(base, layer)
			allNodes := view.AllNodes()
			allEdges := view.AllEdges()

			nodeKindSet := map[graph.NodeKind]bool{graph.KindImport: true}
			for _, n := range base.AllNodes() {
				nodeKindSet[n.Kind] = true
			}
			for _, n := range allNodes {
				nodeKindSet[n.Kind] = true
			}
			for kind := range nodeKindSet {
				got := nodeKeys(collectNodeSeq(view.NodesByKind(kind)))
				want := nodeKeys(nodesOfKind(allNodes, kind))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("NodesByKind(%q) = %v, AllNodes filtered = %v", kind, got, want)
				}
			}

			edgeKindSet := map[graph.EdgeKind]bool{graph.EdgeImports: true}
			for _, e := range base.AllEdges() {
				edgeKindSet[e.Kind] = true
			}
			for _, e := range allEdges {
				edgeKindSet[e.Kind] = true
			}
			for kind := range edgeKindSet {
				got := edgeKeys(collectEdgeSeq(view.EdgesByKind(kind)))
				want := edgeKeys(edgesOfKind(allEdges, kind))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("EdgesByKind(%q) = %v, AllEdges filtered = %v", kind, got, want)
				}
			}
		})
	}
}

// assertKindScansHonourOverlayPayload pins the two things the
// equivalence check can't state outright: a re-emitted ID comes back
// once carrying the layer's payload (including its new Kind), and a
// symbol the overlay hid never resurfaces through the kind index.
func assertKindScansHonourOverlayPayload(t *testing.T, newLayer LayerFactory) {
	base, layer := kindChurnFixture(newLayer)
	view := graph.NewOverlaidViewWithLayer(base, layer)

	// Base filed Morph under "function"; the layer re-emitted it as a
	// method. The stale kind must be gone and the fresh one present.
	wantFunctions := nodeKeys([]*graph.Node{base.GetNode(churnHandleID)})
	if got := nodeKeys(collectNodeSeq(view.NodesByKind(graph.KindFunction))); !reflect.DeepEqual(got, wantFunctions) {
		t.Fatalf("NodesByKind(function) = %v, want only the untouched Handler %v", got, wantFunctions)
	}
	methods := collectNodeSeq(view.NodesByKind(graph.KindMethod))
	if len(methods) != 1 || methods[0].ID != churnMorphID {
		t.Fatalf("NodesByKind(method) = %v, want exactly the re-emitted %s", nodeKeys(methods), churnMorphID)
	}
	if methods[0].StartLine != 50 {
		t.Fatalf("NodesByKind(method) returned base's payload (line %d), want the layer's (line 50)", methods[0].StartLine)
	}

	// Dropped was hidden: it must be absent from its own kind scan, and
	// base's reference into it must be absent from the edge scan.
	for _, n := range collectNodeSeq(view.NodesByKind(graph.KindType)) {
		if n.ID == churnDropID {
			t.Fatalf("NodesByKind(type) resurfaced the hidden %s", churnDropID)
		}
	}
	for _, e := range collectEdgeSeq(view.EdgesByKind(graph.EdgeReferences)) {
		if e.To == churnDropID {
			t.Fatalf("EdgesByKind(references) kept an edge into the hidden %s", churnDropID)
		}
		if e.From == churnMorphID {
			t.Fatalf("EdgesByKind(references) kept base's edge out of the re-emitted %s", churnMorphID)
		}
	}
	calls := edgeKeys(collectEdgeSeq(view.EdgesByKind(graph.EdgeCalls)))
	wantCalls := edgeKeys([]*graph.Edge{
		{From: churnHandleID, To: churnMorphID, Kind: graph.EdgeCalls, FilePath: churnKeepFil, Line: 3},
		{From: churnMorphID, To: churnHandleID, Kind: graph.EdgeCalls, FilePath: churnEditFil, Line: 51},
	})
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("EdgesByKind(calls) = %v, want %v", calls, wantCalls)
	}
}

const (
	fileNodeKeepName = "keep.go"
	fileNodeEditName = "edit.go"
)

// fileNodeCoverage selects what the layer does with the covered file's
// own KindFile node.
type fileNodeCoverage int

const (
	// fileNodeReEmitted is what an extractor pass produces: the overlay
	// parses the buffer and emits the file node under the same ID.
	fileNodeReEmitted fileNodeCoverage = iota
	// fileNodeDropped covers the file without re-emitting its node and
	// records base's ID in the removal index.
	fileNodeDropped
	// fileNodeDroppedUnmarked is the same without the removal marker, so
	// only the covered-path rule can hide base's file node. A layer
	// builder that truncates its read of base's node list produces this.
	fileNodeDroppedUnmarked
)

// fileNodeFixture builds a base graph that carries a KindFile node for
// each file next to the symbols in it, plus a layer covering the edit
// file. A file node's ID is its bare path — no "::" separator — so this
// is the case where an ownership check that only reads the part before
// "::" sees no file at all.
//
// Edges exercise a file node on both sides:
//
//	keep.go -imports->    edit.go   (uncovered file node into the covered one)
//	Keeper  -references-> edit.go   (uncovered symbol into the covered file node)
//	edit.go -defines->    Kept      (the covered file node's own out-edge)
//
// A re-emitted file node must answer every reader once with the layer's
// payload; a dropped one must disappear from base as well.
func fileNodeFixture(newLayer LayerFactory, coverage fileNodeCoverage) (*graph.Graph, graph.OverlayLayerReader, *graph.Node) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: consistencyKeepFil, Name: fileNodeKeepName, Kind: graph.KindFile,
		FilePath: consistencyKeepFil, RepoPrefix: consistencyRepo, StartLine: 1, EndLine: 10})
	base.AddNode(&graph.Node{ID: consistencyEditFil, Name: fileNodeEditName, Kind: graph.KindFile,
		FilePath: consistencyEditFil, RepoPrefix: consistencyRepo, StartLine: 1, EndLine: 20})
	base.AddNode(&graph.Node{ID: consistencyKeepID, Name: "Keeper", Kind: graph.KindFunction,
		FilePath: consistencyKeepFil, RepoPrefix: consistencyRepo})
	base.AddNode(&graph.Node{ID: consistencyKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: consistencyEditFil, RepoPrefix: consistencyRepo, StartLine: 10})
	base.AddEdge(&graph.Edge{From: consistencyKeepFil, To: consistencyEditFil, Kind: graph.EdgeImports,
		FilePath: consistencyKeepFil, Line: 1})
	base.AddEdge(&graph.Edge{From: consistencyKeepID, To: consistencyEditFil, Kind: graph.EdgeReferences,
		FilePath: consistencyKeepFil, Line: 5})
	base.AddEdge(&graph.Edge{From: consistencyEditFil, To: consistencyKeptID, Kind: graph.EdgeDefines,
		FilePath: consistencyEditFil, Line: 10})

	layer := newLayer()
	layer.MarkFile(consistencyEditFil, false)
	layer.AddNode(consistencyEditFil, &graph.Node{ID: consistencyKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: consistencyEditFil, RepoPrefix: consistencyRepo, StartLine: 40})
	if coverage != fileNodeReEmitted {
		if coverage == fileNodeDropped {
			layer.MarkRemoved(fileNodeEditName, consistencyEditFil)
		}
		return base, layer.Freeze(), nil
	}
	replacement := &graph.Node{ID: consistencyEditFil, Name: fileNodeEditName, Kind: graph.KindFile,
		FilePath: consistencyEditFil, RepoPrefix: consistencyRepo, StartLine: 1, EndLine: 99}
	layer.AddNode(consistencyEditFil, replacement)
	layer.AddEdge(&graph.Edge{From: consistencyEditFil, To: consistencyKeptID, Kind: graph.EdgeDefines,
		FilePath: consistencyEditFil, Line: 40})
	return base, layer.Freeze(), replacement
}

func copiesOf(nodes []*graph.Node, id string) []*graph.Node {
	var out []*graph.Node
	for _, n := range nodes {
		if n != nil && n.ID == id {
			out = append(out, n)
		}
	}
	return out
}

// assertReEmittedFileNodeSurfacesOnce pins the file-node half of the
// one-copy rule: a covered file whose KindFile node the overlay
// re-emitted answers every node reader once, with the layer's payload.
func assertReEmittedFileNodeSurfacesOnce(t *testing.T, newLayer LayerFactory) {
	base, layer, replacement := fileNodeFixture(newLayer, fileNodeReEmitted)
	view := graph.NewOverlaidViewWithLayer(base, layer)

	if got := view.GetNode(consistencyEditFil); !sameNode(got, replacement) {
		t.Fatalf("GetNode(%q) = %s, want the layer's file node (end line %d)",
			consistencyEditFil, overlayNodeKey(got), replacement.EndLine)
	}
	if got := view.GetNodesByIDs([]string{consistencyEditFil})[consistencyEditFil]; !sameNode(got, replacement) {
		t.Fatalf("GetNodesByIDs[%q] = %s, want the layer's file node", consistencyEditFil, overlayNodeKey(got))
	}

	readers := map[string][]*graph.Node{
		"AllNodes":     view.AllNodes(),
		"GetRepoNodes": view.GetRepoNodes(consistencyRepo),
		"GetFileNodes": view.GetFileNodes(consistencyEditFil),
		"NodesByKind":  collectNodeSeq(view.NodesByKind(graph.KindFile)),
	}
	for name, nodes := range readers {
		t.Run(name, func(t *testing.T) {
			hits := copiesOf(nodes, consistencyEditFil)
			if len(hits) != 1 {
				t.Fatalf("%s returned %d copies of the file node %s, want exactly 1",
					name, len(hits), consistencyEditFil)
			}
			if !sameNode(hits[0], replacement) {
				t.Fatalf("%s returned base's file node (end line %d), want the layer's (end line %d)",
					name, hits[0].EndLine, replacement.EndLine)
			}
		})
	}

	// The kind scan and the bulk read must describe the same file set.
	if got, want := nodeKeys(collectNodeSeq(view.NodesByKind(graph.KindFile))),
		nodeKeys(nodesOfKind(view.AllNodes(), graph.KindFile)); !reflect.DeepEqual(got, want) {
		t.Fatalf("NodesByKind(file) = %v, AllNodes filtered to file = %v", got, want)
	}
	// The untouched file keeps base's single copy.
	if hits := copiesOf(view.AllNodes(), consistencyKeepFil); len(hits) != 1 {
		t.Fatalf("AllNodes returned %d copies of the untouched file node %s, want 1",
			len(hits), consistencyKeepFil)
	}
	// Name lookup answers with the layer's node, not base's.
	byName := view.FindNodesByName(fileNodeEditName)
	if len(byName) != 1 || !sameNode(byName[0], replacement) {
		t.Fatalf("FindNodesByName(%q) = %v, want exactly the layer's file node",
			fileNodeEditName, nodeKeys(byName))
	}
}

// assertCoveredFileNodeHiddenWithoutReEmission pins the other direction:
// when the overlay covers a file but its node list carries no KindFile
// node, base's copy must not survive — the same rule every dropped
// symbol follows.
func assertCoveredFileNodeHiddenWithoutReEmission(t *testing.T, newLayer LayerFactory) {
	for name, build := range map[string]func() (*graph.Graph, graph.OverlayLayerReader){
		"covered without re-emission": func() (*graph.Graph, graph.OverlayLayerReader) {
			base, layer, _ := fileNodeFixture(newLayer, fileNodeDropped)
			return base, layer
		},
		"covered without a removal marker": func() (*graph.Graph, graph.OverlayLayerReader) {
			base, layer, _ := fileNodeFixture(newLayer, fileNodeDroppedUnmarked)
			return base, layer
		},
		"tombstoned file": func() (*graph.Graph, graph.OverlayLayerReader) {
			base, _, _ := fileNodeFixture(newLayer, fileNodeDroppedUnmarked)
			layer := newLayer()
			layer.MarkFile(consistencyEditFil, true)
			layer.MarkRemoved(fileNodeEditName, consistencyEditFil)
			layer.MarkRemoved("Kept", consistencyKeptID)
			return base, layer.Freeze()
		},
	} {
		t.Run(name, func(t *testing.T) {
			base, layer := build()
			view := graph.NewOverlaidViewWithLayer(base, layer)

			if got := view.GetNode(consistencyEditFil); got != nil {
				t.Fatalf("GetNode(%q) = %s, want nil — the overlay covers the file and kept no node for it",
					consistencyEditFil, overlayNodeKey(got))
			}
			if got, ok := view.GetNodesByIDs([]string{consistencyEditFil})[consistencyEditFil]; ok {
				t.Fatalf("GetNodesByIDs resurfaced the hidden file node %s", overlayNodeKey(got))
			}
			readers := map[string][]*graph.Node{
				"AllNodes":     view.AllNodes(),
				"GetRepoNodes": view.GetRepoNodes(consistencyRepo),
				"GetFileNodes": view.GetFileNodes(consistencyEditFil),
				"NodesByKind":  collectNodeSeq(view.NodesByKind(graph.KindFile)),
			}
			for reader, nodes := range readers {
				if hits := copiesOf(nodes, consistencyEditFil); len(hits) != 0 {
					t.Fatalf("%s resurfaced the hidden file node %s", reader, consistencyEditFil)
				}
			}
			if got := view.FindNodesByName(fileNodeEditName); len(got) != 0 {
				t.Fatalf("FindNodesByName(%q) = %v, want none", fileNodeEditName, nodeKeys(got))
			}
			// The untouched file node is unaffected.
			if view.GetNode(consistencyKeepFil) == nil {
				t.Fatalf("GetNode(%q) dropped the untouched file node", consistencyKeepFil)
			}
		})
	}
}

// assertFileNodeEdgeVisibility pins the edge side: a file node is an
// ordinary endpoint, so edges into and out of it follow the same
// ownership rules a symbol endpoint follows, across the point, batched
// and bulk readers.
func assertFileNodeEdgeVisibility(t *testing.T, newLayer LayerFactory) {
	layerDefines := overlayEdgeKey(&graph.Edge{From: consistencyEditFil, To: consistencyKeptID,
		Kind: graph.EdgeDefines, FilePath: consistencyEditFil, Line: 40})
	baseImports := overlayEdgeKey(&graph.Edge{From: consistencyKeepFil, To: consistencyEditFil,
		Kind: graph.EdgeImports, FilePath: consistencyKeepFil, Line: 1})
	baseReferences := overlayEdgeKey(&graph.Edge{From: consistencyKeepID, To: consistencyEditFil,
		Kind: graph.EdgeReferences, FilePath: consistencyKeepFil, Line: 5})

	cases := []struct {
		name     string
		coverage fileNodeCoverage
		wantOut  []string // out-edges of the covered file node
		wantIn   []string // in-edges of the covered file node
		wantKeep []string // out-edges of the untouched file node
	}{
		{
			name:     "re-emitted file node keeps its inbound edges and swaps its own",
			coverage: fileNodeReEmitted,
			wantOut:  []string{layerDefines},
			wantIn:   []string{baseImports, baseReferences},
			wantKeep: []string{baseImports},
		},
		{
			name:     "hidden file node drops every edge in both directions",
			coverage: fileNodeDropped,
			wantOut:  []string{},
			wantIn:   []string{},
			wantKeep: []string{},
		},
		{
			name:     "coverage alone hides the edges without a removal marker",
			coverage: fileNodeDroppedUnmarked,
			wantOut:  []string{},
			wantIn:   []string{},
			wantKeep: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, layer, _ := fileNodeFixture(newLayer, tc.coverage)
			view := graph.NewOverlaidViewWithLayer(base, layer)

			sort.Strings(tc.wantIn)
			if got := edgeKeys(view.GetOutEdges(consistencyEditFil)); !reflect.DeepEqual(got, tc.wantOut) {
				t.Fatalf("GetOutEdges(%q) = %v, want %v", consistencyEditFil, got, tc.wantOut)
			}
			if got := edgeKeys(view.GetInEdges(consistencyEditFil)); !reflect.DeepEqual(got, tc.wantIn) {
				t.Fatalf("GetInEdges(%q) = %v, want %v", consistencyEditFil, got, tc.wantIn)
			}
			if got := edgeKeys(view.GetOutEdges(consistencyKeepFil)); !reflect.DeepEqual(got, tc.wantKeep) {
				t.Fatalf("GetOutEdges(%q) = %v, want %v", consistencyKeepFil, got, tc.wantKeep)
			}
			// A symbol's edge into the file node follows the same rule.
			wantSymbolOut := []string{}
			if tc.coverage == fileNodeReEmitted {
				wantSymbolOut = []string{baseReferences}
			}
			if got := edgeKeys(view.GetOutEdges(consistencyKeepID)); !reflect.DeepEqual(got, wantSymbolOut) {
				t.Fatalf("GetOutEdges(%q) = %v, want %v", consistencyKeepID, got, wantSymbolOut)
			}

			ids := []string{consistencyKeepFil, consistencyEditFil, consistencyKeepID, consistencyKeptID}
			batchedOut := view.GetOutEdgesByNodeIDs(ids)
			batchedIn := view.GetInEdgesByNodeIDs(ids)
			for _, id := range ids {
				if got, want := edgeKeys(batchedOut[id]), edgeKeys(view.GetOutEdges(id)); !reflect.DeepEqual(got, want) {
					t.Fatalf("GetOutEdgesByNodeIDs[%q] = %v, GetOutEdges = %v", id, got, want)
				}
				if got, want := edgeKeys(batchedIn[id]), edgeKeys(view.GetInEdges(id)); !reflect.DeepEqual(got, want) {
					t.Fatalf("GetInEdgesByNodeIDs[%q] = %v, GetInEdges = %v", id, got, want)
				}
			}

			var union []*graph.Edge
			for _, n := range view.AllNodes() {
				union = append(union, view.GetOutEdges(n.ID)...)
			}
			if got, want := edgeKeys(view.AllEdges()), edgeKeys(union); !reflect.DeepEqual(got, want) {
				t.Fatalf("AllEdges = %v, union of GetOutEdges over visible sources = %v", got, want)
			}
			for kind := range map[graph.EdgeKind]bool{graph.EdgeImports: true, graph.EdgeReferences: true, graph.EdgeDefines: true} {
				got := edgeKeys(collectEdgeSeq(view.EdgesByKind(kind)))
				want := edgeKeys(edgesOfKind(view.AllEdges(), kind))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("EdgesByKind(%q) = %v, AllEdges filtered = %v", kind, got, want)
				}
			}
		})
	}
}

// elsewhereFixture stages the case where one source's outgoing edges are
// recorded in two files, only one of which the layer covers.
//
//	base: Kept -calls->      Keeper   recorded in edit.go (covered)
//	      Kept -value_flow-> Keeper   recorded in keep.go (untouched)
//	      Keeper -calls->    Kept     recorded in keep.go (untouched)
//	layer: Kept' -calls->    Keeper   recorded in edit.go
//
// The value flow is the row a source-granular ownership rule loses: it
// leaves a covered file's symbol, so a layer that read "I cover edit.go"
// as "I speak for everything Kept points at" would hide it — while the
// layer carries no copy, because re-extracting edit.go cannot produce an
// edge keep.go holds.
//
// reEmit false drops Kept from the covered file instead of replacing it,
// which is the case where the row must go after all: its source is gone.
func elsewhereFixture(newLayer LayerFactory, reEmit bool) (*graph.Graph, graph.OverlayLayerReader) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: consistencyKeepID, Name: "Keeper", Kind: graph.KindFunction,
		FilePath: consistencyKeepFil, RepoPrefix: consistencyRepo})
	base.AddNode(&graph.Node{ID: consistencyKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: consistencyEditFil, RepoPrefix: consistencyRepo, StartLine: 10})
	base.AddEdge(&graph.Edge{From: consistencyKeptID, To: consistencyKeepID, Kind: graph.EdgeCalls,
		FilePath: consistencyEditFil, Line: 20})
	base.AddEdge(&graph.Edge{From: consistencyKeptID, To: consistencyKeepID, Kind: graph.EdgeValueFlow,
		FilePath: consistencyKeepFil, Line: 4})
	base.AddEdge(&graph.Edge{From: consistencyKeepID, To: consistencyKeptID, Kind: graph.EdgeCalls,
		FilePath: consistencyKeepFil, Line: 4})

	layer := newLayer()
	layer.MarkFile(consistencyEditFil, false)
	if !reEmit {
		layer.MarkRemoved("Kept", consistencyKeptID)
		return base, layer.Freeze()
	}
	layer.AddNode(consistencyEditFil, &graph.Node{ID: consistencyKeptID, Name: "Kept",
		Kind: graph.KindFunction, FilePath: consistencyEditFil, RepoPrefix: consistencyRepo, StartLine: 40})
	layer.AddEdge(&graph.Edge{From: consistencyKeptID, To: consistencyKeepID, Kind: graph.EdgeCalls,
		FilePath: consistencyEditFil, Line: 41})
	return base, layer.Freeze()
}

// assertEdgesRecordedElsewhereSurvive pins whose edge a covered file
// replaces: the edges RECORDED in it, not every edge leaving its
// symbols. A base row some other file holds stays visible, because the
// layer never re-derived the file that produced it and carries nothing
// to put in its place — and it goes only when the row's own source or
// target stops being visible.
func assertEdgesRecordedElsewhereSurvive(t *testing.T, newLayer LayerFactory) {
	elsewhere := overlayEdgeKey(&graph.Edge{From: consistencyKeptID, To: consistencyKeepID,
		Kind: graph.EdgeValueFlow, FilePath: consistencyKeepFil, Line: 4})
	inbound := overlayEdgeKey(&graph.Edge{From: consistencyKeepID, To: consistencyKeptID,
		Kind: graph.EdgeCalls, FilePath: consistencyKeepFil, Line: 4})
	replaced := overlayEdgeKey(&graph.Edge{From: consistencyKeptID, To: consistencyKeepID,
		Kind: graph.EdgeCalls, FilePath: consistencyEditFil, Line: 41})

	cases := []struct {
		name        string
		reEmit      bool
		wantKeptOut []string
		wantKeepIn  []string
	}{
		{
			name:        "re-emitted source keeps the row its caller's file holds",
			reEmit:      true,
			wantKeptOut: []string{elsewhere, replaced},
			wantKeepIn:  []string{elsewhere, replaced},
		},
		{
			name:        "a dropped source loses it with everything else",
			reEmit:      false,
			wantKeptOut: []string{},
			wantKeepIn:  []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, layer := elsewhereFixture(newLayer, tc.reEmit)
			view := graph.NewOverlaidViewWithLayer(base, layer)

			sort.Strings(tc.wantKeptOut)
			sort.Strings(tc.wantKeepIn)
			if got := edgeKeys(view.GetOutEdges(consistencyKeptID)); !reflect.DeepEqual(got, tc.wantKeptOut) {
				t.Fatalf("GetOutEdges(%q) = %v, want %v", consistencyKeptID, got, tc.wantKeptOut)
			}
			if got := edgeKeys(view.GetInEdges(consistencyKeepID)); !reflect.DeepEqual(got, tc.wantKeepIn) {
				t.Fatalf("GetInEdges(%q) = %v, want %v", consistencyKeepID, got, tc.wantKeepIn)
			}
			// The untouched file's own call into the covered symbol
			// follows the ordinary target rule.
			wantKeepOut := []string{}
			if tc.reEmit {
				wantKeepOut = []string{inbound}
			}
			if got := edgeKeys(view.GetOutEdges(consistencyKeepID)); !reflect.DeepEqual(got, wantKeepOut) {
				t.Fatalf("GetOutEdges(%q) = %v, want %v", consistencyKeepID, got, wantKeepOut)
			}

			ids := []string{consistencyKeepID, consistencyKeptID}
			batchedOut := view.GetOutEdgesByNodeIDs(ids)
			batchedIn := view.GetInEdgesByNodeIDs(ids)
			for _, id := range ids {
				if got, want := edgeKeys(batchedOut[id]), edgeKeys(view.GetOutEdges(id)); !reflect.DeepEqual(got, want) {
					t.Fatalf("GetOutEdgesByNodeIDs[%q] = %v, GetOutEdges = %v", id, got, want)
				}
				if got, want := edgeKeys(batchedIn[id]), edgeKeys(view.GetInEdges(id)); !reflect.DeepEqual(got, want) {
					t.Fatalf("GetInEdgesByNodeIDs[%q] = %v, GetInEdges = %v", id, got, want)
				}
			}

			var union []*graph.Edge
			for _, n := range view.AllNodes() {
				union = append(union, view.GetOutEdges(n.ID)...)
			}
			if got, want := edgeKeys(view.AllEdges()), edgeKeys(union); !reflect.DeepEqual(got, want) {
				t.Fatalf("AllEdges = %v, union of GetOutEdges over visible sources = %v", got, want)
			}
			if got, want := view.EdgeCount(), len(view.AllEdges()); got != want {
				t.Fatalf("EdgeCount = %d, AllEdges has %d", got, want)
			}
			for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeValueFlow} {
				got := edgeKeys(collectEdgeSeq(view.EdgesByKind(kind)))
				want := edgeKeys(edgesOfKind(view.AllEdges(), kind))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("EdgesByKind(%q) = %v, AllEdges filtered = %v", kind, got, want)
				}
			}
		})
	}
}

// assertKindScansStopEarly pins the iterator contract the Reader
// doc-comment promises: a consumer that breaks out of the range stops
// the scan instead of draining both legs.
func assertKindScansStopEarly(t *testing.T, newLayer LayerFactory) {
	base, layer := kindChurnFixture(newLayer)
	view := graph.NewOverlaidViewWithLayer(base, layer)

	seen := 0
	for range view.NodesByKind(graph.KindType) {
		seen++
		break
	}
	if seen != 1 {
		t.Fatalf("NodesByKind yielded %d nodes after an early break, want 1", seen)
	}
	seen = 0
	for range view.EdgesByKind(graph.EdgeCalls) {
		seen++
		break
	}
	if seen != 1 {
		t.Fatalf("EdgesByKind yielded %d edges after an early break, want 1", seen)
	}
}
