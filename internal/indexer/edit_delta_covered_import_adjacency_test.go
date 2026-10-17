package indexer

import (
	"fmt"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// endpointReadCounter is the store with its edge-endpoint reads counted.
type endpointReadCounter struct {
	*store_sqlite.Store
	reads int
}

func (c *endpointReadCounter) EdgeEndpointsFrom(ids []string, kinds []graph.EdgeKind) []graph.EdgeEndpointRow {
	c.reads++
	return c.Store.EdgeEndpointsFrom(ids, kinds)
}

// A path the delta's own layer covers — the changed file — answers its
// import adjacency from the delta's own rows, equal to the composition's
// rows, without reading the stack below for it: covering masks every row
// recorded there below, and import edges are recorded at their source's file.
func TestEditDeltaImportAdjacencyOfACoveredPathComesFromTheDelta(t *testing.T) {
	const module = "example.com/imports"
	repo := builderTempDir(t, "repo")
	builderWriteTree(t, repo, map[string]string{
		"go.mod": "module " + module + "\n\ngo 1.22\n",
		"a/a.go": "package a\n\nfunc A() int { return 1 }\n",
		"b/b.go": "package b\n\nimport \"" + module + "/a\"\n\nfunc B() int { return a.A() }\n",
		"c/c.go": "package c\n\nimport (\n\t\"" + module + "/a\"\n\t\"" + module + "/b\"\n)\n\nfunc C() int { return a.A() + b.B() }\n",
	})
	store := builderOpenStore(t, "covered-imports")
	builderIndex(t, store, repo)
	const c = "repo/c/c.go"
	paths := []string{"repo/a/a.go", "repo/b/b.go", c}

	// The file's rows as the store holds them, to restate with one import
	// fewer (a save that drops an import).
	plain := graph.NewDeltaWriter(store, nil)
	nodes := plain.GetFileNodes(c)
	var ids []string
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	var edges []*graph.Edge
	dropped := false
	for _, out := range plain.GetOutEdgesByNodeIDs(ids) {
		for _, e := range out {
			if e.FilePath != c {
				continue
			}
			if e.Kind == graph.EdgeImports && !dropped {
				dropped = true
				continue
			}
			edges = append(edges, e)
		}
	}
	if !dropped {
		t.Fatal("fixture precondition: c.go has no import")
	}

	below := &endpointReadCounter{Store: store}
	dw := graph.NewDeltaWriter(below, nil)
	dw.SetBaseProjectionCache(graph.NewBaseProjectionCache())
	dw.EvictFiles([]string{c})
	dw.AddBatch(nodes, edges)

	reference := func() map[string][]string {
		out := make(map[string][]string)
		for _, p := range paths {
			var ids []string
			for _, n := range dw.GetFileNodes(p) {
				ids = append(ids, n.ID)
			}
			for _, es := range dw.GetOutEdgesByNodeIDs(ids) {
				for _, e := range es {
					if e.Kind == graph.EdgeImports {
						out[p] = append(out[p], e.To)
					}
				}
			}
			sort.Strings(out[p])
		}
		return out
	}
	want := reference()
	if len(want[c]) != 1 {
		t.Fatalf("fixture precondition: the restated c.go imports %v, want one", want[c])
	}
	before := below.reads
	got, complete := dw.ProjectImportAdjacency([]string{c})
	if !complete {
		t.Fatal("the projection refused a canonical request")
	}
	if below.reads != before {
		t.Fatalf("the covered path read the store's edge endpoints %d times", below.reads-before)
	}
	sort.Strings(got[c])
	if fmt.Sprint(got[c]) != fmt.Sprint(want[c]) {
		t.Fatalf("covered c.go imports %v through the projection, %v through the rows", got[c], want[c])
	}
	// Mixed with stable paths the answer still equals the rows.
	all, _ := dw.ProjectImportAdjacency(paths)
	for _, p := range paths {
		sort.Strings(all[p])
		if fmt.Sprint(all[p]) != fmt.Sprint(want[p]) {
			t.Errorf("%s imports %v through the projection, %v through the rows", p, all[p], want[p])
		}
	}
}
