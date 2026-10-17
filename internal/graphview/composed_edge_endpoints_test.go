package graphview

import (
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Composed-view endpoint projections: every answer must equal the composed
// view's full-row readers reduced to endpoints, over a view whose layers carry
// every mask shape — a replaced file (with a dropped symbol), a deleted file, a
// context path, a node tombstone in an untouched file and a replaced edge
// source — and over a two-generation chain. A mask that let a base row leak
// through the projection shows up as a row only one side holds.

const (
	epReplaced  = "repo/replaced.go"
	epDeleted   = "repo/deleted.go"
	epContext   = "repo/context.go"
	epUntouched = "repo/untouched.go"
	epSource    = "repo/source.go"
	epCaller    = "repo/caller.go"
)

func epNode(file, name string) *graph.Node {
	return &graph.Node{ID: file + "::" + name, Kind: graph.KindFunction, Name: name, FilePath: file}
}

func epEdge(from, to string, kind graph.EdgeKind, file string, line int) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: kind, FilePath: file, Line: line}
}

func epIDs(files ...string) []string {
	var out []string
	for _, f := range files {
		for _, n := range []string{"A", "B", "Keep", "Drop", "Gone", "Helper", "Hub", "Call", "New"} {
			out = append(out, f+"::"+n)
		}
	}
	return out
}

// epBase is the committed corpus: symbols in every file and edges recorded
// both in the source's own file and in another file (the caller's).
func epBase(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	nodes := []*graph.Node{
		epNode(epReplaced, "Keep"), epNode(epReplaced, "Drop"),
		epNode(epDeleted, "Gone"),
		epNode(epContext, "Helper"),
		epNode(epUntouched, "A"), epNode(epUntouched, "B"),
		epNode(epSource, "Hub"),
		epNode(epCaller, "Call"),
	}
	id := func(file, name string) string { return file + "::" + name }
	edges := []*graph.Edge{
		epEdge(id(epReplaced, "Keep"), id(epContext, "Helper"), graph.EdgeCalls, epReplaced, 3),
		epEdge(id(epReplaced, "Drop"), id(epUntouched, "A"), graph.EdgeReferences, epReplaced, 4),
		{From: id(epReplaced, "Keep"), To: id(epCaller, "Call"), Kind: graph.EdgeValueFlow, FilePath: epCaller, Line: 9, Confidence: 0.5, Meta: map[string]any{"return_position": 0}}, // recorded elsewhere
		epEdge(id(epDeleted, "Gone"), id(epUntouched, "B"), graph.EdgeCalls, epDeleted, 2),
		epEdge(id(epContext, "Helper"), id(epUntouched, "A"), graph.EdgeCalls, epContext, 5),
		epEdge(id(epUntouched, "A"), id(epUntouched, "B"), graph.EdgeCalls, epUntouched, 6),
		epEdge(id(epUntouched, "B"), id(epDeleted, "Gone"), graph.EdgeCalls, epUntouched, 7),
		epEdge(id(epSource, "Hub"), id(epUntouched, "A"), graph.EdgeCalls, epSource, 8),
		epEdge(id(epSource, "Hub"), id(epCaller, "Call"), graph.EdgeImports, epCaller, 1),
		epEdge(id(epCaller, "Call"), id(epReplaced, "Drop"), graph.EdgeCalls, epCaller, 10),
		epEdge(id(epCaller, "Call"), id(epReplaced, "Keep"), graph.EdgeCalls, epCaller, 11),
		epEdge(id(epCaller, "Call"), "unresolved::Missing", graph.EdgeCalls, epCaller, 12),
	}
	store.AddBatch(nodes, edges)
}

