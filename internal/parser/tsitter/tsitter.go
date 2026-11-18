// Package tsitter is a thin compatibility shim over
// github.com/tree-sitter/go-tree-sitter. Its surface intentionally
// mirrors the smacker/go-tree-sitter API so that the ~90 language
// extractors in gortex can be migrated by changing only import paths —
// method names and signatures stay the same.
//
// The shim wraps native tree-sitter types (Node, Tree, Parser, Query)
// and adapts:
//   - Type() → Kind()
//   - Content(src) → Utf8Text(src)
//   - StartPoint/EndPoint → StartPosition/EndPosition (uint32 Row/Column)
//   - int-indexed children → uint-indexed (internally converted)
//   - ParseCtx(ctx, old, src) built on top of ParseWithOptions
//
// The cursor-based query iteration is not re-exposed here: it lives in
// the parent parser package, which uses the official API directly.
package tsitter

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"unsafe"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// Language is a parser language. Exposed as an alias so grammar
// sub-packages can return *ts.Language directly.
type Language = ts.Language

// NewLanguage constructs a Language from a grammar's raw C pointer —
// used by the per-language shim sub-packages.
func NewLanguage(ptr unsafe.Pointer) *Language { return ts.NewLanguage(ptr) }

// Point mirrors the smacker Point layout (uint32 row/column).
type Point struct {
	Row    uint32
	Column uint32
}

func fromTSPoint(p ts.Point) Point {
	return Point{Row: uint32(p.Row), Column: uint32(p.Column)}
}

// Node wraps *ts.Node with smacker-compatible method names. Nodes are
// valid for the lifetime of their Tree; copying by value is cheap
// (single C struct field).
type Node struct {
	inner ts.Node
	// set true when constructed; distinguishes the zero value from a real node.
	valid bool
	// langKey is the stable C-pointer identity of this node's language,
	// propagated from the root through every navigation step so Type()
	// can resolve the node kind from a per-language table with no CGO
	// call and no allocation. Nil means "not stamped" — Type() then
	// derives the language, which is slower but still correct.
	langKey unsafe.Pointer
	// arena bump-allocates Node wrappers in chunks so a deep tree walk
	// produces a few large backing arrays instead of millions of tiny heap
	// objects. The per-object GC mark cost dominated CPU when indexing a
	// large TS monorepo (vscode: ~70% of cycles in GC, scanning millions of
	// 1-node spans). Chunks are never explicitly freed — they are reachable
	// only through the Nodes that point into them and are reclaimed by the
	// GC once the tree's nodes are dropped. A nil arena falls back to a plain
	// heap Node (zero-value and SetInner-pooled nodes have no arena).
	arena *nodeArena
}

// nodeArena is a per-tree bump allocator for Node wrappers. It is not
// safe for concurrent use; each parse tree is walked by a single
// goroutine, and distinct files use distinct trees (and arenas).
//
// Arenas are pooled and reused across files (see arenaPool): a Tree takes
// one on its first RootNode() and returns it on Close(). reset() rewinds
// the allocation cursor but RETAINS the backing chunks, so a warm pool
// serves each file's node count with no fresh allocation. This is the
// dominant GC-pressure lever on a large index: profiling vscode and
// kubernetes put tree-sitter Node wrappers at 70–82% of every byte
// allocated, almost all of it per-file chunk garbage. Retaining the chunks
// turns that churn into a bounded, reused working set.
type nodeArena struct {
	chunks [][]Node // backing arrays, retained across resets for reuse
	ci     int      // index of the current chunk within chunks
	used   int      // slots used in chunks[ci]
}

// nodeArenaMark identifies one allocation cursor within an arena. Marks are
// deliberately private: only Node.WithScratch may expose the lifetime boundary
// they create, so callers cannot rewind an arena while live wrappers still
// escape from it.
type nodeArenaMark struct {
	ci   int
	used int
}

