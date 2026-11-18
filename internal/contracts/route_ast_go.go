package contracts

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// goRouteMatch is the AST equivalent of one `httpPatterns` regex
// match. It carries the same shape the regex pipeline produced so
// HTTPExtractor.extract can stitch contracts the same way regardless
// of which detector found the route.
type goRouteMatch struct {
	method       string // "GET" / "ANY" / etc.
	path         string // raw path text (with the verb prefix stripped for Go 1.22+ form)
	handlerIdent string // first identifier in the handler-arg position, or ""
	handlerTrail string // source span from the first handler argument, or ""
	line         int    // 1-based line of the call_expression
	framework    string // "net/http" / "gin/echo/chi" / "fiber"
	confidence   float64
}

// detectGoRoutesAST walks the parse tree's call_expressions and
// returns every HTTP route registration. Replaces the four Go entries
// in httpPatterns; structurally distinguishes a string literal inside
// `[]byte(".GET(")` (which is *not* a route) from `.GET("/users", h)`
// (which is) — the regex layer couldn't.
func detectGoRoutesAST(root *sitter.Node, src []byte, filePath, repoPrefix string, store EndpointConstStore) []goRouteMatch {
	if root == nil {
		return nil
	}
	var out []goRouteMatch
	walkGoCallExprs(root, func(call *sitter.Node) {
		if m, ok := goRouteFromCall(call, src, filePath, repoPrefix, store); ok {
			out = append(out, m)
		}
	})
	return out
}

// walkGoCallExprs descends through every named child of the root,
// invoking fn on each call_expression. We don't run a tree-sitter
// query here — the manual walk is faster (single C-side pass per
// node) and the predicate logic is simple enough.
func walkGoCallExprs(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	if n.Type() == "call_expression" {
		fn(n)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		ch := n.NamedChild(i)
		if ch == nil {
			continue
		}
		walkGoCallExprs(ch, fn)
	}
}

// goRouteFromCall classifies a call_expression as an HTTP route
// registration. Returns (match, true) when it matches one of the
// four supported shapes; (zero, false) otherwise.
func goRouteFromCall(call *sitter.Node, src []byte, filePath, repoPrefix string, store EndpointConstStore) (goRouteMatch, bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "selector_expression" {
		return goRouteMatch{}, false
	}
	field := fn.ChildByFieldName("field")
	if field == nil {
		return goRouteMatch{}, false
	}
	methodTok := field.Content(src)
	// Reject non-route selector calls before inspecting or dereferencing their
	// first argument. detectGoRoutesAST visits every Go call expression, so
	// resolving an identifier here before the method switch turns ordinary
	// calls (fmt.Sprintf, strings.Contains, database methods, and so on) into
	// graph-wide constant lookups that can allocate large candidate slices.
	// Keep this token set exactly aligned with the accepted switch cases below.
	switch methodTok {
	case "Handle", "HandleFunc",
		"Get", "Post", "Put", "Delete", "Patch", "Head", "Options",
		"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
	default:
		return goRouteMatch{}, false
	}

	args := call.ChildByFieldName("arguments")
	if args == nil {
		return goRouteMatch{}, false
	}
	argList := namedChildren(args)
	if len(argList) == 0 {
		return goRouteMatch{}, false
	}

	// Resolve the path argument graph-wide: a string literal, a const
	// identifier (so `const P = "/api/x"; r.GET(P, h)` mints a route — even
	// when P is defined in another file of the same repo), or a composite
	// literal's endpoint field. forRoute=true vets a const / composite
	// dereference through the route guard so a value that is really a
	// filesystem path is dropped; a plain literal is returned unchanged.
	pathArg := argList[0]
	pathText, ok := ResolveEndpointArg(pathArg, src, filePath, repoPrefix, store, true)
	if !ok {
		return goRouteMatch{}, false
	}

	// Identify framework + method by the field name and path shape.
	switch methodTok {
	case "Handle", "HandleFunc":
		// net/http: receiver name is anything (mux, router, http) — we
		// don't care. Method is embedded in the path for Go 1.22+ or
		// "ANY" for legacy. Path must start with `/` (legacy) OR
		// "VERB /" (Go 1.22+).
		method, path, ok := splitNetHTTPPattern(pathText)
		if !ok {
			return goRouteMatch{}, false
		}
		conf := 0.9
		if method != "ANY" {
			conf = 0.95
		}
		return finishGoRoute(call, args, argList, src, method, path, "net/http", conf), true

	case "Get", "Post", "Put", "Delete", "Patch", "Head", "Options":
		// gin/echo/chi: lowercase-method call on a router-shaped
		// receiver. The original regex restricted receivers to
		// {r, g, e, router, group, api, v1, mux, app}. Tightening
		// further is risky — production code uses other names.
		// Reject when receiver is `http` (avoids matching
		// http.Get(url) consumer patterns).
		if isHTTPStdlibConsumer(fn, src) {
			return goRouteMatch{}, false
		}
		// Path must start with `/` to avoid false-matching arbitrary
		// `.Get("name")` calls on non-router types.
		if !strings.HasPrefix(pathText, "/") {
			return goRouteMatch{}, false
		}
		return finishGoRoute(call, args, argList, src, strings.ToUpper(methodTok), pathText, "gin/echo/chi", 0.9), true

	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		// fiber: uppercase method call. Path must start with `/`.
		if !strings.HasPrefix(pathText, "/") {
			return goRouteMatch{}, false
		}
		return finishGoRoute(call, args, argList, src, methodTok, pathText, "fiber", 0.9), true
	}
	return goRouteMatch{}, false
}

