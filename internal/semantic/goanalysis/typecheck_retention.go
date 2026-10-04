package goanalysis

import (
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/semantic"
)

// Retention of a checkout's compiler state across deltas.
//
// An edit is followed by a pass over the edited package (the handle root).
// The next edit of the same package must re-check only that package, with
// every import served from the retained state. Three things used to take
// that state away between two touches of one package:
//
//   - a listing that ran while another package of the checkout did not
//     compile (an edit in flight) returns that package, and every package
//     importing it, without export data, and a cgo importer without its
//     generated files. Merged over good retained metadata it made the next
//     pass list the closure again (a stale manifest) or hand the pass to
//     the plain load (import "C" in a file compiled as-is). keepRetained
//     keeps retained metadata that still describes its directory whenever
//     the listing's entry is degraded, and a background warm-up never
//     replaces valid retained metadata a pass relies on;
//   - a merged listing whose export file for one package changed dropped
//     every retained type of the checkout. invalidateTypes drops only that
//     package's types and its importers';
//   - the memory cap dropped a checkout's whole type state, and ordered
//     checkouts by any use, a warm-up included. The cap (below) orders by
//     pass touch and never evicts the working set of the pass that just
//     ran.
//
// Memory cap policy (enforceTypecheckBudget), applied after every cached
// pass while the pass holds its checkout's state:
//
//  1. other checkouts' retained types are dropped whole, the checkout a pass
//     touched least recently first (a checkout only a warm-up used goes
//     before any a pass touched);
//  2. then the current checkout's retained syntax outside the pass's
//     working set, the files of the least recently touched package first;
//  3. then the current checkout's retained dependency types outside the
//     working set, least recently touched first, a package only after every
//     retained package importing it (a package's types refer to its
//     imports' objects, so an importer never outlives its import);
//  4. only when the working set alone exceeds the cap, the current
//     checkout's types are dropped whole (the pass then reports
//     StateEvicted).
//
// The working set of a pass is its roots, every package in their listed
// closure, and the dependencies it checked from source (with their files):
// exactly what the next delta over the same roots reads.

// keepRetained reports whether the state keeps its retained metadata of
// pkg's path instead of the listing's entry: always when a background
// warm-up would replace valid metadata of a package a pass touched (or
// whose retained export data is exact), and for any listing whose entry is
// degraded while the retained metadata is valid.
func (st *checkoutTypecheckState) keepRetained(pkg *packages.Package, l *tcListing) bool {
	prev := st.meta[pkg.PkgPath]
	if prev == nil {
		return false
	}
	degraded := listingEntryDegraded(pkg, l.manifests[pkg.PkgPath])
	if !l.warmup && !degraded {
		return false
	}
	valid, contentChanged := st.retainedValid(prev)
	if !valid {
		return false
	}
	if l.warmup && !degraded && contentChanged && st.touch[pkg.PkgPath] == 0 {
		// No pass relies on this package's retained types: the warm-up's
		// exact export data serves better than checking it from source.
		return false
	}
	return true
}

// listingEntryDegraded reports a listing entry that cannot serve a pass as
// well as valid retained metadata: a mutable package without export data
// (it, or a dependency, did not compile while the go command ran; a cgo
// package then also lacks its generated files), or one whose files changed
// while the go command ran.
func listingEntryDegraded(pkg *packages.Package, m *tcManifest) bool {
	if !mutablePackage(pkg) {
		return false
	}
	return pkg.ExportFile == "" || (m != nil && m.stale)
}

// retainedValid reports whether retained metadata still serves a pass: it
// has export data that is not stale, and (for a mutable package) its
// directory still builds the listed files. contentChanged reports that the
// files' content changed since (a pass checks the package from source); a
// cgo package whose cgo sources changed is not valid.
func (st *checkoutTypecheckState) retainedValid(prev *packages.Package) (valid, contentChanged bool) {
	if prev.ExportFile == "" || st.exportStale[prev.PkgPath] {
		return false, false
	}
	if !mutablePackage(prev) {
		return true, false
	}
	m := st.manifests[prev.PkgPath]
	if m == nil || m.stale || prev.Dir == "" {
		return false, false
	}
	scratch := &semantic.CompilerCacheStats{}
	switch m.dependencyState(prev.Dir, scratch) {
	case dependencyUnchanged:
		return true, false
	case dependencyContentChanged:
		if !sameFiles(prev.CompiledGoFiles, prev.GoFiles) && !m.unchangedCgo(prev, scratch) {
			return false, true
		}
		return true, true
	}
	return false, false
}

