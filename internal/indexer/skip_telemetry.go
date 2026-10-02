package indexer

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime/debug"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// errExtractTimeout is the sentinel returned when a file's extraction
// exceeds IndexConfig.MaxExtractMillis.
var errExtractTimeout = errors.New("extraction exceeded time budget")

// extractorPanicError wraps a recovered extractor panic so callers can
// tell a parser crash (quarantine the file, keep indexing) apart from
// an ordinary extraction error. The original panic value and a stack
// trace ride along for diagnostics.
type extractorPanicError struct {
	file  string
	value any
	stack []byte
}

func (e *extractorPanicError) Error() string {
	return fmt.Sprintf("extractor panic on %s: %v", e.file, e.value)
}

// safeExtractWithOptions runs the central extraction dispatcher guarded by a
// recover so a panic on one malformed file becomes an error instead of
// crashing the whole indexing run. The request options are immutable and
// repository-scoped.
func safeExtractWithOptions(
	ext parser.Extractor,
	relPath string,
	src []byte,
	opts parser.ExtractionOptions,
) (result *parser.ExtractionResult, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result = nil
			err = &extractorPanicError{file: relPath, value: rec, stack: debug.Stack()}
		}
	}()
	return parser.Extract(ext, relPath, src, opts)
}

// skippedFile records a file dropped by the size cap or a full-index
// parse failure, kept so a synthetic telemetry node can be emitted after
// the parse pass. cause carries the extraction error for parse failures
// (empty for size skips).
type skippedFile struct {
	relPath string
	lang    string
	size    int64
	cause   string
	// reason is the content-admission skip reason (large_document /
	// vector_data / large_data_asset) for files the asset gate dropped at
	// the walk; empty for size / parse-failure skips.
	reason string
}

// walkedFile records a file that survived the walk-time filters,
// together with its walk-time ModTime and detected language so the
// worker and the post-parse fileMtimes loop don't need to re-stat /
// re-detect. The walk does exactly one os.Stat (via d.Info()) and one
// language detection per surviving file; everything downstream reads
// from this struct.
type walkedFile struct {
	path      string
	lang      string
	size      int64
	mtimeNano int64
}

// sortBySizeDesc orders walked files largest-first, in place, so the
// parse worker pool starts the biggest files before the long tail of
// small ones. Dispatching the largest file last would make the whole
// index block on it after every other worker has drained; starting it
// first overlaps its parse with the rest and cuts tail-latency variance.
// The sort is stable, so equal-size files keep their walk order and the
// dispatch order stays deterministic. It is a pure permutation of the
// slice — the set of files (and therefore the resulting graph) is
// unchanged; only the order in which they reach the workers differs.
func sortBySizeDesc(files []walkedFile) {
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].size > files[j].size
	})
}

// Files above this threshold are large enough that reading several of
// them concurrently can dominate RSS before extraction even starts. Keep
// normal source-file throughput high while bounding the “few huge PDFs /
// spreadsheets / vector artifacts” class reported in #120.
const largeFileReadThresholdBytes int64 = 16 << 20 // 16 MiB

func largeFileReadParallelism(workers int) int {
	if workers <= 1 {
		return 1
	}
	return min(2, workers)
}

// extractWithTimeoutDone runs ext.Extract under the per-file extraction
// budget and invokes done when extraction actually finishes. With no budget
// configured it calls Extract directly. On timeout it returns
// errExtractTimeout; the slow extraction runs on to completion in its goroutine
// (tree-sitter's own 5s parse cap bounds the worst case) and its result is
// discarded.
func (idx *Indexer) extractWithTimeoutDone(
	ext parser.Extractor,
	relPath string,
	src []byte,
	done func(),
) (*parser.ExtractionResult, error) {
	if done == nil {
		done = func() {}
	}
	releaseLifecycle, err := idx.extractionLifecycle.admit()
	if err != nil {
		done()
		return nil, err
	}
	finish := func() {
		defer releaseLifecycle()
		done()
	}
	opts := idx.extractionOptionsValue()
	budget := effectiveExtractBudget(idx.config.MaxExtractMillis, len(src))
	if budget <= 0 {
		defer finish()
		return safeExtractWithOptions(ext, relPath, src, opts)
	}
	type outcome struct {
		result *parser.ExtractionResult
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, err := safeExtractWithOptions(ext, relPath, src, opts)
		finish()
		ch <- outcome{result: r, err: err}
	}()
	timer := time.NewTimer(time.Duration(budget) * time.Millisecond)
	defer timer.Stop()
	select {
	case o := <-ch:
		return o.result, o.err
	case <-timer.C:
		return nil, errExtractTimeout
	}
}

