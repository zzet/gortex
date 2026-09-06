package indexer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// Load once per census. SQLite restricts repo/kind/current generation before
// hydration; adapters retain their existing view-aware projection semantics.
// This intentionally reuses the existing API rather than a new schema or
// decoder. Its cost is proportional to the repository's file-node population.
func (idx *Indexer) sizeSkipCensusNodes() map[string]*graph.Node {
	// A repository-wide census must include legacy file stubs whose workspace
	// was never stamped. Repo prefix and the selected store still scope the
	// read; an optional workspace filter would hide those legitimate stubs.
	nodes := graph.ReadRepoNodesByKindsWithMetaKey(idx.graph, idx.repoPrefix,
		"", []graph.NodeKind{graph.KindFile}, "skipped_due_to_size")
	out := make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		// Keep malformed prior markers too: an equal-mtime policy increase
		// must not mistake a damaged skip stub for an already parsed file.
		if rel, ok := idx.graphPathRelKey(node.FilePath); ok {
			out[rel] = node
		}
	}
	return out
}

// info must come from the census's os.Stat so policy and mtime checks reuse
// one syscall and preserve IsStale's follow-stat behavior. The caller has
// already applied the existing language/manifest and exclusion gates.
func (idx *Indexer) sizeSkipCensusIsStale(relPath string, info os.FileInfo, oversize bool, prior map[string]*graph.Node) bool {
	relPath = pathkey.Normalize(filepath.ToSlash(relPath))
	idx.mtimeMu.RLock()
	storedMtime, known := idx.fileMtimes[relPath]
	idx.mtimeMu.RUnlock()
	if !known || info == nil || info.ModTime().UnixNano() != storedMtime {
		return true
	}
	stub := prior[relPath]
	// The caller reuses its one language check and central strict size-cap
	// predicate. Language-less root manifests are not subject to that cap.
	if !oversize {
		return stub != nil // equal-mtime policy increase admits the prior skip
	}
	if stub == nil || stub.Meta["skipped_due_to_size"] != true || stub.Meta["skip_reason"] != "size" {
		return true // formerly parsed/missing/wrong stub must refresh, not hide
	}
	size, sizeOK := sizeSkipMetaInt64(stub.Meta["file_size_bytes"])
	limit, limitOK := sizeSkipMetaInt64(stub.Meta["max_file_size_bytes"])
	return !sizeOK || !limitOK || size != info.Size() || limit != idx.config.MaxFileSize
}

func sizeSkipMetaInt64(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case int:
		return int64(value), true
	case float64:
		integer := int64(value)
		return integer, float64(integer) == value
	case json.Number:
		integer, err := value.Int64()
		return integer, err == nil
	default:
		return 0, false
	}
}

// Capture metadata identity only: intentionally skipped assets must never be
// read into memory merely to mint a receipt. The normal post-publication
// receipt validator must still check this version before persisting it.
func (idx *Indexer) coldSizeSkipReceipt(path string, info os.FileInfo) fileReadReceipt {
	receipt := fileReadReceipt{absPath: path, mtimeKey: idx.relKey(path)}
	if runtime.GOOS == "windows" {
		// Some Windows directory metadata defers identity lookup until
		// SameFile. Freeze it now, before the path can be replaced.
		receipt.readVersion = coldSizeSkipDescriptorVersion(path, info)
	} else {
		receipt.readVersion = fileReadVersion{
			info: info, mtime: info.ModTime().UnixNano(), size: info.Size(), valid: true,
		}
	}
	return receipt
}

// File.Stat captures identity from the open handle without reading file bytes.
// The helper is platform-neutral so its failure and identity boundaries can be
// tested directly; only Windows cold capture needs this extra handle. A failed
// capture returns an invalid version, which the normal receipt finalizer marks
// stale and invalidates rather than certifying an old durable mtime.
func coldSizeSkipDescriptorVersion(path string, walked os.FileInfo) fileReadVersion {
	if walked == nil || !walked.Mode().IsRegular() {
		return fileReadVersion{}
	}
	file, err := os.Open(path)
	if err != nil {
		return fileReadVersion{}
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !info.Mode().IsRegular() ||
		info.Size() != walked.Size() || info.ModTime().UnixNano() != walked.ModTime().UnixNano() {
		return fileReadVersion{}
	}
	return fileReadVersion{
		info: info, mtime: info.ModTime().UnixNano(), size: info.Size(), valid: true,
	}
}

// Invoked by the existing post-publication full-index defer, before its
// manifest census finalizer. Capture callers must first require
// supportsColdManifestReceipts, so failed receipts remain invalidatable.
// The slice belongs only to this raw-index attempt; no deferred state is
// introduced for policy skips.
func (idx *Indexer) finishColdSizeSkips(ctx context.Context, receipts []fileReadReceipt, mtimes map[string]int64, published bool) bool {
	if len(receipts) == 0 {
		return false
	}
	if !published || ctx.Err() != nil {
		paths := make([]string, 0, len(receipts))
		for _, receipt := range receipts {
			paths = append(paths, receipt.mtimeKey)
		}
		idx.invalidateColdFileMtimes(paths)
		return true
	}
	fresh, stale := idx.recordFileReadVersionsBatched(receipts)
	accepted := make(map[string]struct{}, len(fresh))
	for _, path := range fresh {
		accepted[idx.relKey(path)] = struct{}{}
	}
	for _, receipt := range receipts {
		if _, ok := accepted[receipt.mtimeKey]; ok {
			mtimes[receipt.mtimeKey] = receipt.readVersion.mtime
		}
	}
	failed := make([]string, 0, len(stale))
	for _, path := range stale {
		failed = append(failed, idx.relKey(path))
	}
	idx.invalidateColdFileMtimes(failed)
	return len(stale) != 0
}
