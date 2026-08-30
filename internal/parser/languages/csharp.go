package languages

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/csharp"
)

// csharpInterfaceNamePattern encodes the C# `I`-prefix convention
// (IService, IRepository, IList): an interface name conventionally
// starts with a capital `I` followed by another uppercase letter. The
// base-list heuristic falls back to this when a base type is defined in
// another compilation unit and so cannot be matched against the file's
// own interface declarations.
var csharpInterfaceNamePattern = regexp.MustCompile(`^I[A-Z]`)

// qCSharpAll is a single tree-sitter query alternating over every
// pattern the C# extractor needs. One tree walk per file replaces the
// 13 `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set. Class / struct / interface
// membership for methods, constructors, fields, and properties is
// resolved via a parent walk on the captured node — the legacy nested
// queries duplicated each member pattern across class_declaration and
// struct_declaration; the parent walk collapses them into a single
// pattern per member kind.
const qCSharpAll = `
[
  (namespace_declaration
    name: (_) @ns.name) @ns.def

  (class_declaration
    name: (identifier) @class.name) @class.def

  (interface_declaration
    name: (identifier) @iface.name) @iface.def

  (struct_declaration
    name: (identifier) @struct.name) @struct.def

  (record_declaration
    name: (identifier) @record.name) @record.def

  (enum_declaration
    name: (identifier) @enum.name) @enum.def

  (anonymous_object_creation_expression) @anon.def

  (method_declaration
    name: (identifier) @method.name) @method.def

  (constructor_declaration
    name: (identifier) @ctor.name) @ctor.def

  (field_declaration
    (variable_declaration
      (variable_declarator
        name: (identifier) @field.name))) @field.def

  (property_declaration
    name: (identifier) @prop.name) @prop.def

  (using_directive (_) @using.path) @using.def

  ; An invocation that spells explicit type arguments parses its callee
  ; name as a generic_name, never a bare identifier — so pinning the name
  ; to (identifier) dropped every generic call site outright, with no edge
  ; and no unresolved stub. That is the dominant .NET call shape
  ; (GetRequiredService<T>(), AddSingleton<TI, TImpl>()), and it left
  ; heavily-called methods looking like dead code. Each alternation
  ; captures the inner identifier, so the callee name stays bare.
  (invocation_expression
    function: [
      (identifier) @call.name
      (generic_name (identifier) @call.name)
    ]) @call.expr

  (invocation_expression
    function: (member_access_expression
      expression: (_) @callm.receiver
      name: [
        (identifier) @callm.method
        (generic_name (identifier) @callm.method)
      ])) @callm.expr

  (invocation_expression
    function: (conditional_access_expression
      condition: (_) @callm.receiver
      (member_binding_expression
        name: [
          (identifier) @callm.method
          (generic_name (identifier) @callm.method)
        ]))) @callm.expr

  (invocation_expression
    function: (conditional_access_expression
      "this"
      (member_binding_expression
        name: [
          (identifier) @callself.method
          (generic_name (identifier) @callself.method)
        ]))) @callself.expr

  (invocation_expression
    function: (conditional_access_expression
      "base"
      (member_binding_expression
        name: [
          (identifier) @callbase.method
          (generic_name (identifier) @callbase.method)
        ]))) @callbase.expr

  (invocation_expression
    function: (member_access_expression
      "this"
      name: [
        (identifier) @callself.method
        (generic_name (identifier) @callself.method)
      ])) @callself.expr

  (invocation_expression
    function: (member_access_expression
      "base"
      name: [
        (identifier) @callbase.method
        (generic_name (identifier) @callbase.method)
      ])) @callbase.expr

  (local_declaration_statement
    (variable_declaration
      type: (_) @lvar.type
      (variable_declarator
        (identifier) @lvar.name))) @lvar.def

  (assignment_expression
    left: (identifier) @fassign.name) @fassign.expr

  (member_access_expression
    name: [
      (identifier) @maccess.name
      (generic_name (identifier) @maccess.name)
    ]) @maccess.expr

  (conditional_access_expression
    (member_binding_expression
      name: [
        (identifier) @maccess.condname
        (generic_name (identifier) @maccess.condname)
      ])) @maccess.condexpr
]
`

// CSharpExtractor extracts C# source files into graph nodes and edges.
type CSharpExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewCSharpExtractor() *CSharpExtractor {
	lang := csharp.GetLanguage()
	return &CSharpExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qCSharpAll, lang),
	}
}

func (e *CSharpExtractor) Language() string     { return "csharp" }
func (e *CSharpExtractor) Extensions() []string { return []string{".cs"} }

// --- Deferred match buffers ----------------------------------------

type csharpDeferredCall struct {
	name     string
	receiver string
	// recvType is the receiver type a `this.`/`base.` qualifier names —
	// resolved at capture time from the enclosing declaration, since the
	// keyword itself never appears in any tenv.
	recvType string
	line     int
	isMember bool
	// returnUsage is how the call site consumes the return value
	// (graph.ReturnUsage* label), classified at capture time and
	// stamped as edge Meta on the EdgeCalls emitted for this site.
	returnUsage string
	// argCount / typeArgCount are the call's applicability evidence:
	// how many arguments it passes and how many type arguments it
	// spells explicitly. Their *Known flags keep "no evidence"
	// distinguishable from a genuine zero — narrowing an overload set
	// on a count we never measured would be a guess, not a rule.
	argCount     int
	argKnown     bool
	typeArgCount int
	typeArgKnown bool
}

// withCSharpCallArity records the applicability counts an invocation
// node carries, so every capture site stamps the same evidence.
func withCSharpCallArity(c csharpDeferredCall, inv *sitter.Node) csharpDeferredCall {
	c.argCount, c.argKnown = csharpCallArgCount(inv)
	c.typeArgCount, c.typeArgKnown = csharpCallTypeArgCount(inv)
	return c
}

// csharpDeferredLocal buffers a local variable declaration for the
// post-pass type-env build. Matches the legacy two-stage pass: Tier 0
// records explicit types (`Foo svc = ...`); Tier 1 walks the def node
// for `var svc = new Foo()` to recover the type when Tier 0 left a
// "var" key without a real annotation.
type csharpDeferredLocal struct {
	name    string
	rawType string
	defNode *sitter.Node
}

// csharpTypeUse buffers a type referenced only in a local-variable
// annotation (`HttpResponse resp = Get();`) so the post-pass can emit an
// EdgeTypedAs from the enclosing function once funcRanges are built.
// Field / property annotations emit their edge inline from the member
// node, so they don't ride this buffer.
type csharpTypeUse struct {
	typeText string
	line     int
}

// Extract parses the C# source, adaptively recovering symbols that tree-sitter
// silently drops inside conditional-compilation branches. The grammar parses a
// #if/#elif/#else block without raising any error, yet omits every declaration
// inside its branches from the tree — so a method guarded by #if vanishes with
// no signal. When the source uses conditional compilation, Extract therefore
// also extracts from a directive-blanked copy (offset-preserving) and keeps
// whichever variant yields more symbols; native wins ties, so a file the
// grammar already handles cleanly is never perturbed. This beats an always-blank
// rewrite, which would discard the grammar's handling on files that don't need
// it and can unbalance braces when both branches are forced live.
func (e *CSharpExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	res, _, err := e.extractCSharp(filePath, src)
	if err != nil {
		return nil, err
	}
	if hasCSharpConditional(src) {
		if alt, _, altErr := e.extractCSharp(filePath, blankConditionalDirectives(src)); altErr == nil && csharpSymbolCount(alt) > csharpSymbolCount(res) {
			return alt, nil
		}
	}
	return res, nil
}

// hasCSharpConditional reports whether src contains a conditional-compilation
// directive — the cheap gate that decides whether the directive-blanked
// re-parse is worth attempting. A false positive (the token in a string or
// comment) only costs one extra parse whose result loses the symbol-count tie.
func hasCSharpConditional(src []byte) bool {
	return bytes.Contains(src, []byte("#if"))
}

// csharpSymbolCount counts the non-file symbol nodes in a result — the metric
// the adaptive re-parse maximises when deciding whether the directive-blanked
// variant recovered more of the file than the native parse.
func csharpSymbolCount(r *parser.ExtractionResult) int {
	if r == nil {
		return 0
	}
	n := 0
	for _, nd := range r.Nodes {
		if nd != nil && nd.Kind != graph.KindFile {
			n++
		}
	}
	return n
}

