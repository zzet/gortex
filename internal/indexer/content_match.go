package indexer

import (
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

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
