package goanalysis

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/go/gcexportdata"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/semantic"
)

// Per-checkout compiler state for handle-rooted checkout passes.
//
// A handle-rooted load spends most of its time outside the handle: the go
// command lists (and, with -export, re-validates or rebuilds) the whole
// dependency closure of the roots, and every pass re-reads the dependencies'
// export data and re-parses every file of the root packages. Between two
// passes of one checkout the closure is almost always unchanged: an edit
// touches the root packages only. The state below keeps, per checkout
// directory:
//
//   - the closure's package metadata as `go list -export -deps` returned it,
//     with a manifest of every mutable (main-module or locally replaced)
//     package's files taken when it was listed;
//   - the dependency types read from export data (one token.FileSet and one
//     path-keyed package map, the view go/packages builds per load);
//   - the roots' parsed files, keyed by file identity.
//
// A pass whose roots' file sets and imports and whose mutable dependencies'
// files are unchanged serves the closure from memory (no `go list`), imports
// dependencies from the retained types and parses only files that changed.
// The roots are type-checked from source exactly as go/packages does. Any
// shape the cached path cannot reproduce (cgo, vendoring, a dependency
// without export data, a package between two roots, test variants) makes
// the pass use the plain load instead.

// Default cap of the retained type state across checkouts (estimated heap).
const defaultTypecheckCacheBytes int64 = 256 << 20

// The retained-heap estimate. Calibrated on this repository's largest
// packages (internal/mcp, internal/indexer, internal/config: measured
// retained heap 5-61 MiB, estimate within about 10%): the types read from
// export data cost about an eighth of the export files' size (the files
// are whole archives, object code included), and a retained syntax tree
// about twice its source when its function bodies are stripped and eight
// times when whole.
const (
	typecheckExportDivisor       = 8
	typecheckStrippedSourceScale = 2
	typecheckWholeSourceScale    = 8
)

func syntaxWeight(p *tcParsed) int64 {
	if p.stripped {
		return typecheckStrippedSourceScale * p.size
	}
	return typecheckWholeSourceScale * p.size
}

// maxTypecheckCheckouts bounds the checkouts whose closure metadata is kept.
const maxTypecheckCheckouts = 16

// errTypecheckExportUnavailable reports that a retained export file could
// not be read (the build cache was trimmed); the pass relists.
var errTypecheckExportUnavailable = errors.New("dependency export data unavailable")

// fileStamp is a file's identity for change detection.
type fileStamp struct {
	size  int64
	mtime int64
	// An inode on Unix; a file index and volume serial on Windows.
	identity [2]uint64
}

// tcFileRecord is one source file of a mutable package as it was when the
// closure was listed: its stamp, the hash of everything that decides whether
// and as which package it builds (the text before the package clause and the
// package name), and the hash of its content.
type tcFileRecord struct {
	stamp   fileStamp
	header  [32]byte
	content [32]byte
}

// tcManifest is a mutable package directory's Go files at listing time.
type tcManifest struct {
	files map[string]tcFileRecord // base name -> record
	// stale is set when a file changed while the go command ran: its export
	// data may describe either version, so the entry is never served.
	stale bool
}

// tcParsed is one retained parsed file.
type tcParsed struct {
	stamp    fileStamp
	stripped bool
	file     *ast.File
	tokFile  *token.File
	err      error
	size     int64
	decl     [32]byte
}

// checkoutTypecheckState is one checkout's retained compiler state. mu is
// held for the whole lifetime of a pass's compiler program (until the pass
// releases it), so the retained syntax and FileSet are never changed under a
// pass that still reads them.
type checkoutTypecheckState struct {
	mu      sync.Mutex
	loadDir string
	digest  string

	// Closure metadata, keyed by package path (no test variants).
	meta      map[string]*packages.Package
	byDir     map[string]*packages.Package
	manifests map[string]*tcManifest
	// listedAt is, per package path, the start of the listing its metadata
	// came from, and fromWarmup whether that listing was the background
	// whole-module warm-up: an older listing never replaces newer metadata.
	listedAt   map[string]time.Time
	fromWarmup map[string]bool
	// exportStale marks retained packages whose metadata still describes
	// their files but whose export data was compiled against a dependency
	// version the state no longer holds (see markExportStale): a pass
	// type-checks them from source and never reads their export data.
	exportStale map[string]bool
	sizes       types.Sizes
	lastList    time.Duration
	// warmList is the wall time of the last warm-up listing merged.
	warmList time.Duration

	// Retained type state.
	fset        *token.FileSet
	view        map[string]*types.Package
	viewExport  map[string]string
	exportBytes int64
	// viewBytes is, per package read from export data, the size of the
	// export file read (the parts of exportBytes).
	viewBytes map[string]int64
	parsed    map[string]*tcParsed
	// syntaxBytes is the retained syntax's estimated heap (syntaxWeight).
	syntaxBytes int64

	// touch is, per package path, the pass clock of the last pass whose
	// working set held the package (see typecheck_retention.go); clock
	// counts the passes. A warm-up or a pre-check never touches.
	touch map[string]uint64
	clock uint64
	// lastKept counts the packages the last merged listing brought back
	// degraded whose retained metadata was kept (keepRetained).
	lastKept int
	// relistSig is the signature of the last scheduled export relist and
	// relistRunning whether one runs (typecheck_relist.go).
	relistSig     string
	relistRunning bool

	// lastUsed is when anything (a pass, a warm-up, a status read) last
	// asked for the state; touchedAt when a pass last used it. Both are
	// guarded by Provider.tcMu. The memory cap and the checkout bound
	// order checkouts by touchedAt, so a warm-up never makes a checkout
	// look more recently used than the ones edits keep touching.
	lastUsed  time.Time
	touchedAt time.Time
	hits      int
	misses    int
	// warmHits counts closure hits whose root metadata came from the
	// background warm-up listing.
	warmHits int
}

func newCheckoutTypecheckState(loadDir, digest string) *checkoutTypecheckState {
	st := &checkoutTypecheckState{loadDir: loadDir, digest: digest}
	st.resetMetadata()
	st.resetTypes()
	return st
}

func (st *checkoutTypecheckState) resetMetadata() {
	st.meta = map[string]*packages.Package{}
	st.byDir = map[string]*packages.Package{}
	st.manifests = map[string]*tcManifest{}
	st.listedAt = map[string]time.Time{}
	st.fromWarmup = map[string]bool{}
	st.exportStale = map[string]bool{}
}

func (st *checkoutTypecheckState) resetTypes() {
	st.fset = token.NewFileSet()
	st.view = map[string]*types.Package{}
	st.viewExport = map[string]string{}
	st.exportBytes = 0
	st.viewBytes = map[string]int64{}
	st.parsed = map[string]*tcParsed{}
	st.syntaxBytes = 0
}

func (st *checkoutTypecheckState) estimatedBytes() int64 {
	return st.exportBytes/typecheckExportDivisor + st.syntaxBytes
}

