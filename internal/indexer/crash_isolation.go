package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/crashpool"
)

// crashIsolationEnabled reports whether tree-sitter extraction should
// run in isolated worker subprocesses. GORTEX_PARSER_ISOLATION
// overrides the index.crash_isolation config key.
func (idx *Indexer) crashIsolationEnabled() bool {
	if v := os.Getenv("GORTEX_PARSER_ISOLATION"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return idx.config.CrashIsolation
}

// newParsePool spawns a crash-isolated parser pool — `workers` worker
// subprocesses, each a `gortex __parse-worker` instance. When a
// per-file extraction budget is configured it also bounds the worker
// round-trip so a hung file is killed on the same deadline.
func (idx *Indexer) newParsePool(workers int) (*crashpool.Pool, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cfg := crashpool.Config{
		Argv:    []string{exe, "__parse-worker"},
		Workers: workers,
		Logger:  idx.logger,
	}
	if ms := idx.config.MaxExtractMillis; ms > 0 {
		cfg.RequestTimeout = time.Duration(ms) * time.Millisecond
	}
	if env := customGrammarEnv(idx.config.Grammars); env != "" {
		cfg.Env = append(cfg.Env, env)
	}
	if env := extractorPluginEnv(idx.config.ExtractorPlugins); env != "" {
		cfg.Env = append(cfg.Env, env)
	}
	if env := fallbackChunkerEnv(idx.config.FallbackChunkers); env != "" {
		cfg.Env = append(cfg.Env, env)
	}
	return crashpool.NewPool(cfg)
}

// customGrammarEnv encodes the configured custom grammars into the
// GORTEX_CUSTOM_GRAMMARS environment entry a crash-isolation worker
// reads at startup. Library paths are resolved to absolute so the
// worker — which may run with a different working directory — can
// still load them. Returns "" when no custom grammars are configured.
func customGrammarEnv(specs []config.GrammarSpec) string {
	if len(specs) == 0 {
		return ""
	}
	resolved := make([]config.GrammarSpec, len(specs))
	for i, s := range specs {
		resolved[i] = s
		if s.Library != "" && !filepath.IsAbs(s.Library) {
			if abs, err := filepath.Abs(s.Library); err == nil {
				resolved[i].Library = abs
			}
		}
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		return ""
	}
	return "GORTEX_CUSTOM_GRAMMARS=" + string(data)
}

// extractorPluginEnv encodes the configured subprocess extractor
// plugins into the GORTEX_EXTRACTOR_PLUGINS environment entry a
// crash-isolation worker reads at startup, so the worker resolves the
// same plugin languages as the parent. Returns "" when none are
// configured.
func extractorPluginEnv(specs []config.ExtractorPluginSpec) string {
	if len(specs) == 0 {
		return ""
	}
	data, err := json.Marshal(specs)
	if err != nil {
		return ""
	}
	return "GORTEX_EXTRACTOR_PLUGINS=" + string(data)
}

// fallbackChunkerEnv encodes the configured regex fallback chunkers into
// the GORTEX_FALLBACK_CHUNKERS environment entry a crash-isolation
// worker reads at startup, so the worker resolves the same grammar-less
// languages as the parent. Returns "" when none are configured.
func fallbackChunkerEnv(specs []config.FallbackChunkerSpec) string {
	if len(specs) == 0 {
		return ""
	}
	data, err := json.Marshal(specs)
	if err != nil {
		return ""
	}
	return "GORTEX_FALLBACK_CHUNKERS=" + string(data)
}

// sharedParsePool returns the long-lived crash-isolation pool and the
// quarantine, creating them on first use. Reusing one pool across
// single-file re-indexes avoids forking a worker subprocess per file
// on the watcher hot path — the dominant cost when crash isolation is
// on. Returns (nil, nil) if no worker can spawn, so the caller falls
// back to in-process extraction.
func (idx *Indexer) sharedParsePool() (*crashpool.Pool, *crashpool.Quarantine) {
	releaseLifecycle, err := idx.extractionLifecycle.admit()
	if err != nil {
		return nil, nil
	}
	defer releaseLifecycle()

	idx.parsePoolMu.Lock()
	defer idx.parsePoolMu.Unlock()
	if idx.parsePool != nil {
		return idx.parsePool, idx.parseQuar
	}
	// A long-lived pool keeps idle workers resident, so cap the worker
	// count well below the bulk-index width.
	workers := idx.config.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4
	}
	p, err := idx.newParsePool(workers)
	if err != nil {
		idx.logger.Warn("indexer: crash-isolation pool unavailable; using in-process extraction",
			zap.Error(err))
		return nil, nil
	}
	idx.parsePool = p
	idx.parseQuar = crashpool.LoadQuarantine(
		filepath.Join(idx.rootPath, ".gortex", "parser-quarantine.json"))
	return idx.parsePool, idx.parseQuar
}

