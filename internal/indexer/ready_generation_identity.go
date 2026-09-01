package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// commitLayerPipelineEpoch is the content-affecting pipeline identity shared by
// every producer and consumer of an immutable commit layer. Bump it whenever
// the same tree/config/extractor tuple could produce different graph payload.
const commitLayerPipelineEpoch = "1"

const commitLayerSourceSnapshotCapability = "source.snapshot"

func commitLayerReadyGenerationKey(
	graphID string,
	baseGenerationID int64,
	treeOID string,
	indexConfigHash string,
	extractorFingerprint string,
) store_sqlite.ReadyGenerationCacheKey {
	return store_sqlite.ReadyGenerationCacheKey{
		GraphID:              graphID,
		BaseGenerationID:     baseGenerationID,
		TreeOID:              treeOID,
		IndexConfigHash:      indexConfigHash,
		ExtractorFingerprint: extractorFingerprint,
		SchemaPipelineEpoch:  commitLayerPipelineEpoch,
	}
}

func commitLayerRequiredCapabilities() []string {
	return []string{commitLayerSourceSnapshotCapability}
}

func commitLayerReadyGenerationKeyFromIdentity(identity GenerationIdentity) store_sqlite.ReadyGenerationCacheKey {
	return commitLayerReadyGenerationKey(identity.GraphID, identity.BaseGenerationID, identity.TreeOID, identity.ConfigHash, identity.ExtractorVersions)
}

// commitLayerReadyGenerationKeyFromRow projects a stored generation onto the
// owner-independent identity used for cross-checkout and ref-view reuse. Keep
// the stored pipeline epoch instead of substituting the current one: callers
// compare this key with the current expected key and must reject generations
// produced by an older pipeline.
func commitLayerReadyGenerationKeyFromRow(row store_sqlite.ViewGeneration) store_sqlite.ReadyGenerationCacheKey {
	return store_sqlite.ReadyGenerationCacheKey{
		GraphID:              row.GraphID,
		BaseGenerationID:     row.BaseGenerationID,
		TreeOID:              row.TreeOID,
		IndexConfigHash:      row.ConfigHash,
		ExtractorFingerprint: row.ExtractorVersions,
		SchemaPipelineEpoch:  row.ResolverVersion,
	}
}

func commitLayerRouteFingerprint(identity GenerationIdentity, profile string) string {
	key := commitLayerReadyGenerationKeyFromIdentity(identity)
	raw := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s",
		key.GraphID, key.BaseGenerationID, key.TreeOID, key.IndexConfigHash,
		key.ExtractorFingerprint, key.SchemaPipelineEpoch, profile)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}
