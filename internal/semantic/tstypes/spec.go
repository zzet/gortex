// Package tstypes implements in-process, LSP-free semantic providers
// over the shared tree-sitter ASTs. One shared engine builds a per-file
// scope graph (params, locals, fields, imports), binds declared and
// constructor-inferred types, propagates them through local assignments
// (single-assignment-lite: a rebind to a different type degrades the
// binding to unknown), and resolves receiver-qualified calls plus
// declared supertype relations against the symbol nodes the graph
// already holds. Per-language LangSpec tables adapt the engine to each
// grammar's node vocabulary.
//
// Provenance: everything this package touches is tree-sitter-derived,
// not compiler-verified, so edges are stamped OriginASTResolved (never
// the lsp_* tiers ConfirmEdge uses) with Meta["semantic_source"] set to
// the provider name ("java-types", "python-types", ...). A resolution
// the engine cannot ground in graph evidence — ambiguous receiver,
// unresolvable type name, overloaded method set — is skipped rather
// than guessed: a false edge is worse than a missing one.
package tstypes

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Binding is one named, optionally typed binding (param or field).
type Binding struct {
	Name string
	Type string // declared type name as written; "" when unannotated
	Line int    // 1-based declaration line
	// Inferred marks a binding whose Type came from an inferential source
	// (a documentation comment, not a checked native annotation). It rides
	// through the type-scope seeding onto the call fact so the apply phase
	// grades a call through such a receiver at the inferred confidence band.
	// Native-annotated bindings leave it false and stay at the direct band.
	Inferred bool
}

// DocTypeKind selects how the binder applies a documentation-comment type
// hint: to a local/field variable, a parameter, or a return type.
type DocTypeKind int

const (
	// DocVar is a `@var T $x` (or bare `@var T` on a property) hint.
	DocVar DocTypeKind = iota
	// DocParam is a `@param T $x` hint.
	DocParam
	// DocReturn is a `@return T` hint (Name is empty).
	DocReturn
)

// DocTypeFact is one type hint recovered from a documentation comment
// (a PHPDoc-style docblock). Kind selects how it applies; Name is the
// variable / parameter name as written in the comment (sigil-included
// where the grammar carries one, e.g. "$x"; empty for a return fact);
// Type is the chosen type name — already reduced to the leftmost
// non-null member of a union and alias-resolved, but NOT yet
// language-normalized (the binder runs spec.normalize on it, which strips
// leading "\", the nullable "?", and generic / array suffixes).
type DocTypeFact struct {
	Kind DocTypeKind
	Name string
	Type string
}

// docParamType returns the `@param` doc-hint type for the parameter named
// name, "" when the docblock carries none.
func docParamType(facts []DocTypeFact, name string) string {
	for _, f := range facts {
		if f.Kind == DocParam && f.Name == name {
			return f.Type
		}
	}
	return ""
}

// docReturnType returns the first `@return` doc-hint type, "" when absent.
func docReturnType(facts []DocTypeFact) string {
	for _, f := range facts {
		if f.Kind == DocReturn {
			return f.Type
		}
	}
	return ""
}

// docVarType returns the `@var` doc-hint type for the variable named name:
// a name-matched `@var T $name` first, else a bare unnamed `@var T`
// fallback (the single-variable form). "" when the docblock carries none.
func docVarType(facts []DocTypeFact, name string) string {
	var unnamed string
	for _, f := range facts {
		if f.Kind != DocVar {
			continue
		}
		if f.Name == name {
			return f.Type
		}
		if f.Name == "" && unnamed == "" {
			unnamed = f.Type
		}
	}
	return unnamed
}

// LocalBind is one local-variable declaration or assignment the engine
// folds into the scope's type environment.
type LocalBind struct {
	Name     string
	DeclType string       // explicit annotation; "" when absent
	Init     *sitter.Node // initializer expression; nil when absent
	Field    bool         // binds in the enclosing type scope (e.g. Ruby @ivar)
}

// SuperRef is one declared supertype relation of a type declaration.
// Kind is EdgeExtends or EdgeImplements when the syntax declares it;
// an empty Kind defers the choice to the apply phase, which picks by
// the resolved target's node kind (used by C#, whose base list does
// not distinguish the base class from interfaces syntactically).
type SuperRef struct {
	Name string
	Kind graph.EdgeKind
	Line int // 1-based
}

