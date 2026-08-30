package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func publishStatusBase(t *testing.T, f *probeFixture, tree string) int64 {
	t.Helper()
	ctx := context.Background()
	id, handle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        probeGraphID,
		LayerID:        probeGraphID + ":base",
		CheckoutID:     probePrimaryID,
		GenerationKind: "dedicated_base",
		TreeOID:        tree,
		CreatedAt:      1000,
	})
	require.NoError(t, err)
	files := []graph.FileMetaRow{
		{FilePath: probePrefix + "/a.go", Size: 10, NodeCount: 10},
		{FilePath: probePrefix + "/b.go", Size: 20, NodeCount: 14},
		{FilePath: probePrefix + "/c.go", Size: 30, NodeCount: 17},
	}
	var nodes []*graph.Node
	for fileIndex, count := range []int{10, 14, 17} {
		for nodeIndex := 0; nodeIndex < count; nodeIndex++ {
			file := files[fileIndex].FilePath
			nodes = append(nodes, &graph.Node{
				ID: fmt.Sprintf("%s::N%02d", file, len(nodes)), Kind: graph.KindFunction,
				Name: fmt.Sprintf("N%02d", len(nodes)), FilePath: file, RepoPrefix: probePrefix,
			})
		}
	}
	edges := make([]*graph.Edge, 55)
	for i := range edges {
		source := nodes[i%len(nodes)]
		edges[i] = &graph.Edge{
			From: source.ID, To: nodes[(i+1)%len(nodes)].ID, Kind: graph.EdgeCalls,
			FilePath: source.FilePath, Line: i + 1,
		}
	}
	handle.AddBatch(nodes, edges)
	require.NoError(t, handle.SetFileMetas(probePrefix, files))
	masks := make([]store_sqlite.FileMask, 0, len(files))
	for _, file := range files {
		masks = append(masks, store_sqlite.FileMask{
			RepoPrefix: probePrefix, FilePath: file.FilePath, Mode: store_sqlite.OwnershipReplace,
		})
	}
	require.NoError(t, handle.SetFileMasks(masks))
	require.NoError(t, handle.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: probePrefix, IndexedAt: 1999, NodeCount: 41, EdgeCount: 55,
	}))
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, id, 2000))
	graphRow, found, err := f.catalog.GetDedicatedGraph(ctx, probeGraphID)
	require.NoError(t, err)
	require.True(t, found)
	graphRow.ActiveGenerationID = id
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, graphRow))
	return id
}

func publishStatusLayer(
	t *testing.T,
	f *probeFixture,
	checkoutID, kind, tree string,
	base int64,
	changed bool,
) int64 {
	t.Helper()
	ctx := context.Background()
	id, handle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          probeGraphID,
		LayerID:          fmt.Sprintf("status-%s-%s", checkoutID, kind),
		CheckoutID:       checkoutID,
		GenerationKind:   kind,
		BaseGenerationID: base,
		TreeOID:          tree,
		CreatedAt:        base + 2000,
	})
	require.NoError(t, err)
	if changed {
		file := probePrefix + "/changed.go"
		require.NoError(t, handle.SetFileMetas(probePrefix, []graph.FileMetaRow{{
			FilePath: file, Size: 7, NodeCount: 1,
		}}))
		require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{
			RepoPrefix: probePrefix, FilePath: file, Mode: store_sqlite.OwnershipReplace,
		}}))
		require.NoError(t, handle.SetRepoIndexState(graph.RepoIndexState{
			RepoPrefix: probePrefix, IndexedAt: base + 3000, NodeCount: 1, EdgeCount: 2,
		}))
	}
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, id, id+3000))
	return id
}

func routeStatusCheckout(
	t *testing.T, f *probeFixture, checkoutID, tree string, commit, dirty int64,
) {
	t.Helper()
	ctx := context.Background()
	checkout, found, err := f.catalog.GetCheckout(ctx, checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	checkout.HeadTree = tree
	require.NoError(t, f.catalog.UpsertCheckout(ctx, checkout))
	require.NoError(t, f.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID: checkoutID, GraphID: probeGraphID,
		CommitGenerationID: commit, DirtyGenerationID: dirty,
		State: store_sqlite.RouteActive,
	}))
}

func routedStatusForPath(t *testing.T, rows []routedRepoStatus, path string) routedRepoStatus {
	t.Helper()
	row, found := matchRoutedRepoStatus(path, "", rows)
	require.True(t, found)
	return row
}

