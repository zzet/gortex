package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

func TestContractCompletedNamespaceCheckpointKeepsHistoricalWorkDelta(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "routes.go")
	factory := NewContractLinkedInputCapture(f.options.Materializer)
	actor := NewContractActor(contractLinkedCheckout, []string{"fixture"})
	var cache *contractDeltaCache
	var barriers []graph.ContractWork
	run := func() ContractFollowupReport {
		f.layers++
		inputs, source, err := factory(t.Context(), actor, "fixture")
		require.NoError(t, err)
		defer source.Release()
		snapshot, targets, err := NewContractFollowupSourceCapture(f.options)(t.Context(), source, inputs, "fixture", contractLinkedCheckout)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		snapshot.Key, snapshot.Work = targets[0].Key, targets[0].Work
		var current []graph.ContractWork
		for _, row := range snapshot.Work {
			if row.FilePath == row.RepoPrefix+"/" {
				current = append(current, row)
			}
		}
		require.NotEmpty(t, current, "real immutable ancestry retains its full-census debt")
		if barriers == nil {
			barriers = current
		} else {
			require.Equal(t, barriers, current, "historical debt is not erased or rewritten")
		}
		_, payload, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "completed-namespace", LayerID: fmt.Sprintf("job-%d", f.layers), GenerationKind: "contract_analysis", CreatedAt: time.Now().UnixNano()})
		require.NoError(t, err)
		session, err := newContractDeltaSession(cache, snapshot, f.options.Config)
		require.NoError(t, err)
		report, err := RunContractFollowup(t.Context(), ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: f.options.Materializer.Leases, Yield: f.options.Yield, Logger: zap.NewNop(), delta: session})
		cache = session.cache
		require.NoError(t, err)
		require.True(t, report.Published)
		require.NotNil(t, cache)
		require.True(t, cache.complete)
		return report
	}
	first := run()
	require.NotNil(t, cache.continuation)
	require.True(t, cache.continuation.resumesBarrier(cache.identity, barriers))
	replaced := append([]graph.ContractWork(nil), barriers...)
	replaced[0].Token += "-new-census"
	require.False(t, cache.continuation.resumesBarrier(cache.identity, replaced), "a new namespace cannot inherit the old census proof")
	require.Equal(t, viewmetrics.ContractModeFill, first.Mode)
	parent := w.commit
	for i, path := range []string{"/after", "/again"} {
		view := w.view(t)
		files["routes.go"] = strings.ReplaceAll(jobRoutes, "RoutePath", `"`+path+`"`)
		writeFile(t, filepath.Join(w.root, "routes.go"), files["routes.go"])
		target, err := source.NewFilesystemSource(w.root)
		require.NoError(t, err)
		dirty, _, err := w.builder.Build(t.Context(), BuildRequest{
			Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: w.graphID, LayerID: dirtyLayerID(contractLinkedCheckout), CheckoutID: contractLinkedCheckout, GenerationKind: DirtyLayerGenerationKind, BaseGenerationID: parent, TreeOID: "tree-linked", ProvenanceCommitOID: "commit-linked", CreatedAt: time.Now().UnixNano()},
			Base:     commitLayerBase{Reader: view.Reader, cloneSources: view.GenerationSources(), cloneCorpusBase: f.store.AtGeneration(0)}, Target: target, Changes: []LayerPathChange{{Path: "routes.go", Kind: LayerPathModified}}, RootPath: w.root, RepoPrefix: "fixture", WorkspaceID: "fixture", ProjectID: "fixture",
		})
		require.NoError(t, target.Close())
		require.NoError(t, err)
		view.Close()
		w.route(t, w.commit, dirty)
		parent = dirty
		report := run()
		require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
		require.Equal(t, 1, report.Files)
		require.Zero(t, report.Revalidated, "unchanged guarded readers must not replay the whole namespace")
		require.Equal(t, []string{"fixture/routes.go"}, report.ChangedFiles)
		require.Equal(t, i+1, cache.deltas)
		current := w.view(t)
		require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(w.contracts(t, current)))
		current.Close()
	}
}

