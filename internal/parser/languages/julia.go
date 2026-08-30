package languages

import (
	"strings"

	juliaforest "github.com/alexaandru/go-sitter-forest/julia"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// JuliaExtractor extracts Julia source through the tree-sitter-julia
// grammar (vendored via alexaandru/go-sitter-forest), replacing the
// original line-regex extractor.
//
// Covered definition forms:
//   - `function f(...) ... end`, including `where`-parametrised
//     signatures, declared return types (`function f(x)::Int`), the
//     empty generic declaration `function f end`, and qualified /
//     operator callees (`function Base.show`, `function Base.:+`)
//   - short-form definitions `f(x) = body`, `f(x)::T = body` and
//     `f(x)::T where T = body`, including nested closures inside `begin`
//     blocks
//   - `macro m(...) ... end`
//   - `struct` / `mutable struct` / `abstract type` / `primitive type`
//     with parametric names (`struct Pair{T,S}`) and supertypes
//     (`<: Living` → EdgeExtends), plus struct fields (KindField),
//     including the `x::T = default` form `Base.@kwdef` requires
//   - `module` / `baremodule` — KindType node whose Meta carries the
//     module's `export` list; definitions, constants and nested modules
//     inside get EdgeMemberOf
//   - `const X = ...` constants (KindVariable)
//
// Node ids stay flat (`<file>::<Name>`, `<file>::<Owner>.<member>`) as in
// every other extractor; the enclosing module rides on
// Meta["scope_mod"], and two definitions that would share an id separate
// through the shared line-suffix helper. A callable named after a type is
// that type's constructor and takes the cross-language `<Type>.<init>`
// spelling.
//
// Imports: `using M`, `using M: a, b`, `import M`, `import M as Alias`,
// dotted and relative import paths (`A.B`, `.Local`, `..Up`), and
// `include("file.jl")` — all as EdgeImports to
// `unresolved::import::<module>`, never to a selected name. A selective
// list also emits one edge per binding to
// `unresolved::import::<module>::<name>`, and a rename rides on
// Edge.Alias (and on the module edge's Meta, which is the persisted
// half). A module alias additionally rewrites qualified callees, so
// `import Foo as F` + `F.process(x)` calls `Foo.process`.
//
// Calls: call_expression / broadcast_call_expression / macrocall
// callees (identifier or qualified field_expression) attribute to the
// enclosing function-like definition as EdgeCalls to
// `unresolved::[Mod.]name`. Unicode identifiers (θ, σ̂), bang names
// (`foo!`), and broadcast (`f.(x)`) are native grammar forms.
//
// Docstrings — a string literal on the line DIRECTLY above a definition,
// which is the adjacency Julia itself requires — attach as Meta["doc"],
// on long and short definitions, types, modules and constants alike.
// The explicit `@doc "text" object` / `Core.@doc "text" object` form
// attaches the same way, with the string taken from inside the macro
// call.
type JuliaExtractor struct {
	lang *sitter.Language
}

func NewJuliaExtractor() *JuliaExtractor {
	return &JuliaExtractor{lang: sitter.NewLanguage(juliaforest.GetLanguage())}
}

func (e *JuliaExtractor) Language() string     { return "julia" }
func (e *JuliaExtractor) Extensions() []string { return []string{".jl"} }

// juliaScope carries the enclosing context down the walk: the innermost
// module (for EdgeMemberOf and export attachment) and the innermost
// function-like definition (for call attribution).
type juliaScope struct {
	moduleID string
	// modulePath is the dotted lexical module path (`Outer.Inner`). Node
	// ids stay flat — see emitCallable — and the path rides on
	// Meta["scope_mod"], the convention the Rust extractor established
	// for exactly this shape.
	modulePath string
	// typeID / typeName name the struct whose body is being walked, so an
	// inner constructor can be attributed to it.
	typeID       string
	typeName     string
	functionID   string
	functionName string
	functionRecv string
	// functionIsCtor marks the enclosing definition as a constructor, so
	// the direct-recursion guard does not eat its delegation calls.
	functionIsCtor bool
}

type juliaWalkState struct {
	filePath string
	fileNode *graph.Node
	result   *parser.ExtractionResult
	seen     map[string]bool
	nodes    map[string]*graph.Node
	// types maps a lexical scope + declared type name to the minted type
	// id, so a definition that shares a type's name is recognised as its
	// constructor and attributed to the RIGHT type when one file declares
	// two same-named types in different modules.
	types map[string]string
	// declaredTypes holds the same keys for every type declared ANYWHERE
	// in the file, filled by the pre-pass. The emitting walk is a single
	// forward pass, and Julia routinely puts outer constructors above the
	// struct they build.
	declaredTypes map[string]bool
	// importAliases maps a module scope + local alias to the module it
	// renames (`import Foo as F` inside `module A` → A/F→Foo), so a
	// qualified call can name the module the resolver can find rather
	// than a file-local nickname. Keyed by scope because `import ... as`
	// binds in the module it appears in, and one file routinely holds
	// several modules giving the same short nickname to different
	// packages.
	importAliases map[string]string
	// modules maps a lexical scope + module name to the minted module
	// id, so `module M … end; function M.f() … end` binds the qualified
	// method to the real module node instead of an invented
	// unresolved::M. Keyed like the type and alias tables — modules are
	// KindType nodes, indistinguishable from structs by kind, so a
	// receiver that is a module needs its own table to resolve through.
	modules map[string]string
}

func (e *JuliaExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "julia",
	}
	result.Nodes = append(result.Nodes, fileNode)

	st := &juliaWalkState{
		filePath:      filePath,
		fileNode:      fileNode,
		result:        result,
		seen:          make(map[string]bool),
		nodes:         map[string]*graph.Node{filePath: fileNode},
		types:         map[string]string{},
		declaredTypes: map[string]bool{},
		importAliases: map[string]string{},
		modules:       map[string]string{},
	}
	juliaPrescan(root, src, "", st)
	e.walk(root, src, juliaScope{}, st)
	return result, nil
}

// walk iterates a node's named children, dispatching definition / import
// / call handlers and recursing with updated scope.
//
// A string literal standing alone before a definition is that
// definition's docstring — but only when it is IMMEDIATELY before it.
// Julia's parser allows exactly one newline between the two, so a blank
// line or an own-line comment detaches the string and leaves the
// definition undocumented (the manual says so twice: "no blank lines or
// comments may intervene"). tree-sitter gives no blank-line signal — the
// adjacent and detached parses are structurally identical — so the line
// numbers are the only discriminator.
func (e *JuliaExtractor) walk(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	e.walkFrom(n, src, scope, st, 0)
}

