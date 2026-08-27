package analysis

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// connNode is a single node in a connectivity test fixture.
type connNode struct {
	id   string
	kind graph.NodeKind
	file string
}

// connEdge is a single directed edge in a connectivity test fixture.
type connEdge struct {
	from string
	to   string
}

// buildConnGraph assembles a graph from a node/edge fixture so each
// test pins exactly which nodes are isolated / leaf / connected.
func buildConnGraph(nodes []connNode, edges []connEdge) (*graph.Graph, []*graph.Node) {
	g := graph.New()
	for _, n := range nodes {
		g.AddNode(&graph.Node{ID: n.id, Kind: n.kind, Name: n.id, FilePath: n.file, Language: "go"})
	}
	for _, e := range edges {
		g.AddEdge(&graph.Edge{From: e.from, To: e.to, Kind: graph.EdgeDefines, Confidence: 1})
	}
	return g, g.AllNodes()
}

func TestGraphConnectivity(t *testing.T) {
	tests := []struct {
		name string
		// fixture
		nodes []connNode
		edges []connEdge
		// expectations
		wantNominal    int
		wantEffective  int
		wantRatio      float64
		wantIsolated   int
		wantLeaf       int
		wantSourceOnly int
		wantSinkOnly   int
	}{
		{
			name:          "empty graph",
			nodes:         nil,
			edges:         nil,
			wantNominal:   0,
			wantEffective: 0,
			wantRatio:     0,
		},
		{
			// file.go defines fn — fn is a leaf (one edge) and
			// sink-only (only incoming); file.go is source-only.
			// No node is isolated.
			name: "fully connected pair",
			nodes: []connNode{
				{"a.go", graph.KindFile, "a.go"},
				{"a.go::Fn", graph.KindFunction, "a.go"},
			},
			edges:          []connEdge{{"a.go", "a.go::Fn"}},
			wantNominal:    2,
			wantEffective:  2,
			wantRatio:      1.0,
			wantIsolated:   0,
			wantLeaf:       2, // both ends of the single edge have degree 1
			wantSourceOnly: 1, // a.go
			wantSinkOnly:   1, // a.go::Fn
		},
		{
			// One isolated function: zero edges of any kind. This is
			// the headline extraction-gap signal — NOT dead code.
			name: "one isolated node",
			nodes: []connNode{
				{"a.go", graph.KindFile, "a.go"},
				{"a.go::Fn", graph.KindFunction, "a.go"},
				{"orphan.go::Lost", graph.KindFunction, "orphan.go"},
			},
			edges:          []connEdge{{"a.go", "a.go::Fn"}},
			wantNominal:    3,
			wantEffective:  2,
			wantRatio:      2.0 / 3.0,
			wantIsolated:   1, // orphan.go::Lost
			wantLeaf:       2, // a.go and a.go::Fn
			wantSourceOnly: 1,
			wantSinkOnly:   1,
		},
		{
			// A chain a -> b -> c. b has in+out (degree 2, neither
			// leaf nor source/sink-only). a is source-only, c is
			// sink-only; both are leaves.
			name: "three-node chain",
			nodes: []connNode{
				{"a.go::A", graph.KindFunction, "a.go"},
				{"a.go::B", graph.KindFunction, "a.go"},
				{"a.go::C", graph.KindFunction, "a.go"},
			},
			edges: []connEdge{
				{"a.go::A", "a.go::B"},
				{"a.go::B", "a.go::C"},
			},
			wantNominal:    3,
			wantEffective:  3,
			wantRatio:      1.0,
			wantIsolated:   0,
			wantLeaf:       2, // A and C
			wantSourceOnly: 1, // A
			wantSinkOnly:   1, // C
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, nodes := buildConnGraph(tt.nodes, tt.edges)
			report := GraphConnectivity(g, nodes, 0)

			assert.Equal(t, tt.wantNominal, report.NominalNodes, "nominal_nodes")
			assert.Equal(t, tt.wantEffective, report.EffectiveNodes, "effective_nodes")
			assert.InDelta(t, tt.wantRatio, report.EffectiveRatio, 1e-9, "effective_ratio")
			assert.Equal(t, tt.wantIsolated, report.Isolated, "isolated")
			assert.Equal(t, tt.wantLeaf, report.Leaf, "leaf")
			assert.Equal(t, tt.wantSourceOnly, report.SourceOnly, "source_only")
			assert.Equal(t, tt.wantSinkOnly, report.SinkOnly, "sink_only")
			assert.NotEmpty(t, report.Note, "report must carry the extraction-vs-dead-code note")
		})
	}
}

