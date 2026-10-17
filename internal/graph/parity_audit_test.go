package graph

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Schema-vs-extractor parity audit.
//
// The graph package declares ~30 NodeKinds and ~55 EdgeKinds. Without
// a regression fence, the schema drifts: a new kind gets added with a
// detailed comment, but nobody wires the emitter, and the constant
// quietly outlives its purpose. We've seen this exact failure mode
// before — `KindRelease` was declared and documented for many releases
// before any extractor actually instantiated it.
//
// TestSchemaParityAudit walks every Go source file under the repo
// (excluding worktrees, vendor, build artifacts) and asserts that
// every declared NodeKind and EdgeKind constant is referenced from
// at least one file outside the declaring file (node.go / edge.go).
//
// "Referenced" here means the bare identifier appears in production
// source — the resolver, an extractor, an enricher, an analyzer, or
// a downstream consumer. Test-only references count too: a test that
// exercises the consumer is still proof the kind has a real role.
//
// When the audit fails the failure message names the orphan constant
// and points at the file where it was declared so the next sweep
// either wires the emitter or removes the constant. Reserved kinds
// (declared today, emitter coming next sprint) go on the explicit
// allowlist with a documented reason — that turns "we forgot" into
// "we deliberately deferred."
func TestSchemaParityAudit(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	declared, err := extractKindDeclarations(filepath.Join(repoRoot, "internal", "graph"))
	if err != nil {
		t.Fatalf("scan declarations: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("scan returned zero declarations — audit is broken")
	}

	usage, err := scanKindUsage(repoRoot, declared)
	if err != nil {
		t.Fatalf("scan usage: %v", err)
	}

	var orphans []string
	for _, d := range declared {
		if reservedKinds[d.Name] {
			continue
		}
		// "Direct" usage means we found the constant referenced
		// anywhere outside its own declaration file. Mapping-table
		// references inside graph/edge.go (CrossRepoKindFor etc.)
		// count because they prove the constant is reachable from
		// production query paths — the actual emit site can then sit
		// in a resolver or enricher that calls the mapping fn.
		if usage[d.Name] == 0 {
			orphans = append(orphans, formatOrphan(d))
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf("schema-parity audit found %d orphan kind(s) — wire emitter or remove from schema:\n\n%s\n\n"+
			"Add to `reservedKinds` in this file with a documented reason if the gap is intentional.",
			len(orphans), strings.Join(orphans, "\n"))
	}
}

// reservedKinds is the explicit allowlist of constants that are
// declared today and intentionally lack a production emit site. Every
// entry must carry a comment explaining the reason — "we forgot" is
// not a valid reason; the answer is to either ship the emitter or
// delete the declaration.
var reservedKinds = map[string]bool{
	// (empty — every declared kind currently has a production emit
	// site. Future deferrals go here with a documented reason.)
}

// kindDecl is a single NodeKind / EdgeKind constant declaration we
// extracted from internal/graph. The Path / Line locate it for the
// failure message so the next sweep knows where to look.
type kindDecl struct {
	Name string
	Kind string // "NodeKind" or "EdgeKind"
	Path string
	Line int
}

// extractKindDeclarations parses every .go file under the given
// directory and returns each `const Name SomeKind = ...` whose RHS
// type is NodeKind or EdgeKind. Anything else is ignored — the audit
// is intentionally scoped to schema kinds.
func extractKindDeclarations(dir string) ([]kindDecl, error) {
	var out []kindDecl
	fset := token.NewFileSet()

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				typeName, ok := simpleTypeName(vs.Type)
				if !ok {
					continue
				}
				if typeName != "NodeKind" && typeName != "EdgeKind" {
					continue
				}
				for _, ident := range vs.Names {
					pos := fset.Position(ident.NamePos)
					out = append(out, kindDecl{
						Name: ident.Name,
						Kind: typeName,
						Path: path,
						Line: pos.Line,
					})
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// simpleTypeName pulls the identifier name out of a simple type
// expression. Returns false for any expression that isn't a bare
// identifier — qualified names, function types, etc. — because the
// audit only cares about the local NodeKind / EdgeKind types.
func simpleTypeName(expr ast.Expr) (string, bool) {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// scanKindUsage counts how many references to each declared constant
// exist outside its declaring file. The scanner is regex-based
// (rather than AST-based) because the audit needs to catch every
// reachable mention — including string-literal mappings and test
// fixtures — without paying the parse cost of every .go file in the
// repo. Word-boundary anchors keep "EdgeReads" and "EdgeReadsCol"
// (and "EdgeReadsConfig") distinct.
func scanKindUsage(repoRoot string, declared []kindDecl) (map[string]int, error) {
	declaringFiles := make(map[string]string, len(declared))
	for _, d := range declared {
		declaringFiles[d.Name] = d.Path
	}

	// One combined alternation rather than one regex per kind. Matching
	// every .go file in the repo against ~100 separate \bName\b patterns
	// is O(files × kinds) and grows past the CI timeout as both the tree
	// and the schema grow; a single \b(?:Name1|Name2|…)\b scanned once per
	// file is O(files). The \b anchors keep the identifiers as distinct as
	// before — `EdgeReads` still does NOT match inside `EdgeReadsCol`,
	// because the inner \b fails between two word characters and the
	// engine falls through to the longer alternative.
	names := make([]string, 0, len(declared))
	seen := make(map[string]bool, len(declared))
	for _, d := range declared {
		if seen[d.Name] {
			continue
		}
		seen[d.Name] = true
		names = append(names, regexp.QuoteMeta(d.Name))
	}
	re := regexp.MustCompile(`\b(?:` + strings.Join(names, "|") + `)\b`)

	usage := make(map[string]int, len(declared))

	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			// Skip vendor, build caches, worktrees, eval venvs,
			// transcript dumps — none of those carry production
			// emit sites.
			switch base {
			case ".git", "vendor", "node_modules", ".claude", ".cache",
				".venv", "venv", "build", "dist", "debug":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Worktrees can hide a duplicate copy of the source tree
		// under .claude/worktrees/* — already skipped by the .claude
		// directory rule above; defensive double-check. Match against
		// the repo-relative path: the checkout ITSELF often lives under
		// a `worktrees/` directory, and testing the absolute path there
		// skips every file in the repo, orphaning all 108 kinds.
		if rel, relErr := filepath.Rel(repoRoot, path); relErr == nil &&
			strings.Contains(filepath.ToSlash(rel), "worktrees/") {
			return nil
		}
		// Don't let the audit's own file count as a reference — it
		// names every constant in its failure-message format so a
		// naive regex would mark every kind as "used."
		if strings.HasSuffix(path, "/parity_audit_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // unreadable files don't fail the audit
		}
		text := stripGoComments(string(data))
		found := make(map[string]bool)
		for _, m := range re.FindAllString(text, -1) {
			found[m] = true
		}
		for name := range found {
			// A constant referenced only inside its own declaring file
			// is not a real consumer — exclude it exactly as the
			// per-pattern scan did.
			if path == declaringFiles[name] {
				continue
			}
			usage[name]++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return usage, nil
}

// findRepoRoot first preserves the package-CWD lookup. An isolated test
// process can instead audit the actual checkout that supplied this source
// file to its binary; no process-CWD or configuration change is needed.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if root, err := findRepoRootFromDir(dir); err == nil {
		return root, nil
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(source) {
		return "", os.ErrNotExist
	}
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() {
		return "", os.ErrNotExist
	}
	return findRepoRootFromDir(filepath.Dir(source))
}

// findRepoRootFromDir retains the original go.mod predicate and upward walk.
func findRepoRootFromDir(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func TestFindRepoRootFromDirUsesActualAuditDirectory(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "internal", "graph", "parity_audit_test.go")
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("actual audit source is not a real file: %v", err)
	}
	fromSource, err := findRepoRootFromDir(filepath.Dir(source))
	if err != nil || fromSource != root {
		t.Fatalf("source-directory lookup returned %q, want %q: %v", fromSource, root, err)
	}
}

func formatOrphan(d kindDecl) string {
	rel := d.Path
	if root, err := findRepoRoot(); err == nil {
		if r, err := filepath.Rel(root, d.Path); err == nil {
			rel = r
		}
	}
	return "  " + d.Kind + "." + d.Name + " (declared at " + rel + ":" + itoa(d.Line) + ")"
}

// stripGoComments removes line and block comments from Go source so
// the usage scan only counts identifiers that actually appear in
// executable code. Without this a constant mentioned in a docstring
// elsewhere in the tree would falsely satisfy the parity audit.
//
// The implementation is intentionally simple — character-level scan
// with a small state machine — because the alternative (full Go
// parsing) is overkill for the audit's needs and slow when invoked
// across every .go file in the repo. Strings (single- and
// double-quoted, plus raw backtick) are preserved verbatim so a
// string literal containing "//" isn't mistaken for a comment.
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	const (
		stateCode = iota
		stateLineComment
		stateBlockComment
		stateString
		stateRawString
		stateRune
	)
	state := stateCode
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case stateCode:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = stateLineComment
				i++
				b.WriteByte(' ')
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = stateBlockComment
				i++
				b.WriteByte(' ')
			case c == '"':
				state = stateString
				b.WriteByte(c)
			case c == '`':
				state = stateRawString
				b.WriteByte(c)
			case c == '\'':
				state = stateRune
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
		case stateLineComment:
			if c == '\n' {
				state = stateCode
				b.WriteByte(c)
			}
		case stateBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = stateCode
				i++
				b.WriteByte(' ')
			}
		case stateString:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i++
			} else if c == '"' {
				state = stateCode
			}
		case stateRawString:
			b.WriteByte(c)
			if c == '`' {
				state = stateCode
			}
		case stateRune:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i++
			} else if c == '\'' {
				state = stateCode
			}
		}
	}
	return b.String()
}

func itoa(n int) string {
	// Tiny helper so the audit file doesn't need strconv just for one
	// formatter. Keeps the import set minimal — easier to read at a
	// glance.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