// typecheckState returns the checkout's state, creating it or starting over
// when the module manifests changed, and evicts the least recently used
// checkouts beyond the metadata bound. The caller locks the returned state.
func (p *Provider) typecheckState(loadDir, digest string) *checkoutTypecheckState {
	p.tcMu.Lock()
	defer p.tcMu.Unlock()
	if p.tcStates == nil {
		p.tcStates = map[string]*checkoutTypecheckState{}
	}
	st := p.tcStates[loadDir]
	if st == nil || st.digest != digest {
		st = newCheckoutTypecheckState(loadDir, digest)
		p.tcStates[loadDir] = st
	}
	st.lastUsed = time.Now()
	for len(p.tcStates) > maxTypecheckCheckouts {
		var oldest *checkoutTypecheckState
		for _, other := range p.tcStates {
			if other != st && (oldest == nil || lessRecentlyTouched(other, oldest)) {
				oldest = other
			}
		}
		if oldest == nil {
			break
		}
		delete(p.tcStates, oldest.loadDir)
	}
	return st
}

// TypecheckCacheStatus is one checkout's retained compiler state.
type TypecheckCacheStatus struct {
	Dir           string
	Packages      int
	Closure       int
	EstimateBytes int64
	Hits, Misses  int
	// WarmHits counts the hits served from the background warm-up listing.
	WarmHits int
	LastUsed time.Time
	// Warmup is the checkout's background whole-module listing.
	Warmup CheckoutWarmupStatus
}

