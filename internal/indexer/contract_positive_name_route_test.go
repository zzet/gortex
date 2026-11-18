package indexer

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"go.uber.org/zap"
)

func (w *contractLinkedWorktree) positiveBuild(t *testing.T, kind string, base int64, view *graphview.RepoView, changed ...string) int64 {
	t.Helper()
	target, err := source.NewFilesystemSource(w.root)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.Close()) }()
	changes := make([]LayerPathChange, len(changed))
	for i, path := range changed {
		changes[i] = LayerPathChange{Path: path, Kind: LayerPathModified}
	}
	generation, _, err := w.builder.Build(t.Context(), BuildRequest{Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: w.graphID, LayerID: "positive-route-chain", CheckoutID: contractLinkedCheckout, GenerationKind: kind, BaseGenerationID: base, TreeOID: "tree-linked", ProvenanceCommitOID: "commit-linked", CreatedAt: time.Now().UnixNano()}, Base: commitLayerBase{Reader: view.Reader, cloneSources: view.GenerationSources()}, Target: target, Changes: changes, RootPath: w.root, RepoPrefix: "fixture", WorkspaceID: "fixture", ProjectID: "fixture"})
	require.NoError(t, err)
	return generation
}
func (w *contractLinkedWorktree) cachedPositiveJob(t *testing.T, view *graphview.RepoView, cache **contractDeltaCache) ContractFollowupReport {
	t.Helper()
	ctx := t.Context()
	options := w.f.options
	inputs, err := options.Materializer.CaptureContractInputs(ctx, view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	snapshot, targets, err := NewContractFollowupCapture(options)(ctx, view, inputs, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	snapshot.Work = targets[0].Work
	session, err := newContractDeltaSession(*cache, snapshot, options.Config)
	require.NoError(t, err)
	_, payload, err := options.Store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "positive-contract-job", LayerID: fmt.Sprint(time.Now().UnixNano()), GenerationKind: "contract_analysis", CreatedAt: time.Now().UnixNano()})
	require.NoError(t, err)
	report, err := RunContractFollowup(ctx, ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: options.Registry, Config: options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: options.Materializer.Leases, Logger: zap.NewNop(), Yield: options.Yield, PublishedEpoch: w.f.epoch.Load, WaitPublished: w.f.waitPublished, delta: session})
	*cache = session.cache
	require.NoError(t, err)
	require.True(t, report.Published)
	require.NotNil(t, *cache)
	require.True(t, (*cache).complete)
	return report
}

func TestContractPositiveNameRouteUpdateUsesCurrentProofAndEqualsCold(t *testing.T) {
	boundary := `package fixture
func boundary(r Router) { r.GET(RoutePath,users);r.GET("/boundary-initial",users) }
`
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper, "boundary.go": boundary}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "boundary.go")
	// Real dedicated positive base: copy all fixture source rows and receipts
	// through the existing production primitive, then publish it normally.
	rootID, _, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: w.graphID, LayerID: "positive-root", CheckoutID: "primary", TreeOID: "tree-linked", GenerationKind: dedicatedGenerationKind, CreatedAt: time.Now().UnixNano()})
	require.NoError(t, err)
	_, err = f.store.CopyPayloadGeneration(t.Context(), 0, rootID, "fixture")
	require.NoError(t, err)
	require.NoError(t, f.store.Catalog().PublishViewGeneration(t.Context(), rootID, time.Now().UnixNano()))
	w.route(t, rootID, 0)
	rootView := w.view(t)
	w.commit = w.positiveBuild(t, CommitLayerGenerationKind, rootID, rootView)
	w.route(t, w.commit, 0)
	commitView := w.view(t)
	var cache *contractDeltaCache
	w.cachedPositiveJob(t, commitView, &cache)
	require.NotNil(t, cache.names, "actual positive capture must retain a bounded owned projection")
	require.NotEmpty(t, cache.names.answers, "baseline must certify actual physical name answers")
	// The first sparse route owns the path; the second edit retains exactly the
	// same hiding predicate while changing its route bytes/accepted input key.
	boundary = strings.Replace(boundary, "boundary-initial", "boundary-first", 1)
	files["boundary.go"] = boundary
	writeFile(t, filepath.Join(w.root, "boundary.go"), boundary)
	dirty := w.positiveBuild(t, DirtyLayerGenerationKind, w.commit, commitView, "boundary.go")
	w.route(t, w.commit, dirty)
	first := w.view(t)
	w.cachedPositiveJob(t, first, &cache)
	boundary = strings.Replace(boundary, "boundary-first", "boundary-next", 1)
	files["boundary.go"] = boundary
	writeFile(t, filepath.Join(w.root, "boundary.go"), boundary)
	next := w.positiveBuild(t, DirtyLayerGenerationKind, dirty, first, "boundary.go")
	w.route(t, w.commit, next)
	current := w.view(t)
	report := w.cachedPositiveJob(t, current, &cache)
	require.Positive(t, report.NameProjectionValueHits, "unchanged exact name answers must use current selected proof, not whole physical replay")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(w.contracts(t, current)))
	require.LessOrEqual(t, cache.retainedBytes, contractDeltaCacheBudget)
}
