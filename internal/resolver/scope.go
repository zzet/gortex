package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Scope-resolution metadata keys. Extractors populate these on call
// edges and node payloads so the resolver can disambiguate same-named
// symbols using language-specific scope rules (C file-static, C++
// namespace + ADL, Java enclosing class / static-import, PHP
// namespace + parent::/self::) before falling back to the generic
// directory-locality cascade.
//
// Keep the key names short — every call edge in a million-symbol
// graph carries them, so a few bytes per key compound fast.
const (
	// MetaScopeNamespace — fully-qualified namespace the symbol lives
	// in (`std::detail`, `App\Service`, `com.example.foo`). Populated
	// on node payloads (KindFunction / KindMethod / KindType / …) and
	// on call edges (the namespace of the *caller*).
	MetaScopeNamespace = "scope_ns"

	// MetaScopeClass — name of the enclosing class for class-bound
	// callers / definitions (`User`, `App\Repository\UserRepo`).
	MetaScopeClass = "scope_class"

	// MetaScopeParentClass — name of the direct parent class. Set on
	// KindType nodes for C++/Java/PHP classes that extend a base.
	// Used by PHP's `parent::method` resolution and Java's super-call
	// disambiguation.
	MetaScopeParentClass = "scope_parent"

	// MetaScopeStatic — true on KindFunction nodes that have file-
	// local linkage (C `static void foo()`, PHP namespaced functions
	// that aren't reachable via `use function`). The resolver prefers
	// a static candidate defined in the caller's file over global
	// candidates with the same name.
	MetaScopeStatic = "scope_static"

	// MetaScopeKind — one of "unqualified", "qualified", "self",
	// "parent", "static", "method". Tells the resolver which scope-
	// resolution strategy to apply on a call edge.
	MetaScopeKind = "scope_kind"

	// MetaScopeArgTypes — comma-separated list of argument-type
	// names hinted by the C++ extractor for ADL ("Argument-Dependent
	// Lookup"). Each entry is a possibly-namespaced type
	// (`std::string`, `MyNs::Widget`); the resolver walks each
	// entry's namespace looking for a free function whose name
	// matches the call.
	MetaScopeArgTypes = "scope_arg_types"

	// MetaScopeUseAliases — semicolon-separated `alias=>target` pairs
	// from PHP `use function NS\foo as bar` declarations in the
	// caller's file. The resolver translates an unresolved call to
	// `bar` into a search for `NS\foo` before falling back to the
	// generic cascade.
	MetaScopeUseAliases = "scope_use_aliases"
)

// Per-language scope-kind constants. Stamped on the call edge by the
// extractor when the call site is something other than a plain
// unqualified identifier — `parent::foo()`, `self::foo()`,
// `Class::staticFoo()`, etc.
const (
	ScopeKindUnqualified = "unqualified"
	ScopeKindQualified   = "qualified"
	ScopeKindSelf        = "self"
	ScopeKindParent      = "parent"
	ScopeKindStatic      = "static"
)

// scopeMetaString returns the string value at key in m, or "" if
// missing / wrong type. Tiny accessor that keeps the hot path tidy.
func scopeMetaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// scopeMetaBool returns the bool value at key in m, or false if
// missing / wrong type.
func scopeMetaBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, _ := m[key].(bool)
	return v
}

// scopeArgTypeHints decodes the MetaScopeArgTypes payload into a
// slice of type-name strings (already split, no whitespace). Empty
// when the key is missing or the call has no positional arguments.
func scopeArgTypeHints(m map[string]any) []string {
	raw := scopeMetaString(m, MetaScopeArgTypes)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// scopePositionalArgHints decodes MetaScopeArgTypes preserving argument
// positions (unlike scopeArgTypeHints, which drops empties for ADL). A "?"
// placeholder — an argument the extractor could not type — decodes to "" so the
// overload ranker treats that slot as unknown (compatible with any parameter).
func scopePositionalArgHints(m map[string]any) []string {
	raw := scopeMetaString(m, MetaScopeArgTypes)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "?" {
			p = ""
		}
		out[i] = p
	}
	return out
}

// scopeUseAliases decodes the MetaScopeUseAliases payload into a
// map of (local-alias → fully-qualified target). Empty when the file
// has no `use function` declarations.
func scopeUseAliases(m map[string]any) map[string]string {
	raw := scopeMetaString(m, MetaScopeUseAliases)
	if raw == "" {
		return nil
	}
	out := make(map[string]string, 4)
	for _, pair := range strings.Split(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 || eq+2 > len(pair) || pair[eq+1] != '>' {
			continue
		}
		alias := strings.TrimSpace(pair[:eq])
		target := strings.TrimSpace(pair[eq+2:])
		if alias == "" || target == "" {
			continue
		}
		out[alias] = target
	}
	return out
}

