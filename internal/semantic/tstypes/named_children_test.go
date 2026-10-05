package tstypes

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/typescript"
)

// parseTS parses a TypeScript source string into a tree, returning the
// tree (caller closes) and its root node.
func parseTS(t testing.TB, src string) (*sitter.Tree, *sitter.Node) {
	t.Helper()
	tree, err := parser.ParseFile([]byte(src), typescript.GetLanguage())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	root := tree.RootNode()
	if root == nil {
		tree.Close()
		t.Fatalf("nil root node")
	}
	return tree, root
}

// indexNamedChildren enumerates a node's named children by index — the
// exact sequence NamedChildren() must reproduce.
func indexNamedChildren(n *sitter.Node) []*sitter.Node {
	out := make([]*sitter.Node, 0, n.NamedChildCount())
	for i := 0; i < int(n.NamedChildCount()); i++ {
		out = append(out, n.NamedChild(i))
	}
	return out
}

// iterNamedChildren enumerates a node's named children via the cursor
// iterator under test.
func iterNamedChildren(n *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for c := range n.NamedChildren() {
		out = append(out, c)
	}
	return out
}

// sameNode reports whether two nodes denote the same syntax node — same
// kind and identical source span.
func sameNode(a, b *sitter.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Type() == b.Type() && a.StartByte() == b.StartByte() && a.EndByte() == b.EndByte()
}

// TestNamedChildrenMatchesIndex walks every node of a representative tree
// — including nodes whose children interleave anonymous tokens (class
// bodies with braces, parameter lists with parens/commas, import clauses)
// — and asserts the cursor iterator visits exactly the same named-child
// sequence, in the same order, as the NamedChild(i) index form. This is
// the correctness guard for the cursor helper: identical visited set =>
// identical resolution behavior.
func TestNamedChildrenMatchesIndex(t *testing.T) {
	const src = `import { A, B as C } from "mod";
import D from "other";

class Foo extends Base implements Iface {
	a = 1;
	b: number = 2;
	greet(x: string, y: number): string {
		const z = new Foo();
		return this.a + z.b;
	}
}

interface Iface extends Base, Other {}

function g(p, q) {
	let r = build();
	return r;
}
`
	tree, root := parseTS(t, src)
	defer tree.Close()

	nodesChecked := 0
	namedSeen := 0
	var check func(n *sitter.Node)
	check = func(n *sitter.Node) {
		if n == nil {
			return
		}
		nodesChecked++
		byIndex := indexNamedChildren(n)
		byIter := iterNamedChildren(n)
		if len(byIndex) != len(byIter) {
			t.Fatalf("node %q: named-child count mismatch index=%d iter=%d",
				n.Type(), len(byIndex), len(byIter))
		}
		for i := range byIndex {
			if !sameNode(byIndex[i], byIter[i]) {
				a, b := byIndex[i], byIter[i]
				t.Fatalf("node %q child %d mismatch: index=(%s %d-%d) iter=(%s %d-%d)",
					n.Type(), i, a.Type(), a.StartByte(), a.EndByte(),
					b.Type(), b.StartByte(), b.EndByte())
			}
			namedSeen++
		}
		// Recurse over ALL children (named + anonymous) so every node in
		// the tree — not just named ones — is exercised by the check.
		for i := 0; i < int(n.ChildCount()); i++ {
			check(n.Child(i))
		}
	}
	check(root)

	if nodesChecked < 20 {
		t.Fatalf("checked only %d nodes — fixture did not parse as expected", nodesChecked)
	}
	if namedSeen == 0 {
		t.Fatalf("no named children were compared")
	}
	t.Logf("equivalence verified over %d nodes (%d named-child comparisons)", nodesChecked, namedSeen)
}

// TestNamedChildrenEmptyAndSingle covers a leaf with no children.
func TestNamedChildrenEmptyAndSingle(t *testing.T) {
	tree, root := parseTS(t, "let x = 1;\n")
	defer tree.Close()

	// Walk to find a leaf node (no children at all).
	var leaf *sitter.Node
	var find func(n *sitter.Node)
	find = func(n *sitter.Node) {
		if leaf != nil || n == nil {
			return
		}
		if n.ChildCount() == 0 {
			leaf = n
			return
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			find(n.Child(i))
		}
	}
	find(root)
	if leaf == nil {
		t.Fatal("no leaf node found")
	}
	if got := iterNamedChildren(leaf); len(got) != 0 {
		t.Fatalf("leaf %q: iterator yielded %d children, want 0", leaf.Type(), len(got))
	}
}

