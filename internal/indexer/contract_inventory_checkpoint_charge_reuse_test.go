package indexer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractInventoryCheckpointChargeTracksOwnedReplacementAndSuccessor(t *testing.T) {
	h := sparseShapeStore(t).AtGeneration(10)
	row := graph.FileMetaRow{FilePath: "r/f.go", ContentHash: "first", Errors: "short"}
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	first := inventoryProjectionFor(h, h)
	_, err := first.read(t.Context(), "r")
	require.NoError(t, err)
	before := first.detachedValues()
	require.NotNil(t, before)
	requireCheckpointInventoryFullCharge(t, before)
	beforeCharge := checkpointInventoryFinish(checkpointInventoryConvergence(t, before))

	current := inventoryProjectionFor(h, h)
	current.seed(before)
	got, err := current.read(t.Context(), "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath])
	require.Zero(t, current.entries["r"].physicalReads)
	got[row.FilePath] = graph.FileMetaRow{Errors: "caller mutation"}
	requireCheckpointInventoryFullCharge(t, current.detachedValues())

	row.ContentHash = "changed"
	row.Errors = strings.Repeat("new owned diagnostic ", 4096)
	require.NoError(t, h.SetFileMetas("r", []graph.FileMetaRow{row}))
	got, err = current.read(t.Context(), "r")
	require.NoError(t, err)
	require.Equal(t, row, got[row.FilePath])
	require.Equal(t, 1, current.entries["r"].physicalReads)
	after := current.detachedValues()
	require.NotNil(t, after)
	requireCheckpointInventoryFullCharge(t, after)
	afterCharge := checkpointInventoryFinish(checkpointInventoryConvergence(t, after))
	require.GreaterOrEqual(t, afterCharge-beforeCharge, int64(len(row.Errors)))
	// Replacing this capture's rows cannot rewrite a predecessor's owned value.
	require.Equal(t, "first", before.entries["r"].rows[row.FilePath].ContentHash)
	require.Equal(t, "short", before.entries["r"].rows[row.FilePath].Errors)
	require.Equal(t, beforeCharge, checkpointInventoryFinish(checkpointInventoryConvergence(t, before)))
	requireCheckpointInventoryFullCharge(t, before)
}

func TestContractInventoryCheckpointMixedManualMutationKeepsFullCharge(t *testing.T) {
	owned := checkpointOwnedInventory(t, 1, 9)
	values := &contractInventoryValues{entries: make(map[string]contractInventoryValueEntry), bytes: 1}
	for repo, entry := range owned.entries {
		values.entries[repo] = entry
	}
	manualRows := map[string]graph.FileMetaRow{"manual/f.go": {FilePath: "manual/f.go", ContentHash: "accepted"}}
	values.entries["manual"] = contractInventoryValueEntry{rows: manualRows, bytes: 1}
	requireCheckpointInventoryFullCharge(t, values)
	small := checkpointInventoryFinish(checkpointInventoryConvergence(t, values))
	row := manualRows["manual/f.go"]
	row.Errors = strings.Repeat("unpooled manual diagnostic ", 4096)
	manualRows["manual/f.go"] = row
	requireCheckpointInventoryFullCharge(t, values)
	large := checkpointInventoryFinish(checkpointInventoryConvergence(t, values))
	require.GreaterOrEqual(t, large-small, int64(len(row.Errors)), "mixed unsealed values must still traverse current owned strings")
}
