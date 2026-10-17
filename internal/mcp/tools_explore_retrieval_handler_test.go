package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

type exploreRetrievalRecordingBackend struct {
	mu      sync.Mutex
	queries []string
}

type exploreRetrievalBlockingBackend struct {
	started      chan struct{}
	release      chan struct{}
	targets      map[string]struct{}
	blockAt      int32
	targetCalls  atomic.Int32
	startOnce    sync.Once
	contextCalls atomic.Int32
	legacyCalls  atomic.Int32
}

func (*exploreRetrievalBlockingBackend) Add(string, ...string) {}
func (*exploreRetrievalBlockingBackend) Remove(string)         {}
func (b *exploreRetrievalBlockingBackend) Search(query string, _ int) []search.SearchResult {
	if _, ok := b.targets[query]; !ok {
		return nil
	}
	b.legacyCalls.Add(1)
	if b.targetCalls.Add(1) < b.blockAt {
		return nil
	}
	b.startOnce.Do(func() { close(b.started) })
	<-b.release
	return nil
}
func (b *exploreRetrievalBlockingBackend) SearchContext(ctx context.Context, query string, _ int) []search.SearchResult {
	if _, ok := b.targets[query]; !ok {
		return nil
	}
	b.contextCalls.Add(1)
	if b.targetCalls.Add(1) < b.blockAt {
		return nil
	}
	b.startOnce.Do(func() { close(b.started) })
	select {
	case <-ctx.Done():
	case <-b.release:
	}
	return nil
}
func (*exploreRetrievalBlockingBackend) Count() int { return 1 }
func (*exploreRetrievalBlockingBackend) Close()     {}

func (*exploreRetrievalRecordingBackend) Add(string, ...string) {}
func (*exploreRetrievalRecordingBackend) Remove(string)         {}
func (b *exploreRetrievalRecordingBackend) Search(query string, _ int) []search.SearchResult {
	return b.search(query)
}
func (b *exploreRetrievalRecordingBackend) SearchContext(_ context.Context, query string, _ int) []search.SearchResult {
	return b.search(query)
}
func (b *exploreRetrievalRecordingBackend) search(query string) []search.SearchResult {
	b.mu.Lock()
	b.queries = append(b.queries, query)
	b.mu.Unlock()
	if strings.Contains(query, "commitLayerBase") {
		return []search.SearchResult{{ID: "candidate"}}
	}
	return nil
}
func (*exploreRetrievalRecordingBackend) Count() int { return 1 }
func (*exploreRetrievalRecordingBackend) Close()     {}

func TestHandleExploreUsesBoundedRetrievalAndRetainsLateIdentifier(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "candidate", Name: "commitLayerBase", Kind: graph.KindMethod, FilePath: "internal/indexer/builder.go", StartLine: 10, EndLine: 20})
	backend := &exploreRetrievalRecordingBackend{}
	eng := query.NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	task := strings.Repeat("Explain how the coordinator processes repeated mutation work and preserves correctness. ", 4) + "The concrete implementation anchor is commitLayerBase."
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task": task, "max_symbols": 1}
	result, err := srv.handleFacade(context.Background(), "explore", req)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || len(result.Content) == 0 {
		t.Fatalf("result=%#v", result)
	}
	backend.mu.Lock()
	queries := append([]string(nil), backend.queries...)
	backend.mu.Unlock()
	t.Logf("queries=%q semantic=%q compact=%q", queries, shapeExploreQuery(task), shapeExploreRetrievalQuery(task, stripLeadingExploreDirective(shapeExploreQuery(task))))
	if len(queries) == 0 {
		t.Fatal("handler did not call search backend")
	}
	semantic := stripLeadingExploreDirective(shapeExploreQuery(task))
	wantQuery := shapeExploreRetrievalQuery(task, semantic)
	if queries[0] != wantQuery {
		t.Fatalf("first retrieval=%q, want compact query %q", queries[0], wantQuery)
	}
	if len(queries[0]) >= len(semantic) {
		t.Fatalf("first retrieval was not bounded: bytes=%d query=%q", len(queries[0]), queries[0])
	}
	if !strings.Contains(queries[0], "commitLayerBase") {
		t.Fatalf("first retrieval lost late identifier: %q", queries[0])
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if !strings.Contains(text, "commitLayerBase") {
		t.Fatalf("result lost matching candidate: %s", text)
	}
}

func TestHandleExploreBoundsExactLiveCommitLayerRequest(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "candidate", Name: "commitLayerBase", Kind: graph.KindMethod, FilePath: "internal/indexer/builder.go", StartLine: 10, EndLine: 20})
	backend := &exploreRetrievalRecordingBackend{}
	eng := query.NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task": exploreExactLiveCommitLayerTask, "max_symbols": 1}
	result, err := srv.handleFacade(context.Background(), "explore", req)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || len(result.Content) == 0 {
		t.Fatalf("result=%#v", result)
	}
	backend.mu.Lock()
	queries := append([]string(nil), backend.queries...)
	backend.mu.Unlock()
	if len(queries) == 0 {
		t.Fatal("handler did not call search backend")
	}
	semantic := stripLeadingExploreDirective(shapeExploreQuery(exploreExactLiveCommitLayerTask))
	want := shapeExploreRetrievalQuery(exploreExactLiveCommitLayerTask, semantic)
	t.Logf("semantic_bytes=%d retrieval_bytes=%d terms=%d retrieval=%q", len(semantic), len(queries[0]), len(strings.Fields(queries[0])), queries[0])
	if queries[0] != want {
		t.Fatalf("first retrieval=%q, want %q", queries[0], want)
	}
	if text := result.Content[0].(mcpgo.TextContent).Text; !strings.Contains(text, "commitLayerBase") {
		t.Fatalf("result lost matching candidate: %s", text)
	}
}

func TestHandleExploreCancellationReachesInFlightRetrievalBackend(t *testing.T) {
	task := strings.Repeat("Explain how the coordinator processes repeated mutation work. ", 4) + "The implementation anchor is commitLayerBase."
	semantic := stripLeadingExploreDirective(shapeExploreQuery(task))
	compact := shapeExploreRetrievalQuery(task, semantic)
	backend := &exploreRetrievalBlockingBackend{
		started: make(chan struct{}),
		release: make(chan struct{}),
		targets: map[string]struct{}{semantic: {}, compact: {}},
		blockAt: 2,
	}
	g := graph.New()
	eng := query.NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task":        task,
		"max_symbols": 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	type response struct {
		result *mcpgo.CallToolResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := srv.handleFacade(ctx, "explore", req)
		done <- response{result: result, err: err}
	}()

	select {
	case <-backend.started:
	case <-time.After(5 * time.Second):
		close(backend.release)
		cancel()
		t.Fatal("explore did not enter the retrieval backend")
	}
	cancel()
	select {
	case got := <-done:
		close(backend.release)
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("handleFacade error = %v, want context.Canceled (result=%#v)", got.err, got.result)
		}
		if got.result != nil {
			t.Fatalf("canceled explore returned an authoritative result: %#v", got.result)
		}
	case <-time.After(250 * time.Millisecond):
		close(backend.release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("request cancellation did not stop the in-flight retrieval backend")
	}
	if calls := backend.contextCalls.Load(); calls < 2 {
		t.Fatalf("context search calls = %d, want at least 2 (scoped and unscoped)", calls)
	}
	if calls := backend.legacyCalls.Load(); calls != 0 {
		t.Fatalf("legacy search calls = %d, want 0", calls)
	}
}
