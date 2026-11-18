package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// ---------------------------------------------------------------------------
// Hardening test harness — an instrumented fake LSP server that tracks the
// open-document lifecycle (didOpen / didClose pairing), peak concurrency, and
// can simulate a mid-flight server exit + a clean reconnect.
//
// These tests cover the LSP enrichment hardening spec
// (docs/spec-lsp-enrichment-hardening.md): per-goroutine doc lifecycle,
// bounded concurrency, and reconnect-with-backoff on server exit.
// ---------------------------------------------------------------------------

// instrumentedServer wraps fakeLSPServer-style routing with lifecycle counters
// so a test can assert didOpen↔didClose pairing and peak concurrency.
type instrumentedServer struct {
	handlers map[string]func(params json.RawMessage) (any, *jsonRPCError)

	mu         sync.Mutex
	openDocs   map[string]int  // uri → currently-open count
	everOpened map[string]bool // uri → ever received a didOpen
	maxOpen    int             // peak simultaneous open docs
	totalOpen  int
	totalClose int
	notifLog   []string
	outMu      sync.Mutex // serialises concurrent response writes
}

func newInstrumentedServer() *instrumentedServer {
	return &instrumentedServer{
		handlers:   make(map[string]func(json.RawMessage) (any, *jsonRPCError)),
		openDocs:   make(map[string]int),
		everOpened: make(map[string]bool),
	}
}

// wasOpened reports whether the server ever received a didOpen for uri.
func (s *instrumentedServer) wasOpened(uri string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.everOpened[uri]
}

func (s *instrumentedServer) handle(method string, fn func(params json.RawMessage) (any, *jsonRPCError)) {
	s.handlers[method] = fn
}

func (s *instrumentedServer) stats() (peak, opens, closes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxOpen, s.totalOpen, s.totalClose
}

func (s *instrumentedServer) recordOpen(uri string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openDocs[uri]++
	s.everOpened[uri] = true
	s.totalOpen++
	cur := 0
	for _, n := range s.openDocs {
		cur += n
	}
	if cur > s.maxOpen {
		s.maxOpen = cur
	}
}

func (s *instrumentedServer) recordClose(uri string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openDocs[uri] > 0 {
		s.openDocs[uri]--
	}
	s.totalClose++
}

