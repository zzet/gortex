package review

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// Change classes — the coarse intent of a single changed symbol. The packaged
// review envelope stamps one of these on every changed symbol so a reviewer can
// triage the diff by kind before reading any code.
const (
	ClassTest     = "test"     // the symbol (or its file) is a test
	ClassConfig   = "config"   // a config / constant / variable surface, not behaviour
	ClassFix      = "fix"      // a bug fix — error handling, guards, nil checks added
	ClassRefactor = "refactor" // a pure restructuring — no added/removed lines, body shuffled
	ClassFeature  = "feature"  // new behaviour — added lines that are not a fix/refactor
)

// ClassifyChange assigns a coarse change class to one changed symbol from its
// graph node kind, its file path, and the rendered +/- hunk text. The order of
// the checks is the precedence: test and config are structural (decided by the
// path / node kind), then the hunk is inspected — a fix (error-handling /
// guarding signal) outranks a feature, and a hunk with no net line change is a
// refactor. It is deterministic and graph-grounded but never calls an LLM.
func ClassifyChange(g graph.Reader, sym analysis.ChangedSymbol, hunk string) string {
	file := cleanPath(sym.FilePath)

	// (1) Test: the file is a test file, or the symbol is a test function.
	if isTestPath(file) || isTestSymbol(g, sym) {
		return ClassTest
	}

	// (2) Config: a non-code surface (a constant / variable / config key /
	// field) or a config-shaped file is a configuration change, not behaviour.
	if isConfigPath(file) || isConfigKind(g, sym) {
		return ClassConfig
	}

	added, removed := countHunkLines(hunk)

	// (3) Fix: the change adds error-handling / guarding signal. A fix is the
	// strongest behavioural signal so it outranks a plain feature.
	if added > 0 && hunkLooksLikeFix(hunk) {
		return ClassFix
	}

	// (4) Refactor: the hunk shuffles code without a net line change (equal
	// added / removed, both non-zero) — a restructuring, not new behaviour.
	if added > 0 && added == removed {
		return ClassRefactor
	}

	// (5) Feature: net-new lines that are neither a fix nor a refactor. This is
	// also the fallback when there is no hunk text to inspect.
	if added >= removed {
		return ClassFeature
	}

	// A net deletion with no other signal reads as a refactor (code removed).
	return ClassRefactor
}

// isTestSymbol reports whether the changed symbol is a test function/method by
// its name or its enclosing file.
func isTestSymbol(g graph.Reader, sym analysis.ChangedSymbol) bool {
	if strings.HasPrefix(sym.Name, "Test") || strings.HasPrefix(sym.Name, "Benchmark") || strings.HasPrefix(sym.Name, "Fuzz") {
		return true
	}
	if g != nil && sym.ID != "" {
		if n := g.GetNode(sym.ID); n != nil && isTestPath(cleanPath(n.FilePath)) {
			return true
		}
	}
	return false
}

// isConfigKind reports whether the changed symbol's graph node is a
// non-behavioural surface — a constant, variable, field, enum member, or
// config key.
func isConfigKind(g graph.Reader, sym analysis.ChangedSymbol) bool {
	kind := sym.Kind
	if kind == "" && g != nil && sym.ID != "" {
		if n := g.GetNode(sym.ID); n != nil {
			kind = string(n.Kind)
		}
	}
	switch graph.NodeKind(kind) {
	case graph.KindConstant, graph.KindVariable, graph.KindField,
		graph.KindEnumMember, graph.KindConfigKey:
		return true
	}
	return false
}

// isTestPath reports whether a path is a test file across the common language
// conventions (Go _test.go, *.test.*, *_spec.*, *Test.java, a /tests/ dir).
func isTestPath(file string) bool {
	base := strings.ToLower(filepath.Base(file))
	lower := strings.ToLower(filepath.ToSlash(file))
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.Contains(base, ".test.") || strings.Contains(base, ".spec."):
		return true
	case strings.HasSuffix(base, "_test.py") || strings.HasPrefix(base, "test_"):
		return true
	case strings.HasSuffix(base, "test.java") || strings.HasSuffix(base, "tests.java"):
		return true
	case strings.Contains(lower, "/tests/") || strings.Contains(lower, "/__tests__/"):
		return true
	}
	return false
}