// TypecheckCacheStatus reports the retained per-checkout compiler state.
// A checkout whose state is in use by a pass is reported without sizes.
func (p *Provider) TypecheckCacheStatus() []TypecheckCacheStatus {
	p.tcMu.Lock()
	states := make([]*checkoutTypecheckState, 0, len(p.tcStates))
	used := make([]time.Time, 0, len(p.tcStates))
	for _, st := range p.tcStates {
		states = append(states, st)
		used = append(used, st.lastUsed)
	}
	p.tcMu.Unlock()
	out := make([]TypecheckCacheStatus, 0, len(states))
	for i, st := range states {
		row := TypecheckCacheStatus{Dir: st.loadDir, LastUsed: used[i]}
		if st.mu.TryLock() {
			row.Packages, row.Closure = len(st.view), len(st.meta)
			row.EstimateBytes = st.estimatedBytes()
			row.Hits, row.Misses, row.WarmHits = st.hits, st.misses, st.warmHits
			st.mu.Unlock()
		}
		row.Warmup = p.CheckoutWarmup(st.loadDir)
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// mutablePackage reports whether a listed package's source can change under
// the checkout: a main-module package or one replaced by a local directory.
// Standard-library and versioned module-cache packages are immutable.
func mutablePackage(pkg *packages.Package) bool {
	if pkg == nil || pkg.Module == nil {
		return false
	}
	mod := pkg.Module
	if mod.Replace != nil {
		mod = mod.Replace
	}
	return mod.Main || mod.Version == ""
}

// goSourceName reports whether a directory entry is a file the go command
// considers for a non-test package build.
func goSourceName(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") &&
		!strings.HasPrefix(name, "_") && !strings.HasPrefix(name, ".")
}

// listGoSources returns the directory's candidate Go file names, sorted.
func listGoSources(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !goSourceName(entry.Name()) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// headerHash hashes what decides whether a file builds and as which package:
// the text before the package clause (build constraints) and the name.
func headerHash(src []byte) [32]byte {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil || file == nil || file.Name == nil {
		return sha256.Sum256(append([]byte("unparsable\x00"), src...))
	}
	offset := fset.Position(file.Package).Offset
	if offset < 0 || offset > len(src) {
		offset = len(src)
	}
	h := sha256.New()
	h.Write(src[:offset])
	h.Write([]byte{0})
	h.Write([]byte(file.Name.Name))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// recordManifest reads a mutable package's directory after a listing. A file
// modified at or after the listing started may be described by either
// version in the export data, so the manifest is marked stale.
func recordManifest(dir string, listStart time.Time) *tcManifest {
	m := &tcManifest{files: map[string]tcFileRecord{}}
	names, err := listGoSources(dir)
	if err != nil {
		m.stale = true
		return m
	}
	for _, name := range names {
		abs := filepath.Join(dir, name)
		stamp, ok := statStamp(abs)
		if !ok {
			m.stale = true
			continue
		}
		if stamp.mtime >= listStart.UnixNano() {
			m.stale = true
		}
		src, readErr := os.ReadFile(abs)
		if readErr != nil {
			m.stale = true
			continue
		}
		m.files[name] = tcFileRecord{stamp: stamp, header: headerHash(src), content: sha256.Sum256(src)}
	}
	return m
}

// dependencyChange classifies a mutable dependency's directory against its
// manifest.
type dependencyChange int

const (
	// dependencyUnchanged: the same files with the same content.
	dependencyUnchanged dependencyChange = iota
	// dependencyContentChanged: the same files, each with the same build
	// header and package name, some with other content. The listed
	// metadata (files, package name) still describes the package; only its
	// export data is stale.
	dependencyContentChanged
	// dependencyShapeChanged: a file was added, removed or unreadable, a
	// build header or package name changed, or the manifest is stale; only
	// a new listing describes the package.
	dependencyShapeChanged
)

// dependencyState classifies dir against the manifest. It reads only files
// whose stamp changed.
func (m *tcManifest) dependencyState(dir string, vc *semantic.CompilerCacheStats) dependencyChange {
	if m == nil || m.stale {
		return dependencyShapeChanged
	}
	names, err := listGoSources(dir)
	if err != nil || len(names) != len(m.files) {
		return dependencyShapeChanged
	}
	state := dependencyUnchanged
	for _, name := range names {
		rec, ok := m.files[name]
		if !ok {
			return dependencyShapeChanged
		}
		abs := filepath.Join(dir, name)
		vc.ValidateStats++
		if stamp, ok := statStamp(abs); ok && stamp == rec.stamp {
			continue
		}
		vc.ValidateReads++
		src, readErr := os.ReadFile(abs)
		if readErr != nil {
			return dependencyShapeChanged
		}
		if sha256.Sum256(src) == rec.content {
			continue
		}
		if headerHash(src) != rec.header {
			return dependencyShapeChanged
		}
		state = dependencyContentChanged
	}
	return state
}

// unchangedRootFileSet reports whether a root directory would build the same
// files as the listed package: the same candidate names, and for every file
// whose stamp changed the same build header and package name. Bodies may
// change freely; the roots are type-checked from source.
func (m *tcManifest) unchangedRootFileSet(dir string, vc *semantic.CompilerCacheStats) bool {
	if m == nil {
		return false
	}
	names, err := listGoSources(dir)
	if err != nil || len(names) != len(m.files) {
		return false
	}
	for _, name := range names {
		rec, ok := m.files[name]
		if !ok {
			return false
		}
		abs := filepath.Join(dir, name)
		vc.ValidateStats++
		if stamp, ok := statStamp(abs); ok && stamp == rec.stamp {
			continue
		}
		vc.ValidateReads++
		src, readErr := os.ReadFile(abs)
		if readErr != nil || headerHash(src) != rec.header {
			return false
		}
	}
	return true
}

// tcListing is one `go list -export -deps` result, flattened, with the
// manifests of its mutable packages recorded right after it. It is built
// without the state lock (the warm-up lists the whole module in the
// background) and merged under it.
type tcListing struct {
	pkgs      []*packages.Package
	manifests map[string]*tcManifest
	sizes     types.Sizes
	start     time.Time
	elapsed   time.Duration
	warmup    bool
}

// runListing runs the metadata+export listing of patterns in loadDir and
// records the manifest of every mutable package it lists.
func (p *Provider) runListing(ctx context.Context, loadDir string, patterns, buildFlags []string) (*tcListing, error) {
	cfg := &packages.Config{
		Context: ctx,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedExportFile | packages.NeedModule |
			packages.NeedTypesSizes,
		Dir:        loadDir,
		Tests:      false,
		BuildFlags: buildFlags,
	}
	load := p.packagesLoad
	if load == nil {
		load = packages.Load
	}
	out := &tcListing{start: time.Now(), manifests: map[string]*tcManifest{}}
	roots, err := load(cfg, patterns...)
	out.elapsed = time.Since(out.start)
	if err != nil {
		return out, err
	}
	seen := map[*packages.Package]bool{}
	var visit func(pkg *packages.Package)
	visit = func(pkg *packages.Package) {
		if pkg == nil || seen[pkg] || pkg.PkgPath == "" {
			return
		}
		seen[pkg] = true
		if pkg.TypesSizes != nil && out.sizes == nil {
			out.sizes = pkg.TypesSizes
		}
		out.pkgs = append(out.pkgs, pkg)
		for _, imp := range pkg.Imports {
			visit(imp)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	for _, pkg := range out.pkgs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if pkg.Dir != "" && mutablePackage(pkg) {
			out.manifests[pkg.PkgPath] = recordManifest(pkg.Dir, out.start)
		}
	}
	return out, nil
}

// mergeListing merges a listing into the state; the caller holds st.mu.
//
// A package whose retained metadata came from a listing that started later
// keeps it, and so does one whose retained metadata still describes its
// directory while the listing's entry would not serve better (keepRetained:
// a warm-up never replaces valid metadata a pass relies on, and no listing
// replaces it with an entry that lost its export data because a dependency
// did not compile while the go command ran). Retained types of a package
// whose export file changed describe the old export data: they are dropped
// with the retained types of every package that imports it, directly or
// not (invalidateTypes); every other package's types stay. Finally every
// package whose direct import no longer has the export file it was compiled
// against is marked export-stale (see markExportStale). It returns the
// packages taken from the listing.
func (st *checkoutTypecheckState) mergeListing(l *tcListing) int {
	invalid := map[string]bool{}
	replaced := map[string]*packages.Package{}
	taken := 0
	st.lastKept = 0
	for _, pkg := range l.pkgs {
		if at, ok := st.listedAt[pkg.PkgPath]; ok && at.After(l.start) {
			continue
		}
		if st.keepRetained(pkg, l) {
			if !l.warmup {
				st.lastKept++
			}
			continue
		}
		if l.sizes != nil && st.sizes == nil {
			st.sizes = l.sizes
		}
		prev := st.meta[pkg.PkgPath]
		if mutablePackage(pkg) {
			if prev != nil && prev.ExportFile != pkg.ExportFile && st.view[pkg.PkgPath] != nil {
				invalid[pkg.PkgPath] = true
			}
			if read := st.viewExport[pkg.PkgPath]; read != "" && read != pkg.ExportFile {
				invalid[pkg.PkgPath] = true
			}
		}
		if prev == nil && st.view[pkg.PkgPath] != nil {
			invalid[pkg.PkgPath] = true
		}
		if prev != nil {
			replaced[pkg.PkgPath] = prev
			if prev.Dir != "" && filepath.Clean(prev.Dir) != filepath.Clean(pkg.Dir) && st.byDir[filepath.Clean(prev.Dir)] == prev {
				delete(st.byDir, filepath.Clean(prev.Dir))
			}
		}
		st.meta[pkg.PkgPath] = pkg
		st.listedAt[pkg.PkgPath] = l.start
		st.fromWarmup[pkg.PkgPath] = l.warmup
		if pkg.Dir != "" {
			st.byDir[filepath.Clean(pkg.Dir)] = pkg
		}
		if m := l.manifests[pkg.PkgPath]; m != nil {
			st.manifests[pkg.PkgPath] = m
		} else {
			delete(st.manifests, pkg.PkgPath)
		}
		taken++
	}
	for path := range st.markExportStale(replaced) {
		invalid[path] = true
	}
	st.invalidateTypes(invalid, replaced)
	if l.warmup {
		st.warmList = l.elapsed
	} else {
		st.lastList = l.elapsed
	}
	return taken
}

// markExportStale recomputes, until none is left, the retained packages one
// of whose direct imports is no longer retained with the export file the
// package's own listing saw, or is itself export-stale. Such a package's
// export data was compiled against a dependency version a later listing
// replaced; serving it would type-check a root against the old
// dependency's API. Its metadata and manifest still describe its own files,
// so it is kept: a pass checks it (and its importers) from source, and a
// root needs no export data at all. A package one of whose imports has no
// metadata left is dropped (its metadata is recorded in dropped), and
// listed again when a pass needs it. It returns the newly stale or dropped
// packages that held retained types.
func (st *checkoutTypecheckState) markExportStale(dropped map[string]*packages.Package) map[string]bool {
	stale := map[string]bool{}
	held := map[string]bool{}
	for {
		var mark, drop []string
		for path, pkg := range st.meta {
			if stale[path] {
				continue
			}
			for _, imp := range pkg.Imports {
				if imp == nil || imp.PkgPath == "" || imp.PkgPath == "unsafe" || imp.PkgPath == "C" {
					continue
				}
				cur := st.meta[imp.PkgPath]
				if cur == nil {
					drop = append(drop, path)
					break
				}
				if cur.ExportFile != imp.ExportFile || stale[imp.PkgPath] {
					mark = append(mark, path)
					break
				}
			}
		}
		if len(mark) == 0 && len(drop) == 0 {
			break
		}
		for _, path := range mark {
			stale[path] = true
			if st.view[path] != nil && !st.exportStale[path] {
				held[path] = true
			}
		}
		for _, path := range drop {
			pkg := st.meta[path]
			if st.view[path] != nil {
				held[path] = true
			}
			if dropped != nil {
				dropped[path] = pkg
			}
			if pkg.Dir != "" && st.byDir[filepath.Clean(pkg.Dir)] == pkg {
				delete(st.byDir, filepath.Clean(pkg.Dir))
			}
			delete(st.meta, path)
			delete(st.manifests, path)
			delete(st.listedAt, path)
			delete(st.fromWarmup, path)
			delete(stale, path)
		}
	}
	st.exportStale = stale
	return held
}

// listClosure runs the metadata+export listing of the roots and merges it
// into the state (the caller holds st.mu).
func (p *Provider) listClosure(ctx context.Context, st *checkoutTypecheckState, patterns []string) (time.Duration, error) {
	// A pass's listing is interactive work: a background warm-up yields.
	endLoad := p.beginCompilerLoad(st.loadDir)
	listing, err := p.runListing(ctx, st.loadDir, patterns, nil)
	endLoad()
	if err != nil {
		return listing.elapsed, err
	}
	st.mergeListing(listing)
	return listing.elapsed, nil
}

// closureCheck is the outcome of validating the retained closure for a set
// of root directories.
type closureCheck struct {
	roots      []*packages.Package
	missReason string
	bypass     string
	// changed, with missReason dependency_changed, lists the mutable
	// dependencies whose content (only) changed since they were listed,
	// and closure every package path below the roots: the pass may
	// type-check the changed part from source instead of listing again.
	changed []*packages.Package
	closure []string
	// noExport lists the closure's mutable dependencies listed without
	// export data (they did not compile when listed; not export-stale):
	// a pass may serve them only by checking them from source.
	noExport []string
	// missPackage names the package whose state caused the miss.
	missPackage string
}

// checkRoots finds the retained metadata of every root directory and checks
// that each still builds the same files.
func (st *checkoutTypecheckState) checkRoots(rootDirs map[string]struct{}, vc *semantic.CompilerCacheStats) closureCheck {
	if len(st.meta) == 0 {
		return closureCheck{missReason: "cold"}
	}
	dirs := make([]string, 0, len(rootDirs))
	for dir := range rootDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var out closureCheck
	for _, dir := range dirs {
		meta := st.byDir[filepath.Clean(dir)]
		if meta == nil {
			return closureCheck{missReason: "root_unlisted"}
		}
		if !mutablePackage(meta) {
			return closureCheck{bypass: "root_not_mutable"}
		}
		manifest := st.manifests[meta.PkgPath]
		if !manifest.unchangedRootFileSet(meta.Dir, vc) {
			return closureCheck{missReason: "root_files_changed"}
		}
		if !sameFiles(meta.CompiledGoFiles, meta.GoFiles) && !manifest.unchangedCgo(meta, vc) {
			// A cgo source file changed (or the go command's generated
			// files are gone): only a new listing regenerates them.
			return closureCheck{missReason: "cgo_changed"}
		}
		out.roots = append(out.roots, meta)
	}
	return out
}

// unchangedCgo reports, for a package whose compiled files are generated
// from cgo sources, that every cgo source (a Go file that is not compiled
// as-is) still has its listed content and every generated file still
// exists. The generated files then describe the current sources.
func (m *tcManifest) unchangedCgo(meta *packages.Package, vc *semantic.CompilerCacheStats) bool {
	if m == nil || m.stale {
		return false
	}
	compiled := make(map[string]struct{}, len(meta.CompiledGoFiles))
	for _, f := range meta.CompiledGoFiles {
		compiled[filepath.Clean(f)] = struct{}{}
	}
	goFiles := make(map[string]struct{}, len(meta.GoFiles))
	for _, f := range meta.GoFiles {
		goFiles[filepath.Clean(f)] = struct{}{}
		if _, ok := compiled[filepath.Clean(f)]; ok {
			continue
		}
		rec, ok := m.files[filepath.Base(f)]
		if !ok {
			return false
		}
		vc.ValidateStats++
		if stamp, ok := statStamp(f); ok && stamp == rec.stamp {
			continue
		}
		vc.ValidateReads++
		src, err := os.ReadFile(f)
		if err != nil || sha256.Sum256(src) != rec.content {
			return false
		}
	}
	for f := range compiled {
		if _, ok := goFiles[f]; ok {
			continue
		}
		if _, ok := statStamp(f); !ok {
			return false
		}
	}
	return true
}

// sameFiles reports whether two file lists name the same files; a package
// whose compiled files differ from its Go files is generated (cgo).
func sameFiles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, f := range a {
		set[filepath.Clean(f)] = struct{}{}
	}
	for _, f := range b {
		if _, ok := set[filepath.Clean(f)]; !ok {
			return false
		}
	}
	return true
}

// checkDependencies walks the closure below the roots' current imports and
// checks every mutable dependency against its manifest. The manifests are
// checked first and in parallel (a stat sweep whose latency is syscall-bound,
// so it overlaps even at GOMAXPROCS=1): a changed dependency means the
// retained metadata is stale, which a new listing settles before any shape
// the stale metadata shows can bypass the pass.
func (st *checkoutTypecheckState) checkDependencies(roots []*packages.Package, imports map[string][]string, vc *semantic.CompilerCacheStats) closureCheck {
	rootPaths := make(map[string]bool, len(roots))
	for _, root := range roots {
		rootPaths[root.PkgPath] = true
	}
	seen := map[string]bool{}
	var queue []string
	for _, root := range roots {
		for _, path := range imports[root.PkgPath] {
			if path == "unsafe" || rootPaths[path] {
				continue
			}
			if path == "C" {
				if sameFiles(root.CompiledGoFiles, root.GoFiles) && !st.brokenDependencyUnchanged(root, vc) {
					// A cgo file compiled as-is: the root's metadata came
					// from a listing whose go command could not run cgo
					// (a dependency did not compile then). A new listing
					// generates the files, unless that dependency is
					// still the same broken version.
					return closureCheck{missReason: "cgo_unlisted", missPackage: root.PkgPath}
				}
				return closureCheck{bypass: "cgo"}
			}
			if st.meta[path] == nil {
				return closureCheck{missReason: "import_added", missPackage: path}
			}
			queue = append(queue, path)
		}
	}
	var (
		bypass   string
		mutable  []*packages.Package
		noExport []string
	)
	for len(queue) > 0 {
		path := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[path] {
			continue
		}
		seen[path] = true
		meta := st.meta[path]
		if meta == nil {
			return closureCheck{missReason: "dependency_changed", missPackage: path}
		}
		if rootPaths[path] && bypass == "" {
			bypass = "dependency_between_roots"
		}
		if path != "unsafe" && meta.ExportFile == "" && !st.exportStale[path] {
			if !mutablePackage(meta) {
				if bypass == "" {
					bypass = "dependency_without_export"
				}
			} else {
				noExport = append(noExport, path)
			}
		}
		if mutablePackage(meta) {
			mutable = append(mutable, meta)
		}
		for _, imp := range meta.Imports {
			if imp == nil || imp.PkgPath == "" {
				continue
			}
			if rootPaths[imp.PkgPath] && bypass == "" {
				bypass = "dependency_between_roots"
			}
			if !seen[imp.PkgPath] {
				queue = append(queue, imp.PkgPath)
			}
		}
	}
	changed, shapeChanged := st.dependencyChanges(mutable, vc)
	if shapeChanged != "" {
		return closureCheck{missReason: "dependency_changed", missPackage: shapeChanged}
	}
	if len(st.exportStale) > 0 {
		// An export-stale dependency is checked from source like a changed
		// one (its own files are unchanged, or in changed already).
		inChanged := make(map[string]bool, len(changed))
		for _, meta := range changed {
			inChanged[meta.PkgPath] = true
		}
		for path := range seen {
			if st.exportStale[path] && !inChanged[path] {
				changed = append(changed, st.meta[path])
			}
		}
		sort.Slice(changed, func(i, j int) bool { return changed[i].PkgPath < changed[j].PkgPath })
	}
	if len(changed) > 0 {
		out := closureCheck{missReason: "dependency_changed"}
		if bypass == "" {
			// Only content changed and no shape the cached path cannot
			// reproduce: the changed part can be checked from source.
			out.roots, out.changed, out.noExport = roots, changed, noExport
			for path := range seen {
				out.closure = append(out.closure, path)
			}
			sort.Strings(out.closure)
		}
		return out
	}
	if bypass == "" && len(noExport) > 0 {
		bypass = "dependency_without_export"
	}
	if bypass != "" {
		return closureCheck{bypass: bypass}
	}
	return closureCheck{roots: roots}
}

// dependencyCheckWorkers bounds the parallel manifest checks.
const dependencyCheckWorkers = 8

// dependencyChanges checks the mutable dependencies' manifests in parallel.
// It returns the dependencies whose content (only) changed, sorted by path,
// and stops early, naming it in shapeChanged, once one needs a new listing.
func (st *checkoutTypecheckState) dependencyChanges(deps []*packages.Package, vc *semantic.CompilerCacheStats) (changed []*packages.Package, shapeChanged string) {
	if len(deps) == 0 {
		return nil, ""
	}
	workers := dependencyCheckWorkers
	if workers > len(deps) {
		workers = len(deps)
	}
	var (
		mu   sync.Mutex
		next int
		wg   sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := &semantic.CompilerCacheStats{}
			defer func() {
				mu.Lock()
				vc.ValidateStats += local.ValidateStats
				vc.ValidateReads += local.ValidateReads
				mu.Unlock()
			}()
			for {
				mu.Lock()
				if shapeChanged != "" || next >= len(deps) {
					mu.Unlock()
					return
				}
				meta := deps[next]
				next++
				mu.Unlock()
				switch st.manifests[meta.PkgPath].dependencyState(meta.Dir, local) {
				case dependencyShapeChanged:
					mu.Lock()
					if shapeChanged == "" {
						shapeChanged = meta.PkgPath
					}
					mu.Unlock()
					return
				case dependencyContentChanged:
					mu.Lock()
					changed = append(changed, meta)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if shapeChanged != "" {
		return nil, shapeChanged
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].PkgPath < changed[j].PkgPath })
	return changed, ""
}

// sourceDependencyFileCap bounds the files a pass type-checks from source
// for changed dependencies; above it the pass lists the closure again.
const sourceDependencyFileCap = 4000

// planSourceDependencies returns, in path order, the closure packages a pass
// type-checks from source because their content changed since the closure
// was listed (changed) or they import such a package, directly or not.
// Their export data describes the old source, and so does every export file
// compiled against it; every other closure package's export data is still
// exact and imports none of them. It returns a reason instead when the set
// cannot be checked from the listed metadata.
func (st *checkoutTypecheckState) planSourceDependencies(closure []string, changed []*packages.Package, vc *semantic.CompilerCacheStats) ([]*packages.Package, string) {
	dirty := make(map[string]bool, len(closure))
	for _, meta := range changed {
		dirty[meta.PkgPath] = true
	}
	var visit func(path string) bool
	done := map[string]bool{}
	visit = func(path string) bool {
		if done[path] {
			return dirty[path]
		}
		done[path] = true
		meta := st.meta[path]
		if meta == nil {
			return dirty[path]
		}
		for _, imp := range meta.Imports {
			if imp == nil || imp.PkgPath == "" {
				continue
			}
			if visit(imp.PkgPath) {
				dirty[path] = true
			}
		}
		return dirty[path]
	}
	var (
		out   []*packages.Package
		files int
	)
	for _, path := range closure {
		if !visit(path) {
			continue
		}
		meta := st.meta[path]
		if meta == nil {
			return nil, "dependency_unlisted"
		}
		if !mutablePackage(meta) {
			return nil, "immutable_dependent"
		}
		if !sameFiles(meta.CompiledGoFiles, meta.GoFiles) && !st.manifests[path].unchangedCgo(meta, vc) {
			// A cgo package is checked over the go command's generated
			// files, as a cgo root is, while its cgo sources are unchanged.
			return nil, "cgo_changed"
		}
		files += len(meta.CompiledGoFiles)
		if files > sourceDependencyFileCap {
			return nil, "too_many_files"
		}
		out = append(out, meta)
	}
	return out, ""
}

// parseRootFiles returns the roots' syntax, serving unchanged files from the
// retained state. Handle files are always parsed whole and re-read; sibling
// files are parsed without function bodies when strip is set.
func (st *checkoutTypecheckState) parseRootFiles(roots []*packages.Package, handleAbs map[string]struct{}, strip bool, stats *semantic.CompilerCacheStats) (map[string][]*ast.File, map[string][]error, map[string][]string) {
	type job struct {
		abs      string
		stripped bool
		handle   bool
	}
	var jobs []job
	for _, root := range roots {
		for _, abs := range root.CompiledGoFiles {
			_, handle := handleAbs[filepath.Clean(abs)]
			jobs = append(jobs, job{abs: abs, stripped: strip && !handle, handle: handle})
		}
	}
	results := make([]*tcParsed, len(jobs))
	reused := make([]bool, len(jobs))
	var toParse []int
	for i, j := range jobs {
		prev := st.parsed[j.abs]
		if prev != nil && !j.handle && prev.stripped == j.stripped {
			if stamp, ok := statStamp(j.abs); ok && stamp == prev.stamp {
				results[i], reused[i] = prev, true
				continue
			}
		}
		toParse = append(toParse, i)
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > len(toParse) {
		workers = len(toParse)
	}
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				results[i] = st.parseOne(jobs[i].abs, jobs[i].stripped)
			}
		}()
	}
	for _, i := range toParse {
		next <- i
	}
	close(next)
	wg.Wait()

	syntax := map[string][]*ast.File{}
	errs := map[string][]error{}
	imports := map[string][]string{}
	i := 0
	for _, root := range roots {
		seenImport := map[string]bool{}
		for range root.CompiledGoFiles {
			res, j := results[i], jobs[i]
			if reused[i] {
				stats.FilesReused++
			} else {
				stats.FilesParsed++
				if prev := st.parsed[j.abs]; prev != nil {
					if j.handle && prev.decl != res.decl {
						stats.DeclChangedFiles++
					}
					if prev.tokFile != nil {
						st.fset.RemoveFile(prev.tokFile)
					}
					st.syntaxBytes -= syntaxWeight(prev)
				}
				st.parsed[j.abs] = res
				st.syntaxBytes += syntaxWeight(res)
			}
			i++
			if res.err != nil {
				errs[root.PkgPath] = append(errs[root.PkgPath], res.err)
			}
			if res.file == nil {
				continue
			}
			syntax[root.PkgPath] = append(syntax[root.PkgPath], res.file)
			for _, spec := range res.file.Imports {
				path := strings.Trim(spec.Path.Value, "\"`")
				if !seenImport[path] {
					seenImport[path] = true
					imports[root.PkgPath] = append(imports[root.PkgPath], path)
				}
			}
		}
	}
	return syntax, errs, imports
}

// parseOne parses one file into the retained FileSet, with the parser mode
// go/packages uses (whole files) or the sibling-stripping one.
func (st *checkoutTypecheckState) parseOne(abs string, stripped bool) *tcParsed {
	out := &tcParsed{stripped: stripped}
	stamp, _ := statStamp(abs)
	src, err := os.ReadFile(abs)
	if err != nil {
		out.err = err
		return out
	}
	// A file changed between stat and read gets a stamp that never matches,
	// so it is parsed again next time.
	if after, ok := statStamp(abs); ok && after == stamp {
		out.stamp = stamp
	} else {
		out.stamp = fileStamp{size: -1}
	}
	out.size = int64(len(src))
	// Whole files use go/packages' parser mode; stripped siblings use the
	// sibling-stripping parser's (stripSiblingBodiesParser), with the
	// declaration hash taken before the bodies go.
	mode := parser.AllErrors | parser.ParseComments
	if stripped {
		mode |= parser.SkipObjectResolution
	}
	file, err := parser.ParseFile(st.fset, abs, src, mode)
	out.file, out.err = file, err
	if file == nil {
		return out
	}
	out.tokFile = st.fset.File(file.Pos())
	out.decl = declarationHash(st.fset, file, src)
	if stripped && !hasLineDirective(src) {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				fn.Body = nil
			}
		}
	}
	return out
}

// declarationHash hashes a file's tokens outside function bodies, comments
// excluded: equal hashes mean equal package-level declarations.
func declarationHash(fset *token.FileSet, file *ast.File, src []byte) [32]byte {
	tf := fset.File(file.Pos())
	type span struct{ from, to int }
	var bodies []span
	if tf != nil {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				bodies = append(bodies, span{tf.Offset(fn.Body.Lbrace), tf.Offset(fn.Body.Rbrace)})
			}
		}
	}
	h := sha256.New()
	var s scanner.Scanner
	sfset := token.NewFileSet()
	s.Init(sfset.AddFile("", -1, len(src)), src, nil, 0)
	b := 0
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		off := sfset.Position(pos).Offset
		for b < len(bodies) && off > bodies[b].to {
			b++
		}
		if b < len(bodies) && off > bodies[b].from && off < bodies[b].to {
			continue
		}
		fmt.Fprintf(h, "%d %s\x00", tok, lit)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// importDependency returns a dependency's types from the retained state,
