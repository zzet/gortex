package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
)

func TestContractCoreEmptyDirtyLayerCarriesSelectedHeader(t *testing.T) {
	ctx := t.Context()
	builder, request, claim := privateClaimedDedicatedFixture(t)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	base, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID})
	require.NoError(t, err)
	parent, found, err := builder.Store.AtGeneration(base).ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, parent.Accepted)
	identity := builderDirtyIdentity()
	identity.GraphID, identity.CheckoutID = request.Identity.GraphID, request.Identity.CheckoutID
	identity.BaseGenerationID = base
	generation, report, err := builder.BuildDirtyLayer(ctx, DirtyLayerRequest{Identity: identity, Base: builder.Store.AtGeneration(base), CheckoutRoot: request.RootPath, RepoPrefix: request.RepoPrefix, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID})
	require.NoError(t, err)
	require.Empty(t, report.IndexedPaths, "the ordinary clean dirty delta must have no indexed paths")
	require.Zero(t, report.DeleteMasks)
	top := builder.Store.AtGeneration(generation)
	carried, found, err := top.ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found, "an empty published positive layer must carry its selected contract identity")
	require.Equal(t, parent, carried)
	mat := &graphview.Materializer{Store: builder.Store, Catalog: builder.Store.Catalog(), Leases: graphview.NewLeaseManager()}
	view, err := mat.MaterializeRefView(ctx, request.Identity.GraphID, generation)
	require.NoError(t, err)
	defer view.Close()
	inputs, err := mat.CaptureContractInputs(ctx, view, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	require.NoError(t, inputs.Validate(ctx))
	require.NotNil(t, view.Reader.GetNode(request.RepoPrefix+"/base.go::Caller"), "the empty delta must retain its parent's core payload")
	rows, err := top.ProducerStates()
	require.NoError(t, err)
	pending := false
	for _, row := range rows {
		if row.Producer == string(graphview.CapContracts) {
			pending = row.State == store_sqlite.ProducerStateIncomplete && row.Reason == graphview.ReasonContractsPending
		}
	}
	require.True(t, pending, "input readiness does not falsely complete contract analysis")
}

// BuildDirtyLayer uses the per-save delta engine even when its report has no
// indexed paths. BuildCommitLayer instead takes the actual sparse skipped-pass
// path; the physical header below must be present before any materialization.
func TestContractCoreEmptySparseCommitLayerCarriesSelectedHeader(t *testing.T) {
	ctx := t.Context()
	builder, request, claim := privateClaimedDedicatedFixture(t)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	base, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID})
	require.NoError(t, err)
	parent, found, err := builder.Store.AtGeneration(base).ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, parent.Accepted)
	identity := builderDirtyIdentity()
	identity.GraphID, identity.CheckoutID = request.Identity.GraphID, request.Identity.CheckoutID
	identity.BaseGenerationID = base
	generation, report, err := builder.BuildCommitLayer(ctx, CommitLayerRequest{
		Identity: identity, Base: builder.Store.AtGeneration(base), RepoDir: request.RootPath,
		BaseTreeOID: request.Identity.TreeOID, TargetTreeOID: request.Identity.TreeOID,
		RootPath: request.RootPath, RepoPrefix: request.RepoPrefix, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID,
	})
	require.NoError(t, err)
	require.NotEqual(t, base, generation, "a fresh physical commit layer must be allocated")
	require.False(t, report.Coalesced, "ready reuse must not satisfy the initialization regression")
	require.Empty(t, report.IndexedPaths)
	require.Zero(t, report.DeleteMasks)
	row, found, err := builder.Store.Catalog().GetViewGeneration(ctx, generation)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, CommitLayerGenerationKind, row.GenerationKind)
	top := builder.Store.AtGeneration(generation)
	carried, found, err := top.ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found, "the fresh sparse empty layer must have its own physical certified header")
	require.Equal(t, parent, carried)
	mat := &graphview.Materializer{Store: builder.Store, Catalog: builder.Store.Catalog(), Leases: graphview.NewLeaseManager()}
	view, err := mat.MaterializeRefView(ctx, request.Identity.GraphID, generation)
	require.NoError(t, err)
	defer view.Close()
	inputs, err := mat.CaptureContractInputs(ctx, view, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	require.NoError(t, inputs.Validate(ctx))
	require.NotNil(t, view.Reader.GetNode(request.RepoPrefix+"/base.go::Caller"))
}