// walkFrom is walk starting at the given named-child index, so a
// definition can skip its own signature and dispatch only its body.
func (e *JuliaExtractor) walkFrom(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, start int) {
	// Only a few blocks can hold documentation: the file, a module body,
	// and a begin / quote block outside any definition. A string standing
	// in a FUNCTION body is ordinary executable code — Julia parses a
	// function body with its plain expression production, never the
	// docstring one — so a value returned from a helper must not become
	// the doc of whatever is defined after it. The scope check matters
	// for `quote ... end` in particular, which is how nearly every macro
	// body is written.
	docContext := false
	switch n.Type() {
	case "source_file", "module_definition":
		docContext = true
	case "compound_statement", "quote_statement":
		docContext = scope.functionID == ""
	}

	pendingDoc, pendingEnd, commentRows := "", 0, 0
	// docFor returns the pending docstring when it sits directly above c.
	// Julia's lexer allows exactly one newline between a docstring and the
	// object it documents, so a blank line detaches it. Comments are not
	// newlines: a trailing `# note` on the docstring's own line, or a
	// block comment that opens there, keeps the two adjacent — so the rows
	// comments occupy are discounted from the distance.
	docFor := func(c *sitter.Node) string {
		if pendingDoc == "" || int(c.StartPoint().Row)-pendingEnd-commentRows > 1 {
			return ""
		}
		return pendingDoc
	}
	for i, count := start, int(n.NamedChildCount()); i < count; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "string_literal":
			if docContext {
				pendingDoc, pendingEnd, commentRows = juliaDocText(c, src), int(c.EndPoint().Row), 0
				continue
			}

		case "line_comment", "block_comment":
			commentRows += int(c.EndPoint().Row) - int(c.StartPoint().Row)
			continue

		case "module_definition":
			e.handleModule(c, src, scope, st, docFor(c))

		case "struct_definition", "abstract_definition", "primitive_definition":
			e.handleType(c, src, scope, st, docFor(c))

		case "function_definition", "macro_definition":
			e.handleFunction(c, src, scope, st, docFor(c))

		case "const_statement":
			doc := docFor(c)
			for a := range c.NamedChildren() {
				if a.Type() == "assignment" {
					e.handleAssignment(a, src, scope, st, true, doc)
				}
			}

		case "assignment":
			e.handleAssignment(c, src, scope, st, false, docFor(c))

		case "using_statement", "import_statement":
			e.handleImport(c, src, st)

		case "export_statement":
			e.handleExport(c, src, scope, st)

		case "public_statement":
			e.handlePublic(c, src, scope, st)

		case "call_expression", "broadcast_call_expression":
			e.handleCall(c, src, scope, st)
			e.walk(c, src, scope, st)

		case "macrocall_expression":
			e.handleMacroCall(c, src, scope, st)
			e.walkMacroArgs(c, src, scope, st, docFor(c))

		default:
			e.walk(c, src, scope, st)
		}
		pendingDoc, pendingEnd, commentRows = "", 0, 0
	}
}

// walkMacroArgs walks a macro call's arguments, carrying a docstring that
// sat above the macro CALL into the definition it wraps.
// juliaDocMacroArg reports the docstring carried INSIDE an explicit
// `@doc "text" object` / `Core.@doc "text" object` call. Julia lowers
// every docstring — triple-quoted or explicit — through Core.@doc, so
// the string beside the object in the macro call is that object's
// documentation, not an argument of anything. Returns false when the
// call is not a doc form or carries no string.
func juliaDocMacroArg(n *sitter.Node, src []byte) (string, bool) {
	var args *sitter.Node
	isDoc := false
	for c := range n.NamedChildren() {
		switch c.Type() {
		case "macro_identifier":
			for m := range c.NamedChildren() {
				if m.Type() == "identifier" && m.Content(src) == "doc" {
					isDoc = true
				}
			}
		case "field_expression":
			count := int(c.NamedChildCount())
			if count < 2 {
				continue
			}
			// Only the standard-library documentation macros lower a
			// docstring: `Core.@doc` and `Base.@doc` (bare `@doc` is the
			// macro_identifier case above). A user macro that merely ends
			// in `.@doc`, like `Foo.@doc`, is something else and must not
			// hijack the docstring slot.
			base, prop := c.NamedChild(0), c.NamedChild(count-1)
			if base.Type() != "identifier" {
				continue
			}
			if mod := base.Content(src); mod != "Core" && mod != "Base" {
				continue
			}
			for m := range prop.NamedChildren() {
				if m.Type() == "identifier" && m.Content(src) == "doc" {
					isDoc = true
				}
			}
		case "macro_argument_list":
			args = c
		}
	}
	if !isDoc || args == nil {
		return "", false
	}
	for a := range args.NamedChildren() {
		if a.Type() == "string_literal" {
			return juliaDocText(a, src), true
		}
		return "", false
	}
	return "", false
}

// `Base.@kwdef struct S ... end` is a documented struct whose docstring
// attaches to the wrapper, so stopping at the macro boundary would leave
// the single most common documented struct form undocumented.
func (e *JuliaExtractor) walkMacroArgs(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, doc string) {
	if inner, ok := juliaDocMacroArg(n, src); ok && doc == "" {
		doc = inner
	}
	for c := range n.NamedChildren() {
		if doc == "" || c.Type() != "macro_argument_list" {
			e.walk(c, src, scope, st)
			continue
		}
		for a := range c.NamedChildren() {
			switch a.Type() {
			case "struct_definition", "abstract_definition", "primitive_definition":
				e.handleType(a, src, scope, st, doc)
			case "function_definition", "macro_definition":
				e.handleFunction(a, src, scope, st, doc)
			case "module_definition":
				e.handleModule(a, src, scope, st, doc)
			case "assignment":
				// `@doc "text" f(x) = x` documents a short-form
				// definition, which arrives as a plain assignment.
				e.handleAssignment(a, src, scope, st, false, doc)
			case "const_statement":
				// `@doc "text" const X = 1` documents a constant, which
				// arrives wrapped in a const_statement — dispatch its
				// inner assignment AS const, the shape walkFrom gives a
				// top-level constant, so neither the constant nor its doc
				// is dropped.
				for inner := range a.NamedChildren() {
					if inner.Type() == "assignment" {
						e.handleAssignment(inner, src, scope, st, true, doc)
					}
				}
			default:
				// Walk a macro argument the way the generic walker
				// would, dispatching the argument's own kind before
				// its children: walk() visits a node's children but
				// never the node itself, so a call_expression argument
				// was never shown to handleCall. `@everywhere
				// include("f.jl")` under a docstring is the shape that
				// loses its edge — call edges otherwise need an
				// enclosing function, which a documented (module- or
				// file-level) macro call never has.
				switch a.Type() {
				case "call_expression", "broadcast_call_expression":
					e.handleCall(a, src, scope, st)
				case "macrocall_expression":
					e.handleMacroCall(a, src, scope, st)
				}
				e.walk(a, src, scope, st)
				continue
			}
			doc = ""
		}
	}
}

