package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/php"
)

// PHP namespace identity.
//
// Every PHP symbol was named and ID'd by its bare short name, and every type
// reference was lowered to a bare name too, so `App\Models\User` and
// `App\Entities\User` were the same symbol to every name-based lookup. On
// laravel/framework that is not a corner case: 31.8% of type-directed edges
// land on a short name more than one class defines (`Post` names 37 classes,
// `User` 27, `Builder` 5 across Contracts / Eloquent / Query).
//
// The resolver was already built for this. internal/resolver/scope.go
// documents MetaScopeNamespace ("scope_ns") with a PHP example and
// MetaScopeUseAliases as a PHP-only key, and preferPhpScopeCandidate reads
// both — the branches were unreachable because nothing ever stamped them.
//
// This pass is additive: node IDs do not change. Each symbol gains the
// namespace it is declared in, types and functions gain a fully-qualified
// QualName, and each type reference gains the fully-qualified name the source
// says it names, resolved through the file's `use` imports. The resolver uses
// that to narrow same-named candidates and falls back to the full set when
// nothing matches, so the change can only sharpen a binding, never lose one.

// phpNsRange is one namespace declaration's line span. A file may hold several
// braced `namespace X { ... }` blocks; the unbraced form runs to the next
// declaration or to end of file.
type phpNsRange struct {
	name  string
	start int
	end   int
}

// phpNameResolver answers "what fully-qualified name does this source text
// mean, written at this line of this file".
type phpNameResolver struct {
	ranges []phpNsRange
	// imports maps the name a file refers to a symbol by — the last segment,
	// or the `as` alias — to its fully-qualified name.
	imports map[string]string
	// funcAliases holds only the `use function X as y` entries, which the
	// resolver's scope_use_aliases contract keeps separate from class imports.
	funcAliases map[string]string
}

// PHPClassNameResolver resolves class references through a file's namespaces and
// use imports. The returned resolver owns only strings; no tree is retained.
func PHPClassNameResolver(src []byte) func(string, int) string {
	tree, err := parser.ParseFile(src, php.GetLanguage())
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()
	return newPHPNameResolver(tree.RootNode(), src).resolve
}

