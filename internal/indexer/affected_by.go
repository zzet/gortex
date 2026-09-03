package indexer

import (
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// Affected-by re-resolution. When a save changes a file's symbol
// SIGNATURES — or removes symbols, or changes their kind — the files
// that referenced those symbols hold edges and persisted reference
// facts derived against the old shape. This pass re-resolves exactly
// those files, synchronously inside the incremental pipeline, bounded
// by a configurable cap: no goroutines, no whole-graph resolves. A
// body-only edit produces no signature delta and fans out to nothing —
// the delta gate is the point.
//
// The delta is computed on a LINE-INSENSITIVE identity (kind + name,
// the name already carrying any container qualifier), not the raw node
// ID: several languages embed a definition's start line in the node ID
// (TS/JS object members `name@<line>`, and the `_L<line>` disambiguator
// C++/Java/C#/Kotlin/Dart/Scala/PHP append to an overloaded or
// same-named member). A body-only edit ABOVE such a symbol shifts its
// line and rewrites its ID; keying the delta on the raw ID would read
// that as a remove + add and fan out on every line-shifting edit,
// defeating the gate. The stable key collapses the old and new IDs to
// the same slot so only a genuine shape change counts.
//
// The comparable shape is derived from more than Meta["signature"]:
// only Go and C stamp a parameter-bearing signature string there, so a
// signature-only delta is blind to a real parameter/return change in
// every other language. symbolShapeFor folds in the language-agnostic
// structure the extractors emit around a definition — its parameter
// nodes (kind, position, type) reached through EdgeParamOf, its return
// types through EdgeReturns, and the C++ parameter-shape Meta keys — so
// the delta fires on a Java/Python/TS/… parameter change too.
//
// The referencing files come from two sources, unioned:
//
//   - the persisted ref-facts sidecar's reverse lookup (to_id → source
//     file), which survives graph eviction and daemon restarts; and
//   - a live in-edge snapshot taken BEFORE the changed file is evicted
//     (Graph.EvictFile drops edges where the evicted node is EITHER
//     endpoint, so in-edges from unchanged files are gone afterwards).
//
// The snapshot is the only source on the in-memory backend (whose live
// edges ARE the facts); on a durable backend it also covers references
// the sidecar can't see, e.g. edges currently parked on an unresolved
// stub and cross-repo sources persisted under a sibling repo prefix.

// defaultAffectedByMax bounds the re-resolve fan-out when the config
// carries no explicit cap. See IndexConfig.AffectedByReresolveMax.
const defaultAffectedByMax = 200

// affectedByMaxFiles returns the effective fan-out cap.
func (idx *Indexer) affectedByMaxFiles() int {
	if n := idx.config.AffectedByReresolveMax; n > 0 {
		return n
	}
	return defaultAffectedByMax
}

// symbolShape is the per-symbol contract the delta compares under the
// stable (line-insensitive) key: kind plus a derived shape string that
// changes when, and only when, a referrer-visible aspect of the symbol
// changes (parameters, return types, signature). Body edits change none
// of these.
type symbolShape struct {
	kind  graph.NodeKind
	shape string
}

// stableSymbolKey is the line-insensitive identity the delta is keyed
// on: kind plus name. The name already carries any container qualifier
// the extractor minted (e.g. `Owner.member` for a JS object member),
// while the start line lives only in the raw node ID — so this key is
// stable across a body-only edit that shifts the definition's line and
// rewrites its `name@<line>` / `..._L<line>` ID.
func stableSymbolKey(n *graph.Node) string {
	return string(n.Kind) + "\x00" + n.Name
}

// symbolShapeAdjacency is the file-bounded graph slice needed to derive every
// symbol shape in one changed file. Both adjacency directions and any parameter
// endpoints are prefetched once and then reused for all definitions.
type symbolShapeAdjacency struct {
	inEdges  map[string][]*graph.Edge
	outEdges map[string][]*graph.Edge
	nodes    map[string]*graph.Node
}

// loadSymbolShapeAdjacency performs a constant number of bounded graph-store
// reads regardless of the number of definitions or parameters in the file.
// knownNodes should be the caller's already-loaded file/extraction nodes; using
// them avoids decoding parameter nodes from SQLite a second time.
func loadSymbolShapeAdjacency(g graph.Store, symbols, knownNodes []*graph.Node) symbolShapeAdjacency {
	ids := make([]string, 0, len(symbols))
	seenIDs := make(map[string]struct{}, len(symbols))
	for _, n := range symbols {
		if n == nil || n.ID == "" {
			continue
		}
		if _, seen := seenIDs[n.ID]; seen {
			continue
		}
		seenIDs[n.ID] = struct{}{}
		ids = append(ids, n.ID)
	}
	adj := symbolShapeAdjacency{
		nodes: make(map[string]*graph.Node, len(knownNodes)),
	}
	if len(ids) > 0 {
		adj.inEdges = g.GetInEdgesByNodeIDs(ids)
		adj.outEdges = g.GetOutEdgesByNodeIDs(ids)
	}
	for _, n := range knownNodes {
		if n != nil && n.ID != "" {
			adj.nodes[n.ID] = n
		}
	}
	missingSet := make(map[string]struct{})
	for _, edges := range adj.inEdges {
		for _, e := range edges {
			if e == nil || e.Kind != graph.EdgeParamOf || e.From == "" {
				continue
			}
			if _, known := adj.nodes[e.From]; !known {
				missingSet[e.From] = struct{}{}
			}
		}
	}
	if len(missingSet) > 0 {
		missing := make([]string, 0, len(missingSet))
		for id := range missingSet {
			missing = append(missing, id)
		}
		for id, n := range g.GetNodesByIDs(missing) {
			adj.nodes[id] = n
		}
	}
	return adj
}

// symbolShapeFor derives one comparable shape. Hot indexing paths load one
// symbolShapeAdjacency for the whole file and call symbolShapeFromAdjacency;
// this wrapper is retained for focused callers and tests.
func symbolShapeFor(g graph.Store, n *graph.Node) string {
	adj := loadSymbolShapeAdjacency(g, []*graph.Node{n}, []*graph.Node{n})
	return symbolShapeFromAdjacency(n, adj)
}

// symbolShapeFromAdjacency derives the comparable shape string for a
// referenceable symbol: the stamped signature (Go/C carry a parameter-bearing
// one), the C++ parameter-shape Meta keys, and the language-agnostic parameter
// and return structure emitted around the definition.
func symbolShapeFromAdjacency(n *graph.Node, adj symbolShapeAdjacency) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	if sig, _ := n.Meta["signature"].(string); sig != "" {
		b.WriteString(sig)
	}
	// C++ stamps its parameter shape under dedicated Meta keys rather
	// than a "signature" string; fold those in so an overload's argument
	// change registers.
	if v, ok := n.Meta["cpp_param_types"].(string); ok && v != "" {
		b.WriteString("|cppt:")
		b.WriteString(v)
	}
	if v, ok := n.Meta["cpp_param_shapes"].(string); ok && v != "" {
		b.WriteString("|cpps:")
		b.WriteString(v)
	}
	if v, ok := n.Meta["cpp_req_params"]; ok {
		b.WriteString("|cppr:")
		b.WriteString(metaToString(v))
	}
	if _, ok := n.Meta["cpp_variadic"]; ok {
		b.WriteString("|cppv")
	}
	// Language-agnostic parameter shape: the function-shape extractors
	// emit one KindParam node per parameter, linked to the owner by an
	// inbound EdgeParamOf, carrying position + type Meta. Sort by
	// position so the shape is order-stable regardless of edge insertion
	// order, and include the type so a same-arity type change still
	// registers.
	type paramShape struct {
		pos      int
		typ      string
		variadic bool
	}
	var params []paramShape
	for _, e := range adj.inEdges[n.ID] {
		if e == nil || e.Kind != graph.EdgeParamOf {
			continue
		}
		p := adj.nodes[e.From]
		if p == nil || p.Kind != graph.KindParam {
			continue
		}
		ps := paramShape{}
		if v, ok := p.Meta["position"]; ok {
			ps.pos = metaToInt(v)
		}
		if t, ok := p.Meta["type"].(string); ok {
			ps.typ = t
		}
		if _, ok := p.Meta["variadic"]; ok {
			ps.variadic = true
		}
		params = append(params, ps)
	}
	if len(params) > 0 {
		sort.Slice(params, func(i, j int) bool {
			if params[i].pos != params[j].pos {
				return params[i].pos < params[j].pos
			}
			return params[i].typ < params[j].typ
		})
		b.WriteString("|p:")
		for _, p := range params {
			b.WriteString(strconv.Itoa(p.pos))
			b.WriteByte(':')
			b.WriteString(p.typ)
			if p.variadic {
				b.WriteByte('*')
			}
			b.WriteByte(';')
		}
	}
	// Return shape: one EdgeReturns per declared return type, owner →
	// type. Collect the target names (a return-type change re-points the
	// edge) ordered by the position Meta the extractors stamp.
	type retShape struct {
		pos    int
		target string
	}
	var rets []retShape
	for _, e := range adj.outEdges[n.ID] {
		if e == nil || e.Kind != graph.EdgeReturns {
			continue
		}
		// Reduce the return target to its bare type name so the shape is
		// resolution-insensitive: the snapshot reads the pre-resolve edge
		// (still an `unresolved::T` stub) while the delta reads it after the
		// changed file's reverse resolve may have rebound it to a concrete
		// `pkg/x.go::T` node. Both must hash to the same `T`, or a body-only
		// edit whose return type happened to (re)bind would fan out.
		rs := retShape{target: bareTypeName(e.To), pos: metaToInt(e.Meta["position"])}
		rets = append(rets, rs)
	}
	if len(rets) > 0 {
		sort.Slice(rets, func(i, j int) bool {
			if rets[i].pos != rets[j].pos {
				return rets[i].pos < rets[j].pos
			}
			return rets[i].target < rets[j].target
		})
		b.WriteString("|r:")
		for _, r := range rets {
			b.WriteString(strconv.Itoa(r.pos))
			b.WriteByte(':')
			b.WriteString(r.target)
			b.WriteByte(';')
		}
	}
	return b.String()
}

