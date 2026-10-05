package indexer

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The fold at the physical bound.
//
// An edit chains above the routed chain up to the physical cap
// (maxChainWalkDepth); a fold of the chain's prefix runs in the background
// meanwhile, started at dirtyChainCompactionDepth. Only an edit that finds the
// chain at the cap folds in its own cycle, and that fold is the last resort:
//
//   - no reader is served a fold that has not passed the exact check: the
//     fold is compared with the chain it replaces (verifyFlattenedChain,
//     read in batches) before it is published, as the background fold is;
//   - it is bounded: the copy and the check together run under
//     dirtyChainInlineFoldBudget, and a fold that fails the check or runs out
//     of budget is abandoned (the edit then builds direct, as it did before
//     any fold existed);
//   - it is checked a second time in the background, with the members held
//     by a lease until then. A fold that does not reproduce the chain there
//     sends the next working-tree build direct
//     (dirtyChainFallbackFoldUnverified).

// dirtyChainInlineFoldBudget bounds the copy and the check of the fold an edit
// at the physical cap makes.
const dirtyChainInlineFoldBudget = 3 * time.Second

// verifyFoldChunk is how many claimed paths (or identities) one verification
// step reads through both views.
const verifyFoldChunk = 256

// inlineFoldMismatches counts inline folds the background verification found
// wrong. Anything but zero is a bug in the copy.
var inlineFoldMismatches atomic.Int64

// inlineFoldsVerified counts inline folds the background verification found
// right.
var inlineFoldsVerified atomic.Int64

// inlineFoldBudget is the copy budget of an inline fold (a test seam may
// shorten it).
func (c *CheckoutCoordinator) inlineFoldBudget() time.Duration {
	c.compaction.mu.Lock()
	defer c.compaction.mu.Unlock()
	if c.compaction.inlineBudget > 0 {
		return c.compaction.inlineBudget
	}
	return dirtyChainInlineFoldBudget
}

// copyChainInline is the copier of an inline fold: the store's stepped fold
// when no other fold runs, the one-shot copy of the (few) layers above a
// running fold otherwise. Either way it ends within the caller's budget or
// fails.
func (c *CheckoutCoordinator) copyChainInline() chainCopier {
	return func(ctx context.Context, oldestFirst []int64, to int64) (store_sqlite.GenerationCopyCounts, func(context.Context, bool), error) {
		// The caller's context carries the budget (foldInlineAtCap).
		backend := c.foldBackend()
		fold, err := backend.BeginChainFold(ctx, oldestFirst, to, c.checkoutID+"/inline")
		if errors.Is(err, store_sqlite.ErrChainFoldBusy) {
			// The background fold holds the store's one stepped fold: the
			// layers above it are copied at once, under the same budget.
			counts, err := c.store.FlattenGenerationChain(ctx, oldestFirst, to)
			if err == nil {
				err = c.afterFoldCopy(ctx, to)
			}
			return counts, func(context.Context, bool) {}, err
		}
		if err != nil {
			return store_sqlite.GenerationCopyCounts{}, nil, err
		}
		_, _, err = runChainFoldSteps(ctx, fold, backend.StepRetryable, nil)
		var counts store_sqlite.GenerationCopyCounts
		if stepped, ok := fold.(interface {
			Counts() (store_sqlite.GenerationCopyCounts, int, int)
		}); ok {
			counts, _, _ = stepped.Counts()
		}
		if err != nil {
			_ = fold.Release(context.WithoutCancel(ctx))
			return counts, nil, err
		}
		if err := c.afterFoldCopy(ctx, to); err != nil {
			_ = fold.Release(context.WithoutCancel(ctx))
			return counts, nil, err
		}
		return counts, func(ctx context.Context, _ bool) { _ = fold.Release(ctx) }, nil
	}
}

