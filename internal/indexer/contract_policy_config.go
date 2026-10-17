package indexer

import "github.com/zzet/gortex/internal/config"

// DirtyChain chooses construction and evidence-scoping strategies, not the
// accepted contract contents. Keep the same exclusion as the generation's
// configuration identity; retain every other extraction-affecting setting.
func contractExtractionSettings(settings config.IndexConfig) config.IndexConfig {
	settings.DirtyChain = nil
	return settings
}
