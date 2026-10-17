package indexer

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func opaqueTestReceipt(t *testing.T, source string, keys map[string]string) graph.ContractBoundaryReceipt {
	t.Helper()
	receipt := &contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: "fixture/routes.go", Source: contractInputHash([]byte(source)), Policy: "current-policy", ProducedInputs: keys}
	row, err := contractCoreStoredReceipt("fixture", "", contractCoreInputChange{FilePath: receipt.FilePath, Current: receipt, SourceFingerprint: receipt.Source})
	require.NoError(t, err)
	return row
}

func TestContractOpaqueReceiptBoundsAndSourceIdentity(t *testing.T) {
	many := make(map[string]string, contractCoreReceiptKeyLimit+1)
	for i := 0; i <= contractCoreReceiptKeyLimit; i++ {
		many[fmt.Sprintf("key-%d", i)] = "value"
	}
	for name, keys := range map[string]map[string]string{
		"keys":      many,
		"key_bytes": {strings.Repeat("k", contractCoreReceiptKeyBytes+1): "value"},
		"payload":   {"key": strings.Repeat("v", contractCoreReceiptPayloadLimit)},
	} {
		t.Run(name, func(t *testing.T) {
			row := opaqueTestReceipt(t, "before", keys)
			require.True(t, row.Scope.Unknown)
			require.Equal(t, []string{contractOverflowLookupKey}, row.LookupKeys)
			require.Empty(t, row.ProducedKeys)
			require.Less(t, len(row.Payload), 1024)
			policy, opaque := contractReceiptOverflowPolicy(&row)
			require.True(t, opaque)
			require.Equal(t, "current-policy", policy)
			row.Accepted = true
			prior, known, err := contractCorePriorReceipt(&row, true)
			require.NoError(t, err)
			require.False(t, known, "opaque source is not dependency evidence")
			require.Nil(t, prior)
			next := opaqueTestReceipt(t, "after", keys)
			require.NotEqual(t, row.Fingerprint, next.Fingerprint)
			require.NotEqual(t, contractReceiptContent(row.Payload, row.SourceFingerprint), contractReceiptContent(next.Payload, next.SourceFingerprint))
		})
	}
	ordinary := opaqueTestReceipt(t, "ordinary", map[string]string{"key": "value"})
	require.False(t, ordinary.Scope.Unknown)
	require.Empty(t, ordinary.LookupKeys)
	require.Equal(t, []string{"key"}, ordinary.ProducedKeys)
	_, opaque := contractReceiptOverflowPolicy(&ordinary)
	require.False(t, opaque)
}

func TestContractOpaqueReceiptRejectsUnprovedMarkers(t *testing.T) {
	original := opaqueTestReceipt(t, "accepted", map[string]string{strings.Repeat("k", contractCoreReceiptKeyBytes+1): "value"})
	for _, name := range []string{"legacy", "source", "policy", "path", "hash", "deleted", "scope", "scope_deleted", "scope_groups", "sentinel", "produced", "extra"} {
		t.Run(name, func(t *testing.T) {
			row := original
			var marker contractOverflowSourceReceipt
			require.NoError(t, json.Unmarshal(row.Payload, &marker))
			switch name {
			case "legacy":
				marker.SourceFingerprint = ""
			case "source":
				marker.SourceFingerprint = "different"
			case "policy":
				marker.Policy = ""
			case "path":
				marker.FilePath = "fixture/foreign.go"
			case "hash":
				row.Fingerprint = "wrong"
			case "deleted":
				row.Deleted = true
			case "scope":
				row.Scope.Unknown = false
			case "scope_deleted":
				row.Scope.Deleted = true
			case "scope_groups":
				row.Scope.Groups = []graph.ContractWorkGroup{{ContractID: "invented-group"}}
			case "sentinel":
				row.LookupKeys = []string{"ordinary-lookup"}
			case "produced":
				row.ProducedKeys = []string{"invented-declaration"}
			}
			if slices.Contains([]string{"legacy", "source", "policy", "path", "extra"}, name) {
				var err error
				row.Payload, err = json.Marshal(marker)
				require.NoError(t, err)
				if name == "extra" {
					row.Payload = append(row.Payload[:len(row.Payload)-1], []byte(`,"extra":true}`)...)
				}
				row.Fingerprint = contractInputHash(row.Payload)
			}
			_, opaque := contractReceiptOverflowPolicy(&row)
			require.False(t, opaque)
		})
	}
}

func opaqueRouteSource(route string) string {
	var source strings.Builder
	fmt.Fprintf(&source, "package fixture\nfunc register(r Router) { r.GET(%q, handle) }\nfunc handle() {}\n", route)
	// Each distinct function publishes local+foreign name and ID keys. This
	// exceeds the real 4096-key envelope without huge source or fake receipts.
	for i := 0; i < 1025; i++ {
		fmt.Fprintf(&source, "func OpaqueHelper%d() int { return %d }\n", i, i)
	}
	return source.String()
}