// afterFoldCopy runs the test seam copyHook, if any, on a copied fold before
// it is checked.
func (c *CheckoutCoordinator) afterFoldCopy(ctx context.Context, to int64) error {
	c.compaction.mu.Lock()
	hook := c.compaction.copyHook
	c.compaction.mu.Unlock()
	if hook == nil {
		return nil
	}
	return hook(ctx, to)
}

// foldInlineAtCap folds, in the edit's cycle, the part of the routed chain
// topped by top that stands on root: the copy and the exact check run under
// the inline budget, and only a fold that passed the check is published. The
// background check is scheduled as the second line. It returns the fold (0
// when it was refused, failed its check or ran out of budget).
func (c *CheckoutCoordinator) foldInlineAtCap(ctx context.Context, commit, root store_sqlite.ViewGeneration, top int64, what string) int64 {
	budget := c.inlineFoldBudget()
	started := time.Now()
	members := c.dirtyChainMembers(ctx, top)
	var held *graphview.Lease
	if c.leases != nil {
		// Held until the background verification has read them: the edit's
		// flip releases the chain to the sweep.
		held = c.leases.Acquire(members...)
	}
	bounded, cancel := context.WithTimeout(ctx, budget)
	var phases *inlineFoldPhases
	if c.logger != nil && c.logger.Core().Enabled(zap.DebugLevel) {
		phases = &inlineFoldPhases{stage: "planning"}
		phases.clock = newPhaseClock(&phases.phases)
		bounded = context.WithValue(bounded, inlineFoldPhasesKey{}, phases)
	}
	built, oldestFirst, err := c.flattenDirtyChainChecked(bounded, commit, root, top, c.copyChainInline(), c.verifyFlattenedChain)
	cancel()
	elapsed := time.Since(started)
	phases.log(c.logger, c.checkoutID, top, err)
	if err != nil {
		if held != nil {
			held.Release()
		}
		c.logger.Info("checkout coordinator: inline fold at the cap refused; building direct",
			zap.String("checkout", c.checkoutID), zap.String("fold", what), zap.Int64("chain_top", top),
			zap.Duration("budget", budget), zap.Duration("elapsed", elapsed), zap.Error(err))
		return 0
	}
	c.retainDirty(ctx, built.Key, built.GenerationID)
	// members is newest first; the edit stands on the fold next.
	whole := slices.Clone(members)
	slices.Reverse(whole)
	if len(whole) >= len(oldestFirst) {
		c.handOverFoldRegistry(ctx, commit.GenerationID, whole[:len(whole)-len(oldestFirst)], oldestFirst, built.GenerationID, nil)
	}
	c.logger.Info("checkout coordinator: chain folded inline at the cap",
		zap.String("checkout", c.checkoutID), zap.String("fold", what), zap.Int64("chain_top", top),
		zap.Int64("folded_generation", built.GenerationID), zap.Duration("budget", budget), zap.Duration("elapsed", elapsed))
	c.verifyInlineFoldLater(root.GenerationID, oldestFirst, built.GenerationID, held)
	return built.GenerationID
}

// verifyInlineFoldLater verifies an inline fold in the background, in steps,
// holding neither the build lane nor the write gate, and releases the members
// once it is done.
func (c *CheckoutCoordinator) verifyInlineFoldLater(root int64, oldestFirst []int64, folded int64, held *graphview.Lease) {
	k := &c.compaction
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		if held != nil {
			defer held.Release()
		}
		ctx := c.lifetimeContext()
		started := time.Now()
		err := c.verifyFlattenedChainInSteps(ctx, root, oldestFirst, folded)
		if err == nil {
			inlineFoldsVerified.Add(1)
		}
		if err == nil || ctx.Err() != nil {
			c.logger.Info("checkout coordinator: inline fold verified",
				zap.String("checkout", c.checkoutID), zap.Int64("folded_generation", folded),
				zap.Duration("elapsed", time.Since(started)), zap.Error(err))
			return
		}
		inlineFoldMismatches.Add(1)
		k.forceDirect.Store(true)
		c.logger.Error("checkout coordinator: inline fold does not reproduce its chain; the next working-tree build is direct",
			zap.String("checkout", c.checkoutID), zap.Int64s("chain", oldestFirst),
			zap.Int64("folded_generation", folded), zap.Error(err))
		c.signalWindow("inline fold unverified", false)
	}()
}

