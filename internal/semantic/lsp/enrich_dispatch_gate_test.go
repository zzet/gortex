package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// #605: the per-file sweep gate. Under the demand default a file used to earn
// its sweep slot by declaring ANY type or interface — true for essentially
// every C# file, so the gate admitted the whole repo. The dispatch half must
// be as discriminating as the per-callable incoming-calls gate already is:
// a file sweeps when it carries unresolved demand, a dispatch-relevant
// callable, or a type actually involved in a super/subtype hierarchy. A
// plain data type with no bases, no subtypes, and no dispatch-relevant
// members buys nothing from hover or hierarchy interrogation.

// The type half of the file gate. Interfaces stay in unconditionally: an
// interface is the dispatch surface by definition, and the AST edges of its
// implementers may be exactly what failed to resolve — the case where it
// looks adjacency-less is the case where the sweep is most needed. A class
// earns its slot only through hierarchy involvement, and edge KINDS survive
// even when the AST could not resolve the target, so a class with an
// unresolvable base list still qualifies.
func TestEnrichTypeIsDispatchRelevantFromView(t *testing.T) {
	iface := &graph.Node{ID: "s.cs::IShape", Kind: graph.KindInterface, Name: "IShape"}
	impl := &graph.Node{ID: "s.cs::Circle", Kind: graph.KindType, Name: "Circle"}
	unresolvedBase := &graph.Node{ID: "s.cs::Widget", Kind: graph.KindType, Name: "Widget"}
	superType := &graph.Node{ID: "s.cs::Animal", Kind: graph.KindType, Name: "Animal"}
	sub := &graph.Node{ID: "s.cs::Dog", Kind: graph.KindType, Name: "Dog"}
	poco := &graph.Node{ID: "s.cs::Box", Kind: graph.KindType, Name: "Box"}
	method := &graph.Node{ID: "s.cs::Box.Size", Kind: graph.KindMethod, Name: "Size"}

	view := newLSPGraphView(
		[]*graph.Node{iface, impl, unresolvedBase, superType, sub, poco, method},
		[]*graph.Edge{
			{From: impl.ID, To: iface.ID, Kind: graph.EdgeImplements},
			{From: unresolvedBase.ID, To: graph.UnresolvedMarker + "VendorBase", Kind: graph.EdgeExtends},
			{From: sub.ID, To: superType.ID, Kind: graph.EdgeExtends},
			{From: method.ID, To: poco.ID, Kind: graph.EdgeMemberOf},
		},
	)

	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, iface, true), "an interface is always dispatch surface")
	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, impl, true), "a type implementing an interface")
	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, unresolvedBase, true), "an unresolvable base list still counts — the sweep is the only path that recovers it")
	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, superType, true), "a type something else extends")
	assert.False(t, enrichTypeIsDispatchRelevantFromView(view, poco, true), "a bare data type is not")
	assert.False(t, enrichTypeIsDispatchRelevantFromView(view, method, true), "callables have their own predicate")
	assert.False(t, enrichTypeIsDispatchRelevantFromView(view, nil, true))

	// Without hierarchy evidence the strict check is circular (the sweep is
	// the only producer of the qualifying edge) — every type falls back to
	// permissive admission while non-types stay out.
	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, poco, false), "no evidence → a bare data type is admitted permissively")
	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, iface, false))
	assert.False(t, enrichTypeIsDispatchRelevantFromView(view, method, false), "the fallback widens types only, never callables")
	assert.False(t, enrichTypeIsDispatchRelevantFromView(view, nil, false))
}

