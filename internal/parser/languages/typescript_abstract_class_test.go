package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestTypeScriptExtractor_AbstractClass(t *testing.T) {
	const src = `abstract class Shape {
 abstract area(): number
 m() { return this.n() }
 protected n() { return 1 }
}
interface Drawable {}
export abstract class Polygon extends Shape implements Drawable {}
class Circle extends Shape {}
`
	for _, file := range []string{"shape.ts", "shape.tsx"} {
		t.Run(file, func(t *testing.T) {
			result, err := NewTypeScriptExtractor().Extract(file, []byte(src))
			require.NoError(t, err)
			types := nodesOfKind(result.Nodes, graph.KindType)
			require.Len(t, types, 3)
			ids := map[string]bool{}
			for _, n := range result.Nodes {
				require.False(t, ids[n.ID], "duplicate node %s", n.ID)
				ids[n.ID] = true
			}
			for _, name := range []string{"Shape", "Polygon", "Circle", "Drawable", "Shape.m", "Shape.n"} {
				require.True(t, ids[file+"::"+name], "missing %s", name)
			}
			// Bodyless abstract signatures stay unextracted, like other signatures.
			require.False(t, ids[file+"::Shape.area"])
			require.Len(t, nodesOfKind(result.Nodes, graph.KindMethod), 2)
			for _, want := range []struct {
				from, to string
				kind     graph.EdgeKind
			}{
				{file + "::Shape.m", file + "::Shape", graph.EdgeMemberOf},
				{file + "::Shape.n", file + "::Shape", graph.EdgeMemberOf},
				{file + "::Shape.m", "unresolved::*.n", graph.EdgeCalls},
				{file + "::Polygon", "unresolved::Shape", graph.EdgeExtends},
				{file + "::Polygon", "unresolved::Drawable", graph.EdgeImplements},
				{file + "::Circle", "unresolved::Shape", graph.EdgeExtends},
			} {
				count := 0
				for _, edge := range result.Edges {
					if edge.From == want.from && edge.To == want.to && edge.Kind == want.kind {
						count++
					}
				}
				require.Equal(t, 1, count, "%s %s -> %s", want.kind, want.from, want.to)
			}
		})
	}
}

func TestTypeScriptExtractor_AbstractClassParity(t *testing.T) {
	const src = `export class Shape {
 m() { return this.n() }
 protected n() { return 1 }
}
class Circle extends Shape {}
`
	ext := NewTypeScriptExtractor()
	ordinary, err := ext.Extract("shape.ts", []byte(src))
	require.NoError(t, err)
	abstract, err := ext.Extract("shape.ts", []byte(strings.Replace(src, "export class", "export abstract class", 1)))
	require.NoError(t, err)
	require.Len(t, abstract.Nodes, len(ordinary.Nodes))
	require.Len(t, abstract.Edges, len(ordinary.Edges))
	nodeKeys := func(nodes []*graph.Node) map[string]graph.NodeKind {
		keys := map[string]graph.NodeKind{}
		for _, n := range nodes {
			keys[n.ID] = n.Kind
		}
		return keys
	}
	edgeKeys := func(edges []*graph.Edge) map[[3]string]int {
		keys := map[[3]string]int{}
		for _, e := range edges {
			keys[[3]string{e.From, e.To, string(e.Kind)}]++
		}
		return keys
	}
	require.Equal(t, nodeKeys(ordinary.Nodes), nodeKeys(abstract.Nodes))
	require.Equal(t, edgeKeys(ordinary.Edges), edgeKeys(abstract.Edges))
}
