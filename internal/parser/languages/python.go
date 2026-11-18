package languages

import (
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/python"
)

// qPyAll is a single tree-sitter query alternating over every pattern
// the Python extractor needs. One tree walk per file replaces the 10
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set. Class membership is
// resolved via a parent walk on the captured node — the function must
// be a direct child of `block` whose direct parent is
// `class_definition`, with an optional `decorated_definition` wrapper
// between the def and the block (decorated methods are methods too —
// see pyDirectClassParent).
const qPyAll = `
[
  (function_definition
    name: (identifier) @func.name) @func.def

  (class_definition
    name: (identifier) @class.name) @class.def

  (import_statement) @import.def

  (import_from_statement
    module_name: (dotted_name) @import.module) @importfrom.def

  (import_from_statement
    module_name: (relative_import) @importfrom.rel) @importfrom_rel.def

  (call
    function: (identifier) @call.name) @call.expr

  (call
    function: (attribute
      object: (_) @callattr.receiver
      attribute: (identifier) @callattr.method)) @callattr.expr

  (call
    function: (subscript
      value: (_) @subcall.receiver
      subscript: (_) @subcall.key)) @subcall.expr

  (assignment
    left: (identifier) @var.name) @var.def

  (assignment
    left: (identifier) @tvar.name
    type: (type (identifier) @tvar.type)) @tvar.def

  (assignment
    left: (identifier) @uvar.name
    right: (call
      function: (identifier) @uvar.callee)) @uvar.def

  (typed_parameter
    (identifier) @tparam.name
    type: (type) @tparam.type) @tparam.def

  (typed_default_parameter
    name: (identifier) @tdparam.name
    type: (type) @tdparam.type) @tdparam.def
]
`

// PythonExtractor extracts Python source files into graph nodes and edges.
type PythonExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewPythonExtractor() *PythonExtractor {
	lang := python.GetLanguage()
	return &PythonExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qPyAll, lang),
	}
}

func (e *PythonExtractor) Language() string     { return "python" }
func (e *PythonExtractor) Extensions() []string { return []string{".py"} }

// --- Deferred match buffers ----------------------------------------

type pyDeferredCall struct {
	name     string
	receiver string // attribute receiver text
	line     int
	isAttr   bool
	expr     *sitter.Node // for FastAPI Depends() arg lookup
	// dynShape / dynKey tag a dynamic-dispatch blind-spot call shape the
	// resolver cannot bind statically (computed-member obj["foo"]()), so the
	// opt-in speculative synthesizer can fan it to plausible targets. dynKey is
	// the literal method name when the subscript is a string literal.
	dynShape string
	dynKey   string
	// returnUsage is how the call site consumes the return value
	// (graph.ReturnUsage* label), classified at capture time and
	// stamped as edge Meta on every EdgeCalls emitted for this site.
	returnUsage string
}

// pyDeferredVar is a module-level `name = ...` binding held back until the
// whole file has been walked, so real definitions win the shared id.
// Values are copied out of the match rather than holding the
// *parser.CapturedNode: EachMatch recycles those between matches, so a
// retained pointer reads back another match's capture. Node pointers are
// stable — they address the tree, which outlives the walk.
type pyDeferredVar struct {
	name      string
	node      *sitter.Node
	startLine int
	endLine   int
}

// pyStringLiteralValue returns the unquoted value of a Python string-literal
// token (handling an optional r/f/b/u prefix), or "" when the token is not a
// string literal (e.g. a variable subscript key obj[name]).
func pyStringLiteralValue(tok string) string {
	tok = strings.TrimSpace(tok)
	for len(tok) > 1 {
		switch tok[0] {
		case 'r', 'R', 'f', 'F', 'b', 'B', 'u', 'U':
			if tok[1] == '\'' || tok[1] == '"' {
				tok = tok[1:]
				continue
			}
		}
		break
	}
	if len(tok) >= 2 {
		q := tok[0]
		if (q == '\'' || q == '"') && tok[len(tok)-1] == q {
			return tok[1 : len(tok)-1]
		}
	}
	return ""
}