// closeParsePool tears down the long-lived crash-isolation pool if one
// was created. It is safe to call when none exists, and idempotent.
func (idx *Indexer) closeParsePool() {
	idx.parsePoolMu.Lock()
	defer idx.parsePoolMu.Unlock()
	if idx.parsePool != nil {
		idx.parsePool.Close()
		idx.parsePool = nil
	}
}

// extractFile produces one file's graph contribution. With pool == nil
// it calls the extractor in-process (the default). With a pool the
const (
	// extractBudgetBytesPerMs grants one extra millisecond of extraction
	// budget per this many source bytes, on top of the base budget. A flat
	// budget skips a legitimately large file (which parses proportionally
	// slower) at the same threshold as a pathological small one; scaling by
	// size keeps real files in while still tripping on genuine runaways.
	extractBudgetBytesPerMs = 1024
	// extractBudgetMaxMultiple caps the size-scaled budget at this multiple
	// of the base so even a multi-megabyte file cannot stall the pass
	// indefinitely.
	extractBudgetMaxMultiple = 8
)

// effectiveExtractBudget scales the base per-file extraction budget (ms) by
// file size in bytes. base<=0 keeps the cap disabled (returns base unchanged),
// and an empty file gets exactly the base. Otherwise the budget grows linearly
// with byte count — one millisecond per extractBudgetBytesPerMs bytes — capped
// at extractBudgetMaxMultiple× the base so the worst case stays bounded.
func effectiveExtractBudget(base, fileBytes int) int {
	if base <= 0 || fileBytes <= 0 {
		return base
	}
	scaled := base + fileBytes/extractBudgetBytesPerMs
	if capped := base * extractBudgetMaxMultiple; scaled > capped {
		return capped
	}
	return scaled
}

// submitWithRetry runs submit once and, if the worker crashed, hung, or
// panicked, retries exactly once. A worker fault can be a worker poisoned by a
// prior parse rather than the file itself; the pool replaces the dead worker
// before returning, so the single retry runs on a clean worker. A file that is
// genuinely the fault fails again and is quarantined as before. A plain
// extraction error (Result.Err without a crash) is deterministic and is not
// retried.
func submitWithRetry(submit func() crashpool.Result) crashpool.Result {
	res := submit()
	if res.Crashed || res.Panicked {
		res = submit()
	}
	return res
}

// parse runs in a worker subprocess: a crash, hang, or panic quarantines
// the file and yields a synthetic KindFile node carrying
// Meta["parse_error"] instead of aborting the whole index pass.
//
// The returned bool reports whether the file was skipped — quarantined
// after a crash, or skipped after blowing the extraction time budget.
// Callers MUST skip coverage + contract extraction for skipped files,
// since both re-parse the source and would re-trigger the fault.
func (idx *Indexer) extractFile(
	pool *crashpool.Pool, q *crashpool.Quarantine,
	path, relPath, lang string, ext parser.Extractor, src []byte,
) (result *parser.ExtractionResult, skipped bool, err error) {
	return idx.extractFileWithRawLease(
		nil, pool, q, path, relPath, lang, ext, src,
	)
}