func (e *CSharpExtractor) extractCSharp(filePath string, src []byte) (*parser.ExtractionResult, bool, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, false, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}
	hadError := root.HasError()

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "csharp",
	}
	// Parse-health signal: a file the grammar could not fully parse (and that
	// the blanked re-parse did not improve) is flagged so a consumer knows its
	// C# member surface may be incomplete — codegraph parses silently with no
	// such signal.
	if hadError {
		fileNode.Meta = map[string]any{"parse_health": "partial"}
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)
	stampCSharpUsings(root, src, fileNode)
	stampCSharpEFFluent(root, src, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)
	ifaceMethods := make(map[string][]string) // interface name → method names

	// Pre-scan the file's own interface declarations. A base type that
	// names one of these is definitively an interface, even when its name
	// doesn't follow the `I`-prefix convention — the base-list heuristic
	// (emitCSharpBaseList) checks this set before falling back to name
	// shape so a locally-known interface always wins.
	localInterfaces := collectCSharpInterfaceNames(root, src)

	var calls []csharpDeferredCall
	var locals []csharpDeferredLocal
	var typeUses []csharpTypeUse
	var accesses []csharpDeferredAccess
	var fieldAssigns []csharpDeferredFieldAssign

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["ns.def"] != nil:
			e.emitNamespace(m, filePath, fileID, result, seen)

		case m.Captures["class.def"] != nil:
			e.emitContainer(m, "class", graph.KindType, filePath, fileID, src, result, seen, annotationSeen, localInterfaces)

		case m.Captures["iface.def"] != nil:
			e.emitContainer(m, "iface", graph.KindInterface, filePath, fileID, src, result, seen, annotationSeen, localInterfaces)

		case m.Captures["struct.def"] != nil:
			e.emitContainer(m, "struct", graph.KindType, filePath, fileID, src, result, seen, annotationSeen, localInterfaces)

		case m.Captures["record.def"] != nil:
			e.emitContainer(m, "record", graph.KindType, filePath, fileID, src, result, seen, annotationSeen, localInterfaces)

		case m.Captures["enum.def"] != nil:
			e.emitContainer(m, "enum", graph.KindType, filePath, fileID, src, result, seen, annotationSeen, localInterfaces)

		case m.Captures["anon.def"] != nil:
			e.emitAnonymousType(m, filePath, fileID, result, seen)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, fileID, src, result, seen, annotationSeen, ifaceMethods)

		case m.Captures["ctor.def"] != nil:
			e.emitConstructor(m, filePath, fileID, src, result, seen)

		case m.Captures["field.def"] != nil:
			e.emitField(m, filePath, fileID, src, result, seen)

		case m.Captures["prop.def"] != nil:
			e.emitProperty(m, filePath, fileID, src, result, seen)

		case m.Captures["using.def"] != nil:
			e.emitUsing(m, filePath, fileID, result)

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, withCSharpCallArity(csharpDeferredCall{
				name:        m.Captures["callm.method"].Text,
				receiver:    m.Captures["callm.receiver"].Text,
				line:        expr.StartLine + 1,
				isMember:    true,
				returnUsage: classifyReturnUsage(expr.Node, src, csharpReturnUsageSpec),
			}, expr.Node))

		case m.Captures["callself.expr"] != nil:
			expr := m.Captures["callself.expr"]
			calls = append(calls, withCSharpCallArity(csharpDeferredCall{
				name:        m.Captures["callself.method"].Text,
				receiver:    "this",
				recvType:    csharpQualifiedReceiverType(expr.Node, src, "this"),
				line:        expr.StartLine + 1,
				isMember:    true,
				returnUsage: classifyReturnUsage(expr.Node, src, csharpReturnUsageSpec),
			}, expr.Node))

		case m.Captures["callbase.expr"] != nil:
			expr := m.Captures["callbase.expr"]
			calls = append(calls, withCSharpCallArity(csharpDeferredCall{
				name:        m.Captures["callbase.method"].Text,
				receiver:    "base",
				recvType:    csharpQualifiedReceiverType(expr.Node, src, "base"),
				line:        expr.StartLine + 1,
				isMember:    true,
				returnUsage: classifyReturnUsage(expr.Node, src, csharpReturnUsageSpec),
			}, expr.Node))

		// Receiverless calls carry applicability stamps too: the
		// same-file and locality tiers pick among ordinary overload
		// sets, and without arg_count that pick is declaration order
		// (#559).
		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, withCSharpCallArity(csharpDeferredCall{
				name:        m.Captures["call.name"].Text,
				line:        expr.StartLine + 1,
				returnUsage: classifyReturnUsage(expr.Node, src, csharpReturnUsageSpec),
			}, expr.Node))

		case m.Captures["fassign.name"] != nil:
			fieldAssigns = append(fieldAssigns, csharpDeferredFieldAssign{
				name: m.Captures["fassign.name"].Text,
				line: m.Captures["fassign.expr"].StartLine + 1,
			})

		case m.Captures["maccess.expr"] != nil:
			accesses = append(accesses, csharpDeferredAccess{
				name: m.Captures["maccess.name"].Text,
				node: m.Captures["maccess.expr"].Node,
				line: m.Captures["maccess.expr"].StartLine + 1,
			})

		case m.Captures["maccess.condexpr"] != nil:
			accesses = append(accesses, csharpDeferredAccess{
				name:        m.Captures["maccess.condname"].Text,
				node:        m.Captures["maccess.condexpr"].Node,
				line:        m.Captures["maccess.condexpr"].StartLine + 1,
				conditional: true,
			})

		case m.Captures["lvar.def"] != nil:
			locals = append(locals, csharpDeferredLocal{
				name:    m.Captures["lvar.name"].Text,
				rawType: m.Captures["lvar.type"].Text,
				defNode: m.Captures["lvar.def"].Node,
			})
			// Buffer the annotated type so the post-pass (once
			// funcRanges exist) can attribute an EdgeTypedAs to the
			// enclosing function — a type used only in a local
			// annotation seeds tenv but otherwise emits no reference,
			// so find_usages would miss it without an LSP.
			typeUses = append(typeUses, csharpTypeUse{
				typeText: m.Captures["lvar.type"].Text,
				line:     m.Captures["lvar.type"].StartLine + 1,
			})
		}
	})

	// Stamp interface method names onto interface nodes' Meta["methods"].
	for _, n := range result.Nodes {
		if n.Kind != graph.KindInterface {
			continue
		}
		if methods, ok := ifaceMethods[n.Name]; ok {
			if n.Meta == nil {
				n.Meta = make(map[string]any)
			}
			n.Meta["methods"] = methods
		}
	}

	// Resolve calls against the function-range lookup + the per-method
	// type environments. Owner attribution runs once per local, call and
	// type use — the sorted lookup keeps that from multiplying into an
	// O(locals×functions) linear-scan product on member-heavy files.
	funcRanges := newCSharpFuncLookup(buildFuncRanges(result))

	// Build type environments in legacy precedence, scoped per enclosing
	// method — a same-named local of a different type in a sibling method
	// must not bleed into this method's receiver stamps (a file-scoped
	// last-wins map mis-typed the receiver and both the extension binder
	// and the receiver gate act on that evidence):
	//   Tier 0 — explicit type annotations (skip "var" placeholder)
	//   Tier 1 — `var x = new Foo()` walk for `var`-keyed locals only
	//   Tier 2 — `var x = await LoadAsync()` walk → the awaited Task<T>'s T
	localOwner := func(l csharpDeferredLocal) string {
		if l.defNode == nil {
			return ""
		}
		return funcRanges.enclosing(int(l.defNode.StartPoint().Row) + 1)
	}
	tenvByOwner := map[string]typeEnv{}
	setLocalType := func(owner, name, typeName string) {
		env := tenvByOwner[owner]
		if env == nil {
			env = make(typeEnv)
			tenvByOwner[owner] = env
		}
		env[name] = typeName
	}
	for _, l := range locals {
		owner := localOwner(l)
		if owner == "" {
			continue
		}
		typeName := normalizeCSharpTypeName(l.rawType)
		if typeName != "" && typeName != "var" {
			setLocalType(owner, l.name, typeName)
		}
	}
	for _, l := range locals {
		owner := localOwner(l)
		if owner == "" || l.rawType != "var" || l.defNode == nil {
			continue
		}
		if _, exists := tenvByOwner[owner][l.name]; exists {
			continue
		}
		// First creation in document order = the outermost one; a
		// nested `new` inside a collection/object initializer must
		// not override it.
		done := false
		walkNodes(l.defNode, func(n *sitter.Node) {
			if !done && n.Type() == "object_creation_expression" {
				typeName := inferTypeFromCSharpNew(n, src)
				if typeName != "" {
					setLocalType(owner, l.name, typeName)
					done = true
				}
			}
		})
	}
	//   Tier 2 — `var x = await LoadAsync()` walk: no object_creation ever
	//   appears; the local's type is the T inside the awaited call's Task<T>,
	//   reachable through the called method's declared return shape.
	for _, l := range locals {
		owner := localOwner(l)
		if owner == "" || l.rawType != "var" || l.defNode == nil {
			continue
		}
		if _, exists := tenvByOwner[owner][l.name]; exists {
			continue
		}
		done := false
		walkNodes(l.defNode, func(n *sitter.Node) {
			if done || n.Type() != "await_expression" {
				return
			}
			done = true
			// The initializer must BE the await (parens aside): in
			// `var w = (await Load()).Weigh()` the local holds Weigh's
			// return, and stamping the awaited T would hand the
			// resolver a confident wrong answer.
			for p := n.Parent(); p != nil; p = p.Parent() {
				switch p.Type() {
				case "parenthesized_expression":
					continue
				case "equals_value_clause", "variable_declarator":
					// direct initializer — accept
				default:
					return // nested inside a longer expression
				}
				break
			}
			inner := n.NamedChild(0)
			if inner == nil {
				return
			}
			if t := csharpAwaitedCallType(inner.Content(src), csharpOwnerTypeName(owner), tenvByOwner[owner], result); t != "" {
				setLocalType(owner, l.name, t)
			}
		})
	}

	// Type SHAPE rides in a parallel per-method map: the core stamps keep
	// their bare spelling (every downstream consumer stays valid), while
	// array/nullable suffixes and generic arguments — which are part of
	// applicability — survive in a receiver_shape stamp.
	shapesByOwner := map[string]map[string]string{}
	setLocalShape := func(owner, name, shape string) {
		m := shapesByOwner[owner]
		if m == nil {
			m = map[string]string{}
			shapesByOwner[owner] = m
		}
		m[name] = shape
	}
	for _, l := range locals {
		owner := localOwner(l)
		if owner == "" {
			continue
		}
		if _, exists := shapesByOwner[owner][l.name]; exists {
			continue
		}
		if shape := csharpCanonTypeShape(l.rawType); shape != "" {
			setLocalShape(owner, l.name, shape)
		} else if l.rawType == "var" && l.defNode != nil {
			// Same first-creation rule as the type walk above — the
			// two stamps must describe the same creation expression.
			done := false
			walkNodes(l.defNode, func(n *sitter.Node) {
				if !done && n.Type() == "object_creation_expression" {
					if tn := n.ChildByFieldName("type"); tn != nil {
						if s := csharpCanonTypeShape(tn.Content(src)); s != "" {
							setLocalShape(owner, l.name, s)
							done = true
						}
					}
				}
			})
		}
	}

	// Builtin locals key per enclosing method: a same-named local of a
	// different type in a sibling method must not bleed into this
	// method's receiver stamp (a file-scoped last-wins map mis-typed
	// the receiver and the extension binder would act on it).
	builtinsByOwner := map[string]map[string]string{}
	for _, l := range locals {
		if l.defNode == nil {
			continue
		}
		bt := csharpBuiltinTypeName(l.rawType)
		if bt == "" {
			continue
		}
		owner := funcRanges.enclosing(int(l.defNode.StartPoint().Row) + 1)
		if owner == "" {
			continue
		}
		m := builtinsByOwner[owner]
		if m == nil {
			m = map[string]string{}
			builtinsByOwner[owner] = m
		}
		m[l.name] = bt
	}

	// Local-variable type annotations → EdgeTypedAs from the enclosing
	// function (file node as fallback). Mirrors the parameter/return
	// type-use emission so a type referenced only in a local body
	// declaration is still a navigable reference without an LSP.
	for _, tu := range typeUses {
		ownerID := funcRanges.enclosing(tu.line)
		if ownerID == "" {
			ownerID = fileID
		}
		emitCSharpTypeUseEdges(ownerID, tu.typeText, filePath, tu.line, result)
	}

	// Expression-site type references the symbol/annotation walk misses:
	// instantiation (`new Foo()`), casts / type-tests (`(Foo)x`, `x is Foo`,
	// `x as Foo`), static / const access (`Foo.Empty`, `typeof(Foo)`,
	// `nameof(Foo)`), and attribute type names (`[Foo]`). Inheritance is
	// already covered by emitCSharpBaseList, so it is not re-emitted here.
	emitCSharpReferenceForms(root, src, filePath, fileID, result)

	for _, c := range calls {
		callerID := funcRanges.enclosing(c.line)
		if callerID == "" {
			continue
		}
		if c.isMember {
			edge := &graph.Edge{
				From: callerID, To: "unresolved::*." + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			if c.recvType != "" {
				// this./base.-qualified: the receiver type came from the
				// enclosing declaration, not from any variable lookup.
				edge.Meta = map[string]any{"receiver_type": c.recvType}
			} else if recvType, ok := tenvByOwner[callerID][c.receiver]; ok {
				edge.Meta = map[string]any{"receiver_type": recvType}
				if shape := shapesByOwner[callerID][c.receiver]; shape != "" && shape != recvType {
					edge.Meta["receiver_shape"] = shape
				}
			} else if bt := builtinsByOwner[callerID][c.receiver]; bt != "" {
				// Builtins stay out of receiver_type (the receiver-gate
				// passes key on user types); extension eligibility still
				// needs them — `n.Foo()` on an int must match
				// `Foo(this int)` and refuse `Foo(this string)`.
				edge.Meta = map[string]any{"receiver_builtin": bt}
				if shape := shapesByOwner[callerID][c.receiver]; shape != "" && shape != bt {
					edge.Meta["receiver_shape"] = shape
				}
			} else if inner := csharpAwaitedReceiver(c.receiver); inner != "" {
				// `(await LoadAsync()).X()` — the chain walker collapses a
				// fully-parenthesized receiver to nothing; the receiver is
				// the T inside the awaited call's Task<T>.
				if t := csharpAwaitedCallType(inner, csharpOwnerTypeName(callerID), tenvByOwner[callerID], result); t != "" {
					edge.Meta = map[string]any{"receiver_type": t}
				}
			} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
				stampFactoryChainReceiver(edge, c.receiver, resolveChainType(c.receiver, tenvByOwner[callerID], result))
				if edge.Meta == nil && !strings.Contains(c.receiver, "(") {
					// A namespace-qualified receiver the chain walker could
					// not type (`Lib.BagExt.Add(bag)`). That is the same
					// static-form evidence as the bare spelling below —
					// without it the binder reads the call as extension
					// form, discounts a `this` slot the argument list never
					// filled, and lands on the wrong overload.
					edge.Meta = map[string]any{"receiver_name": c.receiver}
				}
			} else if c.receiver != "" {
				// A bare receiver nothing above could type. Its spelling
				// is still evidence: reaching here means no local, param
				// or builtin in scope carries that name, so a receiver
				// that names a static class is the STATIC form of an
				// extension call (`BagExt.Add(bag)`) — where the `this`
				// slot is filled by the first argument, not the
				// receiver. The extension binder needs that distinction
				// before it can compare argument counts.
				edge.Meta = map[string]any{"receiver_name": c.receiver}
			}
			// Eviction restubs a member call to a bare unresolved name; the
			// marker is what lets the resolver still route the rebind through
			// the extension rule instead of a locality guess.
			if edge.Meta == nil {
				edge.Meta = map[string]any{}
			}
			edge.Meta["member_call"] = true
			// Applicability evidence for the overload set behind this
			// name. Member calls carry it because they are the shape
			// the extension binder resolves; a plain `Foo()` is already
			// bound by scope rules that never consult arity.
			if c.argKnown {
				edge.Meta["arg_count"] = c.argCount
			}
			if c.typeArgKnown {
				edge.Meta["type_arg_count"] = c.typeArgCount
			}
			stampReturnUsage(edge, c.returnUsage)
			result.Edges = append(result.Edges, edge)
			continue
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::" + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		// Applicability evidence rides receiverless calls too: the
		// resolver's same-file and locality tiers pick among ordinary
		// overload sets, and without arg_count that pick is declaration
		// order (#559).
		if c.argKnown {
			if edge.Meta == nil {
				edge.Meta = map[string]any{}
			}
			edge.Meta["arg_count"] = c.argCount
		}
		if c.typeArgKnown {
			if edge.Meta == nil {
				edge.Meta = map[string]any{}
			}
			edge.Meta["type_arg_count"] = c.typeArgCount
		}
		stampReturnUsage(edge, c.returnUsage)
		result.Edges = append(result.Edges, edge)
	}

	// Member accesses ride the same deferred machinery as calls — the
	// receiver-typing ladder needs the finished tenv.
	emitCSharpMemberAccesses(accesses, src, filePath, funcRanges,
		tenvByOwner, builtinsByOwner, result)

	// Field-identifier uses need the shadow indexes: every DECLARED
	// local by name (typed or not — tenv alone holds only the typed
	// ones), parameters, and builtin-typed locals.
	localNamesByOwner := map[string]map[string]bool{}
	for _, l := range locals {
		owner := localOwner(l)
		if owner == "" {
			continue
		}
		m := localNamesByOwner[owner]
		if m == nil {
			m = map[string]bool{}
			localNamesByOwner[owner] = m
		}
		m[l.name] = true
	}
	emitCSharpFieldIdentifierUses(calls, accesses, fieldAssigns, src,
		filePath, funcRanges, localNamesByOwner, builtinsByOwner, result)

	// .NET surfaces a symbol walk misses: DI registrations + COM
	// interop. Stamped onto the file node.
	detectDotNetSurfaces(src, result)

	// Same-file constant/variable value references → impact-radius reads.
	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)

	captureMediatRDispatch(result, root, filePath, src)

	return result, hadError, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *CSharpExtractor) emitNamespace(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["ns.name"].Text
	def := m.Captures["ns.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "csharp",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitContainer collapses the per-kind class/interface/struct/enum
// node emission. The capture-name prefix selects which capture set to
// read from (the legacy code repeated this body four times).
func (e *CSharpExtractor) emitContainer(m parser.QueryResult, kind string, nodeKind graph.NodeKind, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, localInterfaces map[string]bool) {
	name := m.Captures[kind+".name"].Text
	def := m.Captures[kind+".def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": csharpVisibility(def.Node, src, VisibilityInternal)}
	// A struct is a value type; record struct too. Surfacing it lets a
	// consumer reason about copy-vs-reference semantics.
	if kind == "struct" {
		meta["value_type"] = true
	}
	// Structural flavor, keyed off the capture that funnelled in here.
	switch kind {
	case "iface":
		meta["type_flavor"] = "interface"
	case "struct":
		meta["type_flavor"] = "struct"
	case "enum":
		meta["type_flavor"] = "enum"
	case "record":
		meta["type_flavor"] = "record"
	default:
		meta["type_flavor"] = "class"
	}
	// Namespace scope so a type in `namespace App.Core` is attributable
	// without re-deriving its enclosing namespace from source.
	if ns := csharpEnclosingNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	if doc := extractCSharpDoc(src, def.StartLine); doc != "" {
		meta["doc"] = doc
	}
	// EF Core fluent mapping: a class implementing
	// IEntityTypeConfiguration<T> carries facts the resolver joins to
	// the entity class later — the entity lives in another file.
	if kind == "class" || kind == "record" {
		stampCSharpEFAttribute(def.Node, src, meta)
		stampCSharpEFConfig(def.Node, src, meta)
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: nodeKind, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitCSharpAnnotationEdges(csharpCollectAttributes(def.Node, src), id, filePath, result, annotationSeen)
	// EF Core model attribution: [Table] → EdgeModelsTable. Classes and
	// records only — EF entities are reference types.
	if kind == "class" || kind == "record" {
		emitCSharpORMEdges(def.Node, src, id, filePath, result)
	}
	emitCSharpGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
	// Classes, structs, records, and interfaces carry a base list;
	// emitCSharpBaseList derives each entry's edge kind from the
	// declaration (base class vs interface for classes, all-interface
	// for structs and records, inheritance for interfaces).
	switch kind {
	case "class", "struct", "record", "iface":
		emitCSharpBaseList(id, def.Node, src, filePath, localInterfaces, result)
	case "enum":
		e.emitCSharpEnumMembers(def.Node, src, filePath, id, name, result, seen)
	}
	if kind == "record" {
		e.emitCSharpRecordPositionalProps(id, name, def.Node, src, filePath, fileID, result, seen)
	}
}

// emitCSharpRecordPositionalProps fabricates property member nodes for a
// record's positional parameters — `record Medal(int Id, string Motto)`
// synthesizes public properties Id and Motto with no declaration node
// for the member walk to find, so the parameter list is the only source.
// Runs at container emission, which precedes the body's member matches
// in tree order: an explicit redeclaration of a positional property
// (legal C# — it replaces the synthesized one) hits the seen guard and
// stays a single node for the same logical member.
func (e *CSharpExtractor) emitCSharpRecordPositionalProps(ownerID, ownerName string, decl *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	// The record's parameter_list is an unnamed child in this grammar —
	// unlike method parameters, ChildByFieldName("parameters") finds
	// nothing, so scan the direct children by type.
	var params *sitter.Node
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		if c := decl.NamedChild(i); c != nil && c.Type() == "parameter_list" {
			params = c
			break
		}
	}
	if params == nil {
		return
	}
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		p := params.NamedChild(i)
		if p == nil || p.Type() != "parameter" {
			continue
		}
		nameNode := p.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		pname := nameNode.Content(src)
		id := filePath + "::" + ownerName + "." + pname
		if seen[id] {
			continue
		}
		seen[id] = true
		line := int(p.StartPoint().Row) + 1
		meta := map[string]any{
			"receiver":   ownerName,
			"visibility": VisibilityPublic,
			"kind":       "property",
			"positional": true,
		}
		if t := p.ChildByFieldName("type"); t != nil {
			meta["field_type"] = strings.TrimSpace(t.Content(src))
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindField, Name: pname,
			FilePath: filePath, StartLine: line, EndLine: line,
			Language: "csharp",
			Meta:     meta,
		})
		result.Edges = append(result.Edges,
			&graph.Edge{From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line},
			&graph.Edge{From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line})
	}
}

