package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractCoreStoragePrimarySourceAcceptanceAndOrdinaryIdentityCarry(t *testing.T) {
	ctx := context.Background()
	idx, store := newSQLiteIndexer(t)
	idx.SetRepoPrefix("fixture")
	root := t.TempDir()
	file := filepath.Join(root, "routes.go")
	source := `package fixture
func register(router *Router) { router.GET("/before", users) }
func users() { before() }
func before() {}
func after() {}
`
	writeFile(t, file, source)
	backend, err := newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
	require.NoError(t, err)
	idx.contractCoreInputs, err = newContractCoreInputJournal(ctx, backend)
	require.NoError(t, err)
	result, err := idx.IndexCtx(ctx, root)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	state, found, err := store.ContractInputStateContext(ctx, "fixture", "")
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, state.Accepted, "the background baseline owns complete namespace certification")
	work, err := store.ContractWorkForScopeContext(ctx, "fixture", "")
	require.NoError(t, err)
	require.NotEmpty(t, work, "core acceptance leaves contract analysis pending")
	require.Nil(t, idx.contractRegistry)
	require.Nil(t, idx.contractInputWitness)
	require.Zero(t, idx.contractRegistryLoads)
	require.Empty(t, store.NodesByKinds([]graph.NodeKind{graph.KindContract}))
	oldReceipt, known, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/routes.go")
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, oldReceipt.Accepted)

	// Only an ordinary computation/comment changes; the route and handler's
	// emitted interface facts are identical. Source eligibility still updates.
	writeFile(t, file, source+"// ordinary accepted comment\n")
	backend, err = newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
	require.NoError(t, err)
	idx.contractCoreInputs, err = newContractCoreInputJournal(ctx, backend)
	require.NoError(t, err)
	require.NoError(t, idx.IndexFile(file))
	carried, _, err := store.ContractInputStateContext(ctx, "fixture", "")
	require.NoError(t, err)
	require.Equal(t, state, carried, "no new attachment identity or worker is needed")
	remaining, err := store.ContractWorkForScopeContext(ctx, "fixture", "")
	require.NoError(t, err)
	require.Equal(t, work, remaining)
	newReceipt, _, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/routes.go")
	require.NoError(t, err)
	require.True(t, newReceipt.Accepted)
	require.NotEqual(t, oldReceipt.SourceFingerprint, newReceipt.SourceFingerprint)
}

func TestContractCoreStorageInterruptedPrimaryDoesNotBlockCoreOrCertifyNamespace(t *testing.T) {
	ctx := context.Background()
	_, store := newSQLiteIndexer(t)
	previous := graph.ContractInputState{RepoPrefix: "fixture", InputVersion: contractCoreInputVersion,
		InputFingerprint: "interrupted", Accepted: false}
	require.NoError(t, store.BeginContractInputMutationContext(ctx, nil, previous, nil))
	backend, err := newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
	require.NoError(t, err, "durable pending namespaces remain recoverable")
	change := contractCoreInputChange{FilePath: "fixture/ordinary.go", SourceFingerprint: "accepted",
		Current: &contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: "fixture/ordinary.go",
			Source: "accepted", Policy: "policy"}}
	require.NoError(t, backend.BeginBoundaryMutation(ctx, []contractCoreInputChange{change}))
	require.NoError(t, backend.AcceptBoundaryMutation(ctx))
	state, found, err := store.ContractInputStateContext(ctx, "fixture", "")
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, state.Accepted, "a later point edit cannot acknowledge an interrupted full census")
	row, known, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", change.FilePath)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, row.Accepted, "the later accepted source receipt is durable independently")
}

func TestContractCoreStorageLegacyMissingStateCannotBeCertifiedByPointEdit(t *testing.T) {
	ctx := context.Background()
	_, store := newSQLiteIndexer(t)
	backend, err := newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
	require.NoError(t, err)
	change := contractCoreInputChange{FilePath: "fixture/ordinary.go", SourceFingerprint: "accepted",
		Current: &contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: "fixture/ordinary.go", Source: "accepted", Policy: "policy"}}
	require.NoError(t, backend.BeginBoundaryMutation(ctx, []contractCoreInputChange{change}))
	require.NoError(t, backend.AcceptBoundaryMutation(ctx))
	state, found, err := store.ContractInputStateContext(ctx, "fixture", "")
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, state.Accepted, "point acceptance cannot certify an unknown historical namespace")
}
