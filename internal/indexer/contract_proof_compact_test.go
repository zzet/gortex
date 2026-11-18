package indexer

import (
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestContractDeclarerIndexTracksCachedMissAndReplacement(t *testing.T) {
	index := contractDeclarerIndex{}
	units := map[string]*contractUnit{"reader": {path: "reader", declared: []string{"path:reader"}}}
	require.Empty(t, index.lookup("name:Missing", units))
	_, known := index.queries["name:Missing"]
	require.True(t, known, "empty differs from an unasked query")
	producer := &contractUnit{path: "producer", declared: []string{"name:Missing", "path:producer"}}
	units[producer.path] = producer
	index.add(producer)
	require.Equal(t, []string{"producer"}, index.lookup("name:Missing", units))
	index.remove(producer)
	renamed := &contractUnit{path: "producer", declared: []string{"name:Renamed", "path:producer"}}
	units[renamed.path] = renamed
	index.add(renamed)
	require.Empty(t, index.lookup("name:Missing", units))
	require.Equal(t, []string{"producer"}, index.lookup("name:Renamed", units), "new query scans every forward set")
	require.Equal(t, []string{"producer"}, index.lookup("path:producer", units))
	require.NotContains(t, index.queries, "path:producer", "own path reads do not build an inverse table")
	index.remove(renamed)
	delete(units, "producer")
	require.Empty(t, index.lookup("name:Renamed", units))
	require.Empty(t, index.lookup("path:producer", units))
}

func TestContractDeclarerIndexPreservesOtherOwnersAndEmptyClone(t *testing.T) {
	index := contractDeclarerIndex{}
	first := &contractUnit{path: "a", declared: []string{"name:Shared", "path:a"}}
	second := &contractUnit{path: "b", declared: []string{"name:Shared", "path:b"}}
	units := map[string]*contractUnit{"a": first, "b": second}
	require.Equal(t, []string{"a", "b"}, index.lookup("name:Shared", units))
	require.Empty(t, index.lookup("name:Empty", units))
	cloned := index.clone(&contractProofStrings{})
	_, knownEmpty := cloned.queries["name:Empty"]
	_, unasked := cloned.queries["name:Unasked"]
	require.True(t, knownEmpty)
	require.False(t, unasked)
	index.remove(first)
	replacement := &contractUnit{path: "a", declared: []string{"name:Renamed", "path:a"}}
	units["a"] = replacement
	index.add(replacement)
	require.Equal(t, []string{"b"}, index.lookup("name:Shared", units))
	require.Equal(t, []string{"a", "b"}, cloned.lookup("name:Shared", units), "checkpoint postings own their slice")
	index.remove(second)
	delete(units, "b")
	require.Empty(t, index.lookup("name:Shared", units))
	require.Equal(t, []string{"a"}, index.lookup("name:Renamed", units))
}

func TestContractProducedReceiptRetainsIdentityAndEveryQueryKey(t *testing.T) {
	original := map[string]string{`["repo","symbol_name","Missing"]`: "old", `["repo","constant_id","node"]`: "one", "future:key": "future"}
	before := newContractDeltaReceipt("same", original)
	require.Equal(t, []string{"future:key", "id:node", "name:Missing"}, before.producedKeys)
	original[`["repo","symbol_name","Missing"]`] = "new"
	after := newContractDeltaReceipt("same", original)
	require.NotEqual(t, before.producedDigest, after.producedDigest)
	require.Equal(t, before.producedKeys, after.producedKeys)
	delete(original, `["repo","symbol_name","Missing"]`)
	require.NotContains(t, newContractDeltaReceipt("same", original).producedKeys, "name:Missing")
}

func TestContractProofPoolOwnsSharedBackingAndDropsDeadStrings(t *testing.T) {
	backing := strings.Repeat("name", 1024)
	short := backing[:4]
	pool := &contractProofStrings{}
	first := contractProofString(short, pool)
	second := contractProofString(short, pool)
	require.True(t, unsafe.StringData(first) == unsafe.StringData(second))
	require.False(t, unsafe.StringData(first) == unsafe.StringData(short))
	count, charge := pool.seal()
	require.Equal(t, 1, count)
	require.Equal(t, contractDeltaAllocation(4), charge)
	require.Nil(t, pool.values)
	next := &contractProofStrings{}
	contractProofString("new", next)
	nextCount, nextCharge := next.seal()
	require.Equal(t, 1, nextCount, "unreferenced prior pool backing is not carried forward")
	require.Equal(t, contractDeltaAllocation(3), nextCharge)
}

func TestContractProofClonePreservesLedgerAliasesAndReleasesDedupMap(t *testing.T) {
	owners, keys := map[string]bool{"owner": true}, map[string]bool{"name:N": true}
	unit := &contractUnit{path: "reader", owners: owners, keys: keys, declared: []string{"path:reader"}, ledger: &contractUnitLedger{owners: owners, keys: keys, recorded: map[string]bool{"old": true}}}
	pool := &contractProofStrings{}
	clone := cloneContractUnitsMode(map[string]*contractUnit{"reader": unit}, false, pool)["reader"]
	require.True(t, clone.ledgerAliases)
	require.Nil(t, clone.ledger.recorded)
	clone.owners["later"] = true
	require.True(t, clone.ledger.owners["later"])
	clone.keys["name:Later"] = true
	require.True(t, clone.ledger.keys["name:Later"])
	require.NotContains(t, unit.owners, "later")
	require.Equal(t, int64(reflect.TypeOf(map[string]bool(nil)).Size()), contractRetainedSize(reflect.ValueOf(map[string]bool(nil))), "nil maps retain no allocated table")
}
