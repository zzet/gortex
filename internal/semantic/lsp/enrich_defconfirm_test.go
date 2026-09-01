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

// The csharp-ls / omnisharp spec opts out of the heavy request classes:
// textDocument/references and callHierarchy/incomingCalls both ride Roslyn's
// FindReferences machinery, which leaks unboundedly per request in csharp-ls.
// Edge confirmation goes through textDocument/definition at the call site
// instead — same verdicts, position-request cost.
func TestCSharpSpecOptsOutOfHeavyRequests(t *testing.T) {
	spec := SpecByName("omnisharp")
	require.NotNil(t, spec)
	assert.True(t, spec.NoHeavyRequests, "csharp spec must skip references/incomingCalls")

	p := NewProviderFromSpec(spec, nil)
	assert.True(t, p.noHeavyRequests, "spec opt-out must reach the provider")

	def := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
	assert.False(t, def.noHeavyRequests, "a spec-less provider keeps the heavy legs")
}

// defConfirmFixture seeds one ambiguous call edge whose site line names the
// target, plus fake-server handlers for references (counting), definition
// (answering the target's declaration), and hover. The returned edge is the
// live store object — ConfirmEdge mutates it in place.
func defConfirmFixture(t *testing.T, repoRoot string, g graph.Store, server *fakeLSPServer) (edge *graph.Edge, refs, defs *atomic.Int64) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "def.go"),
		[]byte("package p\nfunc target() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "call.go"),
		[]byte("package p\nfunc caller() { target() }\n"), 0o644))

	g.AddNode(&graph.Node{ID: "def.go::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "def.go", StartLine: 2, EndLine: 2, Language: "go"})
	g.AddNode(&graph.Node{ID: "call.go::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "call.go", StartLine: 2, EndLine: 2, Language: "go"})
	edge = &graph.Edge{
		From: "call.go::caller", To: "def.go::target", Kind: graph.EdgeCalls,
		FilePath: "call.go", Line: 2,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
	}
	g.AddEdge(edge)

	refs, defs = &atomic.Int64{}, &atomic.Int64{}
	// References would confirm the site (the location matches the edge) — the
	// point of the heavy opt-out is that it must never be asked.
	server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
		refs.Add(1)
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, "call.go")),
			Range: Range{Start: Position{Line: 1, Character: 16}, End: Position{Line: 1, Character: 22}},
		}}, nil
	})
	server.handle("textDocument/definition", func(json.RawMessage) (any, *jsonRPCError) {
		defs.Add(1)
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, "def.go")),
			Range: Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 11}},
		}}, nil
	})
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	return edge, refs, defs
}

// With the heavy opt-out, an ambiguous edge is confirmed through definition at
// its call site — and the references round trip never happens.
func TestLSP_Enrich_NoHeavy_DefinitionConfirmsWithoutReferences(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot := t.TempDir()
	g := graph.New()
	server := newFakeLSPServer()
	edge, refs, defs := defConfirmFixture(t, repoRoot, g, server)

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.noHeavyRequests = true

	require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

	assert.Zero(t, refs.Load(), "references must never be requested under the heavy opt-out")
	assert.Positive(t, defs.Load(), "the confirm verdict must come from definition")
	assert.Equal(t, graph.OriginLSPResolved, edge.Origin, "the edge must be promoted")
	assert.Equal(t, 1.0, edge.Confidence)
}

// Without the opt-out nothing changes: the references confirm sweep still runs
// first — this pins that gopls-shaped providers are untouched.
func TestLSP_Enrich_HeavyDefault_StillIssuesReferences(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot := t.TempDir()
	g := graph.New()
	server := newFakeLSPServer()
	edge, refs, _ := defConfirmFixture(t, repoRoot, g, server)

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

	assert.Positive(t, refs.Load(), "the default path still confirms through references")
	assert.Equal(t, graph.OriginLSPResolved, edge.Origin)
}

