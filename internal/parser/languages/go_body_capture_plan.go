package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// goBodyCapturePlan holds only this declaration's borrowed nodes and owned
// names. Collection emits no graph facts: metrics and shape phases retain
// their original positions, and every queue is consumed before scratch reset.
type goBodyCapturePlan struct {
	closures []goPlannedClosure
	channels []*sitter.Node
	flow     []*sitter.Node
}
type goPlannedClosure struct {
	node   *sitter.Node
	locals map[string]bool
	refs   []goClosureReferenceFact
}
type goClosureReferenceFact struct {
	node *sitter.Node
	name string
}

func collectGoBodyCapturePlan(node *graph.Node, body *sitter.Node, src []byte) *goBodyCapturePlan {
	if node == nil || body == nil {
		return nil
	}
	plan := &goBodyCapturePlan{}
	spec := loopSignalTables["go"]
	cyc, cognitive, maxLoop := 1, 0, 0
	maxAccess := 0
	linear, alloc, recur := false, false, false
	var walk func(*sitter.Node, int, int, bool, bool)
	walk = func(n *sitter.Node, nesting, loopDepth int, metricsActive, flowActive bool) {
		if n == nil {
			return
		}
		kind := n.Type()
		depth := loopDepth
		if metricsActive {
			if goComplexityNodes[kind] {
				cyc++
				cognitive += 1 + nesting
			}
			depth = loopDepth
			if goLoopTypes[kind] {
				depth++
				if depth > maxLoop {
					maxLoop = depth
				}
			}
			if src != nil {
				if kind == spec.memberType {
					if d := memberChainSegments(n, spec); d > maxAccess {
						maxAccess = d
					}
				}
				// Loop membership comes from ancestors, before entering this node's own
				// loop. This preserves header/body and same-line statement semantics.
				if loopDepth > 0 {
					if kind == spec.callType {
						if name := calleeFinalName(n, src, spec); name != "" {
							if linearScanCallNames[name] {
								linear = true
							}
							if spec.allocCallNames[name] {
								alloc = true
							}
							if name == node.Name {
								recur = true
							}
						}
					}
					if spec.compositeTypes[kind] {
						alloc = true
					}
				}
			}

		}
		// All four body analyses count/inspect the current node before their own
		// pruning. Nested named declarations prune metrics only, not other phases.
		if kind == "func_literal" {
			plan.closures = append(plan.closures, collectGoClosureCapturePlan(n, src))
			return
		}
		switch kind {
		case "send_statement":
			plan.channels = append(plan.channels, n)
		case "unary_expression":
			if op := n.ChildByFieldName("operator"); op != nil && op.Content(src) == "<-" {
				plan.channels = append(plan.channels, n)
			}
		}
		childFlow := flowActive
		if flowActive && goBodyFlowDispatch(kind) {
			plan.flow = append(plan.flow, n)
			childFlow = false
		}
		childMetrics := metricsActive && !goComplexitySkip[kind]
		childNesting := nesting
		if goNestingTypes[kind] {
			childNesting++
		}
		for i, count := 0, int(n.NamedChildCount()); i < count; i++ {
			walk(n.NamedChild(i), childNesting, depth, childMetrics, childFlow)
		}
	}
	walk(body, 0, 0, true, true)
	if cyc <= 1 && cognitive <= 1 && maxLoop == 0 && maxAccess < 3 && !linear && !alloc && !recur {
		return plan
	}
	if node.Meta == nil {
		node.Meta = map[string]any{}
	}
	ApplyComplexityMeta(node.Meta, cyc, cognitive, maxLoop)
	if maxAccess >= 3 {
		node.Meta["max_access_depth"] = maxAccess
	}
	if linear {
		node.Meta["linear_scan_in_loop"] = true
	}
	if alloc {
		node.Meta["alloc_in_loop"] = true
	}
	if recur {
		node.Meta["recursion_in_loop"] = true
	}
	return plan
}

func goBodyFlowDispatch(kind string) bool {
	switch kind {
	case "short_var_declaration", "var_spec", "assignment_statement", "return_statement", "range_clause", "call_expression":
		return true
	}
	return false
}

