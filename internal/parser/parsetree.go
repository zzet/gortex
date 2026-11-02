package parser

import (
	"fmt"
	"sync/atomic"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// ParseTree is a ref-counted handle to a tree-sitter parse tree plus
// the source bytes the tree was parsed from. The language extractor
// produces it; the indexer hands it to contract extractors and the
// post-pass resolvers and closes it (decrementing refs) when each
// consumer is done.
//
// Lifetime: a ParseTree starts with refs=1. Every consumer that
// retains the handle (e.g. caches it across calls) must call Acquire
// to bump the count and Release to drop it. Single-shot consumers
// that read and return don't need to touch the count — the producer's
// close balances the producer's create.
//
// ParseTree is safe for concurrent reads (tree-sitter trees are
// immutable after parse) but not for concurrent close. Acquire/Release
// are atomic; closing happens at most once when refs reach 0.
type ParseTree struct {
	tree *sitter.Tree
	src  []byte
	lang string
	refs atomic.Int32
}

// NewParseTree wraps an already-parsed *sitter.Tree with the source
// bytes and language code. The returned handle starts with refs=1;
// the producer must Close (or Release) it once.
func NewParseTree(tree *sitter.Tree, src []byte, lang string) *ParseTree {
	pt := &ParseTree{tree: tree, src: src, lang: lang}
	pt.refs.Store(1)
	return pt
}

// Tree returns the underlying tree. Returns nil if the ParseTree has
// been closed.
func (pt *ParseTree) Tree() *sitter.Tree {
	if pt == nil {
		return nil
	}
	return pt.tree
}

// Source returns the source bytes the tree was parsed from. Same
// slice the extractor was handed; do not mutate.
func (pt *ParseTree) Source() []byte {
	if pt == nil {
		return nil
	}
	return pt.src
}

// Lang returns the language code ("go", "typescript", …).
func (pt *ParseTree) Lang() string {
	if pt == nil {
		return ""
	}
	return pt.lang
}

// Acquire bumps the ref count. Pair every Acquire with a Release.
func (pt *ParseTree) Acquire() {
	if pt == nil {
		return
	}
	pt.refs.Add(1)
}

// Release decrements the ref count and closes the underlying tree
// when it reaches zero. Safe to call on nil.
func (pt *ParseTree) Release() {
	if pt == nil {
		return
	}
	if pt.refs.Add(-1) <= 0 {
		if pt.tree != nil {
			pt.tree.Close()
			pt.tree = nil
		}
	}
}

// Close is an alias for Release that lets ParseTree satisfy a
// generic io.Closer-style contract for defer convenience.
func (pt *ParseTree) Close() {
	pt.Release()
}

// ReleaseTree drops the result's reference to its parse tree, if it
// holds one. Nil-safe on the result, on the handle, and on repeat
// calls, so every path that consumes an ExtractionResult can
// unconditionally `defer result.ReleaseTree()` right after extraction
// without first knowing whether this language's extractor retains a
// tree (only the Go extractor does today) or whether the extraction
// even succeeded.
//
// Clearing the field before releasing is what makes the repeat call
// safe: a second ReleaseTree must not decrement a count the caller no
// longer owns, which would close the tree out from under a consumer
// that legitimately holds its own Acquire'd reference.
func (r *ExtractionResult) ReleaseTree() {
	if r == nil || r.Tree == nil {
		return
	}
	tree := r.Tree
	r.Tree = nil
	tree.Release()
}

// HasParseErrors reports whether the underlying tree contains any
// ERROR or MISSING nodes. Returns false for a nil ParseTree so
// language extractors that drop their tree (most non-Go languages)
// don't appear to have parse errors when the signal isn't available.
func (pt *ParseTree) HasParseErrors() bool {
	if pt == nil || pt.tree == nil {
		return false
	}
	root := pt.tree.RootNode()
	if root == nil {
		return false
	}
	return root.HasError()
}

// CountParseErrors returns the number of ERROR or MISSING nodes in
// the tree. Useful for stamping a quantitative parse_errors metric
// on a KindFile node so index_health can rank the worst offenders.
// Returns 0 when the ParseTree is nil.
func (pt *ParseTree) CountParseErrors() int {
	if pt == nil || pt.tree == nil {
		return 0
	}
	root := pt.tree.RootNode()
	if root == nil {
		return 0
	}
	_, count := ParseErrorVerdict(root)
	return count
}

// ParseErrorVerdict captures the syntax verdict of a successful full parse
// while its root is live. The clean path is one native HasError query; only
// error trees are walked. A missing root is unknown rather than checked zero.
func ParseErrorVerdict(root *sitter.Node) (checked bool, count int) {
	if root == nil {
		return false, 0
	}
	if !root.HasError() {
		return true, 0
	}
	walkParseErrors(root, &count)
	return true, count
}

func walkParseErrors(n *sitter.Node, count *int) {
	if n == nil {
		return
	}
	if n.Type() == "ERROR" || n.IsMissing() {
		*count++
	}
	childCount := int(n.ChildCount())
	for i := 0; i < childCount; i++ {
		walkParseErrors(n.Child(i), count)
	}
}

// parseErrorLocationCap bounds the number of error locations collected so a
// pathologically broken file can't produce a multi-kilobyte errors blob.
const parseErrorLocationCap = 50

// ParseErrorLocations returns the 1-based "row:col" location of each ERROR /
// MISSING node, capped at parseErrorLocationCap. Returns nil when the tree is
// nil or clean — the per-file metadata sidecar stores these as a JSON array
// so index_health can point at where a file failed to parse.
func (pt *ParseTree) ParseErrorLocations() []string {
	if pt == nil || pt.tree == nil {
		return nil
	}
	root := pt.tree.RootNode()
	if root == nil {
		return nil
	}
	var locs []string
	walkParseErrorLocations(root, &locs)
	return locs
}

func walkParseErrorLocations(n *sitter.Node, locs *[]string) {
	if n == nil || len(*locs) >= parseErrorLocationCap {
		return
	}
	if n.Type() == "ERROR" || n.IsMissing() {
		p := n.StartPoint()
		*locs = append(*locs, fmt.Sprintf("%d:%d", p.Row+1, p.Column+1))
	}
	childCount := int(n.ChildCount())
	for i := 0; i < childCount && len(*locs) < parseErrorLocationCap; i++ {
		walkParseErrorLocations(n.Child(i), locs)
	}
}
