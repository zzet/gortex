package graph

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type overlayStatsContextBase struct {
	Reader

	mu              sync.Mutex
	legacyStats     GraphStats
	contextStats    GraphStats
	legacyCalls     int
	contextCalls    int
	contextBehavior func(context.Context, int) (GraphStats, error)
	fileNodes       map[string][]*Node
	nodesByID       map[string]*Node
	edges           []*Edge
}

func (b *overlayStatsContextBase) Stats() GraphStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.legacyCalls++
	return b.legacyStats
}

func (b *overlayStatsContextBase) StatsContext(ctx context.Context) (GraphStats, error) {
	b.mu.Lock()
	b.contextCalls++
	call := b.contextCalls
	behavior := b.contextBehavior
	stats := b.contextStats
	b.mu.Unlock()
	if behavior != nil {
		return behavior(ctx, call)
	}
	return stats, ctx.Err()
}

func (b *overlayStatsContextBase) calls() (legacy, contextual int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.legacyCalls, b.contextCalls
}

func (b *overlayStatsContextBase) GetFileNodes(path string) []*Node {
	return append([]*Node(nil), b.fileNodes[path]...)
}

func (b *overlayStatsContextBase) GetNodesByIDs(ids []string) map[string]*Node {
	out := make(map[string]*Node, len(ids))
	for _, id := range ids {
		if node := b.nodesByID[id]; node != nil {
			out[id] = node
		}
	}
	return out
}

func (b *overlayStatsContextBase) AllEdges() []*Edge {
	return append([]*Edge(nil), b.edges...)
}

func (b *overlayStatsContextBase) EdgeCount() int { return len(b.edges) }

type overlayLegacyStatsBase struct {
	Reader

	mu     sync.Mutex
	stats  GraphStats
	cancel context.CancelFunc
	calls  int
}

func (b *overlayLegacyStatsBase) Stats() GraphStats {
	b.mu.Lock()
	b.calls++
	cancel := b.cancel
	stats := b.stats
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return stats
}

func (b *overlayLegacyStatsBase) setCancel(cancel context.CancelFunc) {
	b.mu.Lock()
	b.cancel = cancel
	b.mu.Unlock()
}

func (b *overlayLegacyStatsBase) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

type overlayStatsResult struct {
	stats GraphStats
	err   error
}

