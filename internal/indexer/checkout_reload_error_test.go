package indexer

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
)

func TestCheckoutLifecycleReloadContinuesAfterCatalogReadError(t *testing.T) {
	for _, table := range []string{"dedicated_graphs", "tracking_intents"} {
		t.Run(table, func(t *testing.T) {
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			for _, name := range []string{"one", "two"} {
				result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: f.gitRepo(name), Name: name}, TrackSourceCLI)
				require.NoError(t, err)
				require.NoError(t, result.CatalogErr)
			}
			f.cm.Global().Repos = nil
			require.NoError(t, f.cm.Global().Save())
			// Keep the real catalog and fail just the selected read boundary.
			db, err := sql.Open("sqlite", f.dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			_, err = db.Exec("ALTER TABLE " + table + " RENAME TO unreadable_" + table)
			require.NoError(t, err)
			core, logs := observer.New(zap.WarnLevel)
			f.lc.logger = zap.New(core)
			result, err := f.lc.ApplyReload(t.Context())
			require.NoError(t, err, "one failed read must not abort the reload")
			assert.Zero(t, result.Removed)
			assert.Zero(t, result.Pending)
			warnings := logs.All()
			require.Len(t, warnings, 2, "every affected root must be visited, independent of map order")
			var prefixes []string
			for _, warning := range warnings {
				prefixes = append(prefixes, warning.ContextMap()["prefix"].(string))
				assert.Contains(t, warning.ContextMap()["error"], "no such table")
			}
			assert.ElementsMatch(t, []string{"one", "two"}, prefixes)
			assert.Len(t, f.mi.AllMetadata(), 2, "a failed read must not authorize removal")
		})
	}
}