// The self-tuning switch for the type gate: evidence is an extends /
// implements edge some non-LSP lane minted for THIS language. The sweep's own
// recoveries (lsp_resolved / lsp_dispatch) and other languages' edges do not
// count — either would flip a hierarchy-blind language onto the circular
// strict gate.
func TestEnrichLanguageHasHierarchyEvidence(t *testing.T) {
	goImpl := &graph.Node{ID: "a.go::Circle", Kind: graph.KindType, Name: "Circle", Language: "go"}
	goIface := &graph.Node{ID: "a.go::Shape", Kind: graph.KindInterface, Name: "Shape", Language: "go"}
	csImpl := &graph.Node{ID: "b.cs::Disc", Kind: graph.KindType, Name: "Disc", Language: "csharp"}
	csIface := &graph.Node{ID: "b.cs::IShape", Kind: graph.KindInterface, Name: "IShape", Language: "csharp"}
	nodes := []*graph.Node{goImpl, goIface, csImpl, csIface}
	matchGo := func(lang string) bool { return lang == "go" }

	astEdge := &graph.Edge{From: goImpl.ID, To: goIface.ID, Kind: graph.EdgeImplements, Origin: graph.OriginASTResolved}
	bareEdge := &graph.Edge{From: goImpl.ID, To: graph.UnresolvedMarker + "Base", Kind: graph.EdgeExtends}
	lspEdge := &graph.Edge{From: goImpl.ID, To: goIface.ID, Kind: graph.EdgeImplements, Origin: graph.OriginLSPResolved}
	dispatchEdge := &graph.Edge{From: goImpl.ID, To: goIface.ID, Kind: graph.EdgeImplements, Origin: graph.OriginLSPDispatch}
	csEdge := &graph.Edge{From: csImpl.ID, To: csIface.ID, Kind: graph.EdgeImplements, Origin: graph.OriginASTResolved}
	callEdge := &graph.Edge{From: goImpl.ID, To: goIface.ID, Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved}

	probe := func(edges ...*graph.Edge) bool {
		return enrichLanguageHasHierarchyEvidence(newLSPGraphView(nodes, edges), edges, matchGo)
	}

	assert.True(t, probe(astEdge), "an extractor-produced implements edge is evidence")
	assert.True(t, probe(bareEdge), "an unresolved-target base-list edge (empty origin) is evidence — the extractor minted it")
	assert.False(t, probe(), "no edges, no evidence")
	assert.False(t, probe(lspEdge), "the sweep's own lsp_resolved recovery is not evidence")
	assert.False(t, probe(dispatchEdge), "lsp_dispatch is not evidence either")
	assert.False(t, probe(csEdge), "another language's edge is not evidence for this one")
	assert.False(t, probe(callEdge), "non-hierarchy kinds never count")
	assert.True(t, probe(lspEdge, csEdge, astEdge), "one qualifying edge among noise is enough")
	assert.False(t, probe(nil), "nil edges are skipped")

	// A confirmed extractor edge keeps its provenance: ConfirmEdge flips the
	// origin to LSP grade but records the prior origin, so confirmation does
	// not decay the evidence the gate depends on.
	confirmedEdge := &graph.Edge{From: goImpl.ID, To: goIface.ID, Kind: graph.EdgeImplements,
		Origin: graph.OriginLSPResolved,
		Meta:   map[string]any{"confirmed_from_origin": string(graph.OriginASTResolved)}}
	assert.True(t, probe(confirmedEdge), "a confirmed extractor edge remains evidence via its preserved provenance")
	confirmedStub := &graph.Edge{From: goImpl.ID, To: goIface.ID, Kind: graph.EdgeExtends,
		Origin: graph.OriginLSPResolved,
		Meta:   map[string]any{"confirmed_from_origin": ""}}
	assert.True(t, probe(confirmedStub), "marker presence is the signal — an empty prior origin still counts")
}

