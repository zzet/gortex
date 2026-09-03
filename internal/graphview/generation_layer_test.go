package graphview

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/overlaytest"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// openTestStore opens an empty database that lives for one test.
func openTestStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graphview.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// generationLayerBuilder stages the conformance matrix's vocabulary into
// a real payload generation: begin, write the content and the ownership
// masks through the ordinary Store surface, publish, then read it back
// as a layer. Nothing here writes rows the payload lifecycle would not,
// so a layer that only passed because its fixture took a shortcut could
// not pass at all.
//
// Every mask and file row carries an empty repo prefix. The prefix is a
// column of the mask and files tables, and the two only have to agree
// with each other for the publish-time integrity check; the layer keys
// on the repo-prefixed graph path alone.
type generationLayerBuilder struct {
	t *testing.T

	order   []string
	deleted map[string]bool
	nodes   map[string][]*graph.Node
	edges   []*graph.Edge
	removed []string
}

func newGenerationLayerBuilder(t *testing.T) overlaytest.LayerBuilder {
	return &generationLayerBuilder{
		t:       t,
		deleted: make(map[string]bool),
		nodes:   make(map[string][]*graph.Node),
	}
}

// generationLayerFactory hands the conformance matrix a fresh builder,
// each with its own database.
func generationLayerFactory(t *testing.T) overlaytest.LayerFactory {
	return func() overlaytest.LayerBuilder { return newGenerationLayerBuilder(t) }
}

func (b *generationLayerBuilder) MarkFile(graphPath string, deleted bool) {
	if !slices.Contains(b.order, graphPath) {
		b.order = append(b.order, graphPath)
	}
	b.deleted[graphPath] = deleted
}

// AddNode stages one node, replacing an earlier one with the same ID at
// the same path — the idempotence the in-memory builder promises, kept
// here so the write never depends on which conflict rule the batch
// insert happens to apply.
func (b *generationLayerBuilder) AddNode(graphPath string, n *graph.Node) {
	if n == nil {
		return
	}
	if !slices.Contains(b.order, graphPath) {
		b.order = append(b.order, graphPath)
	}
	if b.deleted[graphPath] {
		return
	}
	staged := b.nodes[graphPath]
	for i, existing := range staged {
		if existing.ID == n.ID {
			staged[i] = n
			b.nodes[graphPath] = staged
			return
		}
	}
	b.nodes[graphPath] = append(staged, n)
}

func (b *generationLayerBuilder) AddEdge(e *graph.Edge) {
	if e != nil {
		b.edges = append(b.edges, e)
	}
}

func (b *generationLayerBuilder) MarkRemoved(_, baseID string) {
	if baseID != "" && !slices.Contains(b.removed, baseID) {
		b.removed = append(b.removed, baseID)
	}
}

func (b *generationLayerBuilder) Freeze() graph.OverlayLayerReader {
	b.t.Helper()
	store := openTestStore(b.t)
	_, handle := beginTestGeneration(b.t, store, "layer-conformance")

	var (
		nodes []*graph.Node
		metas []graph.FileMetaRow
		masks []store_sqlite.FileMask
	)
	for _, path := range b.order {
		if b.deleted[path] {
			masks = append(masks, store_sqlite.FileMask{FilePath: path, Mode: store_sqlite.OwnershipDelete})
			continue
		}
		masks = append(masks, store_sqlite.FileMask{FilePath: path, Mode: store_sqlite.OwnershipReplace})
		nodes = append(nodes, b.nodes[path]...)
		metas = append(metas, graph.FileMetaRow{
			FilePath: path, ContentHash: "hash/" + path, Size: 1, NodeCount: len(b.nodes[path]),
		})
	}
	handle.AddBatch(nodes, b.edges)
	if err := handle.SetFileMetas("", metas); err != nil {
		b.t.Fatalf("SetFileMetas: %v", err)
	}
	if err := handle.SetNodeTombstones(b.removed); err != nil {
		b.t.Fatalf("SetNodeTombstones: %v", err)
	}
	if err := handle.SetFileMasks(masks); err != nil {
		b.t.Fatalf("SetFileMasks: %v", err)
	}
	publishTestGeneration(b.t, store, handle.ViewGeneration())

	layer, err := NewGenerationLayer(handle)
	if err != nil {
		b.t.Fatalf("NewGenerationLayer: %v", err)
	}
	return layer
}

// beginTestGeneration opens a building generation and returns its id and
// its write handle.
func beginTestGeneration(t *testing.T, store *store_sqlite.Store, layerID string) (int64, *store_sqlite.Store) {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        testGraphID,
		LayerID:        layerID,
		CheckoutID:     testCheckoutID,
		GenerationKind: "dirty",
		TreeOID:        "tree/" + layerID,
		CreatedAt:      1000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(%s): %v", layerID, err)
	}
	return generationID, handle
}

// publishTestGeneration seals a generation, which runs the same mask
// integrity and producer checks production publishes run.
func publishTestGeneration(t *testing.T, store *store_sqlite.Store, generationID int64) {
	t.Helper()
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 2000); err != nil {
		t.Fatalf("PublishPayloadGeneration(%d): %v", generationID, err)
	}
}

// TestGenerationLayerConformance runs the whole composition matrix
// against a layer whose content lives in a published payload generation.
func TestGenerationLayerConformance(t *testing.T) {
	overlaytest.Run(t, generationLayerFactory(t))
}