func (e *PythonExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	checked, parseErrors := parser.ParseErrorVerdict(root)
	result := &parser.ExtractionResult{ParseChecked: checked, ParseErrors: parseErrors}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "python",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)
	imports := map[string]string{} // alias → module path
	// Name→type bindings are collected flat here and bucketed into
	// per-function scopes once the function ranges exist — the query walk
	// runs in document order, before any function node is available to
	// own them. See buildPyScopedTypeEnv.
	var assigns []pyAssign

	var calls []pyDeferredCall
	var typeUses []deferredTypeUse
	var topLevelVars []pyDeferredVar

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["class.def"] != nil:
			e.emitClass(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, src, result, imports)

		case m.Captures["importfrom.def"] != nil:
			e.emitImportFrom(m, filePath, fileID, src, result, imports)

		case m.Captures["importfrom_rel.def"] != nil:
			e.emitImportFromRelative(m, filePath, fileID, src, result, imports)

		case m.Captures["callattr.expr"] != nil:
			expr := m.Captures["callattr.expr"]
			calls = append(calls, pyDeferredCall{
				name:        m.Captures["callattr.method"].Text,
				receiver:    m.Captures["callattr.receiver"].Text,
				line:        expr.StartLine + 1,
				isAttr:      true,
				expr:        expr.Node,
				returnUsage: classifyReturnUsage(expr.Node, src, pyReturnUsageSpec),
			})

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, pyDeferredCall{
				name:        m.Captures["call.name"].Text,
				line:        expr.StartLine + 1,
				expr:        expr.Node,
				returnUsage: classifyReturnUsage(expr.Node, src, pyReturnUsageSpec),
			})

		case m.Captures["subcall.expr"] != nil:
			// Computed-member call `obj["foo"]()` — a dynamic-dispatch blind
			// spot. Captured for the opt-in speculative synthesizer; emits no
			// edge by itself unless that pass is enabled.
			expr := m.Captures["subcall.expr"]
			dc := pyDeferredCall{
				line:        expr.StartLine + 1,
				expr:        expr.Node,
				dynShape:    "computed_member",
				returnUsage: classifyReturnUsage(expr.Node, src, pyReturnUsageSpec),
			}
			if r := m.Captures["subcall.receiver"]; r != nil {
				dc.receiver = r.Text
			}
			if k := m.Captures["subcall.key"]; k != nil {
				dc.dynKey = pyStringLiteralValue(k.Text)
			}
			calls = append(calls, dc)

		case m.Captures["tvar.def"] != nil:
			// Tier 0: a written annotation on an assignment.
			rawType := m.Captures["tvar.type"].Text
			line := m.Captures["tvar.def"].StartLine + 1
			if typeName := normalizePyTypeName(rawType); typeName != "" {
				assigns = append(assigns, pyAssign{
					name: m.Captures["tvar.name"].Text, typeName: typeName,
					line: line, explicit: true,
				})
			}
			typeUses = append(typeUses, deferredTypeUse{
				typeText: rawType,
				line:     line,
			})

		case m.Captures["tparam.def"] != nil:
			// A written parameter annotation declares that parameter's
			// type for the whole function body. Annotated parameters are
			// the second-largest source of statically stated receivers
			// after `self`, and were previously ignored entirely.
			pyAppendParamAssign(&assigns, m.Captures["tparam.name"], m.Captures["tparam.type"])

		case m.Captures["tdparam.def"] != nil:
			pyAppendParamAssign(&assigns, m.Captures["tdparam.name"], m.Captures["tdparam.type"])

		case m.Captures["uvar.def"] != nil:
			// Tier 1: constructor-call inference. Precedence against an
			// explicit annotation, and the two-different-classes conflict
			// rule, are both applied in buildPyScopedTypeEnv once the
			// binding's scope is known.
			callee := m.Captures["uvar.callee"].Text
			if callee != "" && unicode.IsUpper(rune(callee[0])) {
				assigns = append(assigns, pyAssign{
					name: m.Captures["uvar.name"].Text, typeName: callee,
					line: m.Captures["uvar.def"].StartLine + 1,
				})
			}

		case m.Captures["var.def"] != nil:
			if nameCap, defCap := m.Captures["var.name"], m.Captures["var.def"]; nameCap != nil && defCap != nil {
				topLevelVars = append(topLevelVars, pyDeferredVar{
					name:      nameCap.Text,
					node:      defCap.Node,
					startLine: defCap.StartLine,
					endLine:   defCap.EndLine,
				})
			}
		}
	})

	// Module-level variables are emitted only after the whole walk, so a
	// class or function of the same name has already claimed the canonical
	// `file::Name` id and the variable yields to it. `seen` is shared
	// across all three emitters — emitting a variable inline would let
	// `Config = load()` sitting above `class Config:` take the id and
	// delete the class from the graph entirely.
	for _, v := range topLevelVars {
		e.emitTopLevelVar(v, filePath, fileID, result, seen)
	}

	// All function/method nodes have been emitted; map call sites to
	// their enclosing definition.
	funcRanges := buildFuncRanges(result)

	// Bucket the name→type bindings into the scope that owns each one.
	scoped := buildPyScopedTypeEnv(assigns, funcRanges)

	// Type-use edges: a `x: T` annotation references type T. Attributed
	// to the enclosing function (fallback: the file node) so find_usages(T)
	// surfaces every annotation site without an LSP.
	for _, tu := range typeUses {
		ownerID := findEnclosingFunc(funcRanges, tu.line)
		if ownerID == "" {
			ownerID = fileID
		}
		emitPyTypeUseEdges(ownerID, tu.typeText, filePath, tu.line, result)
	}

	// Reference forms the call/annotation passes don't cover:
	// instantiation, inheritance, isinstance/issubclass, static/class
	// access, and decorator references to Capitalized type names.
	emitPythonReferenceForms(root, src, filePath, fileID, funcRanges, result)

	for _, c := range calls {
		ownerFunc := findEnclosingFunc(funcRanges, c.line)
		callerID := ownerFunc
		if callerID == "" {
			// Module-level call (`app = Flask(__name__)`), a call in a
			// class body (`name = Column(String)`), or a decorator
			// expression — decorators sit above the `def` line, so they
			// fall outside every function range. All three execute at
			// import time and belong to the file. Attribute them to the
			// file node rather than dropping them: dropping made every
			// such callee invisible to find_usages / get_callers, which
			// in Python costs the whole module-level configuration layer
			// (routes, ORM columns, settings, registrations).
			callerID = fileID
		}
		if c.dynShape != "" {
			// Tagged dynamic-dispatch blind-spot call. The placeholder carries
			// the shape + literal key; the speculative synthesizer (opt-in)
			// fans it to plausible targets.
			meta := map[string]any{"dyn_shape": c.dynShape}
			if c.dynKey != "" {
				meta["dyn_key"] = c.dynKey
			}
			if c.receiver != "" {
				meta["dyn_receiver"] = c.receiver
			}
			edge := &graph.Edge{
				From: callerID, To: "unresolved::*",
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line, Meta: meta,
			}
			stampReturnUsage(edge, c.returnUsage)
			result.Edges = append(result.Edges, edge)
			continue
		}
		if c.isAttr {
			// Module-qualified call (requests.get, np.array, os.path.join):
			// attach the import path so resolver can classify externally.
			if importPath, ok := lookupPyImport(c.receiver, imports); ok {
				edge := &graph.Edge{
					From: callerID, To: "unresolved::extern::" + importPath + "::" + c.name,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
				}
				stampReturnUsage(edge, c.returnUsage)
				result.Edges = append(result.Edges, edge)
				continue
			}

			edge := &graph.Edge{
				From: callerID, To: "unresolved::*." + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			switch c.receiver {
			case "self", "cls":
				// `self`/`cls` name the class the call is written in.
				// That is known exactly — no inference, no ambiguity —
				// and it is the single most common typed receiver in
				// idiomatic Python. It is also the shape an inherited
				// method needs: the declaring class may be an ancestor,
				// which a name-only match cannot find.
				if cls := pyEnclosingClassName(c.expr, src); cls != "" {
					edge.Meta = map[string]any{"receiver_type": cls}
				}
			default:
				if recvType, ok := scoped.lookup(ownerFunc, c.receiver); ok {
					edge.Meta = map[string]any{"receiver_type": recvType}
				} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
					stampFactoryChainReceiver(edge, c.receiver,
						resolveChainType(c.receiver, scoped.flatten(ownerFunc), result))
				}
			}
			stampReturnUsage(edge, c.returnUsage)
			result.Edges = append(result.Edges, edge)
			continue
		}

		// Plain call. When the call name itself is bound by a
		// `from X import Y [as Z]` (or `import X as Y`) statement,
		// route the edge through the same module-attributed
		// extern stub the attribute-call branch uses — that's
		// what makes `from numpy import array; array(...)` and
		// `import numpy as np; np.array(...)` both attribute to
		// numpy after the resolver post-pass.
		if importPath, ok := lookupPyImport(c.name, imports); ok {
			edge := &graph.Edge{
				From: callerID, To: "unresolved::extern::" + importPath + "::" + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			stampReturnUsage(edge, c.returnUsage)
			result.Edges = append(result.Edges, edge)
		} else {
			edge := &graph.Edge{
				From: callerID, To: "unresolved::" + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			stampReturnUsage(edge, c.returnUsage)
			result.Edges = append(result.Edges, edge)
		}

		// FastAPI dependency injection: Depends(target) — emit a direct
		// edge from the enclosing function to the first identifier
		// argument of Depends so the target shows up as a caller
		// relationship.
		if c.name == "Depends" && c.expr != nil {
			if dep := firstIdentifierArg(c.expr, src); dep != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From: callerID, To: "unresolved::" + dep,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
					Meta: map[string]any{"via": "fastapi.Depends"},
				})
			}
		}
	}

	// --- Event pub/sub edges ---
	pubsubImportPaths := importPathValues(imports)
	var pubsubEvents []pubsubEvent
	for _, c := range calls {
		if !c.isAttr || c.expr == nil {
			continue
		}
		if ev, ok := detectPyPubsubCall(c.expr, c.name, src, pubsubImportPaths, c.line); ok {
			pubsubEvents = append(pubsubEvents, ev)
		}
	}
	emitPubsubEvents(pubsubEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, "python", result)

	// SQL function call sites (Supabase .rpc('fn'), SQLAlchemy func.fn()).
	emitSQLCallsiteEdges(src, "python",
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	MaybeEnrichDatabricks(filePath, fileID, src, result)

	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)
	captureCeleryDispatch(result, root, filePath, src)
	captureDjangoDescriptors(result, root, filePath, src)
	captureFastAPIRouterRefs(result, root, filePath, src)
	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

