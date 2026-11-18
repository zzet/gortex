package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/java"
)

// qJavaAll is a single tree-sitter query alternating over every pattern
// the Java extractor needs. One tree walk per file replaces the 13
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set. Class/interface/enum
// membership is resolved via a parent walk on the captured node rather
// than nested queries — same behaviour, one cursor pass.
const qJavaAll = `
[
  (class_declaration
    name: (identifier) @class.name) @class.def

  (interface_declaration
    name: (identifier) @iface.name) @iface.def

  (annotation_type_declaration
    name: (identifier) @iface.name) @iface.def

  (enum_declaration
    name: (identifier) @enum.name) @enum.def

  (object_creation_expression
    (class_body)) @anon.def

  (method_declaration
    name: (identifier) @method.name) @method.def

  (constructor_declaration
    name: (identifier) @ctor.name) @ctor.def

  (enum_constant
    name: (identifier) @enum_member.name) @enum_member.def

  (field_declaration
    type: (_) @fvar.type
    declarator: (variable_declarator
      name: (identifier) @fvar.name)) @fvar.def

  (local_variable_declaration
    type: (_) @lvar.type
    declarator: (variable_declarator
      name: (identifier) @lvar.name)) @lvar.def

  (import_declaration
    (scoped_identifier) @import.path) @import.def

  (method_invocation
    name: (identifier) @call.name) @call.expr

  (method_invocation
    object: (_) @callm.receiver
    name: (identifier) @callm.method) @callm.expr
]
`

// JavaExtractor extracts Java source files into graph nodes and edges.
type JavaExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
	// javaInvokers / javaInvokerMethods configure the Temporal invoker
	// detector (corporate, per-repo), installed via SetTemporalInvokers /
	// ConfigureTemporalJavaInvokers. javaInvokers holds invoker class
	// simple-names; javaInvokerMethods the dispatch method names (defaults to
	// javaInvokerDefaultMethods when nil). Empty javaInvokers → detection OFF.
	javaInvokers       map[string]bool
	javaInvokerMethods map[string]bool
}

func NewJavaExtractor() *JavaExtractor {
	lang := java.GetLanguage()
	return &JavaExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qJavaAll, lang),
	}
}

// SetTemporalInvokers installs the per-repo corporate Temporal invoker config:
// `invokers` are invoker class simple-names; `methods` overrides the default
// dispatch method names (nil/empty → defaults). Stored as sets for O(1) lookup.
// Called once during extractor registration; must not race with Extract.
func (e *JavaExtractor) SetTemporalInvokers(invokers, methods []string) {
	e.javaInvokers = toLowerableSet(invokers, false)
	if len(methods) == 0 {
		e.javaInvokerMethods = nil
	} else {
		e.javaInvokerMethods = toLowerableSet(methods, false)
	}
}

