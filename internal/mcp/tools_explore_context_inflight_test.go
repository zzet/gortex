package mcp

import (
	"context"
	"errors"
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

type exploreContextBlockingBackend struct {
	block        bool
	started      chan struct{}
	startOnce    sync.Once
	contextCalls atomic.Int32
	legacyCalls  atomic.Int32
}

func (*exploreContextBlockingBackend) Add(string, ...string) {}
func (*exploreContextBlockingBackend) Remove(string)         {}
func (b *exploreContextBlockingBackend) Search(string, int) []search.SearchResult {
	b.legacyCalls.Add(1)
	return nil
}
func (*exploreContextBlockingBackend) Count() int { return 1 }
func (*exploreContextBlockingBackend) Close()     {}

func (b *exploreContextBlockingBackend) SearchContext(ctx context.Context, _ string, _ int) []search.SearchResult {
	b.contextCalls.Add(1)
	b.startOnce.Do(func() {
		if b.started != nil {
			close(b.started)
		}
	})
	if b.block {
		<-ctx.Done()
	}
	return nil
}

func newExploreContextCancellationServer(backend search.Backend) *Server {
	g := graph.New()
	eng := query.NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil)
}

func exploreContextCancellationRequest() mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task": "find blockingLookup implementation",
		"options": map[string]any{
			"max_symbols": 1,
		},
	}
	return req
}

type exploreContextResponse struct {
	result *mcpgo.CallToolResult
	err    error
}

func callExploreContext(srv *Server, ctx context.Context, req mcpgo.CallToolRequest) <-chan exploreContextResponse {
	done := make(chan exploreContextResponse, 1)
	go func() {
		result, err := srv.handleFacade(ctx, "explore", req)
		done <- exploreContextResponse{result: result, err: err}
	}()
	return done
}

func awaitExploreWorker(done <-chan exploreContextResponse) {
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func TestFacadeExploreCancellationStopsInFlightSearch(t *testing.T) {
	backend := &exploreContextBlockingBackend{
		block:   true,
		started: make(chan struct{}),
	}
	srv := newExploreContextCancellationServer(backend)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := callExploreContext(srv, ctx, exploreContextCancellationRequest())

	select {
	case <-backend.started:
	case <-ctx.Done():
		cancel()
		awaitExploreWorker(done)
		t.Fatal("explore never entered the context-aware backend")
	}
	cancel()

	var got exploreContextResponse
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("explore did not return after request cancellation")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("handleFacade error = %v, want context.Canceled (result=%#v)", got.err, got.result)
	}
	if got.result != nil {
		t.Fatalf("canceled explore returned an authoritative result: %#v", got.result)
	}
	if calls := backend.contextCalls.Load(); calls != 1 {
		t.Fatalf("context search calls = %d, want 1", calls)
	}
	if calls := backend.legacyCalls.Load(); calls != 0 {
		t.Fatalf("canceled explore fell back to legacy search %d times", calls)
	}
}

func TestFacadeExplorePreCanceledAndLiveEmptyContracts(t *testing.T) {
	req := exploreContextCancellationRequest()

	preCanceled := &exploreContextBlockingBackend{}
	srv := newExploreContextCancellationServer(preCanceled)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := srv.handleFacade(ctx, "explore", req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled error = %v, want context.Canceled (result=%#v)", err, result)
	}
	if result != nil || preCanceled.contextCalls.Load() != 0 || preCanceled.legacyCalls.Load() != 0 {
		t.Fatalf("pre-canceled request did work: result=%#v context=%d legacy=%d",
			result, preCanceled.contextCalls.Load(), preCanceled.legacyCalls.Load())
	}

	live := &exploreContextBlockingBackend{}
	srv = newExploreContextCancellationServer(live)
	liveCtx, liveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer liveCancel()
	liveDone := callExploreContext(srv, liveCtx, req)
	var got exploreContextResponse
	select {
	case got = <-liveDone:
	case <-liveCtx.Done():
		liveCancel()
		awaitExploreWorker(liveDone)
		t.Fatal("live empty explore did not return before its deadline")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.result == nil || got.result.IsError || len(got.result.Content) == 0 {
		t.Fatalf("uncanceled empty explore lost its result contract: %#v", got.result)
	}
	if calls := live.contextCalls.Load(); calls == 0 {
		t.Fatal("live empty control did not exercise the context-aware backend")
	}
	if calls := live.legacyCalls.Load(); calls != 0 {
		t.Fatalf("live context-aware empty search used legacy fallback %d times", calls)
	}
}
