package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	wire "github.com/gortexhq/gcx-go"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func newTestNode(id, name string, kind graph.NodeKind, path string, line int) *graph.Node {
	return &graph.Node{
		ID:        id,
		Name:      name,
		Kind:      kind,
		FilePath:  path,
		StartLine: line,
		EndLine:   line + 5,
		Meta:      map[string]any{"signature": "func " + name + "()"},
	}
}

func TestEncodeSearchSymbols_HeaderAndRows(t *testing.T) {
	nodes := []*graph.Node{
		newTestNode("a.go::Foo", "Foo", graph.KindFunction, "a.go", 10),
		newTestNode("b.go::Decoder.Bar", "Bar", graph.KindMethod, "b.go", 20),
	}
	nodes[0].Meta["type_flavor"] = "struct"
	nodes[0].Meta["ui_component"] = "react"
	payload, err := encodeSearchSymbols(nodes, 2, 10)
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "search_symbols", h.Tool)
	require.Equal(t, []string{"id", "kind", "name", "path", "path_abs", "line", "sig", "enclosing", "is_test", "test_role", "test_runner", "type_flavor", "ui_component"}, h.Fields)
	require.Equal(t, "2", h.Meta["total"])
	require.Equal(t, "false", h.Meta["truncated"])

	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "a.go::Foo", rows[0]["id"])
	require.Equal(t, "function", rows[0]["kind"])
	require.Equal(t, "Foo", rows[0]["name"])
	require.Equal(t, "10", rows[0]["line"])
	require.Equal(t, "func Foo()", rows[0]["sig"])
	require.Equal(t, "", rows[0]["path_abs"], "path_abs column present, empty when the node carries no resolved absolute path")
	require.Equal(t, "false", rows[0]["is_test"])
	require.Equal(t, "", rows[0]["test_role"])
	// A top-level function has no enclosing owner; a method does.
	require.Equal(t, "", rows[0]["enclosing"], "a top-level function has no enclosing owner")
	require.Equal(t, "Decoder", rows[1]["enclosing"], "a method reports its receiver type as the enclosing owner")
	// New structural-flavor columns surface the node's own type_flavor /
	// ui_component, empty when the node carries neither.
	require.Equal(t, "struct", rows[0]["type_flavor"])
	require.Equal(t, "react", rows[0]["ui_component"])
	require.Equal(t, "", rows[1]["type_flavor"])
	require.Equal(t, "", rows[1]["ui_component"])
}

func TestEncodeSearchSymbols_RespectsLimitAndTruncation(t *testing.T) {
	nodes := make([]*graph.Node, 5)
	for i := range nodes {
		nodes[i] = newTestNode("x.go::N", "N", graph.KindFunction, "x.go", i)
	}
	payload, err := encodeSearchSymbols(nodes, 5, 3)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, _ := dec.Header()
	require.Equal(t, "true", h.Meta["truncated"])
	rows, _ := dec.All()
	require.Len(t, rows, 3)
}

func TestEncodeSearchSymbols_SkipsFileAndImport(t *testing.T) {
	nodes := []*graph.Node{
		newTestNode("f.go", "f.go", graph.KindFile, "f.go", 1),
		newTestNode("f.go::Foo", "Foo", graph.KindFunction, "f.go", 5),
		newTestNode("f.go::imp", "imp", graph.KindImport, "f.go", 2),
	}
	payload, err := encodeSearchSymbols(nodes, 3, 10)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	_, _ = dec.Header()
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "Foo", rows[0]["name"])
}

func TestEncodeGetSymbolSource_EmbeddedNewlinesRoundTrip(t *testing.T) {
	node := newTestNode("f.go::Foo", "Foo", graph.KindFunction, "f.go", 10)
	src := "func Foo() {\n\tfmt.Println(\"x\\ty\")\n}"
	payload, err := encodeGetSymbolSource(node, src, 9, "etag123", "")
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "get_symbol_source", h.Tool)
	require.Equal(t, "etag123", h.Meta["etag"])

	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, src, rows[0]["source"])
	require.Equal(t, "9", rows[0]["from_line"])
	require.Equal(t, "etag123", rows[0]["etag"])
}

