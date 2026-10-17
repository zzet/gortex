package indexer

import (
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
)

func TestContractDeltaCheckpointOwnsRawAndProofClock(t *testing.T) {
	c := &contractConvergence{
		units:     map[string]*contractUnit{"empty": {path: "empty", round: 9}, "route": {path: "route", round: 9, raw: []contracts.Contract{{Meta: map[string]any{"nested": map[string]any{"value": "before"}}}}, ledger: &contractUnitLedger{reads: []contractRecordedRead{{args: []string{"lookup"}, owners: []string{"empty"}}}}}},
		live:      map[string]bool{"empty": true, "route": true},
		clock:     contractChangeClock{clock: 9, last: map[string]ContractReceiptStamp{"route": {Seq: 4}}, changedAt: map[string]uint64{"empty": 8}},
		contentAt: map[string]uint64{"empty": 8}, maxContent: 8,
	}
	p := checkpointContractDelta(c, contractDeltaIdentity{}, nil, 1, true)
	require.True(t, p.complete)
	c.units["route"].raw[0].Meta["nested"].(map[string]any)["value"] = "changed"
	c.units["route"].ledger.reads[0].args[0] = "changed"
	c.clock.changedAt["empty"] = 99
	restored := &contractConvergence{quietFrom: 100}
	p.restore(restored)
	require.Equal(t, uint64(9), restored.clock.clock)
	require.Equal(t, uint64(8), restored.clock.changedAt["empty"])
	require.Equal(t, "before", restored.units["route"].raw[0].Meta["nested"].(map[string]any)["value"])
	require.Equal(t, "lookup", restored.units["route"].ledger.reads[0].args[0])
	require.NotNil(t, restored.units["empty"])
	require.Zero(t, restored.quietFrom)
	restored.units["route"].ledger.reads[0].owners[0] = "changed"
	require.Equal(t, "empty", p.units["route"].ledger.reads[0].owners[0])
}

func TestContractDeltaPlanFindsProducerAndDepartedOwnerReaders(t *testing.T) {
	identity := contractDeltaIdentity{selection: "chain", receiptVersion: "v"}
	old := map[string]contractDeltaReceipt{"reader": {boundary: "r"}, "negative": {boundary: "n"}, "owner": {boundary: "o"}}
	p := &contractDeltaCache{identity: identity, complete: true, live: map[string]bool{"reader": true, "negative": true, "owner": true}, receipts: old, units: map[string]*contractUnit{
		"reader": {owners: map[string]bool{"owner": true}}, "negative": {keys: map[string]bool{"name:Missing": true}}, "owner": {},
	}}
	p.indexReaders()
	now := cloneContractDeltaReceipts(old)
	delete(now, "owner")
	now["producer"] = newContractDeltaReceipt("new", map[string]string{`["repo","symbol_name","Missing"]`: "digest"})
	now["go.mod"] = contractDeltaReceipt{boundary: "module"}
	census := map[string]bool{"reader": true, "negative": true, "producer": true, "go.mod": true}
	plan := p.plan(identity, census, now, false)
	require.Equal(t, "delta", plan.mode)
	require.Equal(t, []string{"go.mod", "producer"}, plan.extract)
	require.Equal(t, []string{"negative", "reader"}, plan.revalidate)
	require.Equal(t, []string{"owner"}, plan.removed)
	// Even an extracted reader remains in the proof queue after its producer
	// moves. Its negative read may precede extraction of that producer.
	now["negative"] = contractDeltaReceipt{boundary: "changed"}
	require.Contains(t, p.plan(identity, census, now, false).revalidate, "negative")
}

func TestContractDeltaPlanResumesMissingCoverageAndResetsIdentity(t *testing.T) {
	identity := contractDeltaIdentity{selection: "one", config: "config", modules: "module", receiptVersion: "v"}
	receipts := map[string]contractDeltaReceipt{"a": {boundary: "a"}, "b": {boundary: "b"}}
	p := &contractDeltaCache{identity: identity, live: map[string]bool{"a": true, "b": true}, receipts: receipts, units: map[string]*contractUnit{"a": {}}}
	census := map[string]bool{"a": true, "b": true}
	plan := p.plan(identity, census, receipts, false)
	require.Equal(t, "fill", plan.mode)
	require.Equal(t, []string{"b"}, plan.extract)
	for _, changed := range []contractDeltaIdentity{{selection: "two", config: "config", modules: "module", receiptVersion: "v"}, {selection: "one", config: "changed", modules: "module", receiptVersion: "v"}, {selection: "one", config: "config", modules: "changed", receiptVersion: "v"}, {selection: "one", config: "config", modules: "module", receiptVersion: "new"}} {
		require.Equal(t, "identity_changed", p.plan(changed, census, receipts, false).reason)
	}
	require.Equal(t, "membership_unknown", p.plan(identity, census, receipts, true).reason)
	p.deltas = 254
	require.Equal(t, "fill", p.plan(identity, census, receipts, false).mode)
	p.complete = true
	p.units["b"] = &contractUnit{}
	p.deltas = 255
	require.Equal(t, "periodic_refresh", p.plan(identity, census, receipts, false).reason)
	p.deltas = 256
	require.Equal(t, "periodic_refresh", p.plan(identity, census, receipts, false).reason)
}