// emitFunction handles every function_definition. The parent check
// (function → [decorated_definition →] block → class_definition)
// classifies direct children of a class body — decorated or not — as
// receiver-qualified methods. Nested functions land in the
// free-function bucket.
func (e *PythonExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine1 := def.StartLine + 1

	doc := pyDocstringFromDef(def.Node, src)
	visibility := VisibilityByUnderscore(name)
	decorators := pyDecoratorNodes(def.Node)
	complexity, cognitive, loopDepth := 0, 0, 0
	if def.Node != nil {
		if body := def.Node.ChildByFieldName("body"); body != nil {
			complexity, cognitive, loopDepth = BodyComplexityMetrics(body, "python")
		}
	}

	className := pyDirectClassParent(def.Node, src)
	if className != "" {
		id, ok := disambiguateID(seen, filePath+"::"+className+"."+name, startLine1)
		if !ok {
			return
		}
		node := &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "python", Meta: map[string]any{
				"receiver":   className,
				"signature":  "def " + name + "(...)",
				"visibility": visibility,
			},
		}
		if doc != "" {
			node.Meta["doc"] = doc
		}
		ApplyComplexityMeta(node.Meta, complexity, cognitive, loopDepth)
		if def.Node != nil {
			if rt := extractPyReturnType(def.Node, src); rt != "" {
				node.Meta["return_type"] = rt
			}
		}
		result.Nodes = append(result.Nodes, node)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		typeID := filePath + "::" + className
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})
		emitPyAnnotationEdges(decorators, id, filePath, src, result, annotationSeen)
		emitPyThrowsEdges(def.Node, src, id, filePath, startLine1, result)
		emitPyFunctionShape(id, def.Node, src, filePath, startLine1, result)
		return
	}

	// Free function (top-level or nested in another function).
	id, ok := disambiguateID(seen, filePath+"::"+name, startLine1)
	if !ok {
		return
	}
	meta := map[string]any{
		"signature":  "def " + name + "(...)",
		"visibility": visibility,
	}
	if doc != "" {
		meta["doc"] = doc
	}
	ApplyComplexityMeta(meta, complexity, cognitive, loopDepth)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "python", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	emitPyAnnotationEdges(decorators, id, filePath, src, result, annotationSeen)
	emitPyThrowsEdges(def.Node, src, id, filePath, startLine1, result)
	emitPyFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

