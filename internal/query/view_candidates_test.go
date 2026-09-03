package query

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// The view-candidate fixture: one store carrying an indexed corpus and two
// published generations over it, each with its own symbol-FTS rows.
//
// keep.go and edit.go are re-derived by the generations, stay.go is touched by
// neither, gone.go is deleted by the commit generation. Every symbol carries
// the token "zephyr", so one multi-word query — which no exact-name or
// substring lane can answer — reaches every corpus in the stack at once and
// the merge has something to compose.
const (
	viewRepo = "repo"

	viewStayFile  = viewRepo + "/stay.go"
	viewKeepFile  = viewRepo + "/keep.go"
	viewEditFile  = viewRepo + "/edit.go"
	viewGoneFile  = viewRepo + "/gone.go"
	viewAddedFile = viewRepo + "/added.go"

	viewStayerID = viewStayFile + "::Stayer"
	viewKeeperID = viewKeepFile + "::Keeper"
	viewDirtyID  = viewKeepFile + "::Dirty"
	viewOldID    = viewEditFile + "::Old"
	viewNewID    = viewEditFile + "::New"
	viewDoomedID = viewGoneFile + "::Doomed"
	viewFreshID  = viewAddedFile + "::Fresh"

	// viewProseQuery matches through the FTS lane alone: it is not any
	// symbol's name and not a substring of one, so the exact-name splice and
	// the substring fallback both answer nothing and every hit had to come
	// from a corpus.
	viewProseQuery = "zephyr scheduler"
)

func viewSymbolNode(id, name, file string, startLine int) *graph.Node {
	return &graph.Node{
		ID:         id,
		Kind:       graph.KindFunction,
		Name:       name,
		QualName:   "repo." + name,
		FilePath:   file,
		RepoPrefix: viewRepo,
		Language:   "go",
		StartLine:  startLine,
		EndLine:    startLine + 2,
	}
}

func viewFileNode(path string) *graph.Node {
	return &graph.Node{
		ID:         path,
		Kind:       graph.KindFile,
		Name:       filepath.Base(path),
		FilePath:   path,
		RepoPrefix: viewRepo,
		Language:   "go",
		EndLine:    40,
	}
}

// indexViewSymbols writes the FTS rows for one generation's nodes. It runs
// before the generation is published, since a published generation refuses
// every payload write.
func indexViewSymbols(t *testing.T, handle *store_sqlite.Store, tokensByID map[string]string) {
	t.Helper()
	for id, tokens := range tokensByID {
		if err := handle.UpsertSymbolFTS(id, tokens); err != nil {
			t.Fatalf("UpsertSymbolFTS(%s): %v", id, err)
		}
	}
}

// viewStack is the composed view a test searches, plus the pieces it needs to
// address the corpora underneath it.
type viewStack struct {
	store  *store_sqlite.Store
	reader graph.Reader
	layers []ViewLayerSource
	commit int64
	dirty  int64
}

func newViewStack(t *testing.T) *viewStack {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "view.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The indexed corpus.
	store.AddBatch([]*graph.Node{
		viewFileNode(viewStayFile), viewFileNode(viewKeepFile),
		viewFileNode(viewEditFile), viewFileNode(viewGoneFile),
		viewSymbolNode(viewStayerID, "Stayer", viewStayFile, 4),
		viewSymbolNode(viewKeeperID, "Keeper", viewKeepFile, 10),
		viewSymbolNode(viewOldID, "Old", viewEditFile, 20),
		viewSymbolNode(viewDoomedID, "Doomed", viewGoneFile, 6),
	}, nil)
	indexViewSymbols(t, store, map[string]string{
		viewStayerID: "stayer zephyr scheduler",
		viewKeeperID: "keeper zephyr scheduler",
		viewOldID:    "old zephyr scheduler",
		viewDoomedID: "doomed zephyr scheduler",
	})

	stack := &viewStack{store: store}
	stack.commit = writeViewCommit(t, store)
	stack.dirty = writeViewDirty(t, store, stack.commit)

	commitHandle := store.AtGeneration(stack.commit)
	dirtyHandle := store.AtGeneration(stack.dirty)
	commitLayer, err := graphview.NewGenerationLayer(commitHandle)
	if err != nil {
		t.Fatalf("NewGenerationLayer(commit): %v", err)
	}
	dirtyLayer, err := graphview.NewGenerationLayer(dirtyHandle)
	if err != nil {
		t.Fatalf("NewGenerationLayer(dirty): %v", err)
	}
	stack.reader = graph.NewOverlaidViewWithLayer(
		graph.NewOverlaidViewWithLayer(store.AtGeneration(0), commitLayer), dirtyLayer)
	stack.layers = []ViewLayerSource{
		{Search: search.NewSymbolSearcherBackend(commitHandle), Layer: commitLayer},
		{Search: search.NewSymbolSearcherBackend(dirtyHandle), Layer: dirtyLayer},
	}
	return stack
}