func (idx *Indexer) extractFileWithRawLease(
	rawLease *parseAdmissionLease,
	pool *crashpool.Pool, q *crashpool.Quarantine,
	path, relPath, lang string, ext parser.Extractor, src []byte,
) (result *parser.ExtractionResult, skipped bool, err error) {
	return idx.extractFileCtxWithRawLease(
		context.Background(), nil, rawLease,
		pool, q, path, relPath, lang, ext, src,
	)
}

func (idx *Indexer) extractFileCtxWithRawLease(
	ctx context.Context,
	nativeAdmission *nativeParseExtractionAdmission,
	rawLease *parseAdmissionLease,
	pool *crashpool.Pool, q *crashpool.Quarantine,
	path, relPath, lang string, ext parser.Extractor, src []byte,
) (result *parser.ExtractionResult, skipped bool, err error) {
	// Binary payloads — not text any grammar can consume. A tool cache
	// rewritten under a claimed extension (Serena's .serena/cache/*.pkl
	// against the Pkl grammar) used to reach tree-sitter, burn the full
	// parse budget twice (first pass + fallback, retried), and re-arm on
	// every reconcile. Detect by content and skip with telemetry: the
	// skip node keeps the file visible, the recorded receipt keeps it
	// inert until its content actually changes, and the parse budget is
	// never spent on bytes that could never yield a node.
	//
	// Non-code extractors are exempt: an image / document / data asset
	// extractor (AssetClass set) deliberately reads binary payloads, so
	// the guard applies only where a TEXT grammar would consume the bytes.
	if parser.AssetClassOf(ext) == "" {
		if reason := binaryArtifactReason(src); reason != "" {
			return binarySkipResult(relPath, lang, reason), true, nil
		}
	}
	// #812 defense in depth: an extraction that reports success but
	// minted zero symbols from a partial parse used to vanish from the
	// graph with no diagnostic. The UTF-16 decode transform removes the
	// known cause; this catches every remaining silent-zero shape. The
	// note goes through the pending set, not the ledger directly — the
	// version receipt this file's caller records afterwards would clear
	// a direct entry before any flush persisted it.
	defer func() {
		if err == nil && !skipped && silentZeroExtraction(result) {
			idx.noteSilentZeroExtraction(relPath)
		}
	}()
	// Bundled / minified build artifacts are synthetic source — a
	// minified bundle or a sourcemap has no meaningful symbols and
	// only pollutes the graph. Detect by content and skip with
	// telemetry, unless the user opted into indexing them.
	if !idx.config.IndexMinified {
		if reason := minifiedArtifactReason(lang, src); reason != "" {
			return minifiedSkipResult(relPath, lang, reason), true, nil
		}
	}
	if !idx.config.IndexGeneratedParsers {
		if projected, ok := generatedTreeSitterParserProjection(relPath, lang, src); ok {
			if q != nil {
				q.Forget(relPath)
			}
			idx.logger.Info("indexer: projected generated tree-sitter parser table",
				zap.String("file", relPath),
				zap.Int("source_bytes", len(src)),
				zap.Int("projected_symbols", len(projected.Nodes)-1))
			return projected, false, nil
		}
	}
	if pool == nil {
		nativeLease, admissionErr := nativeAdmission.acquire(ctx, lang, int64(len(src)))
		if admissionErr != nil {
			return nil, false, fmt.Errorf("native parse admission: %w", admissionErr)
		}
		// The caller still owns the raw-source lease for coverage and graph
		// construction after a successful parse. Add one extractor-owned hold
		// so a timeout can return immediately without admitting replacement
		// bytes while the background extractor still retains src.
		releaseRawHold := rawLease.retain()
		r, eerr := idx.extractWithTimeoutDone(ext, relPath, src, func() {
			nativeLease.Release()
			releaseRawHold()
		})
		if errors.Is(eerr, errExtractTimeout) {
			budget := effectiveExtractBudget(idx.config.MaxExtractMillis, len(src))
			idx.logger.Warn("indexer: file extraction exceeded budget; skipped",
				zap.String("file", relPath), zap.Int("budget_ms", budget))
			return timeoutSkipResult(relPath, lang, budget), true,
				fmt.Errorf("extraction exceeded %dms budget: %s", budget, relPath)
		}
		// An extractor panic was recovered in-process: isolate the file
		// like the subprocess pool does — emit a quarantine node so it
		// stays visible, record the error, and let indexing continue
		// rather than crashing the whole run.
		var pe *extractorPanicError
		if errors.As(eerr, &pe) {
			idx.logger.Warn("indexer: recovered extractor panic; file isolated",
				zap.String("file", relPath), zap.String("reason", fmt.Sprint(pe.value)))
			return quarantineResult(relPath, lang, "extractor panic: "+fmt.Sprint(pe.value)),
				true, eerr
		}
		if eerr != nil {
			return nil, false, eerr
		}
		stampParseErrors(r)
		normalizeExtractionMetadata(r, src)
		return r, false, nil
	}

	var mtime int64
	if info, statErr := os.Stat(path); statErr == nil {
		mtime = info.ModTime().UnixNano()
	}
	if q.IsQuarantined(relPath, mtime) {
		return quarantineResult(relPath, lang, "skipped — file previously crashed the parser"),
			true, fmt.Errorf("skipped quarantined file %s", relPath)
	}

	releaseLifecycle, lifecycleErr := idx.extractionLifecycle.admit()
	if lifecycleErr != nil {
		return nil, false, lifecycleErr
	}
	defer releaseLifecycle()
	helperNames := idx.extractionOptionsValue().TemporalEnvHelpers()
	res := submitWithRetry(func() crashpool.Result {
		return pool.SubmitWithOptions(relPath, lang, src, helperNames)
	})
	switch {
	case res.Crashed || res.Panicked:
		q.Add(relPath, res.Err, mtime)
		idx.logger.Warn("indexer: parser isolated a bad file",
			zap.String("file", relPath), zap.String("reason", res.Err))
		return quarantineResult(relPath, lang, res.Err), true,
			fmt.Errorf("parser crash isolated on %s: %s", relPath, res.Err)
	case res.Err != "":
		return nil, false, errors.New(res.Err)
	}

	// Clean parse: if the file was quarantined under an older revision
	// and has since been fixed, drop the stale entry.
	q.Forget(relPath)
	result = &parser.ExtractionResult{Nodes: res.Nodes, Edges: res.Edges}
	if res.HasParseErr {
		stampParseErrorCount(result.Nodes, res.ParseErrors)
	}
	normalizeExtractionMetadata(result, src)
	return result, false, nil
}

// quarantineResult builds a synthetic single-node result for a file
// that could not be parsed, so the file stays visible in the graph
// with the failure reason attached.
func quarantineResult(relPath, lang, reason string) *parser.ExtractionResult {
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{{
			ID:       relPath,
			Kind:     graph.KindFile,
			Name:     filepath.Base(relPath),
			FilePath: relPath,
			Language: lang,
			Meta: map[string]any{
				"skip_reason": "parse_panic",
				"parse_error": reason,
				"quarantined": true,
			},
		}},
	}
}

// stampParseErrorCount stamps a known parse-error count onto a file
// node — the subprocess-path equivalent of stampParseErrors, which
// can't run in the parent because the parse tree never crosses the
// process boundary.
func stampParseErrorCount(nodes []*graph.Node, count int) {
	if count <= 0 {
		return
	}
	for _, n := range nodes {
		if n.Kind != graph.KindFile {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["parse_errors"] = count
		n.Meta["has_parse_errors"] = true
		return
	}
}
