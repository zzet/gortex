package indexer

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
)

const contractDeltaCacheBudget int64 = 64 << 20
const contractDeltaRefresh = 256

// contractDeltaIdentity binds a checkpoint to the selected chain and extraction
// policy. Selection describes stable actor/cohort and overlay selection rules,
// never physical generation IDs: publication/fold moves are diffable stamps.
type contractDeltaIdentity struct {
	selection, config, modules, receiptVersion string
}

// contractDeltaReceipt is syntax evidence, not work debt. Boundary excludes
// source-only changes; produced includes every producer key, including those
// from files that extract no contracts. Unknown syntax remains a local delta.
type contractDeltaReceipt struct {
	boundary       string
	producedDigest string
	producedKeys   []string
}

// Aggregate identity preserves every produced value change; only keys are
// required to reach old/new dependent queries, so per-key values need not live
// beyond selected receipt decoding.
func newContractDeltaReceipt(boundary string, produced map[string]string) contractDeltaReceipt {
	encoded, _ := json.Marshal(produced)
	keys := make(map[string]bool)
	for key := range produced {
		contractDeltaProducedKeys(keys, key)
	}
	return contractDeltaReceipt{boundary: boundary, producedDigest: contractInputHash(encoded), producedKeys: slices.Sorted(maps.Keys(keys))}
}

// contractDeltaCache contains only values owned by this actor. No evidence,
// reader, store, context, source closure or postpass result survives a job.
// Clock and unit rounds are one coordinate system and must travel together.
type contractDeltaCache struct {
	continuation             *contractFillContinuation
	identity                 contractDeltaIdentity
	units                    map[string]*contractUnit
	live, excluded, suspect  map[string]bool
	retryOwners              map[string]map[string]bool
	declarers                contractDeclarerIndex
	clock                    contractChangeClock
	contentAt                map[string]uint64
	maxContent               uint64
	contents                 map[string]string
	receipts                 map[string]contractDeltaReceipt
	ownerReaders, keyReaders map[string][]string
	// deltas counts completed delta jobs since the last cold/fill baseline.
	deltas   int
	complete bool
	// hasBaseline records a previously completed baseline, not current proof.
	hasBaseline      bool
	retainedBytes    int64
	proofStringBytes int64
	proofStringCount int
}

// checkpointContractDelta snapshots a proven round boundary. Complete is an
// additional publication prerequisite, never inferred from a unit count.
func checkpointContractDelta(c *contractConvergence, identity contractDeltaIdentity, receipts map[string]contractDeltaReceipt, deltas int, complete bool) *contractDeltaCache {
	for _, unit := range c.units {
		if unit == nil {
			continue
		}
		for _, raw := range unit.raw {
			// JSON also rejects cycles before the recursive shape check or clone.
			if _, err := json.Marshal(raw.Meta); err != nil || !contractDeltaMetaValue(reflect.ValueOf(raw.Meta)) {
				return nil
			}
		}
	}
	p := &contractDeltaCache{identity: contractDeltaIdentity{selection: strings.Clone(identity.selection), config: strings.Clone(identity.config), modules: strings.Clone(identity.modules), receiptVersion: strings.Clone(identity.receiptVersion)}, units: cloneContractUnits(c.units), live: cloneContractDeltaMap(c.live), excluded: cloneContractDeltaMap(c.excluded), suspect: cloneContractDeltaMap(c.suspect), retryOwners: cloneContractIndex(c.retryOwners), declarers: c.declarers.clone(), clock: cloneContractClock(c.clock), contentAt: cloneContractDeltaMap(c.contentAt), maxContent: c.maxContent, contents: cloneContractDeltaMap(c.contents), receipts: cloneContractDeltaReceipts(receipts), deltas: deltas, complete: complete}
	p.indexReaders()
	if len(p.suspect) != 0 {
		p.complete = false
	}
	for path := range p.live {
		if !p.excluded[path] {
			u := p.units[path]
			if u == nil || len(u.torn) != 0 || u.round > p.clock.clock {
				p.complete = false
			}
		}
	}
	p.hasBaseline = p.complete
	return p
}

// restore copies values into a fresh job. Round-local proof state, enrichment,
// seeds and the quiet fence are deliberately recreated by that job.
func (p *contractDeltaCache) restore(c *contractConvergence) {
	c.units = cloneContractUnits(p.units)
	c.live, c.excluded, c.suspect = cloneContractDeltaMap(p.live), cloneContractDeltaMap(p.excluded), cloneContractDeltaMap(p.suspect)
	c.retryOwners, c.declarers = cloneContractIndex(p.retryOwners), p.declarers.clone()
	c.clock, c.contentAt, c.maxContent, c.contents = cloneContractClock(p.clock), cloneContractDeltaMap(p.contentAt), p.maxContent, cloneContractDeltaMap(p.contents)
	c.quietFrom = 0
}

