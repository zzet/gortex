package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

type endpointReplayRowsReader struct {
	graph.Reader
	rows     []*graph.Node
	repeat   int
	err      error
	badBatch bool
	guard    *contractReadGuard
	calls    int
}

func (r *endpointReplayRowsReader) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	return r.visit(ctx, []string{name}, yield)
}
func (r *endpointReplayRowsReader) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	return r.visit(ctx, names, yield)
}
func (r *endpointReplayRowsReader) visit(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	r.calls++
	if !r.guard.recorded() {
		return graph.ErrContractProjectionIncomplete
	}
	if len(names) > 1 && r.badBatch {
		yield(nil)
		return r.err
	}
	for _, name := range names {
		for _, node := range r.rows {
			if node.Name == name {
				for range max(1, r.repeat) {
					if !yield(node) {
						return ctx.Err()
					}
				}
			}
		}
	}
	if r.err != nil {
		return r.err
	}
	return ctx.Err()
}
func endpointReplayEvidence(t *testing.T, r *endpointReplayRowsReader, names ...string) *contractFollowupEvidence {
	t.Helper()
	guard := &contractReadGuard{open: true}
	r.guard = guard
	b := &contractEndpointReplayBatch{}
	for _, name := range names {
		b.add(name)
	}
	return &contractFollowupEvidence{ctx: t.Context(), core: r, guard: guard, ledger: newContractUnitLedger(), endpointReplay: b, allowedRepos: map[string]bool{"repo": true}}
}
func TestContractEndpointReplayBatchPreservesLogicalProofsAndFreshBrackets(t *testing.T) {
	r := &endpointReplayRowsReader{rows: []*graph.Node{
		{ID: "repo/a.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "repo/a.go", RepoPrefix: "repo", Meta: map[string]any{"semantic_type": "rich"}},
		{ID: "repo/a.go::param:A", Name: "A", Kind: graph.KindParam, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "other/a.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "other/a.go", RepoPrefix: "repo"},
	}}
	e := endpointReplayEvidence(t, r, "A", "Missing")
	want := []contracts.EndpointConstantCandidate{{ID: "repo/a.go::A", FilePath: "repo/a.go"}}
	got := e.FindEndpointConstantCandidates("A", "repo")
	require.Equal(t, want, got)
	require.Empty(t, e.FindEndpointConstantCandidates("Missing", "repo"))
	require.NoError(t, e.err)
	require.Equal(t, 1, r.calls)
	require.Len(t, e.ledger.reads, 2)
	require.True(t, e.ledger.keys["name:Missing"])
	require.Equal(t, map[string]bool{"repo/a.go": true}, e.ledger.owners)
	got[0].FilePath = "mutated"
	require.Equal(t, want, e.FindEndpointConstantCandidates("A", "repo"))
	require.Equal(t, 1, r.calls)
	e.endpointReplay.close()
	require.Nil(t, e.endpointReplay.rows)
	require.Nil(t, e.endpointReplay.names)
	r.rows = append(r.rows, &graph.Node{ID: "repo/new.go::Missing", Name: "Missing", Kind: graph.KindConstant, FilePath: "repo/new.go", RepoPrefix: "repo"})
	next := endpointReplayEvidence(t, r, "A", "Missing")
	require.Equal(t, []contracts.EndpointConstantCandidate{{ID: "repo/new.go::Missing", FilePath: "repo/new.go"}}, next.FindEndpointConstantCandidates("Missing", "repo"))
	require.NoError(t, next.err)
	require.Equal(t, 2, r.calls)
	// Invalid selected ownership remains an error, including on a cached answer.
	r.rows = append(r.rows, &graph.Node{Name: "A", Kind: graph.KindConstant, FilePath: "repo/invalid.go", RepoPrefix: "repo"})
	invalid := endpointReplayEvidence(t, r, "A", "Missing")
	invalid.FindEndpointConstantCandidates("A", "repo")
	require.ErrorContains(t, invalid.err, "invalid endpoint constant declaration")
}
func TestContractEndpointReplayBatchFallsBackWithoutCertifyingPartialRows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bad     bool
		repeat  int
		err     error
		wantErr error
	}{
		{name: "unassignable row", bad: true},
		{name: "group row cap", repeat: graph.ContractProjectionRowLimit/2 + 1},
		{name: "per name row cap", repeat: graph.ContractProjectionRowLimit + 1, wantErr: graph.ErrContractProjectionLimit},
		{name: "partial then error", err: errors.New("selected iterator failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &endpointReplayRowsReader{rows: []*graph.Node{{Name: "A", Kind: graph.KindParam}, {Name: "B", Kind: graph.KindParam}}, badBatch: tc.bad, repeat: tc.repeat, err: tc.err}
			e := endpointReplayEvidence(t, r, "A", "B")
			_, err := e.endpointConstantCandidates("A", "repo")
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
			} else if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 2, r.calls, "an incomplete grouped read must fall back to the original single-name door")
			require.Nil(t, e.endpointReplay.rows, "partial/oversized/error rows are never retained as absence proof")
		})
	}
}
func TestContractEndpointReplayBatchBoundsNamesAndPreservesCancellation(t *testing.T) {
	r := &endpointReplayRowsReader{}
	e := endpointReplayEvidence(t, r)
	for i := 0; i < contractFrontierReadBatchSize+1; i++ {
		e.endpointReplay.add(fmt.Sprint(i))
	}
	require.Len(t, e.endpointReplay.names, contractFrontierReadBatchSize)
	e.endpointReplay.add("0")
	require.Len(t, e.endpointReplay.names, contractFrontierReadBatchSize)
	_, err := e.endpointConstantCandidates(fmt.Sprint(contractFrontierReadBatchSize), "repo")
	require.NoError(t, err)
	require.Equal(t, 2, r.calls, "unlisted names keep their original lookup")
	budget := &contractEndpointReplayBatch{bytes: contractFollowupCompactLimit - 2}
	budget.add("long")
	require.Empty(t, budget.names)
	e.endpointReplay.close()
	require.Nil(t, e.endpointReplay.rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e.ctx = ctx
	_, err = e.endpointConstantCandidates("0", "repo")
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, e.endpointReplay.names, "closed batch retains no name state")
}
