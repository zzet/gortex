package store_sqlite

import (
	"sort"
	"sync"
	"sync/atomic"
)

// The fresh-rows hand-off. When a generation is built, its symbol full-text
// documents are written through BatchUpsertSymbolFTS, which holds every value a
// reader needs at that moment: the node id and tokens from the caller, the
// repository prefix it looks up, and the rowid it assigns. The first search
// after the generation is published would otherwise read those same rows back
// out of symbol_fts. So the write path keeps them, per generation, after its
// transaction commits, and TakeFreshSymbolFTSRows hands them over once.
//
// Bounds and life:
//
//   - only generations above zero (a generation being built; the base corpus
//     never);
//   - at most freshFTSMaxRowsPerGeneration rows per generation: past it the
//     generation is marked incomplete and its rows dropped;
//   - at most freshFTSMaxBytes and freshFTSMaxGenerations across generations:
//     past either, the generation that began buffering first is dropped (an
//     abandoned build, or one nobody took, ages out this way);
//   - taken once: TakeFreshSymbolFTSRows forgets the generation;
//   - any other writer of a buffered generation's documents (a repository
//     wipe, a bulk or streamed replacement) marks it incomplete. Deletes
//     through BatchDeleteSymbolFTS are applied to the buffer. Paths outside
//     this file (a generation copy or flatten) do not report here; a reader
//     therefore checks the rows against the generation's own statistics
//     before relying on them (the count and the first and last rowid).
//
// Recording costs the write path a map insert per row under one mutex, after
// the commit (BenchmarkBatchUpsertSymbolFTSWithFreshRows).

const freshFTSMaxGenerations = 64

// freshFTSMaxRowsPerGeneration bounds one generation's kept rows (a variable so
// a test can reach it).
var freshFTSMaxRowsPerGeneration = 20_000

// freshFTSMaxBytes bounds the rows kept across generations.
var freshFTSMaxBytes int64 = 32 << 20

// freshFTSOff turns the recording off (benchmark comparison; tests).
var freshFTSOff atomic.Bool

type freshFTSGeneration struct {
	rows       map[string]SymbolFTSRow // node id -> row
	bytes      int64
	incomplete bool
	seq        uint64
}

type freshFTSBuffers struct {
	mu    sync.Mutex
	gens  map[int64]*freshFTSGeneration
	bytes int64
	seq   uint64
}

var freshFTS sync.Map // *storeCore -> *freshFTSBuffers

// forgetFreshFTS drops a closing core's buffers (Store.Close), so a closed
// store keeps nothing alive.
func forgetFreshFTS(core *storeCore) {
	if core != nil {
		freshFTS.Delete(core)
	}
}

func (s *Store) freshFTSBuffers() *freshFTSBuffers {
	if s.coreless() {
		return nil
	}
	if v, ok := freshFTS.Load(s.storeCore); ok {
		return v.(*freshFTSBuffers)
	}
	v, _ := freshFTS.LoadOrStore(s.storeCore, &freshFTSBuffers{gens: make(map[int64]*freshFTSGeneration)})
	return v.(*freshFTSBuffers)
}

func freshFTSRowBytes(r SymbolFTSRow) int64 {
	return int64(len(r.NodeID)+len(r.RepoPrefix)+len(r.Tokens)) + 64
}

// recordFreshFTS keeps rows written for generation in a committed
// transaction.
func (s *Store) recordFreshFTS(generation int64, rows []SymbolFTSRow) {
	if generation <= baseViewGeneration || len(rows) == 0 || freshFTSOff.Load() {
		return
	}
	b := s.freshFTSBuffers()
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.gens[generation]
	if g == nil {
		b.seq++
		g = &freshFTSGeneration{rows: make(map[string]SymbolFTSRow, len(rows)), seq: b.seq}
		b.gens[generation] = g
	}
	if !g.incomplete {
		for _, r := range rows {
			if old, ok := g.rows[r.NodeID]; ok {
				g.bytes -= freshFTSRowBytes(old)
				b.bytes -= freshFTSRowBytes(old)
			}
			g.rows[r.NodeID] = r
			size := freshFTSRowBytes(r)
			g.bytes += size
			b.bytes += size
		}
		if len(g.rows) > freshFTSMaxRowsPerGeneration {
			b.dropRowsLocked(g)
			g.incomplete = true
		}
	}
	b.evictLocked(generation)
}

func (b *freshFTSBuffers) dropRowsLocked(g *freshFTSGeneration) {
	b.bytes -= g.bytes
	g.bytes = 0
	g.rows = nil
}

// evictLocked drops whole generations, the one that began buffering first
// first, while the buffers are over their bounds; keep is not dropped.
func (b *freshFTSBuffers) evictLocked(keep int64) {
	for b.bytes > freshFTSMaxBytes || len(b.gens) > freshFTSMaxGenerations {
		oldest, found := int64(0), false
		var seq uint64
		for gen, g := range b.gens {
			if gen == keep {
				continue
			}
			if !found || g.seq < seq {
				oldest, seq, found = gen, g.seq, true
			}
		}
		if !found {
			return
		}
		b.dropRowsLocked(b.gens[oldest])
		delete(b.gens, oldest)
	}
}

// forgetFreshFTSNodes applies a committed delete to the buffer.
func (s *Store) forgetFreshFTSNodes(generation int64, nodeIDs []string) {
	if generation <= baseViewGeneration || len(nodeIDs) == 0 {
		return
	}
	b := s.freshFTSBuffers()
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.gens[generation]
	if g == nil || g.incomplete {
		return
	}
	for _, id := range nodeIDs {
		if old, ok := g.rows[id]; ok {
			g.bytes -= freshFTSRowBytes(old)
			b.bytes -= freshFTSRowBytes(old)
			delete(g.rows, id)
		}
	}
}

// markFreshFTSIncomplete records that generation's documents were written by a
// path that does not report rows here.
func (s *Store) markFreshFTSIncomplete(generation int64) {
	if generation <= baseViewGeneration {
		return
	}
	b := s.freshFTSBuffers()
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.gens[generation]
	if g == nil {
		b.seq++
		g = &freshFTSGeneration{seq: b.seq}
		b.gens[generation] = g
	}
	b.dropRowsLocked(g)
	g.incomplete = true
	b.evictLocked(generation)
}

// TakeFreshSymbolFTSRows returns and forgets the symbol full-text rows
// written for generation through the batch path since it began building, in
// rowid order (TokenCount 0: the reader counts the tokens), and whether they
// were all written there. complete is false for a generation with no rows kept,
// one past the bounds, or one another writer touched. A caller must still
// check the rows against the generation's statistics before relying on them:
// a copy or flatten of the generation writes rows this buffer never sees.
func (s *Store) TakeFreshSymbolFTSRows(generation int64) (rows []SymbolFTSRow, complete bool) {
	b := s.freshFTSBuffers()
	if b == nil {
		return nil, false
	}
	b.mu.Lock()
	g := b.gens[generation]
	if g != nil {
		delete(b.gens, generation)
		b.bytes -= g.bytes
	}
	b.mu.Unlock()
	if g == nil || g.incomplete || len(g.rows) == 0 {
		return nil, false
	}
	rows = make([]SymbolFTSRow, 0, len(g.rows))
	for _, r := range g.rows {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RowID < rows[j].RowID })
	return rows, true
}
