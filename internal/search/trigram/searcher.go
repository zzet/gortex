package trigram

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graphpath"
	"github.com/zzet/gortex/internal/pathguard"
)

// Match is one line of one file that contains the query literal.
type Match struct {
	Path string `json:"path"` // forward-slash repo-relative path
	Line int    `json:"line"` // 1-based line number
	Text string `json:"text"` // the matching line
}

// Searcher is a trigram-accelerated literal code search over a fixed
// set of files. Build it once against a repo's file list, then Grep it
// repeatedly. It is safe for concurrent Grep calls.
type Searcher struct {
	root         string
	resolvedRoot string // root with symlinks evaluated, for confinement tests
	ix           *Index

	// mu guards the path metadata below against a concurrent Update.
	// Index has its own lock for the postings. Existing path entries are
	// never rewritten — Update only appends — so a reader that snapshots
	// the slice header under RLock can range it without holding the lock.
	mu        sync.RWMutex
	paths     []string          // docID -> forward-slash repo-relative path
	pathIndex map[string]uint32 // reverse of paths, for Update
}

// Build reads every file — forward-slash repo-relative paths under
// root — and indexes its content. A file that cannot be read is left
// unindexed (it never matches) but keeps its docID slot so the rest
// stay aligned.
//
// Two further classes are kept out, and like an unreadable file they
// never match:
//
//   - Binary content (IsBinary). A literal text query cannot meaningfully
//     match compressed or encoded bytes, and indexing them costs roughly
//     50x the file's own size — a single 2 MiB PNG measured 101 MiB of
//     index.
//   - Anything over maxIndexedBytes, a per-document sanity ceiling well
//     above any file a person greps.
//
// A path that is a symlink out of root is treated as unreadable. The
// indexer walk already refuses to admit one, so this is the second of two
// independent barriers: it keeps a corpus assembled by some other route
// (a stale on-disk file list, a caller building its own relPaths) from
// turning this searcher into an arbitrary-file-read primitive.
func Build(root string, relPaths []string) *Searcher {
	s := &Searcher{
		root:         root,
		resolvedRoot: pathguard.ResolveRoot(root),
		ix:           New(),
		paths:        make([]string, len(relPaths)),
		pathIndex:    make(map[string]uint32, len(relPaths)),
	}
	for i, rel := range relPaths {
		rel = filepath.ToSlash(rel)
		s.paths[i] = rel
		s.pathIndex[rel] = uint32(i)
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if pathguard.EscapesResolvedRoot(abs, s.resolvedRoot) {
			continue
		}
		// Stat first: an oversized file must not be read at all, which is
		// the whole point of the ceiling.
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() || info.Size() > maxIndexedBytes {
			continue
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if IsBinary(content) {
			continue
		}
		s.ix.Add(uint32(i), content)
	}
	return s
}

// Update re-reads one repo-relative path and refreshes its postings in
// place, so an incremental index pass costs one file instead of a whole
// corpus rebuild. A path the searcher has not seen gets a fresh docID.
//
// Content that has become binary, oversized, or unreadable is dropped
// from the index by the same rules Build applies — including a file that
// was deleted, which is how a removal is expressed.
func (s *Searcher) Update(rel string) {
	if s == nil || rel == "" {
		return
	}
	rel = filepath.ToSlash(rel)

	s.mu.Lock()
	docID, known := s.pathIndex[rel]
	if !known {
		docID = uint32(len(s.paths))
		s.paths = append(s.paths, rel)
		s.pathIndex[rel] = docID
	}
	s.mu.Unlock()

	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	if pathguard.EscapesResolvedRoot(abs, s.resolvedRoot) {
		s.dropDoc(docID)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() || info.Size() > maxIndexedBytes {
		s.dropDoc(docID)
		return
	}
	content, err := os.ReadFile(abs)
	if err != nil || IsBinary(content) {
		s.dropDoc(docID)
		return
	}
	// Add is documented as the re-index path for a changed file: it drops
	// the previous postings before recording the new ones.
	s.ix.Add(docID, content)
}

func (s *Searcher) dropDoc(docID uint32) {
	s.ix.Remove(docID)
}

// snapshotPaths returns the current docID -> path mapping. The slice is
// append-only and its existing entries are never rewritten, so the
// caller may range it after the lock is dropped.
func (s *Searcher) snapshotPaths() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paths
}

// candidates returns the docIDs to verify for query.
func (s *Searcher) candidates(query string) []uint32 {
	return s.ix.Candidates(query)
}

// ApproxIndexBytes estimates the heap the index retains, for budgeting.
// The dominant terms are the posting map (one entry plus a one-element
// posting slice per distinct (doc, trigram) pair) and the per-document
// trigram slices; a path string rounds out each doc. The coefficients
// were fitted against measured heap for this repo's corpus and track it
// within about 10%.
func (s *Searcher) ApproxIndexBytes() int64 {
	if s == nil {
		return 0
	}
	return s.ix.approxBytes() + int64(len(s.snapshotPaths()))*40
}

// openConfined opens the indexed file at rel for scanning, refusing one
// whose real location escapes the searcher's root.
//
// Only the leaf is Lstat'd. That is sufficient rather than sloppy: an
// intermediate directory cannot be a symlink, because filepath.WalkDir
// never descends one, so no path in the corpus reaches through a linked
// directory. Checking the leaf costs one Lstat per scanned file; fully
// resolving every component would cost several per file on the hot search
// path and buy nothing given that provenance.
func (s *Searcher) openConfined(rel string) (*os.File, error) {
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	if pathguard.EscapesResolvedRoot(abs, s.resolvedRoot) {
		return nil, os.ErrPermission
	}
	return os.Open(abs)
}

// Grep returns up to limit lines, across the indexed files, that
// contain the literal query. The trigram index narrows the file set;
// each candidate file is then scanned to confirm the match and locate
// its lines. Results are ordered by file, then by line. A non-positive
// limit returns every match.
func (s *Searcher) Grep(query string, limit int) []Match {
	return s.GrepPaths(query, nil, limit)
}

// GrepPaths is Grep restricted to documents under one of the
// forward-slash path prefixes. The restriction runs BEFORE the limit
// cut: a scoped query must surface scoped matches even when a wall of
// out-of-scope files owns the head of the global ordering (issue #827).
// Empty prefixes mean unscoped — identical to Grep. Prefixes are
// matched on the same segment-boundary semantics GrepRegexp's
// pathPrefix uses (graphpath.HasPrefix).
func (s *Searcher) GrepPaths(query string, prefixes []string, limit int) []Match {
	if query == "" {
		return nil
	}
	paths := s.snapshotPaths()
	var matches []Match
	for _, docID := range s.candidates(query) {
		if int(docID) >= len(paths) {
			continue
		}
		rel := paths[docID]
		if !pathUnderAnyPrefix(rel, prefixes) {
			continue
		}
		f, err := s.openConfined(rel)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			if strings.Contains(text, query) {
				matches = append(matches, Match{Path: rel, Line: line, Text: text})
				if limit > 0 && len(matches) >= limit {
					_ = f.Close()
					return matches
				}
			}
		}
		_ = f.Close()
	}
	return matches
}