// run consumes framed JSON-RPC messages, tracking didOpen/didClose, and
// dispatches requests to registered handlers.
func (s *instrumentedServer) run(in *bufio.Reader, out io.Writer) {
	for {
		body, ok := readFramed(in)
		if !ok {
			return
		}
		var probe struct {
			Method string `json:"method"`
			ID     *int64 `json:"id,omitempty"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			continue
		}

		if probe.ID == nil {
			// Notification — track open/close lifecycle.
			s.mu.Lock()
			s.notifLog = append(s.notifLog, probe.Method)
			s.mu.Unlock()
			switch probe.Method {
			case "textDocument/didOpen":
				var p struct {
					TextDocument struct {
						URI string `json:"uri"`
					} `json:"textDocument"`
				}
				var wrap struct {
					Params json.RawMessage `json:"params"`
				}
				_ = json.Unmarshal(body, &wrap)
				if json.Unmarshal(wrap.Params, &p) == nil {
					s.recordOpen(p.TextDocument.URI)
				}
			case "textDocument/didClose":
				var p struct {
					TextDocument struct {
						URI string `json:"uri"`
					} `json:"textDocument"`
				}
				var wrap struct {
					Params json.RawMessage `json:"params"`
				}
				_ = json.Unmarshal(body, &wrap)
				if json.Unmarshal(wrap.Params, &p) == nil {
					s.recordClose(p.TextDocument.URI)
				}
			}
			continue
		}

		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			continue
		}
		// Dispatch each request in its own goroutine — a real language
		// server services hover requests concurrently, which is what lets
		// the concurrency-bound assertions observe genuine overlap.
		go func(req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}) {
			resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
			if h, ok := s.handlers[req.Method]; ok {
				result, errResp := h(req.Params)
				if errResp != nil {
					resp.Error = errResp
				} else if result != nil {
					raw, err := json.Marshal(result)
					if err != nil {
						resp.Error = &jsonRPCError{Code: -32603, Message: "marshal: " + err.Error()}
					} else {
						resp.Result = raw
					}
				}
			} else {
				resp.Result = json.RawMessage("null")
			}
			data, err := json.Marshal(resp)
			if err != nil {
				return
			}
			s.outMu.Lock()
			fmt.Fprintf(out, "Content-Length: %d\r\n\r\n", len(data))
			_, _ = out.Write(data)
			s.outMu.Unlock()
		}(req)
	}
}

// providerWithInstrumentedServer wires a Provider to a running instrumented
// server. maxParallel is honored (0 → default 10).
func providerWithInstrumentedServer(t *testing.T, server *instrumentedServer, languages []string, maxParallel int) (*Provider, func()) {
	t.Helper()
	c, serverIn, serverOut, cleanup := newPipedClient(t)
	go server.run(serverIn, serverOut)
	p := NewProvider("fake-lsp", nil, languages, false, maxParallel, zap.NewNop())
	p.lifetimeMu.Lock()
	p.client = c
	p.lifetimeMu.Unlock()
	return p, cleanup
}

// writeManyGoNodes writes a single .go file and seeds n function nodes into g,
// returning the repo root.
func seedRepo(t *testing.T, n int) (string, graph.Store) {
	t.Helper()
	repoRoot := t.TempDir()
	var sb strings.Builder
	sb.WriteString("package main\n\n")
	g := graph.New()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("F%d", i)
		fmt.Fprintf(&sb, "func %s() {}\n", name)
		g.AddNode(&graph.Node{
			ID: "main.go::" + name, Kind: graph.KindFunction, Name: name,
			FilePath: "main.go", StartLine: 3 + i, EndLine: 3 + i, Language: "go",
		})
	}
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(sb.String()), 0o644))
	return repoRoot, g
}

func runEnrich(t *testing.T, p *Provider, g graph.Store, repoRoot string, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatal("Enrich timed out")
		return nil
	}
}

// ---------------------------------------------------------------------------
// Issue 1 — concurrency bound: at most maxParallel goroutines run at once.
// ---------------------------------------------------------------------------

func TestLSP_Enrich_ConcurrencyBounded(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot, g := seedRepo(t, 50)

	var inFlight atomic.Int64
	var peak atomic.Int64
	server := newInstrumentedServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		cur := inFlight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond) // hold the slot so overlap is observable
		inFlight.Add(-1)
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func F()"}}, nil
	})

	const maxParallel = 4
	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, maxParallel)
	defer cleanup()

	require.NoError(t, runEnrich(t, p, g, repoRoot, 10*time.Second))

	assert.LessOrEqual(t, int(peak.Load()), maxParallel,
		"peak concurrent hovers (%d) must not exceed maxParallel (%d)", peak.Load(), maxParallel)
	assert.Greater(t, int(peak.Load()), 1, "expected some concurrency, got serial execution")
}

// ---------------------------------------------------------------------------
// Issue 2 — per-goroutine doc lifecycle: every didOpen is paired with a
// didClose, and at most maxParallel docs are open at once.
// ---------------------------------------------------------------------------

func TestLSP_Enrich_DocLifecyclePairedAndBounded(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	// Many distinct files so each node opens its own document.
	g := graph.New()
	const nFiles = 20
	for i := 0; i < nFiles; i++ {
		fn := fmt.Sprintf("f%d.go", i)
		require.NoError(t, os.WriteFile(
			filepath.Join(repoRoot, fn),
			[]byte("package main\n\nfunc F() {}\n"),
			0o644,
		))
		g.AddNode(&graph.Node{
			ID: fn + "::F", Kind: graph.KindFunction, Name: "F",
			FilePath: fn, StartLine: 3, EndLine: 3, Language: "go",
		})
	}

	server := newInstrumentedServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		time.Sleep(2 * time.Millisecond)
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func F()"}}, nil
	})

	const maxParallel = 5
	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, maxParallel)
	defer cleanup()

	require.NoError(t, runEnrich(t, p, g, repoRoot, 10*time.Second))

	// The instrumented server reads the message stream asynchronously, so
	// wait for it to drain every didOpen/didClose before sampling counts.
	require.Eventually(t, func() bool {
		_, o, c := server.stats()
		return o == nFiles && c == nFiles
	}, 2*time.Second, 5*time.Millisecond, "server should drain all didOpen/didClose")

	peak, opens, closes := server.stats()
	assert.Equal(t, opens, closes, "every didOpen must be matched by a didClose (opens=%d closes=%d)", opens, closes)
	assert.Equal(t, nFiles, opens, "expected one didOpen per distinct file")
	// The shared document session keeps a warm LRU tail of recently-released
	// files across phases, so up to 2*maxParallel docs can be open at once:
	// maxParallel pinned by the in-flight file goroutines plus the tail. The
	// session evicts (didCloses) down to that ceiling before each new open.
	assert.LessOrEqual(t, peak, 2*maxParallel,
		"peak simultaneously-open docs (%d) must not exceed the session cap 2*maxParallel (%d)", peak, 2*maxParallel)
}

// didClose must happen even when hover fails.
func TestLSP_Enrich_DocClosedEvenOnHoverError(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	g := graph.New()
	const nFiles = 8
	for i := 0; i < nFiles; i++ {
		fn := fmt.Sprintf("f%d.go", i)
		require.NoError(t, os.WriteFile(
			filepath.Join(repoRoot, fn),
			[]byte("package main\n\nfunc F() {}\n"),
			0o644,
		))
		g.AddNode(&graph.Node{
			ID: fn + "::F", Kind: graph.KindFunction, Name: "F",
			FilePath: fn, StartLine: 3, EndLine: 3, Language: "go",
		})
	}

	server := newInstrumentedServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		// Every hover fails — but NOT a server-exit error, so no reconnect.
		return nil, &jsonRPCError{Code: -32603, Message: "internal error"}
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()

	require.NoError(t, runEnrich(t, p, g, repoRoot, 10*time.Second))

	// Wait for the async server to drain the message stream before sampling.
	require.Eventually(t, func() bool {
		_, o, c := server.stats()
		return o == nFiles && c == nFiles
	}, 2*time.Second, 5*time.Millisecond, "server should drain all didOpen/didClose")

	_, opens, closes := server.stats()
	assert.Equal(t, opens, closes, "didClose must fire even when hover errors (opens=%d closes=%d)", opens, closes)
	assert.Equal(t, nFiles, closes, "each opened doc must be closed")
}

// ---------------------------------------------------------------------------
// Issue 3 — reconnect with backoff on server exit.
// ---------------------------------------------------------------------------

// TestLSP_Enrich_ReconnectsOnServerExit drives one goroutine into a
// "LSP server exited" error, then verifies the provider reconnects (via the
// connectOnce seam) and the enrichment completes without error.
func TestLSP_Enrich_ReconnectsOnServerExit(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot, g := seedRepo(t, 12)

	// First server: after the 3rd hover, it "dies" (we close its client's
	// done channel from the handler), so subsequent Call()s return
	// "LSP server exited".
	server1 := newInstrumentedServer()
	var hoverCount atomic.Int64
	var killOnce sync.Once

	p, cleanup := providerWithInstrumentedServer(t, server1, []string{"go"}, 3)
	defer cleanup()
	deadClient := p.client

	server1.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		n := hoverCount.Add(1)
		if n == 3 {
			// Simulate the server process dying: close the client's done
			// channel so all in-flight + future Call()s observe exit.
			killOnce.Do(func() {
				deadClient.mu.Lock()
				if !deadClient.closed {
					deadClient.closed = true
					close(deadClient.done)
				}
				deadClient.mu.Unlock()
			})
			return nil, &jsonRPCError{Code: -32603, Message: "dying"}
		}
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func F()"}}, nil
	})

	// Wire the reconnect seam: build a fresh in-memory client backed by a
	// healthy second server.
	server2 := newInstrumentedServer()
	server2.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func F()"}}, nil
	})
	var reconnects atomic.Int64
	p.connectOnce = func(absRoot string) error {
		reconnects.Add(1)
		c2, in2, out2, cl2 := newPipedClient(t)
		go server2.run(in2, out2)
		t.Cleanup(cl2)
		p.lifetimeMu.Lock()
		p.client = c2
		p.lifetimeMu.Unlock()
		return nil
	}
	// Pin backoff small so the test is fast.
	p.dialBackoffStart = 1 * time.Millisecond
	p.maxDialBackoff = 5 * time.Millisecond

	require.NoError(t, runEnrich(t, p, g, repoRoot, 15*time.Second))

	assert.GreaterOrEqual(t, int(reconnects.Load()), 1, "expected at least one reconnect on server exit")
}

// TestLSP_Enrich_UsesRetriedHoverResult forces the single hover down the
// recover-and-retry path and asserts the retried hover's payload is actually
// recorded. The first (and only) hover kills its client and then parks, so the
// goroutine observes "LSP server exited" — never a response — which makes the
// reconnect-and-retry the ONLY path by which this node can be stamped. The
// second server serves a real type payload; the node must end the pass carrying
// that type.
func TestLSP_Enrich_UsesRetriedHoverResult(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot, g := seedRepo(t, 1)

	server1 := newInstrumentedServer()
	p, cleanup := providerWithInstrumentedServer(t, server1, []string{"go"}, 1)
	defer cleanup()
	deadClient := p.client

	// The parked handler never answers; releasing it at cleanup lets the
	// leaked server goroutine unwind.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var killOnce sync.Once
	server1.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		killOnce.Do(func() {
			deadClient.mu.Lock()
			if !deadClient.closed {
				deadClient.closed = true
				close(deadClient.done)
			}
			deadClient.mu.Unlock()
		})
		<-release // never reply: the goroutine must observe the server exit, not a response
		return nil, &jsonRPCError{Code: -32603, Message: "dead"}
	})

	// Second server: healthy, serves a type payload extractTypeFromHover parses.
	server2 := newInstrumentedServer()
	server2.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func F0() string"}}, nil
	})
	var reconnects atomic.Int64
	p.connectOnce = func(absRoot string) error {
		reconnects.Add(1)
		c2, in2, out2, cl2 := newPipedClient(t)
		go server2.run(in2, out2)
		t.Cleanup(cl2)
		p.lifetimeMu.Lock()
		p.client = c2
		p.lifetimeMu.Unlock()
		return nil
	}
	p.dialBackoffStart = 1 * time.Millisecond
	p.maxDialBackoff = 5 * time.Millisecond

	var result *semantic.EnrichResult
	var enrichErr error
	done := make(chan struct{})
	go func() {
		result, enrichErr = p.Enrich(g, repoRoot)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Enrich timed out")
	}

	require.NoError(t, enrichErr)
	require.GreaterOrEqual(t, int(reconnects.Load()), 1, "the dead server must have forced a reconnect")
	require.NotNil(t, result)
	assert.Equal(t, 1, result.NodesEnriched, "the retried hover's result must be recorded, not discarded")

	node := g.GetNode("main.go::F0")
	require.NotNil(t, node)
	require.NotNil(t, node.Meta)
	assert.Equal(t, "func F0() string", node.Meta["semantic_type"],
		"the node must carry the type from the retried hover")
}

// TestLSP_Enrich_SingleReconnectUnderConcurrency verifies that when many
// goroutines observe server-exit simultaneously, only ONE reconnection is
// performed (others wait and retry).
func TestLSP_Enrich_SingleReconnectUnderConcurrency(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot, g := seedRepo(t, 40)

	server1 := newInstrumentedServer()
	p, cleanup := providerWithInstrumentedServer(t, server1, []string{"go"}, 10)
	defer cleanup()
	deadClient := p.client

	var killOnce sync.Once
	server1.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		// Kill the server on the very first hover so a burst of concurrent
		// goroutines all observe the exit together.
		killOnce.Do(func() {
			deadClient.mu.Lock()
			if !deadClient.closed {
				deadClient.closed = true
				close(deadClient.done)
			}
			deadClient.mu.Unlock()
		})
		return nil, &jsonRPCError{Code: -32603, Message: "dying"}
	})

	server2 := newInstrumentedServer()
	server2.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func F()"}}, nil
	})
	var reconnects atomic.Int64
	var mu sync.Mutex
	p.connectOnce = func(absRoot string) error {
		reconnects.Add(1)
		mu.Lock()
		c2, in2, out2, cl2 := newPipedClient(t)
		go server2.run(in2, out2)
		t.Cleanup(cl2)
		p.lifetimeMu.Lock()
		p.client = c2
		p.lifetimeMu.Unlock()
		mu.Unlock()
		return nil
	}
	p.dialBackoffStart = 1 * time.Millisecond
	p.maxDialBackoff = 5 * time.Millisecond

	require.NoError(t, runEnrich(t, p, g, repoRoot, 15*time.Second))

	// Exactly one reconnect even though up to 10 goroutines saw the exit.
	assert.Equal(t, int64(1), reconnects.Load(),
		"only one reconnection should occur even under concurrent server-exit detection")
}

// TestLSP_Enrich_AbortsWhenReconnectFails verifies that a permanently-dead
// server causes Enrich to return an error after exhausting retries.
func TestLSP_Enrich_AbortsWhenReconnectFails(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot, g := seedRepo(t, 6)

	server1 := newInstrumentedServer()
	p, cleanup := providerWithInstrumentedServer(t, server1, []string{"go"}, 2)
	defer cleanup()
	deadClient := p.client

	var killOnce sync.Once
	server1.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		killOnce.Do(func() {
			deadClient.mu.Lock()
			if !deadClient.closed {
				deadClient.closed = true
				close(deadClient.done)
			}
			deadClient.mu.Unlock()
		})
		return nil, &jsonRPCError{Code: -32603, Message: "dying"}
	})

	var attempts atomic.Int64
	p.connectOnce = func(absRoot string) error {
		attempts.Add(1)
		return fmt.Errorf("dial refused")
	}
	p.dialBackoffStart = 1 * time.Millisecond
	p.maxDialBackoff = 3 * time.Millisecond

	err := runEnrich(t, p, g, repoRoot, 15*time.Second)
	require.Error(t, err, "Enrich must return an error when reconnection fails permanently")
	assert.GreaterOrEqual(t, int(attempts.Load()), 3, "must retry reconnect at least 3 times before giving up")
}

// TestLSP_Enrich_ReopensDocsOnNewServerAfterReconnect verifies the strict
// post-reconnect contract: after the server dies mid-flight while a file is
// held open, documents are re-opened on the FRESH server (which has no record
// of the dead session's opens) rather than hovered against an assumed-open
// doc. The new server must therefore receive a paired didOpen/didClose.
func TestLSP_Enrich_ReopensDocsOnNewServerAfterReconnect(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot, g := seedRepo(t, 8)        // 8 nodes, all in main.go

	server1 := newInstrumentedServer()
	p, cleanup := providerWithInstrumentedServer(t, server1, []string{"go"}, 8)
	defer cleanup()
	deadClient := p.client

	var killOnce sync.Once
	server1.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		// Kill on the first hover so the whole burst observes the exit
		// while main.go is still held open on server1.
		killOnce.Do(func() {
			deadClient.mu.Lock()
			if !deadClient.closed {
				deadClient.closed = true
				close(deadClient.done)
			}
			deadClient.mu.Unlock()
		})
		return nil, &jsonRPCError{Code: -32603, Message: "dying"}
	})

	server2 := newInstrumentedServer()
	server2.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func F()"}}, nil
	})
	p.connectOnce = func(absRoot string) error {
		c2, in2, out2, cl2 := newPipedClient(t)
		go server2.run(in2, out2)
		t.Cleanup(cl2)
		p.lifetimeMu.Lock()
		p.client = c2
		p.lifetimeMu.Unlock()
		return nil
	}
	p.dialBackoffStart = 1 * time.Millisecond
	p.maxDialBackoff = 5 * time.Millisecond

	require.NoError(t, runEnrich(t, p, g, repoRoot, 15*time.Second))

	// Wait for the fresh server to drain its didOpen/didClose before sampling.
	require.Eventually(t, func() bool {
		_, o, c := server2.stats()
		return o >= 1 && o == c
	}, 2*time.Second, 5*time.Millisecond, "fresh server should drain its didOpen/didClose")

	_, opens2, closes2 := server2.stats()
	assert.GreaterOrEqual(t, opens2, 1,
		"main.go must be re-opened on the fresh server after reconnect, not assumed open")
	assert.Equal(t, opens2, closes2,
		"every didOpen on the new server must be paired with a didClose (opens=%d closes=%d)", opens2, closes2)
}

// ---------------------------------------------------------------------------
// isServerExitError detection.
// ---------------------------------------------------------------------------

func TestLSP_IsServerExitError(t *testing.T) {
	assert.True(t, isServerExitError(fmt.Errorf("LSP server exited")))
	assert.True(t, isServerExitError(fmt.Errorf("write |1: broken pipe")))
	assert.True(t, isServerExitError(fmt.Errorf("read tcp: connection reset by peer")))
	assert.True(t, isServerExitError(fmt.Errorf("client is closed")))
	assert.False(t, isServerExitError(nil))
	assert.False(t, isServerExitError(&jsonRPCError{Code: -32603, Message: "internal error"}))
}
