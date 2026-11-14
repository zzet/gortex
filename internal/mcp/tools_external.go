package mcp

import (
	"context"
	"go/ast"
	"go/build"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/elide"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathguard"
)

// externalNode reports whether node is an external (ext::) symbol: a
// stdlib / module-cache object materialised by the goanalysis externals
// pass, not a declaration in an indexed repository.
func externalNode(node *graph.Node) bool {
	if node == nil {
		return false
	}
	v, ok := node.Meta["external"].(bool)
	return ok && v
}

// externalRefusal is the guard message for the mutation paths: an
// external symbol is refused as external and read-only rather than as a
// missing line range; every other line-range miss keeps its existing
// message.
func externalRefusal(node *graph.Node, id, generic string) string {
	if externalNode(node) {
		return "symbol " + id + " is external and read-only"
	}
	return generic
}

// metaInt reads a numeric Meta value. The graph store round-trips Meta as
// JSON, so the same value arrives as int from a fresh enrich pass and
// float64 after a reload.
func metaInt(n *graph.Node, key string) int {
	if n == nil || n.Meta == nil {
		return 0
	}
	switch v := n.Meta[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// externalSourceRoots returns the only trees an external symbol's
// recorded source_file may be served from: the Go standard library and
// the module cache. A var so tests can point it at a fixture tree.
var externalSourceRoots = func() []string {
	roots := []string{runtime.GOROOT()}
	if mod := goModCacheRoot(); mod != "" {
		roots = append(roots, mod)
	}
	return roots
}

// goModCacheRoot mirrors the go command's GOMODCACHE resolution:
// $GOMODCACHE when set, else the first GOPATH entry + /pkg/mod
// (build.Default.GOPATH applies the same default the go command does).
func goModCacheRoot() string {
	if mod := os.Getenv("GOMODCACHE"); mod != "" {
		return mod
	}
	gopath := build.Default.GOPATH
	if gopath == "" {
		return ""
	}
	first := strings.SplitN(gopath, string(filepath.ListSeparator), 2)[0]
	if first == "" {
		return ""
	}
	return filepath.Join(first, "pkg", "mod")
}

// confineExternalSource reports whether path may be served as external
// declaration source: it must sit inside one of the allowed roots and
// must not reach there through a link that leaves the root — the same
// confinement rule pathguard enforces for indexed repositories.
func confineExternalSource(path string) bool {
	for _, root := range externalSourceRoots() {
		if root == "" {
			continue
		}
		if !pathguard.WithinRoot(path, pathguard.ResolveRoot(root)) {
			continue
		}
		return !pathguard.SymlinkEscapes(path, root)
	}
	return false
}

// externalSourceResult is the read-only declaration view of an external
// symbol: the source with its extent when it could be served, or the
// reason it could not.
type externalSourceResult struct {
	source    string
	startLine int
	endLine   int
	fromLine  int
	reason    string
}

// externalDeclarationSource resolves an external node's recorded
// declaration position to its read-only source. Only the start line was
// stored at index time, so the extent comes from the AST: the declaration
// covering source_line, including its doc comment.
func externalDeclarationSource(node *graph.Node, contextLines int) externalSourceResult {
	sourceFile := metaString(node, "source_file")
	sourceLine := metaInt(node, "source_line")
	if sourceFile == "" || sourceLine <= 0 {
		return externalSourceResult{reason: "no recorded source position"}
	}
	if !confineExternalSource(sourceFile) {
		return externalSourceResult{reason: "source file outside the allowed roots"}
	}
	raw, err := os.ReadFile(sourceFile)
	if err != nil {
		return externalSourceResult{reason: "source file not found"}
	}
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, sourceFile, raw, goparser.ParseComments)
	if err != nil {
		return externalSourceResult{reason: "source file could not be parsed"}
	}
	for _, decl := range file.Decls {
		start := fset.Position(decl.Pos()).Line
		end := fset.Position(decl.End()).Line
		if sourceLine < start || sourceLine > end {
			continue
		}
		if doc := declDoc(decl); doc != nil {
			start = fset.Position(doc.Pos()).Line
		}
		source, from, _, err := extractLinesFromContent(string(raw), start, end, contextLines)
		if err != nil {
			return externalSourceResult{reason: "declaration not found in source file"}
		}
		return externalSourceResult{source: source, startLine: start, endLine: end, fromLine: from}
	}
	return externalSourceResult{reason: "declaration not found in source file"}
}

// declDoc returns the doc comment attached to a declaration, if any.
func declDoc(decl ast.Decl) *ast.CommentGroup {
	switch d := decl.(type) {
	case *ast.GenDecl:
		return d.Doc
	case *ast.FuncDecl:
		return d.Doc
	}
	return nil
}

// addExternalMetaFields copies the external node's attribution onto a
// result payload: module kind/path/version and the type-checker signature.
func addExternalMetaFields(out map[string]any, node *graph.Node) {
	for _, key := range []string{"module_kind", "module_path", "version", "signature"} {
		if v := metaString(node, key); v != "" {
			out[key] = v
		}
	}
}

// externalSymbolSourceResult serves get_symbol_source for an external
// (ext::) symbol: the recorded declaration source when it can be
// confined and read, otherwise a successful structured answer saying why
// there is no source — never the "no line range" error.
func (s *Server) externalSymbolSourceResult(ctx context.Context, req mcp.CallToolRequest, node *graph.Node) (*mcp.CallToolResult, error) {
	src := externalDeclarationSource(node, req.GetInt("context_lines", 3))

	result := map[string]any{
		"id":        node.ID,
		"kind":      node.Kind,
		"name":      node.Name,
		"file_path": node.FilePath,
		"external":  true,
		"read_only": true,
	}
	addExternalMetaFields(result, node)
	if src.reason != "" {
		result["source"] = nil
		result["source_unavailable_reason"] = src.reason
		return s.respondJSONOrTOON(ctx, req, result)
	}
	if req.GetBool("compress_bodies", false) && elide.IsSupported(node.Language) {
		if out, eerr := elide.CompressString(src.source, node.Language); eerr == nil && out != src.source {
			src.source = out
			result["bodies_elided"] = true
		}
	}
	result["source"] = src.source
	result["start_line"] = src.startLine
	result["end_line"] = src.endLine
	result["from_line"] = src.fromLine
	return s.respondJSONOrTOON(ctx, req, result)
}