// takeForceDirect reports, once, that the next working-tree build must not
// stand on the chain.
func (c *CheckoutCoordinator) takeForceDirect() bool {
	return c.compaction.forceDirect.CompareAndSwap(true, false)
}

// verifyFlattenedChainInSteps is verifyFlattenedChain read in chunks of
// verifyFoldChunk claimed paths or identities, the context checked and the
// processor yielded between them. It holds no lane and no write gate.
func (c *CheckoutCoordinator) verifyFlattenedChainInSteps(ctx context.Context, commitGeneration int64, oldestFirst []int64, folded int64) error {
	base, release, err := c.generationLayerReader(ctx, commitGeneration)
	if err != nil {
		return err
	}
	defer release()
	var chainView graph.Reader = base
	paths := map[string]struct{}{}
	ids := map[string]struct{}{}
	for _, id := range oldestFirst {
		layer, err := graphview.NewGenerationLayerContext(ctx, c.store.AtGeneration(id))
		if err != nil {
			return err
		}
		for _, p := range layer.FilePaths() {
			paths[p] = struct{}{}
		}
		for removed := range layer.RemovedIDs() {
			ids[removed] = struct{}{}
		}
		marks, err := c.store.AtGeneration(id).EdgeSourceMasksContext(ctx)
		if err != nil {
			return err
		}
		for _, m := range marks {
			ids[m.SourceID] = struct{}{}
		}
		chainView = graph.NewOverlaidViewWithLayer(chainView, layer)
	}
	foldedLayer, err := graphview.NewGenerationLayerContext(ctx, c.store.AtGeneration(folded))
	if err != nil {
		return err
	}
	foldedView := graph.NewOverlaidViewWithLayer(base, foldedLayer)
	if got, want := foldedLayer.FilePaths(), foldSortedKeys(paths); !foldEqualStrings(got, want) {
		return fmt.Errorf("the fold claims %d paths, the chain %d", len(got), len(want))
	}
	// The fold's own masks name identities too: a mask the chain does not
	// have hides a lower row only there, so it is read on both sides.
	for removed := range foldedLayer.RemovedIDs() {
		ids[removed] = struct{}{}
	}
	foldMarks, err := c.store.AtGeneration(folded).EdgeSourceMasksContext(ctx)
	if err != nil {
		return err
	}
	for _, m := range foldMarks {
		ids[m.SourceID] = struct{}{}
	}
	for n := range foldedLayer.DetachedNodeSummaries() {
		if n != nil {
			ids[n.ID] = struct{}{}
		}
	}
	pathList := foldSortedKeys(paths)
	idList := foldSortedKeys(ids)
	wantSeen, gotSeen := map[string]struct{}{}, map[string]struct{}{}
	for start := 0; start < len(pathList)+len(idList); start += verifyFoldChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+verifyFoldChunk, len(pathList)+len(idList))
		want, err := renderFoldChunk(ctx, chainView, pathList, idList, start, end, wantSeen)
		if err != nil {
			return err
		}
		got, err := renderFoldChunk(ctx, foldedView, pathList, idList, start, end, gotSeen)
		if err != nil {
			return err
		}
		if !foldEqualStrings(got, want) {
			return fmt.Errorf("the fold serves %d rows where the chain serves %d in step %d (first difference: %s)",
				len(got), len(want), start/verifyFoldChunk, foldFirstDifference(got, want))
		}
		runtime.Gosched()
	}
	return ctx.Err()
}

