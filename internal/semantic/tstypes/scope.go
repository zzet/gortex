package tstypes

import (
	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// fileFacts is the pure-syntax output of one file's binder walk. It
// carries no graph references so the parse/walk phase can run on
// worker goroutines while the apply phase owns every store interaction.
type fileFacts struct {
	file       string // graph FilePath of the analyzed file
	repoPrefix string
	imports    []Import
	calls      []callFact
	supers     []superFact
	metas      []metaFact
	aliases    []aliasFact
}

// callFact is one receiver-qualified call site with whatever receiver
// evidence the binder could ground. Exactly one of recvType /
// recvPendingCallee / recvIdent is usually set; the apply phase
// resolves them in that priority order and skips the call when none
// lands on a verified graph type node.
type callFact struct {
	line   int // 1-based call line
	method string
	// recvType is the receiver's bound type name (annotation,
	// constructor inference, or propagation through locals).
	recvType string
	// recvPendingCallee is set when the receiver local was initialised
	// from a bare call (`u = build_user()`), or the receiver is itself a
	// bare free-function call standing in receiver position
	// (`listOf<Foo>().x()`, `collect()->y()`); the apply phase resolves the
	// callee's graph return_type, falling back to the stdlib seed table.
	recvPendingCallee string
	// recvCallTypeArg carries the explicit generic type ARGUMENT of a bare
	// free-function call receiver (the `Foo` in `listOf<Foo>()`), when the
	// grammar exposes one. It lets the apply phase element-type a stdlib
	// collection access (`mutableListOf<Foo>().first()` -> Foo). "" when the
	// call site carried no type argument.
	recvCallTypeArg string
	// recvIdent is set when the receiver is an identifier with no
	// binding in scope — a type-qualified (static) call candidate. Only
	// used when it resolves to a real graph type node.
	recvIdent string
	// recvChain is set when the receiver is itself a method call
	// (`a.step().done()`): it carries the inner call's receiver evidence
	// (recvType / recvPendingCallee / recvIdent / recvChain) plus the
	// inner method name. The apply phase resolves the inner method's
	// return type — applying the fluent self / trait rewrite — to type
	// the outer call's receiver, and grades the outer edge as inferred.
	recvChain *callFact
	// inferred is set when recvType was bound by a guard narrowing
	// (`if ($x instanceof Foo)`) rather than a direct annotation /
	// constructor / propagation. The apply phase grades the resulting call
	// edge at the inferred confidence band, honestly weaker than a direct
	// resolution.
	inferred bool
	// argCount is the number of argument expressions at the call site, and
	// argKnown reports whether it was determined. It is filled only for the
	// direct receiver-qualified call path, on languages that provide the
	// CallArgCount hook. When argKnown is false the apply phase treats the
	// call's arity as unknown and never narrows an overload set by it.
	argCount int
	argKnown bool
	// owner is the member declaration that authored this call site, spelled
	// as the extractor names that member's node (LangSpec.MemberDeclName);
	// "" when the spec names no members. The apply phase reads it for one
	// purpose: breaking a same-line same-name stub tie in favour of the
	// fact's own author.
	owner string
}

// arity returns the call site's argument count, or -1 when it was not
// determined — the sentinel the apply phase reads to leave an overload
// set un-narrowed.
func (cf callFact) arity() int {
	if cf.argKnown {
		return cf.argCount
	}
	return -1
}

// superFact is one declared supertype relation, pending graph
// resolution of both endpoints.
type superFact struct {
	typeName  string
	superName string
	kind      graph.EdgeKind // empty: decide by resolved target kind
	line      int
}

// metaFact is one Node.Meta fill: stamp key=value on the symbol node
// matched by (owner, name) or by declaration line.
type metaFact struct {
	key   string
	value string
	owner string // receiver type for field stamps; "" for line-matched
	name  string // field name; "" for line-matched
	line  int    // declaration line for line-matched stamps
}

// aliasFact is one trait-use alias adaptation pending graph resolution:
// on type typeName, the name `alias` resolves to method `method` of
// trait `trait` (trait "" when the adaptation is unqualified).
type aliasFact struct {
	typeName string
	alias    string
	trait    string
	method   string
	line     int
}

// bindingState tracks one name's type through the
// single-assignment-lite discipline: the first typed binding wins, a
// later conflicting (or unknowable) rebind poisons the binding so the
// engine never resolves through a type it cannot defend.
type bindingState struct {
	typ           string
	pendingCallee string
	poisoned      bool
	// elem is the ELEMENT type captured from a generic collection
	// annotation (`List<Foo>` -> "Foo"), normalized to the bare name; ""
	// when the binding's declared type carried no angle-bracket type
	// argument. It is consulted only by the higher-order collection-lambda
	// path, which binds a callback parameter to the receiver's element type
	// so an inner member call on it resolves.
	elem string
	// inferred marks a binding whose type came from an inferential source
	// rather than a direct native annotation — a guard narrowing
	// (`if ($x instanceof Foo)`) or a documentation-comment type hint
	// (`@var Foo $x`). It rides onto the call fact so the apply phase grades
	// a call through it at the inferred confidence band.
	inferred bool
}

type scopeKind int

const (
	scopeFile scopeKind = iota
	scopeType
	scopeFunc
	// scopeBlock is a transparent nested scope used to shadow a binding
	// inside an if's then-branch (type narrowing). It is neither a type
	// nor a callable scope, so enclosingTypeName / nearestTypeScope walk
	// straight through it to the real enclosing type.
	scopeBlock
)

type scopeEnv struct {
	parent   *scopeEnv
	kind     scopeKind
	typeName string // set on scopeType
	vars     map[string]*bindingState
}

func newScope(parent *scopeEnv, kind scopeKind) *scopeEnv {
	return &scopeEnv{parent: parent, kind: kind, vars: make(map[string]*bindingState)}
}

// lookup walks the scope chain for name.
func (s *scopeEnv) lookup(name string) *bindingState {
	for e := s; e != nil; e = e.parent {
		if st, ok := e.vars[name]; ok {
			return st
		}
	}
	return nil
}

// enclosingTypeName returns the nearest type scope's name.
func (s *scopeEnv) enclosingTypeName() string {
	for e := s; e != nil; e = e.parent {
		if e.kind == scopeType {
			return e.typeName
		}
	}
	return ""
}

// nearestTypeScope returns the nearest enclosing type scope.
func (s *scopeEnv) nearestTypeScope() *scopeEnv {
	for e := s; e != nil; e = e.parent {
		if e.kind == scopeType {
			return e
		}
	}
	return nil
}

// bind applies the single-assignment-lite rule: first binding wins; a
// rebind that does not provably preserve the type degrades the binding
// to unknown (poisoned), permanently for this scope chain. inferred marks
// the fresh binding as derived from an inferential source (a doc-comment
// type hint) so a call through it is graded at the inferred band.
func (s *scopeEnv) bind(name string, typ, pendingCallee, elem string, inferred bool) {
	if name == "" {
		return
	}
	if st := s.lookup(name); st != nil {
		if st.poisoned {
			return
		}
		if typ != st.typ || pendingCallee != st.pendingCallee {
			st.typ = ""
			st.pendingCallee = ""
			st.elem = ""
			st.poisoned = true
		}
		return
	}
	s.vars[name] = &bindingState{typ: typ, pendingCallee: pendingCallee, elem: elem, inferred: inferred}
}

// fieldType is one prepass field binding: its resolved type plus whether
// that type came from an inferential source (a `@var` docblock) rather
// than a native annotation, so the seeded binding grades calls honestly.
type fieldType struct {
	typ      string
	elem     string // element type of a generic collection field (`List<Foo>` -> "Foo")
	inferred bool
}

// binder runs the scope-graph walk over one parsed file.
type binder struct {
	spec  *LangSpec
	src   []byte
	facts *fileFacts
	// fieldsByType is the file-level pre-pass result: declared (and
	// conventionally initialised) field types per type name. Seeding
	// every type scope from it lets a method body resolve fields
	// declared after it — and, for Rust, fields declared on the struct
	// while the method lives in a separate impl block.
	fieldsByType map[string]map[string]fieldType
	// member is the innermost member declaration the walk is inside, as
	// LangSpec.MemberDeclName spells it; "" at type / file scope or when
	// the spec names no members. Every call fact recorded under it carries
	// it as its authored owner.
	member string
}

func newBinder(spec *LangSpec, src []byte, facts *fileFacts) *binder {
	return &binder{spec: spec, src: src, facts: facts, fieldsByType: make(map[string]map[string]fieldType)}
}

func (b *binder) run(root *sitter.Node) {
	if root == nil {
		return
	}
	b.prepassFields(root)
	fileScope := newScope(nil, scopeFile)
	if b.spec.Imports != nil {
		b.facts.imports = b.spec.Imports(root, b.src)
	}
	b.walk(root, fileScope)
}

// prepassFields collects field types for every type declaration in the
// file before the main walk.
func (b *binder) prepassFields(n *sitter.Node) {
	if n == nil {
		return
	}
	if b.spec.TypeDeclTypes[n.Type()] && b.spec.TypeDeclName != nil {
		if name := b.spec.TypeDeclName(n, b.src); name != "" && b.spec.Fields != nil {
			fields := b.fieldsByType[name]
			if fields == nil {
				fields = make(map[string]fieldType)
				b.fieldsByType[name] = fields
			}
			for _, f := range b.spec.Fields(n, b.src) {
				typ := b.spec.normalize(f.Type)
				if prev, ok := fields[f.Name]; ok && prev.typ != typ {
					// Conflicting declarations degrade to unknown —
					// same rule as local rebinds.
					fields[f.Name] = fieldType{}
					continue
				}
				fields[f.Name] = fieldType{typ: typ, elem: b.spec.normalize(firstTypeArg(f.Type)), inferred: f.Inferred}
				if typ != "" {
					b.facts.metas = append(b.facts.metas, metaFact{
						key: "semantic_type", value: typ, owner: name, name: f.Name,
					})
				}
			}
		}
	}
	for c := range n.NamedChildren() {
		b.prepassFields(c)
	}
}

func (b *binder) walk(n *sitter.Node, env *scopeEnv) {
	if n == nil {
		return
	}
	t := n.Type()

	// Entering a member declaration names the author of every call fact
	// recorded beneath it; the previous author is restored on the way out,
	// whichever branch below returns.
	if b.spec.MemberDeclName != nil {
		if name, ok := b.spec.MemberDeclName(n, b.src); ok {
			prev := b.member
			b.member = name
			defer func() { b.member = prev }()
		}
	}

	if b.spec.TypeDeclTypes[t] && b.spec.TypeDeclName != nil {
		name := b.spec.TypeDeclName(n, b.src)
		if name != "" {
			if b.spec.Supertypes != nil {
				for _, s := range b.spec.Supertypes(n, b.src) {
					super := b.spec.normalize(s.Name)
					if super == "" || super == name {
						continue
					}
					b.facts.supers = append(b.facts.supers, superFact{
						typeName: name, superName: super, kind: s.Kind, line: s.Line,
					})
				}
			}
			if b.spec.TraitAliases != nil {
				for _, al := range b.spec.TraitAliases(n, b.src) {
					if al.Alias == "" || al.Method == "" {
						continue
					}
					b.facts.aliases = append(b.facts.aliases, aliasFact{
						typeName: name, alias: al.Alias,
						trait: b.spec.normalize(al.Trait), method: al.Method, line: al.Line,
					})
				}
			}
			tEnv := newScope(env, scopeType)
			tEnv.typeName = name
			for fname, ft := range b.fieldsByType[name] {
				tEnv.vars[fname] = &bindingState{typ: ft.typ, elem: ft.elem, inferred: ft.inferred}
			}
			b.walkChildren(n, tEnv)
			return
		}
	}

	if b.spec.FuncDeclTypes[t] {
		fEnv := newScope(env, scopeFunc)
		// The callable's own docblock (`@param` / `@return`) is the fallback
		// type source for any parameter / return that carries no native
		// annotation. Resolved once for the whole callable.
		var docFacts []DocTypeFact
		if b.spec.DocType != nil {
			docFacts = b.spec.DocType(n, b.src)
		}
		if b.spec.Params != nil {
			for _, p := range b.spec.Params(n, b.src) {
				typ := b.spec.normalize(p.Type)
				inferred := false
				if typ == "" {
					// Native annotation absent: fall back to `@param T $x`.
					if dt := b.spec.normalize(docParamType(docFacts, p.Name)); dt != "" {
						typ, inferred = dt, true
					}
				}
				fEnv.vars[p.Name] = &bindingState{typ: typ, elem: b.spec.normalize(firstTypeArg(p.Type)), inferred: inferred}
			}
		}
		rt := ""
		if b.spec.ReturnType != nil {
			rt = b.spec.normalize(b.spec.ReturnType(n, b.src))
		}
		if rt == "" {
			// Native return absent: fall back to `@return T` (`$this` / self /
			// static are preserved verbatim so the apply phase's fluent
			// self-return rewrite types the result as the enclosing class).
			rt = b.spec.normalize(docReturnType(docFacts))
		}
		if rt != "" {
			b.facts.metas = append(b.facts.metas, metaFact{
				key: "return_type", value: rt, line: nodeLine(n),
			})
		}
		b.walkChildren(n, fEnv)
		return
	}

	if b.spec.LocalBinding != nil {
		if lb, ok := b.spec.LocalBinding(n, b.src); ok {
			typ := b.spec.normalize(lb.DeclType)
			// Capture the declared collection element type (`List<Foo>` ->
			// "Foo") so a higher-order call on this local can bind its lambda
			// parameter to the element. Only the native declared annotation
			// carries one; the inference / docblock paths below leave it "".
			elem := b.spec.normalize(firstTypeArg(lb.DeclType))
			pending := ""
			inferred := false
			if typ == "" || isInferenceKeyword(typ) {
				// A `@var T $x` docblock on the statement is an explicit author
				// assertion: it wins over initializer inference, but only when
				// the binding has no native annotation (which locals in these
				// grammars never do). It grades the binding inferred. When no
				// docblock types the local, fall back to initializer inference
				// exactly as before (including the inference-keyword case).
				docTyped := false
				if b.spec.DocType != nil {
					if dt := b.spec.normalize(docVarType(b.spec.DocType(n, b.src), lb.Name)); dt != "" {
						typ, inferred, docTyped = dt, true, true
					}
				}
				if !docTyped {
					typ, pending = b.exprType(lb.Init, env)
				}
			}
			if lb.Field {
				if ts := env.nearestTypeScope(); ts != nil {
					ts.bind(lb.Name, typ, pending, elem, inferred)
				}
			} else {
				env.bind(lb.Name, typ, pending, elem, inferred)
			}
			// Fall through: the initializer may contain calls worth
			// recording.
		}
	}

	if b.spec.Call != nil {
		if recv, method, ok := b.spec.Call(n, b.src); ok && method != "" {
			if cf, grounded := b.receiverFact(recv, env); grounded {
				cf.line = nodeLine(n)
				cf.method = method
				cf.owner = b.member
				if b.spec.CallArgCount != nil {
					if c, ok := b.spec.CallArgCount(n, b.src); ok {
						cf.argCount = c
						cf.argKnown = true
					}
				}
				b.facts.calls = append(b.facts.calls, cf)
			}
		}
	}

	// Operator desugaring: an operator / sugar expression is sugar for a
	// named member call (Kotlin `a + b` => `a.plus(b)`). Gated on
	// SyntheticCalls so every language that leaves the hook nil never enters
	// here (byte-for-byte no-op). Each fact is grounded and emitted through
	// the very same receiverFact path as a real `recv.method()` call, so a
	// synthetic call resolves only when the receiver types to an in-repo
	// member of the desugared name — an operator on a primitive / non-user
	// receiver grounds nothing (or resolves to nothing) and emits no edge.
	if b.spec.SyntheticCalls != nil {
		for _, sc := range b.spec.SyntheticCalls(n, b.src) {
			if sc.Method == "" || sc.Receiver == nil {
				continue
			}
			if cf, grounded := b.receiverFact(sc.Receiver, env); grounded {
				cf.line = nodeLine(n)
				cf.method = sc.Method
				cf.owner = b.member
				b.facts.calls = append(b.facts.calls, cf)
			}
		}
	}

	// Higher-order collection lambda: a call like `xs.filter { it.foo() }`
	// (Kotlin) / `xs.forEach(x -> x.foo())` (Java) whose callback's first
	// parameter IS the receiver collection's element type. Gated on
	// CollectionLambda so every language that leaves the hook nil never
	// enters here (byte-for-byte no-op). The Call branch above has already
	// emitted the outer call fact (it falls through); this block re-walks
	// the lambda body under a child scope that binds the parameter to the
	// receiver's element type, so an inner member call on the parameter
	// resolves on that type. When the element type is unknown the body is
	// walked unrefined, exactly as before — so a collection without a known
	// element type, or a non-element-callback method, regresses nothing.
	if b.spec.CollectionLambda != nil {
		if lc, ok := b.spec.CollectionLambda(n, b.src); ok {
			b.walkCollectionLambda(n, lc, env)
			return
		}
	}

	// Type narrowing: an if-statement whose guard refines a variable's
	// type. Gated on Narrowings + IfStmt so every language that leaves the
	// hooks unset descends if-statements exactly as before (nil hook =
	// no-op). An if-statement is not a type / func / local / call node, so
	// none of the branches above claim it before we reach here.
	if b.spec.Narrowings != nil && b.spec.IfStmt != nil {
		if cond, body, ok := b.spec.IfStmt(n, b.src); ok {
			b.walkIf(n, cond, body, env)
			return
		}
	}

	// Subject-dispatch narrowing: a `when (x) { is Foo -> … }` refines the
	// subject within each type-matched arm. Gated on SubjectNarrowings so
	// every language that leaves it nil descends the construct through the
	// generic child walk exactly as before (nil hook = no-op).
	if b.spec.SubjectNarrowings != nil {
		if m, ok := b.spec.SubjectNarrowings(n, b.src); ok {
			b.walkSubject(m, env)
			return
		}
	}

	b.walkChildren(n, env)
}

// walkIf descends an if-statement, applying type narrowing. Reached only
// when the spec wires Narrowings + IfStmt, so a language without those
// hooks never enters here.
//
// Two refinements land:
//   - then-branch: each non-negated fact (`$x instanceof Foo`) binds the
//     variable to the narrowed type in a child scope that shadows the
//     outer binding, so calls inside the then-body resolve on that type.
//   - guard tail: when the then-body is an early exit
//     (`if (!(...)) { return; }`), each negated fact binds the variable in
//     the CURRENT scope, refining it for the statements that FOLLOW the if
//     — control reaching them implies the guard did not fire, so the
//     variable provably holds the narrowed type.
//
// The else / else-if branches are walked WITHOUT narrowing (a v1
// conservativeness choice), and a non-guard negated fact narrows nothing.
func (b *binder) walkIf(n, cond, body *sitter.Node, env *scopeEnv) {
	var facts []NarrowFact
	if cond != nil {
		facts = b.spec.Narrowings(cond, b.src)
		// The condition may itself hold calls / locals worth recording.
		b.walk(cond, env)
	}

	// then-branch: non-negated facts narrow inside a shadowing child scope.
	if body != nil {
		thenEnv := env
		for _, f := range facts {
			if f.Negated {
				continue
			}
			if thenEnv == env {
				thenEnv = newScope(env, scopeBlock)
			}
			b.bindNarrow(thenEnv, f.Variable, f.Type)
		}
		b.walk(body, thenEnv)
	}

	// Remaining children (else / else-if clauses) are walked unrefined.
	for c := range n.NamedChildren() {
		if (cond != nil && c.Equal(cond)) || (body != nil && c.Equal(body)) {
			continue
		}
		b.walk(c, env)
	}

	// Guard tail: a negated fact under an early-exit then-body refines the
	// variable for the rest of THIS scope. Applied last so it never leaks
	// into the branches walked above.
	if body != nil && b.spec.EarlyExit != nil && b.spec.EarlyExit(body, b.src) {
		for _, f := range facts {
			if f.Negated {
				b.bindNarrow(env, f.Variable, f.Type)
			}
		}
	}
}

// walkSubject descends a subject-dispatch construct (Kotlin `when`),
// applying per-arm type narrowing. Reached only when the spec wires
// SubjectNarrowings. The subject expression and every arm pattern are
// walked unrefined (for any calls they hold); each arm body is walked
// under a child scope that shadows the subject with the arm's non-negated
// facts — the same shadowing the if then-branch uses — so a call inside a
// type-matched arm resolves on the narrowed type and grades inferred. An
// arm that narrows nothing (an `else` arm, or an ambiguous multi-pattern
// arm) is walked unrefined. Every node of the construct is walked exactly
// once, so no call / local the generic walk would record is lost.
func (b *binder) walkSubject(m SubjectMatch, env *scopeEnv) {
	if m.Subject != nil {
		b.walk(m.Subject, env)
	}
	for _, br := range m.Branches {
		for _, c := range br.Conds {
			if c != nil {
				b.walk(c, env)
			}
		}
		if br.Body == nil {
			continue
		}
		armEnv := env
		for _, f := range br.Facts {
			if f.Negated {
				continue
			}
			if armEnv == env {
				armEnv = newScope(env, scopeBlock)
			}
			b.bindNarrow(armEnv, f.Variable, f.Type)
		}
		b.walk(br.Body, armEnv)
	}
}

// bindNarrow shadows name with the narrowed type directly in env,
// bypassing the single-assignment-lite poison rule: a narrowing is a
// deliberate refinement of a variable whose outer binding (often an
// untyped param) we intend to override within the guarded scope, not a
// conflicting reassignment. The binding is flagged narrowed so the call
// edge it grounds lands at the inferred confidence band. An empty or
// unresolvable type is skipped — precision over recall.
func (b *binder) bindNarrow(env *scopeEnv, name, typ string) {
	if name == "" {
		return
	}
	typ = b.spec.normalize(typ)
	if typ == "" {
		return
	}
	env.vars[name] = &bindingState{typ: typ, inferred: true}
}

// walkCollectionLambda re-binds a higher-order collection call's lambda
// parameter to the receiver's element type and walks the lambda body under
// that binding, so an inner member call on the parameter resolves on the
// element type at the inferred band. Reached only when the spec wires
// CollectionLambda. When the receiver yields no known element type (a
// non-generic / untyped collection, or a chained `.stream()` receiver this
// engine does not thread an element type through), the whole call is walked
// generically — identical to the pre-hook behavior, so nothing the body
// holds is lost and no false edge is minted.
func (b *binder) walkCollectionLambda(n *sitter.Node, lc CollectionLambdaCall, env *scopeEnv) {
	elem := b.receiverElementType(lc.Receiver, env)
	if elem == "" || lc.Param == "" || lc.Body == nil {
		b.walkChildren(n, env)
		return
	}
	// The receiver subtree may itself hold calls worth recording
	// (`xs.stream().filter { … }` — the inner `stream()` call); walk it
	// under the outer scope. The element-callback methods take exactly one
	// lambda argument, so the receiver and the body are the only subtrees
	// that carry facts.
	if lc.Receiver != nil {
		b.walk(lc.Receiver, env)
	}
	bodyEnv := newScope(env, scopeBlock)
	// The element type is an inference (the receiver's declared generic
	// argument), so a call grounded through it grades at the inferred band.
	bodyEnv.vars[lc.Param] = &bindingState{typ: elem, inferred: true}
	b.walk(lc.Body, bodyEnv)
}

// receiverElementType returns the ELEMENT type of a higher-order call's
// collection receiver: a local / parameter identifier's captured element
// type (`xs : List<Foo>` -> "Foo"), or a `this.field` collection's element
// type. Returns "" when the receiver is not a directly-typed collection in
// scope — a poisoned binding, an untyped or non-generic collection, or a
// chained-call receiver (`xs.stream()`), which this engine does not thread
// an element type through. The returned name is already normalized.
func (b *binder) receiverElementType(recv *sitter.Node, env *scopeEnv) string {
	if recv == nil {
		return ""
	}
	if identifierLike(recv.Type()) {
		if st := env.lookup(recv.Content(b.src)); st != nil && !st.poisoned {
			return st.elem
		}
		return ""
	}
	if b.spec.FieldRef != nil {
		if fname, ok := b.spec.FieldRef(recv, b.src); ok {
			if ts := env.nearestTypeScope(); ts != nil {
				if st, found := ts.vars[fname]; found && !st.poisoned {
					return st.elem
				}
			}
		}
	}
	return ""
}

func (b *binder) walkChildren(n *sitter.Node, env *scopeEnv) {
	for c := range n.NamedChildren() {
		b.walk(c, env)
	}
}

// exprType evaluates an initializer expression to (type name, pending
// bare callee). Both empty means unknown.
func (b *binder) exprType(init *sitter.Node, env *scopeEnv) (string, string) {
	if init == nil {
		return "", ""
	}
	if b.spec.NewExprType != nil {
		if t := b.spec.normalize(b.spec.NewExprType(init, b.src)); t != "" {
			return t, ""
		}
	}
	if identifierLike(init.Type()) {
		if st := env.lookup(init.Content(b.src)); st != nil && !st.poisoned {
			return st.typ, st.pendingCallee
		}
		return "", ""
	}
	if b.spec.FieldRef != nil {
		if fname, ok := b.spec.FieldRef(init, b.src); ok {
			if ts := env.nearestTypeScope(); ts != nil {
				if st, found := ts.vars[fname]; found && !st.poisoned {
					return st.typ, st.pendingCallee
				}
			}
			return "", ""
		}
	}
	if callee := bareCallee(init, b.src); callee != "" {
		return "", callee
	}
	return "", ""
}

// receiverFact grounds a call's receiver expression. Returns ok=false
// when the receiver is structurally outside what the engine can defend
// (chained expressions, poisoned bindings, unknown shapes).
func (b *binder) receiverFact(recv *sitter.Node, env *scopeEnv) (callFact, bool) {
	if recv == nil {
		return callFact{}, false
	}
	text := recv.Content(b.src)
	if b.spec.SelfName != "" && text == b.spec.SelfName {
		if tn := env.enclosingTypeName(); tn != "" {
			return callFact{recvType: tn}, true
		}
		return callFact{}, false
	}
	if b.spec.FieldRef != nil {
		if fname, ok := b.spec.FieldRef(recv, b.src); ok {
			if ts := env.nearestTypeScope(); ts != nil {
				if st, found := ts.vars[fname]; found && !st.poisoned && st.typ != "" {
					return callFact{recvType: st.typ, inferred: st.inferred}, true
				}
			}
			return callFact{}, false
		}
	}
	if identifierLike(recv.Type()) {
		if st := env.lookup(text); st != nil {
			if st.poisoned {
				return callFact{}, false
			}
			if st.typ != "" {
				return callFact{recvType: st.typ, inferred: st.inferred}, true
			}
			if st.pendingCallee != "" {
				return callFact{recvPendingCallee: st.pendingCallee}, true
			}
			return callFact{}, false
		}
		// Unbound identifier: a static / type-qualified call candidate.
		// The apply phase only acts when it resolves to a type node.
		return callFact{recvIdent: text}, true
	}
	if b.spec.NewExprType != nil {
		if t := b.spec.normalize(b.spec.NewExprType(recv, b.src)); t != "" {
			return callFact{recvType: t}, true
		}
	}
	// Bare free-function call in receiver position (`listOf<Foo>().x()`,
	// `collect()->y()`): ground the callee name (and any explicit type
	// argument) so the apply phase can resolve its return type — an in-repo
	// function first, then the stdlib seed table as a last resort.
	if b.spec.BareCall != nil {
		if callee, typeArg, ok := b.spec.BareCall(recv, b.src); ok && callee != "" {
			return callFact{recvPendingCallee: callee, recvCallTypeArg: typeArg}, true
		}
	}
	// Chained receiver: the receiver is itself a method call
	// (`a.step().done()`). Ground the inner call's receiver and carry its
	// method name so the apply phase can type the outer receiver from the
	// inner method's (rewritten) return type.
	if b.spec.ChainedReceivers && b.spec.Call != nil {
		if innerRecv, innerMethod, ok := b.spec.Call(recv, b.src); ok && innerMethod != "" {
			if inner, grounded := b.receiverFact(innerRecv, env); grounded {
				inner.method = innerMethod
				return callFact{recvChain: &inner}, true
			}
		}
	}
	return callFact{}, false
}

// bareCallee returns the callee name when n is a call expression whose
// function is a bare identifier; "" otherwise. Handles the grammars'
// two common shapes (call / call_expression / invocation_expression
// with a `function` field).
func bareCallee(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "call", "call_expression", "invocation_expression":
		if fn := n.ChildByFieldName("function"); fn != nil && fn.Type() == "identifier" {
			return fn.Content(src)
		}
	}
	return ""
}

// isInferenceKeyword reports whether a written "type" is actually the
// language's inference keyword and should defer to the initializer.
func isInferenceKeyword(t string) bool {
	switch t {
	case "var", "let", "auto":
		return true
	}
	return false
}
