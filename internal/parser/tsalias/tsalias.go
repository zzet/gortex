// Package tsalias resolves TypeScript / JavaScript path-alias imports
// declared in `tsconfig.json` / `jsconfig.json` to repo-relative file
// paths the rest of the indexer can consume.
//
// Recognised shape:
//
//	{
//	  "compilerOptions": {
//	    "baseUrl": "./src",
//	    "paths": {
//	      "@/*": ["lib/*"],
//	      "@components/*": ["src/components/*"],
//	      "$utils": ["src/util/index.ts"]
//	    }
//	  }
//	}
//
// Resolution semantics follow the tsserver / Vite / Webpack consensus:
//
//   - Entries are matched longest-prefix-first so `@components/Button`
//     matches `@components/*` ahead of a hypothetical `@/*`.
//   - A single `*` wildcard splits the pattern into a prefix and a
//     suffix; the substring matched by `*` is slotted into the target
//     at the corresponding `*` position.
//   - Patterns without `*` are exact-match.
//   - Targets are joined with `baseUrl` (if set). Source extensions are
//     stripped for probing; explicit declaration files retain their suffix.
//   - Multi-target arrays (`"@/*": ["a/*", "b/*"]`) are resolved by disk
//     existence: each candidate is probed under the repo root in priority
//     order and the first that exists on disk wins, falling back to the
//     first entry (the documented "primary" path) when none do.
//
// tsconfig JSONC features the TypeScript tooling itself accepts — `//` and
// `/* */` comments and trailing commas — are stripped before parsing, so a
// commented config does not silently drop every alias for the repo. Relative
// `extends` chains (including arrays) are followed with TypeScript option
// precedence; package specifiers and project references are not traversed.
package tsalias

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Alias is one entry in the `paths` map.
type Alias struct {
	// AliasPrefix is the portion of the source pattern before `*`
	// (or the full pattern when HasWildcard is false).
	AliasPrefix string
	// AliasSuffix is the portion after `*`. Usually empty.
	AliasSuffix string
	// TargetPrefix / TargetSuffix split the primary (first) resolved value
	// the same way. Retained as the single-target fast path and so an
	// externally hand-built Alias keeps working.
	TargetPrefix string
	TargetSuffix string
	// Targets is every replacement value for this pattern, in tsconfig
	// priority order, each split into prefix/suffix. When non-empty and a
	// repo root is known, Resolve probes them on disk and returns the first
	// that exists, falling back to Targets[0] — so a multi-target alias
	// lands on the file that actually exists, not blindly on the first
	// entry. Empty for a hand-built single-target Alias (TargetPrefix wins).
	Targets     []AliasTarget
	HasWildcard bool
}

// AliasTarget is one replacement value split around its `*`.
type AliasTarget struct {
	Prefix string
	Suffix string
}

// Map is the alias set declared by one tsconfig/jsconfig file.
type Map struct {
	Entries []Alias
	// BaseURL is the relative path the targets resolve against. Empty
	// when the config didn't declare one — callers should treat
	// targets as repo-relative in that case.
	BaseURL string
	// DirPrefix is the repo-relative path of the config file's
	// directory. Used by Collection to pick the nearest ancestor scope.
	DirPrefix string
	// targetBase can be the declaring parent rather than the child scope.
	targetBase string
	// exists reports whether a repo-relative slash path is readable content
	// in the tree this map was loaded from. Set by the loaders, it is what
	// grounds a multi-target alias in the tree the map came from — the
	// working copy for Load, the snapshot for LoadTree. Nil for hand-built
	// maps, which then resolve by first-match without probing anything.
	exists func(rel string) bool
}

// Collection aggregates every alias map found by a loader, sorted by
// DirPrefix length descending so nearest-ancestor lookup is a single
// linear scan.
type Collection struct {
	scopes           []*Map
	alternateTargets [][]string // normalized target patterns, only for multi-target aliases
}

