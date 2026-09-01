package languages

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitCSharpFunctionShape emits KindParam/EdgeParamOf/EdgeTypedAs/
// EdgeReturns/KindGenericParam edges for a C# method or constructor
// declaration. Parameter shapes (`Foo Bar`, `Foo Bar = default`,
// `params Foo[] xs`, generic `Foo<Bar> baz`) all flow through this
// helper so DI containers and codegen tooling see dependencies.
func emitCSharpFunctionShape(ownerID string, methodNode *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	if methodNode == nil {
		return
	}
	if params := methodNode.ChildByFieldName("parameters"); params != nil {
		emitCSharpParamNodes(ownerID, params, src, filePath, declLine, result)
	}
	if rt := csharpReturnTypeRaw(methodNode, src); rt != "" {
		emitCSharpReturnEdges(ownerID, rt, filePath, declLine, result)
	}
	emitCSharpGenericParamNodes(ownerID, methodNode, src, filePath, declLine, result)
}

func emitCSharpParamNodes(ownerID string, params *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	entries, _ := csharpParamEntries(params, src)
	for _, entry := range entries {
		if entry.name == "" {
			continue
		}
		paramID := ownerID + "#param:" + entry.name + "@" + strconv.Itoa(entry.position)
		meta := map[string]any{"position": entry.position}
		if entry.variadic {
			meta["variadic"] = true
		}
		if entry.typeRaw != "" {
			meta["type"] = entry.typeRaw
		}
		startLine := entry.startLine
		if startLine == 0 {
			startLine = declLine
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        paramID,
			Kind:      graph.KindParam,
			Name:      entry.name,
			FilePath:  filePath,
			StartLine: startLine,
			EndLine:   entry.endLine,
			Language:  "csharp",
			Meta:      meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     paramID,
			To:       ownerID,
			Kind:     graph.EdgeParamOf,
			FilePath: filePath,
			Line:     startLine,
			Origin:   graph.OriginASTResolved,
		})
		if canon := canonicalizeCSharpTypeRef(entry.typeRaw); canon != "" && !isCSharpPrimitive(canon) {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     paramID,
				To:       "unresolved::" + canon,
				Kind:     graph.EdgeTypedAs,
				FilePath: filePath,
				Line:     startLine,
				Origin:   graph.OriginASTInferred,
			})
		}
	}
}

func csharpReturnTypeRaw(methodNode *sitter.Node, src []byte) string {
	// In tree-sitter-c-sharp, method_declaration has a `type` field
	// for the return type. Constructors don't (the type is implicitly
	// the enclosing class).
	if rt := methodNode.ChildByFieldName("type"); rt != nil {
		return strings.TrimSpace(rt.Content(src))
	}
	if rt := methodNode.ChildByFieldName("returns"); rt != nil {
		return strings.TrimSpace(rt.Content(src))
	}
	return ""
}

func emitCSharpReturnEdges(ownerID, returnText, filePath string, line int, result *parser.ExtractionResult) {
	if returnText == "" {
		return
	}
	t := canonicalizeCSharpTypeRef(returnText)
	if t == "" || isCSharpPrimitive(t) {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       "unresolved::" + t,
		Kind:     graph.EdgeReturns,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
		Meta: map[string]any{
			"position": 0,
		},
	})
}

func emitCSharpGenericParamNodes(ownerID string, methodNode *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
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
		return
	}
	for i, _nc := 0, int(tparams.NamedChildCount()); i < _nc; i++ {
		tp := tparams.NamedChild(i)
		if tp == nil || tp.Type() != "type_parameter" {
			continue
		}
		var name string
		for j, _nc := 0, int(tp.NamedChildCount()); j < _nc; j++ {
			c := tp.NamedChild(j)
			if c != nil && c.Type() == "identifier" {
				name = c.Content(src)
				break
			}
		}
		if name == "" {
			continue
		}
		gpID := ownerID + "#tparam:" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        gpID,
			Kind:      graph.KindGenericParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: line,
			EndLine:   line,
			Language:  "csharp",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     gpID,
			To:       ownerID,
			Kind:     graph.EdgeMemberOf,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
		})
	}
}

// csharpContainerGenerics are wrapper types whose single type argument is
// the type a reference is really about; canonicalisation unwraps them.
var csharpContainerGenerics = []string{
	"Task", "ValueTask", "List", "IList", "IEnumerable",
	"ICollection", "IReadOnlyList", "IReadOnlyCollection",
	"IAsyncEnumerable", "Nullable", "Span", "ReadOnlySpan",
}

