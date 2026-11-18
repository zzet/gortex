package graphview

import (
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A generation that only masks (no node or edge rows of its own, like the
// live chain's clean commit generation) answers its row reads without SQL,
// while its masks still hide base rows through the composed view: the
// three-level chain's projections equal its full-row readers, and the
// masks-only level's tombstone is honoured.
func TestMasksOnlyGenerationLayerSkipsRowReadsAndKeepsItsMasks(t *testing.T) {
	store := openTestStore(t)
	epBase(t, store)
	first := epFirstGeneration(t, store)
	_, handle := beginTestGeneration(t, store, "masks-only")
	if err := handle.SetFileMasks([]store_sqlite.FileMask{{FilePath: epDeleted, Mode: store_sqlite.OwnershipDelete}}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := handle.SetNodeTombstones([]string{epUntouched + "::A"}); err != nil {
		t.Fatalf("SetNodeTombstones: %v", err)
	}
	publishTestGeneration(t, store, handle.ViewGeneration())

	firstLayer, err := NewGenerationLayer(first)
	if err != nil {
		t.Fatalf("NewGenerationLayer(first): %v", err)
	}
	empty, err := NewGenerationLayer(handle)
	if err != nil {
		t.Fatalf("NewGenerationLayer(masks-only): %v", err)
	}
	if nodes, edges := firstLayer.payloadPresence(); !nodes || !edges {
		t.Fatalf("a generation with rows reported nodes=%v edges=%v", nodes, edges)
	}
	if nodes, edges := empty.payloadPresence(); nodes || edges {
		t.Fatalf("the masks-only generation reported nodes=%v edges=%v", nodes, edges)
	}
	chain := graph.NewOverlaidViewWithLayer(graph.NewOverlaidViewWithLayer(store, firstLayer), empty)
	epAssertIdentity(t, "masks-only chain", chain)

	proj, _ := graph.EdgeEndpointsOf(chain)
	rows := epRender(proj.EdgeEndpointsRecordedAt([]string{epUntouched, epContext, epSource}))
	for _, row := range rows {
		if slices.Contains([]string{epContext + "::Helper -calls-> " + epUntouched + "::A @" + epContext}, row) {
			t.Errorf("the masks-only level's tombstone leaked: %s", row)
		}
	}
	if chain.GetNode(epUntouched+"::A") != nil {
		t.Error("the masks-only level's tombstoned node is still served")
	}
	if len(empty.OutEdges(epUntouched+"::A")) != 0 || empty.NodeByID(epReplaced+"::Keep") != nil {
		t.Error("an empty layer answered rows")
	}
}