// The heavy opt-out also silences callHierarchy/incomingCalls — even for a
// dispatch-relevant method that the demand default would interrogate. The
// outgoing side stays: it is free today and lights up if the server ever
// implements it.
func TestLSP_Enrich_NoHeavy_SkipsIncomingEvenForDispatchMethod(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "svc.go"),
		[]byte("package p\n\ntype Shape interface{ Area() float64 }\n\ntype Circle struct{}\n\nfunc (c Circle) Area() float64 { return 0 }\n"), 0o644))

	server := newFakeLSPServer()
	prepare, outgoing, incoming := callHierarchyCounters(server, repoRoot)

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.noHeavyRequests = true

	g := graph.New()
	g.AddNode(&graph.Node{ID: "svc.go::Shape", Kind: graph.KindInterface, Name: "Shape",
		FilePath: "svc.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "svc.go::Circle", Kind: graph.KindType, Name: "Circle",
		FilePath: "svc.go", StartLine: 5, EndLine: 5, Language: "go"})
	g.AddNode(&graph.Node{ID: "svc.go::Circle.Area", Kind: graph.KindMethod, Name: "Area",
		FilePath: "svc.go", StartLine: 7, EndLine: 7, Language: "go"})
	g.AddEdge(&graph.Edge{From: "svc.go::Circle.Area", To: "svc.go::Circle", Kind: graph.EdgeMemberOf})
	g.AddEdge(&graph.Edge{From: "svc.go::Circle", To: "svc.go::Shape", Kind: graph.EdgeImplements})

	require.NoError(t, runEnrich(t, p, g, repoRoot, 3*time.Second))

	assert.Positive(t, prepare.Load())
	assert.Positive(t, outgoing.Load(), "outgoing stays on — it is not a heavy request")
	assert.Zero(t, incoming.Load(), "incoming rides FindReferences and must be skipped")
}

// Definition on `new T(...)` answers the constructor's declaration line, not
// the type's. An instantiates edge targeting the type must still count that
// as an exact confirm: the constructor is a member of the very type the edge
// names.
func TestLSP_Enrich_DefConfirm_CtorHitConfirmsTypeTarget(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "dom.go"),
		[]byte("package p\ntype Crate struct{\n\tn int\n}\nfunc ctor() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "use.go"),
		[]byte("package p\nfunc maker() { _ = Crate{} }\n"), 0o644))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "dom.go::Crate", Kind: graph.KindType, Name: "Crate",
		FilePath: "dom.go", StartLine: 2, EndLine: 5, Language: "go"})
	// The constructor: a differently-named member on a line inside the type's
	// span — the shape a C# `Crate.<init>` node has.
	ctor := &graph.Node{ID: "dom.go::Crate.<init>", Kind: graph.KindMethod, Name: "Crate.<init>",
		FilePath: "dom.go", StartLine: 5, EndLine: 5, Language: "go"}
	g.AddNode(ctor)
	g.AddEdge(&graph.Edge{From: ctor.ID, To: "dom.go::Crate", Kind: graph.EdgeMemberOf})
	g.AddNode(&graph.Node{ID: "use.go::maker", Kind: graph.KindFunction, Name: "maker",
		FilePath: "use.go", StartLine: 2, EndLine: 2, Language: "go"})
	edge := &graph.Edge{
		From: "use.go::maker", To: "dom.go::Crate", Kind: graph.EdgeInstantiates,
		FilePath: "use.go", Line: 2,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
	}
	g.AddEdge(edge)

	server := newFakeLSPServer()
	server.handle("textDocument/definition", func(json.RawMessage) (any, *jsonRPCError) {
		// The ctor's declaration line (0-based 4), inside Crate's span.
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, "dom.go")),
			Range: Range{Start: Position{Line: 4, Character: 5}, End: Position{Line: 4, Character: 9}},
		}}, nil
	})
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.noHeavyRequests = true

	require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

	assert.Equal(t, graph.OriginLSPResolved, edge.Origin,
		"a definition landing on the ctor confirms the containing-type target")
	assert.Equal(t, 1.0, edge.Confidence)
	assert.Equal(t, "dom.go::Crate", edge.To, "the edge keeps its type target — no retarget to the ctor")
}

