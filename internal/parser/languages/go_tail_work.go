package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// goTailWork shares the existing Go type-reference preorder with function-value
// capture and GoFrame's three unpruned whole-tree scans. Each queue retains the named-node preorder;
// unnamed nodes cannot match these grammar kinds. Emission remains staged so
// later type parameters and Bind calls are known before their consumers run.
// Wrappers stay inside the caller's single scratch lifetime and never escape
// into the owned graph.
type goTailWork struct {
	references     []goReferenceFact
	referenceScope goReferenceScope
	typeParams     map[string]bool
	typeContainers []*sitter.Node
	goframeCalls   []*sitter.Node
	goframeTypes   []*sitter.Node
	goframeMethods []*sitter.Node
}

// Facts retain owned names and scratch-borrowed nodes only until both reference
// phases complete. The full preorder supplies parent context without another
// native Parent lookup for every identifier. Only known symbol names and all
// qualified selectors need storage; declarations are complete before collection.
type goReferenceFact struct {
	node, parent           *sitter.Node
	kind, parentKind, name string
}
type goReferenceScope struct {
	lang, fileID      string
	functions, values map[string]bool
	ranges            []funcRange
}

func collectGoTailWork(root *sitter.Node, src []byte, result *parser.ExtractionResult, filePath string, ranges []funcRange) goTailWork {
	work := goTailWork{typeParams: map[string]bool{}}
	work.referenceScope = goReferenceScope{functions: map[string]bool{}, values: map[string]bool{}, ranges: ranges}
	scope := &work.referenceScope
	for _, n := range result.Nodes {
		if n == nil || n.FilePath != filePath {
			continue
		}
		switch n.Kind {
		case graph.KindFile:
			if scope.lang == "" {
				scope.lang = n.Language
			}
			if scope.fileID == "" {
				scope.fileID = n.ID
			}
		case graph.KindFunction, graph.KindMethod:
			scope.functions[n.Name] = true
		case graph.KindConstant, graph.KindVariable:
			if isDistinctiveConstName(n.Name) {
				scope.values[n.Name] = true
			}
		}
	}
	if root == nil {
		return work
	}
	visit := func(n, parent *sitter.Node, t, parentKind string) {
		switch t {
		// These are the only Go grammar kinds accepted by FnValue's ID spec
		// or special-form normalizer; its semantic callback remains unchanged.
		case "identifier":
			name := n.Content(src)
			if scope.functions[name] || scope.values[name] {
				work.references = append(work.references, goReferenceFact{n, parent, t, parentKind, name})
			}
		case "selector_expression":
			work.references = append(work.references, goReferenceFact{n, parent, t, parentKind, fnRefNodeName(n, src)})
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
			if parent == nil || parentKind != "type_parameter_list" {
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
	}
	// Callback returns do not prune: ERROR, generic containers, anonymous tokens
	// and nested declarations all retain the original all-child preorder.
	var walk func(*sitter.Node, *sitter.Node, string)
	walk = func(n, parent *sitter.Node, parentKind string) {
		if n == nil {
			return
		}
		kind := n.Type()
		visit(n, parent, kind, parentKind)
		for i, count := 0, int(n.ChildCount()); i < count; i++ {
			walk(n.Child(i), n, kind)
		}
	}
	parent := root.Parent()
	parentKind := ""
	if parent != nil {
		parentKind = parent.Type()
	}
	walk(root, parent, parentKind)
	return work
}
