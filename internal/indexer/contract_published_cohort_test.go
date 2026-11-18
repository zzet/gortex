package indexer

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

func TestContractPublishedCohortRefusesRetiredProofAndReleasesWinnerLease(t *testing.T) {
	f := newContractWorkerFixture(t)
	a := f.request(t, "repo-a", "published-cohort-winner-a")
	b := f.request(t, "repo-b", "published-cohort-winner-b")
	batch, err := RunContractFollowupBatch(t.Context(), ContractFollowupBatchRequest{Snapshot: a.Snapshot, Targets: []ContractFollowupTarget{{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Payload: a.Payload, Catalog: a.Catalog}, {Key: b.Snapshot.Key, Work: b.Snapshot.Work, Payload: b.Payload, Catalog: b.Catalog}}, RebuildReason: a.RebuildReason, Registry: a.Registry, Config: a.Config, Leases: a.Leases, Yield: a.Yield})
	require.NoError(t, err)
	proof := cloneContractPublishedCohort(batch.Publication)
	require.NotNil(t, proof)
	proof.Seq = 5
	report := batch.Targets[0].Report
	actor := NewContractActor("", []string{"repo-a", "repo-b"})
	c := &ContractAnalysisCoordinator{options: ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases}, actors: map[ContractActor]*contractActor{actor: {id: actor, lastCohort: cloneContractPublishedCohort(proof)}}}
	leased, release, err := c.AcquireLastPublishedCohort(t.Context(), actor)
	require.NoError(t, err)
	require.Equal(t, report.PayloadGeneration, leased.Targets[0].PayloadGeneration)
	require.True(t, f.leases.InUse(report.PayloadGeneration))
	release()
	release()
	require.False(t, f.leases.InUse(report.PayloadGeneration))
	incomplete := cloneContractPublishedCohort(proof)
	incomplete.Targets = incomplete.Targets[:1]
	c.actors[actor].lastCohort = incomplete
	_, release, err = c.AcquireLastPublishedCohort(t.Context(), actor)
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.Nil(t, release)
	require.False(t, f.leases.InUse(report.PayloadGeneration))
	c.actors[actor].lastCohort = cloneContractPublishedCohort(proof)
	const departedGeneration int64 = 1 << 60
	c.actors[actor].lastCohort.Stamps["repo-a/departed.go"] = ContractReceiptStamp{Generation: departedGeneration, Accepted: true}
	_, release, err = c.AcquireLastPublishedCohort(t.Context(), actor)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Nil(t, release)
	require.False(t, f.leases.InUse(report.PayloadGeneration), "refusal releases actual winner custody")
	require.False(t, f.leases.InUse(departedGeneration))
}

func TestContractPublishedCohortNoopAndMixedJobDoNotInventNewProof(t *testing.T) {
	actor := NewContractActor("", []string{"repo"})
	original := &ContractPublishedCohort{Seq: 3, Witnesses: []graph.ContractInputWitness{{Found: true, State: graph.ContractInputState{RepoPrefix: "repo", Accepted: true}}}, Stamps: map[string]ContractReceiptStamp{"repo/a.go": {Fingerprint: "proved"}}}
	a := &contractActor{id: actor, changed: make(chan struct{}), lastCohort: cloneContractPublishedCohort(original)}
	c := &ContractAnalysisCoordinator{capacityChanged: make(chan struct{})}
	want := contractActorWant{key: "new-key", seq: 4}
	a.want = want
	c.endJob(a, want, "", time.Second)
	require.Equal(t, uint64(4), a.lastPublished.seq)
	require.Equal(t, original, a.lastCohort, "no-op has no fresh extraction/census proof")
	want.seq = 5
	a.want = want
	c.endJob(a, want, viewmetrics.ContractJobPublished, time.Second, nil)
	require.Equal(t, uint64(5), a.lastPublished.seq)
	require.Equal(t, original, a.lastCohort, "a mixed job supplies no complete atomic proof vector")
}

