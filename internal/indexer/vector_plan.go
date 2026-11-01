package indexer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

// preparedVectorPlan is the immutable hand-off between embedding and vector
// publication. It owns only IDs and the vectors the provider returned (each
// vector has exactly one owner, the plan item): graph nodes and source
// buffers are deliberately not retained across a cold-shadow drain.
type preparedVectorPlan struct {
	repoPrefix   string
	dims         int
	items        []graph.VectorCorpusItem
	chunkMap     map[string]string
	dropped      int
	droppedIDs   []string
	embeddedText int
}

// Release drops every potentially large reference held by the plan. Heap
// vector publication copies the chunk map first; durable publication copies
// vectors into its transaction before this method runs.
func (p *preparedVectorPlan) Release() {
	if p == nil {
		return
	}
	for i := range p.items {
		p.items[i].NodeID = ""
		p.items[i].ParentID = ""
		p.items[i].Vec = nil
	}
	p.items = nil
	clear(p.chunkMap)
	p.chunkMap = nil
	clear(p.droppedIDs)
	p.droppedIDs = nil
	p.repoPrefix = ""
	p.dims = 0
	p.dropped = 0
	p.embeddedText = 0
}

// prepareSearchIndex prepares the vector corpus without mutating either the
// durable vector store or the active vector backend. The caller decides when
// publication is safe.
func (idx *Indexer) prepareSearchIndex(ctx context.Context) (*preparedVectorPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	idx.lastVectorBuildErr = nil
	if idx.embedder == nil {
		return nil, nil
	}

	// Content sections belong to content search, not symbol/vector search.
	nodes := graph.RepoCodeNodes(idx.graph, idx.repoPrefix)
	return idx.prepareVectorPlan(ctx, nodes)
}

