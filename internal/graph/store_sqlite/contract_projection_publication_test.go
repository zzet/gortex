package store_sqlite

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestContractBatchProjectionWitnessRefusesLateBindingAcrossCohort(t *testing.T) {
	for _, mode := range []string{"replace_repository", "replace_file", "delete_file"} {
		t.Run(mode, func(t *testing.T) {
			s := openCatalogStore(t)
			ctx := t.Context()
			targets := []ContractAttachmentPublishTarget{attachmentBatchTarget(t, s, "one"), attachmentBatchTarget(t, s, "two")}
			site := graph.SemanticBindingSite{RepoPrefix: "two", FilePath: "two/handler.go", Line: 7, Name: "value"}
			h := targets[1].Store
			require.NoError(t, h.ReplaceSemanticBindingTypes("two", []graph.SemanticBindingType{{Site: site, TypeName: "Before"}}))
			for i := range targets {
				var err error
				targets[i].ProjectionWitness, err = s.CapturePayloadInputWitness(ctx, []int64{targets[i].Store.viewGen})
				require.NoError(t, err)
			}
			rows := []graph.SemanticBindingType{{Site: site, TypeName: "After"}}
			switch mode {
			case "replace_repository":
				require.NoError(t, h.ReplaceSemanticBindingTypes("two", rows))
			case "replace_file":
				require.NoError(t, h.ReplaceSemanticBindingTypesForFiles("two", []string{site.FilePath}, rows))
			case "delete_file":
				require.NoError(t, h.DeleteSemanticBindingTypesByFiles("two", []string{site.FilePath}))
			}
			answer, err := h.SemanticBindingTypes([]graph.SemanticBindingSite{site})
			require.NoError(t, err)
			if mode == "delete_file" {
				require.NotContains(t, answer, site)
			} else {
				require.Equal(t, "After", answer[site])
			}
			result, err := s.PublishContractAttachmentsWithInputsContext(ctx, targets, 1)
			require.ErrorIs(t, err, ErrPayloadInputChanged)
			require.Nil(t, result)
			assertAttachmentBatchUnpublished(t, s, targets)
		})
	}
}

func TestContractBatchProjectionWitnessExactSelectionAndUnselectedWrites(t *testing.T) {
	for _, mode := range []string{"extra_generation", "sibling", "foreign", "unselected", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s := openCatalogStore(t)
			ctx := t.Context()
			target := attachmentBatchTarget(t, s, "one")
			sibling := attachmentBatchTarget(t, s, "unrelated").Store.viewGen
			var err error
			switch mode {
			case "extra_generation":
				target.ProjectionWitness, err = s.CapturePayloadInputWitness(ctx, []int64{target.Store.viewGen, sibling})
			case "sibling":
				target.ProjectionWitness, err = s.CapturePayloadInputWitness(ctx, []int64{sibling})
			case "foreign":
				other := openCatalogStore(t)
				foreign := attachmentBatchTarget(t, other, "one")
				require.Equal(t, target.Store.viewGen, foreign.Store.viewGen)
				target.ProjectionWitness, err = other.CapturePayloadInputWitness(ctx, []int64{foreign.Store.viewGen})
			default:
				target.ProjectionWitness, err = s.CapturePayloadInputWitness(ctx, []int64{target.Store.viewGen})
			}
			require.NoError(t, err)
			if mode == "unselected" {
				require.NoError(t, s.AtGeneration(sibling).ReplaceSemanticBindingTypes("one", []graph.SemanticBindingType{{Site: graph.SemanticBindingSite{FilePath: "one/handler.go", Line: 1}, TypeName: "Unrelated"}}))
			}
			if mode == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			result, err := s.PublishContractAttachmentsWithInputsContext(ctx, []ContractAttachmentPublishTarget{target}, 1)
			if mode == "unselected" {
				require.NoError(t, err)
				require.Len(t, result, 1)
			} else {
				require.Error(t, err)
				require.Nil(t, result)
				assertAttachmentBatchUnpublished(t, s, []ContractAttachmentPublishTarget{target})
			}
		})
	}
}
