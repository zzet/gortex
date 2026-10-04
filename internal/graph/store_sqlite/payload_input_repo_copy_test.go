package store_sqlite

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestPayloadInputWitnessRepoCopyChangesTargetOnly(t *testing.T) {
	s := openPayloadStore(t)
	seedPayloadControlPlane(t, s)
	s.AddNode(&graph.Node{ID: "repo/source", Name: "source", RepoPrefix: "repo", FilePath: "source.go", Kind: graph.KindFunction})
	input := witnessBuilding(t, s, "input")
	output := witnessBuilding(t, s, "output")
	baseOutput := witnessBuilding(t, s, "base-output")
	selected, err := s.CapturePayloadInputWitness(t.Context(), []int64{input})
	require.NoError(t, err)
	base, err := s.CapturePayloadInputWitness(t.Context(), []int64{0})
	require.NoError(t, err)
	counts, err := s.CopyPayloadGeneration(t.Context(), 0, input, "repo")
	require.NoError(t, err)
	require.Positive(t, counts.Nodes)
	require.ErrorIs(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), output, 100, selected), ErrPayloadInputChanged)
	require.NoError(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), baseOutput, 100, base), "copy must not change source generation witness")
	fresh, err := s.CapturePayloadInputWitness(t.Context(), []int64{input})
	require.NoError(t, err)
	_, err = s.CopyPayloadGeneration(t.Context(), 0, input, "repo")
	require.ErrorIs(t, err, ErrGenerationBulkLoadPopulated)
	require.NoError(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), output, 100, fresh), "refused second copy must not withdraw witness")
}

func TestPayloadInputWitnessRepoCopyNoopAndRollback(t *testing.T) {
	for _, kind := range []string{"empty", "rollback"} {
		t.Run(kind, func(t *testing.T) {
			s := openPayloadStore(t)
			seedPayloadControlPlane(t, s)
			input := witnessBuilding(t, s, "input")
			output := witnessBuilding(t, s, "output")
			if kind == "rollback" {
				s.AddNode(&graph.Node{ID: "repo/source", Name: "source", RepoPrefix: "repo", FilePath: "source.go", Kind: graph.KindFunction})
				require.NoError(t, s.BulkSetConstantValues("repo", []graph.ConstantValueRow{{NodeID: "repo/source", FilePath: "source.go", Value: "one"}}))
				_, err := s.writerDB.Exec(fmt.Sprintf("CREATE TRIGGER fail_repo_copy BEFORE INSERT ON constant_values WHEN NEW.view_gen=%d BEGIN SELECT RAISE(ABORT,'copy failed'); END", input))
				require.NoError(t, err)
			}
			w, err := s.CapturePayloadInputWitness(t.Context(), []int64{input})
			require.NoError(t, err)
			counts, err := s.CopyPayloadGeneration(t.Context(), 0, input, "repo")
			if kind == "rollback" {
				require.Error(t, err)
				require.Nil(t, s.AtGeneration(input).GetNode("repo/source"), "partial node copy must roll back")
			} else {
				require.NoError(t, err)
				require.Zero(t, counts.Rows)
			}
			require.NoError(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), output, 100, w), "no-op/rolled-back copy must not withdraw witness")
		})
	}
}