func (idx *Indexer) prepareVectorPlan(ctx context.Context, nodes []*graph.Node) (*preparedVectorPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	texts, ids, collectedChunks, skipped := idx.collectEmbedTexts(nodes)
	if skipped > 0 {
		idx.logger.Info("skipped embedding for low-value nodes",
			zap.Int("count", skipped),
			zap.Int("embedded", len(texts)))
	}

	const (
		defaultEmbedMaxSymbols = 100_000
		embedChunkSize         = 500
		embedChunkTimeout      = 5 * time.Minute
	)
	embedMaxSymbols := defaultEmbedMaxSymbols
	if idx.embedMaxSymbols > 0 {
		embedMaxSymbols = idx.embedMaxSymbols
	}
	if env := os.Getenv("GORTEX_EMBEDDINGS_MAX_SYMBOLS"); env != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(env)); err == nil && n > 0 {
			embedMaxSymbols = n
		}
	}
	if len(texts) > embedMaxSymbols {
		return nil, fmt.Errorf("embedding text count %d exceeds threshold %d (raise embedding.max_symbols)", len(texts), embedMaxSymbols)
	}

	// An empty successful plan is authoritative: installing it removes stale
	// vectors for a repository that no longer has embeddable symbols.
	if len(texts) == 0 {
		return &preparedVectorPlan{
			repoPrefix: idx.repoPrefix,
			dims:       embeddingDimsOrDefault(idx.embedder),
		}, nil
	}

	var embedWithRetry func(context.Context, []string) ([][]float32, error)
	embedWithRetry = func(parent context.Context, items []string) ([][]float32, error) {
		chunkCtx, cancel := context.WithTimeout(parent, embedChunkTimeout)
		out, err := idx.embedder.EmbedBatch(chunkCtx, items)
		cancel()
		if err == nil {
			return out, nil
		}
		if parent.Err() != nil || !errors.Is(err, context.DeadlineExceeded) || len(items) <= 1 {
			return nil, err
		}
		idx.logger.Warn("embed chunk timed out, retrying with halved batch",
			zap.Int("size", len(items)), zap.Error(err))
		mid := len(items) / 2
		left, leftErr := embedWithRetry(parent, items[:mid])
		if leftErr != nil {
			return nil, leftErr
		}
		right, rightErr := embedWithRetry(parent, items[mid:])
		if rightErr != nil {
			return nil, rightErr
		}
		return append(left, right...), nil
	}

	vectors, err := idx.embedAllChunks(ctx, texts, embedChunkSize, embedWithRetry)
	if err != nil {
		return nil, fmt.Errorf("chunk embedding failed: %w", err)
	}
	if len(vectors) != len(ids) {
		return nil, fmt.Errorf("embedding provider returned %d vectors for %d texts", len(vectors), len(ids))
	}

	// A provider-reported width is authoritative. Providers that learn their
	// dimensions on first use are inferred from the first non-empty result.
	dims := idx.embedder.Dimensions()
	if dims <= 0 {
		for _, vec := range vectors {
			if len(vec) > 0 {
				dims = len(vec)
				break
			}
		}
	}
	if dims <= 0 {
		dims = embeddingDimsOrDefault(idx.embedder)
	}

	plan := &preparedVectorPlan{
		repoPrefix:   idx.repoPrefix,
		dims:         dims,
		items:        make([]graph.VectorCorpusItem, 0, len(vectors)),
		embeddedText: len(texts),
	}
	// The provider returns freshly allocated vectors and keeps no reference
	// to them (see embedding.Provider.EmbedBatch), so each valid vector moves
	// into its plan item instead of being copied. Clearing the source entry
	// leaves the item as the vector's only owner.
	for i, vec := range vectors {
		vectors[i] = nil
		valid := len(vec) == dims
		if valid {
			for _, value := range vec {
				f := float64(value)
				if math.IsNaN(f) || math.IsInf(f, 0) {
					valid = false
					break
				}
			}
		}
		if !valid {
			plan.dropped++
			if len(plan.droppedIDs) < 5 {
				plan.droppedIDs = append(plan.droppedIDs, ids[i])
			}
			continue
		}
		parentID := collectedChunks[ids[i]]
		plan.items = append(plan.items, graph.VectorCorpusItem{
			NodeID:   ids[i],
			ParentID: parentID,
			Vec:      vec,
		})
		if parentID != "" {
			if plan.chunkMap == nil {
				plan.chunkMap = make(map[string]string)
			}
			plan.chunkMap[ids[i]] = parentID
		}
	}
	if len(plan.items) == 0 {
		dropped := plan.dropped
		plan.Release()
		return nil, fmt.Errorf("all %d embedding vectors were invalid (want width %d)", dropped, dims)
	}
	return plan, nil
}

// prepareSearchIndexForPublication retains the previous vector publication on
// provider, cap, or validation failures. Parent cancellation is different: it
// aborts the index operation and must not publish anything.
func (idx *Indexer) prepareSearchIndexForPublication(ctx context.Context) (*preparedVectorPlan, error) {
	plan, err := idx.prepareSearchIndex(ctx)
	if err == nil {
		return plan, nil
	}
	idx.lastVectorBuildErr = err
	idx.logger.Warn("vector index preparation failed; retaining previous vector publication", zap.Error(err))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, nil
}