// reading its export data on first use. The caller holds st.mu.
func (st *checkoutTypecheckState) importDependency(path string, stats *semantic.CompilerCacheStats, direct bool) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if tpkg := st.view[path]; tpkg != nil && tpkg.Complete() && st.viewExport[path] != "" {
		if direct {
			stats.TypesReused++
		}
		return tpkg, nil
	}
	if st.exportStale[path] {
		// Never reached through a consistent plan; relist rather than read
		// export data compiled against an old dependency.
		return nil, fmt.Errorf("%w: %s export data is stale", errTypecheckExportUnavailable, path)
	}
	meta := st.meta[path]
	if meta == nil {
		return nil, fmt.Errorf("no metadata for %s", path)
	}
	if meta.ExportFile == "" {
		return nil, fmt.Errorf("no export data file for %s", path)
	}
	f, err := os.Open(meta.ExportFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errTypecheckExportUnavailable, err)
	}
	defer f.Close()
	r, err := compilerExportDataSection(f)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", errTypecheckExportUnavailable, meta.ExportFile, err)
	}
	tpkg, err := gcexportdata.Read(r, st.fset, st.view, path)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", errTypecheckExportUnavailable, meta.ExportFile, err)
	}
	st.viewExport[path] = meta.ExportFile
	if info, statErr := f.Stat(); statErr == nil {
		st.exportBytes += info.Size() - st.viewBytes[path]
		st.viewBytes[path] = info.Size()
	}
	stats.ExportReads++
	return tpkg, nil
}

