package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/rust"
)

// qRustAll is a single tree-sitter query alternating over every pattern
// the Rust extractor needs. One tree walk per file replaces the 14
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set. Impl/trait membership is
// resolved via a parent walk on the captured node rather than nested
// queries — same behaviour, one cursor pass.
const qRustAll = `
[
  (function_item
    name: (identifier) @func.name) @func.def

  (function_signature_item
    name: (identifier) @sig.name) @sig.def

  (struct_item
    name: (type_identifier) @struct.name) @struct.def

  (enum_item
    name: (type_identifier) @enum.name) @enum.def

  (trait_item
    name: (type_identifier) @trait.name) @trait.def

  (enum_variant
    name: (identifier) @variant.name) @variant.def

  (field_declaration
    name: (field_identifier) @field.name) @field.def

  (const_item
    name: (identifier) @const.name) @const.def

  (static_item
    name: (identifier) @static.name) @static.def

  (use_declaration
    argument: (_) @use.path) @use.def

  (mod_item
    name: (identifier) @mod.name) @mod.def

  (type_item
    name: (type_identifier) @alias.name) @alias.def

  (union_item
    name: (type_identifier) @union.name) @union.def

  (macro_definition
    name: (identifier) @macro.name) @macro.def

  (extern_crate_declaration) @xcrate.def

  (associated_type) @assoc.def

  (ordered_field_declaration_list) @tuplefields.def

  (impl_item) @impl.def

  (macro_invocation) @minvoke.expr

  (let_declaration
    pattern: (identifier) @lvar.name
    type: (_)? @lvar.type
    value: (_)? @lvar.value) @lvar.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (scoped_identifier
      name: (identifier) @callp.name)) @callp.expr

  (call_expression
    function: (field_expression
      value: (_) @callm.receiver
      field: (field_identifier) @callm.method)) @callm.expr

  ; Turbofish call sites. A trailing ::<T> wraps the callee in a
  ; generic_function node, so the three patterns above never match
  ; xs.collect::<Vec<_>>() or parse::<u32>(s) — 5,301 such sites went
  ; unrecorded across a 4,000-file sample of crate source.
  (call_expression
    function: (generic_function
      function: (identifier) @callg.name)) @callg.expr

  (call_expression
    function: (generic_function
      function: (scoped_identifier
        name: (identifier) @callgp.name))) @callgp.expr

  (call_expression
    function: (generic_function
      function: (field_expression
        value: (_) @callgm.receiver
        field: (field_identifier) @callgm.method))) @callgm.expr
]
`

// RustExtractor extracts Rust source files into graph nodes and edges.
type RustExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewRustExtractor() *RustExtractor {
	lang := rust.GetLanguage()
	return &RustExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qRustAll, lang),
	}
}

func (e *RustExtractor) Language() string     { return "rust" }
func (e *RustExtractor) Extensions() []string { return []string{".rs"} }

// --- Deferred match buffers ----------------------------------------

type rustDeferredCall struct {
	name       string
	receiver   string // selector receiver text (empty for plain/path calls)
	path       string // full scoped_identifier text for path calls (e.g. "Foo::new", "crate::util::helper"); "" otherwise
	line       int
	isSelector bool
	// returnUsage is how the call site consumes the return value
	// (graph.ReturnUsage* label), classified at capture time and
	// stamped as edge Meta on the EdgeCalls emitted for this site.
	returnUsage string
	// viaMacro names the macro whose body the site was recovered from
	// ("" for an ordinary call_expression). Recovered sites are token
	// matches rather than parsed expressions, so consumers that want
	// only grammar-certain edges can filter on it.
	viaMacro string
	// isMacro marks the site as a macro invocation rather than a
	// function call, so it can bind to a `macro_rules!` node.
	isMacro bool
}

// rustDeferredLet buffers a let_declaration for the post-pass type-env
// build. Tier 0 (explicit type) is processed across all lets first;
// Tier 1 (`new()` / struct expression on the RHS) only fills in keys
// the explicit pass did not — same precedence the legacy two-query
// version produced.
type rustDeferredLet struct {
	name     string
	explicit string       // normalized type from explicit annotation, "" if none
	typeRaw  string       // verbatim explicit-annotation source (pre-canonicalization), "" if none
	typeLine int          // 1-based line of the explicit annotation (0 if none)
	value    *sitter.Node // RHS expression node, or nil
}

// rustFuncDef buffers a function/method definition node for the post-emit
// per-function receiver-scope build (paramsByFunc).
type rustFuncDef struct {
	node  *sitter.Node
	owner string // impl/trait type that self binds to; "" for free functions
	line  int    // 1-based start line, mapped back to the emitted id via funcRanges
}

