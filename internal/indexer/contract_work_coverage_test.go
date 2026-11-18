package indexer

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractWorkCoverageRequiresActualReaderProof(t *testing.T) {
	work := graph.ContractWork{Token: "debt", RepoPrefix: "repo", FilePath: "repo/producer.go", State: graph.ContractWorkPending}
	w := newContractWorkCoverage([]graph.ContractWork{work})
	producer := &contractUnit{path: work.FilePath, declared: []string{"name:Value"}}
	reader := &contractUnit{path: "repo/reader.go", keys: map[string]bool{"name:Value": true}, ledger: newContractUnitLedger()}
	unrelated := &contractUnit{path: "repo/unrelated.go", ledger: newContractUnitLedger()}
	c := &contractConvergence{coverage: w, repos: map[string]bool{"repo": true}, units: map[string]*contractUnit{producer.path: producer, reader.path: reader, unrelated.path: unrelated}, live: map[string]bool{producer.path: true, reader.path: true, unrelated.path: true}, suspect: map[string]bool{}}
	w.anchored = true
	w.observe(c)
	require.True(t, w.readers[reader.path], "negative lookups owe a proof even without an owner")
	require.False(t, w.readers[unrelated.path])
	w.extracted(producer)
	reader.round = 100
	require.False(t, w.seal(c), "advancing a round without asking cannot cover a dependent")
	w.record(reader, []string{producer.path, reader.path})
	require.True(t, w.seal(c))
	covered, err := w.sealed.rows([]graph.ContractWork{work})
	require.NoError(t, err)
	require.Equal(t, []graph.ContractWork{work}, covered)
	changed := work
	changed.OriginGeneration++
	_, err = w.sealed.rows([]graph.ContractWork{changed})
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	changed = work
	changed.Scope.LookupKeys = []string{"name:Other"}
	_, err = w.sealed.rows([]graph.ContractWork{changed})
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	_, err = (contractCoveredWork{}).rows(nil)
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete, "an empty target still requires a completed cohort")
}

func TestContractWorkCoverageRetainsOldAndNewFrontiers(t *testing.T) {
	w := newContractWorkCoverage([]graph.ContractWork{{Token: "debt", RepoPrefix: "repo", FilePath: "repo/producer.go", Scope: graph.ContractWorkScope{Unknown: true}}})
	w.remember(&contractUnit{path: "repo/producer.go", declared: []string{"name:Old"}})
	w.remember(&contractUnit{path: "repo/producer.go", declared: []string{"name:New"}})
	for _, key := range []string{"name:Old", "name:New"} {
		require.True(t, w.affected(&contractUnit{path: "repo/reader.go", keys: map[string]bool{key: true}}))
	}
	require.True(t, w.affected(&contractUnit{path: "repo/source_reader.go", ledger: &contractUnitLedger{sources: map[string]string{"repo/producer.go": "bytes"}}}))
	require.False(t, w.affected(&contractUnit{path: "repo/unrelated.go"}), "a per-file unknown is local")
	w.receiptKeys(map[string]contractDeltaReceipt{"repo/producer.go": newContractDeltaReceipt("boundary", map[string]string{`["repo","symbol_name","ReceiptOnly"]`: "value"})})
	require.True(t, w.affected(&contractUnit{path: "repo/receipt_reader.go", keys: map[string]bool{"name:ReceiptOnly": true}}))
}

func TestContractWorkCoverageReusesGuardedSameJobReads(t *testing.T) {
	w := newContractWorkCoverage([]graph.ContractWork{{Token: "debt", RepoPrefix: "repo", FilePath: "repo/producer.go"}})
	w.anchored = true
	reader := &contractUnit{path: "repo/reader.go", keys: map[string]bool{"name:New": true}, ledger: newContractUnitLedger()}
	// Its extraction's bracket included the debt producer, even before the
	// producer's later extraction established the declaration frontier.
	w.extracted(reader, "repo/producer.go")
	producer := &contractUnit{path: "repo/producer.go", declared: []string{"name:New"}}
	c := &contractConvergence{coverage: w, repos: map[string]bool{"repo": true}, units: map[string]*contractUnit{producer.path: producer, reader.path: reader}, live: map[string]bool{producer.path: true, reader.path: true}, suspect: map[string]bool{}}
	w.extracted(producer)
	pending := map[string]string{}
	w.refresh(c, pending)
	require.Empty(t, pending, "later extraction chronology alone does not require another query")
	require.True(t, w.seal(c))
	// A partial cache's earlier proof is never imported into a new capture.
	next := newContractWorkCoverage(w.work)
	next.anchored = true
	next.observe(c)
	require.False(t, next.seal(c))
}