func TestEncodeBatchSymbols_IncludeSource(t *testing.T) {
	rows := []map[string]any{
		{
			"id":         "a.go::Foo",
			"kind":       graph.KindFunction,
			"name":       "Foo",
			"file_path":  "a.go",
			"start_line": 10,
			"end_line":   20,
			"signature":  "func Foo()",
			"source":     "func Foo() {}",
		},
		{
			"id":    "x.go::Missing",
			"error": "symbol not found",
		},
	}
	payload, err := encodeBatchSymbols(rows, true)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Contains(t, h.Fields, "source")
	require.Contains(t, h.Fields, "error")
	got, err := dec.All()
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "func Foo()", got[0]["sig"])
	require.Equal(t, "symbol not found", got[1]["error"])
}

// TestEncodeSubGraph_DistinctCallSitesNotDeduped pins the regression
// for the walk_graph / get_callers duplicate-row complaint: two
// distinct call sites between the same caller and callee must appear
// as two rows the agent can tell apart by line + file_path, not
// collapse into identical wire rows.
func TestEncodeSubGraph_DistinctCallSitesNotDeduped(t *testing.T) {
	sg := &query.SubGraph{
		Nodes: []*graph.Node{
			newTestNode("a.go::Caller", "Caller", graph.KindFunction, "a.go", 10),
			newTestNode("b.go::Target", "Target", graph.KindFunction, "b.go", 20),
		},
		Edges: []*graph.Edge{
			{From: "a.go::Caller", To: "b.go::Target", Kind: "calls", Confidence: 0.9, Origin: "ast_resolved", FilePath: "a.go", Line: 27},
			{From: "a.go::Caller", To: "b.go::Target", Kind: "calls", Confidence: 0.9, Origin: "ast_resolved", FilePath: "a.go", Line: 42},
		},
		TotalNodes: 2,
	}
	payload, err := encodeSubGraph("walk_graph", sg, nil)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	_, err = dec.Header()
	require.NoError(t, err)
	_, err = dec.All()
	require.NoError(t, err)

	h2, err := dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "walk_graph.edges", h2.Tool)
	require.Contains(t, h2.Fields, "line")
	require.Contains(t, h2.Fields, "file_path")
	edges, err := dec.All()
	require.NoError(t, err)
	require.Len(t, edges, 2)
	require.Equal(t, "27", edges[0]["line"])
	require.Equal(t, "42", edges[1]["line"])
}

func TestEncodeSubGraph_NodesAndEdgesSections(t *testing.T) {
	sg := &query.SubGraph{
		Nodes: []*graph.Node{
			newTestNode("a.go::Foo", "Foo", graph.KindFunction, "a.go", 10),
			newTestNode("b.go::Bar", "Bar", graph.KindFunction, "b.go", 20),
		},
		Edges: []*graph.Edge{
			{From: "a.go::Foo", To: "b.go::Bar", Kind: "calls", Confidence: 0.9, Origin: "ast_resolved"},
		},
		TotalNodes: 2,
	}
	payload, err := encodeSubGraph("get_callers", sg, nil)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	h1, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "get_callers.nodes", h1.Tool)
	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 2)

	h2, err := dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "get_callers.edges", h2.Tool)
	edges, err := dec.All()
	require.NoError(t, err)
	require.Len(t, edges, 1)
	require.Equal(t, "calls", edges[0]["kind"])
	require.Equal(t, "ast_resolved", edges[0]["origin"])
}

func TestEncodeFindUsages_OneRowPerEdge(t *testing.T) {
	sg := &query.SubGraph{
		Nodes: []*graph.Node{
			newTestNode("a.go::Caller", "Caller", graph.KindFunction, "a.go", 10),
			newTestNode("b.go::Target", "Target", graph.KindFunction, "b.go", 20),
		},
		Edges: []*graph.Edge{
			{From: "a.go::Caller", To: "b.go::Target", Kind: "calls", Origin: "lsp_resolved", Confidence: 1.0},
		},
	}
	payload, err := encodeFindUsages(sg, nil)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	_, err = dec.Header()
	require.NoError(t, err)
	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "a.go::Caller", rows[0]["from"])
	require.Equal(t, "b.go::Target", rows[0]["to"])
	require.Equal(t, "Caller", rows[0]["from_name"])
	// Edge with no Line falls back to the caller's start line.
	require.Equal(t, "10", rows[0]["from_line"])
}

