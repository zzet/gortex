package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

func selectedProjectionRegistry(t *testing.T) {
	t.Helper()
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
}

func selectedPendingEntry(t *testing.T, stack *viewStack) *projectionEntry {
	t.Helper()
	ctx := routedEnrichmentCtx(t, stack)
	path := filepath.Join(stack.worktreeRoot, "keep.go")
	require.NoError(t, os.WriteFile(path, []byte("package repo\n\nfunc Keeper() {}\nfunc Latest() {}\n"), 0o644))
	entry, err := stack.srv.projectCurrentTrackedFile(t.Context(), ctx, path, "repo/keep.go", "keep.go")
	require.NoError(t, err)
	require.NotNil(t, entry)
	return entry
}

func publishSelectedEntry(t *testing.T, stack *viewStack, entry *projectionEntry, deleted bool) {
	t.Helper()
	generation, handle, err := stack.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: stack.graphID, LayerID: viewTestDirtyLayerID, CheckoutID: viewTestWorktree, GenerationKind: "dirty", BaseGenerationID: stack.dirty, TreeOID: "selected-current", CreatedAt: 5000,
	})
	require.NoError(t, err)
	mode := store_sqlite.OwnershipReplace
	if deleted {
		mode = store_sqlite.OwnershipDelete
	} else {
		handle.AddBatch(entry.layer.FileNodes(entry.graphPath), nil)
		require.NoError(t, handle.SetFileMetas(entry.repoPrefix, []graph.FileMetaRow{{FilePath: entry.graphPath, ContentHash: entry.hash}}))
	}
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: entry.repoPrefix, FilePath: entry.graphPath, Mode: mode}}))
	require.NoError(t, stack.store.PublishPayloadGeneration(t.Context(), generation, 6000))
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, generation, store_sqlite.RouteActive)
	stack.dirty = generation
}

func TestSelectedProjectionRetiresCaughtUpLeasedReader(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	entry := selectedPendingEntry(t, stack)
	publishSelectedEntry(t, stack, entry, false)
	ctx := routedEnrichmentCtx(t, stack)
	_, err := stack.srv.ensureProjected(ctx, []string{entry.graphPath})
	require.NoError(t, err)
	require.Nil(t, liveEntry(pendingFileProjections, entry.absPath), "selected accepted bytes must retire without consulting canonical old inventory")
}

func TestSelectedProjectionRetirementKeepsSiblingPhysicalEntry(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	entry := selectedPendingEntry(t, stack)
	siblingPath := filepath.Join(stack.repoRoot, "keep.go")
	sibling := entry.committedCopy(0, entry.disk)
	sibling.absPath = siblingPath
	sibling.key = pathkey.MutationKey(siblingPath)
	require.NoError(t, pendingFileProjections.publish(sibling))
	publishSelectedEntry(t, stack, entry, false)
	_, err := stack.srv.ensureProjected(routedEnrichmentCtx(t, stack), []string{entry.graphPath})
	require.NoError(t, err)
	require.Nil(t, liveEntry(pendingFileProjections, entry.absPath))
	require.Same(t, sibling, liveEntry(pendingFileProjections, sibling.absPath))
}

func TestSelectedProjectionDurableDeleteRetiresTombstone(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	entry := selectedPendingEntry(t, stack)
	require.NoError(t, os.Remove(entry.absPath))
	tombstone, err := stack.srv.projectCurrentTrackedFile(t.Context(), routedEnrichmentCtx(t, stack), entry.absPath, entry.graphPath, "keep.go")
	require.NoError(t, err)
	require.True(t, tombstone.tombstone)
	publishSelectedEntry(t, stack, tombstone, true)
	_, err = stack.srv.ensureProjected(routedEnrichmentCtx(t, stack), []string{entry.graphPath})
	require.NoError(t, err)
	require.Nil(t, liveEntry(pendingFileProjections, entry.absPath), "durable newest selected ownership-delete is positive deletion proof")
}

