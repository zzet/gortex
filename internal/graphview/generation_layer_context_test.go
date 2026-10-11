package graphview

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const (
	ctxChangedFile = "repo/changed.go"
	ctxContextFile = "repo/dep.go"
	ctxChangedID   = ctxChangedFile + "::Changed"
	ctxContextID   = ctxContextFile + "::Helper"
)

// TestGenerationLayerContextPathIsNotClaimed pins the whole ownership answer
// for a context mask: the generation read the path and claims nothing, so
// every claim probe says "the layer below", and the path is reported on the
// context surface instead of the claimed one.
func TestGenerationLayerContextPathIsNotClaimed(t *testing.T) {
	store := openTestStore(t)
	_, handle := beginTestGeneration(t, store, "layer-context")
	handle.AddBatch([]*graph.Node{
		{ID: ctxChangedID, Kind: graph.KindFunction, Name: "Changed", FilePath: ctxChangedFile},
	}, []*graph.Edge{
		{From: ctxChangedID, To: ctxContextID, Kind: graph.EdgeCalls, FilePath: ctxChangedFile, Line: 4},
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: ctxChangedFile, Mode: store_sqlite.OwnershipReplace},
		{FilePath: ctxContextFile, Mode: store_sqlite.OwnershipContext},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	publishTestGeneration(t, store, handle.ViewGeneration())

	layer, err := NewGenerationLayer(handle)
	if err != nil {
		t.Fatalf("NewGenerationLayer: %v", err)
	}
	if layer.HasFile(ctxContextFile) {
		t.Fatalf("HasFile(%q) = true — a context mask claims nothing", ctxContextFile)
	}
	if layer.IsTombstone(ctxContextFile) {
		t.Fatalf("IsTombstone(%q) = true — a context mask is not a deletion", ctxContextFile)
	}
	if got := layer.FilePaths(); !slices.Equal(got, []string{ctxChangedFile}) {
		t.Fatalf("FilePaths = %v, want only the claimed path", got)
	}
	if got := layer.ContextPaths(); !slices.Equal(got, []string{ctxContextFile}) {
		t.Fatalf("ContextPaths = %v, want %v", got, []string{ctxContextFile})
	}
	if layer.CoversNodeID(ctxContextID) || layer.OwnsNodeIdentity(ctxContextID) {
		t.Fatalf("the layer claims an identity at a context path")
	}
	if layer.OwnsOutEdges(ctxContextID) {
		t.Fatalf("the layer claims adjacency at a context path")
	}
	if got := layer.FileNodes(ctxContextFile); got != nil {
		t.Fatalf("FileNodes(%q) = %v, want nil", ctxContextFile, got)
	}
	// The claimed half is untouched by the new mode.
	if !layer.HasFile(ctxChangedFile) || !layer.OwnsNodeIdentity(ctxChangedID) {
		t.Fatalf("the changed file lost its claim")
	}
}

// TestComposedViewServesAContextPathFromBelow is the composition half: with
// the context path unclaimed, the base corpus keeps answering for the file
// whole — its node, its location and the edges recorded in it — while the
// generation's own changed file replaces only itself.
func TestComposedViewServesAContextPathFromBelow(t *testing.T) {
	store := openTestStore(t)
	base := []*graph.Node{
		{ID: ctxChangedID, Kind: graph.KindFunction, Name: "Changed", FilePath: ctxChangedFile, StartLine: 1},
		{ID: ctxContextID, Kind: graph.KindFunction, Name: "Helper", FilePath: ctxContextFile, StartLine: 7},
	}
	store.AddBatch(base, []*graph.Edge{
		{From: ctxContextID, To: ctxChangedID, Kind: graph.EdgeCalls, FilePath: ctxContextFile, Line: 8},
	})

	_, handle := beginTestGeneration(t, store, "layer-context-compose")
	handle.AddBatch([]*graph.Node{
		{ID: ctxChangedID, Kind: graph.KindFunction, Name: "Changed", FilePath: ctxChangedFile, StartLine: 2},
	}, nil)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: ctxChangedFile, Mode: store_sqlite.OwnershipReplace},
		{FilePath: ctxContextFile, Mode: store_sqlite.OwnershipContext},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	publishTestGeneration(t, store, handle.ViewGeneration())

	layer, err := NewGenerationLayer(handle)
	if err != nil {
		t.Fatalf("NewGenerationLayer: %v", err)
	}
	view := graph.NewOverlaidViewWithLayer(store, layer)

	helper := view.GetNode(ctxContextID)
	if helper == nil || helper.StartLine != 7 {
		t.Fatalf("composed GetNode(%q) = %+v, want the base row at line 7", ctxContextID, helper)
	}
	changed := view.GetNode(ctxChangedID)
	if changed == nil || changed.StartLine != 2 {
		t.Fatalf("composed GetNode(%q) = %+v, want the generation's row at line 2", ctxChangedID, changed)
	}
	if got := len(view.GetFileNodes(ctxContextFile)); got != 1 {
		t.Fatalf("composed GetFileNodes(%q) returned %d nodes, want the base's one", ctxContextFile, got)
	}
	if got := view.GetOutEdges(ctxContextID); len(got) != 1 || got[0].To != ctxChangedID {
		t.Fatalf("composed GetOutEdges(%q) = %+v, want the base's call edge", ctxContextID, got)
	}
	if got := view.GetInEdges(ctxChangedID); len(got) != 1 {
		t.Fatalf("composed GetInEdges(%q) returned %d edges, want the context file's call", ctxChangedID, len(got))
	}
	// No duplicate: the whole-graph walk sees one row per identity.
	seen := map[string]int{}
	for _, node := range view.AllNodes() {
		seen[node.ID]++
	}
	if seen[ctxContextID] != 1 || seen[ctxChangedID] != 1 {
		t.Fatalf("composed AllNodes duplicated an identity: %v", seen)
	}
}