// Import is one name-binding import: Local is the identifier the file
// sees; Path is a slash-separated location hint used to prefer the
// matching definition file when several nodes share the name.
type Import struct {
	Local string
	Path  string
}

// AliasRef is one trait-use adaptation that renames an aliased member
// onto the using type (PHP `use T { T::fn as renamed; }`): Alias is the
// new name the using type exposes, Method is the original member name,
// and Trait names the trait the member comes from ("" when the
// adaptation is unqualified, e.g. `use T { fn as renamed; }`).
// Conflict-resolution adaptations (`insteadof`) are deliberately NOT
// represented here — an ambiguous member stays unresolved rather than
// being bound to one arbitrary side.
type AliasRef struct {
	Alias  string
	Trait  string
	Method string
	Line   int
}

// NarrowFact is one type refinement a guard condition imposes on a
// variable. Within the scope the guard governs, Variable provably holds
// type Type. Negated inverts the sense: `!($x instanceof Foo)` yields a
// negated fact — true where the condition is FALSE, i.e. the tail of an
// early-exit guard. The binder applies non-negated facts to the
// then-branch and negated facts to the tail after an early-exit guard;
// it never narrows the else branch.
type NarrowFact struct {
	Variable string // receiver name as written (sigil-included, e.g. "$x")
	Type     string // narrowed type name as written ("" / unresolved is skipped)
	Negated  bool
}

// SubjectMatch is the decomposition of a subject-dispatch construct — a
// match / switch on a single subject value (Kotlin `when (x) { is Foo -> … }`)
// — into the subject expression and its branches. The binder walks the
// subject and each branch's pattern unrefined, and walks each branch body
// under a child scope carrying that branch's narrowing facts (so a
// type-matched branch resolves calls on the narrowed type). It is the
// grammar adapter that lets the shared binder narrow a match arm without
// baking per-grammar node names into the binder.
type SubjectMatch struct {
	Subject  *sitter.Node
	Branches []SubjectBranch
}

// SubjectBranch is one arm of a SubjectMatch: the pattern condition nodes
// (walked unrefined, for any calls they hold), the arm body, and the type
// refinements the pattern imposes on the subject within that body. Facts is
// nil for an arm that narrows nothing (an `else` arm, or an ambiguous
// multi-pattern arm) — its body is then walked unrefined.
type SubjectBranch struct {
	Conds []*sitter.Node
	Body  *sitter.Node
	Facts []NarrowFact
}

// SyntheticCall is one member call an expression desugars to: an operator
// expression is sugar for a named member function (Kotlin `a + b` is
// `a.plus(b)`, `a[i]` is `a.get(i)`, `a in b` is `b.contains(a)`). Receiver
// is the AST node whose resolved type owns the desugared member — for most
// operators the left operand, but for `in` it is the right operand (the
// collection), so the binder types the correct side. Method is the desugared
// member name. Args carries the argument expression nodes for completeness;
// the apply phase resolves the call by member name on the receiver type and
// does not consult argument types, so an empty Args never blocks resolution.
type SyntheticCall struct {
	Receiver *sitter.Node
	Method   string
	Args     []*sitter.Node
}

// CollectionLambdaCall is the decomposition of a higher-order collection
// call whose callback's first parameter is the receiver collection's element
// type — Kotlin `xs.filter { it.foo() }`, Java `xs.forEach(x -> x.foo())`.
// Receiver is the collection receiver node (the binder reads its captured
// element type), Param is the callback's first parameter name (the implicit
// `it` for a Kotlin lambda with no parameter list, or the written name), and
// Body is the lambda body to re-walk under the element binding. A language's
// hook returns ok=true only for a call whose method is in that language's
// element-callback set and whose sole argument is a single-parameter lambda
// it can decode — so a non-element-callback method, or a multi-parameter /
// destructuring lambda, decodes to nothing and is walked generically.
type CollectionLambdaCall struct {
	Receiver *sitter.Node
	Param    string
	Body     *sitter.Node
}