func TestContractWorkSameFingerprintDebtReasksCachedDependents(t *testing.T) {
	f := newRouteConstantFixture(t)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	debt, _ := advanceContractCoverageInputs(t, f.store, "fixture", "fixture/consts.go", graph.ContractWorkScope{})
	extracted := map[string]int{}
	asking, actualReads, passes := false, 0, 0
	hooks := &contractConvergenceHooks{
		beforeExtract: func(path string) { extracted[path]++ },
		beforeAsk:     func() { asking = true; actualReads = 0 },
		beforeRevalidateR1: func() {
			if asking {
				require.Positive(t, actualReads, "same-fingerprint debt cannot pass through a zero-query revalidation")
				passes++
			}
			asking = false
		},
	}
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: &cache, hooks: hooks, epoch: f.epoch.Load, wait: f.waitPublished, adjust: func(snapshot *ContractFollowupSnapshot) {
		require.Contains(t, snapshot.Work, debt)
		snapshot.Core = &contractCountingReader{Reader: snapshot.Core, count: func() {
			if asking {
				actualReads++
			}
		}}
	}})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Positive(t, passes)
	require.Equal(t, 1, extracted["fixture/consts.go"], "the debt file owes its own extraction")
	require.Zero(t, extracted["fixture/helper.go"], "unrelated units remain reusable")
	require.Equal(t, contractMeta(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractMeta(f.contracts()))
	pending, err := f.store.ContractWorkContext(t.Context())
	require.NoError(t, err)
	for _, row := range pending {
		require.NotEqual(t, debt.Token, row.Token)
	}
}

func TestContractWorkCoverageBracketRequiresClosedUnchangedInputs(t *testing.T) {
	closed := ContractReceiptStamp{Fingerprint: "same", Accepted: true}
	writing := closed
	writing.Writing = true
	r0 := ContractFollowupScan{Stamps: map[string]ContractReceiptStamp{"producer": closed}}
	r1 := ContractFollowupScan{Stamps: map[string]ContractReceiptStamp{"producer": closed}}
	require.True(t, contractCoverageBracketHolds(r0, r1, []string{"producer", "departed"}))
	r0.Stamps["producer"], r1.Stamps["producer"] = writing, writing
	require.False(t, contractCoverageBracketHolds(r0, r1, []string{"producer"}), "equal answers inside an open window prove nothing")
	r0.Stamps["producer"], r1.Stamps["producer"] = closed, closed
	r1.Stamps["new_owner"] = closed
	require.False(t, contractCoverageBracketHolds(r0, r1, []string{"new_owner"}), "a returned owner needs a prior R0, including receipt absence")
}

func TestContractWorkCoverageRefusesUnclassifiedOrForeignSources(t *testing.T) {
	row := graph.ContractWork{Token: "debt", RepoPrefix: "repo", FilePath: "repo/source.go"}
	w := newContractWorkCoverage([]graph.ContractWork{row})
	c := &contractConvergence{repos: map[string]bool{"repo": true}, live: map[string]bool{}, excluded: map[string]bool{}}
	c.clock.lastCensus = map[string]bool{row.FilePath: true}
	require.ErrorIs(t, w.validate(c), graph.ErrContractProjectionIncomplete)
	delete(c.clock.lastCensus, row.FilePath)
	require.NoError(t, w.validate(c), "an exact selected census departure is admissible")
	c.clock.lastCensus[row.FilePath] = true
	c.excluded[row.FilePath] = true
	require.NoError(t, w.validate(c), "an explicit envelope is admissible")
	delete(c.repos, "repo")
	require.ErrorIs(t, w.validate(c), graph.ErrContractProjectionIncomplete)
}

