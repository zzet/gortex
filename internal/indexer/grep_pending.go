package indexer

import (
	"context"
	"regexp"

	"github.com/zzet/gortex/internal/search/trigram"
)

// GrepTextPathsExcept leaves pending files to the caller's verified source
// scan. Exclusion precedes the limit and preserves native-cache search for
// unaffected files; it neither widens the page nor enumerates the filesystem.
func (idx *Indexer) GrepTextPathsExcept(query string, prefixes []string, excluded map[string]struct{}, limit int) []trigram.Match {
	if len(excluded) == 0 {
		return idx.GrepTextPaths(query, prefixes, limit)
	}
	if query == "" {
		return nil
	}
	if len(prefixes) == 0 {
		if searcher := idx.warmTrigramSearcher(); searcher != nil {
			return searcher.GrepPathsExcept(query, nil, excluded, limit)
		}
	}
	paths := excludeGrepPaths(filterKnownPathsByPrefix(idx.knownFilePaths(), prefixes), excluded)
	matches, _ := trigram.GrepPathsBounded(context.Background(), idx.rootPath, paths, query, limit, 0)
	return matches
}

// GrepRegexpPathsExcept excludes pending files before the regexp page limit.
func (idx *Indexer) GrepRegexpPathsExcept(pattern string, prefixes []string, excluded map[string]struct{}, limit int) ([]trigram.Match, error) {
	if len(excluded) == 0 {
		return idx.GrepRegexpPaths(pattern, prefixes, limit)
	}
	if pattern == "" {
		return nil, nil
	}
	expression, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if searcher := idx.warmTrigramSearcher(); searcher != nil {
		return searcher.GrepRegexpPathsExcept(expression, extractRegexLiterals(pattern), prefixes, excluded, limit), nil
	}
	paths := excludeGrepPaths(filterKnownPathsByPrefix(idx.knownFilePaths(), prefixes), excluded)
	matches, _ := trigram.GrepRegexpPathsBounded(context.Background(), idx.rootPath, paths, expression, "", limit, 0)
	return matches, nil
}

func excludeGrepPaths(paths []string, excluded map[string]struct{}) []string {
	kept := paths[:0]
	for _, path := range paths {
		if _, skip := excluded[path]; !skip {
			kept = append(kept, path)
		}
	}
	return kept
}

// GrepTextForReposPathsExcept retains the existing per-repository cap while
// leaving pending files to the tool's exact scan before its final request cap.
func (mi *MultiIndexer) GrepTextForReposPathsExcept(query string, repoAllow map[string]bool, shared []string, perRepo map[string][]string, excluded map[string]map[string]struct{}, limit int) []trigram.Match {
	var matches []trigram.Match
	for _, job := range mi.grepRepoJobs(repoAllow) {
		prefixes, allowed := effectivePathPrefixes(job.prefix, shared, perRepo)
		if !allowed {
			continue
		}
		matches = append(matches, stampGrepMatchPaths(job.prefix, job.idx.GrepTextPathsExcept(query, prefixes, excluded[job.prefix], limit))...)
	}
	return matches
}

// GrepRegexpForReposPathsExcept is the regexp counterpart of that scoped union.
func (mi *MultiIndexer) GrepRegexpForReposPathsExcept(pattern string, repoAllow map[string]bool, shared []string, perRepo map[string][]string, excluded map[string]map[string]struct{}, limit int) ([]trigram.Match, error) {
	var matches []trigram.Match
	for _, job := range mi.grepRepoJobs(repoAllow) {
		prefixes, allowed := effectivePathPrefixes(job.prefix, shared, perRepo)
		if !allowed {
			continue
		}
		current, err := job.idx.GrepRegexpPathsExcept(pattern, prefixes, excluded[job.prefix], limit)
		if err != nil {
			return nil, err
		}
		matches = append(matches, stampGrepMatchPaths(job.prefix, current)...)
	}
	return matches, nil
}