// handleModule emits the module as a KindType node (legacy mapping;
// graph.KindModule is reserved for ecosystem packages) and walks its
// body with the module scope pushed.
func (e *JuliaExtractor) handleModule(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, doc string) {
	name := ""
	if nn := n.ChildByFieldName("name"); nn != nil {
		name = nn.Content(src)
	}
	inner := scope
	if name != "" {
		line := int(n.StartPoint().Row) + 1
		id, minted := disambiguateID(st.seen, st.filePath+"::"+name, line)
		if minted {
			node := &graph.Node{
				ID: id, Kind: graph.KindType, Name: name,
				FilePath:  st.filePath,
				StartLine: line,
				EndLine:   int(n.EndPoint().Row) + 1,
				Language:  "julia",
			}
			meta := map[string]any{}
			if doc != "" {
				meta["doc"] = doc
			}
			if scope.modulePath != "" {
				meta["scope_mod"] = scope.modulePath
			}
			if len(meta) > 0 {
				node.Meta = meta
			}
			st.result.Nodes = append(st.result.Nodes, node)
			st.nodes[id] = node
			// Record the module by lexical scope so a later qualified
			// method (`function M.f()`) in the same file resolves it as
			// the receiver's owner rather than inventing unresolved::M.
			st.modules[juliaTypeKey(scope.modulePath, name)] = id
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: st.fileNode.ID, To: id, Kind: graph.EdgeDefines,
				FilePath: st.filePath, Line: line,
			})
			// A nested module belongs to its parent just as any other
			// resident does; without the edge, a traversal from Outer
			// stops at functions and types and never reaches Inner.
			if scope.moduleID != "" {
				st.result.Edges = append(st.result.Edges, &graph.Edge{
					From: id, To: scope.moduleID, Kind: graph.EdgeMemberOf,
					FilePath: st.filePath, Line: line,
				})
			}
		}
		inner.moduleID = id
		inner.modulePath = name
		if scope.modulePath != "" {
			inner.modulePath = scope.modulePath + "." + name
		}
		// A module body opens a fresh type scope: a struct declared
		// outside it does not own a constructor declared inside.
		inner.typeID, inner.typeName = "", ""
	}
	e.walk(n, src, inner, st)
}

// juliaTypeKey keys the file's type table by lexical module path, so a
// constructor binds to the type declared in its own module rather than to
// a same-named type in a sibling module of the same file.
func juliaTypeKey(modulePath, name string) string { return modulePath + "\x00" + name }

// lookupType finds the type a definition name would construct, in the
// SAME module only. A Julia submodule does not inherit its parent's
// bindings — each module starts with the implicit `using Base` and
// nothing else — so `Outer.Inner.Thing` is an independent generic
// function, not a constructor for `Outer.Thing`, unless it was imported
// explicitly. Searching outward would fabricate a member_of edge into a
// type the definition has no relationship with.
func (st *juliaWalkState) lookupType(modulePath, name string) (id, declared string, ok bool) {
	if id, hit := st.types[juliaTypeKey(modulePath, name)]; hit {
		return id, name, true
	}
	return "", "", false
}

// juliaTypeHeadInfo decodes a `type_head` child: the declared name
// (inside identifier / parametrized_type_expression / the lhs of the
// `<:` binary_expression) and the supertype text.
//
// Type parameters are deliberately not collected. The house key for them
// is Node.Meta["type_params"], whose readers are language-gated (the Go
// generic-instantiation lookup and the Rust scope index), and whose
// []map[string]string shape falls outside the store's flat meta codec —
// so stamping it here would spill every parametric type node's metadata
// into the JSON fallback to feed nothing. The supertype's own parameters
// are already preserved on the extends edge as Meta["base_path"].
func juliaTypeHeadInfo(head *sitter.Node, src []byte) (name, super string) {
	if head == nil || head.Type() != "type_head" {
		return "", ""
	}
	c := head.NamedChild(0)
	if c == nil {
		return "", ""
	}
	if c.Type() == "binary_expression" {
		// Named children are [lhs, operator, rhs] — the operator is a
		// named node in this grammar, so address by position, not index 1.
		lhs := c.NamedChild(0)
		rhs := c.NamedChild(int(c.NamedChildCount()) - 1)
		if lhs != nil {
			name = juliaTypeHeadName(lhs, src)
		}
		if rhs != nil && rhs.Type() != "operator" {
			super = rhs.Content(src)
		}
		return name, super
	}
	return juliaTypeHeadName(c, src), ""
}

// juliaTypeHeadName extracts the declared name from an identifier or a
// parametrized_type_expression (`Pair{T,S}` → `Pair`).
func juliaTypeHeadName(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "identifier":
		return n.Content(src)
	case "parametrized_type_expression":
		for c := range n.NamedChildren() {
			if c.Type() == "identifier" {
				return c.Content(src)
			}
		}
	}
	return ""
}

// juliaFieldName returns the field a direct struct member declares, or ""
// when the member is not a field. Three shapes reach it:
//
//	x::T        typed_expression
//	x           identifier
//	x::T = 1    assignment — the shape `Base.@kwdef` requires, and the
//	            shape every field of a @kwdef struct therefore has
//
// The assignment case is deliberately narrow: an INNER CONSTRUCTOR is a
// direct struct child and an assignment too (`Guard(v) = new(v)`), but its
// left-hand side is a call, so it falls through and is left to the
// definition handling.
func juliaFieldName(c *sitter.Node, src []byte) string {
	n := c
	if n.Type() == "assignment" {
		n = n.NamedChild(0)
	}
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "typed_expression":
		if id := n.NamedChild(0); id != nil && id.Type() == "identifier" {
			return id.Content(src)
		}
	case "identifier":
		return n.Content(src)
	}
	return ""
}

