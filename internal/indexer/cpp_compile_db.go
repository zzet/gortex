package indexer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// cppTU is one C/C++ translation unit's reconstructed include search path,
// keyed in the compile-DB result by the TU's repo-relative source path.
type cppTU struct {
	file        string   // repo-relative source path
	includeDirs []string // repo-relative -I / -isystem / -iquote dirs, in order
}

// compileCommand is one entry of a compile_commands.json database.
type compileCommand struct {
	Directory string   `json:"directory"`
	File      string   `json:"file"`
	Command   string   `json:"command"`
	Arguments []string `json:"arguments"`
}

// cppIncludeDirCache is the long-lived, memory-bounded LRU of per-repo-root
// compile-DB include-dir sets (bounded by GORTEX_RESOLVER_CACHE_MAX_MB).
var cppIncludeDirCache = newCppIncludeDirCache()

// loadCompileCommands parses compile_commands.json at the repo root (and any
// build*/compile_commands.json) out of tree, reconstructing each TU's ordered
// include search path (the `-I` / `-isystem` / `-iquote` dirs) normalized to
// repo-relative slash paths, dropping directories outside the repo (toolchain
// / system).
//
// A working-copy tree caches its result per repo root, keyed on the newest
// compile_commands.json modtime: a later edit to compile_commands.json (even
// with no other source change) is re-read on the next load, and
// clearCppIncludeDirCache invalidates it on a full reindex. A source-backed
// tree does not use that cache at all — the cache is keyed by root, which
// cannot tell two snapshots of one checkout apart, and a build's snapshot is
// read exactly once by the private indexer that owns it.
func loadCompileCommands(tree manifestTree) map[string]cppTU {
	repoRoot := tree.root()
	if repoRoot == "" {
		return nil
	}
	cacheable := !tree.sourced()
	var mtime int64
	if cacheable {
		mtime = compileDBMtime(repoRoot)
		if c, ok := cppIncludeDirCache.get(repoRoot, mtime); ok {
			return c
		}
	}

	out := map[string]cppTU{}
	for _, rel := range compileDBLocations(tree) {
		data, ok := tree.readFile(rel)
		if !ok {
			continue
		}
		var cmds []compileCommand
		if json.Unmarshal(data, &cmds) != nil {
			continue
		}
		for _, cc := range cmds {
			fileRel := repoRelPath(repoRoot, cc.Directory, cc.File)
			if fileRel == "" {
				continue
			}
			out[fileRel] = cppTU{file: fileRel, includeDirs: extractIncludeDirs(cc, repoRoot)}
		}
	}

	if cacheable {
		cppIncludeDirCache.put(repoRoot, out, mtime)
	}
	return out
}

// compileDBMtime returns the newest modtime (unix nanoseconds) across the
// compile_commands.json files that loadCompileCommands reads for repoRoot, or 0
// when none exist. It is the cache freshness key: when this exceeds the cached
// entry's recorded mtime, the entry is reloaded.
//
// It reads the working copy, and only the working-copy tree consults it: a
// snapshot has no modtime axis, so a source-backed load neither asks for this
// nor caches an answer keyed by it.
func compileDBMtime(repoRoot string) int64 {
	var newest int64
	tree := newDiskManifestTree(repoRoot)
	for _, rel := range compileDBLocations(tree) {
		fi, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		if m := fi.ModTime().UnixNano(); m > newest {
			newest = m
		}
	}
	return newest
}

// clearCppIncludeDirCache drops the cached include-dir set for a repo root so
// the next index re-reads compile_commands.json.
func clearCppIncludeDirCache(repoRoot string) {
	cppIncludeDirCache.clear(repoRoot)
}

// compileDBLocations returns the repo-relative slash paths of the
// compile_commands.json files to consider: the repo root plus any
// build*/compile_commands.json. The order is the precedence the caller
// applies — a root database is read first and a build directory overrides it.
func compileDBLocations(tree manifestTree) []string {
	var out []string
	if tree.isFile(compileDBName) {
		out = append(out, compileDBName)
	}
	return append(out, tree.matchFiles("build*/"+compileDBName)...)
}

// compileDBName is the compile database's file name, at the repo root and
// inside a build directory alike.
const compileDBName = "compile_commands.json"