// preferScopeCandidate returns the best per-language candidate for
// an unresolved call edge, or nil to fall through to the generic
// resolver cascade. Each language's branch is conservative — it
// returns nil unless it has high-confidence scope evidence — so the
// generic cascade still handles the long tail.
//
// The dispatch keys off the caller node's Language (not the edge,
// because legacy edges may not carry language). Returning nil keeps
// the resolver behavior identical for unsupported languages.
func (r *Resolver) preferScopeCandidate(e *graph.Edge, name string, candidates []*graph.Node) *graph.Node {
	caller := r.cachedGetNode(e.From)
	if caller == nil {
		return nil
	}
	switch caller.Language {
	case "c":
		return r.preferCStaticCandidate(e, caller, candidates)
	case "cpp":
		return r.preferCppScopeCandidate(e, caller, name, candidates)
	case "java":
		return r.preferJavaScopeCandidate(e, caller, name, candidates)
	case "php":
		return r.preferPhpScopeCandidate(e, caller, name, candidates)
	}
	return nil
}

// preferCStaticCandidate — C scope rule: a `static` function has
// file-local linkage, so a same-file static candidate is the only
// legal target for an unresolved call when one exists. Prevents the
// generic cascade from binding the call to an extern function of
// the same name in a different translation unit.
func (r *Resolver) preferCStaticCandidate(e *graph.Edge, caller *graph.Node, candidates []*graph.Node) *graph.Node {
	for _, c := range candidates {
		if c.Kind != graph.KindFunction {
			continue
		}
		if c.FilePath != caller.FilePath {
			continue
		}
		if scopeMetaBool(c.Meta, MetaScopeStatic) {
			return c
		}
	}
	// File-local-static rule cuts the other way too: if a candidate
	// is `static` in a *different* file, it cannot be a legal target.
	// Filter it out by returning the first non-static candidate when
	// every same-file alternative is non-static. Caller falls through
	// otherwise.
	return nil
}

// preferCppScopeCandidate — C++ scope rule: namespace match wins
// over directory/locality match. ADL (Argument-Dependent Lookup):
// for an unqualified call `foo(a, b)`, if any of a's, b's argument
// types name a class in namespace `N`, then `N::foo` is a candidate.
// Implementation order:
//  1. Same-namespace function/method match (lexical scope).
//  2. ADL: walk each scope_arg_types entry's namespace.
//  3. Fall through to the generic cascade.
func (r *Resolver) preferCppScopeCandidate(e *graph.Edge, caller *graph.Node, name string, candidates []*graph.Node) *graph.Node {
	// In-engine overload resolution first: rank the candidates by C++ arity +
	// implicit-conversion-sequence and pick the best-viable. Returns nil to
	// DEGRADE to the namespace cascade when it cannot decide (no signatures,
	// no viable candidate, or genuine ambiguity), so it never binds a wrong
	// overload.
	if best := ResolveCppOverload(scopePositionalArgHints(e.Meta), candidates); best != nil {
		return best
	}
	callerNs := scopeMetaString(caller.Meta, MetaScopeNamespace)
	if callerNs != "" {
		for _, c := range candidates {
			if c.Kind != graph.KindFunction && c.Kind != graph.KindMethod {
				continue
			}
			if scopeMetaString(c.Meta, MetaScopeNamespace) == callerNs {
				return c
			}
		}
	}
	hints := scopeArgTypeHints(e.Meta)
	if len(hints) == 0 {
		return nil
	}
	adlNamespaces := make(map[string]struct{}, len(hints))
	for _, typeName := range hints {
		ns := splitNamespaceFromQualifiedName(typeName)
		if ns == "" {
			continue
		}
		adlNamespaces[ns] = struct{}{}
	}
	if len(adlNamespaces) == 0 {
		return nil
	}
	for _, c := range candidates {
		if c.Kind != graph.KindFunction && c.Kind != graph.KindMethod {
			continue
		}
		if _, ok := adlNamespaces[scopeMetaString(c.Meta, MetaScopeNamespace)]; ok {
			return c
		}
	}
	return nil
}

// preferJavaScopeCandidate — Java scope rule: an unqualified call
// inside class C must bind to a method declared on C (or inherited
// via extends/implements) before any other class. Static-imported
// methods + outer-class methods are tried in order before the
// generic cascade.
func (r *Resolver) preferJavaScopeCandidate(e *graph.Edge, caller *graph.Node, name string, candidates []*graph.Node) *graph.Node {
	enclosing := callerEnclosingClass(caller)
	if enclosing == "" {
		return nil
	}
	// A selector call whose receiver is typed as a DIFFERENT class is not an
	// enclosing-class call: `testee.triggerException()` must bind to the
	// receiver's type, not to a same-named method that merely happens to live
	// in the caller's own class. Only bare calls (no receiver type) and calls
	// on a receiver typed as the enclosing class itself use this rule; the
	// receiver-type passes own everything else.
	if rt := edgeReceiverType(e); rt != "" && rt != enclosing {
		return nil
	}
	// Pass 1: exact same enclosing class.
	for _, c := range candidates {
		if c.Kind != graph.KindMethod {
			continue
		}
		if scopeMetaString(c.Meta, MetaScopeClass) == enclosing {
			return c
		}
		if receiverEquals(c, enclosing) {
			return c
		}
	}
	// Pass 2: super-class chain. We follow EdgeExtends from the
	// enclosing class to walk up the inheritance tree until either a
	// matching method is found or the chain runs out.
	visited := map[string]struct{}{enclosing: {}}
	current := enclosing
	for hops := 0; hops < 8; hops++ {
		parent := r.javaParentClass(caller, current)
		if parent == "" {
			break
		}
		if _, seen := visited[parent]; seen {
			break
		}
		visited[parent] = struct{}{}
		for _, c := range candidates {
			if c.Kind != graph.KindMethod {
				continue
			}
			if scopeMetaString(c.Meta, MetaScopeClass) == parent {
				return c
			}
			if receiverEquals(c, parent) {
				return c
			}
		}
		current = parent
	}
	return nil
}

