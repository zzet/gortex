package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestEnrichCallableIsDispatchRelevant(t *testing.T) {
	// A concrete method whose declaring type implements an interface: the
	// call to it may dispatch through the interface, so only its incoming
	// side names the concrete callers.
	implMethod := func() (graph.Store, *graph.Node) {
		g := graph.New()
		m := &graph.Node{ID: "s.go::Circle.Area", Kind: graph.KindMethod, Name: "Area"}
		g.AddNode(m)
		g.AddNode(&graph.Node{ID: "s.go::Circle", Kind: graph.KindType, Name: "Circle"})
		g.AddNode(&graph.Node{ID: "s.go::Shape", Kind: graph.KindInterface, Name: "Shape"})
		g.AddEdge(&graph.Edge{From: m.ID, To: "s.go::Circle", Kind: graph.EdgeMemberOf})
		g.AddEdge(&graph.Edge{From: "s.go::Circle", To: "s.go::Shape", Kind: graph.EdgeImplements})
		return g, m
	}
	// A method whose declaring type extends a base type.
	extendMethod := func() (graph.Store, *graph.Node) {
		g := graph.New()
		m := &graph.Node{ID: "s.go::Dog.Speak", Kind: graph.KindMethod, Name: "Speak"}
		g.AddNode(m)
		g.AddNode(&graph.Node{ID: "s.go::Dog", Kind: graph.KindType, Name: "Dog"})
		g.AddNode(&graph.Node{ID: "s.go::Animal", Kind: graph.KindType, Name: "Animal"})
		g.AddEdge(&graph.Edge{From: m.ID, To: "s.go::Dog", Kind: graph.EdgeMemberOf})
		g.AddEdge(&graph.Edge{From: "s.go::Dog", To: "s.go::Animal", Kind: graph.EdgeExtends})
		return g, m
	}
	// A method carrying an explicit overrides edge.
	overrideMethod := func() (graph.Store, *graph.Node) {
		g := graph.New()
		m := &graph.Node{ID: "s.go::Impl.Do", Kind: graph.KindMethod, Name: "Do"}
		g.AddNode(m)
		g.AddEdge(&graph.Edge{From: m.ID, To: "s.go::Base.Do", Kind: graph.EdgeOverrides})
		return g, m
	}
	// A plain method on a type that neither implements nor extends anything.
	plainMethod := func() (graph.Store, *graph.Node) {
		g := graph.New()
		m := &graph.Node{ID: "s.go::Box.Size", Kind: graph.KindMethod, Name: "Size"}
		g.AddNode(m)
		g.AddNode(&graph.Node{ID: "s.go::Box", Kind: graph.KindType, Name: "Box"})
		g.AddEdge(&graph.Edge{From: m.ID, To: "s.go::Box", Kind: graph.EdgeMemberOf})
		return g, m
	}
	// An abstract-marked method (e.g. an interface member) with no edges.
	abstractMethod := func() (graph.Store, *graph.Node) {
		g := graph.New()
		m := &graph.Node{ID: "s.go::Shape.Area", Kind: graph.KindMethod, Name: "Area",
			Meta: map[string]any{"iface_member": true}}
		g.AddNode(m)
		return g, m
	}
	// A plain free function with only unresolved-call demand — demand is a
	// separate signal; dispatch-relevance alone must not fire for it.
	demandFunc := func() (graph.Store, *graph.Node) {
		g := graph.New()
		f := &graph.Node{ID: "s.go::Free", Kind: graph.KindFunction, Name: "Free"}
		g.AddNode(f)
		g.AddEdge(&graph.Edge{From: "s.go::caller", To: graph.UnresolvedMarker + "*.Free", Kind: graph.EdgeCalls})
		return g, f
	}
	// A type node is never a call-hierarchy subject.
	typeNode := func() (graph.Store, *graph.Node) {
		g := graph.New()
		n := &graph.Node{ID: "s.go::Circle", Kind: graph.KindType, Name: "Circle"}
		g.AddNode(n)
		return g, n
	}

	cases := []struct {
		name  string
		build func() (graph.Store, *graph.Node)
		want  bool
	}{
		{"nil", func() (graph.Store, *graph.Node) { return graph.New(), nil }, false},
		{"impl method via type implements", implMethod, true},
		{"method via type extends", extendMethod, true},
		{"method with overrides edge", overrideMethod, true},
		{"abstract-marked method", abstractMethod, true},
		{"plain method on plain type", plainMethod, false},
		{"function with demand only", demandFunc, false},
		{"type node", typeNode, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, n := tc.build()
			assert.Equal(t, tc.want, enrichCallableIsDispatchRelevant(g, n))
		})
	}
}