// extractIncludeDirs reconstructs the ordered include search path from a
// compile command, preferring the structured arguments array and falling back
// to a shlex split of the command string.
func extractIncludeDirs(cc compileCommand, repoRoot string) []string {
	toks := cc.Arguments
	if len(toks) == 0 && cc.Command != "" {
		toks = shlexSplit(cc.Command)
	}
	var dirs []string
	seen := map[string]bool{}
	add := func(raw string) {
		if rel := repoRelPath(repoRoot, cc.Directory, raw); rel != "" && !seen[rel] {
			seen[rel] = true
			dirs = append(dirs, rel)
		}
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case strings.HasPrefix(t, "-I") && len(t) > 2:
			add(t[2:])
		case t == "-I" && i+1 < len(toks):
			i++
			add(toks[i])
		case (t == "-isystem" || t == "-iquote" || t == "-idirafter") && i+1 < len(toks):
			i++
			add(toks[i])
		}
	}
	return dirs
}

// cppHeaderExts are the header extensions the include-root heuristic looks
// for when it decides a top-level directory is worth probing.
var cppHeaderExts = []string{".h", ".hpp", ".hh", ".hxx", ".h++"}

// conventionalCppIncludeRoots are the include roots a C/C++ repo with no
// compile database is probed for, in the priority order the resolver's ordered
// -I probe consumes them.
var conventionalCppIncludeRoots = []string{"include", "src", "inc", "api", "lib"}

// heuristicIncludeDirs returns the conventional C/C++ include-root search path
// for a repo that has no compile_commands.json: the conventional roots
// (include / src / inc / api / lib) that actually exist, in priority order,
// followed by any other top-level directory that directly contains a C/C++
// header. Paths are repo-relative slash paths. Feeds the resolver's ordered
// include probe so collisions still break deterministically without a DB.
//
// The directory layout is read out of tree, so a generation built from a
// committed snapshot probes the include roots that snapshot has rather than
// whatever directories the checkout currently holds.
//
// The two clauses probe differently, and did so before the tree existed. A
// conventional root is whatever the NAME resolves to, so a symlinked include/
// is an include root; the "any other directory holding a header" clause reads
// a directory listing, which calls a symlink a symlink. Collapsing them onto
// one probe would either drop a symlinked root from the live index's ordered
// -I set — leaving a quoted include to fall through to the suffix-unique
// fallback and bind differently or refuse — or widen the second clause.
func heuristicIncludeDirs(tree manifestTree) []string {
	if tree.root() == "" {
		return nil
	}
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	// One enumeration answers most of both clauses: which conventional roots
	// exist, and which other top-level directory directly holds a header.
	// Asking them separately would cost a source-backed tree one snapshot walk
	// per question.
	topLevel := tree.topLevelDirs(cppHeaderExts...)
	for _, name := range conventionalCppIncludeRoots {
		if _, present := topLevel[name]; present {
			add(name)
			continue
		}
		// A name the listing did not report may still resolve to a directory:
		// a symlinked root, or a root whose parent could not be listed at all.
		// Only the working copy needs the second probe — for a snapshot the
		// enumeration IS complete (a source holds no directory entries, so a
		// directory exists exactly when something in it does) and the extra
		// question would cost a walk per absent root.
		if !tree.sourced() && tree.isDir(name) {
			add(name)
		}
	}
	withHeaders := make([]string, 0, len(topLevel))
	for name, holdsHeader := range topLevel {
		if holdsHeader {
			withHeaders = append(withHeaders, name)
		}
	}
	sort.Strings(withHeaders)
	for _, name := range withHeaders {
		add(name)
	}
	return dirs
}

// repoRelPath resolves p (relative to dir, or repoRoot when dir is empty)
// against the repo root, returning a clean repo-relative slash path — or ""
// when p resolves outside the repo (a toolchain / system path) or to the root
// itself.
func repoRelPath(repoRoot, dir, p string) string {
	if p == "" {
		return ""
	}
	abs := p
	if !filepath.IsAbs(abs) {
		base := dir
		if base == "" {
			base = repoRoot
		}
		abs = filepath.Join(base, p)
	}
	rel, err := filepath.Rel(repoRoot, filepath.Clean(abs))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}
