package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/lspuri"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// Exercise the real provider and output authority without an installed language
// server. The first hierarchy response models an indexing server's empty answer.
func newReferenceRetryStack(t *testing.T, first string, beforeIncoming func()) (*viewStack, *graph.Node, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	stack := newViewStack(t)
	source := "fn target() {}\nfn caller() { target(); }\n"
	path := filepath.Join(stack.repoRoot, "retry.rs")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	// A Windows drive must be in the URI path, never its authority.
	uri := lspuri.PathToURI(path)
	if got := lspuri.URIToAbsPath(uri); got != path {
		t.Fatalf("fixture URI %q resolves to %q, want %q", uri, got, path)
	}
	target := viewRepoNode("repo/retry.rs::target", "target", graph.KindFunction, "repo/retry.rs", 1)
	caller := viewRepoNode("repo/retry.rs::caller", "caller", graph.KindFunction, "repo/retry.rs", 2)
	target.Language, caller.Language = "rust", "rust"
	stack.store.AddBatch([]*graph.Node{target, caller}, []*graph.Edge{{
		From: caller.ID, To: target.ID, Kind: graph.EdgeCalls,
		FilePath: caller.FilePath, Line: 2, Origin: graph.OriginASTResolved,
	}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	calls, connections := &atomic.Int32{}, &atomic.Int32{}
	done := make(chan struct{})
	var active net.Conn
	accepted := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		active = conn
		connections.Add(1)
		close(accepted)
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			size := 0
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if line == "\r\n" {
					break
				}
				if strings.HasPrefix(line, "Content-Length:") {
					size, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:")))
				}
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(reader, body); err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(body, &req) != nil {
				return
			}
			if len(req.ID) == 0 {
				continue
			}
			var result any
			var rpcError any
			switch req.Method {
			case "initialize":
				result = map[string]any{"capabilities": map[string]any{"callHierarchyProvider": true}}
			case "textDocument/prepareCallHierarchy":
				attempt := calls.Add(1)
				if first == "error" && attempt == 1 {
					rpcError = map[string]any{"code": -32603, "message": "workspace loading"}
				}
				if first != "zero" && (attempt > 1 || first == "positive" || first == "incoming-error") {
					result = []lsp.CallHierarchyItem{{Name: "target", Kind: 12, URI: uri, Range: lsp.Range{Start: lsp.Position{Line: 0}, End: lsp.Position{Line: 0, Character: 14}}, SelectionRange: lsp.Range{Start: lsp.Position{Line: 0, Character: 3}, End: lsp.Position{Line: 0, Character: 9}}}}
				} else {
					result = []any{}
				}
			case "callHierarchy/incomingCalls":
				if first == "incoming-error" && calls.Load() == 1 {
					rpcError = map[string]any{"code": -32603, "message": "workspace loading"}
				}
				if beforeIncoming != nil {
					beforeIncoming()
				}
				result = []lsp.CallHierarchyIncomingCall{{From: lsp.CallHierarchyItem{Name: "caller", Kind: 12, URI: uri, Range: lsp.Range{Start: lsp.Position{Line: 1}, End: lsp.Position{Line: 1, Character: 25}}, SelectionRange: lsp.Range{Start: lsp.Position{Line: 1, Character: 3}, End: lsp.Position{Line: 1, Character: 9}}}, FromRanges: []lsp.Range{{Start: lsp.Position{Line: 1, Character: 14}, End: lsp.Position{Line: 1, Character: 20}}}}}
			}
			reply := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
			if rpcError != nil {
				delete(reply, "result")
				reply["error"] = rpcError
			}
			response, _ := json.Marshal(reply)
			if _, err := fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n%s", len(response), response); err != nil {
				return
			}
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := lsp.NewProviderFromSpec(&lsp.ServerSpec{
		Name: "reference-fixture", Command: executable, Languages: []string{"rust"},
		Daemon: true, NoHeavyRequests: first == "optout",
		Connect: &lsp.ConnectSpec{Network: "tcp", Address: ln.Addr().String()},
	}, zap.NewNop())
	stack.srv.semanticMgr = semantic.NewManager(semantic.Config{}, zap.NewNop())
	stack.srv.semanticMgr.RegisterProvider(provider)
	t.Cleanup(func() {
		_ = provider.Close()
		_ = ln.Close()
		<-accepted
		if active != nil {
			_ = active.Close()
		}
		<-done
	})
	return stack, target, calls, connections
}

func TestColdReferenceConfirmationIsNotPermanent(t *testing.T) {
	stack, node, calls, _ := newReferenceRetryStack(t, "empty", nil)
	stack.srv.confirmSymbolRefsOnDemand(context.Background(), node)
	if calls.Load() != 1 {
		t.Fatalf("hierarchy requests = %d, want 1", calls.Load())
	}
	if entry, ok := stack.srv.refsConfirmed.Load(node.ID); ok {
		if entry.(*referenceConfirmation).confirmed {
			t.Fatal("cold empty response permanently confirmed the symbol")
		}
	}
}

func TestReferenceConfirmationRecoversAfterCooldown(t *testing.T) {
	for _, first := range []string{"empty", "error", "incoming-error", "positive", "zero"} {
		t.Run(first, func(t *testing.T) {
			stack, node, calls, connections := newReferenceRetryStack(t, first, nil)
			now := time.Now()
			stack.srv.confirmSymbolRefsAt(context.Background(), node, now)
			for range 3 {
				stack.srv.confirmSymbolRefsAt(context.Background(), node, now.Add(referenceRetryCooldown-time.Nanosecond))
			}
			if calls.Load() != 1 {
				t.Fatalf("cooldown requests = %d, want 1", calls.Load())
			}
			stack.srv.confirmSymbolRefsAt(context.Background(), node, now.Add(referenceRetryCooldown))
			want := int32(2)
			if first == "positive" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("post-cooldown requests = %d, want %d", calls.Load(), want)
			}
			if first != "zero" {
				edges := stack.store.GetInEdges(node.ID)
				found := false
				for _, edge := range edges {
					if edge.From == "repo/retry.rs::caller" && edge.Kind == graph.EdgeCalls && edge.Origin == graph.OriginLSPResolved {
						found = true
					}
				}
				if !found {
					t.Fatalf("retry did not promote caller edge: %+v", edges)
				}
				stack.srv.confirmSymbolRefsAt(context.Background(), node, now.Add(2*referenceRetryCooldown))
				if calls.Load() != want {
					t.Fatal("positive confirmation was not cached")
				}
			}
			if first == "zero" {
				stack.srv.confirmSymbolRefsAt(context.Background(), node, now.Add(2*referenceRetryCooldown))
				if calls.Load() != 3 {
					t.Fatal("valid zero callers lost eventual retry eligibility")
				}
			}
			if connections.Load() != 1 {
				t.Fatalf("provider connections = %d, want 1", connections.Load())
			}
		})
	}
}

