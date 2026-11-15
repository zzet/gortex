package tstypes

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/javascript"
	"github.com/zzet/gortex/internal/parser/tsitter/tsx"
	"github.com/zzet/gortex/internal/parser/tsitter/typescript"
)

// TypeScriptSpec adapts the engine to the TS / TSX / JS grammar
// family. One provider serves both graph languages: .tsx picks the TSX
// grammar (JSX nodes), .js/.jsx/.mjs/.cjs the JavaScript grammar
// (where annotations don't exist and the binder leans on `new`
// inference), everything else plain TypeScript.
func TypeScriptSpec() *LangSpec {
	tsGrammar := typescript.GetLanguage()
	tsxGrammar := tsx.GetLanguage()
	jsGrammar := javascript.GetLanguage()
	return &LangSpec{
		ProviderName: "typescript-types",
		Languages:    []string{"typescript", "javascript"},
		CandidateLanguages: []string{
			"typescript", "tsx", "ts", "javascript", "jsx", "js",
		},
		GrammarFor: func(filePath string) *sitter.Language {
			lower := strings.ToLower(filePath)
			switch {
			case strings.HasSuffix(lower, ".tsx"):
				return tsxGrammar
			case strings.HasSuffix(lower, ".js"), strings.HasSuffix(lower, ".jsx"),
				strings.HasSuffix(lower, ".mjs"), strings.HasSuffix(lower, ".cjs"):
				return jsGrammar
			default:
				return tsGrammar
			}
		},
		TypeDeclTypes: map[string]bool{
			"class_declaration":          true,
			"abstract_class_declaration": true,
			"interface_declaration":      true,
		},
		FuncDeclTypes: map[string]bool{
			"function_declaration":           true,
			"generator_function_declaration": true,
			"method_definition":              true,
			"arrow_function":                 true,
			"function_expression":            true,
		},
		SelfName:     "this",
		TypeDeclName: nameField,
		Supertypes:   tsSupertypes,
		Fields:       tsFields,
		Params:       tsParams,
		ReturnType: func(fn *sitter.Node, src []byte) string {
			return typeAnnotationText(fn.ChildByFieldName("return_type"), src)
		},
		LocalBinding: tsLocalBinding,
		Call:         tsCall,
		NewExprType: func(n *sitter.Node, src []byte) string {
			if n.Type() != "new_expression" {
				return ""
			}
			ctor := n.ChildByFieldName("constructor")
			if ctor == nil || ctor.Type() != "identifier" {
				return ""
			}
			return ctor.Content(src)
		},
		FieldRef: func(n *sitter.Node, src []byte) (string, bool) {
			if n.Type() != "member_expression" {
				return "", false
			}
			obj := n.ChildByFieldName("object")
			if obj == nil || obj.Type() != "this" {
				return "", false
			}
			return fieldText(n, "property", src), true
		},
		Imports: tsImports,
	}
}

func tsSupertypes(n *sitter.Node, src []byte) []SuperRef {
	var out []SuperRef
	collect := func(c *sitter.Node, kind graph.EdgeKind) {
		for t := range c.NamedChildren() {
			switch t.Type() {
			case "identifier", "type_identifier", "generic_type", "nested_type_identifier", "member_expression":
				out = append(out, SuperRef{Name: t.Content(src), Kind: kind, Line: nodeLine(t)})
			}
		}
	}
	switch n.Type() {
	case "class_declaration", "abstract_class_declaration":
		for i := 0; i < int(n.ChildCount()); i++ {
			h := n.Child(i)
			if h == nil || h.Type() != "class_heritage" {
				continue
			}
			sawClause := false
			for c := range h.NamedChildren() {
				switch c.Type() {
				case "extends_clause":
					sawClause = true
					collect(c, graph.EdgeExtends)
				case "implements_clause":
					sawClause = true
					collect(c, graph.EdgeImplements)
				}
			}
			if !sawClause {
				// JavaScript grammar: class_heritage is `extends <expr>`
				// with the expression as a direct child.
				collect(h, graph.EdgeExtends)
			}
		}
	case "interface_declaration":
		for i := 0; i < int(n.ChildCount()); i++ {
			c := n.Child(i)
			if c != nil && (c.Type() == "extends_type_clause" || c.Type() == "extends_clause") {
				collect(c, graph.EdgeExtends)
			}
		}
	}
	return out
}

