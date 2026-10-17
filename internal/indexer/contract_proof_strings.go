package indexer

import (
	"maps"
	"strings"
)

// The canonical map is job-local. Only unique owned string backing held by
// proof references survive checkpoint construction; unreferenced old
// pool strings disappear with the previous checkpoint on every return.
type contractProofStrings struct {
	values       map[string]string
	count        int
	backingBytes int64
}

func contractProofString(value string, pools ...*contractProofStrings) string {
	if len(pools) == 0 || pools[0] == nil {
		return strings.Clone(value)
	}
	if value == "" {
		return ""
	}
	p := pools[0]
	if found, ok := p.values[value]; ok {
		return found
	}
	if p.values == nil {
		p.values = make(map[string]string)
	}
	owned := strings.Clone(value)
	p.values[owned] = owned
	p.count++
	p.backingBytes += contractDeltaAllocation(int64(len(owned)))
	return owned
}
func (p *contractProofStrings) seal() (int, int64) {
	// Every interned string is already referenced by copied proof fields. The
	// canonical lookup table and any redundant roots must not survive the job.
	p.values = nil
	return p.count, p.backingBytes
}
func (p *contractProofStrings) identity(in contractDeltaIdentity) contractDeltaIdentity {
	return contractDeltaIdentity{selection: contractProofString(in.selection, p), config: contractProofString(in.config, p), modules: contractProofString(in.modules, p), receiptVersion: contractProofString(in.receiptVersion, p)}
}

func contractUnitSharesLedger(unit *contractUnit) bool {
	return unit.ledger != nil && maps.Equal(unit.owners, unit.ledger.owners) && maps.Equal(unit.keys, unit.ledger.keys)
}
