package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// GoFrame reflective route binding. GoFrame binds an HTTP route to a
// controller method by the method's request-struct type, not its name: the
// request struct embeds `g.Meta` carrying `path`/`method` tags, and the
// controller method takes that struct as a pointer parameter
// (`func (c *Ctrl) Create(ctx, req *CreateReq) (*CreateRes, error)`). This
// pass materialises a route node per request struct and tags each
// controller method with its request type; the resolver joins them by type
// (a signature join, not a name match).

// goframeRouteViaTag must match resolver.goframeRouteVia — the languages
// package does not import resolver, so the two agree by value.
const goframeRouteViaTag = "goframe-route"

// captureGoFrameRoutes materialises GoFrame route nodes and tags controller
// methods. Runs at the tail of Extract so the method nodes exist.
func captureGoFrameRoutes(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	methodByLine := map[int][]*graph.Node{}
	for _, n := range result.Nodes {
		if n != nil && n.Kind == graph.KindMethod {
			methodByLine[n.StartLine] = append(methodByLine[n.StartLine], n)
		}
	}

	// The file's own package — the qualifier of a request struct declared here.
	// A handler in another package refers to it as `*<pkg>.CreateReq`, so this
	// name pins which package a route's request type belongs to and prevents a
	// same-named struct in another package from claiming the route.
	filePkg := goframePackageName(root, src)

	// Controllers bound via `g.Bind(new(Ctrl))` — the addonRoot tiebreak set.
	bound := goframeBoundControllers(root, src)

	// Pass 1: request structs → route nodes + placeholders.
	goframeWalk(root, func(n *sitter.Node) {
		if n.Type() != "type_spec" {
			return
		}
		nameNode := n.ChildByFieldName("name")
		st := goframeStructType(n)
		if nameNode == nil || st == nil {
			return
		}
		method, path, ok := goframeMetaTag(st, src)
		if !ok {
			return
		}
		reqType := nameNode.Content(src)
		routeID := "route::goframe::" + method + "::" + path
		nodeMeta := map[string]any{
			"type": "http", "role": "provider", "method": method, "path": path,
			"framework": "goframe", "goframe_request_type": reqType,
		}
		edgeMeta := map[string]any{"via": goframeRouteViaTag, "goframe_request_type": reqType, "goframe_route": routeID}
		if filePkg != "" {
			nodeMeta["goframe_request_pkg"] = filePkg
			edgeMeta["goframe_request_pkg"] = filePkg
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: routeID, Kind: graph.KindContract, Name: method + " " + path,
			FilePath: filePath, StartLine: int(n.StartPoint().Row) + 1, Language: "go",
			Meta: nodeMeta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: routeID, To: "unresolved::*." + reqType, Kind: graph.EdgeCalls,
			FilePath: filePath, Line: int(n.StartPoint().Row) + 1,
			Meta: edgeMeta,
		})
	})

	// Pass 2: tag controller methods by request-struct pointer parameter.
	goframeWalk(root, func(n *sitter.Node) {
		if n.Type() != "method_declaration" {
			return
		}
		name, recvType, reqType, reqPkg, ok := goframeMethodParts(n, src)
		if !ok {
			return
		}
		// A bare `*CreateReq` param names a struct in this file's package; a
		// qualified `*api.CreateReq` carries its own package qualifier.
		if reqPkg == "" {
			reqPkg = filePkg
		}
		line := int(n.StartPoint().Row) + 1
		for _, nd := range methodByLine[line] {
			if nd.Name != name {
				continue
			}
			if nd.Meta == nil {
				nd.Meta = map[string]any{}
			}
			nd.Meta["goframe_request_type"] = reqType
			if reqPkg != "" {
				nd.Meta["goframe_request_pkg"] = reqPkg
			}
			if bound[recvType] {
				nd.Meta["goframe_bound"] = true
			}
			break
		}
	})
}

// goframeStructType returns the struct_type of a type_spec, or nil.
func goframeStructType(typeSpec *sitter.Node) *sitter.Node {
	for i, _nc := 0, int(typeSpec.NamedChildCount()); i < _nc; i++ {
		if c := typeSpec.NamedChild(i); c != nil && c.Type() == "struct_type" {
			return c
		}
	}
	return nil
}

