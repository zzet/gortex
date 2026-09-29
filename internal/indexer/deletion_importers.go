package indexer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// The per-save deletion frontier.
//
// Deleting a file removes the declarations other files bound to. A whole index
// of the tree after the deletion resolves those files' references from their
// own source: a Go call into a package that no longer exists in the
// repository becomes an external terminal (`dep::<import path>::<name>`, its
// `module::` stub, the `external-call::` node), the import edge lands on
// `external::<import path>`, the dataflow rows follow. The per-save engine
// used to park such references under a bare `unresolved::<name>` and drop the
// import edge with the evicted file node: the qualifier the extractor emitted
// is gone, so no later pass could reach the whole index's rows.
//
// The frontier therefore re-derives, from source, every surviving file that
// records an edge into a deleted file's nodes: the same extraction and
// resolution a whole index runs for it. A file that is itself changed or
// deleted in the batch is already covered.

// deletionImporterFiles returns the absolute paths of the files that record an
// edge into a node of the deleted files (repository-relative paths), minus the
// deleted files, the files in skip (absolute), and paths no longer on disk.
func (idx *Indexer) deletionImporterFiles(deleted []string, skip []string) []string {
	if idx == nil || len(deleted) == 0 {
		return nil
	}
	graphPaths := make([]string, 0, len(deleted))
	deletedGraph := make(map[string]struct{}, len(deleted))
	for _, rel := range deleted {
		gp := idx.prefixPath(filepath.ToSlash(rel))
		graphPaths = append(graphPaths, gp)
		deletedGraph[gp] = struct{}{}
	}
	var ids []string
	for _, nodes := range idx.graph.GetFileNodesByPaths(graphPaths) {
		for _, node := range nodes {
			if node != nil && node.ID != "" {
				ids = append(ids, node.ID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	skipSet := make(map[string]struct{}, len(skip))
	for _, path := range skip {
		skipSet[filepath.Clean(path)] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []string
	prefix := ""
	if idx.repoPrefix != "" {
		prefix = idx.repoPrefix + "/"
	}
	for _, edges := range idx.graph.GetInEdgesByNodeIDs(ids) {
		for _, edge := range edges {
			if edge == nil || edge.FilePath == "" {
				continue
			}
			if _, gone := deletedGraph[edge.FilePath]; gone {
				continue
			}
			if prefix != "" && !strings.HasPrefix(edge.FilePath, prefix) {
				continue // recorded in another repository
			}
			rel := strings.TrimPrefix(edge.FilePath, prefix)
			abs := filepath.Clean(filepath.Join(idx.rootPath, filepath.FromSlash(rel)))
			if _, dup := seen[abs]; dup {
				continue
			}
			seen[abs] = struct{}{}
			if _, covered := skipSet[abs]; covered {
				continue
			}
			if info, err := os.Stat(abs); err != nil || info.IsDir() {
				continue
			}
			out = append(out, abs)
		}
	}
	sort.Strings(out)
	return out
}

// forceReparse reports that filePath must be re-derived from source in this
// batch even when its bytes and stored fingerprints say it is unchanged.
func (idx *Indexer) forceReparse(filePath string) bool {
	if idx.forceReparseDropsResolutions(filePath) {
		return true
	}
	if len(idx.reparseKeepingResolutions) == 0 {
		return false
	}
	_, forced := idx.reparseKeepingResolutions[filepath.Clean(filePath)]
	return forced
}

// forceReparseDropsResolutions reports a forced re-parse whose prior
// resolutions must not be reused (forcedReparse): the importer of a deleted
// file, and the unchanged files a per-file delta re-derives because the change
// can move their rows.
func (idx *Indexer) forceReparseDropsResolutions(filePath string) bool {
	if len(idx.forcedReparse) == 0 {
		return false
	}
	_, forced := idx.forcedReparse[filepath.Clean(filePath)]
	return forced
}

// withoutEdgesRecordedAt drops, from an in-edge index, the edges recorded at
// the given graph paths.
func withoutEdgesRecordedAt(in map[string][]*graph.Edge, paths map[string]struct{}) map[string][]*graph.Edge {
	if len(paths) == 0 || len(in) == 0 {
		return in
	}
	out := make(map[string][]*graph.Edge, len(in))
	for id, edges := range in {
		kept := edges[:0:0]
		for _, e := range edges {
			if e != nil {
				if _, reparsed := paths[e.FilePath]; reparsed {
					continue
				}
			}
			kept = append(kept, e)
		}
		out[id] = kept
	}
	return out
}
