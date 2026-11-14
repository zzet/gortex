package indexer

import "github.com/zzet/gortex/internal/config"

// Share these exclusions between contract policy and generation identity.
// DirtyChain and Workers choose construction strategies, not accepted contents.
// Worker counts can change with CPU affinity between daemon starts; retain
// every extraction-affecting setting in both identities.
func contractExtractionSettings(settings config.IndexConfig) config.IndexConfig {
	settings.DirtyChain = nil
	settings.Workers = 0
	return settings
}
