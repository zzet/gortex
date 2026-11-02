package languages

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Keep the old independent body walks as the ordering and scope oracle.
// Call multiplicity is semantic here: nested calls currently emit duplicate
// arg_of rows, and a structural plan must preserve them without deduplication.
func TestGoBodyCapturePlan_OrderedIndependentBodyParity(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"flow_frontier", "var zero int; value := payload; value = pkg.Transform(value); other.Value = value; for i,item := range items { value = item; _=i }; return value"},
		{"nested_calls", "value := outer(mid(inner(payload))); return value"},
		{"channels", "ch <- payload; value := <-ch; select {case ch <- value: value = <-ch; default: value=payload}; return value"},
		{"pruned_closure", "callback := func(){ ch <- payload; value:=outer(payload); _=value; _=func(){ch<-value} }; go func(){ _=payload }(); _=callback; return payload"},
		{"same_line_closures", "_=func(){_=payload}; _=func(){_=other}; return payload"},
		{"metrics", "for _,item:=range items { if item>0 { for i:=0;i<item;i++ { if i>1 {continue}; _=strings.Contains(\"a\",\"b\"); _=make([]int,2) } } }; _=other.A.B.C; return payload"},
		{"trailing_comma", "if payload > 0 { ch <- payload }; value := outer(payload,); return value"},
		{"malformed_body", "value := outer(payload); if ; { ch <- payload }; return value"},
		{"late_closure_locals", "_=func(){ _=later; later:=payload; _=later; var shared int; _=shared; _=other.Field; _=pkg.Value }; return payload"},
		{"closure_ranges", "_=func(){ for k,v:=range items {_=k;_=v;_=payload}; switch value:=any(other).(type){case int:_=value;default:_=payload} }; return payload"},
		{"closure_params_results", "_=func(first,second int, rest ...int)(named int){ _=first;_=second;_=rest;_=named;_=payload; return }; return payload"},
		{"closure_legacy_for_initializer", "_=func(){ for iter:=payload; iter<other.Limit; iter++ {_=iter}; _=payload; _=other }; return payload"},
		{"closure_legacy_typeswitch_nested", "_=func(){ _=leaked; switch selected:=any(other).(type){case int:_=func(){pair:=leaked;_=pair};default:_=payload}; _=leaked }; return payload"},
		{"closure_unicode", "_=func(){ 値:=payload; _=値; _=other; _=func(){_=値;_=hidden} }; return payload"},
		{"closure_late_shadow", "_=func(){ _=payload; if true {payload:=other;_=payload}; _=payload }; return payload"},
		{"method_body", "value:=outer(mid(payload)); ch <- value; _=func(){_=other}; return value"},
	} {
		for _, ending := range []string{"lf", "crlf", "no_final_newline"} {
			t.Run(tc.name+"/"+ending, func(t *testing.T) {
				source := "package body\nimport pkg \"some/pkg\"\n// Target keeps docs.\nfunc Target(ch chan int, payload int, other Data, items []int) int {\n" + tc.body + "\n}\n"
				if tc.name == "method_body" {
					source = strings.Replace(source, "// Target keeps docs.", "type Server struct{}\n// Target keeps docs.", 1)
					source = strings.Replace(source, "func Target(", "func(s *Server) Target(", 1)
				}
				if ending == "crlf" {
					source = strings.ReplaceAll(source, "\n", "\r\n")
				}
				if ending == "no_final_newline" {
					source = strings.TrimSuffix(source, "\n")
				}
				src := []byte(source)
				actual, err := NewGoExtractor().Extract("body_plan.go", src)
				require.NoError(t, err)
				defer actual.ReleaseTree()
				var owner *graph.Node
				for _, n := range actual.Nodes {
					if (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) && n.Name == "Target" {
						owner = n
						break
					}
				}
				require.NotNil(t, owner)
				root := actual.Tree.Tree().RootNode()
				if tc.name == "malformed_body" {
					require.True(t, root.HasError(), "fixture must exercise error-tree pruning")
				}
				var def *sitter.Node
				walkNodes(root, func(n *sitter.Node) {
					if n.Type() == "function_declaration" || n.Type() == "method_declaration" {
						if name := n.ChildByFieldName("name"); name != nil && name.Content(src) == "Target" {
							def = n
						}
					}
				})
				require.NotNil(t, def)
				body := goFuncBody(def)
				require.NotNil(t, body)
				before := goScratchSemantics(t, actual)
				oracle := &parser.ExtractionResult{}
				metricOracle := &graph.Node{Name: owner.Name, Meta: map[string]any{}}
				keys := map[string]bool{"complexity": true, "cognitive": true, "loop_depth": true, "max_access_depth": true, "linear_scan_in_loop": true, "alloc_in_loop": true, "recursion_in_loop": true}
				for key, value := range owner.Meta {
					if !keys[key] {
						metricOracle.Meta[key] = value
					}
				}
				root.WithScratch(func() {
					StampFunctionMetrics(metricOracle, body, "go")
					StampLoopSignals(metricOracle, body, src, "go")
					legacyBodyCaptureClosureNodes(owner.ID, owner.StartLine, body, src, "body_plan.go", oracle)
					emitGoChannelOps(owner.ID, body, src, "body_plan.go", oracle)
					params := goParamNamesFromCapture(&parser.CapturedNode{Node: def.ChildByFieldName("parameters")}, src)
					emitGoDataflow(owner.ID, owner.StartLine, body, params, map[string]string{"pkg": "some/pkg"}, src, "body_plan.go", oracle)
				})
				if tc.name == "closure_legacy_typeswitch_nested" {
					root.WithScratch(func() {
						walkGoNodes(body, func(n *sitter.Node) bool {
							if n.Type() != "func_literal" {
								return true
							}
							locals := map[string]bool{}
							legacyBodyCaptureClosureLocals(n, src, locals)
							t.Logf("untouched legacy locals=%v AST=%s", locals, n.String())
							return false
						})
					})
				}
				require.Equal(t, metricOracle.Meta, owner.Meta, "all body metrics and surrounding metadata retain old independent walkers")
				var actualNodes []*graph.Node
				for _, n := range actual.Nodes {
					if strings.HasPrefix(n.ID, owner.ID+"#closure@") || strings.HasPrefix(n.ID, owner.ID+"#local:") {
						actualNodes = append(actualNodes, n)
					}
				}
				nodeRows := func(nodes []*graph.Node) string {
					b, err := json.Marshal(nodes)
					require.NoError(t, err)
					return string(b)
				}
				if len(oracle.Nodes) == 0 {
					oracle.Nodes = nil
				}
				require.Equal(t, nodeRows(oracle.Nodes), nodeRows(actualNodes), "ordered closure/local nodes and every node field")
				var actualEdges []*graph.Edge
				for _, edge := range actual.Edges {
					switch edge.Kind {
					case graph.EdgeCaptures, graph.EdgeSends, graph.EdgeRecvs, graph.EdgeValueFlow, graph.EdgeArgOf, graph.EdgeReturnsTo:
						actualEdges = append(actualEdges, edge)
					case graph.EdgeMemberOf:
						if strings.HasPrefix(edge.From, owner.ID+"#closure@") || strings.HasPrefix(edge.From, owner.ID+"#local:") {
							actualEdges = append(actualEdges, edge)
						}
					case graph.EdgeSpawns:
						if strings.HasPrefix(edge.To, owner.ID+"#closure@") {
							actualEdges = append(actualEdges, edge)
						}
					}
				}
				require.Equal(t, string(goFnValueEdgeRows(t, oracle.Edges)), string(goFnValueEdgeRows(t, actualEdges)), "complete ordered body edges, internal Meta/confidence, duplicate multiplicity and phase order")
				if tc.name == "nested_calls" {
					count := 0
					for _, edge := range oracle.Edges {
						if edge.Kind == graph.EdgeArgOf && edge.To == "unresolved::inner" {
							count++
						}
					}
					require.GreaterOrEqual(t, count, 4, "negative-control fixture must include repeated nested call emissions")
				}
				require.Equal(t, string(before), string(goScratchSemantics(t, actual)), "independent replay may not mutate graph or owned sidecars")
			})
		}
	}
}

