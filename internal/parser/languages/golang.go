package languages

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/golang"
)

// qGoAll is a single tree-sitter query that alternates over every pattern
// the Go extractor needs. One tree walk per file replaces the 20+
// `parser.RunQuery` calls the previous design made per Extract call
// (each of which recompiled its query and ran an independent cursor
// over the whole tree). Capture names are disjoint across patterns so
// the dispatch in Extract can branch on which name is set.
const qGoAll = `
[
  (package_clause (package_identifier) @pkg.name)

  (function_declaration
    name: (identifier) @func.name
    parameters: (parameter_list) @func.params
    result: (_)? @func.result) @func.def

  (method_declaration
    receiver: (parameter_list) @method.receiver
    name: (field_identifier) @method.name
    parameters: (parameter_list) @method.params
    result: (_)? @method.result) @method.def

  (type_declaration
    (type_spec
      name: (type_identifier) @typedef.name
      type: (_) @typedef.body)) @typedef.def

  (type_declaration
    (type_alias
      name: (type_identifier) @alias.name
      type: (_) @alias.type)) @alias.def

  (import_spec
    name: (package_identifier)? @import.alias
    path: (interpreted_string_literal) @import.path) @import.spec

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (selector_expression
      operand: (_) @callm.receiver
      field: (field_identifier) @callm.method)) @callm.expr

  ; Explicitly instantiated generic calls. With ONE type argument the
  ; grammar cannot tell Zero[int]() from indexing a func-valued map, so
  ; it parses the callee as an index_expression and the patterns above
  ; match nothing — every such call vanished, edge and stub alike. Go
  ; forbids a func and a var sharing a package-scope name, so binding the
  ; operand name is safe: a genuine handlers[k]() finds no function of
  ; that name and stays an unresolved stub. Two or more type arguments
  ; already parse as a plain callee with a sibling type_arguments field
  ; and were never affected.
  (call_expression
    function: (index_expression
      operand: (identifier) @callg.name)) @callg.expr

  (call_expression
    function: (index_expression
      operand: (selector_expression
        operand: (_) @callmg.receiver
        field: (field_identifier) @callmg.method))) @callmg.expr

  ; ...and with exactly one VALUE argument the same call is not even a
  ; call_expression: One[int](1) is indistinguishable from a conversion
  ; to the generic type One[int], and the grammar picks the conversion.
  ; Same reasoning applies — a real conversion names a type, and a type
  ; cannot share a package-scope name with a function.
  (type_conversion_expression
    type: (generic_type
      type: (type_identifier) @callg.name)) @callg.expr

  (type_conversion_expression
    type: (generic_type
      type: (qualified_type
        package: (_) @callmg.receiver
        name: (type_identifier) @callmg.method))) @callmg.expr

  (var_declaration
    (var_spec
      name: (identifier) @var.name
      type: (_)? @var.type)) @var.def

  (const_declaration
    (const_spec
      name: (identifier) @const.name)) @const.def

  (short_var_declaration
    left: (expression_list (identifier) @svar.name)
    right: (expression_list (_) @svar.value)) @svar.def

  (composite_literal
    type: (type_identifier) @comp.type) @comp.expr

  (composite_literal
    type: (qualified_type
      package: (package_identifier) @compq.pkg
      name: (type_identifier) @compq.type)) @compq.expr

  (field_declaration
    type: (type_identifier) @ftype.name) @ftype.decl

  (const_spec
    type: (type_identifier) @ctype.name) @ctype.decl

  (var_spec
    type: (type_identifier) @vtype.name) @vtype.decl

  (parameter_declaration
    type: (type_identifier) @ptype.name) @ptype.decl

  ; Type assertions: x.(T) / x.(pkg.T). The asserted type is a
  ; reference to T. Without this an interface used only via a type
  ; assertion has no incoming edge and looks like dead code.
  (type_assertion_expression
    type: (type_identifier) @assert.name) @assert.decl

  (type_assertion_expression
    type: (qualified_type
      package: (package_identifier) @assertq.pkg
      name: (type_identifier) @assertq.name)) @assertq.decl

  (argument_list
    (selector_expression
      operand: (_) @selarg.receiver
      field: (field_identifier) @selarg.field)) @selarg.list

  (argument_list
    (identifier) @identarg.name) @identarg.list

  (keyed_element
    (literal_element (identifier) @fieldval.key)
    (literal_element (identifier) @fieldval.value)) @fieldval.elem

  ; Struct field set to a string literal, e.g. ActivityName: "Charge".
  ; The Temporal step/executor resolver joins these to a struct whose
  ; method dispatches via that field.
  (keyed_element
    (literal_element (identifier) @fieldstr.key)
    (literal_element (interpreted_string_literal) @fieldstr.val)) @fieldstr.elem

  (keyed_element
    (literal_element (identifier) @fieldsel.key)
    (literal_element
      (selector_expression
        operand: (_) @fieldsel.receiver
        field: (field_identifier) @fieldsel.method))) @fieldsel.elem

  ; Assignment LHS selector. EdgeWrites from the enclosing function
  ; to the field. Covers plain "s.field = x" and "s.field += x".
  (assignment_statement
    left: (expression_list
      (selector_expression
        operand: (_) @assign.receiver
        field: (field_identifier) @assign.field))) @assign.def

  ; Inc/dec selector statements: s.field++ / s.field-- both write.
  (inc_statement
    (selector_expression
      operand: (_) @incsel.receiver
      field: (field_identifier) @incsel.field)) @incsel.def

  (dec_statement
    (selector_expression
      operand: (_) @decsel.receiver
      field: (field_identifier) @decsel.field)) @decsel.def

  ; Return-statement value-side reads. "return s.port, s.addr" reads
  ; both fields. We capture each selector in the return expression
  ; list so the schema's "every value use is a read" rule covers
  ; functions whose only output is the read.
  (return_statement
    (expression_list
      (selector_expression
        operand: (_) @retsel.receiver
        field: (field_identifier) @retsel.field)) @retsel.list)

  (return_statement
    (expression_list
      (identifier) @retident.name) @retident.list)
]
`

// GoExtractor extracts Go source files into graph nodes and edges.
type GoExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewGoExtractor() *GoExtractor {
	lang := golang.GetLanguage()
	return &GoExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qGoAll, lang),
	}
}

func (e *GoExtractor) Language() string     { return "go" }
func (e *GoExtractor) Extensions() []string { return []string{".go"} }

// --- Deferred match buffers ----------------------------------------

type goDeferredCall struct {
	callName   string // plain call
	method     string // selector call method name
	receiver   string // selector call receiver text
	line       int    // 1-based line of call_expression
	isSelector bool
	// genericInst marks a call whose callee spelled explicit type
	// arguments. Go writes those exactly like indexing a func-valued
	// map or field, and like converting to a generic type, so the
	// marker rides onto the edge and the resolver binds it only to a
	// declaration that is actually generic.
	genericInst bool
	spawn       bool // call is launched via `go` — emit EdgeSpawns alongside EdgeCalls
	// returnUsage is how the call site consumes the return value
	// (graph.ReturnUsage* label), classified from the call node's
	// parent chain at capture time and stamped as edge Meta on every
	// EdgeCalls emitted for this site. Empty when unclassifiable.
	returnUsage string
	// gRPC server registration. Set when this call is the generated
	// `Register<Service>Server(registrar, impl)` helper: grpcRegService
	// is the service name and grpcRegArgNode is the second-argument AST
	// node naming the impl. The impl type itself is resolved in the
	// post-pass below — after the file-wide type environment is fully
	// populated, since the impl may be a variable declared anywhere in
	// the file. Stamped as edge Meta so the resolver's
	// ResolveGRPCStubCalls pass can join client-stub calls to the
	// server-side handler method.
	grpcRegService string
	grpcRegArgNode *sitter.Node
	// Temporal SDK call attribution. tempKind is "" for non-Temporal
	// calls; "activity" / "workflow" / "register_activity" /
	// "register_workflow" identifies which Temporal pattern this call
	// is. tempName is the activity / workflow name extracted from the
	// call (function reference or string literal). tempLocal flags
	// `workflow.ExecuteLocalActivity`. The actual edge target +
	// `via=temporal.stub` / `via=temporal.register` meta is stamped
	// in the call post-pass below; the resolver's ResolveTemporalCalls
	// pass joins stub calls to the registered handler.
	tempKind  string
	tempName  string
	tempLocal bool
	// tempEnvDefault is set when tempName was resolved from a bare
	// variable read from an env var with a literal default (e.g.
	// `cmp.Or(os.Getenv("K"), "Default")`). The stub edge is then tagged
	// `temporal_name_origin=env_default` so the resolver lands it at the
	// speculative tier — the runtime env value may differ from the default.
	tempEnvDefault bool
	// tempVarTrace is set when tempName was recovered by tracing a bare
	// local dispatch variable to its last intra-procedural assignment — a
	// string literal, a const reference, or a no-arg const-returning func
	// (the "meta_vars" broken-dispatch category). The stub edge is tagged
	// `temporal_name_origin=var_trace` so the resolver lands it at the
	// speculative tier — last-assignment-wins is a best-effort static guess.
	tempVarTrace bool
	// tempHandlerKind is "query" / "signal" / "update" when this call
	// is a `workflow.SetQueryHandler` / `GetSignalChannel` /
	// `SetUpdateHandler` in-workflow handler declaration; tempName then
	// carries the handler's string name. `via=temporal.handler` meta is
	// stamped on the emitted edge in the call post-pass below.
	tempHandlerKind string
	// tempDefaultConst is set when the env-default's default argument was a
	// constant REFERENCE (`config.ACTIVITY_NAME_DEFAULT` / a bare const name)
	// rather than a string literal. temporal_name then stays the dispatch
	// variable name; the resolver substitutes the constant's literal value
	// from its const-deref index. Emitted as `temporal_default_const`.
	tempDefaultConst string
	// tempEnvSource records HOW the env-default name was recognised:
	// "os_getenv" (provable os.Getenv / os.LookupEnv read), "allowlist" (a
	// helper name in the configured env-helper allow-list), or "heuristic"
	// (a generic env-named helper matched structurally). Emitted as
	// `temporal_env_source` so the resolver can tier the edge: allow-list /
	// os.Getenv land at the inferred tier, the heuristic stays speculative.
	tempEnvSource string
	// tempOutKind is "signal" / "query" when this call is an outbound
	// signal-send / query-call against a running workflow
	// (SignalExternalWorkflow / SignalWorkflow / QueryWorkflow); tempName
	// then carries the signal/query name. `via=temporal.signal-send` /
	// `temporal.query-call` meta is stamped on the emitted edge below.
	tempOutKind string
	// tempRegisteredName is the canonical registered name when a
	// RegisterActivityWithOptions / RegisterWorkflowWithOptions call
	// overrides it via RegisterOptions{Name: "..."}. tempName still holds
	// the function-reference identifier (used to locate the node); this
	// is the name the activity/workflow is dispatched under and becomes
	// the resolver's index key. Empty when no Name override is present.
	tempRegisteredName string
	// tempRegisterPlural marks a `w.RegisterActivities(&MyActivities{})`
	// struct registration: tempName then holds the struct TYPE name and
	// the resolver promotes every exported method of that struct to a
	// temporal activity keyed by the method name.
	tempRegisterPlural bool
	// tempStartName is the workflow name when this call STARTS a workflow
	// (client.ExecuteWorkflow / SignalWithStartWorkflow). `via=temporal.start`
	// meta is stamped on the emitted edge and the resolver rewrites it to
	// the registered workflow — the "who starts this workflow" edge.
	tempStartName string
	// callNode is the call_expression AST node, retained so the general
	// call-edge emission can extract positional arg names
	// (attachGoTemporalCallArgNames) for the resolver's wrapper-following
	// pass; the executor-field resolver also uses it to inspect the
	// dispatch argument shape (e.g. `e.ActivityName` as a selector).
	// Nil for synthetic / non-call entries.
	callNode *sitter.Node
}

// PURPOSE — captures a struct-literal field assignment to a string literal
// so the Temporal executor-field resolver can join the construction site
// to a dispatch method that reads the same field.
// RATIONALE — separate from goDeferredCall because it fires on composite
// literals, not call expressions; the two passes share a single tree walk.
// KEYWORDS — temporal, executor, struct-field
type goExecutorField struct {
	typeName string
	field    string
	value    string
	line     int
}

type goDeferredTypeRef struct {
	typeName string
	pkg      string // optional qualifier
	line     int
	kind     graph.EdgeKind
}

type goDeferredValueSel struct {
	field    string
	receiver string
	line     int
	kind     graph.EdgeKind
}

type goDeferredValueIdent struct {
	name string
	line int
}

func (e *GoExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	return e.ExtractWithOptions(filePath, src, parser.ExtractionOptions{})
}

