package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// RecordFingerprintVersion identifies the canonical record encoding.
// Dependency, source and extractor versions must be validated separately;
// matching output records alone do not prove freshness.
const RecordFingerprintVersion = "contract-records-v1"

// RecordFingerprints separates output semantics from top-level source lines.
type RecordFingerprints struct {
	Semantic string
	Full     string
}

// FingerprintRecords preserves multiplicity, complete metadata, confidence,
// scope and owner identity. Only the top-level source line is excluded from
// Semantic; metadata fields, including nested schema/location fields, are
// retained because their meaning belongs to the individual extractor. The
// input slice and metadata are not changed. These hashes certify serialized
// output equality only, not dependency or source freshness.
func FingerprintRecords(records []Contract) (RecordFingerprints, error) {
	full := make([]string, 0, len(records))
	semantic := make([]string, 0, len(records))
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			return RecordFingerprints{}, err
		}
		full = append(full, string(encoded))
		record.Line = 0
		encoded, err = json.Marshal(record)
		if err != nil {
			return RecordFingerprints{}, err
		}
		semantic = append(semantic, string(encoded))
	}
	return RecordFingerprints{Semantic: fingerprintRecordRows(semantic), Full: fingerprintRecordRows(full)}, nil
}

func fingerprintRecordRows(rows []string) string {
	sort.Strings(rows)
	encoded, _ := json.Marshal(struct {
		Version string
		Rows    []string
	}{RecordFingerprintVersion, rows})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