// writeViewCommit publishes the commit generation: edit.go re-derived with Old
// renamed to New, added.go new, gone.go deleted.
func writeViewCommit(t *testing.T, store *store_sqlite.Store) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        "graph-view",
		LayerID:        "layer-view-commit",
		CheckoutID:     "wt-view",
		GenerationKind: "commit",
		TreeOID:        "tree-commit",
		CreatedAt:      1000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(commit): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		viewFileNode(viewEditFile), viewFileNode(viewAddedFile),
		viewSymbolNode(viewNewID, "New", viewEditFile, 18),
		viewSymbolNode(viewFreshID, "Fresh", viewAddedFile, 3),
	}, nil)
	indexViewSymbols(t, handle, map[string]string{
		viewNewID:   "new zephyr scheduler",
		viewFreshID: "fresh zephyr scheduler",
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: viewRepo, FilePath: viewEditFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: viewRepo, FilePath: viewAddedFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: viewRepo, FilePath: viewGoneFile, Mode: store_sqlite.OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks(commit): %v", err)
	}
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 2000); err != nil {
		t.Fatalf("PublishPayloadGeneration(commit): %v", err)
	}
	return generationID
}

// writeViewDirty publishes the working-tree generation: keep.go re-derived
// with Keeper moved and Dirty added, and edit.go claimed a second time so the
// same identity is carried by two generations at once.
func writeViewDirty(t *testing.T, store *store_sqlite.Store, base int64) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          "graph-view",
		LayerID:          "layer-view-dirty",
		CheckoutID:       "wt-view",
		GenerationKind:   "dirty",
		BaseGenerationID: base,
		TreeOID:          "tree-dirty",
		CreatedAt:        3000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(dirty): %v", err)
	}
	handle.AddBatch([]*graph.Node{
		viewFileNode(viewKeepFile), viewFileNode(viewEditFile),
		viewSymbolNode(viewKeeperID, "Keeper", viewKeepFile, 70),
		viewSymbolNode(viewDirtyID, "Dirty", viewKeepFile, 80),
		viewSymbolNode(viewNewID, "New", viewEditFile, 90),
	}, nil)
	indexViewSymbols(t, handle, map[string]string{
		viewKeeperID: "keeper zephyr scheduler",
		viewDirtyID:  "dirty zephyr scheduler",
		viewNewID:    "new zephyr scheduler",
	})
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: viewRepo, FilePath: viewKeepFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: viewRepo, FilePath: viewEditFile, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(dirty): %v", err)
	}
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 4000); err != nil {
		t.Fatalf("PublishPayloadGeneration(dirty): %v", err)
	}
	return generationID
}

// baseEngine reads and searches the indexed corpus alone — the request every
// caller made before routed views existed.
func (v *viewStack) baseEngine() *Engine {
	e := NewEngine(v.store)
	e.SetSearch(search.NewSymbolSearcherBackend(v.store))
	e.SetRerank(nil)
	return e
}

// viewEngine reads through the composed stack and enumerates candidates across
// every corpus in it.
func (v *viewStack) viewEngine() *Engine {
	return v.baseEngine().WithViewLayers(v.reader, v.layers)
}

func candidateIDs(cands []*rerank.Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Node.ID)
	}
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func gatherIDs(e *Engine, query string, limit int) []string {
	return candidateIDs(e.GatherSymbolCandidates(query, limit, QueryOptions{}, nil))
}

type rankedMergeFixture struct {
	id     string
	origin string
}

// TestMergeRankedSourcesPreservesRankAcrossUniqueResults pins the distinction
// between duplicate ownership and blanket layer priority. A unique rank-zero
// base result still outranks a unique rank-one overlay result, while the
// overlay copy is the only copy retained for an identity both sources carry.
func TestMergeRankedSourcesPreservesRankAcrossUniqueResults(t *testing.T) {
	base := []rankedMergeFixture{
		{id: "a-base", origin: "base-unique"},
		{id: "m-shared", origin: "base-shared"},
	}
	overlay := []rankedMergeFixture{
		{id: "m-shared", origin: "overlay-shared"},
		{id: "z-overlay", origin: "overlay-unique"},
	}

	got := MergeRankedSources(
		[][]rankedMergeFixture{base, overlay},
		func(item rankedMergeFixture) string { return item.id },
	)
	want := []rankedMergeFixture{
		{id: "a-base", origin: "base-unique"},
		{id: "m-shared", origin: "overlay-shared"},
		{id: "z-overlay", origin: "overlay-unique"},
	}
	if len(got) != len(want) {
		t.Fatalf("merged results = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("merged results = %+v, want %+v", got, want)
		}
	}
}

