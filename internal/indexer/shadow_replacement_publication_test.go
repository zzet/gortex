package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"go.uber.org/zap"
)

func TestShadowReplacementPublishesBaselineOnlyAfterSuccessfulDrain(t *testing.T) {
	for _, prefix := range []string{"", builderRepoPrefix} {
		t.Run(prefix, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/shadow\n\ngo 1.22\n")
			file := filepath.Join(dir, "fixture.go")
			writeFile(t, file, "package shadow\nfunc Before() int { return 1 }\n")
			store := builderOpenStore(t, "shadow-publication")
			cfg := config.Default().Index
			cfg.Merkle = true
			makeIdx := func() *Indexer {
				idx := New(store, builderRegistry(), cfg, zap.NewNop())
				idx.SetRepoPrefix(prefix)
				t.Cleanup(idx.Close)
				return idx
			}
			idx := makeIdx()
			_, err := idx.IndexCtx(t.Context(), dir)
			idx.Close()
			require.NoError(t, err)
			_, present, err := store.GetRepoIndexState(prefix)
			require.NoError(t, err)
			require.True(t, present)
			_, err = os.Stat(merkleTreeFile(dir))
			require.NoError(t, err)
			writeFile(t, file, "package shadow\nfunc After() int { return 2 }\n")
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), buildCommitPointKey{}, true))
			defer cancel()
			oldHook := drainSubBatchHook
			drainSubBatchHook = cancel
			t.Cleanup(func() { drainSubBatchHook = oldHook })
			idx = makeIdx()
			_, err = idx.IndexCtx(ctx, dir)
			require.True(t, errors.Is(err, context.Canceled), "%v", err)
			drainSubBatchHook = oldHook
			_, present, err = store.GetRepoIndexState(prefix)
			require.NoError(t, err)
			require.False(t, present, "failed warm replacement retained success provenance")
			_, err = os.Stat(merkleTreeFile(dir))
			require.True(t, os.IsNotExist(err), "old Merkle baseline survived failed drain: %v", err)
			require.Zero(t, idx.indexCount.Load(), "same live Indexer must retry a failed shadow as a full replacement")
			_, err = idx.IndexCtx(t.Context(), dir)
			idx.Close()
			require.NoError(t, err)
			_, present, err = store.GetRepoIndexState(prefix)
			require.NoError(t, err)
			require.True(t, present)
			_, err = os.Stat(merkleTreeFile(dir))
			require.NoError(t, err)
			var before, after bool
			for _, n := range store.AllNodes() {
				before = before || n.Name == "Before"
				after = after || n.Name == "After"
			}
			require.False(t, before)
			require.True(t, after)
		})
	}
}