// dispatchedCallFixture seeds a dispatched call site: an interface with a
// declared member, one concrete impl, and a stored AST-inferred call edge
// targeting the impl. The fake server's definition answers the interface's
// declared member — the compiler vouching for the declaration, not the
// devirtualization guess. Returns the live impl edge.
func dispatchedCallFixture(t *testing.T, repoRoot string, g graph.Store, server *fakeLSPServer) *graph.Edge {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "scales.go"),
		[]byte("package p\ntype IScale interface {\n\tWeigh() int\n}\n\ntype DrumScale struct{\n\tWeighImpl int\n}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "station.go"),
		[]byte("package p\nfunc station() { Weigh() }\n"), 0o644))

	g.AddNode(&graph.Node{ID: "scales.go::IScale", Kind: graph.KindInterface, Name: "IScale",
		FilePath: "scales.go", StartLine: 2, EndLine: 4, Language: "go"})
	g.AddNode(&graph.Node{ID: "scales.go::IScale.Weigh", Kind: graph.KindMethod, Name: "Weigh",
		FilePath: "scales.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddEdge(&graph.Edge{From: "scales.go::IScale.Weigh", To: "scales.go::IScale", Kind: graph.EdgeMemberOf})
	g.AddNode(&graph.Node{ID: "scales.go::DrumScale", Kind: graph.KindType, Name: "DrumScale",
		FilePath: "scales.go", StartLine: 6, EndLine: 8, Language: "go"})
	g.AddNode(&graph.Node{ID: "scales.go::DrumScale.Weigh", Kind: graph.KindMethod, Name: "Weigh",
		FilePath: "scales.go", StartLine: 7, EndLine: 7, Language: "go"})
	g.AddEdge(&graph.Edge{From: "scales.go::DrumScale.Weigh", To: "scales.go::DrumScale", Kind: graph.EdgeMemberOf})
	g.AddEdge(&graph.Edge{From: "scales.go::DrumScale", To: "scales.go::IScale", Kind: graph.EdgeImplements})
	g.AddNode(&graph.Node{ID: "station.go::station", Kind: graph.KindFunction, Name: "station",
		FilePath: "station.go", StartLine: 2, EndLine: 2, Language: "go"})
	implEdge := &graph.Edge{
		From: "station.go::station", To: "scales.go::DrumScale.Weigh", Kind: graph.EdgeCalls,
		FilePath: "station.go", Line: 2,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginASTInferred,
	}
	g.AddEdge(implEdge)

	server.handle("textDocument/definition", func(json.RawMessage) (any, *jsonRPCError) {
		// The compiler's answer: the interface's declared member (0-based 2).
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, "scales.go")),
			Range: Range{Start: Position{Line: 2, Character: 1}, End: Position{Line: 2, Character: 6}},
		}}, nil
	})
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	return implEdge
}

// A dispatched call site: definition answers the interface's declared member
// while the stored edge targets one concrete impl. The declared-member edge is
// added at LSP grade; the impl edge — an inference the compiler cannot vouch
// for — is left exactly as it was: not retargeted, not demoted, not promoted.
func TestLSP_Enrich_DefConfirm_DispatchedCallAddsDeclaredMemberEdge(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot := t.TempDir()
	g := graph.New()
	server := newFakeLSPServer()
	implEdge := dispatchedCallFixture(t, repoRoot, g, server)

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.noHeavyRequests = true

	require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

	assert.Equal(t, "scales.go::DrumScale.Weigh", implEdge.To,
		"the impl edge keeps its target — the devirtualization guess is not destroyed")
	assert.Equal(t, graph.OriginASTInferred, implEdge.Origin,
		"the impl edge keeps its origin — an inference stays labeled as one")
	assert.Equal(t, 0.7, implEdge.Confidence)

	var declared *graph.Edge
	for _, e := range g.GetOutEdges("station.go::station") {
		if e.To == "scales.go::IScale.Weigh" && e.Kind == graph.EdgeCalls {
			declared = e
		}
	}
	require.NotNil(t, declared, "the compiler-proven edge to the declared member must be added")
	assert.Equal(t, graph.OriginLSPResolved, declared.Origin)
	assert.Equal(t, 2, declared.Line, "the added edge carries the call site")
}