// TestViewSearchFindsAGenerationOnlySymbol is the defining case: Dirty exists
// in the working-tree generation and nowhere else, and the query names neither
// its identifier nor a substring of one, so only that generation's corpus can
// produce it. The base corpus answers the same query with its own symbols,
// which is what makes the miss invisible without this: the search looks like
// it worked.
func TestViewSearchFindsAGenerationOnlySymbol(t *testing.T) {
	stack := newViewStack(t)

	got := gatherIDs(stack.viewEngine(), viewProseQuery, 20)
	if !containsID(got, viewDirtyID) {
		t.Fatalf("the routed view did not surface the generation-only symbol; got %v", got)
	}
	if !containsID(got, viewFreshID) {
		t.Errorf("the routed view did not surface the commit generation's added symbol; got %v", got)
	}
	if !containsID(got, viewStayerID) {
		t.Errorf("the routed view lost a corpus symbol no generation touched; got %v", got)
	}

	if base := gatherIDs(stack.baseEngine(), viewProseQuery, 20); containsID(base, viewDirtyID) {
		t.Fatalf("the base corpus answered with a generation's symbol; got %v", base)
	}
}

// TestViewSearchHidesADeletedBaseSymbol pins the other direction. Doomed is in
// the indexed corpus and its exact name is the strongest possible hit — the
// store's own exact-name tier scores it above everything — yet the commit
// generation deleted its file, so the view must not answer with it under any
// query.
func TestViewSearchHidesADeletedBaseSymbol(t *testing.T) {
	stack := newViewStack(t)

	if got := gatherIDs(stack.viewEngine(), "Doomed", 20); containsID(got, viewDoomedID) {
		t.Fatalf("the routed view answered with a symbol its generation deleted; got %v", got)
	}
	if got := gatherIDs(stack.viewEngine(), viewProseQuery, 20); containsID(got, viewDoomedID) {
		t.Fatalf("the routed view answered with a deleted symbol on a prose query; got %v", got)
	}
	if got := gatherIDs(stack.baseEngine(), "Doomed", 20); !containsID(got, viewDoomedID) {
		t.Fatalf("the base corpus should still carry the symbol; got %v", got)
	}
}

// TestViewSearchReplacedSymbolReturnsTheLayerPayload: Old is replaced by New in
// edit.go, so the corpus's row is gone from the view and the one that answers
// is the generation's.
func TestViewSearchReplacedSymbolReturnsTheLayerPayload(t *testing.T) {
	stack := newViewStack(t)

	if got := gatherIDs(stack.viewEngine(), "Old", 20); containsID(got, viewOldID) {
		t.Fatalf("the routed view answered with the replaced corpus symbol; got %v", got)
	}
	cands := stack.viewEngine().GatherSymbolCandidates("New", 20, QueryOptions{}, nil)
	found := 0
	for _, c := range cands {
		if c.Node.ID != viewNewID {
			continue
		}
		found++
		if c.Node.StartLine != 90 {
			t.Errorf("New came back at line %d, want the working tree's 90", c.Node.StartLine)
		}
	}
	if found != 1 {
		t.Fatalf("New appeared %d times, want exactly once", found)
	}
}

// TestViewSearchTwoGenerationsReEmitOneID: both generations carry edit.go's New
// and both index it. The candidate set must contain one of it, from the higher
// generation — the lower corpus's hit is masked by the same ownership claim
// that hides its row from the reader.
func TestViewSearchTwoGenerationsReEmitOneID(t *testing.T) {
	stack := newViewStack(t)

	cands := stack.viewEngine().GatherSymbolCandidates(viewProseQuery, 20, QueryOptions{}, nil)
	seen := 0
	for _, c := range cands {
		if c.Node.ID != viewNewID {
			continue
		}
		seen++
		if c.Node.StartLine != 90 {
			t.Errorf("New came back at line %d, want the working tree's 90", c.Node.StartLine)
		}
	}
	if seen != 1 {
		t.Fatalf("New appeared %d times in the merged candidates, want exactly once", seen)
	}
}