const (
	// arenaFirstChunk keeps the first backing array small so a file with a
	// handful of nodes (the common case in a many-small-files repo) wastes
	// little; chunks then double up to arenaMaxChunk so a deep file still
	// ends up with only a few large objects.
	arenaFirstChunk = 64
	arenaMaxChunk   = 4096

	// arenaPoolRetainBytes bounds the high-water capacity a single outlier
	// parse may leave in the process-wide sync.Pool. Scratch scopes normally
	// make the high-water mark one traversal instead of the sum of every
	// traversal; an exceptionally large single traversal is still useful only
	// to similarly large files, so keeping it would turn transient work into an
	// idle heap floor.
	arenaPoolRetainBytes = 8 << 20
)

func newNodeArena() *nodeArena { return &nodeArena{} }

// alloc returns a pointer to a fresh Node. The pointer is stable for the
// life of the arena: a chunk is never resized in place — when the current
// chunk fills, allocation advances to the next retained chunk, or appends a
// geometrically larger one past the high-water mark, so earlier &chunk[i]
// pointers never move. Callers overwrite all fields of the returned Node
// immediately, so a reused slot needs no zeroing here; reset() clears stale
// slots when the arena is recycled.
func (a *nodeArena) alloc() *Node {
	switch {
	case len(a.chunks) == 0:
		a.chunks = append(a.chunks, make([]Node, arenaFirstChunk))
		a.ci, a.used = 0, 0
	case a.used >= len(a.chunks[a.ci]):
		a.ci++
		if a.ci >= len(a.chunks) {
			size := len(a.chunks[a.ci-1]) * 2
			if size > arenaMaxChunk {
				size = arenaMaxChunk
			}
			a.chunks = append(a.chunks, make([]Node, size))
		}
		a.used = 0
	}
	n := &a.chunks[a.ci][a.used]
	a.used++
	return n
}

func (a *nodeArena) mark() nodeArenaMark {
	return nodeArenaMark{ci: a.ci, used: a.used}
}

// rewind releases every wrapper allocated after mark while preserving older
// wrappers. Clearing the released slots is required: a Node contains both the
// underlying tree-sitter node and an arena pointer, either of which would keep
// the parse tree or the arena graph reachable if a high-water chunk were
// reused later. The cursor can move backwards across any number of chunks, so
// nested scratch scopes work naturally.
func (a *nodeArena) rewind(mark nodeArenaMark) {
	if a == nil || len(a.chunks) == 0 {
		return
	}
	for i := mark.ci; i <= a.ci && i < len(a.chunks); i++ {
		start := 0
		if i == mark.ci {
			start = mark.used
		}
		end := len(a.chunks[i])
		if i == a.ci {
			end = a.used
		}
		if start < end {
			clear(a.chunks[i][start:end])
		}
	}
	a.ci, a.used = mark.ci, mark.used
}

func (a *nodeArena) retainedBytes() uintptr {
	var total uintptr
	nodeBytes := unsafe.Sizeof(Node{})
	for _, chunk := range a.chunks {
		total += uintptr(cap(chunk)) * nodeBytes
	}
	return total
}

// trimRetained caps the chunk prefix kept for reuse and clears every discarded
// slice header before shortening chunks. Keeping the bounded prefix preserves
// the allocation win for ordinary files after an outlier, while the full-slice
// expression prevents the backing header array from pinning discarded slabs.
func (a *nodeArena) trimRetained(limit uintptr) {
	var retained uintptr
	keep := 0
	for keep < len(a.chunks) {
		chunkBytes := uintptr(cap(a.chunks[keep])) * unsafe.Sizeof(Node{})
		if retained+chunkBytes > limit {
			break
		}
		retained += chunkBytes
		keep++
	}
	if keep == len(a.chunks) {
		return
	}
	clear(a.chunks[keep:])
	a.chunks = a.chunks[:keep:keep]
}

