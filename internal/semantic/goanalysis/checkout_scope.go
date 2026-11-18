package goanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// Handle-rooted compiler loads for checkout passes.
//
// A checkout pass enriches a generation handle: every fact the provider
// writes is keyed to a node of a file the handle carries. A package whose
// files the handle does not carry is type-checked by a whole-module load and
// then thrown away. Loading only the packages of the handle's Go files (plus
// dependencies from export data) therefore writes the same output while doing
// work proportional to the edit rather than to the module.

// handleGoFiles returns the graph paths of the Go source files the handle's
// Go node projection names.
func handleGoFiles(nodes []*graph.Node) map[string]struct{} {
	files := make(map[string]struct{})
	for _, node := range nodes {
		if node == nil || node.FilePath == "" {
			continue
		}
		p := normalizeRelPath(node.FilePath)
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		files[p] = struct{}{}
	}
	return files
}

// handleRootPlan is the root selection for one checkout pass.
type handleRootPlan struct {
	// full reports that a whole-module load is required; reason says why.
	full   bool
	reason string
	// patterns are the go/packages directory patterns ("./dir", or "." for
	// the module directory), sorted.
	patterns []string
	// rootDirs are the absolute directories of patterns.
	rootDirs map[string]struct{}
	// handleAbs are the absolute paths of the handle's loadable Go files.
	handleAbs map[string]struct{}
}

func (p handleRootPlan) empty() bool { return !p.full && len(p.patterns) == 0 }

// goManifestPaths are the files whose content decides the module's build list.
var goManifestPaths = []string{"go.mod", "go.sum", "go.work", "go.work.sum", "vendor/modules.txt"}

// goToolIgnoredSegment reports whether a path segment makes "./..." skip the
// directory (or file) it names.
func goToolIgnoredSegment(segment string) bool {
	return segment == "testdata" || segment == "vendor" ||
		strings.HasPrefix(segment, "_") || strings.HasPrefix(segment, ".")
}

// planHandleRoots maps the handle's Go files onto directory patterns relative
// to loadDir. Files "./..." never loads (testdata, vendor, "_"/"." prefixed
// segments, files under a nested module) are excluded without a fallback.
func planHandleRoots(absRoot, loadDir, repoPrefix string, files map[string]struct{}, scope semantic.CheckoutCompilerScope) handleRootPlan {
	if scope.ManifestChanged {
		return handleRootPlan{full: true, reason: semantic.CompilerScopeReasonManifestChanged}
	}
	if filepath.Clean(loadDir) != filepath.Clean(absRoot) || fileExists(filepath.Join(loadDir, "go.work")) {
		return handleRootPlan{full: true, reason: semantic.CompilerScopeReasonMultiModule}
	}
	prefix := normalizeRelPath(repoPrefix)
	nestedModule := map[string]bool{}
	underNestedModule := func(dir string) bool {
		for d := dir; d != "." && d != "/" && d != ""; d = path.Dir(d) {
			nested, seen := nestedModule[d]
			if !seen {
				nested = fileExists(filepath.Join(loadDir, filepath.FromSlash(d), "go.mod"))
				nestedModule[d] = nested
			}
			if nested {
				return true
			}
		}
		return false
	}
	plan := handleRootPlan{rootDirs: map[string]struct{}{}, handleAbs: map[string]struct{}{}}
	dirs := map[string]struct{}{}
	for graphPath := range files {
		rel := graphPath
		if prefix != "" {
			if !strings.HasPrefix(graphPath, prefix+"/") {
				return handleRootPlan{full: true, reason: semantic.CompilerScopeReasonPathUnmappable}
			}
			rel = strings.TrimPrefix(graphPath, prefix+"/")
		}
		if rel == "" || path.IsAbs(rel) || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return handleRootPlan{full: true, reason: semantic.CompilerScopeReasonPathUnmappable}
		}
		rel = path.Clean(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			return handleRootPlan{full: true, reason: semantic.CompilerScopeReasonPathUnmappable}
		}
		ignored := false
		for _, segment := range strings.Split(rel, "/") {
			if goToolIgnoredSegment(segment) {
				ignored = true
				break
			}
		}
		dir := path.Dir(rel)
		if ignored || underNestedModule(dir) {
			continue
		}
		dirs[dir] = struct{}{}
		plan.handleAbs[filepath.Join(loadDir, filepath.FromSlash(rel))] = struct{}{}
	}
	for dir := range dirs {
		plan.addDir(loadDir, dir)
	}
	sort.Strings(plan.patterns)
	return plan
}