// TestGraphConnectivity_DeadWeightByFile asserts the per-file
// dead-weight attribution ranks files by isolated+leaf contribution
// so an extraction gap can be localised.
func TestGraphConnectivity_DeadWeightByFile(t *testing.T) {
	// gappy.go contributes 3 isolated nodes; ok.go contributes a
	// single connected pair (two leaves, zero isolated).
	nodes := []connNode{
		{"ok.go", graph.KindFile, "ok.go"},
		{"ok.go::Fn", graph.KindFunction, "ok.go"},
		{"gappy.go::L1", graph.KindFunction, "gappy.go"},
		{"gappy.go::L2", graph.KindFunction, "gappy.go"},
		{"gappy.go::L3", graph.KindFunction, "gappy.go"},
	}
	edges := []connEdge{{"ok.go", "ok.go::Fn"}}

	g, allNodes := buildConnGraph(nodes, edges)
	report := GraphConnectivity(g, allNodes, 0)

	require.Len(t, report.DeadWeightByFile, 2, "both files contribute dead-weight nodes")

	// gappy.go ranks first: 3 isolated nodes > ok.go's 2 leaf nodes.
	top := report.DeadWeightByFile[0]
	assert.Equal(t, "gappy.go", top.FilePath)
	assert.Equal(t, 3, top.Isolated)
	assert.Equal(t, 0, top.Leaf)
	assert.Equal(t, 3, top.DeadWeight)

	second := report.DeadWeightByFile[1]
	assert.Equal(t, "ok.go", second.FilePath)
	assert.Equal(t, 0, second.Isolated)
	assert.Equal(t, 2, second.Leaf)
	assert.Equal(t, 2, second.DeadWeight)
}

// TestGraphConnectivity_FileLimit asserts the fileLimit argument
// truncates the dead-weight ranking to the top-N files.
func TestGraphConnectivity_FileLimit(t *testing.T) {
	nodes := []connNode{
		{"a.go::A", graph.KindFunction, "a.go"},
		{"b.go::B", graph.KindFunction, "b.go"},
		{"c.go::C", graph.KindFunction, "c.go"},
	}
	g, allNodes := buildConnGraph(nodes, nil) // all three isolated

	report := GraphConnectivity(g, allNodes, 2)
	assert.Len(t, report.DeadWeightByFile, 2, "fileLimit=2 must cap the ranking at 2 files")
	assert.Equal(t, 3, report.Isolated, "the isolated count is not affected by fileLimit")
}

// TestGraphConnectivity_ByKind asserts the per-node-kind breakdown
// tallies isolated / leaf counts separately per kind.
func TestGraphConnectivity_ByKind(t *testing.T) {
	// One connected file->function pair, plus an isolated type.
	nodes := []connNode{
		{"a.go", graph.KindFile, "a.go"},
		{"a.go::Fn", graph.KindFunction, "a.go"},
		{"a.go::Orphan", graph.KindType, "a.go"},
	}
	edges := []connEdge{{"a.go", "a.go::Fn"}}

	g, allNodes := buildConnGraph(nodes, edges)
	report := GraphConnectivity(g, allNodes, 0)

	byKind := map[string]ConnectivityKindEntry{}
	for _, k := range report.ByKind {
		byKind[k.Kind] = k
	}

	require.Contains(t, byKind, "type")
	assert.Equal(t, 1, byKind["type"].Total)
	assert.Equal(t, 1, byKind["type"].Isolated, "the orphan type is isolated")
	assert.Equal(t, 0, byKind["type"].Leaf)

	require.Contains(t, byKind, "function")
	assert.Equal(t, 1, byKind["function"].Total)
	assert.Equal(t, 0, byKind["function"].Isolated)
	assert.Equal(t, 1, byKind["function"].Leaf, "the defined function has degree 1")
}