func completedNamespaceProofFixture(t *testing.T) (*contractConvergence, *contractDeltaCache, []graph.ContractWork) {
	t.Helper()
	c, p, work := continuationProofFixture(t)
	work = append(work, graph.ContractWork{Token: "complete-census", OriginGeneration: 7, RepoPrefix: "repo", FilePath: "repo/", InputVersion: "v", InputFingerprint: "f", State: graph.ContractWorkPending, Scope: graph.ContractWorkScope{Unknown: true, Causes: []string{"full_core_census_complete"}}})
	c.coverage = newContractWorkCoverage(work)
	c.coverage.anchored = true
	c.coverage.observe(c)
	for _, unit := range c.units {
		c.coverage.extracted(unit, "repo/producer.go", "repo/reader.go")
		c.noteFillRead(unit)
	}
	p.continuation = checkpointContractFill(c, p, &contractProofStrings{})
	require.NotNil(t, p.continuation)
	p.continuation.namespaceBaseline = true
	p.hasBaseline = true
	c.coverage = newContractWorkCoverage(work)
	c.coverage.anchored = true
	return c, p, work
}

func TestContractCompletedNamespaceProofReusesOnlyCurrentGuards(t *testing.T) {
	for _, name := range []string{"unrelated_work", "changed_work_row", "new_namespace", "new_unknown_dependency", "changed_census", "writing_guard", "changed_source", "selection"} {
		t.Run(name, func(t *testing.T) {
			c, p, work := completedNamespaceProofFixture(t)
			switch name {
			case "unrelated_work":
				work = append(work, graph.ContractWork{Token: "unrelated", RepoPrefix: "repo", FilePath: "repo/unrelated.go", State: graph.ContractWorkPending})
			case "changed_work_row":
				work[0].Token += "-new"
			case "new_namespace":
				work[1].Token += "-new"
			case "new_unknown_dependency":
				work = append(work, graph.ContractWork{Token: "new-unknown", RepoPrefix: "repo", FilePath: "repo/unrelated.go", State: graph.ContractWorkPending, Scope: graph.ContractWorkScope{Causes: []string{"dependency_membership_unknown"}}})
			case "changed_census":
				c.clock.lastCensus["repo/new.go"] = true
			case "writing_guard":
				stamp := c.clock.last["repo/producer.go"]
				stamp.Writing = true
				c.clock.last["repo/producer.go"] = stamp
			case "changed_source":
				stamp := c.clock.last["repo/producer.go"]
				stamp.SourceFingerprint = "new-source"
				c.clock.last["repo/producer.go"] = stamp
			case "selection":
				c.req.delta.identity.selection = "other"
			}
			c.coverage = newContractWorkCoverage(work)
			c.coverage.anchored = true
			p.continuation.resume(c, p)
			require.Equal(t, name == "unrelated_work", c.coverage.readSatisfied(c.units["repo/reader.go"]), "only identical barrier plus individually current row/census/source/read guards can reuse")
		})
	}
}

func TestContractCompletedNamespaceProofSurvivesWarmInterruption(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint("cancelled=", cancelled), func(t *testing.T) {
			c, p, work := completedNamespaceProofFixture(t)
			p.continuation.resume(c, p)
			c.report = &ContractFollowupReport{Mode: viewmetrics.ContractModeDelta}
			c.logger = zap.NewNop()
			c.snap.Work = work
			session := &contractDeltaSession{cache: p, identity: p.identity, adopted: true}
			c.req.delta = session
			if cancelled {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				c.ctx = ctx
			}
			session.finish(c, false)
			require.NotNil(t, session.cache)
			require.False(t, session.cache.complete, "an interrupted delta is not complete")
			require.True(t, session.cache.hasBaseline)
			if cancelled {
				require.Nil(t, session.cache.continuation, "cancelled context cannot mint fresh continuation provenance")
			} else {
				require.NotNil(t, session.cache.continuation)
				require.True(t, session.cache.continuation.completedBarrierCurrent(session.identity, work))
			}
		})
	}
}

