package indexer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/indexer/source"
)

// walkAdmission is the verdict of the walk-entry gate every index walk
// shares — the bulk walk, the scoped incremental walk, and the walk over
// a content source.
type walkAdmission struct {
	// lang is the language the registry (or a transform rule) claims the
	// entry for, and is empty when nothing claims it.
	lang string
	// admit reports that the entry cleared every gate and belongs in the
	// walk's file set.
	admit bool
	// pruneDir reports that a directory's whole subtree can be skipped.
	// Only a directory entry ever sets it.
	pruneDir bool
	// oversize reports the one rejection callers still account for
	// themselves: a recognised, non-excluded file over MaxFileSize. The
	// bulk walk reports those to the user; a caller that passes a
	// negative size never sees it.
	oversize bool
	// excluded reports that an exclude / ignore RULE rejected the entry.
	// It carries what lang can no longer imply: exclusion is decided
	// before language detection, so an excluded entry has no lang to
	// distinguish it from one nothing claims.
	excluded bool
	// manifest reports a root go.mod or go.work that cleared the ignore
	// rules. No extractor claims either, but the walks keep them in their
	// file set for the module contracts.
	manifest bool
}

// admitWalkEntry applies the walk-time gates every index walk shares:
// directory pruning, language detection, the effective ignore list, and
// the configured size cap.
//
// absPath is the entry's absolute path under root. A content-source walk
// has no on-disk path of its own and synthesises one by joining root with
// the source-relative path: the language registry and the ignore matchers
// are lexical, and the one filesystem check left — symlink confinement —
// is inert for a path that is not on disk.
//
// size is the entry's byte size, or negative when the caller does not
// know it and does not want the cap applied (the scoped walk stats
// nothing at walk time). isDir short-circuits everything but pruning;
// content sources enumerate files and symlinks only, so only the
// filesystem walks ever pass true.
func (idx *Indexer) admitWalkEntry(root, absPath string, size int64, isDir bool) walkAdmission {
	if isDir {
		return walkAdmission{pruneDir: idx.shouldPruneDir(absPath, root)}
	}
	// Exclude rules run before language detection on purpose, and the order is
	// load-bearing rather than tidy: effectiveLanguage falls through to
	// readSniffPrefix, which os.Opens the file. Language-first opened files
	// inside vendored trees, and opened them before shouldExclude's
	// SymlinkEscapes guard had refused links pointing out of the repo.
	if idx.shouldExclude(absPath, root, false) {
		return walkAdmission{excluded: true}
	}
	return idx.admitUnexcludedWalkFile(absPath, size)
}

// admitWalkFileKnownType reuses metadata the caller already obtained without
// following the entry's final symlink (Lstat, DirEntry.Info, or DirEntry.Type).
// Regular files need no second Lstat for confinement. Actual symlinks still
// take the full guard, ahead of every exclusion override and content read.
func (idx *Indexer) admitWalkFileKnownType(root, absPath string, size int64, mode os.FileMode) walkAdmission {
	return idx.admitFileWithRules(root, absPath, size, mode, pathExclusionRules{flat: idx.excludeMatcher(), perFolder: idx.perFolderIgnores(root)})
}

// admitFileInOpenDir is only for files yielded by walkDisk, which has
// already checked every parent, including ancestors of a scoped root.
func (idx *Indexer) admitFileInOpenDir(root, absPath string, size int64, mode os.FileMode) walkAdmission {
	return idx.admitFileWithRules(root, absPath, size, mode, pathExclusionRules{flat: idx.excludeMatcher(), perFolder: idx.perFolderIgnores(root), parentsOpen: true})
}

func (idx *Indexer) admitFileWithRules(root, absPath string, size int64, mode os.FileMode, rules pathExclusionRules) walkAdmission {
	if !mode.IsRegular() {
		return idx.admitWalkEntry(root, absPath, size, false)
	}
	if rules.match(absPath, root, false) {
		return walkAdmission{excluded: true}
	}
	return idx.admitUnexcludedWalkFile(absPath, size)
}

// admitUnexcludedWalkFile may read a prefix for shebang detection, so callers
// must finish confinement and exclusion checks before reaching this stage.
func (idx *Indexer) admitUnexcludedWalkFile(absPath string, size int64) walkAdmission {
	lang, ok := idx.effectiveLanguage(absPath, nil)
	if !ok {
		return walkAdmission{manifest: idx.isIncrementalContractManifest(absPath)}
	}
	if maxSize := idx.config.MaxFileSize; maxSize > 0 && size > maxSize {
		return walkAdmission{lang: lang, oversize: true}
	}
	return walkAdmission{lang: lang, admit: true}
}

// admitScopedWalkFile is the scoped reindex's file gate: the shared
// admission body plus the escape hatch that keeps a root go.mod / go.work
// in the disk set even though no extractor claims it. The manifest still
// has to survive the ignore list, exactly as it did when the two checks
// were written out at each call site.
func (idx *Indexer) admitScopedWalkFile(root, absPath string) bool {
	adm := idx.admitWalkEntry(root, absPath, -1, false)
	return adm.admit || adm.manifest
}

// walkDisk walks scope, prunes the directories shouldPruneDir rejects
// (every .git folder among them) and leaves file admission to fn.
func (idx *Indexer) walkDisk(root, scope string, fn fs.WalkDirFunc) error {
	return filepath.WalkDir(scope, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			// The scope may start below an excluded ancestor. Check it in
			// full; descendants have open parents proved by this walk.
			rules := pathExclusionRules{flat: idx.excludeMatcher(), perFolder: idx.perFolderIgnores(root), parentsOpen: path != scope}
			if rules.match(path, root, true) {
				return filepath.SkipDir
			}
		}
		return fn(path, d, err)
	})
}

// walkSource enumerates src through the same admission gate the
// filesystem walks use and hands fn every entry the gate did not reject
// outright, together with its verdict.
//
// An entry the gate admitted and one it rejected for size both reach fn.
// The size cap is the one rejection a walk accounts for rather than
// swallows — the file still earns a skip node, so the path is visible and
// claimed — and the verdict is what lets the caller tell the two apart.
// Everything else the gate drops (no language, excluded) is dropped here.
//
// Two properties of a snapshot walk differ from a filesystem walk and are
// the caller's to account for. Nothing prunes: a source enumerates files
// and symlinks only, so the ignore rules apply per file instead of
// skipping a subtree, and per-directory ignore files are not consulted at
// all (see shouldExclude) — an index built from this walk should declare
// that omission in its producer state. And mtimeNano is zero on every
// entry, because a snapshot has no modification time; a caller that
// stamps the mtime ledger from a walk must not stamp it from this one.
//
// The path handed to fn is absolute, joined onto the repository root, so
// the rest of the parse pipeline keeps its usual currency; reads on it
// route back through the source in readFileWithVersion.
func (idx *Indexer) walkSource(
	ctx context.Context,
	src source.ContentSource,
	fn func(walkedFile, walkAdmission) error,
) error {
	if src == nil {
		return errors.New("walk source: no content source")
	}
	if fn == nil {
		return errors.New("walk source: nil walk function")
	}
	root := idx.rootPath
	return src.Walk(ctx, func(meta source.FileMeta) error {
		absPath := filepath.Join(root, filepath.FromSlash(meta.Path))
		adm := idx.admitWalkEntry(root, absPath, meta.Size, false)
		if !adm.admit && !adm.oversize {
			return nil
		}
		return fn(walkedFile{path: absPath, lang: adm.lang, size: meta.Size}, adm)
	})
}
