package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/search/trigram"
)

// GrepTextBounded searches already-indexed files without triggering the lazy
// full trigram build. A current warm searcher supplies candidate filtering;
// otherwise the same bounded scanner runs over a sorted snapshot of known
// files. The boolean result is true when a cap or cancellation may have hidden
// additional matches.
func (idx *Indexer) GrepTextBounded(
	ctx context.Context,
	query string,
	limit int,
	maxFiles int,
) ([]trigram.Match, bool) {
	if idx == nil || query == "" {
		return nil, false
	}
	gen := idx.indexGen.Load()
	idx.trigramMu.Lock()
	searcher := idx.trigramSearcher
	warm := searcher != nil && idx.trigramGen == gen
	var release func()
	var bytes int64
	if warm {
		idx.applyToolWritesLocked()
		release = idx.trigramReleaseLocked()
		bytes = searcher.ApproxIndexBytes()
	}
	idx.trigramMu.Unlock()
	if warm {
		idx.trigramBudget().touch(idx, release, bytes)
		matches, stats := searcher.GrepBounded(ctx, query, limit, maxFiles)
		return matches, stats.Incomplete
	}

	paths := idx.knownFilePaths()
	matches, stats := trigram.GrepPathsBounded(ctx, idx.rootPath, paths, query, limit, maxFiles)
	return matches, stats.Incomplete
}

// GrepLiteralBounded is the localization-specific variant. It filters out
// substring-only occurrences, prioritizes production files, and retains at
// most one representative per file so a test-heavy path prefix cannot consume
// the global recall budget.
func (idx *Indexer) GrepLiteralBounded(
	ctx context.Context,
	query string,
	limit int,
	maxFiles int,
) ([]trigram.Match, bool) {
	if idx == nil || query == "" {
		return nil, false
	}
	gen := idx.indexGen.Load()
	idx.trigramMu.Lock()
	searcher := idx.trigramSearcher
	warm := searcher != nil && idx.trigramGen == gen
	var release func()
	var bytes int64
	if warm {
		idx.applyToolWritesLocked()
		release = idx.trigramReleaseLocked()
		bytes = searcher.ApproxIndexBytes()
	}
	idx.trigramMu.Unlock()
	if warm {
		idx.trigramBudget().touch(idx, release, bytes)
		matches, stats := searcher.GrepLiteralBounded(
			ctx, query, limit, maxFiles, isProductionSourcePath,
		)
		return matches, stats.Incomplete
	}

	paths := idx.knownFilePaths()
	matches, stats := trigram.GrepLiteralPathsBounded(
		ctx, idx.rootPath, paths, query, limit, maxFiles, isProductionSourcePath,
	)
	return matches, stats.Incomplete
}

func isProductionSourcePath(path string) bool {
	return !IsTestFile(path)
}

// GrepTextForRepoBounded is the single-repository MultiIndexer bridge. It
// intentionally cannot fan out: source-literal localization must remain bound
// to the active repository even when the daemon tracks many repositories.
func (mi *MultiIndexer) GrepTextForRepoBounded(
	ctx context.Context,
	repoPrefix string,
	query string,
	limit int,
	maxFiles int,
) ([]trigram.Match, bool) {
	if mi == nil || repoPrefix == "" {
		return nil, false
	}
	idx := mi.GetIndexer(repoPrefix)
	if idx == nil {
		return nil, false
	}
	matches, incomplete := idx.GrepTextBounded(ctx, query, limit, maxFiles)
	return stampGrepMatchPaths(idx.RepoPrefix(), matches), incomplete
}

type BoundedRepoLiteralResult struct {
	Matches    []trigram.Match
	Incomplete bool
	Owned      bool
	Configured bool
	RepoPrefix string
}

// GrepLiteralForRepoBounded is the single-repository bridge for the
// localization-specific literal policy. Ownership and configured state are
// captured under one MultiIndexer lock, so an unresolved repository cannot
// race into a direct-backend fallback.
func (mi *MultiIndexer) GrepLiteralForRepoBounded(
	ctx context.Context,
	repoPrefix string,
	query string,
	limit int,
	maxFiles int,
) BoundedRepoLiteralResult {
	idx, configured := mi.boundedRepoIndexer(repoPrefix)
	if idx == nil {
		return BoundedRepoLiteralResult{Configured: configured}
	}
	matches, incomplete := idx.GrepLiteralBounded(ctx, query, limit, maxFiles)
	graphPrefix := idx.RepoPrefix()
	return BoundedRepoLiteralResult{
		Matches:    stampGrepMatchPaths(graphPrefix, matches),
		Incomplete: incomplete,
		Owned:      true,
		Configured: true,
		RepoPrefix: graphPrefix,
	}
}

// boundedRepoIndexer resolves ownership and the presence of any configured
// repository atomically. Empty prefix remains valid only for one registered
// indexer, regardless of the registry key used to store it.
func (mi *MultiIndexer) boundedRepoIndexer(repoPrefix string) (*Indexer, bool) {
	if mi == nil {
		return nil, false
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	configured := len(mi.indexers) > 0
	if repoPrefix != "" {
		return mi.indexers[repoPrefix], configured
	}
	if len(mi.indexers) != 1 {
		return nil, configured
	}
	for _, idx := range mi.indexers {
		return idx, true
	}
	return nil, configured
}