func TestRoutedStatusUsesPopulatedDedicatedGenerationInsteadOfEmptyShell(t *testing.T) {
	f := newProbeFixture(t)
	const tree = "tree-status-dedicated"
	base := publishStatusBase(t, f, tree)
	commit := publishStatusLayer(t, f, probePrimaryID, "commit", tree, base, false)
	dirty := publishStatusLayer(t, f, probePrimaryID, "dirty", tree, commit, false)
	routeStatusCheckout(t, f, probePrimaryID, tree, commit, dirty)

	snapshot := f.controller.collectRoutedRepoStatuses(context.Background())
	require.True(t, snapshot.enabled)
	require.True(t, snapshot.available)
	view := routedStatusForPath(t, snapshot.rows, f.primaryRoot)
	assert.Equal(t, daemon.RepoViewStateReady, view.viewState)
	assert.True(t, view.countsKnown)
	assert.Equal(t, []int64{base, commit, dirty}, view.generations)
	assert.Equal(t, 3, view.files)
	assert.Equal(t, 41, view.nodes)
	assert.Equal(t, 55, view.edges)

	row := daemon.TrackedRepoStatus{
		Prefix: probePrefix, Path: f.primaryRoot, Workspace: probePrefix,
		WorkspaceProject: probePrefix, Unloaded: true,
	}
	applyRoutedRepoStatus(&row, view)
	assert.False(t, row.Unloaded)
	assert.True(t, repoStatusCountsKnown(row))
	assert.False(t, repoStatusMemoryKnown(row),
		"search/vector heap cannot be attributed to one selected generation")
	assert.Equal(t, daemon.MemoryBreakdown{}, row.Memory)
	assert.Equal(t, "ok", repoStateLabel(row))
	assert.False(t, repoIndexIsEmpty(row), "an empty generation-zero shell is not the selected graph")
	assert.Equal(t, 41, row.Nodes)

	body, err := json.Marshal(row)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"counts_known":true`)
	assert.Contains(t, string(body), `"memory_known":false`)

	var rendered strings.Builder
	renderDaemonRepos(&rendered, daemon.StatusResponse{TrackedRepos: []daemon.TrackedRepoStatus{row}})
	assert.Equal(t, 5, strings.Count(rendered.String(), "?"),
		"only total and the four memory columns are unknown; structural counts stay numeric")

	model, _ := newStatusTUI(time.Second).Update(tuiStatusMsg(daemon.StatusResponse{
		TrackedRepos: []daemon.TrackedRepoStatus{row},
	}))
	tui := model.(statusTUI)
	require.Len(t, tui.repos.Items(), 1)
	item := tui.repos.Items()[0].(repoItem)
	assert.Equal(t, "?", item.memory)
	assert.Equal(t, 3, item.files)
	assert.Equal(t, 41, item.nodes)
	assert.Equal(t, 55, item.edges)
	assert.Empty(t, item.state, "unknown attribution is not a broken route")
}

func TestRoutedStatusMarksChangedAutomaticOverlayCountsUnavailable(t *testing.T) {
	f := newProbeFixture(t)
	const tree = "tree-status-automatic"
	base := publishStatusBase(t, f, tree)
	commit := publishStatusLayer(t, f, probeWorktreeID, "commit", tree, base, false)
	dirty := publishStatusLayer(t, f, probeWorktreeID, "dirty", tree, commit, true)
	routeStatusCheckout(t, f, probeWorktreeID, tree, commit, dirty)

	snapshot := f.controller.collectRoutedRepoStatuses(context.Background())
	view := routedStatusForPath(t, snapshot.rows, f.worktreeRoot)
	assert.Equal(t, daemon.RepoViewStateReady, view.viewState)
	assert.False(t, view.countsKnown,
		"sparse replacement totals must not be manufactured by summing physical generations")
	assert.Equal(t, []int64{base, commit, dirty}, view.generations)

	row := daemon.TrackedRepoStatus{Prefix: probePrefix, Path: f.worktreeRoot, Unloaded: true}
	applyRoutedRepoStatus(&row, view)
	assert.False(t, row.Unloaded)
	assert.False(t, repoStatusCountsKnown(row))
	assert.False(t, repoStatusMemoryKnown(row))
	assert.Equal(t, "view counts unavailable", repoStateLabel(row))
	assert.Equal(t, "ready — view counts unavailable", repoItemState(row))
	assert.False(t, repoIndexIsEmpty(row))

	workspaces := workspaceSummaries([]daemon.TrackedRepoStatus{row})
	require.Len(t, workspaces, 1)
	assert.False(t, workspaceStatusCountsKnown(workspaces[0]))
}

func TestRoutedStatusCatalogFailureDegradesOnlyLastKnownRoutedRows(t *testing.T) {
	f := newProbeFixture(t)
	initial := f.controller.collectRoutedRepoStatuses(context.Background())
	require.True(t, initial.available)
	require.NotEmpty(t, initial.rows)
	_, found := matchRoutedRepoStatus(f.worktreeRoot, "", initial.rows)
	require.True(t, found)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failed := f.controller.collectRoutedRepoStatuses(ctx)
	require.True(t, failed.enabled)
	require.False(t, failed.available)
	require.NotEmpty(t, failed.rows, "the last successful identity set scopes degradation")

	rows := []daemon.TrackedRepoStatus{
		{
			Prefix: "shell", Path: f.worktreeRoot, Files: 9, Nodes: 90, Edges: 99,
			Memory: daemon.MemoryBreakdown{TotalBytes: 1000}, LastIndex: 123,
		},
		{
			Prefix: "legacy", Path: "/not/a/git/checkout", Files: 4, Nodes: 40, Edges: 44,
			Memory: daemon.MemoryBreakdown{TotalBytes: 500}, LastIndex: 456,
		},
	}
	legacy := rows[1]
	assert.Equal(t, 1, projectRoutedRepoRows(rows, failed))
	assert.Equal(t, daemon.RepoViewStateDegraded, rows[0].ViewState)
	assert.False(t, repoStatusCountsKnown(rows[0]))
	assert.False(t, repoStatusMemoryKnown(rows[0]))
	assert.Zero(t, rows[0].Files)
	assert.Zero(t, rows[0].Memory.TotalBytes)
	assert.Equal(t, legacy, rows[1],
		"a catalog failure does not invalidate independent generation-zero truth")
}

func TestRoutedStatusFirstCatalogFailureLeavesUnmatchedLegacyRowsAlone(t *testing.T) {
	f := newProbeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failed := f.controller.collectRoutedRepoStatuses(ctx)
	require.True(t, failed.enabled)
	require.False(t, failed.available)
	assert.Empty(t, failed.rows, "there is no identity cache before the first successful read")

	rows := []daemon.TrackedRepoStatus{{
		Prefix: "legacy", Path: "/not/a/git/checkout", Files: 4, Nodes: 40, Edges: 44,
		Memory: daemon.MemoryBreakdown{TotalBytes: 500}, LastIndex: 456,
	}}
	want := rows[0]
	assert.Zero(t, projectRoutedRepoRows(rows, failed))
	assert.Equal(t, want, rows[0])
}

func TestRoutedStatusBuildingNeverRendersNotIndexedOrEmpty(t *testing.T) {
	row := daemon.TrackedRepoStatus{Unloaded: true, Files: 0, LastIndex: 100}
	applyRoutedRepoStatus(&row, routedRepoStatus{viewState: daemon.RepoViewStateBuilding})
	assert.Equal(t, "view building", repoStateLabel(row))
	assert.Equal(t, "view building", repoItemState(row))
	assert.False(t, repoIndexIsEmpty(row))
}

func BenchmarkRoutedStatusAggregation256Repos(b *testing.B) {
	const repos = 256
	baseRows := make([]daemon.TrackedRepoStatus, repos)
	views := make([]routedRepoStatus, repos)
	for i := 0; i < repos; i++ {
		prefix := fmt.Sprintf("repo-%03d", i)
		path := fmt.Sprintf("/status/%03d", i)
		baseRows[i] = daemon.TrackedRepoStatus{
			Prefix: prefix, Path: path, Workspace: "workspace", WorkspaceProject: prefix,
		}
		views[i] = routedRepoStatus{
			checkoutID: fmt.Sprintf("checkout-%03d", i), rootPath: path, repoPrefix: prefix,
			viewState: daemon.RepoViewStateReady, countsKnown: i%2 == 0,
			generations: []int64{1, int64(i*2 + 2), int64(i*2 + 3)},
			files:       10, nodes: 100, edges: 200,
		}
	}
	snapshot := routedRepoStatusSnapshot{enabled: true, available: true, rows: views}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows := append([]daemon.TrackedRepoStatus(nil), baseRows...)
		projectRoutedRepoRows(rows, snapshot)
		if got := len(workspaceSummaries(rows)); got != 1 {
			b.Fatalf("workspace summaries = %d", got)
		}
	}
}
