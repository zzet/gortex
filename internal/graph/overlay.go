package graph

import (
	"context"
	"iter"
	"sort"
	"strings"
	"sync"
)

// OverlayLayer is one MCP session's parsed editor-buffer state. It
// holds the nodes and edges that the overlay introduces (or hides via
// tombstones) on top of an immutable base graph. The layer is built
// once per (session, content-hash) tuple by the MCP overlay middleware
// (`internal/mcp/overlay_view.go::buildOverlayLayer`) and is consulted
// read-only by `OverlaidView`.
//
// **Identity is preserved.** Gortex node IDs are derived from
// `file::symbol` paths, so a symbol that exists in both the on-disk
// and overlay versions of a file ends up with the same ID — the
// view substitutes the overlay's version transparently. New overlay
// symbols (a function the user just typed) get IDs that don't exist
// in base; deleted symbols (removed from the buffer) simply aren't in
// the layer's per-file node list.
//
// The layer is immutable after construction. The middleware never
// mutates it once the View is in flight; the base graph is never
// mutated by overlay flow at all. This is what makes the design
// safe for concurrent multi-session deployments — no shared mutable
// state between sessions or between an overlay-active session and a
// non-overlay session.
type OverlayLayer struct {
	// Files covered by the overlay. The key is the file's graph path
	// (repo-prefixed in multi-repo mode). Presence in this map means
	// "the View should hide base's view of this path" — either to
	// replace it with overlay content (entries[path] != nil) or to
	// tombstone it (entries[path].Deleted).
	entries map[string]*overlayFileEntry

	// nodeByID lets GetNode hit a single map lookup. Holds every
	// non-tombstoned overlay node across every overlay file.
	nodeByID map[string]*Node

	// outEdges maps each overlay-introduced source node ID to its
	// resolved outgoing edges. Filled by the local resolver pass at
	// layer construction.
	outEdges map[string][]*Edge

	// inEdges is the reverse index of outEdges keyed by target ID,
	// so OverlaidView.GetInEdges can merge overlay-originating
	// edges with base in-edges in O(1).
	inEdges map[string][]*Edge

	// nodesByName/Qual index overlay nodes for FindNodesByName /
	// GetNodeByQualName fast paths.
	nodesByName map[string][]*Node
	nodesByQual map[string]*Node

	// nameRemoved is the set of (name → IDs from base that are no
	// longer present under the View). FindNodesByName uses this to
	// filter base hits whose enclosing file is overlaid but whose
	// id disappeared from the overlay's node list.
	nameRemoved map[string]map[string]bool
	// removedByID is the immutable identity-side index for bounded point and
	// adjacency projections. It avoids rescanning every name bucket per ID.
	removedByID map[string]bool
}

// overlayFileEntry carries one file's overlay state inside the
// layer. Deleted=true is the tombstone variant — no nodes, no edges.
type overlayFileEntry struct {
	Path    string
	Deleted bool
	Nodes   []*Node
}

// NewOverlayLayer constructs an empty layer. Callers build it up via
// AddFile / AddNode / AddEdge during the per-request layer-build
// pass, then freeze it by handing it to NewOverlaidView. After that
// point the layer is treated as immutable; the View never writes
// back.
func NewOverlayLayer() *OverlayLayer {
	return &OverlayLayer{
		entries:     make(map[string]*overlayFileEntry),
		nodeByID:    make(map[string]*Node),
		outEdges:    make(map[string][]*Edge),
		inEdges:     make(map[string][]*Edge),
		nodesByName: make(map[string][]*Node),
		nodesByQual: make(map[string]*Node),
		nameRemoved: make(map[string]map[string]bool),
		removedByID: make(map[string]bool),
	}
}

// MarkFile registers an overlay file. Call once per overlay path
// before AddNode / AddEdge for that file. `deleted` true means the
// path is a tombstone — the View hides base's view of the path
// entirely, returning no nodes from GetFileNodes and treating the
// path's node IDs as non-existent for GetNode.
func (l *OverlayLayer) MarkFile(graphPath string, deleted bool) {
	l.entries[graphPath] = &overlayFileEntry{Path: graphPath, Deleted: deleted}
}

// AddNode attaches one parsed overlay node to the layer. Must be
// called after MarkFile for the node's file. Idempotent on (graphPath,
// node ID) — second add silently replaces.
func (l *OverlayLayer) AddNode(graphPath string, n *Node) {
	if n == nil {
		return
	}
	entry, ok := l.entries[graphPath]
	if !ok {
		entry = &overlayFileEntry{Path: graphPath}
		l.entries[graphPath] = entry
	}
	if entry.Deleted {
		// Tombstone: silently drop. Caller bug — but cheap to absorb.
		return
	}
	if old := l.nodeByID[n.ID]; old != nil {
		for index, candidate := range entry.Nodes {
			if candidate == nil || candidate.ID != n.ID {
				continue
			}
			entry.Nodes[index] = n
			l.nodeByID[n.ID] = n
			l.replaceNodeIndexes(old, n)
			return
		}
	}
	entry.Nodes = append(entry.Nodes, n)
	l.nodeByID[n.ID] = n
	if n.Name != "" {
		l.nodesByName[n.Name] = append(l.nodesByName[n.Name], n)
	}
	if n.QualName != "" {
		l.nodesByQual[n.QualName] = n
	}
}