func legacyBodyCaptureClosureNodes(ownerID string, ownerStartLine int, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	idx := 0
	walkGoNodes(body, func(n *sitter.Node) bool {
		if n.Type() != "func_literal" {
			return true
		}
		startLine := int(n.StartPoint().Row) + 1
		// ID anchors on the owner-relative offset (+ prefix) so edits
		// above the enclosing function don't churn the closure's ID.
		// Name keeps the absolute line for human readability in search
		// results / outlines.
		offset := startLine
		if ownerStartLine > 0 {
			offset = startLine - ownerStartLine + 1
		}
		closureID := ownerID + "#closure@+" + strconv.Itoa(offset)
		// If two anonymous functions start on the same line, append a
		// stable suffix so IDs stay unique. Rare in practice but
		// defensive.
		if idx > 0 {
			closureID += "#" + strconv.Itoa(idx)
		}
		idx++
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        closureID,
			Kind:      graph.KindClosure,
			Name:      "closure@" + strconv.Itoa(startLine),
			FilePath:  filePath,
			StartLine: startLine,
			EndLine:   int(n.EndPoint().Row) + 1,
			Language:  "go",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     closureID,
			To:       ownerID,
			Kind:     graph.EdgeMemberOf,
			FilePath: filePath,
			Line:     startLine,
			Origin:   graph.OriginASTResolved,
		})
		// `go func() {...}()` — the closure is launched as a
		// goroutine. Emit an EdgeSpawns from the enclosing function
		// to the closure, mirroring how named-call spawns produce
		// EdgeSpawns to the called function. Without this, agents
		// asking "what goroutines does Run launch?" miss the entire
		// inline-closure pattern.
		if isGoroutineSpawnedClosure(n) {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     ownerID,
				To:       closureID,
				Kind:     graph.EdgeSpawns,
				FilePath: filePath,
				Line:     startLine,
				Origin:   graph.OriginASTResolved,
				Meta: map[string]any{
					"mode": "goroutine",
				},
			})
		}
		// EdgeCaptures: every bare identifier the closure references
		// without locally declaring/binding is a capture of an outer
		// scope binding. The resolver later lands the unresolved::
		// targets on the actual variable / function node.
		legacyBodyCaptureClosureCaptures(closureID, n, src, filePath, result)
		// Don't recurse into nested func_literals — they belong to
		// the inner closure, not the outer one. The outer walker will
		// pick them up when (if) closures-within-closures are
		// supported. For Phase 1 the flat enumeration is sufficient.
		return false
	})
}