// wideProgram returns a TypeScript program whose root has exactly n
// named children (top-level const declarations).
func wideProgram(n int) string {
	var sb strings.Builder
	sb.Grow(n * 16)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "const x%d = %d;\n", i, i)
	}
	return sb.String()
}

// TestNamedChildrenWideTraversal checks both widths formerly used by the
// wall-time ratio gate. Exact count, source spans, and order are deterministic;
// a ratio of timings across different working-set sizes is not a complexity
// proof. Scaling measurements belong in BenchmarkNamedChildren.
func TestNamedChildrenWideTraversal(t *testing.T) {
	for _, width := range []int{10_000, 100_000} {
		t.Run(fmt.Sprintf("N=%d", width), func(t *testing.T) {
			src := wideProgram(width)
			source := []byte(src)
			tree, root := parseTS(t, src)
			defer tree.Close()
			if got := int(root.NamedChildCount()); got != width {
				t.Fatalf("root has %d named children, want %d", got, width)
			}
			root.WithScratch(func() {
				count, offset := 0, 0
				for child := range root.NamedChildren() {
					want := fmt.Sprintf("const x%d = %d;", count, count)
					if child == nil || child.Type() != "lexical_declaration" ||
						int(child.StartByte()) != offset || int(child.EndByte()) != offset+len(want) ||
						child.Content(source) != want {
						t.Fatalf("child %d differs from declaration at bytes %d..%d", count, offset, offset+len(want))
					}
					count++
					offset += len(want) + 1
				}
				if count != width || offset != len(src) {
					t.Fatalf("visited %d declarations covering %d bytes, want %d/%d", count, offset, width, len(src))
				}
			})
		})
	}
}

func TestNamedChildrenStopsAndRestartsWithAnonymousChildren(t *testing.T) {
	tree, root := parseTS(t, "function f(a, b, c) { return a; }\n")
	defer tree.Close()
	var parameters *sitter.Node
	var find func(*sitter.Node)
	find = func(n *sitter.Node) {
		if n == nil || parameters != nil {
			return
		}
		if n.Type() == "formal_parameters" {
			parameters = n
			return
		}
		for child := range n.NamedChildren() {
			find(child)
		}
	}
	find(root)
	if parameters == nil || parameters.NamedChildCount() != 3 || parameters.ChildCount() <= parameters.NamedChildCount() {
		t.Fatal("fixture must have three named parameters interleaved with anonymous punctuation")
	}
	want := indexNamedChildren(parameters)
	seq := parameters.NamedChildren()
	for _, stop := range []int{1, 2, 3} {
		parameters.WithScratch(func() {
			count := 0
			for child := range seq {
				if count >= len(want) || !sameNode(child, want[count]) {
					t.Fatalf("restart after stop %d yielded a different child at %d", stop, count)
				}
				count++
				if count == stop {
					break
				}
			}
			if count != stop {
				t.Fatalf("stop %d yielded %d children", stop, count)
			}
		})
	}
	if got := iterNamedChildren(parameters); len(got) != len(want) {
		t.Fatalf("full traversal after early stops yielded %d children, want %d", len(got), len(want))
	}
}

func walkIter(n *sitter.Node) int {
	cnt := 0
	for range n.NamedChildren() {
		cnt++
	}
	return cnt
}

func walkIndex(n *sitter.Node) int {
	cnt := 0
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if n.NamedChild(i) != nil {
			cnt++
		}
	}
	return cnt
}

// BenchmarkNamedChildren reports traversal and per-child costs without turning
// machine-dependent wall-time ratios into correctness assertions. Indexed
// navigation can skip hidden subtrees, so its scaling depends on tree shape.
func BenchmarkNamedChildren(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		tree, root := parseTS(b, wideProgram(n))
		for _, walker := range []struct {
			name string
			walk func(*sitter.Node) int
		}{{"iter", walkIter}, {"index", walkIndex}} {
			b.Run(fmt.Sprintf("%s/N=%d", walker.name, n), func(b *testing.B) {
				// Reuse one pass's arena capacity; no wrapper escapes a count-only
				// walk. This measures navigation rather than accumulating arenas.
				root.WithScratch(func() {
					if got := walker.walk(root); got != n {
						b.Fatalf("warmup visited %d, want %d", got, n)
					}
				})
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					root.WithScratch(func() {
						if got := walker.walk(root); got != n {
							b.Fatalf("visited %d, want %d", got, n)
						}
					})
				}
				b.StopTimer()
				b.ReportMetric(float64(n), "children/op")
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/child")
			})
		}
		tree.Close()
	}
}
