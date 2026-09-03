package graphview

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The stacked-view equivalence fixture.
//
// One store carries a base corpus plus two sparse generations written
// through the real payload lifecycle: a commit generation that renames a
// symbol, deletes one file, adds another, and reaches into a file it does
// not mask at all — retargeting one symbol's calls and tombstoning
// another — plus a working-tree generation on top that rewrites a third
// file and deletes the added one. A second store is indexed with the tree
// those three states describe, as one plain corpus with no generations at
// all.
//
// Everything a graph.Reader answers is then compared between the
// composed stack and the flat corpus. The composition is only correct if
// no reader can tell them apart.
const (
	stackRepo = "repo"

	stackKeepFile  = stackRepo + "/keep.go"
	stackDepFile   = stackRepo + "/dep.go"
	stackEditFile  = stackRepo + "/edit.go"
	stackGoneFile  = stackRepo + "/gone.go"
	stackAddedFile = stackRepo + "/added.go"

	stackKeeperID  = stackKeepFile + "::Keeper"
	stackConfigID  = stackKeepFile + "::Config"
	stackCallerID  = stackDepFile + "::Caller"
	stackStaleID   = stackDepFile + "::Stale"
	stackOldID     = stackEditFile + "::Old"
	stackDroppedID = stackEditFile + "::Dropped"
	stackDoomedID  = stackGoneFile + "::Doomed"
	stackNewID     = stackEditFile + "::New"
	stackFreshID   = stackAddedFile + "::Fresh"

	stackCommitLayerID = "layer-commit"
	stackDirtyLayerID  = "layer-dirty"
)

// stackFileNode builds a file node: a bare-path identity, which is the
// shape the composition's path ownership has to recognise without an
// "::" separator to split on.
func stackFileNode(path string, endLine int) *graph.Node {
	return &graph.Node{
		ID:         path,
		Kind:       graph.KindFile,
		Name:       path[strings.LastIndexByte(path, '/')+1:],
		FilePath:   path,
		RepoPrefix: stackRepo,
		Language:   "go",
		EndLine:    endLine,
	}
}

// stackSymbol builds one symbol node. StartLine is what tells two
// versions of the same identity apart, so a reader that returned the
// lower layer's copy where a higher one re-emitted the ID is caught.
func stackSymbol(id, name string, kind graph.NodeKind, file string, startLine int) *graph.Node {
	return &graph.Node{
		ID:         id,
		Kind:       kind,
		Name:       name,
		QualName:   "pkg." + name,
		FilePath:   file,
		RepoPrefix: stackRepo,
		Language:   "go",
		StartLine:  startLine,
		EndLine:    startLine + 3,
	}
}

func stackEdge(from, to string, kind graph.EdgeKind, file string, line int) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: kind, FilePath: file, Line: line}
}

func stackFileMeta(path string, nodeCount int) graph.FileMetaRow {
	return graph.FileMetaRow{
		FilePath:    path,
		ContentHash: "hash/" + path,
		Size:        64 * nodeCount,
		NodeCount:   nodeCount,
	}
}