// TestEncodeFindUsages_FromLineIsCallSite pins the call-site-line
// behaviour: when an edge carries its own Line (the actual offset of
// the `Target(...)` call expression), find_usages must surface that
// — not the enclosing caller's start line. Two calls from the same
// caller used to collapse onto the caller's first line, which made
// it impossible to tell duplicate-looking rows apart.
func TestEncodeFindUsages_FromLineIsCallSite(t *testing.T) {
	sg := &query.SubGraph{
		Nodes: []*graph.Node{
			newTestNode("a.go::Caller", "Caller", graph.KindFunction, "a.go", 10),
			newTestNode("b.go::Target", "Target", graph.KindFunction, "b.go", 20),
		},
		Edges: []*graph.Edge{
			{From: "a.go::Caller", To: "b.go::Target", Kind: "calls", Origin: "lsp_resolved", Confidence: 1.0, FilePath: "a.go", Line: 27},
			{From: "a.go::Caller", To: "b.go::Target", Kind: "calls", Origin: "lsp_resolved", Confidence: 1.0, FilePath: "a.go", Line: 42},
		},
	}
	payload, err := encodeFindUsages(sg, nil)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	_, err = dec.Header()
	require.NoError(t, err)
	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "27", rows[0]["from_line"])
	require.Equal(t, "42", rows[1]["from_line"])
}

// TestEncodeFindUsages_FromFlavor pins the owner-resolution surfacing:
// the from_type_flavor column reports the FROM site's enclosing owner
// type's flavor, and from_ui_component the FROM node's own framework.
func TestEncodeFindUsages_FromFlavor(t *testing.T) {
	g := graph.New()
	owner := newTestNode("b.go::Svc", "Svc", graph.KindType, "b.go", 5)
	owner.Meta["type_flavor"] = "struct"
	caller := newTestNode("b.go::Svc.Run", "Run", graph.KindMethod, "b.go", 10)
	caller.Meta["ui_component"] = "react"
	target := newTestNode("c.go::Target", "Target", graph.KindFunction, "c.go", 20)
	g.AddNode(owner)
	g.AddNode(caller)
	g.AddNode(target)

	sg := &query.SubGraph{
		Nodes: []*graph.Node{caller, target},
		Edges: []*graph.Edge{
			{From: "b.go::Svc.Run", To: "c.go::Target", Kind: "calls", Origin: "ast_resolved", Confidence: 1.0, Line: 12},
		},
	}
	payload, err := encodeFindUsages(sg, g)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	_, err = dec.Header()
	require.NoError(t, err)
	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "struct", rows[0]["from_type_flavor"], "owner type's flavor")
	require.Equal(t, "react", rows[0]["from_ui_component"], "caller's own ui_component")
}

func TestEncodeAnalyze_DeadCode(t *testing.T) {
	items := []deadCodeItem{
		{ID: "a.go::Unused", Kind: "function", Name: "Unused", Path: "a.go", Line: 42, Reason: "no incoming edges"},
	}
	payload, err := encodeAnalyze("dead_code", items)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, _ := dec.Header()
	require.Equal(t, "analyze.dead_code", h.Tool)
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "Unused", rows[0]["name"])
	require.Equal(t, "no incoming edges", rows[0]["reason"])
}