// invalidateTypes drops the retained types of the invalid packages and of
// every retained package importing one of them, directly or not (over the
// current metadata and the metadata the merge replaced or dropped), and of
// any retained package without metadata. Every other package's types,
// the retained syntax and the FileSet stay. It returns the packages
// dropped.
func (st *checkoutTypecheckState) invalidateTypes(invalid map[string]bool, replaced map[string]*packages.Package) int {
	if len(invalid) == 0 || len(st.view) == 0 {
		return 0
	}
	importers := map[string][]string{}
	edges := func(pkg *packages.Package) {
		for _, imp := range pkg.Imports {
			if imp != nil && imp.PkgPath != "" {
				importers[imp.PkgPath] = append(importers[imp.PkgPath], pkg.PkgPath)
			}
		}
	}
	for _, pkg := range st.meta {
		edges(pkg)
	}
	for _, pkg := range replaced {
		if pkg != nil {
			edges(pkg)
		}
	}
	drop := make(map[string]bool, len(invalid))
	queue := make([]string, 0, len(invalid))
	for path := range invalid {
		drop[path] = true
		queue = append(queue, path)
	}
	for len(queue) > 0 {
		path := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, importer := range importers[path] {
			if !drop[importer] {
				drop[importer] = true
				queue = append(queue, importer)
			}
		}
	}
	for path := range st.view {
		if st.meta[path] == nil {
			drop[path] = true
		}
	}
	n := 0
	for path := range drop {
		if st.dropViewPackage(path) {
			n++
		}
	}
	return n
}

// dropViewPackage forgets one package's retained types.
func (st *checkoutTypecheckState) dropViewPackage(path string) bool {
	if st.view[path] == nil {
		return false
	}
	delete(st.view, path)
	delete(st.viewExport, path)
	st.exportBytes -= st.viewBytes[path]
	delete(st.viewBytes, path)
	return true
}

// brokenDependencyUnchanged reports whether a mutable package in root's
// listed closure has no export data and still has exactly the files it had
// when listed: listing again would not compile it either.
func (st *checkoutTypecheckState) brokenDependencyUnchanged(root *packages.Package, vc *semantic.CompilerCacheStats) bool {
	seen := map[string]bool{root.PkgPath: true}
	var queue []string
	for path := range root.Imports {
		queue = append(queue, path)
	}
	for len(queue) > 0 {
		path := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[path] {
			continue
		}
		seen[path] = true
		meta := st.meta[path]
		if meta == nil {
			continue
		}
		if mutablePackage(meta) && meta.ExportFile == "" && meta.Dir != "" &&
			st.manifests[path].dependencyState(meta.Dir, vc) == dependencyUnchanged {
			return true
		}
		for imp := range meta.Imports {
			queue = append(queue, imp)
		}
	}
	return false
}

// covers reports whether every given package is checked from source.
func (s *sourceDependencies) covers(paths []string) bool {
	if len(paths) == 0 {
		return true
	}
	if s == nil {
		return false
	}
	in := make(map[string]bool, len(s.pkgs))
	for _, pkg := range s.pkgs {
		in[pkg.PkgPath] = true
	}
	for _, path := range paths {
		if !in[path] {
			return false
		}
	}
	return true
}

// workingSet is a pass's working set: its package paths and the files the
// pass parsed for its roots and source dependencies.
type workingSet struct {
	packages map[string]bool
	files    map[string]bool
}

// touchWorkingSet advances the pass clock and touches the pass's working
// set: the roots, their listed closure and the source dependencies. The
// caller holds st.mu.
func (st *checkoutTypecheckState) touchWorkingSet(roots []*packages.Package, source *sourceDependencies) workingSet {
	if st.touch == nil {
		st.touch = map[string]uint64{}
	}
	st.clock++
	ws := workingSet{packages: map[string]bool{}, files: map[string]bool{}}
	var visit func(path string)
	visit = func(path string) {
		if path == "" || ws.packages[path] {
			return
		}
		ws.packages[path] = true
		st.touch[path] = st.clock
		meta := st.meta[path]
		if meta == nil {
			return
		}
		for _, imp := range meta.Imports {
			if imp != nil {
				visit(imp.PkgPath)
			}
		}
	}
	addFiles := func(pkg *packages.Package) {
		for _, f := range pkg.CompiledGoFiles {
			ws.files[filepath.Clean(f)] = true
		}
	}
	for _, root := range roots {
		visit(root.PkgPath)
		addFiles(root)
	}
	if source != nil {
		for _, pkg := range source.pkgs {
			visit(pkg.PkgPath)
			addFiles(pkg)
		}
	}
	return ws
}

// markTouched records that a pass used st now.
func (p *Provider) markTouched(st *checkoutTypecheckState) {
	p.tcMu.Lock()
	st.touchedAt = time.Now()
	p.tcMu.Unlock()
}

