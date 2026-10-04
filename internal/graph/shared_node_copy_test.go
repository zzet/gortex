package graph

import "testing"

// A node ID several files emit (one annotation applied in several files, one
// error message in several files) keeps the copy from the smallest file path
// whatever order the files were added in; a re-parse of the kept file still
// replaces it.
func TestSharedNodeKeepsTheSmallestFileWhateverTheOrder(t *testing.T) {
	copyFrom := func(file string, line int) *Node {
		return &Node{
			ID: "repo/annotation::typescript::Module", Kind: KindType, Name: "Module",
			FilePath: file, StartLine: line, EndLine: line, Language: "typescript", RepoPrefix: "repo",
			Meta: map[string]any{"kind": "annotation", "synthetic": true},
		}
	}
	for _, order := range [][]string{{"repo/b.ts", "repo/a.ts", "repo/c.ts"}, {"repo/c.ts", "repo/a.ts", "repo/b.ts"}} {
		g := New()
		for i, file := range order {
			if i%2 == 0 {
				g.AddNode(copyFrom(file, 5))
			} else {
				g.AddBatch([]*Node{copyFrom(file, 5)}, nil)
			}
		}
		got := g.GetNode("repo/annotation::typescript::Module")
		if got == nil || got.FilePath != "repo/a.ts" {
			t.Fatalf("order %v kept %+v, want the repo/a.ts copy", order, got)
		}
		if len(g.GetFileNodes("repo/a.ts")) != 1 || len(g.GetFileNodes("repo/b.ts")) != 0 || len(g.GetFileNodes("repo/c.ts")) != 0 {
			t.Fatalf("order %v: the file index disagrees with the kept copy", order)
		}
		g.AddNode(copyFrom("repo/a.ts", 9))
		if got := g.GetNode("repo/annotation::typescript::Module"); got.StartLine != 9 {
			t.Fatalf("a re-add from the kept file did not replace it: %+v", got)
		}
	}
	// Shared string literals are not marked synthetic; the rule is by file.
	g := New()
	g.AddNode(&Node{ID: "repo/string::error_msg::boom", Kind: KindString, Name: "boom", FilePath: "repo/b.go"})
	g.AddNode(&Node{ID: "repo/string::error_msg::boom", Kind: KindString, Name: "boom", FilePath: "repo/a.go"})
	g.AddNode(&Node{ID: "repo/string::error_msg::boom", Kind: KindString, Name: "boom", FilePath: "repo/c.go"})
	if got := g.GetNode("repo/string::error_msg::boom"); got.FilePath != "repo/a.go" {
		t.Fatalf("a shared string keeps the smallest file's copy: %+v", got)
	}
	// Another kind is an upgrade, not a copy: a reconcile pass turning a
	// contract into a topic replaces it whatever the files.
	g.AddNode(&Node{ID: "topic::kafka::orders", Kind: KindContract, Name: "orders", FilePath: "repo/a.go"})
	g.AddNode(&Node{ID: "topic::kafka::orders", Kind: KindTopic, Name: "orders", FilePath: "repo/b.go"})
	if got := g.GetNode("topic::kafka::orders"); got.Kind != KindTopic {
		t.Fatalf("a re-add of another kind must replace: %+v", got)
	}
	// A declaration-shaped node keeps last-add-wins.
	g.AddNode(&Node{ID: "x", Kind: KindFunction, Name: "x", FilePath: "repo/a.go"})
	g.AddNode(&Node{ID: "x", Kind: KindFunction, Name: "x", FilePath: "repo/b.go"})
	if got := g.GetNode("x"); got.FilePath != "repo/b.go" {
		t.Fatalf("a non-registry re-add must replace: %+v", got)
	}
	// A pathless copy on either side keeps the last-add-wins contract.
	g.AddNode(&Node{ID: "stub", Kind: KindFunction, Name: "stub"})
	g.AddNode(&Node{ID: "stub", Kind: KindFunction, Name: "stub", FilePath: "repo/z.go"})
	if got := g.GetNode("stub"); got.FilePath != "repo/z.go" {
		t.Fatalf("a pathed re-add over a pathless node must replace: %+v", got)
	}
	g.AddNode(&Node{ID: "stub", Kind: KindFunction, Name: "stub"})
	if got := g.GetNode("stub"); got.FilePath != "" {
		t.Fatalf("a pathless re-add must replace: %+v", got)
	}
}
