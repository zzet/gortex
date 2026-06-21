package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/tsx"
	"github.com/zzet/gortex/internal/parser/tsitter/typescript"
)

// tsQAll is one tree-sitter query with 13 alternation patterns covering
// everything the TypeScript extractor needs at the root of a file. One
// query = one tree walk per Extract call, down from 14 walks in the
// per-query-per-call design. Each pattern uses a disjoint set of
// capture names so the dispatch below can branch on which name is set.
//
// method_definition and public_field_definition are captured at file
// scope here (hoisted out of per-class subqueries); the dispatcher
// walks up from the matched node to find the enclosing class.
const tsQAll = `
[
  (function_declaration
    name: (identifier) @func.name) @func.def

  (lexical_declaration
    (variable_declarator
      name: (identifier) @arrow.name
      value: (arrow_function) @arrow.body)) @arrow.def

  (pair
    key: (property_identifier) @objfn.name
    value: (arrow_function) @objfn.body) @objfn.def

  (class_declaration
    name: (type_identifier) @class.name) @class.def

  (interface_declaration
    name: (type_identifier) @iface.name) @iface.def

  (type_alias_declaration
    name: (type_identifier) @type.name) @type.def

  (enum_declaration
    name: (identifier) @enum.name) @enum.def

  (import_statement
    source: (string) @import.path) @import.def

  (export_statement
    source: (string) @reexport.path) @reexport.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (member_expression
      object: (_) @callm.receiver
      property: (property_identifier) @callm.method)) @callm.expr

  (lexical_declaration
    (variable_declarator
      name: (identifier) @tvar.name
      type: (type_annotation (_) @tvar.type))) @tvar.def

  (lexical_declaration
    (variable_declarator
      name: (identifier) @var.name)) @var.def

  (method_definition
    name: (property_identifier) @method.name) @method.def

  (public_field_definition
    name: (property_identifier) @prop.name) @prop.def
]
`

// TypeScriptExtractor extracts TypeScript/JavaScript source files.
//
// Two grammars share one extractor: the plain `typescript` grammar
// (used for .ts) and the `tsx` grammar (used for .tsx). Both expose
// the same node types for the patterns this extractor cares about
// (function_declaration / method_definition / class_declaration /
// arrow_function / etc.); TSX additionally exposes JSX nodes
// (jsx_element, jsx_self_closing_element) that the EdgeRendersChild
// walker matches against. We can't merge into one grammar because
// TS and TSX disagree on `<T>x` (TSX treats it as a JSX opening element,
// TS as a type assertion); per-extension dispatch keeps both honest.
type TypeScriptExtractor struct {
	lang    *sitter.Language
	qAll    *parser.PreparedQuery
	tsxLang *sitter.Language
	tsxQAll *parser.PreparedQuery
}

func NewTypeScriptExtractor() *TypeScriptExtractor {
	lang := typescript.GetLanguage()
	tsxLang := tsx.GetLanguage()
	return &TypeScriptExtractor{
		lang:    lang,
		qAll:    parser.MustPreparedQuery(tsQAll, lang),
		tsxLang: tsxLang,
		tsxQAll: parser.MustPreparedQuery(tsQAll, tsxLang),
	}
}

func (e *TypeScriptExtractor) Language() string { return "typescript" }

// .mts (ES module TS) and .cts (CommonJS TS) parse with the plain TS
// grammar; grammarFor only selects TSX for the .tsx suffix.
func (e *TypeScriptExtractor) Extensions() []string {
	return []string{".ts", ".tsx", ".mts", ".cts"}
}

// grammarFor returns the (language, prepared-query) pair appropriate
// for filePath. .tsx files use the TSX grammar so JSX nodes
// (jsx_element, jsx_self_closing_element) appear in the parse tree
// for the EdgeRendersChild walker; .ts files use the plain TS
// grammar to preserve the `<T>x` type-assertion semantics that the
// TSX grammar would mis-parse as a JSX opening element.
func (e *TypeScriptExtractor) grammarFor(filePath string) (*sitter.Language, *parser.PreparedQuery) {
	if strings.HasSuffix(strings.ToLower(filePath), ".tsx") {
		return e.tsxLang, e.tsxQAll
	}
	return e.lang, e.qAll
}

// deferredCall holds a call_expression match whose edge can only be
// emitted after every function/method node exists (so funcRanges can
// attribute the call to its enclosing definition) and after the type
// env is fully populated.
type deferredCall struct {
	name     string // identifier callee name (or "" for member calls)
	method   string // method name for member calls
	receiver string // receiver text for member calls
	line     int    // 1-based line of the call_expression
	isMember bool
	// expr is the call_expression node, kept for member calls so the
	// post-pass can inspect arguments for pub/sub topic detection.
	expr *sitter.Node
	// returnUsage is how the call site consumes the return value
	// (graph.ReturnUsage* label), classified at capture time and
	// stamped as edge Meta on every EdgeCalls emitted for this site.
	returnUsage string
}

// deferredVar holds a lexical_declaration match whose emission is
// delayed until arrow-function names for the whole file are known
// (otherwise a `const foo = () => {}` would emit both a function and a
// variable node for foo).
type deferredVar struct {
	name    string
	defNode *sitter.Node
	startLn int // 0-based
	endLn   int // 0-based
}

// deferredTypeUse holds a variable / const type annotation whose
// EdgeTypedAs is emitted after funcRanges is built, so the reference is
// attributed to its enclosing function (falling back to the file node).
type deferredTypeUse struct {
	typeText string
	line     int // 1-based
}