// goframeMetaTag finds an embedded `g.Meta` field with a `path`/`method`
// struct tag and returns the method + path.
func goframeMetaTag(structType *sitter.Node, src []byte) (method, path string, ok bool) {
	var fields *sitter.Node
	for i, _nc := 0, int(structType.NamedChildCount()); i < _nc; i++ {
		if c := structType.NamedChild(i); c != nil && c.Type() == "field_declaration_list" {
			fields = c
			break
		}
	}
	if fields == nil {
		return "", "", false
	}
	for i, _nc := 0, int(fields.NamedChildCount()); i < _nc; i++ {
		fd := fields.NamedChild(i)
		if fd == nil || fd.Type() != "field_declaration" {
			continue
		}
		if !goframeIsMetaEmbed(fd, src) {
			continue
		}
		tag := goframeFieldTag(fd, src)
		if tag == "" {
			continue
		}
		m := goframeTagValue(tag, "method")
		p := goframeTagValue(tag, "path")
		if p == "" {
			continue
		}
		if m == "" {
			m = "GET"
		}
		return strings.ToUpper(m), p, true
	}
	return "", "", false
}

// goframeIsMetaEmbed reports whether a field_declaration is an embedded
// `g.Meta` (or `gmeta.Meta`) field.
func goframeIsMetaEmbed(fd *sitter.Node, src []byte) bool {
	for i, _nc := 0, int(fd.NamedChildCount()); i < _nc; i++ {
		c := fd.NamedChild(i)
		if c != nil && c.Type() == "qualified_type" {
			if t := c.ChildByFieldName("name"); t != nil && t.Content(src) == "Meta" {
				return true
			}
		}
	}
	return false
}

// goframeFieldTag returns the struct-tag content of a field_declaration,
// reading the literal's content child so the surrounding ` / " delimiters
// (not the inner key:"value" quotes) are stripped.
func goframeFieldTag(fd *sitter.Node, src []byte) string {
	for i, _nc := 0, int(fd.NamedChildCount()); i < _nc; i++ {
		c := fd.NamedChild(i)
		if c == nil || (c.Type() != "raw_string_literal" && c.Type() != "interpreted_string_literal") {
			continue
		}
		for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
			if cc := c.NamedChild(j); cc != nil && strings.HasSuffix(cc.Type(), "content") {
				return cc.Content(src)
			}
		}
		// No content child (empty literal) — strip the matching delimiter.
		if c.Type() == "raw_string_literal" {
			return strings.Trim(c.Content(src), "`")
		}
		return strings.Trim(c.Content(src), "\"")
	}
	return ""
}

// goframeTagValue extracts the value of a `key:"value"` struct-tag entry.
func goframeTagValue(tag, key string) string {
	i := strings.Index(tag, key+":\"")
	if i < 0 {
		return ""
	}
	rest := tag[i+len(key)+2:]
	if v, _, ok := strings.Cut(rest, "\""); ok {
		return v
	}
	return ""
}

// goframeMethodParts reads a method_declaration: name, receiver type, the
// request-struct type of its pointer parameter (plus its package qualifier, ""
// for a same-package bare type), and whether it has the GoFrame handler shape
// (a pointer request parameter + a result list).
func goframeMethodParts(md *sitter.Node, src []byte) (name, recvType, reqType, reqPkg string, ok bool) {
	var plists []*sitter.Node
	var nameNode *sitter.Node
	for i, _nc := 0, int(md.NamedChildCount()); i < _nc; i++ {
		c := md.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "parameter_list":
			plists = append(plists, c)
		case "field_identifier":
			if nameNode == nil {
				nameNode = c
			}
		}
	}
	if nameNode == nil || len(plists) < 3 {
		return "", "", "", "", false // need receiver + params + results
	}
	recvType = goframePointedType(plists[0], src)
	reqType, reqPkg = goframeLastPointerParamType(plists[1], src)
	if reqType == "" {
		return "", "", "", "", false
	}
	return nameNode.Content(src), recvType, reqType, reqPkg, true
}

