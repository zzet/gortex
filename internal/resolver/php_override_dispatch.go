package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// phpOverrideDispatchCap bounds how many overrides a single ambiguous PHP
// call may fan out to. A name shared by more definitions than this is too
// generic to attribute confidently, so the call is left ambiguous.
const phpOverrideDispatchCap = 8

// resolvePHPOverrideDispatch binds ambiguous PHP member / scoped calls that
// static resolution left unresolved, using the class hierarchy:
//
//   - scope_kind parent|self  → the enclosing class's ancestor chain is
//     walked to the nearest type declaring the method (a precise single
//     bind). This makes parent::__construct() resolve through a multi-level
//     extends chain, not only the direct parent.
//   - plain member calls       → when the same-name candidates form an
//     override family related through the hierarchy (a common interface /
//     abstract base / used trait), the call fans out to every override — the
//     fan-out-to-implementations semantics a language server presents.
//
// Edges land at the ast_inferred tier (visible in default find_usages, unlike
// the speculative Java analogue) because the PHP dispatch surface is the
// primary recall source for the language and the relatedness gate keeps
// precision high: an untyped receiver whose candidates share no common
// ancestor is left unresolved rather than sprayed repo-wide. Runs AFTER the
// cross-package guard so its edges are never reverted. PHP-only.
func (r *Resolver) resolvePHPOverrideDispatch() int {
	return r.resolvePHPOverrideDispatchCandidates(r.collectOverrideDispatchCandidates())
}

func (r *Resolver) resolvePHPOverrideDispatchCandidates(candidates overrideDispatchCandidates) int {
	g := r.graph
	if g == nil {
		return 0
	}
	direct, closure := r.phpTypeHierarchy()
	if len(direct) == 0 {
		return 0
	}

	type job struct {
		edge   *graph.Edge
		single *graph.Node // scope-bind target; nil for a fan-out job
		base   *graph.Node
		others []*graph.Node
	}
	var jobs []job

	// Collect first — mutating the graph while ranging EdgesByKind is unsafe.
	for e := range candidates.phpEdges(g) {
		if e == nil || e.Kind != graph.EdgeCalls || e.IsSpeculative() {
			continue
		}
		// Scoped warm pass: an unchanged repo's calls were already dispatched (or
		// left ambiguous) by a prior full pass over the same hierarchy, so only
		// reconsider the changed repos' calls.
		if !r.edgeFromInScope(e.From) {
			continue
		}
		name := javaUnresolvedMemberName(e.To)
		if name == "" || strings.HasSuffix(name, ".<init>") {
			continue
		}
		// SQLite joined caller language in the compact census. Only scope-chain
		// binding needs the full caller node; adapters without exact lookup retain
		// the historical language gate here.
		var caller *graph.Node
		if !candidates.exact {
			caller = r.cachedGetNode(e.From)
			if caller == nil || caller.Language != "php" {
				continue
			}
		}
		repo := r.callerRepoPrefix(e)

		// Scope path: parent:: / self:: / static:: bind precisely up the
		// enclosing class's ancestor chain.
		if sk := phpEdgeMetaString(e, "scope_kind"); sk == "parent" || sk == "self" {
			if caller == nil {
				caller = r.cachedGetNode(e.From)
			}
			if caller == nil || caller.Language != "php" {
				continue
			}
			encloser := phpEnclosingClass(caller)
			if encloser == "" {
				continue
			}
			var start []string
			if sk == "parent" {
				for p := range direct[encloser] {
					start = append(start, p)
				}
			} else {
				start = []string{encloser}
			}
			if len(start) == 0 {
				continue
			}
			if target := r.nearestPHPMethod(name, start, direct, repo); target != nil && target.ID != e.From {
				jobs = append(jobs, job{edge: e, single: target})
			}
			continue
		}

		// Receiver-directed path: the extractor typed the receiver
		// (`$this->m()`, a typed property / parameter, `$v = new Foo`,
		// `Foo::m()`), so the call binds to the method that type declares —
		// or, when the type inherits it, to the nearest ancestor declaring
		// it. This is a single precise bind, not a fan-out: the receiver's
		// static type is stated by the source, so there is nothing to guess.
		// The generic resolver already binds the case where the receiver type
		// declares the method itself; this pass exists for the inherited case,
		// which a same-receiver name match cannot see.
		if rt := edgeReceiverType(e); rt != "" {
			if base := phpBaseTypeName(rt); base != "" {
				if target := r.nearestPHPMethod(name, []string{base}, direct, repo); target != nil && target.ID != e.From {
					jobs = append(jobs, job{edge: e, single: target})
				}
			}
			// A typed receiver that resolved to nothing stays unresolved: a
			// name-only fan-out would contradict the type the source declared.
			continue
		}

		// Fan-out path: a plain member call whose same-name candidates are an
		// override family related through the hierarchy.
		cands := phpOverrideCandidates(r.cachedFindNodesByNameInRepo(name, repo))
		if len(cands) < 2 || len(cands) > phpOverrideDispatchCap {
			continue
		}
		if !javaOverridesRelated(cands, closure) {
			continue
		}
		jobs = append(jobs, job{edge: e, base: cands[0], others: cands[1:]})
	}

	n := 0
	for _, j := range jobs {
		if j.single != nil {
			if j.edge.To == j.single.ID {
				continue
			}
			oldTo := j.edge.To
			j.edge.To = j.single.ID
			j.edge.Origin = graph.OriginASTInferred
			if j.edge.Confidence < phpDispatchConfidence {
				j.edge.Confidence = phpDispatchConfidence
			}
			if edgeReceiverType(j.edge) != "" {
				phpEnsureMeta(j.edge)["dispatch"] = "receiver"
			} else {
				phpEnsureMeta(j.edge)["dispatch"] = "scope"
			}
			g.ReindexEdges([]graph.EdgeReindex{{Edge: j.edge, OldTo: oldTo}})
			n++
			continue
		}
		oldTo := j.edge.To
		j.edge.To = j.base.ID
		j.edge.Origin = graph.OriginASTInferred
		j.edge.Confidence = phpDispatchConfidence
		phpEnsureMeta(j.edge)["dispatch"] = "override"
		g.ReindexEdges([]graph.EdgeReindex{{Edge: j.edge, OldTo: oldTo}})
		n++
		for _, o := range j.others {
			if phpEdgeExists(g, j.edge.From, o.ID) {
				continue // idempotent: the extra override edge is already present
			}
			g.AddEdge(&graph.Edge{
				From: j.edge.From, To: o.ID, Kind: graph.EdgeCalls,
				FilePath: j.edge.FilePath, Line: j.edge.Line,
				Origin:     graph.OriginASTInferred,
				Confidence: phpDispatchConfidence,
				Meta:       map[string]any{"dispatch": "override"},
			})
			n++
		}
	}
	return n
}

