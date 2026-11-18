package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractPointStampClaimsMatchOnlyRequestedFullMasks(t *testing.T) {
	store := newFTSStore(t)
	top := store.AtGeneration(1)
	require.NoError(t, top.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/replaced.go", Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: "repo", FilePath: "repo/deleted.go", Mode: store_sqlite.OwnershipDelete},
		{RepoPrefix: "repo", FilePath: "repo/context.go", Mode: store_sqlite.OwnershipContext},
		{RepoPrefix: "repo", FilePath: "repo/not-asked.go", Mode: store_sqlite.OwnershipDelete},
		{RepoPrefix: "other", FilePath: "repo/missing.go", Mode: store_sqlite.OwnershipDelete},
	}))
	wanted := []string{"repo/replaced.go", "repo/deleted.go", "repo/context.go", "repo/missing.go", "repo/replaced.go"}
	hidden := map[string]bool{}
	require.NoError(t, (contractScanLayer{handle: top}).claims(t.Context(), "repo", wanted, false, hidden))
	require.Equal(t, map[string]bool{"repo/replaced.go": true, "repo/deleted.go": true}, hidden)
	full := map[string]bool{}
	require.NoError(t, (contractScanLayer{handle: top}).claims(t.Context(), "repo", nil, true, full))
	require.Equal(t, map[string]bool{"repo/replaced.go": true, "repo/deleted.go": true, "repo/not-asked.go": true}, full)
}

// Pre-cancelled point scans must not enter SQLite at all. A closed handle
// distinguishes cancellation admission from a later SQL error without sleeps
// or a throughput/timing assertion; this uses the pre-existing claims API.
func TestContractPointStampClaimsHonorCancellationBeforeSQLite(t *testing.T) {
	store := newFTSStore(t)
	top := store.AtGeneration(1)
	require.NoError(t, store.Close())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	hidden := map[string]bool{}
	err := (contractScanLayer{handle: top}).claims(ctx, "repo", []string{"repo/file.go"}, false, hidden)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, hidden)
}
