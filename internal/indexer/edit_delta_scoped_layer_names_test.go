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

// A repository name read through a stack of generation layers is scoped to
// the repository and language set in each layer's own query: it answers what
// the unscoped read filtered in Go answers, and a language the layer holds no
// node of reads no layer row at all.
func TestLayerNameReadsAreScopedInTheLayersQuery(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
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
	commitLayer, err := graphview.NewGenerationLayer(store.AtGeneration(commitGeneration))
	if err != nil {
		t.Fatal(err)
	}
	base := graph.NewOverlaidViewWithLayer(store.AtGeneration(0), commitLayer)
	nameSet := map[string]struct{}{}
	for _, n := range commitLayer.LayerNodesInScope([]string{builderRepoPrefix}, nil, false) {
		if n.Name != "" {
			nameSet[n.Name] = struct{}{}
		}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("fixture precondition: the commit layer names nothing")
	}
	render := func(m map[string][]*graph.Node) string {
		var rows []string
		for name, nodes := range m {
			for _, n := range nodes {
				rows = append(rows, fmt.Sprintf("%s=%s", name, n.ID))
			}
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	read := func(languages []string) (string, int) {
		dw := graph.NewDeltaWriter(base, nil)
		got, err := dw.FindNodesByResolverNameScopes([]graph.ResolverNameScope{{RepoPrefix: builderRepoPrefix, Languages: languages, Names: names}})
		if err != nil {
			t.Fatal(err)
		}
		return render(got[0]), dw.DeltaStats().LayerRowsByRead["repo_names"]
	}
	want := map[string][]*graph.Node{}
	for name, nodes := range graph.NewDeltaWriter(base, nil).FindNodesByNames(names) {
		for _, n := range nodes {
			if n.RepoPrefix == builderRepoPrefix && n.Language == "go" {
				want[name] = append(want[name], n)
			}
		}
	}
	if got, _ := read([]string{"go"}); got != render(want) || got == "" {
		t.Fatalf("the scoped read answered\n%s\nwant\n%s", got, render(want))
	}
	if got, rows := read([]string{"cobol"}); got != "" || rows != 0 {
		t.Fatalf("a language the layer holds nothing of answered %q and read %d layer rows", got, rows)
	}
}