// phpDispatchConfidence is the confidence stamped on a dispatch-resolved edge
// — in the ast_inferred band so DefaultOriginFor agrees with the explicit
// Origin and the read path keeps the edge visible.
const phpDispatchConfidence = 0.7

// phpTypeHierarchy builds the PHP class hierarchy: `direct` maps each type's
// simple name to its DIRECT parents (superclass via scope_parent, implemented
// / extended interfaces via scope_interfaces, used traits via EdgeExtends),
// and `closure` is the transitive ancestor set used by the relatedness gate.
func (r *Resolver) phpTypeHierarchy() (direct, closure map[string]map[string]bool) {
	g := r.graph
	direct = map[string]map[string]bool{}
	add := func(child, parent string) {
		child = phpBaseTypeName(child)
		parent = phpBaseTypeName(parent)
		if child == "" || parent == "" || child == parent {
			return
		}
		set := direct[child]
		if set == nil {
			set = map[string]bool{}
			direct[child] = set
		}
		set[parent] = true
	}
	for _, kind := range []graph.NodeKind{graph.KindType, graph.KindInterface} {
		for n := range g.NodesByKind(kind) {
			if n == nil || n.Language != "php" || n.Name == "" || n.Meta == nil {
				continue
			}
			if p, ok := n.Meta[MetaScopeParentClass].(string); ok {
				add(n.Name, p)
			}
			if ifaces, ok := n.Meta["scope_interfaces"].(string); ok && ifaces != "" {
				for _, iface := range strings.Split(ifaces, ",") {
					add(n.Name, iface)
				}
			}
		}
	}
	// Trait `use` (and any other extends-shaped) relationship is a graph edge.
	for e := range graph.EdgesLightSeq(g, graph.EdgeExtends) {
		if e == nil {
			continue
		}
		from := r.cachedGetNode(e.From)
		if from == nil || from.Language != "php" || from.Name == "" {
			continue
		}
		add(from.Name, phpEdgeTargetName(g, e.To))
	}
	if len(direct) == 0 {
		return direct, nil
	}
	closure = make(map[string]map[string]bool, len(direct))
	var visit func(t string, acc, seen map[string]bool)
	visit = func(t string, acc, seen map[string]bool) {
		for p := range direct[t] {
			if seen[p] {
				continue
			}
			seen[p] = true
			acc[p] = true
			visit(p, acc, seen)
		}
	}
	for t := range direct {
		acc := map[string]bool{}
		visit(t, acc, map[string]bool{t: true})
		closure[t] = acc
	}
	return direct, closure
}

