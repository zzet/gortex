package mcp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// Seed a static ready chain from a REAL accepted payload, while no lifecycle
// owns a coordinator. CopyPayloadGeneration carries the accepted nodes/edges,
// masks, source metadata and complete input manifest; Publish seals/validates
// each member. Every layer replaces the same accepted bytes, so the composed
// state is exact. This is fixture setup, never completion of a pending ticket.
func seedMutationFoldCap(t *testing.T, f *realCheckoutMutationFixture) store_sqlite.CheckoutRoute {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	catalog := f.store.Catalog()
	route, found, err := catalog.GetCheckoutRoute(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.RouteActive, route.State)
	source, found, err := catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, route.CommitGenerationID, source.BaseGenerationID)
	require.Equal(t, f.checkoutID, source.CheckoutID)
	accepted := f.store.AtGeneration(source.GenerationID)
	meta, entries, found, err := accepted.InputManifest(ctx)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, meta.IsFull)
	require.GreaterOrEqual(t, len(entries), 3)
	for _, entry := range entries {
		if entry.State == store_sqlite.InputManifestPresent && entry.Admission == store_sqlite.InputManifestAdmitted {
			bytes, readErr := os.ReadFile(filepath.Join(f.worktree, filepath.FromSlash(entry.FilePath)))
			require.NoError(t, readErr)
			// DirtyContent/InputManifest hashes use the Git blob framing of the
			// actual raw bytes, not a bare content SHA256 (gitstate.hashDirtyReader).
			blob := append([]byte(fmt.Sprintf("blob %d\x00", len(bytes))), bytes...)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(blob)), entry.ContentSHA256)
		}
	}
	// The old owner is fully joined before any fixture payload or route write.
	// No background reservation can survive that join.
	require.NoError(t, f.srv.lifecycle.Close())
	parent := source.GenerationID
	for depth := 2; depth <= graphview.MaxDirtyChainDepth; depth++ {
		id, handle, beginErr := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
			OwnerKind: source.OwnerKind, GraphID: source.GraphID, LayerID: source.LayerID,
			CheckoutID: source.CheckoutID, GenerationKind: source.GenerationKind,
			BaseGenerationID: parent, LowerViewFingerprint: source.LowerViewFingerprint,
			TreeOID: source.TreeOID, ProvenanceCommitOID: source.ProvenanceCommitOID,
			ConfigHash: source.ConfigHash, ExtractorVersions: source.ExtractorVersions,
			ResolverVersion: source.ResolverVersion, DependencyRevision: source.DependencyRevision,
			CreatedAt: time.Now().Unix(),
		})
		require.NoError(t, beginErr)
		counts, copyErr := f.store.CopyPayloadGeneration(ctx, source.GenerationID, id, "repo")
		require.NoError(t, copyErr)
		require.Positive(t, counts.Nodes)
		copiedMeta, copiedEntries, copied, readErr := handle.InputManifest(ctx)
		require.NoError(t, readErr)
		require.True(t, copied)
		require.Equal(t, meta, copiedMeta)
		require.Equal(t, entries, copiedEntries)
		for _, symbol := range []string{"repo/edit.go::New", "repo/stable1.go::Stable1", "repo/stable2.go::Stable2"} {
			require.NotNil(t, accepted.GetNode(symbol))
			require.Equal(t, accepted.GetNode(symbol), handle.GetNode(symbol))
		}
		require.NoError(t, f.store.PublishPayloadGeneration(ctx, id, time.Now().Unix()))
		parent = id
	}
	route.DirtyGenerationID = parent
	route.RouteEpoch++
	require.NoError(t, catalog.UpsertCheckoutRoute(ctx, route))
	return route
}

