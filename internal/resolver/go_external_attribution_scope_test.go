package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestGoExternalAttributionIgnoresPythonCallers(t *testing.T) {
	const goCaller = "main.go::GoCaller"
	const pyCaller = "worker.py::python_caller"
	const goTarget = "stdlib::fmt::Println"
	const pyTarget = "dep::collections.Counter::Counter"
	for _, name := range []string{"whole graph", "single file"} {
		t.Run(name, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: goCaller, FilePath: "main.go", Kind: graph.KindFunction, Language: "go"})
			g.AddNode(&graph.Node{ID: pyCaller, FilePath: "worker.py", Kind: graph.KindFunction, Language: "python"})
			g.AddEdge(&graph.Edge{From: goCaller, To: goTarget, Kind: graph.EdgeCalls, FilePath: "main.go"})
			g.AddEdge(&graph.Edge{From: pyCaller, To: pyTarget, Kind: graph.EdgeCalls, FilePath: "worker.py"})
			r := New(g)
			if name == "whole graph" {
				r.attributeGoExternalCalls()
			} else {
				r.attributeGoExternalCallsForFile("main.go")
				r.attributeGoExternalCallsForFile("worker.py")
			}
			require.NotNil(t, g.GetNode("module::go:fmt"))
			assert.Nil(t, g.GetNode("module::go:collections.Counter"))
		})
	}
}
