package indexer

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func positiveProjectionGeneration(t *testing.T, s *store_sqlite.Store, kind string, base int64) int64 {
	t.Helper()
	id, err := s.Catalog().CreateViewGeneration(t.Context(), store_sqlite.ViewGeneration{OwnerKind: "proof_fixture", GenerationKind: kind, BaseGenerationID: base, State: store_sqlite.ViewGenerationBuilding})
	require.NoError(t, err)
	return id
}
func positiveProjectionCapture(t *testing.T, reader graph.Reader, handles ...*store_sqlite.Store) *contractPositiveNameProjection {
	t.Helper()
	for _, h := range handles {
		row, found, err := h.Catalog().GetViewGeneration(t.Context(), h.ViewGeneration())
		require.NoError(t, err)
		if found && row.State == store_sqlite.ViewGenerationBuilding {
			require.NoError(t, h.Catalog().PublishViewGeneration(t.Context(), h.ViewGeneration(), 1))
		}
	}
	p, err := capturePositiveNames(t.Context(), map[string]graph.Reader{"r": reader}, map[string][]*store_sqlite.Store{"r": handles}, 1<<20)
	require.NoError(t, err)
	return p
}

func TestContractPositiveNameProjectionChecksFoldPrefixAndNegativeMasks(t *testing.T) {
	s := sparseShapeStore(t)
	rootID := positiveProjectionGeneration(t, s, CommitLayerGenerationKind, 0)
	base := s.AtGeneration(rootID)
	base.AddNode(&graph.Node{ID: "r/f.go::Old", Name: "Old", Kind: graph.KindConstant, FilePath: "r/f.go", RepoPrefix: "r"})
	a := positiveProjectionGeneration(t, s, DirtyLayerGenerationKind, rootID)
	b := positiveProjectionGeneration(t, s, DirtyLayerGenerationKind, a)
	folded := positiveProjectionGeneration(t, s, DirtyLayerGenerationKind, rootID)
	nodeF := &graph.Node{ID: "r/f.go::Route", Name: "Route", Kind: graph.KindFunction, FilePath: "r/f.go", RepoPrefix: "r"}
	nodeG := &graph.Node{ID: "r/g.go::Boundary", Name: "Boundary", Kind: graph.KindConstant, FilePath: "r/g.go", RepoPrefix: "r"}
	maskF := store_sqlite.FileMask{RepoPrefix: "r", FilePath: "r/f.go", Mode: store_sqlite.OwnershipReplace}
	maskG := store_sqlite.FileMask{RepoPrefix: "r", FilePath: "r/g.go", Mode: store_sqlite.OwnershipReplace}
	_, la := sparseShapeLayer(t, s, a, []*graph.Node{nodeF}, []store_sqlite.FileMask{maskF})
	_, lb := sparseShapeLayer(t, s, b, []*graph.Node{nodeG}, []store_sqlite.FileMask{maskG})
	_, lf := sparseShapeLayer(t, s, folded, []*graph.Node{nodeF, nodeG}, []store_sqlite.FileMask{maskF, maskG})
	oldReader := graph.NewOverlaidViewWithLayer(graph.NewOverlaidViewWithLayer(base, la), lb)
	newReader := graph.NewOverlaidViewWithLayer(base, lf)
	old := positiveProjectionCapture(t, oldReader, base, s.AtGeneration(a), s.AtGeneration(b))
	next := positiveProjectionCapture(t, newReader, base, s.AtGeneration(folded))
	require.NotNil(t, old)
	require.NotNil(t, next)
	for _, name := range []string{"Route", "Boundary", "Old", "Missing"} {
		require.True(t, old.sameName(next, name), name)
		require.Equal(t, sparseShapeRead(t, oldReader, name), sparseShapeRead(t, newReader, name))
	}
	// A permitted late compiler write to the positive prefix invalidates its
	// clock even when both declaration answers remain empty.
	require.NoError(t, base.ReplaceSemanticBindingTypes("r", []graph.SemanticBindingType{{Site: graph.SemanticBindingSite{RepoPrefix: "r", FilePath: "r/f.go", Line: 1, Name: "value"}, TypeName: "New"}}))
	changed := positiveProjectionCapture(t, newReader, base, s.AtGeneration(folded))
	require.False(t, old.sameName(changed, "Hidden"))
	unmasked := positiveProjectionGeneration(t, s, DirtyLayerGenerationKind, rootID)
	_, lu := sparseShapeLayer(t, s, unmasked, nil, nil)
	revealed := positiveProjectionCapture(t, graph.NewOverlaidViewWithLayer(base, lu), base, s.AtGeneration(unmasked))
	require.False(t, changed.sameName(revealed, "Hidden"))
	require.Nil(t, positiveProjectionCapture(t, s.AtGeneration(0), s.AtGeneration(0)), "mutable generation zero never enters carried proof")
}