// emitCSharpEnumMembers emits one KindEnumMember per `enum_member_declaration`
// in an enum body, with its explicit value (when given) and a MemberOf edge to
// the enum — so an enum's members are navigable symbols, not lost in the type.
func (e *CSharpExtractor) emitCSharpEnumMembers(enumNode *sitter.Node, src []byte, filePath, enumID, enumName string, result *parser.ExtractionResult, seen map[string]bool) {
	var list *sitter.Node
	for i, _nc := 0, int(enumNode.ChildCount()); i < _nc; i++ {
		if c := enumNode.Child(i); c != nil && c.Type() == "enum_member_declaration_list" {
			list = c
			break
		}
	}
	if list == nil {
		return
	}
	for i, _nc := 0, int(list.NamedChildCount()); i < _nc; i++ {
		mem := list.NamedChild(i)
		if mem.Type() != "enum_member_declaration" {
			continue
		}
		var nameNode, valNode *sitter.Node
		if nn := mem.ChildByFieldName("name"); nn != nil {
			nameNode = nn
		}
		for j, _nc := 0, int(mem.NamedChildCount()); j < _nc; j++ {
			c := mem.NamedChild(j)
			if c.Type() == "identifier" && nameNode == nil {
				nameNode = c
			} else if c != mem.ChildByFieldName("name") && c.Type() != "identifier" {
				valNode = c
			}
		}
		if nameNode == nil {
			continue
		}
		mname := nameNode.Content(src)
		line := int(mem.StartPoint().Row) + 1
		id, ok := disambiguateID(seen, filePath+"::"+enumName+"."+mname, line)
		if !ok {
			continue
		}
		emeta := map[string]any{"enum": enumID, "receiver": enumName}
		if valNode != nil {
			emeta["value"] = strings.TrimSpace(valNode.Content(src))
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindEnumMember, Name: mname,
			FilePath: filePath, StartLine: line, EndLine: line, Language: "csharp", Meta: emeta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: enumID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
		})
	}
}