// Persistent metadata accepts owned scalar/container values only. A new
// extractor that supplies a pointer, closure or resource-bearing struct must
// convert it to values before its result may be retained across jobs.
func contractDeltaMetaValue(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Interface:
		return v.IsNil() || contractDeltaMetaValue(v.Elem())
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return false
		}
		iter := v.MapRange()
		for iter.Next() {
			if !contractDeltaMetaValue(iter.Value()) {
				return false
			}
		}
		return true
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if !contractDeltaMetaValue(v.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// Fresh maps avoid retaining historical bucket capacity after growth/deletion.
// Every string retained from a job owns exactly its visible bytes; header-only
// copies of substrings would invalidate length-based backing-storage charges.
func cloneContractDeltaMap[V any](in map[string]V, pool ...*contractProofStrings) map[string]V {
	if in == nil {
		return nil
	}
	out := make(map[string]V, len(in))
	for key, value := range in {
		switch v := any(value).(type) {
		case string:
			value = any(contractProofString(v, pool...)).(V)
		case ContractReceiptStamp:
			value = any(cloneContractDeltaStamp(v, pool...)).(V)
		}
		out[contractProofString(key, pool...)] = value
	}
	return out
}
func cloneContractDeltaStamp(s ContractReceiptStamp, pool ...*contractProofStrings) ContractReceiptStamp {
	s.Actor, s.Fingerprint, s.SourceFingerprint = contractProofString(s.Actor, pool...), contractProofString(s.Fingerprint, pool...), contractProofString(s.SourceFingerprint, pool...)
	return s
}
func cloneContractDeltaStrings(in []string, pool ...*contractProofStrings) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, value := range in {
		out[i] = contractProofString(value, pool...)
	}
	return out
}

func cloneContractDeltaRaw(in []contracts.Contract) []contracts.Contract {
	if in == nil {
		return nil
	}
	out := make([]contracts.Contract, len(in))
	for i, r := range in {
		r.ID, r.SymbolID, r.FilePath = strings.Clone(r.ID), strings.Clone(r.SymbolID), strings.Clone(r.FilePath)
		r.Type, r.Role = contracts.ContractType(strings.Clone(string(r.Type))), contracts.Role(strings.Clone(string(r.Role)))
		r.RepoPrefix, r.WorkspaceID, r.ProjectID = strings.Clone(r.RepoPrefix), strings.Clone(r.WorkspaceID), strings.Clone(r.ProjectID)
		if r.Meta != nil {
			r.Meta = cloneContractDeltaValue(reflect.ValueOf(r.Meta)).Interface().(map[string]any)
		}
		out[i] = r
	}
	return out
}