// compilerExportDataSection positions a reader at the export-data section of
// a compiler-produced package archive, the file format `go list -export`
// names in each package's Export field, and bounds it to that section so
// gcexportdata.Read sees only the unified export data.
//
// The archive layout is: the "!<arch>\n" signature, a 60-byte member header
// for "__.PKGDEF" carrying the member size, a "go object ..." header line and
// further header lines up to the "$$" marker, the binary export-data header
// "$$B\n", the export data itself, and the "\n$$\n" end-of-section marker.
// This mirrors the section location that gcexportdata.NewReader performs;
// that function is deprecated, and gcexportdata.Read rejects archives, so the
// location is done here.
func compilerExportDataSection(in io.Reader) (io.Reader, error) {
	r := bufio.NewReader(in)
	line, err := r.ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("can't find export data (%v)", err)
	}
	if string(line) != "!<arch>\n" {
		return nil, fmt.Errorf("not the start of an archive file (%q)", line)
	}
	const memberHeaderSize = 60
	var hdr [memberHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("not a package file: %v", err)
	}
	if name := strings.Trim(string(hdr[0:16]), " "); !strings.HasPrefix(name, "__.PKGDEF") {
		return nil, fmt.Errorf("not a package file: first member is %q", name)
	}
	memberSize, err := strconv.Atoi(strings.Trim(string(hdr[48:58]), " "))
	if err != nil || memberSize <= 0 {
		return nil, fmt.Errorf("not a package file: bad member size %q", hdr[48:58])
	}
	size := int64(memberSize)

	// Object headers: "go object ..." first, then any lines up to "$$".
	if line, err = r.ReadSlice('\n'); err != nil {
		return nil, fmt.Errorf("can't find export data (%v)", err)
	}
	if !strings.HasPrefix(string(line), "go object ") {
		return nil, fmt.Errorf("not a go object file: %s", line)
	}
	size -= int64(len(line))
	for {
		peek, err := r.Peek(2)
		if err != nil {
			return nil, fmt.Errorf("can't find export data (%v)", err)
		}
		if string(peek) == "$$" {
			break
		}
		if line, err = r.ReadSlice('\n'); err != nil {
			return nil, fmt.Errorf("can't find export data (%v)", err)
		}
		size -= int64(len(line))
	}

	if line, err = r.ReadSlice('\n'); err != nil {
		return nil, fmt.Errorf("can't find export data (%v)", err)
	}
	if string(line) != "$$B\n" {
		return nil, fmt.Errorf("unknown export data header: %q", line)
	}
	size -= int64(len(line))

	// The end-of-section marker closes the member and is not export data.
	const endOfSection = "\n$$\n"
	size -= int64(len(endOfSection))
	if size < 0 {
		return nil, fmt.Errorf("invalid size (%d) in the archive file: %d bytes remain without section headers", memberSize, size)
	}
	return &io.LimitedReader{R: r, N: size}, nil
}

