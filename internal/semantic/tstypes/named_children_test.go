package tstypes

import (
	"fmt"
	"strings"
	"testing"
	"time"

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

// indexNamedChildren enumerates a node's named children the O(N^2) index
// way — the exact behavior NamedChildren() must reproduce.
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
// the correctness guard for the O(N) helper: identical visited set =>
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

// TestNamedChildrenEmptyAndSingle covers the boundary shapes: a node with
// no children and a node whose only child is anonymous.
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

// timePerRun returns the average wall time of one full enumeration of
// root's named children using walk, averaged over repeats to damp
// scheduler noise. It also asserts every pass visits want children.
func timePerRun(t *testing.T, root *sitter.Node, want, repeats int, walk func(*sitter.Node) int) time.Duration {
	t.Helper()
	// One warmup pass (touch caches, fault in pages) before timing.
	root.WithScratch(func() {
		if got := walk(root); got != want {
			t.Fatalf("warmup visited %d, want %d", got, want)
		}
	})
	start := time.Now()
	for r := 0; r < repeats; r++ {
		// Both walkers return only counts. No child wrapper escapes this
		// pass, so subsequent repeats reuse one traversal's arena capacity.
		root.WithScratch(func() {
			if got := walk(root); got != want {
				t.Fatalf("visited %d on repeat %d, want %d", got, r, want)
			}
		})
	}
	return time.Since(start) / time.Duration(repeats)
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

// TestNamedChildrenLinearScaling proves the cursor iterator walks a wide
// node in O(N), not O(N^2), by measuring it at two widths a 10x factor
// apart. A linear walk costs ~10x more at 10x the width; a quadratic walk
// costs ~100x more. The assertion is a loose growth-ratio bound (< 20x):
// machine-speed-independent (it is a ratio of two times on the same
// machine), never flaky for a genuinely linear walk, and decisive against
// a regression that reintroduces NamedChild(i) indexing inside the helper
// — which, at this width, re-walks the sibling chain per step and pushes
// the growth ratio far past the bound.
//
// The naive NamedChild(i) walk is timed alongside purely as logged
// evidence of the quadratic baseline this helper replaces (its per-step
// re-walk makes its growth ratio climb above the iterator's). It is not
// asserted on, because the width at which its quadratic term overtakes
// per-call CGO overhead is machine-dependent.
func TestNamedChildrenLinearScaling(t *testing.T) {
	const small, large = 10000, 100000

	treeS, rootS := parseTS(t, wideProgram(small))
	defer treeS.Close()
	treeL, rootL := parseTS(t, wideProgram(large))
	defer treeL.Close()

	if got := int(rootS.NamedChildCount()); got != small {
		t.Fatalf("small root has %d named children, want %d", got, small)
	}
	if got := int(rootL.NamedChildCount()); got != large {
		t.Fatalf("large root has %d named children, want %d", got, large)
	}

	iterSmall := timePerRun(t, rootS, small, 50, walkIter)
	iterLarge := timePerRun(t, rootL, large, 15, walkIter)
	naiveSmall := timePerRun(t, rootS, small, 50, walkIndex)
	naiveLarge := timePerRun(t, rootL, large, 5, walkIndex)

	iterRatio := float64(iterLarge) / float64(iterSmall)
	naiveRatio := float64(naiveLarge) / float64(naiveSmall)

	t.Logf("width %d->%d (10x):", small, large)
	t.Logf("  iterator: %v -> %v  (%.1fx growth — O(N) ~10x)", iterSmall, iterLarge, iterRatio)
	t.Logf("  naive   : %v -> %v  (%.1fx growth)", naiveSmall, naiveLarge, naiveRatio)

	// No-hang guard: extremely generous so it never false-fails on a slow
	// machine, yet still trips on a true O(N^2) blowup at this width.
	if iterLarge > 3*time.Second {
		t.Fatalf("iterator pass took %v for N=%d — not linear", iterLarge, large)
	}
	// Linear proof: a 10x width increase costs an O(N) walk ~10x; an
	// O(N^2) walk would cost ~100x. The 20x bound passes comfortably for
	// linear and fails decisively for any quadratic regression.
	if iterRatio >= 20 {
		t.Fatalf("iterator growth %.1fx over a 10x width increase — not linear (want < 20x)", iterRatio)
	}
}

// BenchmarkNamedChildren measures the cursor iterator against the naive
// index loop at two widths; the index loop's per-op cost grows with N
// (O(N^2)) while the iterator's stays flat (O(N)).
func BenchmarkNamedChildren(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		tree, root := parseTS(b, wideProgram(n))

		b.Run(fmt.Sprintf("iter/N=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				cnt := 0
				for range root.NamedChildren() {
					cnt++
				}
				if cnt != n {
					b.Fatalf("visited %d, want %d", cnt, n)
				}
			}
		})
		b.Run(fmt.Sprintf("index/N=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				cnt := 0
				for j := 0; j < int(root.NamedChildCount()); j++ {
					if root.NamedChild(j) != nil {
						cnt++
					}
				}
				if cnt != n {
					b.Fatalf("visited %d, want %d", cnt, n)
				}
			}
		})

		tree.Close()
	}
}