// bareTypeName reduces a type-reference edge target to a bare, line- and
// resolution-insensitive name. It strips an `unresolved::` stub prefix,
// then keeps only the trailing component after the last `::` (the
// file/repo scope) and the last `.` (an owner qualifier) — so a stub
// `unresolved::T`, a resolved `pkg/x.go::T`, and a resolved member
// `pkg/x.go::Owner.T` all reduce to `T`.
func bareTypeName(target string) string {
	if target == "" {
		return ""
	}
	if n := graph.UnresolvedName(target); n != "" {
		target = n
	}
	if i := strings.LastIndex(target, "::"); i >= 0 {
		target = target[i+2:]
	}
	if i := strings.LastIndex(target, "."); i >= 0 {
		target = target[i+1:]
	}
	return target
}

// metaToString renders an int/int64/string Meta value as a string for
// shape composition. The function-shape extractors stamp counts as int.
func metaToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}

// metaToInt reads an int-ish Meta value, tolerating the int / int64 /
// float64 (JSON round-trip) forms a persisted node can carry.
func metaToInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	default:
		return 0
	}
}

// affectedBySnapshot captures, before a changed file's nodes are
// evicted, the two things eviction destroys: the file's referenceable
// symbol shapes (for the signature delta) and the source node IDs of
// live reference edges into those symbols (the reverse-lookup fallback).
//
// refSources holds the FROM node IDs of the in-edges, not their file
// paths: resolving each source's file is deferred to affectedFilesFor,
// which runs only when a delta exists. A body-only edit therefore never
// pays a single GetNode for a referrer — its delta is empty and the
// fallback is never consulted.
//
// idsByKey records the concrete (old) target node IDs that hashed into
// each stable key. The persisted ref-facts sidecar indexes facts by the
// exact target ID — which still embeds the pre-edit line for a
// line-suffixed language — so the durable reverse lookup must be issued
// against these old IDs, not the changed file's fresh post-edit nodes.
type affectedBySnapshot struct {
	symbols    map[string]symbolShape         // stable key → pre-edit shape
	refSources map[string]map[string]struct{} // stable key → referencing source node IDs
	idsByKey   map[string][]string            // stable key → pre-edit target node IDs
}