// reset rewinds the allocation cursor to the start while retaining the
// backing chunks for reuse. It clears the slots touched since the last
// reset so a stale Node value — whose embedded ts.Node pins its now-closed
// *ts.Tree — cannot survive into the next file and leak. Clearing only the
// used prefix keeps the cost proportional to the file just processed, not
// the high-water capacity.
func (a *nodeArena) reset() {
	a.rewind(nodeArenaMark{})
}

// arenaPool recycles per-tree arenas — with their retained chunks — across
// files. A Tree gets one lazily on its first RootNode() and returns it on
// Close(). sync.Pool may drop entries under GC pressure; a cold Get then
// just starts with no chunks and warms up again, so correctness never
// depends on retention.
var arenaPool = sync.Pool{New: func() any { return &nodeArena{} }}

func getArena() *nodeArena { return arenaPool.Get().(*nodeArena) }

func putArena(a *nodeArena) {
	if a == nil {
		return
	}
	a.reset()
	a.trimRetained(arenaPoolRetainBytes)
	arenaPool.Put(a)
}

// WrapVal wraps a ts.Node reached from n (e.g. a query capture),
// carrying n's language key so Type() on the result and its descendants
// needs neither CGO nor allocation.
func (n *Node) WrapVal(c ts.Node) *Node {
	if n.arena == nil {
		return &Node{inner: c, valid: true, langKey: n.langKey}
	}
	nn := n.arena.alloc()
	nn.inner = c
	nn.valid = true
	nn.langKey = n.langKey
	nn.arena = n.arena
	return nn
}

// WithScratch runs fn with a temporary lifetime for Node wrappers allocated by
// navigation from n. When fn returns -- or panics -- every wrapper allocated
// after entry is cleared and the arena cursor is rewound, so the next pass can
// reuse the same slabs instead of growing the tree's arena monotonically.
// Scratch scopes may be nested.
//
// The callback MUST NOT retain a *Node allocated inside the scope, directly or
// inside another value, after the callback returns. Such wrappers become
// invalid at scope exit and may be overwritten by the next traversal. Nodes
// that existed before entry, including n itself, remain valid. A Node without
// an arena still runs fn, but cannot reclaim individual wrappers early.
func (n *Node) WithScratch(fn func()) {
	if fn == nil {
		return
	}
	if n == nil || n.arena == nil {
		fn()
		return
	}
	mark := n.arena.mark()
	defer n.arena.rewind(mark)
	fn()
}

// SetInner overwrites the receiver's wrapped ts.Node and marks it
// valid. Lets callers reuse a *Node out of a pool / backing slice
// instead of allocating a new one per query match — see EachMatch in
// internal/parser/treesitter.go. The receiver must already exist
// (caller-owned), so SetInner cannot be used on a nil pointer.
func (n *Node) SetInner(inner ts.Node) {
	n.inner = inner
	n.valid = true
}

// Inner returns a pointer to the underlying ts.Node. Internal use by
// the parser package's query runners.
func (n *Node) Inner() *ts.Node {
	if n == nil || !n.valid {
		return nil
	}
	return &n.inner
}

// Type returns the node kind string ("identifier", "function_declaration", …).
func (n *Node) Type() string { return internedKind(n.inner, n.langKey) }

// kindTables memoises per-language node-kind name tables. tree-sitter
// node kinds are a small fixed set (NodeKindCount — typically <300),
// but ts.Node.Kind() crosses CGO and allocates a fresh Go string on
// every call; a single index walks millions of nodes, and profiling
// put node-kind GoString conversions at ~22% of all allocations. The
// table turns Type() into a slice index — zero CGO, zero allocation —
// after a one-time build per language.
//
// Keyed by the C TSLanguage pointer (stable per registered grammar);
// sync.Map because indexing runs many languages concurrently.
var kindTables sync.Map // unsafe.Pointer(*C.TSLanguage) -> []string

