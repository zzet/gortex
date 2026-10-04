package store_sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChainFoldRequestedShortStepsCommitAndPreservePayload(t *testing.T) {
	store := openCatalogStore(t)
	ctx := t.Context()
	chain := foldChain(t, store, 0)
	reference := reservedGeneration(t, store, "reference")
	_, err := store.FlattenGenerationChain(ctx, chain, reference)
	require.NoError(t, err)
	target := reservedGeneration(t, store, "short-steps")
	original := chainFoldStepTarget
	fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: target, Owner: "short-step-test", StepTarget: time.Millisecond})
	require.NoError(t, err)
	defer func() { require.NoError(t, fold.Release(context.WithoutCancel(ctx))) }()
	phases := 0
	chainFoldInjectFailure = func(int) error { phases++; time.Sleep(2 * time.Millisecond); return nil }
	t.Cleanup(func() { chainFoldInjectFailure = nil })
	done, err := fold.Step(ctx)
	chainFoldInjectFailure = nil
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, 1, phases, "a slow phase must commit before another phase consumes the short target")
	require.Equal(t, original, chainFoldStepTarget, "the request must not change another fold's default")
	runFold(t, fold, nil)
	require.Equal(t, renderGenerationNodes(t, store, reference), renderGenerationNodes(t, store, target))
	require.Equal(t, renderGenerationEdges(t, store, reference), renderGenerationEdges(t, store, target))
	require.Equal(t, renderFoldMasks(t, store, reference), renderFoldMasks(t, store, target))
	require.Equal(t, renderFoldFTS(t, store, reference), renderFoldFTS(t, store, target))
}