// The declared-member arm lives in the shared definition-rebind pass, so it is
// default-path behavior, not part of the heavy opt-out. With the heavy legs ON
// and references answering empty, the unconfirmed site falls through to the
// definition pass — and the verdict must be the same add-not-rebind: previously
// this path REBOUND the impl edge to the declared member, so a gopls/tsserver/
// pyright repo now keeps the devirtualization guess AND gains the declared edge.
func TestLSP_Enrich_HeavyDefault_DispatchedCallAddsDeclaredMemberEdge(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot := t.TempDir()
	g := graph.New()
	server := newFakeLSPServer()
	implEdge := dispatchedCallFixture(t, repoRoot, g, server)

	// The references sweep runs first and must yield no verdict, leaving the
	// site to the definition pass.
	refs := &atomic.Int64{}
	server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
		refs.Add(1)
		return []Location{}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	// noHeavyRequests stays false — this is every spec-less server's path.

	require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

	assert.Positive(t, refs.Load(), "the default path must still run its references sweep")
	assert.Equal(t, "scales.go::DrumScale.Weigh", implEdge.To,
		"the impl edge keeps its target — the devirtualization guess is not destroyed")
	assert.Equal(t, graph.OriginASTInferred, implEdge.Origin,
		"the impl edge keeps its origin — an inference stays labeled as one")
	assert.Equal(t, 0.7, implEdge.Confidence)

	var declared *graph.Edge
	for _, e := range g.GetOutEdges("station.go::station") {
		if e.To == "scales.go::IScale.Weigh" && e.Kind == graph.EdgeCalls {
			declared = e
		}
	}
	require.NotNil(t, declared, "the compiler-proven edge must be added on the default path too")
	assert.Equal(t, graph.OriginLSPResolved, declared.Origin)
	assert.Equal(t, 2, declared.Line, "the added edge carries the call site")
}

// GORTEX_LSP_HEAVY overrides the heavy-request opt-out in both directions:
// "on" restores references / incomingCalls for a spec that opted out (the
// operator runs a patched server without the leak), "off" force-disables
// them for every server. Empty or unrecognised falls through to the spec.
func TestResolveNoHeavyRequests_Precedence(t *testing.T) {
	csharp := SpecByName("omnisharp")
	require.NotNil(t, csharp)

	t.Run("default follows the spec", func(t *testing.T) {
		t.Setenv(HeavyRequestsEnv, "")
		assert.False(t, resolveNoHeavyRequests(nil), "spec-less providers keep the heavy legs")
		assert.True(t, resolveNoHeavyRequests(csharp))
	})
	t.Run("env on re-enables heavies for an opted-out spec", func(t *testing.T) {
		t.Setenv(HeavyRequestsEnv, "on")
		assert.False(t, resolveNoHeavyRequests(csharp))
	})
	t.Run("env off disables heavies for every server", func(t *testing.T) {
		t.Setenv(HeavyRequestsEnv, "off")
		assert.True(t, resolveNoHeavyRequests(nil))
	})
	t.Run("unrecognised value falls through to the spec", func(t *testing.T) {
		t.Setenv(HeavyRequestsEnv, "garbage")
		assert.True(t, resolveNoHeavyRequests(csharp))
		assert.False(t, resolveNoHeavyRequests(nil))
	})
	t.Run("constructors plumb the override", func(t *testing.T) {
		t.Setenv(HeavyRequestsEnv, "on")
		p := NewProviderFromSpec(csharp, nil)
		assert.False(t, p.noHeavyRequests, "a patched server's operator can restore references/incoming")
		t.Setenv(HeavyRequestsEnv, "off")
		def := NewProvider("fake-lsp", nil, []string{"go"}, false, 2, nil)
		assert.True(t, def.noHeavyRequests)
	})
}

