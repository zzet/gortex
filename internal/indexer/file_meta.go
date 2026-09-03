package indexer

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/zeebo/blake3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

var errFileVersionChanged = errors.New("file changed while index mutation was in progress")

type fileReadVersion struct {
	info  os.FileInfo
	mtime int64
	size  int64
	valid bool
	// snapshot marks a version read from an immutable content source.
	// There is no on-disk file to restat and no mtime to stamp: the bytes
	// cannot change under the reader, so the version is valid by
	// construction and every restat check accepts it without a syscall.
	snapshot bool
}

// readOSFileWithVersion returns the bytes together with the stable stat version
// they came from. A concurrent replace/write does not fail the read; it only
// makes the receipt invalid so the caller cannot stamp newer disk state onto
// older parsed bytes. Callers go through (*Indexer).readFileWithVersion, which
// picks this or the content source.
func readOSFileWithVersion(path string) ([]byte, fileReadVersion, error) {
	before, beforeErr := os.Stat(path)
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fileReadVersion{}, err
	}
	after, afterErr := os.Stat(path)
	if beforeErr != nil || afterErr != nil || !sameFileVersion(before, after) {
		return src, fileReadVersion{}, nil
	}
	return src, fileReadVersion{
		info: before, mtime: before.ModTime().UnixNano(), size: before.Size(), valid: true,
	}, nil
}

func sameFileVersion(a, b os.FileInfo) bool {
	return a != nil && b != nil &&
		!a.IsDir() && !b.IsDir() &&
		os.SameFile(a, b) &&
		a.Size() == b.Size() &&
		a.ModTime().UnixNano() == b.ModTime().UnixNano()
}

func contentHashForSource(src []byte) string {
	sum := blake3.Sum256(src)
	return hex.EncodeToString(sum[:])
}

// indexedFileReceiptMatches confirms that an equal-mtime modify is truly a
// replay. The mtime is only the fast reject; equal timestamps are ambiguous on
// coarse filesystems and can be restored explicitly, so the slow path performs
// one primary-key metadata lookup and one source read/hash. Missing capability,
// metadata, or a concurrent file change falls through to normal reindexing.
func (idx *Indexer) indexedFileReceiptMatches(filePath string) bool {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return false
	}
	before, err := os.Stat(absPath)
	if err != nil || before.IsDir() {
		return false
	}
	mtimeKey := idx.relKey(absPath)
	idx.mtimeMu.RLock()
	recordedMtime, tracked := idx.fileMtimes[mtimeKey]
	idx.mtimeMu.RUnlock()
	if !tracked || recordedMtime != before.ModTime().UnixNano() {
		return false
	}

	reader, ok := idx.graph.(graph.FileMetaPathReader)
	if !ok {
		return false
	}
	relPath := idx.graphRelKey(absPath)
	graphPath := idx.prefixPath(relPath)
	rows, err := reader.FileMetasByPaths(idx.repoPrefix, []string{graphPath})
	if err != nil {
		return false
	}
	row, ok := rows[graphPath]
	if !ok || row.ContentHash == "" {
		return false
	}

	src, err := os.ReadFile(absPath)
	if err != nil {
		return false
	}
	after, err := os.Stat(absPath)
	if err != nil || !sameFileVersion(before, after) {
		return false
	}
	src = idx.transforms.run(relPath, src)
	return len(src) == row.Size && contentHashForSource(src) == row.ContentHash
}

// fileMtimeMatches keeps the watcher call site compact; despite the historical
// name, equal mtime alone never returns true — indexedFileReceiptMatches also
// confirms the persisted content identity.
func (idx *Indexer) fileMtimeMatches(filePath string) bool {
	return idx.indexedFileReceiptMatches(filePath)
}

// persistFileMeta records one file's per-file metadata row — the BLAKE3
// content hash (the same algorithm as the Merkle leaf), byte size, extracted
// node count, and a JSON array of parse-error locations — in the backend's
// files sidecar (when it implements graph.FileMetaWriter; the on-disk and
// in-memory backends both do). index_health reads these rows to report
// per-file parse errors + node counts; the Merkle tree stays the authoritative
// change detector.
//
// relPath / src are the pre-repo-prefix path and the (transformed) content;
// the persisted file_path is repo-prefixed so it matches the graph node ids.
// The file's prior row is deleted first so a reindex replaces it cleanly.
func (idx *Indexer) persistFileMeta(relPath string, src []byte, result *parser.ExtractionResult) {
	row, ok := idx.prepareFileMeta(relPath, src, result)
	if !ok {
		return
	}
	persistFileMetaRows(idx.graph, idx.repoPrefix, []graph.FileMetaRow{row})
}

