package languages

import sitter "github.com/zzet/gortex/internal/parser/tsitter"

// goTailWork shares the existing Go type-reference preorder with GoFrame's
// three unpruned whole-tree scans. Each queue retains the named-node preorder;
// unnamed nodes cannot match these grammar kinds. Emission remains staged so
// later type parameters and Bind calls are known before their consumers run.
// Wrappers stay inside the caller's single scratch lifetime and never escape
// into the owned graph.
type goTailWork struct {
	typeParams     map[string]bool
	typeContainers []*sitter.Node
	goframeCalls   []*sitter.Node
	goframeTypes   []*sitter.Node
	goframeMethods []*sitter.Node
}

func collectGoTailWork(root *sitter.Node, src []byte) goTailWork {
	work := goTailWork{typeParams: map[string]bool{}}
	if root == nil {
		return work
	}
	walkNodes(root, func(n *sitter.Node) {
		t := n.Type()
		switch t {
		case "call_expression":
			work.goframeCalls = append(work.goframeCalls, n)
		case "type_spec":
			work.goframeTypes = append(work.goframeTypes, n)
		case "method_declaration":
			work.goframeMethods = append(work.goframeMethods, n)
		}
		if t == "type_arguments" || t == "map_type" || t == "channel_type" {
			work.typeContainers = append(work.typeContainers, n)
			return
		}
		if t != "type_parameter_declaration" && t != "parameter_declaration" {
			return
		}
		// type_parameter_declaration is the modern grammar shape; older
		// grammars reuse parameter_declaration inside type_parameter_list.
		// Only the latter, when its parent is a type_parameter_list,
		// declares type parameters.
		if t == "parameter_declaration" {
			parent := n.Parent()
			if parent == nil || parent.Type() != "type_parameter_list" {
				return
			}
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c == nil {
				continue
			}
			if ct := c.Type(); ct == "identifier" || ct == "type_identifier" {
				work.typeParams[c.Content(src)] = true
			}
		}
	})
	return work
}
