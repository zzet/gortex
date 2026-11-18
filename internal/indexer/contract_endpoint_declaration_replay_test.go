package indexer

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

type endpointDeclarationReader struct {
	*endpointReplayRowsReader
	declarationCalls int
	fullCalls        int
}

func (r *endpointDeclarationReader) VisitNodeDeclarationsByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	r.declarationCalls++
	return r.visit(ctx, names, yield)
}
func (r *endpointDeclarationReader) VisitNodesByNamesContext(context.Context, []string, func(*graph.Node) bool) error {
	r.fullCalls++
	return errors.New("endpoint replay requested unconsumed full metadata")
}
func TestContractEndpointReplayUsesCheckedDeclarationFacts(t *testing.T) {
	r := &endpointReplayRowsReader{rows: []*graph.Node{
		{ID: "repo/a.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/a.go::param:A", Name: "A", Kind: graph.KindParam, FilePath: "repo/a.go", RepoPrefix: "repo"},
	}}
	e := endpointReplayEvidence(t, r, "A", "Missing")
	facts := &endpointDeclarationReader{endpointReplayRowsReader: r}
	e.core = facts
	require.Equal(t, []contracts.EndpointConstantCandidate{{ID: "repo/a.go::A", FilePath: "repo/a.go"}}, e.FindEndpointConstantCandidates("A", "repo"))
	require.Empty(t, e.FindEndpointConstantCandidates("Missing", "repo"))
	require.NoError(t, e.err)
	require.Equal(t, 1, facts.declarationCalls)
	require.Zero(t, facts.fullCalls)
	require.Equal(t, 2, e.endpointReplay.rows["A"].count, "nonconstant rows must still count toward unchanged projection cap")
	require.Len(t, e.ledger.reads, 2)
	require.True(t, e.ledger.keys["name:Missing"])
}
func TestContractCapturedDeclarationFactsStayWithinAdmittedRepo(t *testing.T) {
	r := &endpointReplayRowsReader{rows: []*graph.Node{
		{ID: "repo/a.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "other/a.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "other/a.go", RepoPrefix: "other"},
	}, guard: &contractReadGuard{open: true}}
	facts := &endpointDeclarationReader{endpointReplayRowsReader: r}
	c := &contractCapturedCore{readers: map[string]graph.Reader{"repo": facts}}
	var ids []string
	closeRead := r.guard.enter()
	defer closeRead()
	require.NoError(t, graph.VisitNodeDeclarationsByNamesContext(t.Context(), c, []string{"A", "Missing"}, func(n *graph.Node) bool { ids = append(ids, n.ID); return true }))
	require.Equal(t, []string{"repo/a.go::A"}, ids)
	require.Equal(t, 1, facts.declarationCalls)
	require.Zero(t, facts.fullCalls)
}