// internedKind returns a node's type name from the per-language table,
// building it on first use. Equivalent to ts.Node.Kind() because
// ts_node_type is itself ts_language_symbol_name(language, symbol).
// Falls back to the allocating Kind() only for an out-of-range symbol
// id, which a well-formed grammar never produces.
func internedKind(n ts.Node, key unsafe.Pointer) string {
	// key is the node's language identity, stamped at wrap time. A nil
	// key means the node was built on a path that didn't stamp it —
	// derive it the slow way so Type() still returns the right answer.
	if key == nil {
		key = unsafe.Pointer(n.Language().Inner)
	}
	id := n.KindId()
	v, ok := kindTables.Load(key)
	if !ok {
		lang := n.Language()
		cnt := lang.NodeKindCount()
		names := make([]string, cnt)
		for i := uint32(0); i < cnt; i++ {
			names[i] = lang.NodeKindForId(uint16(i))
		}
		v, _ = kindTables.LoadOrStore(key, names)
	}
	names := v.([]string)
	if int(id) < len(names) {
		return names[id]
	}
	return n.Kind()
}

// Content returns the UTF-8 text of the node as a slice of src.
func (n *Node) Content(src []byte) string { return n.inner.Utf8Text(src) }

// StartPoint returns the (row, column) position of the node start.
func (n *Node) StartPoint() Point { return fromTSPoint(n.inner.StartPosition()) }

// EndPoint returns the (row, column) position one past the node end.
func (n *Node) EndPoint() Point { return fromTSPoint(n.inner.EndPosition()) }

// StartByte returns the byte offset of the node start.
func (n *Node) StartByte() uint32 { return uint32(n.inner.StartByte()) }

// EndByte returns the byte offset one past the node end.
func (n *Node) EndByte() uint32 { return uint32(n.inner.EndByte()) }

// ChildCount returns the number of children (named + anonymous).
func (n *Node) ChildCount() uint32 { return uint32(n.inner.ChildCount()) }

// NamedChildCount returns the number of named children.
func (n *Node) NamedChildCount() uint32 { return uint32(n.inner.NamedChildCount()) }

// Child returns the i-th child (named or anonymous) or nil. It reaches the
// child through a direct C call that returns the node by value, so the result
// is bump-allocated in the arena with no go-tree-sitter heap node (newNode).
func (n *Node) Child(i int) *Node {
	if i < 0 {
		return nil
	}
	c, ok := childDirect(n.inner, i)
	if !ok {
		return nil
	}
	return n.WrapVal(c)
}

// NamedChild returns the i-th named child or nil. Like Child, it avoids
// go-tree-sitter's per-node heap allocation.
func (n *Node) NamedChild(i int) *Node {
	if i < 0 {
		return nil
	}
	c, ok := namedChildDirect(n.inner, i)
	if !ok {
		return nil
	}
	return n.WrapVal(c)
}

// NamedChildren yields n's named children, in order, walking the sibling
// chain once with a tree-sitter cursor, retaining its traversal position.
// Indexed NamedChild lookups restart navigation, although the native library
// can skip hidden subtrees using cached child counts. Their cost therefore
// depends on the tree's internal shape; they are not a universal quadratic
// baseline for this iterator.
//
// The visited set and order are identical to the NamedChild index form:
// anonymous (unnamed) children are skipped and named children are
// yielded in their natural child order.
func (n *Node) NamedChildren() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if n == nil || !n.valid {
			return
		}
		cursor := n.inner.Walk()
		defer cursor.Close()
		if !cursor.GotoFirstChild() {
			return
		}
		for {
			c := cursorCurrentNode(cursor)
			if c.IsNamed() {
				if !yield(n.WrapVal(c)) {
					return
				}
			}
			if !cursor.GotoNextSibling() {
				return
			}
		}
	}
}

// ChildByFieldName returns the first child with the given field name or nil.
// Uses a direct C call so the result is arena-allocated with no heap node.
func (n *Node) ChildByFieldName(name string) *Node {
	c, ok := childByFieldNameDirect(n.inner, name)
	if !ok {
		return nil
	}
	return n.WrapVal(c)
}

// FieldNameForChild returns the field name of the i-th child, or "" if none.
func (n *Node) FieldNameForChild(i int) string {
	if i < 0 {
		return ""
	}
	return n.inner.FieldNameForChild(uint32(i))
}