// nearestPHPMethod walks up the hierarchy from start (breadth-first, nearest
// first) to the first ancestor type declaring method name, returning that
// method node. Used by the parent::/self:: scope bind.
func (r *Resolver) nearestPHPMethod(name string, start []string, direct map[string]map[string]bool, repo string) *graph.Node {
	byRecv := map[string]*graph.Node{}
	for _, m := range r.cachedFindNodesByNameInRepo(name, repo) {
		if m == nil || m.Language != "php" || m.Kind != graph.KindMethod {
			continue
		}
		if graph.IsStub(m.ID) || graph.IsUnresolvedTarget(m.ID) {
			continue
		}
		if rc := nodeReceiverType(m); rc != "" {
			if _, ok := byRecv[rc]; !ok {
				byRecv[rc] = m
			}
		}
	}
	if len(byRecv) == 0 {
		return nil
	}
	visited := map[string]bool{}
	queue := append([]string{}, start...)
	for _, s := range start {
		visited[s] = true
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if m := byRecv[cur]; m != nil {
			return m
		}
		for p := range direct[cur] {
			if !visited[p] {
				visited[p] = true
				queue = append(queue, p)
			}
		}
	}
	return nil
}

// phpOverrideCandidates filters name-matched nodes to in-repo PHP method
// definitions, one per declaring type (deduped by receiver), excluding stubs
// and definitions with no declaring type.
func phpOverrideCandidates(raw []*graph.Node) []*graph.Node {
	var out []*graph.Node
	seen := map[string]bool{}
	for _, n := range raw {
		if n == nil || n.Language != "php" || n.Kind != graph.KindMethod {
			continue
		}
		if graph.IsStub(n.ID) || graph.IsUnresolvedTarget(n.ID) {
			continue
		}
		recv := nodeReceiverType(n)
		if recv == "" || seen[recv] {
			continue
		}
		seen[recv] = true
		out = append(out, n)
	}
	return out
}

// phpEnclosingClass returns the simple name of the class a caller node belongs
// to (its receiver / scope_class), or "" for a free function.
func phpEnclosingClass(caller *graph.Node) string {
	if caller == nil || caller.Meta == nil {
		return ""
	}
	if r, ok := caller.Meta["receiver"].(string); ok && r != "" {
		return phpBaseTypeName(r)
	}
	if r, ok := caller.Meta["scope_class"].(string); ok && r != "" {
		return phpBaseTypeName(r)
	}
	return ""
}

// phpEdgeExists reports whether a call edge from→to already exists (used to
// keep the fan-out idempotent across re-resolution).
func phpEdgeExists(g graph.Store, from, to string) bool {
	for _, e := range g.GetOutEdges(from) {
		if e != nil && e.To == to && e.Kind == graph.EdgeCalls {
			return true
		}
	}
	return false
}

// phpEdgeTargetName resolves an edge target id to a simple type name, handling
// both an unresolved::<name> stub and a resolved node id.
func phpEdgeTargetName(g graph.Store, to string) string {
	if graph.IsUnresolvedTarget(to) {
		return graph.UnresolvedName(to)
	}
	if n := g.GetNode(to); n != nil {
		return n.Name
	}
	return graph.UnresolvedName(to)
}