// The definition pass fans out across site files. The serial loop predates
// the NoDidOpen lifecycle — it existed to keep document opens from
// overlapping — but with no didOpen to serialize, a heavy-opt-out server
// answering 46k definitions one at a time is the pass's longest pole. Six
// site files with a slow definition handler must overlap.
func TestLSP_Enrich_DefinitionPassRunsSiteFilesInParallel(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot := t.TempDir()
	g := graph.New()

	const n = 6
	for i := 0; i < n; i++ {
		defFile := fmt.Sprintf("def%d.go", i)
		callFile := fmt.Sprintf("call%d.go", i)
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, defFile),
			[]byte(fmt.Sprintf("package p\nfunc target%d() {}\n", i)), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, callFile),
			[]byte(fmt.Sprintf("package p\nfunc caller%d() { target%d() }\n", i, i)), 0o644))
		g.AddNode(&graph.Node{ID: fmt.Sprintf("%s::target%d", defFile, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("target%d", i), FilePath: defFile, StartLine: 2, EndLine: 2, Language: "go"})
		g.AddNode(&graph.Node{ID: fmt.Sprintf("%s::caller%d", callFile, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("caller%d", i), FilePath: callFile, StartLine: 2, EndLine: 2, Language: "go"})
		g.AddEdge(&graph.Edge{
			From: fmt.Sprintf("%s::caller%d", callFile, i), To: fmt.Sprintf("%s::target%d", defFile, i),
			Kind: graph.EdgeCalls, FilePath: callFile, Line: 2,
			Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
		})
	}

	// The instrumented rig dispatches each request in its own goroutine, so
	// concurrency is genuinely observable (fakeLSPServer answers inline in
	// its read loop and would pin in-flight at 1 regardless of the client).
	server := newInstrumentedServer()
	var inflight, maxInflight atomic.Int64
	server.handle("textDocument/definition", func(params json.RawMessage) (any, *jsonRPCError) {
		cur := inflight.Add(1)
		for {
			prev := maxInflight.Load()
			if cur <= prev || maxInflight.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		inflight.Add(-1)
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		var idx int
		if _, err := fmt.Sscanf(filepath.Base(uriToAbsPath(req.TextDocument.URI)), "call%d.go", &idx); err != nil {
			return []Location{}, nil
		}
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, fmt.Sprintf("def%d.go", idx))),
			Range: Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 12}},
		}}, nil
	})
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()
	p.noHeavyRequests = true

	require.NoError(t, runEnrich(t, p, g, repoRoot, 10*time.Second))

	assert.GreaterOrEqual(t, maxInflight.Load(), int64(2),
		"definition requests for distinct site files must overlap")
	confirmed := 0
	for i := 0; i < n; i++ {
		for _, e := range g.GetOutEdges(fmt.Sprintf("call%d.go::caller%d", i, i)) {
			if e.Kind == graph.EdgeCalls && e.Origin == graph.OriginLSPResolved {
				confirmed++
			}
		}
	}
	assert.Equal(t, n, confirmed, "parallelism must not cost any confirm verdict")
}

// The no-heavy fallback must include every SITED target, not just those the
// references sweep could serve: groupConfirmTargets drops targets on
// referent-side conditions (no position, unserved referent file) that exist
// only because findReferences opens the referent's file. The definition pass
// opens the call-site file, so a misbound edge whose stored target has no
// usable position must still reach the pass — and get rebound to the real
// declaration.
func TestLSP_Enrich_NoHeavy_RebindsSitedEdgeWithPositionlessTarget(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "def.go"),
		[]byte("package p\nfunc target() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "call.go"),
		[]byte("package p\nfunc caller() { target() }\n"), 0o644))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "def.go::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "def.go", StartLine: 2, EndLine: 2, Language: "go"})
	// The heuristically-bound target: same name, NO position — the exact
	// shape groupConfirmTargets cannot serve.
	g.AddNode(&graph.Node{ID: "ghost.go::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "ghost.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "call.go::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "call.go", StartLine: 2, EndLine: 2, Language: "go"})
	edge := &graph.Edge{
		From: "call.go::caller", To: "ghost.go::target", Kind: graph.EdgeCalls,
		FilePath: "call.go", Line: 2,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
	}
	g.AddEdge(edge)

	server := newFakeLSPServer()
	server.handle("textDocument/definition", func(json.RawMessage) (any, *jsonRPCError) {
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, "def.go")),
			Range: Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 11}},
		}}, nil
	})
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.noHeavyRequests = true

	require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

	assert.Equal(t, "def.go::target", edge.To,
		"the sited edge must be rebound to the declaration the compiler names")
	assert.Equal(t, 1.0, edge.Confidence)
}
