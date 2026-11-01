package store_sqlite

// Cleanup causes are persisted in the existing journal's opaque JSON. Unknown
// causes never authorize removal of the user's tracking configuration.
const (
	CleanupCauseUntrack               = "untrack"
	CleanupCauseForget                = "forget"
	CleanupCauseReloadRemoved         = "reload_removed_from_config"
	CleanupCauseDemotion              = "demotion"
	CleanupCauseReconcileRemoval      = "reconcile_removal"
	CleanupCauseReconcileAvailability = "reconcile_availability"
)

// CleanupCauseAllowsConfigRemoval reports whether a cause authorizes untracking.
func CleanupCauseAllowsConfigRemoval(cause string) bool {
	switch cause {
	case CleanupCauseUntrack, CleanupCauseForget, CleanupCauseReloadRemoved, CleanupCauseDemotion:
		return true
	}
	return false
}
