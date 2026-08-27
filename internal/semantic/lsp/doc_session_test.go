package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// A single file that feeds three phases — the interface pass (a KindInterface
// node), the ambiguous-edge confirm pass (an edge whose referent lives in the
// file), and the per-file hover sweep — must be didOpen'd exactly once: the
// shared document session keeps it warm across phases instead of reopening it.
func TestLSP_Enrich_SessionSharedAcrossPasses(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // keep the per-file sweep in play so it shares the warm document
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "shared.go"),
		[]byte("package shared\n\ntype Greeter interface { Greet() string }\n\nfunc Hello() string { return \"\" }\n\nfunc Caller() { _ = Hello() }\n"),
		0o644,
	))

	server := newInstrumentedServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) { return nil, nil })

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()
	// Advertise call / type hierarchy so the sweep exercises the hierarchy
	// path too; the instrumented server answers null, so no hops are added.
	p.caps = ServerCapabilities{CallHierarchyProvider: true, TypeHierarchyProvider: true}

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "shared.go::Greeter", Kind: graph.KindInterface, Name: "Greeter",
		FilePath: "shared.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "shared.go::Hello", Kind: graph.KindFunction, Name: "Hello",
		FilePath: "shared.go", StartLine: 5, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "shared.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: "shared.go", StartLine: 7, EndLine: 7, Language: "go",
	})
	// Ambiguous edge whose referent (Hello) lives in shared.go — drives the
	// confirm pass to open shared.go.
	g.AddEdge(&graph.Edge{
		From: "shared.go::Caller", To: "shared.go::Hello", Kind: graph.EdgeCalls,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginASTInferred,
	})

	require.NoError(t, runEnrich(t, p, g, repoRoot, 10*time.Second))

	require.Eventually(t, func() bool {
		_, o, c := server.stats()
		return o == 1 && c == 1
	}, 3*time.Second, 5*time.Millisecond, "shared.go should be opened once and closed once")

	_, opens, closes := server.stats()
	assert.Equal(t, 1, opens, "shared.go opened exactly once across interface + confirm + sweep")
	assert.Equal(t, opens, closes, "the single open must be paired with a single close")
}

// A caller with several misbound sites in one file must open that site file
// once during the definition-rebind fallback, not once per site. Callers live
// in a.go, the callee in b.go; references never match (forcing fallback) and
// definition answers with the callee, so all sites confirm off one open of
// a.go. Total didOpens stay at two files: a.go (fallback) and b.go (confirm).
func TestLSP_Enrich_FallbackRebindOpensSiteFileOnce(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "a.go"),
		[]byte("package main\nfunc c0() { callee() }\nfunc c1() { callee() }\nfunc c2() { callee() }\nfunc c3() { callee() }\nfunc c4() { callee() }\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "b.go"),
		[]byte("package main\nfunc callee() {}\n"),
		0o644,
	))

	server := newInstrumentedServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	// No matching references → every ambiguous edge falls through to the
	// definition-rebind fallback.
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{}, nil
	})
	// Definition resolves each site to the callee declaration in b.go.
	server.handle("textDocument/definition", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, "b.go")),
			Range: Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 11}},
		}}, nil
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()
	// Call hierarchy present (references-add pass off); type hierarchy present.
	p.caps = ServerCapabilities{CallHierarchyProvider: true, TypeHierarchyProvider: true}

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "b.go::callee", Kind: graph.KindFunction, Name: "callee",
		FilePath: "b.go", StartLine: 2, EndLine: 2, Language: "go",
	})
	for i := 0; i < 5; i++ {
		caller := "c" + string(rune('0'+i))
		siteLine := 2 + i
		g.AddNode(&graph.Node{
			ID: "a.go::" + caller, Kind: graph.KindFunction, Name: caller,
			FilePath: "a.go", StartLine: siteLine, EndLine: siteLine, Language: "go",
		})
		// Misbound ambiguous edge anchored at the caller's call site in a.go.
		g.AddEdge(&graph.Edge{
			From: "a.go::" + caller, To: "b.go::callee", Kind: graph.EdgeCalls,
			FilePath: "a.go", Line: siteLine,
			Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginASTInferred,
		})
	}

	var result *semantic.EnrichResult
	done := make(chan error, 1)
	go func() {
		r, err := p.Enrich(g, repoRoot)
		result = r
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Enrich timed out")
	}

	require.Eventually(t, func() bool {
		_, o, c := server.stats()
		return o == 2 && o == c
	}, 3*time.Second, 5*time.Millisecond, "a.go (fallback) and b.go (confirm) each opened once")

	_, opens, closes := server.stats()
	assert.Equal(t, 2, opens, "five fallback sites in a.go open it once, not once per site")
	assert.Equal(t, opens, closes, "every open must be paired with a close")
	require.NotNil(t, result)
	assert.Equal(t, 5, result.EdgesConfirmed, "all five sites confirm off one open of a.go")
}

