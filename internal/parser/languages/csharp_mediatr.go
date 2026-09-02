package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// MediatR request/notification dispatch binding. .NET CQRS code dispatches
// a request or notification through a mediator (`_mediator.Send(new
// CreateOrder())`, `_bus.Publish(new OrderPlaced())`) that is handled by a
// class implementing `IRequestHandler<TReq>` / `INotificationHandler<TN>`.
// The dispatch is keyed on the request type carried in the handler's base
// list, which the static call graph cannot see. This pass tags each
// handler's Handle method and stamps a placeholder per Send/Publish site,
// which the resolver's ResolveMediatRCalls binds (one handler for Send,
// fan-out for Publish).

// mediatrViaTag must match resolver.mediatrVia — the languages package does
// not import resolver, so the two agree by value.
const mediatrViaTag = "mediatr-dispatch"

// mediatrHandlerInterfaces maps a MediatR handler interface to its dispatch
// kind.
var mediatrHandlerInterfaces = map[string]string{
	"IRequestHandler":       "request",
	"IStreamRequestHandler": "request",
	"INotificationHandler":  "notification",
}

// mediatrHandleMethods are the handler entry-point method names.
var mediatrHandleMethods = map[string]bool{"Handle": true, "HandleAsync": true}

// captureMediatRDispatch tags handler methods and stamps Send/Publish
// placeholders. Runs at the tail of Extract so the method nodes exist.
func captureMediatRDispatch(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte, funcBytes, funcLines map[string][2]int) {
	if root == nil || result == nil {
		return
	}
	nodesByLine := map[int][]*graph.Node{}
	for _, n := range result.Nodes {
		if n != nil && (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction) {
			nodesByLine[n.StartLine] = append(nodesByLine[n.StartLine], n)
		}
	}

	// Pass 1: tag handler methods.
	mediatrWalk(root, func(n *sitter.Node) {
		if n.Type() != "class_declaration" {
			return
		}
		reqType, kind := mediatrHandlerType(n, src)
		if reqType == "" {
			return
		}
		body := n.ChildByFieldName("body")
		if body == nil {
			return
		}
		for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
			m := body.NamedChild(i)
			if m == nil || m.Type() != "method_declaration" {
				continue
			}
			name := mediatrMethodName(m, src)
			if !mediatrHandleMethods[name] {
				continue
			}
			line := int(m.StartPoint().Row) + 1
			for _, nd := range nodesByLine[line] {
				if nd.Name != name {
					continue
				}
				if nd.Meta == nil {
					nd.Meta = map[string]any{}
				}
				nd.Meta["mediatr_request_type"] = reqType
				nd.Meta["mediatr_kind"] = kind
				break
			}
		}
	})

	// Pass 2: Send/Publish dispatch sites. The widened owner set, not the
	// methods-only one - a dispatch site inside an accessor or
	// initializer body found no owner and the placeholder was dropped
	// outright at the from=="" gate below.
	funcRanges := csharpOwnerRanges(result, funcBytes, funcLines)
	seen := map[string]bool{}
	mediatrWalk(root, func(call *sitter.Node) {
		if call.Type() != "invocation_expression" {
			return
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || fn.Type() != "member_access_expression" {
			return
		}
		method := ""
		if nm := fn.ChildByFieldName("name"); nm != nil {
			method = nm.Content(src)
		}
		kind := ""
		switch method {
		case "Send":
			kind = "request"
		case "Publish":
			kind = "notification"
		default:
			return
		}
		reqType := mediatrArgType(call, src)
		if reqType == "" {
			return
		}
		receiver := ""
		if r := fn.ChildByFieldName("expression"); r != nil {
			receiver = strings.TrimSpace(r.Content(src))
		}
		line := int(call.StartPoint().Row) + 1
		from := findEnclosingFunc(funcRanges, line)
		if from == "" {
			return
		}
		k := from + "\x00" + reqType + "\x00" + kind
		if seen[k] {
			return
		}
		seen[k] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     from,
			To:       "unresolved::*.Handle",
			Kind:     graph.EdgeCalls,
			FilePath: filePath,
			Line:     line,
			Meta: map[string]any{
				"via":                  mediatrViaTag,
				"mediatr_request_type": reqType,
				"mediatr_kind":         kind,
				"mediatr_receiver":     receiver,
			},
		})
	})
}

// mediatrHandlerType reads a class base list for a MediatR handler
// interface, returning the request/notification type (first generic arg)
// and the dispatch kind.
func mediatrHandlerType(classDecl *sitter.Node, src []byte) (reqType, kind string) {
	for i, _nc := 0, int(classDecl.NamedChildCount()); i < _nc; i++ {
		bl := classDecl.NamedChild(i)
		if bl == nil || bl.Type() != "base_list" {
			continue
		}
		for j, _nc := 0, int(bl.NamedChildCount()); j < _nc; j++ {
			base := bl.NamedChild(j)
			if base == nil || base.Type() != "generic_name" {
				continue
			}
			iface, arg := mediatrGenericNameParts(base, src)
			if k, ok := mediatrHandlerInterfaces[iface]; ok && arg != "" {
				return arg, k
			}
		}
	}
	return "", ""
}

// mediatrGenericNameParts splits a generic_name into its base identifier
// and first type argument's simple name.
func mediatrGenericNameParts(gn *sitter.Node, src []byte) (base, firstArg string) {
	for i, _nc := 0, int(gn.NamedChildCount()); i < _nc; i++ {
		c := gn.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier":
			if base == "" {
				base = c.Content(src)
			}
		case "type_argument_list":
			for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
				a := c.NamedChild(j)
				if a != nil && a.IsNamed() {
					firstArg = mediatrSimpleType(a.Content(src))
					break
				}
			}
		}
	}
	return base, firstArg
}

// mediatrArgType returns the request type of a Send/Publish call's first
// argument: the constructed type of `new X(...)`, else "".
func mediatrArgType(call *sitter.Node, src []byte) string {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		a := args.NamedChild(i)
		if a == nil {
			continue
		}
		// argument wraps the expression.
		expr := a
		if a.Type() == "argument" && a.NamedChildCount() > 0 {
			expr = a.NamedChild(0)
		}
		if expr != nil && expr.Type() == "object_creation_expression" {
			if t := expr.ChildByFieldName("type"); t != nil {
				return mediatrSimpleType(t.Content(src))
			}
		}
		return ""
	}
	return ""
}

func mediatrMethodName(method *sitter.Node, src []byte) string {
	if n := method.ChildByFieldName("name"); n != nil {
		return n.Content(src)
	}
	return ""
}

// mediatrSimpleType reduces a C# type to its simple name (generics and
// namespace qualifiers removed).
func mediatrSimpleType(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.IndexByte(t, '<'); i >= 0 {
		t = t[:i]
	}
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

// mediatrWalk visits n and all its named descendants.
func mediatrWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		mediatrWalk(n.NamedChild(i), fn)
	}
}