func (l *OverlayLayer) replaceNodeIndexes(old, replacement *Node) {
	if old.Name == replacement.Name {
		if old.Name != "" {
			replaced := false
			for index, candidate := range l.nodesByName[old.Name] {
				if candidate != nil && candidate.ID == replacement.ID {
					l.nodesByName[old.Name][index] = replacement
					replaced = true
				}
			}
			if !replaced {
				l.nodesByName[old.Name] = append(l.nodesByName[old.Name], replacement)
			}
		}
	} else {
		if old.Name != "" {
			bucket := l.nodesByName[old.Name]
			filtered := bucket[:0]
			for _, candidate := range bucket {
				if candidate == nil || candidate.ID != replacement.ID {
					filtered = append(filtered, candidate)
				}
			}
			if len(filtered) == 0 {
				delete(l.nodesByName, old.Name)
			} else {
				l.nodesByName[old.Name] = filtered
			}
		}
		if replacement.Name != "" {
			l.nodesByName[replacement.Name] = append(l.nodesByName[replacement.Name], replacement)
		}
	}

	if old.QualName != "" && old.QualName != replacement.QualName {
		if current := l.nodesByQual[old.QualName]; current != nil && current.ID == replacement.ID {
			delete(l.nodesByQual, old.QualName)
		}
	}
	if replacement.QualName != "" {
		l.nodesByQual[replacement.QualName] = replacement
	}
}

// AddEdge attaches one resolved overlay edge. The local-resolver
// pass at layer construction is expected to have rewritten any
// `unresolved::*` placeholders to point at concrete (overlay or
// base) node IDs before calling this; edges still carrying the
// placeholder are kept verbatim so OverlaidView.GetOutEdges still
// surfaces them — query tools can decide how to handle them, just
// like base's resolver-skipped edges.
func (l *OverlayLayer) AddEdge(e *Edge) {
	if e == nil {
		return
	}
	l.outEdges[e.From] = append(l.outEdges[e.From], e)
	l.inEdges[e.To] = append(l.inEdges[e.To], e)
}

// MarkRemoved tells the layer that a base node ID is hidden by the
// overlay even though the overlay didn't re-emit it (a symbol the
// user deleted from the buffer). FindNodesByName uses this to filter
// stale base hits.
func (l *OverlayLayer) MarkRemoved(baseName, baseID string) {
	if baseName == "" || baseID == "" {
		return
	}
	set, ok := l.nameRemoved[baseName]
	if !ok {
		set = make(map[string]bool)
		l.nameRemoved[baseName] = set
	}
	set[baseID] = true
	l.removedByID[baseID] = true
}

// HasFile reports whether the overlay covers a particular graph path
// (either with replacement content or as a tombstone). The View uses
// this to decide whether to consult overlay or base for the path's
// reads.
func (l *OverlayLayer) HasFile(graphPath string) bool {
	if l == nil {
		return false
	}
	_, ok := l.entries[graphPath]
	return ok
}

// IsTombstone reports whether the overlay marks the path as deleted.
func (l *OverlayLayer) IsTombstone(graphPath string) bool {
	if l == nil {
		return false
	}
	e := l.entries[graphPath]
	return e != nil && e.Deleted
}