// snapshotAffectedBy builds the pre-evict snapshot for graphPath. Must
// run before restubIncomingRefs / EvictFile — afterwards the in-edges
// point at unresolved stubs (or are gone) and the old signatures are
// unreadable. Returns nil when the file defines no referenceable
// symbols, which callers treat as "no pass".
//
// The in-edge fan-in is read in one batched GetInEdgesByNodeIDs call so
// the durable backend pays one query for the whole file rather than one
// per symbol; the per-edge source-node lookup is deferred to the delta
// path entirely.
func (idx *Indexer) snapshotAffectedBy(graphPath string) *affectedBySnapshot {
	nodes := idx.graph.GetFileNodes(graphPath)
	if len(nodes) == 0 {
		return nil
	}
	snap := &affectedBySnapshot{
		symbols:    make(map[string]symbolShape),
		refSources: make(map[string]map[string]struct{}),
		idsByKey:   make(map[string][]string),
	}
	refNodes := make([]*graph.Node, 0, len(nodes))
	keyByID := make(map[string]string, len(nodes))
	ownIDs := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if n != nil {
			ownIDs[n.ID] = struct{}{}
		}
	}
	for _, n := range nodes {
		if n == nil || n.Name == "" || !graph.IsReferenceableSymbol(n.Kind) {
			continue
		}
		refNodes = append(refNodes, n)
	}
	if len(refNodes) == 0 {
		return nil
	}
	adj := loadSymbolShapeAdjacency(idx.graph, refNodes, nodes)
	for _, n := range refNodes {
		key := stableSymbolKey(n)
		// A file can carry two same-name same-kind definitions (an
		// overload); they share a stable key. Their shapes compose so the
		// key's shape changes if either overload's shape does — coarse but
		// correct for a fan-out gate (over-firing is bounded and safe,
		// under-firing leaves a stale edge).
		cur := snap.symbols[key]
		cur.kind = n.Kind
		cur.shape += symbolShapeFromAdjacency(n, adj) + "\n"
		snap.symbols[key] = cur
		snap.idsByKey[key] = append(snap.idsByKey[key], n.ID)
		keyByID[n.ID] = key
	}
	// Reuse the same inbound adjacency that supplied parameter shapes for the
	// live referrer snapshot; no second backend query is needed.
	for id, edges := range adj.inEdges {
		key := keyByID[id]
		if key == "" {
			continue
		}
		for _, e := range edges {
			if e == nil || !graph.IsResolvableRefEdge(e.Kind) {
				continue
			}
			if _, ours := ownIDs[e.From]; ours {
				continue // intra-file reference: re-resolved with the file itself
			}
			set := snap.refSources[key]
			if set == nil {
				set = make(map[string]struct{})
				snap.refSources[key] = set
			}
			set[e.From] = struct{}{}
		}
	}
	return snap
}

