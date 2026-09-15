package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/zzet/gortex/internal/config"
)

// snapshotDedicatedBaseConfig freezes the effective index configuration and
// fingerprints the output context stamped by SparseGenerationBuilder. Callers
// must pass resolved context and must not mutate cfg concurrently with capture.
// The returned config owns every nested value; it borrows nothing from the
// ConfigManager's shallow GetRepoConfig result. Extractor/resolver versions and
// source-tree identity remain separate generation-identity inputs.
func snapshotDedicatedBaseConfig(
	cfg config.IndexConfig, repoPrefix, workspaceID, projectID string,
) (config.IndexConfig, string, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return config.IndexConfig{}, "", fmt.Errorf("encode dedicated base config: %w", err)
	}
	var owned config.IndexConfig
	if err := json.Unmarshal(encoded, &owned); err != nil {
		return config.IndexConfig{}, "", fmt.Errorf("decode dedicated base config snapshot: %w", err)
	}
	// A non-nil pointer to a nil slice is an explicit empty allow-list, not
	// the nil-pointer default. JSON null cannot preserve that distinction.
	// Also retain the caller's original order and duplicates in the snapshot.
	if cfg.FrameworkSynthesizers != nil {
		frameworks := slices.Clone(*cfg.FrameworkSynthesizers)
		owned.FrameworkSynthesizers = &frameworks
	}

	fingerprinted := owned
	if owned.FrameworkSynthesizers != nil {
		frameworks := slices.Clone(*owned.FrameworkSynthesizers)
		slices.Sort(frameworks)
		frameworks = slices.Compact(frameworks)
		if len(frameworks) == 0 {
			frameworks = []string{}
		}
		fingerprinted.FrameworkSynthesizers = &frameworks
	}
	identity := struct {
		Domain      string             `json:"domain"`
		Version     int                `json:"version"`
		RepoPrefix  string             `json:"repo_prefix"`
		WorkspaceID string             `json:"workspace_id"`
		ProjectID   string             `json:"project_id"`
		Index       config.IndexConfig `json:"index"`
	}{
		Domain:      "gortex.dedicated-base.config",
		Version:     1,
		RepoPrefix:  repoPrefix,
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Index:       fingerprinted,
	}
	encoded, err = json.Marshal(identity)
	if err != nil {
		return config.IndexConfig{}, "", fmt.Errorf("fingerprint dedicated base config: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return owned, hex.EncodeToString(sum[:]), nil
}
