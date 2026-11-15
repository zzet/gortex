package mcp

import (
	"bytes"
	"context"
	"go/ast"
	"go/build"
	goparser "go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

// goRootTimeout bounds the go env GOROOT subprocess used when GOROOT is
// unset. Discovery runs inside the first request that needs it, so a
// slow or wedged toolchain must not hold that request open for good.
const goRootTimeout = 10 * time.Second

// goRootWaitDelay stops the wait even when the killed subprocess's
// output pipes stay open, for example because a grandchild holds them.
const goRootWaitDelay = 2 * time.Second

// goRootOutputLimit caps the captured go env output: GOROOT is a single
// path, so more than this is a wedged child, not a longer answer.
const goRootOutputLimit = 64 << 10

// goRootFlight is one discovery subprocess in flight. Waiters watch done
// instead of their own contexts, so the outcome stays independent of
// whichever request happened to trigger the discovery.
type goRootFlight struct {
	done chan struct{}
	root string
}

var (
	goRootMu     sync.Mutex
	goRootCached string
	goRootOK     bool
	goRootActive *goRootFlight
)

// goRoot resolves the Go installation root the way the go command does:
// $GOROOT when set, else the GOROOT of the `go` binary on PATH.
// runtime.GOROOT is deprecated and answers the root the binary was built
// in, which may be empty or point at a tree that does not exist on the
// machine serving the source. An empty result means no Go root is known
// and external reads degrade to the structured answer.
//
// The discovery subprocess is bounded (deadline, output cap, wait delay)
// and shared through a single flight. A caller whose context ends while
// waiting returns without the root instead of blocking on the toolchain,
// and only a discovered root is cached: a canceled or failed first
// request must not poison the process-wide cache.
func goRoot(ctx context.Context) string {
	if root := os.Getenv("GOROOT"); root != "" {
		return root
	}
	goRootMu.Lock()
	if goRootOK {
		root := goRootCached
		goRootMu.Unlock()
		return root
	}
	flight := goRootActive
	if flight == nil {
		flight = &goRootFlight{done: make(chan struct{})}
		goRootActive = flight
		go func() {
			root := discoverGoRoot()
			flight.root = root
			close(flight.done)
			goRootMu.Lock()
			if root != "" {
				goRootCached, goRootOK = root, true
			}
			goRootActive = nil
			goRootMu.Unlock()
		}()
	}
	goRootMu.Unlock()

	select {
	case <-flight.done:
		return flight.root
	case <-ctx.Done():
		return ""
	}
}

// discoverGoRoot runs one bounded `go env GOROOT`. The context kill is
// the child cleanup: exec.CommandContext terminates the process at the
// deadline, and WaitDelay keeps the wait itself bounded when pipes stay
// open after the kill.
func discoverGoRoot() string {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), goRootTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "env", "GOROOT")
	cmd.WaitDelay = goRootWaitDelay
	var out goRootOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.buf.String())
}

// goRootOutput captures the subprocess output up to goRootOutputLimit
// and drops the rest, reporting success so the child keeps draining
// until the deadline kill instead of dying on a full pipe.
type goRootOutput struct {
	buf bytes.Buffer
}

func (w *goRootOutput) Write(p []byte) (int, error) {
	if room := goRootOutputLimit - w.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		w.buf.Write(p)
	}
	return len(p), nil
}

// externalSourceRoots returns the only trees an external symbol's
// recorded source_file may be served from: the Go standard library and
// the module cache. A var so tests can point it at a fixture tree.
var externalSourceRoots = func(ctx context.Context) []string {
	roots := []string{goRoot(ctx)}
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
// declaration source. The stored path is read as given, so the candidate
// and the roots are resolved before the containment check: a regular
// file beneath a directory link that leaves the root is not itself a
// link, and a leaf-only check would admit it while the read follows the
// ancestor outside. Both sides resolve with the same resolver: ResolveRoot
// keeps a Windows junction in the root's spelling, so a root configured
// as a junction, or beneath one, would never contain a resolved candidate.
func confineExternalSource(ctx context.Context, path string) bool {
	resolved, err := pathguard.EvalLinks(path)
	if err != nil {
		return false
	}
	for _, root := range externalSourceRoots(ctx) {
		if root == "" {
			continue
		}
		resolvedRoot, err := pathguard.EvalLinks(root)
		if err != nil {
			continue
		}
		if !pathguard.WithinRoot(resolved, resolvedRoot) {
			continue
		}
		return true
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
func externalDeclarationSource(ctx context.Context, node *graph.Node, contextLines int) externalSourceResult {
	sourceFile := metaString(node, "source_file")
	sourceLine := metaInt(node, "source_line")
	if sourceFile == "" || sourceLine <= 0 {
		return externalSourceResult{reason: "no recorded source position"}
	}
	if !confineExternalSource(ctx, sourceFile) {
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
	src := externalDeclarationSource(ctx, node, req.GetInt("context_lines", 3))
	// The root lookup waits on the request context; if the deadline fired
	// there, surface it rather than answering a dead request.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
