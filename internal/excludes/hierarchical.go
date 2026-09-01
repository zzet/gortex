package excludes

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Hierarchical matches paths against per-directory ignore files
// discovered along the chain from a repo root down to each path's
// parent directory. Unlike the repo-root-only .gitignore handling, an
// ignore file placed in any directory is honored, with its patterns
// anchored at the directory that contains it — a pattern in
// <root>/sub/.gortexignore constrains only paths under <root>/sub.
//
// Each directory's ignore files are read and compiled once, on first
// request, and cached. A full index walk therefore pays one read per
// directory regardless of how deep the tree is. Hierarchical is safe
// for concurrent use.
type Hierarchical struct {
	root      string
	filenames []string

	mu    sync.RWMutex
	cache map[string]*Matcher // abs dir -> compiled matcher; nil value = directory has no ignore files
}

// NewHierarchical builds a per-directory ignore matcher rooted at root.
// filenames are the ignore-file basenames honored in every directory
// (e.g. ".gortexignore"). An empty filename list yields a matcher that
// excludes nothing.
func NewHierarchical(root string, filenames ...string) *Hierarchical {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Hierarchical{
		root:      filepath.Clean(root),
		filenames: filenames,
		cache:     make(map[string]*Matcher),
	}
}

// Match reports whether an absolute path is excluded by an ignore file
// in any ancestor directory between the root and the path's parent.
// When isDir is true the path is treated as a directory, so trailing-
// slash patterns prune the whole subtree. A path outside the root is
// never excluded.
func (h *Hierarchical) Match(absPath string, isDir bool) bool {
	matched, _, _ := h.Explain(absPath, isDir)
	return matched
}

// Explain is Match plus attribution: the absolute directory whose ignore
// file excluded the path, and the pattern that did it. Both are "" when
// nothing matched. Used to name the culprit when an over-broad ignore
// leaves a repo with nothing to index.
func (h *Hierarchical) Explain(absPath string, isDir bool) (bool, string, string) {
	if h == nil || len(h.filenames) == 0 {
		return false, "", ""
	}
	absPath = filepath.Clean(absPath)
	rel, err := filepath.Rel(h.root, absPath)
	if err != nil {
		return false, "", ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, "../") {
		return false, "", ""
	}

	// Test the path against the root's ignore matcher and that of every
	// ancestor directory down to (but excluding) the path itself. Any
	// level that excludes the path wins; a file's own directory cannot
	// exclude the file from itself.
	dir := h.root
	if matched, pat := h.dirMatcher(dir).ExplainAbsDir(absPath, dir, isDir); matched {
		return true, dir, pat
	}
	segs := strings.Split(rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		dir = filepath.Join(dir, seg)
		if matched, pat := h.dirMatcher(dir).ExplainAbsDir(absPath, dir, isDir); matched {
			return true, dir, pat
		}
	}
	return false, "", ""
}

// HasNegatedDescendant reports whether any per-directory ignore file
// along the chain from the root down to absDir carries a re-include
// ("!") pattern that could match a path strictly beneath absDir. The
// index walk uses it to keep descending an excluded directory whose
// subtree a negation could resurrect, instead of pruning it outright —
// the per-directory counterpart of Matcher.HasNegatedDescendant. absDir
// is an absolute directory path; a path outside the root never matches.
func (h *Hierarchical) HasNegatedDescendant(absDir string) bool {
	if h == nil || len(h.filenames) == 0 {
		return false
	}
	absDir = filepath.Clean(absDir)
	rel, err := filepath.Rel(h.root, absDir)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}

	// Each directory's ignore patterns are anchored at that directory, so
	// the question "is there a negated descendant of absDir?" is asked of
	// every ancestor matcher (and absDir's own) with absDir expressed
	// relative to that ancestor.
	dir := h.root
	if h.dirMatcher(dir).HasNegatedDescendant(relUnder(dir, absDir)) {
		return true
	}
	if rel == "." || rel == "" {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		dir = filepath.Join(dir, seg)
		if h.dirMatcher(dir).HasNegatedDescendant(relUnder(dir, absDir)) {
			return true
		}
	}
	return false
}

// relUnder returns child expressed relative to dir, in forward-slash
// form. It returns "" when child is dir itself.
func relUnder(dir, child string) string {
	rel, err := filepath.Rel(dir, child)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return ""
	}
	return rel
}

// dirMatcher returns the compiled ignore matcher for one directory,
// reading and parsing its ignore files on first request. A directory
// with no ignore files (or only empty ones) caches a nil matcher; the
// *Matcher methods are nil-safe so callers need no guard.
func (h *Hierarchical) dirMatcher(dir string) *Matcher {
	h.mu.RLock()
	m, ok := h.cache[dir]
	h.mu.RUnlock()
	if ok {
		return m
	}

	var patterns []string
	for _, name := range h.filenames {
		patterns = append(patterns, readIgnoreFile(filepath.Join(dir, name))...)
	}
	if len(patterns) > 0 {
		m = New(patterns)
	}

	h.mu.Lock()
	h.cache[dir] = m
	h.mu.Unlock()
	return m
}

// readIgnoreFile reads one ignore file and returns its non-blank,
// non-comment lines as gitignore-syntax patterns. A missing or
// unreadable file yields nil — honoring ignore files is a convenience,
// never a hard requirement, so a missing or permission-denied file
// silently no-ops.
func readIgnoreFile(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var patterns []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}
