package indexer

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

type endpointScalarDeclarationReader struct{ *endpointDeclarationReader }

func (r *endpointScalarDeclarationReader) VisitNodesByNameContext(context.Context, string, func(*graph.Node) bool) error {
	r.fullCalls++
	return errors.New("endpoint scalar requested unconsumed full metadata")
}
func TestContractEndpointScalarUsesCheckedDeclarationFacts(t *testing.T) {
	for _, mode := range []string{"enrichment_unbatched", "one_question_bracket", "incomplete_group_fallback"} {
		t.Run(mode, func(t *testing.T) {
			rows := &endpointReplayRowsReader{rows: []*graph.Node{
				{ID: "repo/a.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "repo/a.go", RepoPrefix: "repo"},
				{ID: "repo/a.go::param:A", Name: "A", Kind: graph.KindParam, FilePath: "repo/a.go", RepoPrefix: "repo"},
				{ID: "elsewhere/a.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "elsewhere/a.go", RepoPrefix: "repo"},
			}}
			e := endpointReplayEvidence(t, rows, "A")
			if mode == "enrichment_unbatched" {
				e.endpointReplay = nil
			}
			if mode == "incomplete_group_fallback" {
				e.endpointReplay.add("Missing")
				rows.badBatch = true
			}
			facts := &endpointScalarDeclarationReader{endpointDeclarationReader: &endpointDeclarationReader{endpointReplayRowsReader: rows}}
			e.core = facts
			got := e.FindEndpointConstantCandidates("A", "repo")
			t.Logf("declaration_calls=%d full_calls=%d", facts.declarationCalls, facts.fullCalls)
			require.NoError(t, e.err, "scalar endpoint resolution must consume only its checked declaration facts")
			require.Equal(t, []contracts.EndpointConstantCandidate{{ID: "repo/a.go::A", FilePath: "repo/a.go"}}, got)
			require.Zero(t, facts.fullCalls, "unconsumed full metadata is never required when checked declaration facts are available")
			wantCalls := 1
			if mode == "incomplete_group_fallback" {
				wantCalls = 2
			}
			require.Equal(t, wantCalls, facts.declarationCalls)
			require.Len(t, e.ledger.reads, 1)
			require.True(t, e.ledger.keys["name:A"])
			require.Equal(t, map[string]bool{"repo/a.go": true}, e.ledger.owners)
		})
	}
}

func TestContractEndpointScalarDeclarationErrorsCapsAndCheckedFallback(t *testing.T) {
	for _, mode := range []string{"partial_error", "all_kind_row_cap", "canceled", "legacy_checked_fallback"} {
		t.Run(mode, func(t *testing.T) {
			row := &graph.Node{ID: "repo/a.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "repo/a.go", RepoPrefix: "repo"}
			rows := &endpointReplayRowsReader{rows: []*graph.Node{row}}
			e := endpointReplayEvidence(t, rows)
			e.endpointReplay = nil
			facts := &endpointScalarDeclarationReader{endpointDeclarationReader: &endpointDeclarationReader{endpointReplayRowsReader: rows}}
			e.core = facts
			failure := errors.New("checked declaration cursor failed")
			switch mode {
			case "partial_error":
				rows.err = failure
			case "all_kind_row_cap":
				row.Kind = graph.KindParam
				rows.repeat = graph.ContractProjectionRowLimit + 1
			case "canceled":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				e.ctx = ctx
			case "legacy_checked_fallback":
				e.core = rows
			}
			got := e.FindEndpointConstantCandidates("A", "repo")
			if mode == "legacy_checked_fallback" {
				require.NoError(t, e.err)
				require.Equal(t, []contracts.EndpointConstantCandidate{{ID: row.ID, FilePath: row.FilePath}}, got)
				require.Equal(t, 1, rows.calls)
				return
			}
			require.Nil(t, got)
			require.Empty(t, e.ledger.reads, "partial,error,capped and canceled answers cannot certify absence")
			switch mode {
			case "partial_error":
				require.ErrorIs(t, e.err, failure)
			case "all_kind_row_cap":
				require.ErrorIs(t, e.err, graph.ErrContractProjectionLimit)
			case "canceled":
				require.ErrorIs(t, e.err, context.Canceled)
			}
			require.Zero(t, facts.fullCalls, "an authoritative declaration failure cannot fall through to another reader")
		})
	}
}