// TestGenerationLayerRefusesBaseHandle pins the constructor's guard: the
// base corpus states no ownership claims, so reading it as a layer would
// compose an overlay that covers nothing while still costing every read
// its overlay branches.
func TestGenerationLayerRefusesBaseHandle(t *testing.T) {
	store := openTestStore(t)
	if _, err := NewGenerationLayer(store); err == nil {
		t.Fatal("NewGenerationLayer accepted a base handle")
	}
	if _, err := NewGenerationLayer(nil); err == nil {
		t.Fatal("NewGenerationLayer accepted a nil handle")
	}
}

// TestGenerationLayerEdgeSourceMarker pins the claim only the persisted
// layer can make: a source whose file the generation does not claim, but
// whose outgoing edge set it replaced, owns its adjacency and nothing
// else. The node keeps belonging to the layer below, which is what makes
// the marker cheaper than re-deriving the whole untouched file.
func TestGenerationLayerEdgeSourceMarker(t *testing.T) {
	const (
		editFile = "repo/edit.go"
		depFile  = "repo/dep.go"
		renamed  = editFile + "::Renamed"
		caller   = depFile + "::Caller"
	)
	store := openTestStore(t)
	_, handle := beginTestGeneration(t, store, "layer-marker")
	handle.AddBatch(
		[]*graph.Node{{ID: renamed, Kind: graph.KindFunction, Name: "Renamed", FilePath: editFile}},
		[]*graph.Edge{{From: caller, To: renamed, Kind: graph.EdgeCalls, FilePath: depFile, Line: 9}},
	)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: editFile, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := handle.SetEdgeSourceMasks([]store_sqlite.EdgeSourceMask{
		{SourceID: caller, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetEdgeSourceMasks: %v", err)
	}
	publishTestGeneration(t, store, handle.ViewGeneration())

	layer, err := NewGenerationLayer(handle)
	if err != nil {
		t.Fatalf("NewGenerationLayer: %v", err)
	}
	if layer.HasFile(depFile) {
		t.Fatalf("HasFile(%q) = true, want false — an edge marker claims no file", depFile)
	}
	if layer.CoversNodeID(caller) {
		t.Fatalf("CoversNodeID(%q) = true, want false", caller)
	}
	if layer.OwnsNodeIdentity(caller) {
		t.Fatalf("OwnsNodeIdentity(%q) = true, want false — the node stays below", caller)
	}
	if !layer.OwnsOutEdges(caller) {
		t.Fatalf("OwnsOutEdges(%q) = false, want true", caller)
	}
	if got := len(layer.OutEdges(caller)); got != 1 {
		t.Fatalf("OutEdges(%q) returned %d edges, want the retargeted one", caller, got)
	}
	// The claimed file answers the ordinary way, and claiming it is not
	// the marker's claim: a file mask replaces the edges recorded in that
	// file, which the composition settles per edge against HasFile, so the
	// symbol living there owns its identity and no adjacency beyond it.
	if !layer.OwnsNodeIdentity(renamed) {
		t.Fatalf("the replaced file's symbol lost its identity claim")
	}
	if layer.OwnsOutEdges(renamed) {
		t.Fatalf("OwnsOutEdges(%q) = true — a file mask is not a claim on every edge out of its symbols", renamed)
	}
}

// TestGenerationLayerRemovalsAreIdentities pins the tombstone
// vocabulary: the table records identities, so a name lookup is answered
// from the identity set and the per-name list is that same set. Naming
// an identity a given lookup could not have returned costs a slot in a
// bounded page's compensation and changes no answer.
func TestGenerationLayerRemovalsAreIdentities(t *testing.T) {
	const (
		editFile = "repo/edit.go"
		kept     = editFile + "::Kept"
		goneID   = "repo/other.go::Gone"
	)
	store := openTestStore(t)
	_, handle := beginTestGeneration(t, store, "layer-removals")
	handle.AddBatch([]*graph.Node{
		{ID: kept, Kind: graph.KindFunction, Name: "Kept", FilePath: editFile},
	}, nil)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: editFile, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := handle.SetNodeTombstones([]string{goneID}); err != nil {
		t.Fatalf("SetNodeTombstones: %v", err)
	}
	publishTestGeneration(t, store, handle.ViewGeneration())

	layer, err := NewGenerationLayer(handle)
	if err != nil {
		t.Fatalf("NewGenerationLayer: %v", err)
	}
	if !layer.IsRemovedID(goneID) || !layer.OwnsNodeIdentity(goneID) {
		t.Fatalf("a tombstoned identity is not owned by the layer")
	}
	if !layer.IsNameRemoved("Gone", goneID) || !layer.IsNameRemoved("Whatever", goneID) {
		t.Fatalf("IsNameRemoved consulted the name, which the tombstone table does not record")
	}
	if got := layer.RemovedIDsForName("Gone"); !slices.Equal(got, []string{goneID}) {
		t.Fatalf("RemovedIDsForName = %v, want %v", got, []string{goneID})
	}
	if got := layer.RemovedIDsForName(""); got != nil {
		t.Fatalf("RemovedIDsForName(\"\") = %v, want nil", got)
	}
	var iterated []string
	for id := range layer.RemovedIDs() {
		iterated = append(iterated, id)
	}
	if !slices.Equal(iterated, []string{goneID}) {
		t.Fatalf("RemovedIDs = %v, want %v", iterated, []string{goneID})
	}
}