// Published attachments acknowledge exact historical work. Real chain copying
// may therefore remove completed non-namespace uncertainty without deleting its
// source file or introducing any new uncertainty.
func TestContractCompletedUnknownDebtPrunedByCopyKeepsDelta(t *testing.T) {
	ctx := t.Context()
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "pending.go": "package fixture\nfunc Pending() {}\n"}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "routes.go", "consts.go", "pending.go")
	copyLayer := func(from int64, addUnknown bool) int64 {
		row, found, err := f.store.Catalog().GetViewGeneration(ctx, from)
		require.NoError(t, err)
		require.True(t, found)
		id, target, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: row.OwnerKind, GraphID: row.GraphID, LayerID: row.LayerID, CheckoutID: row.CheckoutID, GenerationKind: row.GenerationKind, BaseGenerationID: 0, LowerViewFingerprint: row.LowerViewFingerprint, TreeOID: row.TreeOID, ProvenanceCommitOID: row.ProvenanceCommitOID, ConfigHash: row.ConfigHash, ExtractorVersions: row.ExtractorVersions, ResolverVersion: row.ResolverVersion, DependencyRevision: row.DependencyRevision, CreatedAt: time.Now().UnixNano()})
		require.NoError(t, err)
		_, err = f.store.FlattenGenerationChain(ctx, []int64{from}, id)
		require.NoError(t, err)
		if addUnknown {
			state, ok, err := target.ContractInputStateContext(ctx, "fixture", contractLinkedCheckout)
			require.NoError(t, err)
			require.True(t, ok)
			require.True(t, state.Accepted)
			require.NoError(t, target.SetContractWork(ctx, []graph.ContractWork{{Token: "completed-pending-dependency", OriginGeneration: id, RepoPrefix: "fixture", CheckoutID: contractLinkedCheckout, FilePath: "fixture/pending.go", InputVersion: state.InputVersion, InputFingerprint: state.InputFingerprint, State: graph.ContractWorkPending, Scope: graph.ContractWorkScope{Unknown: true, Causes: []string{"dependency_membership_unknown"}}}}))
		}
		require.NoError(t, f.store.PublishPayloadGeneration(ctx, id, time.Now().Unix()))
		w.route(t, id, 0)
		return id
	}
	bottom := copyLayer(w.commit, true)
	var cache *contractDeltaCache
	var captured []graph.ContractWork
	run := func() ContractFollowupReport {
		inputs, leased, err := NewContractLinkedInputCapture(f.options.Materializer)(ctx, NewContractActor(contractLinkedCheckout, []string{"fixture"}), "fixture")
		require.NoError(t, err)
		defer leased.Release()
		snapshot, targets, err := NewContractFollowupSourceCapture(f.options)(ctx, leased, inputs, "fixture", contractLinkedCheckout)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		snapshot.Key, snapshot.Work = targets[0].Key, targets[0].Work
		captured = append([]graph.ContractWork(nil), snapshot.Work...)
		f.layers++
		_, payload, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "completed-debt-copy", LayerID: fmt.Sprintf("job-%d", f.layers), GenerationKind: "contract_analysis", CreatedAt: time.Now().UnixNano()})
		require.NoError(t, err)
		session, err := newContractDeltaSession(cache, snapshot, f.options.Config)
		require.NoError(t, err)
		report, err := RunContractFollowup(ctx, ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: f.options.Materializer.Leases, Yield: f.options.Yield, Logger: zap.NewNop(), delta: session})
		cache = session.cache
		require.NoError(t, err)
		require.True(t, report.Published)
		require.NotNil(t, cache.continuation)
		require.True(t, cache.continuation.namespaceBaseline)
		return report
	}
	first := run()
	require.Equal(t, viewmetrics.ContractModeFill, first.Mode)
	var oldUnknown []graph.ContractWork
	var oldRoots []graph.ContractWork
	for _, row := range captured {
		if row.FilePath == "fixture/pending.go" && contractDeltaMembershipUnknown([]graph.ContractWork{row}) {
			oldUnknown = append(oldUnknown, row)
		}
		if row.FilePath == "fixture/" {
			oldRoots = append(oldRoots, row)
		}
	}
	require.Len(t, oldUnknown, 1)
	require.NotEmpty(t, oldRoots)
	// The real attachment publication certifies this exact physical row; the
	// store's ordinary flatten then prunes it through contractWorkAcknowledgedSQL.
	folded := copyLayer(bottom, false)
	retained, err := f.store.AtGeneration(folded).ContractWorkForScopeContext(ctx, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	for _, row := range retained {
		require.NotEqual(t, oldUnknown[0].Token, row.Token)
	}
	require.FileExists(t, filepath.Join(w.root, "pending.go"))
	view := w.view(t)
	files["routes.go"] = strings.ReplaceAll(jobRoutes, "RoutePath", `"/after-pruned-debt"`)
	writeFile(t, filepath.Join(w.root, "routes.go"), files["routes.go"])
	dirty := w.build(t, DirtyLayerGenerationKind, folded, view, "routes.go")
	view.Close()
	w.route(t, folded, dirty)
	last := run()
	var newRoots []graph.ContractWork
	for _, row := range captured {
		if row.FilePath == "fixture/" {
			newRoots = append(newRoots, row)
		}
		require.NotEqual(t, oldUnknown[0].Token, row.Token)
	}
	require.Equal(t, oldRoots, newRoots, "namespace roots retain their exact identity")
	require.Equal(t, viewmetrics.ContractModeDelta, last.Mode)
	require.Equal(t, 1, last.Files)
	require.Equal(t, []string{"fixture/routes.go"}, last.ChangedFiles)
	require.Equal(t, 1, cache.deltas)
	current := w.view(t)
	defer current.Close()
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(w.contracts(t, current)))
}

