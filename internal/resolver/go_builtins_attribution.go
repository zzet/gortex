package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// goBuiltinFuncs is the complete set of pre-declared Go built-in
// functions. Source: https://pkg.go.dev/builtin (functions section).
// Kept in sync with the language spec — when a new builtin lands
// (e.g. clear / min / max in Go 1.21) add it here.
var goBuiltinFuncs = map[string]struct{}{
	"append": {}, "cap": {}, "clear": {}, "close": {}, "complex": {},
	"copy": {}, "delete": {}, "imag": {}, "len": {}, "make": {},
	"max": {}, "min": {}, "new": {}, "panic": {}, "print": {},
	"println": {}, "real": {}, "recover": {},
}

// goBuiltinTypes is the complete set of pre-declared Go built-in
// types. Source: https://pkg.go.dev/builtin (types section).
var goBuiltinTypes = map[string]struct{}{
	"any": {}, "bool": {}, "byte": {}, "comparable": {},
	"complex64": {}, "complex128": {}, "error": {},
	"float32": {}, "float64": {},
	"int": {}, "int8": {}, "int16": {}, "int32": {}, "int64": {},
	"rune": {}, "string": {},
	"uint": {}, "uint8": {}, "uint16": {}, "uint32": {}, "uint64": {},
	"uintptr": {},
}

// goBuiltinConsts is the set of pre-declared Go constants (true,
// false, iota, nil). Mostly emitted for completeness — `true` /
// `false` rarely show up as unresolved edge targets in practice
// because the parser handles them inline.
var goBuiltinConsts = map[string]struct{}{
	"true": {}, "false": {}, "iota": {}, "nil": {},
}

// goBuiltinUnresolvedTargets is the finite target set this pass can ever
// rewrite. Keeping it sorted makes resolution and tests deterministic while
// allowing disk stores to answer the whole pass through one indexed IN query.
var goBuiltinUnresolvedTargets = func() []string {
	targets := make([]string, 0, len(goBuiltinFuncs)+len(goBuiltinTypes)+len(goBuiltinConsts))
	for name := range goBuiltinFuncs {
		targets = append(targets, "unresolved::"+name)
	}
	for name := range goBuiltinTypes {
		targets = append(targets, "unresolved::"+name)
	}
	for name := range goBuiltinConsts {
		targets = append(targets, "unresolved::"+name)
	}
	sort.Strings(targets)
	return targets
}()

// attributeGoBuiltins rewrites `unresolved::<name>` edges whose name
// is a Go language intrinsic onto the canonical `builtin::go::*` ID,
// and materialises a single KindBuiltin node per unique builtin so
// the rewritten edges land at a real graph node instead of a
// rel-table FK stub. Mirrors the existing builtin::py / builtin::ts
// classifier in internal/resolver/builtins.go but completes the
// pattern by also creating nodes for the targets — so
// `find_usages(builtin::go::type::float64)` answers "every variable
// typed as float64 in this codebase", and the on-disk-backend stub
// inflation drops by ~50k rows on a gortex-scale Go codebase.
//
// Three ID namespaces under `builtin::go::`:
//
//	functions: builtin::go::<name>          (append, len, make, ...)
//	types:     builtin::go::type::<name>    (string, int, float64, ...)
//	constants: builtin::go::const::<name>   (true, false, iota, nil)
//
// Functions get the shortest namespace because their fan-in is the
// biggest and the shorter ID is what most downstream `find_usages`
// queries will type.
func (r *Resolver) attributeGoBuiltins() {
	// Go-only pass: skip the scan entirely when the graph has no Go nodes
	// (e.g. a TS/Python repo).
	if !r.graphHasLanguage("go") {
		return
	}

	// SQLite can project the complete logical identities for this finite target
	// set without decoding confidence, provenance, or metadata. When paired with
	// its identity-only target updater, the whole pass stays compact end-to-end.
	// Adapter stores retain the established full-edge path below.
	reader, readOK := r.graph.(graph.InEdgeIdentityBatchReader)
	targeter, targetOK := r.graph.(graph.UnresolvedEdgeTargetBatchReindexer)
	if readOK && targetOK {
		byTarget := reader.GetInEdgeIdentitiesByNodeIDs(goBuiltinUnresolvedTargets)
		if r.attributeGoBuiltinIdentityCandidates(targeter, byTarget) {
			return
		}
	}

	var candidates []*graph.Edge
	// The compatibility path still limits discovery to exact builtin targets.
	byTarget := r.graph.GetInEdgesByNodeIDs(goBuiltinUnresolvedTargets)
	for _, target := range goBuiltinUnresolvedTargets {
		candidates = append(candidates, byTarget[target]...)
	}
	r.attributeGoBuiltinCandidates(candidates)
}

