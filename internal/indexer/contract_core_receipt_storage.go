package indexer

import (
	"encoding/json"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// Match the durable optional-analysis envelope. An accepted ordinary parse is
// never rejected solely because its contract receipt exceeds that envelope.
const contractCoreReceiptPayloadLimit = 8 << 20
const contractCoreReceiptKeyLimit = 4096
const contractCoreReceiptKeyBytes = 8192

func contractCoreStoredReceipt(repo, checkout string, change contractCoreInputChange) (graph.ContractBoundaryReceipt, error) {
	row := graph.ContractBoundaryReceipt{
		RepoPrefix: repo, CheckoutID: checkout, FilePath: change.FilePath,
		Version: contractBoundaryReceiptVersion, SourceFingerprint: change.SourceFingerprint,
		Deleted: change.Deleted, Scope: change.Delta.Scope,
	}
	if row.Deleted {
		row.SourceFingerprint = contractInputHash([]byte("deleted:" + row.FilePath))
	}
	if change.Current != nil {
		var err error
		row.Payload, err = json.Marshal(change.Current)
		if err != nil {
			return row, err
		}
		row.LookupKeys = appendUniqueSorted(nil, change.Current.LookupKeys...)
		for key := range change.Current.ProducedInputs {
			row.ProducedKeys = append(row.ProducedKeys, key)
		}
		for key := range change.Current.MatcherInputs {
			row.ProducedKeys = append(row.ProducedKeys, key)
		}
		row.ProducedKeys = appendUniqueSorted(nil, row.ProducedKeys...)
	}
	keysTooLarge := len(row.LookupKeys)+len(row.ProducedKeys) > contractCoreReceiptKeyLimit
	for _, keys := range [][]string{row.LookupKeys, row.ProducedKeys} {
		for _, key := range keys {
			keysTooLarge = keysTooLarge || key == "" || len(key) > contractCoreReceiptKeyBytes
		}
	}
	if change.Current == nil || len(row.Payload) > contractCoreReceiptPayloadLimit || keysTooLarge {
		reason := change.Uncertainty
		if reason == "" && !change.Deleted {
			reason = "contract_receipt_envelope_exceeded"
		}
		if reason != "" {
			// The optional-analysis envelope itself must stay bounded. Unknown
			// retains real source presence/removal without oversized group keys.
			row.Scope = graph.ContractWorkScope{Unknown: true, Deleted: row.Deleted, Causes: []string{reason}}
		}
		row.LookupKeys, row.ProducedKeys = nil, nil
		var err error
		row.Payload, err = json.Marshal(struct {
			Version, FilePath, Uncertainty string
			Deleted bool
		}{contractBoundaryReceiptVersion, row.FilePath, reason, row.Deleted})
		if err != nil {
			return row, err
		}
	}
	row.Fingerprint = contractInputHash(row.Payload)
	if row.FilePath == "" || row.SourceFingerprint == "" {
		return row, fmt.Errorf("contract core receipt: missing accepted source identity")
	}
	return row, nil
}

// A pending physical receipt retains at most one accepted predecessor. Unknown
// markers must remain unknown; decoding them as an empty record set would turn
// optional-analysis uncertainty into an invalid no-change certificate.
func contractCorePriorReceipt(row *graph.ContractBoundaryReceipt, known bool) (*contractBoundaryReceipt, bool, error) {
	if row == nil {
		return nil, known, nil
	}
	if !row.Accepted {
		if row.Previous == nil {
			return nil, false, nil
		}
		row = row.Previous
	}
	if row.Deleted {
		return nil, true, nil
	}
	var receipt contractBoundaryReceipt
	if err := json.Unmarshal(row.Payload, &receipt); err != nil {
		return nil, false, err
	}
	if receipt.Version != contractBoundaryReceiptVersion || receipt.FilePath != row.FilePath || receipt.Source != row.SourceFingerprint || receipt.Policy == "" {
		if row.Scope.Unknown {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("contract core receipt: accepted payload identity mismatch")
	}
	return &receipt, true, nil
}