func TestContractCoreEmptyCommitAndDeletionOnlyLayerEqualCold(t *testing.T) {
	f := newRouteConstantFixture(t)
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	w := newContractLinkedWorktree(t, f, files) // no changed file: a fresh empty commit over primary
	view := w.view(t)
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	before, found, err := f.store.AtGeneration(w.commit).ContractInputStateContext(t.Context(), "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, before.Accepted)
	initial, err := w.job(t, view, nil)
	require.NoError(t, err)
	require.True(t, initial.Published)
	require.Contains(t, contractIDs(w.contracts(t, view)), "http::GET::/before")

	require.NoError(t, os.Remove(filepath.Join(w.root, "routes.go")))
	target, err := source.NewFilesystemSource(w.root)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.Close()) }()
	generation, report, err := w.builder.Build(t.Context(), BuildRequest{
		Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: w.graphID, LayerID: "linked-deletion-only", CheckoutID: contractLinkedCheckout, GenerationKind: DirtyLayerGenerationKind, BaseGenerationID: w.commit, TreeOID: "tree-deletion", ProvenanceCommitOID: "commit-linked"},
		Base:     commitLayerBase{Reader: view.Reader, cloneSources: view.GenerationSources(), cloneCorpusBase: f.store.AtGeneration(0)},
		Target:   target, Changes: []LayerPathChange{{Path: "routes.go", Kind: LayerPathDeleted}}, RootPath: w.root, RepoPrefix: "fixture", WorkspaceID: "fixture", ProjectID: "fixture",
	})
	require.NoError(t, err)
	require.Empty(t, report.IndexedPaths, "the regression must exercise deletion without runPass")
	require.Equal(t, 1, report.DeleteMasks)
	top := f.store.AtGeneration(generation)
	state, found, err := top.ContractInputStateContext(t.Context(), "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, state.Accepted)
	require.NotEqual(t, before.InputFingerprint, state.InputFingerprint)
	deleted, known, err := top.ContractBoundaryReceiptContext(t.Context(), "fixture", contractLinkedCheckout, "fixture/routes.go")
	require.NoError(t, err)
	require.True(t, known)
	require.NotNil(t, deleted)
	require.True(t, deleted.Accepted)
	require.True(t, deleted.Scope.Deleted)
	work, err := top.PendingContractWorkForScopeContext(t.Context(), "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.NotEmpty(t, work)
	deletionWork := false
	for _, row := range work {
		deletionWork = deletionWork || row.Scope.Deleted
	}
	require.True(t, deletionWork, "the delete mask must move contract inputs and leave actual deletion debt")
	w.route(t, w.commit, generation)
	current := w.view(t)
	require.Nil(t, current.Reader.GetNode("fixture/routes.go::register"))
	result, err := w.job(t, current, nil)
	require.NoError(t, err)
	require.True(t, result.Published)
	got := w.contracts(t, current)
	require.NotContains(t, contractIDs(got), "http::GET::/before")
	delete(files, "routes.go")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(got), "the actual followup must agree with independent cold extraction")
}