// installVectorPlan publishes one complete plan. Expensive embedding happens
// before SerializeVectorUpdate; the shared lane spans only durable replacement
// and the matching process-local publication.
func (idx *Indexer) installVectorPlan(ctx context.Context, durable graph.Store, plan *preparedVectorPlan) error {
	if plan == nil {
		return nil
	}
	defer plan.Release()
	if ctx == nil {
		ctx = context.Background()
	}

	sw := idx.swappable()
	return sw.SerializeVectorUpdate(func() error {
		if err := ctx.Err(); err != nil {
			idx.lastVectorBuildErr = err
			return err
		}

		installer, hasInstaller := durable.(graph.AtomicVectorCorpusInstaller)
		vectorSearcher, hasVectorSearcher := durable.(graph.VectorSearcher)
		var nativeErr error
		if hasInstaller && hasVectorSearcher {
			stats, err := installer.ReplaceVectorCorpus(ctx, plan.repoPrefix, plan.dims, plan.items)
			if err == nil {
				dims := stats.Dims
				if dims <= 0 {
					dims = plan.dims
				}
				delegated := search.NewDelegatedVector(
					dims,
					&vectorSearcherDelegate{s: vectorSearcher},
					stats.VectorCount,
					stats.ChunkCount,
				)
				sw.ReplaceHybridVector(delegated, idx.embedder)
				idx.lastVectorBuildErr = nil
				idx.logVectorPublication("durable vector corpus published", plan, stats.VectorCount, stats.ChunkCount, true)
				return nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				idx.lastVectorBuildErr = ctxErr
				return ctxErr
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				idx.lastVectorBuildErr = err
				return err
			}
			nativeErr = fmt.Errorf("durable vector corpus install failed: %w", err)
		} else if hasInstaller != hasVectorSearcher {
			nativeErr = fmt.Errorf("durable vector backend exposes an incomplete atomic corpus capability")
		}

		// A repository-local heap cannot replace a process-global vector channel:
		// it would hide every sibling repository. Preserve the last committed
		// delegate in multi-repo mode and surface the degraded install instead.
		if idx.repositoryMutationOwner != nil {
			if nativeErr == nil {
				nativeErr = fmt.Errorf("durable atomic vector corpus installation is unavailable in multi-repository mode")
			}
			idx.lastVectorBuildErr = nativeErr
			idx.logger.Warn("vector publication retained previous global corpus",
				zap.String("repo", plan.repoPrefix), zap.Error(nativeErr))
			return nil
		}

		heap := search.NewVector(plan.dims)
		for _, item := range plan.items {
			heap.Add(item.NodeID, item.Vec)
		}
		if len(plan.chunkMap) > 0 {
			chunks := make(map[string]string, len(plan.chunkMap))
			for child, parent := range plan.chunkMap {
				chunks[child] = parent
			}
			heap.SetChunkMap(chunks)
		}
		sw.ReplaceHybridVector(heap, idx.embedder)
		if nativeErr != nil {
			idx.lastVectorBuildErr = nativeErr
			idx.logger.Warn("durable vector install failed; published process-local fallback",
				zap.String("repo", plan.repoPrefix), zap.Error(nativeErr),
				zap.String("restart_behavior", "durable corpus remains at its previous generation"))
		} else {
			idx.lastVectorBuildErr = nil
		}
		idx.logVectorPublication("process-local vector corpus published", plan, heap.Count(), len(plan.chunkMap), false)
		return nil
	})
}

func (idx *Indexer) logVectorPublication(message string, plan *preparedVectorPlan, vectors, chunks int, delegated bool) {
	fields := []zap.Field{
		zap.String("repo", plan.repoPrefix),
		zap.Int("vectors", vectors),
		zap.Int("chunk_vectors", chunks),
		zap.Int("dimensions", plan.dims),
		zap.Int("dropped", plan.dropped),
		zap.Bool("delegated", delegated),
	}
	if len(plan.droppedIDs) > 0 {
		fields = append(fields, zap.Strings("dropped_sample_ids", plan.droppedIDs))
	}
	if acc, ok := idx.embedder.(interface{ TokensUsed() int64 }); ok {
		if tokens := acc.TokensUsed(); tokens > 0 {
			fields = append(fields, zap.Int64("embed_tokens", tokens))
		}
	}
	idx.logger.Info(message, fields...)
}

