package mcp

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// Preserve selected node projection without advertising edge traversal.
type contractCoreScopedProjection struct {
	selected graph.ScopedNodeProjectionSequencer
}

func (p *contractCoreScopedProjection) NodesInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	return p.selected.NodesInScopeSeq(repos, files, kinds...)
}
func (p *contractCoreScopedProjection) NodesLightInScopeSeq(repos, files []string) iter.Seq[*graph.Node] {
	return p.selected.NodesLightInScopeSeq(repos, files)
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
	projection, ok := selected.(graph.ScopedNodeProjectionSequencer)
	if !ok {
		return wrapped
	}
	p := &contractCoreScopedProjection{selected: projection}
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
