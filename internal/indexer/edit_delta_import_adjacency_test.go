package indexer

import (
	"fmt"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The delta's import-adjacency projection answers from the store at the
// bottom of the stack for the paths no layer touches and through the
// composition for the rest; either way it must equal what the resolver's
// ordinary fallback computes from full rows (the file's nodes' import edges).
func TestEditDeltaImportAdjacencyMatchesTheCompositionsRows(t *testing.T) {
	const module = "example.com/imports"
	repo := builderTempDir(t, "repo")
	builderWriteTree(t, repo, map[string]string{
		"go.mod": "module " + module + "\n\ngo 1.22\n",
		"a/a.go": "package a\n\nfunc A() int { return 1 }\n",
		"b/b.go": "package b\n\nimport \"" + module + "/a\"\n\nfunc B() int { return a.A() }\n",
		"c/c.go": "package c\n\nimport (\n\t\"" + module + "/a\"\n\t\"" + module + "/b\"\n)\n\nfunc C() int { return a.A() + b.B() }\n",
		"d/d.go": "package d\n\nimport \"strings\"\n\nfunc D() string { return strings.ToUpper(\"d\") }\n",
	})
	store := builderOpenStore(t, "imports")
	builderIndex(t, store, repo)

	paths := []string{"repo/a/a.go", "repo/b/b.go", "repo/c/c.go", "repo/d/d.go"}
	reference := func(dw *graph.DeltaWriter) map[string][]string {
		out := make(map[string][]string)
		for _, p := range paths {
			var ids []string
			for _, n := range dw.GetFileNodes(p) {
				ids = append(ids, n.ID)
			}
			for _, edges := range dw.GetOutEdgesByNodeIDs(ids) {
				for _, e := range edges {
					if e.Kind == graph.EdgeImports {
						out[p] = append(out[p], e.To)
					}
				}
			}
			sort.Strings(out[p])
		}
		return out
	}
	check := func(label string, dw *graph.DeltaWriter) {
		t.Helper()
		got, complete := dw.ProjectImportAdjacency(paths)
		if !complete {
			t.Fatalf("%s: the projection refused a canonical request", label)
		}
		for _, p := range paths {
			sort.Strings(got[p])
		}
		want := reference(dw)
		for _, p := range paths {
			if fmt.Sprint(got[p]) != fmt.Sprint(want[p]) {
				t.Errorf("%s: %s imports %v through the projection, %v through the rows", label, p, got[p], want[p])
			}
		}
	}

	untouched := graph.NewDeltaWriter(store, nil)
	if want := reference(untouched); len(want["repo/c/c.go"]) != 2 || len(want["repo/b/b.go"]) != 1 {
		t.Fatalf("the fixture's imports are not what it declares: %v", want)
	}
	check("untouched", untouched)

	// Evicting a file claims the sources of the edges into its nodes (c and
	// b import a) and hides the edges into its identities; an import edge
	// removed out of c's file node makes c a touched path while d stays on
	// the store's projection.
	dw := graph.NewDeltaWriter(store, nil)
	dw.EvictFiles([]string{"repo/a/a.go"})
	check("after an eviction", dw)
	for _, e := range dw.GetOutEdges("repo/c/c.go") {
		if e.Kind == graph.EdgeImports {
			dw.RemoveEdgesExact([]*graph.Edge{e})
			break
		}
	}
	check("after removing one of c's imports", dw)
}