func TestContractOpaqueOverflowRunsColdAndTracksProducerEdits(t *testing.T) {
	files := map[string]string{"routes.go": "package fixture\nfunc register(r Router) { r.GET(\"/opaque-before\", handle) }\nfunc handle() {}\n", "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	require.NotNil(t, cache, "start with actual retained state that must not be adopted")
	files["routes.go"] = opaqueRouteSource("/opaque-before")
	f.save("routes.go", files["routes.go"])
	for phase := 0; phase < 3; phase++ {
		inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
		require.NoError(t, err)
		require.True(t, inputs.State.Accepted)
		if phase > 0 {
			if phase == 1 {
				files["routes.go"] = opaqueRouteSource("/opaque-after")
				f.save("routes.go", files["routes.go"])
			} else {
				files["helper.go"] += "\nfunc UnrelatedProducer() int { return 3 }\n"
				f.save("helper.go", files["helper.go"])
			}
			next, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
			require.NoError(t, err)
			require.True(t, next.State.Accepted)
			require.NotEqual(t, inputs.State.InputFingerprint, next.State.InputFingerprint, "opaque membership must invalidate on another producer's edit too")
		}
		row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/routes.go")
		require.NoError(t, err)
		require.True(t, row.Accepted)
		require.True(t, row.Scope.Unknown)
		require.Less(t, len(row.Payload), 1024)
		// Repeated baseline requests must stay settled and preserve the exact
		// opaque source witness, not recreate pending repair work or empty facts.
		before := row.Fingerprint
		for i := 0; i < 2; i++ {
			require.NoError(t, reconcilePrimaryContractBaseline(t.Context(), f.options, "fixture"))
		}
		row, _, err = f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/routes.go")
		require.NoError(t, err)
		require.Equal(t, before, row.Fingerprint)
		var extracted []string
		f.layers++
		report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: &cache, otherActorWaiting: func() bool { return true },
			adjust: func(s *ContractFollowupSnapshot) { require.True(t, s.opaqueContractInputs) },
			hooks:  &contractConvergenceHooks{afterExtract: func(path string) { extracted = append(extracted, path) }}})
		require.NoError(t, err)
		require.True(t, report.Published)
		require.Equal(t, viewmetrics.ContractModeCold, report.Mode)
		require.Equal(t, "receipt_envelope_exceeded", report.CacheReason)
		require.Equal(t, len(files), report.Files)
		slices.Sort(extracted)
		require.Equal(t, []string{"fixture/helper.go", "fixture/routes.go"}, extracted)
		require.Nil(t, cache, "no opaque cache or continuation can survive")
		require.Contains(t, contractIDs(f.contracts()), []string{"http::GET::/opaque-before", "http::GET::/opaque-after", "http::GET::/opaque-after"}[phase])
		if phase > 0 {
			require.NotContains(t, contractIDs(f.contracts()), "http::GET::/opaque-before")
		}
		require.Equal(t, contractColdMatrixCold(t, files), contractColdMatrixRead(t, f), "entire actual payload must equal independent cold")
	}
}

func TestContractOpaqueOverflowSourceMoveCannotReuseContent(t *testing.T) {
	files := map[string]string{"routes.go": opaqueRouteSource("/race-before"), "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	var cache *contractDeltaCache
	moved, extracted := false, 0
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: &cache,
		hooks: &contractConvergenceHooks{afterExtract: func(path string) {
			if path != "fixture/routes.go" {
				return
			}
			extracted++
			if !moved {
				moved = true
				files["routes.go"] = opaqueRouteSource("/race-after")
				f.saveKeepingKey("routes.go", files["routes.go"])
			}
		}}})
	require.NoError(t, err)
	require.True(t, moved)
	require.GreaterOrEqual(t, extracted, 2, "the old source extraction cannot certify the changed opaque source")
	require.True(t, report.Published)
	require.Equal(t, viewmetrics.ContractModeCold, report.Mode)
	require.Nil(t, cache)
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/race-after")
	require.NotContains(t, contractIDs(f.contracts()), "http::GET::/race-before")
	require.Equal(t, contractColdMatrixCold(t, files), contractColdMatrixRead(t, f))
}