// Parent returns the parent node or nil for the root. Avoids
// go-tree-sitter's per-node heap allocation via a direct C call.
func (n *Node) Parent() *Node {
	c, ok := parentDirect(n.inner)
	if !ok {
		return nil
	}
	return n.WrapVal(c)
}

// NextSibling returns the next sibling (named or anonymous) or nil. Direct C
// call, arena-allocated result, no heap node.
func (n *Node) NextSibling() *Node {
	c, ok := nextSiblingDirect(n.inner)
	if !ok {
		return nil
	}
	return n.WrapVal(c)
}

// PrevSibling returns the previous sibling (named or anonymous) or nil.
func (n *Node) PrevSibling() *Node {
	c, ok := prevSiblingDirect(n.inner)
	if !ok {
		return nil
	}
	return n.WrapVal(c)
}

// NextNamedSibling returns the next named sibling or nil.
func (n *Node) NextNamedSibling() *Node {
	c, ok := nextNamedSiblingDirect(n.inner)
	if !ok {
		return nil
	}
	return n.WrapVal(c)
}

// PrevNamedSibling returns the previous named sibling or nil.
func (n *Node) PrevNamedSibling() *Node {
	c, ok := prevNamedSiblingDirect(n.inner)
	if !ok {
		return nil
	}
	return n.WrapVal(c)
}

// IsNamed reports whether the node corresponds to a named grammar rule.
func (n *Node) IsNamed() bool { return n.inner.IsNamed() }

// IsMissing reports whether the parser inserted this node to recover from an error.
func (n *Node) IsMissing() bool { return n.inner.IsMissing() }

// IsError reports whether this is a synthetic ERROR node.
func (n *Node) IsError() bool { return n.inner.IsError() }

// HasError reports whether the subtree under this node contains any ERROR nodes.
func (n *Node) HasError() bool { return n.inner.HasError() }

// String returns the s-expression representation of the node.
func (n *Node) String() string { return n.inner.ToSexp() }

// Id returns a stable numeric identity for the underlying node. Safe
// to use as a map key; equal across multiple wrappers of the same
// tree-sitter node. (Required because our shim creates a fresh *Node
// on every traversal, so pointer identity is not meaningful.)
func (n *Node) Id() uintptr {
	if n == nil {
		return 0
	}
	return n.inner.Id()
}

// Equal reports whether two shim Nodes wrap the same underlying
// tree-sitter node. Prefer this to `==` pointer comparison — our
// wrappers are freshly allocated on every navigation.
func (n *Node) Equal(other *Node) bool {
	if n == nil || other == nil {
		return n == other
	}
	return n.inner.Equals(other.inner)
}

// Tree wraps *ts.Tree.
type Tree struct {
	inner *ts.Tree
	arena *nodeArena // pooled; taken lazily on first RootNode, returned on Close
}

// Inner exposes the underlying *ts.Tree for internal use.
func (t *Tree) Inner() *ts.Tree { return t.inner }

// RootNode returns the root node of the parse tree, stamped with the
// tree's language so Type() lookups across the walk need no CGO call.
func (t *Tree) RootNode() *Node {
	root := t.inner.RootNode()
	if root == nil {
		return nil
	}
	// Take a pooled arena on first use and reuse it for any later RootNode
	// call on the same tree, so every node walked from this tree allocates
	// into one recycled arena. Close() returns it to the pool.
	if t.arena == nil {
		t.arena = getArena()
	}
	a := t.arena
	nn := a.alloc()
	nn.inner = *root
	nn.valid = true
	nn.langKey = unsafe.Pointer(root.Language().Inner)
	nn.arena = a
	return nn
}