func TestOverlaidViewStatsContextCancellationDoesNotPoisonCache(t *testing.T) {
	entered := make(chan struct{})
	base := &overlayStatsContextBase{
		legacyStats:  GraphStats{TotalNodes: 99, TotalEdges: 88},
		contextStats: GraphStats{TotalNodes: 7, TotalEdges: 5},
	}
	base.contextBehavior = func(ctx context.Context, _ int) (GraphStats, error) {
		close(entered)
		<-ctx.Done()
		return GraphStats{TotalNodes: 1000, TotalEdges: 1000}, ctx.Err()
	}
	view := NewOverlaidViewWithLayer(base, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan overlayStatsResult, 1)
	go func() {
		stats, err := view.StatsContext(ctx)
		done <- overlayStatsResult{stats: stats, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("context statistics did not enter the base reader")
	}
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || !reflect.DeepEqual(result.stats, GraphStats{}) {
			t.Fatalf("canceled statistics = %#v, %v", result.stats, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled statistics remained blocked")
	}

	base.mu.Lock()
	base.contextBehavior = func(context.Context, int) (GraphStats, error) {
		return GraphStats{TotalNodes: 2000, TotalEdges: 2000}, context.DeadlineExceeded
	}
	base.mu.Unlock()
	got, err := view.StatsContext(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(got, GraphStats{}) {
		t.Fatalf("backend deadline statistics = %#v, %v", got, err)
	}

	base.mu.Lock()
	base.contextBehavior = nil
	base.mu.Unlock()
	want := GraphStats{TotalNodes: 7, TotalEdges: 5}
	got, err = view.StatsContext(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("live retry = %#v, %v; want %#v", got, err, want)
	}
	if legacy := view.Stats(); !reflect.DeepEqual(legacy, want) {
		t.Fatalf("legacy cache after live retry = %#v, want %#v", legacy, want)
	}
	if _, err := view.StatsContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	legacyCalls, contextCalls := base.calls()
	if legacyCalls != 0 || contextCalls != 3 {
		t.Fatalf("base calls = legacy %d context %d, want 0/3", legacyCalls, contextCalls)
	}
}

func TestOverlaidViewStatsAndContextShareFirstSuccessfulCache(t *testing.T) {
	want := GraphStats{TotalNodes: 12, TotalEdges: 9}
	t.Run("legacy-first", func(t *testing.T) {
		base := &overlayStatsContextBase{legacyStats: want, contextStats: GraphStats{TotalNodes: 100}}
		view := NewOverlaidViewWithLayer(base, nil)
		if got := view.Stats(); !reflect.DeepEqual(got, want) {
			t.Fatalf("Stats = %#v, want %#v", got, want)
		}
		got, err := view.StatsContext(context.Background())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("StatsContext = %#v, %v; want cached %#v", got, err, want)
		}
		legacyCalls, contextCalls := base.calls()
		if legacyCalls != 1 || contextCalls != 0 {
			t.Fatalf("base calls = legacy %d context %d, want 1/0", legacyCalls, contextCalls)
		}
	})
	t.Run("context-first", func(t *testing.T) {
		base := &overlayStatsContextBase{legacyStats: GraphStats{TotalNodes: 100}, contextStats: want}
		view := NewOverlaidViewWithLayer(base, nil)
		got, err := view.StatsContext(context.Background())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("StatsContext = %#v, %v; want %#v", got, err, want)
		}
		if got := view.Stats(); !reflect.DeepEqual(got, want) {
			t.Fatalf("Stats = %#v, want cached %#v", got, want)
		}
		legacyCalls, contextCalls := base.calls()
		if legacyCalls != 0 || contextCalls != 1 {
			t.Fatalf("base calls = legacy %d context %d, want 0/1", legacyCalls, contextCalls)
		}
	})
}

func TestOverlaidViewStatsContextCanceledFirstCallerDoesNotWaitForOtherBuild(t *testing.T) {
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	want := GraphStats{TotalNodes: 4, TotalEdges: 3}
	base := &overlayStatsContextBase{contextStats: want}
	base.contextBehavior = func(ctx context.Context, call int) (GraphStats, error) {
		if call == 1 {
			close(firstEntered)
			<-releaseFirst
			return want, nil
		}
		close(secondEntered)
		<-ctx.Done()
		return GraphStats{TotalNodes: 1000}, ctx.Err()
	}
	view := NewOverlaidViewWithLayer(base, nil)
	firstDone := make(chan overlayStatsResult, 1)
	go func() {
		stats, err := view.StatsContext(context.Background())
		firstDone <- overlayStatsResult{stats: stats, err: err}
	}()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first statistics call did not enter")
	}

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan overlayStatsResult, 1)
	go func() {
		stats, err := view.StatsContext(ctx)
		secondDone <- overlayStatsResult{stats: stats, err: err}
	}()
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second statistics call waited behind the first")
	}
	cancel()
	select {
	case result := <-secondDone:
		if !errors.Is(result.err, context.Canceled) || !reflect.DeepEqual(result.stats, GraphStats{}) {
			t.Fatalf("second statistics = %#v, %v", result.stats, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("second canceled statistics remained blocked")
	}
	close(releaseFirst)
	select {
	case result := <-firstDone:
		if result.err != nil || !reflect.DeepEqual(result.stats, want) {
			t.Fatalf("first statistics = %#v, %v; want %#v", result.stats, result.err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("first statistics did not finish")
	}
	if got := view.Stats(); !reflect.DeepEqual(got, want) {
		t.Fatalf("cached statistics = %#v, want %#v", got, want)
	}
}

func TestOverlaidViewStatsContextLegacyFallbackDoesNotCacheCanceledResult(t *testing.T) {
	want := GraphStats{TotalNodes: 6, TotalEdges: 2}
	base := &overlayLegacyStatsBase{stats: want}
	view := NewOverlaidViewWithLayer(base, nil)
	ctx, cancel := context.WithCancel(context.Background())
	base.setCancel(cancel)
	got, err := view.StatsContext(ctx)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, GraphStats{}) {
		t.Fatalf("canceled fallback = %#v, %v", got, err)
	}
	base.setCancel(nil)
	got, err = view.StatsContext(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("live fallback = %#v, %v; want %#v", got, err, want)
	}
	if base.callCount() != 2 {
		t.Fatalf("legacy calls = %d, want 2", base.callCount())
	}
}

func TestOverlaidViewStatsContextPreservesOverlayDeltas(t *testing.T) {
	baseA := &Node{ID: "a", FilePath: "a.go"}
	baseB := &Node{ID: "b", FilePath: "a.go"}
	base := &overlayStatsContextBase{
		contextStats: GraphStats{TotalNodes: 2, TotalEdges: 1},
		fileNodes:    map[string][]*Node{"a.go": {baseA, baseB}},
		nodesByID:    map[string]*Node{"a": baseA, "b": baseB},
		edges:        []*Edge{{From: "a", To: "b", FilePath: "a.go"}},
	}
	layer := NewOverlayLayer()
	layer.MarkFile("a.go", false)
	layer.AddNode("a.go", &Node{ID: "a", FilePath: "a.go"})
	view := NewOverlaidView(base, layer)

	want := GraphStats{TotalNodes: 1, TotalEdges: 0}
	got, err := view.StatsContext(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay statistics = %#v, %v; want %#v", got, err, want)
	}
	if legacy := view.Stats(); !reflect.DeepEqual(legacy, want) {
		t.Fatalf("cached legacy overlay statistics = %#v, want %#v", legacy, want)
	}
	legacyCalls, contextCalls := base.calls()
	if legacyCalls != 0 || contextCalls != 1 {
		t.Fatalf("base calls = legacy %d context %d, want 0/1", legacyCalls, contextCalls)
	}
}

func TestOverlaidViewStatsContextNilBase(t *testing.T) {
	view := NewOverlaidViewWithLayer(nil, nil)
	got, err := view.StatsContext(context.Background())
	if err != nil || !reflect.DeepEqual(got, GraphStats{}) {
		t.Fatalf("nil-base context statistics = %#v, %v", got, err)
	}
	if legacy := view.Stats(); !reflect.DeepEqual(legacy, GraphStats{}) {
		t.Fatalf("nil-base legacy statistics = %#v", legacy)
	}
}