// csharpHasModifier reports whether a declaration carries the given modifier
// keyword (const / static / async / readonly / …).
func csharpHasModifier(decl *sitter.Node, src []byte, mod string) bool {
	if decl == nil {
		return false
	}
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		c := decl.Child(i)
		if c != nil && c.Type() == "modifier" && strings.TrimSpace(c.Content(src)) == mod {
			return true
		}
	}
	return false
}

// csharpExtensionReceiverType returns the generics- and namespace-stripped
// type of a method's first parameter when that parameter carries the `this`
// modifier — the receiver type of a C# extension method (`static int Foo(this
// string s)` → "string"). Returns "" for a non-extension method. Unlike
// normalizeCSharpTypeName it keeps primitive receivers (string / int), since
// extension methods commonly extend them.
func csharpExtensionReceiverType(methodNode *sitter.Node, src []byte) string {
	if t := csharpExtensionReceiverTypeNode(methodNode, src); t != nil {
		return normalizeCSharpBaseName(t.Content(src))
	}
	return ""
}

// csharpExtensionReceiverRaw returns the this-param's type as written —
// qualification, generic arguments, and array/nullable suffixes intact.
// "" for a non-extension method.
func csharpExtensionReceiverRaw(methodNode *sitter.Node, src []byte) string {
	if t := csharpExtensionReceiverTypeNode(methodNode, src); t != nil {
		return t.Content(src)
	}
	return ""
}

// csharpExtensionReceiverTypeNode finds the type node of a method's
// `this`-marked first parameter — nil for a non-extension method.
func csharpExtensionReceiverTypeNode(methodNode *sitter.Node, src []byte) *sitter.Node {
	if methodNode == nil {
		return nil
	}
	params := methodNode.ChildByFieldName("parameters")
	if params == nil {
		return nil
	}
	var first *sitter.Node
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		c := params.NamedChild(i)
		if c != nil && c.Type() == "parameter" {
			first = c
			break
		}
	}
	if first == nil {
		return nil
	}
	hasThis := false
	for i, _nc := 0, int(first.ChildCount()); i < _nc; i++ {
		c := first.Child(i)
		if c == nil {
			continue
		}
		// The `this` marker is a `modifier` child (grammar revisions may also
		// spell it `parameter_modifier` or a bare `this` keyword node).
		ct := c.Type()
		if (ct == "modifier" || ct == "parameter_modifier") && strings.TrimSpace(c.Content(src)) == "this" {
			hasThis = true
			break
		}
		if ct == "this" {
			hasThis = true
			break
		}
	}
	if !hasThis {
		return nil
	}
	return first.ChildByFieldName("type")
}

// csharpEnclosingNamespace returns the dotted name of the enclosing
// namespace scope, or "". Nested block declarations join outer-to-inner
// (`namespace A { namespace B {` → "A.B").
func csharpEnclosingNamespace(node *sitter.Node, src []byte) string {
	var parts []string
	root := node
	for n := node; n != nil; n = n.Parent() {
		t := n.Type()
		if t == "namespace_declaration" || t == "file_scoped_namespace_declaration" {
			if nm := csharpNamespaceName(n, src); nm != "" {
				parts = append(parts, nm)
			}
		}
		root = n
	}
	if len(parts) > 0 {
		// Collected innermost-first — reverse into source order.
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
		return strings.Join(parts, ".")
	}
	// File-scoped form: `namespace X;` spans only its own statement in the
	// AST — the declarations it governs are later siblings under the
	// compilation unit, so the ancestor walk above never sees it.
	for i, _nc := 0, int(root.NamedChildCount()); i < _nc; i++ {
		c := root.NamedChild(i)
		if c.Type() == "file_scoped_namespace_declaration" && c.StartByte() <= node.StartByte() {
			return csharpNamespaceName(c, src)
		}
	}
	return ""
}

// csharpNamespaceName extracts the dotted name from a namespace_declaration
// or file_scoped_namespace_declaration node.
func csharpNamespaceName(n *sitter.Node, src []byte) string {
	if nm := n.ChildByFieldName("name"); nm != nil {
		return strings.TrimSpace(nm.Content(src))
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		c := n.NamedChild(i)
		if c.Type() == "identifier" || c.Type() == "qualified_name" {
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// emitAnonymousType indexes a C# anonymous type — `new { Name = ..., Age = ... }`
// — as a synthetic KindType node with an EdgeExtends to object, its implicit
// base. C# anonymous types are nameless compiler-generated classes that derive
// directly from System.Object; surfacing each instantiation as a distinct type
// keeps the graph's type set complete and gives the projection a node to anchor
// to, rather than vanishing into the expression that produced it.
func (e *CSharpExtractor) emitAnonymousType(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["anon.def"]
	line := def.StartLine + 1
	name := fmt.Sprintf("anon@%d", line)
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: line, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     map[string]any{"anonymous": true, "type_flavor": "anonymous_class"},
	})
	result.Edges = append(result.Edges,
		&graph.Edge{From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line},
		&graph.Edge{From: id, To: "unresolved::object", Kind: graph.EdgeExtends, FilePath: filePath, Line: line, Origin: graph.OriginASTInferred},
	)
}

// csharpVisibility scans a declaration's modifier children for an
// access modifier. C# defaults are container-dependent — defaultVis is
// "internal" for top-level types and "private" for class members.
func csharpVisibility(decl *sitter.Node, src []byte, defaultVis string) string {
	if decl == nil {
		return defaultVis
	}
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		c := decl.Child(i)
		if c == nil {
			continue
		}
		if c.Type() != "modifier" {
			continue
		}
		switch strings.TrimSpace(c.Content(src)) {
		case "public":
			return VisibilityPublic
		case "private":
			return VisibilityPrivate
		case "protected":
			return VisibilityProtected
		case "internal":
			return VisibilityInternal
		}
	}
	return defaultVis
}