// emitPyThrowsEdges walks a function/method body for raise_statement
// nodes and emits an EdgeThrows per distinct exception name.
// `raise SomeError` and `raise SomeError("...")` both surface; bare
// `raise` (re-raise) is skipped because there's no specific type.
// `Origin: ASTInferred` because Python doesn't enforce a checked-
// exception contract — the body scan is a best-effort summary, not
// a proof of every exception that can propagate.
func emitPyThrowsEdges(funcNode *sitter.Node, src []byte, fromID, filePath string, fromLine int, result *parser.ExtractionResult) {
	if funcNode == nil {
		return
	}
	body := funcNode.ChildByFieldName("body")
	if body == nil {
		return
	}
	seen := map[string]bool{}
	pyWalkRaises(body, src, fromID, filePath, fromLine, seen, result)
}

func pyWalkRaises(node *sitter.Node, src []byte, fromID, filePath string, fromLine int, seen map[string]bool, result *parser.ExtractionResult) {
	if node == nil {
		return
	}
	if node.Type() == "raise_statement" {
		name := pyRaiseExceptionName(node, src)
		if name != "" && !seen[name] {
			seen[name] = true
			result.Edges = append(result.Edges, &graph.Edge{
				From:     fromID,
				To:       "unresolved::" + name,
				Kind:     graph.EdgeThrows,
				FilePath: filePath,
				Line:     int(node.StartPoint().Row) + 1,
				Origin:   graph.OriginASTInferred,
			})
		}
		return
	}
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		// Don't descend into nested function/class bodies — their raises
		// belong to those functions, not to us.
		if c == nil {
			continue
		}
		if c.Type() == "function_definition" || c.Type() == "class_definition" || c.Type() == "decorated_definition" || c.Type() == "lambda" {
			continue
		}
		pyWalkRaises(c, src, fromID, filePath, fromLine, seen, result)
	}
}

