package parser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	ts "github.com/tree-sitter/go-tree-sitter"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

const parseTimeout = 5 * time.Second

// parserPool reuses *sitter.Parser instances across ParseFile calls so
// each indexer worker amortises one parser allocation instead of
// allocating + freeing a C-side TSParser per file.
//
// Only parsers whose last parse SUCCEEDED are pooled. A parse cancelled
// during tree-sitter's balancing phase leaves the C parser's internal
// canceled_balancing flag set, and ts_parser_reset does NOT clear it —
// so the next ts_parser_parse on that parser jumps straight to the
// balance label and aborts the whole process on ts_assert(finished_tree).
// ParseFile therefore Closes any parser whose parse returned an error
// instead of recycling it; Reset() cannot sanitise such a parser.
var parserPool = sync.Pool{
	New: func() any { return sitter.NewParser() },
}

// getParser checks a parser out of the pool and binds lang to it.
func getParser(lang *sitter.Language) *sitter.Parser {
	p := parserPool.Get().(*sitter.Parser)
	p.SetLanguage(lang)
	return p
}

// putParser returns a parser to the pool after a SUCCESSFUL parse.
// Reset drops the finished-tree and old-tree references so the pooled
// parser doesn't pin that memory. Never call this for a parser whose
// parse errored — Reset cannot clear the C-side canceled_balancing
// flag, so ParseFile Closes errored parsers instead.
func putParser(p *sitter.Parser) {
	if p == nil {
		return
	}
	p.Reset()
	parserPool.Put(p)
}

// CapturedNode holds information about a single captured tree-sitter node.
type CapturedNode struct {
	Text      string
	StartLine int // 0-based (tree-sitter native)
	EndLine   int // 0-based
	StartCol  int
	EndCol    int
	Node      *sitter.Node
}

// QueryResult represents a single match from a tree-sitter query.
type QueryResult struct {
	Captures map[string]*CapturedNode
}

// ErrUTF16Source is returned when the source carries a UTF-16 byte-order
// mark. Feeding UTF-16 bytes to a grammar as if they were UTF-8 produces
// NUL-interleaved garbage whose error recovery is pathological (minutes
// per file); transcoding to UTF-8 first would shift every byte coordinate
// the extractor slices from the original buffer. The right fix per caller
// is to skip the file (indexing) or transcode and keep the transcode as
// the extraction buffer.
var ErrUTF16Source = errors.New("source is UTF-16: refusing to parse NUL-interleaved bytes as UTF-8")

// hasUTF16BOM reports whether src starts with a UTF-16 byte-order mark
// (LE or BE).
func hasUTF16BOM(src []byte) bool {
	return len(src) >= 2 && ((src[0] == 0xFF && src[1] == 0xFE) || (src[0] == 0xFE && src[1] == 0xFF))
}

// ErrBinarySource is returned when the source carries a NUL byte within
// its first binarySniffBytes — the same tell git uses to classify a blob
// as binary. A NUL-bearing source is not text a text grammar can consume
// without pathological error recovery: a binary payload that a language
// extension nonetheless claimed (a tool cache .pkl, an object file)
// drives tree-sitter into pathological balancing, burning the whole
// parse budget for zero nodes. UTF-16 text is NUL-interleaved too and is
// classified here as binary — the pipeline never transcodes it, so it
// never yielded useful nodes as "text" either.
var ErrBinarySource = errors.New("source is binary: refusing to feed NUL-bearing bytes to a text grammar")

// binarySniffBytes bounds the binary content sniff. Git classifies a blob
// as binary when its first 8000 bytes contain a NUL; 8 KiB is the same
// heuristic rounded to a page-friendly bound.
const binarySniffBytes = 8192

// LooksBinary reports whether src carries a NUL byte within its first
// binarySniffBytes. Exported so the indexer's extraction admission can
// share one definition with this parse guard — a file the indexer skips
// as binary and one ParseFile refuses must be the same file.
func LooksBinary(src []byte) bool {
	if len(src) > binarySniffBytes {
		src = src[:binarySniffBytes]
	}
	return bytes.IndexByte(src, 0) >= 0
}

// ParseFile parses source bytes with the given language and returns the tree.
// The caller must call tree.Close() when done.
func ParseFile(src []byte, lang *sitter.Language) (*sitter.Tree, error) {
	// Guard before the parser pool. The UTF-16 check must run BEFORE any
	// binary/NUL sniff: a UTF-16 source's NUL-interleaved bytes would
	// otherwise be labelled "binary" instead of "UTF-16", and nothing would
	// name the real cause. The indexer's BOM-strip transform deliberately
	// leaves UTF-16 marks in place so this check can fire.
	if hasUTF16BOM(src) {
		return nil, ErrUTF16Source
	}
	// A binary payload (a tool-cache pickle, an object file) claimed by a
	// language extension is not text a grammar can consume, and its error
	// recovery is pathological. The indexer's extraction admission skips
	// these before they reach here; this backstop covers every other
	// ParseFile caller.
	if LooksBinary(src) {
		return nil, ErrBinarySource
	}
	parser := getParser(lang)
	// Pool the parser only on a clean parse. An errored parse (cancelled
	// / timed out) may have left the C parser's canceled_balancing flag
	// set, which ts_parser_reset cannot clear — recycling it would abort
	// the process on the next caller's parse. The defer Closes the
	// parser unless the success path pooled it.
	pooled := false
	defer func() {
		if !pooled {
			parser.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), parseTimeout)
	defer cancel()

	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil {
		return nil, fmt.Errorf("tree-sitter parse: %w", err)
	}
	putParser(parser)
	pooled = true
	return tree, nil
}