func TestContractWorkCoverageDepartureStillOwesItsReaders(t *testing.T) {
	row := graph.ContractWork{Token: "deleted", RepoPrefix: "repo", FilePath: "repo/deleted.go"}
	w := newContractWorkCoverage([]graph.ContractWork{row})
	w.anchored = true
	reader := &contractUnit{path: "repo/reader.go", owners: map[string]bool{row.FilePath: true}, ledger: newContractUnitLedger()}
	c := &contractConvergence{repos: map[string]bool{"repo": true}, live: map[string]bool{reader.path: true}, units: map[string]*contractUnit{reader.path: reader}}
	c.clock.lastCensus = map[string]bool{reader.path: true}
	w.observe(c)
	require.NoError(t, w.validate(c))
	require.False(t, w.seal(c), "departure discharges only the absent file's own read")
	w.record(reader, []string{reader.path, row.FilePath})
	require.True(t, w.seal(c))
}

func TestContractWorkBatchCoversLaterTargetsBeforeSharedPreparation(t *testing.T) {
	f := newContractWorkerFixture(t)
	warm := f.request(t, "repo-a", "coverage-warm")
	warm.Snapshot.Work = nil
	session, err := newContractDeltaSession(nil, warm.Snapshot, warm.Config)
	require.NoError(t, err)
	warm.delta = session
	report, err := RunContractFollowup(t.Context(), warm)
	require.NoError(t, err)
	require.True(t, report.Published)
	// Move the cumulative key through the production dependency journal while
	// keeping source receipts intact; the warmed attachment is deliberately old.
	owedA, acceptedA := advanceContractCoverageInputs(t, f.store, "repo-a", "repo-a/server.go", graph.ContractWorkScope{})
	f.inputs[0].State = acceptedA
	a := f.request(t, "repo-a", "coverage-first")
	b := f.request(t, "repo-b", "coverage-second")
	session, err = newContractDeltaSession(session.cache, a.Snapshot, a.Config)
	require.NoError(t, err)
	extracted := map[string]int{}
	batch, err := RunContractFollowupBatch(t.Context(), ContractFollowupBatchRequest{
		Snapshot: a.Snapshot,
		Targets: []ContractFollowupTarget{
			{Key: a.Snapshot.Key, Payload: a.Payload, Catalog: a.Catalog},
			{Key: b.Snapshot.Key, Work: b.Snapshot.Work, Payload: b.Payload, Catalog: b.Catalog},
		},
		RebuildReason: a.RebuildReason, Registry: a.Registry, Config: a.Config, Leases: a.Leases, Yield: a.Yield,
		delta: session, convergenceHooks: &contractConvergenceHooks{beforeExtract: func(path string) { extracted[path]++ }},
	})
	require.NoError(t, err)
	require.Len(t, batch.Targets, 2)
	for _, result := range batch.Targets {
		require.NoError(t, result.Err)
		require.True(t, result.Report.Published)
	}
	require.Equal(t, 1, extracted["repo-b/client.go"], "later target's same-input debt must participate before preparation")
	require.Zero(t, extracted["repo-a/server.go"], "the first target's unchanged unit is reusable")
	pending, err := f.store.ContractWorkContext(t.Context())
	require.NoError(t, err)
	require.Contains(t, pending, owedA, "uncaptured A debt is not acknowledged by the B coverage test")
	for _, row := range pending {
		require.NotEqual(t, b.Snapshot.Work[0].Token, row.Token, "exact captured B debt was covered and acknowledged")
	}
}

func TestContractWorkPreparedCohortRefusesUncoveredTarget(t *testing.T) {
	f := newContractWorkerFixture(t)
	a := f.request(t, "repo-a", "coverage-gate-a")
	b := f.request(t, "repo-b", "coverage-gate-b")
	prepared := &contractFollowupPrepared{}
	defer func() { require.NoError(t, prepared.close()) }()
	report, err := runContractFollowupPrepared(t.Context(), a, prepared)
	require.NoError(t, err)
	require.True(t, report.Published)
	report, err = runContractFollowupPrepared(t.Context(), b, prepared)
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.False(t, report.Published)
	header, err := f.store.GetContractAttachmentContext(t.Context(), b.Snapshot.Key)
	require.NoError(t, err)
	require.Nil(t, header, "a prepared cohort cannot acknowledge a target it never captured")
	work, err := f.store.ContractWorkContext(t.Context())
	require.NoError(t, err)
	require.Contains(t, work, f.work["repo-b"])
}

