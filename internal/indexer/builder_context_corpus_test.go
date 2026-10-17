package indexer

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
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
