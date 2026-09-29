package indexer

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// bareContextCorpus forwards contextCorpus and nothing else: every optional
// capability a wrapper can drop is missing.
type bareContextCorpus struct{ contextCorpus }

// TestContextCorpusCapabilitiesAreRequired pins each capability check: the
// in-memory graph and the store satisfy all of them, a corpus that forwards
// only the core surface lacks every one, and the withhold step refuses it
// instead of skipping cleanups.
func TestContextCorpusCapabilitiesAreRequired(t *testing.T) {
	if missing := missingContextCorpusCapabilities(graph.New()); len(missing) > 0 {
		t.Errorf("the in-memory corpus lacks %v", missing)
	}
	store := builderOpenStore(t, "corpus-capabilities")
	if missing := missingContextCorpusCapabilities(store); len(missing) > 0 {
		t.Errorf("the store corpus lacks %v", missing)
	}
	bare := bareContextCorpus{contextCorpus: store}
	want := []string{
		"BatchDeleteSymbolFTS", "DeleteCloneShingles", "DeleteConstantValuesByFiles",
		"EvictContractNodesByIDs", "EvictPathlessNodesByIDs", "RemoveEdgesExact",
	}
	if got := missingContextCorpusCapabilities(bare); !slices.Equal(got, want) {
		t.Errorf("a bare corpus is missing %v, want every capability %v", got, want)
	}
	b := builderNewBuilder(store)
	_, err := b.withholdContextPayload(context.Background(), BuildRequest{RepoPrefix: builderRepoPrefix, Base: store}, buildPlan{}, bare)
	if err == nil || !strings.Contains(err.Error(), "lacks") {
		t.Errorf("withholding from a bare corpus returned %v, want a refusal naming what it lacks", err)
	}
	if err := purgeIdentitySidecars(bare, []string{"x"}); err == nil {
		t.Error("purging identity rows from a bare corpus succeeded")
	}
}

// layerBaseWithoutRecordedEdges is a layer below that cannot serve edges by
// recording file.
type layerBaseWithoutRecordedEdges struct{ LayerBase }

// TestRedundantPathlessPruneRefusesABaseWithoutRecordedEdges: a stub copy with
// an edge recorded at no file can only be proven redundant against the layer
// below's rows by recording file; without that reader the prune refuses
// rather than keep every stub (and hide its adjacency) silently.
func TestRedundantPathlessPruneRefusesABaseWithoutRecordedEdges(t *testing.T) {
	store := builderOpenStore(t, "prune-capability")
	stub := &graph.Node{ID: "repo::stdlib::fmt::Errorf", Kind: graph.KindFunction, Name: "Errorf", Language: "go", RepoPrefix: "repo"}
	module := &graph.Node{ID: "repo::module::go:fmt", Kind: graph.KindModule, Name: "fmt", Language: "go", RepoPrefix: "repo"}
	member := &graph.Edge{From: stub.ID, To: module.ID, Kind: graph.EdgeMemberOf}
	store.AddBatch([]*graph.Node{stub, module}, []*graph.Edge{member})
	corpus := graph.New()
	corpus.AddBatch([]*graph.Node{stub, module}, []*graph.Edge{member})

	if _, _, _, err := pruneRedundantPathless(corpus, layerBaseWithoutRecordedEdges{LayerBase: store}); err == nil {
		t.Fatal("the prune accepted a layer below that serves no edges by recording file")
	}
	if _, _, pruned, err := pruneRedundantPathless(corpus, store); err != nil || len(pruned) != 2 {
		t.Fatalf("against the store the prune returned %v, %v; want both identical stubs pruned", pruned, err)
	}
	if _, _, _, err := pruneRedundantPathless(graph.New(), nil); err == nil {
		t.Fatal("the prune accepted no layer below")
	}
}