// openStackStore opens an empty database for one test.
func openStackStore(t testing.TB, name string) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), name+".sqlite"))
	if err != nil {
		t.Fatalf("open %s store: %v", name, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedStackCorpus writes the indexed corpus every stack sits on: four
// files, seven symbols, and the adjacency between them.
func seedStackCorpus(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	store.AddBatch([]*graph.Node{
		stackFileNode(stackKeepFile, 40),
		stackFileNode(stackDepFile, 20),
		stackFileNode(stackEditFile, 50),
		stackFileNode(stackGoneFile, 15),
		stackSymbol(stackKeeperID, "Keeper", graph.KindFunction, stackKeepFile, 12),
		stackSymbol(stackConfigID, "Config", graph.KindType, stackKeepFile, 30),
		stackSymbol(stackCallerID, "Caller", graph.KindFunction, stackDepFile, 5),
		stackSymbol(stackStaleID, "Stale", graph.KindFunction, stackDepFile, 12),
		stackSymbol(stackOldID, "Old", graph.KindFunction, stackEditFile, 18),
		stackSymbol(stackDroppedID, "Dropped", graph.KindFunction, stackEditFile, 28),
		stackSymbol(stackDoomedID, "Doomed", graph.KindFunction, stackGoneFile, 6),
	}, []*graph.Edge{
		stackEdge(stackKeepFile, stackKeeperID, graph.EdgeContains, stackKeepFile, 12),
		stackEdge(stackKeepFile, stackConfigID, graph.EdgeContains, stackKeepFile, 30),
		stackEdge(stackDepFile, stackCallerID, graph.EdgeContains, stackDepFile, 5),
		stackEdge(stackDepFile, stackStaleID, graph.EdgeContains, stackDepFile, 12),
		stackEdge(stackEditFile, stackOldID, graph.EdgeContains, stackEditFile, 18),
		stackEdge(stackEditFile, stackDroppedID, graph.EdgeContains, stackEditFile, 28),
		stackEdge(stackGoneFile, stackDoomedID, graph.EdgeContains, stackGoneFile, 6),
		stackEdge(stackKeeperID, stackConfigID, graph.EdgeReferences, stackKeepFile, 14),
		stackEdge(stackCallerID, stackOldID, graph.EdgeCalls, stackDepFile, 7),
		stackEdge(stackCallerID, stackDoomedID, graph.EdgeCalls, stackDepFile, 8),
		stackEdge(stackStaleID, stackKeeperID, graph.EdgeCalls, stackDepFile, 13),
		stackEdge(stackOldID, stackKeeperID, graph.EdgeCalls, stackEditFile, 20),
		stackEdge(stackDroppedID, stackKeeperID, graph.EdgeCalls, stackEditFile, 30),
		stackEdge(stackDoomedID, stackKeeperID, graph.EdgeCalls, stackGoneFile, 9),
	})
	if err := store.SetFileMetas(stackRepo, []graph.FileMetaRow{
		stackFileMeta(stackKeepFile, 3),
		stackFileMeta(stackDepFile, 3),
		stackFileMeta(stackEditFile, 3),
		stackFileMeta(stackGoneFile, 2),
	}); err != nil {
		t.Fatalf("SetFileMetas corpus: %v", err)
	}
}

// writeStackCommitGeneration publishes the commit generation: edit.go is
// re-derived with Old renamed to New and Dropped gone, added.go is new,
// gone.go is deleted, dep.go's Caller keeps its node in the corpus while
// its outgoing edges are retargeted through an edge-source marker, and
// dep.go's Stale is removed through a node tombstone. The last two are
// the claims a file-granular layer cannot make: dep.go is masked by no
// generation in the stack, yet one of its symbols moves and another
// disappears.
func writeStackCommitGeneration(t *testing.T, store *store_sqlite.Store, baseGenerations ...int64) int64 {
	t.Helper()
	if len(baseGenerations) > 1 {
		t.Fatalf("writeStackCommitGeneration: got %d base generations, want at most one", len(baseGenerations))
	}
	baseGeneration := int64(0)
	if len(baseGenerations) == 1 {
		baseGeneration = baseGenerations[0]
	}
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          testGraphID,
		LayerID:          stackCommitLayerID,
		CheckoutID:       testCheckoutID,
		GenerationKind:   "commit",
		BaseGenerationID: baseGeneration,
		TreeOID:          "tree-commit",
		CreatedAt:        1000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(commit): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		stackFileNode(stackEditFile, 25),
		stackFileNode(stackAddedFile, 8),
		stackSymbol(stackNewID, "New", graph.KindFunction, stackEditFile, 18),
		stackSymbol(stackFreshID, "Fresh", graph.KindFunction, stackAddedFile, 2),
	}, []*graph.Edge{
		stackEdge(stackEditFile, stackNewID, graph.EdgeContains, stackEditFile, 18),
		stackEdge(stackAddedFile, stackFreshID, graph.EdgeContains, stackAddedFile, 2),
		stackEdge(stackNewID, stackKeeperID, graph.EdgeCalls, stackEditFile, 21),
		stackEdge(stackFreshID, stackNewID, graph.EdgeCalls, stackAddedFile, 3),
		stackEdge(stackCallerID, stackNewID, graph.EdgeCalls, stackDepFile, 7),
	})
	if err := handle.SetFileMetas(stackRepo, []graph.FileMetaRow{
		stackFileMeta(stackEditFile, 2),
		stackFileMeta(stackAddedFile, 2),
	}); err != nil {
		t.Fatalf("SetFileMetas(commit): %v", err)
	}
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: stackRepo, FilePath: stackEditFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: stackRepo, FilePath: stackAddedFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: stackRepo, FilePath: stackGoneFile, Mode: store_sqlite.OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks(commit): %v", err)
	}
	if err := handle.SetEdgeSourceMasks([]store_sqlite.EdgeSourceMask{
		{SourceID: stackCallerID, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetEdgeSourceMasks(commit): %v", err)
	}
	if err := handle.SetNodeTombstones([]string{stackStaleID}); err != nil {
		t.Fatalf("SetNodeTombstones(commit): %v", err)
	}
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 2000); err != nil {
		t.Fatalf("PublishPayloadGeneration(commit): %v", err)
	}
	return generationID
}