// ExtractWithOptions applies request-scoped repository configuration without
// mutating this shared extractor. Configured Temporal helpers extend the
// built-in helper set for this extraction only.
func (e *GoExtractor) ExtractWithOptions(filePath string, src []byte, opts parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	// Keys are lower-cased so a repo-local helper name matches its call site
	// the same case-insensitive way a built-in goEnvHelperNames entry does.
	// Without this, `env_helpers: [getCorpValue]` silently fails to promote a
	// `GetCorpValue(...)` dispatch — see goEnvHelperDefaultLiteral.
	envHelperExtra := make(map[string]bool)
	for _, name := range opts.TemporalEnvHelpers() {
		envHelperExtra[strings.ToLower(name)] = true
	}
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	// Wrap in a ref-counted handle and stamp it on the result so the
	// indexer can hand it to contract enrichers. The wrapper owns
	// the tree from here; callers Release() once they're done. We
	// don't defer Close — closing happens in the indexer after the
	// contract pass releases its ref.
	pt := parser.NewParseTree(tree, src, "go")
	result := &parser.ExtractionResult{Tree: pt}

	root := tree.RootNode()

	fileNode := &graph.Node{
		ID:        filePath,
		Kind:      graph.KindFile,
		Name:      filePath,
		FilePath:  filePath,
		StartLine: 1,
		EndLine:   int(root.EndPoint().Row) + 1,
		Language:  "go",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	imports := map[string]string{} // alias → importPath
	// Local alias the Temporal workflow package is imported under in this
	// file (default "workflow"; "" when not imported). The receiver-gated
	// temporal detectors canonicalise a matching receiver to "workflow" so
	// an aliased `import wf "go.temporal.io/sdk/workflow"` is recognised.
	wfAlias := goWorkflowReceiverAlias(root, src)
	tenv := make(typeEnv)
	// paramsByFunc: enclosing-function ID → (param/receiver name → type).
	// Function parameters and method receivers shadow file-wide tenv at
	// call resolution time so each function's locals stay sandboxed.
	paramsByFunc := map[string]typeEnv{}
	// paramNamesByFunc: function/method ID → set of ALL its parameter
	// names (paramsByFunc only keeps params with non-builtin types). Used
	// by the Temporal wrapper detector to recognise a dispatch whose name
	// is a forwarded parameter.
	paramNamesByFunc := map[string]map[string]bool{}
	// seenIDs: per-file node-ID collision set. Two declarations that build
	// the same base ID (e.g. two func init()) both survive as distinct nodes
	// via a line suffix instead of one silently overwriting the other.
	seenIDs := map[string]bool{}
	seenTypeName := map[string]bool{} // dedup when alias + typedef match same name

	var calls []goDeferredCall
	// grpcStubVars maps a local variable name to the gRPC service it is
	// a client stub for, seeded from `v := pkg.New<Service>Client(conn)`
	// short-var declarations. Lets `v.Method(...)` call sites resolve as
	// gRPC stub calls in the post-pass below.
	grpcStubVars := map[string]string{}
	var typeRefs []goDeferredTypeRef
	var instantiates []goDeferredTypeRef
	var valueSels []goDeferredValueSel
	var valueIdents []goDeferredValueIdent
	var fieldValSels []goDeferredValueSel
	var fieldValIdents []goDeferredValueIdent
	var observabilityEvents []goObservabilityEvent
	var pubsubCandidates []goPubsubCandidate
	var stringEvents []goStringEvent
	var flagEvents []goFlagEvent
	var configEvents []goConfigEvent
	var sqlEvents []goSQLEvent
	// writes buffers selector LHS of assignment / inc / dec
	// statements. Emitted in the post-pass once funcRanges and tenv
	// are settled so each EdgeWrites is attributed to its enclosing
	// function and (when known) carries the receiver type for the
	// resolver to land on the right field node.
	var writes []goDeferredValueSel
	var executorFields []goExecutorField

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		// Package: just a marker; not emitted as a graph node.
		case m.Captures["pkg.name"] != nil:
			// No-op (the package name is not currently surfaced as a node).

		case m.Captures["func.def"] != nil:
			root.WithScratch(func() {
				e.emitFunction(m, filePath, fileID, src, result, paramsByFunc, paramNamesByFunc, imports, seenIDs)
			})

		case m.Captures["method.def"] != nil:
			root.WithScratch(func() {
				e.emitMethod(m, filePath, fileID, src, result, paramsByFunc, paramNamesByFunc, imports, seenIDs)
			})

		case m.Captures["typedef.def"] != nil:
			e.emitTypeDecl(m, filePath, fileID, src, result, seenTypeName)

		case m.Captures["alias.def"] != nil:
			e.emitTypeAlias(m, filePath, fileID, src, result, seenTypeName)

		case m.Captures["import.spec"] != nil:
			e.emitImport(m, filePath, fileID, result, imports, fileNode)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			callName := m.Captures["call.name"].Text
			dc := goDeferredCall{
				callName:    callName,
				line:        expr.StartLine + 1,
				spawn:       isGoroutineSpawn(expr.Node),
				returnUsage: classifyReturnUsage(expr.Node, src, goReturnUsageSpec),
				callNode:    expr.Node,
			}
			if svc, argNode, ok := grpcRegisterArgNode(expr.Node, callName); ok {
				dc.grpcRegService, dc.grpcRegArgNode = svc, argNode
			}
			calls = append(calls, dc)

		// An explicitly instantiated generic call. The grammar spells
		// these exactly like indexing a func-valued map/field and like
		// converting to a generic type, so the marker rides along and
		// the resolver refuses to bind unless the target really is
		// generic — see goGenericInstantiationOnly.
		case m.Captures["callg.expr"] != nil:
			expr := m.Captures["callg.expr"]
			calls = append(calls, goDeferredCall{
				callName:    m.Captures["callg.name"].Text,
				line:        expr.StartLine + 1,
				spawn:       isGoroutineSpawn(expr.Node),
				returnUsage: classifyReturnUsage(expr.Node, src, goReturnUsageSpec),
				callNode:    expr.Node,
				genericInst: true,
			})

		case m.Captures["callmg.expr"] != nil:
			expr := m.Captures["callmg.expr"]
			calls = append(calls, goDeferredCall{
				method:      m.Captures["callmg.method"].Text,
				receiver:    m.Captures["callmg.receiver"].Text,
				line:        expr.StartLine + 1,
				isSelector:  true,
				spawn:       isGoroutineSpawn(expr.Node),
				returnUsage: classifyReturnUsage(expr.Node, src, goReturnUsageSpec),
				callNode:    expr.Node,
				genericInst: true,
			})

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			method := m.Captures["callm.method"].Text
			receiver := m.Captures["callm.receiver"].Text
			dc := goDeferredCall{
				method:      method,
				receiver:    receiver,
				line:        expr.StartLine + 1,
				isSelector:  true,
				spawn:       isGoroutineSpawn(expr.Node),
				returnUsage: classifyReturnUsage(expr.Node, src, goReturnUsageSpec),
				callNode:    expr.Node,
			}
			dc.callNode = expr.Node
			if svc, argNode, ok := grpcRegisterArgNode(expr.Node, method); ok {
				dc.grpcRegService, dc.grpcRegArgNode = svc, argNode
			}
			// Temporal workflow → activity dispatch:
			// `workflow.ExecuteActivity(ctx, X, ...)` etc. Canonicalise an
			// aliased workflow-package receiver (e.g. `wf`) to "workflow"
			// so the receiver-gated detectors recognise it.
			tempRecv := goCanonicalWorkflowReceiver(receiver, wfAlias)
			if kind, local, ok := goTemporalDispatchKind(tempRecv, method); ok {
				argNode := goTemporalDispatchArg(expr.Node)
				if name := goTemporalNameFromExpr(argNode, src); name != "" {
					dc.tempKind = kind
					dc.tempName = name
					dc.tempLocal = local
					// Env-default refinement: when the name is a bare local
					// variable, try to resolve it to an env-var-with-literal
					// -default so the dispatch lands on the default activity.
					if argNode != nil && argNode.Type() == "identifier" {
						if litDef, constName, source, ok := goTemporalEnvDefaultName(expr.Node, name, src, envHelperExtra); ok {
							dc.tempEnvDefault = true
							dc.tempEnvSource = source
							if constName != "" {
								// Const-reference default: keep temporal_name as the
								// dispatch variable; the resolver substitutes the
								// constant's literal value via temporal_default_const.
								dc.tempDefaultConst = constName
							} else {
								dc.tempName = litDef
							}
						} else if lit, cn, ok := goTemporalVarTrace(expr.Node, name, src); ok {
							// Variable tracing (Cat 2): the dispatch name is a bare
							// local var assigned a string literal, a const
							// reference, or a no-arg const-returning func. Emit the
							// traced value as the dispatch name — a literal resolves
							// directly; a const NAME is dereferenced by the
							// resolver's const-deref map (a non-const name simply
							// fails to deref and stays a broken_dispatch).
							switch {
							case lit != "":
								dc.tempName = lit
							case cn != "":
								dc.tempName = cn
							}
							dc.tempVarTrace = true
						}
					}
				}
			} else if kind, plural, ok := goTemporalRegisterKind(method); ok {
				// Temporal worker registration:
				// `w.RegisterActivity(F)` etc.
				if plural {
					// `w.RegisterActivities(&MyActivities{})` — every
					// exported method of the struct becomes an activity.
					// tempName carries the struct TYPE name; the resolver
					// promotes the methods.
					if st := goTemporalRegisterStructType(expr.Node, src); st != "" {
						dc.tempKind = "register_" + kind
						dc.tempName = st
						dc.tempRegisterPlural = true
					}
				} else if name := goTemporalRegisterName(expr.Node, src); name != "" {
					dc.tempKind = "register_" + kind
					dc.tempName = name
					// RegisterActivityWithOptions / RegisterWorkflowWithOptions
					// may override the registered name via
					// RegisterOptions{Name: "..."} — that is the name a
					// dispatch matches against, so capture it as the index key.
					if override := goTemporalRegisterNameOverride(expr.Node, src); override != "" {
						dc.tempRegisteredName = override
					}
				}
			} else if hkind, ok := goTemporalHandlerKind(tempRecv, method); ok {
				// Temporal in-workflow handler declaration:
				// `workflow.SetQueryHandler(ctx, "name", fn)` etc.
				if name := goTemporalHandlerName(expr.Node, src); name != "" {
					dc.tempHandlerKind = hkind
					dc.tempName = name
				}
			} else if okind, namePos, ok := goTemporalSignalQueryOutKind(tempRecv, method); ok {
				// Outbound signal-send / query-call against a running
				// workflow: SignalExternalWorkflow / SignalWorkflow /
				// QueryWorkflow. The name is the 4th positional literal.
				if name := goTemporalNthStringLiteralArg(expr.Node, namePos, src); name != "" {
					dc.tempOutKind = okind
					dc.tempName = name
				}
			} else if wfPos, ok := goTemporalStartKind(method); ok {
				// Service-side workflow START: client.ExecuteWorkflow /
				// SignalWithStartWorkflow. The workflow is the wfPos-th
				// positional arg (a func ref, selector, or string type name).
				if name := goTemporalNthArgName(expr.Node, wfPos, src); name != "" {
					dc.tempStartName = name
				}
			}
			calls = append(calls, dc)
			if name, ok := detectGoLogEvent(expr.Node, method, src); ok {
				observabilityEvents = append(observabilityEvents, goObservabilityEvent{
					method: method,
					name:   name,
					line:   expr.StartLine + 1,
				})
			}
			if cand, ok := detectGoPubsubCall(expr.Node, method, src); ok {
				pubsubCandidates = append(pubsubCandidates, cand)
			}
			receiverText := m.Captures["callm.receiver"].Text
			if name, ok := detectGoMetric(expr.Node, method, src); ok {
				stringEvents = append(stringEvents, goStringEvent{
					context: stringCtxMetric,
					method:  method,
					value:   name,
					line:    expr.StartLine + 1,
				})
			}
			if msg, ok := detectGoErrorMessage(expr.Node, receiverText, method, src); ok {
				stringEvents = append(stringEvents, goStringEvent{
					context: stringCtxErrorMsg,
					method:  receiverText + "." + method,
					value:   msg,
					line:    expr.StartLine + 1,
				})
			}
			if route, ok := detectGoRoute(expr.Node, method, src); ok {
				stringEvents = append(stringEvents, goStringEvent{
					context: stringCtxRoute,
					method:  method,
					value:   route,
					line:    expr.StartLine + 1,
				})
			}
			if provider, flagName, ok := detectGoFlagCheck(expr.Node, method, src); ok {
				flagEvents = append(flagEvents, goFlagEvent{
					provider: provider,
					method:   method,
					name:     flagName,
					line:     expr.StartLine + 1,
				})
			}
			if source, op, key, ok := detectGoConfigKey(expr.Node, m.Captures["callm.receiver"].Text, method, src); ok {
				// Gate provider-specific classifications on the import
				// set so a `req.GetString("query")` against an mcp
				// request type doesn't get mistaken for a viper read in
				// a file that never imports viper.
				if goConfigSourceImported(source, imports) {
					configEvents = append(configEvents, goConfigEvent{
						source: source,
						op:     op,
						method: method,
						key:    key,
						line:   expr.StartLine + 1,
					})
				}
			}
			if tables, cols, query, ok := detectGoSQLCall(expr.Node, method, src); ok {
				sqlEvents = append(sqlEvents, goSQLEvent{
					method:  method,
					tables:  tables,
					columns: cols,
					query:   query,
					line:    expr.StartLine + 1,
				})
			}

		case m.Captures["var.def"] != nil:
			e.emitVar(m, filePath, fileID, result, tenv)

		case m.Captures["const.def"] != nil:
			e.emitConst(m, filePath, fileID, src, result)

		case m.Captures["svar.def"] != nil:
			e.recordShortVarType(m, src, tenv)
			recordGoGRPCStubVar(m, src, grpcStubVars)

		case m.Captures["comp.expr"] != nil:
			expr := m.Captures["comp.expr"]
			instantiates = append(instantiates, goDeferredTypeRef{
				typeName: m.Captures["comp.type"].Text,
				line:     expr.StartLine + 1,
				kind:     graph.EdgeInstantiates,
			})

		case m.Captures["compq.expr"] != nil:
			expr := m.Captures["compq.expr"]
			instantiates = append(instantiates, goDeferredTypeRef{
				typeName: m.Captures["compq.type"].Text,
				pkg:      m.Captures["compq.pkg"].Text,
				line:     expr.StartLine + 1,
				kind:     graph.EdgeInstantiates,
			})

		case m.Captures["ftype.decl"] != nil:
			decl := m.Captures["ftype.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["ftype.name"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["ctype.decl"] != nil:
			decl := m.Captures["ctype.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["ctype.name"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["vtype.decl"] != nil:
			decl := m.Captures["vtype.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["vtype.name"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["ptype.decl"] != nil:
			decl := m.Captures["ptype.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["ptype.name"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["assert.decl"] != nil:
			decl := m.Captures["assert.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["assert.name"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["assertq.decl"] != nil:
			decl := m.Captures["assertq.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["assertq.name"].Text,
				pkg:      m.Captures["assertq.pkg"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["selarg.list"] != nil:
			list := m.Captures["selarg.list"]
			valueSels = append(valueSels, goDeferredValueSel{
				field:    m.Captures["selarg.field"].Text,
				receiver: m.Captures["selarg.receiver"].Text,
				line:     list.StartLine + 1,
				kind:     graph.EdgeReads,
			})

		case m.Captures["identarg.list"] != nil:
			list := m.Captures["identarg.list"]
			valueIdents = append(valueIdents, goDeferredValueIdent{
				name: m.Captures["identarg.name"].Text,
				line: list.StartLine + 1,
			})

		case m.Captures["fieldval.elem"] != nil:
			elem := m.Captures["fieldval.elem"]
			fieldValIdents = append(fieldValIdents, goDeferredValueIdent{
				name: m.Captures["fieldval.value"].Text,
				line: elem.StartLine + 1,
			})

		case m.Captures["fieldstr.elem"] != nil:
			elem := m.Captures["fieldstr.elem"]
			if typeName := goCompositeLiteralType(elem.Node, src); typeName != "" {
				val := m.Captures["fieldstr.val"].Text
				if len(val) >= 2 && (val[0] == '"' || val[0] == '`') {
					val = val[1 : len(val)-1]
				}
				executorFields = append(executorFields, goExecutorField{
					typeName: typeName,
					field:    m.Captures["fieldstr.key"].Text,
					value:    val,
					line:     elem.StartLine + 1,
				})
			}

		case m.Captures["assign.def"] != nil:
			def := m.Captures["assign.def"]
			writes = append(writes, goDeferredValueSel{
				field:    m.Captures["assign.field"].Text,
				receiver: m.Captures["assign.receiver"].Text,
				line:     def.StartLine + 1,
				kind:     graph.EdgeWrites,
			})

		case m.Captures["incsel.def"] != nil:
			def := m.Captures["incsel.def"]
			writes = append(writes, goDeferredValueSel{
				field:    m.Captures["incsel.field"].Text,
				receiver: m.Captures["incsel.receiver"].Text,
				line:     def.StartLine + 1,
				kind:     graph.EdgeWrites,
			})

		case m.Captures["decsel.def"] != nil:
			def := m.Captures["decsel.def"]
			writes = append(writes, goDeferredValueSel{
				field:    m.Captures["decsel.field"].Text,
				receiver: m.Captures["decsel.receiver"].Text,
				line:     def.StartLine + 1,
				kind:     graph.EdgeWrites,
			})

		case m.Captures["fieldsel.elem"] != nil:
			elem := m.Captures["fieldsel.elem"]
			fieldValSels = append(fieldValSels, goDeferredValueSel{
				field:    m.Captures["fieldsel.method"].Text,
				receiver: m.Captures["fieldsel.receiver"].Text,
				line:     elem.StartLine + 1,
				kind:     graph.EdgeReads,
			})

		case m.Captures["retsel.list"] != nil:
			list := m.Captures["retsel.list"]
			valueSels = append(valueSels, goDeferredValueSel{
				field:    m.Captures["retsel.field"].Text,
				receiver: m.Captures["retsel.receiver"].Text,
				line:     list.StartLine + 1,
				kind:     graph.EdgeReads,
			})

		case m.Captures["retident.list"] != nil:
			list := m.Captures["retident.list"]
			valueIdents = append(valueIdents, goDeferredValueIdent{
				name: m.Captures["retident.name"].Text,
				line: list.StartLine + 1,
			})
		}
	})

	// All function/method nodes have been emitted; now map call sites to
	// their enclosing definition.
	funcRanges := buildFuncRanges(result)

	// lookupRecvType resolves a receiver-expression name first against
	// the enclosing function's parameter scope, then the file-wide tenv.
	// Per-function scope is needed so calls like `func F(reg *Registry)
	// { reg.Register(...) }` get the right `receiver_type` even when
	// another function in the same file has a parameter or local named
	// `reg` with a different type.
	lookupRecvType := func(callerID, name string) (string, bool) {
		if callerID != "" {
			if scope, ok := paramsByFunc[callerID]; ok {
				if t, ok := scope[name]; ok {
					return t, true
				}
			}
		}
		t, ok := tenv[name]
		return t, ok
	}

	// recvNameByID maps a method node ID to its receiver variable name, so a
	// selector call can be classified as invoked on the method's own receiver
	// (s.counter.Increment() / s.helper()) — the basis for indirect
	// receiver-field-mutation attribution.
	recvNameByID := map[string]string{}
	recvTypeByID := map[string]string{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindMethod && n.Meta != nil {
			if rn, _ := n.Meta["recv_name"].(string); rn != "" {
				recvNameByID[n.ID] = rn
			}
			if rt, _ := n.Meta["receiver"].(string); rt != "" {
				recvTypeByID[n.ID] = rt
			}
		}
	}

	// --- Calls ---
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		// gRPC client-stub call: `pkg.New<Service>Client(conn).Method(...)`
		// (inline chain) or `stub.Method(...)` where `stub` came from a
		// `New<Service>Client` constructor. Emitted to a dedicated
		// placeholder that the resolver's ResolveGRPCStubCalls pass lands
		// on the server-side handler method. Intercepted ahead of normal
		// selector handling so the call doesn't resolve onto the generated
		// client-interface method instead of the actual handler.
		if c.isSelector {
			if svc, ok := goGRPCStubService(c, grpcStubVars); ok {
				target := "unresolved::grpc::" + svc + "::" + c.method
				edge := &graph.Edge{
					From: callerID, To: target,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
					Meta: map[string]any{
						"via":          "grpc.stub",
						"grpc_service": svc,
						"grpc_method":  c.method,
					},
				}
				stampReturnUsage(edge, c.returnUsage)
				result.Edges = append(result.Edges, edge)
				emitGoSpawnEdge(c, callerID, target, filePath, result)
				continue
			}
			// Temporal workflow → activity / child-workflow dispatch:
			// `workflow.ExecuteActivity(ctx, X, ...)` etc. Emitted to a
			// dedicated placeholder that the resolver's
			// ResolveTemporalCalls pass lands on the activity / workflow
			// function the worker has registered under that name.
			// Intercepted ahead of normal selector handling so the call
			// doesn't resolve onto the SDK's generic `ExecuteActivity`
			// helper instead of the actual activity body.
			if c.tempKind == "activity" || c.tempKind == "workflow" {
				// Wrapper detection: when the dispatch name is a PARAMETER of
				// the enclosing function, this function is a dispatch wrapper
				// (e.g. executeActivity(ctx, ao, name, …) {
				// workflow.ExecuteActivity(ctx, name, …) }). The parameter is
				// not a real activity name, so the stub could never resolve —
				// suppress the noise and mark the wrapper instead. Propagating
				// a caller's literal/const argument into the dispatch
				// (cross-file wrapper-FOLLOWING) needs call-argument flow and
				// is a deliberate, documented blind spot for now.
				if !c.tempEnvDefault {
					if names, ok := paramNamesByFunc[callerID]; ok {
						if names[c.tempName] {
							markGoTemporalWrapper(result, callerID, c.tempKind, c.tempName)
							// Emit a wrapper-stub edge that the resolver's
							// wrapper-following pass can use as an anchor:
							// from=callerID, target=placeholder,
							// temporal_name_param=paramName. No literal
							// temporal_name resolution here (it's a param);
							// the resolver synthesises a proper stub for each
							// caller that passes a literal at this position.
							target := "unresolved::temporal::" + c.tempKind + "::" + c.tempName
							meta := map[string]any{
								"via":                 "temporal.stub",
								"temporal_kind":       c.tempKind,
								"temporal_name":       c.tempName,
								"temporal_name_param": c.tempName,
							}
							if c.tempLocal {
								meta["temporal_local"] = true
							}
							result.Edges = append(result.Edges, &graph.Edge{
								From: callerID, To: target,
								Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
								Meta: meta,
							})
							continue
						}
					}
				}
				target := "unresolved::temporal::" + c.tempKind + "::" + c.tempName
				meta := map[string]any{
					"via":           "temporal.stub",
					"temporal_kind": c.tempKind,
					"temporal_name": c.tempName,
				}
				if c.tempLocal {
					meta["temporal_local"] = true
				}
				if c.tempEnvDefault {
					meta["temporal_name_origin"] = "env_default"
					if c.tempEnvSource != "" {
						meta["temporal_env_source"] = c.tempEnvSource
					}
					if c.tempDefaultConst != "" {
						meta["temporal_default_const"] = c.tempDefaultConst
					}
				}
				if c.tempVarTrace {
					// Variable-traced dispatch name (Cat 2): the name was the
					// last intra-procedural assignment to a bare local — a
					// best-effort static guess (a later conditional reassign
					// may not execute). The resolver lands it at the
					// speculative tier, and (when the traced value is a const
					// NAME) dereferences it to the literal via the const map.
					meta["temporal_name_origin"] = "var_trace"
				}
				if recvName := recvNameByID[callerID]; recvName != "" {
					if arg := goTemporalDispatchArg(c.callNode); arg != nil && arg.Type() == "selector_expression" {
						op := arg.ChildByFieldName("operand")
						fld := arg.ChildByFieldName("field")
						if op != nil && fld != nil && op.Content(src) == recvName {
							meta["temporal_name_field"] = fld.Content(src)
							if rt := recvTypeByID[callerID]; rt != "" {
								meta["temporal_recv_type"] = rt
							}
						}
					}
				}
				edge := &graph.Edge{
					From: callerID, To: target,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
					Meta: meta,
				}
				stampReturnUsage(edge, c.returnUsage)
				result.Edges = append(result.Edges, edge)
				emitGoSpawnEdge(c, callerID, target, filePath, result)
				continue
			}
		}
		var target string
		if !c.isSelector {
			target = "unresolved::" + c.callName
			edge := &graph.Edge{
				From: callerID, To: target,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			stampGoGenericInstantiation(edge, c)
			applyGoGRPCRegisterMeta(edge, c, src, tenv)
			applyGoTemporalRegisterMeta(edge, c)
			applyGoTemporalHandlerMeta(edge, c)
			applyGoTemporalSignalQueryMeta(edge, c)
			applyGoTemporalStartMeta(edge, c)
			stampReturnUsage(edge, c.returnUsage)
			attachGoTemporalCallArgNames(edge, c, c.callNode, src, paramNamesByFunc[callerID])
			result.Edges = append(result.Edges, edge)
			emitGoSpawnEdge(c, callerID, target, filePath, result)
			continue
		}
		if importPath, ok := imports[c.receiver]; ok {
			target = "unresolved::extern::" + importPath + "::" + c.method
			edge := &graph.Edge{
				From: callerID, To: target,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			stampGoGenericInstantiation(edge, c)
			applyGoGRPCRegisterMeta(edge, c, src, tenv)
			applyGoTemporalRegisterMeta(edge, c)
			applyGoTemporalHandlerMeta(edge, c)
			applyGoTemporalSignalQueryMeta(edge, c)
			applyGoTemporalStartMeta(edge, c)
			stampReturnUsage(edge, c.returnUsage)
			attachGoTemporalCallArgNames(edge, c, c.callNode, src, paramNamesByFunc[callerID])
			result.Edges = append(result.Edges, edge)
			emitGoSpawnEdge(c, callerID, target, filePath, result)
			continue
		}
		target = "unresolved::*." + c.method
		edge := &graph.Edge{
			From: callerID, To: target,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		// Classify the call against the enclosing method's own receiver so the
		// capability synthesizer can attribute indirect field mutations:
		//   s.counter.Increment()  → recv_field=counter (mutates s.counter)
		//   s.helper()             → recv_self=true     (mutates s's fields
		//                                                 transitively via helper)
		if recvName := recvNameByID[callerID]; recvName != "" {
			if c.receiver == recvName {
				edge.Meta = map[string]any{"recv_self": true}
			} else if f, ok := ownReceiverField(c.receiver, recvName); ok {
				edge.Meta = map[string]any{"recv_field": f}
			}
		}
		if recvType, ok := lookupRecvType(callerID, c.receiver); ok {
			if edge.Meta == nil {
				edge.Meta = map[string]any{}
			}
			edge.Meta["receiver_type"] = recvType
		} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
			// Chain walk needs both the file-wide tenv and the
			// enclosing function's parameter scope as one map. Compose
			// without mutating either.
			composed := tenv
			if scope, ok := paramsByFunc[callerID]; ok && len(scope) > 0 {
				composed = make(typeEnv, len(tenv)+len(scope))
				for k, v := range tenv {
					composed[k] = v
				}
				for k, v := range scope {
					composed[k] = v
				}
			}
			stampFactoryChainReceiver(edge, c.receiver, resolveChainType(c.receiver, composed, result))
		}
		stampGoGenericInstantiation(edge, c)
		applyGoGRPCRegisterMeta(edge, c, src, tenv)
		applyGoTemporalRegisterMeta(edge, c)
		applyGoTemporalSignalQueryMeta(edge, c)
		applyGoTemporalStartMeta(edge, c)
		stampReturnUsage(edge, c.returnUsage)
		attachGoTemporalCallArgNames(edge, c, c.callNode, src, paramNamesByFunc[callerID])
		result.Edges = append(result.Edges, edge)
		emitGoSpawnEdge(c, callerID, target, filePath, result)
	}

	// --- Observability events ---
	emitGoObservabilityEvents(observabilityEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- Event pub/sub edges ---
	// Transport resolution needs the file's full import set, which the
	// tree walk above finished collecting, so it runs here in the
	// post-pass alongside the other deferred coverage emitters.
	emitGoPubsubEvents(pubsubCandidates, importPathValues(imports),
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- String observations (metrics, error messages, routes) ---
	emitGoStringEvents(stringEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- Feature flag checks ---
	emitGoFlagChecks(flagEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- Config keys (viper) ---
	emitGoConfigKeys(configEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- SQL queries ---
	// Dialect inference uses the same imports map the call walker
	// already builds. When the file imports a recognised SQL
	// driver, KindTable nodes get db::<dialect>::... IDs; otherwise
	// they fall back to db::generic::...
	emitGoSQLEvents(sqlEvents, inferGoSQLDialect(imports),
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- Composite literals (instantiations) ---
	for _, r := range instantiates {
		callerID := findEnclosingFunc(funcRanges, r.line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + r.typeName,
			Kind: r.kind, FilePath: filePath, Line: r.line,
		})
	}

	// --- Type assertions + declaration type references ---
	for _, r := range typeRefs {
		callerID := findEnclosingFunc(funcRanges, r.line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + r.typeName,
			Kind: r.kind, FilePath: filePath, Line: r.line,
		})
	}

	// --- Function/method values passed as arguments ---
	// Selector expression in arg position: h.handleHealth
	for _, v := range valueSels {
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + v.field,
			Kind: v.kind, FilePath: filePath, Line: v.line,
		}
		if recvType, ok := lookupRecvType(callerID, v.receiver); ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		}
		result.Edges = append(result.Edges, edge)
	}

	// Bare identifier in arg position: funcName as a value.
	for _, v := range valueIdents {
		if isGoBuiltinOrKeyword(v.name) {
			continue
		}
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + v.name,
			Kind: graph.EdgeReads, FilePath: filePath, Line: v.line,
		})
	}

	// Bare identifier as struct field value: &X{RunE: runClean}
	for _, v := range fieldValIdents {
		if isGoBuiltinOrKeyword(v.name) {
			continue
		}
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + v.name,
			Kind: graph.EdgeReads, FilePath: filePath, Line: v.line,
		})
	}

	// Selector as struct field value: {Handler: h.handleHealth}
	for _, v := range fieldValSels {
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + v.field,
			Kind: v.kind, FilePath: filePath, Line: v.line,
		}
		if recvType, ok := lookupRecvType(callerID, v.receiver); ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		}
		result.Edges = append(result.Edges, edge)
	}

	// Temporal step/executor field: `ActivityExecutor{ActivityName: "X"}`.
	// PURPOSE — emits a marker edge so the resolver can join the string
	// literal at the construction site to the dispatch that reads this field.
	// RATIONALE — separate pass because it fires on keyed_element nodes, not
	// call expressions; the construction site and dispatch site may be in
	// different functions or even files.
	// KEYWORDS — temporal, executor-field, marker
	for _, ef := range executorFields {
		callerID := findEnclosingFunc(funcRanges, ef.line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID,
			To:   "unresolved::temporal-executor::" + ef.typeName + "::" + ef.field,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: ef.line,
			Meta: map[string]any{
				"via":            "temporal.executor-field",
				"executor_type":  ef.typeName,
				"executor_field": ef.field,
				"executor_value": ef.value,
			},
		})
	}

	// Assignment / inc / dec selector LHS — EdgeWrites from the
	// enclosing function to the assigned field. Same resolution path
	// as the value-side selectors: the resolver lands on the field
	// node when we know the receiver's type, otherwise the edge stays
	// unresolved::*.field for downstream cleanup.
	for _, v := range writes {
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + v.field,
			Kind: v.kind, FilePath: filePath, Line: v.line,
		}
		if recvType, ok := lookupRecvType(callerID, v.receiver); ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		}
		result.Edges = append(result.Edges, edge)
	}

	// ORM TableName() override resolution. detectGoORMModel uses gorm's
	// snake_case+plural naming convention, but gorm honours an explicit
	// `func (T) TableName() string { return "..." }` override. The
	// override method emits as a separate KindMethod node, so this
	// post-pass runs after every node in the file has been emitted and
	// rewires any convention-derived EdgeModelsTable to the literal.
	root.WithScratch(func() { rewireORMTableNameOverrides(root, src, result) })

	// WebSocket upgrade handlers → real-time endpoint edges.
	emitGoWebSocketEdges(src, filePath, result)

	// Same-file constant/variable value references → impact-radius reads.
	root.WithScratch(func() { captureValueRefCandidates(result, root, filePath, src) })
	root.WithScratch(func() { captureFnValueCandidates(result, root, filePath, src) })

	// Gin middleware-chain dispatcher + route registrations → resolver hints.
	mineGinMiddleware(src, result)

	// Composite/generic element type references — generic arguments,
	// map key/value, channel element. These positions are dropped by the
	// bare-name type passes, so without this find_usages of a type misses
	// every place it appears only as an argument/element type.
	root.WithScratch(func() { emitGoTypeArgReferences(root, src, filePath, fileID, funcRanges, result) })

	// GoFrame reflective routes: g.Meta-tagged request structs → controllers.
	root.WithScratch(func() { captureGoFrameRoutes(result, root, filePath, src) })

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *GoExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, paramsByFunc map[string]typeEnv, paramNamesByFunc map[string]map[string]bool, imports map[string]string, seenIDs map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	id, ok := disambiguateID(seenIDs, filePath+"::"+name, def.StartLine+1)
	if !ok {
		return
	}
	if pc, ok := m.Captures["func.params"]; ok && pc != nil {
		if names := extractGoParamNames(pc.Node, src); len(names) > 0 {
			paramNamesByFunc[id] = names
		}
	}
	node := &graph.Node{
		ID:        id,
		Kind:      graph.KindFunction,
		Name:      name,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "go",
		Meta:      make(map[string]any),
	}
	node.Meta["signature"] = buildFuncSignature(name, m.Captures["func.params"], m.Captures["func.result"])
	if paramsCap, ok := m.Captures["func.params"]; ok && paramsCap != nil {
		if scope := extractGoParamTypes(paramsCap.Node, src); len(scope) > 0 {
			paramsByFunc[id] = scope
		}
	}
	if resultCap, ok := m.Captures["func.result"]; ok && resultCap.Text != "" {
		if rt := normalizeGoTypeName(resultCap.Text); rt != "" {
			node.Meta["return_type"] = rt
		}
	}
	if tp := goTypeParams(def.Node, src); len(tp) > 0 {
		node.Meta["type_params"] = tp
	}
	declStartByte := int(def.Node.StartByte())
	if doc := ExtractDocAboveByte(src, declStartByte, DocLangSlashSlash); doc != "" {
		node.Meta["doc"] = doc
	}
	node.Meta["visibility"] = VisibilityByCase(name)
	if body := goFuncBody(def.Node); body != nil {
		stampGoBodyMetrics(node, body, src)
	}
	scanGoPragmasAboveByte(src, declStartByte, node)
	result.Nodes = append(result.Nodes, node)
	// Record func-returning-literal into ConstValues sidecar for const-deref dispatch.
	if v, ok := goFuncSingleReturnLiteral(def.Node, src); ok {
		result.ConstValues = append(result.ConstValues, parser.ConstValue{
			NodeID: id, FilePath: filePath, Value: v,
		})
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitGoThrowsEdges(node, m.Captures["func.result"], filePath, result)
	emitGoFunctionShape(id, def.Node, m.Captures["func.params"], m.Captures["func.result"],
		src, filePath, def.StartLine+1, imports, result)
}