// finishGoRoute builds the goRouteMatch with handler metadata.
func finishGoRoute(
	call *sitter.Node,
	args *sitter.Node,
	argList []*sitter.Node,
	src []byte,
	method, path, framework string,
	confidence float64,
) goRouteMatch {
	m := goRouteMatch{
		method:     method,
		path:       path,
		framework:  framework,
		confidence: confidence,
		line:       int(call.StartPoint().Row) + 1,
	}
	if len(argList) >= 2 {
		// handlerIdent is the FIRST identifier we can pluck from the
		// handler-arg position. For wrapped calls (`WithAuth(h, fn)`)
		// this is the wrapper, but it's enough for the post-pass to
		// resolve via the call-trail.
		handlerArg := argList[1]
		m.handlerIdent = extractFirstHandlerIdent(handlerArg, src)
		// Start at the actual handler expression, retaining later middleware
		// arguments. Words in the preceding route literal are not references.
		m.handlerTrail = strings.TrimSpace(string(src[handlerArg.StartByte() : args.EndByte()-1]))
	}
	return m
}

// extractFirstHandlerIdent returns the first identifier-shaped name
// in the handler-arg expression. For:
//
//	listUsers              → "listUsers"
//	h.CreateUser           → "h"            (matches the regex output)
//	WithAuth(h.CreateUser) → "WithAuth"
//
// The post-pass's resolveProviderHandlers then walks handlerTrail to
// find the innermost-resolvable handler.
func extractFirstHandlerIdent(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch node.Type() {
	case "identifier":
		return node.Content(src)
	case "selector_expression":
		// `h.CreateUser` — match regex behaviour: capture "h" only.
		op := node.ChildByFieldName("operand")
		if op != nil {
			return extractFirstHandlerIdent(op, src)
		}
	case "call_expression":
		// Wrapper call: pull the function ident.
		fn := node.ChildByFieldName("function")
		if fn != nil {
			return extractFirstHandlerIdent(fn, src)
		}
	}
	return ""
}

// stringLiteralValue returns the unquoted text of a string literal
// node. Returns ("", false) if the node isn't a string literal.
func stringLiteralValue(n *sitter.Node, src []byte) (string, bool) {
	if n == nil {
		return "", false
	}
	switch n.Type() {
	case "interpreted_string_literal", "raw_string_literal":
		return unquoteStringLiteral(n.Content(src)), true
	}
	return "", false
}