// TestGraphConnectivity_IsolatedIsNotDeadCode pins the load-bearing
// distinction: an isolated node (zero edges of ANY kind) is an
// extraction-gap signal, whereas a dead-code node still carries a
// structural edge. A node that is `defines`-linked from its file but
// has no incoming usage edge is dead code, NOT isolated — this
// analyzer must not count it as isolated.
func TestGraphConnectivity_IsolatedIsNotDeadCode(t *testing.T) {
	nodes := []connNode{
		{"a.go", graph.KindFile, "a.go"},
		// Defined by its file but never used — classic dead code.
		{"a.go::Unused", graph.KindFunction, "a.go"},
		// No edges at all — an extraction gap.
		{"b.go::Missing", graph.KindFunction, "b.go"},
	}
	edges := []connEdge{{"a.go", "a.go::Unused"}}

	g, allNodes := buildConnGraph(nodes, edges)
	report := GraphConnectivity(g, allNodes, 0)

	// Only the zero-edge node counts as isolated; the dead-code node
	// has a structural edge and does not.
	assert.Equal(t, 1, report.Isolated, "only the zero-edge node is isolated")

	// Cross-check against the shared classifier the analyzer reuses.
	assert.Equal(t, graph.ZeroEdgePossibleExtractionGap,
		graph.ClassifyZeroEdge(g, "b.go::Missing"),
		"the zero-edge node classifies as an extraction gap")
	assert.NotEqual(t, graph.ZeroEdgePossibleExtractionGap,
		graph.ClassifyZeroEdge(g, "a.go::Unused"),
		"the structurally-linked dead-code node is NOT an extraction gap")
}

// TestConnectivityNote_MatchesSharedZeroEdgeStance pins the standing note
// against the per-symbol caveat vocabulary this analyzer says it stays in
// lockstep with. graph.ClassifyZeroEdge — reused here for the
// isolated/leaf split — deliberately refuses to call a zero-incoming
// symbol proof of anything, because an unresolved or name-only call site
// leaves the same shape. The note describing kind=dead_code must not
// contradict that by promising the row is safe to delete.
func TestConnectivityNote_MatchesSharedZeroEdgeStance(t *testing.T) {
	nodes := []connNode{
		{"a.go", graph.KindFile, "a.go"},
		// Zero incoming *usage* edges, one structural edge — exactly the
		// shape kind=dead_code reports on.
		{"a.go::Unused", graph.KindFunction, "a.go"},
	}
	g, allNodes := buildConnGraph(nodes, []connEdge{{"a.go", "a.go::Unused"}})

	// Control: the shared classifier sees this very shape and still hedges.
	caveat := graph.CaveatForZeroEdge(g, "a.go::Unused")
	require.NotNil(t, caveat, "the dead-code shape carries a zero-edge caveat")
	require.Equal(t, graph.ZeroEdgeLikelyUnused, caveat.Class)
	require.Contains(t, caveat.Message, "not proof",
		"the shared caveat refuses to call zero incoming usage edges proof")

	report := GraphConnectivity(g, allNodes, 0)
	require.Equal(t, connectivityNote, report.Note, "the report carries the standing note")

	// The distinction the note exists to draw must survive any rewording.
	assert.Contains(t, report.Note, "dead_code", "the note still names the analyzer it contrasts with")
	assert.Contains(t, report.Note, "INCOMING", "the note still states the dead_code signal")

	// What must never come back: a removal verdict the classifier withholds.
	lowered := strings.ToLower(report.Note)
	for _, verdict := range []string{
		"safe to remove", "safe to delete",
		"can be removed", "can be deleted",
	} {
		assert.NotContains(t, lowered, verdict,
			"the note must not promise removal safety the shared classifier withholds")
	}
}

// TestGraphConnectivity_NilGraph asserts a nil graph yields a zero
// report rather than panicking.
func TestGraphConnectivity_NilGraph(t *testing.T) {
	report := GraphConnectivity(nil, nil, 0)
	assert.Equal(t, 0, report.NominalNodes)
	assert.Equal(t, 0, report.EffectiveNodes)
	assert.True(t, math.IsNaN(report.EffectiveRatio) == false, "ratio stays a real number")
}