// handleType covers struct / mutable struct / abstract type /
// primitive type: KindType node, EdgeExtends for the supertype (bare
// name target + full path in Meta, matching the python extractor
// convention), KindField nodes for struct members, and EdgeMemberOf for
// definitions nested inside.
func (e *JuliaExtractor) handleType(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, doc string) {
	var head *sitter.Node
	for c := range n.NamedChildren() {
		if c.Type() == "type_head" {
			head = c
			break
		}
	}
	name, super := juliaTypeHeadInfo(head, src)
	if name == "" {
		e.walk(n, src, scope, st)
		return
	}

	line := int(n.StartPoint().Row) + 1
	id, minted := disambiguateID(st.seen, st.filePath+"::"+name, line)
	if !minted {
		e.walk(n, src, scope, st)
		return
	}
	node := &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath:  st.filePath,
		StartLine: line,
		EndLine:   int(n.EndPoint().Row) + 1,
		Language:  "julia",
	}
	meta := map[string]any{}
	if doc != "" {
		meta["doc"] = doc
	}
	if scope.modulePath != "" {
		meta["scope_mod"] = scope.modulePath
	}
	if len(meta) > 0 {
		node.Meta = meta
	}
	st.result.Nodes = append(st.result.Nodes, node)
	st.nodes[id] = node
	st.types[juliaTypeKey(scope.modulePath, name)] = id
	st.result.Edges = append(st.result.Edges, &graph.Edge{
		From: st.fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: st.filePath, Line: line,
	})
	if super != "" {
		bare := super
		if idx := strings.IndexAny(bare, "{"); idx > 0 {
			bare = bare[:idx]
		}
		edge := &graph.Edge{
			From: id, To: "unresolved::" + bare, Kind: graph.EdgeExtends,
			FilePath: st.filePath, Line: line,
		}
		if super != bare {
			edge.Meta = map[string]any{"base_path": super}
		}
		st.result.Edges = append(st.result.Edges, edge)
	}
	if scope.moduleID != "" {
		st.result.Edges = append(st.result.Edges, &graph.Edge{
			From: id, To: scope.moduleID, Kind: graph.EdgeMemberOf,
			FilePath: st.filePath, Line: line,
		})
	}

	// Struct fields at the top level of the struct body. This runs only for a
	// struct THIS call minted, so a struct whose name collides with an
	// earlier declaration cannot hang its fields off that other node.
	if n.Type() == "struct_definition" {
		for c := range n.NamedChildren() {
			if c.Type() == "type_head" {
				continue
			}
			fieldName := juliaFieldName(c, src)
			if fieldName == "" {
				continue
			}
			fieldLine := int(c.StartPoint().Row) + 1
			fid, fieldMinted := disambiguateID(st.seen, id+"."+fieldName, fieldLine)
			if !fieldMinted {
				continue
			}
			st.result.Nodes = append(st.result.Nodes, &graph.Node{
				ID: fid, Kind: graph.KindField, Name: fieldName,
				FilePath:  st.filePath,
				StartLine: fieldLine,
				EndLine:   int(c.EndPoint().Row) + 1,
				Language:  "julia",
			})
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: fid, To: id, Kind: graph.EdgeMemberOf,
				FilePath: st.filePath, Line: fieldLine,
			})
		}
	}

	inner := scope
	inner.typeID, inner.typeName = id, name
	e.walk(n, src, inner, st)
}

// juliaUnwrappedName decodes a single possibly-wrapped name down to its
// plain spelling. An operator callee can wear two wrappers: `:+` is a
// quote_expression around the operator and `:(==)` a quote_expression
// around a parenthesized_expression around it, while a bare `(==)` callee
// wears only the parenthesized one. Returns "" when the node is not a
// name in any of these shapes.
func juliaUnwrappedName(n *sitter.Node, src []byte) string {
	for n != nil {
		switch n.Type() {
		case "identifier", "operator":
			return n.Content(src)
		case "quote_expression", "parenthesized_expression":
			n = n.NamedChild(0)
		default:
			return ""
		}
	}
	return ""
}

// juliaCalleeName decodes a call callee: bare identifiers, qualified
// field_expressions (`Base.show`, `A.B.c`), quoted operators (`:+`,
// `:(==)`), and parametrized constructors (`Vector{Int}`). The
// field_expression is decoded from its base and property CHILDREN, never
// from source text: text split on the last dot dragged a chained
// callee's arguments (`get(cfg).run`) and even a line break inside a
// multi-line chain into the call target. A base that is not itself a
// dotted name leaves only the property decodable, so the callee degrades
// to its bare method name — the only part a resolver could ever match.
// Returns name, receiver.
func juliaCalleeName(n *sitter.Node, src []byte) (name, receiver string) {
	if n == nil {
		return "", ""
	}
	switch n.Type() {
	case "identifier", "operator":
		return n.Content(src), ""
	case "quote_expression", "parenthesized_expression": // bare `:+` / `(==)` callee
		return juliaUnwrappedName(n, src), ""
	case "parametrized_type_expression": // `Vector{Int}(xs)`
		return juliaParametrizedCallee(n, src)
	case "field_expression":
		count := int(n.NamedChildCount())
		if count < 2 {
			return "", ""
		}
		name = juliaUnwrappedName(n.NamedChild(count-1), src)
		if name == "" {
			return "", ""
		}
		switch base := n.NamedChild(0); base.Type() {
		case "identifier":
			return name, base.Content(src)
		case "field_expression":
			inner, recv := juliaCalleeName(base, src)
			if inner == "" {
				return "", ""
			}
			if recv == "" {
				return name, inner
			}
			return name, recv + "." + inner
		default:
			// `get(cfg).run(x)` — the base is a call, not a name, so
			// only the method name is decodable.
			return name, ""
		}
	}
	return "", ""
}

// juliaParametrizedCallee decodes a `Vector{Int}(xs)`-style constructor
// callee: the head name — possibly qualified, as `Base.Vector{Int}` —
// followed by the literal type parameters, which are part of the
// constructor's name the way Julia prints it. The curly list is rebuilt
// from its children so a parameter list broken across lines cannot leak
// a newline into the target.
func juliaParametrizedCallee(n *sitter.Node, src []byte) (name, receiver string) {
	head := n.NamedChild(0)
	if head == nil {
		return "", ""
	}
	switch head.Type() {
	case "identifier":
		name = head.Content(src)
	case "field_expression":
		inner, recv := juliaCalleeName(head, src)
		if inner == "" {
			return "", ""
		}
		name, receiver = inner, recv
	default:
		return "", ""
	}
	var params []string
	for i, count := 1, int(n.NamedChildCount()); i < count; i++ {
		if c := n.NamedChild(i); c != nil && c.Type() == "curly_expression" {
			for j, jcount := 0, int(c.NamedChildCount()); j < jcount; j++ {
				if p := c.NamedChild(j); p != nil {
					// Canonicalise nested type parameters so a nested `{…}`
					// split across lines cannot leak a newline into the
					// target.
					params = append(params, juliaCanonType(p, src))
				}
			}
		}
	}
	if len(params) > 0 {
		name += "{" + strings.Join(params, ",") + "}"
	}
	return name, receiver
}

// juliaMacroReceiver decodes the receiver of a module-qualified macro
// call (`Base.@time`, `Base.Threads.@threads`) into its dotted spelling,
// from children rather than source text so a chain of any depth keeps its
// full qualification. Returns "" when the receiver is not a name.
func juliaMacroReceiver(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "identifier":
		return n.Content(src)
	case "field_expression":
		name, recv := juliaCalleeName(n, src)
		if name == "" {
			return ""
		}
		if recv == "" {
			return name
		}
		return recv + "." + name
	}
	return ""
}

// juliaCanonType renders a type expression to a single-line canonical
// spelling, rebuilding any nested `{…}` parameter list from its children
// so source formatting — inner spaces, or a parameter list split across
// lines — cannot leak into a constructor-callee target.
func juliaCanonType(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "parametrized_type_expression":
		head := juliaCanonType(n.NamedChild(0), src)
		var params []string
		for i, count := 1, int(n.NamedChildCount()); i < count; i++ {
			c := n.NamedChild(i)
			if c == nil || c.Type() != "curly_expression" {
				continue
			}
			for j, jcount := 0, int(c.NamedChildCount()); j < jcount; j++ {
				if p := c.NamedChild(j); p != nil {
					params = append(params, juliaCanonType(p, src))
				}
			}
		}
		if len(params) > 0 {
			return head + "{" + strings.Join(params, ",") + "}"
		}
		return head
	case "field_expression":
		name, recv := juliaCalleeName(n, src)
		if name == "" {
			return strings.Join(strings.Fields(n.Content(src)), "")
		}
		if recv == "" {
			return name
		}
		return recv + "." + name
	default:
		// identifier, operator, a literal type parameter — collapse any
		// internal whitespace so a line break cannot survive.
		return strings.Join(strings.Fields(n.Content(src)), "")
	}
}