// FilePaths returns the sorted list of overlay-covered paths. Used
// by analyzers / the diff tool to enumerate the overlay's footprint.
func (l *OverlayLayer) FilePaths() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.entries))
	for p := range l.entries {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// HasNode reports whether the overlay layer carries a node with this
// ID. Used by the local-resolver pass in the mcp layer to drop base
// hits whose file is overlaid but whose specific ID wasn't kept by
// the overlay (i.e. the user deleted that symbol from the buffer).
func (l *OverlayLayer) HasNode(id string) bool {
	if l == nil {
		return false
	}
	_, ok := l.nodeByID[id]
	return ok
}

// NodesByName returns the overlay-introduced nodes with the given
// short name. Empty slice when none. Used by the local-resolver
// pass.
func (l *OverlayLayer) NodesByName(name string) []*Node {
	if l == nil {
		return nil
	}
	src := l.nodesByName[name]
	out := make([]*Node, len(src))
	copy(out, src)
	return out
}

// OutEdgesByFromAll returns a snapshot of the layer's outgoing-edge
// map keyed by source ID. The resolver pass iterates this to rewrite
// `unresolved::*` placeholders. The returned map shares its slices
// with the layer (resolver mutates Edge.To in place); the map keys
// are stable for the snapshot.
func (l *OverlayLayer) OutEdgesByFromAll() map[string][]*Edge {
	if l == nil {
		return nil
	}
	out := make(map[string][]*Edge, len(l.outEdges))
	for k, v := range l.outEdges {
		out[k] = v
	}
	return out
}

// RebuildInEdges rebuilds the reverse-index map after the local
// resolver pass mutates Edge.To in place. Cheap: O(#overlay edges).
func (l *OverlayLayer) RebuildInEdges() {
	if l == nil {
		return
	}
	l.inEdges = make(map[string][]*Edge, len(l.outEdges))
	for _, edges := range l.outEdges {
		for _, e := range edges {
			l.inEdges[e.To] = append(l.inEdges[e.To], e)
		}
	}
}

// OverlaidView composes an immutable base Reader with a per-session
// overlay layer. Every read path consults the layer first for paths
// the overlay covers; falls through to base otherwise. The base is
// never mutated; the layer is built once per request and discarded
// with the request. This means concurrent sessions — overlay-active
// or not — each see their own consistent view, and the file watcher's
// reindex passes (which mutate base) don't corrupt overlay queries.
//
// The layer is held as the OverlayLayerReader contract, so the same
// composition serves any layer implementation.
type OverlaidView struct {
	base  Reader
	layer OverlayLayerReader

	// statsOnce caches the (potentially expensive) Stats walk so
	// repeated calls within one request don't pay the AllNodes /
	// AllEdges cost twice.
	statsOnce sync.Once
	stats     GraphStats
}

// NewOverlaidView builds a view over the in-memory layer. If layer is
// nil the view is a pure pass-through and consumers pay no overlay
// overhead.
func NewOverlaidView(base Reader, layer *OverlayLayer) *OverlaidView {
	if layer == nil {
		return &OverlaidView{base: base}
	}
	return &OverlaidView{base: base, layer: layer}
}

// NewOverlaidViewWithLayer builds a view over any layer implementation.
// Pass a nil layer for a pure pass-through — never a nil pointer of a
// concrete layer type, which would read as a layer that covers nothing
// yet still costs the composition its overlay branches.
func NewOverlaidViewWithLayer(base Reader, layer OverlayLayerReader) *OverlaidView {
	return &OverlaidView{base: base, layer: layer}
}

// Base exposes the underlying base reader. The diff tool reads
// against (view.Base()) and against (view) directly to compute the
// delta induced by the overlay.
func (v *OverlaidView) Base() Reader { return v.base }

// Layer exposes the per-session in-memory overlay layer, nil when the
// view composes none (or composes a different layer implementation).
// Diagnostic / debug tools use it to introspect what the overlay
// covers.
func (v *OverlaidView) Layer() *OverlayLayer {
	layer, _ := v.layer.(*OverlayLayer)
	return layer
}

// IDFile returns the file path encoded in a Gortex node ID, or "" if
// the id isn't file-anchored. Gortex IDs follow the pattern
// `<filepath>::<symbol>[.member][#param:name]` so the file prefix is
// the substring before the first `::`. Module / package / virtual
// nodes use other prefixes that won't match an overlay path.
//
// A file node's ID is the bare path with no `::`, so this returns "" for
// it — callers that need the file an arbitrary node ID belongs to must
// handle that case; overlay ownership does it in coversNodeID.
func IDFile(id string) string {
	if id == "" {
		return ""
	}
	if i := strings.Index(id, "::"); i > 0 {
		return id[:i]
	}
	return ""
}

// nodeBelongsToOverlay reports whether an ID's file is covered by
// the layer.
func (v *OverlaidView) nodeBelongsToOverlay(id string) bool {
	if v.layer == nil {
		return false
	}
	return v.layer.CoversNodeID(id)
}

// baseNodeVisible is the single predicate every reader applies to a base
// node, so point, batched and bulk reads all expose the same node set.
//
// A base row survives while the layer does not speak for its identity.
// The layer speaks for every identity in a file it covers — re-emitting
// a row replaces it, leaving one out hides it — and for an identity it
// removed or re-emitted from outside those files, which is exactly what
// a node tombstone claims: one identity gone without claiming the whole
// file it lives in.
//
// It is the node-side sibling of baseEdgeVisible and reads through the
// same ownership helper the bounded readers use, so every node surface
// answers alike.
func (v *OverlaidView) baseNodeVisible(n *Node) bool {
	if n == nil {
		return false
	}
	return !v.overlayOwnsIdentity(n.ID)
}

// GetNode returns the layer's version of a node whenever the layer
// speaks for the ID — because it covers the ID's file, carries a node
// under the ID, or marked the ID removed. Returns nil when the layer
// speaks for the ID and kept nothing under it: a symbol dropped from an
// overlaid file, or one a node tombstone removed without claiming that
// file. Everything else comes from base.
func (v *OverlaidView) GetNode(id string) *Node {
	if v.overlayOwnsIdentity(id) {
		return v.layer.NodeByID(id) // may be nil — the layer hid it
	}
	if v.base == nil {
		return nil
	}
	return v.base.GetNode(id)
}

// GetNodesByIDs returns the overlay-aware *Node for each input ID.
// Overlay-owned IDs short-circuit to the per-session layer (and may
// resolve to nil when the overlay deleted the node); the remainder
// fans out as a single batched lookup against the base store. Missing
// IDs are simply absent from the returned map.
func (v *OverlaidView) GetNodesByIDs(ids []string) map[string]*Node {
	nodes, _ := v.GetNodesByIDsContext(context.Background(), ids)
	return nodes
}

// GetNodeByQualName: overlay first, then base. A base hit is dropped
// when the layer speaks for its identity — the overlay's view wins,
// whether it covers the hit's file or only tombstoned the ID.
func (v *OverlaidView) GetNodeByQualName(qualName string) *Node {
	if v.layer != nil {
		if n := v.layer.NodeByQualName(qualName); n != nil {
			return n
		}
	}
	if v.base == nil {
		return nil
	}
	n := v.base.GetNodeByQualName(qualName)
	if !v.baseNodeVisible(n) {
		// The layer speaks for the identity base answered with, and its
		// own answer for this qualified name was already consulted
		// above — so nothing survives under this name.
		return nil
	}
	return n
}

type qualifiedNameBatchReader interface {
	GetNodesByQualNames(qualNames []string) map[string][]*Node
}

// GetNodesByQualNames merges every overlay and surviving base candidate.
// Base identities the layer speaks for stay hidden, and each result slice is
// deduplicated and sorted by ID so resolver selection is backend-independent.
func (v *OverlaidView) GetNodesByQualNames(qualNames []string) map[string][]*Node {
	requested := make(map[string]struct{}, len(qualNames))
	for _, q := range qualNames {
		if q != "" {
			requested[q] = struct{}{}
		}
	}
	if len(requested) == 0 {
		return nil
	}

	out := make(map[string][]*Node, len(requested))
	seen := make(map[string]map[string]struct{}, len(requested))
	add := func(n *Node) {
		if n == nil {
			return
		}
		if _, ok := requested[n.QualName]; !ok {
			return
		}
		ids := seen[n.QualName]
		if ids == nil {
			ids = make(map[string]struct{})
			seen[n.QualName] = ids
		}
		if _, exists := ids[n.ID]; exists {
			return
		}
		ids[n.ID] = struct{}{}
		out[n.QualName] = append(out[n.QualName], n)
	}

	if v.layer != nil {
		for n := range v.layer.Nodes() {
			add(n)
		}
	}
	if v.base != nil {
		var baseHits map[string][]*Node
		if batch, ok := v.base.(qualifiedNameBatchReader); ok {
			baseHits = batch.GetNodesByQualNames(qualNames)
		} else {
			baseHits = make(map[string][]*Node, len(requested))
			for q := range requested {
				if n := v.base.GetNodeByQualName(q); n != nil {
					baseHits[q] = []*Node{n}
				}
			}
		}
		for _, hits := range baseHits {
			for _, n := range hits {
				if !v.baseNodeVisible(n) {
					continue
				}
				add(n)
			}
		}
	}
	for q := range out {
		sort.Slice(out[q], func(i, j int) bool { return out[q][i].ID < out[q][j].ID })
	}
	return out
}

// FindNodesByName merges base hits (filtered to drop every identity the
// layer speaks for) with overlay hits. Order is overlay-first, then
// base — callers that picked "first match" semantics get the overlay
// version automatically.
func (v *OverlaidView) FindNodesByName(name string) []*Node {
	var out []*Node
	if v.layer != nil {
		out = append(out, v.layer.NodesByName(name)...)
	}
	if v.base == nil {
		return out
	}
	for _, n := range v.base.FindNodesByName(name) {
		// A base hit the layer speaks for is always hidden. If the
		// layer re-emitted the same ID it's already in `out` from the
		// layer's own name index above; if the layer hid the symbol —
		// by covering its file without re-emitting it, or by
		// tombstoning the identity — it must not surface at all.
		// Either way we skip: no need to discriminate.
		if !v.baseNodeVisible(n) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// FindNodesByNameContaining merges overlay-touched name hits with the
// base result, then re-applies the same ownership mask FindNodesByName
// does. Order is overlay-first, then base; the limit caps the merged
// total. Empty substr or both layers nil returns nil.
func (v *OverlaidView) FindNodesByNameContaining(substr string, limit int) []*Node {
	if substr == "" {
		return nil
	}
	needle := strings.ToLower(substr)
	var out []*Node
	// Overlay-side: walk the layer's nodesByName index — the same
	// bucket FindNodesByName reads from — and accept any name whose
	// lowercase form contains the needle.
	if v.layer != nil {
		for name, bucket := range v.layer.NamedNodes() {
			if strings.Contains(strings.ToLower(name), needle) {
				out = append(out, bucket...)
				if limit > 0 && len(out) >= limit {
					return out[:limit]
				}
			}
		}
	}
	if v.base == nil {
		return out
	}
	// Base-side: fetch with an inflated limit so overlay-mask drops
	// don't leave a short page. Then re-apply the same overlaid-file
	// + name-removed mask FindNodesByName uses.
	fetch := limit
	if fetch > 0 {
		fetch *= 2
	}
	for _, n := range v.base.FindNodesByNameContaining(substr, fetch) {
		if !v.baseNodeVisible(n) {
			continue
		}
		out = append(out, n)
		if limit > 0 && len(out) >= limit {
			return out[:limit]
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// GetFileNodes: if the path is overlaid, return overlay's nodes
// (empty for tombstones). Otherwise base's, minus the rows the layer
// speaks for anyway — a node tombstone removes one identity without
// claiming the file it lives in, so an uncovered path can still lose a
// symbol.
func (v *OverlaidView) GetFileNodes(filePath string) []*Node {
	if v.layer != nil && v.layer.HasFile(filePath) {
		// The layer owns its slice, so hand callers their own copy.
		src := v.layer.FileNodes(filePath)
		out := make([]*Node, len(src))
		copy(out, src)
		return out
	}
	if v.base == nil {
		return nil
	}
	baseNodes := v.base.GetFileNodes(filePath)
	if v.layer == nil {
		return baseNodes
	}
	out := make([]*Node, 0, len(baseNodes))
	for _, n := range baseNodes {
		if !v.baseNodeVisible(n) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// GetRepoNodes filters base's per-repo node list by dropping every
// identity the layer speaks for and appending the overlay's nodes for
// any overlaid file inside the requested repo prefix.
func (v *OverlaidView) GetRepoNodes(repoPrefix string) []*Node {
	if v.base == nil {
		return nil
	}
	baseNodes := v.base.GetRepoNodes(repoPrefix)
	if v.layer == nil {
		return baseNodes
	}
	out := make([]*Node, 0, len(baseNodes))
	for _, n := range baseNodes {
		if !v.baseNodeVisible(n) {
			// The layer speaks for this identity. Re-emitted IDs come
			// back below from the layer's own per-file list (with the
			// layer's payload, not base's); IDs the layer dropped or
			// tombstoned stay hidden. Either way base's copy is
			// skipped, so a re-emitted ID is returned once.
			continue
		}
		out = append(out, n)
	}
	for _, path := range v.layer.FilePaths() {
		if !strings.HasPrefix(path, repoPrefix+"/") && path != repoPrefix {
			continue
		}
		out = append(out, v.layer.FileNodes(path)...)
	}
	return out
}

// baseEdgeVisible is the single predicate every reader applies to a
// base edge, so point, batched and bulk reads all expose the same
// edge relation.
//
// Recording side: the edge is dropped once the layer speaks for it —
// because it covers the file base recorded the edge in, or because it
// replaced the source's adjacency wherever that adjacency lives. An
// edge base recorded in a file the layer never claimed is not the
// layer's to replace and stays, even when its source lives in a covered
// file: re-deriving a symbol re-derives the edges its own file holds,
// and the edges a caller's file holds into it are the caller's.
//
// Endpoint side: the edge survives while both identities are still
// visible through the view. An endpoint the overlay re-emitted under the
// same ID keeps the edge (the logical symbol is still there); one the
// overlay hid removes it, source and target alike.
//
// Every side reads through the same ownership helpers the bounded
// adjacency readers use, so every edge surface answers alike.
func (v *OverlaidView) baseEdgeVisible(e *Edge) bool {
	if e == nil {
		return false
	}
	if v.layer == nil {
		return true
	}
	if v.overlayOwnsBaseEdge(e.From, e.FilePath) {
		return false
	}
	return v.overlayIdentityVisible(e.From) && v.overlayIdentityVisible(e.To)
}

// GetOutEdges merges the base edges out of a node that survive the
// overlay with the layer's own edges out of it, the way GetInEdges
// merges the inbound direction.
//
// The merge is what the per-edge ownership rule requires: a covered
// file's symbol keeps the base edges other files recorded out of it
// while the layer supplies the ones its own file holds, so neither side
// alone is the answer. baseEdgeVisible has already dropped everything
// the layer speaks for, so nothing surfaces twice.
func (v *OverlaidView) GetOutEdges(nodeID string) []*Edge {
	if v.layer == nil {
		if v.base == nil {
			return nil
		}
		return v.base.GetOutEdges(nodeID)
	}
	var out []*Edge
	if v.base != nil {
		for _, e := range v.base.GetOutEdges(nodeID) {
			if !v.baseEdgeVisible(e) {
				continue
			}
			out = append(out, e)
		}
	}
	return append(out, v.layer.OutEdges(nodeID)...)
}

// GetInEdges merges base's incoming edges (filtered to drop those
// originating in overlaid files, since those are replaced by overlay
// versions) with the overlay's in-edges for the same target.
func (v *OverlaidView) GetInEdges(nodeID string) []*Edge {
	if v.layer == nil {
		if v.base == nil {
			return nil
		}
		return v.base.GetInEdges(nodeID)
	}
	var out []*Edge
	if v.base != nil {
		for _, e := range v.base.GetInEdges(nodeID) {
			if !v.baseEdgeVisible(e) {
				continue
			}
			out = append(out, e)
		}
	}
	out = append(out, v.layer.InEdges(nodeID)...)
	return out
}

// GetOutEdgesByNodeIDs returns the overlay-aware outgoing-edge map for
// every input id. It is GetOutEdges's per-id semantics in one batched
// base round-trip: base's edges out of each id, filtered by the same
// visibility predicate, merged with the layer's own edges out of it.
//
// Every id goes to base, covered ones included, because a covered
// file's symbol can still hold base edges other files recorded out of
// it — a claim that is per edge and cannot be settled from the id.
func (v *OverlaidView) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]*Edge, len(ids))
	seen := make(map[string]struct{}, len(ids))
	uniq := ids[:0:0]
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return out
	}
	if v.base != nil {
		base := v.base.GetOutEdgesByNodeIDs(uniq)
		for _, id := range uniq {
			edges := base[id]
			if v.layer == nil {
				out[id] = edges
				continue
			}
			filtered := edges[:0:0]
			for _, e := range edges {
				if !v.baseEdgeVisible(e) {
					continue
				}
				filtered = append(filtered, e)
			}
			out[id] = filtered
		}
	}
	if v.layer != nil {
		for _, id := range uniq {
			if extras := v.layer.OutEdges(id); len(extras) > 0 {
				out[id] = append(out[id], extras...)
			}
		}
	}
	return out
}

// GetInEdgesByNodeIDs is the inbound sibling of GetOutEdgesByNodeIDs.
// Merges base in-edges (filtered to drop edges sourced in overlaid
// files) with overlay-introduced in-edges for each input id, all in a
// single batched base round-trip.
func (v *OverlaidView) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]*Edge, len(ids))
	seen := make(map[string]struct{}, len(ids))
	uniq := ids[:0:0]
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return out
	}
	if v.base != nil {
		base := v.base.GetInEdgesByNodeIDs(uniq)
		for _, id := range uniq {
			edges := base[id]
			if v.layer == nil {
				out[id] = edges
				continue
			}
			filtered := edges[:0:0]
			for _, e := range edges {
				if !v.baseEdgeVisible(e) {
					continue
				}
				filtered = append(filtered, e)
			}
			out[id] = filtered
		}
	}
	if v.layer != nil {
		for _, id := range uniq {
			if extras := v.layer.InEdges(id); len(extras) > 0 {
				out[id] = append(out[id], extras...)
			}
		}
	}
	return out
}