// typecheckRoots type-checks the roots from the given syntax, in dependency
// order, importing everything else from the retained state. It mirrors
// go/packages' checker configuration for an initial package.
func (st *checkoutTypecheckState) typecheckRoots(ctx context.Context, roots []*packages.Package, syntax map[string][]*ast.File, parseErrs map[string][]error, imports map[string][]string, stats *semantic.CompilerCacheStats) ([]*packages.Package, error) {
	byPath := make(map[string]*packages.Package, len(roots))
	for _, root := range roots {
		byPath[root.PkgPath] = root
	}
	var order []*packages.Package
	state := map[string]int{}
	var visit func(root *packages.Package)
	visit = func(root *packages.Package) {
		if state[root.PkgPath] != 0 {
			return
		}
		state[root.PkgPath] = 1
		for _, path := range imports[root.PkgPath] {
			if dep := byPath[path]; dep != nil && state[path] == 0 {
				visit(dep)
			}
		}
		state[root.PkgPath] = 2
		order = append(order, root)
	}
	for _, root := range roots {
		visit(root)
	}

	checked := make(map[string]*packages.Package, len(roots))
	out := make([]*packages.Package, len(roots))
	index := make(map[string]int, len(roots))
	for i, root := range roots {
		index[root.PkgPath] = i
	}
	for _, meta := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pkg := &packages.Package{
			ID:              meta.ID,
			Name:            meta.Name,
			PkgPath:         meta.PkgPath,
			Dir:             meta.Dir,
			GoFiles:         append([]string(nil), meta.GoFiles...),
			CompiledGoFiles: append([]string(nil), meta.CompiledGoFiles...),
			IgnoredFiles:    append([]string(nil), meta.IgnoredFiles...),
			Module:          meta.Module,
			Imports:         map[string]*packages.Package{},
			Fset:            st.fset,
			Syntax:          syntax[meta.PkgPath],
			TypesSizes:      st.sizes,
		}
		if pkg.Syntax == nil {
			pkg.Syntax = []*ast.File{}
		}
		appendError := func(err error) {
			switch err := err.(type) {
			case scanner.ErrorList:
				for _, e := range err {
					pkg.Errors = append(pkg.Errors, packages.Error{Pos: e.Pos.String(), Msg: e.Msg, Kind: packages.ParseError})
				}
			case types.Error:
				pkg.TypeErrors = append(pkg.TypeErrors, err)
				pkg.Errors = append(pkg.Errors, packages.Error{Pos: err.Fset.Position(err.Pos).String(), Msg: err.Msg, Kind: packages.TypeError})
			default:
				pkg.Errors = append(pkg.Errors, packages.Error{Pos: "-", Msg: err.Error(), Kind: packages.UnknownError})
			}
		}
		for _, err := range parseErrs[meta.PkgPath] {
			appendError(err)
		}
		for _, path := range imports[meta.PkgPath] {
			if path == "unsafe" {
				continue
			}
			if dep := checked[path]; dep != nil {
				pkg.Imports[path] = dep
			} else if dep := st.meta[path]; dep != nil {
				pkg.Imports[path] = dep
			}
		}
		var importErr error
		importer := importerFunc(func(path string) (*types.Package, error) {
			if path == "unsafe" {
				return types.Unsafe, nil
			}
			if dep := checked[path]; dep != nil {
				return dep.Types, nil
			}
			tpkg, err := st.importDependency(path, stats, true)
			if err != nil && errors.Is(err, errTypecheckExportUnavailable) && importErr == nil {
				importErr = err
			}
			return tpkg, err
		})
		pkg.Types = types.NewPackage(meta.PkgPath, meta.Name)
		pkg.TypesInfo = &types.Info{
			Types:        make(map[ast.Expr]types.TypeAndValue),
			Defs:         make(map[*ast.Ident]types.Object),
			Uses:         make(map[*ast.Ident]types.Object),
			Implicits:    make(map[ast.Node]types.Object),
			Instances:    make(map[*ast.Ident]types.Instance),
			Scopes:       make(map[ast.Node]*types.Scope),
			Selections:   make(map[*ast.SelectorExpr]*types.Selection),
			FileVersions: make(map[*ast.File]string),
		}
		tc := &types.Config{Importer: importer, Error: appendError, Sizes: st.sizes}
		if meta.Module != nil && meta.Module.GoVersion != "" {
			tc.GoVersion = "go" + meta.Module.GoVersion
		}
		typErr := types.NewChecker(tc, st.fset, pkg.Types, pkg.TypesInfo).Files(pkg.Syntax)
		if importErr != nil {
			return nil, importErr
		}
		if typErr != nil && len(pkg.Errors) == 0 {
			appendError(typErr)
		}
		pkg.IllTyped = len(pkg.Errors) > 0
		checked[meta.PkgPath] = pkg
		out[index[meta.PkgPath]] = pkg
	}
	return out, nil
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// cachedProgram is the outcome of loadCheckoutProgramCached.
type cachedProgram struct {
	program compilerProgram
	// closure is the checkout's retained metadata; read-only while the
	// state lock is held (until release).
	closure map[string]*packages.Package
	// bypass is set when the pass must use the plain load instead.
	bypass string
	// release unlocks the checkout state; the pass calls it once its
	// compiler program is released.
	release func()
}

