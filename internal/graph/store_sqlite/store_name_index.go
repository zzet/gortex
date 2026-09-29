package store_sqlite

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

// generationNameIndexMaxNodes bounds the generations whose names are held in
// memory. A published derived generation is immutable, so its (id, name) pairs
// can be read once and every substring lookup answered by a scan of memory
// instead of a LIKE over every row of the generation — which has no index to
// use (a leading % defeats nodes_by_name) and on the live store costs seconds
// per composed-view search, because a dedicated root and its deltas hold tens
// of thousands of rows each. Larger generations keep the SQL scan.
const generationNameIndexMaxNodes = 400_000

// generationNameIndexFetchChunk is how many matching rows one payload read
// fetches.
const generationNameIndexFetchChunk = 256

// generationNameEntry is one named row of a generation, kept small: the id the
// row is fetched by, its lower-cased name, and — only for a name with non-ASCII
// bytes, where SQLite's ASCII-only LIKE folding and strings.ToLower disagree —
// the name as stored. (The id, not the rowid: a fresh schema declares nodes
// WITHOUT ROWID.)
type generationNameEntry struct {
	id   string
	fold string
	raw  string
}

// likeFold is the name under SQLite's LIKE folding (ASCII letters only).
func (e *generationNameEntry) likeFold() string {
	if e.raw != "" {
		return asciiLower(e.raw)
	}
	return e.fold
}

// generationNameIndexBudget bounds the entries every name index in the process
// holds together (~100 bytes each), so the memo never grows without limit as
// generations accumulate. An index is released with its generation's seal;
// the budget is returned when the index is collected.
const generationNameIndexBudget = 1_000_000

var generationNameIndexHeld atomic.Int64

// generationNameIndex is a sealed generation's named rows in id order — the
// order every substring lookup returns.
type generationNameIndex struct {
	entries []generationNameEntry
}

// generationNameIndexSlot hangs one lazily built index off a generation's
// shared payloadSeal, so every handle on the generation shares it and it is
// dropped with the seal when the generation is retired.
type generationNameIndexSlot struct {
	mu    sync.Mutex
	index *generationNameIndex
	// refused records that the generation is too large to index.
	refused bool
	// building records a background build in flight.
	building bool
}

// generationNameIndexSyncRows is the largest generation whose index is built
// inline, by the request that first needs it: a working-tree generation (a few
// hundred rows) costs less to index than to scan once. A larger one — a
// dedicated root holds ~180k rows, and reading them is seconds — is indexed in
// the background while that first request, and any other until the index is
// ready, is answered by the SQL scan exactly as before, so no request ever
// waits longer than it did without the index. A variable so tests can force
// the background path.
var generationNameIndexSyncRows = 5000

// sealedNameIndex returns the generation's name index when this handle reads a
// published, immutable derived generation and the index is ready. ok=false
// means the caller must use the SQL scan: the base corpus (mutable), a
// generation still building or being retired, one too large to hold, an index
// still being built in the background, or a read that failed.
func (s *Store) sealedNameIndex(ctx context.Context) (*generationNameIndex, bool) {
	seal, ok := s.sealedGeneration(s.viewGen)
	if !ok {
		return nil, false
	}
	slot := &seal.names
	slot.mu.Lock()
	if slot.index != nil || slot.refused || slot.building {
		index := slot.index
		slot.mu.Unlock()
		return index, index != nil
	}
	slot.building = true
	slot.mu.Unlock()

	small, err := s.generationRowsAtMost(ctx, generationNameIndexSyncRows)
	if err != nil {
		slot.finish(nil, false)
		return nil, false
	}
	if !small {
		handle := *s
		go func() {
			index, fits, err := handle.loadGenerationNameIndex(context.Background())
			slot.finish(keptNameIndex(seal, index, fits, err), err == nil && !fits)
		}()
		return nil, false
	}
	index, fits, err := s.loadGenerationNameIndex(ctx)
	index = keptNameIndex(seal, index, fits, err)
	slot.finish(index, err == nil && !fits)
	return index, index != nil
}

// keptNameIndex is the index a finished build may keep. Publication sealed the
// payload before the catalog said so, but a retirement may have begun while
// the rows were read; only a generation still published keeps its index.
func keptNameIndex(seal *payloadSeal, index *generationNameIndex, fits bool, err error) *generationNameIndex {
	if err != nil || !fits || index == nil || seal.state.Load() == payloadSealRetired {
		return nil
	}
	return index
}

// finish records a build's outcome. A build that failed for a transient
// reason (a cancelled request, a read error) leaves the slot empty so a later
// request builds again; a generation that does not fit is remembered.
func (slot *generationNameIndexSlot) finish(index *generationNameIndex, tooLarge bool) {
	slot.mu.Lock()
	defer slot.mu.Unlock()
	slot.building = false
	slot.index = index
	slot.refused = index == nil && tooLarge
}