// The second default edit waits on the first edit's real withdrawn route.
// That publication must copy/verify the actual chain at the physical cap, not
// succeed by timing out its fold and rebuilding direct. On the original
// wrapper, the second edit's ordinary announcement refuses that inline Begin.
// The inline copier returns that error without retrying Begin; its direct
// fallback fails the folded-parent oracle even if both disk commits succeed.
// The production three-second fold budget is unchanged.
func TestDefaultWorktreeMutationWaitingForRouteDoesNotBlockRequiredFold(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f := newRealCheckoutMutationFixtureWithSetup(t, nil, func(_ *indexer.CheckoutLifecycle, worktree string) {
		// These real files enter the initial accepted dirty census. Later
		// one-file edits leave them unchanged; no catalog/manifest is seeded.
		for i := 1; i <= 2; i++ {
			path := filepath.Join(worktree, fmt.Sprintf("stable%d.go", i))
			src := fmt.Sprintf("package repo\n\nfunc Stable%d() {}\n", i)
			require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
		}
	})
	f.srv.mutationReindexWait = 20 * time.Millisecond
	primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	capped := seedMutationFoldCap(t, f)
	gate := indexer.NewViewBuildGate()
	gate.Open()
	gateCtx, cancelGate := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancelGate)
	releaseLane, err := gate.Acquire(gateCtx, indexer.ViewBuildInteractive)
	require.NoError(t, err)
	releaseLane = sync.OnceFunc(releaseLane)
	t.Cleanup(releaseLane)
	fresh, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer: f.srv.multiIndexer, ConfigManager: f.srv.configManager,
		Graph: f.store, Logger: f.srv.logger, ViewLeases: f.srv.materializer.Leases,
	})
	require.NoError(t, err)
	fresh.SetBuildGate(gate)
	t.Cleanup(func() { _ = fresh.Close() })
	f.srv.lifecycle = fresh
	require.True(t, fresh.ActivateCheckout(f.checkoutID, "static-fold-cap-regression"))
	checkout, found, err := f.store.Catalog().GetCheckout(context.Background(), f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Eventually(t, func() bool { return fresh.LiveCoordinators(checkout.FamilyID) > 0 }, 20*time.Second, time.Millisecond,
		"the fresh lifecycle must own the persisted accepted route")
	ctx := context.Background()
	catalog := f.store.Catalog()
	route := func() store_sqlite.CheckoutRoute {
		row, found, readErr := catalog.GetCheckoutRoute(ctx, f.checkoutID)
		require.NoError(t, readErr)
		require.True(t, found)
		return row
	}
	require.Equal(t, capped, route(), "fresh activation must preserve the exact ready static chain")
	oldName := "New"
	chain := make([]int64, 0, graphview.MaxDirtyChainDepth)
	for id := capped.DirtyGenerationID; id != capped.CommitGenerationID; {
		row, found, readErr := catalog.GetViewGeneration(ctx, id)
		require.NoError(t, readErr)
		require.True(t, found)
		require.Equal(t, f.checkoutID, row.CheckoutID)
		require.Equal(t, indexer.DirtyLayerGenerationKind, row.GenerationKind)
		require.Contains(t, []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady, store_sqlite.ViewGenerationSuperseded}, row.State)
		chain = append(chain, id)
		require.LessOrEqual(t, len(chain), graphview.MaxDirtyChainDepth)
		id = row.BaseGenerationID
	}
	require.Len(t, chain, graphview.MaxDirtyChainDepth)
	require.Zero(t, gate.Stats().AdmittedBackground, "a settled persisted route owes no background build or fold reservation")

	first := f.facade(t, f.worktree, "edit", map[string]any{
		"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
		"match": "func " + oldName + "() {}", "replacement": "func FoldFirst() {}",
	})
	firstPayload := lifecycleResultPayload(t, first)
	require.Equal(t, "pending", firstPayload["graph_status"])
	require.Eventually(t, func() bool { return gate.Stats().InteractiveQueued >= 1 }, time.Second, time.Millisecond,
		"first refresh must queue behind the held lane")
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce, enteredOnce sync.Once
	var waitEntries atomic.Int32
	unblock := func() { resumeOnce.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	f.srv.mutationRouteWaitEntered = func(requestCtx context.Context) {
		waitEntries.Add(1)
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-resume:
		case <-requestCtx.Done():
		}
	}
	secondDone := make(chan *mcplib.CallToolResult, 1)
	secondFinished := make(chan struct{})
	secondCtx, secondCancel := context.WithTimeout(WithSessionCWD(WithSessionID(ctx, "fold-second-default"), f.worktree), 30*time.Second)
	t.Cleanup(func() {
		secondCancel()
		unblock()
		select {
		case <-secondFinished:
		case <-time.After(30 * time.Second):
			t.Error("second mutation handler did not join before lifecycle cleanup")
		}
	})
	tool := f.srv.mcpServer.GetTool("edit")
	require.NotNil(t, tool)
	go func() {
		defer close(secondFinished)
		req := mcplib.CallToolRequest{}
		req.Params.Name, req.Params.Arguments = "edit", map[string]any{
			"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
			"match": "func FoldFirst() {}", "replacement": "func FoldSecond() {}",
		}
		result, callErr := tool.Handler(secondCtx, req)
		if callErr != nil {
			result = mcplib.NewToolResultError(callErr.Error())
		}
		secondDone <- result
	}()
	select {
	case <-entered:
	case <-secondCtx.Done():
		t.Fatal("second default linked-CWD mutation did not enter the actual route wait")
	}
	require.Equal(t, int32(1), waitEntries.Load(), "the witnessed entry belongs to the second request")
	pending := route()
	require.Equal(t, store_sqlite.RoutePending, pending.State)
	require.Equal(t, capped.CommitGenerationID, pending.CommitGenerationID)
	require.Zero(t, pending.DirtyGenerationID)
	bytesBeforeSecond, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(bytesBeforeSecond), "func FoldFirst() {}")
	require.NotContains(t, string(bytesBeforeSecond), "FoldSecond")
	require.Zero(t, gate.Stats().AdmittedBackground)
	require.Zero(t, gate.Stats().BackgroundQueued, "no background compactor may race inline Begin")
	require.GreaterOrEqual(t, gate.Stats().InteractiveQueued, 1, "the real first refresh still needs the lane")
	releaseLane()
	f.awaitMutation(t, f.worktree, first)
	firstRoute := route()
	firstRow, found, err := catalog.GetViewGeneration(ctx, firstRoute.DirtyGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	folded, found, err := catalog.GetViewGeneration(ctx, firstRow.BaseGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEqual(t, capped.CommitGenerationID, firstRow.BaseGenerationID,
		"a successful direct fallback after inline Begin refusal does not satisfy progress")
	require.NotContains(t, chain, folded.GenerationID)
	require.Equal(t, indexer.DirtyLayerGenerationKind, folded.GenerationKind)
	require.Equal(t, f.checkoutID, folded.CheckoutID)
	require.Equal(t, capped.CommitGenerationID, folded.BaseGenerationID, "the first publication must have depth two over a real verified fold")
	foldStore := f.store.AtGeneration(folded.GenerationID)
	require.NotNil(t, foldStore.GetNode("repo/edit.go::"+oldName), "fold must retain the real cap input, before either overlap edit")
	require.Nil(t, foldStore.GetNode("repo/edit.go::FoldFirst"))
	for i := 1; i <= 2; i++ {
		require.NotNil(t, foldStore.GetNode(fmt.Sprintf("repo/stable%d.go::Stable%d", i, i)), "verified fold retains accepted stable dirty declarations")
	}
	selected := f.store.AtGeneration(firstRoute.DirtyGenerationID)
	require.NotNil(t, selected.GetNode("repo/edit.go::FoldFirst"))
	unblock()
	select {
	case second := <-secondDone:
		lifecycleResultPayload(t, second)
		f.awaitMutation(t, f.worktree, second)
	case <-secondCtx.Done():
		t.Fatal("second default mutation did not finish inside its original deadline")
	}
	require.Equal(t, int32(1), waitEntries.Load(), "the second request completed one pending-route selection")
	for i := 1; i <= 2; i++ {
		stable, readErr := os.ReadFile(filepath.Join(f.worktree, fmt.Sprintf("stable%d.go", i)))
		require.NoError(t, readErr)
		require.Equal(t, fmt.Sprintf("package repo\n\nfunc Stable%d() {}\n", i), string(stable))
		_, primaryErr := os.Stat(filepath.Join(f.primary, fmt.Sprintf("stable%d.go", i)))
		require.True(t, os.IsNotExist(primaryErr), "worktree-only dirty file must not appear in primary")
		require.Nil(t, f.store.GetNode(fmt.Sprintf("repo/stable%d.go::Stable%d", i, i)))
	}
	finalBytes, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(finalBytes), "func FoldSecond() {}")
	finalStore := f.store.AtGeneration(route().DirtyGenerationID)
	require.NotNil(t, finalStore.GetNode("repo/edit.go::FoldSecond"))
	primaryAfter, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	require.Equal(t, primaryBefore, primaryAfter)
	require.NotNil(t, f.store.GetNode("repo/edit.go::Old"))
	require.Nil(t, f.store.GetNode("repo/edit.go::FoldFirst"))
	require.Nil(t, f.store.GetNode("repo/edit.go::FoldSecond"))
}
