package indexer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A stepped fold releases its build permit before copying. Its exact source
// holds and destination flight, rather than a global checkout flag, must
// protect its payload while unrelated aged work uses a fair background turn.
func TestAgedRetirementDrainsUnrelatedGenerationDuringAPausedOffLaneFold(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	l.logger = zap.NewNop()
	var trigger CheckoutCycle
	for i := 0; i < 4; i++ {
		trigger = mcpEdit(t, l, f, func() { chainEditIsland(t, f, i) })
	}
	require.Equal(t, 4, trigger.DirtyChainDepth)
	chainAssertFlat(t, f, "off-lane-accepted-input")
	oldest, found, err := f.catalog.GetViewGeneration(t.Context(), trigger.DirtyGenerationID)
	require.NoError(t, err)
	require.True(t, found)
	obsolete, handle, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: oldest.OwnerKind, GraphID: oldest.GraphID, LayerID: oldest.LayerID,
		CheckoutID: oldest.CheckoutID, GenerationKind: oldest.GenerationKind,
		ConfigHash: "unrelated-retirement", ExtractorVersions: oldest.ExtractorVersions,
		ResolverVersion: oldest.ResolverVersion, CreatedAt: time.Now().Add(-time.Hour).Unix(),
	})
	require.NoError(t, err)
	node := &graph.Node{ID: builderRepoPrefix + "/obsolete.go::Obsolete", Kind: graph.KindFunction,
		Name: "Obsolete", FilePath: builderRepoPrefix + "/obsolete.go", RepoPrefix: builderRepoPrefix}
	handle.AddNode(node)
	require.NoError(t, f.catalog.SetViewGenerationState(t.Context(), obsolete, store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationBuilding))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	entered, permit, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unpark := sync.OnceFunc(func() { close(permit) })
	var once sync.Once
	var report DirtyChainCompaction
	c.compaction.mu.Lock()
	c.compaction.stepHook = func(ctx context.Context, step int) {
		if step != 0 {
			return
		}
		once.Do(func() { close(entered) })
		select {
		case <-permit:
		case <-ctx.Done():
		}
	}
	c.compaction.mu.Unlock()
	defer func() {
		cancel()
		unpark()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("off-lane fold did not join before fixture cleanup")
		}
	}()
	go func() {
		report = c.compactDirtyChain(ctx, trigger)
		close(done)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fold did not enter its first real Store step")
	}
	require.True(t, c.compaction.folding.Load())
	require.True(t, c.compaction.stepping.Load())
	require.False(t, c.gate.Stats().Active, "the real stepped fold released its permit before pausing")
	sources := c.foldingChain()
	require.Len(t, sources, trigger.DirtyChainDepth)
	building, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{States: []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationBuilding}})
	require.NoError(t, err)
	var output int64
	for _, row := range building {
		if f.store.PayloadBuildFlightActive(row.GenerationID) {
			require.Zero(t, output, "one paused fold owns one destination")
			output = row.GenerationID
		}
	}
	require.Positive(t, output)
	protected := append(append([]int64{}, sources...), trigger.CommitGenerationID, output)
	for _, id := range protected {
		before, found, err := f.catalog.GetViewGeneration(ctx, id)
		require.NoError(t, err)
		require.True(t, found)
		progress, err := f.store.RetirePayloadGenerationQuantum(ctx, id, l.deferredRetirementInUse)
		require.True(t, errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) || errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced), "generation %d protection: %v", id, err)
		require.Zero(t, progress.ChunksCommitted)
		require.Zero(t, progress.RowsDeleted)
		require.False(t, progress.CatalogRemoved)
		after, found, err := f.catalog.GetViewGeneration(ctx, id)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, before, after, "fold ownership refuses before changing the generation fence")
	}

	l.SetBuildGate(c.gate)
	l.owed = map[int64]struct{}{obsolete: {}}
	l.foregroundWork = func() (string, time.Time) { return "refresh_ticket", time.Now() }
	l.interactiveDemand = func() bool { return true }
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	for i := 0; i < 20; i++ {
		_, _, err := l.SweepDeferredRetirements(ctx)
		require.NoError(t, err)
		_, found, err := f.catalog.GetViewGeneration(ctx, obsolete)
		require.NoError(t, err)
		if !found {
			break
		}
	}
	_, survives, err := f.catalog.GetViewGeneration(ctx, obsolete)
	require.NoError(t, err)
	require.False(t, survives, "an off-lane fold must not prevent unrelated retirement")
	require.Nil(t, f.store.AtGeneration(obsolete).GetNode(node.ID))
	for _, id := range protected {
		_, found, err := f.catalog.GetViewGeneration(ctx, id)
		require.NoError(t, err)
		require.True(t, found)
	}
	unpark()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("protected fold did not finish after unrelated retirement")
	}
	require.NoError(t, report.Err)
	require.Equal(t, dirtyChainCompactionFlipped, report.Outcome)
	require.Positive(t, report.GenerationID)
	requireFullManifest(t, f, report.GenerationID)
	chainAssertFlat(t, f, "off-lane-fold-after-retirement")
}
