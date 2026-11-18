package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
)

func TestPrefixCollisionWarnsOnceAcrossAdmissionSurfaces(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner := f.gitRepo("src")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: owner}, TrackSourceCLI)
	require.NoError(t, err)
	loser := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(loser, 0o755))
	core, logs := observer.New(zap.WarnLevel)
	f.mi.logger = zap.New(core)
	entry := config.RepoEntry{Path: loser}
	require.Error(t, f.lc.PreflightTrack(t.Context(), entry))
	_, err = f.lc.Register(t.Context(), entry, TrackSourceCLI)
	require.Error(t, err)
	_, err = f.mi.TrackRepoCtx(t.Context(), entry)
	require.Error(t, err)
	require.Error(t, f.lc.RecordImplicit(t.Context(), loser))
	warnings := logs.All()
	require.Len(t, warnings, 1)
	text := warnings[0].ContextMap()["error"]
	require.Contains(t, text, owner)
	require.Contains(t, text, loser)
	require.Contains(t, text, "use `gortex track --name")
	require.NotContains(t, text, "watcher already exists")
}