// goframePackageName returns the file's package name from its package_clause.
func goframePackageName(root *sitter.Node, src []byte) string {
	pkg := ""
	goframeWalk(root, func(n *sitter.Node) {
		if pkg != "" || n.Type() != "package_clause" {
			return
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			if c := n.NamedChild(i); c != nil && c.Type() == "package_identifier" {
				pkg = strings.TrimSpace(c.Content(src))
				return
			}
		}
	})
	return pkg
}

// goframePointedType returns the (pointer-stripped) type name of a
// parameter list's single parameter — used for the receiver type.
func goframePointedType(plist *sitter.Node, src []byte) string {
	for i, _nc := 0, int(plist.NamedChildCount()); i < _nc; i++ {
		pd := plist.NamedChild(i)
		if pd == nil || pd.Type() != "parameter_declaration" {
			continue
		}
		return goframeParamTypeName(pd, src)
	}
	return ""
}

// goframeLastPointerParamType returns the pointed-to type (and its package
// qualifier, "" when unqualified) of the last pointer-typed parameter — the
// GoFrame request struct.
func goframeLastPointerParamType(plist *sitter.Node, src []byte) (typeName, pkg string) {
	for i, _nc := 0, int(plist.NamedChildCount()); i < _nc; i++ {
		pd := plist.NamedChild(i)
		if pd == nil || pd.Type() != "parameter_declaration" {
			continue
		}
		if t, p := goframePointerParamType(pd, src); t != "" {
			typeName, pkg = t, p
		}
	}
	return typeName, pkg
}

// goframeParamTypeName returns the type identifier of a parameter,
// stripping a leading pointer.
func goframeParamTypeName(pd *sitter.Node, src []byte) string {
	t := pd.ChildByFieldName("type")
	if t == nil {
		return ""
	}
	if t.Type() == "pointer_type" && t.NamedChildCount() > 0 {
		t = t.NamedChild(0)
	}
	if t.Type() == "type_identifier" {
		return t.Content(src)
	}
	return ""
}

// goframePointerParamType returns the pointed-to type identifier of a pointer
// parameter and its package qualifier. A bare `*CreateReq` yields ("CreateReq",
// ""); a qualified `*api.CreateReq` yields ("CreateReq", "api"). Returns "" for
// a non-pointer or otherwise unrecognised parameter.
func goframePointerParamType(pd *sitter.Node, src []byte) (typeName, pkg string) {
	t := pd.ChildByFieldName("type")
	if t == nil || t.Type() != "pointer_type" || t.NamedChildCount() == 0 {
		return "", ""
	}
	inner := t.NamedChild(0)
	if inner == nil {
		return "", ""
	}
	switch inner.Type() {
	case "type_identifier":
		return inner.Content(src), ""
	case "qualified_type":
		// `pkg.Type` — the package qualifier pins which package the request
		// struct belongs to across packages.
		name := inner.ChildByFieldName("name")
		pkgNode := inner.ChildByFieldName("package")
		if name == nil {
			return "", ""
		}
		p := ""
		if pkgNode != nil {
			p = strings.TrimSpace(pkgNode.Content(src))
		}
		return name.Content(src), p
	}
	return "", ""
}

// goframeBoundControllers collects controller types registered via
// `g.Bind(new(Ctrl))` (the addonRoot tiebreak set).
func goframeBoundControllers(root *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	goframeWalk(root, func(n *sitter.Node) {
		if n.Type() != "call_expression" {
			return
		}
		fn := n.ChildByFieldName("function")
		if fn == nil || fn.Type() != "selector_expression" {
			return
		}
		if f := fn.ChildByFieldName("field"); f == nil || f.Content(src) != "Bind" {
			return
		}
		args := n.ChildByFieldName("arguments")
		if args == nil {
			return
		}
		for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
			a := args.NamedChild(i)
			if a == nil || a.Type() != "call_expression" {
				continue
			}
			af := a.ChildByFieldName("function")
			if af == nil || af.Content(src) != "new" {
				continue
			}
			aargs := a.ChildByFieldName("arguments")
			if aargs != nil && aargs.NamedChildCount() > 0 {
				if t := aargs.NamedChild(0); t != nil {
					out[strings.TrimPrefix(strings.TrimSpace(t.Content(src)), "&")] = true
				}
			}
		}
	})
	return out
}

// goframeWalk visits n and all its named descendants.
func goframeWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		goframeWalk(n.NamedChild(i), fn)
	}
}
