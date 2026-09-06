package indexer

import (
	"context"
	"errors"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

var errColdManifestIncomplete = errors.New("cold manifest processing did not complete")

// A durable backend must be able to invalidate a previously successful
// receipt. Unsupported writer-only backends keep the old manifest behavior;
// do not introduce receipts that they cannot later invalidate safely.
func supportsColdManifestReceipts(store graph.Store) bool {
	_, writer := store.(graph.FileMtimeWriter)
	_, replacer := store.(graph.FileMtimeReplacer)
	if !writer && !replacer {
		return true
	}
	_, ok := store.(graph.FileMtimeDeleter)
	return ok
}

// Caller has already applied root-manifest admission and ignore/security
// gates. This runs during the existing filesystem walk, before any shadow.
func (idx *Indexer) captureColdManifest(census **coldManifestCensus, ctx context.Context, path string, priorErr error) {
	if !supportsColdManifestReceipts(idx.graph) {
		return
	}
	if *census == nil {
		*census = &coldManifestCensus{ctx: ctx, manifests: make(map[string]*coldManifestRead, 2)}
		idx.pendingColdManifests = *census
	}
	key := idx.relKey(path)
	if prior := (*census).manifests[key]; prior != nil && prior.err != nil {
		return
	}
	var src []byte
	var version fileReadVersion
	err := priorErr
	if err == nil {
		src, version, err = idx.readFileWithVersion(path)
	}
	if err == nil && !version.valid {
		err = errFileVersionChanged
	}
	manifest := &coldManifestRead{
		source: src, err: err,
		receipt:         fileReadReceipt{absPath: path, mtimeKey: key, readVersion: version},
		modulesRequired: key == "go.mod" && idx.config.Coverage.IsEnabled("modules"),
	}
	if key == "go.work" {
		manifest.source = nil // no cold byte consumer; keep only the version
	}
	(*census).manifests[key] = manifest
	if err != nil {
		(*census).failed = true
		idx.noteFileIndexFailure(path, err)
	}
}

func (idx *Indexer) coldManifestsForRegistry(reg *contracts.Registry) *coldManifestCensus {
	b := idx.pendingColdManifests
	if b == nil || reg == nil || b.registry != reg {
		return nil
	}
	return b
}

// Both cold module consumers use the one captured go.mod version. Generic
// callers pass nil and retain their existing read/extraction behavior.
func (idx *Indexer) extractExternalModulesForCensus(reg *contracts.Registry) {
	if !idx.config.Coverage.IsEnabled("modules") {
		return
	}
	b := idx.coldManifestsForRegistry(reg)
	if b == nil {
		idx.extractExternalModules()
		return
	}
	for _, spec := range rootManifests() {
		var manifest *coldManifestRead
		if b != nil {
			manifest = b.manifests[spec.path]
		}
		if manifest == nil {
			idx.extractOneModuleManifest(spec.path, spec.parse, spec.ownPathFromSrc)
		} else if manifest.err == nil {
			idx.extractOneModuleManifestSource(spec.path, manifest.source, spec.parse, spec.ownPathFromSrc)
			manifest.modulesDone = true
		}
	}
	idx.extractPackageWorkspace()
}

// Called after final graph/FTS/vector publication, and after actual contract
// commit. Either order is valid, but neither boundary alone may certify a
// manifest or authoritatively prune the previous census.
func (idx *Indexer) finishColdManifests(b *coldManifestCensus) {
	if b == nil || idx.pendingColdManifests != b || !b.graphReady {
		return
	}
	if b.ctx.Err() != nil {
		paths := make([]string, 0, len(b.manifests))
		for path := range b.manifests {
			paths = append(paths, path)
		}
		idx.invalidateColdFileMtimes(paths)
		idx.pendingColdManifests = nil
		return
	}
	if !b.contractsReady {
		// Preserve early parser progress throughout long enrichment. Old
		// manifest receipts cannot certify the newly published graph yet.
		idx.publishColdFileMtimes(b.census, false)
		paths := make([]string, 0, len(b.manifests))
		for path := range b.manifests {
			paths = append(paths, path)
		}
		idx.invalidateColdFileMtimes(paths)
		return
	}
	var receipts []fileReadReceipt
	var failedPaths []string
	for path, manifest := range b.manifests {
		if manifest.err == nil && path == "go.mod" &&
			(!manifest.dependencyDone || (manifest.modulesRequired && !manifest.modulesDone)) {
			manifest.err = errColdManifestIncomplete
		}
		if manifest.err != nil {
			b.failed = true
			failedPaths = append(failedPaths, path)
			idx.noteFileIndexFailure(manifest.receipt.absPath, manifest.err)
			continue
		}
		receipts = append(receipts, manifest.receipt)
	}
	// Existing helper revalidates identity/size/mtime and publishes only
	// successful versions. Never pass a latched failed receipt to it.
	fresh, stale := idx.recordFileReadVersionsBatched(receipts)
	for _, path := range fresh {
		key := idx.relKey(path)
		b.census[key] = b.manifests[key].receipt.readVersion.mtime
	}
	for _, path := range stale {
		failedPaths = append(failedPaths, idx.relKey(path))
		b.failed = true
	}
	idx.invalidateColdFileMtimes(failedPaths)
	idx.publishColdFileMtimes(b.census, !b.failed && b.ctx.Err() == nil)
	idx.flushFileIndexFailures()
	idx.pendingColdManifests = nil
}

// Remove only specifically failed/pending receipts; never prune unrelated
// old keys on failure. Replacement cannot safely emulate deletion: empty
// input is a no-op and the local map may omit durable peers.
func (idx *Indexer) invalidateColdFileMtimes(paths []string) {
	if len(paths) == 0 {
		return
	}
	idx.mtimeMu.Lock()
	idx.ensureFileMtimesWritableLocked()
	for _, path := range paths {
		delete(idx.fileMtimes, path)
	}
	idx.mtimeMu.Unlock()
	if _, ok := idx.graph.(graph.FileMtimeDeleter); ok {
		idx.pruneDeletedFileMtimes(paths)
	} else {
		_, writer := idx.graph.(graph.FileMtimeWriter)
		_, replacer := idx.graph.(graph.FileMtimeReplacer)
		if writer || replacer {
			idx.markFileMtimePersistenceDirty()
			idx.logger.Warn("cannot invalidate cold file receipts without deletion support",
				zap.String("repo", idx.repoPrefix), zap.Int("count", len(paths)))
		}
	}
}

// The previous full-index persistence block, shared by early progress and
// final replacement. Existing COW ownership and dirty-state APIs are reused.
func (idx *Indexer) publishColdFileMtimes(mtimes map[string]int64, authoritative bool) {
	if !authoritative && len(mtimes) == 0 {
		return
	}
	// A durable write failure marks retry state; it must not hide already
	// committed parser progress from the current process or its watchers.
	if authoritative {
		idx.SetFileMtimes(mtimes)
	} else {
		idx.mtimeMu.Lock()
		idx.ensureFileMtimesWritableLocked()
		for path, mtime := range mtimes {
			idx.fileMtimes[path] = mtime
		}
		idx.mtimeMu.Unlock()
	}
	var err error
	persisted, replaced := false, false
	if authoritative && len(mtimes) == 0 {
		// ReplaceFileMtimes deliberately treats empty input as a no-op.
		// This boundary proves a complete successful census, so explicitly
		// delete the persisted roster, not merely this Indexer's old map.
		reader, canRead := idx.graph.(graph.FileMtimeReader)
		deleter, canDelete := idx.graph.(graph.FileMtimeDeleter)
		if canRead && canDelete {
			stored := reader.LoadFileMtimes(idx.repoPrefix)
			paths := make([]string, 0, len(stored))
			for path := range stored {
				paths = append(paths, path)
			}
			err = deleter.DeleteFileMtimes(idx.repoPrefix, paths)
			persisted, replaced = true, true
			if err == nil {
				idx.fileMtimePersistenceDirty.Store(false)
			}
		} else {
			_, writer := idx.graph.(graph.FileMtimeWriter)
			_, replacer := idx.graph.(graph.FileMtimeReplacer)
			if writer || replacer {
				err = errors.New("store cannot clear a completed empty file census")
			}
		}
	} else if replacer, ok := idx.graph.(graph.FileMtimeReplacer); authoritative && ok {
		err = replacer.ReplaceFileMtimes(idx.repoPrefix, mtimes)
		persisted, replaced = true, true
		if err == nil {
			idx.fileMtimePersistenceDirty.Store(false)
		}
	} else if writer, ok := idx.graph.(graph.FileMtimeWriter); ok {
		err = writer.BulkSetFileMtimes(idx.repoPrefix, mtimes)
		persisted = true
	}
	if err != nil {
		idx.markFileMtimePersistenceDirty()
		idx.logger.Warn("persist file mtimes failed", zap.String("repo", idx.repoPrefix), zap.Error(err))
	} else if persisted {
		idx.logger.Info("persisted file mtimes", zap.String("repo", idx.repoPrefix),
			zap.Int("count", len(mtimes)), zap.Bool("authoritative", replaced))
	}
}

func (mi *MultiIndexer) refreshColdCensusMetadata(idx *Indexer) {
	mtimes := idx.publishFileMtimes()
	mi.mu.Lock()
	defer mi.mu.Unlock()
	meta := mi.repos[idx.repoPrefix]
	if meta == nil || mi.indexers[idx.repoPrefix] != idx {
		return
	}
	next := *meta
	next.FileMtimes = mtimes
	mi.repos[idx.repoPrefix] = &next
}

// Called by indexCtxRaw's post-publication defer. The existing successfulFiles
// map supplies parser candidates; the existing failure ledger supplies exact
// invalidations. No parallel per-file outcome state is introduced.
func (idx *Indexer) finishColdIndexCensus(ctx context.Context, result *IndexResult, indexErr error,
	mtimes map[string]int64, b *coldManifestCensus, failed bool) {
	if b != nil && idx.pendingColdManifests != b {
		return
	}
	if idx.contentSource() != nil {
		if indexErr == nil && result != nil && mtimes != nil {
			idx.SetFileMtimes(nil)
		}
		return // immutable sources never write filesystem receipts
	}
	var paths []string
	for _, path := range idx.fileIndexFailurePaths() {
		if rel, ok := idx.graphPathRelKey(path); ok {
			paths = append(paths, rel)
		}
	}
	idx.invalidateColdFileMtimes(paths)
	if indexErr != nil || result == nil || mtimes == nil {
		// Graph publication can succeed before a later FTS/vector boundary
		// fails. Old receipts must not certify that incomplete replacement.
		// Invalidate only this attempt's captured manifests, not prior peers.
		if b != nil {
			paths = paths[:0]
			for path := range b.manifests {
				paths = append(paths, path)
			}
			idx.invalidateColdFileMtimes(paths)
		}
		idx.pendingColdManifests = nil
		return
	}
	if b != nil {
		b.graphReady = true
		b.census = mtimes
		b.failed = b.failed || failed
	}
	if ctx.Err() != nil {
		idx.finishColdManifests(b) // invalidate only pending manifest receipts
		return
	}
	if b == nil {
		idx.publishColdFileMtimes(mtimes, !failed && ctx.Err() == nil)
	} else {
		b.failed = b.failed || failed
		idx.finishColdManifests(b)
	}
}