func TestContractCompletedDebtRemovalRetainsExactNamespaceAndGuards(t *testing.T) {
	for _, name := range []string{"removed_completed", "partial_fill", "removed_root", "new_unknown", "changed_unknown", "other_actor", "changed_origin", "changed_input", "selection", "writing_guard", "changed_census", "changed_source"} {
		t.Run(name, func(t *testing.T) {
			c, p, work := completedNamespaceProofFixture(t)
			unknown := graph.ContractWork{Token: "completed-unknown", OriginGeneration: 11, RepoPrefix: "repo", CheckoutID: "linked", FilePath: "repo/other.go", InputVersion: "v", InputFingerprint: "f", State: graph.ContractWorkPending, Scope: graph.ContractWorkScope{Unknown: true, Causes: []string{"dependency_membership_unknown"}}}
			work = append(work, unknown)
			c.coverage = newContractWorkCoverage(work)
			c.coverage.anchored = true
			c.coverage.observe(c)
			for _, unit := range c.units {
				c.coverage.extracted(unit, "repo/producer.go", "repo/reader.go")
				c.noteFillRead(unit)
			}
			p.continuation = checkpointContractFill(c, p, &contractProofStrings{})
			require.NotNil(t, p.continuation)
			p.continuation.namespaceBaseline = true
			current := append([]graph.ContractWork(nil), work[:len(work)-1]...)
			switch name {
			case "partial_fill":
				p.continuation.namespaceBaseline = false
			case "removed_root":
				current = current[:1]
			case "new_unknown":
				unknown.Token += "-new"
				current = append(current, unknown)
			case "changed_unknown":
				unknown.Scope.Causes = append(unknown.Scope.Causes, "new-obligation")
				current = append(current, unknown)
			case "other_actor":
				unknown.CheckoutID = "other"
				current = append(current, unknown)
			case "changed_origin":
				unknown.OriginGeneration++
				current = append(current, unknown)
			case "changed_input":
				unknown.InputFingerprint += "-new"
				current = append(current, unknown)
			case "selection":
				c.req.delta.identity.selection = "other"
			case "writing_guard":
				stamp := c.clock.last["repo/producer.go"]
				stamp.Writing = true
				c.clock.last["repo/producer.go"] = stamp
			case "changed_census":
				c.clock.lastCensus["repo/new.go"] = true
			case "changed_source":
				stamp := c.clock.last["repo/producer.go"]
				stamp.SourceFingerprint = "changed"
				c.clock.last["repo/producer.go"] = stamp
			}
			c.coverage = newContractWorkCoverage(current)
			c.coverage.anchored = true
			barrierCurrent := name == "removed_completed" || name == "writing_guard" || name == "changed_census" || name == "changed_source"
			require.Equal(t, barrierCurrent, p.continuation.resumesBarrier(c.req.delta.identity, current), "root removal or new/changed debt cannot reuse completed barrier authority")
			p.continuation.resume(c, p)
			// Once no namespace obligation remains, ordinary scoped read reuse
			// is independent of the refused completed-barrier authority.
			require.Equal(t, name == "removed_completed" || name == "removed_root", c.coverage.readSatisfied(c.units["repo/reader.go"]))
		})
	}
}
