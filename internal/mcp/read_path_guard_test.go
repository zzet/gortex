package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

// reindexSinks reindex synchronously. A read handler that reaches one waits
// behind the edit pipeline and repeats the watcher's work for the same file.
var reindexSinks = map[string]bool{
	"reindexFile":             true,
	"IncrementalReindexRepo":  true,
	"IncrementalReindexPaths": true,
}

// readPathGuardEnforced turns the guard from a report into a gate once read
// handlers read through pending edits instead of reindexing.
const readPathGuardEnforced = false

// packageRefs maps every function declared in files, by bare name, to the
// names its body references, called or passed as a value, and every tool
// registered with addTool or addControlTool, as "tool:<name>", to the names
// its handler expression references. Methods of different types that share a
// name merge, which can only over-report a path.
func packageRefs(files map[string]*ast.File) map[string]map[string]bool {
	refs := make(map[string]map[string]bool)
	collect := func(key string, n ast.Node) {
		set := refs[key]
		if set == nil {
			set = make(map[string]bool)
			refs[key] = set
		}
		ast.Inspect(n, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok {
				set[ident.Name] = true
			}
			return true
		})
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			collect(fn.Name.Name, fn.Body)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				register, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (register.Sel.Name != "addTool" && register.Sel.Name != "addControlTool") {
					return true
				}
				tool, ok := call.Args[0].(*ast.CallExpr)
				if !ok || len(tool.Args) == 0 {
					return true
				}
				lit, ok := tool.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if name, err := strconv.Unquote(lit.Value); err == nil {
					collect("tool:"+name, call.Args[1])
				}
				return true
			})
		}
	}
	return refs
}

// readReindexPaths returns, for every read tool whose handler can reach a
// reindex sink, the shortest reference chain to it.
func readReindexPaths(files map[string]*ast.File, isRead func(tool string) bool) []string {
	refs := packageRefs(files)
	var out []string
	for key := range refs {
		tool, ok := strings.CutPrefix(key, "tool:")
		if !ok || !isRead(tool) {
			continue
		}
		parent := map[string]string{key: ""}
		queue := []string{key}
	search:
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			names := make([]string, 0, len(refs[cur]))
			for name := range refs[cur] {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if reindexSinks[name] {
					chain := []string{name}
					for at := cur; at != ""; at = parent[at] {
						chain = append(chain, at)
					}
					slices.Reverse(chain)
					out = append(out, strings.Join(chain, " -> "))
					break search
				}
				if _, declared := refs[name]; !declared {
					continue
				}
				if _, seen := parent[name]; !seen {
					parent[name] = cur
					queue = append(queue, name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestReadHandlersNeverReachReindex lists the read tools whose handlers can
// reach a synchronous reindex. It reports until reads go through pending
// edits, then gates.
func TestReadHandlersNeverReachReindex(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files[path] = file
	}
	isRead := func(tool string) bool { return !daemon.IsEffectful(tool) }
	violations := readReindexPaths(files, isRead)
	for _, violation := range violations {
		t.Log("read handler reaches a reindex: " + violation)
	}
	if readPathGuardEnforced {
		require.Empty(t, violations)
	}

	// The guard must follow a read handler through a helper and leave
	// mutating tools alone.
	fixture, err := parser.ParseFile(fset, "fixture.go", `package mcp
func (s *Server) registerFixture() {
	s.addTool(mcp.NewTool("read_fixture"), s.handleReadFixture)
	s.addTool(mcp.NewTool("edit_fixture"), s.handleEditFixture)
}
func (s *Server) handleReadFixture() { s.refresh() }
func (s *Server) refresh()           { s.reindexFile("x") }
func (s *Server) handleEditFixture() { s.reindexFile("x") }
`, parser.SkipObjectResolution)
	require.NoError(t, err)
	require.Equal(t, []string{"tool:read_fixture -> handleReadFixture -> refresh -> reindexFile"},
		readReindexPaths(map[string]*ast.File{"fixture.go": fixture}, func(tool string) bool { return tool == "read_fixture" }))
}