// pyRaiseExceptionName returns the exception type name from a
// raise_statement. Handles `raise X`, `raise X("msg")`, `raise X(...)`,
// and chained `raise X from Y` (we record X). Returns "" for bare
// `raise` (re-raise).
func pyRaiseExceptionName(raise *sitter.Node, src []byte) string {
	for i, _nc := 0, int(raise.NamedChildCount()); i < _nc; i++ {
		c := raise.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier":
			return c.Content(src)
		case "attribute":
			// e.g. errors.MyError → take the trailing attribute name.
			text := c.Content(src)
			if i := strings.LastIndex(text, "."); i >= 0 {
				return strings.TrimSpace(text[i+1:])
			}
			return strings.TrimSpace(text)
		case "call":
			fn := c.ChildByFieldName("function")
			if fn == nil {
				continue
			}
			text := fn.Content(src)
			if i := strings.LastIndex(text, "."); i >= 0 {
				return strings.TrimSpace(text[i+1:])
			}
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func (e *PythonExtractor) emitClass(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	id := filePath + "::" + name
	decorators := pyDecoratorNodes(def.Node)
	if seen[id] {
		emitPyAnnotationEdges(decorators, id, filePath, src, result, annotationSeen)
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": VisibilityByUnderscore(name), "type_flavor": "class"}
	if doc := pyDocstringFromDef(def.Node, src); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "python",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitPyAnnotationEdges(decorators, id, filePath, src, result, annotationSeen)
	// PEP-695 generic class declarations (`class Foo[T]:`) carry a
	// `type_parameters` child same as functions; reuse the helper.
	emitPyGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
	// Base classes: `class A(B, mod.C, Generic[T])`.
	pyEmitBaseClassEdges(def.Node, src, id, filePath, result)
	// ORM model attribution: emit EdgeModelsTable when the class
	// inherits from a known ORM base (SQLAlchemy / Django).
	detectPythonORMModel(def.Node, src, id, name, filePath, result)
}

// pyEmitBaseClassEdges emits one EdgeExtends per declared base class.
// Python was the only major language in the graph emitting no
// inheritance edges at all — C#, Dart, Erlang, Go, Haskell, Java,
// Kotlin, Pascal, PHP, R, Ruby, Rust and TypeScript all do — which left
// every hierarchy-shaped query (relations hierarchy, override dispatch,
// mixin reachability, abstract-base coverage) blind on Python. 83% of
// classes in a 4,392-file corpus of Django, SQLAlchemy, pydantic, rich,
// flask, httpx and requests declare at least one base: 21,290 of 25,576.
//
// The `superclasses` field is an argument_list, so besides real bases it
// carries keyword arguments (`metaclass=ABCMeta`, PEP-487
// `__init_subclass__` kwargs) and splats (`*bases`). Those are skipped
// rather than minted as phantom parents — a wrong parent edge would
// propagate through every inherited-member walk that trusts it.
func pyEmitBaseClassEdges(classNode *sitter.Node, src []byte, typeID, filePath string, result *parser.ExtractionResult) {
	if classNode == nil {
		return
	}
	supers := classNode.ChildByFieldName("superclasses")
	if supers == nil {
		return
	}
	seen := map[string]bool{}
	for i, _nc := 0, int(supers.NamedChildCount()); i < _nc; i++ {
		arg := supers.NamedChild(i)
		if arg == nil {
			continue
		}
		name, qualified, generic := pyBaseClassName(arg, src)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		edge := &graph.Edge{
			From: typeID, To: "unresolved::" + name, Kind: graph.EdgeExtends,
			FilePath: filePath, Line: int(arg.StartPoint().Row) + 1,
		}
		meta := map[string]any{}
		// Keep the written-out path (`models.Model`) alongside the bare
		// name. 330 class names in the corpus are defined in more than one
		// file, so the bare name alone cannot pick out the right base —
		// the qualifier is what a resolver needs to disambiguate.
		if qualified != "" && qualified != name {
			meta["base_path"] = qualified
		}
		if generic {
			meta["via"] = "generic"
		}
		if len(meta) > 0 {
			edge.Meta = meta
		}
		result.Edges = append(result.Edges, edge)
	}
}

// pyBaseClassName reduces one entry of a class's superclass list to its
// bare base-class name, the written-out path, and whether it was written
// as a generic subscript. Returns an empty name for entries that are not
// base classes at all.
func pyBaseClassName(arg *sitter.Node, src []byte) (name, qualified string, generic bool) {
	switch arg.Type() {
	case "identifier":
		text := strings.TrimSpace(arg.Content(src))
		return text, text, false
	case "attribute":
		// `models.Model`, `abc.ABC` — bind on the trailing name, the same
		// reduction pyRaiseExceptionName uses for `raise mod.Err`.
		text := strings.TrimSpace(arg.Content(src))
		if i := strings.LastIndex(text, "."); i >= 0 {
			return strings.TrimSpace(text[i+1:]), text, false
		}
		return text, text, false
	case "subscript":
		// `Generic[T]`, `Sequence[int]`, `Protocol[T]` — the base is the
		// subscripted value; the type arguments are not parents.
		if v := arg.ChildByFieldName("value"); v != nil {
			n, q, _ := pyBaseClassName(v, src)
			return n, q, true
		}
	}
	// keyword_argument (`metaclass=…`), list_splat (`*bases`),
	// dictionary_splat (`**kwargs`), call — not a base class.
	return "", "", false
}

// pyDecoratorNodes returns the `decorator` AST nodes attached to a
// function_definition or class_definition. In tree-sitter Python the
// decorators wrap the def in a `decorated_definition` parent —
// children of that parent that come before the def are the decorators.
func pyDecoratorNodes(defNode *sitter.Node) []*sitter.Node {
	if defNode == nil {
		return nil
	}
	parent := defNode.Parent()
	if parent == nil || parent.Type() != "decorated_definition" {
		return nil
	}
	var out []*sitter.Node
	for i, _nc := 0, int(parent.ChildCount()); i < _nc; i++ {
		c := parent.Child(i)
		if c != nil && c.Type() == "decorator" {
			out = append(out, c)
		}
	}
	return out
}

// emitPyAnnotationEdges emits an EdgeAnnotated edge per decorator node
// applied to the function/class identified by `fromID`. The decorator
// expression after the `@` is taken as-is; identifier-only decorators
// (`@deprecated`) and call decorators (`@app.route("/x")`) both map
// to the bare callable name.
func emitPyAnnotationEdges(decorators []*sitter.Node, fromID, filePath string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	for _, dec := range decorators {
		name, args := pyDecoratorNameAndArgs(dec, src)
		if name == "" {
			continue
		}
		EmitAnnotationEdge(fromID, "python", name, args, filePath, int(dec.StartPoint().Row)+1, result, seen)
	}
}

// pyDecoratorNameAndArgs reads a `decorator` AST node. Tree-sitter
// Python wraps the post-`@` expression as the named child — typically
// `identifier`, `attribute`, or `call`.
func pyDecoratorNameAndArgs(dec *sitter.Node, src []byte) (string, string) {
	if dec == nil {
		return "", ""
	}
	for i, _nc := 0, int(dec.NamedChildCount()); i < _nc; i++ {
		c := dec.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier", "attribute", "dotted_name":
			return c.Content(src), ""
		case "call":
			fn := c.ChildByFieldName("function")
			args := c.ChildByFieldName("arguments")
			name := ""
			if fn != nil {
				name = fn.Content(src)
			}
			argText := ""
			if args != nil {
				argText = args.Content(src)
				if len(argText) >= 2 && argText[0] == '(' && argText[len(argText)-1] == ')' {
					argText = argText[1 : len(argText)-1]
				}
			}
			return name, argText
		}
	}
	return "", ""
}

// pyDocstringFromDef returns the docstring of a function_definition or
// class_definition node — the first string literal in the body block —
// or "" when none is present. Returns "" for nil nodes.
func pyDocstringFromDef(defNode *sitter.Node, src []byte) string {
	if defNode == nil {
		return ""
	}
	body := defNode.ChildByFieldName("body")
	if body == nil {
		return ""
	}
	for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
		stmt := body.NamedChild(i)
		if stmt == nil {
			continue
		}
		if stmt.Type() != "expression_statement" {
			return ""
		}
		// Walk into expression_statement to find a string literal.
		for j, _nc := 0, int(stmt.NamedChildCount()); j < _nc; j++ {
			c := stmt.NamedChild(j)
			if c == nil {
				continue
			}
			if c.Type() == "string" {
				return ExtractPyDocstring(c.Content(src))
			}
		}
		return ""
	}
	return ""
}

// emitImport handles `import os`, `import os.path`, `import numpy as np`.
// Walks the import_statement node to populate the alias→module map used
// by attribute-call classification.
//
// The query deliberately matches `(import_statement)` with no `name:`
// constraint. That field holds a `dotted_name` for `import os` but an
// `aliased_import` for `import numpy as np`, so a pattern anchored on
// `(dotted_name)` never fired for an aliased import — and `import x as y`
// is how the entire numpy/pandas idiom is written. Walking the children
// here covers both shapes, and every name in the statement.
func (e *PythonExtractor) emitImport(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, imports map[string]string) {
	def, ok := m.Captures["import.def"]
	if !ok || def.Node == nil {
		return
	}
	stmt := def.Node
	emit := func(modulePath, alias string, line int) {
		if modulePath == "" {
			return
		}
		pyEmitImportNode(filePath, fileID, modulePath, alias, line, result)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: "unresolved::import::" + modulePath,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}
	// One statement can bind several modules (`import os.path, numpy as
	// np`), so every name child emits its own import node — the previous
	// single-node-per-statement shape dropped the trailing names.
	for i, _nc := 0, int(stmt.NamedChildCount()); i < _nc; i++ {
		child := stmt.NamedChild(i)
		if child == nil {
			continue
		}
		line := int(child.StartPoint().Row) + 1
		switch child.Type() {
		case "dotted_name":
			dotted := child.Content(src)
			bind := dotted
			if j := strings.Index(dotted, "."); j >= 0 {
				bind = dotted[:j] // `import os.path` binds `os`
			}
			imports[bind] = dotted
			emit(dotted, "", line)
		case "aliased_import":
			var modulePath, alias string
			for j, _jc := 0, int(child.NamedChildCount()); j < _jc; j++ {
				cc := child.NamedChild(j)
				if cc == nil {
					continue
				}
				switch cc.Type() {
				case "dotted_name":
					modulePath = cc.Content(src)
				case "identifier":
					alias = cc.Content(src)
				}
			}
			if alias == "" || modulePath == "" {
				continue
			}
			imports[alias] = modulePath
			emit(modulePath, alias, line)
		}
	}
}

// emitImportFrom handles `from X import Y`, `from X import Y as Z`, and
// `from X import Y, Z`. Each imported name is registered in the per-
// file alias map so attribute-style calls (`Y(...)`) — and any later
// `Z(...)` aliases — resolve through `lookupPyImport` to the right
// dotted module path. Without this, every `from`-imported name fell
// through to the bare-call branch and never attributed to its module.
func (e *PythonExtractor) emitImportFrom(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, imports map[string]string) {
	mod := m.Captures["import.module"]
	pyEmitImportNode(filePath, fileID, mod.Text, "", mod.StartLine+1, result)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + mod.Text,
		Kind: graph.EdgeImports, FilePath: filePath, Line: mod.StartLine + 1,
	})

	def, ok := m.Captures["importfrom.def"]
	if !ok || def.Node == nil {
		return
	}
	stmt := def.Node
	// Walk the statement's named children. The module is the first
	// `dotted_name` (already captured); subsequent `dotted_name`
	// children are imported names. `aliased_import` children carry
	// the optional `as <alias>`. `wildcard_import` (`from X import *`)
	// is intentionally skipped — there's no specific name to bind.
	moduleSeen := false
	for i, _nc := 0, int(stmt.NamedChildCount()); i < _nc; i++ {
		child := stmt.NamedChild(i)
		switch child.Type() {
		case "dotted_name":
			if !moduleSeen {
				moduleSeen = true
				continue
			}
			name := child.Content(src)
			if name == "" {
				continue
			}
			imports[name] = mod.Text + "." + name
		case "aliased_import":
			var importedName, alias string
			for j, _nc := 0, int(child.NamedChildCount()); j < _nc; j++ {
				cc := child.NamedChild(j)
				switch cc.Type() {
				case "dotted_name":
					importedName = cc.Content(src)
				case "identifier":
					alias = cc.Content(src)
				}
			}
			if importedName == "" || alias == "" {
				continue
			}
			imports[alias] = mod.Text + "." + importedName
		}
	}
}