func TestContractDeltaCheckpointCannotCertifyPartialOrTornCoverage(t *testing.T) {
	for _, units := range []map[string]*contractUnit{nil, {"a": {round: 1, torn: map[string]ContractReceiptStamp{"owner": {}}}}, {"a": {round: 2}}} {
		c := &contractConvergence{live: map[string]bool{"a": true}, units: units, clock: contractChangeClock{clock: 1}}
		require.False(t, checkpointContractDelta(c, contractDeltaIdentity{}, nil, 0, true).complete)
	}
}

func TestContractDeltaLRUEvictsActorsWithinSharedBudget(t *testing.T) {
	first, second, third := NewContractActor("", []string{"a"}), NewContractActor("", []string{"b"}), NewContractActor("", []string{"c"})
	p := &contractDeltaCache{live: map[string]bool{"empty": true}, units: map[string]*contractUnit{"empty": {}}}
	l := &contractDeltaLRU{}
	l.put(first, p)
	size := l.bytes
	require.Positive(t, size)
	l.budget = 2 * size
	l.put(second, p)
	require.Same(t, p, l.get(first))
	backing := l.entries[:cap(l.entries)]
	l.put(third, p)
	for _, entry := range backing {
		require.NotEqual(t, second, entry.actor)
	}
	require.Nil(t, l.get(second))
	require.Same(t, p, l.get(first))
	require.Same(t, p, l.get(third))
	require.LessOrEqual(t, l.bytes, l.budget)
	l.put(first, nil)
	require.Nil(t, l.get(first))
	l.budget = size - 1
	backing = l.entries[:cap(l.entries)]
	l.put(third, p)
	require.Nil(t, backing[0].cache)
	require.Nil(t, l.get(third))
}

func TestContractDeltaCheckpointRejectsResourceMetadata(t *testing.T) {
	resource := &struct{ Value string }{Value: "resource"}
	c := &contractConvergence{units: map[string]*contractUnit{"a": {raw: []contracts.Contract{{Meta: map[string]any{"resource": resource}}}}}}
	require.Nil(t, checkpointContractDelta(c, contractDeltaIdentity{}, nil, 0, true))
}

func TestContractDeltaProducedChangeInvalidatesNegativeLookupWithoutBoundaryMove(t *testing.T) {
	identity := contractDeltaIdentity{}
	producer := newContractDeltaReceipt("same", map[string]string{`["repo","constant_id","id"]`: "old"})
	p := &contractDeltaCache{identity: identity, complete: true, live: map[string]bool{"producer": true, "reader": true}, receipts: map[string]contractDeltaReceipt{"producer": producer, "reader": {boundary: "reader"}}, units: map[string]*contractUnit{"producer": {}, "reader": {keys: map[string]bool{"id:id": true}}}}
	p.indexReaders()
	now := cloneContractDeltaReceipts(p.receipts)
	now["producer"] = newContractDeltaReceipt("same", map[string]string{`["repo","constant_id","id"]`: "new"})
	plan := p.plan(identity, p.live, now, false)
	require.Equal(t, []string{"producer"}, plan.extract)
	require.Equal(t, []string{"reader"}, plan.revalidate)
	now["producer"] = newContractDeltaReceipt("same", nil)
	require.Equal(t, []string{"reader"}, p.plan(identity, p.live, now, false).revalidate)
}

