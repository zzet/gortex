package graphview

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A published generation is immutable, and the one a route flip introduces is
// usually small: an edit's working-tree generation carries the rows of a few
// files. Every request over the new route nevertheless answered that
// generation's node and edge reads with SQL, one statement per node ID for the
// point reads the composition makes (NodeByID, OutEdges, InEdges), per request,
// because a GenerationLayer's point memo lives only as long as its request.
// After a publication that was about a third of a symbol search's CPU.
//
// generationRows holds such a generation's served rows in memory, loaded once
// — when the route is prewarmed before it flips (WarmRoute), so the first
// request finds them — and shared by every layer opened over the generation,
// the same way its masks are. The layer then answers its row reads from
// memory, in the order each of its SQL reads returns them:
//
//   - NodeByID: the generation's node under the ID, as GetNode returns it;
//   - OutEdges: the node's out-edges ordered by (line, id), as GetOutEdges;
//   - InEdges: the node's in-edges ordered by (kind, id), as GetInEdges;
//   - GetOutEdgesByNodeIDs: per source, the rows the store's own batch read
//     returned for it — the preload makes exactly that read for every source,
//     so a source's rows are in the order the batch statement yields them.
//
// Every answer passes through the layer's serve filter (context paths) as the
// SQL answer did, and is a copy: rows are shared across requests, and a caller
// may annotate what it is handed. A generation larger than the caps keeps the
// SQL reads. TestGenerationRowsAnswerLikeTheirSQLReads pins every answer
// against the handle's.
type generationRows struct {
	nodes    map[string]*graph.Node
	outLine  map[string][]*graph.Edge
	outBatch map[string][]*graph.Edge
	inKind   map[string][]*graph.Edge
	weight   int
}

// generationRowsMaxNodes / generationRowsMaxEdges bound the generations whose
// rows are preloaded: a working-tree generation of a few files, not a commit
// layer or a dedicated root.
const (
	generationRowsMaxNodes = 20_000
	generationRowsMaxEdges = 100_000
)

// loadGenerationRows reads a small generation's rows, or reports that the
// generation is too large (or the read failed) with a nil result.
func loadGenerationRows(ctx context.Context, handle *store_sqlite.Store) *generationRows {
	if handle == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	hasNodes, hasEdges := handle.GenerationPayloadPresence()
	nodeCount, edgeCount := 0, 0
	if hasNodes {
		nodeCount = handle.NodeCount()
	}
	if hasEdges {
		edgeCount = handle.EdgeCount()
	}
	if nodeCount > generationRowsMaxNodes || edgeCount > generationRowsMaxEdges || ctx.Err() != nil {
		return nil
	}
	rows := &generationRows{
		nodes:    make(map[string]*graph.Node, nodeCount),
		outLine:  make(map[string][]*graph.Edge),
		outBatch: make(map[string][]*graph.Edge),
		inKind:   make(map[string][]*graph.Edge),
	}
	if hasNodes {
		for _, n := range handle.AllNodes() {
			if n != nil && n.ID != "" {
				rows.nodes[n.ID] = n
			}
		}
	}
	if hasEdges {
		// AllEdges is ordered by id, so a stable sort of one node's rows by
		// line (out) or kind (in) is the (line, id) / (kind, id) order of the
		// point reads.
		var sources []string
		for _, e := range handle.AllEdges() {
			if e == nil {
				continue
			}
			if _, seen := rows.outLine[e.From]; !seen {
				sources = append(sources, e.From)
			}
			rows.outLine[e.From] = append(rows.outLine[e.From], e)
			rows.inKind[e.To] = append(rows.inKind[e.To], e)
		}
		for _, edges := range rows.outLine {
			sort.SliceStable(edges, func(i, j int) bool { return edges[i].Line < edges[j].Line })
		}
		for _, edges := range rows.inKind {
			sort.SliceStable(edges, func(i, j int) bool { return edges[i].Kind < edges[j].Kind })
		}
		// The batch read has no ORDER BY: its per-source order is the index
		// the planner seeks. Make that very read for every source, in the
		// batch size the layer's callers use.
		for start := 0; start < len(sources); start += generationRowsBatch {
			end := min(start+generationRowsBatch, len(sources))
			for id, edges := range handle.GetOutEdgesByNodeIDs(sources[start:end]) {
				rows.outBatch[id] = edges
			}
			if ctx.Err() != nil {
				return nil
			}
		}
	}
	rows.weight = len(rows.nodes) + 3*edgeCount + 1
	return rows
}

// generationRowsBatch is the batch size the preload reads out-edges in; it is
// the bounded adjacency build's batch size.
const generationRowsBatch = 512

func copyNode(n *graph.Node) *graph.Node {
	if n == nil {
		return nil
	}
	c := *n
	return &c
}

func (l *GenerationLayer) copyServedEdges(edges []*graph.Edge) []*graph.Edge {
	served := l.serveEdges(edges)
	if len(served) == 0 {
		return served
	}
	out := make([]*graph.Edge, len(served))
	for i, e := range served {
		c := *e
		out[i] = &c
	}
	return out
}

