package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPayloadInputWitnessDetectsCommittedBindingTypeChanges(t *testing.T) {
	for _, mode := range []string{"replace_repository", "replace_file", "delete_file"} {
		t.Run(mode, func(t *testing.T) {
			s := openPayloadStore(t)
			site := graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: "repo/handler.go", Line: 17, Name: "value"}
			require.NoError(t, s.ReplaceSemanticBindingTypes("repo", []graph.SemanticBindingType{{Site: site, TypeName: "Before"}}))
			witness, err := s.CapturePayloadInputWitness(t.Context(), []int64{0})
			require.NoError(t, err)
			rows := []graph.SemanticBindingType{{Site: site, TypeName: "After"}}
			switch mode {
			case "replace_repository":
				err = s.ReplaceSemanticBindingTypes("repo", rows)
			case "replace_file":
				err = s.ReplaceSemanticBindingTypesForFiles("repo", []string{site.FilePath}, rows)
			case "delete_file":
				err = s.DeleteSemanticBindingTypesByFiles("repo", []string{site.FilePath})
			}
			require.NoError(t, err)
			current, err := s.SemanticBindingTypes([]graph.SemanticBindingSite{site})
			require.NoError(t, err)
			if mode == "delete_file" {
				require.NotContains(t, current, site, "actual semantic answer must change before testing the witness")
			} else {
				require.Equal(t, "After", current[site])
			}
			s.writeMu.Lock()
			err = witness.validateLocked(s, t.Context())
			s.writeMu.Unlock()
			require.ErrorIs(t, err, ErrPayloadInputChanged, "a selected proof may not certify an older semantic binding answer")
		})
	}
}

func TestSemanticBindingProofIsScopedWithoutInvalidatingNodeProjection(t *testing.T) {
	s := openPayloadStore(t)
	s.AddNode(&graph.Node{ID: "repo/handler.go::Handle", Name: "Handle", Kind: graph.KindFunction, FilePath: "repo/handler.go"})
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	before, err := s.CaptureContractProjectionCursor(t.Context())
	require.NoError(t, err)
	rowProjection := s.PayloadInputRevision()
	rows := []graph.SemanticBindingType{
		{Site: graph.SemanticBindingSite{RepoPrefix: "ignored", FilePath: "repo/handler.go", Line: 17, Name: "value"}, TypeName: "Response"},
		{Site: graph.SemanticBindingSite{FilePath: "repo/other.go", Line: 2, Name: "other"}, TypeName: "Other"},
	}
	require.NoError(t, s.ReplaceSemanticBindingTypesForFiles("repo", []string{"repo/handler.go"}, rows))
	require.Equal(t, rowProjection, s.PayloadInputRevision(), "binding-only writes do not stale a cached node/mask projection")
	delta, err := s.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.True(t, delta.Complete)
	require.ElementsMatch(t, []ContractProjectionDomain{
		{Kind: "semantic_file", Scope: "repo", Key: "repo/handler.go"},
		{Kind: "semantic_file", Scope: "repo", Key: "repo/other.go"},
	}, delta.Domains, "historically allowed inserts outside files remain part of the real proof frontier")

	witness, err := s.CapturePayloadInputWitness(t.Context(), []int64{0})
	require.NoError(t, err)
	require.NoError(t, s.AtGeneration(11).ReplaceSemanticBindingTypes("repo", rows))
	s.writeMu.Lock()
	err = witness.validateLocked(s, t.Context())
	s.writeMu.Unlock()
	require.NoError(t, err, "unselected physical generation cannot invalidate this exact witness")
}

func TestSemanticBindingOnlyEvictionInvalidatesWitness(t *testing.T) {
	s := openPayloadStore(t)
	sites := []graph.SemanticBindingSite{
		{RepoPrefix: "", FilePath: "shared/orphan.go", Line: 3, Name: "x"},
		{RepoPrefix: "other", FilePath: "shared/orphan.go", Line: 3, Name: "x"},
	}
	for _, site := range sites {
		require.NoError(t, s.ReplaceSemanticBindingTypes(site.RepoPrefix, []graph.SemanticBindingType{{Site: site, TypeName: "Before"}}))
	}
	current, err := s.SemanticBindingTypes(sites)
	require.NoError(t, err)
	require.Len(t, current, 2, "same physical path can have binding rows in different repositories")
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	before, err := s.CaptureContractProjectionCursor(t.Context())
	require.NoError(t, err)
	witness, err := s.CapturePayloadInputWitness(t.Context(), []int64{0})
	require.NoError(t, err)
	nodes, edges := s.EvictFile(sites[0].FilePath)
	require.Zero(t, nodes)
	require.Zero(t, edges)
	current, err = s.SemanticBindingTypes(sites)
	require.NoError(t, err)
	require.Empty(t, current, "eviction removes all repository bindings for the path")
	s.writeMu.Lock()
	err = witness.validateLocked(s, t.Context())
	s.writeMu.Unlock()
	require.ErrorIs(t, err, ErrPayloadInputChanged, "no-node early return must still announce changed sidecars")
	delta, err := s.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.False(t, delta.Complete, "unobserved removed repository scopes require physical proof")
	require.Nil(t, delta.Domains, "empty repository is literal, not a wildcard for all repositories")
}

func TestSemanticBindingRollbackAndEmptyDeleteDoNotAdvanceProof(t *testing.T) {
	s := openPayloadStore(t)
	site := graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: "repo/handler.go", Line: 3, Name: "x"}
	require.NoError(t, s.ReplaceSemanticBindingTypes("repo", []graph.SemanticBindingType{{Site: site, TypeName: "Before"}}))
	_, err := s.writerDB.Exec(`CREATE TRIGGER reject_binding_proof_test BEFORE INSERT ON semantic_binding_types WHEN NEW.type_name='Fail' BEGIN SELECT RAISE(ABORT, 'intentional binding failure'); END`)
	require.NoError(t, err)
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	before, err := s.CaptureContractProjectionCursor(t.Context())
	require.NoError(t, err)
	witness, err := s.CapturePayloadInputWitness(t.Context(), []int64{0})
	require.NoError(t, err)
	require.Error(t, s.ReplaceSemanticBindingTypes("repo", []graph.SemanticBindingType{{Site: site, TypeName: "Fail"}}))
	current, err := s.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, err)
	require.Equal(t, "Before", current[site], "failed replacement restores the deleted old value")
	require.NoError(t, s.DeleteSemanticBindingTypesByFiles("repo", []string{"repo/missing.go"}))
	delta, err := s.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.True(t, delta.Complete)
	require.Equal(t, before, delta.After)
	require.Empty(t, delta.Domains)
	s.writeMu.Lock()
	err = witness.validateLocked(s, t.Context())
	s.writeMu.Unlock()
	require.NoError(t, err)
}