func (e *RustExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "rust",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)
	traitMethods := make(map[string][]string) // trait name → declared method names
	containers := make(rustContainerIDs)      // container start row → minted node id

	var calls []rustDeferredCall
	var lets []rustDeferredLet
	var funcDefs []rustFuncDef
	var implBlockEdges []*graph.Edge // edges sourced at an impl block's target type

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen, annotationSeen)
			fd := m.Captures["func.def"]
			owner := rustImplMethodReceiver(fd.Node, src)
			if owner == "" {
				owner = rustTraitMethodOwner(fd.Node, src)
			}
			funcDefs = append(funcDefs, rustFuncDef{node: fd.Node, owner: owner, line: fd.StartLine + 1})

		case m.Captures["sig.def"] != nil:
			e.recordTraitMethod(m, filePath, fileID, src, result, traitMethods, seen, annotationSeen)
			sd := m.Captures["sig.def"]
			funcDefs = append(funcDefs, rustFuncDef{node: sd.Node, owner: rustTraitMethodOwner(sd.Node, src), line: sd.StartLine + 1})

		case m.Captures["struct.def"] != nil:
			e.emitStruct(m, filePath, fileID, src, result, seen, annotationSeen, containers)

		case m.Captures["enum.def"] != nil:
			e.emitEnum(m, filePath, fileID, src, result, seen, annotationSeen, containers)

		case m.Captures["trait.def"] != nil:
			e.emitTrait(m, filePath, fileID, src, result, seen, annotationSeen, traitMethods, containers)

		case m.Captures["variant.def"] != nil:
			e.emitVariant(m, filePath, src, result, containers)

		case m.Captures["field.def"] != nil:
			e.emitField(m, filePath, src, result, containers)

		case m.Captures["const.def"] != nil:
			e.emitNamed(m, "const", filePath, fileID, src, result, seen, annotationSeen, containers)

		case m.Captures["static.def"] != nil:
			e.emitNamed(m, "static", filePath, fileID, src, result, seen, annotationSeen, containers)

		case m.Captures["use.def"] != nil:
			e.emitUse(m, filePath, fileID, src, result)

		case m.Captures["mod.def"] != nil:
			emitRustModule(m.Captures["mod.def"].Node, m.Captures["mod.name"].Text,
				filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["alias.def"] != nil:
			emitRustTypeAlias(m.Captures["alias.def"].Node, m.Captures["alias.name"].Text,
				filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["union.def"] != nil:
			emitRustUnion(m.Captures["union.def"].Node, m.Captures["union.name"].Text,
				filePath, fileID, src, result, seen, annotationSeen, containers)

		case m.Captures["macro.def"] != nil:
			emitRustMacroDef(m.Captures["macro.def"].Node, m.Captures["macro.name"].Text,
				filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["xcrate.def"] != nil:
			emitRustExternCrate(m.Captures["xcrate.def"].Node, filePath, fileID, src, result)

		case m.Captures["impl.def"] != nil:
			before := len(result.Edges)
			emitRustImplBlock(m.Captures["impl.def"].Node, filePath, src, result, annotationSeen)
			implBlockEdges = append(implBlockEdges, result.Edges[before:]...)

		case m.Captures["tuplefields.def"] != nil:
			emitRustTupleFields(m.Captures["tuplefields.def"].Node, filePath, src, result, seen, containers)

		case m.Captures["assoc.def"] != nil:
			emitRustAssociatedType(m.Captures["assoc.def"].Node, filePath, src, result, seen)

		case m.Captures["minvoke.expr"] != nil:
			inv := m.Captures["minvoke.expr"]
			// The invocation itself is a use of the macro, so a
			// `macro_rules!` definition gets incoming edges.
			if mn := inv.Node.ChildByFieldName("macro"); mn != nil {
				full := mn.Content(src)
				c := rustDeferredCall{
					name:    rustLastPathSegment(full),
					line:    inv.StartLine + 1,
					isMacro: true,
				}
				if strings.Contains(full, "::") {
					c.path = full
				}
				calls = append(calls, c)
			}
			for _, mc := range scanRustMacroCalls(inv.Node, src) {
				calls = append(calls, rustDeferredCall{
					name:       mc.name,
					receiver:   mc.receiver,
					path:       mc.path,
					line:       mc.line,
					isSelector: mc.isSelector,
					viaMacro:   mc.macroName,
					isMacro:    mc.isMacro,
				})
			}

		case m.Captures["lvar.def"] != nil:
			d := rustDeferredLet{
				name: m.Captures["lvar.name"].Text,
			}
			if t, ok := m.Captures["lvar.type"]; ok {
				d.explicit = normalizeRustTypeName(t.Text)
				// Keep the verbatim annotation source + its line so the
				// post-pass can emit a cross-file EdgeTypedAs from the
				// enclosing function to the named type, mirroring the
				// param/return function-shape emission. normalizeRustTypeName
				// is lossy (drops wrappers the canonicalizer keeps), so we
				// re-canonicalize from the raw text downstream.
				d.typeRaw = t.Text
				d.typeLine = t.StartLine + 1
			}
			if v, ok := m.Captures["lvar.value"]; ok && v.Node != nil {
				d.value = v.Node
			}
			lets = append(lets, d)

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, rustDeferredCall{
				name:        m.Captures["callm.method"].Text,
				receiver:    m.Captures["callm.receiver"].Text,
				line:        expr.StartLine + 1,
				isSelector:  true,
				returnUsage: classifyReturnUsage(expr.Node, src, rustReturnUsageSpec),
			})

		case m.Captures["callp.expr"] != nil:
			expr := m.Captures["callp.expr"]
			c := rustDeferredCall{
				name:        m.Captures["callp.name"].Text,
				line:        expr.StartLine + 1,
				returnUsage: classifyReturnUsage(expr.Node, src, rustReturnUsageSpec),
			}
			// The query only captures the scoped_identifier's trailing
			// segment, so the qualifier (Foo / Self / crate / super /
			// module path) would otherwise be lost. Read the full
			// `function` child text so the Rust scope resolver can bind
			// `Type::method`, `Self::method`, and `crate::/super::/self::`
			// module paths.
			if expr.Node != nil {
				if fn := expr.Node.ChildByFieldName("function"); fn != nil {
					c.path = fn.Content(src)
				}
			}
			calls = append(calls, c)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, rustDeferredCall{
				name:        m.Captures["call.name"].Text,
				line:        expr.StartLine + 1,
				returnUsage: classifyReturnUsage(expr.Node, src, rustReturnUsageSpec),
			})

		case m.Captures["callgm.expr"] != nil:
			expr := m.Captures["callgm.expr"]
			calls = append(calls, rustDeferredCall{
				name:        m.Captures["callgm.method"].Text,
				receiver:    m.Captures["callgm.receiver"].Text,
				line:        expr.StartLine + 1,
				isSelector:  true,
				returnUsage: classifyReturnUsage(expr.Node, src, rustReturnUsageSpec),
			})

		case m.Captures["callgp.expr"] != nil:
			expr := m.Captures["callgp.expr"]
			c := rustDeferredCall{
				name:        m.Captures["callgp.name"].Text,
				line:        expr.StartLine + 1,
				returnUsage: classifyReturnUsage(expr.Node, src, rustReturnUsageSpec),
			}
			if fn := expr.Node.ChildByFieldName("function"); fn != nil {
				if inner := fn.ChildByFieldName("function"); inner != nil {
					c.path = inner.Content(src)
				}
			}
			calls = append(calls, c)

		case m.Captures["callg.expr"] != nil:
			expr := m.Captures["callg.expr"]
			calls = append(calls, rustDeferredCall{
				name:        m.Captures["callg.name"].Text,
				line:        expr.StartLine + 1,
				returnUsage: classifyReturnUsage(expr.Node, src, rustReturnUsageSpec),
			})
		}
	})
	rebindRustLocalGenericMemberOwners(filePath, result)

	// Stamp trait method names onto trait nodes' Meta["methods"]. Some
	// trait nodes were emitted before their function_signature_item
	// children fired through the alternation, so we backfill at the end.
	for _, n := range result.Nodes {
		if n.Kind != graph.KindInterface {
			continue
		}
		if methods, ok := traitMethods[n.Name]; ok {
			if n.Meta == nil {
				n.Meta = make(map[string]any)
			}
			n.Meta["methods"] = methods
		}
	}

	// Build type environment in the legacy precedence:
	//   Tier 0 — explicit `let x: Type = ...` annotations (overwrite)
	//   Tier 1 — RHS inference (`StructExpr {...}`, `Type::new(...)`),
	//            only when Tier 0 didn't supply a type.
	tenv := make(typeEnv)
	for _, l := range lets {
		if l.explicit != "" {
			tenv[l.name] = l.explicit
		}
	}
	for _, l := range lets {
		if _, exists := tenv[l.name]; exists {
			continue
		}
		if l.value == nil {
			continue
		}
		if inferred := inferTypeFromRustExpr(l.value, src); inferred != "" {
			tenv[l.name] = inferred
		}
	}

	// All function/method nodes have been emitted; map call sites to
	// their enclosing definition.
	funcRanges := buildFuncRanges(result)

	// Per-function receiver scope: map each function/method's parameters and
	// self receiver to their types so a selector call on a parameter receiver
	// (args.foo()) resolves — mirroring Go's paramsByFunc. Keyed by the
	// enclosing function id via funcRanges.
	paramsByFunc := make(map[string]map[string]string)
	for _, fd := range funcDefs {
		id := findEnclosingFunc(funcRanges, fd.line)
		if id == "" {
			continue
		}
		if pm := rustFuncParamTypes(fd.node, fd.owner, src); len(pm) > 0 {
			paramsByFunc[id] = pm
		}
	}

	// Emit a cross-file type-usage edge for every `let x: Type = ...`
	// binding annotation. Without this a type referenced only in a local
	// binding (never in a param/return) is invisible to find_usages
	// absent a language server. Attributed to the enclosing function
	// (file node fallback), canonicalized to the bare named type, with
	// primitives skipped — same shape as the param/return emission.
	for _, l := range lets {
		if l.typeRaw == "" {
			continue
		}
		ownerID := findEnclosingFunc(funcRanges, l.typeLine)
		if ownerID == "" {
			ownerID = fileID
		}
		emitRustTypeUseEdges(ownerID, l.typeRaw, filePath, l.typeLine, result)
	}

	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			// A call outside any function body — e.g. inside a const/static
			// initialiser (`const ALIASES = [alias(...), alias(...)]`).
			// Attribute it to the file node instead of dropping the site, so
			// find_usages still surfaces the call. Mirrors the let-binding
			// type-use pass's file-node fallback.
			callerID = fileID
		}
		if c.isSelector {
			edge := &graph.Edge{
				From: callerID, To: "unresolved::*." + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			if recvType, ok := lookupRustRecvType(paramsByFunc, tenv, callerID, c.receiver); ok {
				edge.Meta = map[string]any{"receiver_type": recvType}
			} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
				stampFactoryChainReceiver(edge, c.receiver, resolveChainType(c.receiver, tenv, result))
			}
			// Record a self/Self receiver so the Rust scope resolver can
			// bind a `self.method()` selector to the enclosing impl type
			// without an explicit `receiver_type` hint. Only the
			// self/Self receivers are stamped — other receivers carry no
			// extra meta (their binding flows through receiver_type).
			if c.receiver == "self" || c.receiver == "Self" {
				if edge.Meta == nil {
					edge.Meta = map[string]any{}
				}
				edge.Meta["rust_recv"] = c.receiver
			} else if _, hasType := edge.Meta["receiver_type"]; !hasType &&
				strings.HasPrefix(c.receiver, "self.") && !strings.ContainsAny(c.receiver, "()[]") {
				// A field-access receiver rooted at self (self.config.line_term
				// .method()). Its type can't be read from a single param/local,
				// but the scope resolver can walk the struct field types from the
				// enclosing impl type. Record the chain so it can.
				if edge.Meta == nil {
					edge.Meta = map[string]any{}
				}
				edge.Meta["rust_recv_expr"] = c.receiver
			}
			stampReturnUsage(edge, c.returnUsage)
			stampRustMacroOrigin(edge, c)
			result.Edges = append(result.Edges, edge)
			continue
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::" + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		// Preserve the full scoped path (e.g. "Foo::new",
		// "crate::util::helper") for the Rust scope resolver — the
		// extractor's call target keeps only the trailing segment.
		if c.path != "" && strings.Contains(c.path, "::") {
			edge.Meta = map[string]any{"rust_path": c.path}
		}
		stampReturnUsage(edge, c.returnUsage)
		stampRustMacroOrigin(edge, c)
		result.Edges = append(result.Edges, edge)
	}

	// Cross-language interop sentinel: when any function in this
	// file carries the `#[wasm_bindgen]` attribute, stamp the file
	// node so audit / porting queries can filter by it. The check
	// runs as a post-pass against the already-emitted annotation
	// edges, avoiding the need to thread fileNode through every
	// emit helper that calls emitRustAnnotationEdges.
	const wasmAnnotationID = "annotation::rust::wasm_bindgen"
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeAnnotated && e.To == wasmAnnotationID {
			if fileNode.Meta == nil {
				fileNode.Meta = map[string]any{}
			}
			fileNode.Meta["uses_wasm_bindgen"] = true
			break
		}
	}
	// Runs after the sentinel so `#[wasm_bindgen] impl Foreign` still
	// stamps the file even though its annotation edge is dropped here.
	dropRustImplEdgesWithoutSource(result, implBlockEdges)

	// Emit the reference-form edges (construction, trait impls / bounds,
	// casts, path / static access, derive attributes) the declaration-only
	// type-use pass above does not cover. Owner attribution reuses the same
	// funcRanges line map (file-node fallback).
	emitRustReferenceForms(root, funcRanges, fileID, filePath, src, result)

	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)
	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