// affectedByDelta returns the stable keys of snapshot symbols whose
// contract changed against the freshly indexed node set: shape changed,
// kind changed, or the symbol is gone. A rename is a remove of the old
// key plus an add of the new one, so it lands here through the removed
// side. Newly added symbols are never part of the delta — nothing can
// hold a stale reference to a symbol that did not exist.
func semanticShapeNodes(nodes []*graph.Node) []*graph.Node {
	out := make([]*graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil || node.Name == "" || !graph.IsReferenceableSymbol(node.Kind) {
			continue
		}
		out = append(out, node)
	}
	return out
}

func semanticShapeSet(nodes []*graph.Node, adj symbolShapeAdjacency) map[string]symbolShape {
	shapes := make(map[string]symbolShape, len(nodes))
	for _, node := range semanticShapeNodes(nodes) {
		key := stableSymbolKey(node)
		shape := shapes[key]
		shape.kind = node.Kind
		shape.shape += symbolShapeFromAdjacency(node, adj) + "\n"
		shapes[key] = shape
	}
	return shapes
}

func semanticShapeDelta(before, after map[string]symbolShape) []string {
	var delta []string
	for key, old := range before {
		now, exists := after[key]
		if !exists || now.kind != old.kind || now.shape != old.shape {
			delta = append(delta, key)
		}
	}
	sort.Strings(delta)
	return delta
}