// splitNetHTTPPattern handles the Go 1.22+ "VERB /path" form vs
// legacy "/path". Returns (method, path, ok).
//
//	"GET /users"   → ("GET",  "/users", true)
//	"/users"       → ("ANY",  "/users", true)
//	"users"        → ("",     "",       false)   ← rejected: no leading /
func splitNetHTTPPattern(s string) (string, string, bool) {
	if strings.HasPrefix(s, "/") {
		return "ANY", s, true
	}
	// "VERB /path" — verb is one of GET/POST/.../OPTIONS, then space,
	// then path starting with /.
	idx := strings.IndexByte(s, ' ')
	if idx <= 0 {
		return "", "", false
	}
	verb := s[:idx]
	path := strings.TrimSpace(s[idx+1:])
	if !isHTTPVerb(verb) || !strings.HasPrefix(path, "/") {
		return "", "", false
	}
	return verb, path, true
}

func isHTTPVerb(s string) bool {
	switch s {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "CONNECT", "TRACE":
		return true
	}
	return false
}

// buildGoRouteContract turns a goRouteMatch into a Contract,
// mirroring the regex pipeline's stitching: subtree handling for
// net/http trailing slashes, NormalizeHTTPPathWithParams for path
// params, handler-ident scratchpad for the resolveProviderHandlers
// post-pass, and an EnrichHTTPContractWithTree call for body facts.
func buildGoRouteContract(
	rm goRouteMatch,
	filePath string,
	fileNodes []*graph.Node,
	lines []string,
	lang string,
	tree *parser.ParseTree,
	_ string, // text: kept for signature symmetry with the regex caller
	_ []byte, // src
) Contract {
	path := rm.path
	subtree := false

	// Same trailing-slash subtree treatment net/http gets in the regex
	// pipeline (http.go ~line 600). gin/echo/chi/fiber treat trailing
	// slash as a literal, so we only patch for net/http.
	if rm.framework == "net/http" && len(path) > 1 && strings.HasSuffix(path, "/") {
		path = strings.TrimRight(path, "/") + "/{rest}"
		subtree = true
	}

	normPath, origNames := NormalizeHTTPPathWithParams(path)
	contractID := "http::" + rm.method + "::" + normPath

	symbolID := findEnclosingSymbol(fileNodes, rm.line)

	// Re-point SymbolID at the actual handler when we can resolve it
	// in the same file. Bare handler: r.GET("/x", listUsers) → resolve
	// "listUsers". Wrapped: WithAuth(h.Foo) → fall through to
	// findInnermostResolvableHandler on the trail.
	if rm.handlerIdent != "" {
		if hID := resolveHandlerIdent(fileNodes, rm.handlerIdent); hID != "" {
			symbolID = hID
		} else if hID := findInnermostResolvableHandler(fileNodes, rm.handlerTrail); hID != "" {
			symbolID = hID
		}
	}

	meta := map[string]any{
		"method":    rm.method,
		"path":      normPath,
		"framework": rm.framework,
	}
	if len(origNames) > 0 {
		meta["path_param_names"] = origNames
	}
	if rm.handlerIdent != "" {
		meta["handler_ident"] = rm.handlerIdent
	}
	if rm.handlerTrail != "" {
		meta["handler_trail"] = rm.handlerTrail
	}
	if subtree {
		meta["subtree"] = true
	}

	c := Contract{
		ID:         contractID,
		Type:       ContractHTTP,
		Role:       RoleProvider,
		SymbolID:   symbolID,
		FilePath:   filePath,
		Line:       rm.line,
		Meta:       meta,
		Confidence: rm.confidence,
	}
	EnrichHTTPContractWithTree(&c, lines, fileNodes, lang, tree)
	return c
}

// isHTTPStdlibConsumer reports whether the selector_expression
// receiver is the `http` package (in which case `http.Get(url, …)`
// is a CONSUMER, not a route registration).
func isHTTPStdlibConsumer(fn *sitter.Node, src []byte) bool {
	if fn == nil {
		return false
	}
	op := fn.ChildByFieldName("operand")
	if op == nil {
		return false
	}
	if op.Type() != "identifier" {
		return false
	}
	return op.Content(src) == "http"
}
