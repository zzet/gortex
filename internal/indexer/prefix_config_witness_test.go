package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestPreparedPrefixConfigDoesNotPublishAcrossOwnerReplacement(t *testing.T) {
	owner, candidate := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	writeFile(t, filepath.Join(owner, ".gortex.yaml"), "workspace: owner\nexclude:\n  - owner-only\n")
	writeFile(t, filepath.Join(candidate, ".gortex.yaml"), "workspace: candidate\nexclude:\n  - rejected-only\n")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	cm.LoadWorkspaceConfig("src", owner)
	before, revision := cm.GetRepoConfig("src"), cm.Revision()
	entry := config.RepoEntry{Path: candidate, Name: "src"}
	// The configured-owner check is already an explicit preparation seam.
	// Replace the previously absent live owner between observation and final
	// in-memory publication: no hook runs under the admission lock.
	check := func(string, string) error {
		mi.mu.Lock()
		mi.repos["src"] = &RepoMetadata{RepoPrefix: "src", RootPath: owner}
		mi.mu.Unlock()
		return nil
	}
	_, err := mi.resolveTrackPrefixContext(t.Context(), &entry, candidate, nil, nil, check,
		time.Now().Add(prefixOwnershipTimeout), nil)
	require.ErrorIs(t, err, errPrefixOwnerChanged)
	require.Equal(t, before, cm.GetRepoConfig("src"))
	require.Equal(t, revision, cm.Revision(), "a stale preparation must not overwrite the owner's workspace or gitignore root")
}