func affectedByDelta(g graph.Store, snap *affectedBySnapshot, newNodes []*graph.Node) []string {
	refNodes := semanticShapeNodes(newNodes)
	current := semanticShapeSet(refNodes, loadSymbolShapeAdjacency(g, refNodes, newNodes))
	return semanticShapeDelta(snap.symbols, current)
}

// affectedFilesFor unions the persisted reverse lookup with the
// pre-evict in-edge snapshot for the delta symbols, excluding the
// changed file itself. The snapshot stores referrer NODE IDs; their
// files are resolved here in one batched GetNodesByIDs — work that runs
// only for a real delta, never on the body-only path. Sorted for
// deterministic truncation and tests.
func (idx *Indexer) affectedFilesFor(changedPath string, deltaKeys []string, snap *affectedBySnapshot) []string {
	fileSet := make(map[string]struct{})
	// Durable reverse lookup: the sidecar answers by concrete target ID,
	// which still embeds the pre-edit line for a line-suffixed language.
	// Issue it against the OLD target IDs the snapshot recorded for each
	// delta key — the changed file's fresh nodes carry new IDs that the
	// seeded sidecar has never seen.
	if r, ok := idx.graph.(graph.RefFactsReader); ok {
		var targetIDs []string
		for _, key := range deltaKeys {
			targetIDs = append(targetIDs, snap.idsByKey[key]...)
		}
		if len(targetIDs) > 0 {
			byFile, err := r.LoadRefFactsByTargets(idx.repoPrefix, targetIDs)
			if err != nil {
				idx.logger.Debug("affected-by: ref-facts reverse lookup failed", zap.Error(err))
			}
			for file := range byFile {
				if file != "" && file != changedPath {
					fileSet[file] = struct{}{}
				}
			}
		}
	}
	// In-edge snapshot fallback: the snapshot stored referrer NODE IDs;
	// resolve them to their files in one batch — work that runs only for a
	// real delta, never on the body-only path.
	srcIDSet := make(map[string]struct{})
	for _, key := range deltaKeys {
		for from := range snap.refSources[key] {
			srcIDSet[from] = struct{}{}
		}
	}
	if len(srcIDSet) > 0 {
		ids := make([]string, 0, len(srcIDSet))
		for id := range srcIDSet {
			ids = append(ids, id)
		}
		byID := idx.graph.GetNodesByIDs(ids)
		for _, n := range byID {
			if n == nil || n.FilePath == "" || n.FilePath == changedPath {
				continue
			}
			fileSet[n.FilePath] = struct{}{}
		}
	}
	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// reresolveAffectedBy is the pass entry point, called from the
// incremental per-file index path after the changed file itself has
// been re-indexed, re-resolved, and had its own facts re-persisted.
// snap is the pre-evict snapshot (nil ⇒ no-op); newNodes are the
// changed file's freshly added nodes (already repo-prefixed). For each
// affected file it re-runs the per-save resolve pair (forward + reverse,
// batched so the resolver's pass indexes are built once), re-materialises
// external-call placeholders, and re-persists that file's reference
// facts — so an edge that degraded to an unresolved stub also drops its
// stale persisted fact, and a rebound edge records its new resolution.
func (idx *Indexer) reresolveAffectedBy(changedPath string, snap *affectedBySnapshot, newNodes []*graph.Node) {
	if snap == nil {
		return
	}
	delta := affectedByDelta(idx.graph, snap, newNodes)
	if len(delta) == 0 {
		return // body-only edit: no contract change, no fan-out
	}
	files := idx.affectedFilesFor(changedPath, delta, snap)
	if len(files) == 0 {
		return
	}
	if maxFiles := idx.affectedByMaxFiles(); len(files) > maxFiles {
		idx.logger.Debug("affected-by: re-resolve set truncated",
			zap.String("file", changedPath),
			zap.Int("affected", len(files)),
			zap.Int("cap", maxFiles),
			zap.Int("dropped", len(files)-maxFiles))
		files = files[:maxFiles]
	}
	idx.resolver.ResolveFilesAndIncoming(files)
	resolver.SynthesizeExternalCallsForFiles(idx.graph, idx.externalCallSynthesisEnabled(), files)
	idx.persistRefFactsForFiles(files)
}
