package indexer

import (
	"context"
	"regexp"
	"regexp/syntax"
	"sort"

	"github.com/zzet/gortex/internal/search/trigram"
)

// GrepText runs a trigram-accelerated literal search for query across
// the indexed repo, returning up to limit matching lines (a
// non-positive limit returns every match). The trigram index is built
// lazily on first use and reused across calls; it is rebuilt only when
// a full or incremental index has advanced the repo generation, so a
// burst of searches between reindexes all hit a warm index.
func (idx *Indexer) GrepText(query string, limit int) []trigram.Match {
	if query == "" {
		return nil
	}
	if s := idx.warmTrigramSearcher(); s != nil {
		return s.Grep(query, limit)
	}
	// No index, and this repo has not earned one. Stream over the known
	// file list instead: same recall, no index state. This is what keeps
	// one unscoped fan-out from building an index for every tracked repo
	// only for the budget to evict all but a few.
	matches, _ := idx.GrepTextBounded(context.Background(), query, limit, 0)
	return matches
}

// GrepTextPaths is GrepText restricted to files under one of the
// forward-slash repo-relative prefixes. The restriction applies BEFORE
// the limit cut — a scoped sweep must surface scoped matches even when
// out-of-scope files own the head of the global ordering (issue #827).
// Empty prefixes mean unscoped (identical to GrepText).
func (idx *Indexer) GrepTextPaths(query string, prefixes []string, limit int) []trigram.Match {
	if query == "" {
		return nil
	}
	if len(prefixes) == 0 {
		return idx.GrepText(query, limit)
	}
	if s := idx.warmTrigramSearcher(); s != nil {
		return s.GrepPaths(query, prefixes, limit)
	}
	// Streaming fallback: apply the prefix restriction to the known file
	// list before the bounded scan, so the cold path keeps the same
	// pre-truncation semantics as the warm one.
	matches, _ := trigram.GrepPathsBounded(context.Background(), idx.rootPath,
		filterKnownPathsByPrefix(idx.knownFilePaths(), prefixes), query, limit, 0)
	return matches
}

// filterKnownPathsByPrefix keeps the known file list entries sitting
// under one of the forward-slash repo-relative prefixes, matched on
// segment boundaries — the same semantics the warm path applies.
func filterKnownPathsByPrefix(paths, prefixes []string) []string {
	out := make([]string, 0, len(paths))
	for _, rel := range paths {
		if trigram.PathUnderAnyPrefix(rel, prefixes) {
			out = append(out, rel)
		}
	}
	return out
}

// warmTrigramSearcher returns the current trigram searcher, rebuilding it
// when the index generation has moved since the cached searcher was
// built. Returns nil before anything has been indexed, and nil when the
// budget forbids building at all — callers must have a streaming fallback.
func (idx *Indexer) warmTrigramSearcher() *trigram.Searcher {
	budget := idx.trigramBudget()
	if !budget.allowsBuild() {
		return nil
	}
	gen := idx.indexGen.Load()

	// A repo already holding a current index skips the demand gate: the
	// build is paid for, so re-earning it would only throw it away.
	if !idx.hasWarmTrigramSearcher() && !budget.earnsBuild(idx) {
		return nil
	}

	// trigramMu is held across the build on purpose: it collapses a burst
	// of concurrent searches on one repo into a single build instead of
	// several racing ones, each paying full corpus memory.
	idx.trigramMu.Lock()
	if idx.trigramSearcher != nil && idx.trigramGen != gen && idx.canPatchTrigramLocked() {
		// An incremental pass moved the generation but only touched a few
		// files. Re-reading those beats re-reading the corpus.
		for rel := range idx.trigramDirty {
			idx.trigramSearcher.Update(rel)
		}
		clear(idx.trigramDirty)
		idx.trigramGen = gen
	}
	if idx.trigramSearcher == nil || idx.trigramGen != gen {
		if root := idx.rootPath; root != "" {
			idx.trigramSearcher = trigram.Build(root, idx.knownFilePaths())
			idx.trigramGen = gen
			clear(idx.trigramDirty)
		}
	}
	searcher := idx.trigramSearcher
	var release func()
	var bytes int64
	if searcher != nil {
		release = idx.trigramReleaseLocked()
		bytes = searcher.ApproxIndexBytes()
	}
	idx.trigramMu.Unlock()

	// Touch only after dropping trigramMu: an over-budget touch may release a
	// different repo, whose callback takes that repo's trigramMu. The lease
	// captured above prevents a delayed callback from racing a later re-touch.
	if release != nil {
		budget.touch(idx, release, bytes)
	}
	return searcher
}