// goFuncBody returns the `block` body child of a function/method
// declaration node, or nil for declarations without a body (interface
// method shapes, abstract decls). Used by complexity counting.
func goFuncBody(decl *sitter.Node) *sitter.Node {
	if decl == nil {
		return nil
	}
	if b := decl.ChildByFieldName("body"); b != nil {
		return b
	}
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		c := decl.Child(i)
		if c != nil && c.Type() == "block" {
			return c
		}
	}
	return nil
}

// ownReceiverField returns the single field name when a selector-call receiver
// is exactly `<recvName>.<field>` — the one-hop case `s.counter.Increment()`
// where counter is a field of the enclosing method's own receiver. Deeper
// chains (s.a.b) and call expressions return ok=false so the indirect-mutation
// synthesizer never over-attributes past the first own-receiver hop.
func ownReceiverField(receiver, recvName string) (string, bool) {
	prefix := recvName + "."
	if !strings.HasPrefix(receiver, prefix) {
		return "", false
	}
	rest := receiver[len(prefix):]
	if rest == "" || strings.ContainsAny(rest, ".([ ") {
		return "", false
	}
	return rest, true
}

func (e *GoExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, paramsByFunc map[string]typeEnv, paramNamesByFunc map[string]map[string]bool, imports map[string]string, seenIDs map[string]bool) {
	name := m.Captures["method.name"].Text
	def := m.Captures["method.def"]
	receiverText := m.Captures["method.receiver"].Text
	receiverType := extractReceiverType(receiverText)
	// The node ID keeps the receiver's type-argument list (`noopCache[T]`,
	// the corpus-wide convention), but every semantic surface — the scope
	// type env, Meta["receiver"], and the member_of target — uses the bare
	// name: type nodes are minted bare, and receiver_type consumers match
	// bare names.
	bareReceiver := stripTypeArgs(receiverType)

	id, idOK := disambiguateID(seenIDs, filePath+"::"+receiverType+"."+name, def.StartLine+1)
	if !idOK {
		return
	}
	if paramsCap, ok := m.Captures["method.params"]; ok && paramsCap != nil {
		if names := extractGoParamNames(paramsCap.Node, src); len(names) > 0 {
			paramNamesByFunc[id] = names
		}
	}
	scope := typeEnv{}
	if recvName := extractReceiverName(receiverText); recvName != "" && bareReceiver != "" {
		scope[recvName] = bareReceiver
	}
	if paramsCap, ok := m.Captures["method.params"]; ok && paramsCap != nil {
		for k, v := range extractGoParamTypes(paramsCap.Node, src) {
			scope[k] = v
		}
	}
	if len(scope) > 0 {
		paramsByFunc[id] = scope
	}
	node := &graph.Node{
		ID:        id,
		Kind:      graph.KindMethod,
		Name:      name,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "go",
		Meta: map[string]any{
			"receiver": bareReceiver,
		},
	}
	if recvName := extractReceiverName(receiverText); recvName != "" {
		node.Meta["recv_name"] = recvName
	}
	node.Meta["signature"] = buildMethodSignature(receiverText, name, m.Captures["method.params"], m.Captures["method.result"])
	if resultCap, ok := m.Captures["method.result"]; ok && resultCap.Text != "" {
		if rt := normalizeGoTypeName(resultCap.Text); rt != "" {
			node.Meta["return_type"] = rt
		}
	}
	if tp := goTypeParams(def.Node, src); len(tp) > 0 {
		node.Meta["type_params"] = tp
	}
	declStartByte := int(def.Node.StartByte())
	if doc := ExtractDocAboveByte(src, declStartByte, DocLangSlashSlash); doc != "" {
		node.Meta["doc"] = doc
	}
	node.Meta["visibility"] = VisibilityByCase(name)
	if body := goFuncBody(def.Node); body != nil {
		stampGoBodyMetrics(node, body, src)
	}
	scanGoPragmasAboveByte(src, declStartByte, node)
	result.Nodes = append(result.Nodes, node)
	// Record method-returning-literal into ConstValues sidecar for const-deref dispatch.
	if v, ok := goFuncSingleReturnLiteral(def.Node, src); ok {
		result.ConstValues = append(result.ConstValues, parser.ConstValue{
			NodeID: id, FilePath: filePath, Value: v,
		})
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})

	typeID := filePath + "::" + bareReceiver
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitGoThrowsEdges(node, m.Captures["method.result"], filePath, result)
	emitGoFunctionShape(id, def.Node, m.Captures["method.params"], m.Captures["method.result"],
		src, filePath, def.StartLine+1, imports, result)
}

