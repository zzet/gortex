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
// commented config does not silently drop every alias for the repo. Relative
// `extends` chains (including arrays) are followed with TypeScript option
// precedence; package specifiers and project references are not traversed.
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
	// targetBase is the repo-relative directory against which effective paths
	// resolve. Unlike DirPrefix (the child scope), it may belong to a parent
	// config. "." denotes the root; empty preserves hand-built Map semantics.
	targetBase string
	// repoRoot is the absolute repository root, set by Load, used to
	// disk-probe multi-target aliases. Empty for hand-built maps, which
	// then resolve by first-match without touching the filesystem.
	repoRoot string
}

// Collection aggregates every alias map found by Load, sorted by
// DirPrefix length descending so nearest-ancestor lookup is a single
// linear scan.
type Collection struct {
	scopes []*Map
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
			// Disk-grounded multi-target: return the first candidate that
			// actually exists. Skipped for hand-built maps (no repoRoot),
			// which fall through to the documented primary path.
			if m.repoRoot != "" && targetExistsOnDisk(m.repoRoot, joined) {
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
// matching stripExt's set plus declaration / json / index-file forms.
var probeExts = []string{".ts", ".tsx", ".d.ts", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs", ".json"}

// targetExistsOnDisk reports whether the repo-relative joined target resolves
// to a real file under repoRoot — as an exact path, with any source extension,
// or as an index file in a directory of that name.
func targetExistsOnDisk(repoRoot, joined string) bool {
	base := filepath.Join(repoRoot, filepath.FromSlash(joined))
	if fileExists(base) {
		return true
	}
	for _, ext := range probeExts {
		if fileExists(base + ext) {
			return true
		}
		if fileExists(filepath.Join(base, "index"+ext)) {
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
	var scopes []*Map
	skipDirs := map[string]struct{}{
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

	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
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
			if _, skip := skipDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name != "tsconfig.json" && name != "jsconfig.json" {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, p)
		if relErr != nil {
			return nil
		}
		dirRel := filepath.ToSlash(filepath.Dir(rel))
		if dirRel == "." {
			dirRel = ""
		}
		if m := parseConfigFile(p, dirRel, repoRoot); m != nil {
			scopes = append(scopes, m)
		}
		return nil
	})
	if err != nil || len(scopes) == 0 {
		return nil
	}
	sort.SliceStable(scopes, func(i, j int) bool {
		return len(scopes[i].DirPrefix) > len(scopes[j].DirPrefix)
	})
	return &Collection{scopes: scopes}
}

func parseConfigFile(absPath, dirPrefix, repoRoot string) *Map {
	co := loadConfigOptions(absPath, make(map[string]bool), 0)
	if co.baseURL == "" && co.paths == nil {
		return nil
	}
	targetDir := co.pathsDir
	if co.baseURL != "" {
		targetDir = co.baseURL
	}
	targetBase, err := filepath.Rel(repoRoot, targetDir)
	if err != nil {
		return nil
	}
	m := &Map{
		DirPrefix:  dirPrefix,
		targetBase: filepath.ToSlash(targetBase),
		repoRoot:   repoRoot,
	}
	if co.baseURL != "" {
		baseURL, relErr := filepath.Rel(filepath.Dir(absPath), co.baseURL)
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
		return m.Entries[i].AliasSuffix < m.Entries[j].AliasSuffix
	})
	return m
}

type configOptions struct {
	baseURL  string
	paths    map[string][]string
	pathsDir string
}

// Each option retains its declaring directory. Inherited paths without a
// baseUrl must not become relative to the child config's directory.
func loadConfigOptions(absPath string, active map[string]bool, depth int) configOptions {
	const maxExtendsDepth = 32
	absPath = filepath.Clean(absPath)
	if depth >= maxExtendsDepth || active[absPath] {
		return configOptions{}
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return configOptions{}
	}
	var raw struct {
		Extends         json.RawMessage `json:"extends"`
		CompilerOptions struct {
			BaseURL *string             `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(stripJSONC(data), &raw); err != nil {
		return configOptions{}
	}
	active[absPath] = true
	defer delete(active, absPath)
	var parents []string
	var parent string
	if json.Unmarshal(raw.Extends, &parent) == nil {
		parents = []string{parent}
	} else {
		_ = json.Unmarshal(raw.Extends, &parents)
	}
	var effective configOptions
	for _, parent := range parents {
		// TS package config resolution is intentionally outside this reader.
		if !strings.HasPrefix(parent, "./") && !strings.HasPrefix(parent, "../") &&
			!strings.HasPrefix(parent, `.\`) && !strings.HasPrefix(parent, `..\`) {
			continue
		}
		if filepath.Ext(parent) == "" {
			parent += ".json"
		}
		inherited := loadConfigOptions(filepath.Join(filepath.Dir(absPath), parent), active, depth+1)
		if inherited.baseURL != "" {
			effective.baseURL = inherited.baseURL
		}
		if inherited.paths != nil {
			effective.paths = inherited.paths
			effective.pathsDir = inherited.pathsDir
		}
	}
	if raw.CompilerOptions.BaseURL != nil {
		effective.baseURL = filepath.Join(filepath.Dir(absPath), *raw.CompilerOptions.BaseURL)
	}
	if raw.CompilerOptions.Paths != nil {
		effective.paths = raw.CompilerOptions.Paths
		effective.pathsDir = filepath.Dir(absPath)
	}
	return effective
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