func TestContractWorkReplaySamplesDiscoveredOwnerBeforeAcknowledgement(t *testing.T) {
	f := newRouteConstantFixture(t)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	owner := "fixture/consts.go"
	reader := cache.units["fixture/routes.go"]
	require.NotNil(t, reader)
	// Keep the real recorded answers but omit cached ownership hints. Fresh
	// replay must establish ownership itself instead of trusting cache indexes.
	delete(reader.owners, owner)
	delete(reader.keys, "path:"+owner)
	delete(reader.ledger.sources, owner)
	for i := range reader.ledger.reads {
		reader.ledger.reads[i].owners = slices.DeleteFunc(reader.ledger.reads[i].owners, func(path string) bool { return path == owner })
	}
	for key := range reader.keys {
		paths := cache.declarers.lookup(key, cache.units)
		if cache.declarers.queries == nil {
			cache.declarers.queries = map[string][]string{}
		}
		cache.declarers.queries[key] = slices.DeleteFunc(paths, func(path string) bool { return path == owner })
	}
	debt, _ := advanceContractCoverageInputs(t, f.store, "fixture", "fixture/departed.go", graph.ContractWorkScope{Deleted: true, LookupKeys: []string{"name:RoutePath"}})
	opened, sampledOwner := false, false
	fullScansBeforeOwner, asks := 0, 0
	hooks := &contractConvergenceHooks{
		beforeAsk: func() {
			asks++
			if !opened {
				opened = true
				f.stageInFlight(owner)
			}
		},
	}
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: &cache, hooks: hooks, epoch: f.epoch.Load, wait: f.waitPublished, adjust: func(snapshot *ContractFollowupSnapshot) {
		scan := snapshot.Scan
		snapshot.Scan = func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
			if opened && !sampledOwner {
				if paths == nil {
					fullScansBeforeOwner++
				}
				if paths != nil && slices.Contains(paths, owner) {
					work, readErr := f.store.ContractWorkContext(ctx)
					require.NoError(t, readErr)
					require.Contains(t, work, debt, "an equal answer under the discovered owner's open window cannot acknowledge work")
					f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
					sampledOwner = true
				}
			}
			return scan(ctx, paths)
		}
	}})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, sampledOwner, "the retry's point R0 must sample the newly discovered owner")
	require.GreaterOrEqual(t, asks, 2, "the first equal replay had no prior owner R0")
	require.Zero(t, fullScansBeforeOwner, "owner discovery extends point R0 instead of scanning the cohort")
	require.Equal(t, contractMeta(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractMeta(f.contracts()))
}

type contractCoverageSemanticReader struct{}

func (contractCoverageSemanticReader) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	answers := make(map[graph.SemanticBindingSite]string, len(sites))
	for _, site := range sites {
		answers[site] = "string"
	}
	return answers, nil
}

func TestContractWorkCoverageIncludesRecordedSemanticSiteOwners(t *testing.T) {
	path := "repo/producer.go"
	evidence := &contractFollowupEvidence{semantic: contractCoverageSemanticReader{}, ledger: newContractUnitLedger()}
	_, err := evidence.semanticTypes([]graph.SemanticBindingSite{{RepoPrefix: "repo", FilePath: path, Line: 1, Name: "Value"}})
	require.NoError(t, err)
	require.Len(t, evidence.ledger.reads, 1)
	require.Contains(t, evidence.ledger.reads[0].owners, path)
	w := newContractWorkCoverage([]graph.ContractWork{{Token: "debt", RepoPrefix: "repo", FilePath: path}})
	unit := &contractUnit{path: "repo/reader.go", ledger: evidence.ledger}
	require.True(t, w.affected(unit), "semantic sites carry explicit source owners even without name/id keys")
	require.Contains(t, w.relevant(unit), path)
}