// phpBaseTypeName reduces a possibly namespace-qualified PHP type reference to
// its simple name (`App\Service\User` → `User`, `\Foo` → `Foo`).
func phpBaseTypeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, `\`)
	if i := strings.LastIndexByte(s, '\\'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// phpEdgeMetaString reads a string-valued edge meta key, "" when absent.
func phpEdgeMetaString(e *graph.Edge, key string) string {
	if e == nil || e.Meta == nil {
		return ""
	}
	if s, ok := e.Meta[key].(string); ok {
		return s
	}
	return ""
}

// phpEnsureMeta returns the edge's meta map, allocating it when nil.
func phpEnsureMeta(e *graph.Edge) map[string]any {
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	return e.Meta
}

// phpTypedReceiverCaller reports whether a call came from PHP source, so the
// resolver knows a stated receiver_type should be honoured by the hierarchy
// walk instead of guessed at by directory adjacency. Scoped to PHP because the
// locality fallback is correct for languages whose directory is a real package
// boundary; PHP's is not.
func phpTypedReceiverCaller(caller *graph.Node) bool {
	return caller != nil && caller.Language == "php"
}

// phpNarrowByTargetFQN filters same-named type candidates down to the ones
// declared under the namespace the reference actually names. The PHP extractor
// stamps target_fqn by resolving the written name through the file's `use`
// imports and its own namespace, which is precisely PHP's own class-name
// resolution rule.
//
// Returns nil when the edge carries no fully-qualified target, when the
// target_fqn is not PHP-shaped (the C# extractor stamps the same key with
// dotted namespaces — see csharpNarrowByNamespace), or when no candidate
// matches — callers treat nil as "keep the candidates you had", so a
// reference into a vendor namespace the graph does not hold degrades to
// the previous ranking instead of losing its edge.
func phpNarrowByTargetFQN(e *graph.Edge, candidates []*graph.Node) []*graph.Node {
	fqn := phpEdgeMetaString(e, "target_fqn")
	if fqn == "" || len(candidates) < 2 {
		return nil
	}
	// A dotted qualifier without a backslash is a C# spelling, never PHP —
	// without this gate it would read as namespace "" and narrow onto a
	// global-namespace PHP class of the same name.
	if strings.Contains(fqn, ".") && !strings.Contains(fqn, `\`) {
		return nil
	}
	ns := ""
	if i := strings.LastIndex(fqn, `\`); i >= 0 {
		ns = fqn[:i]
	}
	var out []*graph.Node
	for _, c := range candidates {
		if c != nil && c.Language == "php" && phpNodeNamespace(c) == ns {
			out = append(out, c)
		}
	}
	return out
}

// phpAliasTypeCandidates handles a PHP type written under a use alias. Unlike
// ordinary namespace narrowing, an alias must never fall back to its written
// name: that name can belong to an unrelated class. The bool keeps a missing
// external target distinct from a reference that is not aliased.
func (r *Resolver) phpAliasTypeCandidates(e *graph.Edge, name, repo string) ([]*graph.Node, bool) {
	fqn := phpEdgeMetaString(e, "target_fqn")
	if fqn == "" || (strings.Contains(fqn, ".") && !strings.Contains(fqn, `\`)) {
		return nil, false
	}
	short := fqn
	if i := strings.LastIndex(fqn, `\`); i >= 0 {
		short = fqn[i+1:]
	}
	if short == name {
		return nil, false
	}
	var out []*graph.Node
	for _, c := range r.phpFindByFQN(fqn, repo) {
		if c.Kind == graph.KindType || c.Kind == graph.KindInterface {
			out = append(out, c)
		}
	}
	return out, true
}

// phpFindByFQN returns the PHP declarations whose namespace and name match a
// fully-qualified name. Used where a lookup by Node.QualName would be natural
// but is not available: qual_name backs a UNIQUE index that a duplicate FQN
// fails outright, so PHP carries its namespace in Meta instead.
func (r *Resolver) phpFindByFQN(fqn, repo string) []*graph.Node {
	name, ns := fqn, ""
	if i := strings.LastIndex(fqn, `\`); i >= 0 {
		ns, name = fqn[:i], fqn[i+1:]
	}
	if name == "" {
		return nil
	}
	var out []*graph.Node
	for _, c := range r.cachedFindNodesByNameInRepo(name, repo) {
		if c == nil || c.Language != "php" {
			continue
		}
		switch c.Kind {
		case graph.KindType, graph.KindInterface, graph.KindFunction:
		default:
			continue
		}
		if phpNodeNamespace(c) == ns {
			out = append(out, c)
		}
	}
	return out
}

// phpNodeNamespace reads the namespace a PHP declaration lives in.
func phpNodeNamespace(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	s, _ := n.Meta[MetaScopeNamespace].(string)
	return s
}
