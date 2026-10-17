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

type contractCoreKindSummaryProjection struct {
	selected graph.ScopedKindSummarySequencer
}

func (p *contractCoreKindSummaryProjection) NodesLightByKindsInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	return p.selected.NodesLightByKindsInScopeSeq(repos, files, kinds...)
}

type contractCoreScopedSummary struct {
	*contractCoreScoped
	*contractCoreKindSummaryProjection
}
type contractCoreScopedFilteredSummary struct {
	*contractCoreScopedFiltered
	*contractCoreKindSummaryProjection
}
type contractCoreScopedBoundedSummary struct {
	*contractCoreScopedBounded
	*contractCoreKindSummaryProjection
}
type contractCoreScopedFilteredBoundedSummary struct {
	*contractCoreScopedFilteredBounded
	*contractCoreKindSummaryProjection
}

func preserveContractCoreScopedProjection(wrapped graph.Reader, selected graph.Reader, core *contractCoreEdges) graph.Reader {
	projection, ok := selected.(graph.ScopedNodeProjectionSequencer)
	if !ok {
		return wrapped
	}
	p := &contractCoreScopedProjection{selected: projection}
	var scoped graph.Reader
	switch r := wrapped.(type) {
	case *contractCoreFilteredNamesBoundedFiles:
		scoped = &contractCoreScopedFilteredBounded{r, p}
	case *contractCoreBoundedFiles:
		scoped = &contractCoreScopedBounded{r, p}
	case *contractCoreFilteredNames:
		scoped = &contractCoreScopedFiltered{r, p}
	default:
		scoped = &contractCoreScoped{core, p}
	}
	// Keep this capability conditional on the exact selected reader. In
	// particular, an overlay without summary composition uses its full rows.
	summary, ok := selected.(graph.ScopedKindSummarySequencer)
	if !ok {
		return scoped
	}
	light := &contractCoreKindSummaryProjection{selected: summary}
	switch r := scoped.(type) {
	case *contractCoreScopedFilteredBounded:
		return &contractCoreScopedFilteredBoundedSummary{r, light}
	case *contractCoreScopedBounded:
		return &contractCoreScopedBoundedSummary{r, light}
	case *contractCoreScopedFiltered:
		return &contractCoreScopedFilteredSummary{r, light}
	case *contractCoreScoped:
		return &contractCoreScopedSummary{r, light}
	default:
		return scoped
	}
}
