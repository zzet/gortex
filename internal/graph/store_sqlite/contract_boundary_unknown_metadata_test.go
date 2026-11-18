package store_sqlite

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractBoundaryUnknownMetadataPreserves4096KeyPendingUnion(t *testing.T) {
	s := openCatalogStore(t)
	ctx := t.Context()
	ordinary := boundaryStorageReceipt("consumer.go", "ordinary")
	ordinary.Scope = graph.ContractWorkScope{}
	ordinary.LookupKeys, ordinary.ProducedKeys = nil, nil
	for i := 0; i < contractBoundaryKeyLimit/2; i++ {
		ordinary.LookupKeys = append(ordinary.LookupKeys, fmt.Sprintf("lookup-%04d", i))
		ordinary.ProducedKeys = append(ordinary.ProducedKeys, fmt.Sprintf("produced-%04d", i))
	}
	require.NoError(t, s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{ordinary}))
	require.NoError(t, s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{ordinary}))
	count := func(want int) {
		t.Helper()
		var actual int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM generation_contract_boundary_keys WHERE view_gen=0 AND repo_prefix=? AND checkout_id='' AND file_path=?`, ordinary.RepoPrefix, ordinary.FilePath).Scan(&actual))
		require.Equal(t, want, actual)
	}
	query := func(lookup bool, key string, want int, accepted bool) []graph.ContractBoundaryReceipt {
		t.Helper()
		var rows []graph.ContractBoundaryReceipt
		var err error
		if lookup {
			rows, err = s.ContractBoundaryReceiptsForLookupKeysContext(ctx, []string{key}, 10)
		} else {
			rows, err = s.ContractBoundaryReceiptsForProducedKeysContext(ctx, []string{key}, 10)
		}
		require.NoError(t, err)
		require.Len(t, rows, want)
		for _, row := range rows {
			require.Equal(t, accepted, row.Accepted)
		}
		return rows
	}
	opaque := ordinary
	opaque.Fingerprint, opaque.SourceFingerprint = "opaque", "opaque-source"
	opaque.LookupKeys = []string{graph.ContractBoundaryUnknownMembershipKey}
	opaque.ProducedKeys = nil
	opaque.Scope = graph.ContractWorkScope{Unknown: true, Causes: []string{"contract_receipt_envelope_exceeded"}}
	opaque.Payload = []byte(`{"opaque":"accepted source identity only"}`)
	require.NoError(t, s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{opaque}))
	count(contractBoundaryKeyLimit + 1)
	for _, i := range []int{0, contractBoundaryKeyLimit/2 - 1} {
		require.True(t, query(true, ordinary.LookupKeys[i], 1, false)[0].Scope.Unknown)
		require.True(t, query(false, ordinary.ProducedKeys[i], 1, false)[0].Scope.Unknown)
	}
	query(true, graph.ContractBoundaryUnknownMembershipKey, 1, false)
	require.NoError(t, s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{opaque}))
	count(1)
	query(true, ordinary.LookupKeys[0], 0, false)
	query(false, ordinary.ProducedKeys[0], 0, false)
	require.True(t, query(true, graph.ContractBoundaryUnknownMembershipKey, 1, true)[0].Scope.Unknown)

	restored := ordinary
	restored.Fingerprint, restored.SourceFingerprint = "restored", "restored-source"
	require.NoError(t, s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{restored}))
	count(contractBoundaryKeyLimit + 1)
	pending := query(true, graph.ContractBoundaryUnknownMembershipKey, 1, false)[0]
	require.NotNil(t, pending.Previous)
	require.True(t, pending.Previous.Scope.Unknown)
	for _, i := range []int{0, contractBoundaryKeyLimit/2 - 1} {
		query(true, ordinary.LookupKeys[i], 1, false)
		query(false, ordinary.ProducedKeys[i], 1, false)
	}
	require.NoError(t, s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{restored}))
	count(contractBoundaryKeyLimit)
	query(true, graph.ContractBoundaryUnknownMembershipKey, 0, false)
	require.False(t, query(true, ordinary.LookupKeys[0], 1, true)[0].Scope.Unknown)
	query(false, ordinary.ProducedKeys[0], 1, true)

	// Neither an arbitrary 4097th key nor the reserved spelling in the wrong
	// key kind is metadata. A refusal retains every previously accepted key.
	for _, key := range []string{"ordinary-extra", graph.ContractBoundaryUnknownMembershipKey} {
		rejected := restored
		rejected.Fingerprint = "must-not-commit"
		rejected.LookupKeys = nil
		rejected.ProducedKeys = []string{key}
		require.ErrorIs(t, s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{rejected}), ErrContractBoundaryReceiptLimit)
		count(contractBoundaryKeyLimit)
		require.Equal(t, restored.Fingerprint, query(true, ordinary.LookupKeys[0], 1, true)[0].Fingerprint)
	}
	tooMany := ordinary
	tooMany.LookupKeys = append(append([]string(nil), ordinary.LookupKeys...), "arbitrary-extra")
	require.ErrorIs(t, s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{tooMany}), ErrContractBoundaryReceiptLimit)
	count(contractBoundaryKeyLimit)
}
