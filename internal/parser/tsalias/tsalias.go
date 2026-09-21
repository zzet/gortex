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
//   - Targets are joined with `baseUrl` (if set) and returned without
//     the trailing `.ts/.tsx/.js/.jsx/.mts/.cts` extension — callers
//     reuse the same probing logic as relative imports.
//   - Multi-target arrays (`"@/*": ["a/*", "b/*"]`) are resolved by disk
//     existence: each candidate is probed under the repo root in priority
//     order and the first that exists on disk wins, falling back to the
//     first entry (the documented "primary" path) when none do.
//
// tsconfig JSONC features the TypeScript tooling itself accepts — `//` and
// `/* */` comments and trailing commas — are stripped before parsing, so a
// commented config does not silently drop every alias for the repo. The
// package still does not follow `extends:` chains or monorepo `references[]`
// traversal; those can be layered on without touching the resolver API.
package tsalias

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
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
	scopes []*Map
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
	return &Collection{scopes: scopes}
}

// Maps returns the underlying scope slice. Test-visibility only.
func (c *Collection) Maps() []*Map { return c.scopes }

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
// repo-relative target (extension stripped) or "" when no entry
// matches. The returned path is forward-slashed and rooted at the
// repository root.
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
			stripped := stripExt(joined)
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
// matching stripExt's set plus declaration / json / index-file forms.
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

func stripExt(p string) string {
	switch ext := filepath.Ext(p); ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		return strings.TrimSuffix(p, ext)
	}
	return p
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
	t.Files(func(rel string) {
		if !IsConfigFileName(path.Base(rel)) || underSkippedDir(rel) {
			return
		}
		data, ok := t.Read(rel)
		if !ok {
			return
		}
		dirRel := path.Dir(rel)
		if dirRel == "." || dirRel == "/" {
			dirRel = ""
		}
		if m := parseConfig(data, dirRel, t.Exists); m != nil {
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
	if repoRoot == "" {
		return nil
	}
	return LoadTree(dirTree{root: repoRoot})
}

// dirTree is the working copy at a root: the tree Load reads.
type dirTree struct{ root string }

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
			if SkipDirName(d.Name()) {
				return filepath.SkipDir
			}
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
	data, err := os.ReadFile(filepath.Join(t.root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, false
	}
	return data, true
}

func (t dirTree) Exists(rel string) bool {
	return fileExists(filepath.Join(t.root, filepath.FromSlash(rel)))
}

// parseConfig parses one config file's bytes into a Map rooted at dirPrefix,
// with exists as the probe its multi-target aliases are grounded in.
func parseConfig(data []byte, dirPrefix string, exists func(rel string) bool) *Map {
	var raw struct {
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	// tsconfig files routinely use the JSONC features tsserver itself
	// accepts — // and /* */ comments and trailing commas. A strict JSON
	// parse fails the WHOLE file on any of them, silently dropping every
	// alias for the repo, so strip JSONC first.
	if err := json.Unmarshal(stripJSONC(data), &raw); err != nil {
		return nil
	}
	co := raw.CompilerOptions
	if co.BaseURL == "" && len(co.Paths) == 0 {
		return nil
	}
	m := &Map{
		BaseURL:   filepath.ToSlash(strings.TrimSpace(co.BaseURL)),
		DirPrefix: dirPrefix,
		exists:    exists,
	}
	for pattern, targets := range co.Paths {
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
		return len(m.Entries[i].AliasPrefix) > len(m.Entries[j].AliasPrefix)
	})
	return m
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
