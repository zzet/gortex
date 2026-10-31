package mcp

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sourceWriteChokePoint is the one function allowed to write or remove source
// files in this package: it refuses dead requests and records the commit, so
// every source write is ordered against the other writers of the same file.
const sourceWriteChokePoint = "Server.commitFileMutation"

// sourceWriteCensusEnforced turns the census from a report into a gate once
// every source writer routes through the choke point.
const sourceWriteCensusEnforced = false

// nonSourceWriters may reference a file writer without the choke point: they
// write tool outputs, transaction journals and spill files, or they are a
// write primitive themselves (references to those still count).
var nonSourceWriters = map[string]string{
	"durableAtomicWriteFile":      "the durable write primitive",
	"Server.persistBatchManifest": "batch transaction manifest",
	"Server.prepareBatchJournal":  "batch transaction backups",
	"Server.rollbackBatchReceipt": "restores a failed batch from its backups",
	"symbolPageStorage.close":     "symbol page spill directory",
	"symbolPageStorage.spill":     "symbol page spill directory",
	"Server.handleExportGraph":    "export output path",
	"Server.handleGenerateSkill":  "skill files",
	"writeWikiFile":               "wiki output",
}

// sourceWriteSink names the file writer n refers to, called or passed as a
// value, or returns "".
func sourceWriteSink(n ast.Node) string {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	switch x := sel.X.(type) {
	case *ast.Ident:
		switch {
		case x.Name == "os" && (sel.Sel.Name == "WriteFile" || sel.Sel.Name == "Remove"):
			return "os." + sel.Sel.Name
		case x.Name == "agents" && sel.Sel.Name == "AtomicWriteFile":
			return "agents.AtomicWriteFile"
		}
	case *ast.CallExpr:
		if inner, ok := x.Fun.(*ast.SelectorExpr); ok && inner.Sel.Name == "batchDurability" && sel.Sel.Name == "writeFile" {
			return "batchDurability().writeFile"
		}
	}
	return ""
}

// funcKey names a declaration as Type.method or func.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// sourceWriteBypasses lists every file-writer reference made outside the
// choke point and the non-source writers, as "file: func -> sink".
func sourceWriteBypasses(files map[string]*ast.File) []string {
	var out []string
	for name, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := funcKey(fn)
			if _, allowed := nonSourceWriters[key]; allowed || key == sourceWriteChokePoint {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sink := sourceWriteSink(n); sink != "" {
					out = append(out, fmt.Sprintf("%s: %s -> %s", name, key, sink))
				}
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}

// TestSourceWriteChokePointCensus lists the source writers that bypass the
// choke point. It reports until every writer is routed, then gates.
func TestSourceWriteChokePointCensus(t *testing.T) {
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
	bypasses := sourceWriteBypasses(files)
	for _, bypass := range bypasses {
		t.Log("source write outside the choke point: " + bypass)
	}
	if sourceWriteCensusEnforced {
		require.Empty(t, bypasses, "route these writers through %s", sourceWriteChokePoint)
	}

	// The census must see a writer passed as a value as well as a call, and
	// must not count the choke point or its callers.
	fixture, err := parser.ParseFile(fset, "fixture.go", `package mcp
func (s *Server) commitFileMutation() { agents.AtomicWriteFile() }
func (s *Server) handleRouted() { s.commitFileMutation() }
func (s *Server) handleFixture() {
	write := s.batchDurability().writeFile
	_ = write
	os.Remove("x")
}
`, parser.SkipObjectResolution)
	require.NoError(t, err)
	require.Equal(t, []string{
		"fixture.go: Server.handleFixture -> batchDurability().writeFile",
		"fixture.go: Server.handleFixture -> os.Remove",
	}, sourceWriteBypasses(map[string]*ast.File{"fixture.go": fixture}))
}
