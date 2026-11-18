package languages

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Freeze the independent full-tree walks before sharing the Go tail worklist.
// Compare ordered output as well as every graph field and owned sidecar.
func TestGoTailWorklist_GoFrameFullOrderedLegacyParity(t *testing.T) {
	const ordinary = "package ctrl\n" +
		"// Create docs.\nfunc(c *Ctrl) Create(ctx Context, req *CreateReq) (*CreateRes,error) { return nil,nil }\n" +
		"func(c *Ctrl) Foreign(req *api.ForeignReq) (*CreateRes,error) { return nil,nil }\n" +
		"type CreateReq struct { g.Meta `path:\"/users\" method:\"post\"`; Value Box[Item] }\n" +
		"type DuplicateReq struct { g.Meta `path:\"/users\" method:\"POST\"` }\n" +
		"type CreateRes struct{}; type Ctrl struct{}; type Item struct{}; type Box[T any] struct{}\n" +
		"func Register() { f:=func() { router.Bind(new(Ctrl)) }; _=f; type LocalReq struct { g.Meta `path:\"/local\"` } }\n"
	for _, tc := range []struct{ name, source string }{
		{"forward_bind_and_routes", ordinary},
		{"earlier_bind", "package ctrl\nfunc Register(){ router.Bind(new(Ctrl)) }\n" + strings.TrimPrefix(ordinary, "package ctrl\n")},
		{"no_framework", "package plain\ntype Box[T any] struct{}\nfunc Target(v map[string]chan Box[Item]) {}\ntype Item struct{}"},
		{"malformed_tail", ordinary + "func Broken("},
		{"missing_package", strings.TrimPrefix(ordinary, "package ctrl\n")},
	} {
		for _, ending := range []string{"lf", "crlf"} {
			t.Run(tc.name+"/"+ending, func(t *testing.T) {
				src := []byte(tc.source)
				if ending == "crlf" {
					src = []byte(strings.ReplaceAll(string(src), "\n", "\r\n"))
				}
				result, err := NewGoExtractor().Extract("tail.go", src)
				require.NoError(t, err)
				defer result.ReleaseTree()
				ordered := func() []byte {
					type edgeRow struct {
						*graph.Edge
						Confidence float64
						Meta       map[string]any
					}
					edges := make([]edgeRow, 0, len(result.Edges))
					for _, e := range result.Edges {
						edges = append(edges, edgeRow{e, e.Confidence, e.Meta})
					}
					data, err := json.Marshal(struct {
						Nodes []*graph.Node
						Edges []edgeRow
					}{result.Nodes, edges})
					require.NoError(t, err)
					return data
				}
				if tc.name == "forward_bind_and_routes" {
					routes := 0
					bound := false
					for _, n := range result.Nodes {
						if n.Meta["framework"] == "goframe" {
							routes++
						}
						if n.Kind == graph.KindMethod && n.Meta["goframe_bound"] == true {
							bound = true
						}
					}
					require.GreaterOrEqual(t, routes, 3)
					require.True(t, bound, "later nested Bind must tag the earlier method")
				}
				wantOrdered := ordered()
				wantAll := goScratchSemantics(t, result)
				nodes := result.Nodes[:0]
				for _, n := range result.Nodes {
					if n.Meta["framework"] == "goframe" {
						continue
					}
					if n.Kind == graph.KindMethod {
						delete(n.Meta, "goframe_request_type")
						delete(n.Meta, "goframe_request_pkg")
						delete(n.Meta, "goframe_bound")
					}
					nodes = append(nodes, n)
				}
				result.Nodes = nodes
				edges := result.Edges[:0]
				for _, e := range result.Edges {
					if e.Meta["via"] != goframeRouteViaTag {
						edges = append(edges, e)
					}
				}
				result.Edges = edges
				root := result.Tree.Tree().RootNode()
				root.WithScratch(func() { legacyGoFrameRoutes(result, root, "tail.go", src) })
				require.Equal(t, string(wantOrdered), string(ordered()), "full ordered nodes/edges must match original traversals")
				require.Equal(t, string(wantAll), string(goScratchSemantics(t, result)), "all graph fields and sidecars must remain identical")
				root.WithScratch(func() { legacyGoFrameWalk(root, func(n *sitter.Node) { require.NotEmpty(t, n.Type()) }) })
				require.Equal(t, string(wantAll), string(goScratchSemantics(t, result)), "owned graph must survive scratch reuse")
			})
		}
	}
}

func legacyGoFrameRoutes(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
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
	bound := legacyGoFrameBoundControllers(root, src)

	// Pass 1: request structs → route nodes + placeholders.
	legacyGoFrameWalk(root, func(n *sitter.Node) {
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
	legacyGoFrameWalk(root, func(n *sitter.Node) {
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

func legacyGoFrameBoundControllers(root *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	legacyGoFrameWalk(root, func(n *sitter.Node) {
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

// legacyGoFrameWalk visits n and all its named descendants.
func legacyGoFrameWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		legacyGoFrameWalk(n.NamedChild(i), fn)
	}
}
