package indexer

import (
	"errors"

	"github.com/zzet/gortex/internal/resolver"
)

// relationsReadFrontier belongs to one point executor and one absolute path.
// It records a conservative admission bound immediately before an OS source
// read. Only the existing successful post-commit version fence can validate it.
// It is never authority for Done, applied generation, derived work or another
// path. The executor owns it; no mutable Indexer/global tracing state is used.
type relationsReadFrontier struct {
	path    string
	capture func() uint64
	covered uint64
}

func firstRelationsRead(reads []*relationsReadFrontier) *relationsReadFrontier {
	if len(reads) == 0 {
		return nil
	}
	return reads[0]
}

func (f *relationsReadFrontier) invalidate(path string) {
	if f != nil && path == f.path {
		f.covered = 0
	}
}

func (f *relationsReadFrontier) beforeRead(path string) uint64 {
	if f == nil || path != f.path {
		return 0
	}
	f.covered = 0 // A retry/new read cannot reuse an older validation.
	return f.capture()
}

func (f *relationsReadFrontier) accept(path string, version fileReadVersion) {
	if f != nil && path == f.path && version.relationsRead == f && version.valid && !version.snapshot {
		f.covered = version.relationsGeneration
	}
}

func (b *reparsePendingEnrichmentBatch) relationsReadFrontier() *relationsReadFrontier {
	if b == nil {
		return nil
	}
	return b.relationsRead
}

func (idx *Indexer) observePointRebind(batch *reparsePendingEnrichmentBatch, stats *resolver.ResolveStats) {
	read := batch.relationsReadFrontier()
	if read == nil || stats == nil || len(stats.RefusedRebindFiles) == 0 {
		return
	}
	if _, refused := stats.RefusedRebindFiles[idx.prefixPath(idx.relKey(read.path))]; refused {
		batch.pointRebindRefused = true
	}
}

// ownedPointRebindOnly reports whether a resolution write refusal belongs
// exclusively to the checked point-rebind path owned by Watcher.pointRelationsMode:
// no uncertified resolution-input pass, no carried resolution retry, no dropped
// invalidation writes, and no refused caller files outside the owned path.
// In that case Watcher.markRefusedRelationTiers records the partial/refused state
// on the ticket and preserves it across same-byte duplicate saves without turning
// the already-landed structural write into a failed mutation or forcing a silent
// duplicate rebind.
func (idx *Indexer) ownedPointRebindOnly(
	batch *reparsePendingEnrichmentBatch,
	result *IndexResult,
	extraRefused bool,
) bool {
	read := batch.relationsReadFrontier()
	if idx == nil || read == nil || batch == nil || !batch.pointRebindRefused || extraRefused {
		return false
	}
	if idx.hasUncertifiedResolutionPass() || (result != nil && result.retriedResolution) {
		return false
	}
	if len(batch.resolution.dropped) > 0 {
		return false
	}
	if !errors.Is(batch.resolution.catchupErr, resolver.ErrReindexWriteRefused) {
		return false
	}
	ownedPath := idx.prefixPath(idx.relKey(read.path))
	for _, f := range batch.resolution.retryResolveFiles {
		if f != ownedPath {
			return false
		}
	}
	return true
}

func (w *Watcher) pointRelationsMode(path string, generation uint64, stamps *PublicationStamps) incrementalPathMode {
	read := &relationsReadFrontier{path: path, capture: func() uint64 {
		w.mu.Lock()
		defer w.mu.Unlock()
		admitted := w.pendingGeneration[path]
		waiter := w.mutationWaiters[path][admitted]
		if w.stopping || !waiter.tool || waiter.ticket == nil || waiter.ticket.Path != path || waiter.ticket.Generation != admitted {
			return 0
		}
		return admitted
	}}
	return incrementalPathMode{
		detectDeletions: true, forceExplicitFiles: true, publication: stamps,
		relationsRead: read,
		onRelations: func(refused bool) {
			if refused {
				w.markRefusedRelationTiers(path, generation, false)
				return
			}
			w.markRelations(path, max(generation, read.covered), generation)
		},
	}
}
