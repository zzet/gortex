package indexer

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// This diagnostic records the actual compiled Store method set, rather than
// interpreting incomplete symbol-search results as absence. The semantic test
// below remains required if an optional bulk implementation is added later.
func TestGoPackageOwnershipActualStoreBulkCapability(t *testing.T) {
	var store *store_sqlite.Store
	_, full := any(store).(graph.BackendResolver)
	scoped, hasScoped := reflect.TypeOf(store).MethodByName("ResolveAllBulkScoped")
	t.Logf("actual Store backend full=%v scoped=%v scoped_type=%v", full, hasScoped, scoped.Type)
}

// This is the retained sensitive real cold/sparse fixture, strengthened to
// require BOTH the package import and call targets. The independent oracle is
// full module + directory identity; cold/composed parity alone is insufficient.
func TestGoPackageOwnershipColdAndSparseExactTargets(t *testing.T) {
	for _, tc := range []struct{ name, exactDir, otherDir string }{
		{"exact_internal", "internal/graph", "misc/graph"},
		{"exact_misc", "misc/graph", "internal/graph"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exactFile, otherFile := tc.exactDir+"/a.go", tc.otherDir+"/b.go"
			treeA := map[string]string{
				"go.mod":  "module example.test/fixture\n\ngo 1.23\n",
				"main.go": "package fixture\n\nfunc Call() {}\n",
				exactFile: "package graph\n\nfunc Use() {}\n",
				otherFile: "package graph\n\nfunc Use() {}\n",
			}
			treeB := maps.Clone(treeA)
			treeB["main.go"] = fmt.Sprintf("package fixture\n\nimport %q\n\nfunc Call() { graph.Use() }\n", "example.test/fixture/"+tc.exactDir)
			c := buildClosureCase(t, treeA, treeB)
			graphExactFile := builderRepoPrefix + "/" + exactFile
			graphOtherFile := builderRepoPrefix + "/" + otherFile
			var intended, unrelated string
			for _, node := range c.flat.GetFileNodes(graphExactFile) {
				if node.Name == "Use" {
					if intended != "" {
						t.Fatal("multiple exact-package Use definitions")
					}
					intended = node.ID
				}
			}
			for _, node := range c.flat.GetFileNodes(graphOtherFile) {
				if node.Name == "Use" {
					unrelated = node.ID
				}
			}
			if intended == "" || unrelated == "" || intended == unrelated {
				t.Fatalf("missing/non-distinct definitions: exact=%q unrelated=%q", intended, unrelated)
			}
			fileNode := c.flat.GetNode(graphExactFile)
			if fileNode == nil || fileNode.Kind != graph.KindFile || fileNode.FilePath != graphExactFile {
				t.Fatalf("exact package file identity absent: %+v", fileNode)
			}
			project := func(reader graph.Reader) (calls, imports []string) {
				for _, node := range reader.GetFileNodes(builderRepoPrefix + "/main.go") {
					for _, edge := range reader.GetOutEdges(node.ID) {
						if node.Name == "Call" && edge.Kind == graph.EdgeCalls {
							calls = append(calls, edge.To)
						}
						if edge.Kind == graph.EdgeImports {
							imports = append(imports, edge.To)
						}
					}
				}
				slices.Sort(calls)
				slices.Sort(imports)
				return calls, imports
			}
			flatCalls, flatImports := project(c.flat)
			composedCalls, composedImports := project(c.composed)
			t.Logf("exact=%s unrelated=%s closure=%v", intended, unrelated, c.report.ClosurePaths)
			t.Logf("cold calls=%v imports=%v; composed calls=%v imports=%v", flatCalls, flatImports, composedCalls, composedImports)
			wantCalls, wantImports := []string{intended}, []string{fileNode.ID}
			for _, result := range []struct {
				name           string
				calls, imports []string
			}{
				{"cold", flatCalls, flatImports}, {"composed", composedCalls, composedImports},
			} {
				if !reflect.DeepEqual(result.calls, wantCalls) {
					t.Errorf("%s violates full-package call oracle: got %v want %v", result.name, result.calls, wantCalls)
				}
				if !reflect.DeepEqual(result.imports, wantImports) {
					t.Errorf("%s violates full-package import oracle: got %v want %v", result.name, result.imports, wantImports)
				}
			}
		})
	}
}