// TestLSP_Enrich_SweepGate drives one pass over three files under the demand
// default and asserts per-file sweep decisions by the hover requests the
// server saw:
//   - poco.go: a bare struct + plain method, no demand — must be SKIPPED.
//   - hier.go: a type implementing an interface — must be swept.
//   - want.go: no types, but a declaration with an unresolved same-name
//     candidate — the demand half must still admit it.
func TestLSP_Enrich_SweepGate(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "poco.go"),
		[]byte("package p\n\ntype Box struct{}\n\nfunc (b Box) Size() int { return 0 }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "hier.go"),
		[]byte("package p\n\ntype Shape interface{ Area() float64 }\n\ntype Circle struct{}\n\nfunc (c Circle) Area() float64 { return 0 }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "want.go"),
		[]byte("package p\n\nfunc Free() {}\n"), 0o644))

	server := newFakeLSPServer()
	var mu sync.Mutex
	hoveredURIs := map[string]bool{}
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		mu.Lock()
		hoveredURIs[req.TextDocument.URI] = true
		mu.Unlock()
		return nil, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	// poco.go — a type and its method, hierarchy-uninvolved, no demand.
	g.AddNode(&graph.Node{ID: "poco.go::Box", Kind: graph.KindType, Name: "Box",
		FilePath: "poco.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "poco.go::Box.Size", Kind: graph.KindMethod, Name: "Size",
		FilePath: "poco.go", StartLine: 5, EndLine: 5, Language: "go"})
	g.AddEdge(&graph.Edge{From: "poco.go::Box.Size", To: "poco.go::Box", Kind: graph.EdgeMemberOf})
	// hier.go — a type that implements an interface declared beside it.
	g.AddNode(&graph.Node{ID: "hier.go::Shape", Kind: graph.KindInterface, Name: "Shape",
		FilePath: "hier.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "hier.go::Circle", Kind: graph.KindType, Name: "Circle",
		FilePath: "hier.go", StartLine: 5, EndLine: 5, Language: "go"})
	g.AddNode(&graph.Node{ID: "hier.go::Circle.Area", Kind: graph.KindMethod, Name: "Area",
		FilePath: "hier.go", StartLine: 7, EndLine: 7, Language: "go"})
	g.AddEdge(&graph.Edge{From: "hier.go::Circle.Area", To: "hier.go::Circle", Kind: graph.EdgeMemberOf})
	g.AddEdge(&graph.Edge{From: "hier.go::Circle", To: "hier.go::Shape", Kind: graph.EdgeImplements})
	// want.go — no types at all; Free still has an unresolved same-name
	// candidate, so the demand half of the gate must admit the file.
	g.AddNode(&graph.Node{ID: "want.go::Free", Kind: graph.KindFunction, Name: "Free",
		FilePath: "want.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddEdge(&graph.Edge{From: "hier.go::Circle.Area", To: graph.UnresolvedMarker + "*.Free",
		Kind: graph.EdgeCalls, FilePath: "hier.go", Line: 7})

	require.NoError(t, runEnrich(t, p, g, repoRoot, 3*time.Second))

	mu.Lock()
	defer mu.Unlock()
	assert.False(t, hoveredURIs[pathToURI(filepath.Join(repoRoot, "poco.go"))],
		"a hierarchy-uninvolved data type must not keep its file in the sweep")
	assert.True(t, hoveredURIs[pathToURI(filepath.Join(repoRoot, "hier.go"))],
		"a type involved in a hierarchy keeps its file in the sweep")
	assert.True(t, hoveredURIs[pathToURI(filepath.Join(repoRoot, "want.go"))],
		"unresolved demand keeps a type-less file in the sweep")
}