// Only admitted metadata reaches this copier: scalar values, slices and
// string-keyed maps. No struct or resource fields are traversed implicitly.
func cloneContractDeltaValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.String:
		out := reflect.New(v.Type()).Elem()
		out.SetString(strings.Clone(v.String()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(cloneContractDeltaValue(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(cloneContractDeltaValue(iter.Key()), cloneContractDeltaValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneContractDeltaValue(v.Index(i)))
		}
		return out

	}
	return v
}

func cloneContractClock(c contractChangeClock, pool ...*contractProofStrings) contractChangeClock {
	c.last, c.lastCensus = cloneContractDeltaMap(c.last, pool...), cloneContractDeltaMap(c.lastCensus, pool...)
	c.changedAt, c.seenAt = cloneContractDeltaMap(c.changedAt, pool...), cloneContractDeltaMap(c.seenAt, pool...)
	return c
}
func cloneContractIndex(in map[string]map[string]bool, pool ...*contractProofStrings) map[string]map[string]bool {
	if in == nil {
		return nil
	}
	out := make(map[string]map[string]bool, len(in))
	for key, paths := range in {
		out[contractProofString(key, pool...)] = cloneContractDeltaMap(paths, pool...)
	}
	return out
}
func cloneContractDeltaReceipts(in map[string]contractDeltaReceipt, pool ...*contractProofStrings) map[string]contractDeltaReceipt {
	out := make(map[string]contractDeltaReceipt, len(in))
	for path, r := range in {
		r.producedKeys = cloneContractDeltaStrings(r.producedKeys, pool...)
		r.producedDigest = contractProofString(r.producedDigest, pool...)
		r.boundary = contractProofString(r.boundary, pool...)
		out[contractProofString(path, pool...)] = r
	}
	return out
}
func cloneContractUnits(in map[string]*contractUnit) map[string]*contractUnit {
	return cloneContractUnitsMode(in, true)
}
func cloneContractUnitsMode(in map[string]*contractUnit, copyRaw bool, pool ...*contractProofStrings) map[string]*contractUnit {
	out := make(map[string]*contractUnit, len(in))
	for path, u := range in {
		if u == nil {
			continue
		}
		v := *u
		v.path = contractProofString(u.path, pool...)
		v.rawDigest = contractProofString(u.rawDigest, pool...)
		v.stamp = cloneContractDeltaStamp(u.stamp, pool...)
		if copyRaw {
			v.raw = cloneContractDeltaRaw(u.raw)
			if u.router != nil {
				facts := u.router.Clone()
				v.router = &facts
			}
		}
		v.owners, v.keys, v.torn = cloneContractDeltaMap(u.owners, pool...), cloneContractDeltaMap(u.keys, pool...), cloneContractDeltaMap(u.torn, pool...)
		v.declared = cloneContractDeltaStrings(u.declared, pool...)
		// Seeds always come from fresh core queries. Frozen test snapshots cannot
		// use persistent checkpoints as a substitute for their seed fixture.
		v.seedNodes, v.seedEdges = nil, nil
		if u.ledger != nil {
			l := *u.ledger
			if contractUnitSharesLedger(u) {
				l.owners, l.keys = v.owners, v.keys
				v.ledgerAliases = true
			} else {
				l.owners, l.keys = cloneContractDeltaMap(l.owners, pool...), cloneContractDeltaMap(l.keys, pool...)
				v.ledgerAliases = false
			}
			l.sources = cloneContractDeltaMap(l.sources, pool...)
			if len(pool) > 0 && pool[0] != nil {
				l.recorded = nil
			} else {
				l.recorded = cloneContractDeltaMap(l.recorded)
			}
			l.reads = make([]contractRecordedRead, len(u.ledger.reads))
			copy(l.reads, u.ledger.reads)
			for i := range l.reads {
				l.reads[i].kind = contractProofString(l.reads[i].kind, pool...)
				l.reads[i].digest = contractProofString(l.reads[i].digest, pool...)
				l.reads[i].args = cloneContractDeltaStrings(l.reads[i].args, pool...)
				l.reads[i].owners = cloneContractDeltaStrings(l.reads[i].owners, pool...)
			}

			v.ledger = &l
		}
		out[contractProofString(path, pool...)] = &v
	}
	return out
}

// contractDeltaPlan separates extraction from reader revalidation. A reader
// already extracted in this job is still a dependent: no set subtraction is
// sound for negative lookups that a newly introduced producer can answer.
type contractDeltaPlan struct {
	extract, revalidate, removed []string
	mode, reason                 string
}

func (p *contractDeltaCache) plan(identity contractDeltaIdentity, census map[string]bool, receipts map[string]contractDeltaReceipt, membershipUnknown bool) contractDeltaPlan {
	result := contractDeltaPlan{mode: "delta"}
	switch {
	case p == nil:
		result.mode, result.reason = "fill", "cache_missing"
	case p.identity != identity:
		result.mode, result.reason = "cold", "identity_changed"
	case membershipUnknown:
		result.mode, result.reason = "cold", "membership_unknown"
	case p.deltas+1 >= contractDeltaRefresh:
		result.mode, result.reason = "cold", "periodic_refresh"
	}
	extract, readers := make(map[string]bool), make(map[string]bool)
	if result.mode != "delta" {
		for path := range census {
			extract[path] = true
		}
	} else {
		if !p.complete && !p.hasBaseline {
			result.mode, result.reason = "fill", "coverage_incomplete"
		}
		moved, keys := make(map[string]bool), make(map[string]bool)
		for path := range p.live {
			if !census[path] {
				moved[path] = true
				result.removed = append(result.removed, path)
			}
		}
		for path := range census {
			old, had := p.receipts[path]
			now, has := receipts[path]
			if !p.live[path] || !had || !has || old.boundary != now.boundary || old.producedDigest != now.producedDigest || !slices.Equal(old.producedKeys, now.producedKeys) {
				moved[path] = true
				extract[path] = true
			}
			if p.units[path] == nil && !p.excluded[path] {
				extract[path] = true
			}
			if p.suspect[path] {
				readers[path] = true
			}
		}
		for path := range moved {
			for _, key := range p.receipts[path].producedKeys {
				keys[key] = true
			}
			for _, key := range receipts[path].producedKeys {
				keys[key] = true
			}
			for _, key := range p.unitsDeclared(path) {
				keys[key] = true
			}
		}
		for owner := range moved {
			for _, reader := range p.ownerReaders[owner] {
				if census[reader] {
					readers[reader] = true
				}
			}
		}
		for key := range keys {
			for _, reader := range p.keyReaders[key] {
				if census[reader] {
					readers[reader] = true
				}
			}
		}
	}
	// Module files must be inspected even when their receipt identity held.
	for path := range census {
		if base := filepath.Base(path); base == "go.mod" || base == "go.work" {
			extract[path] = true
		}
	}
	result.extract, result.revalidate = slices.Sorted(maps.Keys(extract)), slices.Sorted(maps.Keys(readers))
	slices.Sort(result.removed)
	return result
}

// indexReaders builds inverse indexes at checkpoint construction. Runtime
// integration should update these alongside changed units, so a small delta
// does not rebuild indexes for every unchanged unit.
func (p *contractDeltaCache) indexReaders() {
	p.ownerReaders, p.keyReaders = make(map[string][]string), make(map[string][]string)
	for reader, unit := range p.units {
		if unit == nil {
			continue
		}
		for owner := range unit.owners {
			p.ownerReaders[owner] = append(p.ownerReaders[owner], reader)
		}
		for key := range unit.keys {
			p.keyReaders[key] = append(p.keyReaders[key], reader)
		}
	}
	// These immutable postings own exact-capacity backing. A clipped slice
	// would retain its historical allocation while hiding it from accounting.
	for _, index := range []map[string][]string{p.ownerReaders, p.keyReaders} {
		for key, readers := range index {
			slices.Sort(readers)
			readers = slices.Compact(readers)
			owned := make([]string, len(readers))
			copy(owned, readers)
			index[key] = owned
		}
	}
}

// Receipt keys are namespace-qualified syntax keys; ledger keys describe the
// query actually asked, including cross-repository fallback. Conservatively
// map both forms, rather than relying on receipt LookupKeys for followup reads.
func contractDeltaProducedKeys(keys map[string]bool, key string) {
	var parts []string
	if json.Unmarshal([]byte(key), &parts) == nil && len(parts) == 3 {
		if strings.HasSuffix(parts[1], "_name") {
			keys["name:"+parts[2]] = true
			return
		}
		if strings.HasSuffix(parts[1], "_id") {
			keys["id:"+parts[2]] = true
			return
		}
	}
	keys[key] = true
}

func (p *contractDeltaCache) unitsDeclared(path string) []string {
	if u := p.units[path]; u != nil {
		return u.declared
	}
	return nil
}

// contractDeltaLRU is coordinator-owned and guarded by its mutex. Interest
// expiry never touches it. Each entry is one actor's checkpoint; pressure
// drops the entire checkpoint, so its next job fills coverage again.
type contractDeltaLRU struct {
	budget, bytes int64
	entries       []contractDeltaCacheEntry
}
type contractDeltaCacheEntry struct {
	actor    ContractActor
	cache    *contractDeltaCache
	bytes    int64
	borrowed bool
}

func (l *contractDeltaLRU) get(actor ContractActor) *contractDeltaCache {
	for i, entry := range l.entries {
		if entry.actor == actor && !entry.borrowed {
			l.entries = append(slices.Delete(l.entries, i, i+1), entry)
			return entry.cache
		}
	}
	return nil
}

// remove releases array capacity as well as clearing discarded referents.
func (l *contractDeltaLRU) remove(index int) {
	old := l.entries
	l.entries = make([]contractDeltaCacheEntry, len(old)-1)
	copy(l.entries, old[:index])
	copy(l.entries[index:], old[index+1:])
	clear(old)
	if len(l.entries) == 0 {
		l.entries = nil
	}
	l.recount()
}
func (l *contractDeltaLRU) recount() {
	l.bytes = contractDeltaAllocation(int64(cap(l.entries)) * int64(reflect.TypeOf(contractDeltaCacheEntry{}).Size()))
	for _, entry := range l.entries {
		l.bytes += entry.bytes
	}
}

type contractDeltaAdmission struct {
	reason                      string
	requested, budget, retained int64
	evicted                     int
}

func (l *contractDeltaLRU) put(actor ContractActor, p *contractDeltaCache) (result contractDeltaAdmission) {
	result.budget = l.budget
	if result.budget <= 0 {
		result.budget = contractDeltaCacheBudget
	}
	defer func() { result.retained = l.bytes }()
	for i, entry := range l.entries {
		if entry.actor == actor {
			l.remove(i)
			break
		}
	}
	if p == nil {
		result.reason = "discarded"
		return
	}
	actor.Checkout, actor.cohort = strings.Clone(actor.Checkout), strings.Clone(actor.cohort)
	retained := p.retainedBytes
	if retained == 0 {
		retained = contractRetainedSize(reflect.ValueOf(p))
	}
	size := retained + 256 + contractDeltaAllocation(int64(len(actor.Checkout))) + contractDeltaAllocation(int64(len(actor.cohort)))
	entrySize := int64(reflect.TypeOf(contractDeltaCacheEntry{}).Size())
	arrayCharge := func(n int) int64 { return contractDeltaAllocation(int64(n) * entrySize) }
	budget := result.budget
	result.requested = size + arrayCharge(1)
	if size+arrayCharge(1) > budget {
		result.reason = "over_budget"
		return
	}
	for len(l.entries) > 0 && l.bytes+size+arrayCharge(len(l.entries)+1)-arrayCharge(cap(l.entries)) > budget {
		victim := -1
		for i, entry := range l.entries {
			if !entry.borrowed {
				victim = i
				break
			}
		}
		if victim < 0 {
			result.reason = "pinned_pressure"
			return
		}
		l.remove(victim)
		result.evicted++
	}
	// Exact capacity prevents historical growth from surviving actor removal.
	entries := make([]contractDeltaCacheEntry, len(l.entries)+1)
	copy(entries, l.entries)
	clear(l.entries)
	entries[len(entries)-1] = contractDeltaCacheEntry{actor: actor, cache: p, bytes: size}
	l.entries = entries
	l.recount()
	result.reason = "admitted"
	return
}

// Count backing storage conservatively: repeated references count repeatedly,
// and map entries include bucket/allocator overhead. This admits less than the
// budget rather than hiding raw metadata and recorded query argument storage.
func contractRetainedSize(v reflect.Value) int64 { return contractRetainedSizeMode(v, true) }

// stringBacking=false is used only after every proof string was canonicalized
// into the checkpoint's owned pool. That pool charges each backing allocation
// once; headers, map tables and exact-capacity containers remain charged here.
func contractRetainedSizeMode(v reflect.Value, stringBacking bool) int64 {
	if !v.IsValid() {
		return 0
	}
	size := int64(v.Type().Size())
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			size += contractRetainedSizeMode(v.Elem(), stringBacking)
			if v.Kind() == reflect.Pointer {
				size += contractDeltaAllocation(int64(v.Type().Elem().Size())) - int64(v.Type().Elem().Size())
			}
		}
	case reflect.String:
		if stringBacking {
			size += contractDeltaAllocation(int64(v.Len()))
		}
	case reflect.Slice:
		size += contractDeltaAllocation(int64(v.Cap()) * int64(v.Type().Elem().Size()))
		for i := 0; i < v.Len(); i++ {
			size += contractRetainedSizeMode(v.Index(i), stringBacking)
		}
	case reflect.Map:
		if v.IsNil() {
			return size
		}
		// Fresh len-sized tables can still allocate an eight-slot bucket for a
		// single entry. Charge spare slots, table headers and allocator rounding,
		// rather than pretending map length is its allocated capacity.
		slots := max(int64(8), contractDeltaAllocation(int64(v.Len())*2))
		size += 512 + contractDeltaAllocation(slots*(int64(v.Type().Key().Size())+int64(v.Type().Elem().Size())+32))
		iter := v.MapRange()
		for iter.Next() {
			size += 64 + contractRetainedSizeMode(iter.Key(), stringBacking) + contractRetainedSizeMode(iter.Value(), stringBacking)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			size += contractRetainedSizeMode(v.Field(i), stringBacking)
		}
	}
	return size
}

// Power-of-two allocation charges deliberately overestimate allocator size
// classes and page rounding, including tiny strings cloned out of substrings.
func contractDeltaAllocation(n int64) int64 {
	if n <= 0 {
		return 0
	}
	charge := int64(16)
	for charge < n {
		charge *= 2
	}
	return charge
}

// borrow transfers exclusive custody while preserving its retained charge.
// New extraction is job-local working state until a proven checkpoint returns.
func (l *contractDeltaLRU) borrow(actor ContractActor) *contractDeltaCache {
	for i := range l.entries {
		entry := &l.entries[i]
		if entry.actor == actor && !entry.borrowed {
			p := entry.cache
			entry.cache = nil
			entry.borrowed = true
			return p
		}
	}
	return nil
}