// emitImportFromRelative handles `from . import foo`, `from .sub import
// bar`, `from ..pkg.deep import x` shapes whose `module_name` is a
// `relative_import` rather than a `dotted_name`. Without this branch the
// parser silently dropped every relative import, leaving the graph
// blind to in-project package edges.
//
// The handler maps the relative reference to a project-rooted file-path
// stem so the resolver post-pass `resolveRelativeImports` can land the
// edge on the actual `KindFile` node. Imported names are bound in the
// per-file alias map with a `pyrel::<stem>` marker; the call-resolution
// loop in Extract recognises that shape and emits dataflow edges that
// the post-pass also rewrites onto internal file symbols.
func (e *PythonExtractor) emitImportFromRelative(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, _ map[string]string) {
	relCap := m.Captures["importfrom.rel"]
	if relCap.Node == nil {
		return
	}
	relNode := relCap.Node
	dots := 0
	modPath := ""
	for i, _nc := 0, int(relNode.NamedChildCount()); i < _nc; i++ {
		child := relNode.NamedChild(i)
		switch child.Type() {
		case "import_prefix":
			dots += strings.Count(child.Content(src), ".")
		case "dotted_name":
			modPath = child.Content(src)
		}
	}
	// Some grammar versions place the dot prefix in unnamed children;
	// scan all children for `.` tokens when the named-child walk yields
	// zero dots so we don't silently drop `from . import x`.
	if dots == 0 {
		for i, _nc := 0, int(relNode.ChildCount()); i < _nc; i++ {
			child := relNode.Child(i)
			if child.Type() == "." {
				dots++
			}
		}
	}
	if dots == 0 {
		return
	}

	stem := pyResolveRelativeStem(filePath, dots, modPath)
	if stem == "" {
		return
	}
	importLine := int(relNode.StartPoint().Row) + 1

	// When the relative import names a module (`from .util import a`),
	// emit one EdgeImports edge to the module stem — the imported
	// names live inside that module. When the dot prefix carries no
	// module name (`from . import util`), each imported NAME is itself
	// a submodule of the current package, so emit one edge per name
	// targeting `<stem>/<name>`. This is the same shape the absolute
	// `from numpy import array` path uses (one import edge to the
	// originating module) — the only difference is that for `from .
	// import X` the module name comes from the import list.
	if modPath != "" {
		pyEmitImportNode(filePath, fileID, modPath, "", importLine, result)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: "unresolved::pyrel::" + stem,
			Kind: graph.EdgeImports, FilePath: filePath, Line: importLine,
		})
	}

	def, ok := m.Captures["importfrom_rel.def"]
	if !ok || def.Node == nil {
		return
	}
	stmt := def.Node
	moduleSeen := false
	emittedSub := map[string]bool{}
	emitSubmodule := func(name, alias string) {
		if modPath != "" || name == "" {
			return
		}
		subStem := stem + "/" + name
		if emittedSub[subStem] {
			return
		}
		emittedSub[subStem] = true
		pyEmitImportNode(filePath, fileID, name, alias, importLine, result)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: "unresolved::pyrel::" + subStem,
			Kind: graph.EdgeImports, FilePath: filePath, Line: importLine,
		})
	}
	for i, _nc := 0, int(stmt.NamedChildCount()); i < _nc; i++ {
		child := stmt.NamedChild(i)
		switch child.Type() {
		case "relative_import":
			moduleSeen = true
			continue
		case "dotted_name":
			if !moduleSeen {
				moduleSeen = true
				continue
			}
			name := child.Content(src)
			emitSubmodule(name, "")
		case "aliased_import":
			var importedName, alias string
			for j, _nc := 0, int(child.NamedChildCount()); j < _nc; j++ {
				cc := child.NamedChild(j)
				switch cc.Type() {
				case "dotted_name":
					importedName = cc.Content(src)
				case "identifier":
					alias = cc.Content(src)
				}
			}
			emitSubmodule(importedName, alias)
		}
	}
}

