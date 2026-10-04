package indexer

import (
	"encoding/json"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestAsyncContractProducerRestagesLegacyGenerationIdentity(t *testing.T) {
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	legacy := extractorVersionsFingerprint()
	encoded, err := json.Marshal(extractorVersionsSnapshot())
	if err != nil || legacy != string(encoded) {
		t.Fatalf("disabled identity changed: %q err=%v", legacy, err)
	}
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "1")
	current := extractorVersionsFingerprint()
	if current == legacy || current != extractorVersionsFingerprint() {
		t.Fatal("enabled producer identity must be distinct and stable")
	}
	identity := GenerationIdentity{ExtractorVersions: current}
	old := store_sqlite.ViewGeneration{ExtractorVersions: legacy}
	if sameCommitLayerInputs(old, identity) {
		t.Fatal("legacy commit layer remained reusable under async producer")
	}
	old.ExtractorVersions = current
	if !sameCommitLayerInputs(old, identity) {
		t.Fatal("rebuilt layer with identical producer inputs was rejected")
	}
	versions := extractorVersionsSnapshot()
	_ = contractGenerationProducerVersions(versions)
	if _, mutated := versions[contractAsyncGenerationProducerKey]; mutated {
		t.Fatal("producer policy mutated ordinary extractor version snapshot")
	}
}
