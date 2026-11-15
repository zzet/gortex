package indexer

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/zeebo/blake3"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/resolver"
)

var errFileVersionChanged = errors.New("file changed while index mutation was in progress")

type fileReadVersion struct {
	relationsRead       *relationsReadFrontier
	relationsGeneration uint64
	info                os.FileInfo
	mtime               int64
	size                int64
	valid               bool
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
	if beforeErr != nil {
		return nil, fileReadVersion{}, beforeErr
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fileReadVersion{}, err
	}
	after, afterErr := os.Stat(path)
	if afterErr != nil {
		return src, fileReadVersion{}, afterErr
	}
	if !sameFileVersion(before, after) {
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
	relPath := idx.relKey(absPath)
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
// and skips the write when clearing a marker that is not set while no type
// tier is configured.
const reparsePendingEnrichmentBatchSize = 256

type reparsePendingEnrichmentBatch struct {
	pointPreparation *watcherPointPreparation
	publication      *PublicationStamps // optional local canonical attempt; never a live record
	readRepair       *ReadRepairObservation
	relationsRead    *relationsReadFrontier
	// npmConsumerFiles are unchanged importers parked by this mutation.
	npmConsumerFiles      []string
	byFile                map[string]bool
	deferResolverCatchup  bool
	deferredAffectedFiles map[string]struct{}
	// deferredPriorDeclarations / deferredPriorPending carry the evicted
	// files' pre-mutation declaration surfaces and prior-unresolved out-edges
	// to the deferred resolver catch-up (resolver_scope.go).
	deferredPriorDeclarations map[string]resolver.DeclarationSurface
	deferredPriorPending      []*graph.Edge
	// First structural observation is independent of declaration eligibility:
	// a C# visibility change or an empty pending set must still mark the file.
	deferredResolverFiles map[string]struct{}
	// deferredPriorBindings are the forward leg's carried bindings
	// (edit_delta_prior_bindings.go); dropped when the batch deletes files.
	deferredPriorBindings map[string]resolver.PriorBinding
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

// setReparsePendingEnrichments applies setReparsePendingEnrichment to many
// files in one batch.
//
// Writing the marker false for any file also raises resumeUntrusted, whether
// or not the marker was set. The write that matters follows enrichment that
// ran for the file in place (a watch save, a batched or point semantic
// catch-up, a forced re-resolve, a file-scoped deferred pass). That
// enrichment re-applies only the file's own facts, but the re-parse also
// evicted the edges other files' type facts point into the file, and only a
// pass that re-applies those files restores them. The ledger no longer
// names the file, so a resumed pass would seal the gap with a completion
// marker; the next whole-repo pass must start from the first page. The other
// false writes are harmless to distrust: a whole-repo discharge follows a
// non-partial pass that already cleared its points, and a file with no type
// tier to wait for (no providers, or an omitted file) costs at most one pass
// that does not resume.
//
// The latch covers only this process, and a restart at the same clean
// revision would honour the persisted points, so the same write first
// deletes the repository's points for every provider. When that fails the
// markers stay on. Every re-parse that can be enriched in place puts its
// file on the ledger under a new graph.MetaReparseEpoch in its own fact
// write (stampReparseLedger), and a type pass never resumes a point recorded
// over another ledger. So a crash anywhere before the marker comes off, or
// a failed delete, leaves a ledger no surviving point covers.
//
// Every write stamps a new epoch, even when the marker is already set: the
// re-parse that preceded it dropped facts a stored point says were applied,
// whether or not the file was written. Clearing the marker replaces the
// epoch rather than removing it, so the ledger digest never returns to one
// a point recorded before the clear carries: a type pass staging meanwhile
// may record such a point after the discard above. A file with no
// marker gets one too while a type tier is configured, since its re-parse
// may have dropped the epoch with the node. The epoch on a clear overlaps
// the one stamped by the write that preceded it (the fact write in
// stampReparseLedger, or a pending write here): on every path that clears
// today either alone moves the digest. The clear keeps its own so the
// guarantee does not rest on that earlier write.
func (idx *Indexer) setReparsePendingEnrichments(pendingByFile map[string]bool) {
	if len(pendingByFile) == 0 {
		return
	}
	paths := make([]string, 0, len(pendingByFile))
	distrust := false
	for graphPath, pending := range pendingByFile {
		if graphPath == "" {
			continue
		}
		paths = append(paths, graphPath)
		if !pending {
			distrust = true
		}
	}
	keepMarkers := false
	if distrust {
		idx.resumeUntrusted.Store(true)
		if rs, ok := idx.graph.(graph.EnrichmentResumeStore); ok {
			if err := rs.ClearRepoEnrichmentResume(idx.repoPrefix); err != nil {
				idx.logger.Warn("indexer: discard enrichment resume points failed; leaving the files on the re-parse ledger",
					zap.String("repo", idx.repoPrefix), zap.Error(err))
				keepMarkers = true
			}
		}
	}
	sort.Strings(paths)
	nodesByFile := idx.graph.GetFileNodesByPaths(paths)
	epoch := newReparseEpoch()
	typeTier := idx.semanticProvidersPresent()
	updates := make([]*graph.Node, 0, len(paths))
	for _, graphPath := range paths {
		pending := pendingByFile[graphPath]
		for _, n := range nodesByFile[graphPath] {
			if n == nil || n.Kind != graph.KindFile {
				continue
			}
			_, had := n.Meta[graph.MetaReparsePendingEnrichment]
			if !pending && (keepMarkers || (!had && !typeTier)) {
				continue // kept on, or nothing a type pass reads — no write
			}
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			if pending {
				n.Meta[graph.MetaReparsePendingEnrichment] = true
			} else {
				delete(n.Meta, graph.MetaReparsePendingEnrichment)
			}
			n.Meta[graph.MetaReparseEpoch] = epoch
			updates = append(updates, n)
			break
		}
	}
	if len(updates) > 0 {
		idx.graph.AddBatch(updates, nil)
	}
}

// retractUnchangedMtimeReceipts deletes the persisted mtime receipts of
// files about to be evicted and re-derived when the receipt already names
// the version being read: a file re-derived with no write to it (the
// importer of a deleted file). A warm-restart census would find such a file
// unchanged, so a crash between its eviction and its fact write would leave
// it with no nodes, off the ledger, while an old resume point survives.
// Without the receipt the restart re-derives it. The fact write's version
// record puts the receipt back. Files whose receipt already differs need
// nothing: the census sees them changed.
func (idx *Indexer) retractUnchangedMtimeReceipts(versions map[string]fileReadVersion) {
	deleter, ok := idx.graph.(graph.FileMtimeDeleter)
	if !ok || len(versions) == 0 {
		return
	}
	var keys []string
	idx.mtimeMu.RLock()
	for key, version := range versions {
		if !version.valid || version.snapshot {
			continue
		}
		if recorded, tracked := idx.fileMtimes[key]; tracked && recorded == version.mtime {
			keys = append(keys, key)
		}
	}
	idx.mtimeMu.RUnlock()
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	if err := deleter.DeleteFileMtimes(idx.repoPrefix, keys); err != nil {
		idx.logger.Warn("indexer: retract mtime receipts before a re-parse failed",
			zap.String("repo", idx.repoPrefix), zap.Int("files", len(keys)), zap.Error(err))
	}
}

// discardResumeBeforeSkipEviction deletes the repository's resume points
// before a skip (size, content, binary, minified or timeout) replaces a
// file's facts with a synthetic node. Other files' type facts may have bound
// into the evicted ones, and no re-parse puts the file on the ledger. A
// failed delete fails the re-index before anything is written, so the file
// keeps its prior state and is retried. The synthetic node also carries a
// new epoch (stampSkipEpoch) for a point recorded after the delete.
func (idx *Indexer) discardResumeBeforeSkipEviction(prior []*graph.Node) error {
	hasFacts := false
	for _, n := range prior {
		if n != nil && n.Kind != graph.KindFile {
			hasFacts = true
			break
		}
	}
	rs, ok := idx.graph.(graph.EnrichmentResumeStore)
	if !hasFacts || !ok {
		return nil
	}
	idx.resumeUntrusted.Store(true)
	if err := rs.ClearRepoEnrichmentResume(idx.repoPrefix); err != nil {
		return fmt.Errorf("discard enrichment resume points before a skip eviction: %w", err)
	}
	return nil
}

// stampSkipEpoch stamps a new graph.MetaReparseEpoch on the synthetic file
// node a skip installs. A type pass that opened before the skip can record
// a point over the old ledger after discardResumeBeforeSkipEviction ran,
// and a restart drops the in-process latch; the epoch moves the ledger
// digest so that point is not resumed.
func stampSkipEpoch(nodes []*graph.Node) {
	for _, n := range nodes {
		if n == nil || n.Kind != graph.KindFile {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta[graph.MetaReparseEpoch] = newReparseEpoch()
		return
	}
}

// newReparseEpoch names one re-parse for graph.MetaReparseEpoch.
func newReparseEpoch() string { return strconv.FormatUint(rand.Uint64(), 36) }

// reparseLedgerApplies reports whether a re-parse of the file result came
// from goes on the per-file ledger: there is a type tier for it to fall
// short of.
func (idx *Indexer) reparseLedgerApplies(result *parser.ExtractionResult) bool {
	return idx.semanticProvidersPresent() && !extractionDispositionFor(result).omitSecondarySourceScans()
}

func (idx *Indexer) semanticProvidersPresent() bool {
	return idx.semanticMgr != nil && idx.semanticMgr.Enabled() && idx.semanticMgr.HasProviders()
}

// stampReparseLedger marks a re-parsed file on the per-file ledger, under a
// new epoch, in the node write that replaces its facts, so no crash can
// leave the new facts stored with the file off the ledger while an old
// resume point survives. Enrichment that runs in place afterwards clears
// the marker through setReparsePendingEnrichments, which discards the
// resume points first.
func stampReparseLedger(nodes []*graph.Node) {
	for _, n := range nodes {
		if n == nil || n.Kind != graph.KindFile {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta[graph.MetaReparsePendingEnrichment] = true
		n.Meta[graph.MetaReparseEpoch] = newReparseEpoch()
		return
	}
}

func (b *reparsePendingEnrichmentBatch) publicationStamps() *PublicationStamps {
	if b == nil {
		return nil
	}
	return b.publication
}