// Dependency invalidations can advance an actor's cumulative inputs without
// changing this source's bytes or receipt. Use the production identity/work
// builders and accepted journal transition, so real capture sees a missing
// key while forced work still owes a read of unchanged source inputs.
func advanceContractCoverageInputs(t *testing.T, store *store_sqlite.Store, repo, path string, scope graph.ContractWorkScope) (graph.ContractWork, graph.ContractInputState) {
	t.Helper()
	ctx := t.Context()
	previous, found, err := store.ContractInputStateContext(ctx, repo, "")
	require.NoError(t, err)
	require.True(t, found)
	receipt, hadReceipt, err := store.ContractBoundaryReceiptContext(ctx, repo, "", path)
	require.NoError(t, err)
	var current *contractBoundaryReceipt
	if hadReceipt {
		var known bool
		current, known, err = contractCorePriorReceipt(receipt, true)
		require.NoError(t, err)
		require.True(t, known)
	}
	scope.Causes = append(scope.Causes, "dependency_input_changed")
	changes := []contractCoreInputChange{{FilePath: path, Deleted: scope.Deleted, Prior: current, Current: current, Delta: contractBoundaryDelta{Scope: scope}}}
	next, changed, err := nextContractCoreInputState(&previous, repo, "", changes)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEqual(t, previous.InputFingerprint, next.InputFingerprint)
	work, err := contractCoreWorkForChanges(0, next, changes)
	require.NoError(t, err)
	require.Len(t, work, 1)
	require.NoError(t, store.BeginContractInputMutationContext(ctx, &previous, next, work))
	require.NoError(t, store.AcceptContractInputMutationContext(ctx, next))
	after, stillHasReceipt, err := store.ContractBoundaryReceiptContext(ctx, repo, "", path)
	require.NoError(t, err)
	require.Equal(t, hadReceipt, stillHasReceipt)
	require.Equal(t, receipt, after, "the forced dependency input leaves the file's source receipt identical")
	accepted, found, err := store.ContractInputStateContext(ctx, repo, "")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, accepted.Accepted)
	return work[0], accepted
}

func TestContractWorkTornQueriesRetainClosedOwnExtraction(t *testing.T) {
	f := newRouteConstantFixture(t)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	debt, _ := advanceContractCoverageInputs(t, f.store, "fixture", "fixture/routes.go", graph.ContractWorkScope{})
	extractions, asks, scans, replays := 0, 0, 0, 0
	var release func()
	hooks := &contractConvergenceHooks{
		afterExtract: func(path string) {
			if path == "fixture/routes.go" {
				extractions++
				if release == nil {
					release = f.beginHeld("fixture/consts.go")
				}
			}
		},
		beforeAsk: func() { asks++ },
		revalidated: func(path string, equal bool) {
			if path == "fixture/routes.go" {
				require.True(t, equal)
				replays++
			}
		},
	}
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: &cache, hooks: hooks, epoch: f.epoch.Load, wait: f.waitPublished, adjust: func(snapshot *ContractFollowupSnapshot) {
		scan := snapshot.Scan
		snapshot.Scan = func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
			if release != nil && paths != nil && slices.Contains(paths, "fixture/consts.go") {
				scans++
				if scans == 2 {
					pending, readErr := f.store.ContractWorkContext(ctx)
					require.NoError(t, readErr)
					require.Contains(t, pending, debt, "retained source bytes cannot acknowledge torn queries")
					release()
				}
			}
			return scan(ctx, paths)
		}
	}})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 1, extractions, "closed own bytes survive dependency replay")
	require.Positive(t, asks, "acknowledgement requires an actual closed ledger replay")
	require.Positive(t, replays, "the retained route must pass through the wired ledger replay")
	require.Equal(t, contractMeta(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractMeta(f.contracts()))
}

func TestContractWorkOwnExtractionRequiresSampledClosedUnchangedSource(t *testing.T) {
	closed := ContractReceiptStamp{Fingerprint: "source", Accepted: true}
	for _, mode := range []string{"closed", "writing", "moved", "unsampled", "missing"} {
		t.Run(mode, func(t *testing.T) {
			unit := &contractUnit{path: "repo/source.go"}
			w := newContractWorkCoverage([]graph.ContractWork{{Token: "debt", RepoPrefix: "repo", FilePath: unit.path}})
			w.anchored = true
			r0 := ContractFollowupScan{Settled: true, Stamps: map[string]ContractReceiptStamp{unit.path: closed}}
			r1 := ContractFollowupScan{Settled: true, Stamps: map[string]ContractReceiptStamp{unit.path: closed}}
			sampled := []string{unit.path}
			switch mode {
			case "writing":
				writing := closed
				writing.Writing = true
				r0.Stamps[unit.path], r1.Stamps[unit.path] = writing, writing
			case "moved":
				moved := closed
				moved.Fingerprint = "other"
				r1.Stamps[unit.path] = moved
			case "unsampled":
				sampled = nil
			case "missing":
				delete(r0.Stamps, unit.path)
				delete(r1.Stamps, unit.path)
			}
			w.ownExtracted(unit, r0, r1, sampled)
			require.Equal(t, mode == "closed", w.own[unit.path] == unit)
			require.False(t, w.readSatisfied(unit), "own source provenance never certifies query answers")
		})
	}
}