// juliaSignatureCall peels the wrappers a definition head can carry until
// it reaches the call_expression that names the definition. Three wrappers
// occur, and they nest in either order:
//
//	signature          the long form's head, `function f(x) ... end`
//	where_expression   `f(x) where T`          → where(call)
//	typed_expression   `f(x)::Int`             → typed(call)
//	                   `f(x)::T where T`       → where(typed(call))
//
// so the peel is a loop rather than a fixed descent. Anything else stops
// it: an assignment whose left-hand side is `x::Int` peels the
// typed_expression and finds a bare identifier, which is a typed variable,
// not a definition.
func juliaSignatureCall(n *sitter.Node) *sitter.Node {
	for n != nil {
		switch n.Type() {
		case "call_expression":
			return n
		case "signature", "where_expression", "typed_expression":
			n = n.NamedChild(0)
		default:
			return nil
		}
	}
	return nil
}

// handleFunction covers `function f(...) end` and `macro m(...) end`.
// The grammar has no named fields here: the first named child is the
// `signature` wrapping the callee call_expression (optionally inside a
// where_expression). Qualified callees become KindMethod with
// Meta["receiver"] + EdgeMemberOf, mirroring the luau extractor.
func (e *JuliaExtractor) handleFunction(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, doc string) {
	head := n.NamedChild(0)
	name, receiver := "", ""
	if call := juliaSignatureCall(head); call != nil {
		name, receiver = juliaCalleeName(call.NamedChild(0), src)
	} else if head != nil && head.Type() == "signature" {
		// `function f end` declares an empty generic function: there is
		// no argument list, so the signature holds the callee directly.
		name, receiver = juliaCalleeName(head.NamedChild(0), src)
	}

	inner := scope
	if name != "" {
		inner = e.emitCallable(n, name, receiver, doc, n.Type() == "macro_definition", scope, st)
	}
	e.walkDefinitionBody(n, juliaSignatureCall(head), src, inner, st)
}

// walkDefinitionBody dispatches a definition's body, treating its own
// signature as a declaration rather than a call site. The signature IS a
// call_expression in this grammar, so walking the definition whole made
// every definition appear to call itself — masked until now by the
// direct-recursion guard, which a constructor no longer applies. Its
// children are still walked, so a call inside a default argument value
// is not lost with it.
func (e *JuliaExtractor) walkDefinitionBody(n, sig *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	if sig != nil {
		e.walk(sig, src, scope, st)
	}
	e.walkFrom(n, src, scope, st, 1)
}

// handleAssignment: `f(x) = body` (and `f(x) where T = body`) are
// short-form function definitions — the LHS is a call_expression,
// directly or under a where_expression. `const X = ...` arrives with
// isConst and a plain identifier LHS.
func (e *JuliaExtractor) handleAssignment(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, isConst bool, doc string) {
	lhs := n.NamedChild(0)

	// Short-form definition? The left-hand side carries the same
	// wrappers a long-form signature does — `f(x) where T`,
	// `f(x)::Int`, `f(x)::T where T` — so peel with the shared helper
	// instead of unwrapping one fixed level.
	if sig := juliaSignatureCall(lhs); sig != nil {
		if name, receiver := juliaCalleeName(sig.NamedChild(0), src); name != "" {
			e.emitShortFunction(n, sig, name, receiver, doc, src, scope, st)
			return
		}
	}

	if isConst && lhs != nil && lhs.Type() == "identifier" {
		name := lhs.Content(src)
		line := int(n.StartPoint().Row) + 1
		if id, minted := disambiguateID(st.seen, st.filePath+"::"+name, line); minted {
			node := &graph.Node{
				ID: id, Kind: graph.KindVariable, Name: name,
				FilePath:  st.filePath,
				StartLine: line,
				EndLine:   int(n.EndPoint().Row) + 1,
				Language:  "julia",
			}
			meta := map[string]any{}
			if doc != "" {
				meta["doc"] = doc
			}
			if scope.modulePath != "" {
				meta["scope_mod"] = scope.modulePath
			}
			if len(meta) > 0 {
				node.Meta = meta
			}
			st.result.Nodes = append(st.result.Nodes, node)
			st.nodes[id] = node
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: st.fileNode.ID, To: id, Kind: graph.EdgeDefines,
				FilePath: st.filePath, Line: line,
			})
			// A constant inside a module belongs to it the same way a
			// function does — the edge is what a traversal from the
			// module reaches; scope_mod on Meta is not traversable.
			if scope.moduleID != "" {
				st.result.Edges = append(st.result.Edges, &graph.Edge{
					From: id, To: scope.moduleID, Kind: graph.EdgeMemberOf,
					FilePath: st.filePath, Line: line,
				})
			}
		}
	}

	e.walk(n, src, scope, st)
}

func (e *JuliaExtractor) emitShortFunction(n, sig *sitter.Node, name, receiver, doc string, src []byte, scope juliaScope, st *juliaWalkState) {
	inner := e.emitCallable(n, name, receiver, doc, false, scope, st)
	e.walkDefinitionBody(n, sig, src, inner, st)
}