// csharpQualifiedTypeRef mirrors canonicalizeCSharpTypeRef but keeps the
// dotted qualifier ("Common.Lookup.Foo?" -> "Common.Lookup.Foo").
// Returns "" when the spelling carries no qualifier.
func csharpQualifiedTypeRef(t string) string {
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "?"))
	for strings.HasSuffix(t, "[]") {
		t = strings.TrimSpace(strings.TrimSuffix(t, "[]"))
	}
	for _, wrapper := range csharpContainerGenerics {
		prefix := wrapper + "<"
		if strings.HasPrefix(t, prefix) && strings.HasSuffix(t, ">") {
			return csharpQualifiedTypeRef(t[len(prefix) : len(t)-1])
		}
	}
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	if !strings.Contains(t, ".") {
		return ""
	}
	return t
}

func canonicalizeCSharpTypeRef(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Strip nullable suffix `?`.
	t = strings.TrimSuffix(t, "?")
	t = strings.TrimSpace(t)
	// Strip array suffix.
	for strings.HasSuffix(t, "[]") {
		t = strings.TrimSuffix(t, "[]")
		t = strings.TrimSpace(t)
	}
	// Unwrap container generics.
	for _, wrapper := range csharpContainerGenerics {
		prefix := wrapper + "<"
		if strings.HasPrefix(t, prefix) && strings.HasSuffix(t, ">") {
			inner := t[len(prefix) : len(t)-1]
			return canonicalizeCSharpTypeRef(inner)
		}
	}
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	if idx := strings.LastIndex(t, "."); idx >= 0 {
		t = t[idx+1:]
	}
	// Same canonical identifier domain the DECLARATION side mints its
	// node IDs in: `@event` is the only legal spelling of a
	// keyword-named type, so a reference must reduce to the identifier
	// the declaration registered or it can never bind.
	return csharpCanonBaseIdent(strings.TrimSpace(t))
}

func isCSharpPrimitive(t string) bool {
	switch t {
	case "", "var", "void", "bool", "byte", "sbyte", "short", "ushort",
		"int", "uint", "long", "ulong", "float", "double", "decimal",
		"char", "string", "object", "dynamic":
		return true
	}
	return false
}

// emitCSharpTypeUseEdges emits one EdgeTypedAs from ownerID to the bare
// named type used in a variable / field / property annotation. The type
// is canonicalised to its simple named form (nullable `T?`, arrays
// `T[]`, and container generics like `List<T>` / `Task<T>` are unwrapped
// by canonicalizeCSharpTypeRef) and primitives / `var` are skipped so
// the graph isn't flooded with unresolved::int / unresolved::var edges
// that never land. Edges ride at OriginASTInferred — the binding is a
// tree-sitter inference, not an LSP-checked fact — so a type that a local
// declaration uses (`HttpResponse resp = Get();`) becomes a traversable
// reference even without a language server.
func emitCSharpTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult) {
	if ownerID == "" {
		return
	}
	canon := canonicalizeCSharpTypeRef(typeText)
	if canon == "" || isCSharpPrimitive(canon) {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       "unresolved::" + canon,
		Kind:     graph.EdgeTypedAs,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
	})
}

// emitCSharpAsyncSpawns walks a C# method body for `await` expressions,
// Task.Run / Task.Factory.StartNew / ThreadPool.QueueUserWorkItem
// calls, and deferred-lambda registrations (csharpDeferredLambdaClients,
// mode "background"). Mode is "async" for await, "task" for Task.Run,
// "thread" for thread-pool queues.
func emitCSharpAsyncSpawns(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	seen := map[string]bool{}
	emit := func(target, mode string, line int) {
		if target == "" {
			return
		}
		key := mode + "\x00" + target
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + target,
			Kind:     graph.EdgeSpawns,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"mode": mode,
			},
		})
	}
	// Lambdas are walked (registrations live inside configuration lambdas
	// — services.AddHangfireServer(opts => { RecurringJob.AddOrUpdate… }))
	// but only the deferred-client detection fires there; task/async
	// spawns keep their original named-body-only attribution.
	var walk func(n *sitter.Node, inLambda bool)
	walk = func(n *sitter.Node, inLambda bool) {
		if n == nil {
			return
		}
		if !visitCSharpSpawnNode(n, inLambda, src, ownerID, filePath, seen, result, emit) {
			return
		}
		if n.Type() == "lambda_expression" || n.Type() == "anonymous_method_expression" {
			inLambda = true
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i), inLambda)
		}
	}
	walk(body, false)
}

