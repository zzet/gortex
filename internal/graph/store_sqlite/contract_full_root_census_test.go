package store_sqlite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractFullRootCensusRestartRebuildsOnlyOwnedNamespace(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	claim := f.claim(t, "interrupted", 0, 0)
	h := f.store.AtGeneration(claim.GenerationID)
	ctx := context.Background()
	old := attachmentState("interrupted")
	old.CheckoutID = f.owner.CheckoutID
	debt := contractWorkFixture("old-debt")
	debt.CheckoutID, debt.OriginGeneration = old.CheckoutID, claim.GenerationID
	require.NoError(t, h.SetContractInputStateWithWorkContext(ctx, nil, old, []graph.ContractWork{debt}))
	row := boundaryStorageReceipt("departed.go", "interrupted")
	row.CheckoutID, row.Accepted = old.CheckoutID, true
	other := row
	other.CheckoutID = "other-actor"
	require.NoError(t, h.SetContractBoundaryReceiptsWithSourcesContext(ctx, []graph.ContractBoundaryReceipt{row, other}, nil))
	require.NoError(t, h.SetContractBoundaryReceiptBaselineContext(ctx, graph.ContractBoundaryReceiptBaseline{
		RepoPrefix: old.RepoPrefix, CheckoutID: old.CheckoutID, Version: row.Version, Fingerprint: "old-census",
	}))
	next := old
	next.InputFingerprint = "pending-new-census"
	pending := debt
	pending.Token = "new-full-census"
	pending.InputFingerprint = next.InputFingerprint
	pending.Scope = graph.ContractWorkScope{Unknown: true, Causes: []string{"full_core_census_pending"}}
	require.NoError(t, h.RestartContractFullRootCensusContext(ctx, old, next, []graph.ContractWork{pending}))
	actual, found, err := h.ContractInputStateContext(ctx, old.RepoPrefix, old.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, next.InputFingerprint, actual.InputFingerprint)
	baseline, err := h.ContractBoundaryReceiptBaselineContext(ctx, old.RepoPrefix, old.CheckoutID)
	require.NoError(t, err)
	require.Nil(t, baseline)
	removed, known, err := h.ContractBoundaryReceiptContext(ctx, old.RepoPrefix, old.CheckoutID, row.FilePath)
	require.NoError(t, err)
	require.False(t, known, "an interrupted rebuild cannot certify file absence")
	require.Nil(t, removed)
	owners, err := h.ContractBoundaryReceiptsForLookupKeysContext(ctx, row.LookupKeys, 10)
	require.NoError(t, err)
	require.Equal(t, []graph.ContractBoundaryReceipt{other}, owners)
	work, err := h.ContractWorkContext(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []graph.ContractWork{debt, pending}, work)
}

func TestContractFullRootCensusRestartRefusesStaleOrUnownedTarget(t *testing.T) {
	for _, mode := range []string{"changed_input", "wrong_actor", "wrong_repo", "ready", "superseded", "replaced_claim", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			claim := f.claim(t, "interrupted", 0, 0)
			h := f.store.AtGeneration(claim.GenerationID)
			ctx := context.Background()
			old := attachmentState("interrupted")
			old.CheckoutID = f.owner.CheckoutID
			require.NoError(t, h.SetContractInputStateWithWorkContext(ctx, nil, old, nil))
			row := boundaryStorageReceipt("accepted.go", "keep")
			row.CheckoutID, row.Accepted = old.CheckoutID, true
			require.NoError(t, h.SetContractBoundaryReceiptsWithSourcesContext(ctx, []graph.ContractBoundaryReceipt{row}, nil))
			expected := old
			switch mode {
			case "changed_input":
				expected.InputFingerprint = "older-input"
			case "wrong_actor":
				expected.CheckoutID = "other-actor"
			case "wrong_repo":
				expected.RepoPrefix = "other-repo"
			case "ready":
				f.publish(t, claim)
			case "superseded":
				f.publish(t, claim)
				f.exec(t, `UPDATE view_generations SET state='superseded' WHERE generation_id=?`, claim.GenerationID)
			case "replaced_claim":
				f.exec(t, `UPDATE dedicated_base_publications SET attempt_state='failed' WHERE graph_id=?`, f.graph.GraphID)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			next := expected
			next.InputFingerprint = "pending-rebuild"
			require.Error(t, h.RestartContractFullRootCensusContext(ctx, expected, next, nil))
			actual, found, err := h.ContractInputStateContext(context.Background(), old.RepoPrefix, old.CheckoutID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, old, actual)
			kept, known, err := h.ContractBoundaryReceiptContext(context.Background(), old.RepoPrefix, old.CheckoutID, row.FilePath)
			require.NoError(t, err)
			require.True(t, known)
			require.Equal(t, &row, kept)
		})
	}
}
