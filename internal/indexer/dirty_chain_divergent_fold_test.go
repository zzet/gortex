package indexer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func divergentFoldWait(t *testing.T, ready <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Reuse a previously accepted parent, then publish a different child. The
// old fold's last member is no longer in the active chain, like undo followed
// by a new edit; neither its input rows nor its landing can replace this view.
func divergentFoldBranch(t *testing.T, f *coordinatorFixture, l *CheckoutLifecycle) CheckoutCycle {
	t.Helper()
	mcpEdit(t, l, f, func() {
		require.NoError(t, os.Rename(filepath.Join(f.worktree, "newname.go"), filepath.Join(f.worktree, "oldname.go")))
	})
	return mcpEdit(t, l, f, func() { foldWiringEdits[4](t, f) })
}

func TestDivergentSteppedFoldReplacementJoinsBeforeStartingAndPreservesSelectedRows(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var trigger CheckoutCycle
	for i := 0; i < 4; i++ {
		trigger = mcpEdit(t, l, f, func() { foldWiringEdits[i](t, f) })
	}
	oldBegun, oldCanceled, oldRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	newBegun, newRelease := make(chan struct{}), make(chan struct{})
	var beginnings atomic.Int32
	c.compaction.mu.Lock()
	c.compaction.closed = false
	c.compaction.stepHook = func(ctx context.Context, step int) {
		if step != 0 {
			return
		}
		switch beginnings.Add(1) {
		case 1:
			close(oldBegun)
			<-ctx.Done()
			close(oldCanceled)
			// Model slow cleanup: the fold still owns its storage slot and
			// input leases after cancellation until this worker unwinds.
			<-oldRelease
		case 2:
			close(newBegun)
			select {
			case <-newRelease:
			case <-ctx.Done():
			}
		}
	}
	c.compaction.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-oldRelease:
		default:
			close(oldRelease)
		}
		select {
		case <-newRelease:
		default:
			close(newRelease)
		}
	})
	require.True(t, c.scheduleDirtyChainCompaction(trigger))
	divergentFoldWait(t, oldBegun, "old fold's storage ownership")
	c.compaction.mu.Lock()
	oldDone := c.compaction.running
	c.compaction.mu.Unlock()
	branch := divergentFoldBranch(t, f, l)
	divergentFoldWait(t, oldCanceled, "divergent fold cancellation")
	select {
	case <-newBegun:
		t.Fatal("replacement started before old storage ownership was released")
	default:
	}
	c.compaction.mu.Lock()
	newDone := c.compaction.running
	c.compaction.mu.Unlock()
	require.NotEqual(t, oldDone, newDone)
	// A further publication must coalesce into the replacement already
	// waiting on oldDone. Canceling that queued task would let a second
	// replacement join its early done channel and bypass old storage cleanup.
	latest := mcpEdit(t, l, f, func() { foldWiringEdits[5](t, f) })
	c.compaction.mu.Lock()
	running, owner := c.compaction.running, c.compaction.foldOwner
	c.compaction.mu.Unlock()
	require.Equal(t, newDone, running)
	require.Equal(t, oldDone, owner)
	select {
	case <-newDone:
		t.Fatal("queued replacement completed before original fold cleanup")
	case <-newBegun:
		t.Fatal("further publication bypassed original fold cleanup")
	default:
	}
	close(oldRelease)
	divergentFoldWait(t, newBegun, "replacement fold after old cleanup")
	divergentFoldWait(t, oldDone, "old scheduled task completion")
	c.compaction.mu.Lock()
	require.NotNil(t, c.compaction.cancel, "old cleanup must not clear replacement task authority")
	require.Equal(t, newDone, c.compaction.running)
	c.compaction.mu.Unlock()
	close(newRelease)
	divergentFoldWait(t, newDone, "replacement publication")
	stats := c.DirtyChainCompactionStats()
	require.Equal(t, 2, stats.Scheduled, "further publications must coalesce into the queued replacement")
	require.Equal(t, int32(2), beginnings.Load())
	require.GreaterOrEqual(t, stats.Canceled, 1)
	require.GreaterOrEqual(t, stats.Flipped, 1)
	require.NotEqual(t, trigger.DirtyGenerationID, f.route().DirtyGenerationID)
	require.NotEqual(t, branch.DirtyGenerationID, f.route().DirtyGenerationID, "replacement must actually compact the accepted branch")
	require.NotEqual(t, latest.DirtyGenerationID, f.route().DirtyGenerationID, "queued replacement must compact the latest accepted branch")
	chainAssertFlat(t, f, "divergent-fold-replacement")
}

