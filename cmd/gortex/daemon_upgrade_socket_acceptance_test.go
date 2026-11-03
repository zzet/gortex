//go:build acceptance && (linux || darwin)

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

// Reuse the real existing owned daemon/socket fixture. Every case releases
// normally; these are coupling controls, not SIGKILL recovery acceptance.
func TestDaemonUpgradeCheckpointMetadataAtActualSites(t *testing.T) {
	for _, site := range []string{"in_step:40", "in_step:42", "in_step:44", "index_recreation:44", "before_stamp:44"} {
		t.Run(site, func(t *testing.T) {
			t.Setenv("GORTEX_TEST_UPGRADE_CHECKPOINT_SITE", site)
			TestDaemonMigrationSocketKeepsMetadataAndScopedLoadingAvailable(t)
		})
	}
}

func migrationSocketAcceptanceBuild(t *testing.T, root string, logger *zap.Logger, observers ...store_sqlite.MigrationObserver) (*daemonState, error, bool) {
	site := os.Getenv("GORTEX_TEST_UPGRADE_CHECKPOINT_SITE")
	if site == "" {
		return nil, nil, false
	}
	state, err := buildDaemonStateWithUpgradeCheckpoint(logger, func(point string, version int) {
		if fmt.Sprintf("%s:%d", point, version) != site {
			return
		}
		require.NoError(t, os.WriteFile(filepath.Join(root, "migration-entered"), []byte(site+"\n"), 0o600))
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(root, "release-migration")); err == nil {
				break
			}
			require.True(t, time.Now().Before(deadline), "parent did not release actual selected upgrade hook")
			time.Sleep(5 * time.Millisecond)
		}
	}, observers...)
	return state, err, true
}
