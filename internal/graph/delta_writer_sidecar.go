package graph

// Side tables of a delta.
//
// A delta's per-file side tables — symbol full-text documents, the files
// inventory, constant values, reference facts, clone shingles, the content
// index, per-file failures and the file-mtime ledger — are keyed by the paths
// the delta covers, and the published generation's file masks hide the layer
// below's rows at those paths. So every side-table WRITE the per-save engine
// makes is forwarded to the generation handle (the sidecar): what it writes
// for a covered path is exactly that path's rows in the published generation.
//
// Reads are answered where the rows live. The reference-fact read composes
// the delta's own facts (for its covered paths) over the view below's; every
// other side-table read the engine makes on the per-save path is either not
// needed by a delta (whole-repository maintenance: normalization reconcile,
// repository resets, bulk windows) or answers from the generation alone, which
// the engine already treats as "nothing recorded" — the conservative answer.
// DeltaWriterCapabilities lists every capability and its decision; the
// indexer's checklist test fails when the per-save engine asserts one that is
// not on the list.

// sidecarOr returns the sidecar as T when it implements it.
func sidecarAs[T any](dw *DeltaWriter) (T, bool) {
	v, ok := dw.sidecar.(T)
	return v, ok
}

// BulkSetFileMtimes implements FileMtimeWriter on the generation.
func (dw *DeltaWriter) BulkSetFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if w, ok := sidecarAs[FileMtimeWriter](dw); ok {
		return w.BulkSetFileMtimes(repoPrefix, mtimes)
	}
	return nil
}

// ReplaceFileMtimes implements FileMtimeReplacer on the generation.
func (dw *DeltaWriter) ReplaceFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if w, ok := sidecarAs[FileMtimeReplacer](dw); ok {
		return w.ReplaceFileMtimes(repoPrefix, mtimes)
	}
	return nil
}

// DeleteFileMtimes implements FileMtimeDeleter on the generation.
func (dw *DeltaWriter) DeleteFileMtimes(repoPrefix string, paths []string) error {
	if w, ok := sidecarAs[FileMtimeDeleter](dw); ok {
		return w.DeleteFileMtimes(repoPrefix, paths)
	}
	return nil
}

// BatchUpsertSymbolFTS implements SymbolFTSBatchUpserter on the generation.
func (dw *DeltaWriter) BatchUpsertSymbolFTS(items []SymbolFTSItem) error {
	if w, ok := sidecarAs[SymbolFTSBatchUpserter](dw); ok {
		return w.BatchUpsertSymbolFTS(items)
	}
	return nil
}

// BatchDeleteSymbolFTS implements SymbolFTSBatchDeleter on the generation. A
// document the generation does not hold is the layer below's, hidden by the
// covering file mask; deleting it here is a no-op by construction.
func (dw *DeltaWriter) BatchDeleteSymbolFTS(nodeIDs []string) error {
	if w, ok := sidecarAs[SymbolFTSBatchDeleter](dw); ok {
		return w.BatchDeleteSymbolFTS(nodeIDs)
	}
	return nil
}

// BulkSetRefFacts implements RefFactsWriter on the generation.
func (dw *DeltaWriter) BulkSetRefFacts(repoPrefix string, facts []RefFact) error {
	if w, ok := sidecarAs[RefFactsWriter](dw); ok {
		return w.BulkSetRefFacts(repoPrefix, facts)
	}
	return nil
}

// DeleteRefFactsByFiles implements RefFactsWriter on the generation.
func (dw *DeltaWriter) DeleteRefFactsByFiles(repoPrefix string, files []string) error {
	if w, ok := sidecarAs[RefFactsWriter](dw); ok {
		return w.DeleteRefFactsByFiles(repoPrefix, files)
	}
	return nil
}

// LoadRefFactsByFiles implements RefFactsReader: the delta's own facts for
// the paths it covers, the view below's for every other path.
func (dw *DeltaWriter) LoadRefFactsByFiles(repoPrefix string, files []string) ([]RefFact, error) {
	var own, rest []string
	for _, f := range files {
		if dw.layer.HasFile(f) {
			own = append(own, f)
		} else {
			rest = append(rest, f)
		}
	}
	var out []RefFact
	if len(own) > 0 {
		if r, ok := sidecarAs[RefFactsReader](dw); ok {
			facts, err := r.LoadRefFactsByFiles(repoPrefix, own)
			if err != nil {
				return nil, err
			}
			out = append(out, facts...)
		}
	}
	if len(rest) > 0 || len(files) == 0 {
		if r, ok := dw.below.(RefFactsReader); ok {
			facts, err := r.LoadRefFactsByFiles(repoPrefix, rest)
			if err != nil {
				return nil, err
			}
			for _, fact := range facts {
				if len(files) == 0 && dw.layer.HasFile(fact.FilePath) {
					continue
				}
				out = append(out, fact)
			}
		}
	}
	return out, nil
}