// restoreDurableVectorBackend republishes an existing complete durable corpus
// without invoking the embedding provider. False means the corpus is absent or
// the store lacks the atomic capability, so the caller may rebuild it.
func (idx *Indexer) restoreDurableVectorBackend(ctx context.Context, durable graph.Store) (bool, error) {
	if idx.embedder == nil {
		return false, nil
	}
	installer, ok := durable.(graph.AtomicVectorCorpusInstaller)
	if !ok {
		return false, nil
	}
	vectorSearcher, ok := durable.(graph.VectorSearcher)
	if !ok {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	restored := false
	sw := idx.swappable()
	err := sw.SerializeVectorUpdate(func() error {
		queryDims := idx.embedder.Dimensions()
		stats, err := installer.VectorCorpusStatsForRepo(ctx, idx.repoPrefix, queryDims)
		if err != nil {
			return err
		}
		if stats.RepositoryVectorCount == 0 || stats.Dims <= 0 {
			return nil
		}
		delegated := search.NewDelegatedVector(
			stats.Dims,
			&vectorSearcherDelegate{s: vectorSearcher},
			stats.VectorCount,
			stats.ChunkCount,
		)
		sw.ReplaceHybridVector(delegated, idx.embedder)
		idx.lastVectorBuildErr = nil
		restored = true
		idx.logger.Info("restored durable vector corpus without embedding",
			zap.String("repo", idx.repoPrefix),
			zap.Int("vectors", stats.VectorCount),
			zap.Int("chunk_vectors", stats.ChunkCount),
			zap.Int("dimensions", stats.Dims))
		return nil
	})
	return restored, err
}

func (idx *Indexer) buildSearchIndexCtx(ctx context.Context) error {
	plan, err := idx.prepareSearchIndexForPublication(ctx)
	if err != nil || plan == nil {
		return err
	}
	return idx.installVectorPlan(ctx, idx.graph, plan)
}

// publishVectorCorpusAfterRepoRemoval runs inside the shared Swappable vector
// update lane after graph/sidecar purge. The empty atomic replacement is both
// a compatibility cleanup for pre-v10 chunk residue and the authoritative
// aggregate-statistics snapshot used for publication.
func (mi *MultiIndexer) publishVectorCorpusAfterRepoRemoval(
	ctx context.Context,
	repoPrefix string,
	sw *search.Swappable,
) error {
	installer, ok := mi.graph.(graph.AtomicVectorCorpusInstaller)
	if !ok {
		return nil
	}

	dims := 0
	if mi.embedder != nil {
		dims = mi.embedder.Dimensions()
	}
	if dims <= 0 {
		if existing, err := installer.VectorCorpusStats(ctx, 0); err == nil && existing.Dims > 0 {
			dims = existing.Dims
		}
	}
	if dims <= 0 {
		dims = embeddingDimsOrDefault(mi.embedder)
	}
	if dims <= 0 {
		// Empty replacement still requires a positive validation width. With no
		// embedder there is no vector publication, so this sentinel is not used
		// to interpret any stored vectors.
		dims = 1
	}

	stats, err := installer.ReplaceVectorCorpus(ctx, repoPrefix, dims, nil)
	if err != nil {
		return fmt.Errorf("remove vector corpus for %s: %w", repoPrefix, err)
	}
	if mi.embedder == nil || sw == nil {
		return nil
	}
	vectorSearcher, ok := mi.graph.(graph.VectorSearcher)
	if !ok {
		return fmt.Errorf("publish vector corpus after untrack: store lacks VectorSearcher")
	}
	publishedDims := stats.Dims
	if publishedDims <= 0 {
		publishedDims = dims
	}
	delegated := search.NewDelegatedVector(
		publishedDims,
		&vectorSearcherDelegate{s: vectorSearcher},
		stats.VectorCount,
		stats.ChunkCount,
	)
	sw.ReplaceHybridVector(delegated, mi.embedder)
	mi.logger.Info("vector corpus refreshed after repository removal",
		zap.String("repo", repoPrefix),
		zap.Int("vectors", stats.VectorCount),
		zap.Int("chunk_vectors", stats.ChunkCount),
		zap.Int("dimensions", publishedDims))
	return nil
}