func TestContractDeltaOwnerRenameUsesInverseReaders(t *testing.T) {
	old := map[string]contractDeltaReceipt{"owner": {boundary: "B"}, "reader": {boundary: "reader"}, "unrelated": {boundary: "other"}}
	p := &contractDeltaCache{complete: true, live: map[string]bool{"owner": true, "reader": true, "unrelated": true}, receipts: old, units: map[string]*contractUnit{
		"owner":     {declared: []string{"name:B"}},
		"reader":    {owners: map[string]bool{"owner": true}, keys: map[string]bool{"name:B": true}},
		"unrelated": {keys: map[string]bool{"name:Other": true}},
	}}
	p.indexReaders()
	now := cloneContractDeltaReceipts(old)
	now["owner"] = newContractDeltaReceipt("B2", map[string]string{`["repo","symbol_name","B2"]`: "new"})
	plan := p.plan(contractDeltaIdentity{}, p.live, now, false)
	require.Equal(t, []string{"owner"}, plan.extract)
	require.Equal(t, []string{"reader"}, plan.revalidate)
	// Rebuilding an index after a reader changes must remove its former edge.
	p.units["reader"].owners = nil
	p.units["reader"].keys = map[string]bool{"name:Other": true}
	p.indexReaders()
	require.Empty(t, p.plan(contractDeltaIdentity{}, p.live, now, false).revalidate)
}

func TestContractDeltaCheckpointOwnsSubstringBacking(t *testing.T) {
	// Pointer identity is inspected only in this test. Production uses the
	// strings.Clone ownership contract and never inspects allocation addresses.
	backing := strings.Repeat("x", 4096)
	short := backing[:1]
	stamp := ContractReceiptStamp{Actor: short, Fingerprint: short, SourceFingerprint: short}
	c := &contractConvergence{units: map[string]*contractUnit{short: {path: short, stamp: stamp, rawDigest: short, raw: []contracts.Contract{{ID: short, Type: contracts.ContractType(short), Role: contracts.Role(short), SymbolID: short, FilePath: short, RepoPrefix: short, WorkspaceID: short, ProjectID: short, Meta: map[string]any{short: []any{short}}}}, owners: map[string]bool{short: true}, keys: map[string]bool{short: true}, declared: []string{short}, torn: map[string]ContractReceiptStamp{short: stamp}, ledger: &contractUnitLedger{sources: map[string]string{short: short}, reads: []contractRecordedRead{{kind: short, digest: short, args: []string{short}, owners: []string{short}}}}}}, live: map[string]bool{short: true}, clock: contractChangeClock{last: map[string]ContractReceiptStamp{short: stamp}}, contents: map[string]string{short: short}}
	receipts := map[string]contractDeltaReceipt{short: newContractDeltaReceipt(short, map[string]string{short: short})}
	p := checkpointContractDelta(c, contractDeltaIdentity{selection: short, config: short, modules: short, receiptVersion: short}, receipts, 0, false)
	require.NotNil(t, p)
	checked := 0
	var inspect func(reflect.Value)
	inspect = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			if v.String() == short {
				require.False(t, unsafe.StringData(short) == unsafe.StringData(v.String()))
				checked++
			}
		case reflect.Interface, reflect.Pointer:
			if !v.IsNil() {
				inspect(v.Elem())
			}
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				inspect(iter.Key())
				inspect(iter.Value())
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				inspect(v.Index(i))
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				inspect(v.Field(i))
			}
		}
	}
	inspect(reflect.ValueOf(p))
	require.Greater(t, checked, 30)
	l := &contractDeltaLRU{}
	l.put(ContractActor{Checkout: short, cohort: short}, p)
	require.False(t, unsafe.StringData(short) == unsafe.StringData(l.entries[0].actor.Checkout))
	require.False(t, unsafe.StringData(short) == unsafe.StringData(l.entries[0].actor.cohort))
}

func TestContractDeltaLRUChargesAndReleasesEntryCapacity(t *testing.T) {
	l := &contractDeltaLRU{}
	p := &contractDeltaCache{}
	actors := []ContractActor{NewContractActor("", []string{"a"}), NewContractActor("", []string{"b"}), NewContractActor("", []string{"c"})}
	for _, actor := range actors {
		l.put(actor, p)
	}
	for _, actor := range actors {
		require.Equal(t, len(l.entries), cap(l.entries))
		payloads := int64(0)
		for _, entry := range l.entries {
			payloads += entry.bytes
		}
		require.Equal(t, payloads+contractDeltaAllocation(int64(cap(l.entries))*int64(reflect.TypeOf(contractDeltaCacheEntry{}).Size())), l.bytes)
		l.put(actor, nil)
	}
	require.Zero(t, l.bytes)
	require.Nil(t, l.entries)
}