// attributeGoBuiltinCandidateKinds is every edge kind a builtin can be the
// target of. Type-system edges (typed_as / returns) carry type references;
// call / arg-of / value-flow carry function or const references.
var attributeGoBuiltinCandidateKinds = map[graph.EdgeKind]struct{}{
	graph.EdgeCalls:        {},
	graph.EdgeReferences:   {},
	graph.EdgeReads:        {},
	graph.EdgeArgOf:        {},
	graph.EdgeValueFlow:    {},
	graph.EdgeReturnsTo:    {},
	graph.EdgeTypedAs:      {},
	graph.EdgeReturns:      {},
	graph.EdgeInstantiates: {},
	graph.EdgeCaptures:     {},
	graph.EdgeThrows:       {},
}

// attributeGoBuiltinsForFile is the single-file scope of attributeGoBuiltins:
// it only inspects the edited file's outgoing edges. A builtin reference's
// source endpoint is always inside the file that mentions it, so this
// produces the same rewrites as the whole-graph sweep for a per-save
// resolve without scanning every edge of eleven kinds across the graph.
func (r *Resolver) attributeGoBuiltinsForFile(filePath string) {
	if !r.graphHasLanguage("go") {
		return
	}
	r.attributeGoBuiltinCandidates(r.fileOutEdges(filePath))
}

// attributeGoBuiltinCandidates resolves a set of possible builtin edges with
// one batched source-node read, one batched node write, and one batched edge
// rewrite. The source batch includes enclosing owners for IDs whose path does
// not directly reveal the language, avoiding a node lookup per edge when the
// resolver cache has not been warmed on cold or partial paths.
func (r *Resolver) attributeGoBuiltinCandidates(candidates []*graph.Edge) {
	if len(candidates) == 0 {
		return
	}
	ids := make(map[string]struct{}, len(candidates)*2)
	for _, e := range candidates {
		if e == nil {
			continue
		}
		if e.From != "" {
			ids[e.From] = struct{}{}
		}
		if owner := enclosingFunctionForBinding(e.From); owner != "" {
			ids[owner] = struct{}{}
		}
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	sourceNodes := r.graph.GetNodesByIDs(idList)

	materialised := make(map[string]*graph.Node)
	batch := make([]graph.EdgeReindex, 0, len(candidates))
	for _, e := range candidates {
		if old := tryAttributeGoBuiltin(e, sourceNodes, materialised); old != "" {
			batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: old})
		}
	}
	if len(materialised) > 0 {
		r.addMissingBuiltinNodes(materialised)
	}
	if len(batch) == 0 {
		return
	}
	r.noteImportEdgeReindexes(batch)
	targeter, ok := r.graph.(graph.UnresolvedEdgeTargetBatchReindexer)
	if !ok {
		r.graph.ReindexEdges(batch)
		return
	}

	// This pass changes only the target identity. Production stores can apply
	// that as a compact to_id update instead of deleting, decoding, and
	// re-inserting every edge payload. Compact only when the entire batch meets
	// the optional interface contract. Any unsafe or converging shape keeps the
	// original order and goes through one legacy full-edge mutation, avoiding
	// partial application across two independent write windows.
	direct, compactOK := compactGoBuiltinTargetReindexes(batch)
	if !compactOK {
		r.graph.ReindexEdges(batch)
		return
	}
	targeter.ReindexUnresolvedEdgeTargets(direct)
}

