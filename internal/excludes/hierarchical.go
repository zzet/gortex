package excludes

import (
	"bufio"
	"bytes"
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
	// forgets counts Forget calls. A read that started before one does not
	// store its result, which may predate the change Forget reports.
	forgets  uint64
	diagnose func(string, *Matcher)

	// observe, when set (ObserveReads), is handed every ignore file read.
	observe func(path string, data []byte, err error)
}

// ObserveReads installs fn, which is handed every ignore file the matcher
// reads: the bytes it read, or the error that ended the read (a missing
// file's fs.ErrNotExist among them). A caller that must account for every
// working-copy read it makes (a build proving its reads) installs it before
// the matcher's first use; it is not safe to install concurrently with one.
func (h *Hierarchical) ObserveReads(fn func(path string, data []byte, err error)) {
	if h != nil {
		h.observe = fn
	}
}

// NewHierarchical builds a per-directory ignore matcher rooted at root.
// filenames are the ignore-file basenames honored in every directory
// (e.g. ".gortexignore"). An empty filename list yields a matcher that
// excludes nothing.
func NewHierarchical(root string, filenames ...string) *Hierarchical {
	return NewHierarchicalWithDiagnostics(root, filenames, nil)
}

// NewHierarchicalWithDiagnostics reports each loaded list once, when cached.
// diagnose must not call back into the hierarchical matcher.
func NewHierarchicalWithDiagnostics(root string, filenames []string, diagnose func(string, *Matcher)) *Hierarchical {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Hierarchical{
		root:      filepath.Clean(root),
		filenames: filenames,
		cache:     make(map[string]*Matcher),
		diagnose:  diagnose,
	}
}

// Forget drops the cached rules of absDir and of every folder beneath it,
// so the next check reads their ignore files again. Call it when an ignore
// file in absDir is created, changed or deleted, or when absDir is removed
// or renamed: a folder moved away and replaced by another tree may bring
// different ignore files at any depth.
//
// A folder is cached only after its parent (see dirMatcher), so when absDir
// is not cached nothing beneath it is either, and Forget costs one map
// lookup. That keeps file removals, which also call Forget, cheap.
func (h *Hierarchical) Forget(absDir string) {
	if h == nil {
		return
	}
	dir := filepath.Clean(absDir)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.forgets++
	if _, ok := h.cache[dir]; !ok {
		return
	}
	prefix := dir + string(filepath.Separator)
	if dir == h.root {
		clear(h.cache)
		return
	}
	for key := range h.cache {
		if key == dir || strings.HasPrefix(key, prefix) {
			delete(h.cache, key)
		}
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
	return h.explain(absPath, isDir, (*Matcher).ExplainAbsDir)
}

// MatchInOpenDir omits parent checks already proved by a pruned disk walk.
func (h *Hierarchical) MatchInOpenDir(absPath string, isDir bool) bool {
	matched, _, _ := h.explain(absPath, isDir, (*Matcher).ExplainAbsDirInOpenDir)
	return matched
}

func (h *Hierarchical) explain(absPath string, isDir bool, match func(*Matcher, string, string, bool) (bool, string)) (bool, string, string) {
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
	if matched, pat := match(h.dirMatcher(dir), absPath, dir, isDir); matched {
		return true, dir, pat
	}
	segs := strings.Split(rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		dir = filepath.Join(dir, seg)
		if matched, pat := match(h.dirMatcher(dir), absPath, dir, isDir); matched {
			return true, dir, pat
		}
	}
	return false, "", ""
}

// dirMatcher returns the compiled ignore matcher for one directory,
// reading and parsing its ignore files on first request. A directory
// with no ignore files (or only empty ones) caches a nil matcher; the
// *Matcher methods are nil-safe so callers need no guard.
func (h *Hierarchical) dirMatcher(dir string) *Matcher {
	h.mu.RLock()
	m, ok := h.cache[dir]
	forgets := h.forgets
	h.mu.RUnlock()
	if ok {
		return m
	}

	var patterns []string
	for _, name := range h.filenames {
		patterns = append(patterns, readIgnoreFile(filepath.Join(dir, name), h.observe)...)
	}
	if len(patterns) > 0 {
		m = New(patterns)
	}

	h.mu.Lock()
	// Store only when no Forget ran since the read and the parent is
	// cached, so a cached folder always has cached ancestors and Forget
	// can skip folders that are not cached.
	if cached, ok := h.cache[dir]; ok {
		m = cached
	} else if h.forgets == forgets && (dir == h.root || h.cached(filepath.Dir(dir))) {
		h.cache[dir] = m
		if m != nil && h.diagnose != nil {
			h.diagnose(dir, m)
		}
	}
	h.mu.Unlock()
	return m
}

// cached reports whether dir has an entry. The caller holds h.mu.
func (h *Hierarchical) cached(dir string) bool {
	_, ok := h.cache[dir]
	return ok
}

// readIgnoreFile reads one ignore file and returns its non-blank,
// non-comment lines as gitignore-syntax patterns. A missing or
// unreadable file yields nil — honoring ignore files is a convenience,
// never a hard requirement, so a missing or permission-denied file
// silently no-ops. observe, when set, is handed what the read found.
func readIgnoreFile(path string, observe func(string, []byte, error)) []string {
	data, err := os.ReadFile(path)
	if observe != nil {
		observe(path, data, err)
	}
	if err != nil {
		return nil
	}

	var patterns []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}