// The strict gate is circular in languages whose extractor emits no base-list
// edges and that have no supplemental type lane (c, cpp, objc, swift): there
// the sweep's typeHierarchy hop is the ONLY producer of extends / implements
// edges, so requiring such an edge for admission demands as input exactly
// what the sweep exists to produce, and the hierarchy is never recovered.
// When the language's edge set carries zero extractor-produced hierarchy
// edges the gate must fall back to the permissive kind check.
func TestLSP_Enrich_SweepGate_PermissiveFallbackWithoutHierarchyEvidence(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "poco.go"),
		[]byte("package p\n\ntype Box struct{}\n\nfunc (b Box) Size() int { return 0 }\n"), 0o644))

	server := newInstrumentedServer()
	mu, hovered := hoverURIRecorder(server)

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()

	// A bare class-only graph: no extends / implements edge exists anywhere in
	// the language — the extractor for this language cannot produce one.
	g := graph.New()
	g.AddNode(&graph.Node{ID: "poco.go::Box", Kind: graph.KindType, Name: "Box",
		FilePath: "poco.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "poco.go::Box.Size", Kind: graph.KindMethod, Name: "Size",
		FilePath: "poco.go", StartLine: 5, EndLine: 5, Language: "go"})
	g.AddEdge(&graph.Edge{From: "poco.go::Box.Size", To: "poco.go::Box", Kind: graph.EdgeMemberOf})

	require.NoError(t, runEnrich(t, p, g, repoRoot, 3*time.Second))

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, hovered["poco.go"],
		"with zero hierarchy edges in the language the type gate must fall back to permissive — the sweep is the only producer of those edges")
}

// The evidence probe is per-language: another language's hierarchy edges say
// nothing about what THIS language's extractor can produce. A repo mixing C#
// (extractor emits implements) with a hierarchy-blind language must not let
// the C# edges flip the blind language onto the strict gate.
func TestLSP_Enrich_SweepGate_OtherLanguageEvidenceDoesNotCount(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "poco.go"),
		[]byte("package p\n\ntype Box struct{}\n\nfunc (b Box) Size() int { return 0 }\n"), 0o644))

	server := newInstrumentedServer()
	mu, hovered := hoverURIRecorder(server)

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{ID: "poco.go::Box", Kind: graph.KindType, Name: "Box",
		FilePath: "poco.go", StartLine: 3, EndLine: 3, Language: "go"})
	// Hierarchy evidence exists in the repo — but in a DIFFERENT language.
	g.AddNode(&graph.Node{ID: "s.cs::IShape", Kind: graph.KindInterface, Name: "IShape",
		FilePath: "s.cs", StartLine: 1, EndLine: 1, Language: "csharp"})
	g.AddNode(&graph.Node{ID: "s.cs::Circle", Kind: graph.KindType, Name: "Circle",
		FilePath: "s.cs", StartLine: 3, EndLine: 3, Language: "csharp"})
	g.AddEdge(&graph.Edge{From: "s.cs::Circle", To: "s.cs::IShape", Kind: graph.EdgeImplements,
		FilePath: "s.cs", Line: 3})

	require.NoError(t, runEnrich(t, p, g, repoRoot, 3*time.Second))

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, hovered["poco.go"],
		"hierarchy evidence from another language must not put this language on the strict gate")
}

// Edges the sweep itself recovered (lsp_resolved / lsp_dispatch) are not
// evidence the extractor can produce them: counting the sweep's own output
// would flip a hierarchy-blind language onto the strict gate on the second
// run, and every class added after that would carry no edge, be dropped, and
// never have its hierarchy recovered.
func TestLSP_Enrich_SweepGate_LSPRecoveredEdgesAreNotEvidence(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "hier.go"),
		[]byte("package p\n\ntype Base struct{}\n\ntype Derived struct{ Base }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "fresh.go"),
		[]byte("package p\n\ntype Fresh struct{}\n"), 0o644))

	server := newInstrumentedServer()
	mu, hovered := hoverURIRecorder(server)

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()

	// The only hierarchy edge in the language is one a PRIOR sweep recovered.
	g := graph.New()
	g.AddNode(&graph.Node{ID: "hier.go::Base", Kind: graph.KindType, Name: "Base",
		FilePath: "hier.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "hier.go::Derived", Kind: graph.KindType, Name: "Derived",
		FilePath: "hier.go", StartLine: 5, EndLine: 5, Language: "go"})
	g.AddEdge(&graph.Edge{From: "hier.go::Derived", To: "hier.go::Base", Kind: graph.EdgeExtends,
		FilePath: "hier.go", Line: 5,
		Confidence: 1.0, ConfidenceLabel: "CONFIRMED", Origin: graph.OriginLSPResolved})
	g.AddNode(&graph.Node{ID: "fresh.go::Fresh", Kind: graph.KindType, Name: "Fresh",
		FilePath: "fresh.go", StartLine: 3, EndLine: 3, Language: "go"})

	require.NoError(t, runEnrich(t, p, g, repoRoot, 3*time.Second))

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, hovered["fresh.go"],
		"sweep-recovered edges are not extractor evidence — a new bare class must still be admitted")
}

