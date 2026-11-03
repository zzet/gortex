package indexer

import (
	"context"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// A source refresh must not fall back to a legacy metadata call which cannot
// carry its cancellation/deadline. Unsupported readers leave matching unknown;
// the caller may use its existing bounded physical projection path instead.
type fileMetaPathContextReader interface {
	FileMetasByPathsContext(context.Context, string, []string) (map[string]graph.FileMetaRow, error)
}

// IndexedContentMatchesContext checks recorded spans without outliving the
// source refresh's original cancellation or deadline.
func (idx *Indexer) IndexedContentMatchesContext(ctx context.Context, relPath string, src []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	reader, ok := idx.graph.(fileMetaPathContextReader)
	if !ok {
		return false, nil
	}
	relPath = pathkey.Normalize(filepath.ToSlash(relPath))
	graphPath := idx.prefixPath(relPath)
	rows, err := reader.FileMetasByPathsContext(ctx, idx.repoPrefix, []string{graphPath})
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err != nil {
		return false, nil // Preserve "canonical content unknown" on ordinary metadata errors.
	}
	row, ok := rows[graphPath]
	if !ok || row.ContentHash == "" {
		return false, nil
	}
	src = idx.transforms.run(relPath, src)
	matches := len(src) == row.Size && contentHashForSource(src) == row.ContentHash
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return matches, nil
}

// IndexedContentMatches reports whether src, bytes the caller already read
// for the repo-relative relPath, are the content the index last recorded for
// that file, so the spans of the file's stored nodes describe src exactly. It
// compares the persisted content identity (size and BLAKE3 over the
// transformed bytes) and answers false whenever the backend cannot tell.
func (idx *Indexer) IndexedContentMatches(relPath string, src []byte) bool {
	reader, ok := idx.graph.(graph.FileMetaPathReader)
	if !ok {
		return false
	}
	relPath = pathkey.Normalize(filepath.ToSlash(relPath))
	graphPath := idx.prefixPath(relPath)
	rows, err := reader.FileMetasByPaths(idx.repoPrefix, []string{graphPath})
	if err != nil {
		return false
	}
	row, ok := rows[graphPath]
	if !ok || row.ContentHash == "" {
		return false
	}
	src = idx.transforms.run(relPath, src)
	return len(src) == row.Size && contentHashForSource(src) == row.ContentHash
}