func TestContractCoreEmptyDirtyLayerSourceFenceStillRejects(t *testing.T) {
	ctx := t.Context()
	builder, request, claim := privateClaimedDedicatedFixture(t)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	base, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath})
	require.NoError(t, err)
	physical := builder.Store.AtGeneration(base)
	parent, found, err := physical.ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	identity := builderDirtyIdentity()
	identity.GraphID, identity.CheckoutID = request.Identity.GraphID, request.Identity.CheckoutID
	identity.BaseGenerationID = base
	generation, report, err := builder.BuildDirtyLayer(ctx, DirtyLayerRequest{Identity: identity, Base: physical, CheckoutRoot: request.RootPath, RepoPrefix: request.RepoPrefix,
		buildBarrier: func() {
			writeFile(t, filepath.Join(request.RootPath, "base.go"), "package dedicated\nfunc MovedAfterEmptySample() {}\n")
		},
	})
	require.ErrorIs(t, err, ErrDirtySnapshotChanged)
	require.Empty(t, report.IndexedPaths)
	row, found, err := builder.Store.Catalog().GetViewGeneration(ctx, generation)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationFailed, row.State)
	require.Zero(t, row.PublishedAt)
	unchanged, found, err := physical.ContractInputStateContext(ctx, request.RepoPrefix, request.Identity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, parent, unchanged)
	mat := &graphview.Materializer{Store: builder.Store, Catalog: builder.Store.Catalog(), Leases: graphview.NewLeaseManager()}
	view, err := mat.MaterializeRefView(ctx, request.Identity.GraphID, generation)
	require.Error(t, err)
	require.Nil(t, view, "the private carried header cannot make a source-rejected layer selectable")
}

// A newer empty header cannot repair or bless an uncertified selected ancestor.
func TestContractCoreEmptyLayerRetainsLegacyAncestorRefusal(t *testing.T) {
	f := newRouteConstantFixture(t)
	w := newContractLinkedWorktree(t, f, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})
	view := w.view(t)
	w.builder.contractCoreRuntime = nil // produce an actual pre-protocol empty layer
	legacy := w.build(t, DirtyLayerGenerationKind, w.commit, view)
	w.route(t, w.commit, legacy)
	legacyView := w.view(t)
	w.builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	legacyRow, found, err := f.store.Catalog().GetViewGeneration(t.Context(), legacy)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, DirtyLayerGenerationKind, legacyRow.GenerationKind)
	target, err := source.NewFilesystemSource(w.root)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.Close()) }()
	// Dirty ancestors must share one chain identity. The one-level w.build
	// helper derives LayerID from its immediate base, so using it again here
	// would create a different layer instead of this legacy layer's child.
	generation, _, err := w.builder.Build(t.Context(), BuildRequest{
		Identity: GenerationIdentity{
			OwnerKind: legacyRow.OwnerKind, GraphID: legacyRow.GraphID, LayerID: legacyRow.LayerID, CheckoutID: legacyRow.CheckoutID,
			GenerationKind: legacyRow.GenerationKind, BaseGenerationID: legacy,
			TreeOID: legacyRow.TreeOID, ProvenanceCommitOID: legacyRow.ProvenanceCommitOID, LowerViewFingerprint: legacyRow.LowerViewFingerprint,
			ConfigHash: legacyRow.ConfigHash, ExtractorVersions: legacyRow.ExtractorVersions, ResolverVersion: legacyRow.ResolverVersion, DependencyRevision: legacyRow.DependencyRevision,
		},
		Base:   commitLayerBase{Reader: legacyView.Reader, cloneSources: legacyView.GenerationSources(), cloneCorpusBase: f.store.AtGeneration(0)},
		Target: target, RootPath: w.root, RepoPrefix: "fixture", WorkspaceID: "fixture", ProjectID: "fixture",
	})
	require.NoError(t, err)
	w.route(t, w.commit, generation)
	current := w.view(t)
	header, found, err := f.store.AtGeneration(generation).ContractInputStateContext(t.Context(), "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, header.Accepted)
	states, err := f.store.AtGeneration(legacy).ContractInputStatesForRepoContext(t.Context(), "fixture")
	require.NoError(t, err)
	require.Empty(t, states)
	_, err = f.options.Materializer.CaptureContractInputs(t.Context(), current, "fixture", contractLinkedCheckout)
	require.ErrorContains(t, err, "selected legacy layer has no certified contract input identity", "core publication succeeds but capture must still reject the uncertified ancestor")
}
