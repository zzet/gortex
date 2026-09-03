package tstypes

import (
	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/csharp"
)

// CSharpSpec adapts the engine to tree-sitter-c-sharp. Like Java the
// types are explicit; the one quirk is the base list, which does not
// syntactically distinguish the base class from interfaces — those
// SuperRefs carry an empty Kind and the apply phase discriminates by
// the resolved node's kind (strictly better than the extractor's
// I-prefix heuristic). `using` directives import namespaces, not
// names, so cross-file types resolve by repo-unique name only.
func CSharpSpec() *LangSpec {
	grammar := csharp.GetLanguage()
	return &LangSpec{
		ProviderName: "csharp-types",
		Languages:    []string{"csharp"},
		GrammarFor:   func(string) *sitter.Language { return grammar },
		TypeDeclTypes: map[string]bool{
			"class_declaration":     true,
			"interface_declaration": true,
			"struct_declaration":    true,
			"record_declaration":    true,
		},
		FuncDeclTypes: map[string]bool{
			"method_declaration":       true,
			"constructor_declaration":  true,
			"local_function_statement": true,
		},
		SelfName:     "this",
		TypeDeclName: nameField,
		Supertypes:   csharpSupertypes,
		Fields:       csharpFields,
		Params:       csharpParams,
		ReturnType: func(fn *sitter.Node, src []byte) string {
			switch fn.Type() {
			case "method_declaration", "local_function_statement":
				if t := fieldText(fn, "returns", src); t != "" {
					return t
				}
				return fieldText(fn, "type", src)
			}
			return ""
		},
		LocalBinding:   csharpLocalBinding,
		MemberDeclName: csharpMemberDeclName,
		Call:           csharpCall,
		NewExprType: func(n *sitter.Node, src []byte) string {
			if n.Type() != "object_creation_expression" {
				return ""
			}
			return fieldText(n, "type", src)
		},
		FieldRef: func(n *sitter.Node, src []byte) (string, bool) {
			if n.Type() != "member_access_expression" {
				return "", false
			}
			obj := n.ChildByFieldName("expression")
			// `this` is a bare keyword node in the current grammar,
			// this_expression in older revisions.
			if obj == nil || (obj.Type() != "this" && obj.Type() != "this_expression") {
				return "", false
			}
			return fieldText(n, "name", src), true
		},
		// using directives bind namespaces, not type names.
		Imports: nil,
	}
}

func csharpSupertypes(n *sitter.Node, src []byte) []SuperRef {
	baseList := n.ChildByFieldName("bases")
	if baseList == nil {
		for i := 0; i < int(n.ChildCount()); i++ {
			if c := n.Child(i); c != nil && c.Type() == "base_list" {
				baseList = c
				break
			}
		}
	}
	if baseList == nil {
		return nil
	}
	kind := graph.EdgeKind("") // apply phase discriminates by node kind
	if n.Type() == "interface_declaration" {
		// An interface's bases can only be interfaces.
		kind = graph.EdgeExtends
	}
	var out []SuperRef
	for entry := range baseList.NamedChildren() {
		name := entry.Content(src)
		if entry.Type() == "primary_constructor_base_type" {
			// `: Base(args)` — the base-class constructor invocation.
			if t := firstChildOfType(entry, "identifier"); t != nil {
				name = t.Content(src)
			}
		}
		if name == "" {
			continue
		}
		out = append(out, SuperRef{Name: name, Kind: kind, Line: nodeLine(entry)})
	}
	return out
}

func csharpFields(n *sitter.Node, src []byte) []Binding {
	body := n.ChildByFieldName("body")
	if body == nil {
		return nil
	}
	var out []Binding
	for c := range body.NamedChildren() {
		switch c.Type() {
		case "field_declaration":
			decl := firstChildOfType(c, "variable_declaration")
			if decl == nil {
				continue
			}
			typ := fieldText(decl, "type", src)
			for d := range decl.NamedChildren() {
				if d.Type() != "variable_declarator" {
					continue
				}
				name := csharpDeclaratorName(d, src)
				if name == "" {
					continue
				}
				out = append(out, Binding{Name: name, Type: typ, Line: nodeLine(d)})
			}
		case "property_declaration":
			name := fieldText(c, "name", src)
			if name == "" {
				continue
			}
			out = append(out, Binding{Name: name, Type: fieldText(c, "type", src), Line: nodeLine(c)})
		}
	}
	return out
}

