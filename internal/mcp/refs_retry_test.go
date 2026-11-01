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
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/lsp"
	"go.uber.org/zap"
)

// Exercise the real provider and output authority without an installed language
// server. The first hierarchy response models an indexing server's empty answer.
func newReferenceRetryStack(t *testing.T) (*viewStack, *graph.Node, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	stack := newViewStack(t)
	source := "fn target() {}\nfn caller() { target(); }\n"
	path := filepath.Join(stack.repoRoot, "retry.rs")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	target := viewRepoNode("repo/retry.rs::target", "target", graph.KindFunction, "repo/retry.rs", 1)
	caller := viewRepoNode("repo/retry.rs::caller", "caller", graph.KindFunction, "repo/retry.rs", 2)
	stack.store.AddBatch([]*graph.Node{target, caller}, nil)
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
			switch req.Method {
			case "initialize":
				result = map[string]any{"capabilities": map[string]any{"callHierarchyProvider": true}}
			case "textDocument/prepareCallHierarchy":
				if calls.Add(1) > 1 {
					result = []lsp.CallHierarchyItem{{Name: "target", Kind: 12, URI: "file://" + path, Range: lsp.Range{Start: lsp.Position{Line: 0}, End: lsp.Position{Line: 0, Character: 14}}, SelectionRange: lsp.Range{Start: lsp.Position{Line: 0, Character: 3}, End: lsp.Position{Line: 0, Character: 9}}}}
				} else {
					result = []any{}
				}
			case "callHierarchy/incomingCalls":
				result = []lsp.CallHierarchyIncomingCall{{From: lsp.CallHierarchyItem{Name: "caller", Kind: 12, URI: "file://" + path, Range: lsp.Range{Start: lsp.Position{Line: 1}, End: lsp.Position{Line: 1, Character: 25}}, SelectionRange: lsp.Range{Start: lsp.Position{Line: 1, Character: 3}, End: lsp.Position{Line: 1, Character: 9}}}, FromRanges: []lsp.Range{{Start: lsp.Position{Line: 1, Character: 14}, End: lsp.Position{Line: 1, Character: 20}}}}}
			}
			response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			if _, err := fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n%s", len(response), response); err != nil {
				return
			}
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := lsp.NewProviderFromSpec(&lsp.ServerSpec{Name: "reference-fixture", Command: executable, Languages: []string{"rust"}, Daemon: true, Connect: &lsp.ConnectSpec{Network: "tcp", Address: ln.Addr().String()}}, zap.NewNop())
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
	stack, node, calls, _ := newReferenceRetryStack(t)
	stack.srv.confirmSymbolRefsOnDemand(context.Background(), node)
	if calls.Load() != 1 {
		t.Fatalf("hierarchy requests = %d, want 1", calls.Load())
	}
	if entry, ok := stack.srv.refsConfirmed.Load(node.ID); ok {
		if _, permanent := entry.(struct{}); permanent {
			t.Fatal("cold empty response permanently confirmed the symbol")
		}
	}
}