// pathUnderAnyPrefix reports whether path sits under one of the
// forward-slash prefixes on segment boundaries. An empty prefix set
// matches everything — the unscoped default.
func pathUnderAnyPrefix(path string, prefixes []string) bool {
	for _, pre := range prefixes {
		if graphpath.HasPrefix(path, pre) {
			return true
		}
	}
	return len(prefixes) == 0
}

// DocCount returns the number of indexed files.
func (s *Searcher) DocCount() int { return s.ix.DocCount() }

// GrepRegexp returns up to limit lines, across the indexed files, that
// the compiled regexp re matches. requiredLiterals is a set of literal
// substrings (each ideally >= 3 bytes) that every matching line's file
// must contain — they come from the regex's own mandatory literal runs
// and let the trigram index narrow the candidate file set. When
// requiredLiterals is empty no trigram pre-filter is possible and every
// indexed file is scanned. Results are ordered by file, then by line.
// A non-positive limit returns every match.
//
// pathPrefix, when non-empty, restricts the scan to files whose
// forward-slash repo-relative path starts with it.
func (s *Searcher) GrepRegexp(re *regexp.Regexp, requiredLiterals []string, pathPrefix string, limit int) []Match {
	if re == nil {
		return nil
	}

	// Build the candidate doc set. Each required literal intersects its
	// trigram posting list into the running set; the first literal
	// seeds it. With no usable literal we fall back to every doc.
	var candidates map[uint32]struct{}
	for _, lit := range requiredLiterals {
		if len(lit) < 3 {
			// Too short to trigram-filter — skip; the regex scan still
			// verifies, so correctness is unaffected.
			continue
		}
		got := make(map[uint32]struct{})
		for _, id := range s.ix.Candidates(lit) {
			got[id] = struct{}{}
		}
		if candidates == nil {
			candidates = got
			continue
		}
		for id := range candidates {
			if _, ok := got[id]; !ok {
				delete(candidates, id)
			}
		}
	}

	var docIDs []uint32
	if candidates == nil {
		// No usable literal to trigram-filter on: scan every searchable
		// document. Binary, oversized and unreadable documents are
		// excluded here exactly as they are from the literal path.
		docIDs = s.ix.docIDs()
	} else {
		docIDs = make([]uint32, 0, len(candidates))
		for id := range candidates {
			docIDs = append(docIDs, id)
		}
		slices.Sort(docIDs)
	}

	paths := s.snapshotPaths()
	var matches []Match
	for _, docID := range docIDs {
		if int(docID) >= len(paths) {
			continue
		}
		rel := paths[docID]
		if !graphpath.HasPrefix(rel, pathPrefix) {
			continue
		}
		f, err := s.openConfined(rel)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			if re.MatchString(text) {
				matches = append(matches, Match{Path: rel, Line: line, Text: text})
				if limit > 0 && len(matches) >= limit {
					_ = f.Close()
					return matches
				}
			}
		}
		_ = f.Close()
	}
	return matches
}