// toLowerableSet builds a presence set; when lower is true keys are lower-cased.
func toLowerableSet(in []string, lower bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	m := make(map[string]bool, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			if lower {
				s = strings.ToLower(s)
			}
			m[s] = true
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func (e *JavaExtractor) Language() string     { return "java" }
func (e *JavaExtractor) Extensions() []string { return []string{".java"} }

// --- Deferred match buffers ----------------------------------------

type javaDeferredCall struct {
	name       string // method name
	receiver   string // selector receiver text (empty for plain call)
	line       int    // 1-based call_expression start line
	isSelector bool
	// tempStartWorkflow is the workflow type name when this call starts a
	// Temporal workflow (`client.newWorkflowStub(OrderWorkflow.class, …)`
	// or `newUntypedWorkflowStub("OrderWorkflow")`). A via=temporal.start
	// edge keyed by this name is emitted in the post-pass, and the
	// resolver cross-resolves it to the workflow's implementation (which
	// may live in a Go repo).
	tempStartWorkflow string
	// tempSignalKind / tempSignalName carry an outbound signal-send /
	// query-call on an untyped WorkflowStub (stub.signal("name", …) /
	// stub.query("name", …)). Emitted in the post-pass only when the
	// receiver's inferred type is WorkflowStub, to keep the common
	// "signal"/"query" method names from false-matching.
	tempSignalKind string
	tempSignalName string
	// returnUsage is how the call site consumes the return value
	// (graph.ReturnUsage* label), classified at capture time and
	// stamped as edge Meta on the EdgeCalls emitted for this site.
	returnUsage string
	// callNode is the method_invocation node, retained for argument
	// inspection by the Temporal invoker detector (emitJavaTemporalInvoker).
	callNode *sitter.Node
}

// javaDeferredVar buffers a variable declaration for the post-pass
// type-environment build. The legacy extractor materialised the env in
// three ordered tiers (lvar explicit, then fvar explicit-no-overwrite,
// then lvar `new Foo()` inference); document-order dispatch alone can't
// reproduce that precedence, so we buffer and resolve at the end.
type javaDeferredVar struct {
	name     string
	explicit string // normalized type from explicit annotation, "" if none
	defNode  *sitter.Node
	isLocal  bool
}

// javaTypeUse buffers a local-variable / field type annotation whose
// EdgeTypedAs is emitted after funcRanges is built, so the reference is
// attributed to its enclosing method (falling back to the file node).
// The raw annotation text is kept verbatim — emitJavaTypeUseEdges
// canonicalizes it (strip generics / array / package prefix) and skips
// primitives, mirroring the param / return type edges.
type javaTypeUse struct {
	typeText string
	line     int // 1-based
}

func (e *JavaExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "java",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)
	ifaceMethods := make(map[string][]string)  // interface name → declared method names
	rnModules := extractJavaRNModuleNames(src) // class → React Native JS module name

	var calls []javaDeferredCall
	var varBuf []javaDeferredVar
	var typeUses []javaTypeUse

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["class.def"] != nil:
			e.emitClass(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["iface.def"] != nil:
			e.emitInterface(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["enum.def"] != nil:
			e.emitEnum(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["anon.def"] != nil:
			e.emitAnonymousClass(m, filePath, fileID, src, result, seen)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, fileID, src, result, seen, annotationSeen, ifaceMethods, rnModules)

		case m.Captures["ctor.def"] != nil:
			e.emitConstructor(m, filePath, fileID, src, result, seen)

		case m.Captures["enum_member.def"] != nil:
			e.emitEnumMember(m, filePath, src, result)

		case m.Captures["fvar.def"] != nil:
			e.emitField(m, filePath, fileID, src, result, seen)
			// Always buffer for tenv post-pass — interface and enum
			// fields contribute to the type env even though they're
			// not emitted as graph nodes.
			varBuf = append(varBuf, javaDeferredVar{
				name:     m.Captures["fvar.name"].Text,
				explicit: normalizeJavaTypeName(m.Captures["fvar.type"].Text),
				defNode:  m.Captures["fvar.def"].Node,
				isLocal:  false,
			})
			// A typed field (`Foo bar;`) references type Foo — buffer it
			// so find_usages(Foo) surfaces the declaration without an LSP.
			typeUses = append(typeUses, javaTypeUse{
				typeText: m.Captures["fvar.type"].Text,
				line:     m.Captures["fvar.def"].StartLine + 1,
			})

		case m.Captures["lvar.def"] != nil:
			varBuf = append(varBuf, javaDeferredVar{
				name:     m.Captures["lvar.name"].Text,
				explicit: normalizeJavaTypeName(m.Captures["lvar.type"].Text),
				defNode:  m.Captures["lvar.def"].Node,
				isLocal:  true,
			})
			// A typed local (`HttpResponse resp = …`) references its type
			// in declaration position — buffer the EdgeTypedAs so it's a
			// first-class cross-file reference even without an LSP.
			typeUses = append(typeUses, javaTypeUse{
				typeText: m.Captures["lvar.type"].Text,
				line:     m.Captures["lvar.def"].StartLine + 1,
			})

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, result)

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			method := m.Captures["callm.method"].Text
			dc := javaDeferredCall{
				name:        method,
				receiver:    m.Captures["callm.receiver"].Text,
				line:        expr.StartLine + 1,
				isSelector:  true,
				returnUsage: classifyReturnUsage(expr.Node, src, javaReturnUsageSpec),
				callNode:    expr.Node,
			}
			if wf := javaTemporalStartWorkflowName(expr.Node, method, src); wf != "" {
				dc.tempStartWorkflow = wf
			}
			if sk, sn := javaTemporalSignalQuery(expr.Node, method, src); sk != "" {
				dc.tempSignalKind, dc.tempSignalName = sk, sn
			}
			calls = append(calls, dc)

		case m.Captures["call.expr"] != nil:
			// The plain-call pattern also fires for the inner `bar` of a
			// selector call `foo.bar()`, which the callm case already emits
			// WITH the receiver. Emitting a second receiver-less edge for the
			// same site collides on the edge key (which excludes Meta) and
			// clobbers the selector edge's receiver_type — sending the call to
			// a same-named method in the caller's own class instead of the
			// receiver's type. Skip it; only true bare calls (no object) fall
			// through here.
			expr := m.Captures["call.expr"]
			if expr.Node != nil && expr.Node.ChildByFieldName("object") != nil {
				return
			}
			calls = append(calls, javaDeferredCall{
				name:        m.Captures["call.name"].Text,
				line:        expr.StartLine + 1,
				returnUsage: classifyReturnUsage(expr.Node, src, javaReturnUsageSpec),
			})
		}
	})

	// Stamp interface method names onto interface nodes' Meta["methods"]
	// for IMPLEMENTS inference.
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

	// Build type environment in the same precedence the legacy code used:
	//   1. lvar Tier 0 — explicit annotation (overwrites prior key)
	//   2. fvar Tier 0 — explicit annotation (no overwrite)
	//   3. lvar Tier 1 — walk defNode for object_creation_expression
	tenv := make(typeEnv)
	for _, v := range varBuf {
		if v.isLocal && v.explicit != "" {
			tenv[v.name] = v.explicit
		}
	}
	for _, v := range varBuf {
		if v.isLocal {
			continue
		}
		if v.explicit == "" {
			continue
		}
		if _, exists := tenv[v.name]; exists {
			continue
		}
		tenv[v.name] = v.explicit
	}
	for _, v := range varBuf {
		if !v.isLocal {
			continue
		}
		if _, exists := tenv[v.name]; exists {
			continue
		}
		if v.defNode == nil {
			continue
		}
		walkNodes(v.defNode, func(n *sitter.Node) {
			if n.Type() == "object_creation_expression" {
				typeName := inferTypeFromJavaNewExpr(n, src)
				if typeName != "" {
					tenv[v.name] = typeName
				}
			}
		})
	}

	// All function/method nodes have been emitted; map call sites to
	// their enclosing definition.
	funcRanges := buildFuncRanges(result)

	// Type-use edges: a `Foo x = …` local declaration or a `Foo bar;`
	// field declaration references type Foo. Attributed to the enclosing
	// method (fallback: the file node) so find_usages(Foo) surfaces every
	// declaration site without an LSP. EdgeTypedAs mirrors the param /
	// return type edges; the resolver lands it cross-file via the same
	// name-based pass. Params / return types already emit their own
	// EdgeTypedAs via emitJavaFunctionShape, so this only covers the
	// local / field annotation positions that were previously edge-less.
	for _, tu := range typeUses {
		ownerID := findEnclosingFunc(funcRanges, tu.line)
		if ownerID == "" {
			ownerID = fileID
		}
		emitJavaTypeUseEdges(ownerID, tu.typeText, filePath, tu.line, result)
	}

	// Expression-position type references — instantiation (`new Foo()`),
	// inheritance (`extends Foo` / `implements Bar`), casts / type-tests
	// (`(Foo) x`, `x instanceof Foo`), static / constant access
	// (`Foo.CONST`, `Foo.class`, `Foo.staticMethod()`), and annotations
	// (`@Foo`). These are the find_usages(Foo) hits the declaration passes
	// above don't cover. Attributed to the enclosing method via funcRanges.
	emitJavaReferenceForms(root, src, filePath, fileID, funcRanges, result)

	// React Native native event emits (getJSModule(...).emit / sendEvent helper)
	// pair with the JS addListener handler on the same rn_native_event topic.
	mineRNJVMEmits(src, func(line int) string {
		return findEnclosingFunc(funcRanges, line)
	}, filePath, "java", result)

	// @Value("${key:Default}") fields → their literal defaults, for invoker
	// dispatch through a Spring-injected field (built only when invoker
	// detection is configured).
	var valueFields map[string]string
	if len(e.javaInvokers) > 0 {
		valueFields = javaCollectValueFields(varBuf, src)
	}
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		// Temporal invoker dispatch (`invoker.invokeAsync("Wf", …)`): emit a
		// via=temporal.stub edge instead of the generic call edge. No-op unless
		// java_temporal_invokers is configured.
		if e.emitJavaTemporalInvoker(c, callerID, tenv, valueFields, filePath, src, result) {
			continue
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		if c.isSelector {
			// `this.<field>` receivers resolve through the enclosing class's
			// field types exactly like the bare `<field>` spelling: strip the
			// `this.` qualifier and look the field up in the type env (field
			// declarations already seed tenv). The two spellings must behave
			// identically so a DI-injected repository called as `this.owners.x()`
			// and `owners.x()` both stamp the field's declared type.
			recv := c.receiver
			if field, ok := strings.CutPrefix(recv, "this."); ok && field != "" && !strings.ContainsAny(field, ".([") {
				recv = field
			}
			if recvType, ok := tenv[recv]; ok {
				edge.Meta = map[string]any{"receiver_type": recvType}
			} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
				stampFactoryChainReceiver(edge, c.receiver, resolveChainType(c.receiver, tenv, result))
			}
		}
		stampReturnUsage(edge, c.returnUsage)
		result.Edges = append(result.Edges, edge)

		// Temporal workflow START (consumer side): emit a via=temporal.start
		// edge keyed by the workflow type name. The resolver cross-resolves
		// it to the registered workflow — which may be implemented in a Go
		// repo — so get_callers on that workflow surfaces this Java service.
		if c.tempStartWorkflow != "" {
			startEdge := &graph.Edge{
				From: callerID, To: "unresolved::temporal::workflow::" + c.tempStartWorkflow,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
				Meta: map[string]any{
					"via":           "temporal.start",
					"temporal_kind": "workflow",
					"temporal_name": c.tempStartWorkflow,
				},
			}
			stampReturnUsage(startEdge, c.returnUsage)
			result.Edges = append(result.Edges, startEdge)
		}
		// Outbound signal-send / query-call on an untyped WorkflowStub,
		// symmetric with the Go side (#81). Gated on the receiver's inferred
		// type being WorkflowStub so the common "signal"/"query" method
		// names don't false-match arbitrary code.
		if c.tempSignalKind != "" && tenv[c.receiver] == "WorkflowStub" {
			via := "temporal.signal-send"
			if c.tempSignalKind == "query" {
				via = "temporal.query-call"
			}
			signalEdge := &graph.Edge{
				From: callerID, To: "unresolved::*." + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
				Meta: map[string]any{
					"via":           via,
					"temporal_kind": c.tempSignalKind,
					"temporal_name": c.tempSignalName,
				},
			}
			stampReturnUsage(signalEdge, c.returnUsage)
			result.Edges = append(result.Edges, signalEdge)
		}
	}

	// React Native Fabric / Paper view managers: a class with @ReactProp
	// methods backs a JS component. Emit a component node so the Fabric
	// synthesizer can link it to the codegen TS spec.
	for _, fm := range extractJavaFabricManagers(src, rnModules) {
		id := filePath + "::fabric:" + fm.component
		if seen[id] {
			continue
		}
		seen[id] = true
		node := &graph.Node{
			ID: id, Kind: graph.KindType, Name: fm.component,
			FilePath: filePath, StartLine: fm.line, EndLine: fm.line,
			Language: "java",
			Meta:     map[string]any{"fabric_component": fm.component, "fabric_native": "java", "type_flavor": "component", "ui_component": "react"},
		}
		if len(fm.props) > 0 {
			node.Meta["fabric_props"] = fm.props
		}
		result.Nodes = append(result.Nodes, node)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: fm.line,
		})
	}

	stampScopePkg(result, javaPackageName(root, src))
	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)

	// Spring @Value / @ConfigurationProperties property reads → resolver hints
	// for the application.yml/.properties config-key graph.
	mineSpringConfigReads(src, result)

	captureSpringEvents(result, root, filePath, src)

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *JavaExtractor) emitClass(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": javaVisibility(def.Node, src, VisibilityPackage)}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	// Direct superclass — populated for the scope-based static
	// resolver's super-method walk. `superclass` field is the Java
	// tree-sitter name for `extends X`.
	if parent := extractJavaParentClass(def.Node, src); parent != "" {
		meta["scope_parent"] = parent
	}
	meta["type_flavor"] = "class"
	anns := javaCollectAnnotations(def.Node, src)
	node := &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	}
	stampJavaSpringConfigAnnotations(node, anns)
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitJavaAnnotationEdges(anns, id, filePath, result, annotationSeen)
	emitJavaGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
	// JPA model attribution: @Entity / @Table → EdgeModelsTable.
	emitJavaORMEdges(def.Node, src, id, name, filePath, result)
}