// writeStackDirtyGeneration publishes the working-tree generation over
// the commit one: keep.go is re-derived with Config gone and Keeper
// moved and now calling New, and added.go is deleted again — a path the
// layer below claimed, so the delete has to win over a replacement
// rather than over the corpus.
func writeStackDirtyGeneration(t *testing.T, store *store_sqlite.Store, baseGeneration int64) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          testGraphID,
		LayerID:          stackDirtyLayerID,
		CheckoutID:       testCheckoutID,
		GenerationKind:   "dirty",
		BaseGenerationID: baseGeneration,
		TreeOID:          "tree-dirty",
		CreatedAt:        3000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(dirty): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		stackFileNode(stackKeepFile, 12),
		stackSymbol(stackKeeperID, "Keeper", graph.KindFunction, stackKeepFile, 2),
	}, []*graph.Edge{
		stackEdge(stackKeepFile, stackKeeperID, graph.EdgeContains, stackKeepFile, 2),
		stackEdge(stackKeeperID, stackNewID, graph.EdgeCalls, stackKeepFile, 5),
	})
	if err := handle.SetFileMetas(stackRepo, []graph.FileMetaRow{stackFileMeta(stackKeepFile, 2)}); err != nil {
		t.Fatalf("SetFileMetas(dirty): %v", err)
	}
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: stackRepo, FilePath: stackKeepFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: stackRepo, FilePath: stackAddedFile, Mode: store_sqlite.OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks(dirty): %v", err)
	}
	if err := handle.SetProducerState(store_sqlite.ProducerCompleteness{
		Producer: string(CapResolutionCrossRepo),
		State:    store_sqlite.ProducerStateIncomplete,
		Reason:   "the working tree is resolved locally only",
	}); err != nil {
		t.Fatalf("SetProducerState(dirty): %v", err)
	}
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 4000); err != nil {
		t.Fatalf("PublishPayloadGeneration(dirty): %v", err)
	}
	return generationID
}