// rowsNode answers NodeByID from the preloaded rows.
func (l *GenerationLayer) rowsNode(rows *generationRows, id string) *graph.Node {
	node := rows.nodes[id]
	if !l.servesNode(node) {
		return nil
	}
	return copyNode(node)
}

// generationRowsRef is the shared slot a generation's rows are installed in.
// Every layer opened over one cached mask set holds the same slot, so rows a
// prewarm installs after a layer was opened are seen by that layer too — the
// content is the same generation's either way.
type generationRowsRef struct {
	rows atomic.Pointer[generationRows]
}

func (r *generationRowsRef) load() *generationRows {
	if r == nil {
		return nil
	}
	return r.rows.Load()
}

// preloadedRows is the layer's installed rows, nil when none.
func (l *GenerationLayer) preloadedRows() *generationRows {
	if generationRowsOff.Load() {
		return nil
	}
	return l.rowsRef.load()
}

// RowsPreloaded reports whether the layer answers its row reads from the
// generation's preloaded rows (diagnostics and tests).
func (l *GenerationLayer) RowsPreloaded() bool { return l.preloadedRows() != nil }

// generationRowsOff makes every layer read its rows with SQL, as before the
// preload. Tests only (DisableGenerationRowsForTest).
var generationRowsOff atomic.Bool

// DisableGenerationRowsForTest makes every generation layer answer its row
// reads with SQL until the returned restore runs, so a test can compare an
// answer through preloaded rows with the SQL path's. Tests only.
func DisableGenerationRowsForTest() (restore func()) {
	generationRowsOff.Store(true)
	return func() { generationRowsOff.Store(false) }
}

// preloadRows installs rows for the cached mask set of generation at
// correction epoch epoch, so every layer over it answers its row reads from
// memory. It is a no-op when the generation is not cached at that epoch,
// already has rows, or is too large. The caller reads epoch before the rows
// are loaded: rows loaded across a correction land in the older epoch's
// entry, which no later open is served from.
func (c *generationLayerCache) preloadRows(ctx context.Context, generation int64, epoch uint64, handle *store_sqlite.Store) bool {
	c.mu.Lock()
	var entry *layerCacheEntry
	for key, e := range c.entries {
		if key.generation == generation && key.correctionEpoch == epoch && e.masks != nil && e.elem != nil {
			entry = e
			break
		}
	}
	c.mu.Unlock()
	if entry == nil || entry.masks.rowsRef == nil || entry.masks.rowsRef.load() != nil {
		return false
	}
	started := time.Now()
	rows := loadGenerationRows(ctx, handle)
	if rows == nil {
		recordRowsPreload(generation, 0, time.Since(started), false)
		return false
	}
	if !entry.masks.rowsRef.rows.CompareAndSwap(nil, rows) {
		return false
	}
	recordRowsPreload(generation, rows.weight, time.Since(started), true)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[entry.key] == entry {
		entry.weight += rows.weight
		c.weight += rows.weight
		c.evictLocked()
	}
	return true
}

// RowsPreloadRecord is one preload attempt that read a generation's rows:
// installed, or declined as too large (or failed).
type RowsPreloadRecord struct {
	Generation int64     `json:"generation"`
	Installed  bool      `json:"installed"`
	Weight     int       `json:"weight"`
	LoadMs     float64   `json:"load_ms"`
	At         time.Time `json:"at"`
}

var rowsPreloadLog struct {
	mu        sync.Mutex
	installed int64
	declined  int64
	recent    []RowsPreloadRecord
}

const rowsPreloadRecent = 64

func recordRowsPreload(generation int64, weight int, took time.Duration, installed bool) {
	rowsPreloadLog.mu.Lock()
	defer rowsPreloadLog.mu.Unlock()
	if installed {
		rowsPreloadLog.installed++
	} else {
		rowsPreloadLog.declined++
	}
	rowsPreloadLog.recent = append(rowsPreloadLog.recent, RowsPreloadRecord{
		Generation: generation, Installed: installed, Weight: weight,
		LoadMs: float64(took.Microseconds()) / 1000, At: time.Now().UTC(),
	})
	if n := len(rowsPreloadLog.recent); n > rowsPreloadRecent {
		rowsPreloadLog.recent = append([]RowsPreloadRecord(nil), rowsPreloadLog.recent[n-rowsPreloadRecent:]...)
	}
}

// RowsPreloadDiagnostics reports the generation-row preloads so far: how many
// were installed, how many were declined, and the most recent attempts
// (diagnostics).
func RowsPreloadDiagnostics() (installed, declined int64, recent []RowsPreloadRecord) {
	rowsPreloadLog.mu.Lock()
	defer rowsPreloadLog.mu.Unlock()
	return rowsPreloadLog.installed, rowsPreloadLog.declined, append([]RowsPreloadRecord(nil), rowsPreloadLog.recent...)
}