// emitAnonymousClass indexes a Java anonymous class — `new T() { ...members }`
// — as a synthetic KindType node with an EdgeExtends to the instantiated type
// T. The anonymous subclass implicitly extends (for a class) or implements
// (for an interface) T; we cannot tell which at extraction time, so we emit
// `extends` and let the interface→implementation resolver, which handles both,
// bridge T's methods to the overrides. Without a node for the anonymous class
// the override site is invisible to call resolution.
func (e *JavaExtractor) emitAnonymousClass(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["anon.def"]
	baseType := inferTypeFromJavaNewExpr(def.Node, src)
	if baseType == "" {
		return
	}
	line := def.StartLine + 1
	name := fmt.Sprintf("%s$anon@%d", baseType, line)
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: line, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     map[string]any{"anonymous": true, "scope_parent": baseType, "type_flavor": "anonymous_class"},
	})
	result.Edges = append(result.Edges,
		&graph.Edge{From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line},
		&graph.Edge{From: id, To: "unresolved::" + baseType, Kind: graph.EdgeExtends, FilePath: filePath, Line: line, Origin: graph.OriginASTInferred},
	)
}

// javaVisibility scans the `modifiers` child of a Java declaration for
// a public/private/protected token. Returns defaultVis when no
// modifier is present (e.g. package-private at top level).
func javaVisibility(decl *sitter.Node, src []byte, defaultVis string) string {
	if decl == nil {
		return defaultVis
	}
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		c := decl.Child(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j, _nc := 0, int(c.ChildCount()); j < _nc; j++ {
			tok := c.Child(j)
			if tok == nil {
				continue
			}
			switch tok.Type() {
			case "public":
				return VisibilityPublic
			case "private":
				return VisibilityPrivate
			case "protected":
				return VisibilityProtected
			}
		}
	}
	return defaultVis
}