// seedStackFlatCorpus indexes the tree the two generations describe as
// one plain corpus. It is written by hand rather than derived from the
// fixtures above, so a mistake in the composition cannot cancel out
// against the same mistake in the reference.
func seedStackFlatCorpus(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	store.AddBatch([]*graph.Node{
		stackFileNode(stackKeepFile, 12),
		stackFileNode(stackDepFile, 20),
		stackFileNode(stackEditFile, 25),
		stackSymbol(stackKeeperID, "Keeper", graph.KindFunction, stackKeepFile, 2),
		stackSymbol(stackCallerID, "Caller", graph.KindFunction, stackDepFile, 5),
		stackSymbol(stackNewID, "New", graph.KindFunction, stackEditFile, 18),
	}, []*graph.Edge{
		stackEdge(stackKeepFile, stackKeeperID, graph.EdgeContains, stackKeepFile, 2),
		stackEdge(stackDepFile, stackCallerID, graph.EdgeContains, stackDepFile, 5),
		stackEdge(stackEditFile, stackNewID, graph.EdgeContains, stackEditFile, 18),
		stackEdge(stackKeeperID, stackNewID, graph.EdgeCalls, stackKeepFile, 5),
		stackEdge(stackCallerID, stackNewID, graph.EdgeCalls, stackDepFile, 7),
		stackEdge(stackNewID, stackKeeperID, graph.EdgeCalls, stackEditFile, 21),
	})
	if err := store.SetFileMetas(stackRepo, []graph.FileMetaRow{
		stackFileMeta(stackKeepFile, 2),
		stackFileMeta(stackDepFile, 2),
		stackFileMeta(stackEditFile, 2),
	}); err != nil {
		t.Fatalf("SetFileMetas flat: %v", err)
	}
}

// stackedReader builds the composed stack over one seeded store and
// returns it with the identity that names it.
func stackedReader(t *testing.T, store *store_sqlite.Store, commit, dirty int64) (graph.Reader, RepoViewID) {
	t.Helper()
	commitLayer, err := NewGenerationLayer(store.AtGeneration(commit))
	if err != nil {
		t.Fatalf("NewGenerationLayer(commit): %v", err)
	}
	dirtyLayer, err := NewGenerationLayer(store.AtGeneration(dirty))
	if err != nil {
		t.Fatalf("NewGenerationLayer(dirty): %v", err)
	}
	base := graph.NewOverlaidViewWithLayer(store.AtGeneration(0), commitLayer)
	id, err := NewRepoViewID(stackRepo, testGraphID, commit,
		LayerRef{Kind: LayerDirty, LayerID: stackDirtyLayerID, Generation: dirty})
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	reader, id, err := ComposeRepoView(base, []graph.OverlayLayerReader{dirtyLayer}, id)
	if err != nil {
		t.Fatalf("ComposeRepoView: %v", err)
	}
	return reader, id
}

// --- differential comparison -------------------------------------------

// stackKinds and stackEdgeKinds are the kinds the fixture uses plus one
// the final tree has none of, so the per-kind scans are checked for
// agreeing on emptiness too.
var (
	stackKinds     = []graph.NodeKind{graph.KindFile, graph.KindFunction, graph.KindType}
	stackEdgeKinds = []graph.EdgeKind{graph.EdgeContains, graph.EdgeCalls, graph.EdgeReferences}
)

// stackProbeIDs is every identity either side could answer for,
// including the ones the stack must hide. An ID that vanished has to
// vanish the same way on both sides.
func stackProbeIDs() []string {
	return []string{
		stackKeepFile, stackDepFile, stackEditFile, stackGoneFile, stackAddedFile,
		stackKeeperID, stackConfigID, stackCallerID, stackStaleID,
		stackOldID, stackDroppedID, stackDoomedID, stackNewID, stackFreshID,
	}
}

func stackProbeFiles() []string {
	return []string{stackKeepFile, stackDepFile, stackEditFile, stackGoneFile, stackAddedFile}
}

func stackProbeNames() []string {
	return []string{"Keeper", "Config", "Caller", "Stale", "Old", "Dropped", "Doomed", "New", "Fresh", "keep.go"}
}

