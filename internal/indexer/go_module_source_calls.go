package indexer

import (
	"crypto/sha256"
	"github.com/zzet/gortex/internal/parser/crashpool"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

// goModuleSourceCacheEntry keeps source facts, not resolution decisions.
// Matching size and mtime reuse those facts. If either differs, the digest
// decides whether to reuse them.
type goModuleSourceCacheEntry struct {
	digest         [32]byte
	edges          []*graph.Edge
	size, modified int64
}

var readGoModuleCallerFile = os.ReadFile

func cloneGoModuleSourceEdges(edges []*graph.Edge) []*graph.Edge {
	out := make([]*graph.Edge, 0, len(edges))
	for _, e := range edges {
		if resolver.GoModuleCallSpec(e) == "" {
			continue
		}
		c := *e
		c.Meta = maps.Clone(e.Meta)
		out = append(out, &c)
	}
	return out
}

// A successful replacement can change bytes without changing the stat.
// Keep source facts, but make recovery validate them with the digest. The
// epoch also tells a recovery that read before this save, even of a file not
// cached yet, not to trust its stat when it stores its result.
func (idx *Indexer) invalidateGoModuleSourceStamp(rel string) {
	idx.goModuleSourceMu.Lock()
	defer idx.goModuleSourceMu.Unlock()
	idx.goModuleSourceEpoch++
	if cached, ok := idx.goModuleSourceCache[rel]; ok {
		cached.size, cached.modified = 0, 0
		idx.goModuleSourceCache[rel] = cached
	}
}

// storeGoModuleSource keeps the facts but drops stat trust when a save or an
// eviction landed after epoch was read, since the bytes may predate it.
func (idx *Indexer) storeGoModuleSource(rel string, entry goModuleSourceCacheEntry, epoch uint64) {
	idx.goModuleSourceMu.Lock()
	defer idx.goModuleSourceMu.Unlock()
	if idx.goModuleSourceEpoch != epoch {
		entry.size, entry.modified = 0, 0
	}
	if idx.goModuleSourceCache == nil {
		idx.goModuleSourceCache = make(map[string]goModuleSourceCacheEntry)
	}
	idx.goModuleSourceCache[rel] = entry
}

func (mi *MultiIndexer) finishGoModuleCalls(cr *resolver.CrossRepoResolver) {
	for _, file := range cr.TakeGoModuleSourceFiles() {
		mi.restoreGoModuleSourceFile(cr, file)
	}
	mi.synthesizeDemotedGoModuleCalls(cr.TakeDemotedGoModuleCallFiles())
}

func (mi *MultiIndexer) restoreGoModuleSourceFile(cr *resolver.CrossRepoResolver, file string) {
	prefix := graph.RepoPrefixOfID(file)
	idx := mi.GetIndexer(prefix)
	if idx == nil {
		return
	}
	rel := strings.TrimPrefix(file, prefix+"/")
	abs := filepath.Join(idx.rootPath, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		mi.logger.Warn("stat Go module caller", zap.String("file", file), zap.Error(err))
		return
	}
	idx.goModuleSourceMu.Lock()
	cached, hit := idx.goModuleSourceCache[rel]
	epoch := idx.goModuleSourceEpoch
	idx.goModuleSourceMu.Unlock()
	if hit && cached.size == info.Size() && cached.modified == info.ModTime().UnixNano() {
		cr.RestoreGoModuleSourceCalls(cloneGoModuleSourceEdges(cached.edges))
		return
	}
	parseLease, err := idx.acquireSharedParsePath(abs)
	if err != nil {
		mi.logger.Warn("admit Go module caller", zap.String("file", file), zap.Error(err))
		return
	}
	defer parseLease.Release()
	src, err := readGoModuleCallerFile(abs)
	if err != nil {
		mi.logger.Warn("read Go module caller", zap.String("file", file), zap.Error(err))
		return
	}
	if idx.config.MaxFileSize > 0 && int64(len(src)) > idx.config.MaxFileSize {
		return
	}
	ext, ok := idx.registry.GetByLanguage("go")
	if !ok {
		return
	}
	src = idx.transforms.run(rel, src)
	digest := sha256.Sum256(src)
	if hit && cached.digest == digest {
		cached.size, cached.modified = info.Size(), info.ModTime().UnixNano()
		idx.storeGoModuleSource(rel, cached, epoch)
		cr.RestoreGoModuleSourceCalls(cloneGoModuleSourceEdges(cached.edges))
		return
	}
	var pool *crashpool.Pool
	var quarantine *crashpool.Quarantine
	if idx.crashIsolationEnabled() {
		pool, quarantine = idx.sharedParsePool()
	}
	result, skipped, err := idx.extractFileWithRawLease(parseLease, pool, quarantine, abs, rel, "go", ext, src)
	if quarantine != nil && quarantine.Len() > 0 {
		_ = quarantine.Save()
	}
	if err != nil {
		mi.logger.Warn("extract Go module caller", zap.String("file", file), zap.Error(err))
	}
	if result == nil {
		return
	}
	if err == nil && !skipped {
		idx.applyRepoPrefix(result.Nodes, result.Edges)
		idx.storeGoModuleSource(rel, goModuleSourceCacheEntry{digest: digest, edges: cloneGoModuleSourceEdges(result.Edges), size: info.Size(), modified: info.ModTime().UnixNano()}, epoch)
		cr.RestoreGoModuleSourceCalls(result.Edges)
	}
	result.ReleaseTree()
}