// LangSpec adapts the shared engine to one language's tree-sitter
// grammar. The node-type sets drive the generic walk; the hooks decode
// the handful of shapes that differ per grammar. Hooks may be nil when
// the language has no equivalent construct (e.g. Ruby has no type
// annotations, C# has no name-binding imports).
type LangSpec struct {
	ProviderName string
	Languages    []string

	// CandidateLanguages, when non-empty, is the full set of node Language
	// values a type-candidate lookup may bind for this spec — the spec's own
	// languages plus sibling grammar variants (tsx/jsx for the TypeScript
	// spec). Candidate hydration is scoped to this set: on a mixed repo a
	// TypeScript pass must not drown in same-named symbols from the host
	// language (a 194k-node Go repo turned a few hundred TS files into a
	// ten-minute pass purely through cross-language candidate pollution).
	// Empty means Languages. Language-neutral ("") nodes are always allowed.
	CandidateLanguages []string

	// GrammarFor returns the grammar for a file path. Per-path because
	// one provider can span sibling grammars (typescript / tsx /
	// javascript).
	GrammarFor func(filePath string) *sitter.Language

	// Suppressed, when non-nil and returning true, makes the provider a
	// complete no-op on this host: EnrichRepo / EnrichFile return an empty
	// result without parsing any file or touching the graph. It is the
	// toolchain-fallback gate for a language whose AUTHORITATIVE provider —
	// a compiler / type-system pass — is the source of truth WHEN AVAILABLE,
	// and which this spec only ever supplements with a tree-sitter
	// resolution FLOOR where that provider cannot run. The Go spec
	// suppresses itself whenever the Go toolchain is installed (go-types then
	// owns Go resolution at compiler grade), so it contributes only on a host
	// without a Go toolchain. nil (the default) never suppresses, so every
	// other spec always contributes exactly as before.
	Suppressed func() bool

	// TypeDeclTypes / FuncDeclTypes are the node types that open a type
	// or callable scope.
	TypeDeclTypes map[string]bool
	FuncDeclTypes map[string]bool

	// MemberDeclName names the member declaration that authors every call
	// site in n's subtree — a method, constructor, property, field
	// declarator, indexer or event — spelled exactly as the extractor names
	// that member's graph node, so the apply phase can pick a call fact's
	// own author out of a same-line same-name stub tie. ok=false for any
	// other node; a nested callable that mints no node of its own (a local
	// function, a lambda) must answer false so its calls stay with the
	// enclosing member, as the extractor's stubs do. nil (the default)
	// records no author, and a tie is then broken by line containment
	// alone, byte-for-byte as before.
	MemberDeclName func(n *sitter.Node, src []byte) (string, bool)

	// SelfName is the receiver keyword ("this", "self"); "" when the
	// language has none.
	SelfName string

	// TypeDeclName extracts the declared type name ("" skips the node).
	TypeDeclName func(n *sitter.Node, src []byte) string

	// Supertypes lists the declared supertype relations of a type decl.
	Supertypes func(n *sitter.Node, src []byte) []SuperRef

	// Fields lists the field bindings of a type decl (declared fields
	// plus whatever conventional initialisations the language grounds,
	// e.g. Python's `self.x = Foo()` or Ruby's `@x = Foo.new`).
	Fields func(n *sitter.Node, src []byte) []Binding

	// Params lists a callable's declared parameters.
	Params func(fn *sitter.Node, src []byte) []Binding

	// ReturnType extracts an explicit return-type annotation ("" when
	// absent or unsupported).
	ReturnType func(fn *sitter.Node, src []byte) string

	// LocalBinding decodes a local declaration / assignment node.
	LocalBinding func(n *sitter.Node, src []byte) (LocalBind, bool)

	// Call decodes a receiver-qualified call: the receiver expression
	// and the method name. ok=false for anything else (including
	// receiverless calls — those are the resolver's job already).
	Call func(n *sitter.Node, src []byte) (recv *sitter.Node, method string, ok bool)

	// CallArgCount returns the number of argument expressions at a
	// receiver-qualified call site — the same node Call decodes. It lets
	// the apply phase disambiguate an overload set by arity: when a
	// receiver type declares several same-named members, the candidate
	// whose declared parameter count uniquely equals this count is the
	// resolved target. ok=false (or a nil hook) leaves the call's arity
	// unknown, so an overload set is never narrowed by arity for that
	// language and the apply phase keeps skipping ambiguous sets.
	CallArgCount func(n *sitter.Node, src []byte) (int, bool)

	// NewExprType returns the constructed type name when n is a
	// constructor expression ("" otherwise). Conventional constructors
	// (Python `Foo()`, Ruby `Foo.new`, Rust `Foo::new`) may be
	// returned too — the apply phase verifies every receiver type
	// against a real graph type node before resolving through it.
	NewExprType func(n *sitter.Node, src []byte) string

	// FieldRef reports that n is a reference to an instance field of
	// the current receiver (`this.x`, `self.x`, `@x`) and returns the
	// field's binding name.
	FieldRef func(n *sitter.Node, src []byte) (string, bool)

	// Imports lists the file's name-binding imports.
	Imports func(root *sitter.Node, src []byte) []Import

	// SupertypeKinds widens the node kinds a declared supertype name
	// may resolve to. nil keeps the receiver default (type /
	// interface). Ruby adds packages: tree-sitter modules index as
	// KindPackage and `include M` targets them.
	SupertypeKinds map[graph.NodeKind]bool

	// InheritEdgeKinds lists the edge kinds methodOn climbs when it
	// looks up an inherited member. An empty slice defaults to
	// {EdgeExtends} — only the superclass / supertype chain. Languages
	// whose inheritance spans more than subclassing widen it: Ruby adds
	// EdgeImplements so the modules pulled in by `include` / `prepend`
	// / `extend` contribute their methods. PHP keeps the {EdgeExtends}
	// default: trait composition (`use T;`) is itself modeled as an
	// extends edge, so the default walk already climbs into used traits
	// once they resolve.
	InheritEdgeKinds []graph.EdgeKind

	// ChainedReceivers enables typing a call whose receiver is itself a
	// method call (`a.step().done()`). When set, the binder grounds the
	// inner call's receiver and method, and the apply phase resolves the
	// inner method's declared return type — applying the fluent self /
	// trait return rewrite — to type the outer call's receiver. Off by
	// default; languages with reliable return-type fidelity and fluent
	// chains opt in. The resulting outer edge is graded as inferred.
	ChainedReceivers bool

	// TraitAliases lists the trait-use adaptations that rename an aliased
	// member onto a using type (PHP `use T { T::fn as renamed; }`). nil
	// for languages without the construct. The apply phase routes a call
	// to the alias name through to the original trait member.
	TraitAliases func(n *sitter.Node, src []byte) []AliasRef

	// NormalizeType reduces a written type to the bare name the graph
	// indexes (strip generics / pointers / qualifiers). nil uses the
	// shared default.
	NormalizeType func(t string) string

	// Narrowings decodes a guard / if CONDITION node into zero or more
	// type refinements (PHP `$x instanceof Foo` -> {"$x", "Foo", false};
	// `!($x instanceof Foo)` -> {"$x", "Foo", true}). nil disables
	// narrowing entirely: the binder then descends if-statements through
	// the generic child walk exactly as it descends any other node, so a
	// language that leaves Narrowings (and IfStmt) unset behaves
	// byte-for-byte as before. Consulted only together with IfStmt.
	Narrowings func(cond *sitter.Node, src []byte) []NarrowFact

	// IfStmt decomposes an if-statement node into its condition and its
	// then-body; ok=false for any other node. It is the grammar adapter
	// that lets the shared binder find an if's parts without baking
	// per-grammar field names into the binder. The remaining children of
	// the if (else / else-if clauses) are walked generically without
	// narrowing. Consulted only when Narrowings is set; nil (the default)
	// leaves if-statements to the generic child walk.
	IfStmt func(n *sitter.Node, src []byte) (cond, body *sitter.Node, ok bool)

	// EarlyExit reports whether an if's then-body is a guard clause: a
	// body whose control unconditionally leaves the surrounding flow
	// (return / throw / continue / break). A negated narrowing under such
	// a body refines the variable for the statements that FOLLOW the if in
	// the same scope (the guard's tail). nil disables tail narrowing while
	// still allowing then-branch narrowing. Consulted only when Narrowings
	// and IfStmt are set.
	EarlyExit func(body *sitter.Node, src []byte) bool

	// ExtensionFunctions enables resolving a receiver-qualified call that
	// misses every real member against the language's extension functions —
	// top-level functions declared with a receiver type (`fun Foo.ext()`),
	// which the extractor emits as KindMethod nodes stamped
	// Meta["extension_receiver"]=<receiver type name>. The call phase
	// consults a (receiver, method) index as a FALLBACK, only after a real
	// member lookup (direct + inherited) and any trait-alias miss, so a real
	// member of the same name always wins (members shadow extensions). An
	// extension claimed by more than one declaration on the same receiver
	// stays unresolved rather than guessed. Off by default — a language
	// without extension functions builds no index and behaves exactly as
	// before.
	ExtensionFunctions bool

	// DocType extracts type hints from the documentation comment (docblock)
	// immediately preceding declNode — a parameter-bearing callable, a
	// property, or a local-assignment / statement node. It returns zero or
	// more facts: `@var T $x` / `@var T` (DocVar), `@param T $x` (DocParam),
	// `@return T` (DocReturn). The binder uses each fact as a FALLBACK type
	// source: a native type annotation on the same binding ALWAYS WINS, and
	// DocType supplies a type only where the native annotation is absent. A
	// docblock that cannot be parsed, or whose tag carries no resolvable
	// type, contributes nothing — the binder never guesses from a comment.
	// Because a documentation comment is an author assertion rather than a
	// checked annotation, every edge resolved THROUGH a DocType-supplied
	// type is graded at the inferred confidence band. nil (the default)
	// disables docblock typing entirely, so a language that leaves it unset
	// behaves byte-for-byte as before.
	DocType func(declNode *sitter.Node, src []byte) []DocTypeFact

	// SyntheticCalls desugars an operator / sugar expression into zero or
	// more member calls the language defines it to mean (Kotlin `a + b` ->
	// `a.plus(b)`, `a[i]` -> `a.get(i)`, `a in b` -> `b.contains(a)`,
	// `for (x in coll)` -> `coll.iterator()`, ...). The binder consults it on
	// every node it walks and, for each returned fact, emits a call fact
	// exactly as if the source had written `Receiver.Method(Args)` — so the
	// standard receiver-typing and member-resolution path applies unchanged.
	// A synthetic call therefore resolves to the operator function ONLY when
	// the receiver's resolved type is an in-repo type that declares a member
	// of the desugared name; an operator on a primitive / non-user receiver
	// (`1 + 2`) types no in-repo member and emits no edge, and the engine
	// never mints an unresolved-target edge for a miss. The resulting edge is
	// a genuine resolution (the operator IS that call), so it lands at the
	// direct AST confidence band, not the inferred band. nil (the default)
	// disables operator desugaring entirely, so a language that leaves it
	// unset behaves byte-for-byte as before.
	SyntheticCalls func(n *sitter.Node, src []byte) []SyntheticCall

	// SubjectNarrowings decodes a subject-dispatch node (Kotlin
	// `when (x) { is Foo -> … }`) into a SubjectMatch, returning ok=false
	// for any other node. The binder, gated on this hook, walks the
	// returned subject and branch patterns unrefined and walks each branch
	// body under a child scope that shadows the subject with the branch's
	// narrowing facts — reusing the exact then-branch machinery if-narrowing
	// uses, so a `when` type-match arm resolves calls on the narrowed type at
	// the inferred confidence band. nil (the default) disables subject
	// narrowing: the construct is then descended through the generic child
	// walk exactly as before, so a language that leaves it unset behaves
	// byte-for-byte as before.
	SubjectNarrowings func(n *sitter.Node, src []byte) (SubjectMatch, bool)

	// BareCall decodes a receiver-less call standing in receiver position —
	// a free function whose result is immediately navigated
	// (`listOf<Foo>().first()`, `collect()->map()`). It returns the callee
	// name and, when the grammar carries one at the call site, the explicit
	// generic type ARGUMENT (the `Foo` in `listOf<Foo>()`); typeArg is ""
	// when absent. ok=false for anything that is not a bare free call
	// (construction, navigation chain, identifier). The apply phase grounds
	// such a receiver through the callee's return type — an in-repo function
	// first, then the stdlib seed table — and uses the type argument to
	// element-type a subsequent collection access. nil (the default) leaves
	// a bare-call receiver ungrounded, byte-for-byte as before.
	BareCall func(n *sitter.Node, src []byte) (callee, typeArg string, ok bool)

	// StdlibReturnType maps a well-known standard-library callable to the
	// container type it returns, consulted ONLY as a last resort — after an
	// in-repo function / method of the same name fails to resolve, so an
	// in-repo symbol always wins. recv is the normalized receiver type for a
	// method call, "" for a free function: ("listOf","") -> "List",
	// ("collect","") -> "Collection", ("map","Collection") -> "Collection".
	// ok=false leaves the callee unseeded. The table must stay TINY and
	// hold only unambiguous mappings — a wrong seed mints a false edge. An
	// edge resolved through a seeded type is graded at the inferred
	// confidence band. nil (the default) disables the seed entirely
	// (byte-for-byte no-op for every other language).
	StdlibReturnType func(callee, recv string) (ret string, ok bool)

	// StdlibElementAccess reports whether `method` reads a single ELEMENT
	// out of a collection produced by the standard-library builder
	// `builder` (`mutableListOf<Foo>().first()` — builder "mutableListOf",
	// method "first"). When it does, a `builder<Elem>().method()` chain
	// types to Elem: the apply phase resolves the captured element type
	// rather than the container, so a call on the element resolves at the
	// inferred band. Consulted only when both the builder and a captured
	// element type are present. nil (the default) disables element typing.
	StdlibElementAccess func(builder, method string) bool

	// CollectionLambda decodes a higher-order collection call whose
	// callback's first parameter is the receiver collection's element type
	// (Kotlin `xs.filter { it.foo() }`, Java `xs.forEach(x -> x.foo())`),
	// returning ok=false for any other call. The hook owns the per-language
	// element-callback method set (only methods whose callback's first
	// parameter is the element — `filter` / `map` / `forEach` / `anyMatch`
	// / …) and the lambda-shape decode. When it returns ok=true, the binder,
	// having captured the receiver's declared generic element type
	// (`List<Foo>` -> "Foo") at bind time, binds the callback parameter to
	// that element type in a child scope and re-walks the body, so an inner
	// member call on the parameter resolves on the element type at the
	// inferred confidence band. When the receiver's element type is unknown
	// (a non-generic / untyped collection, or a `.stream()`-chained
	// receiver) the body is walked unrefined, so resolution honestly stops
	// rather than guessing. nil (the default) disables the path entirely, so
	// a language that leaves it unset behaves byte-for-byte as before.
	CollectionLambda func(n *sitter.Node, src []byte) (CollectionLambdaCall, bool)
}