func (p *handleRootPlan) addDir(loadDir, dir string) {
	abs := filepath.Clean(filepath.Join(loadDir, filepath.FromSlash(dir)))
	if _, dup := p.rootDirs[abs]; dup {
		return
	}
	p.rootDirs[abs] = struct{}{}
	if dir == "." {
		p.patterns = append(p.patterns, ".")
		return
	}
	p.patterns = append(p.patterns, "./"+dir)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// missingRootDirs returns the root directories no loaded package came from.
// A pattern whose directory holds no buildable file still yields a package
// (carrying the error), so a missing directory means the load did not cover
// the root at all.
func missingRootDirs(roots map[string]struct{}, raw []*packages.Package) []string {
	seen := make(map[string]struct{}, len(raw))
	for _, pkg := range raw {
		if pkg == nil || pkg.Dir == "" {
			continue
		}
		dir := filepath.Clean(pkg.Dir)
		seen[dir] = struct{}{}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			seen[resolved] = struct{}{}
		}
	}
	var missing []string
	for dir := range roots {
		if _, ok := seen[dir]; ok {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			if _, ok := seen[resolved]; ok {
				continue
			}
		}
		missing = append(missing, dir)
	}
	sort.Strings(missing)
	return missing
}

// checkoutScopeCache is what the provider keeps per module directory between
// checkout passes: the dependency metadata index (import path -> package with
// Name/PkgPath/Module only) extended lazily as loads need new paths, and the
// set of source files carrying a hand-written line directive. Both are keyed
// by the digest of the module manifests; a digest change starts over.
type checkoutScopeCache struct {
	digest string
	// index is copy-on-write: a pass reads the map it was handed without a
	// lock, and an extension installs a new map.
	index map[string]*packages.Package
	// lineFiles maps a module-relative directory to the files in it with a
	// hand-written //line or /*line directive. nil until first scanned.
	lineFiles map[string]map[string]struct{}
}

