package indexer

import (
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The delta answers kind scans, the repository edge projection, name batches,
// scoped node reads and adjacency batches through the layers' generation-
// scoped projections instead of loading them. Over a real chain of published
// generations each must equal what the composed view answers.
func TestEditDeltaProjectionsMatchTheComposedView(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	for i, body := range []string{
		"package a\n\nimport \"errors\"\n\nfunc A() error {\n\treturn errors.New(\"bang\")\n}\n\nfunc Size(xs []int) int {\n\treturn len(xs) + 1\n}\n",
		"package a\n\nimport \"errors\"\n\nfunc A() error {\n\treturn errors.New(\"bang\")\n}\n\nfunc Size(xs []int) int {\n\treturn len(xs) + 2\n}\n\nfunc More() int { return Size(nil) }\n",
	} {
		if err := os.WriteFile(filepath.Join(f.worktree, "a", "a.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			// A file only a layer serves.
			if err := os.WriteFile(filepath.Join(f.worktree, "a", "added.go"), []byte("package a\n\nfunc Added() int { return 1 }\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out := coordinatorReconcile(t, c)
		if !out.DirtyBuilt {
			t.Fatalf("edit %d built nothing", i)
		}
		if i == 1 && out.DirtyParentGenerationID == 0 {
			t.Fatalf("the second edit was not chained over the first (generation %d): the view has one layer", out.DirtyGenerationID)
		}
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	dw := graph.NewDeltaWriter(view.Reader, nil)

	renderEdges := func(edges []*graph.Edge) string {
		var rows []string
		for _, e := range edges {
			rows = append(rows, fmt.Sprintf("%s-%s->%s@%s:%d", e.From, e.Kind, e.To, e.FilePath, e.Line))
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeImports, graph.EdgeDefines, graph.EdgeArgOf} {
		var got, want []*graph.Edge
		for e := range dw.EdgesByKind(kind) {
			got = append(got, e)
		}
		for e := range view.Reader.EdgesByKind(kind) {
			want = append(want, e)
		}
		if renderEdges(got) != renderEdges(want) {
			t.Errorf("EdgesByKind(%s):\n delta:\n%s\n view:\n%s", kind, renderEdges(got), renderEdges(want))
		}
	}
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeImports}
	renderRows := func(rows []graph.RepoEdgeRow) string {
		var out []*graph.Edge
		for _, r := range rows {
			out = append(out, r.Edge)
		}
		return renderEdges(out)
	}
	if got, want := renderRows(dw.RepoEdgesByKinds([]string{builderRepoPrefix}, kinds)),
		renderRows(graph.ReadRepoEdgesByKinds(struct{ graph.Store }{graph.NewDeltaWriter(view.Reader, nil)}, []string{builderRepoPrefix}, kinds)); got != want {
		t.Errorf("RepoEdgesByKinds:\n delta:\n%s\n generic:\n%s", got, want)
	}
	var ids []string
	for _, p := range []string{"repo/a/a.go", "repo/b/b.go"} {
		for _, n := range view.Reader.GetFileNodes(p) {
			ids = append(ids, n.ID)
		}
	}
	for _, incoming := range []bool{false, true} {
		var got, want map[string][]*graph.Edge
		if incoming {
			got, want = dw.GetInEdgesByNodeIDs(ids), view.Reader.GetInEdgesByNodeIDs(ids)
		} else {
			got, want = dw.GetOutEdgesByNodeIDs(ids), view.Reader.GetOutEdgesByNodeIDs(ids)
		}
		for _, id := range ids {
			if renderEdges(got[id]) != renderEdges(want[id]) {
				t.Errorf("adjacency (incoming=%t) of %s:\n delta:\n%s\n view:\n%s", incoming, id, renderEdges(got[id]), renderEdges(want[id]))
			}
		}
	}
	names := []string{"A", "Size", "More", "B"}
	gotNames := dw.FindNodesByNames(names)
	for _, name := range names {
		if want := view.Reader.FindNodesByName(name); len(gotNames[name]) != len(want) {
			t.Errorf("FindNodesByNames(%s): delta %d nodes, view %d", name, len(gotNames[name]), len(want))
		}
	}
	var scoped int
	for range dw.NodesInScopeSeq([]string{builderRepoPrefix}, []string{"repo/a/a.go"}, graph.KindFunction) {
		scoped++
	}
	want := 0
	for _, n := range view.Reader.GetFileNodes("repo/a/a.go") {
		if n.Kind == graph.KindFunction {
			want++
		}
	}
	if scoped != want || want == 0 {
		t.Errorf("NodesInScopeSeq: delta %d functions at a/a.go, view %d", scoped, want)
	}
	var gotFiles, wantFiles []string
	for row := range dw.FileNodeIdentitiesSeq([]string{builderRepoPrefix}) {
		gotFiles = append(gotFiles, row.ID+"@"+row.FilePath)
	}
	for n := range view.Reader.NodesByKind(graph.KindFile) {
		if n.RepoPrefix == builderRepoPrefix {
			wantFiles = append(wantFiles, n.ID+"@"+n.FilePath)
		}
	}
	sort.Strings(gotFiles)
	sort.Strings(wantFiles)
	if strings.Join(gotFiles, "\n") != strings.Join(wantFiles, "\n") || !strings.Contains(strings.Join(wantFiles, "\n"), "a/added.go") {
		t.Errorf("FileNodeIdentitiesSeq:\n delta:\n%s\n view:\n%s", strings.Join(gotFiles, "\n"), strings.Join(wantFiles, "\n"))
	}
	inFiles := func(nodes []*graph.Node) string {
		var rows []string
		for _, n := range nodes {
			rows = append(rows, n.ID+"@"+string(n.Kind))
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	var wantInFiles []*graph.Node
	for _, p := range []string{"repo/a/a.go", "repo/a/added.go", "repo/b/b.go"} {
		for _, n := range view.Reader.GetFileNodes(p) {
			if n.Kind == graph.KindFunction || n.Kind == graph.KindFile {
				wantInFiles = append(wantInFiles, n)
			}
		}
	}
	if got, want := inFiles(dw.NodesInFilesByKind([]string{"repo/a/a.go", "repo/a/added.go", "repo/b/b.go"}, []graph.NodeKind{graph.KindFunction, graph.KindFile})), inFiles(wantInFiles); got != want || want == "" {
		t.Errorf("NodesInFilesByKind:\n delta:\n%s\n view:\n%s", got, want)
	}
	// NodesInFilesByKind reads by path: the scoped projection with no
	// repository scope is not keyed by path on a generation handle.
	counted := &scopedReadCounter{Reader: view.Reader}
	if len(graph.NewDeltaWriter(counted, nil).NodesInFilesByKind([]string{"repo/a/a.go"}, []graph.NodeKind{graph.KindFunction})) == 0 {
		t.Error("NodesInFilesByKind over the counting base found no function at a/a.go")
	}
	if counted.scoped != 0 {
		t.Errorf("NodesInFilesByKind read the base's scoped projection %d time(s); it reads by path", counted.scoped)
	}
	if slow := dw.DeltaStats().SlowReads; len(slow) > 0 {
		t.Errorf("reads fell back to scanning the composed view: %v", slow)
	}
}

// scopedReadCounter counts the scoped node projections a base serves.
type scopedReadCounter struct {
	graph.Reader
	scoped int
}

func (c *scopedReadCounter) NodesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	c.scoped++
	return c.Reader.(graph.ScopedProjectionSequencer).NodesInScopeSeq(repoPrefixes, filePaths, kinds...)
}

func (c *scopedReadCounter) NodesLightInScopeSeq(repoPrefixes, filePaths []string) iter.Seq[*graph.Node] {
	c.scoped++
	return c.Reader.(graph.ScopedProjectionSequencer).NodesLightInScopeSeq(repoPrefixes, filePaths)
}

func (c *scopedReadCounter) EdgesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	return c.Reader.(graph.ScopedProjectionSequencer).EdgesInScopeSeq(repoPrefixes, filePaths, kinds...)
}
