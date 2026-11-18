package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Unlike the cooperative startup copier, this primitive never yields. A quiet
// copy must still refuse an interrupted destination that has only sidecars.
type quietContractCopyProbe struct {
	store *store_sqlite.Store
	calls int
}

func (p *quietContractCopyProbe) CopyPayloadGeneration(ctx context.Context, from, to int64, repo string) (GenerationCopyCounts, error) {
	p.calls++
	return p.store.CopyPayloadGeneration(ctx, from, to, repo)
}

func TestClaimedAsyncBaseReparsesSidecarOnlyInterruptedTarget(t *testing.T) {
	ctx := context.Background()
	b, req, claim := asyncClaimedCopyFixture(t)
	core, logs := observer.New(zap.InfoLevel)
	b.Logger = zap.New(core)
	indexAsyncCopyPrimary(t, b, req, true)
	target := b.Store.AtGeneration(claim.GenerationID)
	copy, _, _ := b.claimedBaseCopyPlan(ctx, target, req.RootPath, req.RepoPrefix,
		req.Identity.TreeOID, req.Identity.ExtractorVersions, true)
	require.True(t, copy, "a current certified primary and clean tree must otherwise qualify for copy")
	retained := graph.ContractInputState{RepoPrefix: req.RepoPrefix, CheckoutID: req.Identity.CheckoutID,
		InputVersion: contractCoreInputVersion, InputFingerprint: "interrupted-owner-census", Accepted: true}
	require.NoError(t, target.SetContractInputStateWithWorkContext(ctx, nil, retained, nil))
	require.Empty(t, target.AllNodes())
	require.Empty(t, target.AllEdges(), "this regression must not be caught by the existing graph-row guards")
	probe := &quietContractCopyProbe{store: b.Store}
	id := buildClaimedBase(t, b, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: req.RootPath, WorkspaceID: req.WorkspaceID, ProjectID: req.ProjectID, CopySource: probe,
	})
	require.Equal(t, claim.GenerationID, id)
	require.Zero(t, probe.calls, "even a non-yielding copy must not merge primary and interrupted owner inputs")
	route, _ := claimedRoute(t, logs)
	require.Equal(t, "reparse_git_tree", route)
	states, err := target.ContractInputStatesForRepoContext(ctx, req.RepoPrefix)
	require.NoError(t, err)
	require.Len(t, states, 1)
	require.Equal(t, req.Identity.CheckoutID, states[0].CheckoutID)
	require.True(t, states[0].Accepted)
	require.NotEqual(t, retained.InputFingerprint, states[0].InputFingerprint)
	view := materializeClaimedBase(t, b, req.Identity.GraphID, id)
	m := &graphview.Materializer{Store: b.Store, Catalog: b.Store.Catalog(), Leases: graphview.NewLeaseManager()}
	selected, err := m.CaptureContractInputs(ctx, view, req.RepoPrefix, req.Identity.CheckoutID)
	require.NoError(t, err)
	require.NoError(t, selected.Validate(ctx))

	reference, rreq, rclaim := asyncClaimedCopyFixture(t)
	rid := buildClaimedBase(t, reference, ClaimedDedicatedBaseRequest{
		Claim: rclaim, RootPath: rreq.RootPath, WorkspaceID: rreq.WorkspaceID, ProjectID: rreq.ProjectID,
	})
	gotN, gotE := claimedBasePayload(view.Reader)
	wantN, wantE := claimedBasePayload(materializeClaimedBase(t, reference, rreq.Identity.GraphID, rid).Reader)
	require.NotEmpty(t, gotN)
	require.NotEmpty(t, gotE)
	require.Equal(t, wantN, gotN)
	require.Equal(t, wantE, gotE)
}
