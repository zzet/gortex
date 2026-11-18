package languages

import (
	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// stampGoBodyMetrics shares one traversal across the three complexity metrics
// and loop signals. Go uses the same pruning and loop sets for these analyses;
// the independent helpers remain the parity oracle and serve other languages.
func stampGoBodyMetrics(node *graph.Node, body *sitter.Node, src []byte) {
	if node == nil || body == nil {
		return
	}
	spec := loopSignalTables["go"]
	cyc, cognitive, maxLoop := 1, 0, 0
	maxAccess := 0
	linear, alloc, recur := false, false, false
	var walk func(*sitter.Node, int, int)
	walk = func(n *sitter.Node, nesting, loopDepth int) {
		if n == nil {
			return
		}
		kind := n.Type()
		if goComplexityNodes[kind] {
			cyc++
			cognitive += 1 + nesting
		}
		depth := loopDepth
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
		// Legacy walkers count the current node before pruning its descendants.
		if goComplexitySkip[kind] {
			return
		}
		childNesting := nesting
		if goNestingTypes[kind] {
			childNesting++
		}
		for i, count := 0, int(n.NamedChildCount()); i < count; i++ {
			walk(n.NamedChild(i), childNesting, depth)
		}
	}
	walk(body, 0, 0)
	if cyc <= 1 && cognitive <= 1 && maxLoop == 0 && maxAccess < 3 && !linear && !alloc && !recur {
		return
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
}
