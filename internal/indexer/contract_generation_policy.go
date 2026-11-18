package indexer

import "encoding/json"

const contractAsyncGenerationProducerKey = "__contract_async_producer"

// contractGenerationProducerVersions changes the existing background generation
// policy identity only when the actual builder has the async producer installed.
// A process-level switch alone cannot certify an embedded or standalone builder.
// Sealed legacy positives are rebuilt by ordinary identity reconciliation; no
// receipts are invented in those payloads.
func contractGenerationProducerVersions(versions map[string]int, installed bool) map[string]int {
	if !installed {
		return versions
	}
	result := make(map[string]int, len(versions)+1)
	for key, version := range versions {
		result[key] = version
	}
	result[contractAsyncGenerationProducerKey] = 1
	return result
}

// A generation advertises contract input receipts only when its producer owns
// the installed runtime. This is independent of daemon process configuration.
func (b *SparseGenerationBuilder) extractorVersionsFingerprint() string {
	installed := b != nil && b.contractCoreRuntime != nil
	encoded, err := json.Marshal(contractGenerationProducerVersions(extractorVersionsSnapshot(), installed))
	if err != nil {
		return ""
	}
	return string(encoded)
}