// attributeGoBuiltinIdentityCandidates evaluates compact logical rows and, when
// every destination remains unique, persists only the target-column changes.
// Returning false asks the caller to repeat the pass through the full-edge
// compatibility path; no nodes or edges have been written in that case.
func (r *Resolver) attributeGoBuiltinIdentityCandidates(
	targeter graph.UnresolvedEdgeTargetBatchReindexer,
	byTarget map[string][]graph.EdgeIdentity,
) bool {
	if targeter == nil {
		return false
	}

	candidateCount := 0
	ids := make(map[string]struct{})
	for _, target := range goBuiltinUnresolvedTargets {
		for _, identity := range byTarget[target] {
			if _, candidate := attributeGoBuiltinCandidateKinds[identity.Kind]; !candidate {
				continue
			}
			candidateCount++
			if identity.From != "" {
				ids[identity.From] = struct{}{}
			}
			if owner := enclosingFunctionForBinding(identity.From); owner != "" {
				ids[owner] = struct{}{}
			}
		}
	}
	if candidateCount == 0 {
		return true
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	sourceNodes := r.graph.GetNodesByIDs(idList)

	materialised := make(map[string]*graph.Node)
	direct := make([]graph.UnresolvedEdgeTargetReindex, 0, candidateCount)
	for _, target := range goBuiltinUnresolvedTargets {
		for _, identity := range byTarget[target] {
			if _, candidate := attributeGoBuiltinCandidateKinds[identity.Kind]; !candidate {
				continue
			}
			edge := graph.Edge{
				From: identity.From, To: identity.To, Kind: identity.Kind,
				FilePath: identity.FilePath, Line: identity.Line,
			}
			oldTo := tryAttributeGoBuiltin(&edge, sourceNodes, materialised)
			if oldTo == "" {
				continue
			}
			if oldTo != identity.To || edge.From != identity.From ||
				edge.Kind != identity.Kind || edge.FilePath != identity.FilePath ||
				edge.Line != identity.Line {
				return false
			}
			direct = append(direct, graph.UnresolvedEdgeTargetReindex{
				Old: identity, NewTo: edge.To,
			})
		}
	}
	if !validGoBuiltinTargetReindexes(direct) {
		return false
	}
	if len(materialised) > 0 {
		r.addMissingBuiltinNodes(materialised)
	}
	if len(direct) > 0 {
		r.noteImportTargetReindexes(direct)
		targeter.ReindexUnresolvedEdgeTargets(direct)
	}
	return true
}

func validGoBuiltinTargetReindexes(batch []graph.UnresolvedEdgeTargetReindex) bool {
	if len(batch) == 0 {
		return true
	}
	order := make([]int, len(batch))
	for i, mutation := range batch {
		if mutation.NewTo == "" || !graph.IsUnresolvedTarget(mutation.Old.To) ||
			graph.IsUnresolvedTarget(mutation.NewTo) {
			return false
		}
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return goBuiltinEdgeIdentityLess(batch[order[i]].Old, batch[order[j]].Old)
	})
	for i := 1; i < len(order); i++ {
		if batch[order[i-1]].Old == batch[order[i]].Old {
			return false
		}
	}
	destination := func(mutation graph.UnresolvedEdgeTargetReindex) graph.EdgeIdentity {
		identity := mutation.Old
		identity.To = mutation.NewTo
		return identity
	}
	sort.Slice(order, func(i, j int) bool {
		return goBuiltinEdgeIdentityLess(
			destination(batch[order[i]]), destination(batch[order[j]]),
		)
	})
	for i := 1; i < len(order); i++ {
		if destination(batch[order[i-1]]) == destination(batch[order[i]]) {
			return false
		}
	}
	return true
}

// compactGoBuiltinTargetReindexes validates the already-mutated batch without
// changing its order. The compact interface requires unique old and destination
// identities, so an index slice is sorted twice for validation while the
// mutations themselves remain in legacy input order. This costs one machine
// word per edge instead of retaining corpus-sized identity-count maps.
func compactGoBuiltinTargetReindexes(batch []graph.EdgeReindex) (
	[]graph.UnresolvedEdgeTargetReindex,
	bool,
) {
	if len(batch) == 0 {
		return nil, false
	}

	direct := make([]graph.UnresolvedEdgeTargetReindex, len(batch))
	order := make([]int, len(batch))
	for i, mutation := range batch {
		if mutation.Edge == nil {
			return nil, false
		}
		destination := graph.EdgeIdentityFor(mutation.Edge)
		if destination.To == "" || !graph.IsUnresolvedTarget(mutation.OldTo) ||
			graph.IsUnresolvedTarget(destination.To) || mutation.OldFrom != "" ||
			mutation.OldKind != "" || mutation.OldFilePath != "" ||
			mutation.OldLine != 0 || mutation.RefreshIdentity {
			return nil, false
		}
		old := destination
		old.To = mutation.OldTo
		direct[i] = graph.UnresolvedEdgeTargetReindex{
			Old: old, NewTo: destination.To,
		}
		order[i] = i
	}

	sort.Slice(order, func(i, j int) bool {
		return goBuiltinEdgeIdentityLess(direct[order[i]].Old, direct[order[j]].Old)
	})
	for i := 1; i < len(order); i++ {
		if direct[order[i-1]].Old == direct[order[i]].Old {
			return nil, false
		}
	}

	destination := func(mutation graph.UnresolvedEdgeTargetReindex) graph.EdgeIdentity {
		identity := mutation.Old
		identity.To = mutation.NewTo
		return identity
	}
	sort.Slice(order, func(i, j int) bool {
		return goBuiltinEdgeIdentityLess(
			destination(direct[order[i]]), destination(direct[order[j]]),
		)
	})
	for i := 1; i < len(order); i++ {
		if destination(direct[order[i-1]]) == destination(direct[order[i]]) {
			return nil, false
		}
	}
	return direct, true
}

