package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// breakerFixture seeds nSites caller files, each with one sited ambiguous
// edge to the same declaration, so the definition pass has one group per
// file. The definition handler is the test's to wire.
func breakerFixture(t *testing.T, repoRoot string, g graph.Store, nSites int) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "def.go"),
		[]byte("package p\nfunc target() {}\n"), 0o644))
	g.AddNode(&graph.Node{ID: "def.go::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "def.go", StartLine: 2, EndLine: 2, Language: "go"})
	for i := 0; i < nSites; i++ {
		rel := fmt.Sprintf("call%03d.go", i)
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, rel),
			[]byte("package p\nfunc caller() { target() }\n"), 0o644))
		callerID := rel + "::caller"
		g.AddNode(&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "caller",
			FilePath: rel, StartLine: 2, EndLine: 2, Language: "go"})
		g.AddEdge(&graph.Edge{
			From: callerID, To: "def.go::target", Kind: graph.EdgeCalls,
			FilePath: rel, Line: 2,
			Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
		})
	}
}

// Under the heavy opt-out the definition pass carries the whole confirm load,
// so it must feed the targeted failure-streak breaker: a server that errors
// every definition request has to abort the pass after the streak limit, not
// grind through every sited target at one timeout each.
func TestLSP_Enrich_DefinitionPassTripsBreakerOnDeadServer(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	t.Setenv("GORTEX_LSP_BREAKER", "5")
	const nSites = 40
	const maxParallel = 2

	repoRoot := t.TempDir()
	g := graph.New()
	server := newFakeLSPServer()
	breakerFixture(t, repoRoot, g, nSites)

	var defs atomic.Int64
	server.handle("textDocument/definition", func(json.RawMessage) (any, *jsonRPCError) {
		defs.Add(1)
		return nil, &jsonRPCError{Code: -32603, Message: "boom"}
	})
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })

	p, cleanup := providerWithFakeServerParallel(t, server, []string{"go"}, maxParallel)
	defer cleanup()
	p.noHeavyRequests = true

	require.NoError(t, runEnrich(t, p, g, repoRoot, 10*time.Second))

	// Streak limit plus the requests already in flight across maxParallel
	// goroutines when the breaker trips.
	assert.LessOrEqual(t, defs.Load(), int64(5+maxParallel),
		"a dead server must trip the targeted breaker, not answer every site")
}

// The trip must come from transport failures only. A server that ANSWERS
// every definition request with an empty result is healthy — no verdict for
// the edge, but the breaker stays disarmed and every site gets asked.
func TestLSP_Enrich_DefinitionPassEmptyAnswersDoNotTrip(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	t.Setenv("GORTEX_LSP_BREAKER", "5")
	const nSites = 40

	repoRoot := t.TempDir()
	g := graph.New()
	server := newFakeLSPServer()
	breakerFixture(t, repoRoot, g, nSites)

	var defs atomic.Int64
	server.handle("textDocument/definition", func(json.RawMessage) (any, *jsonRPCError) {
		defs.Add(1)
		return []Location{}, nil
	})
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })

	p, cleanup := providerWithFakeServerParallel(t, server, []string{"go"}, 2)
	defer cleanup()
	p.noHeavyRequests = true

	require.NoError(t, runEnrich(t, p, g, repoRoot, 10*time.Second))

	assert.Equal(t, int64(nSites), defs.Load(),
		"answered-but-empty is not a failure; every site must still be asked")
}

// A PRODUCTIVE definition pass must survive the productivity checkpoint.
// Under the heavy opt-out the def pass carries the whole confirm load, and
// on a warm graph it is the only yield source until the hover phase — if its
// confirm/add/rebind arms do not feed usefulYield, the checkpoint reads a
// pass that is settling hundreds of edges as zero-yield and cancels it at
// the first tick after request volume flows.
func TestLSP_Enrich_DefinitionPassYieldSurvivesProductivityCheckpoint(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	t.Setenv("GORTEX_LSP_PRODUCTIVITY_WINDOW", "150ms")
	const nSites = 300

	repoRoot := t.TempDir()
	g := graph.New()
	server := newFakeLSPServer()
	breakerFixture(t, repoRoot, g, nSites)

	server.handle("textDocument/definition", func(json.RawMessage) (any, *jsonRPCError) {
		// Slow enough that the pass spans several checkpoint windows while
		// confirming at ~30 edges per window — far above the 10-per-window
		// floor a productive pass must sustain.
		time.Sleep(5 * time.Millisecond)
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, "def.go")),
			Range: Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 11}},
		}}, nil
	})
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })

	p, cleanup := providerWithFakeServerParallel(t, server, []string{"go"}, 2)
	defer cleanup()
	p.noHeavyRequests = true

	require.NoError(t, runEnrich(t, p, g, repoRoot, 60*time.Second))

	confirmed := 0
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeCalls && e.Confidence == 1.0 {
			confirmed++
		}
	}
	assert.Equal(t, nSites, confirmed,
		"a pass confirming hundreds of edges must not be cut by the productivity checkpoint")
}

// The heavy opt-out must also cover the on-demand path: ConfirmSymbolRefs
// rides callHierarchy/incomingCalls, the same FindReferences machinery the
// sweep skips. A long-lived daemon answering find_usages against a leaky
// server would otherwise accumulate the leak one query at a time.
func TestLSP_ConfirmSymbolRefs_NoHeavySkipsIncoming(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "svc.go"),
		[]byte("package p\nfunc served() {}\n"), 0o644))
	g := graph.New()
	n := &graph.Node{ID: "svc.go::served", Kind: graph.KindFunction, Name: "served",
		FilePath: "svc.go", StartLine: 2, EndLine: 2, Language: "go"}
	g.AddNode(n)

	server := newFakeLSPServer()
	var prepares, incomings atomic.Int64
	server.handle("textDocument/prepareCallHierarchy", func(json.RawMessage) (any, *jsonRPCError) {
		prepares.Add(1)
		return []CallHierarchyItem{{Name: "served"}}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		incomings.Add(1)
		return []CallHierarchyIncomingCall{}, nil
	})

	p, cleanup := providerWithFakeServerParallel(t, server, []string{"go"}, 2)
	defer cleanup()
	p.noHeavyRequests = true

	confirmed, err := p.ConfirmSymbolRefs(g, repoRoot, n)
	require.NoError(t, err)
	assert.Zero(t, confirmed)
	assert.Zero(t, incomings.Load(),
		"incomingCalls must never be issued under the heavy opt-out")
}