// csharpCollectAttributes walks a declaration's children for
// `attribute_list` nodes ([Attr1, Attr2(...)]) and returns each
// attribute's bare name plus verbatim args. Multiple attributes can
// appear inside one bracket pair, and multiple bracket pairs can
// stack on the same declaration.
func csharpCollectAttributes(decl *sitter.Node, src []byte) []javaAnnotation {
	if decl == nil {
		return nil
	}
	var out []javaAnnotation
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		c := decl.Child(i)
		if c == nil || c.Type() != "attribute_list" {
			continue
		}
		for j, _nc := 0, int(c.ChildCount()); j < _nc; j++ {
			a := c.Child(j)
			if a == nil || a.Type() != "attribute" {
				continue
			}
			var name, args string
			line := int(a.StartPoint().Row) + 1
			if nm := a.ChildByFieldName("name"); nm != nil {
				name = nm.Content(src)
			}
			for k, _nc := 0, int(a.ChildCount()); k < _nc; k++ {
				inner := a.Child(k)
				if inner == nil {
					continue
				}
				if inner.Type() == "attribute_argument_list" {
					txt := inner.Content(src)
					if len(txt) >= 2 && txt[0] == '(' && txt[len(txt)-1] == ')' {
						txt = txt[1 : len(txt)-1]
					}
					args = txt
				}
			}
			if name != "" {
				out = append(out, javaAnnotation{name: name, args: args, line: line})
			}
		}
	}
	return out
}

func emitCSharpAnnotationEdges(anns []javaAnnotation, fromID, filePath string, result *parser.ExtractionResult, seen map[string]bool) {
	for _, a := range anns {
		if a.name == "" {
			continue
		}
		EmitAnnotationEdge(fromID, "csharp", a.name, a.args, filePath, a.line, result, seen)
	}
}

// extractCSharpDoc tries the XML-doc form first (/// <summary>…) and
// falls back to /** … */ block comments (less common in C# but valid).
func extractCSharpDoc(src []byte, startRow int) string {
	if d := ExtractDocAbove(src, startRow, DocLangCSharpXML); d != "" {
		return d
	}
	return ExtractDocAbove(src, startRow, DocLangBlockStar)
}

func (e *CSharpExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, ifaceMethods map[string][]string) {
	name := m.Captures["method.name"].Text
	def := m.Captures["method.def"]
	startLine1 := def.StartLine + 1

	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration", "interface_declaration", "record_declaration")
	if owner.kind == "" {
		// Method outside a recognised container — legacy didn't emit
		// these (its nested queries required class/struct/interface
		// parentage), so skip.
		return
	}

	// Interface members feed the interface node's method-name list (read
	// back where interface nodes are stamped). Beyond that list they also
	// need their own member-level node: a through-interface call site binds
	// to the interface member, so find_usages can only answer — and the
	// member-level dispatch synthesis can only fan out to implementations —
	// when that node exists. A C# 8 default interface method carries a body
	// and otherwise flows through the concrete-method path unchanged.
	isIface := owner.kind == "interface_declaration"
	if isIface {
		ifaceMethods[owner.name] = append(ifaceMethods[owner.name], name)
	}

	id := filePath + "::" + owner.name + "." + name
	if seen[id] {
		id = filePath + "::" + owner.name + "." + name + "_L" + fmt.Sprint(startLine1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	// Interface members are implicitly public; an explicit modifier (a C# 8
	// default member marked private/protected) still wins via csharpVisibility.
	defaultVis := VisibilityPrivate
	if isIface {
		defaultVis = VisibilityPublic
	}
	meta := map[string]any{
		"receiver":   owner.name,
		"visibility": csharpVisibility(def.Node, src, defaultVis),
	}
	// Distinguish a bodyless interface declaration from a concrete method so
	// dispatch synthesis and analyzers can tell them apart; a bodyless
	// member must never be treated as a dead-code candidate just for lacking
	// a body.
	if isIface {
		meta["iface_member"] = true
	}
	if rt, shape := extractCSharpMethodReturnType(def.Node, src, name); rt != "" {
		meta["return_type"] = rt
		if shape != rt {
			meta["return_shape"] = shape
		}
	}
	if csharpHasModifier(def.Node, src, "async") {
		meta["async"] = true
	}
	if csharpHasModifier(def.Node, src, "static") {
		meta["static"] = true
	}
	// Extension method: a static method whose first parameter carries the
	// `this` modifier. Record the receiver type it extends so member-call
	// resolution can bind `x.Foo()` to it (the id stays <StaticClass>.<name>).
	// The method's own type parameters are applicability evidence for
	// EVERY method, not just extensions: an explicit `Foo<int>(x)` call
	// splits a generic/non-generic ordinary overload pair only when the
	// generic one is stamped (#559).
	tparams := csharpMethodTypeParamNames(def.Node, src)
	if len(tparams) > 0 {
		names := make([]string, 0, len(tparams))
		for n := range tparams {
			names = append(names, n)
		}
		sort.Strings(names)
		meta["method_type_params"] = strings.Join(names, ",")
	}
	if extType := csharpExtensionReceiverType(def.Node, src); extType != "" {
		meta["extension"] = true
		meta["this_param_type"] = extType
		// `Foo<T>(this T v)` — the this-param names the method's own
		// type parameter, i.e. it matches any receiver; the binder must
		// not treat it as a concrete type named "T". A `where T : X`
		// clause bounds that: the constraint core names ride along so
		// the binder can exclude receivers that provably fail them.
		if tparams[extType] {
			meta["this_param_generic"] = true
			if cons := csharpTypeParamConstraints(def.Node, src, extType); len(cons) > 0 {
				meta["this_param_constraints"] = strings.Join(cons, ",")
			}
		}
		// Shape (generic args, array/nullable suffixes) is part of
		// applicability — it rides beside the bare core stamp.
		if raw := csharpExtensionReceiverRaw(def.Node, src); raw != "" {
			if shape := csharpCanonTypeShape(raw); shape != "" && shape != extType {
				meta["this_param_shape"] = shape
			}
		}
	}
	// Parameter arity — the evidence that splits a same-name overload set
	// the receiver type alone cannot. Stamped on the node rather than read
	// back off the KindParam nodes because those carry no default-value
	// marker, so they cannot answer "how many arguments MUST a caller supply".
	if count, required, variadic, ok := csharpParamArity(def.Node, src); ok {
		meta["param_count"] = count
		if required != count {
			meta["param_required"] = required
		}
		if variadic {
			meta["param_variadic"] = true
		}
	}
	if ns := csharpEnclosingNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	if doc := extractCSharpDoc(src, def.StartLine); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	ownerID := filePath + "::" + owner.name
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
	})
	emitCSharpAnnotationEdges(csharpCollectAttributes(def.Node, src), id, filePath, result, annotationSeen)
	if body := csharpFunctionBody(def.Node); body != nil {
		emitCSharpAsyncSpawns(id, body, src, filePath, result)
	}
	emitCSharpFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

func (e *CSharpExtractor) emitConstructor(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["ctor.def"]
	startLine1 := def.StartLine + 1
	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration", "record_declaration")
	if owner.kind == "" {
		return
	}
	id := filePath + "::" + owner.name + ".<init>"
	if seen[id] {
		id = filePath + "::" + owner.name + ".<init>_L" + fmt.Sprint(startLine1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	// Ctors own call edges the same way methods do — without the scope
	// stamp the resolver's namespace walk would read a ctor caller as
	// global-namespace code.
	meta := map[string]any{"receiver": owner.name}
	if ns := csharpEnclosingNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: owner.name + ".<init>",
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	ownerID := filePath + "::" + owner.name
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
	})
	// Constructor params: same shape as methods so DI containers and
	// codegen tooling see the dependencies they need.
	if body := csharpFunctionBody(def.Node); body != nil {
		emitCSharpAsyncSpawns(id, body, src, filePath, result)
	}
	emitCSharpFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

func (e *CSharpExtractor) emitField(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["field.def"]
	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration", "interface_declaration", "record_declaration")
	if owner.kind == "" {
		return
	}
	name := m.Captures["field.name"].Text
	id := filePath + "::" + owner.name + "." + name
	if seen[id] {
		return
	}
	seen[id] = true
	// Interface fields (C# 8+ static/const members) are implicitly public.
	isIface := owner.kind == "interface_declaration"
	defaultVis := VisibilityPrivate
	if isIface {
		defaultVis = VisibilityPublic
	}
	meta := map[string]any{
		"receiver":   owner.name,
		"visibility": csharpVisibility(def.Node, src, defaultVis),
	}
	if isIface {
		meta["iface_member"] = true
	}
	// A field_declaration's type lives on its nested variable_declaration
	// (`field_declaration → variable_declaration[type] → variable_declarator`),
	// not as a direct `type` field of the field_declaration itself.
	fieldTypeRaw := csharpFieldDeclType(def.Node, src)
	if fieldTypeRaw != "" {
		meta["field_type"] = fieldTypeRaw
	}
	// A `const` field is a compile-time constant, not a mutable field —
	// classify it as KindConstant so it joins the value-reference impact
	// surface. `static` / `readonly` are stamped for completeness.
	fieldKind := graph.KindField
	if csharpHasModifier(def.Node, src, "const") {
		fieldKind = graph.KindConstant
	}
	if csharpHasModifier(def.Node, src, "static") {
		meta["static"] = true
	}
	if csharpHasModifier(def.Node, src, "readonly") {
		meta["readonly"] = true
	}
	if doc := extractCSharpDoc(src, def.StartLine); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: fieldKind, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	ownerID := filePath + "::" + owner.name
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
	// Field type annotation → EdgeTypedAs from the field node, so a type
	// used only as a field's declared type (`private Session _s;`) is a
	// navigable reference without an LSP.
	emitCSharpTypeUseEdges(id, fieldTypeRaw, filePath, def.StartLine+1, result)
}

func (e *CSharpExtractor) emitProperty(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["prop.def"]
	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration", "interface_declaration", "record_declaration")
	if owner.kind == "" {
		return
	}
	name := m.Captures["prop.name"].Text
	id := filePath + "::" + owner.name + "." + name
	if seen[id] {
		return
	}
	seen[id] = true
	// Interface properties are implicitly public; explicit modifiers still win.
	isIface := owner.kind == "interface_declaration"
	defaultVis := VisibilityPrivate
	if isIface {
		defaultVis = VisibilityPublic
	}
	meta := map[string]any{
		"receiver":   owner.name,
		"visibility": csharpVisibility(def.Node, src, defaultVis),
		"kind":       "property",
	}
	if isIface {
		meta["iface_member"] = true
	}
	var propTypeRaw string
	if t := def.Node.ChildByFieldName("type"); t != nil {
		propTypeRaw = strings.TrimSpace(t.Content(src))
		meta["field_type"] = propTypeRaw
	}
	if doc := extractCSharpDoc(src, def.StartLine); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindField, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	ownerID := filePath + "::" + owner.name
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
	// Property type annotation → EdgeTypedAs from the property node.
	emitCSharpTypeUseEdges(id, propTypeRaw, filePath, def.StartLine+1, result)
}

// stampCSharpUsings records the file's plain namespace usings (global
// ones included) on the file node as Meta["usings"]. Aliases grant no
// bare-name namespace visibility and are skipped. Two side stamps carry
// the forms the plain shape cannot: Meta["global_usings"] (project-scoped
// — the resolver propagates them beyond the declaring file) and
// Meta["using_static"] (class targets whose extension methods stay
// callable in extension form). Resolution rewrites the per-directive
// import edges, so the resolver's namespace narrowing reads these
// shapes, which nothing mutates.
//
// C# using visibility is scope-by-scope — a using inside `namespace A`
// applies within A only, invisible to a sibling namespace in the same
// file — so each plain using is ALSO stamped with its declaring scope
// as Meta["scoped_usings"] ("scope|name", empty scope = compilation
// unit). Additive: the flat keys keep their exact legacy shape.
func stampCSharpUsings(root *sitter.Node, src []byte, fileNode *graph.Node) {
	var usings, globals, statics, scoped, globalStatics []string
	seen := map[string]bool{}
	seenStatic := map[string]bool{}
	seenScoped := map[string]bool{}
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "using_directive" {
			return
		}
		var name string
		isGlobal, isStatic := false, false
		for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
			c := n.Child(i)
			switch c.Type() {
			case "global":
				isGlobal = true
			case "static":
				isStatic = true
			case "name_equals", "=":
				return
			case "identifier", "qualified_name":
				name = strings.TrimSpace(c.Content(src))
			}
		}
		if name == "" {
			return
		}
		if isStatic {
			if !seenStatic[name] {
				seenStatic[name] = true
				statics = append(statics, name)
				// A `global using static` is compilation-scoped like its
				// namespace sibling — its own stamp lets the resolver
				// propagate it beyond the declaring file.
				if isGlobal {
					globalStatics = append(globalStatics, name)
				}
			}
			return
		}
		if !seen[name] {
			seen[name] = true
			usings = append(usings, name)
			if isGlobal {
				globals = append(globals, name)
			}
		}
		// The scoped entry dedups per (scope, name) — the same namespace
		// imported at two scopes is two distinct visibility facts.
		if key := csharpEnclosingNamespace(n, src) + "|" + name; !seenScoped[key] {
			seenScoped[key] = true
			scoped = append(scoped, key)
		}
	})
	if len(usings) == 0 && len(statics) == 0 {
		return
	}
	if fileNode.Meta == nil {
		fileNode.Meta = map[string]any{}
	}
	if len(usings) > 0 {
		fileNode.Meta["usings"] = usings
	}
	if len(globals) > 0 {
		fileNode.Meta["global_usings"] = globals
	}
	if len(statics) > 0 {
		fileNode.Meta["using_static"] = statics
	}
	if len(scoped) > 0 {
		fileNode.Meta["scoped_usings"] = scoped
	}
	if len(globalStatics) > 0 {
		fileNode.Meta["global_using_static"] = globalStatics
	}
}