func stackProbeQualNames() []string {
	return []string{
		"pkg.Keeper", "pkg.Config", "pkg.Caller", "pkg.Stale",
		"pkg.Old", "pkg.Dropped", "pkg.Doomed", "pkg.New", "pkg.Fresh",
	}
}

// renderNode prints every field of a node except the one the graph never
// persists: AbsoluteFilePath is stamped on per-response copies by the
// MCP layer, so it is view-only metadata rather than content.
func renderNode(n *graph.Node) string {
	if n == nil {
		return "<nil>"
	}
	copied := *n
	copied.AbsoluteFilePath = ""
	return fmt.Sprintf("%+v", copied)
}

// renderEdge prints every field of an edge except the ones response
// encoders fill in on demand and the store never writes. Tier is not one
// of them — edges carry a persisted tier column — so it stays in the
// comparison and a generation that stamped a different tier than the flat
// corpus would show up as a difference rather than be normalized away.
func renderEdge(e *graph.Edge) string {
	if e == nil {
		return "<nil>"
	}
	copied := *e
	copied.Context = ""
	copied.ReturnUsage = ""
	copied.Via = ""
	copied.Alias = ""
	copied.NameOnly = false
	return fmt.Sprintf("%+v", copied)
}

func renderNodes(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, renderNode(n))
	}
	slices.Sort(out)
	return out
}

func renderEdges(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, renderEdge(e))
	}
	slices.Sort(out)
	return out
}

func collectNodes(seq func(func(*graph.Node) bool)) []*graph.Node {
	var out []*graph.Node
	for n := range seq {
		out = append(out, n)
	}
	return out
}

func collectEdges(seq func(func(*graph.Edge) bool)) []*graph.Edge {
	var out []*graph.Edge
	for e := range seq {
		out = append(out, e)
	}
	return out
}

func assertSameStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if slices.Equal(got, want) {
		return
	}
	t.Errorf("%s\n composed: %s\n     flat: %s", what, strings.Join(got, "\n           "), strings.Join(want, "\n           "))
}

// assertReadersAgree drives every read on the graph.Reader surface
// through both graphs and compares the answers.
func assertReadersAgree(t *testing.T, composed, flat graph.Reader) {
	t.Helper()

	assertSameStrings(t, "AllNodes", renderNodes(composed.AllNodes()), renderNodes(flat.AllNodes()))
	assertSameStrings(t, "AllEdges", renderEdges(composed.AllEdges()), renderEdges(flat.AllEdges()))
	if got, want := composed.NodeCount(), flat.NodeCount(); got != want {
		t.Errorf("NodeCount = %d, flat corpus has %d", got, want)
	}
	if got, want := composed.EdgeCount(), flat.EdgeCount(); got != want {
		t.Errorf("EdgeCount = %d, flat corpus has %d", got, want)
	}

	ids := stackProbeIDs()
	for _, id := range ids {
		assertSameStrings(t, "GetNode("+id+")",
			[]string{renderNode(composed.GetNode(id))}, []string{renderNode(flat.GetNode(id))})
		assertSameStrings(t, "GetOutEdges("+id+")",
			renderEdges(composed.GetOutEdges(id)), renderEdges(flat.GetOutEdges(id)))
		assertSameStrings(t, "GetInEdges("+id+")",
			renderEdges(composed.GetInEdges(id)), renderEdges(flat.GetInEdges(id)))
	}

	composedOut, flatOut := composed.GetOutEdgesByNodeIDs(ids), flat.GetOutEdgesByNodeIDs(ids)
	composedIn, flatIn := composed.GetInEdgesByNodeIDs(ids), flat.GetInEdgesByNodeIDs(ids)
	composedNodes, flatNodes := composed.GetNodesByIDs(ids), flat.GetNodesByIDs(ids)
	for _, id := range ids {
		assertSameStrings(t, "GetOutEdgesByNodeIDs["+id+"]",
			renderEdges(composedOut[id]), renderEdges(flatOut[id]))
		assertSameStrings(t, "GetInEdgesByNodeIDs["+id+"]",
			renderEdges(composedIn[id]), renderEdges(flatIn[id]))
		assertSameStrings(t, "GetNodesByIDs["+id+"]",
			[]string{renderNode(composedNodes[id])}, []string{renderNode(flatNodes[id])})
	}

	for _, path := range stackProbeFiles() {
		assertSameStrings(t, "GetFileNodes("+path+")",
			renderNodes(composed.GetFileNodes(path)), renderNodes(flat.GetFileNodes(path)))
	}
	assertSameStrings(t, "GetRepoNodes",
		renderNodes(composed.GetRepoNodes(stackRepo)), renderNodes(flat.GetRepoNodes(stackRepo)))

	for _, name := range stackProbeNames() {
		assertSameStrings(t, "FindNodesByName("+name+")",
			renderNodes(composed.FindNodesByName(name)), renderNodes(flat.FindNodesByName(name)))
	}
	for _, qual := range stackProbeQualNames() {
		assertSameStrings(t, "GetNodeByQualName("+qual+")",
			[]string{renderNode(composed.GetNodeByQualName(qual))},
			[]string{renderNode(flat.GetNodeByQualName(qual))})
	}

	for _, kind := range stackKinds {
		assertSameStrings(t, "NodesByKind("+string(kind)+")",
			renderNodes(collectNodes(composed.NodesByKind(kind))),
			renderNodes(collectNodes(flat.NodesByKind(kind))))
	}
	for _, kind := range stackEdgeKinds {
		assertSameStrings(t, "EdgesByKind("+string(kind)+")",
			renderEdges(collectEdges(composed.EdgesByKind(kind))),
			renderEdges(collectEdges(flat.EdgesByKind(kind))))
	}
}

