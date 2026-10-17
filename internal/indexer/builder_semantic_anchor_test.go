package indexer

import (
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// anchorTree is a two-package module: app imports lib, and the resolver
// binds that import to one of lib's files (the package's anchor file).
func anchorTree() map[string]string {
	return map[string]string{
		"go.mod":      "module example.com/m\n\ngo 1.22\n",
		"lib/a.go":    "package lib\n\n// A is lib's entry.\nfunc A() int { return helper() }\n\nfunc helper() int { return 1 }\n",
		"lib/b.go":    "package lib\n\n// B is lib's other entry.\nfunc B() int { return 2 }\n",
		"app/main.go": "package app\n\nimport \"example.com/m/lib\"\n\n// Run calls lib.\nfunc Run() int { return lib.A() + lib.B() }\n",
	}
}

// anchorFile is the lib file app's import edge lands on in the flat index.
func anchorFile(t *testing.T, flat graph.Reader) string {
	t.Helper()
	for _, edge := range flat.AllEdges() {
		if edge.Kind == graph.EdgeImports && edge.FilePath == builderRepoPrefix+"/app/main.go" {
			if node := flat.GetNode(edge.To); node != nil && node.Kind == graph.KindFile {
				rel, _ := builderRelPath(builderRepoPrefix, node.FilePath)
				return rel
			}
		}
	}
	t.Fatal("app/main.go has no import edge onto a lib file node: the fixture does not cover the anchor")
	return ""
}

// TestEditingAPackagesImportAnchorLeavesItsImportersOut pins that an edit to
// the file a package's importers bind their import to — a body edit, and a
// declaration rename nothing outside the file calls — does not re-derive every
// importer: the import binds to the file by its place in the tree, which the
// edit does not move. On the real repository this was every importer of
// internal/gitstate for any edit of dirty.go (15 files re-derived and 21,891
// edge rows stored for a one-line rename). The served view must still equal a
// clean index of the edited tree.
func TestEditingAPackagesImportAnchorLeavesItsImportersOut(t *testing.T) {
	treeA := anchorTree()
	const anchor = "lib/a.go"

	for name, edit := range map[string]func(string) string{
		"body": func(src string) string {
			return src + "\nfunc anchorExtra() int { return 3 }\n"
		},
		"rename": func(src string) string {
			const decl = "func helper() int { return 1 }"
			if !strings.Contains(src, decl) {
				t.Fatalf("%s does not declare helper", anchor)
			}
			return strings.Replace(src, decl, "func helperRen() int { return 1 }\n\nfunc helper() int { return helperRen() }", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			treeB := make(map[string]string, len(treeA))
			for p, body := range treeA {
				treeB[p] = body
			}
			treeB[anchor] = edit(treeA[anchor])
			c := buildClosureCase(t, treeA, treeB)
			if got := anchorFile(t, c.flat); got != anchor {
				t.Fatalf("app's import lands on %s, not %s: the edit does not cover the anchor", got, anchor)
			}
			if slices.Contains(c.report.ClosurePaths, "app/main.go") {
				t.Errorf("an edit of the import anchor %s re-derives its importer: closure %v, dependents %v",
					anchor, c.report.ClosurePaths, c.report.ClosureDependentPaths)
			}
			builderAssertReadersAgree(t, c.composed, c.flat)
		})
	}
}

// TestGroupedResultsOfOneTypeAreNotAShapeChange pins the target-side shape of a
// declaration whose results share a type on one line: the store keeps one
// `returns string` edge for `(a, b string, err error)` (one edge identity),
// the extraction emits two, and comparing the raw extraction with the stored
// shape reported a signature change on every edit of the file — so every
// caller of the function was re-derived as a dependent.
func TestGroupedResultsOfOneTypeAreNotAShapeChange(t *testing.T) {
	treeA := map[string]string{
		"split.go":  "package fixture\n\nfunc Split(s string) (a, b string, err error) { return s, s, nil }\n",
		"caller.go": "package fixture\n\nfunc Caller() string { x, _, _ := Split(\"v\"); return x }\n",
	}
	treeB := map[string]string{
		"split.go":  "package fixture\n\nfunc Split(s string) (a, b string, err error) {\n\t// body-only edit\n\treturn s, s, nil\n}\n",
		"caller.go": treeA["caller.go"],
	}
	c := buildClosureCase(t, treeA, treeB)
	if slices.Contains(c.report.ClosurePaths, "caller.go") {
		t.Errorf("a body edit of a grouped-results function re-derives its caller: closure %v", c.report.ClosurePaths)
	}
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestStoredExtractionIdentitiesCollapseLikeTheStore pins the normalization
// itself: one node per id and one edge per identity, the later one winning.
func TestStoredExtractionIdentitiesCollapseLikeTheStore(t *testing.T) {
	first := &graph.Edge{From: "f", To: "string", Kind: graph.EdgeReturns, FilePath: "x.go", Line: 3, Meta: map[string]any{"position": 0}}
	second := &graph.Edge{From: "f", To: "string", Kind: graph.EdgeReturns, FilePath: "x.go", Line: 3, Meta: map[string]any{"position": 1}}
	other := &graph.Edge{From: "f", To: "error", Kind: graph.EdgeReturns, FilePath: "x.go", Line: 3, Meta: map[string]any{"position": 2}}
	n1 := &graph.Node{ID: "v", Name: "v", Kind: graph.KindVariable}
	n2 := &graph.Node{ID: "v", Name: "v", Kind: graph.KindVariable, Meta: map[string]any{"later": true}}
	nodes, edges := storedExtractionIdentities([]*graph.Node{n1, n2}, []*graph.Edge{first, other, second})
	if len(nodes) != 1 || nodes[0] != n2 {
		t.Fatalf("nodes = %v, want the later node with id v only", nodes)
	}
	if len(edges) != 2 || edges[0] != second || edges[1] != other {
		t.Fatalf("edges = %v, want [second other]", edges)
	}
}

// TestAnExportedAdditionToTheImportAnchorReachesWaitingImporters is the other
// side of the rule: an importer that already calls a name the package does not
// define yet (`lib.New()`, parked on an external-call placeholder) must be
// re-derived when the anchor file adds it, so its call binds as a clean index
// binds it.
func TestAnExportedAdditionToTheImportAnchorReachesWaitingImporters(t *testing.T) {
	treeA := anchorTree()
	treeA["app/main.go"] = "package app\n\nimport \"example.com/m/lib\"\n\n// Run calls lib.\nfunc Run() int { return lib.A() + lib.New() }\n"
	treeB := make(map[string]string, len(treeA))
	for p, body := range treeA {
		treeB[p] = body
	}
	treeB["lib/a.go"] = treeA["lib/a.go"] + "\n// New is new.\nfunc New() int { return 5 }\n"
	c := buildClosureCase(t, treeA, treeB)
	if got := anchorFile(t, c.flat); got != "lib/a.go" {
		t.Fatalf("app's import lands on %s, not lib/a.go", got)
	}
	if !slices.Contains(c.report.ClosurePaths, "app/main.go") {
		t.Errorf("an exported addition to the import anchor left its waiting importer out: closure %v", c.report.ClosurePaths)
	}
	// The importer's own edges bind as a clean index binds them. (The whole
	// view is not compared: the pathless stubs the old unresolved call minted
	// stay served — a known composition gap that predates this rule and shows
	// identically with the importer re-derived through the file node.)
	recordedAt := func(r graph.Reader) []string {
		var out []string
		for _, edge := range r.AllEdges() {
			if edge != nil && edge.FilePath == builderRepoPrefix+"/app/main.go" {
				out = append(out, builderRenderEdge(edge))
			}
		}
		slices.Sort(out)
		return out
	}
	builderSameStrings(t, "edges recorded at app/main.go", recordedAt(c.composed), recordedAt(c.flat))
}
