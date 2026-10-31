package lsp

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLSP_Enrich_CrashLoopGuardAbandonsPass pins the crash-loop guard: a server
// that connects cleanly but keeps exiting mid-request (the clangd/clang-tidy
// failure shape) must not pin the pass in an endless crash -> reconnect -> crash
// loop. After the per-pass cap the provider's enrichment is abandoned with an
// error and a bounded reconnect count, and whatever landed earlier stays.
func TestLSP_Enrich_CrashLoopGuardAbandonsPass(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot, g := seedRepo(t, 60)

	server1 := newInstrumentedServer()
	p, cleanup := providerWithInstrumentedServer(t, server1, []string{"go"}, 4)
	defer cleanup()
	// Keep backoff tiny so the crash loop churns quickly.
	p.dialBackoffStart = 1 * time.Millisecond
	p.maxDialBackoff = 2 * time.Millisecond

	kill := func(c *Client) {
		c.mu.Lock()
		if !c.closed {
			c.closed = true
			close(c.done)
		}
		c.mu.Unlock()
	}
	// dyingHover kills its own client on the first hover, so the initial server
	// and every reconnect's replacement die mid-request in turn.
	dyingHover := func(c *Client) func(json.RawMessage) (any, *jsonRPCError) {
		var once sync.Once
		return func(_ json.RawMessage) (any, *jsonRPCError) {
			once.Do(func() { kill(c) })
			return nil, &jsonRPCError{Code: -32603, Message: "server exited"}
		}
	}
	server1.handle("textDocument/hover", dyingHover(p.client))

	var reconnects atomic.Int64
	p.connectOnce = func(_ string) error {
		reconnects.Add(1)
		srv := newInstrumentedServer()
		c, in, out, cl := newPipedClient(t)
		go srv.run(in, out)
		t.Cleanup(cl)
		srv.handle("textDocument/hover", dyingHover(c))
		p.lifetimeMu.Lock()
		p.client = c
		p.lifetimeMu.Unlock()
		return nil
	}

	err := runEnrich(t, p, g, repoRoot, 20*time.Second)
	require.Error(t, err, "a server that keeps crashing must abandon the pass, not loop forever")

	n := int(reconnects.Load())
	assert.Greater(t, n, 0, "should have attempted at least one reconnect")
	assert.LessOrEqual(t, n, maxEnrichReconnectCycles+1,
		"reconnects must be bounded by the crash-loop cap, got %d", n)
}