// The GCX header carries the closure metadata: rows alone cannot tell a
// complete blast radius from a bounded one.
func TestEncodeAnalyze_ImpactTargetHeaderCarriesTheClosure(t *testing.T) {
	target := &impactTargetScope{
		Symbol: "core.go::Hub", Seeds: []string{"core.go::Hub"},
		Depth:   map[string]int{"core.go::Hub": 0, "callers.go::a": 1},
		ByDepth: map[int]int{1: 1}, Total: 1,
	}
	rows := []impactRow{
		{ID: "core.go::Hub", Name: "Hub", Kind: "function", File: "core.go", Line: 10, Score: 42.5, Risk: "MEDIUM", Target: true},
		{ID: "callers.go::a", Name: "a", Kind: "function", File: "callers.go", Line: 1, Score: 3.5, Risk: "LOW", Depth: 1},
	}
	payload, err := encodeAnalyze("impact.target", impactTargetPayload{Target: target, Rows: rows})
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "analyze.impact.target", h.Tool)
	require.Equal(t, "core.go::Hub", h.Meta["target"])
	require.Equal(t, "1", h.Meta["dependents"])
	decoded, err := dec.All()
	require.NoError(t, err)
	require.Len(t, decoded, 2)
	require.Equal(t, "core.go::Hub", decoded[0]["id"])
	require.Equal(t, "1", decoded[1]["depth"])
}

func TestEncodeAnalyze_ImpactTargetZeroDependentsCarriesCaveat(t *testing.T) {
	target := &impactTargetScope{Symbol: "leaf.go::Leaf", Seeds: []string{"leaf.go::Leaf"}, Depth: map[string]int{"leaf.go::Leaf": 0}, ByDepth: map[int]int{}}
	payload, err := encodeAnalyze("impact.target", impactTargetPayload{
		Target: target,
		Rows:   []impactRow{{ID: "leaf.go::Leaf", Target: true}},
	})
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "true", h.Meta["zero_dependents_unproven"])
}

func TestEncodeAnalyze_UnknownKindFallsBackToGeneric(t *testing.T) {
	payload, err := encodeAnalyze("weird", map[string]any{"x": 1})
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "analyze.weird", h.Tool)
}

func TestEncodeContractsList_FlattensByRepoAndPromotesMethodPath(t *testing.T) {
	rows := []contracts.Contract{
		{
			ID:         "http::GET::/search",
			Type:       contracts.ContractHTTP,
			Role:       contracts.RoleProvider,
			SymbolID:   "cmd/api/main.go::realMain",
			FilePath:   "sapi-backend/cmd/api/main.go",
			Line:       531,
			RepoPrefix: "sapi-backend",
			Confidence: 0.9,
			Meta:       map[string]any{"method": "GET", "path": "/search", "framework": "gin/echo/chi"},
		},
		{
			ID:         "dep::github.com/FindHotel/raa-sdk",
			Type:       contracts.ContractDependency,
			Role:       contracts.RoleConsumer,
			FilePath:   "sapi-backend/go.mod",
			Line:       21,
			RepoPrefix: "sapi-backend",
			Confidence: 1,
			Meta:       map[string]any{"module": "github.com/FindHotel/raa-sdk", "version": "v0.102.0"},
		},
	}
	payload, err := encodeContractsList(rows, len(rows))
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "contracts.list", h.Tool)
	require.Equal(t, "2", h.Meta["total"])
	require.Equal(t, contractFields, h.Fields)

	got, err := dec.All()
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.Equal(t, "http", got[0]["type"])
	require.Equal(t, "provider", got[0]["role"])
	require.Equal(t, "sapi-backend", got[0]["repo"])
	require.Equal(t, "GET", got[0]["method"])
	require.Equal(t, "/search", got[0]["path"])
	require.Equal(t, "531", got[0]["line"])
	require.Equal(t, "framework=gin/echo/chi", got[0]["meta"], "method/path must be excluded from meta column")

	require.Equal(t, "dependency", got[1]["type"])
	require.Equal(t, "", got[1]["method"])
	require.Equal(t, "", got[1]["path"])
	require.Equal(t, "module=github.com/FindHotel/raa-sdk;version=v0.102.0", got[1]["meta"])
}