func (e *JavaExtractor) emitInterface(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["iface.name"].Text
	def := m.Captures["iface.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": javaVisibility(def.Node, src, VisibilityPackage)}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	meta["type_flavor"] = "interface"
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitJavaAnnotationEdges(javaCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
	emitJavaGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

func (e *JavaExtractor) emitEnum(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["enum.name"].Text
	def := m.Captures["enum.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"kind":        "enum",
		"type_flavor": "enum",
		"visibility":  javaVisibility(def.Node, src, VisibilityPackage),
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	emitJavaAnnotationEdges(javaCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
}

func (e *JavaExtractor) emitEnumMember(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult) {
	def := m.Captures["enum_member.def"]
	enumNode := findEnclosingJavaContainer(def.Node, "enum_declaration")
	if enumNode == nil {
		return
	}
	enumName := javaIdentifierName(enumNode, src)
	if enumName == "" {
		return
	}
	memberName := m.Captures["enum_member.name"].Text
	enumID := filePath + "::" + enumName
	memberID := enumID + "." + memberName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: memberID, Kind: graph.KindEnumMember, Name: memberName,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "java",
		Meta:      map[string]any{"receiver": enumName, "enum": enumID},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: memberID, To: enumID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *JavaExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, ifaceMethods map[string][]string, rnModules map[string]string) {
	name := m.Captures["method.name"].Text
	def := m.Captures["method.def"]
	startLine1 := def.StartLine + 1
	lineKey := filePath + "::_method_L" + fmt.Sprint(startLine1)
	if seen[lineKey] {
		return
	}
	seen[lineKey] = true

	enclosing := findEnclosingJavaContainerAny(def.Node, "class_declaration", "interface_declaration", "enum_declaration")

	// Inside a class — emit as receiver-qualified method (the only
	// container the legacy extractor's class-method query matched).
	if enclosing != nil && enclosing.Type() == "class_declaration" {
		className := javaIdentifierName(enclosing, src)
		if className == "" {
			return
		}
		id := filePath + "::" + className + "." + name
		if seen[id] {
			id = filePath + "::" + className + "." + name + "_L" + fmt.Sprint(startLine1)
		}
		if seen[id] {
			return
		}
		seen[id] = true
		node := &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "java",
			Meta: map[string]any{
				"receiver":    className,
				"scope_class": className,
				"visibility":  javaVisibility(def.Node, src, VisibilityPackage),
			},
		}
		isSpringBeanMethod := def.Node != nil && javaMethodHasAnnotation(def.Node, src, "Bean")
		if def.Node != nil {
			if rt := extractJavaMethodReturnType(def.Node, src); rt != "" {
				node.Meta["return_type"] = rt
			}
			if isSpringBeanMethod {
				if params := javaParamsSource(def.Node, src); params != "" {
					node.Meta["params_src"] = params
				}
			}
		}
		if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
			node.Meta["doc"] = doc
		}
		anns := javaCollectAnnotations(def.Node, src)
		stampJavaSpringConfigAnnotations(node, anns)
		if def.Node != nil {
			if body := def.Node.ChildByFieldName("body"); body != nil {
				StampFunctionMetrics(node, body, "java")
			}
		}
		// React Native: an @ReactMethod method is callable from JS as
		// NativeModules.<module>.<method>(...). Stamp the JS module +
		// method so the bridge synthesizer can land the JS call here.
		if def.Node != nil && javaHasReactMethod(anns) {
			node.Meta["rn_method"] = name
			if mod := rnModules[className]; mod != "" {
				node.Meta["rn_module"] = mod
			} else {
				node.Meta["rn_module"] = className
			}
		}
		result.Nodes = append(result.Nodes, node)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		classID := filePath + "::" + className
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})

		// Spring @Bean factory methods: when a method in a
		// @Configuration class is decorated with @Bean, Spring calls
		// it at context-init to produce a bean of the method's return
		// type. Emit an EdgeProvides from the config class to the
		// method so the indexer's DI post-pass links consumers typed
		// as the return type back to this factory.
		if isSpringBeanMethod {
			if rt, _ := node.Meta["return_type"].(string); rt != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From:     classID,
					To:       id,
					Kind:     graph.EdgeProvides,
					FilePath: filePath,
					Line:     startLine1,
					Meta: map[string]any{
						graph.MetaDIProvidesFor: rt,
						graph.MetaDIBinding:     graph.DIBindingBean,
					},
				})
			}
		}
		emitJavaAnnotationEdges(anns, id, filePath, result, annotationSeen)
		emitJavaThrowsEdges(def.Node, src, id, filePath, startLine1, result)
		emitJavaFunctionShape(id, def.Node, src, filePath, startLine1, result)
		return
	}

	// Interface method — record the name for IMPLEMENTS inference and
	// emit a flat method node (mirrors legacy fallback).
	if enclosing != nil && enclosing.Type() == "interface_declaration" {
		ifaceName := javaIdentifierName(enclosing, src)
		if ifaceName != "" {
			ifaceMethods[ifaceName] = append(ifaceMethods[ifaceName], name)
		}
	}

	// Fallback: enum method, interface method, or method outside any
	// container — emit flat (legacy `javaQMethod` fallback path).
	id := filePath + "::" + name
	if seen[id] {
		id = filePath + "::" + name + "_L" + fmt.Sprint(startLine1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	flatNode := &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "java",
	}
	// Interface and enum method nodes keep the flat `<file>::<name>` ID (the
	// bench arm and existing graphs depend on it) but carry their declaring
	// type as Meta["receiver"] so the resolver's exact-type method-call passes
	// (nodeReceiverType == receiver_type) can bind to them the same way they
	// bind to class methods.
	if enclosing != nil {
		switch enclosing.Type() {
		case "interface_declaration", "enum_declaration":
			if recv := javaIdentifierName(enclosing, src); recv != "" {
				flatNode.Meta = map[string]any{"receiver": recv}
			}
		}
	}
	result.Nodes = append(result.Nodes, flatNode)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	emitJavaAnnotationEdges(javaCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
	emitJavaFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

func (e *JavaExtractor) emitConstructor(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["ctor.def"]
	startLine1 := def.StartLine + 1
	lineKey := filePath + "::_ctor_L" + fmt.Sprint(startLine1)
	if seen[lineKey] {
		return
	}
	seen[lineKey] = true

	enclosing := findEnclosingJavaContainer(def.Node, "class_declaration")
	if enclosing == nil {
		// Legacy fallback path — constructor outside a class. The
		// tree-sitter-java grammar makes this unreachable in valid
		// source, but keep parity with the old extractor.
		name := m.Captures["ctor.name"].Text
		id := filePath + "::" + name + ".<init>"
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name + ".<init>",
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "java",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		return
	}

	className := javaIdentifierName(enclosing, src)
	if className == "" {
		return
	}
	id := filePath + "::" + className + ".<init>"
	if seen[id] {
		id = filePath + "::" + className + ".<init>_L" + fmt.Sprint(startLine1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	// Stash param-type text so the indexer's Spring-bean post-pass can
	// match consumers to factory methods by type name.
	meta := map[string]any{"receiver": className}
	if params := javaParamsSource(def.Node, src); params != "" {
		meta["params_src"] = params
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: className + ".<init>",
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	classID := filePath + "::" + className
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
	})
	// Constructor params land in the same shape as method params:
	// EdgeParamOf + EdgeTypedAs are how Spring's @Autowired CDI
	// post-pass figures out which beans flow in.
	emitJavaFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

func (e *JavaExtractor) emitField(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["fvar.def"]
	enclosing := findEnclosingJavaContainer(def.Node, "class_declaration")
	if enclosing == nil {
		return
	}
	className := javaIdentifierName(enclosing, src)
	if className == "" {
		return
	}
	name := m.Captures["fvar.name"].Text
	id := filePath + "::" + className + "." + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"receiver":   className,
		"visibility": javaVisibility(def.Node, src, VisibilityPackage),
	}
	if t := def.Node.ChildByFieldName("type"); t != nil {
		meta["field_type"] = strings.TrimSpace(t.Content(src))
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	// A `static final String X = "literal"` is a Java string constant. Stamp
	// its literal under the same Meta["value"] key Go string constants use, so
	// the resolver's constVal index can resolve a const-ref Temporal dispatch
	// (`invoker.invokeAsync(Constants.X, …)`) cross-language to the registered
	// Go workflow/activity. Keyed by the field NAME (the dispatch records the
	// trailing identifier).
	if v, ok := javaStaticFinalStringValue(def.Node, src); ok {
		meta["value"] = v
	}
	// `static final` is a Java compile-time constant — classify it as
	// KindConstant so it joins the value-reference impact surface.
	fieldKind := graph.KindField
	if javaIsStaticFinal(def.Node, src) {
		fieldKind = graph.KindConstant
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: fieldKind, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	classID := filePath + "::" + className
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// javaStaticFinalStringValue returns the literal of a Java
// `static final String NAME = "literal"` field declaration, or ("", false)
// for anything else (missing static/final, non-string or absent
// initializer). Modifier tokens are scanned the same way javaVisibility
// reads the `modifiers` child. Used to index Java string constants into the
// resolver's constVal so a cross-language const-ref Temporal dispatch
// resolves.
// javaIsStaticFinal reports whether a field declaration carries both `static`
// and `final` — the Java compile-time-constant shape.
func javaIsStaticFinal(decl *sitter.Node, src []byte) bool {
	if decl == nil {
		return false
	}
	hasStatic, hasFinal := false, false
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		c := decl.Child(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j, _nc := 0, int(c.ChildCount()); j < _nc; j++ {
			switch c.Child(j).Type() {
			case "static":
				hasStatic = true
			case "final":
				hasFinal = true
			}
		}
	}
	return hasStatic && hasFinal
}

// javaPackageName returns the dotted name of the file's `package` declaration,
// or "".
func javaPackageName(root *sitter.Node, src []byte) string {
	for i, _nc := 0, int(root.NamedChildCount()); i < _nc; i++ {
		c := root.NamedChild(i)
		if c.Type() != "package_declaration" {
			continue
		}
		for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
			id := c.NamedChild(j)
			if t := id.Type(); t == "scoped_identifier" || t == "identifier" {
				return strings.TrimSpace(id.Content(src))
			}
		}
	}
	return ""
}

// stampScopePkg records the enclosing package on every type/member node so a
// JVM symbol is attributable to its package without re-deriving it.
func stampScopePkg(result *parser.ExtractionResult, pkg string) {
	if pkg == "" {
		return
	}
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		switch n.Kind {
		case graph.KindType, graph.KindInterface, graph.KindMethod, graph.KindField,
			graph.KindConstant, graph.KindVariable, graph.KindEnumMember:
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			n.Meta["scope_pkg"] = pkg
		}
	}
}

func javaStaticFinalStringValue(decl *sitter.Node, src []byte) (string, bool) {
	if decl == nil {
		return "", false
	}
	hasStatic, hasFinal := false, false
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		c := decl.Child(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j, _nc := 0, int(c.ChildCount()); j < _nc; j++ {
			tok := c.Child(j)
			if tok == nil {
				continue
			}
			switch tok.Type() {
			case "static":
				hasStatic = true
			case "final":
				hasFinal = true
			}
		}
	}
	if !hasStatic || !hasFinal {
		return "", false
	}
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "variable_declarator" {
			continue
		}
		if val := c.ChildByFieldName("value"); val != nil && val.Type() == "string_literal" {
			return javaStringLiteralText(val, src), true
		}
	}
	return "", false
}

func (e *JavaExtractor) emitImport(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	path := m.Captures["import.path"]
	importPath := strings.ReplaceAll(path.Text, ".", "/")
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// findEnclosingJavaContainer walks the parent chain of n looking for
// the nearest ancestor whose Type() matches t. Returns nil if none.
func findEnclosingJavaContainer(n *sitter.Node, t string) *sitter.Node {
	if n == nil {
		return nil
	}
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() == t {
			return p
		}
	}
	return nil
}

// findEnclosingJavaContainerAny walks the parent chain of n looking for
// the nearest ancestor whose Type() matches any of types. Returns nil
// if none.
func findEnclosingJavaContainerAny(n *sitter.Node, types ...string) *sitter.Node {
	if n == nil {
		return nil
	}
	for p := n.Parent(); p != nil; p = p.Parent() {
		pt := p.Type()
		for _, t := range types {
			if pt == t {
				return p
			}
		}
	}
	return nil
}

// javaIdentifierName returns the source text of the `name` field
// (typically an `identifier`) on a Java declaration node, or "" if
// missing.
func javaIdentifierName(declNode *sitter.Node, src []byte) string {
	if declNode == nil {
		return ""
	}
	nameNode := declNode.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return nameNode.Content(src)
}

// normalizeJavaTypeName strips generics and array markers from a Java type name.
// "User" -> "User", "List<User>" -> "List", "User[]" -> "User"
func normalizeJavaTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Remove array suffix.
	t = strings.TrimSuffix(t, "[]")
	// Remove generics.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Skip Java primitives and common non-class types.
	switch t {
	case "int", "long", "short", "byte", "float", "double", "boolean", "char", "void", "var", "String":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return "" // skip lowercase type names (primitives)
	}
	return t
}

// javaParamsSource returns the raw source text of a constructor or
// method's formal_parameters child, including the parentheses. Used by
// the DI post-pass to string-match parameter types without a full
// re-parse of the method signature.
func javaParamsSource(methodNode *sitter.Node, src []byte) string {
	if methodNode == nil {
		return ""
	}
	for i, _nc := 0, int(methodNode.NamedChildCount()); i < _nc; i++ {
		c := methodNode.NamedChild(i)
		if c != nil && c.Type() == "formal_parameters" {
			return c.Content(src)
		}
	}
	return ""
}

// javaCollectAnnotations walks the `modifiers` child of a Java
// declaration and returns each annotation's bare name and verbatim
// argument text. Mirrors javaMethodHasAnnotation's traversal but
// returns every annotation rather than checking a single name.
func javaCollectAnnotations(decl *sitter.Node, src []byte) []javaAnnotation {
	if decl == nil {
		return nil
	}
	var out []javaAnnotation
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
			m := c.NamedChild(j)
			if m == nil {
				continue
			}
			if m.Type() != "marker_annotation" && m.Type() != "annotation" {
				continue
			}
			nameNode := m.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			ann := javaAnnotation{
				name: nameNode.Content(src),
				line: int(m.StartPoint().Row) + 1,
			}
			if argNode := m.ChildByFieldName("arguments"); argNode != nil {
				txt := argNode.Content(src)
				if len(txt) >= 2 && txt[0] == '(' && txt[len(txt)-1] == ')' {
					txt = txt[1 : len(txt)-1]
				}
				ann.args = txt
			}
			out = append(out, ann)
		}
	}
	return out
}

