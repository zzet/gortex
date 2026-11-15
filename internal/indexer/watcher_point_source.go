package indexer

import (
	"context"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathguard"
)

// pointDuplicateState brackets the existing persisted content receipt check.
// It is not acceptance authority: completion still validates the original raw
// mutation lease and owner. Unknown backends, snapshots, derived generations or
// any observed identity/revision movement leave completion conservative.
type pointDuplicateState struct {
	store              *store_sqlite.Store
	input              store_sqlite.PayloadInputRevision
	mutation, analysis uint64
	owner              OutputMutationTarget
	info               os.FileInfo
	root, path         string
}

func (idx *Indexer) capturePointDuplicateState(ctx context.Context, path string) *pointDuplicateState {
	if ctx == nil || ctx.Err() != nil || idx == nil || idx.contentSrc.Load() != nil {
		return nil
	}
	store, ok := idx.graph.(*store_sqlite.Store)
	if !ok || store == nil || store.ViewGeneration() != 0 {
		return nil
	}
	current, err := idx.currentRepositoryMutationIndexer()
	if err != nil || current != idx {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	if _, ok := idx.sourceRelPath(abs); !ok {
		return nil
	}
	root, err := pathguard.EvalLinks(idx.rootPath)
	if err != nil {
		return nil
	}
	resolved, err := pathguard.EvalLinks(abs)
	if err != nil || !pathguard.WithinRoot(resolved, root) {
		return nil
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	revision, err := store.PayloadInputRevisionContext(ctx)
	if err != nil {
		return nil
	}
	return &pointDuplicateState{
		store: store, input: revision, mutation: store.MutationRevision(),
		analysis: store.AnalysisStoreRevision(), owner: idx.legacyOutputTarget(),
		info: info, root: root, path: resolved,
	}
}

func (s *pointDuplicateState) matches(after *pointDuplicateState) bool {
	return s != nil && after != nil && s.store == after.store &&
		s.input == after.input && s.mutation == after.mutation &&
		s.analysis == after.analysis && s.owner == after.owner &&
		s.root == after.root && s.path == after.path && sameFileVersion(s.info, after.info)
}

func (idx *Indexer) pointDuplicateSourceContent(ctx context.Context, path string) (bool, *OutputSourceContent) {
	before := idx.capturePointDuplicateState(ctx, path)
	// Keep the original duplicate predicate even when witness proof is unknown.
	// Equal mtime alone never passes: this reads and hashes the actual bytes and
	// checks the persisted transformed content hash/size and file version.
	if !idx.fileMtimeMatches(path) {
		return false, nil
	}
	if !before.matches(idx.capturePointDuplicateState(ctx, path)) {
		return true, nil
	}
	return true, &OutputSourceContent{Root: before.owner.RootPath, Paths: []string{path}, Moved: false}
}