func projectedCarryFixture(t *testing.T, selected bool) (*Server, context.Context, string) {
	t.Helper()
	selectedProjectionRegistry(t)
	srv, root := newRelationFixture(t)
	var reader graph.Reader = srv.graph
	if selected {
		layer := graph.NewOverlayLayer()
		layer.MarkFile("target.go", false)
		layer.AddNode("target.go", &graph.Node{ID: "target.go::SelectedTarget", Name: "Target", FilePath: "target.go", Kind: graph.KindFunction})
		layer.AddEdge(&graph.Edge{From: "caller.go::Caller", To: "target.go::SelectedTarget", Kind: graph.EdgeCalls, FilePath: "caller.go", Line: 4})
		reader = graph.NewOverlaidView(srv.graph, layer)
	}
	ctx := t.Context()
	if selected {
		ctx = withRequestView(ctx, &requestView{reader: reader, viewRoot: root, kind: requestViewKindWorktree})
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "caller.go"), []byte("package p\n\n// Current caller documentation.\nfunc Caller() {\n Target()\n}\n"), 0o644))
	return srv, ctx, "caller.go::Caller"
}

func TestProjectedCarryPreservesCurrentCallerAndCallee(t *testing.T) {
	srv, ctx, caller := projectedCarryFixture(t, false)
	ctx, err := srv.ensureProjected(ctx, []string{"caller.go"})
	require.NoError(t, err)
	var carried *graph.Edge
	for _, edge := range srv.readerFor(ctx).GetOutEdges(caller) {
		if edge.To == "target.go::Target" {
			carried = edge
		}
	}
	require.NotNil(t, carried, "current unresolved call must preserve matching base adjacency")
	require.True(t, carried.Provisional)
	require.Equal(t, 5, carried.Line)
	require.Contains(t, srv.readerFor(ctx).GetInEdges("target.go::Target"), carried)
}

func TestProjectedCarryUsesSameSelectedBase(t *testing.T) {
	srv, ctx, caller := projectedCarryFixture(t, true)
	ctx, err := srv.ensureProjected(ctx, []string{"caller.go"})
	require.NoError(t, err)
	var found bool
	for _, edge := range srv.readerFor(ctx).GetOutEdges(caller) {
		require.NotEqual(t, "target.go::Target", edge.To, "canonical target must not leak into selected carry")
		found = found || edge.To == "target.go::SelectedTarget" && edge.Provisional
	}
	require.True(t, found)
}

func TestProjectedCarryKeepsImmutableFullNodes(t *testing.T) {
	srv, ctx, caller := projectedCarryFixture(t, false)
	ctx, err := srv.ensureProjected(ctx, []string{"caller.go"})
	require.NoError(t, err)
	entry := liveEntry(pendingFileProjections, filepath.Join(srv.indexer.RootPath(), "caller.go"))
	require.NotNil(t, entry)
	require.Same(t, entry.layer.NodeByID(caller), srv.readerFor(ctx).GetNode(caller), "full extracted nodes including Meta must remain intact")
	for _, edges := range entry.layer.OutEdgesByFromAll() {
		for _, edge := range edges {
			require.False(t, edge.Provisional, "published registry layer must stay immutable")
		}
	}
	var found bool
	for _, edge := range srv.readerFor(ctx).GetOutEdges(caller) {
		found = found || edge.Provisional
	}
	require.True(t, found, "request visibility carries copies, not registry mutations")
}

type selectedRetirementProbe struct {
	graph.Reader
	metadataCalls       int
	beforeMetadata      func(int)
	beforeNodes         func()
	metadataError       error
	nodesError          error
	waitMetadataContext bool
	waitNodesContext    bool
}

func (p *selectedRetirementProbe) ReadConstantValueProjectionContext(ctx context.Context, ids []string, files []graph.ConstantFileKey) (graph.ConstantValueProjection, error) {
	p.metadataCalls++
	if p.beforeMetadata != nil {
		p.beforeMetadata(p.metadataCalls)
	}
	if p.waitMetadataContext {
		<-ctx.Done()
		return graph.ConstantValueProjection{}, ctx.Err()
	}
	if p.metadataError != nil {
		return graph.ConstantValueProjection{}, p.metadataError
	}
	return p.Reader.(graph.ConstantValueProjectionReader).ReadConstantValueProjectionContext(ctx, ids, files)
}