// Local discovery and reference snapshots share one closure-body walk.
// Parameter/result seeding and the rare initializer/type-switch subtree
// operations deliberately retain the original broad suppression semantics.
func collectGoClosureCapturePlan(funcLit *sitter.Node, src []byte) goPlannedClosure {
	work := goPlannedClosure{node: funcLit, locals: map[string]bool{}}
	if funcLit == nil {
		return work
	}
	body := funcLit.ChildByFieldName("body")
	if body == nil {
		return work
	}
	locals := work.locals
	seenReferences := map[string]bool{}
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
	walkGoNodes(body, func(n *sitter.Node) bool {
		kind := n.Type()
		if kind == "func_literal" {
			return false
		}
		switch kind {
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
		if kind == "identifier" {
			name := n.Content(src)
			// Known locals only accumulate. Later declarations still suppress
			// an earlier queued reference in the final emission filter.
			if name != "" && name != "_" && !isGoBuiltinOrKeyword(name) && !locals[name] && !seenReferences[name] && isGoClosureValueRef(n) {
				seenReferences[name] = true
				work.refs = append(work.refs, goClosureReferenceFact{node: n, name: name})
			}
		}
		return true
	})
	return work
}

func emitGoPlannedClosures(ownerID string, ownerStartLine int, src []byte, filePath string, result *parser.ExtractionResult, plan *goBodyCapturePlan) {
	idx := 0
	for i := range plan.closures {
		n := plan.closures[i].node
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
		emitGoPlannedClosureCaptures(closureID, &plan.closures[i], filePath, result)
	}
}

func emitGoPlannedClosureCaptures(closureID string, work *goPlannedClosure, filePath string, result *parser.ExtractionResult) {
	for _, ref := range work.refs {
		name := ref.name
		if work.locals[name] {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: closureID, To: "unresolved::" + name, Kind: graph.EdgeCaptures,
			FilePath: filePath, Line: int(ref.node.StartPoint().Row) + 1,
			Origin: graph.OriginASTInferred, Meta: map[string]any{"name": name},
		})
	}
}

func emitGoPlannedChannels(ownerID string, src []byte, filePath string, result *parser.ExtractionResult, plan *goBodyCapturePlan) {
	for _, n := range plan.channels {
		switch n.Type() {
		case "func_literal":
			// Don't recurse into nested closures — handled
			// elsewhere. Same convention as emitGoClosureNodes.
			continue
		case "send_statement":
			channel := n.ChildByFieldName("channel")
			if channel != nil {
				name := strings.TrimSpace(channel.Content(src))
				if name != "" {
					result.Edges = append(result.Edges, &graph.Edge{
						From:     ownerID,
						To:       "unresolved::" + name,
						Kind:     graph.EdgeSends,
						FilePath: filePath,
						Line:     int(n.StartPoint().Row) + 1,
						Origin:   graph.OriginASTInferred,
					})
				}
			}
		case "unary_expression":
			// Receive operations have operator "<-" and an
			// operand pointing at the channel.
			// The collector admitted only <-; nested receives retain preorder.
			operand := n.ChildByFieldName("operand")
			if operand != nil {
				name := strings.TrimSpace(operand.Content(src))
				if name != "" {
					result.Edges = append(result.Edges, &graph.Edge{
						From:     ownerID,
						To:       "unresolved::" + name,
						Kind:     graph.EdgeRecvs,
						FilePath: filePath,
						Line:     int(n.StartPoint().Row) + 1,
						Origin:   graph.OriginASTInferred,
					})
				}
			}
			// `for v := range ch` is the third receive shape in
			// Go but distinguishing channel-range from map-range
			// or slice-range needs type info we don't propagate
			// here. Emitting a recv edge for every range target
			// would over-fire on every map/slice iteration; the
			// alternative — name-pattern heuristics — has worse
			// precision than just leaving the gap. Tracked as a
			// v1 limitation; a future pass that threads
			// paramsByFunc into the channel walker can filter
			// range RHSes by chan-typed variables only.
		}
	}
}

func emitGoPlannedDataflow(ownerID string, ownerStartLine int, plan *goBodyCapturePlan, paramsByName map[string]string, imports map[string]string, src []byte, filePath string, result *parser.ExtractionResult) {
	if plan == nil {
		return
	}
	scope := newGoFlowScope()
	for name := range paramsByName {
		if name == "" || name == "_" {
			continue
		}
		paramID := goParamNodeID(ownerID, name, 0)
		scope.bindings[name] = []string{paramID}
	}
	walker := &goFlowWalker{
		ownerID:        ownerID,
		ownerStartLine: ownerStartLine,
		filePath:       filePath,
		src:            src,
		scope:          scope,
		result:         result,
		emittedLocals:  map[string]struct{}{},
		imports:        imports,
	}
	for _, n := range plan.flow {
		walker.walk(n)
	}
}
func emitGoFunctionShapeWithCapturePlan(ownerID string, defNode *sitter.Node, paramsCap, resultCap *parser.CapturedNode, src []byte, filePath string, declLine int, imports map[string]string, result *parser.ExtractionResult, plan *goBodyCapturePlan) {
	if defNode == nil {
		return
	}
	if plan == nil {
		emitGoFunctionShape(ownerID, defNode, paramsCap, resultCap, src, filePath, declLine, imports, result)
		return
	}
	emitGoParamNodes(ownerID, paramsCap, src, filePath, declLine, result)
	emitGoReturnEdges(ownerID, resultCap, src, filePath, declLine, result)
	emitGoGenericParamNodes(ownerID, defNode, src, filePath, declLine, result)
	emitGoPlannedClosures(ownerID, declLine, src, filePath, result, plan)
	emitGoPlannedChannels(ownerID, src, filePath, result, plan)
	params := goParamNamesFromCapture(paramsCap, src)
	emitGoPlannedDataflow(ownerID, declLine, plan, params, imports, src, filePath, result)
}
