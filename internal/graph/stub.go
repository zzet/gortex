package graph

import (
	"strings"
)

// Stub-node identifier conventions.
//
// A "stub" is a placeholder Node the resolver materialises for a
// symbol the indexer can see referenced but not defined in the
// current repo's source: a stdlib call, a language builtin, an
// external module import, etc. Stubs let the graph hold edges
// to "external" targets uniformly with edges to first-party
// nodes.
//
// Format (all stubs):
//
//	<repoPrefix>::<kind>::<rest>
//
// where:
//
//	repoPrefix — the owning repo's RepoPrefix (Indexer.RepoPrefix).
//	             Empty only when the stub is created outside a
//	             per-repo context (legacy single-repo daemons).
//	kind       — one of: stdlib, builtin, external_call, module.
//	rest       — kind-specific (e.g. "fmt::Errorf" for stdlib).
//
// Why per-repo? Two repos pinned to different language SDK
// versions have semantically distinct stdlib symbols. Go 1.21's
// `min` is a builtin; in 1.20 it isn't. A global `builtin::go::min`
// node would conflate them and produce wrong cross-repo edges.
// Per-repo prefix keeps them as distinct nodes; a future
// "same-as" edge can union them when the workspace knows the
// versions actually match.
const (
	StubKindStdlib       = "stdlib"
	StubKindBuiltin      = "builtin"
	StubKindExternalCall = "external_call"
	StubKindModule       = "module"
)

// StubID composes a stub identifier with the per-repo prefix.
// Pass repoPrefix = "" when the caller is outside a per-repo
// context (single-repo daemons that haven't set a prefix).
func StubID(repoPrefix, kind string, parts ...string) string {
	var b strings.Builder
	if repoPrefix != "" {
		b.WriteString(repoPrefix)
		b.WriteString("::")
	}
	b.WriteString(kind)
	for _, p := range parts {
		b.WriteString("::")
		b.WriteString(p)
	}
	return b.String()
}

// IsStub reports whether id is any stub kind. Cheaper than
// StubKind when callers only need a yes/no.
func IsStub(id string) bool {
	return StubKind(id) != ""
}

// StubKind extracts the stub category (stdlib / builtin /
// external_call / module) from id. Returns "" if id is not a
// stub.
//
// Format dispatch:
//   - "<kind>::<rest>"               — legacy, no repo prefix
//   - "<repo>::<kind>::<rest>"       — per-repo prefix
//
// We match by looking for one of the known kind segments
// anywhere in the first two "::"-separated positions.
func StubKind(id string) string {
	for _, k := range stubKinds {
		// Without repo prefix: "<kind>::..."
		if strings.HasPrefix(id, k+"::") {
			return k
		}
	}
	// With repo prefix: "<repo>::<kind>::..."
	// Find the second "::" segment.
	first := strings.Index(id, "::")
	if first < 0 {
		return ""
	}
	rest := id[first+2:]
	for _, k := range stubKinds {
		if strings.HasPrefix(rest, k+"::") {
			return k
		}
	}
	return ""
}

// stubKinds is the closed set of stub categories. Ordered by
// expected frequency so the lookup loop bails early in the
// common case.
var stubKinds = []string{
	StubKindStdlib,
	StubKindExternalCall,
	StubKindBuiltin,
	StubKindModule,
}

// IsStdlibStub etc are convenience predicates that don't make
// the caller compare StubKind's return against a literal.
func IsStdlibStub(id string) bool  { return StubKind(id) == StubKindStdlib }
func IsBuiltinStub(id string) bool { return StubKind(id) == StubKindBuiltin }

// StubRest returns the kind-specific tail of a stub id (the
// portion after "<repo>::<kind>::" or "<kind>::"). Returns "" if
// id is not a stub. Useful for the "fmt::Errorf" portion of a
// stdlib stub when callers need to inspect the symbol identity.
func StubRest(id string) string {
	kind := StubKind(id)
	if kind == "" {
		return ""
	}
	prefix := kind + "::"
	if idx := strings.Index(id, prefix); idx >= 0 {
		return id[idx+len(prefix):]
	}
	return ""
}