func csharpParams(fn *sitter.Node, src []byte) []Binding {
	params := fn.ChildByFieldName("parameters")
	if params == nil {
		return nil
	}
	var out []Binding
	for p := range params.NamedChildren() {
		if p.Type() != "parameter" {
			continue
		}
		name := fieldText(p, "name", src)
		if name == "" {
			continue
		}
		out = append(out, Binding{Name: name, Type: fieldText(p, "type", src), Line: nodeLine(p)})
	}
	return out
}

func csharpLocalBinding(n *sitter.Node, src []byte) (LocalBind, bool) {
	switch n.Type() {
	case "local_declaration_statement":
		decl := firstChildOfType(n, "variable_declaration")
		if decl == nil {
			return LocalBind{}, false
		}
		d := firstChildOfType(decl, "variable_declarator")
		if d == nil {
			return LocalBind{}, false
		}
		var init *sitter.Node
		if eq := firstChildOfType(d, "equals_value_clause"); eq != nil {
			// Older grammar revisions wrap the initializer.
			init = eq.NamedChild(int(eq.NamedChildCount()) - 1)
		} else if count := int(d.NamedChildCount()); count > 1 {
			// Current grammar: `s = <expr>` puts the initializer as the
			// declarator's trailing named child.
			init = d.NamedChild(count - 1)
		}
		// Target-typed `new()` carries no type of its own — the
		// declared type is authoritative either way; the engine only
		// falls back to the initializer for `var`.
		return LocalBind{
			Name:     csharpDeclaratorName(d, src),
			DeclType: fieldText(decl, "type", src),
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

// csharpDeclaratorName handles both grammar revisions: a `name` field
// or a bare identifier child.
func csharpDeclaratorName(d *sitter.Node, src []byte) string {
	if name := fieldText(d, "name", src); name != "" {
		return name
	}
	if id := firstChildOfType(d, "identifier"); id != nil {
		return id.Content(src)
	}
	return ""
}

func csharpCall(n *sitter.Node, src []byte) (*sitter.Node, string, bool) {
	if n.Type() != "invocation_expression" {
		return nil, "", false
	}
	fn := n.ChildByFieldName("function")
	if fn == nil || fn.Type() != "member_access_expression" {
		return nil, "", false
	}
	obj := fn.ChildByFieldName("expression")
	if obj == nil {
		return nil, "", false
	}
	// A `this` receiver needs no special case: its content matches
	// SelfName, so it resolves against the enclosing type.
	return obj, fieldText(fn, "name", src), true
}

// csharpMemberDeclName spells the authoring member the way the extractor
// names its node: methods, properties and accessor events by identifier,
// field declarators by their own name, constructors as `<Type>.<init>`,
// indexers as `this[]`. Local functions and lambdas are deliberately
// absent — the extractor mints no node for them, so their calls belong to
// the enclosing member, exactly where its stubs already sit.
func csharpMemberDeclName(n *sitter.Node, src []byte) (string, bool) {
	switch n.Type() {
	case "method_declaration", "property_declaration", "event_declaration":
		name := fieldText(n, "name", src)
		return name, name != ""
	case "constructor_declaration":
		if owner := csharpEnclosingTypeName(n, src); owner != "" {
			return owner + ".<init>", true
		}
	case "indexer_declaration":
		return "this[]", true
	case "variable_declarator":
		// Only a FIELD's declarator owns code (its initializer); a local's
		// declarator sits inside a member named above already.
		if p := n.Parent(); p != nil && p.Type() == "variable_declaration" {
			if gp := p.Parent(); gp != nil && gp.Type() == "field_declaration" {
				name := csharpDeclaratorName(n, src)
				return name, name != ""
			}
		}
	}
	return "", false
}

// csharpEnclosingTypeName returns the name of the nearest type
// declaration enclosing n ("" at file scope).
func csharpEnclosingTypeName(n *sitter.Node, src []byte) string {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "class_declaration", "struct_declaration", "record_declaration", "interface_declaration":
			return fieldText(p, "name", src)
		}
	}
	return ""
}