// epFirstGeneration replaces epReplaced (keeping Keep, dropping Drop), deletes
// epDeleted, declares epContext context, tombstones untouched.go::B and
// replaces the adjacency of source.go::Hub.
func epFirstGeneration(t *testing.T, store *store_sqlite.Store) *store_sqlite.Store {
	t.Helper()
	_, handle := beginTestGeneration(t, store, "endpoints-first")
	keep := epNode(epReplaced, "Keep")
	newer := epNode(epReplaced, "New")
	handle.AddBatch([]*graph.Node{keep, newer, epNode(epSource, "Hub")}, []*graph.Edge{
		epEdge(keep.ID, epContext+"::Helper", graph.EdgeCalls, epReplaced, 3),
		epEdge(newer.ID, epUntouched+"::A", graph.EdgeReads, epReplaced, 5),
		epEdge(epSource+"::Hub", newer.ID, graph.EdgeCalls, epSource, 8),
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: epReplaced, Mode: store_sqlite.OwnershipReplace},
		{FilePath: epDeleted, Mode: store_sqlite.OwnershipDelete},
		{FilePath: epContext, Mode: store_sqlite.OwnershipContext},
		{FilePath: epSource, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := handle.SetNodeTombstones([]string{epUntouched + "::B"}); err != nil {
		t.Fatalf("SetNodeTombstones: %v", err)
	}
	if err := handle.SetEdgeSourceMasks([]store_sqlite.EdgeSourceMask{
		{SourceID: epSource + "::Hub", Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetEdgeSourceMasks: %v", err)
	}
	publishTestGeneration(t, store, handle.ViewGeneration())
	return handle
}

// epSecondGeneration replaces caller.go on top of the first generation.
func epSecondGeneration(t *testing.T, store *store_sqlite.Store) *store_sqlite.Store {
	t.Helper()
	_, handle := beginTestGeneration(t, store, "endpoints-second")
	call := epNode(epCaller, "Call")
	handle.AddBatch([]*graph.Node{call}, []*graph.Edge{
		epEdge(call.ID, epReplaced+"::New", graph.EdgeCalls, epCaller, 10),
		epEdge(call.ID, epReplaced+"::Keep", graph.EdgeReferences, epCaller, 11),
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: epCaller, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	publishTestGeneration(t, store, handle.ViewGeneration())
	return handle
}

func epRender(rows []graph.EdgeEndpointRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s -%s-> %s @%s", r.From, r.Kind, r.To, r.FilePath))
	}
	sort.Strings(out)
	return out
}

func epRenderEdges(edges []*graph.Edge, keep func(*graph.Edge) bool) []string {
	var rows []graph.EdgeEndpointRow
	for _, e := range edges {
		if e != nil && (keep == nil || keep(e)) {
			rows = append(rows, graph.EdgeEndpointRow{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath})
		}
	}
	return epRender(rows)
}

// epAssertIdentity compares every projection with the view's full-row
// readers.
func epAssertIdentity(t *testing.T, label string, view graph.Reader) {
	t.Helper()
	proj, ok := graph.EdgeEndpointsOf(view)
	if !ok {
		t.Fatalf("%s: the composed view serves no endpoint projections", label)
	}
	paths := []string{epReplaced, epDeleted, epContext, epUntouched, epSource, epCaller}
	ids := epIDs(paths...)

	for _, subset := range [][]string{paths, {epCaller}, {epContext, epDeleted}, {epReplaced, epSource}} {
		want := map[string]struct{}{}
		for _, p := range subset {
			want[p] = struct{}{}
		}
		got := epRender(proj.EdgeEndpointsRecordedAt(subset))
		full := epRenderEdges(view.AllEdges(), func(e *graph.Edge) bool { _, ok := want[e.FilePath]; return ok })
		if !slices.Equal(got, full) {
			t.Errorf("%s: EdgeEndpointsRecordedAt(%v)\n projection: %v\n full rows:  %v", label, subset, got, full)
		}
	}

	recorded, ok := graph.RecordedEdgesOf(view)
	if !ok {
		t.Fatalf("%s: the composed view serves no by-file edge reader", label)
	}
	for _, subset := range [][]string{paths, {epCaller}, {epContext, epDeleted}, {epReplaced, epSource}} {
		want := map[string]struct{}{}
		for _, p := range subset {
			want[p] = struct{}{}
		}
		got := epRenderFull(recorded.RecordedEdgesAt(subset), nil)
		full := epRenderFull(view.AllEdges(), func(e *graph.Edge) bool { _, ok := want[e.FilePath]; return ok })
		if !slices.Equal(got, full) {
			t.Errorf("%s: RecordedEdgesAt(%v)\n reader:    %v\n full rows: %v", label, subset, got, full)
		}
	}

	var outEdges []*graph.Edge
	for _, list := range view.GetOutEdgesByNodeIDs(ids) {
		outEdges = append(outEdges, list...)
	}
	for _, kinds := range [][]graph.EdgeKind{nil, {graph.EdgeCalls}, {graph.EdgeImports, graph.EdgeValueFlow, graph.EdgeReads}, {""}} {
		set := map[graph.EdgeKind]struct{}{}
		for _, k := range kinds {
			if k != "" {
				set[k] = struct{}{}
			}
		}
		got := epRender(proj.EdgeEndpointsFrom(ids, kinds))
		var full []string
		if len(kinds) == 0 || len(set) > 0 {
			full = epRenderEdges(outEdges, func(e *graph.Edge) bool {
				if len(kinds) == 0 {
					return true
				}
				_, ok := set[e.Kind]
				return ok
			})
		}
		if !slices.Equal(got, full) {
			t.Errorf("%s: EdgeEndpointsFrom(kinds=%v)\n projection: %v\n full rows:  %v", label, kinds, got, full)
		}
	}

	names := proj.NodeNamesByIDs(ids)
	full := view.GetNodesByIDs(ids)
	if len(names) != len(full) {
		t.Errorf("%s: NodeNamesByIDs returned %d ids, GetNodesByIDs %d: %v", label, len(names), len(full), names)
	}
	for id, node := range full {
		if name, ok := names[id]; !ok || name != node.Name {
			t.Errorf("%s: NodeNamesByIDs[%s] = %q (present %v), want %q", label, id, name, ok, node.Name)
		}
	}

	gotPaths := proj.OutEdgePathsFrom(ids)
	wantPaths := map[string][]string{}
	for _, e := range outEdges {
		if !slices.Contains(wantPaths[e.From], e.FilePath) {
			wantPaths[e.From] = append(wantPaths[e.From], e.FilePath)
		}
	}
	for from := range wantPaths {
		sort.Strings(wantPaths[from])
	}
	if len(gotPaths) != len(wantPaths) {
		t.Errorf("%s: OutEdgePathsFrom = %v, want %v", label, gotPaths, wantPaths)
	}
	for from, want := range wantPaths {
		if !slices.Equal(gotPaths[from], want) {
			t.Errorf("%s: OutEdgePathsFrom[%s] = %v, want %v", label, from, gotPaths[from], want)
		}
	}
}

// TestComposedViewEdgeEndpointsMatchTheFullRowReaders runs the identity check
// over a one-generation view and a two-generation chain, and pins that the
// masks actually hide rows here (so the identity is not vacuous).
func TestComposedViewEdgeEndpointsMatchTheFullRowReaders(t *testing.T) {
	store := openTestStore(t)
	epBase(t, store)
	first := epFirstGeneration(t, store)
	second := epSecondGeneration(t, store)

	firstLayer, err := NewGenerationLayer(first)
	if err != nil {
		t.Fatalf("NewGenerationLayer(first): %v", err)
	}
	secondLayer, err := NewGenerationLayer(second)
	if err != nil {
		t.Fatalf("NewGenerationLayer(second): %v", err)
	}
	one := graph.NewOverlaidViewWithLayer(store, firstLayer)
	chain := graph.NewOverlaidViewWithLayer(one, secondLayer)

	epAssertIdentity(t, "one-layer", one)
	epAssertIdentity(t, "chain", chain)

	// The base serves rows the masks must hide: the deleted file's edge, the
	// replaced file's dropped symbol, the tombstoned target, the replaced
	// source's edge recorded in another file.
	proj, _ := graph.EdgeEndpointsOf(chain)
	rows := epRender(proj.EdgeEndpointsRecordedAt([]string{epDeleted, epReplaced, epUntouched, epCaller}))
	for _, leaked := range []string{
		epDeleted + "::Gone -calls-> " + epUntouched + "::B @" + epDeleted,
		epReplaced + "::Drop -references-> " + epUntouched + "::A @" + epReplaced,
		epUntouched + "::A -calls-> " + epUntouched + "::B @" + epUntouched,
		epSource + "::Hub -imports-> " + epCaller + "::Call @" + epCaller,
		epCaller + "::Call -calls-> " + epReplaced + "::Drop @" + epCaller,
	} {
		if slices.Contains(rows, leaked) {
			t.Errorf("a masked base row leaked through the projection: %s", leaked)
		}
	}
	if !slices.Contains(rows, epCaller+"::Call -calls-> "+epReplaced+"::New @"+epCaller) {
		t.Errorf("the chain's own row is missing: %v", rows)
	}
}

// TestComposedViewEdgeEndpointsNeedEveryReaderBelow: a view over a reader
// that cannot project (the in-memory graph) serves no projections, so a caller
// falls back to the full-row readers instead of reading a partial answer.
func TestComposedViewEdgeEndpointsNeedEveryReaderBelow(t *testing.T) {
	store := openTestStore(t)
	epBase(t, store)
	layer, err := NewGenerationLayer(epFirstGeneration(t, store))
	if err != nil {
		t.Fatalf("NewGenerationLayer: %v", err)
	}
	if _, ok := graph.EdgeEndpointsOf(graph.NewOverlaidViewWithLayer(graph.New(), layer)); ok {
		t.Fatal("a view over an in-memory graph claimed endpoint projections")
	}
	if _, ok := graph.RecordedEdgesOf(graph.NewOverlaidViewWithLayer(graph.New(), layer)); ok {
		t.Fatal("a view over an in-memory graph claimed the by-file edge reader")
	}
	if _, ok := graph.EdgeEndpointsOf(graph.NewOverlaidViewWithLayer(store, layer)); !ok {
		t.Fatal("a view over a store handle served no endpoint projections")
	}
}

// epRenderFull renders every field of each edge (line, confidence, Meta), so
// the by-file reader is held to full-row identity, not endpoints.
func epRenderFull(edges []*graph.Edge, keep func(*graph.Edge) bool) []string {
	var out []string
	for _, e := range edges {
		if e != nil && (keep == nil || keep(e)) {
			out = append(out, fmt.Sprintf("%+v", *e))
		}
	}
	sort.Strings(out)
	return out
}