func TestEncodeContractsCheck_EmitsThreeSections(t *testing.T) {
	provider := contracts.Contract{
		ID: "http::GET::/x", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: "a/provider.go", Line: 10, RepoPrefix: "a",
	}
	consumer := contracts.Contract{
		ID: "http::GET::/x", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		FilePath: "b/consumer.go", Line: 20, RepoPrefix: "b",
	}
	orphanProv := contracts.Contract{
		ID: "http::GET::/dead", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: "a/dead.go", Line: 5, RepoPrefix: "a", Meta: map[string]any{"method": "GET", "path": "/dead"},
	}
	orphanCons := contracts.Contract{
		ID: "http::GET::/nowhere", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		FilePath: "c/lost.go", Line: 8, RepoPrefix: "c",
	}
	result := contracts.MatchResult{
		Matched: []contracts.CrossLink{
			{ContractID: "http::GET::/x", Provider: provider, Consumer: consumer, CrossRepo: true},
		},
		OrphanProviders: []contracts.Contract{orphanProv},
		OrphanConsumers: []contracts.Contract{orphanCons},
	}
	payload, err := encodeContractsCheck(result)
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	h1, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "contracts.check.matched", h1.Tool)
	require.Equal(t, "1", h1.Meta["count"])
	matched, err := dec.All()
	require.NoError(t, err)
	require.Len(t, matched, 1)
	require.Equal(t, "http::GET::/x", matched[0]["contract_id"])
	require.Equal(t, "true", matched[0]["cross_repo"])
	require.Equal(t, "a", matched[0]["provider_repo"])
	require.Equal(t, "b", matched[0]["consumer_repo"])

	h2, err := dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "contracts.check.orphan_providers", h2.Tool)
	orphans, err := dec.All()
	require.NoError(t, err)
	require.Len(t, orphans, 1)
	require.Equal(t, "GET", orphans[0]["method"])
	require.Equal(t, "/dead", orphans[0]["path"])

	h3, err := dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "contracts.check.orphan_consumers", h3.Tool)
	cons, err := dec.All()
	require.NoError(t, err)
	require.Len(t, cons, 1)
	require.Equal(t, "c", cons[0]["repo"])
}

func TestEncodeEditingContext_FourSectionsWithFileMeta(t *testing.T) {
	file := map[string]any{"id": "pkg/foo.go", "language": "go"}
	defines := []map[string]any{
		{"id": "pkg/foo.go::Foo", "kind": "function", "name": "Foo", "start_line": 10, "signature": "func Foo()"},
	}
	imports := []map[string]any{
		{"id": "external::fmt", "external": true},
	}
	calledBy := []map[string]any{
		{"id": "pkg/bar.go::Bar", "name": "Bar", "file_path": "pkg/bar.go", "start_line": 5},
	}
	calls := []map[string]any{
		{"id": "pkg/baz.go::Baz", "name": "Baz", "file_path": "pkg/baz.go", "start_line": 3},
	}
	payload, err := encodeEditingContext(file, defines, imports, calledBy, calls, "etag-xyz", "")
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "get_editing_context.defines", h.Tool)
	require.Equal(t, "etag-xyz", h.Meta["etag"])
	require.Equal(t, "go", h.Meta["language"])
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "func Foo()", rows[0]["sig"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "get_editing_context.imports", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "true", rows[0]["external"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "get_editing_context.called_by", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "pkg/bar.go", rows[0]["path"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "get_editing_context.calls", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "Baz", rows[0]["name"])
}

func TestEncodeSmartContext_OmitsEmptySections(t *testing.T) {
	result := map[string]any{
		"task": "add a tool",
		"relevant_symbols": []map[string]any{
			{"id": "a.go::Foo", "kind": "function", "name": "Foo", "file_path": "a.go", "start_line": 10, "signature": "func Foo()"},
		},
		"related_test_files": []string{"a_test.go"},
		"files_to_edit":      []string{"a.go", "a_test.go"},
	}
	payload, err := encodeSmartContext(result)
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "smart_context.symbols", h.Tool)
	require.Equal(t, "1", h.Meta["count"])
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "Foo", rows[0]["name"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "smart_context.tests", h.Tool, "cross_repo/entry_file/callers/callees must be skipped when empty")
	rows, _ = dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "a_test.go", rows[0]["path"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "smart_context.files", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 2)
}

