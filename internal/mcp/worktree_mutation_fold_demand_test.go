package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

type mutationFoldGateGrant struct {
	release func()
	err     error
}

// Keep interactive waiters queued while real refreshes construct the chain.
// awaitForegroundQuiet observes this queue, so no background compactor can
// begin/reserve the Store fold. Each handoff is observed; no quiet-window sleep
// or assumption about cancellation of a queued stepped compactor is needed.
type mutationFoldGateBaton struct {
	gate    *indexer.ViewBuildGate
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	ready   chan mutationFoldGateGrant
	held    func()
}

func newMutationFoldGateBaton(t *testing.T, gate *indexer.ViewBuildGate) *mutationFoldGateBaton {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	b := &mutationFoldGateBaton{gate: gate, ctx: ctx, cancel: cancel, ready: make(chan mutationFoldGateGrant, 64)}
	var err error
	b.held, err = gate.Acquire(ctx, indexer.ViewBuildInteractive)
	require.NoError(t, err)
	t.Cleanup(b.close)
	b.queue(t)
	b.queue(t)
	return b
}

func (b *mutationFoldGateBaton) queue(t *testing.T) {
	t.Helper()
	before := b.gate.Stats().InteractiveQueued
	b.workers.Add(1)
	go func() {
		defer b.workers.Done()
		release, err := b.gate.Acquire(b.ctx, indexer.ViewBuildInteractive)
		b.ready <- mutationFoldGateGrant{release: release, err: err}
	}()
	require.Eventually(t, func() bool { return b.gate.Stats().InteractiveQueued > before }, time.Second, time.Millisecond,
		"test waiter must be observed queued before handing off the lane")
}

func (b *mutationFoldGateBaton) release() {
	if b.held != nil {
		b.held()
		b.held = nil
	}
}

func (b *mutationFoldGateBaton) advance(t *testing.T, published func() bool) {
	t.Helper()
	for {
		require.GreaterOrEqual(t, b.gate.Stats().InteractiveQueued, 2)
		b.release()
		select {
		case grant := <-b.ready:
			require.NoError(t, grant.err)
			b.held = grant.release
		case <-b.ctx.Done():
			t.Fatal("real refresh did not hand the lane back: ", b.ctx.Err())
		}
		// One other waiter remained queued while this one was granted. Restore
		// the second before another handoff, including while a refresh owns it.
		b.queue(t)
		if published() {
			return
		}
	}
}

func (b *mutationFoldGateBaton) stopWaiters() {
	b.cancel()
	b.workers.Wait()
	for {
		select {
		case grant := <-b.ready:
			if grant.release != nil {
				grant.release()
			}
		default:
			return
		}
	}
}

func (b *mutationFoldGateBaton) close() {
	b.stopWaiters()
	b.release()
}

// The second default edit waits on the first edit's real withdrawn route.
// That publication must copy/verify the actual chain at the physical cap, not
// succeed by timing out its fold and rebuilding direct. On the original
// wrapper, the second edit's ordinary announcement prevents that inline Begin
// until its unchanged three-second budget expires; the folded-parent oracle
// fails even if both tools eventually report successful disk commits.
func TestDefaultWorktreeMutationWaitingForRouteDoesNotBlockRequiredFold(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	gate := indexer.NewViewBuildGate()
	gate.Open()
	f := newRealCheckoutMutationFixtureWithSetup(t, nil, func(l *indexer.CheckoutLifecycle) { l.SetBuildGate(gate) })
	f.srv.mutationReindexWait = 20 * time.Millisecond
	primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	baton := newMutationFoldGateBaton(t, gate)
	backgroundBefore := gate.Stats().AdmittedBackground
	ctx := context.Background()
	catalog := f.store.Catalog()
	route := func() store_sqlite.CheckoutRoute {
		row, found, readErr := catalog.GetCheckoutRoute(ctx, f.checkoutID)
		require.NoError(t, readErr)
		require.True(t, found)
		return row
	}
	oldTop := route().DirtyGenerationID
	oldName := "New"
	for depth := 2; depth <= graphview.MaxDirtyChainDepth; depth++ {
		name := fmt.Sprintf("FoldSetup%d", depth)
		result := f.facade(t, f.worktree, "edit", map[string]any{
			"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
			"match": "func " + oldName + "() {}", "replacement": "func " + name + "() {}",
		})
		lifecycleResultPayload(t, result)
		previous := oldTop
		baton.advance(t, func() bool {
			r := route()
			return r.State == store_sqlite.RouteActive && r.DirtyGenerationID > 0 && r.DirtyGenerationID != previous
		})
		f.awaitMutation(t, f.worktree, result)
		r := route()
		row, found, readErr := catalog.GetViewGeneration(ctx, r.DirtyGenerationID)
		require.NoError(t, readErr)
		require.True(t, found)
		require.Equal(t, previous, row.BaseGenerationID, "setup must really chain, never fabricate or rebuild direct")
		require.Equal(t, f.checkoutID, row.CheckoutID)
		oldTop, oldName = r.DirtyGenerationID, name
	}
	capped := route()
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
	require.Equal(t, backgroundBefore, gate.Stats().AdmittedBackground, "no background lane admission means no background Store fold reservation")

	first := f.facade(t, f.worktree, "edit", map[string]any{
		"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
		"match": "func " + oldName + "() {}", "replacement": "func FoldFirst() {}",
	})
	firstPayload := lifecycleResultPayload(t, first)
	require.Equal(t, "pending", firstPayload["graph_status"])
	require.Eventually(t, func() bool { return gate.Stats().InteractiveQueued >= 3 }, time.Second, time.Millisecond,
		"first refresh must queue behind the held lane and two setup waiters")
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	unblock := func() { resumeOnce.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	f.srv.mutationRouteWaitEntered = func(requestCtx context.Context) {
		close(entered)
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
	pending := route()
	require.Equal(t, store_sqlite.RoutePending, pending.State)
	require.Equal(t, capped.CommitGenerationID, pending.CommitGenerationID)
	require.Zero(t, pending.DirtyGenerationID)
	bytesBeforeSecond, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(bytesBeforeSecond), "func FoldFirst() {}")
	require.NotContains(t, string(bytesBeforeSecond), "FoldSecond")
	require.Equal(t, backgroundBefore, gate.Stats().AdmittedBackground)
	baton.stopWaiters()
	require.GreaterOrEqual(t, gate.Stats().InteractiveQueued, 1, "only the real first refresh still needs the lane")
	baton.release()
	f.awaitMutation(t, f.worktree, first)
	firstRoute := route()
	firstRow, found, err := catalog.GetViewGeneration(ctx, firstRoute.DirtyGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	folded, found, err := catalog.GetViewGeneration(ctx, firstRow.BaseGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEqual(t, capped.CommitGenerationID, firstRow.BaseGenerationID,
		"a successful direct fallback after the three-second fold budget does not satisfy progress")
	require.NotContains(t, chain, folded.GenerationID)
	require.Equal(t, indexer.DirtyLayerGenerationKind, folded.GenerationKind)
	require.Equal(t, f.checkoutID, folded.CheckoutID)
	require.Equal(t, capped.CommitGenerationID, folded.BaseGenerationID, "the first publication must have depth two over a real verified fold")
	foldStore := f.store.AtGeneration(folded.GenerationID)
	require.NotNil(t, foldStore.GetNode("repo/edit.go::"+oldName), "fold must retain the real cap input, before either overlap edit")
	require.Nil(t, foldStore.GetNode("repo/edit.go::FoldFirst"))
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
