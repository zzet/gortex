package indexer

import "github.com/zzet/gortex/internal/indexer/source"

// setTrackContentSource installs a borrowed full-tree source on an indexer.
// Nil intentionally leaves the indexer's legacy filesystem source selection
// untouched. Ownership stays with the caller.
func setTrackContentSource(idx *Indexer, content source.ContentSource) {
	if idx != nil && content != nil {
		idx.SetContentSource(content)
	}
}
