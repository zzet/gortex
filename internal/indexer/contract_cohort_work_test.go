package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// A real immutable namespace remains an obligation when its attachment is
// already ready. The payload target list must not become the cache's census.
func TestContractCohortWorkSurvivesPrioritySubsetRecaptureAndNextEdit(t *testing.T) {
	ctx := t.Context()
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "routes.go")
	// A real committed-base copy carries the mutable corpus's accepted
	// namespace into immutable positive storage without rewriting its actor.
	base, _, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: w.graphID, CheckoutID: "primary", GenerationKind: "dedicated", TreeOID: "tree-linked", ProvenanceCommitOID: "commit-linked", CreatedAt: time.Now().UnixNano()})
	require.NoError(t, err)
	_, err = f.store.CopyPayloadGeneration(ctx, 0, base, "fixture")
	require.NoError(t, err)
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, base, time.Now().Unix()))
	w.commit = base
	w.route(t, w.commit, 0)
	companionRoot := t.TempDir()
	writeFile(t, filepath.Join(companionRoot, "companion.go"), "package companion\nfunc Unrelated() int { return 7 }\n")
	_, err = f.mi.TrackRepoCtx(ctx, config.RepoEntry{Name: "companion", Path: companionRoot})
	require.NoError(t, err)
	require.NoError(t, reconcilePrimaryContractBaseline(ctx, f.options, "companion"))
	actor := NewContractActor(contractLinkedCheckout, []string{"fixture", "companion"})
	factory := NewContractLinkedInputCapture(f.options.Materializer)
	var cache *contractDeltaCache
	capture := func() (ContractFollowupSnapshot, []ContractFollowupTarget) {
		inputs, selected, err := factory(ctx, actor, "fixture")
		require.NoError(t, err)
		defer selected.Release()
		snapshot, targets, err := NewContractFollowupSourceCapture(f.options)(ctx, selected, inputs, "fixture", contractLinkedCheckout)
		require.NoError(t, err)
		for i := range targets {
			f.layers++
			_, targets[i].Payload, err = f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "cohort-work", LayerID: fmt.Sprintf("target-%d", f.layers), GenerationKind: "contract_analysis", CreatedAt: time.Now().UnixNano()})
			require.NoError(t, err)
		}
		return snapshot, targets
	}
	run := func(snapshot ContractFollowupSnapshot, targets []ContractFollowupTarget, partial bool) (ContractFollowupBatchReport, error) {
		session, err := newContractDeltaSession(cache, snapshot, f.options.Config)
		require.NoError(t, err)
		req := ContractFollowupBatchRequest{Snapshot: snapshot, Targets: targets, RebuildReason: ContractFollowupUnknownInterval, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: f.options.Materializer.Leases, Yield: f.options.Yield, delta: session}
		if partial {
			req.PriorityRepos = []string{"fixture"}
			req.BackgroundCurrent = func(context.Context) error { return errContractJobKeyMoved }
		}
		report, runErr := RunContractFollowupBatch(ctx, req)
		cache = session.cache
		return report, runErr
	}
	roots := func(work []graph.ContractWork) []graph.ContractWork {
		var out []graph.ContractWork
		for _, row := range work {
			if row.FilePath == row.RepoPrefix+"/" {
				out = append(out, row)
			}
		}
		return out
	}
	first, targets := capture()
	require.Len(t, targets, 2)
	initialRoots := roots(first.cohortWork)
	require.NotEmpty(t, initialRoots)
	var immutableFixtureRoot bool
	for _, witness := range first.Inputs {
		if witness.GenerationID <= 0 || witness.State.RepoPrefix != "fixture" {
			continue
		}
		custody, found, readErr := f.store.Catalog().GetViewGeneration(ctx, witness.GenerationID)
		require.NoError(t, readErr)
		require.True(t, found)
		require.Equal(t, store_sqlite.ViewGenerationReady, custody.State)
		require.Equal(t, w.commit, witness.GenerationID)
		physical, readErr := f.store.AtGeneration(witness.GenerationID).PendingContractWorkForScopeContext(ctx, "fixture", witness.State.CheckoutID)
		require.NoError(t, readErr)
		for _, row := range roots(physical) {
			require.Contains(t, initialRoots, row, "captured roots retain exact positive physical-source rows")
			immutableFixtureRoot = true
		}
	}
	require.True(t, immutableFixtureRoot, "the test must use namespace debt physically retained in a selected positive generation")
	report, err := run(first, targets, true)
	require.ErrorIs(t, err, errContractJobKeyMoved)
	require.Nil(t, report.Publication)
	require.NotNil(t, cache)
	require.True(t, cache.complete)
	require.True(t, cache.continuation.resumesBarrier(cache.identity, first.cohortWork))
	for _, result := range report.Targets {
		require.Equal(t, result.Key.RepoPrefix == "fixture", result.Report.Published)
	}
	// Actual content-equal folding changes physical witness identity, as in
	// the native regression, while retaining exact immutable root rows.
	before, found, err := f.store.Catalog().GetViewGeneration(ctx, w.commit)
	require.NoError(t, err)
	require.True(t, found)
	folded, _, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: before.OwnerKind, GraphID: before.GraphID, LayerID: before.LayerID, CheckoutID: before.CheckoutID, GenerationKind: before.GenerationKind, BaseGenerationID: 0, LowerViewFingerprint: before.LowerViewFingerprint, TreeOID: before.TreeOID, ProvenanceCommitOID: before.ProvenanceCommitOID, ConfigHash: before.ConfigHash, ExtractorVersions: before.ExtractorVersions, ResolverVersion: before.ResolverVersion, DependencyRevision: before.DependencyRevision, CreatedAt: time.Now().UnixNano()})
	require.NoError(t, err)
	_, err = f.store.FlattenGenerationChain(ctx, []int64{w.commit}, folded)
	require.NoError(t, err)
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, folded, time.Now().Unix()))
	w.route(t, folded, 0)
	subset, remaining := capture()
	require.Len(t, remaining, 1)
	require.Equal(t, "companion", remaining[0].Key.RepoPrefix)
	require.Equal(t, initialRoots, roots(subset.cohortWork), "ready target's namespace must remain in captured coverage")
	var remainingOnly []graph.ContractWork
	for _, target := range remaining {
		remainingOnly = append(remainingOnly, target.Work...)
	}
	require.False(t, cache.continuation.resumesBarrier(cache.identity, remainingOnly), "the old target-union boundary loses a real root")
	report, err = run(subset, remaining, false)
	require.NoError(t, err)
	require.Equal(t, viewmetrics.ContractModeDelta, report.Cohort.Mode)
	require.Nil(t, report.Publication, "subset work never fabricates full-target publication proof")
	require.True(t, cache.continuation.resumesBarrier(cache.identity, first.cohortWork), "subset completion must not replace the full admitted barrier proof")
	view := w.view(t)
	files["routes.go"] = strings.ReplaceAll(jobRoutes, "RoutePath", `"/after-priority-subset"`)
	writeFile(t, filepath.Join(w.root, "routes.go"), files["routes.go"])
	dirty := w.build(t, DirtyLayerGenerationKind, folded, view, "routes.go")
	view.Close()
	w.route(t, folded, dirty)
	next, all := capture()
	require.Len(t, all, 2, "one input change gives all dependency targets a new logical key")
	report, err = run(next, all, false)
	require.NoError(t, err)
	require.Equal(t, viewmetrics.ContractModeDelta, report.Cohort.Mode)
	require.Equal(t, []string{"fixture/routes.go"}, report.Cohort.ChangedFiles)
	require.Equal(t, 1, report.Cohort.Files)
	require.True(t, contractPublishedCohortComplete(report.Publication))
	var payload *store_sqlite.Store
	for _, target := range all {
		if target.Key.RepoPrefix == "fixture" {
			payload = target.Payload
		}
	}
	require.NotNil(t, payload)
	nodes, err := payload.NodesByKindsContext(ctx, []graph.NodeKind{graph.KindContract})
	require.NoError(t, err)
	actual := make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		actual[node.ID] = node
	}
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(actual))
}

func TestContractCohortWorkEmptyDoesNotInheritTargetCoverage(t *testing.T) {
	for _, captured := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-empty-%t", captured), func(t *testing.T) {
			f := newContractWorkerFixture(t)
			req, a, b := priorityBatchRequest(t, f)
			req.PriorityRepos = nil
			if captured {
				req.Snapshot.cohortWork = []graph.ContractWork{}
			}
			report, err := RunContractFollowupBatch(t.Context(), req)
			if captured {
				require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
			} else {
				require.NoError(t, err, "legacy snapshots retain the original target-work union")
			}
			for _, target := range []ContractFollowupRequest{a, b} {
				header, readErr := f.store.GetContractAttachmentContext(t.Context(), target.Snapshot.Key)
				require.NoError(t, readErr)
				require.Equal(t, !captured, header != nil)
				require.False(t, f.leases.InUse(target.Payload.ViewGeneration()))
			}
			if captured {
				require.Nil(t, report.Publication)
			}
			require.Equal(t, 1, f.releases)
		})
	}
}