// emitFunction handles every function_item in the file, classifying it
// by its enclosing container:
//   - direct child of an impl_item's body → KindMethod with receiver
//   - direct child of a trait_item's body → KindFunction (default impl,
//     legacy parity — the old extractor's class-method query did not
//     match trait bodies, so these landed in the free-function pass)
//   - anything else → free function
func (e *RustExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine1 := def.StartLine + 1

	doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash)
	visibility := rustVisibility(def.Node, src)
	typeParams := rustEffectiveTypeParams(def.Node, src)
	complexity, cognitive, loopDepth := 0, 0, 0
	if def.Node != nil {
		if body := def.Node.ChildByFieldName("body"); body != nil {
			complexity, cognitive, loopDepth = BodyComplexityMetrics(body, "rust")
		}
	}

	implType := rustImplMethodReceiver(def.Node, src)
	if implType != "" {
		id, ok := disambiguateID(seen, filePath+"::"+implType+"."+name, startLine1)
		if !ok {
			return
		}
		meta := map[string]any{
			"receiver":  implType,
			"signature": buildRustSignature(def.Node, name, src),
			// A trait impl's method is callable by anyone who can name
			// the trait, so an absent `pub` does not make it private.
			"visibility": rustTraitImplVisibility(def.Node, src),
		}
		applyRustFnQualifiers(meta, def.Node, src)
		applyRustScopeMod(meta, def.Node, src)
		if doc != "" {
			meta["doc"] = doc
		}
		if rt := extractRustReturnType(def.Node, src); rt != "" {
			meta["return_type"] = rt
		}
		if len(typeParams) > 0 {
			meta["type_params"] = typeParams
		}
		ApplyComplexityMeta(meta, complexity, cognitive, loopDepth)
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "rust", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		typeID := filePath + "::" + implType
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})
		// `impl Trait for Type` methods override the trait declaration's
		// method. Emit a method-level EdgeOverrides so find_implementations
		// pairs them and find_usages can fan dispatch sites out through the
		// trait — resolved (possibly cross-file) by the Rust scope pass. This
		// covers impls on external types (io::Error) that inferOverrides
		// cannot, since inferOverrides needs both endpoints to be local type
		// nodes and an external type has none.
		if traitBase := rustImplTraitName(def.Node, src); traitBase != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From: id, To: "unresolved::" + traitBase + "." + name,
				Kind: graph.EdgeOverrides, FilePath: filePath, Line: startLine1,
			})
		}
		emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
		emitRustThrowsEdges(def.Node, src, id, filePath, startLine1, result)
		emitRustFunctionShape(id, def.Node, src, filePath, startLine1, result)
		return
	}

	// A fn directly inside a trait body is a default-method declaration:
	// emit it as <file>::<Trait>.<method> (KindMethod) like the impl case
	// rather than a bare free function, so trait-dispatch resolution and
	// find_implementations pairing see a consistent method node.
	if traitName := rustTraitMethodOwner(def.Node, src); traitName != "" {
		e.emitTraitMethodNode(traitName, name, def, filePath, fileID, src, result, seen, annotationSeen)
		return
	}

	// Free function. Mirror the legacy rsQFunction pass — emit as KindFunction.
	id, ok := disambiguateID(seen, filePath+"::"+name, startLine1)
	if !ok {
		return
	}
	meta := map[string]any{
		"signature":  buildRustSignature(def.Node, name, src),
		"visibility": visibility,
	}
	applyRustFnQualifiers(meta, def.Node, src)
	applyRustScopeMod(meta, def.Node, src)
	if doc != "" {
		meta["doc"] = doc
	}
	// Parity with the impl-method and trait-method paths, which have
	// always stamped return_type — a free function is just as worth
	// filtering by what it returns.
	if rt := extractRustReturnType(def.Node, src); rt != "" {
		meta["return_type"] = rt
	}
	if len(typeParams) > 0 {
		meta["type_params"] = typeParams
	}
	ApplyComplexityMeta(meta, complexity, cognitive, loopDepth)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "rust", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustThrowsEdges(def.Node, src, id, filePath, startLine1, result)
	emitRustFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

// rebindRustLocalGenericMemberOwners repairs generic inherent/trait impl
// ownership only when the extractor can prove the bare declaration exists in
// this file. Method IDs and receiver metadata retain the instantiated spelling
// (`Replacer<M>.replace_all`), while member_of targets the declaration node
// (`Replacer`). Qualified targets such as `std::vec::Vec<T>` and cross-module
// impls remain unresolved: stripping their qualifier could falsely attach them
// to an unrelated same-file type with the same basename.
func rebindRustLocalGenericMemberOwners(filePath string, result *parser.ExtractionResult) {
	if result == nil {
		return
	}
	prefix := filePath + "::"
	localTypes := make(map[string]struct{})
	methods := make(map[string]struct{})
	for _, node := range result.Nodes {
		if node == nil {
			continue
		}
		if node.Kind == graph.KindMethod {
			methods[node.ID] = struct{}{}
		}
		if node.FilePath == filePath && (node.Kind == graph.KindType || node.Kind == graph.KindInterface) {
			localTypes[node.ID] = struct{}{}
		}
	}
	for _, edge := range result.Edges {
		if edge == nil || edge.Kind != graph.EdgeMemberOf {
			continue
		}
		if _, method := methods[edge.From]; !method || !strings.HasPrefix(edge.To, prefix) {
			continue
		}
		owner := strings.TrimPrefix(edge.To, prefix)
		if !strings.Contains(owner, "<") || strings.Contains(owner, "::") {
			continue
		}
		base := rustTraitPathBaseName(owner)
		if base == "" {
			continue
		}
		target := prefix + base
		if _, local := localTypes[target]; local {
			edge.To = target
		}
	}
}