// trigramPatchLimit is how many changed files an incremental patch will
// absorb before a full rebuild is the cheaper option. Patching costs one
// read per changed file plus posting churn; past roughly this fraction of
// a repo, rebuilding from a single pass wins.
const trigramPatchLimit = 256

// canPatchTrigramLocked reports whether the pending dirty set is small
// enough to apply file by file. The caller must hold trigramMu.
func (idx *Indexer) canPatchTrigramLocked() bool {
	n := len(idx.trigramDirty)
	return n > 0 && n <= trigramPatchLimit
}

// noteTrigramDirty records that an incremental pass changed these
// repo-relative paths, so the next search patches them into a live
// searcher instead of rebuilding the corpus. Deletions are included: the
// patch re-stats each path and drops one that is gone.
//
// Paths accumulate only while a searcher is live; with none built there is
// nothing to patch and the next search builds from the current corpus.
func (idx *Indexer) noteTrigramDirty(paths ...string) {
	if idx == nil || len(paths) == 0 {
		return
	}
	idx.trigramMu.Lock()
	defer idx.trigramMu.Unlock()
	if idx.trigramSearcher == nil {
		return
	}
	if idx.trigramDirty == nil {
		idx.trigramDirty = make(map[string]struct{}, len(paths))
	}
	for _, p := range paths {
		if p != "" {
			idx.trigramDirty[p] = struct{}{}
		}
	}
}

// hasWarmTrigramSearcher reports whether this repo already holds a built
// searcher, without building one. Callers that can stream instead use it
// to keep a broad fan-out from building an index per repo.
func (idx *Indexer) hasWarmTrigramSearcher() bool {
	if idx == nil {
		return false
	}
	gen := idx.indexGen.Load()
	idx.trigramMu.Lock()
	defer idx.trigramMu.Unlock()
	return idx.trigramSearcher != nil && idx.trigramGen == gen
}

// trigramBudget returns the budget this Indexer participates in, defaulting
// to the process-wide one so an Indexer built without wiring is still bounded.
func (idx *Indexer) trigramBudget() *trigramBudget {
	if idx.trigramBudgetOverride != nil {
		return idx.trigramBudgetOverride
	}
	return processTrigramBudget
}

// releaseTrigramSearcher drops this repo's built trigram index. Called by the
// budget when the repo has gone idle or has been pushed out by more recently
// used repos. Safe to call when nothing is built; the next search rebuilds
// from disk, which costs latency but never correctness — the searcher is only
// a candidate filter over files Grep re-reads anyway.
func (idx *Indexer) releaseTrigramSearcher() {
	if idx == nil {
		return
	}
	idx.trigramMu.Lock()
	idx.trigramLease++
	idx.trigramSearcher = nil
	idx.trigramGen = 0
	clear(idx.trigramDirty)
	idx.trigramMu.Unlock()
}

// trigramReleaseLocked returns a callback tied to the current cache-use lease.
// The caller must hold trigramMu. A re-touch advances the lease before the old
// callback can take the mutex, making a delayed timer/LRU release a no-op.
func (idx *Indexer) trigramReleaseLocked() func() {
	idx.trigramLease++
	lease := idx.trigramLease
	return func() { idx.releaseTrigramSearcherLease(lease) }
}

func (idx *Indexer) releaseTrigramSearcherLease(lease uint64) {
	idx.trigramMu.Lock()
	if idx.trigramLease == lease {
		idx.trigramSearcher = nil
		idx.trigramGen = 0
		clear(idx.trigramDirty)
	}
	idx.trigramMu.Unlock()
}

