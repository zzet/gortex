package mcp

import (
	"context"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

type projectionSearchName struct{ id, name, qualified string }

// Built with the immutable file layer, never by a reader or a second parse.
// Rank is local to this name corpus; the engine's existing ranked-source
// merge keeps it separate from the base corpus's BM25 scores.
type projectionNameSearch struct {
	names        []projectionSearchName
	chargedBytes int64
}

func newProjectionNameSearch(nodes []*graph.Node) *projectionNameSearch {
	out := &projectionNameSearch{names: make([]projectionSearchName, 0, len(nodes))}
	for _, node := range nodes {
		if node == nil || node.Kind == graph.KindFile || node.Kind == graph.KindImport {
			continue
		}
		out.names = append(out.names, projectionSearchName{node.ID, strings.ToLower(node.Name), strings.ToLower(node.QualName)})
	}
	// Include the cache pointer/header and full backing capacity. Double
	// array/string bytes plus small-allocation slack conservatively cover
	// allocator rounding; already-lowercase shared strings are overcharged.
	out.chargedBytes = int64(unsafe.Sizeof(out)) + int64(unsafe.Sizeof(*out)) + 2*int64(cap(out.names))*int64(unsafe.Sizeof(projectionSearchName{}))
	for _, name := range out.names {
		for _, text := range []string{name.name, name.qualified} {
			if text != "" {
				out.chargedBytes += 2*int64(len(text)) + 16
			}
		}
	}
	return out
}

func (*projectionNameSearch) Add(string, ...string) {}
func (*projectionNameSearch) Remove(string)         {}
func (*projectionNameSearch) Close()                {}
func (s *projectionNameSearch) Count() int          { return len(s.names) }
func (s *projectionNameSearch) Search(q string, limit int) []search.SearchResult {
	return s.SearchContext(context.Background(), q, limit)
}

func (s *projectionNameSearch) SearchContext(ctx context.Context, q string, limit int) []search.SearchResult {
	if ctx.Err() != nil || limit <= 0 {
		return nil
	}
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	terms := search.TokenizeQuery(q)
	var hits []search.SearchResult
	for _, name := range s.names {
		if ctx.Err() != nil {
			return nil
		}
		score := 0.0
		switch {
		case name.name == q || name.qualified == q:
			score = 3
		case strings.HasPrefix(name.name, q) || strings.HasPrefix(name.qualified, q):
			score = 2
		case strings.Contains(name.name, q) || strings.Contains(name.qualified, q):
			score = 1
		default:
			for _, term := range terms {
				if strings.Contains(name.name, term) || strings.Contains(name.qualified, term) {
					score += 0.25 / float64(max(1, len(terms)))
				}
			}
		}
		if score > 0 {
			hits = append(hits, search.SearchResult{ID: name.id, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	return hits[:min(limit, len(hits))]
}

type pendingSearchKey struct{}

type pendingSearchSnapshot struct{ candidates []query.ViewLayerSource }

type pendingSymbolCaptureKey struct{}

// A capture belongs to one registered tools/call, never to a reusable caller
// context. It certifies only the immutable pending layers selected by that
// call; canonical, route, cursor and cancellation gates remain independent.
type pendingSymbolCapture struct {
	mu       sync.RWMutex
	facts    *requestProjectionFacts
	entries  map[string]*projectionEntry
	disabled bool
}

func withPendingSymbolCapture(ctx context.Context, tool string) context.Context {
	if tool != "search_symbols" {
		return ctx
	}
	return context.WithValue(ctx, pendingSymbolCaptureKey{}, &pendingSymbolCapture{})
}

func sealPendingSymbolCapture(ctx context.Context) {
	capture, _ := ctx.Value(pendingSymbolCaptureKey{}).(*pendingSymbolCapture)
	facts, _ := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if capture == nil || facts == nil {
		return
	}
	facts.mu.RLock()
	entries := maps.Clone(facts.entries)
	facts.mu.RUnlock()
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if !capture.disabled && capture.entries == nil && len(entries) > 0 {
		capture.facts, capture.entries = facts, entries
	}
}

// Literal recovery reads a separate mutable text lane, so its answer keeps
// the existing strict final projection validation even within symbol search.
func disablePendingSymbolCapture(ctx context.Context) {
	capture, _ := ctx.Value(pendingSymbolCaptureKey{}).(*pendingSymbolCapture)
	if capture == nil {
		return
	}
	capture.mu.Lock()
	capture.disabled = true
	capture.facts, capture.entries = nil, nil
	capture.mu.Unlock()
}

func validateSymbolResponseProjections(ctx context.Context, tool string) error {
	if tool != "search_symbols" {
		return validateRequestProjections(ctx)
	}
	capture, _ := ctx.Value(pendingSymbolCaptureKey{}).(*pendingSymbolCapture)
	facts, _ := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if capture == nil || facts == nil {
		return validateRequestProjections(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	capture.mu.RLock()
	defer capture.mu.RUnlock()
	if capture.facts != facts || len(capture.entries) == 0 {
		return validateRequestProjections(ctx)
	}
	facts.mu.RLock()
	defer facts.mu.RUnlock()
	if !maps.Equal(capture.entries, facts.entries) {
		return projectionSuperseded
	}
	// Later publications may invalidate shared old facts without changing
	// this answer's already selected immutable nodes and canonical mask.
	return ctx.Err()
}

func requestSearchCandidates(ctx context.Context) []query.ViewLayerSource {
	if snapshot, _ := ctx.Value(pendingSearchKey{}).(*pendingSearchSnapshot); snapshot != nil {
		return snapshot.candidates
	}
	return requestViewFromContext(ctx).candidateLayers()
}

// Bind one registry snapshot only after the request has selected its view.
// Prefix equality alone is insufficient: another daemon or linked checkout
// can publish the same graph path from a different physical source root.
func (s *Server) bindPendingSearch(ctx context.Context) (context.Context, error) {
	if requestReadsCommittedTree(ctx) {
		return ctx, nil
	}
	if ctx.Value(pendingSearchKey{}) != nil {
		return ctx, validateRequestProjections(ctx)
	}
	if err := validateRequestProjections(ctx); err != nil {
		return ctx, err
	}
	snapshot := pendingFileProjections.snap.Load()
	if len(snapshot.entries) == 0 {
		return ctx, nil
	}
	refreshDeadline := time.Now().Add(projectedReadBudget)
	if original, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok && original.Before(refreshDeadline) {
		refreshDeadline = original
	}
	var refreshCtx context.Context
	keys := make([]string, 0, len(snapshot.entries))
	for key := range snapshot.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	view := requestViewFromContext(ctx)
	var layers []graph.OverlayLayerReader
	var sources []query.ViewLayerSource
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return ctx, err
		}
		entry := snapshot.entries[key]
		idx, rel := s.indexerForRel(entry.graphPath)
		if idx == nil || idx.RepoPrefix() != entry.repoPrefix {
			continue
		}
		root := idx.RootPath()
		if view.routed() {
			root = view.viewRoot
		}
		if root == "" || pathkey.MutationKey(filepath.Join(root, filepath.FromSlash(rel))) != entry.key {
			continue
		}
		if pendingFileProjections.snap.Load().entries[key] != entry {
			return ctx, projectionSuperseded
		}
		if entry.suspect.Load() {
			// Legacy lookup consumes an old pointer's invalidation on equal
			// bytes. Reuse the existing bounded COW path instead: old readers
			// stay invalid, and this request borrows only its checked successor.
			if refreshCtx == nil {
				ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, refreshDeadline)
				var cancel context.CancelFunc
				refreshCtx, cancel = context.WithDeadline(ctx, refreshDeadline)
				defer cancel() // initialized at most once for the entire selection
			}
			current, err := s.projectCurrentTrackedFile(refreshCtx, ctx, entry.absPath, entry.graphPath, rel)
			if err != nil {
				return ctx, err
			}
			if current == nil {
				continue // the existing helper checked actual canonical content
			}
			entry = current
		}
		borrowed, err := retainRequestProjection(ctx, entry)
		if err != nil {
			return ctx, err
		}
		ctx = borrowed
		layers = append(layers, entry.layer)
		var backend search.Backend
		if entry.names != nil {
			backend = entry.names
		}
		sources = append(sources, query.ViewLayerSource{Search: backend, Layer: entry.layer})
	}
	if err := validateRequestProjections(ctx); err != nil {
		return ctx, err
	}
	if len(layers) == 0 {
		return ctx, nil
	}
	layers = append(layers, requestProjectionLayers(ctx)...)
	candidates := append([]query.ViewLayerSource(nil), view.candidateLayers()...)
	candidates = append(candidates, sources...)
	ctx = context.WithValue(ctx, pendingSearchKey{}, &pendingSearchSnapshot{candidates: candidates})
	ctx = context.WithValue(ctx, requestProjectionKey{}, layers)
	if editor := OverlayViewFromContext(ctx); editor != nil {
		ctx = WithOverlayView(ctx, graph.NewOverlaidViewWithLayer(s.requestBaseReader(ctx), editor.LayerReader()))
	}
	sealPendingSymbolCapture(ctx)
	return ctx, nil
}
