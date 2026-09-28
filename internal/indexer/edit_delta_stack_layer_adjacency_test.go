package indexer

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// A layer below the delta is read by identity once per stack: two deltas over
// the same commit-layer stack sharing its projection cache answer the same
// adjacency, and the second reads no row from the layer.
func TestALayerBelowTheDeltaIsReadOncePerStack(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	repo := propNewRepo(t, rng)
	baseTree := builderGit(t, repo.dir, "rev-parse", "HEAD^{tree}")
	store := builderOpenStore(t, "base")
	propIndex(t, store, repo.dir)
	propApplyScript(t, repo, rng)
	builderGit(t, repo.dir, "add", "-A")
	builderGit(t, repo.dir, "commit", "-m", "B")
	targetTree := builderGit(t, repo.dir, "rev-parse", "HEAD^{tree}")
	commitOID := builderGit(t, repo.dir, "rev-parse", "HEAD")
	commitGeneration, _ := propBuildCommitLayer(t, store, repo, baseTree, targetTree, commitOID)
	corpus := store.AtGeneration(0)
	commitLayer, err := graphview.NewGenerationLayer(store.AtGeneration(commitGeneration))
	if err != nil {
		t.Fatalf("NewGenerationLayer: %v", err)
	}
	base := graph.NewOverlaidViewWithLayer(corpus, commitLayer)
	var ids []string
	for _, n := range commitLayer.LayerNodesInScope([]string{builderRepoPrefix}, nil, false) {
		ids = append(ids, n.ID)
	}
	if len(ids) == 0 {
		t.Fatal("fixture precondition: the commit layer holds no node")
	}
	render := func(in, out map[string][]*graph.Edge) string {
		var rows []string
		for dir, m := range map[string]map[string][]*graph.Edge{"in": in, "out": out} {
			for id, edges := range m {
				for _, e := range edges {
					rows = append(rows, fmt.Sprintf("%s %s %s-%s->%s@%d", dir, id, e.From, e.Kind, e.To, e.Line))
				}
			}
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	plain := graph.NewDeltaWriter(base, nil)
	want := render(plain.GetInEdgesByNodeIDs(ids), plain.GetOutEdgesByNodeIDs(ids))
	cache := graph.NewBaseProjectionCache()
	var layerRows []int
	for delta := 0; delta < 2; delta++ {
		dw := graph.NewDeltaWriter(base, nil)
		dw.SetBaseProjectionCache(cache)
		if got := render(dw.GetInEdgesByNodeIDs(ids), dw.GetOutEdgesByNodeIDs(ids)); got != want {
			t.Fatalf("delta %d answered\n%s\nwant\n%s", delta, got, want)
		}
		by := dw.DeltaStats().LayerRowsByRead
		layerRows = append(layerRows, by["in_edges_by_ids"]+by["out_edges_by_ids"])
	}
	if layerRows[0] == 0 {
		t.Fatal("fixture precondition: the first delta read no row from the layer")
	}
	if layerRows[1] != 0 {
		t.Fatalf("the second delta over the stack read %d layer rows again", layerRows[1])
	}
}