func (e *CSharpExtractor) emitUsing(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	path := m.Captures["using.path"]
	importPath := strings.ReplaceAll(path.Text, ".", "/")
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// csharpFuncLookup answers "which function owns this line" by binary
// search instead of the linear scan findEnclosingFunc pays per query —
// the extractor asks once per local, call and type use, an
// O(locals×functions) product on member-heavy files. Ranges are sorted
// by start line with a running max-end, so a stabbing query walks back
// only while an overlap is still possible; overlapping ranges (a local
// function inside a method) pick the innermost.
type csharpFuncLookup struct {
	ranges []funcRange
	maxEnd []int
	ord    []int // original extraction order — the deterministic tie-break
}

func newCSharpFuncLookup(ranges []funcRange) *csharpFuncLookup {
	sorted := append([]funcRange(nil), ranges...)
	ord := make([]int, len(sorted))
	for i := range ord {
		ord[i] = i
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].startLine < sorted[j].startLine })
	// SliceStable keeps equal start lines in extraction order, so ord
	// still needs the same permutation applied.
	sort.SliceStable(ord, func(i, j int) bool { return ranges[ord[i]].startLine < ranges[ord[j]].startLine })
	maxEnd := make([]int, len(sorted))
	running := 0
	for i, r := range sorted {
		if r.endLine > running {
			running = r.endLine
		}
		maxEnd[i] = running
	}
	return &csharpFuncLookup{ranges: sorted, maxEnd: maxEnd, ord: ord}
}

func (l *csharpFuncLookup) enclosing(line int) string {
	i := sort.Search(len(l.ranges), func(j int) bool { return l.ranges[j].startLine > line }) - 1
	best := ""
	bestSpan := math.MaxInt
	bestOrd := math.MaxInt
	for ; i >= 0; i-- {
		if l.maxEnd[i] < line {
			break
		}
		if r := l.ranges[i]; line <= r.endLine {
			span := r.endLine - r.startLine
			// Innermost wins; identical ranges (expression-bodied
			// members sharing a line) go to the first extracted.
			if span < bestSpan || (span == bestSpan && l.ord[i] < bestOrd) {
				best, bestSpan, bestOrd = r.id, span, l.ord[i]
			}
		}
	}
	return best
}

type csharpOwner struct {
	kind string // class_declaration / struct_declaration / interface_declaration
	name string
}

// csharpDirectMemberOwner mirrors the legacy nested queries: the
// member must be a direct child of the container's declaration_list.
// Returns kind == "" when the member isn't directly inside one of the
// allowed container kinds (skipping nested types, top-level statements,
// etc. — none of which the legacy extractor handled).
func csharpDirectMemberOwner(member *sitter.Node, src []byte, allowed ...string) csharpOwner {
	if member == nil {
		return csharpOwner{}
	}
	parent := member.Parent()
	if parent == nil || parent.Type() != "declaration_list" {
		return csharpOwner{}
	}
	grand := parent.Parent()
	if grand == nil {
		return csharpOwner{}
	}
	gtype := grand.Type()
	for _, a := range allowed {
		if gtype == a {
			nameNode := grand.ChildByFieldName("name")
			if nameNode == nil {
				return csharpOwner{}
			}
			return csharpOwner{kind: gtype, name: nameNode.Content(src)}
		}
	}
	return csharpOwner{}
}

// collectCSharpInterfaceNames walks the tree for every
// interface_declaration and records its bare name. The base-list
// heuristic consults this set first: a base type that names a
// locally-declared interface is unambiguously an interface, regardless
// of whether its name follows the `I`-prefix convention.
func collectCSharpInterfaceNames(root *sitter.Node, src []byte) map[string]bool {
	names := make(map[string]bool)
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "interface_declaration" {
			return
		}
		if nameNode := n.ChildByFieldName("name"); nameNode != nil {
			names[nameNode.Content(src)] = true
		}
	})
	return names
}