// lessRecentlyTouched orders checkouts for eviction: the one a pass touched
// least recently first, never-touched ones (only a warm-up used them)
// before any touched one. The caller holds Provider.tcMu.
func lessRecentlyTouched(a, b *checkoutTypecheckState) bool {
	if !a.touchedAt.Equal(b.touchedAt) {
		return a.touchedAt.Before(b.touchedAt)
	}
	return a.lastUsed.Before(b.lastUsed)
}

// evictionResult is what one enforcement of the cap dropped.
type evictionResult struct {
	self     bool
	packages int
	files    int
}

// enforceTypecheckBudget applies the memory cap policy described at the top
// of this file. The caller holds cur.mu; other checkouts' states are dropped
// only when their lock is free (a busy one is skipped this time).
func (p *Provider) enforceTypecheckBudget(cur *checkoutTypecheckState, budget int64, ws workingSet) evictionResult {
	if budget <= 0 {
		budget = defaultTypecheckCacheBytes
	}
	var res evictionResult
	p.tcMu.Lock()
	others := make([]*checkoutTypecheckState, 0, len(p.tcStates))
	for _, st := range p.tcStates {
		if st != cur {
			others = append(others, st)
		}
	}
	sort.SliceStable(others, func(i, j int) bool { return lessRecentlyTouched(others[i], others[j]) })
	p.tcMu.Unlock()
	total := cur.estimatedBytes()
	sizes := make([]int64, len(others))
	for i, st := range others {
		if st.mu.TryLock() {
			sizes[i] = st.estimatedBytes()
			st.mu.Unlock()
		}
		total += sizes[i]
	}
	for i, st := range others {
		if total <= budget {
			break
		}
		if sizes[i] == 0 || !st.mu.TryLock() {
			continue
		}
		st.resetTypes()
		st.mu.Unlock()
		total -= sizes[i]
	}
	if total <= budget {
		return res
	}
	res.files = cur.evictSyntax(ws, &total, budget)
	if total <= budget {
		return res
	}
	res.packages = cur.evictTypes(ws, &total, budget)
	if total > budget {
		cur.resetTypes()
		res.self = true
	}
	return res
}

// evictSyntax drops retained parsed files outside the working set, the
// files of the least recently touched package first, until total fits the
// budget. It returns the files dropped.
func (st *checkoutTypecheckState) evictSyntax(ws workingSet, total *int64, budget int64) int {
	type candidate struct {
		abs   string
		touch uint64
	}
	var cands []candidate
	for abs := range st.parsed {
		if ws.files[abs] {
			continue
		}
		var touch uint64
		if meta := st.byDir[filepath.Dir(abs)]; meta != nil {
			if ws.packages[meta.PkgPath] {
				continue
			}
			touch = st.touch[meta.PkgPath]
		}
		cands = append(cands, candidate{abs: abs, touch: touch})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].touch != cands[j].touch {
			return cands[i].touch < cands[j].touch
		}
		return cands[i].abs < cands[j].abs
	})
	n := 0
	for _, c := range cands {
		if *total <= budget {
			break
		}
		parsed := st.parsed[c.abs]
		if parsed.tokFile != nil {
			st.fset.RemoveFile(parsed.tokFile)
		}
		w := syntaxWeight(parsed)
		st.syntaxBytes -= w
		*total -= w
		delete(st.parsed, c.abs)
		n++
	}
	return n
}

// evictTypes drops retained dependency types outside the working set, least
// recently touched first, each only once no retained package importing it
// is left, until total fits the budget. A retained package without
// metadata is never dropped alone. It returns the packages dropped.
func (st *checkoutTypecheckState) evictTypes(ws workingSet, total *int64, budget int64) int {
	importers := map[string][]string{}
	var cands []string
	for path := range st.view {
		meta := st.meta[path]
		if meta == nil {
			continue
		}
		for _, imp := range meta.Imports {
			if imp != nil && imp.PkgPath != "" && st.view[imp.PkgPath] != nil {
				importers[imp.PkgPath] = append(importers[imp.PkgPath], path)
			}
		}
		if !ws.packages[path] {
			cands = append(cands, path)
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		ti, tj := st.touch[cands[i]], st.touch[cands[j]]
		if ti != tj {
			return ti < tj
		}
		return cands[i] < cands[j]
	})
	n := 0
	for progress := true; progress && *total > budget; {
		progress = false
		for _, path := range cands {
			if *total <= budget {
				break
			}
			if st.view[path] == nil {
				continue
			}
			blocked := false
			for _, importer := range importers[path] {
				if st.view[importer] != nil {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			bytes := st.viewBytes[path]
			if st.dropViewPackage(path) {
				*total -= bytes / typecheckExportDivisor
				n++
				progress = true
			}
		}
	}
	return n
}
