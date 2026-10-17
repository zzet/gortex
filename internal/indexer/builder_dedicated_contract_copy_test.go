package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func asyncClaimedCopyFixture(t *testing.T) (*SparseGenerationBuilder, dedicatedBuilderFixtureRequest, store_sqlite.DedicatedBaseBuildClaim) {
	t.Helper()
	builder, request, _ := privateDedicatedBuilderFixture(t)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	request.Identity.ExtractorVersions = builder.extractorVersionsFingerprint()
	ctx := context.Background()
	// Production Open must install the publication schema; do not create it in this fixture.
	catalog := builder.Store.Catalog()
	authority, err := catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: request.Identity.GraphID,
		Owner:   store_sqlite.DedicatedBaseOwner{CheckoutID: request.Identity.CheckoutID, Incarnation: "private-incarnation"},
		Token:   "private-authority-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: authority, Identity: store_sqlite.DedicatedBaseIdentity{
			TreeOID: request.Identity.TreeOID, ConfigHash: request.Identity.ConfigHash,
			ExtractorVersions: request.Identity.ExtractorVersions, ResolverVersion: request.Identity.ResolverVersion,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "private-attempt-one", ProvenanceCommitOID: request.Identity.ProvenanceCommitOID, CreatedAt: 1,
	})
	if err != nil || claim.GenerationID <= 0 || claim.Status != "allocated" {
		t.Fatalf("initial claim=%+v err=%v", claim, err)
	}
	return builder, request, claim
}

// Certify the primary through the actual installed indexer and raw-owner
// baseline protocol. No header or receipt is fabricated for the positive.
func indexAsyncCopyPrimary(t *testing.T, b *SparseGenerationBuilder, req dedicatedBuilderFixtureRequest, certify bool) {
	t.Helper()
	ctx := context.Background()
	idx := New(b.Store, b.Registry, b.Config, b.Logger)
	idx.SetRepoPrefix(req.RepoPrefix)
	idx.SetWorkspaceID(req.WorkspaceID)
	idx.SetProjectID(req.ProjectID)
	idx.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	t.Cleanup(idx.Close)
	result, err := idx.IndexCtx(ctx, req.RootPath)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	pending, found, err := b.Store.ContractInputStateContext(ctx, req.RepoPrefix, "")
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, pending.Accepted)
	if !certify {
		return
	}
	mi := NewMultiIndexer(b.Store, b.Registry, search.NewNull(), nil, zap.NewNop())
	mi.repos[req.RepoPrefix] = &RepoMetadata{RepoPrefix: req.RepoPrefix, RootPath: req.RootPath}
	mi.indexers[req.RepoPrefix] = idx
	leases := graphview.NewLeaseManager()
	registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: req.RepoPrefix, RootIdentity: req.RootPath, Incarnation: t.Name()}, nil)
	require.NoError(t, err)
	_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "accepted-copy-source")
	require.NoError(t, err)
	mi.SetOutputGenerationAuthority(NewOutputGenerationAuthority(leases))
	options := ContractFollowupCaptureOptions{Context: ctx, Store: b.Store, Materializer: &graphview.Materializer{Store: b.Store, Catalog: b.Store.Catalog(), Leases: leases}, MultiIndexer: mi, Registry: b.Registry, Config: b.Config, Logger: b.Logger, Yield: func(ctx context.Context) error { return ctx.Err() }}
	require.NoError(t, reconcilePrimaryContractBaseline(ctx, options, req.RepoPrefix))
	accepted, found, err := b.Store.ContractInputStateContext(ctx, req.RepoPrefix, "")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, accepted.Accepted)
}