func TestReferenceConfirmationClaimBoundsConcurrentQueries(t *testing.T) {
	stack, node, calls, _ := newReferenceRetryStack(t, "empty", nil)
	now := time.Now()
	for _, stamp := range []time.Time{now, now.Add(referenceRetryCooldown)} {
		var queries sync.WaitGroup
		for range 16 {
			queries.Go(func() { stack.srv.confirmSymbolRefsAt(context.Background(), node, stamp) })
		}
		queries.Wait()
	}
	if calls.Load() != 2 {
		t.Fatalf("concurrent queries made %d hierarchy requests, want 2", calls.Load())
	}
}

func TestReferenceConfirmationRefusedCompletionRetries(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stack, node, calls, _ := newReferenceRetryStack(t, "positive", func() { once.Do(func() { close(started); <-release }) })
	now := time.Now()
	done := make(chan struct{})
	go func() { defer close(done); stack.srv.confirmSymbolRefsAt(context.Background(), node, now) }()
	<-started
	out, err := BeginBaseEnrichment(context.Background(), stack.srv.outputGenerationAuthority(), stack.srv.graph, EnrichProducerSymbolRefs, node.RepoPrefix, stack.repoRoot)
	close(release)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	out.Abandon()
	entry, _ := stack.srv.refsConfirmed.Load(node.ID)
	if entry.(*referenceConfirmation).confirmed {
		t.Fatal("superseded output was permanently confirmed")
	}
	stack.srv.confirmSymbolRefsAt(context.Background(), node, now.Add(referenceRetryCooldown))
	if calls.Load() != 2 {
		t.Fatalf("refused completion made %d requests, want 2", calls.Load())
	}
}