// goTypeParams reads the `type_parameters` child of a Go declaration
// node (function_declaration, method_declaration, type_spec) and
// returns the declared type parameters as a list of {name, bound}
// maps. Multi-name parameter declarations like `[T, U comparable]`
// produce one entry per name, each with the same bound. Returns nil
// when the declaration is not generic.
func goTypeParams(decl *sitter.Node, src []byte) []map[string]string {
	if decl == nil {
		return nil
	}
	tps := decl.ChildByFieldName("type_parameters")
	if tps == nil {
		// type_spec uses a different field shape — fall back to a
		// child-type scan.
		for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
			c := decl.Child(i)
			if c != nil && c.Type() == "type_parameter_list" {
				tps = c
				break
			}
		}
	}
	if tps == nil {
		return nil
	}
	var out []map[string]string
	for i, _nc := 0, int(tps.NamedChildCount()); i < _nc; i++ {
		pd := tps.NamedChild(i)
		if pd == nil {
			continue
		}
		if pd.Type() != "parameter_declaration" && pd.Type() != "type_parameter_declaration" {
			continue
		}
		// One parameter_declaration may carry multiple identifier
		// names that share a single type/bound:
		// `[T, U comparable]` → two names, one bound.
		var names []string
		var bound string
		// Names appear via ChildByFieldName("name") — Go grammar uses
		// field 'name' for the leading identifier list. For multi-name
		// declarations the grammar emits multiple entries with the
		// same field name; we walk children to find them all.
		for j, _nc := 0, int(pd.ChildCount()); j < _nc; j++ {
			c := pd.Child(j)
			if c == nil {
				continue
			}
			t := c.Type()
			if t == "identifier" || t == "field_identifier" || t == "type_identifier" {
				names = append(names, c.Content(src))
			}
		}
		if tn := pd.ChildByFieldName("type"); tn != nil {
			bound = strings.TrimSpace(tn.Content(src))
		}
		for _, n := range names {
			entry := map[string]string{"name": n}
			if bound != "" {
				entry["bound"] = bound
			}
			out = append(out, entry)
		}
	}
	return out
}

