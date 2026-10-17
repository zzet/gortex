package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A Go import binds to one file of the imported package. Which file must not
// depend on the order the store lists the package's files in: every pass
// binds the path-first file.
func TestGoImportBindsThePathFirstFileOfThePackage(t *testing.T) {
	for run := 0; run < 20; run++ {
		g := graph.New()
		nodes := []*graph.Node{
			{ID: "repo/cmd/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "repo/cmd/main.go", Language: "go", RepoPrefix: "repo"},
		}
		// Insert the package's files in a run-dependent order.
		for i := 0; i < 8; i++ {
			name := fmt.Sprintf("f%d.go", (i*3+run)%8)
			path := "repo/internal/graph/" + name
			nodes = append(nodes, &graph.Node{ID: path, Kind: graph.KindFile, Name: name, FilePath: path, Language: "go", RepoPrefix: "repo"})
		}
		import_ := &graph.Edge{From: "repo/cmd/main.go", To: graph.UnresolvedMarker + "import::repo/internal/graph",
			Kind: graph.EdgeImports, FilePath: "repo/cmd/main.go", Line: 3}
		g.AddBatch(nodes, []*graph.Edge{import_})
		New(g).ResolveAll()
		var got string
		for _, e := range g.GetOutEdges("repo/cmd/main.go") {
			if e.Kind == graph.EdgeImports {
				got = e.To
			}
		}
		if got != "repo/internal/graph/f0.go" {
			t.Fatalf("run %d: the import bound to %q, want the path-first file repo/internal/graph/f0.go", run, got)
		}
	}
}
