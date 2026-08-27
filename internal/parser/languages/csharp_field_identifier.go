package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// C# field-identifier usage edges.
//
// Fields are read through bare identifiers, not dotted access: an
// injected field is a call receiver (`_store.Add(1)`), an access
// receiver (`_store.Count`), or an assignment target (`_store = store`).
// The member-access emitter records the accessed MEMBER and the call
// path records the called METHOD, but neither position emitted any edge
// naming the field itself — so find_usages on a field answered empty no
// matter how often the class used it, and only the unidiomatic
// `this._store` spelling left a trace. This emitter closes that gap for
// the enclosing type's own fields: the one case where the implicit
// receiver is known at extraction time (it is `this`), so the edge can
// carry receiver_type evidence and ride the exact resolution path
// this.-qualified accesses already use.
//
// Shadowing: a declared local, a parameter, or a builtin-typed local
// with the field's name owns the identifier inside that method — no
// field edge is minted there. Deliberately deferred (named remainder):
// bare identifiers in argument/return position (`Frob(_store)`), which
// need a wider capture surface than the receiver/assignment positions
// covered here.

// csharpDeferredFieldAssign buffers one bare-identifier assignment
// target (`_store = ...`) until the shadow indexes exist.
type csharpDeferredFieldAssign struct {
	name string
	line int
}

// csharpFieldNamesByType indexes the file's declared field names per
// enclosing type name, from the field nodes the extractor already
// minted (ID form `<file>::<Type>.<field>`).
func csharpFieldNamesByType(result *parser.ExtractionResult) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, n := range result.Nodes {
		if n == nil || n.Kind != graph.KindField {
			continue
		}
		_, symbolPart, ok := strings.Cut(n.ID, "::")
		if !ok {
			continue
		}
		dot := strings.LastIndex(symbolPart, ".")
		if dot <= 0 {
			continue
		}
		typeName := symbolPart[:dot]
		// Nested/namespaced owners keep only the innermost type name —
		// csharpOwnerTypeName applies the same trim to method owners, so
		// the two lookups meet on the same key.
		if inner := strings.LastIndex(typeName, "."); inner >= 0 {
			typeName = typeName[inner+1:]
		}
		m := out[typeName]
		if m == nil {
			m = map[string]bool{}
			out[typeName] = m
		}
		m[n.Name] = true
	}
	return out
}

// csharpBareIdentifier reports whether a captured receiver text is a
// single bare identifier — no chain, no call, no keyword.
func csharpBareIdentifier(text string) bool {
	if text == "" || text == "this" || text == "base" {
		return false
	}
	return !strings.ContainsAny(text, ".()?[] \t\n")
}

// emitCSharpFieldIdentifierUses emits EdgeReads/EdgeWrites for bare
// identifiers that name a field of the enclosing type, from the three
// covered positions: call receivers, member-access receivers, and
// assignment targets.
func emitCSharpFieldIdentifierUses(
	calls []csharpDeferredCall, accesses []csharpDeferredAccess,
	fieldAssigns []csharpDeferredFieldAssign,
	src []byte, filePath string, funcRanges *csharpFuncLookup,
	localNamesByOwner map[string]map[string]bool,
	builtinsByOwner map[string]map[string]string,
	result *parser.ExtractionResult,
) {
	fieldsByType := csharpFieldNamesByType(result)
	if len(fieldsByType) == 0 {
		return
	}
	paramsByOwner := csharpParamNamesByOwner(result)

	// eligible resolves the enclosing owner and reports whether name is
	// an unshadowed field of the owner's type.
	eligible := func(line int, name string) (owner, ownerType string, ok bool) {
		owner = funcRanges.enclosing(line)
		if owner == "" {
			return "", "", false
		}
		ownerType = csharpOwnerTypeName(owner)
		if ownerType == "" || !fieldsByType[ownerType][name] {
			return "", "", false
		}
		if paramsByOwner[owner][name] || localNamesByOwner[owner][name] ||
			builtinsByOwner[owner][name] != "" {
			return "", "", false
		}
		return owner, ownerType, true
	}

	// One edge per (owner, name, line, kind): a site seen through two
	// buffers (a call's receiver and the same node's member access) must
	// not double-emit.
	type siteKey struct {
		owner string
		name  string
		line  int
		kind  graph.EdgeKind
	}
	seen := map[siteKey]bool{}
	emit := func(line int, name string, kind graph.EdgeKind) {
		owner, ownerType, ok := eligible(line, name)
		if !ok {
			return
		}
		k := siteKey{owner, name, line, kind}
		if seen[k] {
			return
		}
		seen[k] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From: owner, To: "unresolved::*." + name,
			Kind: kind, FilePath: filePath, Line: line,
			Meta: map[string]any{"receiver_type": ownerType},
		})
	}

	for _, c := range calls {
		// this./base.-qualified calls carry recvType and no bare
		// receiver; member calls through a field carry the identifier.
		if !c.isMember || c.recvType != "" || !csharpBareIdentifier(c.receiver) {
			continue
		}
		emit(c.line, c.receiver, graph.EdgeReads)
	}

	for _, a := range accesses {
		if a.node == nil {
			continue
		}
		recv := a.node.ChildByFieldName("expression")
		if a.conditional {
			recv = a.node.ChildByFieldName("condition")
		}
		if recv == nil || recv.Type() != "identifier" {
			continue
		}
		// Call-position accesses are covered through the calls buffer;
		// skipping them here keeps one read per site.
		if csharpAccessInCallPosition(a.node) {
			continue
		}
		emit(a.line, recv.Content(src), graph.EdgeReads)
	}

	for _, fa := range fieldAssigns {
		emit(fa.line, fa.name, graph.EdgeWrites)
	}
}