// timeoutSkipResult builds a synthetic single-node result for a file
// whose extraction blew the time budget, so it stays visible in the
// graph with skip telemetry attached.
func timeoutSkipResult(relPath, lang string, budgetMS int) *parser.ExtractionResult {
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{
			ID:       relPath,
			Kind:     graph.KindFile,
			Name:     filepath.Base(relPath),
			FilePath: relPath,
			Language: lang,
			Meta: map[string]any{
				"skip_reason":            "timeout",
				"skipped_due_to_timeout": true,
				"extract_budget_ms":      budgetMS,
			},
		}},
	}
}

// binarySkipResult builds a synthetic single-node result for a binary
// payload a language extension nonetheless claimed (a tool-cache .pkl, an
// object file), detected by content and skipped before any parse. It
// stays visible in the graph with the skip reason attached, and — like
// the minified class — counts as a successful read, so its receipt is
// recorded and the file goes inert until its content changes instead of
// re-arming the parse-retry path on every reconcile.
func binarySkipResult(relPath, lang, reason string) *parser.ExtractionResult {
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{
			ID:       relPath,
			Kind:     graph.KindFile,
			Name:     filepath.Base(relPath),
			FilePath: relPath,
			Language: lang,
			Meta: map[string]any{
				"skip_reason":           "binary",
				"skipped_due_to_binary": true,
				"binary_reason":         reason,
			},
		}},
	}
}

// minifiedSkipResult builds a synthetic single-node result for a build
// artifact (a minified bundle or a sourcemap) detected by content and
// skipped, so it stays visible in the graph with the skip reason
// attached instead of polluting it with mangled symbols.
func minifiedSkipResult(relPath, lang, reason string) *parser.ExtractionResult {
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{
			ID:       relPath,
			Kind:     graph.KindFile,
			Name:     filepath.Base(relPath),
			FilePath: relPath,
			Language: lang,
			Meta: map[string]any{
				"skip_reason":             "minified",
				"skipped_due_to_minified": true,
				"minified_reason":         reason,
			},
		}},
	}
}

// sizeSkipNode builds a synthetic file node for a file dropped by the
// size cap.
func sizeSkipNode(sf skippedFile, maxSize int64) *graph.Node {
	return &graph.Node{
		ID:       sf.relPath,
		Kind:     graph.KindFile,
		Name:     filepath.Base(sf.relPath),
		FilePath: sf.relPath,
		Language: sf.lang,
		Meta: map[string]any{
			"skip_reason":         "size",
			"skipped_due_to_size": true,
			"file_size_bytes":     sf.size,
			"max_file_size_bytes": maxSize,
		},
	}
}

// parseFailedSkipResult builds a synthetic single-node result for a file
// whose extractor returned an error that did not panic or time out — an
// ordinary parse failure that would otherwise be dropped silently.
// Keeping the file in the graph as a skip node makes "why is this symbol
// missing" answerable (index_health rolls the skip reasons up), and the
// Merkle reconcile retries it automatically once its content changes or
// its language's extractor version is bumped — so no separate retry
// ledger is needed.
func parseFailedSkipResult(relPath, lang string, cause error) *parser.ExtractionResult {
	reason := "parse failed"
	if cause != nil {
		reason = cause.Error()
	}
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{
			ID:       relPath,
			Kind:     graph.KindFile,
			Name:     filepath.Base(relPath),
			FilePath: relPath,
			Language: lang,
			Meta: map[string]any{
				"skip_reason": "parse_failed",
				"parse_error": reason,
			},
		}},
	}
}

// emitSizeSkipNodes adds synthetic file nodes for every size-skipped
// file so the file stays visible in the graph (queryable,
// get_file_summary works) with skip telemetry instead of vanishing.
func (idx *Indexer) emitSizeSkipNodes(skipped []skippedFile) {
	if len(skipped) == 0 {
		return
	}
	maxSize := idx.config.MaxFileSize
	nodes := make([]*graph.Node, 0, len(skipped))
	for _, sf := range skipped {
		nodes = append(nodes, sizeSkipNode(sf, maxSize))
	}
	idx.applyRepoPrefix(nodes, nil)
	idx.graph.AddBatch(nodes, nil)
}

// emitParseFailedSkipNodes adds a synthetic file node for every file that
// failed extraction during a FULL index and produced no nodes, so the
// file stays visible (index_health rolls it up under skip_reason
// "parse_failed") instead of vanishing silently. Only the full-index path
// uses this: the live-modify path deliberately keeps a file's prior nodes
// through a transient mid-edit parse failure, so it must never be fed
// here. The Merkle reconcile retries a failed file when its content or
// extractor version changes.
func (idx *Indexer) emitParseFailedSkipNodes(failed []skippedFile) {
	if len(failed) == 0 {
		return
	}
	nodes := make([]*graph.Node, 0, len(failed))
	for _, sf := range failed {
		var cause error
		if sf.cause != "" {
			cause = errors.New(sf.cause)
		}
		nodes = append(nodes, parseFailedSkipResult(sf.relPath, sf.lang, cause).Nodes...)
	}
	idx.applyRepoPrefix(nodes, nil)
	idx.graph.AddBatch(nodes, nil)
}