func TestContractPublishedCohortCaptureTimeMixedTargetsDoNotAdvanceProof(t *testing.T) {
	ctx := t.Context()
	_, store := newSQLiteIndexer(t)
	rootA, rootB := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(rootA, "route.go"), "package server\nfunc route(r Router) { r.GET(\"/a\", users) }\nfunc users() {}\n")
	writeFile(t, filepath.Join(rootB, "route.go"), "package server\nfunc route(r Router) { r.GET(\"/b\", users) }\nfunc users() {}\n")
	cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "repo-a", Path: rootA}, {Name: "repo-b", Path: rootB}}}
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg.SetConfigPath(path)
	require.NoError(t, cfg.Save())
	manager, err := config.NewConfigManager(path)
	require.NoError(t, err)
	registry := newTestRegistry()
	mi := NewMultiIndexer(store, registry, search.NewNull(), manager, zap.NewNop())
	defer func() { require.NoError(t, mi.Close(context.Background())) }()
	mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: func(context.Context, string, string) {}})
	_, err = mi.IndexAll()
	require.NoError(t, err)
	materializer := &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: graphview.NewLeaseManager()}
	options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: materializer, MultiIndexer: mi, Registry: registry, Config: config.Default().Index, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
	for _, repo := range []string{"repo-a", "repo-b"} {
		require.NoError(t, reconcilePrimaryContractBaseline(ctx, options, repo))
	}
	inputA, err := materializer.CaptureContractInputs(ctx, nil, "repo-a", "")
	require.NoError(t, err)
	inputB, err := materializer.CaptureContractInputs(ctx, nil, "repo-b", "")
	require.NoError(t, err)
	selected, err := graphview.ComposeSelectedContractInputs("repo-a", "", inputA, inputB)
	require.NoError(t, err)
	first, targets, err := NewContractFollowupCapture(options)(ctx, nil, selected, "repo-a", "")
	require.NoError(t, err)
	require.Len(t, targets, 2)
	require.Equal(t, "repo-a", targets[0].Key.RepoPrefix)
	_, payload, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "mixed-publication", LayerID: "first-a", GenerationKind: "contract_analysis", CreatedAt: 1})
	require.NoError(t, err)
	first.Work = targets[0].Work
	report, err := RunContractFollowup(ctx, ContractFollowupRequest{Snapshot: first, RebuildReason: ContractFollowupColdBaseline, Payload: payload, Catalog: targets[0].Catalog, Registry: registry, Config: options.Config, RepoConfigs: first.RepoConfigs, Leases: materializer.Leases, Yield: options.Yield})
	require.NoError(t, err)
	require.True(t, report.Published)
	// Production capture drops A before execute ever sees job.targets.
	mixed, remaining, err := NewContractFollowupCapture(options)(ctx, nil, selected, "repo-a", "")
	require.NoError(t, err)
	require.Len(t, mixed.Inputs, 2)
	require.Len(t, mixed.Files, 2)
	require.Len(t, remaining, 1)
	require.Equal(t, "repo-b", remaining[0].Key.RepoPrefix)
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: store, Leases: materializer.Leases, Registry: registry, Config: options.Config, Capture: NewContractFollowupSourceCapture(options), Yield: options.Yield})
	require.NoError(t, err)
	defer func() { require.NoError(t, c.Close()) }()
	job := &contractAnalysisJob{id: "capture-time-mixed", snapshot: mixed, targets: remaining, queuedAt: time.Now()}
	require.Equal(t, viewmetrics.ContractJobPublished, c.execute(job))
	require.Nil(t, job.publication, "B-only publication cannot replace an exact full-cohort predecessor proof")
}