// generationRowsAtMost reports whether the generation holds at most n named
// rows, reading no more than n+1 index entries.
func (s *Store) generationRowsAtMost(ctx context.Context, n int) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (SELECT 1 FROM nodes WHERE view_gen = ? LIMIT ?)`, s.viewGen, n+1).Scan(&count); err != nil {
		return false, err
	}
	return count <= n, nil
}

// sealedGeneration returns the shared seal of generation when it is a
// published, immutable derived generation.
func (s *Store) sealedGeneration(generation int64) (*payloadSeal, bool) {
	if s.coreless() || generation <= baseViewGeneration {
		return nil, false
	}
	seal := s.seal
	if seal == nil || s.viewGen != generation {
		seal = s.payloadSealIfPresent(generation)
	}
	if seal != nil {
		switch seal.state.Load() {
		case payloadSealSealed:
			return seal, true
		case payloadSealUnknown:
		default:
			return nil, false
		}
	}
	// Read-only probe: the write gate owns the cached verdict, so this asks
	// the catalog without storing anything. The seal is minted only for a
	// generation the catalog says is published, so an id from outside the
	// lifecycle never grows the seal map.
	probe := *s
	probe.viewGen = generation
	verdict, _, found, err := probe.catalogSealVerdict()
	if err != nil || !found || verdict != payloadSealSealed {
		return nil, false
	}
	if seal == nil {
		seal = s.payloadSealFor(generation)
	}
	return seal, seal != nil
}

// loadGenerationNameIndex reads every named row of the generation in id order.
func (s *Store) loadGenerationNameIndex(ctx context.Context) (*generationNameIndex, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name FROM nodes WHERE view_gen = ? AND name <> '' ORDER BY id`, s.viewGen)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	index := &generationNameIndex{}
	for rows.Next() {
		if len(index.entries) >= generationNameIndexMaxNodes {
			return nil, false, nil
		}
		var (
			entry generationNameEntry
			name  string
		)
		if err := rows.Scan(&entry.id, &name); err != nil {
			return nil, false, err
		}
		entry.fold = strings.ToLower(name)
		if !isASCII(name) {
			entry.raw = name
		}
		index.entries = append(index.entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	// Entries stay in id order (the ORDER BY), which is the order every
	// lookup answers in; the rows are fetched back ORDER BY id as well.
	held := int64(len(index.entries))
	if generationNameIndexHeld.Add(held) > generationNameIndexBudget {
		generationNameIndexHeld.Add(-held)
		return nil, false, nil
	}
	runtime.AddCleanup(index, func(n int64) { generationNameIndexHeld.Add(-n) }, held)
	return index, true, nil
}

// asciiLower folds ASCII letters only — SQLite's LIKE folding.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// likeContains is `name LIKE '%' || escaped(substr) || '%'` under SQLite's
// default (ASCII-only) case folding.
func likeContains(entry *generationNameEntry, foldedSubstr string) bool {
	return strings.Contains(entry.likeFold(), foldedSubstr)
}

// matchingIDs returns, in id order, the ids whose entry satisfies match, up
// to limit (0 = no limit).
func (idx *generationNameIndex) matchingIDs(ctx context.Context, limit int, match func(*generationNameEntry) bool) ([]string, error) {
	var ids []string
	for i := range idx.entries {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if match(&idx.entries[i]) {
			ids = append(ids, idx.entries[i].id)
			if limit > 0 && len(ids) >= limit {
				break
			}
		}
	}
	return ids, nil
}

// nodesByIDsInOrder reads the rows for ids (already in id order) with the
// substring lookups' own projection, in id order.
func (s *Store) nodesByIDsInOrder(ctx context.Context, ids []string) ([]*graph.Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE view_gen = ? AND id IN (?` +
		strings.Repeat(`,?`, len(ids)-1) + `) ORDER BY id`
	args := make([]any, 0, len(ids)+1)
	args = append(args, s.viewGen)
	for _, id := range ids {
		args = append(args, id)
	}
	return s.queryNodesSQLContext(ctx, q, args...)
}

// findNodesByNameContainingIndexed is FindNodesByNameContainingContext over a
// sealed generation's name index: the same rows, in the same order, with the
// same limit.
func (s *Store) findNodesByNameContainingIndexed(ctx context.Context, index *generationNameIndex, substr string, limit int) ([]*graph.Node, error) {
	folded := asciiLower(substr)
	ids, err := index.matchingIDs(ctx, limit, func(entry *generationNameEntry) bool {
		return likeContains(entry, folded)
	})
	if err != nil {
		return nil, err
	}
	var out []*graph.Node
	for start := 0; start < len(ids); start += generationNameIndexFetchChunk {
		end := min(start+generationNameIndexFetchChunk, len(ids))
		nodes, err := s.nodesByIDsInOrder(ctx, ids[start:end])
		if err != nil {
			return out, err
		}
		out = append(out, nodes...)
	}
	return out, ctx.Err()
}

// visitNodesByNameContainingFoldedIndexed is
// VisitNodesByNameContainingFoldedContext over a sealed generation's name
// index: every row whose lower-cased name contains the lower-cased needle, in
// id order, stopping when yield returns false.
func (s *Store) visitNodesByNameContainingFoldedIndexed(ctx context.Context, index *generationNameIndex, substr string, yield func(*graph.Node) bool) error {
	needle := strings.ToLower(substr)
	ids, err := index.matchingIDs(ctx, 0, func(entry *generationNameEntry) bool {
		return strings.Contains(entry.fold, needle)
	})
	if err != nil {
		return err
	}
	for start := 0; start < len(ids); start += generationNameIndexFetchChunk {
		end := min(start+generationNameIndexFetchChunk, len(ids))
		nodes, err := s.nodesByIDsInOrder(ctx, ids[start:end])
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if node == nil || node.Name == "" || !strings.Contains(strings.ToLower(node.Name), needle) {
				continue
			}
			if !yield(node) {
				return ctx.Err()
			}
		}
	}
	return ctx.Err()
}