type javaAnnotation struct {
	name string
	args string
	line int
}

// emitJavaThrowsEdges walks a method_declaration's `throws_clause`
// child and emits one EdgeThrows per declared exception type. Java's
// throws clause is the canonical compiler-checked source of an
// exception contract — every checked exception that can propagate
// must appear here, so the resulting edges form a complete
// error-surface for downstream queries.
func emitJavaThrowsEdges(methodNode *sitter.Node, src []byte, fromID, filePath string, line int, result *parser.ExtractionResult) {
	if methodNode == nil {
		return
	}
	for i, _nc := 0, int(methodNode.ChildCount()); i < _nc; i++ {
		c := methodNode.Child(i)
		if c == nil || c.Type() != "throws" {
			continue
		}
		for j, _nc := 0, int(c.ChildCount()); j < _nc; j++ {
			t := c.Child(j)
			if t == nil {
				continue
			}
			tt := t.Type()
			if tt != "type_identifier" && tt != "scoped_type_identifier" && tt != "generic_type" {
				continue
			}
			name := strings.TrimSpace(t.Content(src))
			// For scoped_type_identifier (java.io.IOException), keep
			// the trailing identifier — that's what the type-resolver
			// can land on.
			if i := strings.LastIndex(name, "."); i >= 0 {
				name = name[i+1:]
			}
			if i := strings.Index(name, "<"); i >= 0 {
				name = name[:i]
			}
			if name == "" {
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     fromID,
				To:       "unresolved::" + name,
				Kind:     graph.EdgeThrows,
				FilePath: filePath,
				Line:     line,
				Origin:   graph.OriginASTResolved,
			})
		}
	}
}