// PreparedQuery is a compiled tree-sitter query safe to reuse across
// many Parse calls. Compile once at extractor init and hang on to it —
// queries are thread-safe for read-only use and avoid the per-call
// CGO compile that dominated large-repo indexing.
type PreparedQuery struct {
	q *sitter.Query
	// names maps a capture index to its name, cached at compile time.
	// ts.Query.CaptureNameForId crosses CGO and allocates a fresh
	// string per call; a query firing thousands of times per file made
	// that roughly one allocation per capture across the whole index.
	names []string
}

// NewPreparedQuery compiles a tree-sitter query pattern for the given
// language. The returned *PreparedQuery is safe for concurrent use by
// many goroutines running queries via a pooled QueryCursor.
func NewPreparedQuery(pattern string, lang *sitter.Language) (*PreparedQuery, error) {
	q, err := sitter.NewQuery([]byte(pattern), lang)
	if err != nil {
		return nil, fmt.Errorf("tree-sitter query compile: %w", err)
	}
	return &PreparedQuery{q: q, names: q.Inner().CaptureNames()}, nil
}

// captureName resolves a capture index to its name from the cached
// slice, falling back to the CGO accessor only for an index outside
// the cached range (e.g. a PreparedQuery built without the cache).
func (pq *PreparedQuery) captureName(id uint32) string {
	if int(id) < len(pq.names) {
		return pq.names[id]
	}
	return pq.q.CaptureNameForId(id)
}

// MustPreparedQuery is NewPreparedQuery that panics on compile error.
// Use for extractor-internal queries that are compile-time constants:
// an error is a bug in the extractor, not runtime data, so crashing
// loud at init is the right behavior.
func MustPreparedQuery(pattern string, lang *sitter.Language) *PreparedQuery {
	q, err := NewPreparedQuery(pattern, lang)
	if err != nil {
		panic(err)
	}
	return q
}

// Close releases the underlying query. After Close the PreparedQuery
// must not be used.
func (pq *PreparedQuery) Close() {
	if pq != nil && pq.q != nil {
		pq.q.Close()
		pq.q = nil
	}
}

// cursorPool reuses *ts.QueryCursor across query runs. The new
// QueryCursor is stateless across Matches() calls — each call starts
// fresh iteration — so pooling is safe.
var cursorPool = sync.Pool{
	New: func() any { return ts.NewQueryCursor() },
}

func getCursor() *ts.QueryCursor  { return cursorPool.Get().(*ts.QueryCursor) }
func putCursor(c *ts.QueryCursor) { cursorPool.Put(c) }

// RunQuery executes a tree-sitter S-expression query against a node and
// returns all matches with their captures. The query is compiled on
// every call — use RunPrepared with a precompiled query in hot paths.
func RunQuery(pattern string, lang *sitter.Language, node *sitter.Node, src []byte) ([]QueryResult, error) {
	q, err := sitter.NewQuery([]byte(pattern), lang)
	if err != nil {
		return nil, fmt.Errorf("tree-sitter query compile: %w", err)
	}
	defer q.Close()
	return runQuery(&PreparedQuery{q: q, names: q.Inner().CaptureNames()}, node, src), nil
}

// RunPrepared executes a precompiled query against a node and returns
// all matches with their captures.
func RunPrepared(pq *PreparedQuery, node *sitter.Node, src []byte) []QueryResult {
	if pq == nil || pq.q == nil {
		return nil
	}
	return runQuery(pq, node, src)
}

// runQuery is the hot iterator: it drives the cursor, copies captures
// out of the cursor's reusable buffer before calling Next() again, and
// assembles QueryResult values the extractors expect.
func runQuery(pq *PreparedQuery, node *sitter.Node, src []byte) []QueryResult {
	if node == nil || node.Inner() == nil {
		return nil
	}
	cursor := getCursor()
	defer putCursor(cursor)

	iter := cursor.Matches(pq.q.Inner(), node.Inner(), src)
	var results []QueryResult
	for {
		match := iter.Next()
		if match == nil {
			break
		}
		if len(match.Captures) == 0 {
			continue
		}
		qr := QueryResult{Captures: make(map[string]*CapturedNode, len(match.Captures))}
		for _, c := range match.Captures {
			// c.Node is a value; copying it detaches from the cursor's
			// per-match buffer so the pointer stays valid after Next().
			nodeCopy := c.Node
			name := pq.captureName(c.Index)
			sp := nodeCopy.StartPosition()
			ep := nodeCopy.EndPosition()
			qr.Captures[name] = &CapturedNode{
				Text:      nodeCopy.Utf8Text(src),
				StartLine: int(sp.Row),
				EndLine:   int(ep.Row),
				StartCol:  int(sp.Column),
				EndCol:    int(ep.Column),
				Node:      node.WrapVal(nodeCopy),
			}
		}
		results = append(results, qr)
	}
	return results
}

