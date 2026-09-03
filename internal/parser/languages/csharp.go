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

  ; An indexer has no name node - "this" is a keyword, not an
  ; identifier - so the whole declaration is captured and the member is
  ; named at emission.
  (indexer_declaration) @indexer.def

  ; An event declared with add/remove accessor bodies. The bodyless
  ; field form (event T E;) is a separate grammar node
  ; (event_field_declaration) with nothing executable in it.
  (event_declaration
    name: (identifier) @event.name) @event.def

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
	// offset is the call expression's start byte. A line number cannot
	// say which side of a block boundary a call falls on, and cannot
	// separate two sites that share one physical line; a scope test
	// needs a coordinate that can.
	offset   int
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
	if inv != nil {
		c.offset = int(inv.StartByte())
	}
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

// csharpLocalScope is one local declaration's binding extent: the byte
// range of the block that declares it. A local shadows a same-named
// field only inside that range — a name declared in a nested block that
// has already closed binds nothing at a later call site, and refusing
// evidence there costs the site its receiver spelling for no reason.
type csharpLocalScope struct {
	start, end int
}

// csharpLocalScopes indexes each function's declared local names by the
// extents that declare them. It replaces a flat name set: the set could
// only answer "declared somewhere in this function", which is not the
// question a shadow test asks.
type csharpLocalScopes map[string]map[string][]csharpLocalScope

// shadows reports whether a local declared in owner and named name is in
// scope at offset.
func (s csharpLocalScopes) shadows(owner, name string, offset int) bool {
	for _, sc := range s[owner][name] {
		if offset >= sc.start && offset < sc.end {
			return true
		}
	}
	return false
}

// shadowsAnywhere is the function-wide question, for consumers whose
// sites carry no byte offset. It is the pre-extent behavior, kept
// deliberately: answering a narrower question without a real coordinate
// would open a hole rather than close one.
func (s csharpLocalScopes) shadowsAnywhere(owner, name string) bool {
	return len(s[owner][name]) > 0
}

// csharpScopeFormers are the ancestors that bound a local binding's
// extent. `block` alone is not enough: a switch section, a
// switch-expression arm, a loop header, or an expression-bodied lambda
// each form a scope the grammar does not spell as a block, and climbing
// past them hands the binding a method-wide extent — which turns the
// shadow refusal back into the function-wide question the extent
// machinery exists to replace, and costs unrelated calls their
// static-form evidence.
//
// `if_statement` is deliberately absent: a pattern variable declared in
// an `if` condition escapes to the ENCLOSING block (definite-assignment
// scoping), so stopping at the `if` would under-refuse. Loop headers do
// not leak their pattern variables past the statement. The switch BODY
// is one declaration space shared by every section for the locals its
// STATEMENT LISTS declare — that is exactly why redeclaring a name in a
// sibling section is CS0128 — so those extents stop at `switch_body`
// (round-6 finding B1: stopping at the section read a sibling-section
// local as expired, dropping its receiver evidence and re-minting its
// assignments as field writes). A pattern variable bound in a case
// LABEL or `when` guard is the one exception: it is section-scoped
// (sibling sections may redeclare it), handled positionally in
// csharpLocalScopeOf rather than by this table — the ancestor node type
// alone cannot express both rules.
var csharpScopeFormers = map[string]bool{
	"block":                       true,
	"switch_body":                 true,
	"switch_expression_arm":       true,
	"lambda_expression":           true,
	"anonymous_method_expression": true,
	"local_function_statement":    true,
	"arrow_expression_clause":     true,
	"while_statement":             true,
	"do_statement":                true,
	"for_statement":               true,
	"foreach_statement":           true,
	"using_statement":             true,
	"lock_statement":              true,
	"fixed_statement":             true,
	// A pattern variable in a catch FILTER scopes over its catch clause
	// (filter plus handler); one in a query clause scopes within the
	// query. Climbing past either shadowed the name method-wide, and an
	// earlier same-named field use lost its evidence (round-6
	// non-blocking finding 1). Locals inside the catch handler's or a
	// query lambda's own block still stop at that block first.
	"catch_clause":     true,
	"query_expression": true,
}

// csharpLocalScopeOf returns the extent of the scope declaring a local.
// A declaration with no scope-forming ancestor gets an unbounded
// extent, which keeps its refusal function-wide — exactly what every
// local had before extents existed, so an unrecognized shape can never
// lose a refusal.
func csharpLocalScopeOf(n *sitter.Node) csharpLocalScope {
	for cur := n; cur != nil; cur = cur.Parent() {
		// A switch SECTION is not a declaration space for the locals its
		// statement list declares (those live in the switch BODY), but it
		// IS one for a pattern variable bound in a case label or a `when`
		// guard. Roslyn accepts `case int x:` beside `case string x:` in a
		// sibling section AND accepts `case 1: int y = 1;` / `default: y =
		// 2;`, so the two rules coexist and the answer depends on where in
		// the section the binder sits: label territory is everything
		// before the section's first statement.
		if cur.Type() == "switch_section" && csharpInSwitchLabel(cur, n) {
			return csharpLocalScope{start: int(cur.StartByte()), end: int(cur.EndByte())}
		}
		if csharpScopeFormers[cur.Type()] {
			return csharpLocalScope{start: int(cur.StartByte()), end: int(cur.EndByte())}
		}
	}
	return csharpLocalScope{start: 0, end: math.MaxInt}
}

// csharpInSwitchLabel reports whether binder sits in section's LABEL
// region - before the first statement the section lists.
func csharpInSwitchLabel(section, binder *sitter.Node) bool {
	for i, _nc := 0, int(section.NamedChildCount()); i < _nc; i++ {
		c := section.NamedChild(i)
		if c == nil {
			continue
		}
		t := c.Type()
		if strings.HasSuffix(t, "_statement") || t == "block" {
			return binder.StartByte() < c.StartByte()
		}
	}
	return false
}

// csharpTypedBinding is one typed local declaration's evidence record:
// the annotated (or inferred) type, the builtin keyword form, the
// canonical shape, and the declaring scope's extent. The tenv/builtin/
// shape maps answer the function-wide question; these records answer it
// AT AN OFFSET, so an explicitly-typed local dies with its block exactly
// like the scope index already lets a var local die - a
// `{ int X = 1; }` must not type a later X receiver as int (round-5
// finding 2).
type csharpTypedBinding struct {
	typ, builtin, shape string
	scope               csharpLocalScope
}

// Verdicts of csharpTypedLocalAt. Absent keeps the caller on its
// function-wide fallback (a name the records never saw); Expired means
// records exist but none covers the offset - the local is dead there,
// and falling back would resurrect the function-wide bug.
const (
	csharpTypedAbsent = iota
	csharpTypedFound
	csharpTypedExpired
)

// csharpTypedLocalAt returns the innermost typed-local record covering
// offset, or the Absent/Expired verdict.
func csharpTypedLocalAt(m map[string]map[string][]*csharpTypedBinding, owner, name string, offset int) (*csharpTypedBinding, int) {
	recs := m[owner][name]
	if len(recs) == 0 {
		return nil, csharpTypedAbsent
	}
	var best *csharpTypedBinding
	for _, b := range recs {
		if offset < b.scope.start || offset >= b.scope.end {
			continue
		}
		if best == nil || (b.scope.end-b.scope.start) < (best.scope.end-best.scope.start) {
			best = b
		}
	}
	if best == nil {
		return nil, csharpTypedExpired
	}
	return best, csharpTypedFound
}

// csharpChainHead returns the leading identifier of a chained receiver
// expression (`h.Make().Ping` -> `h`), the only name the chain walker
// looks up in the type environment.
func csharpChainHead(expr string) string {
	cleaned := strings.ReplaceAll(stripCallArgs(expr), "::", ".")
	if i := strings.IndexByte(cleaned, '.'); i >= 0 {
		cleaned = cleaned[:i]
	}
	return strings.TrimSpace(cleaned)
}