// pyResolveRelativeStem maps a (filePath, dotCount, modPath) triple to
// the project-rooted file-path stem the import targets. Returns "" when
// the dot prefix would walk to or above the repo root — CPython raises
// ImportError on the same shape, so we refuse rather than invent a
// project-root pseudo-package.
//
//	pyResolveRelativeStem("app/main.py", 1, "")        → "app"
//	pyResolveRelativeStem("app/main.py", 1, "util")    → "app/util"
//	pyResolveRelativeStem("app/sub/x.py", 2, "parent") → "app/parent"
//	pyResolveRelativeStem("app/sub/x.py", 3, "y")      → "" (above root)
//	pyResolveRelativeStem("main.py", 1, "x")           → "" (no package)
func pyResolveRelativeStem(filePath string, dots int, modPath string) string {
	if dots <= 0 || filePath == "" {
		return ""
	}
	dir := ""
	if i := strings.LastIndex(filePath, "/"); i >= 0 {
		dir = filePath[:i]
	}
	levels := 0
	if dir != "" {
		levels = strings.Count(dir, "/") + 1
	}
	// `dots-1` is the number of parent-package walks. Walking to or
	// past `levels` means we'd land at (or above) the implicit repo
	// root with no real package — refuse rather than fabricate one.
	if dots-1 >= levels {
		return ""
	}
	for i := 1; i < dots; i++ {
		if j := strings.LastIndex(dir, "/"); j >= 0 {
			dir = dir[:j]
		} else {
			dir = ""
		}
	}
	if modPath == "" {
		return dir
	}
	suffix := strings.ReplaceAll(modPath, ".", "/")
	if dir == "" {
		return suffix
	}
	return dir + "/" + suffix
}

// pyEmitImportNode appends a KindImport node + Defines edge for a
// Python `import X` or `from X import …` statement. is_external is
// true when the path doesn't begin with a relative prefix and isn't
// in the small stdlib whitelist — close enough for routing UX while
// being trivially cheap.
func pyEmitImportNode(filePath, fileID, importPath, alias string, line int, result *parser.ExtractionResult) {
	if importPath == "" {
		return
	}
	importNodeID := filePath + "::import::" + importPath
	meta := map[string]any{
		"path":        importPath,
		"is_external": isExternalPyImport(importPath),
	}
	if alias != "" {
		meta["alias"] = alias
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID:        importNodeID,
		Kind:      graph.KindImport,
		Name:      pyImportDisplayName(importPath),
		FilePath:  filePath,
		StartLine: line,
		EndLine:   line,
		Language:  "python",
		Meta:      meta,
	})
	// File → import-node uses EdgeContains (the file contains an
	// import statement; it doesn't define the imported module).
	// GetFileSubGraph walks EdgeDefines ∪ EdgeContains to recover the
	// full file neighbourhood.
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: importNodeID,
		Kind: graph.EdgeContains, FilePath: filePath, Line: line,
	})
}

