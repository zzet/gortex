package indexer

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Reuse only the current accepted parser receipt, never a pending row's
// predecessor. The caller still reads every accepted file, checks its closed
// census, compares the exact prior rows, and commits namespace certification.
// Scope.Unknown can describe historical change discovery without invalidating
// a complete Current payload; uncertainty envelopes cannot pass these checks.
func reuseContractBaselineCurrentReceipt(idx *Indexer, file ContractFollowupFile, accepted ContractAcceptedSource, row *graph.ContractBoundaryReceipt) (contractBoundaryReceipt, bool) {
	var receipt contractBoundaryReceipt
	if row == nil || !row.Accepted || row.Deleted || row.RepoPrefix != file.RepoPrefix || row.CheckoutID != "" || row.FilePath != file.Path || row.Version != contractBoundaryReceiptVersion || row.Fingerprint != contractInputHash(row.Payload) || row.SourceFingerprint != accepted.SourceFingerprint || accepted.SourceFingerprint != contractInputHash(accepted.Bytes) {
		return receipt, false
	}
	if err := json.Unmarshal(row.Payload, &receipt); err != nil || receipt.Version != contractBoundaryReceiptVersion || receipt.FilePath != file.Path || receipt.Language != file.Language || receipt.Source != accepted.SourceFingerprint || receipt.Records.Semantic == "" || receipt.Records.Full == "" {
		return contractBoundaryReceipt{}, false
	}
	projected, err := idx.contractFollowupPolicyMode(file.Language, receipt.Policy)
	if err != nil {
		return contractBoundaryReceipt{}, false
	}
	if projected {
		rel := strings.TrimPrefix(file.Path, file.RepoPrefix+"/")
		if _, recognized := generatedTreeSitterParserProjection(rel, file.Language, accepted.Bytes); !recognized {
			return contractBoundaryReceipt{}, false
		}
	}
	// Confirm the complete envelope also owns the dependency memberships used
	// by storage. This does not reconstruct parser facts from enriched core rows.
	current, err := contractCoreStoredReceipt(file.RepoPrefix, "", contractCoreInputChange{FilePath: file.Path, Current: &receipt, SourceFingerprint: accepted.SourceFingerprint})
	if err != nil || current.Scope.Unknown || !reflect.DeepEqual(current.LookupKeys, row.LookupKeys) || !reflect.DeepEqual(current.ProducedKeys, row.ProducedKeys) {
		return contractBoundaryReceipt{}, false
	}
	return receipt, true
}