// csharpOffsetEnv hands the offset-blind chain/awaited walkers a type
// environment whose HEAD entry is corrected by the offset-aware
// typed-local records (issue #725 item 3: the function-wide tenv is
// written last-wins by sibling redeclarations, so consulting it raw
// types a chain through whichever sibling declared LAST). A Found
// record overrides the head, an Expired or type-less record removes it
// (the flat value belongs to a sibling), and an Absent name keeps the
// function-wide fallback exactly as before.
func csharpOffsetEnv(typedLocals map[string]map[string][]*csharpTypedBinding, tenv typeEnv, owner, head string, offset int) typeEnv {
	if head == "" {
		return tenv
	}
	b, state := csharpTypedLocalAt(typedLocals, owner, head, offset)
	if state == csharpTypedAbsent {
		return tenv
	}
	if state == csharpTypedFound && b.typ != "" && tenv[head] == b.typ {
		return tenv
	}
	env := make(typeEnv, len(tenv))
	for k, v := range tenv {
		env[k] = v
	}
	if state == csharpTypedFound && b.typ != "" {
		env[head] = b.typ
	} else {
		delete(env, head)
	}
	return env
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
	// Function/ctor BYTE extents, recorded at emission: a line number
	// cannot say which of two members sharing a physical line owns a
	// call site; the byte interval can (round-5 finding 4).
	funcBytes := make(map[string][2]int)
	// Line spans matching funcBytes for member kinds whose NODE lines
	// can differ from the extent that owns their code - a C# 13 partial
	// property's node keeps the first fragment's lines while the extents
	// belong to the implementing fragment, and a field's extent is its
	// declarator, not the whole declaration.
	funcLines := make(map[string][2]int)
	// Properties carrying a set/init accessor, recorded at emission so
	// the deferred typing pass can seed the accessor's implicit `value`
	// parameter with the property's declared type.
	valueProps := make(map[string]csharpValueProp)
	// Type IDs whose FIRST declaration spelled `partial` — the gate for
	// preserving a later same-file fragment's base list (round-5
	// finding 5).
	partialSeen := make(map[string]*csharpPartialIdentity)

	// Pre-scan the file's own interface declarations. A base type that
	// names one of these is definitively an interface, even when its name
	// doesn't follow the `I`-prefix convention — the base-list heuristic
	// (emitCSharpBaseList) checks this set before falling back to name
	// shape so a locally-known interface always wins.
	localInterfaces := collectCSharpInterfaceNames(root, src)

	// Using-alias names, collected once per file: the type-argument stamp
	// sites consult them per declaration, and a per-declaration rescan of
	// the enclosing namespace was quadratic in sibling count.
	fileAliases := csharpFileAliasNames(root, src)

	// Per type node ID, across every declaration in the file — see
	// csharpBaseNameCounts for why one declaration's own base list is not
	// a sufficient ambiguity check.
	baseNameCounts := csharpBaseNameCounts(root, src, filePath, fileAliases)

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
			e.emitContainer(m, "class", graph.KindType, filePath, fileID, src, result, seen, annotationSeen, localInterfaces, fileAliases, baseNameCounts, partialSeen)

		case m.Captures["iface.def"] != nil:
			e.emitContainer(m, "iface", graph.KindInterface, filePath, fileID, src, result, seen, annotationSeen, localInterfaces, fileAliases, baseNameCounts, partialSeen)

		case m.Captures["struct.def"] != nil:
			e.emitContainer(m, "struct", graph.KindType, filePath, fileID, src, result, seen, annotationSeen, localInterfaces, fileAliases, baseNameCounts, partialSeen)

		case m.Captures["record.def"] != nil:
			e.emitContainer(m, "record", graph.KindType, filePath, fileID, src, result, seen, annotationSeen, localInterfaces, fileAliases, baseNameCounts, partialSeen)

		case m.Captures["enum.def"] != nil:
			e.emitContainer(m, "enum", graph.KindType, filePath, fileID, src, result, seen, annotationSeen, localInterfaces, fileAliases, baseNameCounts, partialSeen)

		case m.Captures["anon.def"] != nil:
			e.emitAnonymousType(m, filePath, fileID, result, seen)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, fileID, src, result, seen, annotationSeen, ifaceMethods, funcBytes)

		case m.Captures["ctor.def"] != nil:
			e.emitConstructor(m, filePath, fileID, src, result, seen, funcBytes)

		case m.Captures["field.def"] != nil:
			e.emitField(m, filePath, fileID, src, result, seen, fileAliases, funcBytes, funcLines)

		case m.Captures["prop.def"] != nil:
			e.emitProperty(m, filePath, fileID, src, result, seen, fileAliases, funcBytes, funcLines, valueProps)

		case m.Captures["indexer.def"] != nil:
			e.emitAccessorMember(m.Captures["indexer.def"], csharpIndexerName, "indexer", filePath, fileID, src, result, seen, fileAliases, funcBytes, funcLines, valueProps)

		case m.Captures["event.def"] != nil:
			// "event_accessor", not "event": this arm sees only the
			// accessor-bearing form. The far commoner field form
			// (event T E;) is a different grammar node and stays
			// unemitted, so a stamp spelled "event" would promise a
			// type's events and deliver a biased minority of them.
			e.emitAccessorMember(m.Captures["event.def"], m.Captures["event.name"].Text, "event_accessor", filePath, fileID, src, result, seen, fileAliases, funcBytes, funcLines, valueProps)

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
			fa := csharpDeferredFieldAssign{
				name:   m.Captures["fassign.name"].Text,
				line:   m.Captures["fassign.expr"].StartLine + 1,
				offset: -1,
			}
			if node := m.Captures["fassign.name"].Node; node != nil {
				fa.offset = int(node.StartByte())
			}
			fieldAssigns = append(fieldAssigns, fa)

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
	csharpStampOwnershipSpans(result, funcLines)
	funcRanges := newCSharpFuncLookup(csharpOwnerRanges(result, funcBytes, funcLines), funcBytes)

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
		return funcRanges.enclosingAt(int(l.defNode.StartPoint().Row)+1, int(l.defNode.StartByte()))
	}
	tenvByOwner := map[string]typeEnv{}
	// The offset-aware mirror of the tenv/shape/builtin maps: one
	// evidence record per typed local declaration, merged across the
	// passes below by declaring extent, consulted by the call-receiver
	// chain at the call's own byte offset.
	typedLocalsByOwner := map[string]map[string][]*csharpTypedBinding{}
	typedRecord := func(owner string, l csharpDeferredLocal) *csharpTypedBinding {
		if l.defNode == nil {
			return nil
		}
		sc := csharpLocalScopeOf(l.defNode)
		m := typedLocalsByOwner[owner]
		if m == nil {
			m = map[string][]*csharpTypedBinding{}
			typedLocalsByOwner[owner] = m
		}
		for _, b := range m[l.name] {
			if b.scope == sc {
				return b
			}
		}
		b := &csharpTypedBinding{scope: sc}
		m[l.name] = append(m[l.name], b)
		return b
	}
	// typedRecordAt is the lookup-only twin of typedRecord: it never
	// mints a record, so a guard can ask "does THIS declaration's scope
	// already carry evidence" without converting an Absent name into a
	// Found-but-empty record that would block the function-wide
	// fallbacks.
	typedRecordAt := func(owner string, l csharpDeferredLocal) *csharpTypedBinding {
		if l.defNode == nil {
			return nil
		}
		sc := csharpLocalScopeOf(l.defNode)
		for _, b := range typedLocalsByOwner[owner][l.name] {
			if b.scope == sc {
				return b
			}
		}
		return nil
	}
	setLocalType := func(owner string, l csharpDeferredLocal, typeName string) {
		env := tenvByOwner[owner]
		if env == nil {
			env = make(typeEnv)
			tenvByOwner[owner] = env
		}
		env[l.name] = typeName
		if b := typedRecord(owner, l); b != nil && b.typ == "" {
			b.typ = typeName
		}
	}
	for _, l := range locals {
		owner := localOwner(l)
		if owner == "" {
			continue
		}
		typeName := normalizeCSharpTypeName(l.rawType)
		if typeName != "" && typeName != "var" {
			setLocalType(owner, l, typeName)
		}
	}
	for _, l := range locals {
		owner := localOwner(l)
		if owner == "" || l.rawType != "var" || l.defNode == nil {
			continue
		}
		// Per-record, not per-function: a second same-named `var` in a
		// SIBLING scope must mint its own offset record, or the offset
		// lookup answers Expired at its sites and drops the receiver
		// evidence entirely (round-6 finding B5).
		if b := typedRecordAt(owner, l); b != nil && b.typ != "" {
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
					setLocalType(owner, l, typeName)
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
		// Same per-record guard as tier 1 (round-6 finding B5).
		if b := typedRecordAt(owner, l); b != nil && b.typ != "" {
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
			// The awaited call's receiver is typed at the DECLARATION's
			// offset, not from the function-wide last-wins map — a sibling
			// block's same-named local must not type this block's await.
			innerText := inner.Content(src)
			env := csharpOffsetEnv(typedLocalsByOwner, tenvByOwner[owner], owner, csharpChainHead(innerText), int(n.StartByte()))
			if t := csharpAwaitedCallType(innerText, csharpOwnerTypeName(owner), env, result); t != "" {
				setLocalType(owner, l, t)
			}
		})
	}

	// Type SHAPE rides in a parallel per-method map: the core stamps keep
	// their bare spelling (every downstream consumer stays valid), while
	// array/nullable suffixes and generic arguments — which are part of
	// applicability — survive in a receiver_shape stamp.
	shapesByOwner := map[string]map[string]string{}
	setLocalShape := func(owner string, l csharpDeferredLocal, shape string) {
		m := shapesByOwner[owner]
		if m == nil {
			m = map[string]string{}
			shapesByOwner[owner] = m
		}
		m[l.name] = shape
		if b := typedRecord(owner, l); b != nil && b.shape == "" {
			b.shape = shape
		}
	}
	for _, l := range locals {
		owner := localOwner(l)
		if owner == "" {
			continue
		}
		// Per-record like the type tiers: a sibling-scope redeclaration
		// carries its own shape, and skipping it on the function-wide
		// map starved the record that the generic-extension veto reads
		// (round-6 finding B5).
		if b := typedRecordAt(owner, l); b != nil && b.shape != "" {
			continue
		}
		if shape := csharpCanonTypeShape(l.rawType); shape != "" {
			setLocalShape(owner, l, shape)
		} else if l.rawType == "var" && l.defNode != nil {
			// Same first-creation rule as the type walk above — the
			// two stamps must describe the same creation expression.
			done := false
			walkNodes(l.defNode, func(n *sitter.Node) {
				if !done && n.Type() == "object_creation_expression" {
					if tn := n.ChildByFieldName("type"); tn != nil {
						if s := csharpCanonTypeShape(tn.Content(src)); s != "" {
							setLocalShape(owner, l, s)
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
		owner := funcRanges.enclosingAt(int(l.defNode.StartPoint().Row)+1, int(l.defNode.StartByte()))
		if owner == "" {
			continue
		}
		m := builtinsByOwner[owner]
		if m == nil {
			m = map[string]string{}
			builtinsByOwner[owner] = m
		}
		m[l.name] = bt
		if b := typedRecord(owner, l); b != nil && b.builtin == "" {
			b.builtin = bt
		}
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
	emitCSharpReferenceForms(root, src, filePath, fileID, result, funcBytes, funcLines)

	// Two same-named calls on ONE line whose receivers differ make the
	// line unattributable: member companions dedupe to a single stored
	// edge (`_a.Fetch(_b.Fetch(1))`), and a receiverless implicit-this
	// call (`Get(1) + _widgets.Get(2)`) leaves the line's only
	// `unresolved::*.Get` companion as the join target for a bound edge
	// that is not its call. Mark those sites so no downstream consumer
	// applies one receiver's typing to the other call's edge (the
	// dispatch gate's receiver evidence in particular).
	//
	// EVERY call seeds the index — the receiverless form as the empty
	// receiver — because an empty receiver differing from a spelled one
	// is exactly as disqualifying as two spelled receivers differing.
	type csharpCallSite struct {
		name string
		line int
	}
	memberSiteReceiver := map[csharpCallSite]string{}
	memberSiteAmbiguous := map[csharpCallSite]bool{}
	for _, c := range calls {
		key := csharpCallSite{c.name, c.line}
		if prev, ok := memberSiteReceiver[key]; ok {
			if prev != c.receiver {
				memberSiteAmbiguous[key] = true
			}
		} else {
			memberSiteReceiver[key] = c.receiver
		}
	}

	// Shadow indexes, built BEFORE the call emission so receiver_name can
	// honor its contract ("a receiver no local, param or builtin
	// explains"): a bare receiver naming a declared parameter or local is
	// that value — never the enclosing type's same-named field, never a
	// static class. Locals ride the tenv only when typed; the scope index
	// covers every declaration, and covers it where it actually binds.
	// The field-identifier emitter reuses both.
	paramsByOwner := csharpParamNamesByOwner(result)
	localScopes := csharpLocalScopes{}
	// The implicit `value` parameter of a set/init accessor carries the
	// property's declared type, and inside those accessors it is ALWAYS
	// the parameter - a same-named member is only reachable there via
	// this.value. Seed it as an offset-scoped record over each accessor
	// span, never owner-wide: in the GETTER the bare name means a member
	// (possibly inherited, possibly declared in another partial
	// fragment), and an owner-wide seed typed those sites with the
	// property type - a confident wrong answer. The scope registration
	// keeps the shadow refusal and the field-identifier lane from
	// reading the parameter as field evidence inside the accessor.
	for id, vp := range valueProps {
		t := normalizeCSharpTypeName(vp.typ)
		if t == "" || t == "var" {
			continue
		}
		recs := typedLocalsByOwner[id]
		if recs == nil {
			recs = map[string][]*csharpTypedBinding{}
			typedLocalsByOwner[id] = recs
		}
		if localScopes[id] == nil {
			localScopes[id] = map[string][]csharpLocalScope{}
		}
		for _, sp := range vp.spans {
			recs["value"] = append(recs["value"], &csharpTypedBinding{typ: t, scope: sp})
			localScopes[id]["value"] = append(localScopes[id]["value"], sp)
		}
	}
	for _, l := range locals {
		owner := localOwner(l)
		if owner == "" {
			continue
		}
		m := localScopes[owner]
		if m == nil {
			m = map[string][]csharpLocalScope{}
			localScopes[owner] = m
		}
		m[l.name] = append(m[l.name], csharpLocalScopeOf(l.defNode))
	}
	// The lvar capture sees only local_declaration_statement; C#'s other
	// binding forms (foreach, patterns, out var, lambda parameters,
	// parenthesized using) shadow a field exactly the same way.
	csharpCollectExtraBindingScopes(root, src, funcRanges, localScopes)

	for _, c := range calls {
		callerID := funcRanges.enclosingAt(c.line, c.offset)
		if callerID == "" {
			continue
		}
		if c.isMember {
			edge := &graph.Edge{
				From: callerID, To: "unresolved::*." + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			// The receiver's typed-local evidence is asked AT THE CALL'S
			// OFFSET: a typed or builtin local whose block has closed
			// before this site types nothing here (round-5 finding 2) —
			// the chain then falls through to the awaited/chain/spelling
			// branches exactly as if the local never existed. The
			// function-wide maps stay as the fallback for names the
			// records never saw.
			typedLocal, typedState := csharpTypedLocalAt(typedLocalsByOwner, callerID, c.receiver, c.offset)
			if c.recvType != "" {
				// this./base.-qualified: the receiver type came from the
				// enclosing declaration, not from any variable lookup.
				edge.Meta = map[string]any{"receiver_type": c.recvType}
			} else if typedState == csharpTypedFound && typedLocal.typ != "" {
				edge.Meta = map[string]any{"receiver_type": typedLocal.typ}
				if typedLocal.shape != "" && typedLocal.shape != typedLocal.typ {
					edge.Meta["receiver_shape"] = typedLocal.shape
				}
			} else if typedState == csharpTypedFound && typedLocal.builtin != "" {
				// Builtins stay out of receiver_type (the receiver-gate
				// passes key on user types); extension eligibility still
				// needs them — `n.Foo()` on an int must match
				// `Foo(this int)` and refuse `Foo(this string)`.
				edge.Meta = map[string]any{"receiver_builtin": typedLocal.builtin}
				if typedLocal.shape != "" && typedLocal.shape != typedLocal.builtin {
					edge.Meta["receiver_shape"] = typedLocal.shape
				}
			} else if recvType, ok := tenvByOwner[callerID][c.receiver]; ok && typedState == csharpTypedAbsent {
				edge.Meta = map[string]any{"receiver_type": recvType}
				if shape := shapesByOwner[callerID][c.receiver]; shape != "" && shape != recvType {
					edge.Meta["receiver_shape"] = shape
				}
			} else if bt := builtinsByOwner[callerID][c.receiver]; bt != "" && typedState == csharpTypedAbsent {
				edge.Meta = map[string]any{"receiver_builtin": bt}
				if shape := shapesByOwner[callerID][c.receiver]; shape != "" && shape != bt {
					edge.Meta["receiver_shape"] = shape
				}
			} else if inner := csharpAwaitedReceiver(c.receiver); inner != "" {
				// `(await LoadAsync()).X()` — the chain walker collapses a
				// fully-parenthesized receiver to nothing; the receiver is
				// the T inside the awaited call's Task<T>.
				env := csharpOffsetEnv(typedLocalsByOwner, tenvByOwner[callerID], callerID, csharpChainHead(inner), c.offset)
				if t := csharpAwaitedCallType(inner, csharpOwnerTypeName(callerID), env, result); t != "" {
					edge.Meta = map[string]any{"receiver_type": t}
				}
			} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
				env := csharpOffsetEnv(typedLocalsByOwner, tenvByOwner[callerID], callerID, csharpChainHead(c.receiver), c.offset)
				stampFactoryChainReceiver(edge, c.receiver, resolveChainType(c.receiver, env, result))
				if edge.Meta == nil && !strings.Contains(c.receiver, "(") {
					// A namespace-qualified receiver the chain walker could
					// not type (`Lib.BagExt.Add(bag)`). That is the same
					// static-form evidence as the bare spelling below —
					// without it the binder reads the call as extension
					// form, discounts a `this` slot the argument list never
					// filled, and lands on the wrong overload.
					edge.Meta = map[string]any{"receiver_name": c.receiver}
				}
			} else if c.receiver != "" &&
				!paramsByOwner[callerID][c.receiver] &&
				!localScopes.shadows(callerID, c.receiver, c.offset) {
				// A bare receiver nothing above could type, and no
				// parameter or local declares its name. Its spelling is
				// still evidence: a receiver that names a static class is
				// the STATIC form of an extension call (`BagExt.Add(bag)`)
				// — where the `this` slot is filled by the first argument,
				// not the receiver — and a same-named field of the
				// enclosing type is only bindable because nothing shadows
				// it. A parameter or local DOES shadow (re-review RED: a
				// shadowing parameter's call bound through the field it
				// shadows on the strength of an unrelated same-line
				// `this.`-qualified read); those receivers stay unknown.
				edge.Meta = map[string]any{"receiver_name": c.receiver}
			}
			// Stamped AFTER the receiver-evidence chain — every branch
			// above assigns a fresh Meta map and would clobber it. A
			// two-members-on-one-line tie is the same verdict from the
			// other side: there the RECEIVERS are attributable but the
			// CALLER is not, and the shadow refusal above consulted the
			// tie-break winner's parameter set — possibly the wrong
			// member's.
			if memberSiteAmbiguous[csharpCallSite{c.name, c.line}] || funcRanges.ambiguousAt(c.line) {
				if edge.Meta == nil {
					edge.Meta = map[string]any{}
				}
				edge.Meta["receiver_ambiguous"] = true
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

	// Field-identifier uses reuse the same shadow indexes the receiver
	// stamps consulted: every DECLARED local by name (typed or not — tenv
	// alone holds only the typed ones), parameters, and builtin-typed
	// locals.
	emitCSharpFieldIdentifierUses(calls, accesses, fieldAssigns, src,
		filePath, funcRanges, paramsByOwner, localScopes, builtinsByOwner, result)

	// .NET surfaces a symbol walk misses: DI registrations + COM
	// interop. Stamped onto the file node.
	detectDotNetSurfaces(src, result)

	// Same-file constant/variable value references → impact-radius reads.
	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)

	captureMediatRDispatch(result, root, filePath, src, funcBytes, funcLines)

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

// csharpOrTypeMeta ORs a boolean stamp onto an already emitted type
// node. Type node IDs carry no arity and no namespace, so declarations
// can collide and be dropped whole - but a REFUSAL signal that only one
// colliding declaration carries has to survive the collision. Union is
// the conservative merge: every consumer of these stamps can only
// widen a fan-out or refuse evidence on seeing one.
func csharpOrTypeMeta(result *parser.ExtractionResult, id, key string) {
	for i := len(result.Nodes) - 1; i >= 0; i-- {
		n := result.Nodes[i]
		if n == nil || n.ID != id {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta[key] = true
		return
	}
}

// emitContainer collapses the per-kind class/interface/struct/enum
// node emission. The capture-name prefix selects which capture set to
// read from (the legacy code repeated this body four times).
func (e *CSharpExtractor) emitContainer(m parser.QueryResult, kind string, nodeKind graph.NodeKind, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, localInterfaces, fileAliases map[string]bool, baseNameCounts map[string]map[string]int, partialSeen map[string]*csharpPartialIdentity) {
	// The declaration side lives in the same canonical identifier domain
	// as the base-list side: a verbatim-declared type (`@event` is the
	// only legal spelling of a keyword-named type) must mint the node ID
	// its base-list uses look up, or it is unreachable from every base
	// list in the file (round-6 finding B2).
	name := csharpCanonBaseIdent(m.Captures[kind+".name"].Text)
	def := m.Captures[kind+".def"]
	id := filePath + "::" + name
	if seen[id] {
		// A second declaration on an ID already taken: the arity pair
		// (ISource / ISource<out T>, Result / Result<T>), same-file
		// partial parts, nested same-named types, or two namespaces in
		// one file. The node is dropped, but two refusal signals must
		// not be dropped with it.
		//
		// Variance: the gate reads that stamp off whichever node
		// survives, and evaluating it behind this return meant a
		// bare-named sibling could delete a covariant family's only
		// protection.
		if kind == "iface" && csharpHasVariantTypeParams(def.Node) {
			csharpOrTypeMeta(result, id, "variant_type_params")
		}
		// The collision itself: a consumer that assembles evidence from
		// this ID (the field-receiver lookup in particular) cannot know
		// WHICH declaration's evidence survived, and no span heuristic
		// can prove it - a same-named type nested inside its twin puts
		// the dropped declaration's lines inside the survivor's span.
		// The positive signal closes every collision shape at once.
		csharpOrTypeMeta(result, id, "duplicate_decl")
		// A genuine same-file PARTIAL part carries real base facts: the
		// second declaration's interface paths must reach the graph even
		// though its node is dropped, or the surviving fragment's stamp
		// reads as the type's unique closure and the family fan-out
		// filters the whole type (round-5 finding 5). `partial` on both
		// declarations proves only a keyword; the merge additionally
		// requires the type-identity key to match - namespace, enclosing
		// type chain, and generic arity - because arity twins, namespace
		// twins, and nested twins can all be partial while being two
		// distinct types (round-6 finding B4). Any mismatch keeps the
		// old drop-the-bases behaviour. The shared extends budget rides
		// the record so two fragments cannot mint two base classes. The
		// cross-declaration baseNameCounts prescan already spans every
		// fragment, so a type closing one erased interface twice across
		// parts still stamps nothing.
		switch kind {
		case "class", "struct", "record", "iface":
			if pi := partialSeen[id]; pi != nil && csharpHasModifier(def.Node, src, "partial") &&
				pi.sameType(csharpPartialIdentityOf(def.Node, src)) {
				pi.extendsBase = emitCSharpBaseList(id, def.Node, src, filePath, localInterfaces, fileAliases, baseNameCounts, result, pi.extendsBase)
			}
		}
		return
	}
	seen[id] = true
	if csharpHasModifier(def.Node, src, "partial") {
		pi := csharpPartialIdentityOf(def.Node, src)
		partialSeen[id] = &pi
	}
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
		if csharpHasVariantTypeParams(def.Node) {
			meta["variant_type_params"] = true
		}
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
	if doc := extractCSharpDoc(src, def.Node); doc != "" {
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
		took := emitCSharpBaseList(id, def.Node, src, filePath, localInterfaces, fileAliases, baseNameCounts, result, "")
		if pi := partialSeen[id]; pi != nil {
			pi.extendsBase = took
		}
	case "enum":
		e.emitCSharpEnumMembers(def.Node, src, filePath, id, name, result, seen)
	}
	if kind == "record" {
		e.emitCSharpRecordPositionalProps(id, name, def.Node, src, filePath, fileID, result, seen, fileAliases)
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
func (e *CSharpExtractor) emitCSharpRecordPositionalProps(ownerID, ownerName string, decl *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool, fileAliases map[string]bool) {
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
	// One set for every positional property: the enclosing chain is the
	// same for all of them (the record's own type parameters included).
	unstampable := csharpUnstampableArgNames(decl, src, fileAliases)
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
			// Same closed-generic-arguments stamp ordinary fields and
			// properties carry (dispatch gate receiver evidence) — a
			// positional property is a first-class receiver.
			if args := csharpTypeArgsFromTypeNode(t, src, unstampable); args != "" {
				meta["field_type_args"] = args
			}
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
// extractCSharpDoc returns the doc comment directly above decl: XML doc
// lines first, then the /** */ or // fallback. Addressed by the node's
// start byte so the walk starts at the declaration instead of counting
// rows from the top of the file for every member.
func extractCSharpDoc(src []byte, decl *sitter.Node) string {
	if decl == nil {
		return ""
	}
	start := int(decl.StartByte())
	if d := ExtractDocAboveByte(src, start, DocLangCSharpXML); d != "" {
		return d
	}
	return ExtractDocAboveByte(src, start, DocLangBlockStar)
}

func (e *CSharpExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, ifaceMethods map[string][]string, funcBytes map[string][2]int) {
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
	if def.Node != nil {
		funcBytes[id] = [2]int{int(def.Node.StartByte()), int(def.Node.EndByte())}
	}
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
	if doc := extractCSharpDoc(src, def.Node); doc != "" {
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

func (e *CSharpExtractor) emitConstructor(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool, funcBytes map[string][2]int) {
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
	if def.Node != nil {
		funcBytes[id] = [2]int{int(def.Node.StartByte()), int(def.Node.EndByte())}
	}
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

func (e *CSharpExtractor) emitField(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool, fileAliases map[string]bool, funcBytes, funcLines map[string][2]int) {
	def := m.Captures["field.def"]
	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration", "interface_declaration", "record_declaration")
	if owner.kind == "" {
		return
	}
	nameCap := m.Captures["field.name"]
	name := nameCap.Text
	id := filePath + "::" + owner.name + "." + name
	if seen[id] {
		return
	}
	seen[id] = true
	// An initializer is executable code with no method around it, so the
	// declaring field owns its calls (round-23 catch AC1's field lane).
	// The extent is the DECLARATOR, not the whole declaration: on a
	// multi-declarator line (`int a = F(), b = G();`) each call must land
	// on the field it actually initializes. Initializer-less declarators
	// record nothing — they contain no code, and keeping them out of the
	// owner set avoids needless one-line ambiguity ties.
	if nameCap.Node != nil && !csharpHasModifier(def.Node, src, "const") {
		if decl := nameCap.Node.Parent(); decl != nil && decl.Type() == "variable_declarator" {
			// Grammar revisions disagree on the wrapper (equals_value_clause
			// vs the expression hanging directly under the declarator — the
			// await-tier walk above tolerates both), so "has an initializer"
			// is: any named child besides the name and a fixed-size-buffer
			// bracketed_argument_list.
			for i := 0; i < int(decl.NamedChildCount()); i++ {
				c := decl.NamedChild(i)
				if c == nil || c.StartByte() == nameCap.Node.StartByte() ||
					c.Type() == "bracketed_argument_list" {
					continue
				}
				funcBytes[id] = [2]int{int(decl.StartByte()), int(decl.EndByte())}
				funcLines[id] = [2]int{int(decl.StartPoint().Row) + 1, int(decl.EndPoint().Row) + 1}
				break
			}
		}
	}
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
	// Every field carries scope_ns for the resolver's scoped-usings
	// narrowing, not only the initialized ones that are call owners: a
	// field's TYPE reference needs the same namespace narrowing whether
	// or not its declarator authored a call.
	if ns := csharpEnclosingNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	if isIface {
		meta["iface_member"] = true
	}
	// A field_declaration's type lives on its nested variable_declaration
	// (`field_declaration → variable_declaration[type] → variable_declarator`),
	// not as a direct `type` field of the field_declaration itself.
	fieldTypeNode := csharpFieldDeclTypeNode(def.Node)
	fieldTypeRaw := ""
	if fieldTypeNode != nil {
		fieldTypeRaw = strings.TrimSpace(fieldTypeNode.Content(src))
	}
	if fieldTypeRaw != "" {
		meta["field_type"] = fieldTypeRaw
		// Closed generic arguments of the declared type — the dispatch
		// gate's receiver evidence (see csharp_base_type_args.go).
		if args := csharpTypeArgsFromTypeNode(fieldTypeNode, src, csharpUnstampableArgNames(def.Node, src, fileAliases)); args != "" {
			meta["field_type_args"] = args
		}
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
	if doc := extractCSharpDoc(src, def.Node); doc != "" {
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

// csharpValueProp records a set/init-carrying property so the deferred
// typing pass can seed the accessor's implicit `value` parameter as an
// offset-scoped record over each accessor's own byte extent.
type csharpValueProp struct {
	typ   string // raw declared property type
	spans []csharpLocalScope
}

// csharpPropertyHasExecutableBody reports whether the fragment carries
// any accessor block or expression body - the discriminator between a
// C# 13 partial property's declaring part (`{ get; set; }`) and its
// implementing part. The walk is coarse: an auto-property with a
// block-lambda INITIALIZER (`public Func<int> F { get; } = () => { … };`)
// would read as body-bearing because the lambda's block matches. That
// misread is unreachable today - this predicate runs only on the
// duplicate-declaration path, and valid C# does not permit two
// same-name property declarations where one is such an auto-property -
// so it is documented rather than special-cased.
func csharpPropertyHasExecutableBody(def *sitter.Node) bool {
	found := false
	walkNodes(def, func(n *sitter.Node) {
		if found {
			return
		}
		switch n.Type() {
		case "block", "arrow_expression_clause":
			found = true
		}
	})
	return found
}

// csharpRecordPropertyOwnership records one fragment's declaration span
// as the property's call-ownership evidence: byte extents, the line
// span (the minted node keeps the FIRST fragment's lines, so a partial
// implementation needs its own), and the set/init spans for the value
// seed. Re-invoked by the duplicate-declaration path when the
// body-bearing fragment of a partial property extracts second.
func csharpRecordPropertyOwnership(id string, node *sitter.Node, startLine, endLine int, src []byte, funcBytes, funcLines map[string][2]int, valueProps map[string]csharpValueProp) {
	if node == nil {
		return
	}
	funcBytes[id] = [2]int{int(node.StartByte()), int(node.EndByte())}
	funcLines[id] = [2]int{startLine + 1, endLine + 1}
	delete(valueProps, id)
	if t := node.ChildByFieldName("type"); t != nil {
		if raw := strings.TrimSpace(t.Content(src)); raw != "" {
			if spans := csharpSetInitAccessorSpans(node); len(spans) > 0 {
				valueProps[id] = csharpValueProp{typ: raw, spans: spans}
			}
		}
	}
}

// csharpSetInitAccessorSpans returns the byte extents of the property's
// set and init accessor declarations - the spans where `value` IS the
// implicit parameter (a same-named member is only reachable there via
// this.value, so a seed scoped to these spans can never collide with
// member evidence). The keyword is an anonymous child of the
// accessor_declaration, so every child's Type is scanned, not just the
// named ones.
func csharpSetInitAccessorSpans(def *sitter.Node) []csharpLocalScope {
	var spans []csharpLocalScope
	walkNodes(def, func(n *sitter.Node) {
		if n.Type() != "accessor_declaration" {
			return
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			if c := n.Child(i); c != nil && (c.Type() == "set" || c.Type() == "init") {
				spans = append(spans, csharpLocalScope{start: int(n.StartByte()), end: int(n.EndByte())})
				return
			}
		}
	})
	return spans
}

func (e *CSharpExtractor) emitProperty(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool, fileAliases map[string]bool, funcBytes, funcLines map[string][2]int, valueProps map[string]csharpValueProp) {
	def := m.Captures["prop.def"]
	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration", "interface_declaration", "record_declaration")
	if owner.kind == "" {
		return
	}
	name := m.Captures["prop.name"].Text
	id := filePath + "::" + owner.name + "." + name
	if seen[id] {
		// A same-file duplicate declaration mints no second node — but a
		// C# 13 partial property splits declaration and implementation
		// across fragments, and either order may extract first. The
		// body-bearing fragment must own the extents and the value
		// spans, or every accessor call in the implementation dies
		// ownerless (the AC1 drop, one level up).
		if def.Node != nil && csharpPropertyHasExecutableBody(def.Node) {
			csharpRecordPropertyOwnership(id, def.Node, def.StartLine, def.EndLine, src, funcBytes, funcLines, valueProps)
		}
		return
	}
	seen[id] = true
	// Accessor bodies, an expression body, and an initializer all live
	// inside the declaration span — recording it makes the property a
	// call owner (round-23 catch AC1: without an owner the funcRanges
	// gate dropped every accessor-body call outright).
	csharpRecordPropertyOwnership(id, def.Node, def.StartLine, def.EndLine, src, funcBytes, funcLines, valueProps)
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
	// Properties are call owners; the resolver's scoped-usings narrowing
	// reads the caller's scope_ns, so they carry it like methods do.
	if ns := csharpEnclosingNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	if isIface {
		meta["iface_member"] = true
	}
	var propTypeRaw string
	if t := def.Node.ChildByFieldName("type"); t != nil {
		propTypeRaw = strings.TrimSpace(t.Content(src))
		meta["field_type"] = propTypeRaw
		// Same closed-generic-arguments stamp fields carry (dispatch
		// gate receiver evidence — csharp_base_type_args.go).
		if args := csharpTypeArgsFromTypeNode(t, src, csharpUnstampableArgNames(def.Node, src, fileAliases)); args != "" {
			meta["field_type_args"] = args
		}
	}
	if doc := extractCSharpDoc(src, def.Node); doc != "" {
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

// csharpIndexerName is the node name an indexer carries. The grammar
// gives it none (`this` is a keyword), and the CLR metadata name `Item`
// is unsafe here: [IndexerName] lets a type spell the indexer one way
// and still declare an unrelated member called Item, so borrowing it
// would fuse two distinct members onto one node. `this[]` cannot collide
// with any legal C# identifier.
const csharpIndexerName = "this[]"

// emitAccessorMember mints the member node for the accessor-bearing
// kinds that carry executable code but had no node at all: indexers and
// events declared with add/remove bodies. Without a node the owner
// lookup could not name the member holding an accessor body, so the
// funcRanges gate dropped every call inside it - the exact loss
// properties took before #720 recorded their extents (probe cells
// AC19-AC21).
//
// They ride KindField with a `kind` stamp, the same shape properties
// use, because csharpOwnerRanges already widens the call-owner set with
// extent-carrying field-kind members. That keeps a real recall fix from
// introducing a new node kind into federation, the wire formats, both
// store backends and every consumer filter at the same time.
//
// Attribution, not just existence, is the point: an indexer sharing a
// physical line with a property used to have its body call parked on
// the property by the extractor's line fallback, and the semantic
// tier's caller adoption then promoted that stub to a confident
// resolved edge on a member that cannot call anything (issue #728).
func (e *CSharpExtractor) emitAccessorMember(def *parser.CapturedNode, name, memberKind, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool, fileAliases map[string]bool, funcBytes, funcLines map[string][2]int, valueProps map[string]csharpValueProp) {
	if def == nil || def.Node == nil || name == "" {
		return
	}
	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration", "interface_declaration", "record_declaration")
	if owner.kind == "" {
		return
	}
	id := filePath + "::" + owner.name + "." + name
	if seen[id] {
		// A C# 13 partial indexer (C# 14: partial event) splits a single
		// member across a declaring fragment and an implementing one, and
		// either may extract first. That is NOT an overload, and minting a
		// second node would leave the name-keyed ID - the one anything
		// queries - pointing at the bodyless fragment while the code sat
		// under a line-suffixed twin. Properties merge these onto the one
		// node by moving the ownership record to the body-bearing
		// fragment; the same rule applies here. The `partial` modifier is
		// the discriminator rather than a body test: C# requires BOTH
		// fragments to spell it, and no overload may, so it separates the
		// two cases exactly.
		if csharpHasModifier(def.Node, src, "partial") {
			if csharpPropertyHasExecutableBody(def.Node) {
				csharpRecordPropertyOwnership(id, def.Node, def.StartLine, def.EndLine, src, funcBytes, funcLines, valueProps)
			}
			return
		}
		// A genuine overload: two indexers differing only by parameter
		// list, both body-bearing. Overloaded METHODS already resolve the
		// ID collision by suffixing the second declaration with its line;
		// taking the same route keeps one convention instead of two. A
		// THIRD declaration on the same line collides again and is
		// dropped, exactly as a third same-line method overload is.
		id = id + "_L" + fmt.Sprint(def.StartLine+1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	// Accessor bodies and an expression body both live inside the
	// declaration span, so recording it makes the member a call owner.
	// An indexer's set accessor carries the implicit `value` parameter
	// exactly as a property's does and rides the same seed; an event's
	// add/remove bind `value` too, but csharpSetInitAccessorSpans scans
	// for set/init only, so events deliberately get no seed here.
	csharpRecordPropertyOwnership(id, def.Node, def.StartLine, def.EndLine, src, funcBytes, funcLines, valueProps)
	isIface := owner.kind == "interface_declaration"
	defaultVis := VisibilityPrivate
	if isIface {
		defaultVis = VisibilityPublic
	}
	meta := map[string]any{
		"receiver":   owner.name,
		"visibility": csharpVisibility(def.Node, src, defaultVis),
		"kind":       memberKind,
	}
	// Call owners carry scope_ns: the resolver's scoped-usings narrowing
	// reads it off the caller.
	if ns := csharpEnclosingNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	if isIface {
		meta["iface_member"] = true
	}
	var typeRaw string
	if t := def.Node.ChildByFieldName("type"); t != nil {
		typeRaw = strings.TrimSpace(t.Content(src))
		meta["field_type"] = typeRaw
		if args := csharpTypeArgsFromTypeNode(t, src, csharpUnstampableArgNames(def.Node, src, fileAliases)); args != "" {
			meta["field_type_args"] = args
		}
	}
	if doc := extractCSharpDoc(src, def.Node); doc != "" {
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
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: filePath + "::" + owner.name, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitCSharpTypeUseEdges(id, typeRaw, filePath, def.StartLine+1, result)
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
	var usings, globals, statics, scoped, globalStatics, globalAliases []string
	seen := map[string]bool{}
	seenStatic := map[string]bool{}
	seenScoped := map[string]bool{}
	seenGlobalAlias := map[string]bool{}
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
				// An alias grants no bare-name namespace visibility, but a
				// GLOBAL alias makes its identifier project-scoped and
				// opaque to string-compared type-argument stamps in every
				// OTHER file — record the name so the dispatch gate can
				// refuse stamps that spell it.
				if isGlobal {
					if a := csharpUsingAliasName(n, src); a != "" && !seenGlobalAlias[a] {
						seenGlobalAlias[a] = true
						globalAliases = append(globalAliases, a)
					}
				}
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
	if len(usings) == 0 && len(statics) == 0 && len(globalAliases) == 0 {
		return
	}
	if fileNode.Meta == nil {
		fileNode.Meta = map[string]any{}
	}
	if len(globalAliases) > 0 {
		fileNode.Meta["global_using_aliases"] = globalAliases
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
	// bytes carries each function's BYTE extent (keyed by node ID) when
	// the extractor recorded one. Line ranges cannot separate two members
	// declared on one physical line; byte intervals can, so consumers
	// with a real coordinate attribute through enclosingAt.
	bytes map[string][2]int
}

// csharpOwnerRanges widens the method/constructor owner set with every
// member that recorded byte extents but is not a KindFunction/KindMethod
// node — properties, indexers and accessor-bearing events today, any
// extent-carrying member kind tomorrow. A
// member that can hold executable code must be able to OWN the calls in
// it: the funcRanges gate drops a call whose enclosing member it cannot
// name, which is how every accessor-body call vanished (round-23 catch
// AC1 — 3 of the probe cell's 4 sites, the expression-bodied method
// being the lone survivor).
func csharpOwnerRanges(result *parser.ExtractionResult, funcBytes, funcLines map[string][2]int) []funcRange {
	ranges := buildFuncRanges(result)
	for _, n := range result.Nodes {
		if n.Kind != graph.KindField {
			continue
		}
		if _, ok := funcBytes[n.ID]; !ok {
			continue
		}
		start, end := n.StartLine, n.EndLine
		// The recorded line span wins over the node's: a partial
		// property's node keeps the declaring fragment's lines while
		// the code lives in the implementing fragment, and a field's
		// extent is its declarator.
		if ln, ok := funcLines[n.ID]; ok {
			start, end = ln[0], ln[1]
		}
		ranges = append(ranges, funcRange{id: n.ID, startLine: start, endLine: end})
	}
	return ranges
}

// csharpStampOwnershipSpans carries the recorded ownership span onto the
// member node whenever the node's own lines do not contain it. A node is
// minted once per id, so a C# 13 partial property extracted declaring
// fragment first (or a property declared in both arms of an #if / #else)
// keeps the first fragment's lines while csharpOwnerRanges owns its
// calls by the body-bearing fragment's span - and every stub the
// extractor parks there sits outside the node. A consumer that tests a
// call's line against the owner's extent (the semantic tier's adoption
// guard, issue #731) needs the span the extractor actually attributed
// by; the stamp is that span, the same 1-based lines csharpOwnerRanges
// reads. Two cases leave a node unstamped, deliberately alike in effect:
// no span was recorded at all (a field without an initializer, a member
// kind that records none), and a recorded span the node's own lines
// already contain (the ordinary property, the implementing-first order,
// a field's declarator inside its declaration). Either way the node's
// lines answer and its meta stays as before. Kept as its own walk rather
// than folded into csharpOwnerRanges so the owner lookup stays a pure
// read of the result.
func csharpStampOwnershipSpans(result *parser.ExtractionResult, funcLines map[string][2]int) {
	for _, n := range result.Nodes {
		if n.Kind != graph.KindField {
			continue
		}
		ln, ok := funcLines[n.ID]
		if !ok || (ln[0] >= n.StartLine && ln[1] <= n.EndLine) {
			continue
		}
		if n.Meta == nil {
			n.Meta = make(map[string]any)
		}
		n.Meta[graph.MetaOwnershipStartLine] = ln[0]
		n.Meta[graph.MetaOwnershipEndLine] = ln[1]
	}
}

func newCSharpFuncLookup(ranges []funcRange, bytes map[string][2]int) *csharpFuncLookup {
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
	return &csharpFuncLookup{ranges: sorted, maxEnd: maxEnd, ord: ord, bytes: bytes}
}

// enclosingAt returns the function whose BYTE extent contains offset,
// innermost byte-span first. Line-keyed attribution hands a call to the
// smaller LINE span when two members with unequal spans share a physical
// line - `public int B(){return 0;} public T A(...) {` - and
// ambiguousAt cannot refuse there (it requires EQUAL spans), so B
// carried A's call and every consumer of the attribution read the wrong
// member's evidence (round-5 finding 4). Falls back to the line answer
// whenever no recorded byte extent contains the offset: extents are
// recorded for methods, constructors, properties, indexers, events and
// initialized field declarators, so a member kind still without them
// (operator, conversion operator, destructor) sharing a line with one
// that has them would otherwise lose its call outright rather than
// degrade to line attribution (round-6 finding B3) - unless the line
// answer's own recorded bytes exclude the offset, in which case the
// fallback is refused (see below).
func (l *csharpFuncLookup) enclosingAt(line, offset int) string {
	if offset < 0 || len(l.bytes) == 0 {
		return l.enclosing(line)
	}
	i := sort.Search(len(l.ranges), func(j int) bool { return l.ranges[j].startLine > line }) - 1
	best := ""
	bestSpan := math.MaxInt
	bestOrd := math.MaxInt
	for ; i >= 0; i-- {
		if l.maxEnd[i] < line {
			break
		}
		r := l.ranges[i]
		if line > r.endLine {
			continue
		}
		b, ok := l.bytes[r.id]
		if !ok {
			continue
		}
		if offset < b[0] || offset >= b[1] {
			continue
		}
		if span := b[1] - b[0]; span < bestSpan || (span == bestSpan && l.ord[i] < bestOrd) {
			best, bestSpan, bestOrd = r.id, span, l.ord[i]
		}
	}
	if best != "" {
		return best
	}
	// The line fallback exists for member kinds that record NO extents
	// at all (operator, conversion operator, destructor). If the line
	// answer does have
	// recorded bytes and the offset sits outside them, the offset is
	// provably not inside that member, so handing it the call invents a
	// false edge on a same-line neighbour - strictly worse than the drop
	// it replaced.
	fb := l.enclosing(line)
	if b, ok := l.bytes[fb]; ok && (offset < b[0] || offset >= b[1]) {
		return ""
	}
	return fb
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

// ambiguousAt reports whether two different functions tie for the
// innermost range covering line - two members declared on one source
// line. enclosing() breaks that tie by extraction order, which is
// deterministic but arbitrary: a line-keyed attribution cannot say
// which member owns a call there, so evidence keyed on the attribution
// (the shadow refusal consulting the attributed member's parameter set
// in particular) must refuse at such a line. Nested shapes - a local
// function inside a method - are ties of COVERAGE, not of span, and
// stay unambiguous: the innermost is genuinely the owner.
func (l *csharpFuncLookup) ambiguousAt(line int) bool {
	i := sort.Search(len(l.ranges), func(j int) bool { return l.ranges[j].startLine > line }) - 1
	bestSpan := math.MaxInt
	ties := 0
	for ; i >= 0; i-- {
		if l.maxEnd[i] < line {
			break
		}
		if r := l.ranges[i]; line <= r.endLine {
			switch span := r.endLine - r.startLine; {
			case span < bestSpan:
				bestSpan, ties = span, 1
			case span == bestSpan:
				ties++
			}
		}
	}
	return ties > 1
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
			// Same canonical domain as the node ID the owner mints
			// (round-6 finding B2): members of a verbatim-declared type
			// must hang off the canonical owner or the type resolves
			// while its member fan-out stays empty.
			return csharpOwner{kind: gtype, name: csharpCanonBaseIdent(nameNode.Content(src))}
		}
	}
	return csharpOwner{}
}

// collectCSharpInterfaceNames walks the tree for every
// interface_declaration and records its bare name. The base-list
// heuristic consults this set first: a base type that names a
// locally-declared interface is unambiguously an interface, regardless
// of whether its name follows the `I`-prefix convention.
// csharpBaseNameCounts counts, per type node ID, how many base-list
// entries across EVERY declaration of that type in the file name the
// same erased base.
//
// A type node ID carries neither arity nor namespace, so same-file
// partial parts and arity twins share one ID. Each part's own base list
// reads as unambiguous while the type AS A WHOLE closes one interface
// twice, and only the first declaration reaches the graph - so a stamp
// taken from it describes one closure and is then applied to members
// implementing the other. Counting across declarations is what makes
// that ambiguity visible at the one place still able to refuse it.
//
// Walking base_list nodes and reading the parent's name avoids
// enumerating declaration node types, which differ across grammar
// revisions.
//
// A base entry that names a using alias is recorded under the alias
// sentinel rather than its own spelling. An alias is an opaque spelling
// of some type - possibly a construction of the very interface a
// sibling entry closes - so a base list containing one can never prove
// its target unique, and the stamp site refuses the whole type. The
// sentinel key contains a NUL so no real base name can collide with it.
const csharpAliasBaseSentinel = "\x00alias-base"

func csharpBaseNameCounts(root *sitter.Node, src []byte, filePath string, fileAliases map[string]bool) map[string]map[string]int {
	counts := map[string]map[string]int{}
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "base_list" {
			return
		}
		decl := n.Parent()
		if decl == nil {
			return
		}
		nameNode := decl.ChildByFieldName("name")
		if nameNode == nil {
			return
		}
		id := filePath + "::" + csharpCanonBaseIdent(nameNode.Content(src))
		m := counts[id]
		if m == nil {
			m = map[string]int{}
			counts[id] = m
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			entry := n.NamedChild(i)
			if entry == nil {
				continue
			}
			if name, _ := csharpBaseTypeName(entry, src); name != "" {
				if fileAliases[name] {
					m[csharpAliasBaseSentinel]++
					continue
				}
				m[name]++
			}
		}
	})
	return counts
}

func collectCSharpInterfaceNames(root *sitter.Node, src []byte) map[string]bool {
	names := make(map[string]bool)
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "interface_declaration" {
			return
		}
		if nameNode := n.ChildByFieldName("name"); nameNode != nil {
			// Canonical, like the base-list lookups that consult this
			// set: a verbatim-declared interface must still classify its
			// implementors' edges as implements (round-6 finding B2).
			names[csharpCanonBaseIdent(nameNode.Content(src))] = true
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
// csharpPartialIdentity records, for the first partial declaration on a
// node ID, the type-identity key later same-ID fragments must match
// before their base lists merge - `partial` alone proves a keyword, not
// an identity (round-6 finding B4) - and whether a fragment has already
// minted the type's single base class, so the merge cannot mint a
// second one (a type node ID carries neither namespace nor arity, so
// arity twins, namespace twins, and nested twins all share an ID with
// a genuinely-partial type's fragments).
type csharpPartialIdentity struct {
	ns         string
	outerChain string
	arity      int
	// extendsBase is the canonical name of the base CLASS an earlier
	// fragment's extends budget was spent on ("" = unspent). It must be
	// the target, not a boolean: C# permits every partial part to
	// repeat the base class, and a repeat of the SAME base is dropped
	// while only a genuinely different class entry degrades to
	// implements.
	extendsBase string
}

func csharpPartialIdentityOf(decl *sitter.Node, src []byte) csharpPartialIdentity {
	return csharpPartialIdentity{
		ns:         csharpCanonDottedName(csharpEnclosingNamespace(decl, src)),
		outerChain: csharpEnclosingTypeChain(decl, src),
		arity:      csharpTypeParamArity(decl),
	}
}

// csharpCanonDottedName canonicalizes each segment of a dotted name so
// the identity key compares identifiers, not spellings.
func csharpCanonDottedName(s string) string {
	if s == "" || !strings.ContainsAny(s, "@\\") {
		return s
	}
	parts := strings.Split(s, ".")
	for i := range parts {
		parts[i] = csharpCanonBaseIdent(strings.TrimSpace(parts[i]))
	}
	return strings.Join(parts, ".")
}

// sameType reports whether a later fragment's identity key matches -
// extendsBase is bookkeeping, not identity, so it stays out.
func (p csharpPartialIdentity) sameType(o csharpPartialIdentity) bool {
	return p.ns == o.ns && p.outerChain == o.outerChain && p.arity == o.arity
}

// csharpEnclosingTypeChain joins the names of every enclosing type
// declaration, innermost first. Only equality is consumed, so the
// direction just needs to be deterministic.
func csharpEnclosingTypeChain(decl *sitter.Node, src []byte) string {
	var parts []string
	for n := decl.Parent(); n != nil; n = n.Parent() {
		switch n.Type() {
		case "class_declaration", "struct_declaration", "record_declaration", "interface_declaration":
			if nm := n.ChildByFieldName("name"); nm != nil {
				parts = append(parts, csharpCanonBaseIdent(nm.Content(src)))
			}
		}
	}
	return strings.Join(parts, ".")
}

// csharpTypeParamArity counts the declaration's type parameters (0 for
// a non-generic type). `type_parameters` is not a named field in every
// grammar revision - same fallback scan as the method-level lookup.
func csharpTypeParamArity(decl *sitter.Node) int {
	tp := decl.ChildByFieldName("type_parameters")
	if tp == nil {
		for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
			if c := decl.Child(i); c != nil && c.Type() == "type_parameter_list" {
				tp = c
				break
			}
		}
	}
	if tp == nil {
		return 0
	}
	return int(tp.NamedChildCount())
}

// emitCSharpBaseList emits the declaration's base-list edges. It
// receives which base class an earlier fragment of the same type
// already minted ("" = none) and reports the state back, so partial
// fragments share ONE extends budget the way entries within one base
// list always have - and a later fragment legally REPEATING that same
// base class emits nothing for it, instead of a demoted implements.
func emitCSharpBaseList(typeID string, decl *sitter.Node, src []byte, filePath string, localInterfaces, fileAliases map[string]bool, baseNameCounts map[string]map[string]int, result *parser.ExtractionResult, extendsBaseAlready string) string {
	if decl == nil {
		return extendsBaseAlready
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
		return extendsBaseAlready
	}
	// Structs and `record struct` cannot derive from a base class — the
	// CLR forbids it — so every entry in their base list is an interface
	// and the "first non-interface is the superclass" branch never runs.
	// An interface's bases can only be interfaces, and the relation is
	// inheritance — every entry rides EdgeExtends (the same convention
	// the semantic engine applies), bypassing the discrimination below.
	ifaceDecl := decl.Type() == "interface_declaration"
	allowsBaseClass := csharpDeclAllowsBaseClass(decl)
	// Names a base argument must never be compared by: type parameters of
	// the declaring type AND every enclosing type (Relay<T> : IBoxStore<T>,
	// or a type nested inside a generic outer), plus in-scope using
	// aliases (opaque spellings).
	declTypeParams := csharpUnstampableArgNames(decl, src, fileAliases)
	// A type closing the SAME erased target twice
	// (Both : IBoxStore<Crate>, IBoxStore<Widget>) collapses to one stored
	// edge — identical (from, to, kind, file, line) — so a stamp would
	// arbitrarily keep one closure and suppress the other's implementors
	// downstream. A repeated target stamps nothing.
	//
	// The count spans every declaration sharing this type's node ID, not
	// just this base list: same-file partial parts and arity twins each
	// look unambiguous alone while the type as a whole is not, and the
	// part that loses the ID race never reaches the graph to contradict
	// the winner's stamp. An absent entry counts as 0 and stamps nothing,
	// so a shape the prescan cannot attribute keeps the full fan-out.
	baseNameCount := baseNameCounts[typeID]
	extendsBase := extendsBaseAlready
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
		// C# permits every partial part to repeat the base class the
		// budget already spent — the repeat names the same base, so it
		// emits nothing rather than a demoted implements.
		if !ifaceDecl && !isInterface && name == extendsBase {
			continue
		}
		kind := graph.EdgeImplements
		switch {
		case ifaceDecl:
			kind = graph.EdgeExtends
		case !isInterface && allowsBaseClass && extendsBase == "":
			kind = graph.EdgeExtends
			extendsBase = name
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
		// Closed generic arguments ride the edge so the dispatch fan-out
		// can exclude type-impossible implementors — see the package doc
		// in csharp_base_type_args.go for the conservative rules. A base
		// list that spells any entry as a using alias stamps nothing at
		// all: the alias is an opaque spelling that may construct the
		// same interface a sibling entry closes, so no entry's target
		// can be proven unique.
		if baseNameCount[csharpAliasBaseSentinel] == 0 && baseNameCount[name] == 1 {
			if args := csharpBaseTypeArgs(entry, src, declTypeParams); args != "" {
				if edge.Meta == nil {
					edge.Meta = map[string]any{}
				}
				edge.Meta["target_type_args"] = args
			}
		}
		result.Edges = append(result.Edges, edge)
	}
	return extendsBase
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

// csharpCanonBaseIdent reduces one base-entry identifier SPELLING to the
// identifier it denotes: the verbatim `@` prefix drops and unicode
// escapes decode, so `@IRack` and the escaped spelling compare equal to
// IRack everywhere the name is used — the alias-sentinel lookup, the
// duplicate count, the I-prefix interface test, and the
// unresolved-target name (round-5 finding 7: the raw spelling bypassed
// all four). A malformed escape keeps the raw spelling, which every
// consumer treats conservatively.
func csharpCanonBaseIdent(s string) string {
	if c := csharpCanonicalIdentifier(s); c != "" {
		return c
	}
	return s
}

// csharpBaseTypeName extracts the bare type name from a single base_list
// entry, stripping generic arguments and namespace qualification so the
// `I`-prefix test sees IList rather than IList<int> or System.IList. The
// bool return reports whether the entry is a primary_constructor_base_type
// (`Base(args)`), which can only ever be a base class.
func csharpBaseTypeName(entry *sitter.Node, src []byte) (string, bool) {
	switch entry.Type() {
	case "identifier":
		return csharpCanonBaseIdent(entry.Content(src)), false
	case "generic_name":
		// First child is the base identifier; the type_argument_list
		// follows. IList<int> → IList.
		if id := entry.ChildByFieldName("name"); id != nil {
			return csharpCanonBaseIdent(id.Content(src)), false
		}
		for i, _nc := 0, int(entry.ChildCount()); i < _nc; i++ {
			if c := entry.Child(i); c != nil && c.Type() == "identifier" {
				return csharpCanonBaseIdent(c.Content(src)), false
			}
		}
	case "qualified_name":
		// System.Object → Object, App.IBox<Crate> → IBox. The final
		// segment is not always an identifier: a constructed generic
		// spells it `generic_name`, and a nested qualification spells it
		// `qualified_name`. Scanning only direct identifier children
		// walked past those and returned the PENULTIMATE segment — the
		// namespace — which then disagreed with the type-argument
		// extractor about what the entry even names.
		if name := entry.ChildByFieldName("name"); name != nil {
			if n, _ := csharpBaseTypeName(name, src); n != "" {
				return n, false
			}
		}
		var last string
		for i, _nc := 0, int(entry.ChildCount()); i < _nc; i++ {
			if c := entry.Child(i); c != nil && c.Type() == "identifier" {
				last = c.Content(src)
			}
		}
		return csharpCanonBaseIdent(last), false
	case "primary_constructor_base_type":
		// `: Base(args)` — record base-constructor call; always a class.
		if id := entry.ChildByFieldName("type"); id != nil {
			return csharpCanonBaseIdent(normalizeCSharpBaseName(id.Content(src))), true
		}
		for i, _nc := 0, int(entry.ChildCount()); i < _nc; i++ {
			c := entry.Child(i)
			if c == nil {
				continue
			}
			if c.Type() == "identifier" || c.Type() == "generic_name" || c.Type() == "qualified_name" {
				return csharpCanonBaseIdent(normalizeCSharpBaseName(c.Content(src))), true
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

// csharpFieldDeclTypeNode returns the declared type NODE of a
// field_declaration — the type-argument stamp derives its arguments from
// the parsed tree, not from raw text, so trivia between tokens stays out
// of the identity. The type is a field of the nested variable_declaration
// node, not of the field_declaration itself, so a direct
// ChildByFieldName("type") on the field_declaration is always nil.
func csharpFieldDeclTypeNode(fieldDecl *sitter.Node) *sitter.Node {
	if fieldDecl == nil {
		return nil
	}
	for i, _nc := 0, int(fieldDecl.NamedChildCount()); i < _nc; i++ {
		c := fieldDecl.NamedChild(i)
		if c == nil || c.Type() != "variable_declaration" {
			continue
		}
		if t := c.ChildByFieldName("type"); t != nil {
			return t
		}
		// Fallback: first named child of the variable_declaration is the
		// type in grammar revisions that don't tag the field.
		if c.NamedChildCount() > 0 {
			if first := c.NamedChild(0); first != nil && first.Type() != "variable_declarator" {
				return first
			}
		}
	}
	return nil
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
