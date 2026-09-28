package mcp

import (
	"container/list"
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// The rerank's bounded centrality reads a candidate-seeded neighbourhood —
// up to rerankBoundedMaxNodes nodes and rerankBoundedMaxEdges call/reference
// edges — through the request's reader. On a routed checkout that reader is a
// stack of composed levels (a dedicated root, its commit-side ancestry, the
// working-tree chain), and every level answered every batch with SQL: one
// search paid the whole neighbourhood's reads at every level, and the next
// search after a publication paid them all again.
//
// A stacked adjacency memo removes that. It keeps, per composed LEVEL of a
// routed stack, exactly the two answers the bounded build asks for — whether a
// node exists, and a node's call/reference out-edges — and the build reads
// through a reader chain that composes each level over the memo of the level
// below with the same graph.OverlaidView the view itself uses.
//
// A level's content is a pure function of the generations composed up to it:
// every generation in a routed stack is published and immutable, generation
// ids are never reused (the catalog allocates them with AUTOINCREMENT), and
// the dedicated root at the bottom is a pinned generation too. So a level's
// memo is valid for as long as it is kept, and it is shared by every view
// whose stack starts with the same generations. That is the invalidation rule:
// a route flip names a new top generation, and with it a new top level whose
// memo starts empty, while every level below it is the same level the previous
// route composed, already warm. The first search after a publication reads the
// new top generation once, for the ids it is asked about, and nothing below.
//
// Exactness. The chain is rebuilt from the view's generation sources in the
// order Materializer.assemble composes them, and is used only when the view's
// own reader has the same depth and the same root handle. Each memo returns,
// for every id it is asked about, what its level would have answered, reduced
// to what the build consumes: node presence (the build only tests a node for
// nil) and the call/reference edges in the order the level returned them
// (per-edge visibility and concatenation commute with that kind filter). The
// top level keeps the read variant the old path used there — a bare root
// handle answers the bounded build through its cancellable light-row read, a
// composed view through the full-row batch — so edge weights are identical.
// TestStackedAdjacencyMemoRanksLikeTheViewReader pins the result against the
// uncached build.
//
// Nothing that did not finish is kept: a batch whose read returned an error or
// whose request ended is returned to its caller and not memoized. The
// full-row batches report no errors at all, so every level is also dropped
// after stackAdjacencyMemoTTL.
//
// The same levels also serve the rerank's batched candidate fan-in / fan-out
// read (memoEdgeLevelReader, wired as rerank.Context.EdgeBatches), with whole
// edge lists of every kind in both directions.

// stackAdjacencyMemoMaxLevels bounds how many composed levels are retained. A
// routed stack is at most a handful deep, so this keeps several checkouts'
// stacks plus the levels of recently superseded routes.
const stackAdjacencyMemoMaxLevels = 48

// stackAdjacencyMemoMaxRows bounds one level's retained rows (node presence
// entries plus edges). A level that would grow past it is reset; the bound
// keeps a level to a few tens of megabytes.
const stackAdjacencyMemoMaxRows = 400_000

// stackedAdjacencyDisabledForTest routes the bounded build through the view's
// own reader, the uncached path the memo must equal. Tests only.
var stackedAdjacencyDisabledForTest bool

// answerPresentNode stands in for a node the memo knows exists. The bounded
// build and the composed levels above a memo only test a node for presence.
var answerPresentNode = &graph.Node{}

type stackAdjacencyLevel struct {
	mu    sync.RWMutex
	nodes map[string]bool
	full  map[string][]*graph.Edge
	light map[string][]*graph.Edge
	// allOut and allIn hold a candidate's whole edge lists (every kind),
	// for the rerank's batched fan-in / fan-out read.
	allOut map[string][]*graph.Edge
	allIn  map[string][]*graph.Edge
	rows   int
}

func newStackAdjacencyLevel() *stackAdjacencyLevel {
	l := &stackAdjacencyLevel{}
	l.resetLocked()
	return l
}

func (l *stackAdjacencyLevel) resetLocked() {
	l.nodes = make(map[string]bool)
	l.full = make(map[string][]*graph.Edge)
	l.light = make(map[string][]*graph.Edge)
	l.allOut = make(map[string][]*graph.Edge)
	l.allIn = make(map[string][]*graph.Edge)
	l.rows = 0
}

// admitLocked charges rows to the level, resetting it first when the bound
// would be crossed.
func (l *stackAdjacencyLevel) admitLocked(rows int) {
	if l.rows+rows <= stackAdjacencyMemoMaxRows {
		l.rows += rows
		return
	}
	l.resetLocked()
	l.rows = rows
}

// stackAdjacencyMemoTTL bounds how long one level's memo is trusted. The
// content is immutable, but the full-row batch reads the memo wraps report no
// errors (a store read that failed answers empty), so a level is re-read from
// scratch after this long rather than keeping such an answer for the life of
// its generation.
const stackAdjacencyMemoTTL = 10 * time.Minute

type stackAdjacencyMemoEntry struct {
	key     string
	level   *stackAdjacencyLevel
	elem    *list.Element
	created time.Time
}

// stackAdjacencyMemo is the per-server set of level memos, least recently used
// first out.
type stackAdjacencyMemo struct {
	mu      sync.Mutex
	entries map[string]*stackAdjacencyMemoEntry
	lru     *list.List

	// Counters for tests and diagnostics.
	nodeHits, nodeMisses atomic.Int64
	edgeHits, edgeMisses atomic.Int64
}

func newStackAdjacencyMemo() *stackAdjacencyMemo {
	return &stackAdjacencyMemo{entries: make(map[string]*stackAdjacencyMemoEntry), lru: list.New()}
}

func (m *stackAdjacencyMemo) level(key string) *stackAdjacencyLevel {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if entry, ok := m.entries[key]; ok {
		if now.Sub(entry.created) < stackAdjacencyMemoTTL {
			m.lru.MoveToFront(entry.elem)
			return entry.level
		}
		m.lru.Remove(entry.elem)
		delete(m.entries, key)
	}
	entry := &stackAdjacencyMemoEntry{key: key, level: newStackAdjacencyLevel(), created: now}
	entry.elem = m.lru.PushFront(entry)
	m.entries[key] = entry
	for m.lru.Len() > stackAdjacencyMemoMaxLevels {
		back := m.lru.Back()
		m.lru.Remove(back)
		delete(m.entries, back.Value.(*stackAdjacencyMemoEntry).key)
	}
	return entry.level
}

// counts returns the memo's hit and miss counters: node hits, node misses,
// edge hits, edge misses (each in ids).
func (m *stackAdjacencyMemo) counts() (int64, int64, int64, int64) {
	return m.nodeHits.Load(), m.nodeMisses.Load(), m.edgeHits.Load(), m.edgeMisses.Load()
}

// stackAdjacencyMemoFor returns the server's memo, creating it on first use.
func (s *Server) stackAdjacencyMemoFor() *stackAdjacencyMemo {
	s.stackAdjacencyOnce.Do(func() { s.stackAdjacency = newStackAdjacencyMemo() })
	return s.stackAdjacency
}

// memoLevelReader is one composed level read through its memo. It is only ever
// handed to the bounded adjacency build (directly, or as the base of the next
// composed level), never to a request as a general reader: its node answers
// are presence markers and its edge answers are the call/reference subset.
type memoLevelReader struct {
	graph.Reader
	memo  *stackAdjacencyMemo
	level *stackAdjacencyLevel
	// light is set on a top level whose uncached read was the store's
	// cancellable light-row batch (a stack that is a bare root handle).
	light bool
}

type memoContextEdgeReader interface {
	GetOutEdgesByNodeIDsContext(context.Context, []string, int) (map[string][]*graph.Edge, bool, error)
}

func adjacencyEdgeKind(e *graph.Edge) bool {
	return e != nil && (e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences)
}

func adjacencyEdgesOnly(edges []*graph.Edge) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range edges {
		if adjacencyEdgeKind(e) {
			out = append(out, e)
		}
	}
	return out
}

func uniqueNonEmptyIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (r *memoLevelReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	out, _ := r.GetNodesByIDsContext(context.Background(), ids)
	return out
}

// GetNodesByIDsContext answers presence for every id, reading only the ids the
// level has not answered before.
func (r *memoLevelReader) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	uniq := uniqueNonEmptyIDs(ids)
	out := make(map[string]*graph.Node, len(uniq))
	var missing []string
	r.level.mu.RLock()
	for _, id := range uniq {
		present, known := r.level.nodes[id]
		if !known {
			missing = append(missing, id)
			continue
		}
		if present {
			out[id] = answerPresentNode
		}
	}
	r.level.mu.RUnlock()
	r.memo.nodeHits.Add(int64(len(uniq) - len(missing)))
	r.memo.nodeMisses.Add(int64(len(missing)))
	if len(missing) == 0 {
		return out, nil
	}
	var (
		fetched map[string]*graph.Node
		err     error
	)
	if contextual, ok := r.Reader.(boundedReadsContextNodes); ok {
		fetched, err = contextual.GetNodesByIDsContext(ctx, missing)
	} else {
		fetched = r.Reader.GetNodesByIDs(missing)
	}
	for _, id := range missing {
		if fetched[id] != nil {
			out[id] = answerPresentNode
		}
	}
	if err != nil || ctx.Err() != nil {
		// A partial answer is the caller's, never the memo's.
		return out, err
	}
	r.level.mu.Lock()
	r.level.admitLocked(len(missing))
	for _, id := range missing {
		r.level.nodes[id] = fetched[id] != nil
	}
	r.level.mu.Unlock()
	return out, nil
}

