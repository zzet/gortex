package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestContractCoreInstalledMultiIndexerAcceptsHTTPChangesWithoutAnalysis(t *testing.T) {
	ctx := context.Background()
	_, store := newSQLiteIndexer(t)
	root := t.TempDir()
	path := filepath.Join(root, "routes.go")
	source := "package fixture\nfunc register(r Router) { r.GET(\"/before\", users) }\nfunc users() { before() }\nfunc before() {}\nfunc after() {}\n"
	writeFile(t, path, source)
	cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "fixture", Path: root}}}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg.SetConfigPath(configPath)
	require.NoError(t, cfg.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(ctx)) })
	published := make(chan string, 8)
	mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: func(_ context.Context, repo, _ string) { published <- repo }})
	results, err := mi.IndexAll()
	require.NoError(t, err)
	require.Empty(t, results["fixture"].FailedFiles)
	idx := mi.indexers["fixture"]
	require.Nil(t, idx.contractRegistry)
	require.Zero(t, idx.contractRegistryLoads)
	require.Nil(t, idx.contractInputWitness)
	require.Nil(t, idx.contractCoreInputs, "outer accepted receipt drains the journal")
	old, known, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/routes.go")
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, old.Accepted)
	work, err := store.PendingContractWorkForScopeContext(ctx, "fixture", "")
	require.NoError(t, err)
	require.NotEmpty(t, work, "a held analysis worker cannot be reported as complete")
	writeFile(t, path, "package fixture\nfunc register(r Router) { r.GET(\"/after\", users) }\nfunc users() { after() }\nfunc before() {}\nfunc after() {}\n")
	result, err := mi.IncrementalReindexRepo("fixture", []string{path})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	users := store.FindNodesByNameInRepo("users", "fixture")
	after := store.FindNodesByNameInRepo("after", "fixture")
	require.Len(t, users, 1)
	require.Len(t, after, 1)
	found := false
	for _, edge := range store.GetOutEdges(users[0].ID) {
		if edge.Kind == graph.EdgeCalls && edge.To == after[0].ID {
			found = true
		}
	}
	require.True(t, found, "ordinary caller freshness is independent of pending contracts")
	fresh, _, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/routes.go")
	require.NoError(t, err)
	require.True(t, fresh.Accepted)
	require.NotEqual(t, old.Fingerprint, fresh.Fingerprint)
	require.Zero(t, idx.contractRegistryLoads)
	require.Nil(t, idx.contractInputWitness)
	require.Empty(t, store.NodesByKinds([]graph.NodeKind{graph.KindContract}))
	require.NotEmpty(t, published)
	// Legacy full/frontier and deferred tails must not enter the resolver lane.
	store.ResolveMutex().Lock()
	done := make(chan struct{})
	go func() {
		mi.ReconcileContractEdges()
		mi.ReconcileContractEdgesForFrontier(DerivedInvalidationPlan{ContractSymbolIDs: []string{users[0].ID}})
		idx.runDeferredContracts()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		store.ResolveMutex().Unlock()
		t.Fatal("installed runtime entered synchronous legacy contract work")
	}
	store.ResolveMutex().Unlock()
}

func TestContractCoreInstalledClaimedRootAndDirtyDeltaPersistReadiness(t *testing.T) {
	ctx := context.Background()
	builder, request, claim := privateClaimedDedicatedFixture(t)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	id, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID})
	require.NoError(t, err)
	require.Equal(t, claim.GenerationID, id)
	physical := builder.Store.AtGeneration(id)
	state, found, err := physical.ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, state.Accepted)
	baseline, err := physical.ContractBoundaryReceiptBaselineContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.NotNil(t, baseline, "only the completed full-root census establishes a namespace baseline")
	receipt, known, err := physical.ContractBoundaryReceiptContext(ctx, request.RepoPrefix, request.Identity.CheckoutID, request.RepoPrefix+"/base.go")
	require.NoError(t, err)
	require.True(t, known)
	require.NotNil(t, receipt)
	require.True(t, receipt.Accepted)
	require.NotNil(t, physical.GetNode(request.RepoPrefix+"/base.go::Caller"))
	rows, err := physical.ProducerStates()
	require.NoError(t, err)
	pending := false
	for _, row := range rows {
		if row.Producer == string(graphview.CapContracts) {
			pending = row.State == "incomplete" && row.Reason == graphview.ReasonContractsPending
		}
	}
	require.True(t, pending, "core-ready root remains explicitly contract-incomplete")
	require.Empty(t, physical.NodesByKinds([]graph.NodeKind{graph.KindContract}))
	writeFile(t, filepath.Join(request.RootPath, "base.go"), "package dedicated\nfunc Committed() string { return \"edited\" }\nfunc Caller() string { return Committed() }\nfunc register(r Router) { r.GET(\"/new\", Caller) }\n")
	identity := builderDirtyIdentity()
	identity.GraphID, identity.CheckoutID = request.Identity.GraphID, request.Identity.CheckoutID
	identity.BaseGenerationID = id
	delta, _, err := builder.BuildDirtyLayer(ctx, DirtyLayerRequest{Identity: identity, Base: physical, CheckoutRoot: request.RootPath, RepoPrefix: request.RepoPrefix, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID})
	require.NoError(t, err)
	top := builder.Store.AtGeneration(delta)
	changed, found, err := top.ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, changed.Accepted)
	require.NotEqual(t, state.InputFingerprint, changed.InputFingerprint)
	dirtyReceipt, _, err := top.ContractBoundaryReceiptContext(ctx, request.RepoPrefix, request.Identity.CheckoutID, request.RepoPrefix+"/base.go")
	require.NoError(t, err)
	require.NotNil(t, dirtyReceipt)
	require.True(t, dirtyReceipt.Accepted)
	work, err := top.PendingContractWorkForScopeContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.NotEmpty(t, work)
	require.Empty(t, top.NodesByKinds([]graph.NodeKind{graph.KindContract}))
}
