package store_sqlite

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestFileInventoryInputRevisionChangesNoopsAndRollback(t *testing.T) {
	s := openPayloadStore(t)
	counter := s.constantInputCounter(0)
	row := graph.FileMetaRow{FilePath: "file.go", ContentHash: "one", Size: 10, NodeCount: 2, Errors: "parse error"}
	require.NoError(t, s.SetFileMetas("repo", []graph.FileMetaRow{row}))
	require.Equal(t, uint64(1), counter.Load())
	require.NoError(t, s.SetFileMetas("repo", []graph.FileMetaRow{row}))
	require.NoError(t, s.ReplaceFileMetas("repo", []graph.FileMetaRow{row}))
	require.NoError(t, s.DeleteFileMetasByFiles("repo", []string{"absent"}))
	require.Equal(t, uint64(1), counter.Load())
	changed := row
	changed.ContentHash = "two"
	require.NoError(t, s.SetFileMetas("repo", []graph.FileMetaRow{changed}))
	require.Equal(t, uint64(2), counter.Load())
	require.Error(t, s.ReplaceFileMetas("repo", []graph.FileMetaRow{row, row}))
	require.Equal(t, uint64(2), counter.Load())
	actual, err := s.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Equal(t, []graph.FileMetaRow{changed}, actual)
	require.NoError(t, s.ReplaceFileMetas("repo", []graph.FileMetaRow{row}))
	require.Equal(t, uint64(3), counter.Load())
	require.NoError(t, s.DeleteFileMetasByFiles("repo", []string{row.FilePath}))
	require.Equal(t, uint64(4), counter.Load())
	require.NoError(t, s.ReplaceFileMetas("repo", nil))
	require.Equal(t, uint64(4), counter.Load())
}

func TestFileInventoryInputWitnessSelectedGenerationOnly(t *testing.T) {
	for _, operation := range []string{"set", "replace", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s := openPayloadStore(t)
			seedPayloadControlPlane(t, s)
			input := witnessBuilding(t, s, "input")
			output := witnessBuilding(t, s, "output")
			baseOutput := witnessBuilding(t, s, "base-output")
			selected := s.AtGeneration(input)
			row := graph.FileMetaRow{FilePath: "file.go", ContentHash: "one"}
			require.NoError(t, selected.SetFileMetas("repo", []graph.FileMetaRow{row}))
			w, err := s.CapturePayloadInputWitness(t.Context(), []int64{input})
			require.NoError(t, err)
			base, err := s.CapturePayloadInputWitness(t.Context(), []int64{0})
			require.NoError(t, err)
			switch operation {
			case "set":
				row.ContentHash = "two"
				require.NoError(t, selected.SetFileMetas("repo", []graph.FileMetaRow{row}))
			case "replace":
				require.NoError(t, selected.ReplaceFileMetas("repo", nil))
			case "delete":
				require.NoError(t, selected.DeleteFileMetasByFiles("repo", []string{row.FilePath}))
			}
			require.ErrorIs(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), output, 100, w), ErrPayloadInputChanged)
			require.NoError(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), baseOutput, 100, base))
		})
	}
}