func TestUsefulSteppedFoldPrefixIsKeptWhenAnEditPublishesAboveIt(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var trigger CheckoutCycle
	for i := 0; i < 4; i++ {
		trigger = mcpEdit(t, l, f, func() { foldWiringEdits[i](t, f) })
	}
	begun, proceed := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	c.compaction.mu.Lock()
	c.compaction.closed = false
	c.compaction.stepHook = func(ctx context.Context, step int) {
		if step == 0 && starts.Add(1) == 1 {
			close(begun)
			select {
			case <-proceed:
			case <-ctx.Done():
			}
		}
	}
	c.compaction.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	})
	require.True(t, c.scheduleDirtyChainCompaction(trigger))
	divergentFoldWait(t, begun, "useful fold")
	c.compaction.mu.Lock()
	done := c.compaction.running
	c.compaction.mu.Unlock()
	mcpEdit(t, l, f, func() { foldWiringEdits[4](t, f) })
	c.compaction.mu.Lock()
	require.Equal(t, done, c.compaction.running, "same ancestry must not restart a useful fold")
	c.compaction.mu.Unlock()
	close(proceed)
	divergentFoldWait(t, done, "useful fold landing")
	require.Equal(t, int32(1), starts.Load())
	require.Zero(t, c.DirtyChainCompactionStats().Canceled)
	chainAssertFlat(t, f, "useful-fold-prefix-kept")
}

// Exercise the scheduling protocol's dangerous interleaving: a valid copied
// fold has finished storage work, but its landing still needs cycleMu. The
// publication cycle must cancel/register a replacement and return, leaving
// the join to the worker; joining synchronously here would deadlock.
func TestDivergentFoldCompletedCopyCannotDeadlockPublicationCycle(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var old CheckoutCycle
	for i := 0; i < 4; i++ {
		old = mcpEdit(t, l, f, func() { foldWiringEdits[i](t, f) })
	}
	commit, found := f.generation(old.CommitGenerationID)
	require.True(t, found)
	built, err := c.flattenDirtyChain(context.Background(), commit, old.DirtyGenerationID)
	require.NoError(t, err)
	folded := c.dirtyChainMembers(context.Background(), old.DirtyGenerationID)
	slices.Reverse(folded)
	branch := divergentFoldBranch(t, f, l)
	oldCtx, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	oldDone := make(chan struct{})
	landingStarted := make(chan struct{})
	c.cycleMu.Lock()
	c.compaction.mu.Lock()
	c.compaction.closed = false
	c.compaction.cancel, c.compaction.running = oldCancel, oldDone
	c.compaction.foldOwner = oldDone
	c.compaction.foldingChain = folded
	c.compaction.stepping.Store(true)
	c.compaction.mu.Unlock()
	go func() {
		close(landingStarted)
		// Like production, a published fold's landing owns cleanup even
		// after its build context was canceled.
		c.landSteppedFold(context.WithoutCancel(oldCtx), old.CommitGenerationID, folded, built, &foldPublication{})
		c.compaction.mu.Lock()
		c.compaction.foldingChain = nil
		c.compaction.foldOwner = nil
		c.compaction.stepping.Store(false)
		if c.compaction.running == oldDone {
			c.compaction.cancel = nil
		}
		c.compaction.mu.Unlock()
		close(oldDone)
	}()
	divergentFoldWait(t, landingStarted, "old landing attempting the held cycle lock")
	scheduled := make(chan bool, 1)
	go func() { scheduled <- c.scheduleDirtyChainCompaction(branch) }()
	select {
	case admitted := <-scheduled:
		c.cycleMu.Unlock()
		require.True(t, admitted)
	case <-time.After(5 * time.Second):
		c.cycleMu.Unlock()
		t.Fatal("replacement joined the old landing while publication still held cycleMu")
	}
	require.ErrorIs(t, oldCtx.Err(), context.Canceled)
	divergentFoldWait(t, oldDone, "completed old fold landing after cycle unlock")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, c.waitDirtyChainCompactions(ctx))
	chainAssertFlat(t, f, "completed-divergent-fold-replacement")
}