// legacyBodyCaptureClosureCaptures walks a func_literal and emits one
// EdgeCaptures per free variable. "Free" means: the identifier is
// used as a value somewhere in the closure body but isn't bound by
// a parameter, short-var-decl, var-spec, or const-spec inside the
// closure. Locally re-declared shadowing names suppress the capture
// (matches Go scoping rules).
//
// v1 limitations:
//   - We don't recurse into nested closures; a nested closure's
//     captures emit against the nested closure node when its own
//     legacyBodyCaptureClosureNodes pass runs.
//   - Selector RHS (`x.field`) only captures the operand `x`; the
//     `.field` part is a field reference, not a free variable.
//   - Identifiers in type position (e.g. `MyType` in `var x MyType`)
//     count as captures — Go closures do close over file-scope type
//     names, and the resolver can land them on the type node.
func legacyBodyCaptureClosureCaptures(closureID string, funcLit *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if funcLit == nil {
		return
	}
	body := funcLit.ChildByFieldName("body")
	if body == nil {
		return
	}
	locals := map[string]bool{}
	legacyBodyCaptureClosureLocals(funcLit, src, locals)

	seen := map[string]bool{}
	walkGoNodes(body, func(n *sitter.Node) bool {
		// Skip nested closures — they own their own captures.
		if n.Type() == "func_literal" {
			return false
		}
		if n.Type() != "identifier" {
			return true
		}
		// Filter out identifiers that aren't actually a value-side
		// reference: the LHS of a short-var-decl, the name on a var
		// or const spec, the parameter name in a function decl, the
		// field portion of a selector — none of these are captures.
		if !legacyBodyCaptureValueRef(n) {
			return true
		}
		name := n.Content(src)
		if name == "" || name == "_" || locals[name] || isGoBuiltinOrKeyword(name) {
			return true
		}
		if seen[name] {
			return true
		}
		seen[name] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     closureID,
			To:       "unresolved::" + name,
			Kind:     graph.EdgeCaptures,
			FilePath: filePath,
			Line:     int(n.StartPoint().Row) + 1,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"name": name,
			},
		})
		return true
	})
}

