package resolver

import (
	"iter"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// reExportScanCounter records the files each re-export scan is asked for.
type reExportScanCounter struct {
	*graph.Graph
	scanned []string
}

func (c *reExportScanCounter) EdgesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	for _, k := range kinds {
		if k == graph.EdgeReExports {
			c.scanned = append(c.scanned, filePaths...)
		}
	}
	return graph.EdgesInScopeSeq(c.Graph, repoPrefixes, filePaths, kinds...)
}

func (c *reExportScanCounter) NodesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	return graph.NodesInScopeSeq(c.Graph, repoPrefixes, filePaths, kinds...)
}

func (c *reExportScanCounter) NodesLightInScopeSeq(repoPrefixes, filePaths []string) iter.Seq[*graph.Node] {
	return graph.NodesLightInScopeSeq(c.Graph, repoPrefixes, filePaths)
}

var _ graph.ScopedProjectionSequencer = (*reExportScanCounter)(nil)

// A caller's imported files are walked for re-exports only when their
// language can emit one: a Go import target is never scanned, a TypeScript
// barrel still is, and its re-exported directory still reaches the caller.
func TestTheReExportWalkSkipsFilesThatCannotBeBarrels(t *testing.T) {
	g := graph.New()
	for _, f := range []string{"repo/cmd/main.go", "repo/lib/lib.go", "repo/web/app.ts", "repo/web/index.ts", "repo/core/impl.ts"} {
		g.AddNode(&graph.Node{ID: f, Kind: graph.KindFile, Name: f, FilePath: f, RepoPrefix: "repo"})
	}
	g.AddBatch(nil, []*graph.Edge{
		{From: "repo/cmd/main.go", To: "repo/lib/lib.go", Kind: graph.EdgeImports, FilePath: "repo/cmd/main.go"},
		{From: "repo/web/app.ts", To: "repo/web/index.ts", Kind: graph.EdgeImports, FilePath: "repo/web/app.ts"},
		{From: "repo/web/index.ts", To: "repo/core/impl.ts", Kind: graph.EdgeReExports, FilePath: "repo/web/index.ts"},
	})
	store := &reExportScanCounter{Graph: g}
	r := New(store)
	closure := r.buildImportClosureForCallerFiles([]string{"repo/cmd/main.go", "repo/web/app.ts"})
	if len(store.scanned) == 0 {
		t.Fatal("the TypeScript barrel was not scanned for re-exports")
	}
	for _, f := range store.scanned {
		if f == "repo/lib/lib.go" {
			t.Fatalf("a Go file was scanned for re-exports: %v", store.scanned)
		}
	}
	if _, ok := closure["repo/web/app.ts"]["repo/core"]; !ok {
		t.Fatalf("the barrel's re-exported directory is missing: %v", closure["repo/web/app.ts"])
	}
	if _, ok := closure["repo/cmd/main.go"]["repo/lib"]; !ok {
		t.Fatalf("the Go import's directory is missing: %v", closure["repo/cmd/main.go"])
	}
}