// TestComposeRepoViewMatchesFlatCorpus is the differential: a stack of
// two persisted generations over an indexed corpus must read exactly
// like the same tree indexed flat.
//
// It covers the sparse cases a generation exists for — a replaced file,
// a deleted file, an added file, a symbol dropped from a replaced file,
// and a path one layer replaced that the layer above deletes — plus the
// two claims that reach into a file no generation masks: dep.go is never
// re-derived, yet its Caller points at the renamed symbol because the
// commit generation carries an edge-source marker for it, and its Stale
// is gone because the same generation tombstoned that identity.
func TestComposeRepoViewMatchesFlatCorpus(t *testing.T) {
	store := openStackStore(t, "stacked")
	seedStackCorpus(t, store)
	commit := writeStackCommitGeneration(t, store)
	dirty := writeStackDirtyGeneration(t, store, commit)

	flat := openStackStore(t, "flat")
	seedStackFlatCorpus(t, flat)

	composed, id := stackedReader(t, store, commit, dirty)
	if id.BaseGeneration != commit || len(id.Layers) != 1 || id.Layers[0].Generation != dirty {
		t.Fatalf("composed identity = %+v, want base %d over layer %d", id, commit, dirty)
	}

	// The stack has to be doing work, or agreeing with the flat corpus
	// would prove nothing about the layers. These pin what each sparse
	// case is for, so a composition that stopped applying one of them
	// fails here and not only as a set difference.
	if corpus := store.AtGeneration(0); corpus.NodeCount() == composed.NodeCount() {
		t.Fatalf("the composed stack has the corpus's %d nodes — the layers changed nothing", corpus.NodeCount())
	}
	if n := composed.GetNode(stackOldID); n != nil {
		t.Errorf("the renamed symbol still resolves: %+v", n)
	}
	if n := composed.GetNode(stackDoomedID); n != nil {
		t.Errorf("a symbol in the deleted file still resolves: %+v", n)
	}
	if n := composed.GetNode(stackFreshID); n != nil {
		t.Errorf("a symbol the top layer deleted still resolves: %+v", n)
	}
	if n := composed.GetNode(stackKeeperID); n == nil || n.StartLine != 2 {
		t.Errorf("Keeper = %+v, want the working tree's copy at line 2", n)
	}
	// The unchanged dependent: dep.go was never re-derived, so its node
	// comes from the corpus while its outgoing edges come from the
	// generation that marked it.
	if n := composed.GetNode(stackCallerID); n == nil || n.StartLine != 5 {
		t.Errorf("Caller = %+v, want the corpus copy at line 5", n)
	}
	out := composed.GetOutEdges(stackCallerID)
	if len(out) != 1 || out[0].To != stackNewID {
		t.Errorf("Caller's out-edges = %s, want the one retargeted call", renderEdges(out))
	}
	// Its neighbour in the same unmasked file is gone instead: a node
	// tombstone hides one identity without claiming the file it lives in,
	// so the file's other symbols are untouched.
	if n := composed.GetNode(stackStaleID); n != nil {
		t.Errorf("a tombstoned symbol still resolves: %+v", n)
	}
	if nodes := composed.GetFileNodes(stackDepFile); len(nodes) != 2 {
		t.Errorf("dep.go = %s, want its file node and Caller", renderNodes(nodes))
	}

	assertReadersAgree(t, composed, flat)
}