// emitCallable mints one function / method / constructor node and returns
// the scope its body should walk under.
//
// Node ids stay FLAT — `<file>::<Name>`, `<file>::<Owner>.<member>` — as
// every other extractor mints them; the enclosing module rides on
// Meta["scope_mod"] instead (the Rust `mod` precedent). Folding the module
// into the id would break graph.EnclosingFromID, which recovers a
// method's or field's owner by cutting at the LAST dot. Two definitions
// that would collide on one id — `f` in module A and `f` in module B —
// separate through the shared line-suffix helper, so both survive as
// navigable nodes instead of one silently swallowing the other's call
// edges.
//
// A callable whose name is a type's name is that type's CONSTRUCTOR:
// Julia has no separate keyword, so `Box(x) = …` beside `struct Box` and
// `function Box() … end` inside it are both constructors. They take the
// cross-language `<Type>.<init>` spelling that Java, C# and Swift already
// emit, so constructor-aware consumers need no Julia special case — and,
// critically, they no longer collide with the type node itself, which
// used to swallow the constructor and adopt its body's call edges.
func (e *JuliaExtractor) emitCallable(
	n *sitter.Node, name, receiver, doc string, isMacro bool,
	scope juliaScope, st *juliaWalkState,
) juliaScope {
	line := int(n.StartPoint().Row) + 1
	kind := graph.KindMethod
	nodeName := name
	isCtor := false
	var baseID, ownerID, ownerTarget, ownerName string

	switch {
	case receiver != "":
		ownerName = receiver
		if id, _, ok := st.lookupType(scope.modulePath, receiver); ok {
			ownerID, ownerTarget = id, id
		} else if id, ok := st.modules[juliaTypeKey(scope.modulePath, receiver)]; ok {
			// `module M … end; function M.f() … end` — the receiver names
			// a module this file declares, so member_of reaches the real
			// module node instead of an invented unresolved::M.
			ownerID, ownerTarget = id, id
		} else {
			// `function Base.show` extends a module this file does not
			// declare, so no node carries the receiver's name — a
			// member_of to the node-shaped id would claim a resident
			// of the graph that does not exist. The method's own id
			// stays flat; the edge target becomes self-describing, the
			// same honesty extends and call edges already carry.
			ownerID = st.filePath + "::" + receiver
			ownerTarget = "unresolved::" + receiver
		}
		baseID = ownerID + "." + name

	default:
		typeID, typeName := "", ""
		// A macro name lives in a disjoint namespace — `macro Tag`
		// defines `@Tag`, which has nothing to do with a type `Tag` — so
		// a macro can never be a constructor.
		switch {
		case isMacro:
		case scope.typeName == name && scope.typeID != "":
			typeID, typeName = scope.typeID, scope.typeName // inner constructor
		default:
			if id, declared, ok := st.lookupType(scope.modulePath, name); ok {
				typeID, typeName = id, declared // outer constructor
			} else if st.declaredTypes[juliaTypeKey(scope.modulePath, name)] &&
				!st.seen[st.filePath+"::"+name] {
				// The struct is declared further down the file. Predict
				// the id it will take rather than taking it here — but
				// only while that id is still free, so a same-named type
				// in another module cannot be adopted by mistake.
				typeID, typeName = st.filePath+"::"+name, name
			}
		}
		if typeID != "" {
			baseID = typeID + ".<init>"
			ownerID, ownerTarget, ownerName = typeID, typeID, typeName
			nodeName = typeName + ".<init>"
			isCtor = true
		} else {
			kind = graph.KindFunction
			baseID = st.filePath + "::" + name
		}
	}

	id, minted := disambiguateID(st.seen, baseID, line)
	if !minted {
		// Same base id AND same start line. Two shapes reach here: the
		// same declaration walked twice, and two `;`-separated methods
		// written on one physical line, which a line suffix cannot
		// separate. Neither gets a second node, but the body still has
		// to attribute its calls somewhere — dropping them loses edges
		// the line-suffix fix was never meant to touch. Definitions on
		// DIFFERENT lines no longer land here at all, which is where
		// the re-parenting this guard used to cause actually mattered.
		inner := scope
		inner.functionID = id
		inner.functionName = name
		inner.functionRecv = receiver
		inner.functionIsCtor = isCtor
		return inner
	}

	node := &graph.Node{
		ID: id, Kind: kind, Name: nodeName,
		FilePath:  st.filePath,
		StartLine: line,
		EndLine:   int(n.EndPoint().Row) + 1,
		Language:  "julia",
	}
	meta := map[string]any{}
	if ownerName != "" {
		meta["receiver"] = ownerName
	}
	if isMacro {
		meta["macro"] = true
	}
	if doc != "" {
		meta["doc"] = doc
	}
	if scope.modulePath != "" {
		meta["scope_mod"] = scope.modulePath
	}
	if len(meta) > 0 {
		node.Meta = meta
	}
	st.result.Nodes = append(st.result.Nodes, node)
	st.nodes[id] = node
	st.result.Edges = append(st.result.Edges, &graph.Edge{
		From: st.fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: st.filePath, Line: line,
	})
	if ownerID != "" {
		st.result.Edges = append(st.result.Edges, &graph.Edge{
			From: id, To: ownerTarget, Kind: graph.EdgeMemberOf,
			FilePath: st.filePath, Line: line,
		})
	}
	if scope.moduleID != "" {
		st.result.Edges = append(st.result.Edges, &graph.Edge{
			From: id, To: scope.moduleID, Kind: graph.EdgeMemberOf,
			FilePath: st.filePath, Line: line,
		})
	}

	inner := scope
	inner.functionID = id
	inner.functionName = name
	inner.functionRecv = receiver
	inner.functionIsCtor = isCtor
	return inner
}

// juliaImportBindingCap bounds how many per-binding import edges one
// statement may contribute, mirroring jsImportBindingCap. Above it the
// module edge alone records the dependency: the selected names stay in
// that edge's Meta, so nothing is lost that a reader cannot recover.
const juliaImportBindingCap = 64

// juliaImportHead returns what a `selected_import` / `import_alias` names
// first, plus the index of the child after it. For a statement-level node
// that is the module: a dotted or relative path (`A.B`, `.Local`, `..Up`)
// is an `import_path`, a single-segment module a bare `identifier`.
// Inside a selected list the same position holds the RENAMED BINDING, so
// operators and macros are accepted there too — `import Base: + as plus`
// and `import Base: @time as t` both put a non-identifier in it.
// Everything after is the selection or the alias.
func juliaImportHead(n *sitter.Node, src []byte) (head string, next int) {
	first := n.NamedChild(0)
	if first == nil {
		return "", 0
	}
	switch first.Type() {
	case "import_path", "identifier", "operator", "macro_identifier":
		return first.Content(src), 1
	}
	return "", 0
}

// juliaAliasParts decodes an `import_alias` — `Foo as F`, `C.D as CD` at
// statement level, `bar as baz` inside a selected list — into the
// upstream name and the local alias.
func juliaAliasParts(n *sitter.Node, src []byte) (orig, alias string) {
	orig, next := juliaImportHead(n, src)
	if orig == "" {
		return "", ""
	}
	for i, count := next, int(n.NamedChildCount()); i < count; i++ {
		s := n.NamedChild(i)
		if s == nil {
			continue
		}
		switch s.Type() {
		case "identifier", "operator", "macro_identifier":
			alias = s.Content(src)
		}
	}
	return orig, alias
}