func isExternalPyImport(path string) bool {
	if path == "" || strings.HasPrefix(path, ".") {
		return false
	}
	// Best-effort stdlib check: a small whitelist of common stdlib
	// roots avoids tagging them as external. False negatives are
	// harmless (the agent can still query the import), false
	// positives would mislead "is this a third-party dep?" queries.
	first := path
	if i := strings.Index(path, "."); i >= 0 {
		first = path[:i]
	}
	switch first {
	case "os", "sys", "io", "re", "json", "math", "random", "time",
		"datetime", "collections", "itertools", "functools", "typing",
		"asyncio", "logging", "pathlib", "subprocess", "threading",
		"multiprocessing", "abc", "enum", "dataclasses", "contextlib",
		"copy", "tempfile", "shutil", "string", "struct", "hashlib",
		"unittest", "ast", "types", "warnings", "weakref", "inspect":
		return false
	}
	return true
}

func pyImportDisplayName(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (e *PythonExtractor) emitTopLevelVar(v pyDeferredVar, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := v.name
	if v.node == nil {
		return
	}
	// An `assignment` is never a direct child of `module` — tree-sitter
	// wraps every bare expression in an `expression_statement` first. The
	// old check compared the assignment's immediate parent against
	// "module", which no module-level assignment could ever satisfy, so
	// this whole function was unreachable and Python emitted zero
	// variable nodes for any file.
	parent := v.node.Parent()
	if parent != nil && parent.Type() == "expression_statement" {
		parent = parent.Parent()
	}
	if parent == nil || parent.Type() != "module" {
		return
	}
	id := filePath + "::" + name
	if seen[id] || strings.HasPrefix(name, "_") {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: v.startLine + 1, EndLine: v.endLine + 1,
		Language: "python",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: v.startLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// pyDirectClassParent returns the enclosing class name when fn is a
// direct child of a class_definition's body block. Decorated methods
// (`@property def encoding`, `@contextmanager def stream`, …) are
// wrapped in a `decorated_definition` node between the def and the
// class body — the walk hops over that wrapper so they classify as
// methods of the class, exactly like their undecorated siblings.
// Without the hop, two same-named decorated members of different
// classes in one file (httpx's Headers.encoding / Response.encoding)
// collapsed into bare colliding `<file>::<name>` IDs, and every
// consumer addressing the class-qualified ID silently landed on the
// wrong declaration. Returns "" for nested functions and top-level
// functions.
func pyDirectClassParent(fn *sitter.Node, src []byte) string {
	if fn == nil {
		return ""
	}
	parent := fn.Parent()
	if parent != nil && parent.Type() == "decorated_definition" {
		parent = parent.Parent()
	}
	if parent == nil || parent.Type() != "block" {
		return ""
	}
	grand := parent.Parent()
	if grand == nil || grand.Type() != "class_definition" {
		return ""
	}
	nameNode := grand.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return nameNode.Content(src)
}

// firstIdentifierArg returns the string content of the first positional
// argument to a Python call_expression when that argument is a bare
// identifier (function name or class name), or "" otherwise. Used for
// FastAPI's Depends(target) where we want the target, not Depends
// itself, to show up as the called symbol. Non-identifier arguments —
// lambdas, attribute access, calls — are skipped because they can't
// be statically resolved to a graph node.
func firstIdentifierArg(callNode *sitter.Node, src []byte) string {
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		arg := args.NamedChild(i)
		if arg == nil {
			continue
		}
		// Skip keyword arguments — Depends(use_cache=False, ...) shouldn't
		// produce a call edge to `False`.
		if arg.Type() == "keyword_argument" {
			continue
		}
		if arg.Type() == "identifier" {
			return arg.Content(src)
		}
		return ""
	}
	return ""
}

// extractPyReturnType walks a function_definition node for a return_type child
// (the `-> Type` annotation) and returns the normalized type name.
func extractPyReturnType(funcNode *sitter.Node, src []byte) string {
	for i, _nc := 0, int(funcNode.NamedChildCount()); i < _nc; i++ {
		child := funcNode.NamedChild(i)
		if child.Type() == "type" {
			// Check if preceding sibling token is "->".
			// In tree-sitter Python grammar, the return type is a "type" child
			// that appears after the parameters.
			return normalizePyTypeName(child.Content(src))
		}
	}
	return ""
}

// normalizePyTypeName strips Optional[], List[], etc. and skips builtins.
func normalizePyTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Strip Optional[...], List[...], etc.
	if idx := strings.Index(t, "["); idx > 0 {
		t = t[:idx]
	}
	switch t {
	case "int", "float", "str", "bool", "bytes", "None", "list", "dict", "set", "tuple", "object":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return ""
	}
	return t
}

// lookupPyImport resolves a dotted Python receiver to an import path.
// Tries the full receiver first, then progressively shorter prefixes
// (os.path → os), so `os.path.join(x)` with `import os.path` finds the
// right module.
func lookupPyImport(receiver string, imports map[string]string) (string, bool) {
	if p, ok := imports[receiver]; ok {
		return p, true
	}
	for i := strings.LastIndex(receiver, "."); i > 0; i = strings.LastIndex(receiver[:i], ".") {
		prefix := receiver[:i]
		if p, ok := imports[prefix]; ok {
			return p, true
		}
	}
	if i := strings.Index(receiver, "."); i > 0 {
		if p, ok := imports[receiver[:i]]; ok {
			return p, true
		}
	}
	return "", false
}
