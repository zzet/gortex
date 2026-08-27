package mcp

import (
	"fmt"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// isolatedNodesSubGraph builds the shape a min_tier / speculative
// filter produces in the wild: every edge dropped, the node list
// intact. The edge-prefix search has nothing to cut here — only a
// node-prefix re-render can fit the budget without byte-slicing the
// grammar.
func isolatedNodesSubGraph(n int) *query.SubGraph {
	sg := &query.SubGraph{}
	for i := 0; i < n; i++ {
		sg.Nodes = append(sg.Nodes, &graph.Node{
			ID:       fmt.Sprintf("pkg/iso%03d.go::Isolated%03d", i, i),
			Kind:     graph.KindFunction,
			Name:     fmt.Sprintf("Isolated%03d", i),
			FilePath: fmt.Sprintf("pkg/iso%03d.go", i),
		})
	}
	return sg
}

func budgetedRequest(maxBytes int) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"max_bytes": maxBytes}
	return req
}

// dotRender mirrors returnSubGraph's production dot callback,
// including the shared budget note, so the fitted size the test
// measures is the size production ships.
func dotRender(base *query.SubGraph) func(*query.SubGraph) string {
	return func(sg *query.SubGraph) string {
		text := sg.ToDot()
		if note := subGraphBudgetNote(sg, base); note != "" {
			text += "// " + note + "\n"
		}
		return text
	}
}

// TestNodeOnlySubGraphRendersValidlyWithinBudget pins the node-prefix
// re-render: a zero-edge, node-heavy subgraph must fit the byte budget
// by re-rendering over a node prefix — falling through to the
// plain-text trimmer corrupts the grammar (a dot document loses its
// closing brace; TOON contradicts its own row counts).
func TestNodeOnlySubGraphRendersValidlyWithinBudget(t *testing.T) {
	const cap = 300

	t.Run("dot", func(t *testing.T) {
		sg := isolatedNodesSubGraph(100)
		out := renderSubGraphWithinBudget(budgetedRequest(cap), sg, dotRender(sg))
		require.LessOrEqual(t, len(out), cap)
		require.Contains(t, out, "digraph")
		require.Contains(t, out, "\n}",
			"a budgeted dot document must still close its digraph block, got tail %q", out[max(0, len(out)-80):])
		require.NotContains(t, out, "trimmed to byte budget")
	})

	t.Run("mermaid", func(t *testing.T) {
		sg := isolatedNodesSubGraph(100)
		out := renderSubGraphWithinBudget(budgetedRequest(cap), sg, func(trial *query.SubGraph) string {
			text := trial.ToMermaid()
			if note := subGraphBudgetNote(trial, sg); note != "" {
				text += "%% " + note + "\n"
			}
			return text
		})
		require.LessOrEqual(t, len(out), cap)
		require.NotContains(t, out, "trimmed to byte budget")
	})

	t.Run("toon", func(t *testing.T) {
		sg := isolatedNodesSubGraph(100)
		out := renderSubGraphWithinBudget(budgetedRequest(cap), sg, func(trial *query.SubGraph) string {
			return subGraphToTOON(trial, nil)
		})
		require.LessOrEqual(t, len(out), cap)
		require.NotContains(t, out, "trimmed to byte budget")
	})

	t.Run("mixed isolated nodes and edges keep the subject", func(t *testing.T) {
		// Isolated nodes dominate while a few edges exist: the edge
		// search alone cannot fit — every page render keeps all 100
		// isolated nodes — so the node cut must engage, and the cut
		// must not drop the nodes the result's edges touch (the
		// queried subject among them).
		// Dot spends ~120 bytes per node row, so this cap fits about
		// two nodes — enough for both edge-touched ones, none of the
		// isolated tail.
		const mixedCap = 500
		sg := isolatedNodesSubGraph(100)
		linked := &graph.Node{ID: "pkg/linked.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "pkg/linked.go"}
		sg.Nodes = append(sg.Nodes, linked)
		sg.Edges = append(sg.Edges, &graph.Edge{From: linked.ID, To: sg.Nodes[0].ID, Kind: graph.EdgeCalls})
		out := renderSubGraphWithinBudget(budgetedRequest(mixedCap), sg, dotRender(sg))
		require.LessOrEqual(t, len(out), mixedCap)
		require.Contains(t, out, "digraph")
		require.Contains(t, out, "\n}")
		require.Contains(t, out, "linked",
			"the node cut must keep the nodes the result's edges touch — the queried subject is one of them")
		require.NotContains(t, out, "trimmed to byte budget")
	})
}

// TestBudgetNoteFiresOnlyOnBudgetCuts pins the note's attribution: a
// handler-side limit cap stamps Truncated and the pre-cut totals on
// the subgraph, and a rendering of that shape that FITS the budget
// must carry no "(byte budget)" note — "pass max_bytes:0" cannot
// restore rows a `limit` removed.
func TestBudgetNoteFiresOnlyOnBudgetCuts(t *testing.T) {
	sg := isolatedNodesSubGraph(5)
	sg.Truncated = true
	sg.TotalNodes = 200
	sg.TotalEdges = 400

	out := renderSubGraphWithinBudget(budgetedRequest(100_000), sg, dotRender(sg))
	require.NotContains(t, out, "byte budget",
		"a limit-capped result that fits the byte budget must not blame the byte budget")

	// The same shape over a tight budget IS byte-cut, and the note
	// names the cut relative to the rows the handler returned.
	cut := renderSubGraphWithinBudget(budgetedRequest(300), sg, dotRender(sg))
	require.LessOrEqual(t, len(cut), 300)
	require.Contains(t, cut, "byte budget")
	require.Contains(t, cut, "of 5 nodes",
		"the note's denominator is what max_bytes:0 would return — the handler's rows, not the pre-limit total")
}