// GrepRegexp runs a trigram-accelerated regular-expression search for
// pattern across the indexed repo, returning up to limit matching lines
// (a non-positive limit returns every match). The pattern's mandatory
// literal runs are extracted and used to pre-filter the candidate file
// set via the trigram index; the compiled regexp then verifies each
// candidate line. pathPrefix, when non-empty, restricts the scan to
// files under that forward-slash repo-relative prefix.
//
// A pattern that does not compile returns an error so the caller can
// surface it; an empty pattern returns no matches and no error.
func (idx *Indexer) GrepRegexp(pattern, pathPrefix string, limit int) ([]trigram.Match, error) {
	if pattern == "" {
		return nil, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if s := idx.warmTrigramSearcher(); s != nil {
		literals := extractRegexLiterals(pattern)
		return s.GrepRegexp(re, literals, pathPrefix, limit), nil
	}
	// See GrepText: stream rather than build an index this repo has not
	// earned. Without a trigram pre-filter every known file is scanned,
	// which is what the literal-free branch of the warm path does anyway.
	matches, _ := trigram.GrepRegexpPathsBounded(
		context.Background(), idx.rootPath, idx.knownFilePaths(), re, pathPrefix, limit, 0,
	)
	return matches, nil
}

// GrepRegexpPaths is GrepRegexp restricted to documents under one of
// the forward-slash path prefixes, matched on segment boundaries. The
// restriction applies BEFORE the limit cut, so a regexp query scoped
// with `path` surfaces scoped matches even when out-of-scope files own
// the head of the global ordering (issue #827 for regexp queries,
// #845). Empty prefixes mean unscoped.
func (idx *Indexer) GrepRegexpPaths(pattern string, prefixes []string, limit int) ([]trigram.Match, error) {
	if pattern == "" {
		return nil, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if s := idx.warmTrigramSearcher(); s != nil {
		literals := extractRegexLiterals(pattern)
		return s.GrepRegexpPaths(re, literals, prefixes, limit), nil
	}
	// Streaming fallback: the prefix restriction filters the known file
	// list before the bounded scan, so the cold path keeps the same
	// pre-truncation semantics as the warm one.
	filtered := filterKnownPathsByPrefix(idx.knownFilePaths(), prefixes)
	matches, _ := trigram.GrepRegexpPathsBounded(
		context.Background(), idx.rootPath, filtered, re, "", limit, 0,
	)
	return matches, nil
}

// knownFilePaths returns a sorted snapshot of the repo's indexed files —
// the corpus every streaming search walks, and the one Build indexes.
//
// Safe to call while holding trigramMu: mtimeMu is a leaf lock, and the
// build path took it in exactly this order before.
func (idx *Indexer) knownFilePaths() []string {
	if idx == nil {
		return nil
	}
	idx.mtimeMu.RLock()
	paths := make([]string, 0, len(idx.fileMtimes))
	for rel := range idx.fileMtimes {
		paths = append(paths, rel)
	}
	idx.mtimeMu.RUnlock()
	sort.Strings(paths)
	return paths
}

// extractRegexLiterals returns the literal substrings the regexp must
// contain on every match. It parses the pattern with regexp/syntax and
// walks the syntax tree for OpLiteral runs that are mandatory — i.e.
// reached through OpConcat / OpCapture only, never under an alternation
// or a zero-min repetition. Each such run is rendered to its UTF-8
// bytes; runs shorter than three bytes are dropped (they cannot be
// trigram-filtered). A pattern that does not parse yields no literals,
// which is safe: the caller falls back to scanning every file.
func extractRegexLiterals(pattern string) []string {
	reSyn, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	var out []string
	var walk func(re *syntax.Regexp)
	walk = func(re *syntax.Regexp) {
		switch re.Op {
		case syntax.OpLiteral:
			if s := string(re.Rune); len(s) >= 3 {
				out = append(out, s)
			}
		case syntax.OpConcat, syntax.OpCapture:
			// A concatenation / capture preserves the mandatory nature
			// of each child, so recurse into all of them.
			for _, sub := range re.Sub {
				walk(sub)
			}
		case syntax.OpPlus:
			// x+ guarantees at least one x — its literal is mandatory.
			for _, sub := range re.Sub {
				walk(sub)
			}
			// Other ops (OpAlternate, OpStar, OpQuest, OpRepeat with
			// Min==0, character classes, anchors) do not contribute a
			// guaranteed literal — stop descending.
		}
	}
	walk(reSyn)
	return out
}
