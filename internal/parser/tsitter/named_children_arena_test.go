package tsitter

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// This checks observable shim work, not a native traversal complexity theorem:
// stopping after k children must create only k wrappers, and scratch repeats
// must reuse one traversal's storage rather than retain every prior pass.
func TestNamedChildrenWrapsLazilyAndReusesScratch(t *testing.T) {
	const width = 8192
	var source strings.Builder
	for i := 0; i < width; i++ {
		fmt.Fprintf(&source, "const x%d = %d;\n", i, i)
	}
	p := NewParser()
	defer p.Close()
	p.SetLanguage(NewLanguage(tree_sitter_typescript.LanguageTypescript()))
	tree, err := p.ParseCtx(context.Background(), nil, []byte(source.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	root := tree.RootNode()
	if root == nil || int(root.NamedChildCount()) != width || root.arena == nil {
		t.Fatal("fixture must have a wide parsed root with an arena")
	}
	a := root.arena
	entry := a.mark()
	allocated := func() int {
		total := a.used
		for _, chunk := range a.chunks[:a.ci] {
			total += len(chunk)
		}
		return total
	}
	before := allocated()
	seq := root.NamedChildren()
	if a.mark() != entry {
		t.Fatal("creating the sequence eagerly allocated child wrappers")
	}
	for _, stop := range []int{1, 64, 65, width} {
		root.WithScratch(func() {
			count := 0
			for child := range seq {
				if child == nil || !child.valid {
					t.Fatal("iterator yielded an invalid child")
				}
				count++
				if count == stop {
					break
				}
			}
			if count != stop || allocated()-before != stop {
				t.Fatalf("stop %d: yielded %d, allocated %d wrappers", stop, count, allocated()-before)
			}
		})
		if a.mark() != entry || !root.valid {
			t.Fatal("scratch traversal did not rewind or invalidated its root")
		}
	}
	retained := a.retainedBytes()
	for pass := 0; pass < 3; pass++ {
		root.WithScratch(func() {
			count := 0
			for range seq {
				count++
			}
			if count != width || allocated()-before != width {
				t.Fatalf("pass %d: yielded %d, allocated %d wrappers", pass, count, allocated()-before)
			}
		})
		if a.mark() != entry || a.retainedBytes() != retained {
			t.Fatalf("pass %d retained extra traversal storage", pass)
		}
	}
}
