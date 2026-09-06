package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/indexer/merkle"
)

// merkleTreeFile is where the per-repo Merkle tree is persisted —
// alongside the parser quarantine, under the repo's .gortex directory.
func merkleTreeFile(rootAbs string) string {
	return filepath.Join(rootAbs, ".gortex", "merkle.json")
}

// merkleEnabled reports whether incremental re-index should detect
// changes with a BLAKE3 Merkle tree instead of per-file mtime
// comparison. GORTEX_MERKLE overrides the index.merkle config key.
func (idx *Indexer) merkleEnabled() bool {
	if v := os.Getenv("GORTEX_MERKLE"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return idx.config.Merkle
}

// merkleBaselineCollector retains compact raw-content identities while full
// indexing already owns each source buffer. It deliberately records raw bytes
// before transforms: FileMeta hashes describe transformed parser input, while
// Merkle leaves must describe the repository snapshot on disk.
type merkleBaselineCollector struct {
	mu    sync.Mutex
	files map[string]merkle.FileNode
}

func newMerkleBaselineCollector(enabled bool, capacity int) *merkleBaselineCollector {
	if !enabled {
		return nil
	}
	return &merkleBaselineCollector{files: make(map[string]merkle.FileNode, capacity)}
}

func (c *merkleBaselineCollector) record(rel string, src []byte, mtime int64) {
	if c == nil || rel == "" || mtime <= 0 {
		return
	}
	node := merkle.FileNode{Hash: contentHashForSource(src), Mtime: mtime}
	c.mu.Lock()
	c.files[filepath.ToSlash(rel)] = node
	c.mu.Unlock()
}

// take transfers ownership after all parse workers have joined. Missing entries
// are intentional: streaming/unreadable files use the builder's disk fallback.
func (c *merkleBaselineCollector) take() map[string]merkle.FileNode {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	files := c.files
	c.files = nil
	c.mu.Unlock()
	return files
}

// merkleStaleFiles returns the absolute paths of files whose content
// changed since the last pass. It rebuilds the Merkle tree (reusing
// hashes for mtime-unchanged files, so only changed files are re-read),
// diffs it against the persisted tree, persists the new tree, and maps
// the changed repo-relative paths back to absolute paths. A file
// touched without a content change is not reported, unlike the
// bare-mtime path.
func (idx *Indexer) merkleStaleFiles(rootAbs string, diskFiles map[string]bool) []string {
	return idx.merkleStaleFilesInScope(rootAbs, diskFiles, nil)
}

// A scoped pass cannot replace the repository baseline with only its frontier:
// the next full pass would then rediscover unchanged files outside that scope.
// Preserve those prior leaves verbatim, without reading or re-salting them.
func (idx *Indexer) merkleStaleFilesInScope(rootAbs string, diskFiles map[string]bool, contains func(string) bool) []string {
	rels := make([]string, 0, len(diskFiles))
	for rel := range diskFiles {
		if contains == nil || contains(rel) {
			rels = append(rels, rel)
		}
	}
	treePath := merkleTreeFile(rootAbs)
	prior, _ := merkle.Load(treePath)
	tree := merkle.Build(rootAbs, rels, prior, merkleSaltFor)
	changed, _ := tree.Diff(prior)
	if contains != nil {
		tree = merkle.MergeScope(prior, tree, contains)
	}
	if err := tree.Save(treePath); err != nil {
		idx.logger.Warn("indexer: merkle tree save failed", zap.Error(err))
	}
	stale := make([]string, 0, len(changed))
	for _, rel := range changed {
		stale = append(stale, filepath.Join(rootAbs, filepath.FromSlash(rel)))
	}
	return stale
}

func (idx *Indexer) saveMerkleBaselineWithKnownFiles(
	rootAbs string,
	absFiles []string,
	knownFiles map[string]merkle.FileNode,
) string {
	rels := make([]string, 0, len(absFiles))
	for _, f := range absFiles {
		if rel, err := filepath.Rel(rootAbs, f); err == nil {
			rels = append(rels, filepath.ToSlash(rel))
		}
	}
	tree := merkle.BuildWithKnownFiles(rootAbs, rels, nil, merkleSaltFor, knownFiles)
	if err := tree.Save(merkleTreeFile(rootAbs)); err != nil {
		idx.logger.Warn("indexer: merkle baseline save failed", zap.Error(err))
	}
	return tree.Root
}
