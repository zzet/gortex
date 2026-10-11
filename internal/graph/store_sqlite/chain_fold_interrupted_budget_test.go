package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A refusal before transaction work says nothing about page sizing. Repeated
// writer interruptions after real SQL must instead shorten the replayed page,
// while preserving its cursor and rows until a whole page commits.
func TestChainFoldInterruptedPagesReduceOnlyAfterTransactionWork(t *testing.T) {
	store := openCatalogStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	bottom := flattenMember(t, store, "budget-bottom", []*graph.Node{flatNode("repo/old.go::Old", "repo/old.go")}, nil, nil, nil, nil)
	files := make([]FileMask, 700)
	for i := range files {
		files[i] = FileMask{RepoPrefix: "repo", FilePath: fmt.Sprintf("repo/page%03d.go", i), Mode: OwnershipReplace}
	}
	upper := flattenMember(t, store, "budget-upper", nil, nil, files, nil, nil)
	for _, id := range []int64{bottom, upper} {
		require.NoError(t, store.Catalog().SetViewGenerationState(ctx, id, ViewGenerationReady, ViewGenerationBuilding))
	}
	reference := reservedGeneration(t, store, "budget-reference")
	_, err := store.FlattenGenerationChain(ctx, []int64{bottom, upper}, reference)
	require.NoError(t, err)
	maskQuantumBudget(t)
	target := reservedGeneration(t, store, "budget-interrupted")
	fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: []int64{bottom, upper}, To: target, Owner: "budget-test"})
	require.NoError(t, err)
	defer func() { _ = fold.Release(context.WithoutCancel(ctx)) }()
	reachFoldMasks(t, fold, ctx)
	for fold.table < 5 {
		_, err = fold.Step(ctx)
		require.NoError(t, err)
	}
	require.Equal(t, foldPhaseMasks, fold.phase)
	require.Equal(t, "generation_file_masks", foldMaskOperations[fold.table].table)
	chainFoldFirstRows, chainFoldMaxRows, chainFoldStepTarget = 512, 512, 50*time.Millisecond
	fold.usPerRow = 0
	before := renderFoldMasks(t, store, target)
	cursor := fold.cursor()
	injected := errors.New("non-writer page failure")
	chainFoldMaskStepHook = func(context.Context, string) error { return injected }
	t.Cleanup(func() { chainFoldMaskStepHook = nil })
	_, err = fold.Step(ctx)
	chainFoldMaskStepHook = nil
	require.ErrorIs(t, err, injected)
	require.Equal(t, 512, fold.stepRows(), "unrelated failures give no writer-interruption sizing evidence")
	interruptions, reductions := fold.InterruptionCounts()
	require.Zero(t, interruptions)
	require.Zero(t, reductions)
	require.Equal(t, cursor, fold.cursor())
	require.Equal(t, before, renderFoldMasks(t, store, target))
	for _, wanted := range []int{256, 128, 64, 64} {
		budget := fold.stepRows()
		interruptions, reductions = fold.InterruptionCounts()
		ordinary := store.AnnounceWrite()
		_, err = fold.Step(ctx)
		ordinary()
		require.ErrorIs(t, err, ErrChainFoldYielded)
		require.Equal(t, budget, fold.stepRows(), "admission refusal is not interrupted page work")
		afterAdmission, afterReduction := fold.InterruptionCounts()
		require.Equal(t, interruptions, afterAdmission)
		require.Equal(t, reductions, afterReduction)
		ready := make(chan struct{})
		writerDone := make(chan error, 1)
		var wait time.Duration
		chainFoldMaskStepHook = func(stepCtx context.Context, table string) error {
			if table != "generation_file_masks" {
				return nil
			}
			close(ready) // The page SQL has run inside the open transaction.
			<-stepCtx.Done()
			return stepCtx.Err()
		}
		go func() {
			select {
			case <-ready:
			case <-ctx.Done():
				writerDone <- ctx.Err()
				return
			}
			started := time.Now()
			err := store.writeMu.LockContext(ctx)
			wait = time.Since(started)
			if err == nil {
				// Exercise SQLite after the interrupted transaction has rolled
				// back, rather than merely acquiring the application gate.
				_, err = store.writerDB.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS fold_budget_writer (value INTEGER)`)
				store.writeMu.Unlock()
			}
			writerDone <- err
		}()
		_, err = fold.Step(ctx)
		writerErr := <-writerDone // Join before resetting the hook or inspecting wait.
		chainFoldMaskStepHook = nil
		require.NoError(t, writerErr)
		require.ErrorIs(t, err, ErrChainFoldYielded)
		require.Equal(t, cursor, fold.cursor(), "an interrupted transaction advanced its source cursor")
		require.Equal(t, before, renderFoldMasks(t, store, target), "an interrupted page escaped rollback")
		limit := 20 * time.Millisecond
		if raceDetectorOn {
			limit = 100 * time.Millisecond
		}
		require.Less(t, wait, limit, "the queued writer did not interrupt the page promptly")
		t.Logf("interrupted SQLite mask page: rows=%d next=%d writer_wait=%s", budget, fold.stepRows(), wait)
		require.Equal(t, wanted, fold.stepRows(), "the rolled-back page will repeat its oversized budget")
	}
	interruptions, reductions = fold.InterruptionCounts()
	require.Equal(t, 4, interruptions)
	require.Equal(t, 3, reductions, "interruption at the minimum cannot reduce the page further")
	_, err = fold.Step(ctx)
	require.NoError(t, err)
	require.Len(t, renderFoldMasks(t, store, target), len(before)+64, "the shortened page must make real committed progress")
	require.Zero(t, fold.interruptedPageRows, "successful commit must release the retry-only limit")
	require.Equal(t, 512, fold.stepRows(), "normal time-based sizing must recover after commit")
	interruptions, reductions = fold.InterruptionCounts()
	require.Equal(t, 4, interruptions, "commits preserve cumulative telemetry")
	require.Equal(t, 3, reductions)
	runFold(t, fold, nil)
	require.Equal(t, renderGenerationNodes(t, store, reference), renderGenerationNodes(t, store, target))
	require.Equal(t, renderGenerationEdges(t, store, reference), renderGenerationEdges(t, store, target))
	require.Equal(t, renderFoldMasks(t, store, reference), renderFoldMasks(t, store, target))
	require.Equal(t, renderFoldFTS(t, store, reference), renderFoldFTS(t, store, target))
}