func (e *TypeScriptExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	lang, qAll := e.grammarFor(filePath)
	tree, err := parser.ParseFile(src, lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "typescript",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	imports := map[string]string{}
	arrowNames := map[string]bool{}
	tenv := make(typeEnv)
	annotationSeen := map[string]bool{}

	var calls []deferredCall
	var vars []deferredVar
	var typeUses []deferredTypeUse
	// objLiteralMembers maps a top-level object-literal owner name to the
	// member-function node IDs declared inside it — both method shorthand
	// (`{ process() {...} }`) and arrow fields (`{ health: () => ... }`).
	// A later `owner.method()` member call resolves directly to the
	// registered node instead of falling through to name-only resolution,
	// which would otherwise mis-bind to an unrelated free function of the
	// same name elsewhere in the repo.
	objLiteralMembers := map[string]map[string]string{}
	registerObjMember := func(owner, member, id string) {
		if owner == "" || member == "" || id == "" {
			return
		}
		set := objLiteralMembers[owner]
		if set == nil {
			set = map[string]string{}
			objLiteralMembers[owner] = set
		}
		set[member] = id
	}
	// importPaths collects every imported module path — including named
	// imports, which emitImport's alias map intentionally skips — so the
	// post-pass can disambiguate generic pub/sub method names and infer
	// the broker transport.
	var importPaths []string
	// classCarries accumulates (class_declaration node, classID) pairs
	// as they're matched, so the post-pass can run a single
	// walkClassMembers per class — covering @Inject consumer edges,
	// NestJS dynamic-module factory providers, and `this.<field>`
	// tenv seeding all in one walk. Replaces three separate walkNodes
	// passes (emitInjectConsumers, emitDynamicModuleBindings,
	// collectThisParamTypesInClass).
	var classCarries []classCarry
	// seenIDs: per-file node-ID collision set. Overload sets (multiple
	// `function foo` signatures, overloaded class methods) build the same
	// base ID; the disambiguator keeps each as a distinct node instead of
	// letting the graph silently overwrite all but one.
	seenIDs := map[string]bool{}

	parser.EachMatch(qAll, root, src, func(m parser.QueryResult) {
		switch {
		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seenIDs)

		case m.Captures["arrow.def"] != nil:
			if name := e.emitArrow(m, filePath, fileID, src, result, seenIDs); name != "" {
				arrowNames[name] = true
			}

		case m.Captures["objfn.def"] != nil:
			registerObjMember(e.emitArrowField(m, filePath, fileID, src, result))

		case m.Captures["class.def"] != nil:
			classID := e.emitClass(m, filePath, fileID, src, result)
			def := m.Captures["class.def"]
			if def.Node != nil && classID != "" {
				classCarries = append(classCarries, classCarry{node: def.Node, id: classID})
				emitTSAnnotationEdges(classDecorators(def.Node), classID, filePath, src, result, annotationSeen)
			}

		case m.Captures["iface.def"] != nil:
			e.emitInterface(m, filePath, fileID, src, result)

		case m.Captures["type.def"] != nil:
			e.emitTypeAlias(m, filePath, fileID, src, result)

		case m.Captures["enum.def"] != nil:
			e.emitEnum(m, filePath, fileID, src, result)

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, src, result, imports)
			if p := m.Captures["import.path"]; p != nil {
				importPaths = append(importPaths, strings.Trim(p.Text, "\"'`"))
			}

		case m.Captures["reexport.def"] != nil:
			e.emitReExport(m, filePath, fileID, src, result)

		case m.Captures["method.def"] != nil:
			registerObjMember(e.emitMethod(m, filePath, src, result, annotationSeen, seenIDs))

		case m.Captures["prop.def"] != nil:
			e.emitClassProperty(m, filePath, src, result, annotationSeen)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, deferredCall{
				name:        m.Captures["call.name"].Text,
				line:        expr.StartLine + 1,
				returnUsage: classifyReturnUsage(expr.Node, src, jsTSReturnUsageSpec),
			})

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, deferredCall{
				method:      m.Captures["callm.method"].Text,
				receiver:    m.Captures["callm.receiver"].Text,
				line:        expr.StartLine + 1,
				isMember:    true,
				expr:        expr.Node,
				returnUsage: classifyReturnUsage(expr.Node, src, jsTSReturnUsageSpec),
			})

		case m.Captures["tvar.def"] != nil:
			name := m.Captures["tvar.name"].Text
			rawType := m.Captures["tvar.type"].Text
			if tn := normalizeTypeName(rawType); tn != "" {
				tenv[name] = tn
			}
			typeUses = append(typeUses, deferredTypeUse{
				typeText: rawType,
				line:     m.Captures["tvar.def"].StartLine + 1,
			})

		case m.Captures["var.def"] != nil:
			def := m.Captures["var.def"]
			vars = append(vars, deferredVar{
				name:    m.Captures["var.name"].Text,
				defNode: def.Node,
				startLn: def.StartLine,
				endLn:   def.EndLine,
			})
		}
	})

	// Tier 0b: single per-class walk dispatching three concerns at
	// once — @Inject consumer edges, NestJS dynamic-module factory
	// providers (forRoot / forFeature / register / *Async), and
	// `this.<field>` tenv seeding from constructor parameter-property
	// shorthand or class field annotations / inject() initializers.
	// Folding the three previous walks into one cuts per-class
	// walkNodes cost by ~3× on NestJS-style class-heavy files.
	for _, cc := range classCarries {
		walkClassMembers(cc.node, src, cc.id, filePath, result, tenv)
	}

	// Tier 1: type inference from `new Expr()` initializers in plain
	// variable declarations. Runs against the already-buffered var
	// matches so we don't re-query the tree.
	for _, v := range vars {
		if _, seen := tenv[v.name]; seen {
			continue
		}
		if v.defNode == nil {
			continue
		}
		walkNodes(v.defNode, func(n *sitter.Node) {
			if n.Type() != "variable_declarator" {
				return
			}
			for i := 0; i < int(n.NamedChildCount()); i++ {
				child := n.NamedChild(i)
				if child.Type() == "new_expression" {
					if tn := inferTypeFromNewExpr(child, src); tn != "" {
						tenv[v.name] = tn
					}
					return
				}
			}
		})
	}

	// Now every function/method node is in result; build the line
	// range map used to attribute calls to their caller.
	funcRanges := buildFuncRanges(result)

	// Type-use edges: a `const x: T` / `let x: T` annotation references
	// type T. Attributed to the enclosing function (fallback: the file
	// node) so find_usages(T) surfaces every annotation site without an
	// LSP. EdgeTypedAs mirrors the param / return type edges; the
	// resolver lands it cross-file via the same name-based pass.
	for _, tu := range typeUses {
		ownerID := findEnclosingFunc(funcRanges, tu.line)
		if ownerID == "" {
			ownerID = fileID
		}
		emitTSTypeUseEdges(ownerID, tu.typeText, filePath, tu.line, result)
	}

	// Cast / assertion type references: `x as Foo`, `x as Foo[]`,
	// `x as NonDeleted<Foo>`, `x satisfies Foo`, and (plain .ts only)
	// the angle-bracket assertion `<Foo>x`. Each names the type(s) it
	// asserts, so find_usages(Foo) should surface the cast site. The
	// angle-bracket form is intentionally skipped on .tsx, where the
	// TSX grammar parses `<Foo>` as a JSX opening element, not a type
	// assertion. Attributed to the enclosing function (fallback: file).
	emitTSCastTypeRefs(root, src, filePath, fileID, funcRanges, result)

	// Instantiation edges: `new Foo(...)` constructs Foo. Emitted as a typed
	// EdgeInstantiates (not a flat call) attributed to the enclosing function,
	// so the resolver lands it on the class and impact/trace see construction.
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "new_expression" {
			return
		}
		ctor := n.ChildByFieldName("constructor")
		if ctor == nil {
			for i := 0; i < int(n.NamedChildCount()); i++ {
				c := n.NamedChild(i)
				if t := c.Type(); t == "identifier" || t == "member_expression" || t == "generic_type" {
					ctor = c
					break
				}
			}
		}
		if ctor == nil {
			return
		}
		tname := tsCtorTypeName(ctor, src)
		if tname == "" {
			return
		}
		line := int(n.StartPoint().Row) + 1
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + tname, Kind: graph.EdgeInstantiates,
			FilePath: filePath, Line: line, Origin: graph.OriginASTResolved,
		})
	})

	// Store-factory destructuring: `const {a,b} = useStore.getState()` binds
	// later bare `a()` / `b()` calls to the store's actions.
	destructured := map[string]string{} // local action name → store binding
	for _, dm := range jsDestructureGetStateRE.FindAllStringSubmatch(string(src), -1) {
		binding := dm[2]
		for _, nm := range strings.Split(dm[1], ",") {
			nm = strings.TrimSpace(nm)
			if i := strings.IndexByte(nm, ':'); i >= 0 {
				nm = strings.TrimSpace(nm[i+1:])
			}
			if nm != "" {
				destructured[nm] = binding
			}
		}
	}

	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		if !c.isMember {
			if binding, ok := destructured[c.name]; ok {
				if memberID := objLiteralMembers[binding][c.name]; memberID != "" {
					edge := &graph.Edge{
						From: callerID, To: memberID,
						Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
						Origin: graph.OriginASTResolved, Confidence: 0.9,
					}
					stampReturnUsage(edge, c.returnUsage)
					result.Edges = append(result.Edges, edge)
					continue
				}
				edge := &graph.Edge{
					From: callerID, To: "unresolved::" + c.name,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
					Meta: map[string]any{"via": "store-factory", "store_binding": binding, "store_action": c.name},
				}
				stampReturnUsage(edge, c.returnUsage)
				result.Edges = append(result.Edges, edge)
				continue
			}
			edge := &graph.Edge{
				From: callerID, To: "unresolved::" + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			stampReturnUsage(edge, c.returnUsage)
			result.Edges = append(result.Edges, edge)
			continue
		}
		// Object-literal member call (`api.process()` where
		// `const api = { process() {...} }` is in this file): bind
		// straight to the member-function node. Resolved at extraction —
		// the resolver never sees an `unresolved::` target, so a free
		// function also named `process` in another package can't capture
		// this edge in the name-only fallback.
		if members, ok := objLiteralMembers[c.receiver]; ok {
			if memberID, ok := members[c.method]; ok {
				edge := &graph.Edge{
					From: callerID, To: memberID,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
					Origin: graph.OriginASTResolved, Confidence: 0.92,
				}
				stampReturnUsage(edge, c.returnUsage)
				result.Edges = append(result.Edges, edge)
				continue
			}
		}
		// Namespace/default import receiver (e.g. `fs.readFile`): attach
		// the module path so the resolver can classify externally.
		if importPath, ok := imports[c.receiver]; ok {
			edge := &graph.Edge{
				From: callerID, To: "unresolved::extern::" + importPath + "::" + c.method,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			stampReturnUsage(edge, c.returnUsage)
			result.Edges = append(result.Edges, edge)
			continue
		}
		// Store-factory chained call: `useStore.getState().action()`.
		if binding, ok := jsParseGetStateChain(c.receiver); ok {
			if memberID := objLiteralMembers[binding][c.method]; memberID != "" {
				edge := &graph.Edge{
					From: callerID, To: memberID,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
					Origin: graph.OriginASTResolved, Confidence: 0.9,
				}
				stampReturnUsage(edge, c.returnUsage)
				result.Edges = append(result.Edges, edge)
				continue
			}
			edge := &graph.Edge{
				From: callerID, To: "unresolved::*." + c.method,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
				Meta: map[string]any{"via": "store-factory", "store_binding": binding, "store_action": c.method},
			}
			stampReturnUsage(edge, c.returnUsage)
			result.Edges = append(result.Edges, edge)
			continue
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + c.method,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		if recvType, ok := tenv[c.receiver]; ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
			if chainType := resolveChainType(c.receiver, tenv, result); chainType != "" {
				edge.Meta = map[string]any{"receiver_type": chainType}
			}
		}
		stampReturnUsage(edge, c.returnUsage)
		result.Edges = append(result.Edges, edge)
	}

	for _, v := range vars {
		if arrowNames[v.name] || v.defNode == nil {
			continue
		}
		parent := v.defNode.Parent()
		if parent != nil && parent.Type() == "export_statement" {
			parent = parent.Parent()
		}
		if parent == nil || parent.Type() != "program" {
			continue
		}
		id := filePath + "::" + v.name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: v.name,
			FilePath: filePath, StartLine: v.startLn + 1, EndLine: v.endLn + 1,
			Language: "typescript",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: v.startLn + 1,
		})
	}

	// --- Event pub/sub edges ---
	var pubsubEvents []pubsubEvent
	for _, c := range calls {
		if !c.isMember || c.expr == nil {
			continue
		}
		if ev, ok := detectJSPubsubCall(c.expr, c.method, src, importPaths, c.line); ok {
			pubsubEvents = append(pubsubEvents, ev)
		}
	}
	// WebSocket / EventSource real-time client channels.
	pubsubEvents = append(pubsubEvents, detectJSRealtimeEvents(src)...)
	emitPubsubEvents(pubsubEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, "typescript", result)

	// SQL function call sites (Supabase/PostgREST .rpc('fn')).
	emitSQLCallsiteEdges(src, "typescript",
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- React Native native-module bridge calls ---
	rnVars := rnNativeModuleVars(src)
	for _, c := range calls {
		if !c.isMember {
			continue
		}
		module := detectRNNativeModule(c.receiver, rnVars)
		if module == "" {
			continue
		}
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		edge := &graph.Edge{
			From: callerID, To: rnNativePlaceholder(module, c.method),
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			Meta: map[string]any{"via": rnNativeVia, "rn_module": module, "rn_method": c.method},
		}
		stampReturnUsage(edge, c.returnUsage)
		result.Edges = append(result.Edges, edge)
	}

	// --- React Native Fabric / Codegen component spec ---
	emitFabricComponentNodes(src, filePath, "typescript", fileID, result)

	// Test-runner classification (Mocha / Bun-test / Jest / Vitest /
	// node:test / Playwright / Cypress). Stamped on the file node so
	// the indexer's test-edge pass can propagate it to every is_test
	// function/method without re-reading the file.
	if runner := DetectJSTSTestRunner(filePath, src, importPaths); runner != "" {
		if fileNode.Meta == nil {
			fileNode.Meta = map[string]any{}
		}
		fileNode.Meta["test_runner"] = runner
	}

	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)
	return result, nil
}

