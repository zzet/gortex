package store_sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileMasksForContextKeepsRequestedKeysGenerationAndModes(t *testing.T) {
	base := openMaskStore(t)
	top, other := base.AtGeneration(1), base.AtGeneration(2)
	var masks []FileMask
	var paths []string
	modes := []OwnershipMode{OwnershipReplace, OwnershipDelete, OwnershipContext}
	for i := 0; i < lookupChunkSize+37; i++ {
		path := fmt.Sprintf("repo/path-%05d.go", i)
		masks = append(masks, FileMask{RepoPrefix: maskTestRepo, FilePath: path, Mode: modes[i%len(modes)]})
		paths = append([]string{path}, paths...)
	}
	require.NoError(t, top.SetFileMasks(masks))
	require.NoError(t, other.SetFileMasks([]FileMask{{RepoPrefix: maskTestRepo, FilePath: paths[0], Mode: OwnershipDelete}}))
	require.NoError(t, top.SetFileMasks([]FileMask{{RepoPrefix: "foreign", FilePath: paths[0], Mode: OwnershipDelete}, {RepoPrefix: maskTestRepo, FilePath: "repo/not-requested.go", Mode: OwnershipDelete}}))
	paths = append(paths, paths[0], "repo/missing.go")
	before := append([]string(nil), paths...)
	got, err := top.FileMasksForContext(t.Context(), maskTestRepo, paths)
	require.NoError(t, err)
	require.Equal(t, masks, got, "all three modes survive across chunks; no duplicate, foreign, missing or unrequested rows")
	require.Equal(t, before, paths, "the caller's point set is immutable")
	for _, m := range got {
		mode, found, err := top.FileMaskFor(maskTestRepo, m.FilePath)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, m.Mode, mode)
	}
	otherRows, err := other.FileMasksForContext(t.Context(), maskTestRepo, paths)
	require.NoError(t, err)
	require.Equal(t, []FileMask{{RepoPrefix: maskTestRepo, FilePath: paths[0], Mode: OwnershipDelete}}, otherRows)
	rows, err := base.FileMasksForContext(t.Context(), maskTestRepo, paths)
	require.NoError(t, err)
	require.Empty(t, rows, "generation0 cannot inherit derived masks")
	rows, err = top.FileMasksForContext(t.Context(), maskTestRepo, nil)
	require.NoError(t, err)
	require.Empty(t, rows, "empty point set never becomes a full mask scan")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = top.FileMasksForContext(ctx, maskTestRepo, paths)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
}
