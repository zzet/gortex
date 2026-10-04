package mcp

import (
	"github.com/zzet/gortex/internal/graph"
	"iter"
)

// Keep the selected projection optional without allowing its edge stream to
// bypass ordinary-core adjacency filtering.
type contractCoreScopedProjection struct {
	core     *contractCoreEdges
	selected graph.ScopedProjectionSequencer
}

func (p *contractCoreScopedProjection) NodesInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	return p.selected.NodesInScopeSeq(repos, files, kinds...)
}
func (p *contractCoreScopedProjection) NodesLightInScopeSeq(repos, files []string) iter.Seq[*graph.Node] {
	return p.selected.NodesLightInScopeSeq(repos, files)
}
func (p *contractCoreScopedProjection) EdgesInScopeSeq(repos, files []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	return func(yield func(graph.ScopedEdgeRow) bool) {
		for row := range p.selected.EdgesInScopeSeq(repos, files, kinds...) {
			if len(p.core.filter([]*graph.Edge{row.Edge})) == 0 {
				continue
			}
			if !yield(row) {
				return
			}
		}
	}
}

type contractCoreScoped struct {
	*contractCoreEdges
	*contractCoreScopedProjection
}
type contractCoreScopedFiltered struct {
	*contractCoreFilteredNames
	*contractCoreScopedProjection
}
type contractCoreScopedBounded struct {
	*contractCoreBoundedFiles
	*contractCoreScopedProjection
}
type contractCoreScopedFilteredBounded struct {
	*contractCoreFilteredNamesBoundedFiles
	*contractCoreScopedProjection
}

func preserveContractCoreScopedProjection(wrapped graph.Reader, selected graph.Reader, core *contractCoreEdges) graph.Reader {
	projection, ok := selected.(graph.ScopedProjectionSequencer)
	if !ok {
		return wrapped
	}
	p := &contractCoreScopedProjection{core: core, selected: projection}
	switch r := wrapped.(type) {
	case *contractCoreFilteredNamesBoundedFiles:
		return &contractCoreScopedFilteredBounded{r, p}
	case *contractCoreBoundedFiles:
		return &contractCoreScopedBounded{r, p}
	case *contractCoreFilteredNames:
		return &contractCoreScopedFiltered{r, p}
	default:
		return &contractCoreScoped{core, p}
	}
}