// renderFoldChunk renders, through r, the claimed paths and identities at
// positions [start, end) of paths followed by ids: every node at a path and
// every edge recorded there, every identity (or its absence), and the out-
// and in-edges of all of them. Adjacency reads are batched in groups of at
// most verifyFoldChunk identities, including nodes from a single large file.
// Legacy in-flight reads cannot be forcibly interrupted; cancellation is
// checked before and after each read and while rendering. seen carries the
// identities already rendered by an earlier chunk of the same view.
func renderFoldChunk(ctx context.Context, r graph.Reader, paths, ids []string, start, end int, seen map[string]struct{}) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []string
	var chunkPaths, chunkIDs []string
	for i := start; i < end; i++ {
		if i < len(paths) {
			chunkPaths = append(chunkPaths, paths[i])
		} else {
			chunkIDs = append(chunkIDs, ids[i-len(paths)])
		}
	}
	var visited, absent []string
	visit := func(n *graph.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == nil {
			return nil
		}
		if _, dup := seen[n.ID]; dup {
			return nil
		}
		seen[n.ID] = struct{}{}
		out = append(out, renderFoldNode(n))
		visited = append(visited, n.ID)
		return nil
	}
	for _, p := range chunkPaths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		nodes := r.GetFileNodes(p)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, n := range nodes {
			if err := visit(n); err != nil {
				return nil, err
			}
		}
	}
	if batch, ok := r.(interface {
		GetNodesByIDs(ids []string) map[string]*graph.Node
	}); ok && len(chunkIDs) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		found := batch.GetNodesByIDs(chunkIDs)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, id := range chunkIDs {
			if n := found[id]; n != nil {
				if err := visit(n); err != nil {
					return nil, err
				}
			} else {
				absent = append(absent, id)
			}
		}
	} else {
		for _, id := range chunkIDs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			n := r.GetNode(id)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if n != nil {
				if err := visit(n); err != nil {
					return nil, err
				}
			} else {
				absent = append(absent, id)
			}
		}
	}
	for _, id := range absent {
		out = append(out, "absent "+id)
	}
	appendEdges := func(edges []*graph.Edge, prefix string) error {
		for _, e := range edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			out = append(out, prefix+renderFoldEdge(e))
		}
		return nil
	}
	outIDs := append(slices.Clone(visited), absent...)
	// A single claimed path can contain thousands of identities. Keep its
	// adjacency reads in the same identity quanta as point verification.
	for first := 0; first < len(outIDs); first += verifyFoldChunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		group := outIDs[first:min(first+verifyFoldChunk, len(outIDs))]
		if batch, ok := r.(interface {
			GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge
		}); ok {
			rows := batch.GetOutEdgesByNodeIDs(group)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for _, edges := range rows {
				if err := appendEdges(edges, ""); err != nil {
					return nil, err
				}
			}
		} else {
			for _, id := range group {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				edges := r.GetOutEdges(id)
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if err := appendEdges(edges, ""); err != nil {
					return nil, err
				}
			}
		}
	}
	// In-edges too: an edge recorded at a claimed path that names an
	// identity the chain removed is visible only from its target's side.
	for first := 0; first < len(visited); first += verifyFoldChunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		group := visited[first:min(first+verifyFoldChunk, len(visited))]
		if batch, ok := r.(interface {
			GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge
		}); ok {
			rows := batch.GetInEdgesByNodeIDs(group)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for _, edges := range rows {
				if err := appendEdges(edges, "in "); err != nil {
					return nil, err
				}
			}
		} else {
			for _, id := range group {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				edges := r.GetInEdges(id)
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if err := appendEdges(edges, "in "); err != nil {
					return nil, err
				}
			}
		}
	}
	// Every edge recorded at a claimed path, whichever its endpoints.
	if recorded, ok := graph.RecordedEdgesOf(r); ok && len(chunkPaths) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		edges := recorded.RecordedEdgesAt(chunkPaths)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := appendEdges(edges, "at "); err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