// EachMatch runs a prepared query and invokes fn for each match.
//
// Hot-path contract: the QueryResult.Captures map and each *CapturedNode
// it returns are REUSED across matches within a single EachMatch call
// (and recycled into a sync.Pool when the call returns). Callers must
// consume what they need from each match synchronously inside fn and
// MUST NOT retain Captures or its *CapturedNode values past the next
// match iteration — the underlying storage is overwritten in place.
// Copy out scalars (Text / StartLine / EndLine / Node) into a caller-
// owned struct if you need to defer work to a post-pass.
//
// This contract eliminates the per-match `make(map[string]*CapturedNode)`
// and per-capture `&CapturedNode{}` allocations that dominated
// EachMatch's heap churn on large repos (5.5 GB cumulative across a
// 35k-file linux/drivers index before pooling).
func EachMatch(pq *PreparedQuery, node *sitter.Node, src []byte, fn func(QueryResult)) {
	if pq == nil || pq.q == nil {
		return
	}
	if node == nil || node.Inner() == nil {
		return
	}
	cursor := getCursor()
	defer putCursor(cursor)

	scratch := getMatchScratch()
	defer putMatchScratch(scratch)

	iter := cursor.Matches(pq.q.Inner(), node.Inner(), src)
	for {
		match := iter.Next()
		if match == nil {
			break
		}
		if len(match.Captures) == 0 {
			continue
		}

		// Reset the per-match views in place. Map storage and node
		// slab survive across matches within this call and across
		// pooled calls; clear() is O(n) but n is tiny (one entry per
		// capture, typically < 8).
		clear(scratch.captures)
		if cap(scratch.nodes) < len(match.Captures) {
			// The old slab is about to become unreachable. Clear its full
			// capacity first so stale Text strings and arena-backed Node
			// pointers do not survive until the next GC cycle.
			clear(scratch.nodes[:cap(scratch.nodes)])
			scratch.nodes = make([]CapturedNode, len(match.Captures))
		} else {
			scratch.nodes = scratch.nodes[:len(match.Captures)]
		}

		for i, c := range match.Captures {
			nodeCopy := c.Node
			name := pq.captureName(c.Index)
			sp := nodeCopy.StartPosition()
			ep := nodeCopy.EndPosition()
			scratch.nodes[i] = CapturedNode{
				Text:      nodeCopy.Utf8Text(src),
				StartLine: int(sp.Row),
				EndLine:   int(ep.Row),
				StartCol:  int(sp.Column),
				EndCol:    int(ep.Column),
				Node:      node.WrapVal(nodeCopy),
			}
			scratch.captures[name] = &scratch.nodes[i]
		}
		fn(QueryResult{Captures: scratch.captures})
	}
}

// matchScratch holds the reusable storage backing a single EachMatch
// call's captures map and *CapturedNode pointers. Pooled across calls
// to amortise the map + slab allocations that dominated EachMatch heap
// churn before the hot-path contract was tightened.
type matchScratch struct {
	captures map[string]*CapturedNode
	nodes    []CapturedNode
}

var matchScratchPool = sync.Pool{
	New: func() any {
		return &matchScratch{
			captures: make(map[string]*CapturedNode, 8),
			nodes:    make([]CapturedNode, 0, 8),
		}
	},
}

func getMatchScratch() *matchScratch {
	return matchScratchPool.Get().(*matchScratch)
}

func putMatchScratch(s *matchScratch) {
	if s == nil {
		return
	}
	clear(s.captures)
	// len only covers the last match. Clear the full backing slab because
	// earlier, larger matches may have left Text and Node fields beyond len;
	// retaining those through sync.Pool pins source strings and tree arenas.
	clear(s.nodes[:cap(s.nodes)])
	// Drop pathological growth so one outlier file (eg autogenerated
	// 50k-match header) doesn't pin a huge buffer across the rest of
	// the run. 256 captures covers normal kernel .c — anything bigger
	// re-allocates next time.
	if cap(s.nodes) > 256 {
		s.nodes = nil
	} else {
		s.nodes = s.nodes[:0]
	}
	matchScratchPool.Put(s)
}

// NOTE: a previous experiment added a per-EachMatch string intern table
// to dedupe Utf8Text copies of repeated identifiers (kmalloc, printk,
// etc. in kernel C). It REGRESSED wall time on linux/drivers by
// 11–13s (62.6s clear-on-put / 64.6s preserve-on-put vs the 51.4s
// pool-only baseline). The map machinery cost per-capture exceeded the
// alloc savings: the unique-identifier rate is too high (most captures
// are file-local function/var names, not the small recurring helper
// set), and preserve-across-calls made it worse as the growing map
// slowed every lookup. The text copy in Utf8Text is fine as-is — keep
// pooling, drop interning.