// NewCollection returns a Collection over maps, ordered nearest-ancestor
// first (the order FindForFile scans). It is the constructor a caller that
// builds its own maps — a snapshot loader, or a test — uses instead of
// reaching for the unexported field. Returns nil for an empty set, which is
// the same "no usable config" answer the loaders give.
func NewCollection(maps []*Map) *Collection {
	scopes := make([]*Map, 0, len(maps))
	for _, m := range maps {
		if m != nil {
			scopes = append(scopes, m)
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	sort.SliceStable(scopes, func(i, j int) bool {
		return len(scopes[i].DirPrefix) > len(scopes[j].DirPrefix)
	})
	c := &Collection{scopes: scopes}
	for _, m := range scopes {
		for _, a := range m.Entries {
			if len(a.Targets) < 2 {
				continue
			}
			var capture string
			if a.HasWildcard {
				capture = "*"
			}
			targets := make([]string, len(a.Targets))
			for i, t := range a.Targets {
				targets[i] = graph.JSModuleStem(m.joinTarget(t.Prefix + capture + t.Suffix))
			}
			c.alternateTargets = append(c.alternateTargets, targets)
		}
	}
	return c
}

// ObservingProbes returns m with every existence probe Resolve makes on it
// reported to observe: the repo-relative path probed and whether it held a
// file. A map with no probe (hand-built), a nil map or a nil observe is
// returned as is. The copy shares m's entries, which stay read-only.
func (m *Map) ObservingProbes(observe func(rel string, found bool)) *Map {
	if m == nil || m.exists == nil || observe == nil {
		return m
	}
	exists := m.exists
	out := *m
	out.exists = func(rel string) bool {
		found := exists(rel)
		observe(rel, found)
		return found
	}
	return &out
}

// Maps returns the underlying scope slice, which callers treat as read-only.
// A nil Collection has none.
func (c *Collection) Maps() []*Map {
	if c == nil {
		return nil
	}
	return c.scopes
}

// Digest identifies the effective scopes and ordered targets, not the
// filesystem probes or the spelling of the configs they came from.
func (c *Collection) Digest() string {
	if c == nil {
		return "none"
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, scope := range c.scopes {
		// Strings and slices of Alias are always JSON-encodable.
		_ = enc.Encode(scope.DirPrefix)
		_ = enc.Encode(scope.BaseURL)
		_ = enc.Encode(scope.targetBase)
		_ = enc.Encode(scope.Entries)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FindForFile returns the alias map for the nearest ancestor scope of
// relPath, or nil when no scope applies.
func (c *Collection) FindForFile(relPath string) *Map {
	if c == nil {
		return nil
	}
	relPath = filepath.ToSlash(relPath)
	for _, m := range c.scopes {
		if m.DirPrefix == "" {
			return m
		}
		if relPath == m.DirPrefix || strings.HasPrefix(relPath, m.DirPrefix+"/") {
			return m
		}
	}
	return nil
}

// Resolve maps modulePath against m's aliases and returns the
// repo-relative probe path or "" when no entry matches. aliasProbePath
// preserves explicit declaration files and strips implementation extensions.
func Resolve(m *Map, modulePath string) string {
	if m == nil || modulePath == "" {
		return ""
	}
	for i := range m.Entries {
		a := &m.Entries[i]
		star, ok := matchAlias(a, modulePath)
		if !ok {
			continue
		}
		targets := a.Targets
		if len(targets) == 0 {
			targets = []AliasTarget{{Prefix: a.TargetPrefix, Suffix: a.TargetSuffix}}
		}
		first := ""
		for ti, tgt := range targets {
			joined := m.joinTarget(tgt.Prefix + star + tgt.Suffix)
			stripped := aliasProbePath(joined)
			if ti == 0 {
				first = stripped
			}
			// Tree-grounded multi-target: return the first candidate that
			// actually exists in the tree this map was loaded from. Skipped
			// for hand-built maps (no exists probe), which fall through to
			// the documented primary path.
			if m.exists != nil && targetExistsInTree(m.exists, joined) {
				return stripped
			}
		}
		return first
	}
	return ""
}

// VisitAlternateStems reports the other possible selections of a multi-target
// alias when one of its target stems changes. Invert the target's wildcard,
// not the importer's path: an inherited map may have a different target base.
func (c *Collection) VisitAlternateStems(stem string, visit func(string)) {
	if c == nil {
		return
	}
	for _, targets := range c.alternateTargets {
		for i, target := range targets {
			prefix, suffix, wildcard := strings.Cut(target, "*")
			var star string
			if wildcard {
				if len(stem) < len(prefix)+len(suffix) || !strings.HasPrefix(stem, prefix) || !strings.HasSuffix(stem, suffix) {
					continue
				}
				star = stem[len(prefix) : len(stem)-len(suffix)]
			} else if target != stem {
				continue
			}
			for j, alt := range targets {
				if j != i {
					visit(strings.Replace(alt, "*", star, 1))
				}
			}
		}
	}
}

// matchAlias reports whether modulePath matches a and, when it does, returns
// the substring captured by the `*` wildcard ("" for an exact pattern).
func matchAlias(a *Alias, modulePath string) (string, bool) {
	if a.HasWildcard {
		if len(modulePath) < len(a.AliasPrefix)+len(a.AliasSuffix) {
			return "", false
		}
		if !strings.HasPrefix(modulePath, a.AliasPrefix) {
			return "", false
		}
		if !strings.HasSuffix(modulePath, a.AliasSuffix) {
			return "", false
		}
		return modulePath[len(a.AliasPrefix) : len(modulePath)-len(a.AliasSuffix)], true
	}
	if modulePath != a.AliasPrefix {
		return "", false
	}
	return "", true
}

// joinTarget resolves a filled-in target against the config's BaseURL and
// DirPrefix, returning a forward-slashed repo-relative path. The result is
// always path-cleaned: a tsconfig written without a baseUrl (legal since
// TS 4.1) keeps its targets verbatim (`"zustand": ["./src/index.ts"]`),
// and an uncleaned `./` prefix would never match a graph file node, so
// every paths-alias import in such a repo silently failed to resolve.
func (m *Map) joinTarget(matched string) string {
	if m.targetBase != "" {
		return path.Clean(path.Join(m.targetBase, filepath.ToSlash(matched)))
	}
	joined := matched
	if m.BaseURL != "" {
		joined = filepath.ToSlash(filepath.Join(m.BaseURL, matched))
	}
	if m.DirPrefix != "" {
		joined = filepath.ToSlash(filepath.Join(m.DirPrefix, joined))
	}
	if joined == "" {
		return ""
	}
	return path.Clean(joined)
}

// probeExts are the source extensions a path-alias target may resolve to,
// matching aliasProbePath's set plus declaration / json / index-file forms.
var probeExts = []string{".ts", ".tsx", ".d.ts", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs", ".json"}

// targetExistsInTree reports whether the repo-relative joined target resolves
// to a real file in the tree exists probes — as an exact path, with any source
// extension, or as an index file in a directory of that name. The probe set and
// its order are the tree's only input, so a snapshot answers the same question
// the working copy does, about its own bytes.
func targetExistsInTree(exists func(rel string) bool, joined string) bool {
	if exists == nil || joined == "" {
		return false
	}
	if exists(joined) {
		return true
	}
	for _, ext := range probeExts {
		if exists(joined + ext) {
			return true
		}
		if exists(path.Join(joined, "index"+ext)) {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func aliasProbePath(p string) string {
	// Keep an explicit declaration file for forward probing; its reverse
	// key, unlike its lookup path, is shared with the implementation.
	if strings.HasSuffix(p, ".d.ts") || strings.HasSuffix(p, ".d.cts") || strings.HasSuffix(p, ".d.mts") {
		return p
	}
	return graph.JSModuleStem(p)
}

// Tree is the repository view a loader reads its configs out of. A working
// copy implements it with the os package; a committed snapshot implements it
// over the content source that serves the tree, so a build that describes a
// commit never has to reach for the checkout to learn its alias scopes.
type Tree interface {
	// Files calls visit with the repo-relative slash path of every file the
	// tree serves as content, in any order. An implementation with real
	// directories may skip descending one SkipDirName names — a saving only:
	// LoadTree applies that rule itself, to every path it is offered. Walk
	// errors are swallowed, not reported: one unreadable corner of a
	// repository must not drop every alias in it.
	Files(visit func(rel string))
	// Read returns the bytes at a repo-relative slash path.
	Read(rel string) ([]byte, bool)
	// Exists reports whether a repo-relative slash path is readable content.
	Exists(rel string) bool
}

// configFileNames are the config files a loader reads.
var configFileNames = [...]string{"tsconfig.json", "jsconfig.json"}

// ConfigFileNames returns the filenames that can introduce an alias scope.
func ConfigFileNames() [len(configFileNames)]string { return configFileNames }

// IsConfigFileName reports whether base — a file's base name — is a config
// file the loaders parse. Exported so a Tree implementation that must decide
// what to feed Files (a snapshot enumerating a whole tree, say) applies the
// loader's own rule rather than a copy of it.
func IsConfigFileName(base string) bool {
	for _, name := range configFileNames {
		if base == name {
			return true
		}
	}
	return false
}

// skipDirs is the small allowlist of directory names no walk descends, which
// keeps the cost bounded on large monorepos.
var skipDirs = map[string]struct{}{
	"node_modules": {},
	".git":         {},
	".hg":          {},
	".svn":         {},
	"vendor":       {},
	"build":        {},
	"dist":         {},
	"target":       {},
	".next":        {},
	".nuxt":        {},
}

// SkipDirName reports whether a directory of this name is skipped by the
// walk. Exported so a Tree implementation can avoid descending it — a saving
// only, never the rule: LoadTree applies the same test to every path it is
// offered, so a Tree that enumerates everything (a snapshot has no directories
// to prune) still yields exactly the scopes the working-copy walk finds.
func SkipDirName(name string) bool {
	_, skip := skipDirs[name]
	return skip
}

// underSkippedDir reports whether any ancestor directory of a repo-relative
// slash path is one the walk does not descend.
func underSkippedDir(rel string) bool {
	for dir := path.Dir(rel); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
		if SkipDirName(path.Base(dir)) {
			return true
		}
	}
	return false
}

// LoadTree reads every tsconfig.json / jsconfig.json the tree holds and
// returns a Collection ready for FindForFile. Returns nil when the tree holds
// no usable config. A malformed config is skipped, not fatal.
//
// This is the whole loader: Load is LoadTree over the working copy, so a
// snapshot and a checkout are parsed by the same code, probe their
// multi-target aliases with the same probe set, and order their scopes the
// same way.
func LoadTree(t Tree) *Collection {
	if t == nil {
		return nil
	}
	var scopes []*Map
	configs := configReader{tree: t, docs: make(map[string]*configDocument)}
	t.Files(func(rel string) {
		if !IsConfigFileName(path.Base(rel)) || underSkippedDir(rel) {
			return
		}
		dirRel := path.Dir(rel)
		if dirRel == "." || dirRel == "/" {
			dirRel = ""
		}
		if m := configs.parseConfig(rel, dirRel); m != nil {
			scopes = append(scopes, m)
		}
	})
	return NewCollection(scopes)
}

// Load walks repoRoot for tsconfig.json / jsconfig.json files and
// returns a Collection ready for FindForFile. Returns nil when the
// walk finds no usable configs. Walk errors on individual files are
// logged-by-skipping — a malformed tsconfig must not stop indexing.
//
// The walk respects a small allowlist of skip-dirs (node_modules, .git,
// vendor, build, dist) to keep cost bounded on large monorepos.
func Load(repoRoot string) *Collection {
	return LoadObserved(repoRoot, nil)
}

// LoadObserved is Load with a callback for each distinct config read,
// including missing relative parents that can later become resolvable.
func LoadObserved(repoRoot string, observe func(rel string, data []byte, found bool)) *Collection {
	if repoRoot == "" {
		return nil
	}
	// Config reads stay inside the root: a relative extends through a
	// symlinked directory must not import another checkout's aliases.
	dir, err := os.OpenRoot(repoRoot)
	if err != nil {
		return nil
	}
	defer dir.Close()
	return LoadTree(dirTree{root: repoRoot, dir: dir, observe: observe})
}

// IsScopeConfig reports whether a repo-relative slash path is a config the
// discovery walk would load as a scope (rather than read only as a parent).
func IsScopeConfig(rel string) bool {
	return IsConfigFileName(path.Base(rel)) && !underSkippedDir(rel)
}

// LoadFiles is LoadObserved over a known scope list instead of a walk. Given
// the scope configs a walk of the same tree visited, it returns the same
// Collection; callers verify that by comparing digests before trusting it.
func LoadFiles(repoRoot string, rels []string, observe func(rel string, data []byte, found bool)) *Collection {
	if repoRoot == "" {
		return nil
	}
	dir, err := os.OpenRoot(repoRoot)
	if err != nil {
		return nil
	}
	defer dir.Close()
	sorted := append([]string(nil), rels...)
	// WalkDir's order: scopes with equally long prefixes keep visit order.
	sort.Slice(sorted, func(i, j int) bool { return walkOrderLess(sorted[i], sorted[j]) })
	return LoadTree(listedTree{dirTree: dirTree{root: repoRoot, dir: dir, observe: observe}, files: sorted})
}

// walkOrderLess orders slash paths the way filepath.WalkDir visits them:
// entries sorted by name within each directory, depth first.
func walkOrderLess(a, b string) bool {
	for {
		ai, bi := strings.IndexByte(a, '/'), strings.IndexByte(b, '/')
		ah, bh := a, b
		if ai >= 0 {
			ah = a[:ai]
		}
		if bi >= 0 {
			bh = b[:bi]
		}
		if ah != bh || ai < 0 || bi < 0 {
			if ah == bh {
				return ai < 0 && bi >= 0 // impossible for two files; keep total order
			}
			return ah < bh
		}
		a, b = a[ai+1:], b[bi+1:]
	}
}

// listedTree is a working copy whose scope configs are already known.
type listedTree struct {
	dirTree
	files []string
}

func (t listedTree) Files(visit func(rel string)) {
	for _, rel := range t.files {
		visit(rel)
	}
}

// WorkingTree visits config candidates plus an optional file in a pruned
// directory. The optional extra has exactly the form "directory/file".
func WorkingTree(root, extra string, scopeFiles ...string) Tree {
	dir, file := path.Split(extra)
	return dirTree{root: root, extraDir: strings.TrimSuffix(dir, "/"), extraFile: file, scopeFiles: scopeFiles}
}

// dirTree is the working copy at a root: the tree Load reads.
type dirTree struct {
	root                string
	dir                 *os.Root // shared config reads during LoadObserved
	observe             func(string, []byte, bool)
	extraDir, extraFile string
	scopeFiles          []string
}

var _ Tree = dirTree{}

// Files walks the working copy, skipping a directory the walk does not
// descend and swallowing per-entry errors, which is what keeps one
// unreadable corner of a repository from dropping every alias in it.
func (t dirTree) Files(visit func(rel string)) {
	_ = filepath.WalkDir(t.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return nil
		}
		if d.IsDir() {
			if p != t.root && SkipDirName(d.Name()) {
				if d.Name() == t.extraDir {
					if rel, err := filepath.Rel(t.root, p); err == nil {
						visit(path.Join(filepath.ToSlash(rel), t.extraFile))
					}
				}
				return filepath.SkipDir
			}
			return nil
		}
		if !IsConfigFileName(d.Name()) && !slices.Contains(t.scopeFiles, d.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(t.root, p)
		if relErr != nil {
			return nil
		}
		visit(filepath.ToSlash(rel))
		return nil
	})
}

func (t dirTree) Read(rel string) ([]byte, bool) {
	dir := t.dir
	if dir == nil {
		var err error
		if dir, err = os.OpenRoot(t.root); err != nil {
			return nil, false
		}
		defer dir.Close()
	}
	data, err := dir.ReadFile(filepath.FromSlash(rel))
	if t.observe != nil {
		t.observe(rel, data, err == nil)
	}
	if err != nil {
		return nil, false
	}
	return data, true
}

func (t dirTree) Exists(rel string) bool {
	return fileExists(filepath.Join(t.root, filepath.FromSlash(rel)))
}

// parseConfig resolves inherited options and probes targets in the same tree.
func (r *configReader) parseConfig(rel, dirPrefix string) *Map {
	var co configOptions
	r.inherit(rel, 0, make(map[string]int), &co)
	if co.baseURL == "" && co.paths == nil {
		return nil
	}
	m := &Map{DirPrefix: dirPrefix, targetBase: co.pathsDir, exists: r.tree.Exists}
	if co.baseURL != "" {
		m.targetBase = co.baseURL
		baseURL, relErr := filepath.Rel(filepath.FromSlash(path.Dir(rel)), filepath.FromSlash(co.baseURL))
		if relErr != nil {
			return nil
		}
		m.BaseURL = filepath.ToSlash(baseURL)
	}
	for pattern, targets := range co.paths {
		if len(targets) == 0 {
			continue
		}
		entry, ok := splitAlias(pattern, targets)
		if !ok {
			continue
		}
		m.Entries = append(m.Entries, entry)
	}
	sort.SliceStable(m.Entries, func(i, j int) bool {
		if len(m.Entries[i].AliasPrefix) != len(m.Entries[j].AliasPrefix) {
			return len(m.Entries[i].AliasPrefix) > len(m.Entries[j].AliasPrefix)
		}
		if m.Entries[i].AliasPrefix != m.Entries[j].AliasPrefix {
			return m.Entries[i].AliasPrefix < m.Entries[j].AliasPrefix
		}
		if m.Entries[i].HasWildcard != m.Entries[j].HasWildcard {
			return !m.Entries[i].HasWildcard
		}
		return m.Entries[i].AliasSuffix < m.Entries[j].AliasSuffix
	})
	return m
}

type configOptions struct {
	baseURL  string
	paths    map[string][]string
	pathsDir string
}

type configDocument struct {
	Extends         json.RawMessage `json:"extends"`
	CompilerOptions struct {
		BaseURL *string             `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
	} `json:"compilerOptions"`
}

type configReader struct {
	tree Tree
	docs map[string]*configDocument // nil for a missing file
}

func (r *configReader) read(rel string) *configDocument {
	if doc, known := r.docs[rel]; known {
		return doc
	}
	var doc *configDocument
	if data, ok := r.tree.Read(rel); ok {
		doc = new(configDocument)
		if json.Unmarshal(stripJSONC(data), doc) != nil {
			// Present but unusable: don't try a .json sibling.
			doc = &configDocument{}
		}
	}
	r.docs[rel] = doc
	return doc
}

// Visit in precedence order: the child first, then parents right to left.
// A shallower revisit can reach ancestors cut off by the depth limit.
func (r *configReader) inherit(rel string, depth int, visited map[string]int, effective *configOptions) {
	if prev, seen := visited[rel]; depth >= 32 || seen && prev <= depth {
		return
	}
	visited[rel] = depth
	raw := r.read(rel)
	if raw == nil {
		return
	}
	if effective.baseURL == "" && raw.CompilerOptions.BaseURL != nil {
		effective.baseURL = path.Join(path.Dir(rel), filepath.ToSlash(*raw.CompilerOptions.BaseURL))
	}
	if effective.paths == nil && raw.CompilerOptions.Paths != nil {
		effective.paths = raw.CompilerOptions.Paths
		effective.pathsDir = path.Dir(rel)
	}
	if effective.baseURL != "" && effective.paths != nil || len(raw.Extends) == 0 || depth == 31 {
		return
	}
	var parents []string
	var parent string
	if json.Unmarshal(raw.Extends, &parent) == nil {
		parents = []string{parent}
	} else {
		_ = json.Unmarshal(raw.Extends, &parents)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		parent := parents[i]
		// TS package config resolution is intentionally outside this reader.
		if !strings.HasPrefix(parent, "./") && !strings.HasPrefix(parent, "../") &&
			!strings.HasPrefix(parent, `.\`) && !strings.HasPrefix(parent, `..\`) {
			continue
		}
		parent = strings.ReplaceAll(parent, `\`, "/")
		parent = path.Join(path.Dir(rel), parent)
		if !filepath.IsLocal(filepath.FromSlash(parent)) {
			continue
		}
		if !strings.HasSuffix(parent, ".json") && r.read(parent) == nil {
			parent += ".json"
		}
		r.inherit(parent, depth+1, visited, effective)
	}
}

// splitAlias splits one `paths` entry (pattern → ordered targets) into an
// Alias. Every target is split around its `*`; the first becomes the primary
// TargetPrefix/TargetSuffix and all of them populate Targets for disk-probing.
// Targets whose wildcard arity disagrees with the pattern (tsserver rejects
// these) are skipped; the entry is dropped only when none remain.
func splitAlias(pattern string, targets []string) (Alias, bool) {
	pStar := strings.Index(pattern, "*")
	a := Alias{HasWildcard: pStar != -1}
	if a.HasWildcard {
		a.AliasPrefix = pattern[:pStar]
		a.AliasSuffix = pattern[pStar+1:]
	} else {
		a.AliasPrefix = pattern
	}
	for _, target := range targets {
		tStar := strings.Index(target, "*")
		if (pStar == -1) != (tStar == -1) {
			// Mismatched wildcard arity between pattern and this target.
			continue
		}
		var t AliasTarget
		if tStar == -1 {
			t.Prefix = target
		} else {
			t.Prefix = target[:tStar]
			t.Suffix = target[tStar+1:]
		}
		a.Targets = append(a.Targets, t)
	}
	if len(a.Targets) == 0 {
		return Alias{}, false
	}
	a.TargetPrefix = a.Targets[0].Prefix
	a.TargetSuffix = a.Targets[0].Suffix
	return a, true
}

// stripJSONC removes // line comments, /* */ block comments, and trailing
// commas from a JSON-with-comments byte slice while leaving the contents of
// string literals (including any comment-like or comma sequences inside them)
// untouched, so a commented tsconfig parses as JSON instead of failing wholesale.
func stripJSONC(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(data) {
			switch data[i+1] {
			case '/':
				i += 2
				for i < len(data) && data[i] != '\n' {
					i++
				}
				if i < len(data) {
					out = append(out, '\n')
				}
				continue
			case '*':
				i += 2
				for i < len(data) {
					if data[i] == '*' && i+1 < len(data) && data[i+1] == '/' {
						i++
						break
					}
					i++
				}
				continue
			}
		}
		out = append(out, c)
	}
	return stripTrailingCommas(out)
}

// stripTrailingCommas drops any comma whose next non-whitespace byte is a } or
// ] closer, outside of string literals.
func stripTrailingCommas(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}
