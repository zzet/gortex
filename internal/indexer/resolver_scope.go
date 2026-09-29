package indexer

import (
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// stagePriorDeclarations is the incoming leg's declaration evidence for one
// structural batch: every staged file's declaration surface as it was before
// the batch evicted it. A stage whose C# using-stamp changed carries none —
// its declarations may bind differently for every dependent even when they
// are textually identical, so its keys keep the exhaustive reverse leg.
func stagePriorDeclarations(stages []*incrementalBatchStage) map[string]resolver.DeclarationSurface {
	prior := make(map[string]resolver.DeclarationSurface, len(stages))
	for _, stage := range stages {
		if stage == nil || stage.graphPath == "" || stage.result == nil {
			continue
		}
		if csharpVisibilityStampForNodes(stage.priorNodes) != csharpVisibilityStampForNodes(stage.result.Nodes) {
			continue
		}
		prior[stage.graphPath] = resolver.DeclarationSurfaceOf(stage.priorNodes)
	}
	return prior
}

// recordDeferredResolverEvidence files one structural chunk's pre-eviction
// resolver evidence for the deferred catch-up: the declaration surfaces the
// incoming leg compares against and the prior-unresolved out-edges the
// forward leg skips — the same two facts the non-deferred commit hands the
// resolver directly (SetPriorDeclarations / SetIncrementalSkip). A path
// already recorded keeps its FIRST surface: nothing resolved it in between,
// so the catch-up must compare against the state its parked references were
// last attempted against, not an intermediate one. enabled is the
// repository resolver's evidence-scoping choice (Resolver.EvidenceScoping);
// with it off nothing is recorded and the catch-up keeps the exhaustive legs.
func (b *reparsePendingEnrichmentBatch) recordDeferredResolverEvidence(
	enabled bool,
	prior map[string]resolver.DeclarationSurface,
	priorPending []*graph.Edge,
) {
	if b == nil || !enabled {
		return
	}
	b.deferredPriorPending = append(b.deferredPriorPending, priorPending...)
	if len(prior) == 0 {
		return
	}
	if b.deferredPriorDeclarations == nil {
		b.deferredPriorDeclarations = make(map[string]resolver.DeclarationSurface, len(prior))
	}
	for path, surface := range prior {
		if _, recorded := b.deferredPriorDeclarations[path]; !recorded {
			b.deferredPriorDeclarations[path] = surface
		}
	}
}

// takeDeferredResolverEvidence hands the recorded evidence to exactly one
// catch-up and forgets it, so no later resolve can compare against it.
func (b *reparsePendingEnrichmentBatch) takeDeferredResolverEvidence() (map[string]resolver.DeclarationSurface, []*graph.Edge) {
	if b == nil {
		return nil, nil
	}
	prior, pending := b.deferredPriorDeclarations, b.deferredPriorPending
	b.deferredPriorDeclarations, b.deferredPriorPending = nil, nil
	if len(prior) == 0 {
		prior = nil
	}
	if len(pending) == 0 {
		pending = nil
	}
	return prior, pending
}

// resolveWithDeferredEvidence runs the deferred catch-up's file resolve with
// the evidence the batch recorded installed on the resolver, and only that
// resolve: the affected-by and name passes that follow resolve OTHER files and
// get the exhaustive legs.
func (idx *Indexer) resolveWithDeferredEvidence(batch *reparsePendingEnrichmentBatch, resolve func()) {
	prior, priorPending := batch.takeDeferredResolverEvidence()
	if idx == nil || idx.resolver == nil || (prior == nil && priorPending == nil) {
		resolve()
		return
	}
	idx.resolver.SetPriorDeclarations(prior)
	idx.resolver.SetIncrementalSkip(priorPending)
	defer func() {
		idx.resolver.SetPriorDeclarations(nil)
		idx.resolver.SetIncrementalSkip(nil)
	}()
	resolve()
}

// logResolvePass records the whole-pass resolver cost of one index pass. The
// per-repository resolver logs nothing of its own (its logger is a no-op), so
// without this line a working-tree build's resolution time is visible only in
// a CPU profile. One record per pass, never per edge.
func (idx *Indexer) logResolvePass(stats *resolver.ResolveStats, elapsed time.Duration) {
	if idx == nil || idx.logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("repo", idx.repoPrefix),
		zap.Duration("elapsed", elapsed),
	}
	if stats != nil {
		fields = append(fields,
			zap.Int("resolved", stats.Resolved),
			zap.Int("unresolved", stats.Unresolved),
			zap.Int("external", stats.External),
			zap.Int("pending_scanned", stats.PendingBefore),
			zap.Int("pending_admitted", stats.PendingAfter))
	}
	idx.logger.Info("indexer: resolve pass", fields...)
}