// emitGoThrowsEdges inspects the result-type capture and emits an
// EdgeThrows edge when the function returns an error. Two cases:
//
//   - last return type is the bare `error` interface → edge to the
//     synthetic external::error sentinel so reverse-walks land on
//     a single node regardless of file/repo.
//   - last return type is a custom error type (`*MyErr`, `MyErr`)
//     → edge to that type, resolved by name later.
//
// Functions that return only non-error types produce no edge.
func emitGoThrowsEdges(node *graph.Node, resultCap *parser.CapturedNode, filePath string, result *parser.ExtractionResult) {
	if resultCap == nil || resultCap.Text == "" {
		return
	}
	errType := parseLastReturnTypeForError(resultCap.Text)
	if errType == "" {
		return
	}
	target := "external::error"
	if errType != "error" {
		target = "unresolved::" + errType
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     node.ID,
		To:       target,
		Kind:     graph.EdgeThrows,
		FilePath: filePath,
		Line:     node.StartLine,
		Origin:   graph.OriginASTInferred,
	})
}

// parseLastReturnTypeForError pulls the last identifier from a Go
// result type expression and returns it when it looks like an error
// type. Recognises the bare `error` interface plus the conventional
// `*MyError` / `MyError` shapes. Returns "" for non-error returns.
func parseLastReturnTypeForError(result string) string {
	s := strings.TrimSpace(result)
	if s == "" {
		return ""
	}
	// Strip parens for tuple returns like `(int, error)`.
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = s[1 : len(s)-1]
	}
	parts := strings.Split(s, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if last == "" {
		return ""
	}
	// Strip leading `*` for pointer returns.
	last = strings.TrimPrefix(last, "*")
	// Take the rightmost identifier of `pkg.Foo`.
	if i := strings.LastIndex(last, "."); i >= 0 {
		last = last[i+1:]
	}
	// Strip generic instantiation suffix `[T]`.
	if i := strings.Index(last, "["); i >= 0 {
		last = last[:i]
	}
	last = strings.TrimSpace(last)
	if last == "error" {
		return "error"
	}
	// Heuristic: identifier ending in "Error" or "Err" — common Go
	// error type convention. Avoids false positives on things like
	// `Result` or `Response` while catching MyError, ParseErr, etc.
	if strings.HasSuffix(last, "Error") || strings.HasSuffix(last, "Err") {
		return last
	}
	return ""
}

// emitTypeDecl handles the generic `type X <body>` form. The body node
// discriminates struct vs interface vs named primitive — interfaces
// carry their method-signature set in Meta for structural inference.
func (e *GoExtractor) emitTypeDecl(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["typedef.name"].Text
	if seen[name] {
		return
	}
	seen[name] = true
	def := m.Captures["typedef.def"]
	body := m.Captures["typedef.body"]
	id := filePath + "::" + name

	node := &graph.Node{
		ID: id, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
	}
	isStruct := false
	isInterface := false
	if body != nil && body.Node != nil && body.Node.Type() == "interface_type" {
		node.Kind = graph.KindInterface
		node.Meta = map[string]any{"methods": extractInterfaceMethods(body.Node, src)}
		isInterface = true
	} else {
		node.Kind = graph.KindType
		node.Meta = map[string]any{}
		if body != nil && body.Node != nil && body.Node.Type() == "struct_type" {
			isStruct = true
		}
	}
	if doc := ExtractDocAboveByte(src, int(def.Node.StartByte()), DocLangSlashSlash); doc != "" {
		node.Meta["doc"] = doc
	}
	node.Meta["visibility"] = VisibilityByCase(name)
	// Structural flavor: struct/interface bodies are discriminated above;
	// everything else funnelling through `type X <named>` is a newtype.
	flavor := "newtype"
	if isInterface {
		flavor = "interface"
	} else if isStruct {
		flavor = "struct"
	}
	node.Meta["type_flavor"] = flavor
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	if isStruct {
		emitGoStructFields(body.Node, src, id, name, filePath, fileID, result)
		// ORM model attribution: emit EdgeModelsTable when the struct
		// carries gorm tags or embeds gorm.Model. Runs after field
		// emission so the field metadata is in place for downstream
		// analyzers that walk both edges together.
		detectGoORMModel(body.Node, src, id, name, filePath, result)
	}
	if isInterface {
		for _, embed := range extractEmbeddedInterfaceTypes(body.Node, src) {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     id,
				To:       "unresolved::" + embed,
				Kind:     graph.EdgeComposes,
				FilePath: filePath,
				Line:     def.StartLine + 1,
				Origin:   graph.OriginASTInferred,
			})
		}
	}
	// Newtype `type X Y` where Y is a named type produces an
	// EdgeExtends so blast-radius walks catch the dependency. Struct
	// and interface bodies don't get an extends edge — their member
	// fields/methods carry the reference set instead.
	if !isStruct && body != nil && body.Node != nil {
		if isExtendsNewtypeBody(body.Node.Type()) {
			if target := canonicalizeGoTypeRef(body.Text); target != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From:     id,
					To:       "unresolved::" + target,
					Kind:     graph.EdgeExtends,
					FilePath: filePath,
					Line:     def.StartLine + 1,
					Origin:   graph.OriginASTInferred,
				})
			}
		}
	}
}

// isExtendsNewtypeBody reports whether a type_spec body's AST node
// is a named-type reference that should produce an EdgeExtends edge.
// Composite anonymous bodies (struct_type / interface_type / map_type
// / func_type) are excluded — their member set carries the
// references; emitting an extends edge to "anonymous" provides no
// additional signal.
func isExtendsNewtypeBody(t string) bool {
	switch t {
	case "type_identifier", "qualified_type", "pointer_type",
		"slice_type", "array_type", "channel_type", "generic_type":
		return true
	}
	return false
}

// emitGoStructFields walks a struct_type node's field_declaration list
// and emits one KindField node per declared field. Each field gets a
// MemberOf edge to its owner type and a Defines edge from the file.
// Embedded fields (anonymous struct/interface inclusion) emit a single
// field node named after the embedded type.
func emitGoStructFields(structNode *sitter.Node, src []byte, ownerID, ownerName, filePath, fileID string, result *parser.ExtractionResult) {
	if structNode == nil {
		return
	}
	var fieldList *sitter.Node
	for i, _nc := 0, int(structNode.ChildCount()); i < _nc; i++ {
		c := structNode.Child(i)
		if c != nil && c.Type() == "field_declaration_list" {
			fieldList = c
			break
		}
	}
	if fieldList == nil {
		return
	}
	for i, _nc := 0, int(fieldList.NamedChildCount()); i < _nc; i++ {
		decl := fieldList.NamedChild(i)
		if decl == nil || decl.Type() != "field_declaration" {
			continue
		}
		// Walk the field_declaration's children once: collect
		// field_identifier names and the trailing type node. The
		// grammar exposes both via ChildByFieldName, but real-world
		// trees contain a mix of named/positional children for
		// embedded vs explicit fields, so a manual walk is the
		// reliable form.
		var nameNodes []*sitter.Node
		var typeNode *sitter.Node
		for j, _nc := 0, int(decl.NamedChildCount()); j < _nc; j++ {
			c := decl.NamedChild(j)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "field_identifier":
				nameNodes = append(nameNodes, c)
			case "type_identifier", "qualified_type", "pointer_type",
				"generic_type", "slice_type", "array_type", "map_type",
				"channel_type", "function_type", "interface_type",
				"struct_type":
				if typeNode == nil {
					typeNode = c
				}
			}
		}
		var fieldType string
		if typeNode != nil {
			// Keep the verbatim type text — primitives ("string",
			// "int", "[]byte", etc.) are valid field types and
			// agents want to see them. normalizeGoTypeName drops
			// primitives because it's tuned for receiver-type
			// resolution; field metadata has different needs.
			fieldType = strings.TrimSpace(typeNode.Content(src))
		}
		if len(nameNodes) > 0 {
			for _, nm := range nameNodes {
				emitGoFieldNode(decl, nm, nm.Content(src), fieldType, ownerID, ownerName, filePath, fileID, src, result)
			}
			continue
		}
		// Embedded field: type itself is the field name.
		if typeNode != nil {
			if fieldName := embeddedFieldName(typeNode, src); fieldName != "" {
				emitGoFieldNode(decl, typeNode, fieldName, fieldType, ownerID, ownerName, filePath, fileID, src, result)
				// Composition signal: the owning type embeds another
				// type. Distinct from EdgeExtends (newtype) and from
				// the field's MemberOf — gives blast-radius walks
				// "what types are composed into X" in one hop.
				if target := canonicalizeGoTypeRef(fieldType); target != "" {
					result.Edges = append(result.Edges, &graph.Edge{
						From:     ownerID,
						To:       "unresolved::" + target,
						Kind:     graph.EdgeComposes,
						FilePath: filePath,
						Line:     int(typeNode.StartPoint().Row) + 1,
						Origin:   graph.OriginASTInferred,
					})
				}
			}
		}
	}
}