func goBuiltinEdgeIdentityLess(a, b graph.EdgeIdentity) bool {
	if a.From != b.From {
		return a.From < b.From
	}
	if a.To != b.To {
		return a.To < b.To
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.FilePath != b.FilePath {
		return a.FilePath < b.FilePath
	}
	return a.Line < b.Line
}

// tryAttributeGoBuiltin checks if e.To is `unresolved::<bareName>`
// where bareName is a Go builtin and the source language is Go (the
// source is inside a Go function / file). On a match it materialises
// the target node (once per unique ID), rewrites e.To, and returns
// the old To value for the batched reindex. Returns "" when the edge
// is left alone.
func tryAttributeGoBuiltin(e *graph.Edge, sourceNodes map[string]*graph.Node, materialised map[string]*graph.Node) string {
	if e == nil || !strings.HasPrefix(e.To, "unresolved::") {
		return ""
	}
	if _, ok := attributeGoBuiltinCandidateKinds[e.Kind]; !ok {
		return ""
	}
	name := strings.TrimPrefix(e.To, "unresolved::")
	if name == "" || strings.ContainsAny(name, ".*:#") {
		return ""
	}
	// Cheap membership check first: three small map lookups, no graph
	// access. Only ~2% of candidate names are ever a Go builtin in
	// practice, so rejecting the rest here — before the language-origin
	// check and the repo-prefix lookup below, both of which can fall back
	// to a graph node lookup — avoids paying for either on the ~98% that
	// were always going to return "" anyway.
	if !isGoBuiltinName(name) {
		return ""
	}
	// Only attribute when the source is Go. Without this guard a
	// Python reference to a local named `len` would get re-targeted
	// at Go's builtin `len`, which would be obviously wrong. Dataflow
	// edges (arg_of / value_flow) carry an `unresolved::` From placeholder
	// that fromIsGo cannot classify, so fall back to the call-site file
	// extension: a `.go` file's `append` / `make` / `len` argument is the Go
	// builtin regardless of whether the argument side ever bound to a node.
	// (langFromFilePath only classifies js/ts/py, so a `.go` suffix test is
	// the right check here.) e.FilePath is a free struct-field read while
	// fromIsGo's fallback path can hit a node lookup, so try FilePath
	// first — De Morgan's / && being commutative means this is the exact
	// same condition, just evaluated in the cheaper order.
	if !strings.HasSuffix(e.FilePath, ".go") && !sourceIsGo(e.From, sourceNodes) {
		return ""
	}
	// Builtins are repository-owned synthetic nodes. Carry the same boundary
	// identity as their source so a file-scoped resolve cannot overwrite a
	// previously stamped builtin with empty workspace/project columns. That
	// empty rewrite made warm-start backfill repeat on every changed Go file.
	boundarySource := sourceNodes[e.From]
	if boundarySource == nil {
		boundarySource = sourceNodes[enclosingFunctionForBinding(e.From)]
	}
	repoPrefix := ""
	workspaceID := ""
	projectID := ""
	if boundarySource != nil {
		repoPrefix = boundarySource.RepoPrefix
		workspaceID = boundarySource.WorkspaceID
		projectID = boundarySource.ProjectID
	}
	newID, kind, builtinKind := goBuiltinTarget(repoPrefix, name)
	if newID == "" {
		return ""
	}
	if _, ok := materialised[newID]; !ok {
		materialised[newID] = &graph.Node{
			ID:          newID,
			Kind:        kind,
			Name:        name,
			Language:    "go",
			RepoPrefix:  repoPrefix,
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Meta: map[string]any{
				"builtin":      true,
				"builtin_kind": builtinKind,
			},
		}
	}
	oldTo := e.To
	e.To = newID
	return oldTo
}

// isGoBuiltinName reports whether name is in any of the three builtin
// namespaces, without needing a repo prefix — the cheap pre-check
// tryAttributeGoBuiltin runs before the (potentially graph-lookup-backed)
// language-origin check and repo-prefix resolution. Mirrors goBuiltinTarget's
// own membership tests exactly; kept as a separate, repo-prefix-free
// function so the common "not a builtin" case never has to compute
// anything else first.
func isGoBuiltinName(name string) bool {
	if _, ok := goBuiltinFuncs[name]; ok {
		return true
	}
	if _, ok := goBuiltinTypes[name]; ok {
		return true
	}
	_, ok := goBuiltinConsts[name]
	return ok
}

// goBuiltinTarget classifies a bare identifier as one of Go's
// intrinsics. Returns the canonical builtin::go:: ID (per-repo
// prefixed via graph.StubID — see internal/graph/stub.go for why
// two repos can disagree on what's a builtin), the NodeKind to
// materialise it under (always KindBuiltin), and a meta tag
// recording which subspace (func / type / const) it belongs to.
// Returns ("", "", "") when the name is not a Go builtin.
// repoPrefix is the owning repo's RepoPrefix (empty in
// single-repo / legacy callers). Callers on the tryAttributeGoBuiltin path
// have already confirmed isGoBuiltinName(name) before calling this, so the
// repeated map lookups here run only on the small matching subset.
func goBuiltinTarget(repoPrefix, name string) (id string, kind graph.NodeKind, builtinKind string) {
	if _, ok := goBuiltinFuncs[name]; ok {
		return graph.StubID(repoPrefix, graph.StubKindBuiltin, "go", name), graph.KindBuiltin, "func"
	}
	if _, ok := goBuiltinTypes[name]; ok {
		return graph.StubID(repoPrefix, graph.StubKindBuiltin, "go", "type", name), graph.KindBuiltin, "type"
	}
	if _, ok := goBuiltinConsts[name]; ok {
		return graph.StubID(repoPrefix, graph.StubKindBuiltin, "go", "const", name), graph.KindBuiltin, "const"
	}
	return "", "", ""
}

// fromIsGo reports whether the source endpoint of an edge sits
// inside Go code. Uses the From's enclosing function (via the same
// suffix-stripping helper bare-name binding uses) — Go is the only
// language whose IDs follow the `file.go::Func` convention with a
// `.go` extension, so a path-based check is both cheap and reliable.
func sourceIsGo(fromID string, sourceNodes map[string]*graph.Node) bool {
	owner := enclosingFunctionForBinding(fromID)
	if owner == "" {
		return false
	}
	if i := strings.Index(owner, "::"); i > 0 {
		// `pkg/foo.go::Func` shape — peek at the file extension.
		head := owner[:i]
		if strings.HasSuffix(head, ".go") {
			return true
		}
	}
	// Fall back to looking up the owner node and checking its
	// Language. More expensive but covers edge cases where the ID
	// doesn't follow the `.go::Func` pattern.
	if n := sourceNodes[owner]; n != nil && n.Language == "go" {
		return true
	}
	return false
}

// addMissingBuiltinNodes writes the builtin sentinel nodes the graph does not
// already serve as materialised. A sentinel is the same node on every save of
// every file that uses it; re-adding it through a per-save delta made the
// delta speak for it again, a composed write per sentinel on every save.
func (r *Resolver) addMissingBuiltinNodes(materialised map[string]*graph.Node) {
	ids := make([]string, 0, len(materialised))
	for id := range materialised {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	existing := r.graph.GetNodesByIDs(ids)
	nodes := make([]*graph.Node, 0, len(ids))
	for _, id := range ids {
		n := materialised[id]
		if have := existing[id]; have != nil && sameBuiltinSentinel(have, n) {
			continue
		}
		nodes = append(nodes, n)
	}
	if len(nodes) > 0 {
		r.graph.AddBatch(nodes, nil)
	}
}

// sameBuiltinSentinel reports whether a served node is the sentinel a
// builtin attribution would write.
func sameBuiltinSentinel(have, want *graph.Node) bool {
	if have.ID != want.ID || have.Kind != want.Kind || have.Name != want.Name || have.Language != want.Language ||
		have.RepoPrefix != want.RepoPrefix || have.WorkspaceID != want.WorkspaceID || have.ProjectID != want.ProjectID ||
		have.FilePath != want.FilePath || len(have.Meta) != len(want.Meta) {
		return false
	}
	for key, value := range want.Meta {
		if have.Meta[key] != value {
			return false
		}
	}
	return true
}