func TestContractDeltaCheckpointNormalizesSparseMapsAndSliceCapacity(t *testing.T) {
	sparse := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		sparse[strings.Repeat("x", i+1)] = true
	}
	for key := range sparse {
		delete(sparse, key)
	}
	sparse["reader"] = true
	args := make([]string, 1, 64)
	args[0] = "query"
	c := &contractConvergence{units: map[string]*contractUnit{"reader": {owners: sparse, ledger: &contractUnitLedger{reads: []contractRecordedRead{{args: args}}}}}, live: sparse}
	p := checkpointContractDelta(c, contractDeltaIdentity{}, nil, 0, false)
	require.Equal(t, map[string]bool{"reader": true}, p.live)
	require.Equal(t, 1, cap(p.units["reader"].ledger.reads[0].args))
	delete(sparse, "reader")
	require.True(t, p.live["reader"])
	require.True(t, p.units["reader"].owners["reader"])
	// Fresh make(len) maps are used for every retained map, including metadata;
	// Go exposes no safe capacity introspection for maps.
}

func TestContractDeltaBorrowKeepsGlobalChargeUntilReturn(t *testing.T) {
	a, b, c := NewContractActor("", []string{"a"}), NewContractActor("", []string{"b"}), NewContractActor("", []string{"c"})
	p := &contractDeltaCache{}
	l := &contractDeltaLRU{}
	l.put(a, p)
	one := l.bytes
	l.budget = 2 * one
	l.put(b, p)
	before := l.bytes
	require.Same(t, p, l.borrow(a))
	require.Equal(t, before, l.bytes)
	require.Nil(t, l.get(a))
	require.Nil(t, l.borrow(a))
	l.put(c, p)
	require.Nil(t, l.get(b))
	require.LessOrEqual(t, l.bytes, l.budget)
	l.put(a, p)
	require.Same(t, p, l.get(a))
	l.put(a, nil)
	require.Nil(t, l.get(a))
}

func TestContractInterestExpiryRetainsOwnedDeltaCache(t *testing.T) {
	actor := NewContractActor("", []string{"repo"})
	expiry := time.Unix(100, 0)
	state := &contractActor{id: actor, state: contractActorIdle, interestUntil: expiry}
	c := &ContractAnalysisCoordinator{actors: map[ContractActor]*contractActor{actor: state}}
	p := &contractDeltaCache{}
	c.deltaCaches.put(actor, p)
	c.expireLocked(expiry.Add(61 * time.Second))
	require.Same(t, p, c.deltaCaches.get(actor))
	require.False(t, state.retryOnInputs)
}

func TestContractDeltaReaderPostingsConservePlanAndOwnBacking(t *testing.T) {
	receipts := map[string]contractDeltaReceipt{"a": {boundary: "a"}, "b": {boundary: "b"}, "owner": {boundary: "owner"}}
	p := &contractDeltaCache{complete: true, receipts: receipts, live: map[string]bool{"a": true, "b": true, "owner": true}, units: map[string]*contractUnit{
		"a":     {owners: map[string]bool{"owner": true, "other": true}, keys: map[string]bool{"name:Missing": true, "id:Shared": true}},
		"b":     {owners: map[string]bool{"owner": true}, keys: map[string]bool{"name:Missing": true}},
		"owner": {},
	}}
	p.indexReaders()
	require.Equal(t, map[string][]string{"owner": {"a", "b"}, "other": {"a"}}, p.ownerReaders)
	require.Equal(t, map[string][]string{"name:Missing": {"a", "b"}, "id:Shared": {"a"}}, p.keyReaders)
	require.Nil(t, p.keyReaders["name:Unasked"])
	for _, index := range []map[string][]string{p.ownerReaders, p.keyReaders} {
		for _, posting := range index {
			require.Equal(t, len(posting), cap(posting))
		}
	}
	old := p.keyReaders["name:Missing"]
	now := cloneContractDeltaReceipts(receipts)
	delete(now, "owner")
	now["producer"] = newContractDeltaReceipt("new", map[string]string{`["repo","symbol_name","Missing"]`: "value"})
	census := map[string]bool{"a": true, "b": true, "producer": true}
	plan := p.plan(contractDeltaIdentity{}, census, now, false)
	require.Equal(t, []string{"a", "b"}, plan.revalidate)
	require.Equal(t, []string{"producer"}, plan.extract)
	require.Equal(t, []string{"owner"}, plan.removed)
	// Replacement rebuild drops obsolete edges without changing prior postings.
	p.units["a"] = &contractUnit{keys: map[string]bool{"name:Replacement": true}}
	delete(p.units, "b")
	p.indexReaders()
	require.Equal(t, []string{"a", "b"}, old)
	require.Empty(t, p.ownerReaders)
	require.Nil(t, p.keyReaders["name:Missing"])
	require.Equal(t, []string{"a"}, p.keyReaders["name:Replacement"])
}
