package indexer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractCoreInputIdentityCarriesOrdinarySourceChanges(t *testing.T) {
	previous := graph.ContractInputState{RepoPrefix: "repo", CheckoutID: "old", InputVersion: contractCoreInputVersion, InputFingerprint: "accepted", Accepted: true}
	change := contractCoreInputChange{FilePath: "repo/helpers.go", Current: &contractBoundaryReceipt{Source: "new-source"}, Delta: contractBoundaryDelta{ChangedProducedKeys: []string{"unreferenced-helper"}}}
	next, changed, err := nextContractCoreInputState(&previous, "repo", "linked", []contractCoreInputChange{change})
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, previous.InputFingerprint, next.InputFingerprint)
	require.Equal(t, "linked", next.CheckoutID)
	work, err := contractCoreWorkForChanges(12, next, []contractCoreInputChange{change})
	require.NoError(t, err)
	require.Empty(t, work, "unreferenced helper changes create no contract work")
}

func TestContractCoreInputIdentityAdvancesOnlyRelevantAcceptedFacts(t *testing.T) {
	previous := graph.ContractInputState{RepoPrefix: "repo", CheckoutID: "actor", InputVersion: contractCoreInputVersion, InputFingerprint: "accepted", Accepted: true}
	change := contractCoreInputChange{FilePath: "repo/routes.go", Current: &contractBoundaryReceipt{Source: "bytes-one", Policy: "policy"}, Delta: contractBoundaryDelta{Scope: graph.ContractWorkScope{Causes: []string{"boundary_changed"}}}}
	one, changed, err := nextContractCoreInputState(&previous, "repo", "actor", []contractCoreInputChange{change})
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, one.Accepted)
	require.NotEqual(t, previous.InputFingerprint, one.InputFingerprint)
	change.Current.Source = "bytes-two"
	two, _, err := nextContractCoreInputState(&previous, "repo", "actor", []contractCoreInputChange{change})
	require.NoError(t, err)
	require.Equal(t, one.InputFingerprint, two.InputFingerprint, "byte provenance alone is not a contract identity")
	change.Dependents = []contractCoreDependent{{RepoPrefix: "other", CheckoutID: "other-actor", FilePath: "other/consumer.go"}}
	work, err := contractCoreWorkForChanges(12, two, []contractCoreInputChange{change})
	require.NoError(t, err)
	require.Len(t, work, 1)
	require.Equal(t, "repo", work[0].RepoPrefix, "cross-repo dependencies are witnessed, never mutated as source actors")
	require.Equal(t, "actor", work[0].CheckoutID)
}

func TestContractCoreReceiptUncertaintyIsNotDeletionOrKnownAbsence(t *testing.T) {
	change := contractCoreInputChange{FilePath: "repo/routes.go", SourceFingerprint: "accepted-bytes", Uncertainty: "unsupported_contract_input"}
	row, err := contractCoreStoredReceipt("repo", "actor", change)
	require.NoError(t, err)
	require.False(t, row.Deleted)
	require.True(t, row.Scope.Unknown)
	row.Accepted = true
	prior, known, err := contractCorePriorReceipt(&row, true)
	require.NoError(t, err)
	require.False(t, known)
	require.Nil(t, prior)
	change.Deleted = true
	change.Uncertainty = ""
	deleted, err := contractCoreStoredReceipt("repo", "actor", change)
	require.NoError(t, err)
	require.True(t, deleted.Deleted)
	deleted.Accepted = true
	prior, known, err = contractCorePriorReceipt(&deleted, true)
	require.NoError(t, err)
	require.True(t, known)
	require.Nil(t, prior)
}

func TestContractCoreReceiptPendingPreservesExactAcceptedPredecessor(t *testing.T) {
	receipt := &contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: "repo/routes.go", Source: "accepted-bytes", Policy: "policy"}
	old, err := contractCoreStoredReceipt("repo", "actor", contractCoreInputChange{FilePath: receipt.FilePath, SourceFingerprint: receipt.Source, Current: receipt})
	require.NoError(t, err)
	old.Accepted = true
	pending := old
	pending.Accepted = false
	pending.Previous = &old
	prior, known, err := contractCorePriorReceipt(&pending, true)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, reflect.DeepEqual(receipt, prior))
}

func TestContractCoreReceiptOversizedOptionalKeyIsBoundedUnknown(t *testing.T) {
	key := strings.Repeat("k", contractCoreReceiptKeyBytes+1)
	change := contractCoreInputChange{FilePath: "repo/routes.go", SourceFingerprint: "accepted",
		Current: &contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: "repo/routes.go",
			Source: "accepted", Policy: "policy", LookupKeys: []string{key}},
		Delta: contractBoundaryDelta{Scope: graph.ContractWorkScope{LookupKeys: []string{key}}}}
	row, err := contractCoreStoredReceipt("repo", "", change)
	require.NoError(t, err, "optional key overflow cannot reject the ordinary core parse")
	require.True(t, row.Scope.Unknown)
	require.False(t, row.Deleted)
	require.Empty(t, row.LookupKeys)
	require.Empty(t, row.Scope.LookupKeys)
	require.Less(t, len(row.Payload), 1024)
}