// visitCSharpSpawnNode handles one node of the spawn walk; reports
// whether to descend into it.
func visitCSharpSpawnNode(n *sitter.Node, inLambda bool, src []byte, ownerID, filePath string, seen map[string]bool, result *parser.ExtractionResult, emit func(string, string, int)) bool {
	switch n.Type() {
	case "method_declaration", "local_function_statement":
		return false
	case "await_expression":
		if inLambda {
			return true
		}
		line := int(n.StartPoint().Row) + 1
		// Look for an inner invocation_expression to grab the
		// callee name.
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c == nil {
				continue
			}
			if c.Type() == "invocation_expression" {
				if name := csharpInvocationTargetName(c, src); name != "" {
					emit(name, "async", line)
				}
			}
		}
	case "invocation_expression":
		fn := n.ChildByFieldName("function")
		if fn == nil {
			return true
		}
		line := int(n.StartPoint().Row) + 1
		if fn.Type() == "member_access_expression" {
			expr := fn.ChildByFieldName("expression")
			name := fn.ChildByFieldName("name")
			if expr != nil && name != nil {
				// Byte-slice map index (no allocation — this path runs
				// for every member call inside every lambda).
				b := src[expr.StartByte():expr.EndByte()]
				if mm := csharpDeferredLambdaClients[string(b[bytes.LastIndexByte(b, '.')+1:])]; mm != nil {
					emitCSharpDeferredLambdaSpawn(n, name, mm, src, ownerID, filePath, line, seen, result)
				}
				if inLambda {
					return true
				}
				obj := expr.Content(src)
				meth := name.Content(src)
				switch obj {
				case "Task":
					switch meth {
					case "Run", "Factory":
						emit("Task."+meth, "task", line)
					}
				case "Task.Factory":
					if meth == "StartNew" {
						emit("Task.Factory.StartNew", "task", line)
					}
				case "ThreadPool":
					if meth == "QueueUserWorkItem" {
						emit("ThreadPool.QueueUserWorkItem", "thread", line)
					}
				case "Parallel":
					switch meth {
					case "ForEach", "For", "Invoke":
						emit("Parallel."+meth, "parallel", line)
					}
				}
			}
		}
	}
	return true
}

// csharpDeferredLambdaClients registers static-client methods whose
// lambda argument a framework executes later, on a receiver typed by
// the generic argument (Hangfire first). Per-API by necessity: whether
// a generic arg types the lambda is a signature fact — List.ConvertAll's
// TOutput, for one, does not.
var csharpDeferredLambdaClients = map[string]map[string]bool{
	"RecurringJob":  {"AddOrUpdate": true},
	"BackgroundJob": {"Enqueue": true, "Schedule": true},
}

// emitCSharpDeferredLambdaSpawn emits the spawn for a registered
// client's lambda: the framework runs it later, and its receiver type
// exists only in the generic argument — carried as the receiver_type
// hint the resolver's *.-target path binds exactly. Origin is left for
// the resolver to stamp at the tier the binding actually earns.
func emitCSharpDeferredLambdaSpawn(inv, nameNode *sitter.Node, methods map[string]bool, src []byte, ownerID, filePath string, line int, seen map[string]bool, result *parser.ExtractionResult) {
	meth := nameNode.Content(src)
	recv := ""
	if nameNode.Type() == "generic_name" {
		meth, recv = csharpGenericNameParts(nameNode, src)
	}
	if !methods[meth] {
		return
	}
	// A type-parameter receiver (Enqueue<T> inside a generic wrapper)
	// names no indexed type; a hint would only steer the resolver's
	// fallback onto an arbitrary same-named method. Emit nothing.
	if recv != "" && csharpRecvIsTypeParam(inv, recv, src) {
		return
	}
	target, memberCall := csharpLambdaInvocationTarget(inv, src)
	if target == "" {
		return
	}
	// A captured-receiver call (() => job.Run()) without a generic hint
	// carries no type evidence either — same fallback hazard, same skip.
	if recv == "" && memberCall {
		return
	}
	to := target
	meta := map[string]any{"mode": "background"}
	if recv != "" {
		to = "*." + target
		meta["receiver_type"] = recv
	}
	key := "background\x00" + recv + "\x00" + to
	if seen[key] {
		return
	}
	seen[key] = true
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       "unresolved::" + to,
		Kind:     graph.EdgeSpawns,
		FilePath: filePath,
		Line:     line,
		Meta:     meta,
	})
}