// loadCheckoutProgramCached loads the handle roots through the checkout's
// retained state. The returned release must be called exactly once, after
// the pass stops reading the program (also on error and bypass).
func (p *Provider) loadCheckoutProgramCached(ctx context.Context, loadDir, digest string, plan handleRootPlan, scope semantic.CheckoutCompilerScope, stats *semantic.CompilerCacheStats) (cachedProgram, error) {
	p.warmupSnapshot(loadDir, stats)
	st := p.typecheckState(loadDir, digest)
	st.mu.Lock()
	var once sync.Once
	release := func() { once.Do(st.mu.Unlock) }
	out := cachedProgram{release: release}
	bypass := func(reason string) (cachedProgram, error) {
		stats.Bypass = reason
		out.bypass = reason
		return out, nil
	}
	if p.includeTest {
		return bypass("tests")
	}
	if goTypesNeedDepsClosure() {
		return bypass("needdeps_closure")
	}
	if fileExists(filepath.Join(loadDir, "vendor", "modules.txt")) {
		return bypass("vendor")
	}

	listed, exportRetried, targetedWaited := false, false, false
	relist := func(reason string) error {
		if stats.MissReason == "" {
			stats.MissReason = reason
		}
		stats.ClosureMisses++
		st.misses++
		elapsed, err := p.listClosure(ctx, st, plan.patterns)
		stats.GoListMs += elapsed.Milliseconds()
		if err == nil {
			p.warm.dropPending(filepath.Clean(loadDir), plan.rootDirs)
		}
		stats.RetainedKept += st.lastKept
		listed = true
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		validateStart := time.Now()
		check := st.checkRoots(plan.rootDirs, stats)
		if check.bypass != "" {
			return bypass(check.bypass)
		}
		if check.missReason != "" {
			stats.ValidateMs += time.Since(validateStart).Milliseconds()
			if !listed && !targetedWaited && (check.missReason == "root_unlisted" || check.missReason == "cold") {
				// The warm-up is listing this root's package right now
				// (a dirty or recently touched package): wait for that
				// listing, bounded, instead of listing it a second time.
				targetedWaited = true
				st.mu.Unlock()
				waited, outcome := p.waitTargetedListing(ctx, filepath.Clean(loadDir), plan.rootDirs, targetedWaitBound())
				st.mu.Lock()
				stats.TargetedWaitMs, stats.TargetedWait = waited.Milliseconds(), outcome
				if outcome != "" {
					continue
				}
			}
			if listed {
				// A fresh listing that still does not validate (a root with
				// no metadata, a file changing under the go command): the
				// plain load decides.
				return bypass("unstable_listing")
			}
			if err := relist(check.missReason); err != nil {
				return out, err
			}
			continue
		}
		stats.ValidateMs += time.Since(validateStart).Milliseconds()

		parseStart := time.Now()
		syntax, parseErrs, imports := st.parseRootFiles(check.roots, plan.handleAbs, scope.StripSiblingBodies, stats)
		stats.ParseMs += time.Since(parseStart).Milliseconds()
		if err := ctx.Err(); err != nil {
			return out, err
		}

		validateStart = time.Now()
		deps := st.checkDependencies(check.roots, imports, stats)
		stats.ValidateMs += time.Since(validateStart).Milliseconds()
		if deps.bypass != "" {
			return bypass(deps.bypass)
		}
		var source *sourceDependencies
		if deps.missReason != "" && len(deps.changed) > 0 {
			// Content-only changes below the roots: check the changed
			// part from source instead of listing (and rebuilding the
			// export data of) everything between it and the roots.
			stats.ChangedDependencies = len(deps.changed)
			var reason string
			source, reason = st.prepareSourceDependencies(check.roots, deps.closure, deps.changed, stats)
			if reason == "" && !source.covers(deps.noExport) {
				// A dependency without export data that the plan does
				// not check from source: only the plain load serves it.
				return bypass("dependency_without_export")
			}
			if reason != "" {
				stats.SourceDependencyFallback = reason
			} else {
				deps.missReason = ""
			}
		}
		if deps.missReason != "" {
			if stats.MissPackage == "" {
				stats.MissPackage = deps.missPackage
			}
			if listed {
				return bypass("unstable_listing")
			}
			if err := relist(deps.missReason); err != nil {
				return out, err
			}
			continue
		}
		if !listed {
			stats.ClosureHits++
			st.hits++
			saved := st.lastList
			for _, root := range check.roots {
				if st.fromWarmup[root.PkgPath] {
					// The root was never listed by a pass: the background
					// warm-up's listing is what this hit did not pay for.
					stats.WarmServed = true
					st.warmHits++
					if saved == 0 {
						saved = st.warmList
					}
					break
				}
			}
			stats.GoListSavedMs += saved.Milliseconds()
		}

		if err := ctx.Err(); err != nil {
			return out, err
		}
		checkStart := time.Now()
		checkRoots := check.roots
		if source != nil {
			checkRoots = source.merge(check.roots, syntax, parseErrs, imports)
		}
		pkgs, err := st.typecheckRoots(ctx, checkRoots, syntax, parseErrs, imports, stats)
		stats.CheckMs += time.Since(checkStart).Milliseconds()
		// Hard errors anywhere in what this pass checked (a dependency
		// checked from source included) mean the tree does not compile.
		compiles := err == nil && classifyLoadErrors(pkgs).hard == 0
		if err == nil && source != nil {
			pkgs = source.rootsOnly(pkgs, st.meta)
		}
		if err != nil {
			if errors.Is(err, errTypecheckExportUnavailable) && !exportRetried {
				exportRetried = true
				// The build cache lost a retained export file: start over
				// from a fresh listing once.
				st.resetMetadata()
				st.resetTypes()
				listed = false
				if relistErr := relist("export_unavailable"); relistErr != nil {
					return out, relistErr
				}
				continue
			}
			return out, err
		}
		program := compilerProgram{pkgs: pkgs, raw: pkgs, fset: st.fset, packages: len(pkgs)}
		for _, pkg := range pkgs {
			program.files += len(pkg.CompiledGoFiles)
		}
		out.program = program
		out.closure = st.meta
		p.warm.noteRoots(loadDir, plan.rootDirs)
		// The pass's working set is what the next delta over these roots
		// needs: it is touched, and the memory cap evicts everything else
		// first, least recently touched first.
		work := st.touchWorkingSet(check.roots, source)
		p.markTouched(st)
		stats.WorkingSetPackages = len(work.packages)
		if compiles {
			if exportless := st.exportlessWorkingSet(work); len(exportless) > 0 && p.scheduleExportRelist(st, check.roots, exportless) {
				stats.RelistScheduled++
			}
		}
		eviction := p.enforceTypecheckBudget(st, scope.TypecheckCacheBytes, work)
		stats.StateEvicted = eviction.self
		stats.EvictedPackages, stats.EvictedFiles = eviction.packages, eviction.files
		stats.StatePackages = len(st.view)
		stats.StateBytes = st.estimatedBytes()
		return out, nil
	}
}