// --- per-match emit helpers ------------------------------------------

func (e *TypeScriptExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seenIDs map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	id, ok := disambiguateID(seenIDs, filePath+"::"+name, def.StartLine+1)
	if !ok {
		return
	}
	meta := map[string]any{"signature": fmt.Sprintf("function %s()", name)}
	docRow, exported := tsDocStartRow(def)
	if doc := ExtractDocAbove(src, docRow, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	meta["visibility"] = tsTopLevelVisibility(exported)
	if tp := tsTypeParams(def.Node, src); len(tp) > 0 {
		meta["type_params"] = tp
	}
	if body := tsFunctionBody(def.Node); body != nil {
		c, cg, ld := BodyComplexityMetrics(body, "typescript")
		ApplyComplexityMeta(meta, c, cg, ld)
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	emitTSFunctionShape(id, def.Node, src, filePath, def.StartLine+1, result)
	emitTSThrowsEdges(def.Node, src, id, filePath, result)
	// JSX child-component attribution — emits EdgeRendersChild for
	// every uppercase-first-letter element rendered inside the body.
	if body := tsFunctionBody(def.Node); body != nil {
		emitJSXRenderEdges(id, body, src, filePath, result)
	}
}

// tsFunctionBody returns the statement_block child of a function or
// method declaration, or nil for abstract members.
func tsFunctionBody(decl *sitter.Node) *sitter.Node {
	if decl == nil {
		return nil
	}
	if b := decl.ChildByFieldName("body"); b != nil {
		return b
	}
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c != nil && (c.Type() == "statement_block" || c.Type() == "block") {
			return c
		}
	}
	return nil
}

func (e *TypeScriptExtractor) emitArrow(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seenIDs map[string]bool) string {
	name := m.Captures["arrow.name"].Text
	def := m.Captures["arrow.def"]
	id, ok := disambiguateID(seenIDs, filePath+"::"+name, def.StartLine+1)
	if !ok {
		return ""
	}
	meta := map[string]any{"signature": fmt.Sprintf("const %s = () =>", name)}
	docRow, exported := tsDocStartRow(def)
	if doc := ExtractDocAbove(src, docRow, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	meta["visibility"] = tsTopLevelVisibility(exported)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	// Function-shape (params/returns/generics) on the arrow's body.
	if body := m.Captures["arrow.body"]; body != nil && body.Node != nil {
		emitTSFunctionShape(id, body.Node, src, filePath, def.StartLine+1, result)
		emitJSXRenderEdges(id, body.Node, src, filePath, result)
	}
	return name
}

// emitArrowField handles the `pair → property_identifier → arrow_function`
// shape — `export const api = { health: async () => ... }`. Without
// this, calls inside the arrow body have no enclosing function for
// findEnclosingFunc to attribute them to, so EdgeCalls is silently
// dropped and downstream features (HTTP wrapper inlining, find_usages,
// etc.) lose every endpoint method.
//
// Naming: when the enclosing object is the value of a top-level
// `const owner = { ... }`, we qualify the function name as
// "owner.property" (e.g. "api.health"). Otherwise (inline object as
// argument or return value) we fall back to the bare property name.
// The graph ID always includes the start line so two same-named
// fields in different objects in the same file don't collide.
func (e *TypeScriptExtractor) emitArrowField(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) (ownerName, member, nodeID string) {
	prop := m.Captures["objfn.name"].Text
	def := m.Captures["objfn.def"]
	if prop == "" || def.Node == nil {
		return "", "", ""
	}
	// Walk up: pair → object → ... look for the nearest variable_declarator
	// or assignment whose name we can borrow.
	owner := tsArrowFieldOwner(def.Node, src)
	storeFactory := ""
	if owner == "" || jsIsStoreOptionKey(owner) {
		if b, _, ok := jsStoreFactoryBinding(def.Node, src); ok {
			owner, storeFactory = b, b
		}
	}
	name := prop
	if owner != "" {
		name = owner + "." + prop
	}
	// Disambiguate same-name fields in different objects within one
	// file by suffixing the start line. Cheap, deterministic, keeps
	// the human-readable name visible as Node.Name.
	id := fmt.Sprintf("%s::%s@%d", filePath, name, def.StartLine+1)
	meta := map[string]any{"signature": fmt.Sprintf("%s: () =>", name)}
	if storeFactory != "" {
		meta["store_factory"] = storeFactory
		meta["store_member"] = prop
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	// Function shape on the arrow body — covers NestJS controller
	// patterns (`@Controller class Foo { @Get() health = async () => …`)
	// and bare `export const api = { health: async (req) => ... }`
	// shapes whose params/returns/generics weren't surfaced before.
	if body := m.Captures["objfn.body"]; body != nil && body.Node != nil {
		emitTSFunctionShape(id, body.Node, src, filePath, def.StartLine+1, result)
		emitJSXRenderEdges(id, body.Node, src, filePath, result)
	} else {
		emitTSFunctionShape(id, def.Node, src, filePath, def.StartLine+1, result)
	}
	if owner == "" {
		return "", "", ""
	}
	return owner, prop, id
}

// tsArrowFieldOwner walks from a `pair` node up the AST looking for
// the most useful enclosing name to qualify the property with. Returns
// "" when no such name is reachable (e.g. an inline object passed as
// an argument). Stops as soon as it hits a program / class / function
// boundary that wouldn't usefully contribute a name.
func tsArrowFieldOwner(pair *sitter.Node, src []byte) string {
	if pair == nil {
		return ""
	}
	cur := pair.Parent()
	for cur != nil {
		switch cur.Type() {
		case "variable_declarator":
			// const owner = { ... }  →  owner is the first named child.
			if name := cur.ChildByFieldName("name"); name != nil && name.Type() == "identifier" {
				return name.Content(src)
			}
			return ""
		case "assignment_expression":
			// owner = { ... }
			if left := cur.ChildByFieldName("left"); left != nil && left.Type() == "identifier" {
				return left.Content(src)
			}
			return ""
		case "pair":
			// Nested object: walk further out, but use the immediate
			// key as the owner so nested fields stay disambiguated.
			if k := cur.ChildByFieldName("key"); k != nil && k.Type() == "property_identifier" {
				return k.Content(src)
			}
		case "program", "class_body", "function_declaration",
			"method_definition", "arrow_function", "function_expression":
			return ""
		}
		cur = cur.Parent()
	}
	return ""
}

// emitClass writes the class node + Defines edge and runs the
// shallow @Module(...) decorator scan. The deeper per-class walk
// (constructor / fields / static factories) is deferred to the
// post-pass walkClassMembers loop so all three concerns can share
// one walkNodes traversal.
func (e *TypeScriptExtractor) emitClass(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) string {
	name := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	id := filePath + "::" + name
	meta := map[string]any{}
	docRow, exported := tsDocStartRow(def)
	if doc := ExtractDocAbove(src, docRow, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	meta["visibility"] = tsTopLevelVisibility(exported)
	if tp := tsTypeParams(def.Node, src); len(tp) > 0 {
		meta["type_params"] = tp
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	if def.Node != nil {
		// @Module(...) lives on the class's direct decorator children
		// (a shallow scan, not a walkNodes), so handling it inline
		// keeps the @Module Provides edges grouped with the class node
		// they originate from. @Inject consumer edges and dynamic-
		// module factory providers are dispatched from walkClassMembers
		// in the post-pass.
		emitModuleBindings(def.Node, src, id, filePath, result)
		// TypeORM model attribution: @Entity decorator → EdgeModelsTable.
		detectTypeScriptORMModel(def.Node, src, id, name, filePath, result)
		// Generic <T extends Y> on the class declaration.
		emitTSGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
	}
	return id
}

func (e *TypeScriptExtractor) emitInterface(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	name := m.Captures["iface.name"].Text
	def := m.Captures["iface.def"]
	id := filePath + "::" + name
	var methods []string
	if def.Node != nil {
		methods = extractTSInterfaceMethods(def.Node, src)
	}
	meta := map[string]any{"methods": methods}
	docRow, exported := tsDocStartRow(def)
	if doc := ExtractDocAbove(src, docRow, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	meta["visibility"] = tsTopLevelVisibility(exported)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	if def.Node != nil {
		emitTSInterfaceMemberTypeRefs(def.Node, src, id, filePath, result)
	}
}

// emitTSInterfaceMemberTypeRefs emits a type-reference edge from an interface to
// every named type used as a property type or method return type — so the
// types an interface depends on are traversable (a contract's dependency
// surface), which name-only member extraction misses.
func emitTSInterfaceMemberTypeRefs(ifaceNode *sitter.Node, src []byte, ifaceID, filePath string, result *parser.ExtractionResult) {
	var body *sitter.Node
	for i := 0; i < int(ifaceNode.NamedChildCount()); i++ {
		c := ifaceNode.NamedChild(i)
		if c.Type() == "interface_body" || c.Type() == "object_type" {
			body = c
			break
		}
	}
	if body == nil {
		return
	}
	seen := map[string]bool{}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		member := body.NamedChild(i)
		ctx := ""
		switch member.Type() {
		case "property_signature":
			ctx = "field"
		case "method_signature":
			ctx = "return_type"
		default:
			continue
		}
		line := int(member.StartPoint().Row) + 1
		// Only the member's OWN type_annotation (property type / return type) —
		// parameter types live inside formal_parameters, a separate child.
		for j := 0; j < int(member.NamedChildCount()); j++ {
			ta := member.NamedChild(j)
			if ta.Type() != "type_annotation" {
				continue
			}
			walkNodes(ta, func(n *sitter.Node) {
				if n.Type() != "type_identifier" {
					return
				}
				tname := n.Content(src)
				if tname == "" || seen[tname] {
					return
				}
				seen[tname] = true
				result.Edges = append(result.Edges, &graph.Edge{
					From: ifaceID, To: "unresolved::" + tname, Kind: graph.EdgeReferences,
					FilePath: filePath, Line: line, Meta: map[string]any{"ref_context": ctx},
				})
			})
		}
	}
}

// tsCtorTypeName returns the constructed type name from a new_expression's
// constructor node (bare identifier, `ns.Foo` member access, or `Foo<T>`).
func tsCtorTypeName(ctor *sitter.Node, src []byte) string {
	switch ctor.Type() {
	case "identifier", "type_identifier":
		return ctor.Content(src)
	case "member_expression":
		if p := ctor.ChildByFieldName("property"); p != nil {
			return p.Content(src)
		}
	case "generic_type":
		for i := 0; i < int(ctor.NamedChildCount()); i++ {
			c := ctor.NamedChild(i)
			if t := c.Type(); t == "type_identifier" || t == "identifier" {
				return c.Content(src)
			}
		}
	}
	return ""
}

func (e *TypeScriptExtractor) emitTypeAlias(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	name := m.Captures["type.name"].Text
	def := m.Captures["type.def"]
	id := filePath + "::" + name
	meta := map[string]any{}
	docRow, exported := tsDocStartRow(def)
	if doc := ExtractDocAbove(src, docRow, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	meta["visibility"] = tsTopLevelVisibility(exported)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	// Alias-body type references: `type Bar = Foo | Baz` references Foo
	// and Baz; `type Qux = NonDeleted<Foo>` references NonDeleted and
	// Foo. Emit one EdgeTypedAs per named type on the RHS (the alias's
	// own name is never a self-edge — it's not in the value text), so
	// find_usages(Foo) surfaces the alias without an LSP.
	if def.Node != nil {
		if rhs := def.Node.ChildByFieldName("value"); rhs != nil {
			emitTSTypeRefEdges(id, strings.TrimSpace(rhs.Content(src)), filePath, def.StartLine+1, "type_annotation", result)
		}
	}
}

func (e *TypeScriptExtractor) emitEnum(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	name := m.Captures["enum.name"].Text
	def := m.Captures["enum.def"]
	id := filePath + "::" + name

	meta := map[string]any{"kind": "enum"}
	docRow, exported := tsDocStartRow(def)
	if doc := ExtractDocAbove(src, docRow, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	meta["visibility"] = tsTopLevelVisibility(exported)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})

	if def.Node == nil {
		return
	}
	// Walk the enum body for member names. The grammar yields enum_body
	// → (property_identifier | enum_assignment) children; handle both.
	for i := 0; i < int(def.Node.ChildCount()); i++ {
		child := def.Node.Child(i)
		if child == nil || child.Type() != "enum_body" {
			continue
		}
		for j := 0; j < int(child.ChildCount()); j++ {
			mem := child.Child(j)
			if mem == nil {
				continue
			}
			var memberName string
			var memberNode *sitter.Node
			switch mem.Type() {
			case "property_identifier":
				memberName = mem.Content(src)
				memberNode = mem
			case "enum_assignment":
				nameNode := mem.ChildByFieldName("name")
				if nameNode != nil {
					memberName = nameNode.Content(src)
					memberNode = mem
				}
			}
			if memberName == "" || memberNode == nil {
				continue
			}
			memberID := id + "." + memberName
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: memberID, Kind: graph.KindVariable, Name: memberName,
				FilePath:  filePath,
				StartLine: int(memberNode.StartPoint().Row) + 1,
				EndLine:   int(memberNode.EndPoint().Row) + 1,
				Language:  "typescript",
				Meta:      map[string]any{"receiver": name, "kind": "enum_member"},
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: memberID, To: id, Kind: graph.EdgeMemberOf,
				FilePath: filePath, Line: int(memberNode.StartPoint().Row) + 1,
			})
		}
	}
}

// emitImport records the import edge and, for default/namespace imports,
// populates the alias→path map used when classifying member-call
// receivers against imported modules.
//
// Named imports are intentionally skipped — `a(x)` is already a plain
// call matched by the call-expression pattern and doesn't go through
// the selector-call path.
func (e *TypeScriptExtractor) emitImport(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, imports map[string]string) {
	path := m.Captures["import.path"]
	importPath := strings.Trim(path.Text, `"'`)
	line := path.StartLine + 1

	// Per-import node: one per `import` statement, deduped by path
	// within a file. Lets find_usages on an import path return the
	// importing files in one hop.
	importNodeID := filePath + "::import::" + importPath
	importMeta := map[string]any{
		"path":        importPath,
		"is_external": isExternalTSImport(importPath),
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID:        importNodeID,
		Kind:      graph.KindImport,
		Name:      tsImportDisplayName(importPath),
		FilePath:  filePath,
		StartLine: line,
		EndLine:   line,
		Language:  "typescript",
		Meta:      importMeta,
	})
	// File → import-node uses EdgeContains (the file contains the
	// import statement; it doesn't define the imported module). The
	// resolver-facing file → unresolved::import path stays on
	// EdgeImports unchanged — that's a file-to-file dependency, a
	// different relationship.
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: importNodeID,
		Kind: graph.EdgeContains, FilePath: filePath, Line: line,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: line,
	})
	defCap, ok := m.Captures["import.def"]
	if !ok || defCap.Node == nil {
		return
	}
	// Per-binding edges (`import { a, b as c }`) on top of the module edge.
	emitJSPerBindingImports(defCap.Node, importPath, fileID, filePath, src, result)
	for i := 0; i < int(defCap.Node.NamedChildCount()); i++ {
		child := defCap.Node.NamedChild(i)
		if child.Type() != "import_clause" {
			continue
		}
		for j := 0; j < int(child.NamedChildCount()); j++ {
			c := child.NamedChild(j)
			switch c.Type() {
			case "identifier": // default: `import Foo from ...`
				imports[c.Content(src)] = importPath
			case "namespace_import": // `import * as Foo from ...`
				for k := 0; k < int(c.NamedChildCount()); k++ {
					if id := c.NamedChild(k); id.Type() == "identifier" {
						imports[id.Content(src)] = importPath
					}
				}
			}
		}
	}
}