func (p *selectedRetirementProbe) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*graph.Node, error) {
	if p.beforeNodes != nil {
		p.beforeNodes()
	}
	if p.waitNodesContext {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if p.nodesError != nil {
		return nil, p.nodesError
	}
	return graph.ContractFileNodesContext(ctx, p.Reader, repo, paths)
}

func TestSelectedProjectionUnknownProofRetainsWithoutCanonicalFallback(t *testing.T) {
	for _, mode := range []string{"old selected", "unsupported", "metadata error", "node error", "closed lease"} {
		t.Run(mode, func(t *testing.T) {
			selectedProjectionRegistry(t)
			stack := newViewStack(t)
			entry := selectedPendingEntry(t, stack)
			// Even matching canonical metadata cannot authorize a selected checkout.
			require.NoError(t, stack.store.SetFileMetas(entry.repoPrefix, []graph.FileMetaRow{{FilePath: entry.graphPath, ContentHash: entry.hash}}))
			if mode != "old selected" {
				publishSelectedEntry(t, stack, entry, false)
			}
			ctx, view := routedEnrichmentView(t, stack)
			switch mode {
			case "unsupported":
				wrapper := struct{ graph.Reader }{view.reader}
				view.reader = wrapper
				view.materialized.Reader = wrapper
			case "metadata error", "node error":
				probe := &selectedRetirementProbe{Reader: view.reader}
				if mode == "metadata error" {
					probe.metadataError = graph.ErrConstantProjectionIncomplete
				} else {
					probe.nodesError = graph.ErrContractProjectionLimit
				}
				view.reader = probe
				view.materialized.Reader = probe
			case "closed lease":
				view.close()
			}
			_, err := stack.srv.ensureProjected(ctx, []string{entry.graphPath})
			require.NoError(t, err)
			require.Same(t, entry, liveEntry(pendingFileProjections, entry.absPath), "unknown selected proof must retain the frontier")
		})
	}
}

func TestSelectedProjectionNewerPointerDuringNodesSurvives(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	entry := selectedPendingEntry(t, stack)
	publishSelectedEntry(t, stack, entry, false)
	ctx, view := routedEnrichmentView(t, stack)
	newer := entry.committedCopy(entry.commitSeq+1, entry.disk)
	require.Greater(t, newer.commitSeq, entry.commitSeq, "the replacement must be newer in a full package run too")
	probe := &selectedRetirementProbe{Reader: view.reader, beforeNodes: func() { require.NoError(t, pendingFileProjections.publish(newer)) }}
	view.reader = probe
	view.materialized.Reader = probe
	_, err := stack.srv.ensureProjected(ctx, []string{entry.graphPath})
	require.NoError(t, err)
	require.Same(t, newer, liveEntry(pendingFileProjections, entry.absPath), "checked selected nodes cannot remove a later registry pointer")
}

func TestSelectedProjectionHandoffKeepsLeaseThroughProof(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	entry := selectedPendingEntry(t, stack)
	publishSelectedEntry(t, stack, entry, false)
	ctx, view := routedEnrichmentView(t, stack)
	generation := view.materialized.Generations()[len(view.materialized.Generations())-1]
	observed := false
	probe := &selectedRetirementProbe{Reader: view.reader, beforeMetadata: func(_ int) {
		view.close()
		require.True(t, stack.leases.InUse(generation), "synchronous proof must keep the same reader leased after origin close")
		observed = true
	}}
	view.reader = probe
	view.materialized.Reader = probe
	_, err := stack.srv.ensureProjected(ctx, []string{entry.graphPath})
	require.NoError(t, err)
	require.True(t, observed)
	require.Nil(t, liveEntry(pendingFileProjections, entry.absPath))
	require.False(t, stack.leases.InUse(generation), "proof handoff must be joined before return")
}

func TestSelectedProjectionDeleteNeedsPositiveNewestOwnership(t *testing.T) {
	for _, mode := range []string{"old replacement", "flat absence", "higher replacement"} {
		t.Run(mode, func(t *testing.T) {
			selectedProjectionRegistry(t)
			stack := newViewStack(t)
			entry := selectedPendingEntry(t, stack)
			require.NoError(t, os.Remove(entry.absPath))
			tombstone, err := stack.srv.projectCurrentTrackedFile(t.Context(), routedEnrichmentCtx(t, stack), entry.absPath, entry.graphPath, "keep.go")
			require.NoError(t, err)
			if mode == "higher replacement" {
				publishSelectedEntry(t, stack, tombstone, true)
				publishSelectedEntry(t, stack, entry, false)
			}
			ctx, view := routedEnrichmentView(t, stack)
			if mode == "flat absence" {
				empty := graph.New()
				view.reader = empty
				view.materialized.Reader = empty
			}
			_, err = stack.srv.ensureProjected(ctx, []string{entry.graphPath})
			require.NoError(t, err)
			retained := liveEntry(pendingFileProjections, entry.absPath)
			require.NotNil(t, retained, "absence or a higher replacement cannot authorize tombstone retirement")
			require.True(t, retained.tombstone)
		})
	}
}

func TestSelectedProjectionChangedAncestorRetainsAfterFirstHash(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	entry := selectedPendingEntry(t, stack)
	publishSelectedEntry(t, stack, entry, false)
	ctx, view := routedEnrichmentView(t, stack)
	changed := false
	probe := &selectedRetirementProbe{Reader: view.reader, beforeNodes: func() {
		correction, err := stack.store.BeginDerivedCorrection(t.Context(), store_sqlite.DerivedCorrectionRequest{GenerationID: stack.commit, Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
		require.NoError(t, err)
		node := stack.store.AtGeneration(stack.commit).GetNode("repo/edit.go::New")
		require.NotNil(t, node)
		updated := *node
		updated.Meta = map[string]any{"source_derived_decl_fingerprint": "changed-during-selected-proof"}
		require.NoError(t, correction.ReplaceSourceEdges(t.Context(), []string{node.ID}, nil, []*graph.Node{&updated}))
		_, err = correction.Finish(t.Context())
		require.NoError(t, err)
		changed = true
	}}
	view.reader = probe
	view.materialized.Reader = probe
	_, err := stack.srv.ensureProjected(ctx, []string{entry.graphPath})
	require.NoError(t, err)
	require.True(t, changed, "actual lower-layer revision must move during proof")
	require.Same(t, entry, liveEntry(pendingFileProjections, entry.absPath))
}

func TestSelectedProjectionCheckedReadsStayOutsideRegistryMutex(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	entry := selectedPendingEntry(t, stack)
	publishSelectedEntry(t, stack, entry, false)
	ctx, view := routedEnrichmentView(t, stack)
	observed := false
	check := func() {
		require.True(t, pendingFileProjections.mu.TryLock(), "checked selected I/O must never hold registry mutex")
		pendingFileProjections.mu.Unlock()
		observed = true
	}
	probe := &selectedRetirementProbe{Reader: view.reader, beforeNodes: check, beforeMetadata: func(int) { check() }}
	view.reader = probe
	view.materialized.Reader = probe
	_, err := stack.srv.ensureProjected(ctx, []string{entry.graphPath})
	require.NoError(t, err)
	require.True(t, observed)
	require.Nil(t, liveEntry(pendingFileProjections, entry.absPath))
}

func TestProjectedCarryDoesNotResurrectRemovedOrDifferentSourceCalls(t *testing.T) {
	for _, current := range []string{"package p\nfunc Caller() { Other() }\n", "package p\nfunc Different() { Target() }\n"} {
		t.Run(current, func(t *testing.T) {
			srv, ctx, _ := projectedCarryFixture(t, false)
			require.NoError(t, os.WriteFile(filepath.Join(srv.indexer.RootPath(), "caller.go"), []byte(current), 0o644))
			ctx, err := srv.ensureProjected(ctx, []string{"caller.go"})
			require.NoError(t, err)
			for _, node := range srv.readerFor(ctx).GetFileNodes("caller.go") {
				for _, edge := range srv.readerFor(ctx).GetOutEdges(node.ID) {
					require.False(t, edge.Provisional, "a removed call or another source cannot inherit the old caller's relation")
				}
			}
		})
	}
}

func TestProjectedCarryUnsupportedKeepsExactPendingFile(t *testing.T) {
	srv, ctx, caller := projectedCarryFixture(t, false)
	// The selected read surface predates the optional checked carry APIs.
	reader := struct{ graph.Reader }{srv.graph}
	ctx = withRequestView(ctx, &requestView{reader: reader, viewRoot: srv.indexer.RootPath(), kind: requestViewKindWorktree})
	ctx, err := srv.ensureProjected(ctx, []string{"caller.go"})
	require.NoError(t, err, "optional adjacency carry must not refuse an exact current file projection")
	require.NotNil(t, srv.readerFor(ctx).GetNode(caller))
	require.True(t, srv.hasRequestProjectionFact(ctx, "caller.go"))
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "get_symbol"
	req.Params.Arguments = map[string]any{"id": caller, "detail": "full"}
	scope, ids, files := freshnessTargets(req.Params.Name, req)
	require.Equal(t, "relations", scope)
	require.Equal(t, "pending", srv.freshnessFor(ctx, scope, ids, files).state, "unsupported carry never makes relation freshness complete")
	for _, edge := range srv.readerFor(ctx).GetOutEdges(caller) {
		require.False(t, edge.Provisional, "unknown selected carry supplies no inherited edge proof")
	}
}

func TestSelectedProjectionLateMetadataFailureRetains(t *testing.T) {
	for _, phase := range []int{2, 3} {
		t.Run(string(rune('0'+phase)), func(t *testing.T) {
			selectedProjectionRegistry(t)
			stack := newViewStack(t)
			entry := selectedPendingEntry(t, stack)
			publishSelectedEntry(t, stack, entry, false)
			ctx, view := routedEnrichmentView(t, stack)
			var probe *selectedRetirementProbe
			probe = &selectedRetirementProbe{Reader: view.reader, beforeMetadata: func(call int) {
				if call == phase {
					probe.metadataError = graph.ErrConstantProjectionStale
				}
			}}
			view.reader = probe
			view.materialized.Reader = probe
			_, err := stack.srv.ensureProjected(ctx, []string{entry.graphPath})
			require.NoError(t, err)
			require.GreaterOrEqual(t, probe.metadataCalls, phase, "exercise the second hash or final pre-COW validation")
			require.Same(t, entry, liveEntry(pendingFileProjections, entry.absPath))
		})
	}
}

func TestProjectedCarryForeignRecordedSiteDoesNotCarry(t *testing.T) {
	srv, ctx, caller := projectedCarryFixture(t, false)
	abs := filepath.Join(srv.indexer.RootPath(), "caller.go")
	entry, err := srv.projectCurrentTrackedFile(t.Context(), ctx, abs, "caller.go", "caller.go")
	require.NoError(t, err)
	layer := graph.NewOverlayLayer()
	layer.MarkFile("caller.go", false)
	for _, node := range entry.layer.FileNodes("caller.go") {
		layer.AddNode("caller.go", node)
	}
	layer.AddEdge(&graph.Edge{From: caller, To: "unresolved::Target", Kind: graph.EdgeCalls, FilePath: "foreign.go", Line: 5})
	owned := entry.committedCopy(0, entry.disk)
	owned.layer = layer
	visible, err := projectedFileVisibility(t.Context(), srv.graph, owned)
	require.NoError(t, err)
	for edge := range visible.Edges() {
		require.False(t, edge.Provisional, "another recording file supplies no carry evidence for this projection")
	}
}

func TestSelectedProjectionCallerDeadlineRetainsEntry(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	entry := selectedPendingEntry(t, stack)
	publishSelectedEntry(t, stack, entry, false)
	ctx, view := routedEnrichmentView(t, stack)
	probe := &selectedRetirementProbe{Reader: view.reader, waitMetadataContext: true}
	view.reader = probe
	view.materialized.Reader = probe
	ctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := stack.srv.ensureProjected(ctx, []string{entry.graphPath})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Greater(t, probe.metadataCalls, 0, "the caller's deadline interrupts actual selected proof")
	require.Less(t, time.Since(started), time.Second)
	require.Same(t, entry, liveEntry(pendingFileProjections, entry.absPath))
}

func TestProjectedCarryCallerDeadlineIsNotOptionalFallback(t *testing.T) {
	srv, ctx, _ := projectedCarryFixture(t, false)
	probe := &selectedRetirementProbe{Reader: srv.graph, waitNodesContext: true}
	ctx = withRequestView(ctx, &requestView{reader: probe, viewRoot: srv.indexer.RootPath(), kind: requestViewKindWorktree})
	ctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	_, err := srv.ensureProjected(ctx, []string{"caller.go"})
	require.ErrorIs(t, err, context.DeadlineExceeded, "actual cancellation is never suppressed by optional carry compatibility")
}