// LoadRefFactsByTargets implements RefFactsReader: the view below's facts at
// uncovered paths plus the delta's own.
func (dw *DeltaWriter) LoadRefFactsByTargets(repoPrefix string, targetIDs []string) (map[string][]RefFact, error) {
	out := make(map[string][]RefFact)
	if r, ok := dw.below.(RefFactsReader); ok {
		load := r.LoadRefFactsByTargets
		if dw.baseCache != nil {
			// The view below is an immutable stack: its facts per target are
			// kept per stack (the affected-by planner asks for the changed
			// declarations' targets on every save of a file). Over a dirty
			// chain they are kept for the stack below the chain, and the
			// chain's own facts are added per read.
			kept, overlay := r.LoadRefFactsByTargets, RefFactsChainOverlay(nil)
			if dw.chainLayers > 0 {
				kept = nil
				if splitter, ok := dw.below.(RefFactsChainSplitter); ok {
					if below, chain, ok := splitter.RefFactsSplitAt(dw.chainLayers); ok {
						kept, overlay = below.LoadRefFactsByTargets, chain
					}
				}
			}
			if kept != nil {
				load = func(repo string, ids []string) (map[string][]RefFact, error) {
					facts, err := dw.baseCache.stackRefFactsByTargets(repo, ids, kept)
					if err != nil || overlay == nil {
						return facts, err
					}
					return overlay(repo, ids, facts)
				}
			}
		}
		below, err := load(repoPrefix, targetIDs)
		if err != nil {
			return nil, err
		}
		for file, facts := range below {
			if dw.layer.HasFile(file) {
				continue
			}
			out[file] = append(out[file], facts...)
		}
	}
	if r, ok := sidecarAs[RefFactsReader](dw); ok {
		own, err := r.LoadRefFactsByTargets(repoPrefix, targetIDs)
		if err != nil {
			return nil, err
		}
		for file, facts := range own {
			out[file] = append(out[file], facts...)
		}
	}
	return out, nil
}

// SetFileMetas implements FileMetaWriter on the generation.
func (dw *DeltaWriter) SetFileMetas(repoPrefix string, rows []FileMetaRow) error {
	if w, ok := sidecarAs[FileMetaWriter](dw); ok {
		return w.SetFileMetas(repoPrefix, rows)
	}
	return nil
}

// DeleteFileMetasByFiles implements FileMetaWriter on the generation.
func (dw *DeltaWriter) DeleteFileMetasByFiles(repoPrefix string, files []string) error {
	if w, ok := sidecarAs[FileMetaWriter](dw); ok {
		return w.DeleteFileMetasByFiles(repoPrefix, files)
	}
	return nil
}

// ReplaceFileIndexFailures implements FileIndexFailureWriter on the
// generation.
func (dw *DeltaWriter) ReplaceFileIndexFailures(repoPrefix string, failures []FileIndexFailure) error {
	if w, ok := sidecarAs[FileIndexFailureWriter](dw); ok {
		return w.ReplaceFileIndexFailures(repoPrefix, failures)
	}
	return nil
}

// ReplaceContentFiles implements ContentFTSBatchReplacer on the generation.
func (dw *DeltaWriter) ReplaceContentFiles(repoPrefix string, files []ContentFTSFileReplacement) error {
	if w, ok := sidecarAs[ContentFTSBatchReplacer](dw); ok {
		return w.ReplaceContentFiles(repoPrefix, files)
	}
	return nil
}

// BulkSetCloneShingles implements CloneShingleWriter on the generation.
func (dw *DeltaWriter) BulkSetCloneShingles(repoPrefix string, rows map[string][]uint64) error {
	if w, ok := sidecarAs[CloneShingleWriter](dw); ok {
		return w.BulkSetCloneShingles(repoPrefix, rows)
	}
	return nil
}

// DeleteCloneShingles implements CloneShingleWriter on the generation.
func (dw *DeltaWriter) DeleteCloneShingles(nodeIDs []string) error {
	if w, ok := sidecarAs[CloneShingleWriter](dw); ok {
		return w.DeleteCloneShingles(nodeIDs)
	}
	return nil
}