// legacyBodyCaptureClosureLocals records every name declared inside the
// closure (parameters, return-named results, var/const/short-var
// decls). Mutates locals in place.
func legacyBodyCaptureClosureLocals(funcLit *sitter.Node, src []byte, locals map[string]bool) {
	if funcLit == nil {
		return
	}
	addIdentNames := func(root *sitter.Node) {
		if root == nil {
			return
		}
		walkGoNodes(root, func(n *sitter.Node) bool {
			if n.Type() == "func_literal" {
				return false
			}
			if n.Type() == "identifier" {
				locals[n.Content(src)] = true
			}
			return true
		})
	}
	if params := funcLit.ChildByFieldName("parameters"); params != nil {
		// Parameter names live under parameter_declaration → name.
		walkGoNodes(params, func(n *sitter.Node) bool {
			if n.Type() == "func_literal" {
				return false
			}
			if n.Type() == "parameter_declaration" {
				if name := n.ChildByFieldName("name"); name != nil {
					addIdentNames(name)
				}
			}
			return true
		})
	}
	if res := funcLit.ChildByFieldName("result"); res != nil {
		walkGoNodes(res, func(n *sitter.Node) bool {
			if n.Type() == "func_literal" {
				return false
			}
			if n.Type() == "parameter_declaration" {
				if name := n.ChildByFieldName("name"); name != nil {
					addIdentNames(name)
				}
			}
			return true
		})
	}
	body := funcLit.ChildByFieldName("body")
	if body == nil {
		return
	}
	walkGoNodes(body, func(n *sitter.Node) bool {
		if n.Type() == "func_literal" {
			return false
		}
		switch n.Type() {
		case "short_var_declaration":
			if left := n.ChildByFieldName("left"); left != nil {
				addIdentNames(left)
			}
		case "var_spec", "const_spec":
			if name := n.ChildByFieldName("name"); name != nil {
				addIdentNames(name)
			}
		case "range_clause":
			// `for k, v := range x { … }` — k, v are loop locals.
			if left := n.ChildByFieldName("left"); left != nil {
				addIdentNames(left)
			}
		case "for_statement":
			// Init clause locals (not common in Go but possible).
			if init := n.ChildByFieldName("initializer"); init != nil {
				addIdentNames(init)
			}
		case "type_switch_statement":
			// `switch v := x.(type)` — v is a local.
			walkGoNodes(n, func(c *sitter.Node) bool {
				if c.Type() == "expression_list" {
					addIdentNames(c)
					return false
				}
				return true
			})
		}
		return true
	})
}

// legacyBodyCaptureValueRef reports whether an identifier node is used as
// a value-side reference rather than a binding-site declaration.
// Returns false for the LHS of a short-var-decl, parameter names,
// variable/const declaration names, and the .field portion of a
// selector expression.
func legacyBodyCaptureValueRef(n *sitter.Node) bool {
	parent := n.Parent()
	if parent == nil {
		return true
	}
	switch parent.Type() {
	case "selector_expression":
		// We capture the operand (LHS), not the field name (RHS).
		if field := parent.ChildByFieldName("field"); field != nil && field.Equal(n) {
			return false
		}
	case "parameter_declaration":
		if name := parent.ChildByFieldName("name"); name != nil {
			declared := false
			walkGoNodes(name, func(c *sitter.Node) bool {
				if c.Equal(n) {
					declared = true
					return false
				}
				return true
			})
			if declared {
				return false
			}
		}
	case "var_spec", "const_spec":
		if name := parent.ChildByFieldName("name"); name != nil {
			declared := false
			walkGoNodes(name, func(c *sitter.Node) bool {
				if c.Equal(n) {
					declared = true
					return false
				}
				return true
			})
			if declared {
				return false
			}
		}
	case "short_var_declaration":
		if left := parent.ChildByFieldName("left"); left != nil {
			declared := false
			walkGoNodes(left, func(c *sitter.Node) bool {
				if c.Equal(n) {
					declared = true
					return false
				}
				return true
			})
			if declared {
				return false
			}
		}
	case "range_clause":
		if left := parent.ChildByFieldName("left"); left != nil {
			declared := false
			walkGoNodes(left, func(c *sitter.Node) bool {
				if c.Equal(n) {
					declared = true
					return false
				}
				return true
			})
			if declared {
				return false
			}
		}
	case "function_declaration", "method_declaration":
		// The function/method's own name isn't a free var inside
		// its closure children — but closures can't be at this
		// scope, so this is mostly defensive.
		return false
	case "field_identifier":
		return false
	}
	return true
}

// isGoroutineSpawnedClosure reports whether a func_literal node is
// the operand of an immediately-invoked call inside a go_statement —
// i.e. the `func() {...}` in `go func() {...}()`. The Go grammar
// shape is go_statement → call_expression → func_literal, so two
// Parent() hops are sufficient.
