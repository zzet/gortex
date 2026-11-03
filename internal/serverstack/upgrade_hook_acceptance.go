//go:build acceptance

package serverstack

import "github.com/zzet/gortex/internal/graph/store_sqlite"

// SetAcceptanceUpgradeHook binds one callback to this config's actual Open.
// No setter exists in an ordinary build.
func SetAcceptanceUpgradeHook(cfg *SharedServerConfig, callback func(string, int)) {
	if callback == nil {
		cfg.sqliteUpgradeOption = nil
		return
	}
	cfg.sqliteUpgradeOption = store_sqlite.WithAcceptanceUpgradeHook(callback)
}