func TestEncodeSmartContext_IncludesAllSectionsWhenPopulated(t *testing.T) {
	result := map[string]any{
		"task": "trace auth",
		"relevant_symbols": []map[string]any{
			{"id": "auth.go::Check", "kind": "function", "name": "Check", "file_path": "auth.go", "start_line": 20},
		},
		"cross_repo_dependencies": []map[string]any{
			{"id": "sdk.go::Auth", "kind": "type", "name": "Auth", "file_path": "sdk/auth.go", "repo_prefix": "sdk", "edge_kind": "calls"},
		},
		"entry_file_symbols": []string{"function Check (line 20)"},
		"callers":            []string{"main.go::main"},
		"callees":            []string{"auth.go::verify"},
		"related_test_files": []string{"auth_test.go"},
		"files_to_edit":      []string{"auth.go"},
	}
	payload, err := encodeSmartContext(result)
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	tools := []string{}
	if h, err := dec.Header(); err == nil {
		tools = append(tools, h.Tool)
		_, _ = dec.All()
		for {
			h2, err := dec.NextSection()
			if err != nil {
				break
			}
			tools = append(tools, h2.Tool)
			_, _ = dec.All()
		}
	}
	require.Equal(t, []string{
		"smart_context.symbols",
		"smart_context.cross_repo",
		"smart_context.entry_file",
		"smart_context.callers",
		"smart_context.callees",
		"smart_context.tests",
		"smart_context.files",
	}, tools)
}

func TestRequestedFormat_CoversCompactAndFormatArgs(t *testing.T) {
	f := wire.ParseFormat("gcx")
	require.Equal(t, wire.FormatGCX, f)
	require.Equal(t, wire.FormatText, wire.ParseFormat("compact"))
	require.Equal(t, wire.FormatJSON, wire.ParseFormat(""))
}

func TestEncodePrefetchContext_RoundTrip(t *testing.T) {
	n := newTestNode("a.go::Foo", "Foo", graph.KindFunction, "a.go", 10)
	cands := []prefetchCandidate{{
		Node:            n,
		ID:              n.ID,
		Kind:            string(n.Kind),
		FilePath:        n.FilePath,
		StartLine:       n.StartLine,
		Reason:          "matches task keyword",
		Confidence:      0.825,
		SearchRelevance: 0.9,
		GraphProximity:  0.5,
		CommunityBonus:  0.0,
	}}
	payload, err := encodePrefetchContext(cands, 1, false, false)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "prefetch_context", h.Tool)
	require.Equal(t, "1", h.Meta["total"])
	require.Equal(t, "false", h.Meta["truncated"])
	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "a.go::Foo", rows[0]["id"])
	require.Equal(t, "matches task keyword", rows[0]["reason"])
	require.Equal(t, "0.825", rows[0]["confidence"])
}

// TestEncodePrefetchContext_IncludeSourceAddsField pins the schema
// flip when include_source is set — the encoder must add a `source`
// column rather than emit it as a meta field, so a decoder iterating
// rows sees the source inline.
func TestEncodePrefetchContext_IncludeSourceAddsField(t *testing.T) {
	n := newTestNode("a.go::Foo", "Foo", graph.KindFunction, "a.go", 10)
	cands := []prefetchCandidate{{
		Node:      n,
		ID:        n.ID,
		Kind:      string(n.Kind),
		FilePath:  n.FilePath,
		StartLine: n.StartLine,
		Source:    "func Foo() {}\n",
	}}
	payload, err := encodePrefetchContext(cands, 1, false, true)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, _ := dec.Header()
	require.Contains(t, h.Fields, "source")
	rows, _ := dec.All()
	require.Equal(t, "func Foo() {}\n", rows[0]["source"])
}