// seededContentFixture is a declared path whose layer-below payload carries a
// content section, and a pass corpus holding a subset of it (as a name-scoped
// seed would).
func seededContentFixture(t *testing.T) (*graph.Graph, BuildRequest, buildPlan) {
	t.Helper()
	store := builderOpenStore(t, "seeded-content")
	doc := builderRepoPrefix + "/notes.md"
	section := &graph.Node{ID: doc + "::intro", Kind: graph.KindDoc, Name: "intro", FilePath: doc,
		Meta: map[string]any{"data_class": "content"}}
	other := &graph.Node{ID: doc + "::usage", Kind: graph.KindDoc, Name: "usage", FilePath: doc,
		Meta: map[string]any{"data_class": "content"}}
	file := &graph.Node{ID: doc, Kind: graph.KindFile, Name: "notes.md", FilePath: doc}
	store.AddBatch([]*graph.Node{file, section, other}, nil)
	corpus := graph.New()
	corpus.AddBatch([]*graph.Node{file, section}, nil) // the seeded subset
	req := BuildRequest{RepoPrefix: builderRepoPrefix, Base: store}
	plan := buildPlan{
		indexed:  []string{"main.go", "notes.md"},
		context:  []string{"notes.md"},
		declared: map[string]struct{}{"notes.md": {}},
		seeded:   []string{"notes.md"},
	}
	return corpus, req, plan
}

// TestSeededDeclaredPathIsNeverKeptPartially: a seeded declared path that the
// withholding cannot drop (here, it carries a content body) must not be kept
// as its seeded subset under a replace mask — that would hide the rest of the
// file's payload below. The withhold step refuses the build instead.
func TestSeededDeclaredPathIsNeverKeptPartially(t *testing.T) {
	corpus, req, plan := seededContentFixture(t)
	b := builderNewBuilder(req.Base.(*store_sqlite.Store))
	separation, err := b.withholdContextPayload(context.Background(), req, plan, corpus)
	if err == nil {
		t.Fatalf("a seeded declared path with a content body was accepted: withheld %v, retained %v",
			separation.withheldPaths, separation.retainedPaths)
	}
	if !strings.Contains(err.Error(), "partial payload") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

// TestContentBearingDeclaredPathIsParsedNotSeeded: the seed decision leaves a
// declared path whose layer-below payload carries a content section to the
// parser, so a kept path is always re-derived in full.
func TestContentBearingDeclaredPathIsParsedNotSeeded(t *testing.T) {
	_, req, plan := seededContentFixture(t)
	plan.seeded = nil
	plan.declared["util.go"] = struct{}{}
	seeded, base := decideDeclaredSeed(req, plan)
	if slices.Contains(seeded, "notes.md") {
		t.Errorf("the content-bearing declared path was seeded: %v", seeded)
	}
	if !slices.Contains(seeded, "util.go") {
		t.Errorf("an ordinary declared path was not seeded: %v", seeded)
	}
	if base == nil {
		t.Error("the decision read no layer-below nodes to reuse")
	}
}

// TestSeededPayloadNeverSurvivesTheSeparation pins the runtime guard behind
// the invariant "seeded payload never reaches the generation": whatever route
// left a node at a seeded path in the corpus, the separation refuses the
// build instead of writing it.
func TestSeededPayloadNeverSurvivesTheSeparation(t *testing.T) {
	store := builderOpenStore(t, "seeded-guard")
	leftover := &graph.Node{ID: builderRepoPrefix + "/util.go::Helper", Kind: graph.KindFunction, Name: "Helper",
		FilePath: builderRepoPrefix + "/util.go", Language: "go"}
	corpus := graph.New()
	corpus.AddBatch([]*graph.Node{leftover}, nil)
	req := BuildRequest{RepoPrefix: builderRepoPrefix, Base: store}
	plan := buildPlan{indexed: []string{"main.go"}, seeded: []string{"util.go"}}
	_, err := builderNewBuilder(store).separateAndPrune(context.Background(), req, plan, corpus)
	if err == nil || !strings.Contains(err.Error(), "survived the withholding") {
		t.Fatalf("a node left at a seeded path was accepted: %v", err)
	}
}