// emitReExport records the alias-aware re-export edges for an
// `export ... from "mod"` statement (the barrel-file forwarding form).
func (e *TypeScriptExtractor) emitReExport(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	def, ok := m.Captures["reexport.def"]
	if !ok || def.Node == nil {
		return
	}
	importPath := strings.Trim(m.Captures["reexport.path"].Text, `"'`+"`")
	emitJSReExport(def.Node, importPath, fileID, filePath, src, result)
}

// emitMethod is called once per method_definition captured at root
// scope. The enclosing class is found by walking up the parent chain
// to the nearest class_declaration. Method shorthand inside an object
// literal — `const api = { process() {...} }` — also parses as a
// `method_definition` (its container is an `object`, not a
// `class_declaration`); those are routed to emitObjectLiteralMethod so
// they get a real KindFunction node instead of being silently dropped.
// The returned (owner, member, id) triple is non-empty only for an
// object-literal shorthand and lets Extract register the member so a
// later `owner.method()` call resolves to it directly.
func (e *TypeScriptExtractor) emitMethod(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult, annotationSeen map[string]bool, seenIDs map[string]bool) (owner, member, id string) {
	def := m.Captures["method.def"]
	if def.Node == nil {
		return "", "", ""
	}
	classNode := findEnclosingClass(def.Node)
	if classNode == nil {
		return e.emitObjectLiteralMethod(m, filePath, src, result)
	}
	classNameNode := classNode.ChildByFieldName("name")
	if classNameNode == nil {
		return "", "", ""
	}
	className := classNameNode.Content(src)
	classID := filePath + "::" + className
	name := m.Captures["method.name"].Text
	methodID, methodOK := disambiguateID(seenIDs, classID+"."+name, def.StartLine+1)
	if !methodOK {
		return "", "", ""
	}
	node := &graph.Node{
		ID: methodID, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     map[string]any{"receiver": className},
	}
	if rt := extractTSMethodReturnType(def.Node, src); rt != "" {
		node.Meta["return_type"] = rt
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		node.Meta["doc"] = doc
	}
	node.Meta["visibility"] = tsMemberVisibility(def.Node, src)
	if body := tsFunctionBody(def.Node); body != nil {
		StampFunctionMetrics(node, body, "typescript")
	}
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: methodID, To: classID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	// Function shape (params, return type, generics) for the method.
	emitTSFunctionShape(methodID, def.Node, src, filePath, def.StartLine+1, result)
	// JSX child rendering inside the method body (covers React class
	// components' render() methods).
	if body := tsFunctionBody(def.Node); body != nil {
		emitJSXRenderEdges(methodID, body, src, filePath, result)
	}
	// NestJS-style dispatch decorators (@UseGuards/@UseInterceptors/...)
	// are SIBLINGS of method_definition inside class_body — walk backward.
	// Each decorator also produces an EdgeAnnotated → annotation node so
	// agents can query "find all @<X>" without re-deriving the dispatch
	// rules every time.
	for sib := def.Node.PrevSibling(); sib != nil && sib.Type() == "decorator"; sib = sib.PrevSibling() {
		emitDispatchFromDecorator(sib, src, methodID, filePath, result)
		emitTSAnnotationEdges([]*sitter.Node{sib}, methodID, filePath, src, result, annotationSeen)
	}
	return "", "", ""
}