func emitGoFieldNode(decl, anchor *sitter.Node, fieldName, fieldType, ownerID, ownerName, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	id := filePath + "::" + ownerName + "." + fieldName
	startLine := int(anchor.StartPoint().Row) + 1
	endLine := int(decl.EndPoint().Row) + 1
	meta := map[string]any{
		"receiver":   ownerName,
		"visibility": VisibilityByCase(fieldName),
	}
	if fieldType != "" {
		meta["field_type"] = fieldType
	}
	if doc := ExtractDocAboveByte(src, int(anchor.StartByte()), DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindField, Name: fieldName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "go", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
	})
}

// embeddedFieldName returns the trailing identifier of a Go embedded
// field type. Strips one leading `*` for pointer-embedded fields and
// drops the package qualifier. Returns "" when no identifier is found.
func embeddedFieldName(typeNode *sitter.Node, src []byte) string {
	if typeNode == nil {
		return ""
	}
	switch typeNode.Type() {
	case "type_identifier":
		return typeNode.Content(src)
	case "pointer_type":
		// Recurse into the pointed-to type.
		for i, _nc := 0, int(typeNode.NamedChildCount()); i < _nc; i++ {
			if n := embeddedFieldName(typeNode.NamedChild(i), src); n != "" {
				return n
			}
		}
	case "qualified_type":
		// pkg.Foo — take the trailing identifier.
		for i := int(typeNode.NamedChildCount()) - 1; i >= 0; i-- {
			c := typeNode.NamedChild(i)
			if c != nil && c.Type() == "type_identifier" {
				return c.Content(src)
			}
		}
	case "generic_type":
		// Foo[T] — name is the inner type_identifier.
		for i, _nc := 0, int(typeNode.NamedChildCount()); i < _nc; i++ {
			if n := embeddedFieldName(typeNode.NamedChild(i), src); n != "" {
				return n
			}
		}
	}
	return ""
}

func (e *GoExtractor) emitTypeAlias(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["alias.name"].Text
	if seen[name] {
		return
	}
	seen[name] = true
	def := m.Captures["alias.def"]
	id := filePath + "::" + name
	node := &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
		Meta: map[string]any{
			// Mark this node as an alias so consumers can scope queries
			// without inspecting the edge set. Newtypes get no flag —
			// the absence is the signal.
			"alias": true,
		},
	}
	if doc := ExtractDocAboveByte(src, int(def.Node.StartByte()), DocLangSlashSlash); doc != "" {
		node.Meta["doc"] = doc
	}
	node.Meta["visibility"] = VisibilityByCase(name)
	node.Meta["type_flavor"] = "type_alias"
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	// EdgeAliases captures the `type X = Y` relationship distinctly
	// from EdgeExtends (newtype). Renaming Y is a no-op for X's
	// callers in the alias case but breaks promoted method sets in
	// the newtype case — agents need to distinguish.
	if aliasType, ok := m.Captures["alias.type"]; ok && aliasType != nil {
		if target := canonicalizeGoTypeRef(aliasType.Text); target != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     id,
				To:       "unresolved::" + target,
				Kind:     graph.EdgeAliases,
				FilePath: filePath,
				Line:     def.StartLine + 1,
				Origin:   graph.OriginASTInferred,
			})
		}
	}
}

// emitImport records an import edge and populates the alias→path map
// used when classifying selector calls against imported packages. Blank
// and dot imports are skipped in the map (they don't introduce a
// callable identifier) but still produce EdgeImports.
//
// In addition to the existing file→import edge, emits a per-import
// node (KindImport) with Meta carrying the import path, alias (if
// renamed), and is_external flag. Lets agents query "what does this
// file import from <pkg>" with one graph hop.
func (e *GoExtractor) emitImport(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, imports map[string]string, fileNode *graph.Node) {
	pathCap := m.Captures["import.path"]
	importPath := strings.Trim(pathCap.Text, `"`)
	line := pathCap.StartLine + 1

	// Mark the file as a cgo user when it imports "C" — the
	// pseudo-import that triggers Go's cgo preprocessor and
	// signals the file has a C source dependency. Agents asking
	// "which files would break if we removed cgo" or "what's the
	// porting surface to a non-cgo build" can then filter by
	// this meta flag without parsing source.
	if importPath == "C" && fileNode != nil {
		if fileNode.Meta == nil {
			fileNode.Meta = map[string]any{}
		}
		fileNode.Meta["uses_cgo"] = true
	}

	rawAlias := ""
	if a, ok := m.Captures["import.alias"]; ok {
		rawAlias = strings.TrimSpace(a.Text)
	}
	displayName := rawAlias
	mapAlias := rawAlias
	switch rawAlias {
	case "_", ".":
		// Blank and dot imports keep their special behaviour for
		// the alias map (no callable identifier introduced) but the
		// import node still gets emitted so reverse-walks work.
		displayName = importPath
		if i := strings.LastIndex(importPath, "/"); i >= 0 {
			displayName = importPath[i+1:]
		}
	case "":
		displayName = importPath
		if i := strings.LastIndex(importPath, "/"); i >= 0 {
			displayName = importPath[i+1:]
		}
		mapAlias = displayName
	}

	importNodeID := filePath + "::import::" + importPath
	importMeta := map[string]any{
		"path":        importPath,
		"is_external": isExternalGoImport(importPath),
	}
	if rawAlias != "" {
		importMeta["alias"] = rawAlias
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID:        importNodeID,
		Kind:      graph.KindImport,
		Name:      displayName,
		FilePath:  filePath,
		StartLine: line,
		EndLine:   line,
		Language:  "go",
		Meta:      importMeta,
	})
	// File → import-node edge. EdgeContains is the semantic fit (the
	// file *contains* an import statement; it doesn't *define* the
	// imported package). The disk-backed GetFileSubGraph walks
	// EdgeDefines ∪ EdgeContains from the file node to enumerate the
	// full neighbourhood in one edge-index pass.
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       importNodeID,
		Kind:     graph.EdgeContains,
		FilePath: filePath,
		Line:     line,
	})
	// Existing file → unresolved import-path edge for resolver
	// behaviour (downstream code resolves the path to the imported
	// repo's file node). Kept additive so consumers that read
	// EdgeImports keep working unchanged.
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + importPath,
		Kind:     graph.EdgeImports,
		FilePath: filePath,
		Line:     line,
	})

	if rawAlias == "_" || rawAlias == "." {
		return
	}
	imports[mapAlias] = importPath
}

// isExternalGoImport returns true when the import path doesn't look
// like a stdlib import. Heuristic: the first path segment contains a
// dot — i.e. it's a module path like `github.com/...` or
// `golang.org/...`. Stdlib paths (`fmt`, `os/exec`) have no dot in
// the first segment.
func isExternalGoImport(path string) bool {
	if path == "" {
		return false
	}
	first := path
	if i := strings.Index(path, "/"); i >= 0 {
		first = path[:i]
	}
	return strings.Contains(first, ".")
}

func (e *GoExtractor) emitVar(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, tenv typeEnv) {
	nameCap := m.Captures["var.name"]
	def := m.Captures["var.def"]
	if nameCap == nil || nameCap.Text == "" || nameCap.Text == "_" {
		return
	}
	name := nameCap.Text
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
		Meta:     map[string]any{"visibility": VisibilityByCase(name)},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	if typeCap, ok := m.Captures["var.type"]; ok && typeCap.Text != "" {
		if typeName := normalizeGoTypeName(typeCap.Text); typeName != "" {
			tenv[name] = typeName
		}
	}
}