// UnresolvedMarker is the prefix the extractor emits for a call/
// reference target the resolver still needs to bind to a concrete
// Node.
//
// Forms:
//
//	unresolved::Name                — legacy / single-repo
//	<repoPrefix>::unresolved::Name  — multi-repo COPY rewrite (in
//	                                   copyBulkLocked, to dodge
//	                                   cross-repo PK collisions)
//
// IsUnresolvedTarget / UnresolvedName / UnresolvedRepoPrefix
// normalise over both shapes so callers (resolver, MCP filters,
// data-flow tracker) don't have to know the encoding.
const UnresolvedMarker = "unresolved::"

// FnValuePlaceholderMarker is the sub-namespace the function-as-value capture
// gate (parser/languages/fn_value_capture.go) owns inside the unresolved
// space: a captured function value parks at `unresolved::fnvalue::<name>` until
// resolver.ResolveFnValueCallbacks binds it to a same-file definition. These
// placeholders are scaffolding for that gate alone — the master name resolver
// can never bind them — so the resolver pending scan (EdgesWithUnresolvedTarget)
// excludes the namespace via IsFnValuePlaceholder; the gate finds them itself
// by walking EdgesByKind(references).
const FnValuePlaceholderMarker = UnresolvedMarker + "fnvalue::"

// IsUnresolvedTarget reports whether id names an unresolved
// extractor stub in either the bare or the multi-repo form.
// StructuralEdgeTargetInvalid reports an edge whose kind can never
// legitimately point at a parameter or local node: nothing implements,
// extends, overrides, is a member of, or instantiates a `#param:`/`#local:`
// symbol. This is the store-level backstop for mapper bugs upstream — one
// mis-mapped interface object once fanned 130,250 implements edges onto a
// single `#param:ctx` node (57% of a workspace's implements set) before the
// pass-level gates existed. Both backends consult it at batch-write time and
// drop violations, so no current or future pass can persist the shape.
func StructuralEdgeTargetInvalid(kind EdgeKind, toID string) bool {
	switch kind {
	case EdgeImplements, EdgeExtends, EdgeOverrides, EdgeInstantiates, EdgeMemberOf:
	default:
		return false
	}
	return strings.Contains(toID, "#param:") || strings.Contains(toID, "#local:")
}

// FilterStructuralEdgeViolations is a pure partition. It copies only after a
// violation appears, returning both kept and rejected edges so the first write
// boundary can attribute every rejected attempt exactly once.
func FilterStructuralEdgeViolations(edges []*Edge) (kept, rejected []*Edge) {
	kept = edges
	for i, edge := range edges {
		if edge != nil && StructuralEdgeTargetInvalid(edge.Kind, edge.To) {
			if len(rejected) == 0 {
				kept = append(make([]*Edge, 0, len(edges)), edges[:i]...)
			}
			rejected = append(rejected, edge)
			continue
		}
		if len(rejected) > 0 {
			kept = append(kept, edge)
		}
	}
	return kept, rejected
}

func IsUnresolvedTarget(id string) bool {
	if id == "" {
		return false
	}
	if strings.HasPrefix(id, UnresolvedMarker) {
		return true
	}
	return strings.Contains(id, "::"+UnresolvedMarker)
}

// UnresolvedNameCandidateIDs returns every placeholder target id that a
// symbol's NAME owns — the ids that unbound call sites naming it park on.
// Four shapes, because a call site is either a free-function call
// (`unresolved::Foo`) or a member call (`unresolved::*.foo`), and either
// may carry the multi-repo `<repoPrefix>::` rewrite.
//
// Unresolved edges are indexed by their target string, so each id is a
// single reverse-edge lookup — GetInEdges over the returned slice is a
// bounded point query, never a scan.
//
// Only bare-name shapes are returned. A placeholder carrying more
// structure (`unresolved::extern::path::sym`, `import::`, `grpc::`) is
// owned by a dedicated resolver pass and holds evidence a name match does
// not; conflating the two would let a name coincidence masquerade as an
// import-grounded reference.
func UnresolvedNameCandidateIDs(n *Node) []string {
	if n == nil {
		return nil
	}
	prefix := n.RepoPrefix
	if prefix == "" {
		prefix = RepoPrefixOfID(n.ID)
	}
	return UnresolvedNameCandidateIDsForName(n.Name, prefix)
}

