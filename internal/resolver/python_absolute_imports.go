package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Python absolute imports reach resolveExtern as a dotted module path:
//
//	import a.b as m; m.f()        → extern::a.b::f
//	from a import b; b.f()        → extern::a.b::f
//	from a.b import f; f()        → extern::a.b.f::f
//	from a.b import f as g; g()   → extern::a.b.f::g
//
// resolveExtern's directory match is Go-shaped (the import path names the
// candidate's directory) and never lines up with a dotted Python module,
// so every call through an absolute import of a first-party module used to
// fall through to a `dep::` stub — invisible to get_callers, dead-code and
// test-coverage analysis. resolvePythonModuleExtern lands those calls on the
// module-level function or class the import names.

// isPythonSourcePath reports whether path is a Python source or stub file.
func isPythonSourcePath(path string) bool {
	return strings.HasSuffix(path, ".py") || strings.HasSuffix(path, ".pyi")
}

// isDottedPythonModule reports whether importPath is shaped like a Python
// module path (dotted identifiers, no slashes).
func isDottedPythonModule(importPath string) bool {
	return importPath != "" && !strings.ContainsAny(importPath, "/:\\")
}

// pythonImportedName returns the attribute a `from a.b import f` binding
// names — the last segment of the dotted import path — or "" when the path
// has a single segment.
func pythonImportedName(importPath string) string {
	if i := strings.LastIndex(importPath, "."); i > 0 && i < len(importPath)-1 {
		return importPath[i+1:]
	}
	return ""
}

// pythonModuleFileMatches reports whether filePath is the source file of the
// dotted module: `a/b.py`, `a/b.pyi` or `a/b/__init__.py`, either at the path
// root or below a source root / repo prefix (`src/a/b.py`, `repo/src/a/b.py`).
func pythonModuleFileMatches(filePath, module string) bool {
	if filePath == "" || module == "" {
		return false
	}
	stem := strings.ReplaceAll(module, ".", "/")
	for _, suffix := range []string{".py", ".pyi", "/__init__.py", "/__init__.pyi"} {
		want := stem + suffix
		if filePath == want || strings.HasSuffix(filePath, "/"+want) {
			return true
		}
	}
	return false
}

// resolvePythonModuleExtern resolves a Python caller's extern edge onto the
// module-level function or class it imports. It tries the two readings of a
// dotted import path in turn — the symbol defined in the module importPath
// names (`m.f()`), then the path's last segment defined in its parent module
// (`from a.b import f [as g]`) — and resolves only on a unique match, so an
// ambiguous layout keeps the previous behaviour. Methods never match: an
// import binds module attributes, not class members.
func (r *Resolver) resolvePythonModuleExtern(e *graph.Edge, importPath, symbol, callerRepo string, stats *ResolveStats) bool {
	if !isPythonSourcePath(e.FilePath) || !isDottedPythonModule(importPath) {
		return false
	}
	type reading struct{ module, name string }
	readings := []reading{{importPath, symbol}}
	if name := pythonImportedName(importPath); name != "" {
		readings = append(readings, reading{importPath[:len(importPath)-len(name)-1], name})
	}
	for _, rd := range readings {
		candidates, err := r.cachedFindExternNodesByName(rd.name, e)
		if err != nil {
			return false
		}
		var match *graph.Node
		ambiguous := false
		for _, c := range candidates {
			if c.Kind != graph.KindFunction && c.Kind != graph.KindType {
				continue
			}
			if !pythonModuleFileMatches(c.FilePath, rd.module) {
				continue
			}
			switch {
			case match == nil:
				match = c
			case match.RepoPrefix != callerRepo && c.RepoPrefix == callerRepo:
				// Prefer the caller's own repo over another checkout of
				// the same module path.
				match, ambiguous = c, false
			case (match.RepoPrefix == callerRepo) == (c.RepoPrefix == callerRepo):
				ambiguous = true
			}
		}
		if match == nil || ambiguous {
			continue
		}
		e.To = match.ID
		if callerRepo != "" && match.RepoPrefix != "" && match.RepoPrefix != callerRepo {
			e.CrossRepo = true
		}
		stats.Resolved++
		return true
	}
	return false
}