// TestGenerationLayerNeverServesCarriedContextRows is the belt to publish
// validation's braces. Even when a generation physically carries rows at a
// context path — the shape the store refuses at publish — the layer answers
// as if it did not, so a carried row can never shadow or duplicate the layer
// below through any reader.
func TestGenerationLayerNeverServesCarriedContextRows(t *testing.T) {
	store := openTestStore(t)
	// The generation is deliberately NOT published: this fixture is the
	// contradiction publish refuses, staged so the layer's own defence is
	// exercised rather than assumed.
	_, handle := beginTestGeneration(t, store, "layer-context-carried")
	handle.AddBatch([]*graph.Node{
		{ID: ctxChangedID, Kind: graph.KindFunction, Name: "Changed", FilePath: ctxChangedFile},
		{ID: ctxContextID, Kind: graph.KindFunction, Name: "Helper", FilePath: ctxContextFile,
			QualName: "repo.dep.Helper"},
	}, []*graph.Edge{
		{From: ctxContextID, To: ctxChangedID, Kind: graph.EdgeCalls, FilePath: ctxContextFile, Line: 8},
		{From: ctxChangedID, To: ctxContextID, Kind: graph.EdgeCalls, FilePath: ctxChangedFile, Line: 4},
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: ctxChangedFile, Mode: store_sqlite.OwnershipReplace},
		{FilePath: ctxContextFile, Mode: store_sqlite.OwnershipContext},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}

	layer, err := NewGenerationLayer(handle)
	if err != nil {
		t.Fatalf("NewGenerationLayer: %v", err)
	}
	if got := layer.NodeByID(ctxContextID); got != nil {
		t.Fatalf("NodeByID served a carried context row: %+v", got)
	}
	if got := layer.NodeByQualName("repo.dep.Helper"); got != nil {
		t.Fatalf("NodeByQualName served a carried context row: %+v", got)
	}
	if got := layer.NodesByName("Helper"); len(got) != 0 {
		t.Fatalf("NodesByName served %d carried context rows", len(got))
	}
	if got := layer.FileNodes(ctxContextFile); len(got) != 0 {
		t.Fatalf("FileNodes served %d carried context rows", len(got))
	}
	if got := layer.OutEdges(ctxContextID); len(got) != 0 {
		t.Fatalf("OutEdges served %d edges recorded at a context path", len(got))
	}
	var walked []string
	for node := range layer.Nodes() {
		walked = append(walked, node.ID)
	}
	if !slices.Equal(walked, []string{ctxChangedID}) {
		t.Fatalf("Nodes walked %v, want only the claimed file's row", walked)
	}
	for name := range layer.NamedNodes() {
		if name == "Helper" {
			t.Fatalf("NamedNodes indexed a carried context row")
		}
	}
	edges := 0
	for edge := range layer.Edges() {
		if edge.FilePath == ctxContextFile {
			t.Fatalf("Edges walked an edge recorded at a context path: %+v", edge)
		}
		edges++
	}
	if edges != 1 {
		t.Fatalf("Edges walked %d edges, want the claimed file's one", edges)
	}
	// The edge the CHANGED file recorded into the context symbol is the
	// generation's own payload and must survive the filter: it is the
	// re-derived call whose target only the layer below carries.
	if got := layer.InEdges(ctxContextID); len(got) != 1 || got[0].FilePath != ctxChangedFile {
		t.Fatalf("InEdges(%q) = %+v, want the changed file's call", ctxContextID, got)
	}
}

