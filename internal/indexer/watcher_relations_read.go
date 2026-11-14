package indexer

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
		onRelations:   func() { w.markRelations(path, max(generation, read.covered)) },
	}
}