// rustTypeParams reads the `type_parameters` child of a Rust item
// (function_item, struct_item, enum_item, trait_item, impl_item) and
// returns each declared type parameter as {name, bound} where bound
// is optional. Multi-trait bounds (`T: Send + Sync`) are joined with
// " + ".
func rustTypeParams(item *sitter.Node, src []byte) []map[string]string {
	if item == nil {
		return nil
	}
	tps := item.ChildByFieldName("type_parameters")
	if tps == nil {
		// Some grammar versions don't expose the field name; fall
		// back to a child-type scan.
		for i, _nc := 0, int(item.ChildCount()); i < _nc; i++ {
			c := item.Child(i)
			if c != nil && c.Type() == "type_parameters" {
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
		tp := tps.NamedChild(i)
		if tp == nil {
			continue
		}
		// Lifetimes (`'a`) have their own node type; skip.
		if tp.Type() == "lifetime" {
			continue
		}
		// Plain identifier — `<T>`.
		if tp.Type() == "type_identifier" {
			out = append(out, map[string]string{"name": tp.Content(src)})
			continue
		}
		// Constrained — `<T: Clone>` or `<T = u8>`. Older grammar
		// versions emit these as `constrained_type_parameter`;
		// newer ones flatten them under a `type_parameter` node.
		entry := map[string]string{}
		for j, _nc := 0, int(tp.ChildCount()); j < _nc; j++ {
			c := tp.Child(j)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "type_identifier":
				if entry["name"] == "" {
					entry["name"] = c.Content(src)
				}
			case "trait_bounds":
				txt := strings.TrimSpace(c.Content(src))
				txt = strings.TrimPrefix(txt, ":")
				entry["bound"] = strings.TrimSpace(txt)
			}
		}
		// As a final fallback for grammars that store the constraint
		// as raw text alongside the identifier, try to parse the full
		// child content if we got a name but no bound.
		if entry["name"] != "" && entry["bound"] == "" {
			text := strings.TrimSpace(tp.Content(src))
			if i := strings.Index(text, ":"); i >= 0 {
				entry["bound"] = strings.TrimSpace(text[i+1:])
			}
		}
		if entry["name"] != "" {
			out = append(out, entry)
		}
	}
	return out
}

// rustEffectiveTypeParams returns the generic parameters visible inside a
// function. Methods inherit parameters declared on their enclosing impl block,
// and both impl/function where clauses refine those parameters. Keeping the
// effective bounds on the emitted function node lets the scope resolver bind a
// call such as `matcher.is_match()` when `matcher: M` and `M: Matcher`.
func rustEffectiveTypeParams(fn *sitter.Node, src []byte) []map[string]string {
	var out []map[string]string
	if impl := rustEnclosingImplItem(fn); impl != nil {
		out = rustMergeTypeParams(out, rustTypeParams(impl, src))
		out = rustApplyWhereBounds(out, rustWhereBounds(impl, src))
	}
	out = rustMergeTypeParams(out, rustTypeParams(fn, src))
	return rustApplyWhereBounds(out, rustWhereBounds(fn, src))
}

func rustEnclosingImplItem(fn *sitter.Node) *sitter.Node {
	if fn == nil {
		return nil
	}
	parent := fn.Parent()
	if parent == nil || parent.Type() != "declaration_list" {
		return nil
	}
	impl := parent.Parent()
	if impl == nil || impl.Type() != "impl_item" {
		return nil
	}
	return impl
}

func rustMergeTypeParams(dst, src []map[string]string) []map[string]string {
	positions := make(map[string]int, len(dst)+len(src))
	out := make([]map[string]string, 0, len(dst)+len(src))
	for _, entry := range dst {
		if name := entry["name"]; name != "" {
			positions[name] = len(out)
			out = append(out, rustCloneTypeParam(entry))
		}
	}
	for _, entry := range src {
		name := entry["name"]
		if name == "" {
			continue
		}
		copy := rustCloneTypeParam(entry)
		if pos, ok := positions[name]; ok {
			out[pos] = copy
			continue
		}
		positions[name] = len(out)
		out = append(out, copy)
	}
	return out
}

func rustCloneTypeParam(entry map[string]string) map[string]string {
	copy := make(map[string]string, len(entry))
	for key, value := range entry {
		copy[key] = value
	}
	return copy
}

func rustApplyWhereBounds(params []map[string]string, bounds map[string]string) []map[string]string {
	if len(params) == 0 || len(bounds) == 0 {
		return params
	}
	for _, entry := range params {
		bound := bounds[entry["name"]]
		if bound == "" {
			continue
		}
		if existing := strings.TrimSpace(entry["bound"]); existing != "" {
			entry["bound"] = existing + " + " + bound
		} else {
			entry["bound"] = bound
		}
	}
	return params
}

// rustWhereBounds extracts the common `where M: Matcher + Send` shape. It is
// intentionally conservative: only predicates whose left side is a simple
// generic identifier are retained; lifetime and associated-type predicates are
// ignored rather than guessed.
func rustWhereBounds(item *sitter.Node, src []byte) map[string]string {
	if item == nil {
		return nil
	}
	var clause *sitter.Node
	for i, n := 0, int(item.ChildCount()); i < n; i++ {
		child := item.Child(i)
		if child != nil && child.Type() == "where_clause" {
			clause = child
			break
		}
	}
	if clause == nil {
		return nil
	}
	text := strings.TrimSpace(clause.Content(src))
	text = strings.TrimSpace(strings.TrimPrefix(text, "where"))
	out := map[string]string{}
	for _, predicate := range rustSplitTopLevel(text, ',') {
		left, right, ok := rustSplitWherePredicate(predicate)
		if !ok || !rustSimpleIdentifier(left) {
			continue
		}
		if prior := out[left]; prior != "" {
			out[left] = prior + " + " + right
		} else {
			out[left] = right
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func rustSplitWherePredicate(predicate string) (string, string, bool) {
	predicate = strings.TrimSpace(predicate)
	angle, paren, bracket := 0, 0, 0
	for i := 0; i < len(predicate); i++ {
		switch predicate[i] {
		case '<':
			angle++
		case '>':
			if angle > 0 {
				angle--
			}
		case '(':
			paren++
		case ')':
			if paren > 0 {
				paren--
			}
		case '[':
			bracket++
		case ']':
			if bracket > 0 {
				bracket--
			}
		case ':':
			if angle != 0 || paren != 0 || bracket != 0 ||
				(i > 0 && predicate[i-1] == ':') ||
				(i+1 < len(predicate) && predicate[i+1] == ':') {
				continue
			}
			left := strings.TrimSpace(predicate[:i])
			right := strings.TrimSpace(predicate[i+1:])
			return left, right, left != "" && right != ""
		}
	}
	return "", "", false
}

func rustSplitTopLevel(text string, separator byte) []string {
	var out []string
	start := 0
	angle, paren, bracket := 0, 0, 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '<':
			angle++
		case '>':
			if angle > 0 {
				angle--
			}
		case '(':
			paren++
		case ')':
			if paren > 0 {
				paren--
			}
		case '[':
			bracket++
		case ']':
			if bracket > 0 {
				bracket--
			}
		default:
			if text[i] == separator && angle == 0 && paren == 0 && bracket == 0 {
				out = append(out, strings.TrimSpace(text[start:i]))
				start = i + 1
			}
		}
	}
	if tail := strings.TrimSpace(text[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}

func rustSimpleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// emitRustThrowsEdges inspects a function's return type for a Result
// wrapper and emits an EdgeThrows to the error type when found. Idiom:
//
//	fn parse(s: &str) -> Result<i32, ParseError> {…}     → throws ParseError
//	fn open(p: &Path) -> Result<File, std::io::Error> {…} → throws Error
//	fn no_error() -> i32 {…}                              → no edge
//
// `Origin: ASTInferred` because we're pattern-matching the return
// type text, not type-checking it.
func emitRustThrowsEdges(funcNode *sitter.Node, src []byte, fromID, filePath string, line int, result *parser.ExtractionResult) {
	if funcNode == nil {
		return
	}
	rt := rustRawReturnType(funcNode, src)
	if rt == "" {
		return
	}
	errType := rustErrorTypeFromResult(rt)
	if errType == "" {
		return
	}
	target := "unresolved::" + errType
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fromID,
		To:       target,
		Kind:     graph.EdgeThrows,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
	})
}

// rustRawReturnType returns the verbatim return-type text of a Rust
// function_item, including generic parameters. Unlike
// extractRustReturnType, it does not normalize the type — preserves
// Result<T, E> shape so the throws extractor can read both arguments.
func rustRawReturnType(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	pastArrow := false
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		text := string(src[child.StartByte():child.EndByte()])
		if text == "->" {
			pastArrow = true
			continue
		}
		if pastArrow {
			if child.Type() == "block" {
				return ""
			}
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// rustErrorTypeFromResult parses a Rust return-type string and returns
// the trailing identifier of the error parameter when the type is a
// Result. Handles:
//
//	Result<T, E>            → E
//	Result<T, std::io::E>   → E (trailing ident)
//	Result<T, Box<dyn E>>   → E
//	anyhow::Result<T>       → "" (single-arg form: error type elided)
//
// Returns "" for non-Result returns or when the error type can't be
// extracted unambiguously.
func rustErrorTypeFromResult(rt string) string {
	rt = strings.TrimSpace(rt)
	// Strip a leading qualifier like `std::result::` to land on
	// `Result`. Anchor on the "Result<" token rather than the last
	// "::" in the string: for `std::result::Result<T, io::Error>` the
	// last "::" sits inside the *error* type, so the old scan looked at
	// "Error>", concluded there was no Result, and dropped the edge.
	if i := strings.Index(rt, "Result<"); i > 0 {
		if strings.HasSuffix(rt[:i], "::") {
			rt = rt[i:]
		}
	}
	if !strings.HasPrefix(rt, "Result<") {
		return ""
	}
	inner := strings.TrimPrefix(rt, "Result<")
	inner = strings.TrimSuffix(inner, ">")
	// Split on the top-level comma. Depth tracking has to cover tuples
	// and slices as well as generics: counting only <> made
	// `Result<(u8, u16), MyError>` split inside the tuple and stamp a
	// throws edge to the literal target "u16)".
	parts := rustSplitTopLevel(inner, ',')
	if len(parts) < 2 {
		return ""
	}
	errPart := strings.TrimSpace(parts[1])
	// Box<dyn ErrType> / Box<dyn ErrType + Send> → ErrType
	for _, prefix := range []string{"Box<dyn ", "Box<", "Arc<dyn ", "Arc<"} {
		if strings.HasPrefix(errPart, prefix) {
			errPart = strings.TrimPrefix(errPart, prefix)
			errPart = strings.TrimSuffix(errPart, ">")
			break
		}
	}
	if i := strings.Index(errPart, "+"); i >= 0 {
		errPart = errPart[:i]
	}
	errPart = strings.TrimSpace(errPart)
	// Strip qualifier like std::io::Error.
	if i := strings.LastIndex(errPart, "::"); i >= 0 {
		errPart = errPart[i+2:]
	}
	// Strip generic instantiation like Error<Foo>.
	if i := strings.Index(errPart, "<"); i >= 0 {
		errPart = errPart[:i]
	}
	errPart = strings.TrimSpace(errPart)
	if errPart == "" || errPart == "_" {
		return ""
	}
	return errPart
}

// rustCollectAttributes walks the previous siblings of an item node
// and returns each `attribute_item` (#[...]) attached to it. Walks
// stop on the first non-attribute sibling. Outer attributes are the
// only form we handle — inner attributes (`#![foo]`) don't apply to a
// specific item.
func rustCollectAttributes(item *sitter.Node) []*sitter.Node {
	if item == nil {
		return nil
	}
	var out []*sitter.Node
	for sib := item.PrevSibling(); sib != nil; sib = sib.PrevSibling() {
		// A doc or explanatory comment interleaved with the attribute
		// stack is ordinary Rust style. Breaking on it discarded every
		// attribute above the comment — so a `#[derive(Serialize)]`
		// separated from its struct by one `// note` line vanished.
		if t := sib.Type(); t == "line_comment" || t == "block_comment" {
			continue
		}
		if sib.Type() != "attribute_item" {
			break
		}
		out = append(out, sib)
	}
	return out
}

// emitRustAnnotationEdges turns a slice of attribute_item nodes into
// EdgeAnnotated edges. `#[derive(Trait1, Trait2)]` is expanded into
// one edge per trait — that's the form that lets agents query "find
// every type that derives Debug" with one hop.
func emitRustAnnotationEdges(attrs []*sitter.Node, fromID, filePath string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	for _, attr := range attrs {
		var attrNode *sitter.Node
		for i, _nc := 0, int(attr.NamedChildCount()); i < _nc; i++ {
			c := attr.NamedChild(i)
			if c != nil && c.Type() == "attribute" {
				attrNode = c
				break
			}
		}
		if attrNode == nil {
			continue
		}
		name, args := rustAttributeNameAndArgs(attrNode, src)
		if name == "" {
			continue
		}
		line := int(attr.StartPoint().Row) + 1
		if name == "derive" && args != "" {
			for _, t := range rustSplitTopLevel(args, ',') {
				traitName := strings.TrimSpace(t)
				if traitName == "" {
					continue
				}
				EmitAnnotationEdge(fromID, "rust", traitName, "", filePath, line, result, seen)
				// A derive really does implement the trait — it is how
				// most Rust types acquire Debug, Clone, Serialize. The
				// annotation edge alone left find_implementations and
				// get_class_hierarchy blind to all of them.
				base := rustTraitPathBaseName(traitName)
				if base == "" {
					continue
				}
				result.Edges = append(result.Edges, &graph.Edge{
					From: fromID, To: "unresolved::" + base,
					Kind: graph.EdgeImplements, FilePath: filePath, Line: line,
					Origin: graph.OriginASTInferred,
					Meta:   map[string]any{"via": "derive"},
				})
			}
			continue
		}
		EmitAnnotationEdge(fromID, "rust", name, args, filePath, line, result, seen)
	}
}

// rustAttributeNameAndArgs reads an `attribute` AST node (the body of
// an attribute_item) and returns the attribute path's text plus any
// args inside the token_tree.
func rustAttributeNameAndArgs(attr *sitter.Node, src []byte) (string, string) {
	if attr == nil {
		return "", ""
	}
	var name, args string
	for i, _nc := 0, int(attr.ChildCount()); i < _nc; i++ {
		c := attr.Child(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier", "scoped_identifier", "path":
			if name == "" {
				name = c.Content(src)
			}
		case "token_tree":
			txt := c.Content(src)
			if len(txt) >= 2 && txt[0] == '(' && txt[len(txt)-1] == ')' {
				txt = txt[1 : len(txt)-1]
			}
			args = txt
		}
	}
	return name, args
}

// rustVisibility inspects an item node for a visibility_modifier child
// and returns the canonical visibility string. Default for items
// without a modifier is "private" (Rust default).
func rustVisibility(item *sitter.Node, src []byte) string {
	if item == nil {
		return VisibilityPrivate
	}
	for i, _nc := 0, int(item.ChildCount()); i < _nc; i++ {
		c := item.Child(i)
		if c == nil {
			continue
		}
		if c.Type() != "visibility_modifier" {
			continue
		}
		text := strings.TrimSpace(c.Content(src))
		switch {
		case text == "pub":
			return VisibilityPublic
		case strings.HasPrefix(text, "pub(crate"):
			return VisibilityInternal
		case strings.HasPrefix(text, "pub(super"), strings.HasPrefix(text, "pub(in"):
			return VisibilityInternal
		case strings.HasPrefix(text, "pub("):
			return VisibilityPublic
		}
	}
	return VisibilityPrivate
}

func (e *RustExtractor) recordTraitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, traitMethods map[string][]string, seen, annotationSeen map[string]bool) {
	def := m.Captures["sig.def"]
	traitNode := findEnclosingRustContainer(def.Node, "trait_item")
	if traitNode == nil {
		// Not a trait signature. The other producer of bodyless
		// functions is an `extern "C" { .. }` block, whose declarations
		// are the whole FFI surface of a -sys crate — 38,973 such
		// blocks across the sample corpus produced no node at all.
		e.emitForeignFunction(m, filePath, fileID, src, result, seen, annotationSeen)
		return
	}
	traitName := rustDeclName(traitNode, src)
	if traitName == "" {
		return
	}
	name := m.Captures["sig.name"].Text
	traitMethods[traitName] = append(traitMethods[traitName], name)
	// A dyn-dispatch call site binds to the trait *declaration*, so the
	// signature needs its own <file>::<Trait>.<method> node for find_usages
	// to answer and for inferOverrides to pair impl methods against it.
	e.emitTraitMethodNode(traitName, name, def, filePath, fileID, src, result, seen, annotationSeen)
}

// emitForeignFunction emits a node for a bodyless `fn` inside an
// `extern "C" { .. }` block. These declare the foreign side of an FFI
// boundary: they are the call targets of every unsafe call into C, and
// an audit that asks "what does this crate link against" has nothing to
// answer with unless they exist as nodes.
func (e *RustExtractor) emitForeignFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	def := m.Captures["sig.def"]
	nameCap := m.Captures["sig.name"]
	if def == nil || nameCap == nil {
		return
	}
	abi, inForeign := rustForeignABI(def.Node, src)
	if !inForeign {
		return
	}
	name := nameCap.Text
	startLine1 := def.StartLine + 1
	id, ok := disambiguateID(seen, filePath+"::"+name, startLine1)
	if !ok {
		return
	}
	meta := map[string]any{
		"signature":  buildRustSignature(def.Node, name, src),
		"visibility": rustVisibility(def.Node, src),
		"is_extern":  true,
		"is_unsafe":  true,
		"abi":        abi,
		// The body lives in another language entirely, so mark the node
		// as a declaration — a dead-code pass must not treat a missing
		// body as a missing implementation.
		"external": true,
	}
	applyRustScopeMod(meta, def.Node, src)
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	if rt := extractRustReturnType(def.Node, src); rt != "" {
		meta["return_type"] = rt
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "rust", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

func (e *RustExtractor) emitStruct(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, containers rustContainerIDs) {
	name := m.Captures["struct.name"].Text
	def := m.Captures["struct.def"]
	id, ok := disambiguateID(seen, filePath+"::"+name, def.StartLine+1)
	if !ok {
		return
	}
	containers.register(def.Node, id)
	meta := map[string]any{"visibility": rustVisibility(def.Node, src)}
	applyRustScopeMod(meta, def.Node, src)
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	meta["type_flavor"] = "struct"
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

func (e *RustExtractor) emitEnum(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, containers rustContainerIDs) {
	name := m.Captures["enum.name"].Text
	def := m.Captures["enum.def"]
	id, ok := disambiguateID(seen, filePath+"::"+name, def.StartLine+1)
	if !ok {
		return
	}
	containers.register(def.Node, id)
	meta := map[string]any{
		"kind":        "enum",
		"type_flavor": "enum",
		"visibility":  rustVisibility(def.Node, src),
	}
	applyRustScopeMod(meta, def.Node, src)
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

func (e *RustExtractor) emitTrait(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, traitMethods map[string][]string, containers rustContainerIDs) {
	name := m.Captures["trait.name"].Text
	def := m.Captures["trait.def"]
	id, ok := disambiguateID(seen, filePath+"::"+name, def.StartLine+1)
	if !ok {
		return
	}
	containers.register(def.Node, id)
	meta := map[string]any{"visibility": rustVisibility(def.Node, src)}
	applyRustScopeMod(meta, def.Node, src)
	if methods, ok := traitMethods[name]; ok {
		meta["methods"] = methods
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	meta["type_flavor"] = "trait"
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	bounds := def.Node.ChildByFieldName("bounds")
	if bounds == nil {
		for child := range def.Node.NamedChildren() {
			if child.Type() == "trait_bounds" {
				bounds = child
				break
			}
		}
	}
	if bounds != nil {
		for _, parentPath := range rustPositiveTraitBounds(bounds.Content(src)) {
			result.Edges = append(result.Edges, &graph.Edge{
				From: id, To: "unresolved::extends::" + parentPath,
				Kind: graph.EdgeExtends, FilePath: filePath, Line: def.StartLine + 1,
				Meta: map[string]any{"rust_trait_path": parentPath},
			})
		}
	}
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

func (e *RustExtractor) emitVariant(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult, containers rustContainerIDs) {
	def := m.Captures["variant.def"]
	enumNode := findEnclosingRustContainer(def.Node, "enum_item")
	if enumNode == nil {
		return
	}
	enumName := rustDeclName(enumNode, src)
	if enumName == "" {
		return
	}
	variantName := m.Captures["variant.name"].Text
	enumID := containers.lookup(enumNode, filePath+"::"+enumName)
	variantID := enumID + "." + variantName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: variantID, Kind: graph.KindVariable, Name: variantName,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "rust",
		Meta:      map[string]any{"receiver": enumName, "kind": "enum_variant"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: variantID, To: enumID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *RustExtractor) emitField(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult, containers rustContainerIDs) {
	def := m.Captures["field.def"]
	// A named field can sit in a struct, a union, or a struct-shaped
	// enum variant. Only the struct case used to be handled, so union
	// fields and variant payloads were dropped outright.
	ownerNode, ownerName, variant := rustFieldOwner(def.Node, src)
	if ownerNode == nil || ownerName == "" {
		return
	}
	fieldName := m.Captures["field.name"].Text
	structID := containers.lookup(ownerNode, filePath+"::"+ownerName)
	if variant != "" {
		structID += "." + variant
		ownerName += "." + variant
	}
	fieldID := structID + "." + fieldName
	meta := map[string]any{
		"receiver":   ownerName,
		"visibility": rustVisibility(def.Node, src),
	}
	if t := def.Node.ChildByFieldName("type"); t != nil {
		meta["field_type"] = strings.TrimSpace(t.Content(src))
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: fieldID, Kind: graph.KindField, Name: fieldName,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "rust",
		Meta:      meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fieldID, To: structID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitNamed handles const_item / static_item — they share the same
// node shape so we collapse them into one helper that takes the
// capture-name prefix.
func (e *RustExtractor) emitNamed(m parser.QueryResult, kind, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, containers rustContainerIDs) {
	nameCap := m.Captures[kind+".name"]
	def := m.Captures[kind+".def"]
	if nameCap == nil || def == nil {
		return
	}
	name := nameCap.Text
	startLine1 := def.StartLine + 1

	meta := map[string]any{
		"decl":       kind,
		"visibility": rustVisibility(def.Node, src),
	}
	applyRustScopeMod(meta, def.Node, src)
	if t := def.Node.ChildByFieldName("type"); t != nil {
		meta["declared_type"] = strings.TrimSpace(t.Content(src))
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}

	// An associated const belongs to its impl or trait. Emitting it at
	// file scope meant `impl A { const MAX }` and `impl B { const MAX }`
	// fought over one id and the loser was dropped entirely.
	base := filePath + "::" + name
	ownerNode, ownerName := rustAssociatedItemOwner(def.Node, src)
	if ownerName != "" {
		base = containers.lookup(ownerNode, filePath+"::"+ownerName) + "." + name
		meta["receiver"] = ownerName
	}
	id, ok := disambiguateID(seen, base, startLine1)
	if !ok {
		return
	}

	// `static mut` is the one form that is actually mutable shared
	// state, which is worth being able to query for on its own.
	nodeKind := graph.KindConstant
	if kind == "static" {
		nodeKind = graph.KindVariable
		for i, _nc := 0, int(def.Node.ChildCount()); i < _nc; i++ {
			if c := def.Node.Child(i); c != nil && c.Content(src) == "mut" {
				meta["mutable"] = true
				break
			}
		}
	}

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: nodeKind, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "rust", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	if ownerName != "" {
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: containers.lookup(ownerNode, filePath+"::"+ownerName),
			Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})
	}
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
}

func (e *RustExtractor) emitUse(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	path := m.Captures["use.path"]
	usePath := strings.ReplaceAll(path.Text, "::", "/")
	edge := &graph.Edge{
		From: fileID, To: "unresolved::import::" + usePath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	}
	// A `pub use` (or `pub(crate) use`) declaration is a re-export: the
	// names it brings into scope become part of this module's public
	// surface. Tag the import edge so the re-export chain follower can
	// tell a transparent forward from a private `use`. A plain `use`
	// carries no tag and never forwards a name to a downstream consumer.
	if def := m.Captures["use.def"]; def != nil {
		if v := rustVisibility(def.Node, src); v == VisibilityPublic || v == VisibilityInternal {
			edge.Meta = map[string]any{"reexport": true}
		}
	}
	result.Edges = append(result.Edges, edge)
}

// --- Helpers --------------------------------------------------------

// findEnclosingRustContainer walks the parent chain of n looking for
// the nearest ancestor whose Type() matches t. Returns nil if none.
func findEnclosingRustContainer(n *sitter.Node, t string) *sitter.Node {
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

// rustImplMethodReceiver returns the receiver type name when fn is a
// direct member of an impl_item's declaration_list, mirroring the
// legacy rsQImplMethod pattern. Returns "" for trait default impls,
// nested fns, and free functions — the legacy code only treated the
// direct-child case as a method.
func rustImplMethodReceiver(fn *sitter.Node, src []byte) string {
	if fn == nil {
		return ""
	}
	parent := fn.Parent()
	if parent == nil || parent.Type() != "declaration_list" {
		return ""
	}
	grand := parent.Parent()
	if grand == nil || grand.Type() != "impl_item" {
		return ""
	}
	typeNode := grand.ChildByFieldName("type")
	if typeNode == nil {
		return ""
	}
	return rustImplReceiverName(typeNode.Content(src))
}

// rustImplReceiverName reduces an impl target to the name a method node
// can hang off. `impl Trait for &'a Buf` used to mint the method id
// `<file>::&'a Buf.show` with a member_of edge to a node that never
// exists; the same went for slice, array and raw-pointer impls.
//
// The generic spelling is deliberately preserved — `Replacer<M>.replace`
// is the established id shape, and rebindRustLocalGenericMemberOwners
// repairs the member_of target for it separately.
func rustImplReceiverName(t string) string {
	t = strings.TrimSpace(t)
	for {
		trimmed := t
		// Reference: &T, &mut T, &'a T.
		if strings.HasPrefix(trimmed, "&") {
			trimmed = strings.TrimSpace(trimmed[1:])
			if strings.HasPrefix(trimmed, "'") {
				if i := strings.IndexAny(trimmed, " \t"); i > 0 {
					trimmed = strings.TrimSpace(trimmed[i:])
				}
			}
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "mut "))
		}
		// Raw pointer: *const T, *mut T.
		if strings.HasPrefix(trimmed, "*") {
			trimmed = strings.TrimSpace(trimmed[1:])
			trimmed = strings.TrimPrefix(trimmed, "const ")
			trimmed = strings.TrimPrefix(trimmed, "mut ")
			trimmed = strings.TrimSpace(trimmed)
		}
		// Slice or array: [T] and [T; N].
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if parts := rustSplitTopLevel(inner, ';'); len(parts) > 0 {
				inner = parts[0]
			}
			trimmed = strings.TrimSpace(inner)
		}
		if strings.HasPrefix(trimmed, "dyn ") {
			trimmed = strings.TrimSpace(trimmed[4:])
		}
		if trimmed == t {
			break
		}
		t = trimmed
	}
	return t
}

// rustTraitMethodOwner returns the trait name when fn is a direct member
// of a trait_item's declaration_list — a trait-method declaration (a
// signature or a default-body method). Mirrors rustImplMethodReceiver but
// for trait bodies; returns "" for impl bodies, nested fns and free fns.
func rustTraitMethodOwner(fn *sitter.Node, src []byte) string {
	if fn == nil {
		return ""
	}
	parent := fn.Parent()
	if parent == nil || parent.Type() != "declaration_list" {
		return ""
	}
	grand := parent.Parent()
	if grand == nil || grand.Type() != "trait_item" {
		return ""
	}
	nameNode := grand.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return nameNode.Content(src)
}

// rustImplTraitName returns the base name of the trait a fn's enclosing
// impl block implements (`impl Trait for Type`), or "" when fn is not an
// impl method or the impl is inherent (`impl Type`). Unwraps scoped and
// generic trait paths (fmt::Display -> Display, Iterator<Item=u8> ->
// Iterator).
func rustImplTraitName(fn *sitter.Node, src []byte) string {
	if fn == nil {
		return ""
	}
	parent := fn.Parent()
	if parent == nil || parent.Type() != "declaration_list" {
		return ""
	}
	impl := parent.Parent()
	if impl == nil || impl.Type() != "impl_item" {
		return ""
	}
	tr := impl.ChildByFieldName("trait")
	if tr == nil {
		return ""
	}
	return rustTraitPathBaseName(tr.Content(src))
}

// rustTraitPathBaseName reduces a trait reference (possibly scoped and/or
// generic) to its base type name: `fmt::Display` -> `Display`,
// `Iterator<Item = u8>` -> `Iterator`.
func rustTraitPathBaseName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "<"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	return strings.TrimSpace(s)
}

// emitTraitMethodNode emits a KindMethod node for a trait-declaration
// method — a signature (function_signature_item, no body) or a default
// method (function_item with a body). Named <file>::<Trait>.<method> so
// the id scheme matches impl methods; EdgeMemberOf points at the trait's
// interface node so inferOverrides pairs each concrete impl method against
// its declaration. Meta["trait_decl"]="true" marks the node as a
// declaration rather than a concrete impl.
func (e *RustExtractor) emitTraitMethodNode(traitName, name string, def *parser.CapturedNode, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	if def == nil || def.Node == nil {
		return
	}
	startLine1 := def.StartLine + 1
	id, ok := disambiguateID(seen, filePath+"::"+traitName+"."+name, startLine1)
	if !ok {
		return
	}
	complexity, cognitive, loopDepth := 0, 0, 0
	if body := def.Node.ChildByFieldName("body"); body != nil {
		complexity, cognitive, loopDepth = BodyComplexityMetrics(body, "rust")
	}
	meta := map[string]any{
		"receiver":   traitName,
		"trait_decl": "true",
		"signature":  buildRustSignature(def.Node, name, src),
		// A trait member carries no visibility modifier of its own — it
		// is exactly as reachable as the trait that declares it. Reading
		// the absent modifier as "private" made every method of every
		// public trait look private, which understates the public API
		// surface and invites the dead-code pass to over-reach.
		"visibility": rustEnclosingTraitVisibility(def.Node, src),
	}
	applyRustFnQualifiers(meta, def.Node, src)
	applyRustScopeMod(meta, def.Node, src)
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	if rt := extractRustReturnType(def.Node, src); rt != "" {
		meta["return_type"] = rt
	}
	if tps := rustTypeParams(def.Node, src); len(tps) > 0 {
		meta["type_params"] = tps
	}
	ApplyComplexityMeta(meta, complexity, cognitive, loopDepth)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "rust", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: filePath + "::" + traitName, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustThrowsEdges(def.Node, src, id, filePath, startLine1, result)
	emitRustFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

// rustDeclName returns the source text of the `name` field on a Rust
// declaration node (struct_item, enum_item, trait_item, etc.).
func rustDeclName(decl *sitter.Node, src []byte) string {
	if decl == nil {
		return ""
	}
	nameNode := decl.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return nameNode.Content(src)
}

// extractRustReturnType walks a function_item node to find the return type after `->`.
func extractRustReturnType(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	// In tree-sitter-rust, function_item has children: fn, name, parameters, ->, type, block.
	// Look for a type child after "->".
	pastArrow := false
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		text := string(src[child.StartByte():child.EndByte()])
		if text == "->" {
			pastArrow = true
			continue
		}
		if pastArrow {
			if child.Type() == "block" {
				return ""
			}
			// This should be the return type node.
			rawType := string(src[child.StartByte():child.EndByte()])
			if rt := normalizeRustTypeName(rawType); rt != "" {
				return rt
			}
			return ""
		}
	}
	return ""
}

// normalizeRustTypeName strips references, generics, and module paths from a Rust type.
func normalizeRustTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Remove reference prefixes.
	t = strings.TrimPrefix(t, "&mut ")
	t = strings.TrimPrefix(t, "&")
	// Remove generics.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Take last segment of module path.
	if idx := strings.LastIndex(t, "::"); idx >= 0 {
		t = t[idx+2:]
	}
	// Skip primitives.
	switch t {
	case "i8", "i16", "i32", "i64", "i128", "isize",
		"u8", "u16", "u32", "u64", "u128", "usize",
		"f32", "f64", "bool", "char", "str", "String",
		"Self", "self":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return ""
	}
	return t
}

// rustFuncParamTypes returns a map from parameter name to its normalized,
// non-primitive type for a function/method node. A self parameter binds to
// ownerType (the enclosing impl/trait type) when known. Mirrors Go's
// paramsByFunc so a selector call on a parameter or self receiver resolves.
func rustFuncParamTypes(fn *sitter.Node, ownerType string, src []byte) map[string]string {
	if fn == nil {
		return nil
	}
	params := fn.ChildByFieldName("parameters")
	if params == nil {
		return nil
	}
	out := map[string]string{}
	for i, n := 0, int(params.NamedChildCount()); i < n; i++ {
		p := params.NamedChild(i)
		if p == nil {
			continue
		}
		switch p.Type() {
		case "self_parameter":
			if ownerType != "" {
				out["self"] = ownerType
			}
		case "parameter":
			pat := p.ChildByFieldName("pattern")
			ty := p.ChildByFieldName("type")
			if pat == nil || ty == nil {
				continue
			}
			if t := normalizeRustTypeName(ty.Content(src)); t != "" {
				out[pat.Content(src)] = t
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lookupRustRecvType resolves a selector-call receiver name to a type,
// checking the caller's own parameter/self scope before the file-wide let
// environment so a parameter shadows a same-named binding elsewhere.
func lookupRustRecvType(paramsByFunc map[string]map[string]string, tenv typeEnv, callerID, name string) (string, bool) {
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

// rustConcreteTypeName reports whether name can seed the local type
// environment from an associated-constructor qualifier as a resolvable,
// concrete type. It must be UpperCamelCase and name a real type, so a single
// uppercase letter (a generic parameter such as T/E/K, which owns no methods
// in the graph) and the `Self` placeholder (which names no node) are both
// rejected. A struct literal cannot name a generic parameter and so is guarded
// more loosely above; only here — the qualifier of a `T::ctor()` call — may a
// bare single letter really be a generic parameter, so the single-letter form
// is refused to keep an inferred receiver from ever pointing at a non-type.
func rustConcreteTypeName(name string) bool {
	if len(name) < 2 || name == "Self" {
		return false
	}
	return name[0] >= 'A' && name[0] <= 'Z'
}

// rustSelfReturningCtor reports whether an associated-function name is one of
// the conventional constructors that yields a value of the type it hangs off.
// Binding a let's inferred type to that qualifier is safe because idiomatic
// Rust reserves these names for constructors that return the owning type itself,
// unwrapped: `T::from`/`T::default` implement the From / Default traits
// (`fn(..) -> Self`), and `new`/`with_capacity`/`with_config`/`empty` are the
// near-universal inherent-constructor convention, each returning `Self`.
//
// Only names whose result IS the qualifier type belong here. Constructors that
// return a *wrapper* around Self are deliberately excluded: `from_str` and
// `open` yield a `Result<Self>` and `builder` a separate builder struct, so the
// binding's real type is that wrapper — not the qualifier. The downstream
// owner-match cannot recover from admitting them: it refuses to bind to a method
// the qualifier type does NOT own, but a wrapper-returning constructor whose
// qualifier happens to own a same-named method (a getter shadowing a builder's
// setter, say) would bind that call to the wrong owner rather than fall through
// to unresolved. Confining the list to genuinely Self-returning names is what
// keeps every seeded receiver pointing at the value actually in hand.
func rustSelfReturningCtor(method string) bool {
	switch method {
	case "new", "default", "from", "with_capacity", "with_config", "empty":
		return true
	}
	return false
}

// inferTypeFromRustExpr inspects a tree-sitter expression node to infer
// the type of a let declaration's RHS.
func inferTypeFromRustExpr(node *sitter.Node, src []byte) string {
	switch node.Type() {
	case "struct_expression":
		// Config { port: 8080 } — first named child is the type name. The
		// literal constructs a value of exactly that type, so a selector call
		// on the binding resolves against it. A struct literal can never name a
		// generic parameter (Rust forbids `T { .. }`), so even a single-letter
		// UpperCamelCase name is a concrete type here and is trusted — the
		// single-letter rejection only guards the associated-call arm below,
		// where the qualifier may instead be a generic parameter.
		if node.NamedChildCount() > 0 {
			name := node.NamedChild(0).Content(src)
			// Strip module path.
			if idx := strings.LastIndex(name, "::"); idx >= 0 {
				name = name[idx+2:]
			}
			if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		}

	case "call_expression":
		// Type::<ctor>(..) — a scoped associated call whose qualifier names the
		// constructed type. Only the conventional Self-returning constructors are
		// trusted, and the qualifier must be a bare concrete type name, so the
		// seeded binding can only point at the value the constructor produces —
		// never at a lowercase module function, `Self`, or a generic parameter.
		if node.NamedChildCount() > 0 {
			funcNode := node.NamedChild(0)
			if funcNode.Type() == "scoped_identifier" {
				funcText := funcNode.Content(src)
				// e.g. "Config::from" or "module::Config::with_capacity".
				if idx := strings.LastIndex(funcText, "::"); idx > 0 {
					method := funcText[idx+2:]
					if rustSelfReturningCtor(method) {
						typePart := funcText[:idx]
						// Take last path segment as the type name.
						if j := strings.LastIndex(typePart, "::"); j >= 0 {
							typePart = typePart[j+2:]
						}
						if rustConcreteTypeName(typePart) {
							return typePart
						}
					}
				}
			}
		}
	}

	return ""
}
