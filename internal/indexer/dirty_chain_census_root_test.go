package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The working-tree census counts generation 0 only when the view composes over
// it: a chain rooted at a full dedicated generation stands on that generation
// alone (the materializer's base is the root handle), so the flat base's rows
// are not part of the checkout's language surface; a chain rooted at a
// non-dedicated generation composes over generation 0 and counts it.
func TestCheckoutLanguageCensusCountsTheBaseOnlyUnderANonDedicatedRoot(t *testing.T) {
	ctx := context.Background()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "census.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog := store.Catalog()

	nodes := func(language string, n int, prefix string) []*graph.Node {
		var out []*graph.Node
		for i := 0; i < n; i++ {
			file := fmt.Sprintf("repo/%s%d.x", prefix, i)
			out = append(out, &graph.Node{ID: file + "::F", Kind: graph.KindFunction, Name: "F", FilePath: file, RepoPrefix: "repo", Language: language})
		}
		return out
	}
	// Generation 0: a flat base with python the chain never serves.
	if err := store.AddBatchChecked(nodes("python", 20, "flat"), nil); err != nil {
		t.Fatalf("write base: %v", err)
	}
	root := func(kind, owner, graphID string) int64 {
		t.Helper()
		id, err := catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
			OwnerKind: owner, GraphID: graphID, CheckoutID: "c-" + kind, GenerationKind: kind,
			TreeOID: "t", ConfigHash: "h", ExtractorVersions: "{}", ResolverVersion: "r",
			LayerID: "root-" + kind, LowerViewFingerprint: "root-" + kind, State: store_sqlite.ViewGenerationBuilding,
		})
		if err != nil {
			t.Fatalf("create %s root: %v", kind, err)
		}
		if err := store.AtGeneration(id).AddBatchChecked(nodes("go", 5, kind), nil); err != nil {
			t.Fatalf("write %s root: %v", kind, err)
		}
		if err := catalog.PublishViewGeneration(ctx, id, 1); err != nil {
			t.Fatalf("publish %s root: %v", kind, err)
		}
		return id
	}
	dedicated := root(dedicatedGenerationKind, "dedicated_graph", "graph-census")
	commit := root("commit", "checkout", "")

	c := &CheckoutCoordinator{store: store, catalog: catalog, repoPrefix: "repo"}
	if got := c.checkoutLanguageCensus(ctx, dedicated); got["python"] != 0 || got["go"] != 5 {
		t.Fatalf("dedicated root census = %v, want go=5 and no generation-0 python", got)
	}
	if got := c.checkoutLanguageCensus(ctx, commit); got["python"] != 20 || got["go"] != 5 {
		t.Fatalf("non-dedicated root census = %v, want go=5 python=20", got)
	}
}