// GetOutEdgesByNodeIDs is the composed level's full-row batch, reduced to the
// call/reference edges.
func (r *memoLevelReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	out, _ := r.outEdges(context.Background(), ids, false)
	return out
}

// GetOutEdgesByNodeIDsContext is what the request-bound reader calls on the
// top level. A level whose uncached read was the store's cancellable light
// batch keeps that read; any other level answers from its full-row batch, as
// the composed view it stands for did.
func (r *memoLevelReader) GetOutEdgesByNodeIDsContext(ctx context.Context, ids []string, _ int) (map[string][]*graph.Edge, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	out, err := r.outEdges(ctx, ids, r.light)
	return out, false, err
}

func (r *memoLevelReader) outEdges(ctx context.Context, ids []string, light bool) (map[string][]*graph.Edge, error) {
	uniq := uniqueNonEmptyIDs(ids)
	out := make(map[string][]*graph.Edge, len(uniq))
	var missing []string
	r.level.mu.RLock()
	cache := r.level.full
	if light {
		cache = r.level.light
	}
	for _, id := range uniq {
		edges, known := cache[id]
		if !known {
			missing = append(missing, id)
			continue
		}
		out[id] = edges
	}
	r.level.mu.RUnlock()
	r.memo.edgeHits.Add(int64(len(uniq) - len(missing)))
	r.memo.edgeMisses.Add(int64(len(missing)))
	if len(missing) == 0 {
		return out, nil
	}
	var (
		fetched map[string][]*graph.Edge
		err     error
	)
	contextual, hasContext := r.Reader.(memoContextEdgeReader)
	if light && hasContext {
		fetched, _, err = contextual.GetOutEdgesByNodeIDsContext(ctx, missing, boundedReadsNoEdgeBudget)
	} else {
		fetched = r.Reader.GetOutEdgesByNodeIDs(missing)
	}
	filtered := make(map[string][]*graph.Edge, len(missing))
	rows := 0
	for _, id := range missing {
		edges := adjacencyEdgesOnly(fetched[id])
		filtered[id] = edges
		out[id] = edges
		rows += len(edges) + 1
	}
	if err != nil || ctx.Err() != nil {
		return out, err
	}
	r.level.mu.Lock()
	r.level.admitLocked(rows)
	cache = r.level.full
	if light {
		cache = r.level.light
	}
	for id, edges := range filtered {
		cache[id] = edges
	}
	r.level.mu.Unlock()
	return out, nil
}

// stackedAdjacencyReader returns the memoized reader chain the bounded
// centrality build reads through, or nil when the request's view is not a
// stack the memo can stand for (see stackedLevels).
func (s *Server) stackedAdjacencyReader(ctx context.Context) graph.Reader {
	return s.stackedLevels(ctx, func(underlying graph.Reader, memo *stackAdjacencyMemo, level *stackAdjacencyLevel, top, bare bool) graph.Reader {
		return &memoLevelReader{Reader: underlying, memo: memo, level: level, light: top && bare}
	})
}

// stackedEdgeBatches returns the memoized reader the rerank's batched
// candidate edge reads (rerank.Context.EdgeBatches) go through, or nil.
func (s *Server) stackedEdgeBatches(ctx context.Context) *memoEdgeLevelReader {
	top := s.stackedLevels(ctx, func(underlying graph.Reader, memo *stackAdjacencyMemo, level *stackAdjacencyLevel, _, _ bool) graph.Reader {
		return &memoEdgeLevelReader{Reader: underlying, memo: memo, level: level}
	})
	if top == nil {
		return nil
	}
	return top.(*memoEdgeLevelReader)
}

