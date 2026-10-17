package store_sqlite

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestConstantInputRevisionCommittedChangesAndNoops(t *testing.T) {
	s := openPayloadStore(t)
	counter := s.constantInputCounter(0)
	row := graph.ConstantValueRow{NodeID: "constant", FilePath: "file.go", Value: "one"}
	require.NoError(t, s.BulkSetConstantValues("repo", []graph.ConstantValueRow{row}))
	require.Equal(t, uint64(1), counter.Load())
	require.NoError(t, s.BulkSetConstantValues("repo", []graph.ConstantValueRow{row}))
	require.Equal(t, uint64(1), counter.Load())
	require.NoError(t, s.ReplaceConstantValues("repo", []graph.ConstantValueRow{row}))
	require.Equal(t, uint64(1), counter.Load())
	require.NoError(t, s.DeleteConstantValuesByFiles("repo", []string{"absent.go"}))
	require.Equal(t, uint64(1), counter.Load())
	changed := row
	changed.Value = "two"
	require.NoError(t, s.BulkSetConstantValues("repo", []graph.ConstantValueRow{changed}))
	require.Equal(t, uint64(2), counter.Load())
	require.Error(t, s.ReplaceConstantValues("repo", []graph.ConstantValueRow{row, row}))
	require.Equal(t, uint64(2), counter.Load(), "rolled back delete/insert cannot move clock")
	actual, err := s.ConstantValuesByNodeIDs([]string{row.NodeID})
	require.NoError(t, err)
	require.Equal(t, "two", actual[row.NodeID])
	require.NoError(t, s.ReplaceConstantValues("repo", []graph.ConstantValueRow{row}))
	require.Equal(t, uint64(3), counter.Load())
	require.NoError(t, s.DeleteConstantValuesByFiles("repo", []string{row.FilePath}))
	require.Equal(t, uint64(4), counter.Load())
	require.NoError(t, s.ReplaceConstantValues("repo", nil))
	require.Equal(t, uint64(4), counter.Load())
	require.NoError(t, s.BulkSetConstantValues("repo", nil))
	require.NoError(t, s.DeleteConstantValuesByFiles("repo", nil))
	require.Equal(t, uint64(4), counter.Load())
}