func (e *GoExtractor) emitConst(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	nameCap := m.Captures["const.name"]
	def := m.Captures["const.def"]
	if nameCap == nil || nameCap.Text == "" || nameCap.Text == "_" {
		return
	}
	name := nameCap.Text
	id := filePath + "::" + name
	// Detect Go's enum-by-convention: a const_declaration block that
	// contains the `iota` identifier anywhere within its body marks
	// every spec in that block as an enum member. The heuristic
	// matches both the explicit form (`Red Color = iota`) and the
	// implicit-continuation form (`Red Color = iota; Green; Blue`),
	// which share the same enclosing block. False positives are
	// possible if the user happens to write `iota` inside a const
	// block for an unrelated reason — vanishingly rare in practice.
	kind := graph.KindConstant
	if def != nil && def.Node != nil && containsGoIotaBlock(def.Text) {
		kind = graph.KindEnumMember
	}
	meta := map[string]any{"visibility": VisibilityByCase(name)}
	// Const-to-const alias (Cat 3): `const ALIAS = RealName` carries no
	// literal of its own, so record the referenced const NAME. The
	// resolver's const-deref map collapses alias chains (ALIAS = REAL =
	// "lit") against the constant_values sidecar by a bounded fixpoint, so
	// an ALL_CAPS dispatch name that is itself a const alias still
	// dereferences to the underlying literal. A const WITH a literal value
	// is handled by the constant_values sidecar below, not here.
	if def != nil && def.Node != nil {
		if _, ok := goConstLiteralValue(def.Node, name, src); !ok {
			if ref, ok := goConstRefName(def.Node, name, src); ok {
				meta["const_ref"] = ref
			}
		}
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	// Retain the literal value (string / numeric) for the queryable
	// constant_values sidecar, so the resolver can dereference a
	// const-identifier dispatch name to its value across files. Computed
	// constants (iota, expressions) carry no literal and are skipped.
	if kind == graph.KindConstant {
		if v, ok := goConstLiteralValue(def.Node, name, src); ok {
			result.ConstValues = append(result.ConstValues, parser.ConstValue{
				NodeID: id, FilePath: filePath, Value: v,
			})
		}
	}
}

// PURPOSE — reports the string literal a function body returns when the body
// is exactly one `return "<literal>"` statement. Used by the const-value
// sidecar to record func-returning-constant nodes for Temporal dispatch
// resolution without adding a new resolver pass.
// RATIONALE — mirrors goConstLiteralValue for the function case.
// KEYWORDS — temporal, const-deref, single-return-literal
func goFuncSingleReturnLiteral(declNode *sitter.Node, src []byte) (string, bool) {
	if declNode == nil {
		return "", false
	}
	body := declNode.ChildByFieldName("body")
	if body == nil || body.Type() != "block" {
		return "", false
	}
	// The Go grammar wraps a block's statements in a `statement_list` node;
	// descend into it when present so the single-statement scan sees the
	// real statements rather than the wrapper.
	stmtParent := body
	if body.NamedChildCount() == 1 {
		if only := body.NamedChild(0); only != nil && only.Type() == "statement_list" {
			stmtParent = only
		}
	}
	var ret *sitter.Node
	stmts := 0
	for i, _nc := 0, int(stmtParent.NamedChildCount()); i < _nc; i++ {
		c := stmtParent.NamedChild(i)
		if c == nil || c.Type() == "comment" {
			continue
		}
		stmts++
		if c.Type() == "return_statement" {
			ret = c
		}
	}
	if stmts != 1 || ret == nil {
		return "", false
	}
	// return_statement -> expression_list -> single string literal
	var expr *sitter.Node
	for i, _nc := 0, int(ret.NamedChildCount()); i < _nc; i++ {
		n := ret.NamedChild(i)
		if n == nil {
			continue
		}
		if n.Type() == "expression_list" {
			if n.NamedChildCount() != 1 {
				return "", false
			}
			expr = n.NamedChild(0)
		} else {
			expr = n
		}
	}
	if expr == nil {
		return "", false
	}
	switch expr.Type() {
	case "interpreted_string_literal", "raw_string_literal":
		t := expr.Content(src)
		if len(t) >= 2 && (t[0] == '"' || t[0] == '`') {
			return t[1 : len(t)-1], true
		}
		return t, true
	}
	return "", false
}

// goConstLiteralValue extracts the literal value of a const_spec
// (`const X = "literal"` / `const X = 42`) from the spec's value field,
// when that value is a string or numeric literal. Returns ("", false)
// for computed / multi-value / non-literal specs.
//
// constSpec may be the const_spec itself or the enclosing
// const_declaration. For a grouped block (`const ( A = ...; B = ... )`)
// the declaration holds several specs; name selects the matching one so
// each member's value is captured independently (not just single-spec
// blocks). For a single-spec block name still selects correctly.
func goConstLiteralValue(constSpec *sitter.Node, name string, src []byte) (string, bool) {
	if constSpec == nil {
		return "", false
	}
	spec := constSpec
	if spec.Type() != "const_spec" {
		// def captures the const_declaration; pick the spec whose name
		// field matches the const being emitted (grouped blocks hold
		// several), falling back to the lone spec when there is exactly
		// one and no name match (defensive).
		var found, only *sitter.Node
		count := 0
		for i, _nc := 0, int(spec.NamedChildCount()); i < _nc; i++ {
			c := spec.NamedChild(i)
			if c == nil || c.Type() != "const_spec" {
				continue
			}
			count++
			only = c
			if nameNode := c.ChildByFieldName("name"); nameNode != nil && nameNode.Content(src) == name {
				found = c
				break
			}
		}
		switch {
		case found != nil:
			spec = found
		case count == 1 && only != nil:
			spec = only
		default:
			return "", false
		}
	}
	valueList := spec.ChildByFieldName("value")
	if valueList == nil || valueList.NamedChildCount() != 1 {
		return "", false
	}
	v := valueList.NamedChild(0)
	if v == nil {
		return "", false
	}
	switch v.Type() {
	case "interpreted_string_literal", "raw_string_literal":
		return goTemporalNameFromExpr(v, src), true
	case "int_literal", "float_literal":
		return v.Content(src), true
	}
	return "", false
}

// goConstRefName returns the NAME of the constant that `name`'s value is a
// plain one-hop reference to (`const ALIAS = RealName`), or ("", false) when
// the value is not a single bare-identifier reference. The resolver aliases
// one constant's value to another via the constant_values sidecar (chains
// collapsed by a bounded fixpoint), so an ALL_CAPS dispatch name that is
// itself a const alias still resolves to the underlying literal.
func goConstRefName(constDecl *sitter.Node, name string, src []byte) (string, bool) {
	if constDecl == nil {
		return "", false
	}
	for i, _nc := 0, int(constDecl.NamedChildCount()); i < _nc; i++ {
		spec := constDecl.NamedChild(i)
		if spec == nil || spec.Type() != "const_spec" {
			continue
		}
		nameNode := spec.ChildByFieldName("name")
		if nameNode == nil || nameNode.Content(src) != name {
			continue
		}
		val := spec.ChildByFieldName("value")
		if val == nil {
			return "", false
		}
		expr := val
		if val.Type() == "expression_list" {
			expr = val.NamedChild(0)
		}
		if expr == nil || expr.Type() != "identifier" {
			return "", false
		}
		ref := expr.Content(src)
		if ref == "" || ref == name {
			return "", false
		}
		return ref, true
	}
	return "", false
}

// containsGoIotaBlock reports whether a const_declaration's source
// text contains the bare `iota` identifier. Word-boundary anchoring
// keeps `bigiotabig` and similar identifier substrings from
// triggering. Simpler than walking the AST for a one-off detection.
func containsGoIotaBlock(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i+4 <= len(text); i++ {
		if text[i:i+4] != "iota" {
			continue
		}
		before := byte(' ')
		if i > 0 {
			before = text[i-1]
		}
		after := byte(' ')
		if i+4 < len(text) {
			after = text[i+4]
		}
		if !isGoIdentByte(before) && !isGoIdentByte(after) {
			return true
		}
	}
	return false
}

func isGoIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// recordShortVarType infers a type from the RHS expression of a short
// variable declaration (`x := NewFoo()` or `x := &Foo{...}`) and records
// it in tenv so subsequent selector-call edges attach a receiver_type
// meta. No graph output — short-var-declared locals are not first-class
// nodes in the current schema.
func (e *GoExtractor) recordShortVarType(m parser.QueryResult, src []byte, tenv typeEnv) {
	name := m.Captures["svar.name"].Text
	valueCap := m.Captures["svar.value"]
	if valueCap == nil || valueCap.Node == nil {
		return
	}
	if inferred := inferTypeFromGoExpr(valueCap.Node, src); inferred != "" {
		tenv[name] = inferred
	}
}

// --- gRPC stub call extraction (M4) --------------------------------
//
// The Go gRPC code generator produces a stable surface that lets us
// resolve cross-service RPC calls structurally, without a type checker:
//
//   - Client construction: `pkg.New<Service>Client(conn)` returns a
//     `<Service>Client` stub interface.
//   - RPC invocation:       `stub.<Method>(ctx, req, ...)`.
//   - Server registration:  `pkg.Register<Service>Server(registrar, impl)`
//     binds a concrete impl type to the service.
//
// recordGoGRPCStubVar / goGRPCStubService recognise the consumer side
// and route each RPC call to an `unresolved::grpc::<Service>::<Method>`
// placeholder; parseGoGRPCRegister recognises the server side and
// stamps `grpc_register_*` meta on the registration call edge. The
// resolver's ResolveGRPCStubCalls pass joins the two.

// recordGoGRPCStubVar inspects a short-var declaration's RHS for the
// gRPC client-stub constructor convention `pkg.New<Service>Client(conn)`
// and, when matched, records `varName → serviceName` so later method
// calls on that variable resolve as gRPC stub calls.
func recordGoGRPCStubVar(m parser.QueryResult, src []byte, stubs map[string]string) {
	nameCap := m.Captures["svar.name"]
	valueCap := m.Captures["svar.value"]
	if nameCap == nil || valueCap == nil || valueCap.Node == nil {
		return
	}
	if svc, ok := goGRPCServiceFromConstructor(valueCap.Node, src); ok {
		stubs[nameCap.Text] = svc
	}
}

// goGRPCServiceFromConstructor returns the gRPC service name when node
// is a `New<Service>Client(...)` call expression (qualified or not).
func goGRPCServiceFromConstructor(node *sitter.Node, src []byte) (string, bool) {
	if node == nil || node.Type() != "call_expression" {
		return "", false
	}
	fn := node.ChildByFieldName("function")
	if fn == nil {
		return "", false
	}
	var name string
	switch fn.Type() {
	case "identifier":
		name = fn.Content(src)
	case "selector_expression":
		if field := fn.ChildByFieldName("field"); field != nil {
			name = field.Content(src)
		}
	}
	return grpcServiceFromClientCtor(name)
}

// grpcServiceFromClientCtor maps `New<Service>Client` → `<Service>`.
func grpcServiceFromClientCtor(name string) (string, bool) {
	const pfx, sfx = "New", "Client"
	if len(name) <= len(pfx)+len(sfx) {
		return "", false
	}
	if !strings.HasPrefix(name, pfx) || !strings.HasSuffix(name, sfx) {
		return "", false
	}
	return name[len(pfx) : len(name)-len(sfx)], true
}

// goGRPCStubService reports whether a selector call's receiver is a
// gRPC client stub — either a variable seeded by recordGoGRPCStubVar,
// or an inline `pkg.New<Service>Client(conn)` chain — and returns the
// service name.
func goGRPCStubService(c goDeferredCall, stubVars map[string]string) (string, bool) {
	if svc, ok := stubVars[c.receiver]; ok {
		return svc, true
	}
	return grpcServiceFromInlineChain(c.receiver)
}

// grpcServiceFromInlineChain matches the inline-chain receiver text
// `pkg.New<Service>Client(...)` (or unqualified) and returns the
// service. The receiver must be exactly one balanced call expression —
// a longer chain like `New<Service>Client(c).Foo()` is rejected so the
// trailing method isn't mistaken for an RPC on the stub.
func grpcServiceFromInlineChain(receiver string) (string, bool) {
	open := strings.IndexByte(receiver, '(')
	if open < 0 || !strings.HasSuffix(receiver, ")") {
		return "", false
	}
	depth := 0
	for i := open; i < len(receiver); i++ {
		switch receiver[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(receiver)-1 {
				return "", false
			}
		}
	}
	head := receiver[:open]
	if dot := strings.LastIndexByte(head, '.'); dot >= 0 {
		head = head[dot+1:]
	}
	return grpcServiceFromClientCtor(head)
}

// grpcRegisterArgNode recognises the generated gRPC server-registration
// call `Register<Service>Server(registrar, impl)` (qualified or not)
// and returns the service name plus the AST node of the second
// argument (the impl). The impl *type* is resolved later, in the call
// post-pass, once the file-wide type environment is fully populated.
// Returns ok=false for any other call.
func grpcRegisterArgNode(callNode *sitter.Node, funcName string) (service string, argNode *sitter.Node, ok bool) {
	service, ok = grpcServiceFromRegister(funcName)
	if !ok {
		return "", nil, false
	}
	if callNode == nil || callNode.Type() != "call_expression" {
		return "", nil, false
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return "", nil, false
	}
	// Second positional argument names the server implementation.
	count := 0
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		count++
		if count == 2 {
			return service, c, true
		}
	}
	return "", nil, false
}

// grpcServiceFromRegister maps `Register<Service>Server` → `<Service>`.
func grpcServiceFromRegister(name string) (string, bool) {
	const pfx, sfx = "Register", "Server"
	if len(name) <= len(pfx)+len(sfx) {
		return "", false
	}
	if !strings.HasPrefix(name, pfx) || !strings.HasSuffix(name, sfx) {
		return "", false
	}
	return name[len(pfx) : len(name)-len(sfx)], true
}

// goGRPCImplType extracts a type name from a gRPC server-registration
// second argument: `&userServer{}`, `userServer{}`, `pkg.userServer{}`,
// `NewUserServer()`, `pkg.NewUserServer()`, or a bare identifier whose
// type is known from the file's type environment. Returns "" when the
// expression doesn't unambiguously name a type.
func goGRPCImplType(node *sitter.Node, src []byte, tenv typeEnv) string {
	if node == nil {
		return ""
	}
	switch node.Type() {
	case "unary_expression", "composite_literal", "call_expression":
		return normalizeGoTypeName(inferTypeFromGoExpr(node, src))
	case "identifier":
		return normalizeGoTypeName(tenv[node.Content(src)])
	}
	return ""
}

// applyGoGRPCRegisterMeta resolves the impl type named by a gRPC
// server-registration call's second argument — against the now-complete
// file-wide type environment — and stamps `grpc_register_*` meta onto
// the registration call's edge so the resolver's ResolveGRPCStubCalls
// pass can join client-stub calls to the server handler. No-op for
// non-registration calls and for impl arguments whose type can't be
// named (e.g. a bare field selector).
func applyGoGRPCRegisterMeta(edge *graph.Edge, c goDeferredCall, src []byte, tenv typeEnv) {
	if edge == nil || c.grpcRegService == "" || c.grpcRegArgNode == nil {
		return
	}
	impl := goGRPCImplType(c.grpcRegArgNode, src, tenv)
	if impl == "" {
		return
	}
	if edge.Meta == nil {
		edge.Meta = map[string]any{}
	}
	edge.Meta["grpc_register_service"] = c.grpcRegService
	edge.Meta["grpc_register_impl"] = impl
}

// --- Helpers -------------------------------------------------------

type funcRange struct {
	id        string
	startLine int
	endLine   int
}

func buildFuncRanges(result *parser.ExtractionResult) []funcRange {
	var ranges []funcRange
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			ranges = append(ranges, funcRange{
				id: n.ID, startLine: n.StartLine, endLine: n.EndLine,
			})
		}
	}
	return ranges
}

// findEnclosingFunc returns the deepest declared function or method covering
// line. Parser emission order is not a scope order, so nested declarations are
// selected by span with a stable identity tie-break. Closures are deliberately
// excluded from buildFuncRanges: line-only references cannot distinguish an
// outer registration call from an inner same-line closure call; extractors that
// retain AST positions must resolve those through syntax ancestry instead.
func findEnclosingFunc(ranges []funcRange, line int) string {
	bestID := ""
	bestStart := -1
	bestSpan := int(^uint(0) >> 1)
	for _, r := range ranges {
		if line < r.startLine || line > r.endLine {
			continue
		}
		span := r.endLine - r.startLine
		if span < 0 {
			continue
		}
		if bestID == "" || span < bestSpan ||
			(span == bestSpan && r.startLine > bestStart) ||
			(span == bestSpan && r.startLine == bestStart && r.id < bestID) {
			bestID = r.id
			bestStart = r.startLine
			bestSpan = span
		}
	}
	return bestID
}

