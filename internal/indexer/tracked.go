package indexer

import (
	"path/filepath"

	"github.com/zzet/gortex/internal/pathkey"
)

// IsTracked reports whether the index holds the repo-relative relPath. Unlike
// TrackedFileState it never stats the file, so it separates a tracked file
// that is missing on disk from an untracked one an editor or git just created.
func (idx *Indexer) IsTracked(relPath string) bool {
	relPath = pathkey.Normalize(filepath.ToSlash(relPath))
	idx.mtimeMu.RLock()
	_, ok := idx.fileMtimes[relPath]
	idx.mtimeMu.RUnlock()
	return ok
}