// emitCSharpBaseList splits a class/struct/record base list into
// EdgeExtends (the superclass) and EdgeImplements (the interfaces).
// An interface's base list bypasses that split entirely — see the
// short-circuit below.
//
// C# lists the optional base class and any implemented interfaces in a
// single comma-separated base_list, and — unlike Go or Java — the
// grammar does not tag which entry is the class. When a base type is
// defined elsewhere (another compilation unit) the extractor cannot
// resolve its kind, so it discriminates with a heuristic:
//
//  1. A base whose name matches a locally-declared interface (the
//     prescan set) is definitively an interface → EdgeImplements.
//  2. Otherwise a base whose name matches the `I`-prefix convention
//     (^I[A-Z], generics stripped first so IList<T> → IList) is treated
//     as an interface → EdgeImplements.
//  3. The first base that is neither is the superclass → EdgeExtends.
//     C# allows at most one base class and it must come first; every
//     base after it is an interface. Structs and `record struct`
//     declarations have no base class, so all of their bases are
//     interfaces regardless of position.
//
// All edges ride at OriginASTInferred: the discrimination is a
// heuristic, not a type-checked fact. Targets are left unresolved so
// the resolver binds them like every other C# reference. A base that
// resolves to a same-file class still flows through unchanged — it is
// neither a known interface nor I-prefixed, so it lands as EdgeExtends.
func emitCSharpBaseList(typeID string, decl *sitter.Node, src []byte, filePath string, localInterfaces map[string]bool, result *parser.ExtractionResult) {
	if decl == nil {
		return
	}
	baseList := decl.ChildByFieldName("bases")
	if baseList == nil {
		// `bases` is not a named field in every grammar revision; fall
		// back to a direct child scan for the base_list node.
		for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
			c := decl.Child(i)
			if c != nil && c.Type() == "base_list" {
				baseList = c
				break
			}
		}
	}
	if baseList == nil {
		return
	}
	// Structs and `record struct` cannot derive from a base class — the
	// CLR forbids it — so every entry in their base list is an interface
	// and the "first non-interface is the superclass" branch never runs.
	// An interface's bases can only be interfaces, and the relation is
	// inheritance — every entry rides EdgeExtends (the same convention
	// the semantic engine applies), bypassing the discrimination below.
	ifaceDecl := decl.Type() == "interface_declaration"
	allowsBaseClass := csharpDeclAllowsBaseClass(decl)
	extendsTaken := false
	for i, _nc := 0, int(baseList.NamedChildCount()); i < _nc; i++ {
		entry := baseList.NamedChild(i)
		if entry == nil {
			continue
		}
		name, isCtorBase := csharpBaseTypeName(entry, src)
		if name == "" {
			continue
		}
		line := int(entry.StartPoint().Row) + 1
		// A primary_constructor_base_type (`: Base(args)`) invokes a base
		// constructor, which is only valid for a base class — it is never
		// an interface.
		isInterface := !isCtorBase &&
			(localInterfaces[name] || csharpInterfaceNamePattern.MatchString(name))
		kind := graph.EdgeImplements
		switch {
		case ifaceDecl:
			kind = graph.EdgeExtends
		case !isInterface && allowsBaseClass && !extendsTaken:
			kind = graph.EdgeExtends
			extendsTaken = true
		}
		edge := &graph.Edge{
			From: typeID, To: "unresolved::" + name,
			Kind: kind, FilePath: filePath, Line: line,
			Origin: graph.OriginASTInferred,
		}
		// A qualified base spelling names its namespace — keep it for the
		// resolver's namespace narrowing (same stamp as reference forms).
		raw := entry.Content(src)
		if isCtorBase {
			if tn := entry.ChildByFieldName("type"); tn != nil {
				raw = tn.Content(src)
			}
		}
		if fqn := csharpQualifiedTypeRef(raw); fqn != "" {
			edge.Meta = map[string]any{"target_fqn": fqn}
		}
		result.Edges = append(result.Edges, edge)
	}
}

// csharpQualifiedReceiverType resolves the type a `this.` or `base.`
// call qualifier names: the innermost enclosing type declaration for
// `this`, its declared base class for `base`. The keywords are anonymous
// tokens in the grammar — a plain `(_)` receiver capture never matches
// them — so the type must be recovered from the declaration instead of a
// tenv. Base discrimination mirrors emitCSharpBaseList: the first
// base_list entry that is a ctor-base or not I-prefixed is the
// superclass. Empty when nothing applies (struct `base.`, interface-only
// bases) — the call still emits as a plain member_call.
func csharpQualifiedReceiverType(node *sitter.Node, src []byte, keyword string) string {
	var decl *sitter.Node
	for n := node; n != nil && decl == nil; n = n.Parent() {
		switch n.Type() {
		case "class_declaration", "struct_declaration", "record_declaration", "interface_declaration":
			decl = n
		}
	}
	if decl == nil {
		return ""
	}
	if keyword == "this" {
		if nm := decl.ChildByFieldName("name"); nm != nil {
			return nm.Content(src)
		}
		return ""
	}
	if !csharpDeclAllowsBaseClass(decl) {
		return ""
	}
	baseList := decl.ChildByFieldName("bases")
	if baseList == nil {
		for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
			if c := decl.Child(i); c != nil && c.Type() == "base_list" {
				baseList = c
				break
			}
		}
	}
	if baseList == nil {
		return ""
	}
	for i, _nc := 0, int(baseList.NamedChildCount()); i < _nc; i++ {
		entry := baseList.NamedChild(i)
		if entry == nil {
			continue
		}
		name, isCtorBase := csharpBaseTypeName(entry, src)
		if name == "" {
			continue
		}
		if isCtorBase || !csharpInterfaceNamePattern.MatchString(name) {
			return name
		}
	}
	return ""
}

// csharpDeclAllowsBaseClass reports whether a class/struct/record
// declaration can have a base class. Structs never can; a record is a
// struct when its declaration carries the `struct` keyword
// (`record struct`), otherwise it is a reference type that can extend a
// base record/class.
func csharpDeclAllowsBaseClass(decl *sitter.Node) bool {
	switch decl.Type() {
	case "struct_declaration":
		return false
	case "record_declaration":
		for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
			if c := decl.Child(i); c != nil && c.Type() == "struct" {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// csharpBaseTypeName extracts the bare type name from a single base_list
// entry, stripping generic arguments and namespace qualification so the
// `I`-prefix test sees IList rather than IList<int> or System.IList. The
// bool return reports whether the entry is a primary_constructor_base_type
// (`Base(args)`), which can only ever be a base class.
func csharpBaseTypeName(entry *sitter.Node, src []byte) (string, bool) {
	switch entry.Type() {
	case "identifier":
		return entry.Content(src), false
	case "generic_name":
		// First child is the base identifier; the type_argument_list
		// follows. IList<int> → IList.
		if id := entry.ChildByFieldName("name"); id != nil {
			return id.Content(src), false
		}
		for i, _nc := 0, int(entry.ChildCount()); i < _nc; i++ {
			if c := entry.Child(i); c != nil && c.Type() == "identifier" {
				return c.Content(src), false
			}
		}
	case "qualified_name":
		// System.Object → Object (the last identifier).
		var last string
		for i, _nc := 0, int(entry.ChildCount()); i < _nc; i++ {
			if c := entry.Child(i); c != nil && c.Type() == "identifier" {
				last = c.Content(src)
			}
		}
		return last, false
	case "primary_constructor_base_type":
		// `: Base(args)` — record base-constructor call; always a class.
		if id := entry.ChildByFieldName("type"); id != nil {
			return normalizeCSharpBaseName(id.Content(src)), true
		}
		for i, _nc := 0, int(entry.ChildCount()); i < _nc; i++ {
			c := entry.Child(i)
			if c == nil {
				continue
			}
			if c.Type() == "identifier" || c.Type() == "generic_name" || c.Type() == "qualified_name" {
				return normalizeCSharpBaseName(c.Content(src)), true
			}
		}
	}
	return "", false
}

// normalizeCSharpBaseName reduces a raw base-type spelling to its bare
// simple name: drops generic arguments (Foo<T> → Foo) and namespace
// qualification (A.B.Foo → Foo).
func normalizeCSharpBaseName(raw string) string {
	raw = strings.TrimSpace(raw)
	if idx := strings.Index(raw, "<"); idx > 0 {
		raw = raw[:idx]
	}
	if idx := strings.LastIndex(raw, "."); idx >= 0 {
		raw = raw[idx+1:]
	}
	return strings.TrimSpace(raw)
}

// csharpCanonTypeShape collapses a raw type spelling to its canonical
// shape — whitespace dropped, structure (generic arguments, array /
// nullable suffixes, qualification) kept verbatim. "" for empty input
// and for the `var` placeholder (no declared shape).
func csharpCanonTypeShape(raw string) string {
	s := strings.Join(strings.Fields(raw), "")
	if s == "" || s == "var" {
		return ""
	}
	return s
}

// csharpTypeParamConstraints returns the core names of the type
// constraints declared for the given method type parameter (`where T :
// ITagged, IOther` → [ITagged, IOther]). Primary-kind constraints
// (class / struct / notnull / unmanaged / new()) carry no type identity
// and are skipped.
func csharpTypeParamConstraints(methodNode *sitter.Node, src []byte, param string) []string {
	if methodNode == nil {
		return nil
	}
	var out []string
	for i, _nc := 0, int(methodNode.NamedChildCount()); i < _nc; i++ {
		clause := methodNode.NamedChild(i)
		if clause == nil || clause.Type() != "type_parameter_constraints_clause" {
			continue
		}
		target := ""
		if n := clause.ChildByFieldName("target"); n != nil {
			target = strings.TrimSpace(n.Content(src))
		} else {
			for j, _jc := 0, int(clause.NamedChildCount()); j < _jc; j++ {
				c := clause.NamedChild(j)
				if c != nil && c.Type() == "identifier" {
					target = strings.TrimSpace(c.Content(src))
					break
				}
			}
		}
		if target != param {
			continue
		}
		walkNodes(clause, func(n *sitter.Node) {
			if n.Type() != "type_parameter_constraint" {
				return
			}
			txt := strings.TrimSpace(n.Content(src))
			switch txt {
			case "class", "class?", "struct", "notnull", "unmanaged":
				return
			}
			if strings.Contains(txt, "(") { // new()
				return
			}
			if name := normalizeCSharpBaseName(txt); name != "" && name != param {
				out = append(out, name)
			}
		})
	}
	return out
}

// extractCSharpMethodReturnType walks a method_declaration node for
// the type child preceding the method name. Returns the normalized name
// plus the raw declared shape — normalization drops generic arguments,
// and for a Task<T> the argument is exactly what an await evaluates to.
func extractCSharpMethodReturnType(methodNode *sitter.Node, src []byte, methodName string) (string, string) {
	if methodNode == nil {
		return "", ""
	}
	for i, _nc := 0, int(methodNode.ChildCount()); i < _nc; i++ {
		child := methodNode.Child(i)
		if child.Type() == "identifier" && string(src[child.StartByte():child.EndByte()]) == methodName {
			break
		}
		switch child.Type() {
		case "predefined_type", "identifier", "qualified_name", "generic_name",
			"nullable_type", "array_type", "tuple_type":
			rawType := string(src[child.StartByte():child.EndByte()])
			if rt := normalizeCSharpTypeName(rawType); rt != "" && rt != "var" {
				return rt, strings.TrimSpace(rawType)
			}
		}
	}
	return "", ""
}

// csharpAwaitedReceiver returns the awaited expression inside a receiver
// that is exactly one parenthesized await group — `(await LoadAsync(id))`
// → `LoadAsync(id)` — and "" for every other receiver shape.
func csharpAwaitedReceiver(recv string) string {
	s := strings.TrimSpace(recv)
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return ""
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return "" // opening paren closes early — not one group
			}
		}
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if rest := strings.TrimPrefix(inner, "await "); rest != inner {
		return strings.TrimSpace(rest)
	}
	return ""
}

// csharpAwaitedCallType resolves the type an awaited call expression
// evaluates to: the called method's declared return shape with the
// Task<>/ValueTask<> wrapper stripped. A chained call types its receiver
// prefix through the shared chain walker first; the final hop reads the
// lossless return_shape because the normalized return_type has already
// dropped the generic argument — which is exactly the awaited T.
// enclosing is the caller's own type: it anchors unqualified and
// this-qualified calls so a same-named method on an unrelated sibling
// class never leaks its return shape in.
func csharpAwaitedCallType(expr, enclosing string, tenv typeEnv, result *parser.ExtractionResult) string {
	expr = strings.TrimSpace(expr)
	// Split the final segment off at the last depth-0 dot; the raw prefix
	// text keeps its call parens so factory-chain seeding still works.
	depth, cut := 0, -1
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '.':
			if depth == 0 {
				cut = i
			}
		}
	}
	if cut < 0 {
		name := stripCallArgs(expr)
		return csharpUnwrapTaskType(csharpCallableReturnShape("", enclosing, name, result))
	}
	name := stripCallArgs(expr[cut+1:])
	var recvType string
	if prefix := expr[:cut]; prefix == "this" {
		recvType = enclosing
	} else {
		recvType = resolveChainType(prefix, tenv, result)
		if recvType == "" && isKnownType(prefix, result) {
			recvType = prefix // static call on a type: `Repo.LoadAsync()`
		}
	}
	if name == "" || recvType == "" {
		return ""
	}
	return csharpUnwrapTaskType(csharpCallableReturnShape(recvType, "", name, result))
}