func (idx *Indexer) prepareFileMeta(relPath string, src []byte, result *parser.ExtractionResult) (graph.FileMetaRow, bool) {
	if result == nil || relPath == "" {
		return graph.FileMetaRow{}, false
	}
	filePath := relPath
	if idx.repoPrefix != "" {
		filePath = idx.repoPrefix + "/" + relPath
	}

	errs := ""
	if result.Tree != nil {
		if locs := result.Tree.ParseErrorLocations(); len(locs) > 0 {
			if b, err := json.Marshal(locs); err == nil {
				errs = string(b)
			}
		}
	}

	return graph.FileMetaRow{
		FilePath:    filePath,
		ContentHash: contentHashForSource(src),
		Size:        len(src),
		NodeCount:   len(result.Nodes),
		Errors:      errs,
	}, true
}

func persistFileMetaRows(target graph.Store, repoPrefix string, rows []graph.FileMetaRow) {
	fw, ok := target.(graph.FileMetaWriter)
	if !ok || len(rows) == 0 {
		return
	}
	files := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.FilePath != "" {
			files = append(files, row.FilePath)
		}
	}
	_ = fw.DeleteFileMetasByFiles(repoPrefix, files)
	_ = fw.SetFileMetas(repoPrefix, rows)
}

// setReparsePendingEnrichment sets or clears
// graph.MetaReparsePendingEnrichment on a file's KindFile node, marking
// whether the most recent live re-parse resolved the file without re-running
// semantic enrichment. It round-trips the node through AddNode so a disk
// backend persists the meta change (an in-place map write is lost on sqlite),
// and skips the write entirely when the marker is already in the desired state
// so a save storm never re-persists unchanged nodes.
const reparsePendingEnrichmentBatchSize = 256

type reparsePendingEnrichmentBatch struct {
	byFile                map[string]bool
	deferResolverCatchup  bool
	deferredAffectedFiles map[string]struct{}
}

func (b *reparsePendingEnrichmentBatch) add(graphPath string, pending bool) bool {
	if b == nil || graphPath == "" {
		return false
	}
	if b.byFile == nil {
		b.byFile = make(map[string]bool)
	}
	b.byFile[graphPath] = pending
	return len(b.byFile) >= reparsePendingEnrichmentBatchSize
}

func (idx *Indexer) flushReparsePendingEnrichment(b *reparsePendingEnrichmentBatch) {
	if b == nil || len(b.byFile) == 0 {
		return
	}
	byFile := b.byFile
	b.byFile = nil
	idx.setReparsePendingEnrichments(byFile)
}

func (idx *Indexer) setReparsePendingEnrichment(graphPath string, pending bool) {
	idx.setReparsePendingEnrichments(map[string]bool{graphPath: pending})
}

func (idx *Indexer) setReparsePendingEnrichments(pendingByFile map[string]bool) {
	if len(pendingByFile) == 0 {
		return
	}
	paths := make([]string, 0, len(pendingByFile))
	for graphPath := range pendingByFile {
		if graphPath != "" {
			paths = append(paths, graphPath)
		}
	}
	sort.Strings(paths)
	nodesByFile := idx.graph.GetFileNodesByPaths(paths)
	updates := make([]*graph.Node, 0, len(paths))
	for _, graphPath := range paths {
		pending := pendingByFile[graphPath]
		for _, n := range nodesByFile[graphPath] {
			if n == nil || n.Kind != graph.KindFile {
				continue
			}
			_, had := n.Meta[graph.MetaReparsePendingEnrichment]
			switch {
			case pending && !had:
				if n.Meta == nil {
					n.Meta = map[string]any{}
				}
				n.Meta[graph.MetaReparsePendingEnrichment] = true
			case !pending && had:
				delete(n.Meta, graph.MetaReparsePendingEnrichment)
			default:
				continue // already in the desired state — no write
			}
			updates = append(updates, n)
			break
		}
	}
	if len(updates) > 0 {
		idx.graph.AddBatch(updates, nil)
	}
}