// sourceDependencies is a pass's changed dependencies (and their importers
// in the closure), parsed with their bodies stripped, to be type-checked
// from source before the roots.
type sourceDependencies struct {
	pkgs      []*packages.Package
	syntax    map[string][]*ast.File
	parseErrs map[string][]error
	imports   map[string][]string
}

// prepareSourceDependencies plans and parses the source dependencies. It
// returns a reason, and nothing, when the pass must list the closure
// instead: the set is not checkable from the listed metadata, or a changed
// file now imports cgo, a root, or a package the closure does not hold.
func (st *checkoutTypecheckState) prepareSourceDependencies(roots []*packages.Package, closure []string, changed []*packages.Package, stats *semantic.CompilerCacheStats) (*sourceDependencies, string) {
	plan, reason := st.planSourceDependencies(closure, changed, stats)
	if reason != "" {
		return nil, reason
	}
	if len(plan) == 0 {
		return nil, "empty_plan"
	}
	parseStart := time.Now()
	local := &semantic.CompilerCacheStats{}
	syntax, parseErrs, imports := st.parseRootFiles(plan, nil, true, local)
	stats.SourceDependencyParseMs += time.Since(parseStart).Milliseconds()
	rootPaths := make(map[string]bool, len(roots))
	for _, root := range roots {
		rootPaths[root.PkgPath] = true
	}
	for _, meta := range plan {
		for _, path := range imports[meta.PkgPath] {
			switch {
			case path == "unsafe":
			case path == "C":
				return nil, "cgo"
			case rootPaths[path]:
				return nil, "dependency_between_roots"
			case st.meta[path] == nil:
				return nil, "import_added"
			}
		}
	}
	stats.SourceDependencies = len(plan)
	for _, meta := range plan {
		stats.SourceDependencyFiles += len(meta.CompiledGoFiles)
	}
	return &sourceDependencies{pkgs: plan, syntax: syntax, parseErrs: parseErrs, imports: imports}, ""
}

// merge adds the source dependencies' syntax, parse errors and imports to
// the roots' and returns the packages to type-check: the dependencies
// first, then the roots (typecheckRoots orders them by import).
func (s *sourceDependencies) merge(roots []*packages.Package, syntax map[string][]*ast.File, parseErrs map[string][]error, imports map[string][]string) []*packages.Package {
	for path, files := range s.syntax {
		syntax[path] = files
	}
	for path, errs := range s.parseErrs {
		parseErrs[path] = errs
	}
	for path, imps := range s.imports {
		imports[path] = imps
	}
	out := make([]*packages.Package, 0, len(s.pkgs)+len(roots))
	out = append(out, s.pkgs...)
	return append(out, roots...)
}

// rootsOnly returns the roots' packages from a check of merge's list. A
// root's import of a source dependency is replaced by the dependency's
// metadata, as for every other dependency, so the pass sees exactly the
// packages it asked for.
func (s *sourceDependencies) rootsOnly(checked []*packages.Package, meta map[string]*packages.Package) []*packages.Package {
	source := make(map[string]bool, len(s.pkgs))
	for _, pkg := range s.pkgs {
		source[pkg.PkgPath] = true
	}
	roots := checked[len(s.pkgs):]
	for _, root := range roots {
		for path := range root.Imports {
			if source[path] && meta[path] != nil {
				root.Imports[path] = meta[path]
			}
		}
	}
	return roots
}