// csharpRecvIsTypeParam reports whether recv names a type parameter of
// an enclosing method, local function, or type declaration — an AST
// fact, not a naming-convention guess (TVShow is a class, TEntity in a
// generic wrapper is not).
func csharpRecvIsTypeParam(n *sitter.Node, recv string, src []byte) bool {
	for p := n; p != nil; p = p.Parent() {
		switch p.Type() {
		case "method_declaration", "local_function_statement", "class_declaration",
			"struct_declaration", "record_declaration", "interface_declaration":
		default:
			continue
		}
		tparams := p.ChildByFieldName("type_parameters")
		if tparams == nil {
			for i, _nc := 0, int(p.NamedChildCount()); i < _nc; i++ {
				if c := p.NamedChild(i); c != nil && c.Type() == "type_parameter_list" {
					tparams = c
					break
				}
			}
		}
		if tparams == nil {
			continue
		}
		for i, _nc := 0, int(tparams.NamedChildCount()); i < _nc; i++ {
			tp := tparams.NamedChild(i)
			if tp == nil || tp.Type() != "type_parameter" {
				continue
			}
			for j, _jc := 0, int(tp.NamedChildCount()); j < _jc; j++ {
				if c := tp.NamedChild(j); c != nil && c.Type() == "identifier" && c.Content(src) == recv {
					return true
				}
			}
		}
	}
	return false
}

// csharpGenericNameParts splits `Enqueue<SyncService>` into the bare
// method name and the first type argument, shortened to the simple type
// name the graph indexes.
func csharpGenericNameParts(n *sitter.Node, src []byte) (name, firstTypeArg string) {
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier":
			name = c.Content(src)
		case "type_argument_list":
			if c.NamedChildCount() > 0 && c.NamedChild(0) != nil {
				arg := c.NamedChild(0).Content(src)
				if i := strings.IndexByte(arg, '<'); i >= 0 {
					arg = arg[:i]
				}
				firstTypeArg = diShortName(arg)
			}
		}
	}
	return name, firstTypeArg
}

// csharpLambdaInvocationTarget returns the callee name of the first
// invocation inside the FIRST lambda argument — the method the deferred
// lambda runs — and whether that callee is member-qualified. Later
// lambdas (a cron callback, say) never substitute for an invocationless
// first one.
func csharpLambdaInvocationTarget(inv *sitter.Node, src []byte) (target string, memberCall bool) {
	args := inv.ChildByFieldName("arguments")
	if args == nil {
		return "", false
	}
	var lambda *sitter.Node
	walkCSharpNodes(args, func(n *sitter.Node) bool {
		if lambda != nil {
			return false
		}
		if n.Type() == "lambda_expression" {
			lambda = n
			return false
		}
		return true
	})
	if lambda == nil {
		return "", false
	}
	walkCSharpNodes(lambda, func(b *sitter.Node) bool {
		if target != "" {
			return false
		}
		if b.Type() == "invocation_expression" {
			target = csharpInvocationTargetName(b, src)
			if fn := b.ChildByFieldName("function"); fn != nil && fn.Type() == "member_access_expression" {
				memberCall = true
			}
			return false
		}
		return true
	})
	return target, memberCall
}

func walkCSharpNodes(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		walkCSharpNodes(n.NamedChild(i), visit)
	}
}

func csharpInvocationTargetName(call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "member_access_expression":
		if name := fn.ChildByFieldName("name"); name != nil {
			return name.Content(src)
		}
	case "generic_name":
		if name := fn.ChildByFieldName("name"); name != nil {
			return name.Content(src)
		}
	}
	return ""
}

// csharpFunctionBody returns the body block of a C# method
// declaration. Expression-bodied methods (`=> expr`) have a different
// shape that we don't walk (no spawn-like calls in idiomatic
// expression bodies).
func csharpFunctionBody(methodNode *sitter.Node) *sitter.Node {
	if methodNode == nil {
		return nil
	}
	if b := methodNode.ChildByFieldName("body"); b != nil {
		return b
	}
	for i, _nc := 0, int(methodNode.NamedChildCount()); i < _nc; i++ {
		c := methodNode.NamedChild(i)
		if c != nil && c.Type() == "block" {
			return c
		}
	}
	return nil
}
