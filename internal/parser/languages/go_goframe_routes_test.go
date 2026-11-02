package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func goframeRouteNode(nodes []*graph.Node, method, path string) *graph.Node {
	for _, n := range nodes {
		if n.Kind != graph.KindContract || n.Meta == nil {
			continue
		}
		m, _ := n.Meta["method"].(string)
		p, _ := n.Meta["path"].(string)
		if m == method && p == path {
			return n
		}
	}
	return nil
}

func goframeMethodNode(nodes []*graph.Node, name string) *graph.Node {
	for _, n := range nodes {
		if n.Kind == graph.KindMethod && n.Name == name && n.Meta != nil && n.Meta["goframe_request_type"] != nil {
			return n
		}
	}
	return nil
}

func TestGoFrame_RouteNodeAndMethodTagging(t *testing.T) {
	src := "package ctrl\n" +
		"type CreateReq struct {\n" +
		"\tg.Meta `path:\"/users\" method:\"POST\"`\n" +
		"\tName string\n" +
		"}\n" +
		"type CreateRes struct{}\n" +
		"type UserCtrl struct{}\n" +
		"func (c *UserCtrl) Create(ctx context.Context, req *CreateReq) (*CreateRes, error) {\n" +
		"\treturn nil, nil\n" +
		"}\n" +
		"func Register(s *ghttp.Server) {\n" +
		"\ts.Group(\"/\", func(g *ghttp.RouterGroup) { g.Bind(new(UserCtrl)) })\n" +
		"}\n"
	res, err := NewGoExtractor().Extract("ctrl/user.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	route := goframeRouteNode(res.Nodes, "POST", "/users")
	if route == nil {
		t.Fatalf("no POST /users route node materialised")
	}
	if rt, _ := route.Meta["goframe_request_type"].(string); rt != "CreateReq" {
		t.Errorf("route request type = %q (want CreateReq)", rt)
	}
	if fw, _ := route.Meta["framework"].(string); fw != "goframe" {
		t.Errorf("route framework = %q (want goframe)", fw)
	}

	m := goframeMethodNode(res.Nodes, "Create")
	if m == nil {
		t.Fatalf("controller method Create not tagged")
	}
	if rt, _ := m.Meta["goframe_request_type"].(string); rt != "CreateReq" {
		t.Errorf("method request type = %q (want CreateReq)", rt)
	}
	if b, _ := m.Meta["goframe_bound"].(bool); !b {
		t.Errorf("Create's controller is bound via g.Bind(new(UserCtrl)) — goframe_bound should be true")
	}

	var ph *graph.Edge
	for _, e := range res.Edges {
		if e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == "goframe-route" {
				ph = e
			}
		}
	}
	if ph == nil {
		t.Fatalf("no goframe-route placeholder edge")
	}
	if ph.To != "unresolved::*.CreateReq" {
		t.Errorf("placeholder To = %q (want unresolved::*.CreateReq)", ph.To)
	}
}

func TestGoFrame_RequestPackageStamped(t *testing.T) {
	// A request struct's declaring package qualifies its route, and a handler's
	// pointer param carries that package — bare for a same-package param, the
	// qualifier for a cross-package `*api.CreateReq`. Both sides must agree so
	// the resolver can join across packages without colliding on the bare name.
	src := "package api\n" +
		"type CreateReq struct {\n" +
		"\tg.Meta `path:\"/users\" method:\"POST\"`\n" +
		"}\n" +
		"type CreateRes struct{}\n" +
		"type SameCtrl struct{}\n" +
		"func (c *SameCtrl) Same(ctx context.Context, req *CreateReq) (*CreateRes, error) { return nil, nil }\n" +
		"type CrossCtrl struct{}\n" +
		"func (c *CrossCtrl) Cross(ctx context.Context, req *other.CreateReq) (*CreateRes, error) { return nil, nil }\n"
	res, err := NewGoExtractor().Extract("api/user.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	route := goframeRouteNode(res.Nodes, "POST", "/users")
	if route == nil {
		t.Fatalf("no POST /users route node materialised")
	}
	if pkg, _ := route.Meta["goframe_request_pkg"].(string); pkg != "api" {
		t.Errorf("route request pkg = %q (want api)", pkg)
	}

	same := goframeMethodNode(res.Nodes, "Same")
	if same == nil {
		t.Fatalf("same-package handler Same not tagged")
	}
	// A bare `*CreateReq` param inherits the file's package.
	if pkg, _ := same.Meta["goframe_request_pkg"].(string); pkg != "api" {
		t.Errorf("Same request pkg = %q (want api, the file package)", pkg)
	}

	cross := goframeMethodNode(res.Nodes, "Cross")
	if cross == nil {
		t.Fatalf("cross-package handler Cross not tagged")
	}
	// A qualified `*other.CreateReq` param carries its own package qualifier.
	if rt, _ := cross.Meta["goframe_request_type"].(string); rt != "CreateReq" {
		t.Errorf("Cross request type = %q (want CreateReq)", rt)
	}
	if pkg, _ := cross.Meta["goframe_request_pkg"].(string); pkg != "other" {
		t.Errorf("Cross request pkg = %q (want other, the param qualifier)", pkg)
	}
}

func TestGoFrame_NonRequestStructIgnored(t *testing.T) {
	// A plain struct without an embedded g.Meta tag is not a route.
	src := "package m\n" +
		"type Plain struct { Name string }\n"
	res, err := NewGoExtractor().Extract("m/plain.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindContract && n.Meta != nil {
			if fw, _ := n.Meta["framework"].(string); fw == "goframe" {
				t.Errorf("a plain struct must not materialise a goframe route")
			}
		}
	}
}