// emitObjectLiteralMethod handles method shorthand inside an object
// literal — `export const api = { process() {...} }`. tree-sitter
// parses `process()` as a `method_definition` whose container is an
// `object`, so emitMethod's class walk finds nothing; without this the
// shorthand method has no graph node at all and a call `api.process()`
// either resolves to nothing or — worse — to an unrelated free
// `process` function elsewhere in the repo.
//
// The node is named `<owner>.<method>` (e.g. `api.process`), matching
// emitArrowField's owner-qualified convention, and its ID carries the
// start line so two same-named shorthand methods in one file don't
// collide. Returns (owner, member, id) when the owner is a top-level
// `const owner = { ... }` binding so Extract can register the member
// for direct call resolution; returns empty strings for an inline /
// anonymous object whose calls can't be attributed to an owner name.
func (e *TypeScriptExtractor) emitObjectLiteralMethod(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult) (owner, member, id string) {
	def := m.Captures["method.def"]
	if def.Node == nil {
		return "", "", ""
	}
	parent := def.Node.Parent()
	if parent == nil || parent.Type() != "object" {
		return "", "", ""
	}
	member = m.Captures["method.name"].Text
	if member == "" {
		return "", "", ""
	}
	owner = tsArrowFieldOwner(def.Node, src)
	storeFactory := ""
	if owner == "" || jsIsStoreOptionKey(owner) {
		if b, _, ok := jsStoreFactoryBinding(def.Node, src); ok {
			owner, storeFactory = b, b
		}
	}
	name := member
	if owner != "" {
		name = owner + "." + member
	}
	id = fmt.Sprintf("%s::%s@%d", filePath, name, def.StartLine+1)
	meta := map[string]any{"signature": fmt.Sprintf("%s()", name)}
	if storeFactory != "" {
		meta["store_factory"] = storeFactory
		meta["store_member"] = member
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	if rt := extractTSMethodReturnType(def.Node, src); rt != "" {
		meta["return_type"] = rt
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: filePath, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	emitTSFunctionShape(id, def.Node, src, filePath, def.StartLine+1, result)
	if body := tsFunctionBody(def.Node); body != nil {
		emitJSXRenderEdges(id, body, src, filePath, result)
	}
	if owner == "" {
		return "", "", ""
	}
	return owner, member, id
}

func (e *TypeScriptExtractor) emitClassProperty(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult, annotationSeen map[string]bool) {
	def := m.Captures["prop.def"]
	if def.Node == nil {
		return
	}
	classNode := findEnclosingClass(def.Node)
	if classNode == nil {
		return
	}
	classNameNode := classNode.ChildByFieldName("name")
	if classNameNode == nil {
		return
	}
	className := classNameNode.Content(src)
	classID := filePath + "::" + className
	name := m.Captures["prop.name"].Text
	id := classID + "." + name
	meta := map[string]any{"receiver": className, "kind": "class_property"}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	meta["visibility"] = tsMemberVisibility(def.Node, src)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	// A typed field (`field: T`) references type T — emit EdgeTypedAs so
	// find_usages(T) surfaces the declaration site without an LSP.
	if rawType := tsReturnTypeRaw(def.Node, src); rawType != "" {
		emitTSTypeUseEdges(id, rawType, filePath, def.StartLine+1, result)
	}
	// Decorators on a class field can appear two ways depending on
	// grammar version: as previous siblings in the class body, or as
	// direct children of the public_field_definition node. Try both
	// locations.
	for sib := def.Node.PrevSibling(); sib != nil && sib.Type() == "decorator"; sib = sib.PrevSibling() {
		emitTSAnnotationEdges([]*sitter.Node{sib}, id, filePath, src, result, annotationSeen)
	}
	for i := 0; i < int(def.Node.ChildCount()); i++ {
		c := def.Node.Child(i)
		if c != nil && c.Type() == "decorator" {
			emitTSAnnotationEdges([]*sitter.Node{c}, id, filePath, src, result, annotationSeen)
		}
	}
}

// findEnclosingClass walks up the parent chain looking for the nearest
// class_declaration ancestor. Returns nil when the node is not inside
// a class.
func findEnclosingClass(n *sitter.Node) *sitter.Node {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() == "class_declaration" {
			return p
		}
	}
	return nil
}

// --- helpers (unchanged) ---------------------------------------------

// extractTSInterfaceMethods walks children of an interface_declaration
// node to find method_signature and property_signature entries and
// returns their names.
func extractTSInterfaceMethods(ifaceNode *sitter.Node, src []byte) []string {
	var methods []string
	var body *sitter.Node
	for i := 0; i < int(ifaceNode.NamedChildCount()); i++ {
		child := ifaceNode.NamedChild(i)
		if child.Type() == "interface_body" || child.Type() == "object_type" {
			body = child
			break
		}
	}
	if body == nil {
		return methods
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		switch child.Type() {
		case "method_signature", "property_signature":
			for j := 0; j < int(child.NamedChildCount()); j++ {
				nameNode := child.NamedChild(j)
				if nameNode.Type() == "property_identifier" {
					methods = append(methods, nameNode.Content(src))
					break
				}
			}
		}
	}
	return methods
}

// emitModuleBindings walks a class_declaration's decorators, finds the
// @Module(...) decorator (if any), and emits one EdgeProvides edge per
// `{ provide: X, useClass: Y }` entry in its `providers` array. The
// edge points from the module class to the concrete implementation Y;
// Meta["provides_for"] names the abstract type X so the resolver can
// prefer Y when a call's receiver_type is X. Non-useClass providers
// (useValue / useFactory / useExisting / bare class references) are
// skipped here — they'll be handled by the @Inject(TOKEN) feature.
func emitModuleBindings(classNode *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult) {
	decorators := classDecorators(classNode)
	for _, dec := range decorators {
		call := nestDecoratorCall(dec)
		if call == nil {
			continue
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || fn.Type() != "identifier" || fn.Content(src) != "Module" {
			continue
		}
		args := call.ChildByFieldName("arguments")
		if args == nil {
			continue
		}
		var config *sitter.Node
		for i := 0; i < int(args.NamedChildCount()); i++ {
			c := args.NamedChild(i)
			if c != nil && c.Type() == "object" {
				config = c
				break
			}
		}
		if config == nil {
			continue
		}
		emitProvidersFromObject(config, src, classID, filePath, result, "@Module")
	}
	// Dynamic-module factory bindings (forRoot/forFeature/register/*Async)
	// are emitted by walkClassMembers in the post-pass — same walkNodes
	// pass that handles @Inject consumers and `this.<field>` tenv
	// seeding.
}

func emitProvidersFromObject(config *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, originTag string) {
	providersNode := objectFieldValue(config, src, "providers")
	if providersNode == nil || providersNode.Type() != "array" {
		return
	}
	for i := 0; i < int(providersNode.NamedChildCount()); i++ {
		entry := providersNode.NamedChild(i)
		if entry == nil || entry.Type() != "object" {
			continue
		}
		abstract := objectFieldToken(entry, src, "provide")
		if abstract == "" {
			continue
		}
		if concrete := objectFieldIdentifier(entry, src, "useClass"); concrete != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     classID,
				To:       "unresolved::" + concrete,
				Kind:     graph.EdgeProvides,
				FilePath: filePath,
				Line:     int(entry.StartPoint().Row) + 1,
				Meta: map[string]any{
					"provides_for": abstract,
					"binding":      "useClass",
					"origin":       originTag,
				},
			})
			continue
		}
		for _, variant := range []string{"useValue", "useFactory", "useExisting"} {
			if objectFieldValue(entry, src, variant) == nil {
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     classID,
				To:       "unresolved::" + abstract,
				Kind:     graph.EdgeProvides,
				FilePath: filePath,
				Line:     int(entry.StartPoint().Row) + 1,
				Meta: map[string]any{
					"di_token": abstract,
					"binding":  variant,
					"origin":   originTag,
				},
			})
			break
		}
	}
}