// AllNodes returns base's nodes minus every identity the layer speaks
// for, plus every node the overlay introduced. Bulk-read consumers
// (analyzers, search reindex, snapshot export) get an overlay-consistent
// view without paying any extra copy beyond the base snapshot's.
func (v *OverlaidView) AllNodes() []*Node {
	if v.base == nil {
		return nil
	}
	baseNodes := v.base.AllNodes()
	if v.layer == nil {
		return baseNodes
	}
	out := make([]*Node, 0, len(baseNodes))
	for _, n := range baseNodes {
		if !v.baseNodeVisible(n) {
			// Whatever the layer kept under this ID is appended below,
			// so a re-emitted identity surfaces once carrying the
			// layer's payload and a hidden one not at all.
			continue
		}
		out = append(out, n)
	}
	for n := range v.layer.Nodes() {
		out = append(out, n)
	}
	return out
}

// AllEdges returns the base edges that survive the overlay plus every
// overlay-introduced edge. Survival uses the same baseEdgeVisible
// predicate the point and batched readers apply, so a bulk read is
// set-equivalent to walking GetOutEdges over every visible source: a
// base edge from an unchanged source into a target the overlay
// re-emitted under the same ID stays, and it goes only when the
// overlay hid the target.
func (v *OverlaidView) AllEdges() []*Edge {
	if v.base == nil {
		return nil
	}
	baseEdges := v.base.AllEdges()
	if v.layer == nil {
		return baseEdges
	}
	out := make([]*Edge, 0, len(baseEdges))
	for _, e := range baseEdges {
		if !v.baseEdgeVisible(e) {
			continue
		}
		out = append(out, e)
	}
	for e := range v.layer.Edges() {
		out = append(out, e)
	}
	return out
}