// stackedLevels rebuilds the request's routed stack as a chain of memoized
// levels, or returns nil when the view is not a stack the memo can stand for:
// no materialized routed checkout, a stack that composes the mutable base
// corpus, a reader that is not the materialized stack itself (an
// editor-buffer overlay, a narrowed base), or a stack whose shape does not
// match its sources. wrap builds one level's memoized reader over the level's
// uncached composition; top marks the level the caller reads, bare a stack
// that is the root handle alone.
func (s *Server) stackedLevels(ctx context.Context, wrap func(underlying graph.Reader, memo *stackAdjacencyMemo, level *stackAdjacencyLevel, top, bare bool) graph.Reader) graph.Reader {
	if stackedAdjacencyDisabledForTest || OverlayViewFromContext(ctx) != nil {
		return nil
	}
	view := requestViewFromContext(ctx)
	if !view.readsOwnCheckout() || view.materialized == nil {
		return nil
	}
	repoView := view.materialized
	if repoView.ComposesBaseCorpus() || repoView.Reader == nil || view.reader != repoView.Reader {
		return nil
	}
	sources := repoView.GenerationSources()
	if len(sources) == 0 || sources[0].Handle == nil {
		return nil
	}
	// The view's reader must be exactly len(sources)-1 composed levels over
	// the root handle, as Materializer.assemble builds it.
	reader := repoView.Reader
	for depth := len(sources) - 1; depth > 0; depth-- {
		composed, ok := reader.(*graph.OverlaidView)
		if !ok {
			return nil
		}
		reader = composed.Base()
	}
	if root, ok := reader.(interface{ ViewGeneration() int64 }); !ok || reader != graph.Reader(sources[0].Handle) || root.ViewGeneration() != sources[0].Generation {
		return nil
	}
	for _, source := range sources[1:] {
		if source.Layer == nil {
			return nil
		}
	}

	memo := s.stackAdjacencyMemoFor()
	var key strings.Builder
	key.WriteString(repoView.ID.RepoPrefix)
	var level graph.Reader
	for index, source := range sources {
		key.WriteByte('/')
		key.WriteString(strconv.FormatInt(source.Generation, 10))
		var underlying graph.Reader
		if index == 0 {
			underlying = source.Handle
		} else {
			underlying = graph.NewOverlaidViewWithLayer(level, source.Layer)
		}
		level = wrap(underlying, memo, memo.level(key.String()), index == len(sources)-1, len(sources) == 1)
	}
	return level
}

// memoEdgeLevelReader is one composed level whose batched whole edge lists
// (every kind, both directions) are read through its memo. It serves the
// rerank's candidate fan-in / fan-out batch; every other read is the level's
// own.
type memoEdgeLevelReader struct {
	graph.Reader
	memo  *stackAdjacencyMemo
	level *stackAdjacencyLevel
}

func (r *memoEdgeLevelReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return r.batch(ids, false)
}

func (r *memoEdgeLevelReader) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return r.batch(ids, true)
}

func (r *memoEdgeLevelReader) batch(ids []string, incoming bool) map[string][]*graph.Edge {
	uniq := uniqueNonEmptyIDs(ids)
	out := make(map[string][]*graph.Edge, len(uniq))
	var missing []string
	r.level.mu.RLock()
	cache := r.level.allOut
	if incoming {
		cache = r.level.allIn
	}
	for _, id := range uniq {
		edges, known := cache[id]
		if !known {
			missing = append(missing, id)
			continue
		}
		out[id] = edges
	}
	r.level.mu.RUnlock()
	r.memo.edgeHits.Add(int64(len(uniq) - len(missing)))
	r.memo.edgeMisses.Add(int64(len(missing)))
	if len(missing) == 0 {
		return out
	}
	var fetched map[string][]*graph.Edge
	if incoming {
		fetched = r.Reader.GetInEdgesByNodeIDs(missing)
	} else {
		fetched = r.Reader.GetOutEdgesByNodeIDs(missing)
	}
	rows := 0
	for _, id := range missing {
		out[id] = fetched[id]
		rows += len(fetched[id]) + 1
	}
	r.level.mu.Lock()
	r.level.admitLocked(rows)
	cache = r.level.allOut
	if incoming {
		cache = r.level.allIn
	}
	for _, id := range missing {
		cache[id] = fetched[id]
	}
	r.level.mu.Unlock()
	return out
}