// A sequential acquire/release of three files under cap 2 evicts (didCloses)
// the oldest released file when the third opens, keeps the open set within
// cap, and pairs every open with a close after closeAll.
func TestDocSession_LRUEvictsPairedAndBounded(t *testing.T) {
	repoRoot := t.TempDir()
	files := []string{"a.go", "b.go", "c.go"}
	for _, f := range files {
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, f), []byte("package main\n"), 0o644))
	}

	server := newInstrumentedServer()
	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 1)
	defer cleanup()

	session := newDocSession(p)
	session.cap = 2

	for _, f := range files {
		_, release, err := session.acquire(p.client, filepath.Join(repoRoot, f))
		require.NoError(t, err)
		release()
	}

	// Opening c.go (the third file) evicts the oldest refcount-0 entry
	// (a.go), so by now exactly one didClose has been sent.
	require.Eventually(t, func() bool {
		_, o, c := server.stats()
		return o == 3 && c == 1
	}, 2*time.Second, 5*time.Millisecond, "third open evicts + closes exactly the oldest file")

	peak, _, _ := server.stats()
	assert.LessOrEqual(t, peak, session.cap,
		"peak simultaneously-open docs (%d) must stay within cap (%d)", peak, session.cap)

	session.closeAll()
	require.Eventually(t, func() bool {
		_, o, c := server.stats()
		return o == 3 && o == c
	}, 2*time.Second, 5*time.Millisecond, "closeAll closes every remaining open document")

	_, opens, closes := server.stats()
	assert.Equal(t, opens, closes, "opens (%d) must equal closes (%d) after closeAll", opens, closes)
	assert.Equal(t, 3, opens, "each of the three files was opened once")
}

// A session with the lifecycle off (sendOpens=false) still evicts to keep
// the content cache bounded — and the evictions counter must record that
// churn. Eviction telemetry tracks the cache, not the didClose traffic;
// the lifecycle counters (didOpens, peak) stay honestly zero.
func TestDocSession_NoSendOpens_EvictionsStillCounted(t *testing.T) {
	repoRoot := t.TempDir()
	files := []string{"a.go", "b.go", "c.go"}
	for _, f := range files {
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, f), []byte("package main\n"), 0o644))
	}

	server := newInstrumentedServer()
	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 1)
	defer cleanup()

	session := newDocSession(p)
	session.cap = 2
	session.sendOpens = false

	for _, f := range files {
		_, release, err := session.acquire(p.client, filepath.Join(repoRoot, f))
		require.NoError(t, err)
		release()
	}

	didOpens, _, evictions, peakOpen := session.stats()
	assert.Zero(t, didOpens, "no lifecycle — didOpen telemetry stays zero")
	assert.Zero(t, peakOpen)
	assert.Equal(t, 1, evictions,
		"the third acquire evicted the oldest cache entry and must be counted")

	_, opens, closes := server.stats()
	assert.Zero(t, opens, "no didOpen is ever sent")
	assert.Zero(t, closes, "no didClose is ever sent")
}

// Pinned entries are never evicted: holding refs on cap files and acquiring one
// more overshoots cap (no didClose) rather than closing a pinned document.
func TestDocSession_PinnedNeverEvicted(t *testing.T) {
	repoRoot := t.TempDir()
	files := []string{"a.go", "b.go", "c.go"}
	for _, f := range files {
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, f), []byte("package main\n"), 0o644))
	}

	server := newInstrumentedServer()
	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 1)
	defer cleanup()

	session := newDocSession(p)
	session.cap = 2

	// Hold pins on cap (2) files, then acquire one more without releasing.
	_, rel1, err := session.acquire(p.client, filepath.Join(repoRoot, "a.go"))
	require.NoError(t, err)
	_, rel2, err := session.acquire(p.client, filepath.Join(repoRoot, "b.go"))
	require.NoError(t, err)
	_, rel3, err := session.acquire(p.client, filepath.Join(repoRoot, "c.go"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, o, _ := server.stats()
		return o == 3
	}, 2*time.Second, 5*time.Millisecond, "all three files opened")

	peak, opens, closes := server.stats()
	assert.Equal(t, 3, opens, "three files opened")
	assert.Equal(t, 0, closes, "a fully-pinned set is never evicted — no didClose before release")
	assert.Equal(t, 3, peak, "the pinned set overshoots cap")

	rel1()
	rel2()
	rel3()
	session.closeAll()

	require.Eventually(t, func() bool {
		_, o, c := server.stats()
		return o == 3 && o == c
	}, 2*time.Second, 5*time.Millisecond, "closeAll pairs every open with a close after release")
}
