package indexer

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Keep bodies small and the accumulated dirty set larger than the next delta.
// The changing literal gives each layer real current content without growing
// the bodies into the deferred clone-followup policy of chainBurstEdit.
func inlineBusyWriteState(t *testing.T, f *coordinatorFixture, n int) {
	t.Helper()
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	builderWriteFile(t, f.worktree, "caller.go", chainCallerEdit)
	builderWriteFile(t, f.worktree, "island.go", fmt.Sprintf("package fixture\n\nfunc Island() { _ = \"state-%04d\" }\n", n))
}

func inlineBusyReserve(t *testing.T, f *coordinatorFixture, head store_sqlite.ViewGeneration, label string) int64 {
	t.Helper()
	to, _, adopted, err := f.store.BeginPayloadGenerationWithStatus(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: head.OwnerKind, GraphID: head.GraphID, LayerID: head.LayerID, CheckoutID: head.CheckoutID,
		GenerationKind: head.GenerationKind, BaseGenerationID: f.route().CommitGenerationID,
		TreeOID: head.TreeOID, ProvenanceCommitOID: head.ProvenanceCommitOID,
		LowerViewFingerprint: label, ConfigHash: head.ConfigHash, ExtractorVersions: head.ExtractorVersions,
		ResolverVersion: head.ResolverVersion, DependencyRevision: head.DependencyRevision, CreatedAt: time.Now().Unix(),
	})
	require.NoError(t, err)
	require.False(t, adopted)
	t.Cleanup(func() { f.copiedGenerationCleanup(t, to) })
	return to
}

// Cleanup is local to the fixture's private reservations and never retires the
// routed source chain. A real held fold is released by its own cleanup first.
func (f *coordinatorFixture) copiedGenerationCleanup(t *testing.T, to int64) {
	t.Helper()
	row, found, err := f.catalog.GetViewGeneration(context.Background(), to)
	require.NoError(t, err)
	if found && row.State == store_sqlite.ViewGenerationBuilding {
		require.NoError(t, f.catalog.SetViewGenerationState(context.Background(), to, store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationBuilding))
	}
}

func inlineBusyHoldRealFold(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator) (*store_sqlite.ChainFold, int64, []int64) {
	t.Helper()
	route := f.route()
	head, found := f.generation(route.DirtyGenerationID)
	require.True(t, found)
	chain := c.dirtyChainMembers(t.Context(), head.GenerationID)
	slices.Reverse(chain)
	require.Greater(t, len(chain), 1)
	to := inlineBusyReserve(t, f, head, "held-real-fold")
	fold, err := f.store.BeginChainFold(t.Context(), store_sqlite.ChainFoldRequest{Chain: chain, To: to, Owner: "other-fold-owner"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fold.Abandon(context.Background())) })
	require.True(t, f.store.PayloadBuildFlightActive(to))
	return fold, to, chain
}

func TestInlineBusyFoldRefusesWithoutMonolithicCopy(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	for n := 1; n <= 3; n++ {
		mcpEdit(t, l, f, func() { inlineBusyWriteState(t, f, n) })
	}
	before := f.route()
	fold, owner, chain := inlineBusyHoldRealFold(t, f, c)
	head, found := f.generation(before.DirtyGenerationID)
	require.True(t, found)
	to := inlineBusyReserve(t, f, head, "refused-inline-fold")
	copied := 0
	c.compaction.mu.Lock()
	c.compaction.copyHook = func(context.Context, int64) error { copied++; return nil }
	c.compaction.mu.Unlock()
	counts, finish, err := c.copyChainInline()(t.Context(), chain, to)
	if finish != nil {
		defer finish(context.Background(), false)
	}
	require.ErrorIs(t, err, store_sqlite.ErrChainFoldBusy)
	require.Zero(t, counts)
	require.Nil(t, finish)
	require.Zero(t, copied, "Busy must not enter the monolithic-copy completion path")
	require.Zero(t, f.store.AtGeneration(to).NodeCount())
	require.Zero(t, f.store.AtGeneration(to).EdgeCount())
	require.False(t, f.store.PayloadBuildFlightActive(to), "refused Begin must not create a second fold owner")
	require.Equal(t, before, f.route(), "a refused optional copy cannot move the route")
	require.True(t, f.store.PayloadBuildFlightActive(owner), "the refused inline attempt must not release the original owner's flight")
	_, _, err = runChainFoldSteps(t.Context(), fold, storeChainFoldBackend{store: f.store}.StepRetryable, nil)
	require.NoError(t, err, "the original fold retains its real lease and can continue")
	ownerCounts, _, _ := fold.Counts()
	require.Positive(t, ownerCounts.Rows)
	require.Equal(t, before, f.route(), "finishing the owner's private copy does not publish a route")
}

type inlineBusyBeginProbe struct {
	storeChainFoldBackend
	destinations []int64
}

func (b *inlineBusyBeginProbe) BeginChainFold(ctx context.Context, chain []int64, to int64, owner string) (chainFoldSteps, error) {
	b.destinations = append(b.destinations, to)
	return b.storeChainFoldBackend.BeginChainFold(ctx, chain, to, owner)
}

func TestInlineBusyFoldAtTheCapPublishesTheExistingDirectFallback(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var at CheckoutCycle
	for n := 1; n <= maxChainWalkDepth; n++ {
		at = mcpEdit(t, l, f, func() { inlineBusyWriteState(t, f, n) })
	}
	require.Equal(t, maxChainWalkDepth, at.DirtyChainDepth)
	fold, owner, _ := inlineBusyHoldRealFold(t, f, c)
	probe := &inlineBusyBeginProbe{storeChainFoldBackend: storeChainFoldBackend{store: f.store}}
	copied := 0
	c.compaction.mu.Lock()
	c.compaction.backend = probe
	c.compaction.copyHook = func(context.Context, int64) error { copied++; return nil }
	c.compaction.mu.Unlock()
	at = mcpEdit(t, l, f, func() { inlineBusyWriteState(t, f, maxChainWalkDepth+1) })
	require.True(t, at.DirtyBuilt)
	require.Zero(t, at.DirtyParentGenerationID)
	require.Equal(t, 1, at.DirtyChainDepth)
	require.Equal(t, dirtyChainFallbackChainDepthExhausted, at.DirtyChainReason)
	require.Len(t, probe.destinations, 1, "the real inline Begin must encounter the occupied global fold lease")
	require.Zero(t, copied)
	refused, found := f.generation(probe.destinations[0])
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationFailed, refused.State, "the failed optional destination is never a selectable parent")
	require.NotEqual(t, refused.GenerationID, f.route().DirtyGenerationID)
	require.Equal(t, at.DirtyGenerationID, f.route().DirtyGenerationID)
	require.True(t, f.store.PayloadBuildFlightActive(owner), "direct fallback leaves the other fold's ownership intact")
	chainAssertFlat(t, f, "inline-busy-direct")
	next := mcpEdit(t, l, f, func() { inlineBusyWriteState(t, f, maxChainWalkDepth+2) })
	require.Equal(t, at.DirtyGenerationID, next.DirtyParentGenerationID)
	require.Equal(t, 2, next.DirtyChainDepth)
	chainAssertFlat(t, f, "inline-busy-next-child")
	_, _, err := runChainFoldSteps(t.Context(), fold, storeChainFoldBackend{store: f.store}.StepRetryable, nil)
	require.NoError(t, err, "neither direct publication nor its next child may steal the held fold's lease")
}