// goManifestDigest hashes the manifests that decide the module's build list.
func goManifestDigest(loadDir string) string {
	h := sha256.New()
	for _, name := range goManifestPaths {
		h.Write([]byte(name))
		data, err := os.ReadFile(filepath.Join(loadDir, filepath.FromSlash(name)))
		if err != nil {
			h.Write([]byte{0})
			continue
		}
		h.Write([]byte{1})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// checkoutCache returns (creating or resetting) the cache entry for loadDir.
// The caller holds p.scopeMu.
func (p *Provider) checkoutCacheLocked(loadDir, digest string) *checkoutScopeCache {
	if p.scopeCache == nil {
		p.scopeCache = make(map[string]*checkoutScopeCache)
	}
	entry := p.scopeCache[loadDir]
	if entry == nil || entry.digest != digest {
		entry = &checkoutScopeCache{digest: digest, index: map[string]*packages.Package{}}
		p.scopeCache[loadDir] = entry
	}
	return entry
}

// lineDirectivePattern matches the two line-directive forms the compiler
// honours: "//line file:line[:col]" at the start of a line, and
// "/*line file:line[:col]*/" anywhere. A directive-shaped string literal still
// matches; a false positive only adds a root.
var lineDirectivePattern = regexp.MustCompile(`(?m)^//line [^\n]*:[0-9]+[ \t]*$|/\*line [^\n*]*:[0-9]+(:[0-9]+)?\*/`)

// hasLineDirective reports whether src may carry a hand-written line
// directive. The cheap substring probe keeps the regular expression off
// almost every file.
func hasLineDirective(src []byte) bool {
	if !bytes.Contains(src, []byte("line ")) {
		return false
	}
	if !bytes.Contains(src, []byte("//line ")) && !bytes.Contains(src, []byte("/*line ")) {
		return false
	}
	return lineDirectivePattern.Match(src)
}

// lineDirectiveDirs returns the module-relative directories holding a source
// file with a hand-written line directive. The first call per manifest digest
// walks the module (skipping what "./..." skips); later calls refresh only the
// handle's files, which are exactly the files that changed since the parent.
func (p *Provider) lineDirectiveDirs(loadDir, digest string, handleAbs map[string]struct{}) []string {
	p.scopeMu.Lock()
	defer p.scopeMu.Unlock()
	entry := p.checkoutCacheLocked(loadDir, digest)
	if entry.lineFiles == nil {
		entry.lineFiles = p.scanLineDirectives(loadDir)
	} else {
		for abs := range handleAbs {
			rel, err := filepath.Rel(loadDir, abs)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			dir := path.Dir(rel)
			data, readErr := os.ReadFile(abs)
			if readErr == nil && hasLineDirective(data) {
				if entry.lineFiles[dir] == nil {
					entry.lineFiles[dir] = map[string]struct{}{}
				}
				entry.lineFiles[dir][rel] = struct{}{}
				continue
			}
			if files := entry.lineFiles[dir]; files != nil {
				delete(files, rel)
				if len(files) == 0 {
					delete(entry.lineFiles, dir)
				}
			}
		}
	}
	out := make([]string, 0, len(entry.lineFiles))
	for dir := range entry.lineFiles {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

func (p *Provider) scanLineDirectives(loadDir string) map[string]map[string]struct{} {
	found := map[string]map[string]struct{}{}
	_ = filepath.WalkDir(loadDir, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if abs == loadDir {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if goToolIgnoredSegment(name) || fileExists(filepath.Join(abs, "go.mod")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || goToolIgnoredSegment(name) {
			return nil
		}
		if !p.includeTest && strings.HasSuffix(name, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(abs)
		if readErr != nil || !hasLineDirective(data) {
			return nil
		}
		rel, relErr := filepath.Rel(loadDir, abs)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		dir := path.Dir(rel)
		if found[dir] == nil {
			found[dir] = map[string]struct{}{}
		}
		found[dir][rel] = struct{}{}
		return nil
	})
	return found
}

// scopedDepIndex returns a dependency metadata index that classifies every
// object the loaded root packages use. It serves the cached index when that
// already covers every used package path, and otherwise loads the missing
// paths' closure (one metadata `go list`) and installs the extended index.
//
// closure, when non-nil, is a listed dependency closure (the checkout's
// retained compiler state) that already answers the missing paths; the
// extension then takes their entries from it instead of running `go list`.
func (p *Provider) scopedDepIndex(ctx context.Context, loadDir, digest string, pkgs []*packages.Package, closure map[string]*packages.Package) (index map[string]*packages.Package, cached bool, elapsed time.Duration, err error) {
	start := time.Now()
	roots := make(map[string]struct{}, len(pkgs))
	for _, pkg := range pkgs {
		if pkg != nil && pkg.PkgPath != "" {
			roots[pkg.PkgPath] = struct{}{}
		}
	}
	p.scopeMu.Lock()
	entry := p.checkoutCacheLocked(loadDir, digest)
	current := entry.index
	p.scopeMu.Unlock()

	missingSet := map[string]struct{}{}
	for _, pkg := range pkgs {
		if pkg == nil || pkg.TypesInfo == nil {
			continue
		}
		for _, obj := range pkg.TypesInfo.Uses {
			if obj == nil || obj.Pkg() == nil {
				continue
			}
			importPath := obj.Pkg().Path()
			if importPath == "" {
				continue
			}
			if _, root := roots[importPath]; root {
				continue
			}
			if _, ok := current[importPath]; ok {
				continue
			}
			missingSet[importPath] = struct{}{}
		}
	}
	if len(missingSet) == 0 {
		return current, true, time.Since(start), nil
	}
	missing := make([]string, 0, len(missingSet))
	for importPath := range missingSet {
		missing = append(missing, importPath)
	}
	sort.Strings(missing)
	var loaded []*packages.Package
	fromClosure := closure != nil
	for _, importPath := range missing {
		pkg := closure[importPath]
		if pkg == nil {
			fromClosure = false
			break
		}
		loaded = append(loaded, pkg)
	}
	if !fromClosure {
		cfg := &packages.Config{
			Context: ctx,
			Mode:    packages.NeedName | packages.NeedImports | packages.NeedDeps | packages.NeedModule,
			Dir:     loadDir,
			Tests:   p.includeTest,
		}
		load := p.packagesLoad
		if load == nil {
			load = packages.Load
		}
		loaded, err = load(cfg, missing...)
		if err != nil {
			return current, false, time.Since(start), err
		}
	}
	p.scopeMu.Lock()
	defer p.scopeMu.Unlock()
	entry = p.checkoutCacheLocked(loadDir, digest)
	next := make(map[string]*packages.Package, len(entry.index)+len(missing))
	for k, v := range entry.index {
		next[k] = v
	}
	var visit func(pkg *packages.Package)
	visit = func(pkg *packages.Package) {
		if pkg == nil || pkg.PkgPath == "" {
			return
		}
		if _, seen := next[pkg.PkgPath]; seen {
			return
		}
		// A package the go command could not resolve carries no module
		// answer; keeping it would classify it as the standard library.
		// Leaving it out drops the use exactly as a missing entry does.
		if len(pkg.Errors) > 0 && pkg.Module == nil {
			return
		}
		next[pkg.PkgPath] = &packages.Package{ID: pkg.ID, Name: pkg.Name, PkgPath: pkg.PkgPath, Module: pkg.Module}
		for _, imp := range pkg.Imports {
			visit(imp)
		}
	}
	for _, pkg := range loaded {
		visit(pkg)
	}
	entry.index = next
	return next, fromClosure, time.Since(start), nil
}

// stripSiblingBodiesParser parses the files the handle carries whole and
// every other file of a root package with its function bodies removed.
// Package-level declarations, method sets and signatures are unchanged, so
// the handle files' definitions, uses and stamps are unchanged; the siblings
// contribute no output. A file with a line directive stays whole.
func stripSiblingBodiesParser(handleAbs map[string]struct{}) func(*token.FileSet, string, []byte) (*ast.File, error) {
	keep := make(map[string]struct{}, len(handleAbs)*2)
	for abs := range handleAbs {
		keep[filepath.Clean(abs)] = struct{}{}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			keep[resolved] = struct{}{}
		}
	}
	const mode = parser.AllErrors | parser.ParseComments | parser.SkipObjectResolution
	return func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
		file, err := parser.ParseFile(fset, filename, src, mode)
		if file == nil {
			return file, err
		}
		if _, whole := keep[filepath.Clean(filename)]; whole || hasLineDirective(src) {
			return file, err
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				fn.Body = nil
			}
		}
		return file, err
	}
}

// filterBindingsToFiles keeps the rows whose file is one of files.
func filterBindingsToFiles(rows []graph.SemanticBindingType, files map[string]struct{}) []graph.SemanticBindingType {
	kept := rows[:0]
	for _, row := range rows {
		if _, ok := files[normalizeRelPath(row.Site.FilePath)]; ok {
			kept = append(kept, row)
		}
	}
	return kept
}

// finishEmptyCheckoutScope completes a checkout pass whose handle carries no
// Go file a whole-module load would type-check. Such a load would write no
// definition, use, stamp or implements fact for the handle, and its binding
// rows restricted to the handle are empty; the pass writes exactly that
// without loading anything.
func (p *Provider) finishEmptyCheckoutScope(g graph.Store, absRoot, repoPrefix string, handleFiles []string, compiler *semantic.CompilerLoadStats, start time.Time) (*semantic.EnrichResult, error) {
	compiler.Scope = semantic.CompilerScopeHandleRoots
	compiler.ScopeReason = semantic.CompilerScopeReasonEmpty
	if writer, ok := g.(graph.SemanticBindingTypeWriter); ok {
		if err := writer.ReplaceSemanticBindingTypes(repoPrefix, nil); err != nil {
			return nil, err
		}
	}
	if _, persistent := g.(graph.SemanticBindingTypeStore); !persistent {
		p.replaceBindingIndex(absRoot, handleFiles, nil)
	}
	if p.logger != nil {
		p.logger.Info("go-types: handle carries no loadable Go file; nothing loaded",
			zap.String("repo_prefix", repoPrefix),
			zap.Int("handle_go_files", len(handleFiles)))
	}
	return &semantic.EnrichResult{
		Provider:   p.Name(),
		Language:   "go",
		Compiler:   compiler,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}