func TestContractOpaqueSentinelPreservesCrossRepositoryOwnership(t *testing.T) {
	_, store := newSQLiteIndexer(t)
	ctx := t.Context()
	row := opaqueTestReceipt(t, "consumer-bytes", map[string]string{strings.Repeat("k", contractCoreReceiptKeyBytes+1): "value"})
	row.RepoPrefix, row.FilePath = "consumer", "consumer/routes.go"
	var marker contractOverflowSourceReceipt
	require.NoError(t, json.Unmarshal(row.Payload, &marker))
	marker.FilePath = row.FilePath
	row.Payload, _ = json.Marshal(marker)
	row.Fingerprint = contractInputHash(row.Payload)
	require.NoError(t, store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row}))
	require.NoError(t, store.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row}))
	before := graph.ContractInputState{RepoPrefix: "producer", InputVersion: contractCoreInputVersion, InputFingerprint: "before", Accepted: true}
	require.NoError(t, store.BeginContractInputMutationContext(ctx, nil, before, nil))
	require.NoError(t, store.AcceptContractInputMutationContext(ctx, before))
	backend, err := newPrimaryContractCoreStorageBackend(ctx, store, "producer")
	require.NoError(t, err)
	journal, err := newContractCoreInputJournal(ctx, backend)
	require.NoError(t, err)
	journal.changes["producer/types.go"] = contractCoreInputChange{FilePath: "producer/types.go", SourceFingerprint: "producer-source",
		Current: &contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: "producer/types.go", Source: "producer-source", Policy: "policy"},
		Delta:   contractBoundaryDelta{ChangedProducedKeys: []string{"foreign-type-key"}}}
	require.NoError(t, journal.begin([]string{"producer/types.go"}))
	require.NoError(t, journal.accept(nil))
	after, found, err := store.ContractInputStateContext(ctx, "producer", "")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEqual(t, before.InputFingerprint, after.InputFingerprint)
	consumer := graph.ContractInputState{RepoPrefix: "consumer", InputVersion: contractCoreInputVersion, InputFingerprint: "consumer-accepted", Accepted: true}
	beforeVector, err := graph.ComposeContractInputState("consumer", "", []graph.ContractInputWitness{{Found: true, State: consumer}, {Found: true, State: before}})
	require.NoError(t, err)
	afterVector, err := graph.ComposeContractInputState("consumer", "", []graph.ContractInputWitness{{Found: true, State: consumer}, {Found: true, State: after}})
	require.NoError(t, err)
	require.NotEqual(t, beforeVector.InputFingerprint, afterVector.InputFingerprint, "relevant composed consumer input changes without mutating its own namespace")
	untouched, _, err := store.ContractBoundaryReceiptContext(ctx, "consumer", "", row.FilePath)
	require.NoError(t, err)
	require.Equal(t, row.Fingerprint, untouched.Fingerprint)
	require.True(t, untouched.Scope.Unknown)
}

func TestContractOpaqueBaselineUpgradesLegacyMarkerAndSettles(t *testing.T) {
	f := newContractJobFixture(t, map[string]string{"routes.go": opaqueRouteSource("/legacy-opaque"), "helper.go": jobHelper})
	ctx := t.Context()
	row, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/routes.go")
	require.NoError(t, err)
	legacy := *row
	legacy.Accepted = false
	legacy.Payload, err = json.Marshal(struct {
		Version, FilePath, Uncertainty string
		Deleted                        bool
	}{contractBoundaryReceiptVersion, row.FilePath, "contract_receipt_envelope_exceeded", false})
	require.NoError(t, err)
	legacy.Fingerprint = contractInputHash(legacy.Payload)
	legacy.LookupKeys = nil
	require.NoError(t, f.store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{legacy}))
	require.NoError(t, f.store.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{legacy}))
	state, found, err := f.store.ContractInputStateContext(ctx, "fixture", "")
	require.NoError(t, err)
	require.True(t, found)
	pending := state
	pending.InputFingerprint = contractInputHash([]byte(state.InputFingerprint + ":legacy-baseline"))
	require.NoError(t, f.store.BeginContractInputMutationContext(ctx, &state, pending, nil))
	ext, ok := f.options.Registry.GetByLanguage("go")
	require.True(t, ok)
	spy := &currentReceiptParseSpy{Extractor: ext}
	f.options.Registry.Register(spy)
	require.NoError(t, reconcilePrimaryContractBaseline(ctx, f.options, "fixture"))
	require.EqualValues(t, 1, spy.reads.Load(), "legacy marker needs exactly one accepted reconstruction")
	spy.forbid.Store(true)
	require.NoError(t, reconcilePrimaryContractBaseline(ctx, f.options, "fixture"))
	require.EqualValues(t, 1, spy.reads.Load(), "settled header must not start another reconstruction")
	settled, _, err := f.store.ContractInputStateContext(ctx, "fixture", "")
	require.NoError(t, err)
	require.True(t, settled.Accepted)
	current, _, err := f.store.ContractBoundaryReceiptContext(ctx, "fixture", "", row.FilePath)
	require.NoError(t, err)
	_, opaque := contractReceiptOverflowPolicy(current)
	require.True(t, opaque)
	require.True(t, current.Scope.Unknown)
	require.True(t, current.Accepted)
	require.Equal(t, row.SourceFingerprint, current.SourceFingerprint)
	require.NotEqual(t, legacy.Fingerprint, current.Fingerprint)
}