// Close releases the tree's C resources and recycles its node arena.
//
// The arena is returned to the pool AFTER the C tree is freed: by the
// Tree's contract every Node wrapper is dead once Close returns, so the
// chunks the arena retains hold nothing live. putArena's reset() clears any
// stale slot, so a recycled arena never pins a closed tree.
func (t *Tree) Close() {
	if t == nil {
		return
	}
	if t.inner != nil {
		t.inner.Close()
		t.inner = nil
	}
	if t.arena != nil {
		putArena(t.arena)
		t.arena = nil
	}
}

// Parser wraps *ts.Parser with a ParseCtx that honours ctx cancellation
// via the new API's progress callback hook.
type Parser struct {
	inner *ts.Parser
}

// NewParser allocates a fresh parser. The caller must Close it.
func NewParser() *Parser { return &Parser{inner: ts.NewParser()} }

// Close releases the parser's C resources.
func (p *Parser) Close() {
	if p != nil && p.inner != nil {
		p.inner.Close()
		p.inner = nil
	}
}

// SetLanguage binds a grammar to the parser. Errors from the new API
// (incompatible ABI versions) are swallowed to keep the smacker-style
// void return; callers trust build-time grammar selection.
func (p *Parser) SetLanguage(lang *Language) { _ = p.inner.SetLanguage(lang) }

// Reset clears retained parse state (finished tree, old-tree refs,
// stack, cached token) so a parser that parsed cleanly can be reused
// for an unrelated document. It does not clear the bound language.
//
// Reset does NOT fully sanitise a parser whose parse was cancelled:
// ts_parser_reset leaves the C parser's canceled_balancing flag set,
// so a parse cancelled during the balancing phase poisons the parser
// permanently — the next Parse jumps to the balance label and aborts
// the process on an internal assertion. Discard (Close) an errored
// parser; never Reset-and-reuse it. See parser.ParseFile.
func (p *Parser) Reset() {
	if p != nil && p.inner != nil {
		p.inner.Reset()
	}
}

// ParseCtx parses src under ctx's deadline, returning a *Tree the
// caller must Close. Cancellation is polled via a ProgressCallback;
// exact-to-the-byte interruption isn't guaranteed — tree-sitter calls
// the callback at its own cadence.
func (p *Parser) ParseCtx(ctx context.Context, old *Tree, src []byte) (*Tree, error) {
	var oldTree *ts.Tree
	if old != nil {
		oldTree = old.inner
	}
	cancelled := false
	opts := &ts.ParseOptions{
		ProgressCallback: func(_ ts.ParseState) bool {
			if ctx.Err() != nil {
				cancelled = true
				return true // true aborts the parse
			}
			return false
		},
	}
	tree := p.inner.ParseWithOptions(func(offset int, _ ts.Point) []byte {
		if offset >= len(src) {
			return nil
		}
		return src[offset:]
	}, oldTree, opts)
	if tree == nil {
		if cancelled {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("tree-sitter: parse cancelled")
		}
		return nil, fmt.Errorf("tree-sitter: parse returned nil")
	}
	return &Tree{inner: tree}, nil
}

// Query is a compiled tree-sitter query. It caches CaptureNames so
// capture-id → name lookups are O(1) and don't cross into CGO.
type Query struct {
	inner *ts.Query
	names []string
}

// NewQuery compiles a query pattern against a language. Signature
// matches smacker's (pattern, lang) order, which is the argument order
// most of our language adapters expect.
func NewQuery(pattern []byte, lang *Language) (*Query, error) {
	q, qerr := ts.NewQuery(lang, string(pattern))
	if qerr != nil {
		return nil, errors.New(qerr.Error())
	}
	return &Query{inner: q, names: q.CaptureNames()}, nil
}

// Inner exposes the underlying *ts.Query for internal query runners.
func (q *Query) Inner() *ts.Query { return q.inner }

// Close releases the query's C resources.
func (q *Query) Close() {
	if q != nil && q.inner != nil {
		q.inner.Close()
		q.inner = nil
	}
}

// CaptureNameForId returns the capture name for a capture index.
func (q *Query) CaptureNameForId(id uint32) string {
	if int(id) >= len(q.names) {
		return ""
	}
	return q.names[id]
}
