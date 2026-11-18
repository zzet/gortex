package indexer

import "reflect"

// These optional values own strings outside the shared proof-string pool.
// Charge their complete containers AND backing bytes; the string-excluding
// proof walk must omit them. This also makes optional admission independent
// from raw-unit continuity: a missed optimization cannot evict a fitting raw
// baseline merely by adding derived values.
func contractOptionalCheckpointSize(p *contractDeltaCache) int64 {
	if p == nil {
		return 0
	}
	total := int64(0)
	if p.names != nil {
		total = contractRetainedSize(reflect.ValueOf(p.names))
	}
	for _, unit := range p.units {
		if unit.bodyBindings != nil {
			total += contractRetainedSize(reflect.ValueOf(unit.bodyBindings))
		}
		if unit.providerSchemas != nil {
			total += contractRetainedSize(reflect.ValueOf(unit.providerSchemas))
		}
	}
	return total
}

func trimContractOptionalCheckpoint(p *contractDeltaCache, budget int64) {
	if p == nil || p.retainedBytes <= budget {
		return
	}
	if p.names != nil {
		p.retainedBytes -= contractRetainedSize(reflect.ValueOf(p.names))
		p.names = nil
	}
	if p.retainedBytes <= budget {
		return
	}
	for _, unit := range p.units {
		if unit.providerSchemas != nil {
			p.retainedBytes -= contractRetainedSize(reflect.ValueOf(unit.providerSchemas))
			unit.providerSchemas = nil
			unit.providerSchemaBytes = 0
		}
	}
	if p.retainedBytes <= budget {
		return
	}
	for _, unit := range p.units {
		if unit.bodyBindings != nil {
			p.retainedBytes -= contractRetainedSize(reflect.ValueOf(unit.bodyBindings))
			unit.bodyBindings = nil
			unit.bodyBindingBytes = 0
		}
	}
	// If the original raw/proof baseline itself exceeds the existing budget,
	// the original LRU admission still rejects it. No cap is raised here.
}