// The decay scenario: an extractor-minted extends edge is confirmed by the
// sweep's typeHierarchy hop, which flips its origin to lsp_resolved — exactly
// what the evidence probe excludes. Confirmation must preserve the prior
// provenance so the NEXT pass still sees extractor evidence; without that the
// gate is self-defeating (admit hierarchy types → sweep confirms their edges →
// evidence gone → permissive fallback), oscillating on every enrich/reindex
// cycle.
func TestLSP_Enrich_SweepGate_ConfirmationDoesNotDecayEvidence(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "hier.go"),
		[]byte("package p\n\ntype Base struct{}\n\ntype Derived struct{ Base }\n"), 0o644))

	server := newInstrumentedServer()
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	derivedItem := TypeHierarchyItem{
		Name: "Derived", URI: pathToURI(filepath.Join(repoRoot, "hier.go")),
		SelectionRange: Range{Start: Position{Line: 4, Character: 5}, End: Position{Line: 4, Character: 12}},
	}
	baseItem := TypeHierarchyItem{
		Name: "Base", URI: pathToURI(filepath.Join(repoRoot, "hier.go")),
		SelectionRange: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 9}},
	}
	server.handle("textDocument/prepareTypeHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			Position Position `json:"position"`
		}
		_ = json.Unmarshal(params, &req)
		if req.Position.Line == 4 {
			return []TypeHierarchyItem{derivedItem}, nil
		}
		return []TypeHierarchyItem{baseItem}, nil
	})
	server.handle("typeHierarchy/supertypes", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			Item TypeHierarchyItem `json:"item"`
		}
		_ = json.Unmarshal(params, &req)
		if req.Item.Name == "Derived" {
			// The compiler agrees with the extractor: Derived extends Base.
			return []TypeHierarchyItem{baseItem}, nil
		}
		return []TypeHierarchyItem{}, nil
	})
	server.handle("typeHierarchy/subtypes", func(params json.RawMessage) (any, *jsonRPCError) {
		return []TypeHierarchyItem{}, nil
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()
	p.caps = ServerCapabilities{TypeHierarchyProvider: true}

	derived := &graph.Node{ID: "hier.go::Derived", Kind: graph.KindType, Name: "Derived",
		FilePath: "hier.go", StartLine: 5, EndLine: 5, Language: "go"}
	base := &graph.Node{ID: "hier.go::Base", Kind: graph.KindType, Name: "Base",
		FilePath: "hier.go", StartLine: 3, EndLine: 3, Language: "go"}
	edge := &graph.Edge{From: derived.ID, To: base.ID, Kind: graph.EdgeExtends,
		FilePath: "hier.go", Line: 5,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginASTResolved}
	g := graph.New()
	g.AddNode(derived)
	g.AddNode(base)
	g.AddEdge(edge)

	require.NoError(t, runEnrich(t, p, g, repoRoot, 3*time.Second))

	// The confirm happened — origin flipped to the excluded grade…
	require.Equal(t, graph.OriginLSPResolved, edge.Origin,
		"fixture sanity: the sweep must confirm the extractor edge in place")
	require.Equal(t, 1.0, edge.Confidence)

	// …and the probe, over the post-pass edge set, still sees evidence.
	edges := []*graph.Edge{edge}
	view := newLSPGraphView([]*graph.Node{derived, base}, edges)
	assert.True(t,
		enrichLanguageHasHierarchyEvidence(view, edges, func(lang string) bool { return lang == "go" }),
		"confirmation must not decay the evidence the gate depends on")
}