// TestViewSearchKeeperComesFromTheWorkingTree: an identity re-emitted by a
// generation is ranked once and carries the generation's payload, not the
// corpus's.
func TestViewSearchKeeperComesFromTheWorkingTree(t *testing.T) {
	stack := newViewStack(t)

	cands := stack.viewEngine().GatherSymbolCandidates("Keeper", 20, QueryOptions{}, nil)
	seen := 0
	for _, c := range cands {
		if c.Node.ID != viewKeeperID {
			continue
		}
		seen++
		if c.Node.StartLine != 70 {
			t.Errorf("Keeper came back at line %d, want the working tree's 70", c.Node.StartLine)
		}
	}
	if seen != 1 {
		t.Fatalf("Keeper appeared %d times, want exactly once", seen)
	}
}

// TestBaseViewCandidatesAreUnchanged is the golden: a request with no stack
// enumerates exactly the corpus's own ranked hits, in the corpus's own order.
// That is what the base path answered before per-view enumeration existed, and
// what it has to keep answering.
func TestBaseViewCandidatesAreUnchanged(t *testing.T) {
	stack := newViewStack(t)

	hits, err := stack.store.SearchSymbols(viewProseQuery, 40)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	want := make([]string, 0, len(hits))
	for _, hit := range hits {
		want = append(want, hit.NodeID)
	}
	got := gatherIDs(stack.baseEngine(), viewProseQuery, 20)
	if len(got) != len(want) {
		t.Fatalf("base candidates = %v, want the corpus's own order %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("base candidates = %v, want the corpus's own order %v", got, want)
		}
	}
}

// TestWithViewLayersEmptyIsTheBasePath: binding an empty stack is the base
// engine, not a composed one — the guard every hot path relies on.
func TestWithViewLayersEmptyIsTheBasePath(t *testing.T) {
	stack := newViewStack(t)
	base := stack.baseEngine()

	empty := base.WithViewLayers(stack.store, nil)
	if empty.viewLayersActive() {
		t.Fatalf("an empty layer slice bound a composed view")
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = base.WithViewLayers(stack.store, nil) }); allocs > 1 {
		t.Errorf("binding an empty stack allocated %v times, want the single WithReader clone", allocs)
	}

	swapped := stack.viewEngine().WithReader(stack.store)
	if swapped.viewLayersActive() {
		t.Fatalf("a reader swap carried the previous view's stack over")
	}
}

// TestComposedViewSkipsTheVectorChannel pins the documented degradation: the
// vector channel is process-wide and cannot answer for a generation, so a
// composed view must not ask it — serving base vectors as the view's would be
// answering about files the view replaced.
func TestComposedViewSkipsTheVectorChannel(t *testing.T) {
	stack := newViewStack(t)
	backend := &countingVectorBackend{inner: search.NewSymbolSearcherBackend(stack.store)}

	base := NewEngine(stack.store)
	base.SetSearch(backend)
	base.SetRerank(nil)

	base.GatherSymbolCandidates(viewProseQuery, 20, QueryOptions{}, nil)
	if backend.vectorCalls == 0 {
		t.Fatalf("the base path never asked the vector channel; the fixture proves nothing")
	}
	before := backend.vectorCalls

	base.WithViewLayers(stack.reader, stack.layers).
		GatherSymbolCandidates(viewProseQuery, 20, QueryOptions{}, nil)
	if backend.vectorCalls != before {
		t.Fatalf("a composed view asked the vector channel %d extra times, want none",
			backend.vectorCalls-before)
	}
}

// countingVectorBackend is the bundle+vector backend the engine's fast path
// detects, counting how often the vector channel is asked.
type countingVectorBackend struct {
	inner       *search.SymbolSearcherBackend
	vectorCalls int
}

func (b *countingVectorBackend) Add(id string, fields ...string) { b.inner.Add(id, fields...) }
func (b *countingVectorBackend) Remove(id string)                { b.inner.Remove(id) }
func (b *countingVectorBackend) Search(q string, limit int) []search.SearchResult {
	return b.inner.Search(q, limit)
}
func (b *countingVectorBackend) Count() int { return b.inner.Count() }
func (b *countingVectorBackend) Close()     {}
func (b *countingVectorBackend) DocCount() (int, bool) {
	return b.inner.DocCount()
}
func (b *countingVectorBackend) SearchSymbolBundles(q string, limit int) []search.SymbolBundle {
	return b.inner.SearchSymbolBundles(q, limit)
}
func (b *countingVectorBackend) VectorChannelOnly(string, int) ([]string, search.ChannelTimings) {
	b.vectorCalls++
	return nil, search.ChannelTimings{}
}