// csharpCallableReturnShape finds the declared return shape of a callable
// named name. With a receiverType, only an exact receiver match counts.
// Without one (an unqualified call inside a method body) the enclosing
// class's own member wins, then a receiver-less free function — never an
// arbitrary same-named method off an unrelated class, whose shape would
// ride into receiver_type as a confident wrong answer.
func csharpCallableReturnShape(receiverType, enclosing, name string, result *parser.ExtractionResult) string {
	var free string
	for _, n := range result.Nodes {
		if n == nil || (n.Kind != graph.KindMethod && n.Kind != graph.KindFunction) || n.Name != name {
			continue
		}
		shape, _ := n.Meta["return_shape"].(string)
		if shape == "" {
			shape, _ = n.Meta["return_type"].(string)
		}
		if shape == "" {
			continue
		}
		recv, _ := n.Meta["receiver"].(string)
		if receiverType != "" {
			if recv == receiverType {
				return shape
			}
			continue
		}
		if enclosing != "" && recv == enclosing {
			return shape
		}
		if recv == "" && free == "" {
			free = shape
		}
	}
	return free
}

// csharpOwnerTypeName extracts the enclosing type's simple name from a
// symbol owner ID ("file.cs::Outer.Inner.Method" → "Inner"); "" for a
// top-level function.
func csharpOwnerTypeName(ownerID string) string {
	idx := strings.LastIndex(ownerID, "::")
	if idx < 0 {
		return ""
	}
	parts := strings.Split(ownerID[idx+2:], ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

// csharpUnwrapTaskType strips the Task<>/ValueTask<> wrapper from a return
// shape and returns the normalized result type — what an await on that
// call evaluates to. Anything else (bare Task, custom awaitables) yields
// "" rather than a guess.
func csharpUnwrapTaskType(shape string) string {
	s := strings.TrimSpace(shape)
	lt := strings.Index(s, "<")
	if lt <= 0 || !strings.HasSuffix(s, ">") {
		return ""
	}
	head := s[:lt]
	if dot := strings.LastIndex(head, "."); dot >= 0 {
		head = head[dot+1:]
	}
	if head != "Task" && head != "ValueTask" {
		return ""
	}
	if t := normalizeCSharpTypeName(s[lt+1 : len(s)-1]); t != "" && t != "var" {
		return t
	}
	return ""
}

// csharpMethodTypeParamNames returns the method's declared generic
// type-parameter names (the `<T, U>` list), for telling a generic
// this-param apart from a concrete type of the same spelling.
func csharpMethodTypeParamNames(methodNode *sitter.Node, src []byte) map[string]bool {
	if methodNode == nil {
		return nil
	}
	tparams := methodNode.ChildByFieldName("type_parameters")
	if tparams == nil {
		for i, _nc := 0, int(methodNode.NamedChildCount()); i < _nc; i++ {
			c := methodNode.NamedChild(i)
			if c != nil && c.Type() == "type_parameter_list" {
				tparams = c
				break
			}
		}
	}
	if tparams == nil {
		return nil
	}
	names := map[string]bool{}
	for i, _nc := 0, int(tparams.NamedChildCount()); i < _nc; i++ {
		tp := tparams.NamedChild(i)
		if tp == nil || tp.Type() != "type_parameter" {
			continue
		}
		for j, _jc := 0, int(tp.NamedChildCount()); j < _jc; j++ {
			c := tp.NamedChild(j)
			if c != nil && c.Type() == "identifier" {
				names[c.Content(src)] = true
				break
			}
		}
	}
	return names
}

// csharpBuiltinTypeName returns the C# builtin keyword named by a local
// declaration type, nullable/array suffixes stripped — "" when the type
// is not a builtin. normalizeCSharpTypeName deliberately drops these;
// this is the parallel lookup for the receiver_builtin stamp.
func csharpBuiltinTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Strip nullable/array suffixes to fixpoint — `int?[]` and `int[]?`
	// spell the same evidence, and the resolver's suffix trim agrees.
	for {
		trimmed := strings.TrimSuffix(t, "?")
		if idx := strings.Index(trimmed, "["); idx > 0 {
			trimmed = trimmed[:idx]
		}
		if trimmed == t {
			break
		}
		t = trimmed
	}
	switch t {
	case "int", "long", "short", "byte", "sbyte", "uint", "ulong", "ushort",
		"float", "double", "decimal", "bool", "char", "string", "object":
		return t
	}
	return ""
}

// normalizeCSharpTypeName strips generics and nullable markers from a C# type name.
func normalizeCSharpTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Remove nullable suffix.
	t = strings.TrimSuffix(t, "?")
	// Remove array suffix.
	if idx := strings.Index(t, "["); idx > 0 {
		t = t[:idx]
	}
	// Remove generics.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Skip C# primitives and keywords.
	switch t {
	case "var", "int", "long", "short", "byte", "float", "double", "decimal",
		"bool", "char", "string", "object", "void", "dynamic":
		if t == "var" {
			return "var" // caller handles this specially
		}
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return ""
	}
	return t
}

// csharpFieldDeclType returns the verbatim declared type of a
// field_declaration. The type is a field of the nested
// variable_declaration node, not of the field_declaration itself, so a
// direct ChildByFieldName("type") on the field_declaration is always nil.
func csharpFieldDeclType(fieldDecl *sitter.Node, src []byte) string {
	if fieldDecl == nil {
		return ""
	}
	for i, _nc := 0, int(fieldDecl.NamedChildCount()); i < _nc; i++ {
		c := fieldDecl.NamedChild(i)
		if c == nil || c.Type() != "variable_declaration" {
			continue
		}
		if t := c.ChildByFieldName("type"); t != nil {
			return strings.TrimSpace(t.Content(src))
		}
		// Fallback: first named child of the variable_declaration is the
		// type in grammar revisions that don't tag the field.
		if c.NamedChildCount() > 0 {
			if first := c.NamedChild(0); first != nil && first.Type() != "variable_declarator" {
				return strings.TrimSpace(first.Content(src))
			}
		}
	}
	return ""
}

// inferTypeFromCSharpNew extracts the type name from a C# object_creation_expression.
// new UserService(...) -> "UserService"
func inferTypeFromCSharpNew(node *sitter.Node, src []byte) string {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		if child.Type() == "identifier" || child.Type() == "type_identifier" ||
			child.Type() == "generic_name" || child.Type() == "qualified_name" {
			name := child.Content(src)
			// Strip generics from generic_name.
			if idx := strings.Index(name, "<"); idx > 0 {
				name = name[:idx]
			}
			if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		}
	}
	return ""
}
