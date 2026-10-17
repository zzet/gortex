package resolver

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// ResolveAllContext's workers call cachedFindNodesByNameInRepoForEdge in
// parallel while a page cache is warmed. A name the warm-up did not read is
// answered through the store's scoped name finder and kept for the page; the
// keeping must not write the page cache the other workers read. Under -race
// the old code (writing into nodesByRepoLanguageName) fails here; without
// -race it can die with "concurrent map read and map write", as a private
// daemon once did.
func TestCachedNameMissIsSafeAcrossWorkers(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const names = 64
	var nodes []*graph.Node
	for i := 0; i < names; i++ {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("r/a.go::N%d", i), Kind: graph.KindFunction, Name: fmt.Sprintf("N%d", i),
			FilePath: "r/a.go", RepoPrefix: "r", Language: "go"})
	}
	src := &graph.Node{ID: "r/b.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "r/b.go", RepoPrefix: "r", Language: "go"}
	store.AddBatch(append(nodes, src), nil)

	r := New(store)
	r.nodeByID = map[string]*graph.Node{src.ID: src}
	// A warmed page that read none of the names: every lookup is a miss.
	r.nodesByRepoLanguageName = map[resolverNameLookupScope]map[string][]*graph.Node{}
	scope, _ := r.resolverNameScopeForEdge(&graph.Edge{From: src.ID}, "r")
	r.nodesByRepoLanguageName[scope] = map[string][]*graph.Node{"Warm": nil}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < names; i++ {
				name := fmt.Sprintf("N%d", (i+w*7)%names)
				e := &graph.Edge{From: src.ID, To: "unresolved::" + name, Kind: graph.EdgeCalls}
				hits := r.cachedFindNodesByNameInRepoForEdge(name, "r", e)
				if len(hits) != 1 || hits[0].Name != name {
					t.Errorf("worker %d: %s answered %d hits", w, name, len(hits))
					return
				}
				_ = r.cachedFindNodesByNameInRepoForEdge("Warm", "r", e)
			}
		}(w)
	}
	wg.Wait()
	if got := len(r.namesMiss[scope]); got != names {
		t.Fatalf("kept %d miss answers, want %d", got, names)
	}
}