// splitReceiverFields splits a receiver's text on top-level whitespace
// only — whitespace inside a type-argument list (`Indexed[K, V]`) is part
// of the type, not a field separator. strings.Fields would split there
// and hand back a `V]` fragment as the receiver type, minting a broken
// identity for the method and every child symbol under it.
func splitReceiverFields(receiver string) []string {
	var fields []string
	depth := 0
	start := -1
	for i, r := range receiver {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && (r == ' ' || r == '\t' || r == '\n' || r == '\r') {
			if start >= 0 {
				fields = append(fields, receiver[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, receiver[start:])
	}
	return fields
}

// stripTypeArgs cuts a receiver type's type-argument list:
// "Indexed[K,V]" -> "Indexed". The bare name is what type nodes, the
// receiver_type resolution hints, and the param-type env all use.
func stripTypeArgs(t string) string {
	if i := strings.Index(t, "["); i >= 0 {
		return t[:i]
	}
	return t
}

// extractReceiverName extracts the receiver name from a Go receiver
// parameter list. "(s *Server)" -> "s", "(Server)" -> "" (unnamed).
// Used to seed paramsByFunc so calls like `s.foo()` inside the method
// body resolve via the receiver's type.
func extractReceiverName(receiver string) string {
	receiver = strings.Trim(receiver, "()")
	parts := splitReceiverFields(receiver)
	if len(parts) < 2 {
		return ""
	}
	name := parts[0]
	// A first token that is (or starts) the type itself means the
	// receiver is unnamed — `(*Indexed[K, V])` splits to one bracket-
	// aware field, but defensive formatting like `(* Indexed[K, V])`
	// still must not read `*` as a name.
	if name == "" || name == "_" || strings.ContainsAny(name, "*[") {
		return ""
	}
	return name
}

// extractGoParamTypes walks a parameter_list AST node and returns a
// map from parameter name to its declared (normalized) type. Multi-name
// declarations like `(a, b *Foo)` produce one entry per name. Unnamed
// parameters, blank identifiers, and types that normalizeGoTypeName
// drops (primitives, map/chan/func) are skipped — callers only care
// about names that point at receiver types we could resolve methods on.
// extractGoParamNames returns the set of ALL parameter names declared in a
// parameter_list, regardless of type (unlike extractGoParamTypes, which
// keeps only params with a non-builtin type for receiver resolution). Used
// by the Temporal wrapper detector to recognise a forwarded parameter.
func extractGoParamNames(paramList *sitter.Node, src []byte) map[string]bool {
	if paramList == nil {
		return nil
	}
	out := map[string]bool{}
	for i, _nc := 0, int(paramList.NamedChildCount()); i < _nc; i++ {
		decl := paramList.NamedChild(i)
		if decl == nil {
			continue
		}
		if t := decl.Type(); t != "parameter_declaration" && t != "variadic_parameter_declaration" {
			continue
		}
		typeNode := decl.ChildByFieldName("type")
		for j, _nc := 0, int(decl.NamedChildCount()); j < _nc; j++ {
			c := decl.NamedChild(j)
			if c == nil || c == typeNode {
				continue
			}
			if c.Type() == "identifier" {
				out[c.Content(src)] = true
			}
		}
	}
	return out
}

func extractGoParamTypes(paramList *sitter.Node, src []byte) typeEnv {
	if paramList == nil {
		return nil
	}
	out := typeEnv{}
	for i, _nc := 0, int(paramList.NamedChildCount()); i < _nc; i++ {
		decl := paramList.NamedChild(i)
		if decl == nil {
			continue
		}
		t := decl.Type()
		if t != "parameter_declaration" && t != "variadic_parameter_declaration" {
			continue
		}
		typeNode := decl.ChildByFieldName("type")
		if typeNode == nil {
			continue
		}
		typeName := normalizeGoTypeName(typeNode.Content(src))
		if typeName == "" {
			continue
		}
		// Walk all identifier children — Go allows multiple names per
		// parameter declaration sharing one type. Skip the type node
		// itself (which may also be reported as a named child).
		for j, _nc := 0, int(decl.NamedChildCount()); j < _nc; j++ {
			c := decl.NamedChild(j)
			if c == nil || c == typeNode {
				continue
			}
			if c.Type() == "identifier" {
				name := c.Content(src)
				if name == "" || name == "_" {
					continue
				}
				out[name] = typeName
			}
		}
	}
	return out
}

// extractReceiverType extracts the type name from a Go receiver parameter list.
// "(s *Server)" -> "Server", "(s Server)" -> "Server".
func extractReceiverType(receiver string) string {
	receiver = strings.Trim(receiver, "()")
	parts := splitReceiverFields(receiver)
	if len(parts) == 0 {
		return ""
	}
	typePart := parts[len(parts)-1]
	typePart = strings.TrimPrefix(typePart, "*")
	// Canonicalise formatting drift inside a type-argument list:
	// `Indexed[K, V]` and `Indexed[K,V]` must mint the same identity.
	if strings.ContainsRune(typePart, ' ') {
		typePart = strings.ReplaceAll(typePart, " ", "")
	}
	return typePart
}

func buildFuncSignature(name string, params, result *parser.CapturedNode) string {
	sig := fmt.Sprintf("func %s%s", name, captureText(params))
	if result != nil && result.Text != "" {
		sig += " " + result.Text
	}
	return sig
}

func buildMethodSignature(receiver, name string, params, result *parser.CapturedNode) string {
	sig := fmt.Sprintf("func (%s) %s%s", receiver, name, captureText(params))
	if result != nil && result.Text != "" {
		sig += " " + result.Text
	}
	return sig
}

// extractInterfaceMethods walks the children of an interface_type node
// and returns the names of all method_spec / method_elem entries.
func extractInterfaceMethods(ifaceNode *sitter.Node, src []byte) []string {
	var methods []string
	for i, _nc := 0, int(ifaceNode.NamedChildCount()); i < _nc; i++ {
		child := ifaceNode.NamedChild(i)
		if child.Type() == "method_elem" || child.Type() == "method_spec" {
			for j, _nc := 0, int(child.NamedChildCount()); j < _nc; j++ {
				nameNode := child.NamedChild(j)
				if nameNode.Type() == "field_identifier" {
					methods = append(methods, nameNode.Content(src))
					break
				}
			}
		}
	}
	return methods
}

// extractEmbeddedInterfaceTypes walks the children of an
// interface_type node and returns the canonicalised names of
// embedded interface references. The Go grammar wraps embedded
// interfaces and type-set elements inside `type_elem` nodes; older
// grammar versions (and gofmt edge cases) sometimes inline the bare
// type_identifier / qualified_type directly. Both shapes are
// accepted so EdgeComposes is emitted regardless of which the
// grammar produces.
func extractEmbeddedInterfaceTypes(ifaceNode *sitter.Node, src []byte) []string {
	var out []string
	visit := func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "type_identifier", "qualified_type", "generic_type":
			if t := canonicalizeGoTypeRef(n.Content(src)); t != "" {
				out = append(out, t)
			}
		}
	}
	for i, _nc := 0, int(ifaceNode.NamedChildCount()); i < _nc; i++ {
		child := ifaceNode.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "type_elem":
			// Modern grammar: each embedded type is one type_elem
			// child. The named child of type_elem is the type
			// reference itself.
			for j, _nc := 0, int(child.NamedChildCount()); j < _nc; j++ {
				visit(child.NamedChild(j))
			}
		case "type_identifier", "qualified_type", "generic_type":
			visit(child)
		}
	}
	return out
}

func captureText(c *parser.CapturedNode) string {
	if c == nil {
		return "()"
	}
	return c.Text
}

// --- Type environment -----------------------------------------------

// typeEnv maps variable name → inferred type name within a file.
type typeEnv map[string]string

// normalizeGoTypeName strips pointer prefix and package qualifier.
// "*pkg.Foo" → "Foo", "[]*Foo" → "Foo", "map[K]V" → "" (skipped —
// receiver typing doesn't help for map/slice/chan types).
func normalizeGoTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Strip array / slice prefixes.
	t = strings.TrimPrefix(t, "[]")
	if strings.HasPrefix(t, "[") {
		if end := strings.Index(t, "]"); end >= 0 {
			t = t[end+1:]
		}
	}
	// Skip map/chan/func types — can't meaningfully resolve a method call
	// through them at the grain we support.
	if strings.HasPrefix(t, "map[") || strings.HasPrefix(t, "chan ") || strings.HasPrefix(t, "func(") {
		return ""
	}
	// Strip pointer prefix.
	t = strings.TrimPrefix(t, "*")
	// Keep only last segment of a package-qualified name.
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	// Skip generics.
	if i := strings.Index(t, "["); i >= 0 {
		t = t[:i]
	}
	// Skip Go primitives — a method call receiver is never a primitive in
	// code we can link to.
	switch t {
	case "string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64", "complex64", "complex128",
		"bool", "byte", "rune", "error", "any":
		return ""
	}
	if t == "" {
		return ""
	}
	return t
}

// inferTypeFromGoExpr inspects the AST of a short-var RHS and returns
// the inferred type name — supports composite literals (`Foo{...}`),
// pointer composite literals (`&Foo{...}`), qualified composite
// literals (`pkg.Foo{...}`), and the Go constructor convention
// (`NewFoo(...)` → `Foo`). Returns "" when the expression doesn't
// unambiguously name a type.
func inferTypeFromGoExpr(node *sitter.Node, src []byte) string {
	switch node.Type() {
	case "composite_literal":
		return compositeLiteralType(node, src)
	case "unary_expression":
		// `&Foo{...}` — operator is "&" (first child), operand is
		// the composite literal.
		for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
			c := node.NamedChild(i)
			if c != nil && c.Type() == "composite_literal" {
				return compositeLiteralType(c, src)
			}
		}
	case "call_expression":
		// Constructor convention: NewFoo(...) → Foo. Only applies
		// when the called identifier starts with "New".
		fn := node.ChildByFieldName("function")
		if fn == nil {
			return ""
		}
		var callName string
		switch fn.Type() {
		case "identifier":
			callName = fn.Content(src)
		case "selector_expression":
			field := fn.ChildByFieldName("field")
			if field != nil {
				callName = field.Content(src)
			}
		}
		if strings.HasPrefix(callName, "New") && len(callName) > 3 {
			return callName[3:]
		}
	}
	return ""
}

// compositeLiteralType returns the type name of a composite literal,
// handling both `Foo{...}` (type_identifier) and `pkg.Foo{...}`
// (qualified_type) shapes.
func compositeLiteralType(lit *sitter.Node, src []byte) string {
	t := lit.ChildByFieldName("type")
	if t == nil {
		return ""
	}
	switch t.Type() {
	case "type_identifier":
		return t.Content(src)
	case "qualified_type":
		nameNode := t.ChildByFieldName("name")
		if nameNode != nil {
			return nameNode.Content(src)
		}
	case "pointer_type":
		// *Foo{...} — rare but defensible.
		for i, _nc := 0, int(t.NamedChildCount()); i < _nc; i++ {
			c := t.NamedChild(i)
			if c != nil && c.Type() == "type_identifier" {
				return c.Content(src)
			}
		}
	}
	return ""
}

// Chained-receiver / factory-chain return-type resolution (resolveChainType,
// stripCallArgs, findMethodReturnType, findFactoryReturnType) is shared across
// every AST extractor and lives in helpers_chaintype.go.

// scanGoPragmasAboveByte inspects up to 5 source lines immediately before a
// function or method declaration looking for `//go:*` or `//export`
// comments and stamps them onto the node's Meta. Lets callers flag
// special Go entry points (cgo exports, linkname) so dead-code
// detection doesn't mark them as dead.
//
// Allocation contract: scans backwards using bytes.LastIndexByte
// against the original src — no per-call lineStarts slice, no per-
// line string copy until a pragma actually matches. The predecessor
// implementation built an O(startLine) []int of line offsets per
// call, which on a Go monorepo like k8s (12 k+ .go files, hundreds
// of symbols each) was the single biggest allocation in the
// indexer (4.57 GB / 14 % of total).
func scanGoPragmasAboveByte(src []byte, startByte int, node *graph.Node) {
	if startByte <= 0 || len(src) == 0 {
		return
	}
	if startByte > len(src) {
		startByte = len(src)
	}
	end := bytes.LastIndexByte(src[:startByte], '\n')
	for scanned := 0; scanned < 5 && end >= 0; scanned++ {
		prev := bytes.LastIndexByte(src[:end], '\n')
		line := bytes.TrimSpace(src[prev+1 : end])
		if len(line) != 0 && !bytes.HasPrefix(line, []byte("//")) {
			return
		}
		if bytes.HasPrefix(line, []byte("//export ")) {
			node.Meta["cgo_export"] = true
			return
		}
		if bytes.HasPrefix(line, []byte("//go:linkname ")) {
			node.Meta["go_linkname"] = true
			return
		}
		if prev < 0 {
			return
		}
		end = prev
	}
}

// isGoBuiltinOrKeyword returns true for identifiers that should not be
// treated as function-value references (common Go builtins, type names,
// literals).
func isGoBuiltinOrKeyword(name string) bool {
	switch name {
	case "nil", "true", "false", "err", "ok", "ctx",
		"string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64", "complex64", "complex128",
		"bool", "byte", "rune", "error", "any",
		"make", "new", "len", "cap", "append", "copy", "delete",
		"panic", "recover", "print", "println", "close":
		return true
	}
	// Skip lowercase single-letter identifiers (loop vars, etc.)
	if len(name) == 1 && name[0] >= 'a' && name[0] <= 'z' {
		return true
	}
	return false
}

// stampGoGenericInstantiation marks a call edge whose callee spelled
// explicit type arguments. Go's grammar cannot separate that from
// indexing a func-valued map or field, nor from a conversion to a
// generic type, so the extractor emits all three and records which ones
// claimed to be instantiations. The resolver then binds such an edge
// only to a declaration that actually declares type parameters, which
// is the one thing a real instantiation guarantees and the impostors
// never do.
func stampGoGenericInstantiation(edge *graph.Edge, c goDeferredCall) {
	if edge == nil || !c.genericInst {
		return
	}
	if edge.Meta == nil {
		edge.Meta = map[string]any{}
	}
	edge.Meta[graph.MetaGenericInstantiation] = true
}