func TestClaimedAsyncBaseCopiesCertifiedPrimaryWithoutReparse(t *testing.T) {
	ctx := context.Background()
	b, req, claim := asyncClaimedCopyFixture(t)
	core, logs := observer.New(zap.InfoLevel)
	b.Logger = zap.New(core)
	indexAsyncCopyPrimary(t, b, req, true)
	zero := generationZeroIndexState(t, b, req)
	require.Equal(t, extractorVersionsFingerprint(), zero.ExtractorVersions)
	require.NotEqual(t, req.Identity.ExtractorVersions, zero.ExtractorVersions, "fixture must exercise installed marker mismatch")
	beforeNodes, beforeEdges := claimedBasePayload(b.Store)
	before, found, err := b.Store.ContractInputStateContext(ctx, req.RepoPrefix, "")
	require.NoError(t, err)
	require.True(t, found)
	baseline, err := b.Store.ContractBoundaryReceiptBaselineContext(ctx, req.RepoPrefix, "")
	require.NoError(t, err)
	require.NotNil(t, baseline)
	id, report := buildClaimedBaseWithReport(t, b, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: req.RootPath, WorkspaceID: req.WorkspaceID, ProjectID: req.ProjectID})
	route, _ := claimedRoute(t, logs)
	require.Equal(t, "copy_generation_zero", route, "accepted primary policy must not force a second full-tree reparse")
	require.Contains(t, report.IndexedPaths, "base.go", "copy report retains actual copied inventory")
	physical := b.Store.AtGeneration(id)
	after, found, err := physical.ContractInputStateContext(ctx, req.RepoPrefix, "")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, before, after, "copy retains original actor and actual identity")
	copiedBaseline, err := physical.ContractBoundaryReceiptBaselineContext(ctx, req.RepoPrefix, "")
	require.NoError(t, err)
	require.Equal(t, baseline, copiedBaseline)
	receipt, known, err := physical.ContractBoundaryReceiptContext(ctx, req.RepoPrefix, "", req.RepoPrefix+"/base.go")
	require.NoError(t, err)
	require.True(t, known)
	require.NotNil(t, receipt)
	require.True(t, receipt.Accepted)
	view := materializeClaimedBase(t, b, req.Identity.GraphID, id)
	afterNodes, afterEdges := claimedBasePayload(view.Reader)
	require.Equal(t, beforeNodes, afterNodes)
	require.Equal(t, beforeEdges, afterEdges)
	m := &graphview.Materializer{Store: b.Store, Catalog: b.Store.Catalog(), Leases: graphview.NewLeaseManager()}
	selected, err := m.CaptureContractInputs(ctx, view, req.RepoPrefix, req.Identity.CheckoutID)
	require.NoError(t, err)
	require.NoError(t, selected.Validate(ctx))
	require.True(t, selected.State.Accepted)
	require.Equal(t, before.InputFingerprint, selected.State.InputFingerprint)
}

func TestClaimedAsyncBaseRefusesUncertifiedOrForeignPrimaryPolicy(t *testing.T) {
	for _, kind := range []string{"legacy", "pending", "accepted_without_census", "extractor_changed", "runtime_absent"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			b, req, _ := asyncClaimedCopyFixture(t)
			switch kind {
			case "legacy":
				indexGenerationZero(t, b, req)
			case "pending", "accepted_without_census":
				indexAsyncCopyPrimary(t, b, req, false)
			default:
				indexAsyncCopyPrimary(t, b, req, true)
			}
			if kind == "accepted_without_census" {
				state, found, err := b.Store.ContractInputStateContext(ctx, req.RepoPrefix, "")
				require.NoError(t, err)
				require.True(t, found)
				require.NoError(t, b.Store.AcceptContractInputMutationWithReceiptsContext(ctx, state, nil))
			}
			if kind == "extractor_changed" {
				state := generationZeroIndexState(t, b, req)
				state.ExtractorVersions = `{"go":9999}`
				setGenerationZeroIndexState(t, b, state)
			}
			if kind == "runtime_absent" {
				b.contractCoreRuntime = nil
			}
			empty, err := b.Store.AtManagedGeneration(1)
			require.NoError(t, err)
			copy, _, _ := b.claimedBaseCopyPlan(ctx, empty, req.RootPath, req.RepoPrefix, req.Identity.TreeOID, req.Identity.ExtractorVersions, true)
			require.False(t, copy, "policy proof must not bless a legacy, pending, missing-baseline or mismatched source")
		})
	}
}

type asyncCopyPendingProbe struct{ store *store_sqlite.Store }

func (p asyncCopyPendingProbe) CopyPayloadGeneration(ctx context.Context, from, to int64, repo string) (GenerationCopyCounts, error) {
	// Begin a real primary mutation AFTER the source plan and BEFORE the copy.
	// The copied transaction must carry the pending blocker, not the earlier
	// accepted observation; the public selected-state setter always accepts.
	current, found, err := p.store.ContractInputStateContext(ctx, repo, "")
	if err != nil {
		return GenerationCopyCounts{}, err
	}
	if !found {
		return GenerationCopyCounts{}, graph.ErrContractInputVector
	}
	next := current
	next.InputFingerprint += "-pending"
	if err := p.store.BeginContractInputMutationContext(ctx, &current, next, nil); err != nil {
		return GenerationCopyCounts{}, err
	}
	return p.store.CopyPayloadGeneration(ctx, from, to, repo)
}

func TestClaimedAsyncBaseRechecksActualCopiedCensusBeforePublish(t *testing.T) {
	ctx := context.Background()
	b, req, claim := asyncClaimedCopyFixture(t)
	indexAsyncCopyPrimary(t, b, req, true)
	_, _, err := b.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: req.RootPath, CopySource: asyncCopyPendingProbe{b.Store}})
	require.ErrorContains(t, err, "copied primary contract input is not accepted")
	generation, found, err := b.Store.Catalog().GetViewGeneration(ctx, claim.GenerationID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEqual(t, store_sqlite.ViewGenerationReady, generation.State)
}