func emitJavaAnnotationEdges(anns []javaAnnotation, fromID, filePath string, result *parser.ExtractionResult, seen map[string]bool) {
	for _, a := range anns {
		if a.name == "" {
			continue
		}
		EmitAnnotationEdge(fromID, "java", a.name, a.args, filePath, a.line, result, seen)
	}
}

// javaMethodHasAnnotation reports whether a method_declaration node
// carries a top-level annotation of the given name (e.g. "Bean",
// "Autowired"). The tree-sitter-java grammar places annotations inside
// a `modifiers` wrapper as either `marker_annotation` (no args) or
// `annotation` (with args). Name is the bare identifier after @.
func javaMethodHasAnnotation(methodNode *sitter.Node, src []byte, name string) bool {
	for i, _nc := 0, int(methodNode.NamedChildCount()); i < _nc; i++ {
		c := methodNode.NamedChild(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
			m := c.NamedChild(j)
			if m == nil {
				continue
			}
			if m.Type() != "marker_annotation" && m.Type() != "annotation" {
				continue
			}
			nameNode := m.ChildByFieldName("name")
			if nameNode != nil && nameNode.Content(src) == name {
				return true
			}
		}
	}
	return false
}

// extractJavaMethodReturnType walks a method_declaration node to find
// the return type child (typically a type_identifier) and returns the
// normalized type name.
func extractJavaMethodReturnType(methodNode *sitter.Node, src []byte) string {
	for i, _nc := 0, int(methodNode.NamedChildCount()); i < _nc; i++ {
		child := methodNode.NamedChild(i)
		switch child.Type() {
		case "type_identifier":
			return normalizeJavaTypeName(child.Content(src))
		case "generic_type":
			// e.g., List<User> — take the first named child (the base type).
			if child.NamedChildCount() > 0 {
				return normalizeJavaTypeName(child.NamedChild(0).Content(src))
			}
		case "array_type":
			return normalizeJavaTypeName(child.Content(src))
		}
	}
	return ""
}