func tsFields(n *sitter.Node, src []byte) []Binding {
	body := n.ChildByFieldName("body")
	if body == nil {
		return nil
	}
	var out []Binding
	for c := range body.NamedChildren() {
		if c.Type() != "public_field_definition" && c.Type() != "field_definition" {
			continue
		}
		name := fieldText(c, "name", src)
		if name == "" {
			continue
		}
		typ := typeAnnotationText(c.ChildByFieldName("type"), src)
		if typ == "" {
			// `count = new Counter()` — infer from the initializer.
			if v := c.ChildByFieldName("value"); v != nil && v.Type() == "new_expression" {
				if ctor := v.ChildByFieldName("constructor"); ctor != nil && ctor.Type() == "identifier" {
					typ = ctor.Content(src)
				}
			}
		}
		out = append(out, Binding{Name: name, Type: typ, Line: nodeLine(c)})
	}
	return out
}

func tsParams(fn *sitter.Node, src []byte) []Binding {
	params := fn.ChildByFieldName("parameters")
	if params == nil {
		return nil
	}
	var out []Binding
	for p := range params.NamedChildren() {
		switch p.Type() {
		case "required_parameter", "optional_parameter":
			pattern := p.ChildByFieldName("pattern")
			if pattern == nil || pattern.Type() != "identifier" {
				continue
			}
			out = append(out, Binding{
				Name: pattern.Content(src),
				Type: typeAnnotationText(p.ChildByFieldName("type"), src),
				Line: nodeLine(p),
			})
		case "identifier":
			// JavaScript grammar: parameters are bare identifiers.
			out = append(out, Binding{Name: p.Content(src), Line: nodeLine(p)})
		}
	}
	return out
}

func tsLocalBinding(n *sitter.Node, src []byte) (LocalBind, bool) {
	switch n.Type() {
	case "variable_declarator":
		name := n.ChildByFieldName("name")
		if name == nil || name.Type() != "identifier" {
			return LocalBind{}, false
		}
		init := n.ChildByFieldName("value")
		// An arrow function initializer is a callable, not a typed
		// local; FuncDeclTypes handles its scope.
		if init != nil && (init.Type() == "arrow_function" || init.Type() == "function_expression") {
			return LocalBind{}, false
		}
		return LocalBind{
			Name:     name.Content(src),
			DeclType: typeAnnotationText(n.ChildByFieldName("type"), src),
			Init:     init,
		}, true
	case "assignment_expression":
		left := n.ChildByFieldName("left")
		if left == nil || left.Type() != "identifier" {
			return LocalBind{}, false
		}
		return LocalBind{Name: left.Content(src), Init: n.ChildByFieldName("right")}, true
	}
	return LocalBind{}, false
}

func tsCall(n *sitter.Node, src []byte) (*sitter.Node, string, bool) {
	if n.Type() != "call_expression" {
		return nil, "", false
	}
	fn := n.ChildByFieldName("function")
	if fn == nil || fn.Type() != "member_expression" {
		return nil, "", false
	}
	obj := fn.ChildByFieldName("object")
	if obj == nil {
		return nil, "", false
	}
	prop := fn.ChildByFieldName("property")
	if prop == nil || prop.Type() != "property_identifier" {
		return nil, "", false
	}
	return obj, prop.Content(src), true
}

func tsImports(root *sitter.Node, src []byte) []Import {
	var out []Import
	for stmt := range root.NamedChildren() {
		if stmt.Type() != "import_statement" {
			continue
		}
		source := fieldText(stmt, "source", src)
		source = strings.Trim(source, "\"'`")
		if source == "" {
			continue
		}
		clause := firstChildOfType(stmt, "import_clause")
		if clause == nil {
			continue
		}
		for c := range clause.NamedChildren() {
			switch c.Type() {
			case "identifier": // default import
				out = append(out, Import{Local: c.Content(src), Path: source})
			case "named_imports":
				for spec := range c.NamedChildren() {
					if spec.Type() != "import_specifier" {
						continue
					}
					local := fieldText(spec, "alias", src)
					if local == "" {
						local = fieldText(spec, "name", src)
					}
					if local != "" {
						out = append(out, Import{Local: local, Path: source})
					}
				}
			}
		}
	}
	return out
}

// typeAnnotationText unwraps a `: T` type_annotation node to T's text.
func typeAnnotationText(annot *sitter.Node, src []byte) string {
	if annot == nil {
		return ""
	}
	if annot.Type() == "type_annotation" {
		if annot.NamedChildCount() == 0 {
			return ""
		}
		return annot.NamedChild(0).Content(src)
	}
	return annot.Content(src)
}