// callHierarchyCounters registers prepare / outgoing / incoming handlers that
// count invocations, so a test can assert exactly which call-hierarchy round
// trips the sweep made. prepareCallHierarchy returns a single item pointing at
// the queried position; outgoing / incoming return empty result sets.
func callHierarchyCounters(server *fakeLSPServer, repoRoot string) (prepare, outgoing, incoming *atomic.Int64) {
	prepare, outgoing, incoming = &atomic.Int64{}, &atomic.Int64{}, &atomic.Int64{}
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/prepareCallHierarchy", func(json.RawMessage) (any, *jsonRPCError) {
		prepare.Add(1)
		return []CallHierarchyItem{{
			Name:           "target",
			URI:            pathToURI(filepath.Join(repoRoot, "svc.go")),
			SelectionRange: Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 1}},
		}}, nil
	})
	server.handle("callHierarchy/outgoingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		outgoing.Add(1)
		return []CallHierarchyOutgoingCall{}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		incoming.Add(1)
		return []CallHierarchyIncomingCall{}, nil
	})
	return prepare, outgoing, incoming
}

// Under the demand default the sweep interrogates a plain static function for
// its outgoing calls but skips its incoming calls: every intra-repo static
// call to it is already recoverable from its caller's outgoing hop, so the
// incoming round trip buys no edge.
func TestLSP_Enrich_IncomingSkippedForPlainStaticFunction(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "svc.go"),
		[]byte("package p\n\ntype Marker struct{}\n\nfunc Plain() {}\n"), 0o644))

	server := newFakeLSPServer()
	prepare, outgoing, incoming := callHierarchyCounters(server, repoRoot)

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	// A hierarchy-involved type keeps the file in the demand-gated sweep
	// (its unresolvable base still counts — see typeIsDispatchRelevant),
	// isolating the incoming decision from the file-level sweep gate.
	g.AddNode(&graph.Node{ID: "svc.go::Marker", Kind: graph.KindType, Name: "Marker",
		FilePath: "svc.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddEdge(&graph.Edge{From: "svc.go::Marker", To: graph.UnresolvedMarker + "VendorBase",
		Kind: graph.EdgeExtends, FilePath: "svc.go", Line: 3})
	g.AddNode(&graph.Node{ID: "svc.go::Plain", Kind: graph.KindFunction, Name: "Plain",
		FilePath: "svc.go", StartLine: 5, EndLine: 5, Language: "go"})

	require.NoError(t, runEnrich(t, p, g, repoRoot, 3*time.Second))

	assert.Positive(t, prepare.Load(), "the swept function must still be prepared for call hierarchy")
	assert.Positive(t, outgoing.Load(), "outgoing calls are always fetched")
	assert.Zero(t, incoming.Load(), "a plain static function's incoming calls must be skipped under the demand default")
	assert.Positive(t, p.reqStats.incomingSkipped.Load(), "the skipped incoming round trip must be counted")
}

// An interface-implementing method is a dynamic-dispatch target: a call to it
// may bind to the interface method, so only its incoming side names the
// concrete callers. Both outgoing and incoming are fetched even under the
// demand default.
func TestLSP_Enrich_IncomingFetchedForDispatchMethod(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "svc.go"),
		[]byte("package p\n\ntype Shape interface{ Area() float64 }\n\ntype Circle struct{}\n\nfunc (c Circle) Area() float64 { return 0 }\n"), 0o644))

	server := newFakeLSPServer()
	prepare, outgoing, incoming := callHierarchyCounters(server, repoRoot)

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

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
	assert.Positive(t, outgoing.Load(), "outgoing calls are always fetched")
	assert.Positive(t, incoming.Load(), "a dispatch-relevant method must have its incoming callers fetched")
	assert.Zero(t, p.reqStats.incomingSkipped.Load(), "no incoming round trip is skipped for a dispatch-relevant method")
}

// A full sweep fetches incoming calls unconditionally, even for a plain static
// function the demand default would skip.
func TestLSP_Enrich_IncomingFetchedInFullMode(t *testing.T) {
	t.Setenv(SweepEnv, "full")

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "svc.go"),
		[]byte("package p\n\nfunc Plain() {}\n"), 0o644))

	server := newFakeLSPServer()
	prepare, outgoing, incoming := callHierarchyCounters(server, repoRoot)

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{ID: "svc.go::Plain", Kind: graph.KindFunction, Name: "Plain",
		FilePath: "svc.go", StartLine: 3, EndLine: 3, Language: "go"})

	require.NoError(t, runEnrich(t, p, g, repoRoot, 3*time.Second))

	assert.Positive(t, prepare.Load())
	assert.Positive(t, outgoing.Load())
	assert.Positive(t, incoming.Load(), "full mode must fetch incoming calls even for a plain function")
	assert.Zero(t, p.reqStats.incomingSkipped.Load(), "full mode skips no incoming round trip")
}