// isConfigPath reports whether a path is a configuration / manifest file by its
// extension or basename.
func isConfigPath(file string) bool {
	base := strings.ToLower(filepath.Base(file))
	switch filepath.Ext(base) {
	case ".yaml", ".yml", ".toml", ".ini", ".env", ".cfg", ".conf", ".properties":
		return true
	}
	switch base {
	case "dockerfile", "makefile", ".gitignore", ".dockerignore":
		return true
	}
	return false
}

// hunkLooksLikeFix reports whether the ADDED lines of a hunk carry an
// error-handling / guarding signal — a strong "this is a bug fix" heuristic. It
// only inspects added (`+`) lines so a fix being removed does not read as a fix.
func hunkLooksLikeFix(hunk string) bool {
	for _, line := range strings.Split(hunk, "\n") {
		if !strings.HasPrefix(line, "+") {
			continue
		}
		body := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "+")))
		if body == "" {
			continue
		}
		for _, kw := range fixKeywords {
			if strings.Contains(body, kw) {
				return true
			}
		}
	}
	return false
}

// fixKeywords are the lower-cased tokens whose presence on an added line marks
// the change as error-handling / guarding — the bug-fix signal.
var fixKeywords = []string{
	"if err",
	"!= nil",
	"== nil",
	"return err",
	"recover()",
	"panic(",
	"errors.",
	"fmt.errorf",
	"try:",
	"except",
	"catch",
	"throw",
	"guard ",
	"nil check",
	"bounds",
	"overflow",
}

// countHunkLines counts the added (`+`) and removed (`-`) content lines of a
// +/- hunk, skipping unified-diff file headers (`+++`, `---`).
func countHunkLines(hunk string) (added, removed int) {
	for _, line := range strings.Split(hunk, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

// VerificationCommand derives the single concrete command a reviewer runs to
// verify the change from the impacted test targets. The targets are the test
// file paths the get-test-targets walk produced; the command runs every
// distinct package directory they live in. Falls back to a whole-tree test run
// when no specific target is known so the command is always runnable.
func VerificationCommand(testTargets []string, lang string) string {
	switch detectLang(lang, testTargets) {
	case "go":
		return goVerificationCommand(testTargets)
	case "python":
		if len(testTargets) == 0 {
			return "pytest"
		}
		return "pytest " + strings.Join(uniqueSorted(testTargets), " ")
	case "javascript", "typescript":
		return "npm test"
	case "rust":
		return "cargo test"
	default:
		// Unknown toolchain: best-effort Go run over the targets (or the whole
		// module when none is known) so the command is always runnable.
		return goVerificationCommand(testTargets)
	}
}

// goVerificationCommand turns a set of Go test files into a `go test` over the
// distinct package directories they live in, e.g. `go test ./internal/svc/...`.
// With no targets it runs the whole module.
func goVerificationCommand(testTargets []string) string {
	dirs := map[string]bool{}
	for _, t := range testTargets {
		t = cleanPath(t)
		if t == "" {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(t))
		if dir == "" || dir == "." {
			dirs["."] = true
			continue
		}
		dirs[dir] = true
	}
	if len(dirs) == 0 {
		return "go test ./..."
	}
	pkgs := make([]string, 0, len(dirs))
	for d := range dirs {
		if d == "." {
			pkgs = append(pkgs, "./...")
			continue
		}
		pkgs = append(pkgs, "./"+d+"/...")
	}
	sort.Strings(pkgs)
	return "go test " + strings.Join(pkgs, " ")
}

// detectLang resolves the toolchain for the verification command: an explicit
// language hint wins, else it is inferred from the test target extensions.
func detectLang(lang string, testTargets []string) string {
	if l := normalizeLang(lang); l != "" {
		return l
	}
	for _, t := range testTargets {
		switch strings.ToLower(filepath.Ext(t)) {
		case ".go":
			return "go"
		case ".py":
			return "python"
		case ".ts", ".tsx":
			return "typescript"
		case ".js", ".jsx":
			return "javascript"
		case ".rs":
			return "rust"
		}
	}
	return ""
}

func normalizeLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "go", "golang":
		return "go"
	case "python", "py":
		return "python"
	case "ts", "typescript":
		return "typescript"
	case "js", "javascript":
		return "javascript"
	case "rust", "rs":
		return "rust"
	}
	return ""
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = cleanPath(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