// TestComposeRepoViewRefusesMalformedStacks pins the guards that keep a
// composed reader and the identity naming it from drifting apart.
func TestComposeRepoViewRefusesMalformedStacks(t *testing.T) {
	base := graph.New()
	oneLayer := []graph.OverlayLayerReader{graph.NewOverlayLayer()}
	valid, err := NewRepoViewID(stackRepo, testGraphID, 7,
		LayerRef{Kind: LayerDirty, LayerID: stackDirtyLayerID, Generation: 8})
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}

	deep := make([]graph.OverlayLayerReader, MaxRepoViewLayers+1)
	deepID := valid
	deepID.Layers = nil
	for i := range deep {
		deep[i] = graph.NewOverlayLayer()
		deepID.Layers = append(deepID.Layers, LayerRef{
			Kind: LayerDirty, LayerID: fmt.Sprintf("l%d", i), Generation: int64(i + 1),
		})
	}

	cases := []struct {
		name   string
		base   graph.Reader
		layers []graph.OverlayLayerReader
		id     RepoViewID
	}{
		{"no base", nil, oneLayer, valid},
		{"too deep", base, deep, deepID},
		{"identity names no layers", base, oneLayer, RepoViewID{
			RepoPrefix: stackRepo, BaseGraphID: testGraphID, BaseGeneration: 7,
		}},
		{"malformed identity", base, nil, RepoViewID{BaseGraphID: testGraphID, BaseGeneration: 7}},
		{"missing layer", base, []graph.OverlayLayerReader{nil}, valid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, _, err := ComposeRepoView(tc.base, tc.layers, tc.id)
			if err == nil {
				t.Fatalf("ComposeRepoView returned %v, want an error", reader)
			}
			if code := CodeOf(err); code != CodeInvalidViewSelector {
				t.Fatalf("error code = %q, want %q", code, CodeInvalidViewSelector)
			}
		})
	}
}

// TestComposeRepoViewWithoutLayersReturnsBase pins the empty stack: a
// checkout whose working tree matches its commit reads the base
// unchanged rather than through a layer that covers nothing.
func TestComposeRepoViewWithoutLayersReturnsBase(t *testing.T) {
	base := graph.New()
	id, err := NewRepoViewID(stackRepo, testGraphID, 7)
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	reader, got, err := ComposeRepoView(base, nil, id)
	if err != nil {
		t.Fatalf("ComposeRepoView: %v", err)
	}
	if reader != graph.Reader(base) {
		t.Fatalf("composing no layers wrapped the base reader")
	}
	if !got.Equal(id) {
		t.Fatalf("identity = %+v, want %+v", got, id)
	}
}