var dynamicModuleMethods = map[string]struct{}{
	"forRoot":         {},
	"forRootAsync":    {},
	"forFeature":      {},
	"forFeatureAsync": {},
	"register":        {},
	"registerAsync":   {},
}

// classCarry pairs a class_declaration node with the classID
// derived from the main-pass match, so the post-pass can dispatch
// per-member work without re-deriving the ID. See walkClassMembers.
type classCarry struct {
	node *sitter.Node
	id   string
}

// walkClassMembers walks a class subtree exactly once and dispatches
// every per-member concern in one pass:
//
//   - constructor parameters → @Inject(TOKEN) decorators emit
//     EdgeConsumes; TS parameter-property shorthand seeds
//     tenv["this.<name>"] from the explicit type annotation.
//   - public_field_definition → field-level @Inject decorators
//     emit EdgeConsumes; field type annotation OR `inject(...)`
//     initializer seeds tenv["this.<name>"].
//   - static factory methods named forRoot / forFeature /
//     register / *Async (NestJS DynamicModule pattern) → emit
//     EdgeProvides for each `{ provide, useClass }` entry in the
//     returned object.
//
// Replaces three previous walkNodes passes (emitInjectConsumers,
// emitDynamicModuleBindings, collectThisParamTypesInClass).
func walkClassMembers(classNode *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, tenv typeEnv) {
	seenInject := make(map[string]struct{})
	walkNodes(classNode, func(n *sitter.Node) {
		switch n.Type() {
		case "method_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			methodName := nameNode.Content(src)
			if methodName == "constructor" {
				dispatchConstructorMembers(n, src, classID, filePath, result, tenv, seenInject)
				return
			}
			// NestJS DynamicModule factory: only static methods named
			// from the dynamicModuleMethods set qualify.
			if _, ok := dynamicModuleMethods[methodName]; !ok {
				return
			}
			if !methodIsStatic(n) {
				return
			}
			body := n.ChildByFieldName("body")
			if body == nil {
				return
			}
			cfg := findReturnedConfigObject(body)
			if cfg == nil {
				return
			}
			emitProvidersFromObject(cfg, src, classID, filePath, result, methodName)

		case "public_field_definition":
			dispatchClassField(n, src, classID, filePath, result, tenv, seenInject)
		}
	})
}