// UnresolvedNameCandidateIDsForName is the name-owned expansion behind
// UnresolvedNameCandidateIDs for callers that hold a bare name and repo
// prefix instead of a node. Every consumer that enumerates the stubs a name
// may be parked under must go through one of these two helpers: the four
// forms (bare, wildcard member, and their repo-prefixed twins) are a single
// contract, and a path that hand-builds a subset silently strands the
// missing forms as permanently pending.
func UnresolvedNameCandidateIDsForName(name, repoPrefix string) []string {
	if name == "" {
		return nil
	}
	ids := []string{
		UnresolvedMarker + name,
		UnresolvedMarker + "*." + name,
	}
	if repoPrefix == "" {
		return ids
	}
	return append(ids,
		repoPrefix+"::"+UnresolvedMarker+name,
		repoPrefix+"::"+UnresolvedMarker+"*."+name,
	)
}

// IsFnValuePlaceholder reports whether id is a fn-value gate placeholder in
// either the bare `unresolved::fnvalue::<name>` form or the multi-repo
// `<repoPrefix>::unresolved::fnvalue::<name>` COPY-rewrite form — mirroring
// IsUnresolvedTarget's two shapes. The fn-value gate owns this namespace; the
// resolver pending scan excludes it.
func IsFnValuePlaceholder(id string) bool {
	if id == "" {
		return false
	}
	if strings.HasPrefix(id, FnValuePlaceholderMarker) {
		return true
	}
	return strings.Contains(id, "::"+FnValuePlaceholderMarker)
}

// UnresolvedName returns the bare symbol name encoded in an
// unresolved target id, stripping the `unresolved::` prefix (and
// any leading `<repoPrefix>::`). Returns "" when id is not an
// unresolved stub.
func UnresolvedName(id string) string {
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, UnresolvedMarker) {
		return id[len(UnresolvedMarker):]
	}
	idx := strings.Index(id, "::"+UnresolvedMarker)
	if idx < 0 {
		return ""
	}
	return id[idx+len("::"+UnresolvedMarker):]
}

// UnresolvedRepoPrefix returns the per-repo prefix encoded in an
// unresolved target id, or "" if the id is bare or not an
// unresolved stub.
func UnresolvedRepoPrefix(id string) string {
	if id == "" || strings.HasPrefix(id, UnresolvedMarker) {
		return ""
	}
	idx := strings.Index(id, "::"+UnresolvedMarker)
	if idx <= 0 {
		return ""
	}
	return id[:idx]
}

// StubRepoPrefix returns the per-repo prefix of a stub id, or
// "" if the id has no prefix or isn't a stub.
func StubRepoPrefix(id string) string {
	kind := StubKind(id)
	if kind == "" {
		return ""
	}
	// If id starts with the kind directly, there's no repo prefix.
	if strings.HasPrefix(id, kind+"::") {
		return ""
	}
	if idx := strings.Index(id, "::"); idx > 0 {
		return id[:idx]
	}
	return ""
}

// IsResolvableRefEdge reports whether an edge of this kind is a
// symbol-level reference that the resolver binds from an
// `unresolved::<Name>` stub — calls, references, value reads/writes,
// type positions (typed_as / returns), and type hierarchy
// (implements / extends / composes / instantiates). These are the edges
// that must survive a definition's re-index as pending stubs rather than
// be dropped wholesale. Structural edges (contains / defines / member_of
// / imports / param_of) and enrichment edges (tests / provides / spawns
// / annotated / …) are not name-resolved and are excluded — re-stubbing
// them would only create edges nothing ever rebinds.
func IsResolvableRefEdge(k EdgeKind) bool {
	switch k {
	case EdgeCalls, EdgeReferences, EdgeReads, EdgeWrites,
		EdgeTypedAs, EdgeReturns, EdgeInstantiates,
		EdgeImplements, EdgeExtends, EdgeComposes:
		return true
	}
	return false
}