func TestReferenceConfirmationResetCannotReviveInflightEntry(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	stack, node, _, _ := newReferenceRetryStack(t, "positive", func() { close(started); <-release })
	done := make(chan struct{})
	go func() { defer close(done); stack.srv.confirmSymbolRefsOnDemand(context.Background(), node) }()
	<-started
	stack.srv.resetConfirmedRefs()
	// A new epoch's entry must survive the previous attempt finishing.
	replacement := stack.srv.claimReferenceConfirmation(node.ID, time.Now())
	close(release)
	<-done
	if entry, ok := stack.srv.refsConfirmed.Load(node.ID); !ok || entry != replacement {
		t.Fatal("old completion replaced the new epoch's claim")
	}
}

func TestReferenceConfirmationClaimsAreIndependent(t *testing.T) {
	srv := &Server{}
	now := time.Now()
	first := srv.claimReferenceConfirmation("first", now)
	if first == nil || srv.claimReferenceConfirmation("second", now) == nil {
		t.Fatal("different symbol could not claim")
	}
	if srv.claimReferenceConfirmation("first", now) != nil {
		t.Fatal("active symbol claimed twice")
	}
	srv.resetConfirmedRefs()
	if srv.refsConfirmed.CompareAndSwap("first", first, &referenceConfirmation{confirmed: true}) {
		t.Fatal("old claim revived after reset")
	}
	if srv.claimReferenceConfirmation("first", now) == nil {
		t.Fatal("reset did not allow a new attempt")
	}
}

func TestReferenceConfirmationRoutedRefusalLeavesPrimaryEligible(t *testing.T) {
	stack, node, calls, connections := newReferenceRetryStack(t, "empty", nil)
	stack.srv.confirmSymbolRefsOnDemand(routedEnrichmentCtx(t, stack), node)
	if calls.Load() != 0 || connections.Load() != 0 {
		t.Fatal("routed refusal reached the language server")
	}
	if _, ok := stack.srv.refsConfirmed.Load(node.ID); ok {
		t.Fatal("routed refusal retained a primary claim")
	}
	stack.srv.confirmSymbolRefsOnDemand(context.Background(), node)
	if calls.Load() != 1 {
		t.Fatalf("primary request made %d calls, want 1", calls.Load())
	}
}

func TestReferenceConfirmationHonorsProviderOptOut(t *testing.T) {
	stack, node, calls, connections := newReferenceRetryStack(t, "optout", nil)
	now := time.Now()
	stack.srv.confirmSymbolRefsAt(context.Background(), node, now)
	stack.srv.confirmSymbolRefsAt(context.Background(), node, now.Add(referenceRetryCooldown))
	if calls.Load() != 0 {
		t.Fatalf("opted-out provider made %d hierarchy requests", calls.Load())
	}
	if connections.Load() != 1 {
		t.Fatalf("opted-out provider connections = %d, want 1", connections.Load())
	}
}

func TestReferenceConfirmationProviderFailureBacksOff(t *testing.T) {
	stack := newLSPEnrichmentStack(t)
	now := time.Now()
	stack.srv.confirmSymbolRefsAt(context.Background(), stack.node, now)
	standing := stack.standingReceipt(t, EnrichProducerSymbolRefs)
	stack.srv.confirmSymbolRefsAt(context.Background(), stack.node, now.Add(referenceRetryCooldown-time.Nanosecond))
	if err := standing.Complete(); err != nil {
		t.Fatalf("provider failure retried inside cooldown: %v", err)
	}
}