// preferPhpScopeCandidate — PHP scope rule: `parent::foo` walks the
// extends chain; `self::foo` is the enclosing class; `use function`
// aliases translate before search; unqualified calls in a namespace
// resolve in the same namespace before the global one. The edge's
// MetaScopeKind tells us which subroutine to run.
func (r *Resolver) preferPhpScopeCandidate(e *graph.Edge, caller *graph.Node, name string, candidates []*graph.Node) *graph.Node {
	switch scopeMetaString(e.Meta, MetaScopeKind) {
	case ScopeKindParent:
		parent, ok := r.phpParentID(strings.TrimSuffix(caller.ID, "."+caller.Name))
		if !ok {
			return nil
		}
		for _, c := range candidates {
			if c.Kind == graph.KindMethod && strings.TrimSuffix(c.ID, "."+c.Name) == parent {
				return c
			}
		}
		return nil
	case ScopeKindSelf:
		enclosing := callerEnclosingClass(caller)
		if enclosing == "" {
			return nil
		}
		for _, c := range candidates {
			if c.Kind != graph.KindMethod {
				continue
			}
			if scopeMetaString(c.Meta, MetaScopeClass) == enclosing ||
				receiverEquals(c, enclosing) {
				return c
			}
		}
		return nil
	}
	// Default: respect `use function` aliases + same-namespace
	// preference for unqualified calls.
	if alias := scopeUseAliases(e.Meta)[name]; alias != "" {
		ns, baseName := splitQualifiedFunctionName(alias)
		for _, c := range candidates {
			if c.Kind != graph.KindFunction {
				continue
			}
			if c.Name != baseName {
				continue
			}
			if ns == "" || scopeMetaString(c.Meta, MetaScopeNamespace) == ns {
				return c
			}
		}
	}
	callerNs := scopeMetaString(caller.Meta, MetaScopeNamespace)
	if callerNs == "" {
		return nil
	}
	for _, c := range candidates {
		if c.Kind != graph.KindFunction {
			continue
		}
		if scopeMetaString(c.Meta, MetaScopeNamespace) == callerNs {
			return c
		}
	}
	return nil
}

// callerEnclosingClass returns the enclosing class name for a caller
// node. Prefers the explicit MetaScopeClass stamp; falls back to the
// receiver field for method nodes so older indexes still work.
func callerEnclosingClass(caller *graph.Node) string {
	if cls := scopeMetaString(caller.Meta, MetaScopeClass); cls != "" {
		return cls
	}
	if caller.Kind == graph.KindMethod {
		return nodeReceiverType(caller)
	}
	return ""
}

// receiverEquals returns true when candidate is a method whose
// receiver type matches name.
func receiverEquals(candidate *graph.Node, name string) bool {
	if candidate.Kind != graph.KindMethod {
		return false
	}
	return nodeReceiverType(candidate) == name
}

// javaParentClass returns the Java parent class name for `child`, by
// looking at the child class's MetaScopeParentClass stamp on its
// graph node. Caller is just used to constrain the search to the
// caller's file/package when stamp data is incomplete.
func (r *Resolver) javaParentClass(caller *graph.Node, child string) string {
	for _, n := range r.graph.FindNodesByName(child) {
		if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
			continue
		}
		if n.Language != "java" {
			continue
		}
		if parent := scopeMetaString(n.Meta, MetaScopeParentClass); parent != "" {
			return parent
		}
	}
	return ""
}

// splitNamespaceFromQualifiedName splits a possibly-namespaced
// identifier into its (namespace, base) parts. "std::string" →
// "std", "App\Service\Foo" → "App\Service". Returns ""
// namespace for bare identifiers.
func splitNamespaceFromQualifiedName(name string) string {
	if i := strings.LastIndex(name, "::"); i >= 0 {
		return name[:i]
	}
	if i := strings.LastIndex(name, `\`); i >= 0 {
		return name[:i]
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[:i]
	}
	return ""
}

// splitQualifiedFunctionName splits a fully-qualified function name
// into (namespace, base). Mirrors splitNamespaceFromQualifiedName
// but also returns the base name.
func splitQualifiedFunctionName(name string) (ns, base string) {
	if i := strings.LastIndex(name, "::"); i >= 0 {
		return name[:i], name[i+2:]
	}
	if i := strings.LastIndex(name, `\`); i >= 0 {
		return name[:i], name[i+1:]
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}