// inheritEdgeKinds returns the edge kinds methodOn climbs when looking
// up an inherited member: the spec's explicit set, or {EdgeExtends}
// when it leaves the field empty — preserving the legacy
// superclass-only walk for every language that does not widen it.
func (s *LangSpec) inheritEdgeKinds() []graph.EdgeKind {
	if len(s.InheritEdgeKinds) > 0 {
		return s.InheritEdgeKinds
	}
	return []graph.EdgeKind{graph.EdgeExtends}
}

func (s *LangSpec) normalize(t string) string {
	if s.NormalizeType != nil {
		return s.NormalizeType(t)
	}
	return NormalizeTypeName(t)
}

// handles reports whether the spec serves the given language code.
func (s *LangSpec) handles(lang string) bool {
	for _, l := range s.Languages {
		if l == lang {
			return true
		}
	}
	return false
}

// NormalizeTypeName is the shared written-type → bare-name reduction:
// strips generic arguments, array suffixes, nullability markers,
// reference sigils, and namespace qualifiers, leaving the identifier
// the graph indexes type nodes under.
func NormalizeTypeName(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Reference / pointer / ownership sigils and prefix keywords.
	for {
		switch {
		case strings.HasPrefix(t, "&"), strings.HasPrefix(t, "*"):
			t = strings.TrimSpace(t[1:])
			continue
		case strings.HasPrefix(t, "mut "):
			t = strings.TrimSpace(t[4:])
			continue
		case strings.HasPrefix(t, "dyn "):
			t = strings.TrimSpace(t[4:])
			continue
		case strings.HasPrefix(t, "impl "):
			t = strings.TrimSpace(t[5:])
			continue
		}
		break
	}
	// Generic arguments and array / nullability suffixes.
	if i := strings.IndexAny(t, "<(["); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSuffix(strings.TrimSuffix(t, "?"), "!")
	// Namespace / module qualifiers — keep the last segment.
	if i := strings.LastIndex(t, "::"); i >= 0 {
		t = t[i+2:]
	}
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

// firstTypeArg returns the first generic type argument written in a type, as
// written: "List<Foo>" -> "Foo", "Map<String, Foo>" -> "String",
// "List<Map<K,V>>" -> "Map<K,V>". "" when the type carries no angle-bracket
// type arguments. The caller normalizes the result to the bare type name.
// Nested generics are tracked by depth so a top-level comma or the matching
// close bracket terminates the first argument correctly.
func firstTypeArg(t string) string {
	open := strings.IndexByte(t, '<')
	if open < 0 {
		return ""
	}
	depth := 0
	for i := open; i < len(t); i++ {
		switch t[i] {
		case '<':
			depth++
		case '>':
			depth--
			if depth == 0 {
				return strings.TrimSpace(t[open+1 : i])
			}
		case ',':
			if depth == 1 {
				return strings.TrimSpace(t[open+1 : i])
			}
		}
	}
	return ""
}

// nodeLine returns the 1-based start line of n.
func nodeLine(n *sitter.Node) int {
	return int(n.StartPoint().Row) + 1
}

// fieldText returns the text of a named field child, "" when absent.
func fieldText(n *sitter.Node, field string, src []byte) string {
	c := n.ChildByFieldName(field)
	if c == nil {
		return ""
	}
	return c.Content(src)
}

// nameField extracts the `name` field's text — the TypeDeclName shape
// every grammar here shares.
func nameField(n *sitter.Node, src []byte) string {
	return fieldText(n, "name", src)
}

// firstChildOfType returns the first named child with the given type.
func firstChildOfType(n *sitter.Node, t string) *sitter.Node {
	for c := range n.NamedChildren() {
		if c.Type() == t {
			return c
		}
	}
	return nil
}

// identifierLike reports whether the node is a bare single-token name
// usable for scope lookup. "name" is tree-sitter-php's bare identifier
// node — it is the scope of a static `Foo::bar()` call, so it must be
// recognised for the type-qualified receiver path. "simple_identifier"
// is tree-sitter-kotlin's bare identifier — the receiver of a Kotlin
// `dep.bar()` and the right-hand side of a `val x = y` propagation. No
// other registered grammar emits a "name" / "simple_identifier" node in
// receiver / initializer position, so adding them is additive per-grammar.
func identifierLike(t string) bool {
	switch t {
	case "identifier", "constant", "type_identifier", "variable_name", "local_variable", "name", "simple_identifier":
		return true
	}
	return false
}

// candidateLanguages returns the language filter for type-candidate lookups:
// CandidateLanguages (or Languages when unset) plus the language-neutral "".
func (s *LangSpec) candidateLanguages() []string {
	base := s.CandidateLanguages
	if len(base) == 0 {
		base = s.Languages
	}
	out := make([]string, 0, len(base)+1)
	seen := make(map[string]struct{}, len(base)+1)
	for _, lang := range append(append([]string(nil), base...), "") {
		if _, dup := seen[lang]; dup {
			continue
		}
		seen[lang] = struct{}{}
		out = append(out, lang)
	}
	return out
}

// allowsCandidateLanguage reports whether a candidate node's language is
// inside the spec's candidate set (the compatibility-path filter mirroring
// the scoped SQL lookup).
func (s *LangSpec) allowsCandidateLanguage(language string) bool {
	for _, lang := range s.candidateLanguages() {
		if lang == language {
			return true
		}
	}
	return false
}