// juliaPrescan walks the file once before extraction, collecting the two
// facts the emitting pass needs BEFORE it reaches the declarations that
// establish them — it is a single forward pass, and Julia orders neither.
//
// Module renames (`import M as A`) are keyed by the module the statement
// appears in, so handleCall can rewrite a qualified callee onto the module
// it actually names. Only MODULE aliases are collected: a renamed binding
// inside a selected list (`import Foo: bar as baz`) renames a function,
// and rewriting a bare call through it would fight any local shadowing the
// extractor cannot see.
//
// Declared type names are keyed the same way, because a callable named
// after a type is that type's constructor and outer constructors are
// routinely written ABOVE the struct they build. Without knowing that up
// front, such a constructor was minted as a plain function on the type's
// own canonical id, and the struct that followed was demoted to a
// line-suffixed one.
func juliaPrescan(n *sitter.Node, src []byte, modulePath string, st *juliaWalkState) {
	switch n.Type() {
	case "using_statement", "import_statement":
		for c := range n.NamedChildren() {
			if c.Type() != "import_alias" {
				continue
			}
			if module, alias := juliaAliasParts(c, src); module != "" && alias != "" && alias != module {
				st.importAliases[juliaTypeKey(modulePath, alias)] = module
			}
		}
		return
	case "struct_definition", "abstract_definition", "primitive_definition":
		for c := range n.NamedChildren() {
			if c.Type() != "type_head" {
				continue
			}
			if name, _ := juliaTypeHeadInfo(c, src); name != "" {
				st.declaredTypes[juliaTypeKey(modulePath, name)] = true
			}
			break
		}
	case "module_definition":
		if nn := n.ChildByFieldName("name"); nn != nil && nn.Content(src) != "" {
			if modulePath == "" {
				modulePath = nn.Content(src)
			} else {
				modulePath += "." + nn.Content(src)
			}
		}
	}
	for c := range n.NamedChildren() {
		juliaPrescan(c, src, modulePath, st)
	}
}

// handleImport covers `using M`, `using M: a, b`, `import M`,
// `import M as Alias`, and dotted / relative import paths. The import
// target is always the MODULE; a selective list additionally emits one
// binding-aware edge per selected name (the JS/TS per-binding
// convention), so "who imports `mean` from Statistics" is a traversable
// question rather than a Meta key nothing reads.
func (e *JuliaExtractor) handleImport(n *sitter.Node, src []byte, st *juliaWalkState) {
	line := int(n.StartPoint().Row) + 1
	emit := func(target, alias string, meta map[string]any) {
		if target == "" {
			return
		}
		if len(meta) == 0 {
			meta = nil
		}
		st.result.Edges = append(st.result.Edges, &graph.Edge{
			From: st.fileNode.ID, To: "unresolved::import::" + target,
			Kind:     graph.EdgeImports,
			FilePath: st.filePath, Line: line,
			// Edge.Alias is the graph's canonical spelling for a renamed
			// binding; Meta keeps the same fact on the durable side,
			// since the SQLite edges table has no alias column.
			Alias: alias,
			Meta:  meta,
		})
	}
	// One edge per selected binding, targeting the binding rather than
	// the module — the representation JS/TS already emits for
	// `import { a, b as c } from "mod"`. A rename rides on Edge.Alias
	// and, because the SQLite edges table has no alias column, on Meta
	// as well, which is the half that survives the round-trip.
	emitBinding := func(module, orig, alias string) {
		if module == "" || orig == "" {
			return
		}
		edge := &graph.Edge{
			From: st.fileNode.ID, To: "unresolved::import::" + module + "::" + orig,
			Kind:     graph.EdgeImports,
			FilePath: st.filePath, Line: line,
			Alias: alias,
		}
		if alias != "" {
			edge.Meta = map[string]any{"alias": alias}
		}
		st.result.Edges = append(st.result.Edges, edge)
	}
	for c := range n.NamedChildren() {
		switch c.Type() {
		case "identifier", "import_path":
			emit(c.Content(src), "", nil)
		case "selected_import":
			// `using A.B: x, y` — the module is the FIRST child and is
			// an import_path whenever the path is dotted or relative.
			// Scanning for identifiers instead skipped it and promoted
			// the first selected name to the import target.
			module, next := juliaImportHead(c, src)
			var names []string
			type binding struct{ orig, alias string }
			var bindings []binding
			for i, count := next, int(c.NamedChildCount()); i < count; i++ {
				s := c.NamedChild(i)
				if s == nil {
					continue
				}
				switch s.Type() {
				case "identifier", "operator", "macro_identifier":
					// `using Base: +, -` selects operators and
					// `using Base: @time` a macro; neither is an
					// identifier node.
					names = append(names, s.Content(src))
					bindings = append(bindings, binding{orig: s.Content(src)})
				case "import_alias":
					// `import Foo: bar as baz` renames one binding.
					orig, alias := juliaAliasParts(s, src)
					if orig == "" {
						continue
					}
					names = append(names, orig)
					if alias == orig {
						alias = ""
					}
					bindings = append(bindings, binding{orig: orig, alias: alias})
				}
			}
			meta := map[string]any{}
			if len(names) > 0 {
				meta["names"] = names
			}
			emit(module, "", meta)
			// Past the cap only the module edge is kept, so one
			// statement cannot dwarf a file's graph — the same bound the
			// JS/TS per-binding helper applies for the same reason.
			if len(bindings) <= juliaImportBindingCap {
				for _, b := range bindings {
					emitBinding(module, b.orig, b.alias)
				}
			}
		case "import_alias":
			path, alias := juliaAliasParts(c, src)
			if alias == path {
				alias = ""
			}
			meta := map[string]any{}
			if alias != "" {
				meta["alias"] = alias
			}
			emit(path, alias, meta)
		}
	}
}

// handleExport records the module's public surface on the enclosing
// module node's Meta (Julia export lists are only meaningful inside
// modules).
func (e *JuliaExtractor) handleExport(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	e.recordModuleNames(n, src, scope, st, "exports")
}

// handlePublic records a Julia 1.11 `public` list the same way. Public
// names are visible API WITHOUT being re-exported, so they ride on their
// own Meta key next to the export list.
func (e *JuliaExtractor) handlePublic(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	e.recordModuleNames(n, src, scope, st, "public")
}

// recordModuleNames collects the names named by an export or public
// statement onto the enclosing module node's Meta under key.
func (e *JuliaExtractor) recordModuleNames(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, key string) {
	if scope.moduleID == "" {
		return
	}
	node, ok := st.nodes[scope.moduleID]
	if !ok {
		return
	}
	names := []string{}
	for c := range n.NamedChildren() {
		// `export apply, @m, ⊗` exports a function, a macro and an
		// operator: a macro name is a macro_identifier node and an
		// operator an operator node — the same distinction import
		// lists already decode (`using Base: @time, +`). Record them
		// verbatim (@m, ⊗) so the module's public surface keeps its
		// macro and operator names.
		switch c.Type() {
		case "identifier", "operator", "macro_identifier":
			names = append(names, c.Content(src))
		}
	}
	if len(names) == 0 {
		return
	}
	if node.Meta == nil {
		node.Meta = map[string]any{}
	}
	prev, _ := node.Meta[key].([]string)
	node.Meta[key] = append(prev, names...)
}