// WipeContent implements ContentSearcher on the generation.
func (dw *DeltaWriter) WipeContent(repoPrefix string) error {
	if w, ok := sidecarAs[ContentSearcher](dw); ok {
		return w.WipeContent(repoPrefix)
	}
	return nil
}

// WipeContentFile implements ContentSearcher on the generation: a content row
// the generation does not hold is the layer below's, hidden by the covering
// file mask.
func (dw *DeltaWriter) WipeContentFile(filePath string) error {
	if w, ok := sidecarAs[ContentSearcher](dw); ok {
		return w.WipeContentFile(filePath)
	}
	return nil
}

// WipeContentFileInRepo is the repository-scoped WipeContentFile.
func (dw *DeltaWriter) WipeContentFileInRepo(repoPrefix, filePath string) error {
	if w, ok := sidecarAs[interface {
		WipeContentFileInRepo(repoPrefix, filePath string) error
	}](dw); ok {
		return w.WipeContentFileInRepo(repoPrefix, filePath)
	}
	return dw.WipeContentFile(filePath)
}

// ContentRepoHasRows reports whether the generation holds content rows for
// the repository: the rows a delta's wipes can reach.
func (dw *DeltaWriter) ContentRepoHasRows(repoPrefix string) (bool, error) {
	if r, ok := sidecarAs[interface {
		ContentRepoHasRows(repoPrefix string) (bool, error)
	}](dw); ok {
		return r.ContentRepoHasRows(repoPrefix)
	}
	return false, nil
}

// AppendContent implements ContentSearcher on the generation.
func (dw *DeltaWriter) AppendContent(repoPrefix string, items []ContentFTSItem) error {
	if w, ok := sidecarAs[ContentSearcher](dw); ok {
		return w.AppendContent(repoPrefix, items)
	}
	return nil
}

// SearchContent implements ContentSearcher over the generation's own rows. The
// per-save engine writes content; it does not search it.
func (dw *DeltaWriter) SearchContent(query, repoPrefix string, limit int) ([]ContentHit, error) {
	if r, ok := sidecarAs[ContentSearcher](dw); ok {
		return r.SearchContent(query, repoPrefix, limit)
	}
	return nil, nil
}

// BuildContentIndex implements ContentSearcher on the generation.
func (dw *DeltaWriter) BuildContentIndex() error {
	if w, ok := sidecarAs[ContentSearcher](dw); ok {
		return w.BuildContentIndex()
	}
	return nil
}

// ScanContent implements ContentSearcher over the generation's own rows.
func (dw *DeltaWriter) ScanContent(repoPrefix string, fn func(nodeID, filePath, body string) bool) error {
	if r, ok := sidecarAs[ContentSearcher](dw); ok {
		return r.ScanContent(repoPrefix, fn)
	}
	return nil
}

var (
	_ ContentSearcher         = (*DeltaWriter)(nil)
	_ FileMtimeWriter         = (*DeltaWriter)(nil)
	_ FileMtimeReplacer       = (*DeltaWriter)(nil)
	_ FileMtimeDeleter        = (*DeltaWriter)(nil)
	_ SymbolFTSBatchUpserter  = (*DeltaWriter)(nil)
	_ SymbolFTSBatchDeleter   = (*DeltaWriter)(nil)
	_ RefFactsWriter          = (*DeltaWriter)(nil)
	_ RefFactsReader          = (*DeltaWriter)(nil)
	_ ConstantValueWriter     = (*DeltaWriter)(nil)
	_ FileMetaWriter          = (*DeltaWriter)(nil)
	_ FileIndexFailureWriter  = (*DeltaWriter)(nil)
	_ ContentFTSBatchReplacer = (*DeltaWriter)(nil)
	_ CloneShingleWriter      = (*DeltaWriter)(nil)
)

// RefFactsChainOverlay adds a dirty chain's reference facts for targets to
// the facts the stack below the chain holds for them (below), as the view
// below the delta composes the two.
type RefFactsChainOverlay func(repoPrefix string, targetIDs []string, below map[string][]RefFact) (map[string][]RefFact, error)

// RefFactsChainSplitter is a view below a delta whose reference facts compose
// a stack of generations: it answers for the generations below the top
// chainLayers alone, and composes the top ones over such an answer.
type RefFactsChainSplitter interface {
	RefFactsSplitAt(chainLayers int) (below RefFactsReader, chain RefFactsChainOverlay, ok bool)
}
