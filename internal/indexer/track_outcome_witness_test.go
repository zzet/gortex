package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestTrackOutcomeWitnessRejectsOwnerAndConfigReplacement(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner := f.gitRepo("owner")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: owner, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	witness := f.mi.CaptureTrackOutcome("src", owner)
	require.True(t, f.mi.TrackOutcomeCurrent(witness))
	f.mi.mu.Lock()
	original := f.mi.repos["src"]
	f.mi.repos["src"] = &RepoMetadata{RootPath: owner, RepoPrefix: "src"}
	f.mi.mu.Unlock()
	require.False(t, f.mi.TrackOutcomeCurrent(witness), "same-root owner replacement still invalidates the outcome")
	f.mi.mu.Lock()
	f.mi.repos["src"] = original
	f.mi.mu.Unlock()
	require.True(t, f.mi.TrackOutcomeCurrent(witness))
	f.mi.DeclareRepos(append(f.cm.Global().ReposSnapshot(), config.RepoEntry{Path: t.TempDir()}))
	require.False(t, f.mi.TrackOutcomeCurrent(witness), "new alias identity publication invalidates a pending outcome")
	witness = f.mi.CaptureTrackOutcome("src", owner)
	require.NoError(t, f.cm.Global().AddRepo(config.RepoEntry{Path: t.TempDir(), Name: "next"}))
	require.False(t, f.mi.TrackOutcomeCurrent(witness), "a changed configuration requires fresh admission")
}