func TestEncodeChangeImpact_SummaryAndEntries(t *testing.T) {
	result := map[string]any{
		"risk":                 analysis.RiskHigh,
		"summary":              "high blast radius",
		"total_affected":       2,
		"cross_repo_impact":    false,
		"affected_processes":   []string{"checkout", "billing"},
		"affected_communities": []string{"core"},
		"test_files":           []string{"foo_test.go"},
		"by_depth": map[int][]analysis.ImpactEntry{
			1: {{ID: "a.go::Foo", Name: "Foo", Kind: "function", FilePath: "a.go", Line: 10, EdgeConfidence: 0.95, ConfidenceLabel: "EXTRACTED"}},
			2: {{ID: "b.go::Bar", Name: "Bar", Kind: "method", FilePath: "b.go", Line: 22}},
		},
		"cross_community_warning": "",
		"community_note":          "change is community-local",
	}
	payload, err := encodeChangeImpact(result)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	// Section 1 — summary.
	h, _ := dec.Header()
	require.Equal(t, "explain_change_impact.summary", h.Tool)
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, string(analysis.RiskHigh), rows[0]["risk"])
	require.Equal(t, "checkout,billing", rows[0]["processes"])

	// Section 2 — entries.
	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "explain_change_impact.entries", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 2)
	require.Equal(t, "1", rows[0]["depth"])
	require.Equal(t, "a.go::Foo", rows[0]["id"])
	require.Equal(t, "2", rows[1]["depth"])
}

// TestEncodeChangeImpact_ContractsSection emits the contract-impact
// counters when the JSON path attaches `contract_impact`. Skipping
// this section when the field is absent is also pinned here.
func TestEncodeChangeImpact_ContractsSection(t *testing.T) {
	result := map[string]any{
		"risk":           analysis.RiskMedium,
		"summary":        "ok",
		"total_affected": 0,
		"by_depth":       map[int][]analysis.ImpactEntry{},
		"contract_impact": &contractImpact{
			Affected: []contractImpactEntry{{ContractID: "x"}},
			Breaking: 1,
			Warning:  2,
			Info:     3,
		},
	}
	payload, err := encodeChangeImpact(result)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	// Read summary, then advance past entries to reach contracts.
	_, _ = dec.Header()
	_, _ = dec.All()
	_, err = dec.NextSection() // entries
	require.NoError(t, err)
	_, _ = dec.All()
	h, err := dec.NextSection() // contracts
	require.NoError(t, err)
	require.Equal(t, "explain_change_impact.contracts", h.Tool)
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "1", rows[0]["breaking"])
	require.Equal(t, "1", rows[0]["affected"])
}

func TestEncodeCheckGuards_RowsAndMeta(t *testing.T) {
	violations := []analysis.GuardViolation{
		{RuleName: "no_cross_layer", Kind: "boundary", Description: "mcp imports daemon"},
	}
	payload, err := encodeCheckGuards(violations, false)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, _ := dec.Header()
	require.Equal(t, "check_guards", h.Tool)
	require.Equal(t, "1", h.Meta["total"])
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "no_cross_layer", rows[0]["rule_name"])
	// Description is a row value (tab-delimited) so it can carry
	// spaces; pin that here so a future schema rev can't accidentally
	// move it into meta.
	require.Equal(t, "mcp imports daemon", rows[0]["description"])

	// No-rules path: status is a single-token meta flag (the wire
	// header parser splits on raw spaces, so multi-word values would
	// corrupt the header).
	payload, err = encodeCheckGuards(nil, true)
	require.NoError(t, err)
	dec = wire.NewDecoder(strings.NewReader(string(payload)))
	h, err = dec.Header()
	require.NoError(t, err)
	require.Equal(t, "0", h.Meta["total"])
	require.Equal(t, "no_rules_configured", h.Meta["status"])
}

func TestEncodeFeedbackQuery_AllSectionsEmittedEvenWhenEmpty(t *testing.T) {
	stats := map[string]any{
		"total_entries": 5,
		"accuracy":      0.8,
		"most_useful":   []any{},
		"most_missed":   []any{},
		"most_demoted":  []any{},
	}
	payload, err := encodeFeedbackQuery(stats)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	tools := []string{}
	h, err := dec.Header()
	require.NoError(t, err)
	tools = append(tools, h.Tool)
	_, _ = dec.All()
	for {
		h, err := dec.NextSection()
		if err != nil {
			break
		}
		tools = append(tools, h.Tool)
		_, _ = dec.All()
	}
	require.Equal(t, []string{
		"feedback.summary",
		"feedback.most_useful",
		"feedback.most_missed",
		"feedback.most_demoted",
	}, tools)
}