// seedRawContextMaskRow writes a file-mask row straight into the database,
// bypassing SetFileMasks — which refuses any mode outside the vocabulary. It
// is the only way to stage the row a FORWARD-dated binary could leave behind:
// a mode this reader has never heard of, on a generation this reader must
// then decide how to compose.
func seedRawContextMaskRow(t *testing.T, dbPath string, viewGen int64, repoPrefix, filePath, mode string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`INSERT OR REPLACE INTO generation_file_masks
  (view_gen, repo_prefix, file_path, ownership_mode) VALUES (?, ?, ?, ?)`,
		viewGen, repoPrefix, filePath, mode); err != nil {
		t.Fatalf("seed raw mask row: %v", err)
	}
}

// TestGenerationLayerRefusesAnUnknownOwnershipMode pins the constructor's
// fail-closed arm, which is the production-reachable half of the downgrade
// story: a generation written by a binary that knows a fourth ownership mode
// cannot be composed by this one, because every arm below would have to guess
// whether the unknown mode hides the layer beneath. Refusing to build the
// layer at all is the only answer that cannot serve a wrong graph.
func TestGenerationLayerRefusesAnUnknownOwnershipMode(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "unknown-mode.sqlite")
	store, err := store_sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, handle := beginTestGeneration(t, store, "layer-unknown-mode")
	handle.AddBatch([]*graph.Node{
		{ID: ctxChangedID, Kind: graph.KindFunction, Name: "Changed", FilePath: ctxChangedFile},
	}, nil)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: ctxChangedFile, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	// The public writer refuses the mode outright, which is why the row has to
	// be seeded raw.
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: ctxContextFile, Mode: store_sqlite.OwnershipMode("shadow")},
	}); err == nil {
		t.Fatalf("SetFileMasks accepted an unknown ownership mode")
	}

	// Sanity: the layer builds while the vocabulary is respected.
	if _, err := NewGenerationLayer(handle); err != nil {
		t.Fatalf("NewGenerationLayer before the forward-dated row: %v", err)
	}

	seedRawContextMaskRow(t, dbPath, handle.ViewGeneration(), "", ctxContextFile, "shadow")

	layer, err := NewGenerationLayer(handle)
	if err == nil {
		t.Fatalf("NewGenerationLayer composed an unknown ownership mode: %+v", layer)
	}
	if !strings.Contains(err.Error(), "unknown ownership mode") ||
		!strings.Contains(err.Error(), ctxContextFile) {
		t.Fatalf("refusal does not name the mode and the path: %v", err)
	}
}

// TestGenerationLayerKeepsCarriedContextRowsOutOfDetachedIdentity covers the
// three readers the serve filter cannot reach: DetachedNodeSummaries,
// DetachedFileNodes / DetachedRepoNodes and the localization projections all
// read the captured detached set directly, so a carried context row admitted
// into it at construction would be served from there whatever the row filter
// says. The constructor therefore skips a context path exactly as it skips a
// claimed one.
func TestGenerationLayerKeepsCarriedContextRowsOutOfDetachedIdentity(t *testing.T) {
	store := openTestStore(t)
	// Deliberately unpublished: this is the contradiction the mask-integrity
	// oracle refuses, staged so the layer's own defence is exercised.
	_, handle := beginTestGeneration(t, store, "layer-context-detached")
	handle.AddBatch([]*graph.Node{
		{ID: ctxChangedID, Kind: graph.KindFunction, Name: "Changed", FilePath: ctxChangedFile},
		{ID: ctxContextID, Kind: graph.KindFunction, Name: "Helper", FilePath: ctxContextFile},
	}, nil)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: ctxChangedFile, Mode: store_sqlite.OwnershipReplace},
		{FilePath: ctxContextFile, Mode: store_sqlite.OwnershipContext},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	// An explicit identity claim on the carried context row: without the
	// constructor's context skip this is precisely what turns it into a
	// detached carried row.
	if err := handle.SetNodeIdentityReplacements([]string{ctxContextID}); err != nil {
		t.Fatalf("SetNodeIdentityReplacements: %v", err)
	}

	layer, err := NewGenerationLayer(handle)
	if err != nil {
		t.Fatalf("NewGenerationLayer: %v", err)
	}
	for node := range layer.DetachedNodeSummaries() {
		if node != nil && node.FilePath == ctxContextFile {
			t.Fatalf("DetachedNodeSummaries carried a context row: %+v", node)
		}
	}
	if got := layer.DetachedFileNodes(ctxContextFile); len(got) != 0 {
		t.Fatalf("DetachedFileNodes(%q) carried %d context rows", ctxContextFile, len(got))
	}
	if got := layer.DetachedRepoNodes(""); len(got) != 0 {
		t.Fatalf("DetachedRepoNodes carried %d context rows", len(got))
	}
	// The identity projections read the same captured set.
	if layer.OwnsNodeIdentity(ctxContextID) {
		t.Fatalf("the layer claims the identity of a carried context row")
	}
}
