package store_sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractBoundaryLookupAllowsFixedMetadataAtOrdinaryKeyLimit(t *testing.T) {
	s := openCatalogStore(t)
	ctx := t.Context()
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprintf("producer-%04d", i)
	}
	consumer := boundaryStorageReceipt("consumer.go", "full-frontier")
	consumer.LookupKeys, consumer.ProducedKeys = keys, nil
	opaque := boundaryStorageReceipt("opaque.go", "opaque-frontier")
	opaque.LookupKeys, opaque.ProducedKeys = []string{graph.ContractBoundaryUnknownMembershipKey}, nil
	opaque.Scope = graph.ContractWorkScope{Unknown: true, Causes: []string{"opaque_boundary"}}
	require.NoError(t, s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{consumer, opaque}))
	require.NoError(t, s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{consumer, opaque}))
	query := append(append([]string(nil), keys...), graph.ContractBoundaryUnknownMembershipKey)
	rows, err := s.ContractBoundaryReceiptsForLookupKeysContext(ctx, query, 4096)
	require.NoError(t, err, "4096 ordinary producer keys plus fixed lookup metadata must remain a complete indexed query")
	require.Len(t, rows, 2)
	found := make(map[string]graph.ContractBoundaryReceipt)
	for _, row := range rows {
		found[row.FilePath] = row
	}
	require.True(t, found[consumer.FilePath].Accepted)
	require.Equal(t, consumer.LookupKeys, found[consumer.FilePath].LookupKeys)
	require.Equal(t, opaque.Scope, found[opaque.FilePath].Scope, "the opaque consumer must not be dropped to make the ordinary frontier fit")
	// Repeated sentinel input is deduplicated, not an unbounded exception.
	rows, err = s.ContractBoundaryReceiptsForLookupKeysContext(ctx, append(query, graph.ContractBoundaryUnknownMembershipKey), 4096)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	_, err = s.ContractBoundaryReceiptsForLookupKeysContext(ctx, append(query, "ordinary-overflow"), 4096)
	require.ErrorIs(t, err, ErrContractBoundaryReceiptLimit, "4097 ordinary keys remain refused")
	_, err = s.ContractBoundaryReceiptsForProducedKeysContext(ctx, query, 4096)
	require.ErrorIs(t, err, ErrContractBoundaryReceiptLimit, "the metadata exemption is lookup-only")
	_, err = s.ContractBoundaryReceiptsForLookupKeysContext(ctx, query, 1)
	require.ErrorIs(t, err, ErrContractBoundaryReceiptLimit, "complete result-row limits are unchanged")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	rows, err = s.ContractBoundaryReceiptsForLookupKeysContext(cancelled, query, 4096)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
}