// dispatchConstructorMembers handles both @Inject(TOKEN) decorators
// on constructor parameters AND tenv seeding from TS parameter-
// property shorthand (`constructor(private readonly foo: Bar) {}`).
func dispatchConstructorMembers(method *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, tenv typeEnv, seenInject map[string]struct{}) {
	params := method.ChildByFieldName("parameters")
	if params == nil {
		return
	}
	for i := 0; i < int(params.NamedChildCount()); i++ {
		p := params.NamedChild(i)
		if p == nil {
			continue
		}
		// @Inject decorator on the parameter → consumer edge.
		for j := 0; j < int(p.ChildCount()); j++ {
			c := p.Child(j)
			if c != nil && c.Type() == "decorator" {
				emitInjectFromDecorator(c, src, classID, filePath, result, seenInject)
			}
		}
		// Parameter-property shorthand → tenv["this.<name>"].
		if p.Type() != "required_parameter" && p.Type() != "optional_parameter" {
			continue
		}
		if !hasParameterPropertyModifier(p) {
			continue
		}
		paramName := paramIdentifier(p, src)
		if paramName == "" {
			continue
		}
		typeName := paramTypeAnnotation(p, src)
		if typeName == "" {
			continue
		}
		tenv["this."+paramName] = typeName
	}
}

// dispatchClassField handles both @Inject decorators and tenv
// seeding for a public_field_definition. tenv prefers an explicit
// type annotation; falls back to the type inferred from an
// `inject(Foo)` initializer.
func dispatchClassField(field *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, tenv typeEnv, seenInject map[string]struct{}) {
	for i := 0; i < int(field.ChildCount()); i++ {
		c := field.Child(i)
		if c != nil && c.Type() == "decorator" {
			emitInjectFromDecorator(c, src, classID, filePath, result, seenInject)
		}
	}
	name := classFieldName(field, src)
	if name == "" {
		return
	}
	if t := classFieldTypeAnnotation(field, src); t != "" {
		tenv["this."+name] = t
		return
	}
	if t := injectInitializerType(field, src); t != "" {
		tenv["this."+name] = t
	}
}

// emitInjectFromDecorator emits one EdgeConsumes for an @Inject(TOKEN)
// decorator, deduping against seenInject so multiple @Inject sites
// for the same token (constructor + field) don't double-emit.
func emitInjectFromDecorator(dec *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, seenInject map[string]struct{}) {
	tok := injectDecoratorArg(dec, src)
	if tok == "" {
		return
	}
	if _, dup := seenInject[tok]; dup {
		return
	}
	seenInject[tok] = struct{}{}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     classID,
		To:       "unresolved::" + tok,
		Kind:     graph.EdgeConsumes,
		FilePath: filePath,
		Line:     int(dec.StartPoint().Row) + 1,
		Meta: map[string]any{
			"di_token": tok,
			"via":      "@Inject",
		},
	})
}

// findReturnedConfigObject finds the first object literal returned
// from a static factory method's body (NestJS DynamicModule shape).
// Inner walkNodes is bounded by method body, which is small.
func findReturnedConfigObject(body *sitter.Node) *sitter.Node {
	var cfg *sitter.Node
	walkNodes(body, func(m *sitter.Node) {
		if cfg != nil || m.Type() != "return_statement" {
			return
		}
		for i := 0; i < int(m.NamedChildCount()); i++ {
			c := m.NamedChild(i)
			if c != nil && c.Type() == "object" {
				cfg = c
				return
			}
		}
	})
	return cfg
}

func methodIsStatic(m *sitter.Node) bool {
	for i := 0; i < int(m.ChildCount()); i++ {
		c := m.Child(i)
		if c != nil && c.Type() == "static" {
			return true
		}
	}
	return false
}

func classDecorators(classNode *sitter.Node) []*sitter.Node {
	var decs []*sitter.Node
	for i := 0; i < int(classNode.ChildCount()); i++ {
		c := classNode.Child(i)
		if c != nil && c.Type() == "decorator" {
			decs = append(decs, c)
		}
	}
	parent := classNode.Parent()
	if parent != nil && parent.Type() == "export_statement" {
		for i := 0; i < int(parent.ChildCount()); i++ {
			c := parent.Child(i)
			if c != nil && c.Type() == "decorator" {
				decs = append(decs, c)
			}
		}
	}
	return decs
}

// tsDecoratorNameAndArgs reads a `decorator` AST node and returns the
// bare decorator name (no leading "@") plus the verbatim argument
// string between the call's outermost parens (or "" when the
// decorator is a bare identifier with no call). Returns "", "" on
// nodes that don't look like decorators.
func tsDecoratorNameAndArgs(dec *sitter.Node, src []byte) (string, string) {
	if dec == nil {
		return "", ""
	}
	// The first named child of a decorator node is typically either an
	// identifier (`@Foo`), a member_expression (`@Foo.bar`), or a
	// call_expression (`@Foo(...)`).
	for i := 0; i < int(dec.NamedChildCount()); i++ {
		c := dec.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier":
			return c.Content(src), ""
		case "member_expression":
			return c.Content(src), ""
		case "call_expression":
			fn := c.ChildByFieldName("function")
			args := c.ChildByFieldName("arguments")
			name := ""
			if fn != nil {
				name = fn.Content(src)
			}
			argText := ""
			if args != nil {
				argText = args.Content(src)
				argText = strings.TrimPrefix(argText, "(")
				argText = strings.TrimSuffix(argText, ")")
			}
			return name, argText
		}
	}
	return "", ""
}

// emitTSAnnotationEdges walks a slice of decorator AST nodes and emits
// an EdgeAnnotated edge from `fromID` for each one, sharing one
// synthetic annotation node per (lang, decorator-name) pair via `seen`.
func emitTSAnnotationEdges(decs []*sitter.Node, fromID, filePath string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	for _, dec := range decs {
		name, args := tsDecoratorNameAndArgs(dec, src)
		if name == "" {
			continue
		}
		EmitAnnotationEdge(fromID, "typescript", name, args, filePath, int(dec.StartPoint().Row)+1, result, seen)
	}
}

func objectFieldValue(objNode *sitter.Node, src []byte, name string) *sitter.Node {
	for i := 0; i < int(objNode.NamedChildCount()); i++ {
		p := objNode.NamedChild(i)
		if p == nil || p.Type() != "pair" {
			continue
		}
		key := p.ChildByFieldName("key")
		if key == nil {
			continue
		}
		if key.Content(src) != name {
			continue
		}
		return p.ChildByFieldName("value")
	}
	return nil
}

func objectFieldIdentifier(objNode *sitter.Node, src []byte, name string) string {
	v := objectFieldValue(objNode, src, name)
	if v == nil || v.Type() != "identifier" {
		return ""
	}
	return v.Content(src)
}

func objectFieldToken(objNode *sitter.Node, src []byte, name string) string {
	v := objectFieldValue(objNode, src, name)
	if v == nil {
		return ""
	}
	switch v.Type() {
	case "identifier":
		return v.Content(src)
	case "string":
		s := v.Content(src)
		if len(s) >= 2 {
			return s[1 : len(s)-1]
		}
	}
	return ""
}