// extractJavaParentClass returns the direct superclass of a Java
// class_declaration, or "" when the class has no `extends` clause.
// Used by the scope-based static resolver to walk the inheritance
// chain when an unqualified call inside the class doesn't bind to a
// method on the class itself.
func extractJavaParentClass(classNode *sitter.Node, src []byte) string {
	if classNode == nil {
		return ""
	}
	sup := classNode.ChildByFieldName("superclass")
	if sup == nil {
		return ""
	}
	for i, _nc := 0, int(sup.NamedChildCount()); i < _nc; i++ {
		child := sup.NamedChild(i)
		switch child.Type() {
		case "type_identifier", "generic_type", "scoped_type_identifier":
			text := strings.TrimSpace(child.Content(src))
			// Strip generic parameters for the lookup key — the
			// scope resolver matches against the class's plain name.
			if i := strings.IndexAny(text, "<"); i > 0 {
				text = text[:i]
			}
			if i := strings.LastIndex(text, "."); i > 0 {
				text = text[i+1:]
			}
			return text
		}
	}
	return ""
}

// inferTypeFromJavaNewExpr extracts the class name from an object_creation_expression node.
// new User(...) -> "User", new ArrayList<String>() -> "ArrayList"
func inferTypeFromJavaNewExpr(node *sitter.Node, src []byte) string {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		if child.Type() == "type_identifier" {
			name := child.Content(src)
			if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		}
	}
	return ""
}
