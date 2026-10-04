package indexer

import "os"

const contractAsyncGenerationProducerKey = "__contract_async_producer"

// contractGenerationProducerVersions changes the existing background generation
// policy identity when the async producer is installed. Sealed legacy positives
// are rebuilt by ordinary identity reconciliation; no receipts are invented in
// those payloads. The diagnostic switch must agree with daemon installation.
func contractGenerationProducerVersions(versions map[string]int) map[string]int {
	if os.Getenv("GORTEX_ASYNC_CONTRACTS") != "1" {
		return versions
	}
	result := make(map[string]int, len(versions)+1)
	for key, version := range versions {
		result[key] = version
	}
	result[contractAsyncGenerationProducerKey] = 1
	return result
}
