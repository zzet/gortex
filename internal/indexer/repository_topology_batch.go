package indexer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/reach"
)

type repositoryTopologyBatchContextKey struct{}

var errRepositoryTopologyBatchOwnerMismatch = errors.New("repository topology batch belongs to another MultiIndexer")

type repositoryTopologyBatch struct {
	owner   *MultiIndexer
	active  atomic.Bool
	changed atomic.Bool
}

func repositoryTopologyBatchFromContext(ctx context.Context) *repositoryTopologyBatch {
	if ctx == nil {
		return nil
	}
	batch, _ := ctx.Value(repositoryTopologyBatchContextKey{}).(*repositoryTopologyBatch)
	return batch
}

func activeRepositoryTopologyBatch(ctx context.Context, owner *MultiIndexer) *repositoryTopologyBatch {
	batch := repositoryTopologyBatchFromContext(ctx)
	if batch == nil || owner == nil || batch.owner != owner || !batch.active.Load() {
		return nil
	}
	return batch
}

// RunRepositoryTopologyBatch owns one reachability-topology generation across
// a joined set of TrackRepoCtx and ReconcileRepoCtx calls. The callback must
// join every goroutine that receives batchCtx before returning; batchCtx must
// not escape. Ordinary mutation pipelines remain blocked at batch admission,
// before taking a repository lane, for the complete callback.
func (mi *MultiIndexer) RunRepositoryTopologyBatch(
	ctx context.Context,
	run func(batchCtx context.Context) error,
) error {
	if run == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if existing := repositoryTopologyBatchFromContext(ctx); existing != nil && existing.active.Load() {
		if existing.owner != mi {
			return errRepositoryTopologyBatchOwnerMismatch
		}
		return run(ctx)
	}

	// Hold the transition writer for the complete callback. Batch-aware
	// Track/Reconcile calls use their stable lane without re-entering this gate;
	// unmarked watcher, MCP, janitor, and global-pass mutations wait before any
	// lane acquisition and therefore cannot form a lane/topology lock cycle.
	mi.batchMutationGate.Lock()
	finishTopologyMutation := reach.BeginTopologyMutation(mi.graph)
	batch := &repositoryTopologyBatch{owner: mi}
	batch.active.Store(true)
	batchCtx := context.WithValue(ctx, repositoryTopologyBatchContextKey{}, batch)
	defer func() {
		batch.active.Store(false)
		finishTopologyMutation(batch.changed.Load())
		mi.batchMutationGate.Unlock()
	}()

	return run(batchCtx)
}

func (mi *MultiIndexer) newPerRepoIndexerForMutation(
	ctx context.Context,
	cfg config.IndexConfig,
) *Indexer {
	if activeRepositoryTopologyBatch(ctx, mi) != nil {
		return mi.newPerRepoIndexerGuarded(cfg)
	}
	return mi.newPerRepoIndexer(cfg)
}

func (mi *MultiIndexer) coordinateRepositoryTopologyMutation(
	ctx context.Context,
	idx *Indexer,
	fn func() error,
) error {
	if activeRepositoryTopologyBatch(ctx, mi) != nil {
		// The lane-only arm is the second door into the same generation-zero
		// payload. It names its output generation through the same authority,
		// inside the lane, exactly as the coordinated arm does.
		return idx.repositoryMutations().runExclusiveLaneOnly(ctx, func() error {
			return idx.withOutputGeneration(ctx, OutputEntryRepositoryTopology, fn)
		})
	}
	return idx.coordinateRepositoryMutation(ctx, OutputEntryRepositoryTopology, fn)
}

func (mi *MultiIndexer) beginRepositoryTopologyMutation(ctx context.Context) func(changed bool) {
	if batch := activeRepositoryTopologyBatch(ctx, mi); batch != nil {
		var once sync.Once
		return func(changed bool) {
			once.Do(func() {
				if changed {
					batch.changed.Store(true)
				}
			})
		}
	}
	return reach.BeginTopologyMutation(mi.graph)
}