func TestContractPositiveNameCarryLeavesUnknownDebtAndLateMutationPhysical(t *testing.T) {
	s := sparseShapeStore(t)
	rootID := positiveProjectionGeneration(t, s, CommitLayerGenerationKind, 0)
	h := s.AtGeneration(rootID)
	node := &graph.Node{ID: "r/a.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "r/a.go", RepoPrefix: "r"}
	h.AddNode(node)
	prior := positiveProjectionCapture(t, h, h)
	current := positiveProjectionCapture(t, h, h)
	rows := &endpointReplayRowsReader{rows: []*graph.Node{node}}
	evidence := endpointReplayEvidence(t, rows, "A")
	// Record the actual complete old answer, including owner provenance.
	evidence.allowedRepos = map[string]bool{"r": true}
	evidence.FindEndpointConstantCandidates("A", "r")
	require.NoError(t, evidence.err)
	reads := slices.Clone(evidence.ledger.reads)
	prior.recordName(contractNameQuestionKey(reads[0].args), reads[0].args[0], reads[0].digest)
	rows.calls = 0
	c := &contractConvergence{ctx: t.Context(), report: &ContractFollowupReport{}, coverage: newContractWorkCoverage([]graph.ContractWork{{Scope: graph.ContractWorkScope{Causes: []string{"local_boundary_changed"}}}})}
	c.req.delta = &contractDeltaSession{adopted: true, cache: &contractDeltaCache{names: prior}, currentNames: current}
	c.snap.positiveNameCurrent = func(ctx context.Context, p *contractPositiveNameProjection) bool {
		clocks, err := positiveNameClocks(ctx, map[string]graph.Reader{"r": h})
		return err == nil && slices.Equal(clocks, p.clocks)
	}
	uncertified := reads[0]
	uncertified.digest = "not-the-observed-answer"
	require.False(t, c.mayCarryPositiveName(uncertified), "a fresh shape cannot retrospectively bless an older unobserved digest")
	equal, err := c.replay(evidence, reads, nil)
	require.NoError(t, err)
	require.True(t, equal)
	require.Equal(t, 1, c.report.NameProjectionCarries)
	require.Zero(t, rows.calls)
	// New dependency-only debt is not blessed by this first slice.
	c.coverage.work[0].Scope.Causes = []string{"dependency_input_changed"}
	evidence.endpointReplay = nil
	equal, err = c.replay(evidence, reads, nil)
	require.NoError(t, err)
	require.True(t, equal)
	require.Positive(t, rows.calls)
	c.coverage.work[0].Scope.Causes = []string{"local_boundary_changed"}
	rows.calls = 0
	require.NoError(t, h.ReplaceSemanticBindingTypes("r", []graph.SemanticBindingType{{Site: graph.SemanticBindingSite{RepoPrefix: "r", FilePath: "r/a.go", Line: 1, Name: "value"}, TypeName: "New"}}))
	equal, err = c.replay(evidence, reads, nil)
	require.NoError(t, err)
	require.True(t, equal)
	require.Positive(t, rows.calls, "a late selected mutation invalidates even a logically unchanged carried name")
}

func TestContractPositiveNameValuesRefuseNewProducerAndNegativeReveal(t *testing.T) {
	s := sparseShapeStore(t)
	rootID := positiveProjectionGeneration(t, s, CommitLayerGenerationKind, 0)
	h := s.AtGeneration(rootID)
	node := &graph.Node{ID: "r/f.go::A", Name: "A", Kind: graph.KindConstant, FilePath: "r/f.go", RepoPrefix: "r"}
	h.AddNode(node)
	prior := positiveProjectionCapture(t, h, h)
	answer := []contracts.EndpointConstantCandidate{{ID: node.ID, FilePath: node.FilePath}}
	digest, valid := contractAnswerDigest(answer)
	require.True(t, valid)
	prior.recordName(contractNameQuestionKey([]string{"A", "r"}), "A", digest, answer)
	emptyDigest, valid := contractAnswerDigest([]contracts.EndpointConstantCandidate(nil))
	require.True(t, valid)
	prior.recordName(contractNameQuestionKey([]string{"Missing", "r"}), "Missing", emptyDigest, nil)
	require.True(t, completePositivePrefixNames(t.Context(), map[string]graph.Reader{"r": h}, prior, nil))
	current := positiveProjectionCapture(t, h, h)
	c := &contractConvergence{ctx: t.Context(), report: &ContractFollowupReport{}}
	c.req.delta = &contractDeltaSession{adopted: true, cache: &contractDeltaCache{names: prior}, currentNames: current}
	c.snap.positiveNameCurrent = func(context.Context, *contractPositiveNameProjection) bool { return true }
	got, ok := c.positiveCandidateValue("A", "r")
	require.True(t, ok)
	require.Equal(t, answer, got)
	got[0].ID = "caller-mutation"
	got, ok = c.positiveCandidateValue("A", "r")
	require.True(t, ok)
	require.Equal(t, node.ID, got[0].ID)
	got, ok = c.positiveCandidateValue("Missing", "r")
	require.True(t, ok)
	require.Nil(t, got, "explicit certified negative preserves the original nil answer and digest")
	_, ok = c.positiveCandidateValue("NewQuestion", "r")
	require.False(t, ok)
	// A new sparse producer changes the selected membership even while the
	// original prefix and all prior positive owner receipts remain unchanged.
	added := positiveProjectionGeneration(t, s, DirtyLayerGenerationKind, rootID)
	newNode := &graph.Node{ID: "r/g.go::Missing", Name: "Missing", Kind: graph.KindConstant, FilePath: "r/g.go", RepoPrefix: "r"}
	_, layer := sparseShapeLayer(t, s, added, []*graph.Node{newNode}, []store_sqlite.FileMask{{RepoPrefix: "r", FilePath: "r/g.go", Mode: store_sqlite.OwnershipReplace}})
	c.req.delta.currentNames = positiveProjectionCapture(t, graph.NewOverlaidViewWithLayer(h, layer), h, s.AtGeneration(added))
	_, ok = c.positiveCandidateValue("Missing", "r")
	require.False(t, ok)
	// An unrelated new mask is permitted only by complete inherited rows for
	// this exact name. A mask removing A must invalidate its positive answer.
	removed := positiveProjectionGeneration(t, s, DirtyLayerGenerationKind, rootID)
	_, deleted := sparseShapeLayer(t, s, removed, nil, []store_sqlite.FileMask{{RepoPrefix: "r", FilePath: "r/f.go", Mode: store_sqlite.OwnershipDelete}})
	c.req.delta.currentNames = positiveProjectionCapture(t, graph.NewOverlaidViewWithLayer(h, deleted), h, s.AtGeneration(removed))
	_, ok = c.positiveCandidateValue("A", "r")
	require.False(t, ok)
	bounded, err := capturePositiveNames(t.Context(), map[string]graph.Reader{"r": h}, map[string][]*store_sqlite.Store{"r": {h}}, 4095)
	require.NoError(t, err)
	require.Nil(t, bounded)
}
