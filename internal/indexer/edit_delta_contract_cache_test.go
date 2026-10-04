package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// Only a view whose every generation is immutable has a key: a nil stack (the
// view composes generation zero) or a generation-zero entry has none, and the
// key separates stores, repositories and stacks.
func TestEditDeltaContractCacheKeyNeedsAnImmutableStack(t *testing.T) {
	store := new(int)
	if _, ok := editDeltaContractCacheKey(commitLayerBase{}, store, "repo", "", ""); ok {
		t.Error("a base with no stack (generation zero composed) has a key")
	}
	if _, ok := editDeltaContractCacheKey(commitLayerBase{stack: []int64{0, 4}}, store, "repo", "", ""); ok {
		t.Error("a stack holding generation zero has a key")
	}
	if _, ok := editDeltaContractCacheKey(graph.New(), store, "repo", "", ""); ok {
		t.Error("a base that is not a composed layer view has a key")
	}
	a, ok := editDeltaContractCacheKey(commitLayerBase{stack: []int64{1, 4}}, store, "repo", "", "")
	if !ok {
		t.Fatal("an immutable stack has no key")
	}
	for name, other := range map[string]func() (string, bool){
		"stack": func() (string, bool) {
			return editDeltaContractCacheKey(commitLayerBase{stack: []int64{1, 6}}, store, "repo", "", "")
		},
		"repository": func() (string, bool) {
			return editDeltaContractCacheKey(commitLayerBase{stack: []int64{1, 4}}, store, "other", "", "")
		},
		"store": func() (string, bool) {
			return editDeltaContractCacheKey(commitLayerBase{stack: []int64{1, 4}}, new(int), "repo", "", "")
		},
	} {
		if b, _ := other(); b == a {
			t.Errorf("a different %s has the same key", name)
		}
	}
}

// A hit installs a registry equal to what the delta would have loaded, and
// each delta's copy is its own: an engine editing its registry does not
// change what the next delta starts from.
func TestEditDeltaContractCacheServesTheLoadedRegistry(t *testing.T) {
	resetEditDeltaContractCache()
	t.Cleanup(resetEditDeltaContractCache)
	dir := t.TempDir()
	for i, name := range []string{"DATABASE_URL", "CACHE_URL"} {
		src := fmt.Sprintf("package main\n\nimport \"os\"\n\nfunc load%d() string {\n\treturn os.Getenv(%q)\n}\n", i, name)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("env%d.go", i)), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New()
	indexed := newTestIndexer(g)
	indexed.SetRepoPrefix("repo")
	if _, err := indexed.Index(dir); err != nil {
		t.Fatal(err)
	}
	fresh := func() *Indexer {
		idx := newTestIndexer(g)
		idx.SetRepoPrefix("repo")
		return idx
	}
	render := func(reg *contracts.Registry) string {
		var rows []string
		for _, c := range reg.ByRepo("repo") {
			rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%d|%v", c.ID, c.Role, c.FilePath, c.SymbolID, c.Line, c.Meta))
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	want := render(fresh().ensureIncrementalContractRegistry())
	if want == "" {
		t.Fatal("fixture precondition: the repository holds no contract")
	}

	first := fresh()
	if seedEditDeltaContractRegistry(first, "key") {
		t.Fatal("the first delta hit an empty cache")
	}
	second := fresh()
	if !seedEditDeltaContractRegistry(second, "key") {
		t.Fatal("the second delta over the same stack missed the cache")
	}
	if got := render(second.contractRegistry); got != want {
		t.Fatalf("the cached registry differs from a load:\n cached:\n%s\n loaded:\n%s", got, want)
	}
	if second.contractRegistryLoad != 0 {
		t.Errorf("a hit still read the registry from the graph (%s)", second.contractRegistryLoad)
	}

	// The second delta's engine edits its copy.
	for _, c := range second.contractRegistry.ByRepo("repo") {
		if c.Meta != nil {
			c.Meta["edited"] = true
		}
		second.contractRegistry.ReplaceFile(c.FilePath, nil)
	}
	third := fresh()
	seedEditDeltaContractRegistry(third, "key")
	if got := render(third.contractRegistry); got != want {
		t.Fatalf("an engine's edits reached the cache:\n next delta:\n%s\n loaded:\n%s", got, want)
	}
}

// The build base carries its stack only when the view does not compose the
// mutable generation zero: a dedicated root's view names its generations, a
// view over the shared corpus names none.
func TestAncestryLayerBaseRecordsOnlyAnImmutableStack(t *testing.T) {
	dedicated := newDedicatedDeltaFixture(t)
	root := dedicated.baseClaim.GenerationID
	view, err := dedicated.materializer.MaterializeRefView(context.Background(), dedicated.baseClaim.Desire.Authority.GraphID, root)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if view.ComposesBaseCorpus() {
		t.Fatal("fixture precondition: a dedicated root's view composes generation zero")
	}
	c := &CheckoutCoordinator{store: dedicated.builder.Store}
	base, ok := c.ancestryLayerBase(view).(commitLayerBase)
	if !ok || fmt.Sprint(base.stack) != fmt.Sprint(view.Generations()) || len(base.stack) == 0 {
		t.Fatalf("the dedicated view's base stack = %v, want %v", base.stack, view.Generations())
	}

	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	coordinatorReconcile(t, f.inertCoordinator(t, CheckoutCoordinatorConfig{}))
	shared := chainMaterialize(t, f)
	defer shared.Close()
	if !shared.ComposesBaseCorpus() {
		t.Fatal("fixture precondition: the coordinator fixture's view does not compose generation zero")
	}
	if base := (&CheckoutCoordinator{store: f.store}).ancestryLayerBase(shared).(commitLayerBase); base.stack != nil {
		t.Fatalf("a view composing generation zero carries stack %v", base.stack)
	}
}