// newPHPNameResolver reads a file's namespace declarations and `use` imports.
func newPHPNameResolver(root *sitter.Node, src []byte) *phpNameResolver {
	r := &phpNameResolver{imports: map[string]string{}, funcAliases: map[string]string{}}
	if root == nil {
		return r
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "namespace_definition":
			name := ""
			for i, c := 0, int(n.NamedChildCount()); i < c; i++ {
				if ch := n.NamedChild(i); ch.Type() == "namespace_name" {
					name = strings.Trim(strings.TrimSpace(ch.Content(src)), `\`)
					break
				}
			}
			r.ranges = append(r.ranges, phpNsRange{
				name:  name,
				start: int(n.StartPoint().Row) + 1,
				end:   int(n.EndPoint().Row) + 1,
			})
		case "namespace_use_declaration":
			r.readUseDeclaration(n, src)
		}
		for i, c := 0, int(n.NamedChildCount()); i < c; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	return r
}

// readUseDeclaration records every name a `use` statement brings into the file,
// including the braced group form and `as` aliases.
func (r *phpNameResolver) readUseDeclaration(node *sitter.Node, src []byte) {
	declKind := phpUseQualifier(node, src)

	record := func(clause *sitter.Node, prefix string) {
		var nameNode *sitter.Node
		for _, t := range []string{"qualified_name", "namespace_name", "name"} {
			for i, c := 0, int(clause.NamedChildCount()); i < c; i++ {
				if ch := clause.NamedChild(i); ch.Type() == t {
					nameNode = ch
					break
				}
			}
			if nameNode != nil {
				break
			}
		}
		if nameNode == nil {
			return
		}
		fqn := strings.Trim(strings.TrimSpace(nameNode.Content(src)), `\`)
		if prefix != "" {
			fqn = strings.TrimRight(prefix, `\`) + `\` + fqn
		}
		if fqn == "" {
			return
		}
		local := fqn
		if i := strings.LastIndex(local, `\`); i >= 0 {
			local = local[i+1:]
		}
		if a := clause.ChildByFieldName("alias"); a != nil {
			if alias := strings.TrimSpace(a.Content(src)); alias != "" {
				local = alias
			}
		}
		kind := declKind
		if k := phpUseQualifier(clause, src); k != "" {
			kind = k
		}
		if kind == "function" {
			r.funcAliases[local] = fqn
			return
		}
		if kind == "const" {
			return
		}
		r.imports[strings.ToLower(local)] = fqn
	}

	if group := node.ChildByFieldName("body"); group != nil {
		prefix := ""
		for i, c := 0, int(node.NamedChildCount()); i < c; i++ {
			if ch := node.NamedChild(i); ch.Type() == "namespace_name" {
				prefix = strings.TrimSpace(ch.Content(src))
				break
			}
		}
		for i, c := 0, int(group.NamedChildCount()); i < c; i++ {
			if clause := group.NamedChild(i); clause.Type() == "namespace_use_clause" {
				record(clause, prefix)
			}
		}
		return
	}
	for i, c := 0, int(node.NamedChildCount()); i < c; i++ {
		if clause := node.NamedChild(i); clause.Type() == "namespace_use_clause" {
			record(clause, "")
		}
	}
}

// namespaceAt returns the namespace in effect at a source line, or "" for the
// global namespace. The innermost enclosing declaration wins; a file whose
// namespace is declared without braces covers every line at or after it.
func (r *phpNameResolver) namespaceAt(line int) string {
	best := ""
	bestStart := -1
	for _, rg := range r.ranges {
		if line < rg.start {
			continue
		}
		// A braced block ends at its closing brace; an unbraced declaration's
		// node ends on its own line, so it covers everything after it that no
		// later declaration claims.
		if line > rg.end && rg.end > rg.start {
			continue
		}
		if rg.start > bestStart {
			best, bestStart = rg.name, rg.start
		}
	}
	return best
}

// resolve turns a type reference as written into a fully-qualified name.
// Mirrors PHP's name resolution for class references: a leading `\` is already
// absolute; a qualified name's first segment may itself be imported; a bare
// name is an import if the file imports it, and otherwise names a class in the
// current namespace. Returns "" when nothing can be said.
func (r *phpNameResolver) resolve(raw string, line int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, `\`) {
		return strings.TrimPrefix(raw, `\`)
	}
	head, rest := raw, ""
	if i := strings.Index(raw, `\`); i >= 0 {
		head, rest = raw[:i], raw[i:]
	}
	if fqn, ok := r.imports[strings.ToLower(head)]; ok {
		return fqn + rest
	}
	ns := r.namespaceAt(line)
	if ns == "" {
		return raw
	}
	return ns + `\` + raw
}

// phpNamespaceScopedKinds are the declaration kinds that live in a namespace
// and so carry scope_ns. Fields and enum members belong to a type, not
// directly to a namespace, but carrying it keeps a member's owner recoverable
// without a second lookup.
func phpNamespaceScopedKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindType, graph.KindInterface, graph.KindFunction,
		graph.KindMethod, graph.KindConstant, graph.KindField, graph.KindEnumMember:
		return true
	}
	return false
}

// applyPHPNamespaceIdentity stamps every PHP declaration with the namespace it
// lives in and records on each type-reference edge the fully-qualified name the
// source names.
//
// Deliberately NOT Node.QualName. Qualified-name persistence now tolerates
// duplicates, but the compatibility lookup API still returns one representative.
// PHP produces duplicate fully-qualified names in practice — indexing
// laravel/framework mints six, where a class and the annotation stub for a
// same-named attribute land in one namespace — so the FQN lives in scope_ns,
// which preserves the complete candidate set. Everything that would have used
// QualName (reference narrowing, import binding) matches on scope_ns instead.
func applyPHPNamespaceIdentity(root *sitter.Node, src []byte, result *parser.ExtractionResult) {
	if result == nil || root == nil {
		return
	}
	nr := newPHPNameResolver(root, src)

	for _, n := range result.Nodes {
		if n == nil || n.Language != "php" || !phpNamespaceScopedKind(n.Kind) {
			continue
		}
		ns := nr.namespaceAt(n.StartLine)
		if ns == "" {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["scope_ns"] = ns
	}

	// `use function NS\foo as bar` — the resolver's scope_use_aliases contract
	// translates the alias before searching. Stamped on call edges only, which
	// is where the resolver reads it.
	aliases := phpEncodeUseAliases(nr.funcAliases)

	for _, e := range result.Edges {
		if e == nil {
			continue
		}
		if rt, _ := e.Meta["receiver_type"].(string); rt != "" {
			// Keep the public short-name hint, but retain class identity for
			// calls before the resolver can choose an unrelated homonym.
			e.Meta["receiver_type"] = canonicalizePHPTypeRef(rt)
			if e.Kind == graph.EdgeCalls && strings.HasPrefix(e.To, "unresolved::*.") &&
				e.Meta["facade"] == nil && e.Meta["via"] != "constructor" {
				fqn := nr.resolve(rt, e.Line)
				if fqn != rt || strings.Contains(rt, `\`) || nr.namespaceAt(e.Line) != "" || len(nr.imports) > 0 {
					e.Meta["receiver_fqn"] = fqn
				}
			}
		}
		if aliases != "" && e.Kind == graph.EdgeCalls {
			if e.Meta == nil {
				e.Meta = map[string]any{}
			}
			e.Meta["scope_use_aliases"] = aliases
		}
		if !phpTypeDirectedEdge(e.Kind) || !graph.IsUnresolvedTarget(e.To) {
			continue
		}
		name := graph.UnresolvedName(e.To)
		// Member-shaped targets (`*.foo`) name a member, not a type.
		if name == "" || strings.HasPrefix(name, "*.") || strings.HasPrefix(name, "import::") {
			continue
		}
		// Prefer the form the source wrote — canonicalisation reduced a
		// qualified reference to its last segment.
		written := name
		if raw, ok := e.Meta[phpRawTypeRefKey].(string); ok && raw != "" {
			written = raw
			delete(e.Meta, phpRawTypeRefKey)
		}
		fqn := nr.resolve(written, e.Line)
		// Receiver ancestry also needs explicit global names: a resolved
		// short-name parent edge may already point at an unrelated namespace.
		if fqn == "" || (fqn == name && e.Kind != graph.EdgeExtends && e.Kind != graph.EdgeImplements) {
			continue
		}
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["target_fqn"] = fqn
	}
}

// phpTypeDirectedEdge reports whether an edge kind names a type at its target,
// so a fully-qualified name can be recorded for it.
func phpTypeDirectedEdge(k graph.EdgeKind) bool {
	switch k {
	case graph.EdgeInstantiates, graph.EdgeExtends, graph.EdgeImplements,
		graph.EdgeTypedAs, graph.EdgeReferences, graph.EdgeReturns:
		return true
	}
	return false
}

// phpEncodeUseAliases renders the `alias=>target` payload the resolver's
// scopeUseAliases decoder expects. Sorted so the value is deterministic.
func phpEncodeUseAliases(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(k)
		b.WriteString("=>")
		b.WriteString(m[k])
	}
	return b.String()
}

// phpStampRawTypeRef records the namespace-qualified form the source wrote for
// a type reference, when canonicalisation to the bare class name lost it.
// applyPHPNamespaceIdentity consumes and removes the key, so it never reaches
// the graph — without it a written `\App\Core\Base` would be qualified as if it
// had been the unqualified `Base`.
func phpStampRawTypeRef(edge *graph.Edge, raw, canon string) {
	raw = strings.TrimSpace(raw)
	if edge == nil || raw == "" || raw == canon || !strings.Contains(raw, `\`) {
		return
	}
	if edge.Meta == nil {
		edge.Meta = map[string]any{}
	}
	edge.Meta[phpRawTypeRefKey] = raw
}

// phpRawTypeRefKey is scratch state between the reference-form pass and the
// namespace pass; it is deleted before the result leaves the extractor.
const phpRawTypeRefKey = "php_raw_type"
