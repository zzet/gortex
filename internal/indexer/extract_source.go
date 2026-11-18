package indexer

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/crashpool"
)

// maxAdHocExtractSourceBytes is a hard ceiling for caller-owned, out-of-index
// validation. Normal indexing may admit larger files under configuration, but
// interactive recovery must not turn one MCP request into an unbounded parse.
const maxAdHocExtractSourceBytes = 1 << 20 // 1 MiB

// ExtractSource parses caller-owned bytes through bounded admission,
// minified-content, timeout, panic-recovery, and optional crash isolation
// without mutating graph state. Preparation must preserve raw byte and line
// coordinates; configured command transforms are refused because they may
// synthesize declarations that cannot safely anchor an edit of the raw file.
// The caller owns the result and must call ReleaseTree when finished.
func (idx *Indexer) ExtractSource(ctx context.Context, filePath string, src []byte) (*parser.ExtractionResult, error) {
	if idx == nil || idx.registry == nil {
		return nil, fmt.Errorf("indexer source extraction is unavailable")
	}
	if ctx == nil {
		return nil, fmt.Errorf("indexer source extraction requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := int64(maxAdHocExtractSourceBytes)
	if idx.config.MaxFileSize > 0 && idx.config.MaxFileSize < limit {
		limit = idx.config.MaxFileSize
	}
	if int64(len(src)) > limit {
		return nil, fmt.Errorf("source exceeds bounded extraction limit (%d bytes)", limit)
	}

	path := filePath
	if !filepath.IsAbs(path) && idx.rootPath != "" {
		path = filepath.Join(idx.rootPath, filepath.FromSlash(filePath))
	}
	prepared, err := idx.transforms.prepareCoordinateStable(path, src)
	if err != nil {
		return nil, fmt.Errorf("coordinate-stable source preparation: %w", err)
	}
	language, ok := idx.effectiveLanguage(path, prepared)
	if !ok {
		return nil, fmt.Errorf("unsupported source language for %s", filePath)
	}
	extractor, ok := idx.registry.GetByLanguage(language)
	if !ok {
		return nil, fmt.Errorf("no extractor registered for %s", language)
	}
	prepared = parser.ApplyPreParse(extractor, prepared)
	if !sameSourceCoordinates(src, prepared) {
		return nil, fmt.Errorf("extractor pre-parse rewrite does not preserve source coordinates")
	}
	if int64(len(prepared)) > limit {
		return nil, fmt.Errorf("prepared source exceeds bounded extraction limit (%d bytes)", limit)
	}

	rawLease, err := acquireParseAdmission(
		ctx, int64(len(prepared)), 0, nil, idx.parseAdmission.Load(),
	)
	if err != nil {
		return nil, fmt.Errorf("source admission: %w", err)
	}
	defer rawLease.Release()
	nativeAdmission := newNativeParseExtractionAdmission(0, nil, idx.nativeParseAdmission.Load())
	return idx.extractSourceWith(
		ctx, rawLease, nativeAdmission,
		path, filePath, language, extractor, prepared,
	)
}

// extractSourceWith parses prepared caller-owned bytes admitted under
// rawLease, with the native-tree admission, crash isolation and index
// safeguards of a normal parse. Interactive callers admit the bytes against
// a reserved budget so a full shared budget cannot hold them behind bulk
// indexing.
func (idx *Indexer) extractSourceWith(
	ctx context.Context,
	rawLease *parseAdmissionLease,
	nativeAdmission *nativeParseExtractionAdmission,
	path, relPath, language string,
	extractor parser.Extractor,
	src []byte,
) (*parser.ExtractionResult, error) {
	var pool *crashpool.Pool
	var quarantine *crashpool.Quarantine
	if idx.crashIsolationEnabled() {
		pool, quarantine = idx.sharedParsePool()
	}
	result, skipped, err := idx.extractFileCtxWithRawLease(
		ctx, nativeAdmission, rawLease, pool, quarantine,
		path, relPath, language, extractor, src,
	)
	if err != nil {
		if result != nil {
			result.ReleaseTree()
		}
		return nil, err
	}
	if skipped {
		if result != nil {
			result.ReleaseTree()
		}
		return nil, fmt.Errorf("source extraction refused by index safeguards")
	}
	if err := ctx.Err(); err != nil {
		if result != nil {
			result.ReleaseTree()
		}
		return nil, err
	}
	return result, nil
}