// IsReferenceableSymbol reports whether a node of this kind can be the
// target of a cross-file symbol reference — and thus the subject of
// reverse resolution by name. Excludes files, imports, packages,
// params, closures, locals, builtins, generic params, and the
// coverage / infra node kinds, none of which a caller binds to by bare
// name from an unresolved stub.
func IsReferenceableSymbol(k NodeKind) bool {
	switch k {
	case KindFunction, KindMethod, KindType, KindInterface,
		KindVariable, KindConstant, KindField, KindEnumMember:
		return true
	}
	return false
}

// ImportStubNamePrefix is the name-space of import-resolution pending
// stubs: an unresolved import edge targets
// `unresolved::import::<path>` (or the repo-prefixed COPY-rewrite form),
// so the receipt name that reaches that stub through
// UnresolvedNameCandidateIDsForName is `import::<path>`.
const ImportStubNamePrefix = "import::"

// ReceiptNamesForEvictedSymbol maps one evicted node to the receipt
// target names under which pending references to it may be parked, and
// reports whether that mapping is exact. This is the single authority
// for the eviction side of mutation receipts on every backend:
//
//   - a referenceable definition's pending references park under its
//     bare name and qualified name;
//   - a package is an import-resolution candidate (the resolver's
//     qualified-name tier considers it), so its pending references park
//     under the import-prefixed stub of its qualified name (and of its
//     bare name, for stem-form specifiers);
//   - any other kind carrying a qualified name also participates in the
//     resolver's qualified-name candidate lookup but has no exact stub
//     mapping yet — the receipt must fail closed rather than certify
//     the eviction as resolution-irrelevant.
//
// Extractors set QualName almost exclusively on referenceable symbols
// and packages, so the fail-closed branch is rare in practice.
//
// The remaining fall-through — a kind carrying no qualified name —
// reports (nil, exact), i.e. resolution-neutral. That is deliberate, and
// it is an approximation rather than a proof: import stubs can bind to a
// KindFile node (relative imports, Lua requires, Godot res:// paths), and
// a file node has no qualified name to derive a stub key from, so an
// eviction that collapses an import ambiguity onto a surviving candidate
// is not described here. Failing closed instead is not available: every
// file reindex evicts its own file node, so that branch would retire the
// exact path altogether. The residual gap is a pre-existing resolver
// ambiguity, and it is bounded by the same rule as every other name — a
// pending stub that could rebind is only reachable by name, never by the
// file frontier.
// EvictedNodeNeedsResolutionFrontier reports whether evicting a node of this
// kind can change resolution even though ReceiptNamesForEvictedSymbol offers
// no stub key for it. Exactly one kind qualifies: a file node is an import
// candidate - relative imports, Lua requires and Godot res:// paths all bind
// to one - so removing it can collapse an ambiguity for a pending import
// elsewhere, while the import specifier it would be parked under is not
// derivable from the file. Its file still has to reach the definition
// frontier, because ResolutionRelevant=false is the one verdict that skips
// the catch-up pass outright.
//
// Every other nameless kind really is neutral. Nothing binds by name to an
// import, param, local or module node, so an eviction that touches only those
// creates no resolution work.
func EvictedNodeNeedsResolutionFrontier(kind NodeKind) bool {
	return kind == KindFile
}

func ReceiptNamesForEvictedSymbol(kind NodeKind, name, qualName string) (names []string, exact bool) {
	switch {
	case IsReferenceableSymbol(kind):
		if name != "" {
			names = append(names, name)
		}
		if qualName != "" && qualName != name {
			names = append(names, qualName)
		}
		return names, true
	case kind == KindPackage:
		if qualName != "" {
			names = append(names, ImportStubNamePrefix+qualName)
		}
		if name != "" && name != qualName {
			names = append(names, ImportStubNamePrefix+name)
		}
		return names, true
	case qualName != "":
		return nil, false
	}
	return nil, true
}