// EdgesByKind is the kind-bounded sibling of AllEdges and yields the
// same relation filtered to one kind: base edges of that kind that
// survive baseEdgeVisible, then the layer's own edges of that kind.
// The base scan stays kind-bounded, so a disk backend still serves it
// from the kind index instead of materialising every edge.
func (v *OverlaidView) EdgesByKind(kind EdgeKind) iter.Seq[*Edge] {
	return func(yield func(*Edge) bool) {
		if v.base == nil {
			return
		}
		for e := range v.base.EdgesByKind(kind) {
			if v.layer != nil && !v.baseEdgeVisible(e) {
				continue
			}
			if !yield(e) {
				return
			}
		}
		if v.layer == nil {
			return
		}
		for e := range v.layer.Edges() {
			if e == nil || e.Kind != kind {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// NodesByKind is the kind-bounded sibling of AllNodes and yields the
// same relation filtered to one kind: base nodes of that kind whose
// identity the layer does not speak for, then the layer's own nodes of
// that kind. A node the overlay re-emitted under a base ID surfaces
// exactly once, carrying the layer's payload — base's copy is skipped
// whether the overlay replaced it, dropped it, or tombstoned it.
func (v *OverlaidView) NodesByKind(kind NodeKind) iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if v.base == nil {
			return
		}
		for n := range v.base.NodesByKind(kind) {
			if v.layer != nil && !v.baseNodeVisible(n) {
				continue
			}
			if !yield(n) {
				return
			}
		}
		if v.layer == nil {
			return
		}
		for n := range v.layer.Nodes() {
			if n == nil || n.Kind != kind {
				continue
			}
			if !yield(n) {
				return
			}
		}
	}
}

// NodeCount / EdgeCount — derived from base counters adjusted by the
// overlay delta. Cheap enough to recompute per call.
func (v *OverlaidView) NodeCount() int {
	if v.base == nil {
		return 0
	}
	if v.layer == nil {
		return v.base.NodeCount()
	}
	return v.base.NodeCount() + v.nodeCountDelta()
}

// nodeCountDelta is the overlay's net effect on any base node total:
// every covered file trades its base nodes for the layer's (none, for
// a tombstone), and every identity the layer speaks for from outside
// those files costs base its row. Walks the overlay's footprint only —
// never the graph.
func (v *OverlaidView) nodeCountDelta() int {
	if v.base == nil || v.layer == nil {
		return 0
	}
	delta := 0
	for _, path := range v.layer.FilePaths() {
		baseCount := len(v.base.GetFileNodes(path))
		if v.layer.IsTombstone(path) {
			delta -= baseCount
			continue
		}
		delta += len(v.layer.FileNodes(path)) - baseCount
	}
	return delta - len(v.detachedBaseNodes())
}

// detachedBaseNodes resolves the base rows the layer hides from outside
// every file it covers: an identity it tombstoned, and one it carries at
// a path other than the ID's own. Neither is priced by a covered file's
// node trade — that term only sees the paths in the layer's own file
// list — so the counters resolve them here, in one batched base read
// over the layer's removal set and its per-file node lists. Both are
// already in hand: the file lists are what the trade above walks.
func (v *OverlaidView) detachedBaseNodes() []*Node {
	if v.base == nil || v.layer == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var ids []string
	claim := func(id string) {
		if id == "" || v.layer.CoversNodeID(id) {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range v.layer.RemovedIDs() {
		claim(id)
	}
	for _, path := range v.layer.FilePaths() {
		for _, n := range v.layer.FileNodes(path) {
			if n != nil {
				claim(n.ID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	out := make([]*Node, 0, len(ids))
	for _, n := range v.base.GetNodesByIDs(ids) {
		if n != nil {
			out = append(out, n)
		}
	}
	return out
}

func (v *OverlaidView) EdgeCount() int {
	if v.base == nil {
		return 0
	}
	if v.layer == nil {
		return v.base.EdgeCount()
	}
	return len(v.AllEdges())
}

// EdgeIdentityRevisions stays base-derived under an overlay:
// provenance churn is a property of the persistent graph, and an
// overlay layer is a non-mutating per-session shadow that never
// upgrades edge provenance, so it contributes no revisions.
func (v *OverlaidView) EdgeIdentityRevisions() int {
	if v.base == nil {
		return 0
	}
	return v.base.EdgeIdentityRevisions()
}

// Stats reports base's analyzer-shaped stats with the overlay folded
// into the totals: TotalNodes carries the same node delta NodeCount
// applies, TotalEdges the same delta EdgeCount applies, so the three
// counters agree on one graph size.
//
// Base-derived under an overlay (deliberately not adjusted):
//   - ByKind and ByLanguage — the layer keeps no per-kind /
//     per-language rollup, and recomputing one means walking every
//     node in the graph.
//
// Caching keeps repeated Stats() calls inside one request to a single
// base lookup.
func (v *OverlaidView) Stats() GraphStats {
	if v.base == nil {
		return GraphStats{}
	}
	v.statsOnce.Do(func() {
		v.stats = v.base.Stats()
		if v.layer == nil {
			return
		}
		v.stats.TotalNodes += v.nodeCountDelta()
		v.stats.TotalEdges += v.EdgeCount() - v.base.EdgeCount()
	})
	return v.stats
}

// RepoStats returns base's per-repo rollup with the overlay's node and
// edge deltas folded into the repo each row belongs to. Only the
// overlay's own footprint is walked — the covered files' base nodes,
// the layer's replacements and its removal set — so the call stays
// proportional to the overlay, not to the graph.
//
// Base-derived under an overlay (deliberately not adjusted):
//   - ByKind and ByLanguage — same reason as Stats.
func (v *OverlaidView) RepoStats() map[string]GraphStats {
	if v.base == nil {
		return nil
	}
	stats := v.base.RepoStats()
	if v.layer == nil {
		return stats
	}
	nodeDelta, edgeDelta := v.repoCountDeltas()
	if len(nodeDelta) == 0 && len(edgeDelta) == 0 {
		return stats
	}
	if stats == nil {
		stats = make(map[string]GraphStats, len(nodeDelta))
	}
	apply := func(prefix string) {
		entry := stats[prefix]
		entry.TotalNodes += nodeDelta[prefix]
		entry.TotalEdges += edgeDelta[prefix]
		stats[prefix] = entry
	}
	for prefix := range nodeDelta {
		apply(prefix)
	}
	for prefix := range edgeDelta {
		if _, done := nodeDelta[prefix]; done {
			continue
		}
		apply(prefix)
	}
	return stats
}

// repoCountDeltas returns the per-repo node and edge deltas the
// overlay induces, keyed by repo prefix. Edges are charged to the
// source node's repo, the same attribution base's RepoStats uses, and
// an edge counts as gone exactly when baseEdgeVisible says the readers
// hide it. Nodes and edges outside a repo (empty prefix) are skipped —
// base leaves them out of the per-repo rollup too.
//
// Every lookup is keyed off the overlay's own footprint: the covered
// files' base nodes, their adjacency, and the layer's replacements.
// Identities the layer speaks for from outside a covered file are
// charged on the node side — the same rows nodeCountDelta prices, read
// in one batched lookup over the layer's own removal set and file
// lists. Adjacency the layer owns outside a covered file — a retargeted
// edge set, or base edges out of an identity the layer only marked
// removed — is not charged to a repo here, because reaching it would
// mean walking the graph rather than the overlay. The whole-graph
// counters (EdgeCount and the Stats totals derived from it) stay exact
// either way: they count AllEdges instead of summing these deltas.
func (v *OverlaidView) repoCountDeltas() (map[string]int, map[string]int) {
	nodes := make(map[string]int)
	edges := make(map[string]int)
	// Sources outside the covered files that lose an edge because the
	// overlay hid its target. Charged to the source's repo, so the
	// sources are resolved in one batched node lookup at the end.
	lostBySource := make(map[string]int)
	for _, path := range v.layer.FilePaths() {
		baseNodes := v.base.GetFileNodes(path)
		repoByID := make(map[string]string, len(baseNodes))
		baseIDs := make([]string, 0, len(baseNodes))
		for _, n := range baseNodes {
			if n == nil || n.RepoPrefix == "" {
				continue
			}
			nodes[n.RepoPrefix]--
			repoByID[n.ID] = n.RepoPrefix
			baseIDs = append(baseIDs, n.ID)
		}
		if len(baseIDs) > 0 {
			// A covered file replaces the edges recorded in it, not
			// every edge leaving its symbols, so each base row is
			// priced by the predicate the readers apply to it.
			for id, outEdges := range v.base.GetOutEdgesByNodeIDs(baseIDs) {
				prefix := repoByID[id]
				if prefix == "" {
					continue
				}
				for _, e := range outEdges {
					if e == nil || v.baseEdgeVisible(e) {
						continue
					}
					edges[prefix]--
				}
			}
			for _, inEdges := range v.base.GetInEdgesByNodeIDs(baseIDs) {
				for _, e := range inEdges {
					if e == nil || v.baseEdgeVisible(e) {
						continue
					}
					if v.layer.CoversNodeID(e.From) {
						continue // already charged on the out-edge side
					}
					lostBySource[e.From]++
				}
			}
		}
		if v.layer.IsTombstone(path) {
			continue
		}
		for _, n := range v.layer.FileNodes(path) {
			if n == nil || n.RepoPrefix == "" {
				continue
			}
			nodes[n.RepoPrefix]++
			edges[n.RepoPrefix] += len(v.layer.OutEdges(n.ID))
		}
	}
	if len(lostBySource) > 0 {
		sourceIDs := make([]string, 0, len(lostBySource))
		for id := range lostBySource {
			sourceIDs = append(sourceIDs, id)
		}
		for id, n := range v.base.GetNodesByIDs(sourceIDs) {
			if n != nil && n.RepoPrefix != "" {
				edges[n.RepoPrefix] -= lostBySource[id]
			}
		}
	}
	for _, n := range v.detachedBaseNodes() {
		if n.RepoPrefix != "" {
			nodes[n.RepoPrefix]--
		}
	}
	return nodes, edges
}

// Compile-time assertion that *OverlaidView satisfies Reader.
var _ Reader = (*OverlaidView)(nil)
