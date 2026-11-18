package goanalysis

import (
	"context"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

// moduleProbeBudget bounds the directory stats the admission probe may spend:
// the gate must stay microseconds-cheap even on a pathological tree, and a
// repo whose first ~2k directories hide no manifest is not a Go module in any
// sense this provider can serve.
const moduleProbeBudget = 2048

// moduleProbeSkippedDirs are directory names that must never vouch for a Go
// module: dependency trees (whose go.mod belongs to someone else) and fixture
// corpora (which the go tool itself ignores).
var moduleProbeSkippedDirs = map[string]struct{}{
	"vendor":       {},
	"node_modules": {},
	"third_party":  {},
	"testdata":     {},
}

// goModulePresent reports whether repoRoot — or a directory at most two
// levels below it — carries a go.mod or go.work. This is the admission
// condition for running go/packages: without a manifest in reach, "./..."
// falls back to a GOPATH-mode scan of the entire repository, which on a
// non-Go tree costs minutes and yields nothing.
func goModulePresent(root string) bool {
	return len(goModuleRoots(root)) > 0
}

// goModuleRoots returns every directory — root itself, or one at most two
// levels below it — that carries a go.mod or go.work, shallowest first and
// otherwise in directory order.
//
// goModulePresent only answers whether a manifest is in reach. Returning the
// locations is what lets the caller run go/packages in the directory that
// actually owns the module. A repository whose Go code lives in a
// subdirectory (services/foo, backend/, src/go/) is a healthy module; only
// "./..." evaluated from the repository root is meaningless for it, and that
// is an argument about where to run, not about whether the module resolves.
//
// The walk shares moduleProbeBudget with the boolean form, so the cost stays
// bounded on a pathological tree.
func goModuleRoots(root string) []string {
	var roots []string
	if hasGoManifest(root) {
		roots = append(roots, root)
	}
	budget := moduleProbeBudget
	level1 := listProbeSubdirs(root, &budget)
	for _, dir := range level1 {
		if hasGoManifest(dir) {
			roots = append(roots, dir)
		}
	}
	for _, dir := range level1 {
		for _, sub := range listProbeSubdirs(dir, &budget) {
			if hasGoManifest(sub) {
				roots = append(roots, sub)
			}
		}
	}
	return roots
}

// goLoadDir reports the directory go/packages must run in for root, and how
// many module roots were found.
//
// A manifest at the repository root keeps the historical behaviour exactly.
// Otherwise the single module below the root is the only directory in which
// "./..." names that module's packages. When several modules sit below the
// root and none is at it there is no single correct directory, so the caller
// degrades — as it already did, but now for a reason that names the shape
// instead of reporting the module as unloadable.
func goLoadDir(root string) (dir string, moduleRoots int) {
	roots := goModuleRoots(root)
	switch {
	case len(roots) == 0:
		return root, 0
	case hasGoManifest(root), len(roots) == 1:
		return roots[0], len(roots)
	default:
		return root, len(roots)
	}
}

func hasGoManifest(dir string) bool {
	for _, manifest := range []string{"go.mod", "go.work"} {
		if info, err := os.Stat(filepath.Join(dir, manifest)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func listProbeSubdirs(dir string, budget *int) []string {
	if *budget <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	subdirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if *budget <= 0 {
			break
		}
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if _, skip := moduleProbeSkippedDirs[name]; skip {
			continue
		}
		*budget--
		subdirs = append(subdirs, filepath.Join(dir, name))
	}
	return subdirs
}

// defaultGoTypesProbeTimeout bounds the metadata-only loadability probe. It
// must comfortably exceed a healthy big module's `go list` enumeration (a few
// seconds even with dozens of CGo dependency modules) while capping the cost
// paid on a doomed module. The probe fails OPEN on timeout, so this is a cost
// bound, never a correctness gate.
const defaultGoTypesProbeTimeout = 30 * time.Second

// goTypesProbeTimeout reads GORTEX_GOTYPES_PROBE_TIMEOUT (a Go duration).
// "0"/"off"/"false" disables the probe entirely (never skip). A malformed
// value falls back to the default.
func goTypesProbeTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("GORTEX_GOTYPES_PROBE_TIMEOUT"))
	if v == "" {
		return defaultGoTypesProbeTimeout
	}
	if v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false") {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return defaultGoTypesProbeTimeout
}

// probeGoPackagesLoadable runs a metadata-only go/packages enumeration
// (NeedName|NeedFiles — `go list` with no dependency download, no compile, no
// typecheck) at dir, OFF the heavy gate, under a tight timeout. It answers one
// question cheaply: does this module actually load?
//
// The full go-types pass uses NeedDeps|NeedTypes, which on an unloadable
// module (missing/renamed/private dep, a go.mod that lives in a subdirectory
// so `./...` from the root is out-of-module, or a non-Go repo) grinds through
// MVS build-list construction and GOPROXY download attempts for minutes before
// erroring — holding the single serialized heavyGate slot the whole time and
// yielding zero edges (measured: 300-587s per doomed repo, the head-of-line
// block of the enrichment chain). This probe removes that repo from the
// critical path before it ever acquires the gate.
//
// It fails OPEN on ambiguity: a timeout (which a slow-but-healthy large module
// could hit) or an injected/absent loader returns loadable=true, so a
// productive repo is never skipped. It fails CLOSED only on a clean, fast load
// failure or an all-errored/empty enumeration — the unambiguous doomed case.
func (p *Provider) probeGoPackagesLoadable(ctx context.Context, dir string) (loadable bool, realPkgs, erroredPkgs int) {
	timeout := goTypesProbeTimeout()
	if timeout <= 0 {
		// Probe disabled by config — never skip on the probe's account.
		return true, 0, 0
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg := &packages.Config{
		Context: probeCtx,
		// NeedName|NeedFiles is a metadata-only enumeration: `go list` with no
		// -deps/-compiled/-export, so no dependency download or typecheck.
		Mode:    packages.NeedName | packages.NeedFiles,
		Dir:     dir,
		Tests:   p.includeTest,
		Fset:    token.NewFileSet(),
		Overlay: committedOverlay(probeCtx),
		Env:     committedEnv(probeCtx),
	}
	// Use the same loader field the heavy load uses (packages.Load in
	// production; a test double when injected) so the probe is exercised
	// identically to the real pass.
	load := p.packagesLoad
	if load == nil {
		load = packages.Load
	}
	pkgs, err := load(cfg, "./...")
	if err != nil {
		if probeCtx.Err() != nil {
			// Timed out — ambiguous. A slow-but-healthy big module can hit
			// this; fail open so it proceeds to the real load.
			return true, 0, 0
		}
		// Clean load failure: the module does not resolve. Fail closed.
		return false, 0, 0
	}
	for _, pkg := range pkgs {
		if pkg == nil {
			continue
		}
		if len(pkg.Errors) > 0 || pkg.PkgPath == "" {
			erroredPkgs++
			continue
		}
		realPkgs++
	}
	// Skip only when NOTHING loaded cleanly — the same "every package errored /
	// empty enumeration" signal the post-load degraded warning keys on, but
	// caught before the expensive typecheck.
	return realPkgs > 0, realPkgs, erroredPkgs
}