func injectDecoratorArg(dec *sitter.Node, src []byte) string {
	call := nestDecoratorCall(dec)
	if call == nil {
		return ""
	}
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "identifier" || fn.Content(src) != "Inject" {
		return ""
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i := 0; i < int(args.NamedChildCount()); i++ {
		arg := args.NamedChild(i)
		if arg == nil {
			continue
		}
		switch arg.Type() {
		case "identifier":
			return arg.Content(src)
		case "string":
			s := arg.Content(src)
			if len(s) >= 2 {
				return s[1 : len(s)-1]
			}
		}
	}
	return ""
}

var nestDispatchDecorators = map[string]string{
	"UseGuards":       "canActivate",
	"UseInterceptors": "intercept",
	"UseFilters":      "catch",
	"UsePipes":        "transform",
}

func emitDispatchFromDecorator(dec *sitter.Node, src []byte, methodID, filePath string, result *parser.ExtractionResult) {
	callNode := nestDecoratorCall(dec)
	if callNode == nil {
		return
	}
	fn := callNode.ChildByFieldName("function")
	if fn == nil || fn.Type() != "identifier" {
		return
	}
	entryMethod, ok := nestDispatchDecorators[fn.Content(src)]
	if !ok {
		return
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	for j := 0; j < int(args.NamedChildCount()); j++ {
		arg := args.NamedChild(j)
		if arg == nil || arg.Type() != "identifier" {
			continue
		}
		argClass := arg.Content(src)
		result.Edges = append(result.Edges, &graph.Edge{
			From:     methodID,
			To:       "unresolved::*." + entryMethod,
			Kind:     graph.EdgeCalls,
			FilePath: filePath,
			Line:     int(dec.StartPoint().Row) + 1,
			Meta: map[string]any{
				"receiver_type":      argClass,
				"dispatch_decorator": fn.Content(src),
			},
		})
	}
}

func nestDecoratorCall(dec *sitter.Node) *sitter.Node {
	for i := 0; i < int(dec.NamedChildCount()); i++ {
		c := dec.NamedChild(i)
		if c != nil && c.Type() == "call_expression" {
			return c
		}
	}
	return nil
}

func classFieldName(field *sitter.Node, src []byte) string {
	nameNode := field.ChildByFieldName("name")
	if nameNode == nil || nameNode.Type() != "property_identifier" {
		return ""
	}
	return nameNode.Content(src)
}

func classFieldTypeAnnotation(field *sitter.Node, src []byte) string {
	for i := 0; i < int(field.NamedChildCount()); i++ {
		c := field.NamedChild(i)
		if c == nil || c.Type() != "type_annotation" {
			continue
		}
		for j := 0; j < int(c.NamedChildCount()); j++ {
			tn := c.NamedChild(j)
			if tn == nil {
				continue
			}
			return normalizeTypeName(tn.Content(src))
		}
	}
	return ""
}

func injectInitializerType(field *sitter.Node, src []byte) string {
	value := field.ChildByFieldName("value")
	if value == nil || value.Type() != "call_expression" {
		return ""
	}
	fn := value.ChildByFieldName("function")
	if fn == nil || fn.Type() != "identifier" || fn.Content(src) != "inject" {
		return ""
	}
	args := value.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i := 0; i < int(args.NamedChildCount()); i++ {
		arg := args.NamedChild(i)
		if arg == nil {
			continue
		}
		if arg.Type() == "identifier" {
			return arg.Content(src)
		}
		return ""
	}
	return ""
}

func hasParameterPropertyModifier(p *sitter.Node) bool {
	for i := 0; i < int(p.ChildCount()); i++ {
		c := p.Child(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "accessibility_modifier", "readonly":
			return true
		case "override_modifier":
			// `override` can accompany a visibility modifier; keep scanning.
		}
	}
	return false
}

func paramIdentifier(p *sitter.Node, src []byte) string {
	pattern := p.ChildByFieldName("pattern")
	if pattern != nil && pattern.Type() == "identifier" {
		return pattern.Content(src)
	}
	for i := 0; i < int(p.NamedChildCount()); i++ {
		c := p.NamedChild(i)
		if c != nil && c.Type() == "identifier" {
			return c.Content(src)
		}
	}
	return ""
}

func paramTypeAnnotation(p *sitter.Node, src []byte) string {
	ta := p.ChildByFieldName("type")
	if ta == nil {
		for i := 0; i < int(p.NamedChildCount()); i++ {
			c := p.NamedChild(i)
			if c != nil && c.Type() == "type_annotation" {
				ta = c
				break
			}
		}
	}
	if ta == nil {
		return ""
	}
	for i := 0; i < int(ta.NamedChildCount()); i++ {
		c := ta.NamedChild(i)
		if c == nil {
			continue
		}
		return normalizeTypeName(c.Content(src))
	}
	return ""
}

// normalizeTypeName strips generics, arrays, and nullable markers.
// "User" → "User", "User[]" → "User", "User<T>" → "User",
// "User | null" → "User".
func normalizeTypeName(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimSuffix(t, "[]")
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	if idx := strings.Index(t, " |"); idx > 0 {
		t = t[:idx]
	}
	switch t {
	case "string", "number", "boolean", "void", "any", "unknown", "never", "null", "undefined":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return ""
	}
	return t
}

// tsDocStartRow returns the 0-based row from which to scan for a doc
// comment, plus whether the captured declaration sits inside an
// `export_statement` (so emit* can also set visibility correctly).
//
// JSDoc/leading line comments live above the OUTERMOST wrapping
// statement: when a `function_declaration` is wrapped in
// `export_statement`, the doc block sits above the `export` keyword,
// not above `function`. Scanning from the inner declaration's row
// works in practice because tree-sitter's typescript grammar reports
// the inner row as the same as the export-statement row when they
// share a line, but for multi-line wrappers this fix-up matters.
func tsDocStartRow(def *parser.CapturedNode) (int, bool) {
	if def == nil || def.Node == nil {
		if def != nil {
			return def.StartLine, false
		}
		return 0, false
	}
	parent := def.Node.Parent()
	if parent != nil && parent.Type() == "export_statement" {
		return int(parent.StartPoint().Row), true
	}
	return def.StartLine, false
}

func tsTopLevelVisibility(exported bool) string {
	if exported {
		return VisibilityPublic
	}
	return VisibilityPrivate
}

// tsMemberVisibility inspects a class member (method_definition,
// public_field_definition) for an accessibility modifier child. The TS
// grammar exposes the modifier as an `accessibility_modifier` node
// containing the keyword text.
func tsMemberVisibility(member *sitter.Node, src []byte) string {
	if member == nil {
		return VisibilityPublic
	}
	for i := 0; i < int(member.ChildCount()); i++ {
		c := member.Child(i)
		if c == nil || c.Type() != "accessibility_modifier" {
			continue
		}
		switch c.Content(src) {
		case "private":
			return VisibilityPrivate
		case "protected":
			return VisibilityProtected
		case "public":
			return VisibilityPublic
		}
	}
	return VisibilityPublic
}

func extractTSMethodReturnType(methodNode *sitter.Node, src []byte) string {
	for i := 0; i < int(methodNode.NamedChildCount()); i++ {
		child := methodNode.NamedChild(i)
		if child.Type() == "type_annotation" {
			if child.NamedChildCount() > 0 {
				typeNode := child.NamedChild(0)
				return normalizeTypeName(typeNode.Content(src))
			}
		}
	}
	return ""
}

func inferTypeFromNewExpr(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "identifier" || child.Type() == "type_identifier" {
			name := child.Content(src)
			if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		}
	}
	return ""
}

// tsTypeParams reads the `type_parameters` child of a TS declaration
// (function_declaration / class_declaration / interface_declaration /
// type_alias_declaration) and returns each declared type parameter
// as {name, bound, default} where bound and default are optional.
//
// TS grammar: type_parameters wraps `type_parameter` children. Each
// type_parameter has `name` (type_identifier), an optional
// `constraint` (extends X) and an optional `default_type`.
func tsTypeParams(decl *sitter.Node, src []byte) []map[string]string {
	if decl == nil {
		return nil
	}
	tps := decl.ChildByFieldName("type_parameters")
	if tps == nil {
		return nil
	}
	var out []map[string]string
	for i := 0; i < int(tps.NamedChildCount()); i++ {
		tp := tps.NamedChild(i)
		if tp == nil || tp.Type() != "type_parameter" {
			continue
		}
		entry := map[string]string{}
		if n := tp.ChildByFieldName("name"); n != nil {
			entry["name"] = n.Content(src)
		}
		// constraint child is the `extends X` part.
		for j := 0; j < int(tp.ChildCount()); j++ {
			c := tp.Child(j)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "constraint":
				// Walk inner type — strip the leading "extends".
				txt := strings.TrimSpace(c.Content(src))
				txt = strings.TrimPrefix(txt, "extends")
				entry["bound"] = strings.TrimSpace(txt)
			case "default_type":
				txt := strings.TrimSpace(c.Content(src))
				txt = strings.TrimPrefix(txt, "=")
				entry["default"] = strings.TrimSpace(txt)
			}
		}
		if entry["name"] != "" {
			out = append(out, entry)
		}
	}
	return out
}

// isExternalTSImport returns true when the import path looks like a
// node module ("react", "@nestjs/common", "lodash/get") rather than a
// relative-path source import ("./foo", "../bar").
func isExternalTSImport(path string) bool {
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, ".") || strings.HasPrefix(path, "/") {
		return false
	}
	return true
}

// tsImportDisplayName picks a short name for an import node — the
// trailing path segment for relative imports, or the module name (or
// scoped/module pair) for node modules.
func tsImportDisplayName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		// Keep scoped @x/y → "@x/y", "@x/y/z" → "y/z" feels odd, but
		// we want a stable handle. Use the bare last segment when the
		// path has no '@' prefix; for scoped packages keep both
		// segments to disambiguate.
		if strings.HasPrefix(path, "@") {
			return path
		}
		return path[i+1:]
	}
	return path
}
