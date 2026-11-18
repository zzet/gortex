//go:build acceptance

package store_sqlite

// WithAcceptanceUpgradeHook adapts the existing private per-Open hook.
// It changes neither disk admission nor backup or migration ordering.
func WithAcceptanceUpgradeHook(callback func(string, int)) Option {
	if callback == nil {
		return withUpgradeHook(nil)
	}
	return withUpgradeHook(func(point upgradePoint, version int) { callback(string(point), version) })
}