func TestContractPublishedCohortDuplicateDoesNotBorrowNewerBodyOnlyStamps(t *testing.T) {
	f := newRouteConstantFixture(t)
	ctx := t.Context()
	inputs, err := f.options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	first, firstTargets, err := NewContractFollowupCapture(f.options)(ctx, nil, inputs, "fixture", "")
	require.NoError(t, err)
	duplicate, duplicateTargets, err := NewContractFollowupCapture(f.options)(ctx, nil, inputs, "fixture", "")
	require.NoError(t, err)
	require.Len(t, firstTargets, 1)
	require.Len(t, duplicateTargets, 1)
	require.Equal(t, firstTargets[0].Work, duplicateTargets[0].Work)
	reserve := func(layer string) *store_sqlite.Store {
		_, payload, reserveErr := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "body-stamp-provenance", LayerID: layer, GenerationKind: "contract_analysis", CreatedAt: 1})
		require.NoError(t, reserveErr)
		return payload
	}
	firstTargets[0].Payload = reserve("original")
	original, err := RunContractFollowupBatch(ctx, ContractFollowupBatchRequest{Snapshot: first, Targets: firstTargets, RebuildReason: ContractFollowupColdBaseline, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: first.RepoConfigs, Leases: f.options.Materializer.Leases, Yield: f.options.Yield})
	require.NoError(t, err)
	require.NotNil(t, original.Publication)
	// Comment-only edits move source receipts while retaining the boundary key
	// and selected accepted input witnesses used by both captured jobs.
	f.save("helper.go", jobHelper+"// source-only move after publication\n")
	current, err := f.options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.Equal(t, inputs.State.InputVersion, current.State.InputVersion)
	require.Equal(t, inputs.State.InputFingerprint, current.State.InputFingerprint)
	// Previous-input rider bookkeeping may advance after the first publish;
	// the physical/current witness identity used by publication CAS stays put.
	identity := func(inputs []graph.ContractInputWitness) []graph.ContractInputWitness {
		copy := slices.Clone(inputs)
		for i := range copy {
			copy[i].State.PreviousInputVersion, copy[i].State.PreviousInputFingerprint = "", ""
		}
		return copy
	}
	require.Equal(t, identity(duplicate.Inputs), identity(current.Witnesses))
	defer duplicate.Release()
	duplicate.Release = func() {}
	duplicate.Work = duplicateTargets[0].Work
	loser := reserve("duplicate")
	lease := f.options.Materializer.Leases.Acquire(loser.ViewGeneration())
	defer lease.Release()
	prepared := &contractFollowupPrepared{}
	defer func() { require.NoError(t, prepared.close()) }()
	_, err = buildContractFollowupPrepared(ctx, ContractFollowupRequest{Snapshot: duplicate, Payload: loser, Catalog: duplicateTargets[0].Catalog, RebuildReason: ContractFollowupColdBaseline, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: duplicate.RepoConfigs, Leases: f.options.Materializer.Leases, Yield: f.options.Yield}, prepared)
	require.NoError(t, err)
	helper := "fixture/helper.go"
	require.NotEqual(t, original.Publication.Stamps[helper].SourceFingerprint, prepared.proof.Stamps[helper].SourceFingerprint, "the duplicate preparation observed newer comment-only source bytes")
	require.NotEqual(t, original.Publication.Stamps[helper], prepared.proof.Stamps[helper], "the duplicate preparation observed a newer source receipt")
	results, err := duplicateTargets[0].Catalog.PublishContractAttachmentsWithInputsContext(ctx, prepared.publications, time.Now().Unix())
	require.NoError(t, err, "same key, witnesses and captured acknowledgments still validate as a duplicate")
	require.Len(t, results, 1)
	require.Equal(t, firstTargets[0].Payload.ViewGeneration(), results[0].PayloadGeneration)
	require.Equal(t, loser.ViewGeneration(), results[0].UnusedPayloadGeneration)
	require.Nil(t, contractPublishedResults(prepared.proof, prepared.publications, results), "newer preparation stamps cannot become the older served winner's provenance")
}