// handleCall emits EdgeCalls from the enclosing function to the callee
// (bare or qualified). `include("f.jl")` becomes an import edge instead,
// preserving the legacy extractor's contract.
func (e *JuliaExtractor) handleCall(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	callee := n.NamedChild(0)
	name, receiver := juliaCalleeName(callee, src)
	if name == "" {
		return
	}
	if name == "include" && receiver == "" {
		if args := n.NamedChild(1); args != nil && args.Type() == "argument_list" {
			if lit := args.NamedChild(0); lit != nil && lit.Type() == "string_literal" {
				st.result.Edges = append(st.result.Edges, &graph.Edge{
					From:     st.fileNode.ID,
					To:       "unresolved::import::" + juliaUnquote(lit.Content(src)),
					Kind:     graph.EdgeImports,
					FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
				})
			}
		}
		return
	}
	if scope.functionID == "" {
		return
	}
	// Direct recursion carries no information, so it is dropped — except
	// from a constructor, where a call to the type's own name is
	// delegation to a DIFFERENT method (`Box() = Box(0)`), which is the
	// single most idiomatic constructor body Julia has. A constructor
	// that genuinely recursed into itself would not terminate.
	if !scope.functionIsCtor && scope.functionName == name && scope.functionRecv == receiver {
		return
	}
	target := name
	if receiver != "" {
		// `import Foo as F` then `F.process(x)`: name the module, not
		// the file-local nickname, so the call target is something
		// another file's `module Foo` can be matched against. The alias
		// is looked up in the module that declared it — a sibling module
		// binding the same nickname to a different package must not
		// rewrite this call.
		if module, ok := st.importAliases[juliaTypeKey(scope.modulePath, receiver)]; ok {
			receiver = module
		}
		target = receiver + "." + name
	}
	meta := map[string]any{}
	if n.Type() == "broadcast_call_expression" {
		meta["broadcast"] = true
	}
	edge := &graph.Edge{
		From: scope.functionID, To: "unresolved::" + target,
		Kind:     graph.EdgeCalls,
		FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
	}
	if len(meta) > 0 {
		edge.Meta = meta
	}
	st.result.Edges = append(st.result.Edges, edge)
}

// handleMacroCall attributes `@macroname ...` and `Mod.@macroname ...`
// invocations to the enclosing function as EdgeCalls with macro:true
// meta. The macro's name lives in a disjoint namespace from types and
// functions, so the target carries the bare name (time, not @time) with
// the module as receiver — the same receiver.name spelling qualified
// call callees use.
func (e *JuliaExtractor) handleMacroCall(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	if scope.functionID == "" {
		return
	}
	emit := func(target string) {
		st.result.Edges = append(st.result.Edges, &graph.Edge{
			From: scope.functionID, To: "unresolved::" + target,
			Kind:     graph.EdgeCalls,
			Meta:     map[string]any{"macro": true},
			FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
		})
	}
	for c := range n.NamedChildren() {
		switch c.Type() {
		case "macro_identifier": // `@time x`
			for m := range c.NamedChildren() {
				if m.Type() == "identifier" {
					emit(m.Content(src))
				}
			}
		case "field_expression": // `Base.@time x`, `Base.Threads.@threads x`
			count := int(c.NamedChildCount())
			if count < 2 {
				continue
			}
			prop, base := c.NamedChild(count-1), c.NamedChild(0)
			if prop.Type() != "macro_identifier" {
				continue
			}
			// The receiver can be a bare module (`Base`) or a dotted
			// chain (`Base.Threads`), decoded from its children to any
			// depth so a multi-segment qualifier keeps its module instead
			// of losing the edge.
			receiver := juliaMacroReceiver(base, src)
			if receiver == "" {
				continue
			}
			name := ""
			for m := range prop.NamedChildren() {
				if m.Type() == "identifier" {
					name = m.Content(src)
				}
			}
			if name == "" {
				continue
			}
			// `import Foo as F` then `F.@spawn ...`: name the module, not
			// the file-local nickname, exactly as a qualified call callee
			// does. An alias binds a single name, so only the leading
			// segment of a dotted receiver can be one.
			head, rest, dotted := strings.Cut(receiver, ".")
			if module, ok := st.importAliases[juliaTypeKey(scope.modulePath, head)]; ok {
				head = module
			}
			if dotted {
				receiver = head + "." + rest
			} else {
				receiver = head
			}
			emit(receiver + "." + name)
		}
	}
}

// juliaDocText normalises a docstring literal down to its first prose
// paragraph. Julia's own documentation convention opens a docstring with
// the signature, indented by four spaces, then a blank line, then the
// description — so taking the first paragraph blindly stores "radius(c)"
// as the documentation of `radius`. Leading paragraphs that are indented
// RELATIVE TO THE BODY are that signature block and are skipped.
//
// The relative part is load-bearing: a docstring written inside an
// indented module or `begin` body has every line indented, so an absolute
// test would call every paragraph a signature block and eat the whole
// docstring. Julia's own Docs machinery dedents the literal before
// anything else; so does this.
func juliaDocText(n *sitter.Node, src []byte) string {
	// A Windows-authored file separates paragraphs with "\r\n\r\n",
	// which holds no "\n\n" — without normalising first, the signature
	// block below would never be recognised and would be stored as the
	// documentation.
	s := strings.ReplaceAll(n.Content(src), "\r\n", "\n")
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, `"""`) {
		s = strings.TrimSuffix(strings.TrimPrefix(s, `"""`), `"""`)
	} else {
		s = strings.TrimSuffix(strings.TrimPrefix(s, `"`), `"`)
	}
	s = juliaDedent(s)
	for {
		s = strings.TrimLeft(s, "\n")
		para, rest, found := strings.Cut(s, "\n\n")
		// Never skip the last paragraph: a docstring that is nothing but
		// a signature still documents better than an empty string.
		if !found || strings.TrimSpace(rest) == "" || !juliaIndentedParagraph(para) {
			return strings.TrimSpace(para)
		}
		s = rest
	}
}

// juliaDedent strips the whitespace prefix every non-empty line shares,
// the way Julia's documentation machinery does before storing a
// docstring. The literal's first line has already lost its indentation to
// the opening quotes, so it is excluded from the common-prefix scan.
func juliaDedent(s string) string {
	lines := strings.Split(s, "\n")
	prefix, seen := "", false
	for _, line := range lines[min(1, len(lines)):] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if !seen {
			prefix, seen = indent, true
			continue
		}
		prefix = juliaCommonPrefix(prefix, indent)
		if prefix == "" {
			return s
		}
	}
	if !seen || prefix == "" {
		return s
	}
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, prefix)
	}
	return strings.Join(lines, "\n")
}

func juliaCommonPrefix(a, b string) string {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[:i]
		}
	}
	return a[:n]
}

// juliaIndentedParagraph reports whether every non-empty line of a
// paragraph is indented — the shape of the signature block a Julia
// docstring conventionally opens with, once the docstring's own common
// indent has been removed.
func juliaIndentedParagraph(p string) bool {
	indented := false
	for _, line := range strings.Split(p, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			return false
		}
		indented = true
	}
	return indented
}

func juliaUnquote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "\"\"\"")
	s = strings.TrimSuffix(s, "\"\"\"")
	s = strings.TrimPrefix(s, "\"")
	s = strings.TrimSuffix(s, "\"")
	return strings.TrimSpace(s)
}

var _ parser.Extractor = (*JuliaExtractor)(nil)
