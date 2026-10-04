package store_sqlite

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestChainFoldSlowMetadataDoesNotStarveBeginOrForegroundWrites(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	s.stopCheckpointLoop()
	chain := foldChain(t, s, 0)
	reference := reservedGeneration(t, s, "metadata-reference")
	_, refErr := s.FlattenGenerationChain(t.Context(), chain, reference)
	require.NoError(t, refErr)
	target := reservedGeneration(t, s, "slow-metadata")
	var prepared atomic.Int64
	chainFoldMetadataHook = func(ctx context.Context, finished bool) error {
		if finished {
			return nil
		}
		prepared.Add(1)
		timer := time.NewTimer(150 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 1200*time.Millisecond)
	var writes atomic.Int64
	writerDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				writerDone <- nil
				return
			case <-ticker.C:
				if err := s.writeMu.LockContext(ctx); err != nil {
					writerDone <- nil
					return
				}
				_, err := s.writerDB.ExecContext(ctx, `INSERT OR REPLACE INTO repo_index_state(view_gen,repo_prefix,indexed_sha,dirty,indexed_at,workspace_fp,node_count,edge_count,extractor_versions) VALUES(0,'foreground','',0,0,'',0,0,'')`)
				s.writeMu.Unlock()
				if err != nil {
					if ctx.Err() != nil {
						writerDone <- nil
					} else {
						writerDone <- err
					}
					return
				}
				writes.Add(1)
			}
		}
	}()
	var joined bool
	defer func() {
		cancel()
		if !joined {
			<-writerDone
		}
		chainFoldMetadataHook = nil
	}()
	var fold *ChainFold
	var err error
	for {
		fold, err = s.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: target, Owner: "slow-metadata", StepTarget: 10 * time.Millisecond})
		if err == nil || ctx.Err() != nil || !errors.Is(err, ErrChainFoldYielded) {
			break
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	cancel()
	writerErr := <-writerDone
	joined = true
	chainFoldMetadataHook = nil
	require.NoError(t, writerErr)
	t.Logf("metadata preparations=%d actual foreground commits=%d begin_error=%v", prepared.Load(), writes.Load(), err)
	require.NoError(t, err, "slow metadata must prepare while real foreground commits continue")
	defer func() { require.NoError(t, fold.Release(t.Context())) }()
	require.GreaterOrEqual(t, writes.Load(), int64(3))
	runFold(t, fold, nil)
	require.Equal(t, renderGenerationNodes(t, s, reference), renderGenerationNodes(t, s, target))
	require.Equal(t, renderGenerationEdges(t, s, reference), renderGenerationEdges(t, s, target))
	require.Equal(t, renderFoldMasks(t, s, reference), renderFoldMasks(t, s, target))
	require.Equal(t, renderFoldFTS(t, s, reference), renderFoldFTS(t, s, target))
}

func TestChainFoldMetadataCancellationReleasesReservation(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	s.stopCheckpointLoop()
	chain := foldChain(t, s, 0)
	target := reservedGeneration(t, s, "metadata-cancel")
	at := make(chan struct{})
	var once sync.Once
	chainFoldMetadataHook = func(ctx context.Context, finished bool) error {
		if finished {
			return nil
		}
		once.Do(func() { close(at) })
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := s.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: target, Owner: "metadata-cancel"})
		done <- err
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
		chainFoldMetadataHook = nil
	}()
	select {
	case <-at:
	case <-time.After(time.Second):
		t.Fatal("metadata preparation not reached")
	}
	require.True(t, s.writeMu.TryLock(), "metadata cancellation must not block foreground writer")
	s.writeMu.Unlock()
	cancel()
	err := <-done
	joined = true
	chainFoldMetadataHook = nil
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, s.chainFold.active.Load())
	require.False(t, s.chainFoldHolds(chain[0]))
	fold, err := s.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: target, Owner: "retry-after-cancel"})
	require.NoError(t, err)
	require.NoError(t, fold.Release(t.Context()))
}

func TestChainFoldMetadataRechecksSchemaAndDestination(t *testing.T) {
	for _, kind := range []string{"schema_changed", "destination_populated"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
			s, _ := openWALReclaimStore(t)
			t.Cleanup(func() { _ = s.Close() })
			s.stopCheckpointLoop()
			chain := foldChain(t, s, 0)
			target := reservedGeneration(t, s, "metadata-recheck")
			chainFoldMetadataHook = func(ctx context.Context, finished bool) error {
				if !finished {
					return nil
				}
				if kind == "destination_populated" {
					return s.AtGeneration(target).AddBatchChecked([]*graph.Node{flatNode("repo/raced.go::R", "repo/raced.go")}, nil)
				}
				if err := s.writeMu.LockContext(ctx); err != nil {
					return err
				}
				defer s.writeMu.Unlock()
				_, err := s.writerDB.ExecContext(ctx, `CREATE INDEX metadata_schema_changed ON repo_index_state(indexed_at)`)
				return err
			}
			defer func() { chainFoldMetadataHook = nil }()
			_, err := s.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: target, Owner: "metadata-recheck"})
			chainFoldMetadataHook = nil
			require.False(t, s.chainFold.active.Load())
			require.False(t, s.chainFoldHolds(chain[0]))
			if kind == "destination_populated" {
				require.ErrorIs(t, err, ErrGenerationBulkLoadPopulated)
				require.Len(t, s.AtGeneration(target).AllNodes(), 1)
				return
			}
			require.ErrorIs(t, err, ErrChainFoldYielded, "new schema needs a bounded retry, never stale shape acceptance")
			fold, err := s.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: target, Owner: "retry-current-schema"})
			require.NoError(t, err)
			require.NoError(t, fold.Release(t.Context()))
		})
	}
}
