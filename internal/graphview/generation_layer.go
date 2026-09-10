package graphview

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// GenerationLayer reads one persisted payload generation as an overlay
// layer, so a generation composes over the layer beneath it through the
// same graph.OverlaidView the in-memory editor-buffer layer uses.
//
// The handle is pinned to the generation, and every read it serves
// already binds that generation, so the layer never has to filter what
// storage hands back: what the handle returns IS what this layer
// carries. The three ownership questions the composition asks on top of
// the payload — which paths the generation claims, which identities it
// removed, whose outgoing edge set it replaced — come from the
// generation's masks.
//
// # What the masks mean here
//
//   - A file mask covers a path whether it says replace or delete;
//     either way the layer below stops showing through for the nodes
//     that live at the path and the edges recorded there. Edges the
//     layer below recorded in OTHER paths keep showing through even
//     when they leave a symbol at a claimed one — the generation did
//     not re-derive the file that holds them. A delete mask
//     additionally reports the path as a tombstone, so the composition
//     answers it as empty rather than from the layer's own payload.
//   - A node tombstone removes one identity the generation did not
//     re-emit and whose file it does not claim.
//   - An edge-source marker replaces one node's outgoing edge set
//     without claiming the node: a rename in one file retargets the
//     calls made from an untouched one, and re-deriving the whole
//     untouched file to say so would defeat the sparse generation.
//     Such a source owns its adjacency and nothing else — the node
//     itself keeps coming from the layer below.
//
// # Cost model
//
// The composition asks two kinds of question: membership probes, which
// it runs per node or per edge, and content reads, which it runs per
// key the caller named. They are served differently on purpose.
//
//   - Prefetched whole, once, at construction: covered paths, node-identity
//     masks, edge-source markers, and summaries fetched ONLY for mask IDs.
//     Membership probes run per lower node/edge without per-row SQL; ordinary
//     file deltas perform no upper-node enumeration for identity membership.
//   - Point reads, memoized for the layer's lifetime: NodeByID (misses
//     included, so a repeated absence costs one query too) and FileNodes
//     (prefetched per touched file, since a file's nodes are always
//     wanted together). OwnsNodeIdentity is the one probe that is not
//     answered from memory alone — it falls through to NodeByID — and it
//     is asked only about identities whose file the generation claims,
//     which the prefetched mask set settles first.
//   - Bounded indexed queries, not memoized: NodeByQualName, NodesByName,
//     OutEdges and InEdges. Each is one indexed seek and the composition
//     asks each at most once per distinct key.
//   - Materialized once: Nodes, NamedNodes and Edges. Their callers are
//     the bulk readers, which are already walking the whole graph, and a
//     generation is by construction sparse — the materialization is
//     bounded by what the generation re-derived, not by the corpus.
//
// The caches live exactly as long as the layer and are shared with
// nothing else. That is what makes them safe to hold without an
// invalidation path: a published generation is immutable, and the lease
// a materialized view holds refuses its retirement, so nothing the layer
// memoized can change underneath it. A generation rebuilt after
// retirement is a new id read through a new layer with an empty cache.
//
// # Precondition
//
// A stored node at a masked path participates in whole-file replacement.
// An explicit node-identity mask speaks for its ID outside masked files.
// Identity-only replacement preserves adjacency; legacy tombstones retain
// their old outgoing-set ownership with or without a carried node row.
// Payload and masks must be immutable before construction; this cached layer
// is not a live BUILDING-generation reader.
//
// A GenerationLayer is safe for concurrent reads from one request.
type GenerationLayer struct {
	handle *store_sqlite.Store
	// Set by the materializer before publication. A healthy checkout must
	// not inherit filesystem failures from its designated primary.
	failureRepoPrefix string
	failureRepoScoped bool

	// covered maps a covered graph path to what the generation claims
	// about it. Both modes hide the layer below; only delete tombstones.
	// A mask also carries the repo prefix, which is not part of this key:
	// every path in the graph is already repo-prefixed, so the prefix
	// column repeats what the path says rather than qualifying it.
	covered map[string]store_sqlite.OwnershipMode
	// paths is covered's key set in sorted order, built once because
	// FilePaths promises that order.
	paths []string
	// removed is the node-tombstone set, and removedIDs its sorted form.
	removed   map[string]struct{}
	removedID []string
	// edgeSources is the set of nodes whose outgoing edge set this
	// generation replaces without claiming their file.
	edgeSources map[string]struct{}

	// Explicit mask-backed rows at unclaimed paths carry node identity. The
	// checked marker-ID projection is captured once for this immutable layer;
	// lower-row membership probes never turn into per-row SQL.
	detachedIDs         map[string]struct{}
	detachedNodes       []graph.Node
	detachedFileIndexes map[string][]int
	detachedPaths       map[string]struct{}
	detachedRepos       map[string]struct{}

	mu        sync.Mutex
	nodeByID  map[string]*graph.Node
	fileNodes map[string][]*graph.Node

	nodesOnce sync.Once
	nodes     []*graph.Node
	named     map[string][]*graph.Node

	edgesOnce sync.Once
	edges     []*graph.Edge
}

// Compile-time assertion that the persisted layer answers the same
// contract the in-memory one does.
var _ graph.OverlayLayerReader = (*GenerationLayer)(nil)

// NewGenerationLayer reads a generation's masks and returns the layer
// over them. handle must be pinned to the generation being read; a base
// handle states no ownership claims and would compose as a layer that
// covers nothing, so it is refused rather than silently serving an empty
// overlay.
func NewGenerationLayer(handle *store_sqlite.Store) (*GenerationLayer, error) {
	if handle == nil {
		return nil, fmt.Errorf("graphview: generation layer needs a store handle")
	}
	generation := handle.ViewGeneration()
	if generation <= 0 {
		return nil, fmt.Errorf("graphview: generation layer needs a derived generation, got %d", generation)
	}

	fileMasks, err := handle.FileMasks()
	if err != nil {
		return nil, fmt.Errorf("graphview: read file masks of generation %d: %w", generation, err)
	}
	// Read the node masks once. Derive legacy removals from the same checked
	// enumeration rather than adding a second query for identity-only masks.
	identityMasks, err := handle.NodeIdentityMasksContext(context.Background())
	if err != nil {
		return nil, fmt.Errorf("graphview: read node identity masks of generation %d: %w", generation, err)
	}
	var tombstones []string
	for _, mask := range identityMasks {
		if mask.Kind == store_sqlite.NodeIdentityMaskLegacy {
			tombstones = append(tombstones, mask.NodeID)
		}
	}
	edgeSources, err := handle.EdgeSourceMasks()
	if err != nil {
		return nil, fmt.Errorf("graphview: read edge-source masks of generation %d: %w", generation, err)
	}
	// Fetch only explicit marker IDs, never enumerate the upper payload.
	// An empty marker set performs no node query.
	summaries, err := handle.NodeIdentityMaskSummariesContext(context.Background(), identityMasks)
	if err != nil {
		return nil, fmt.Errorf("graphview: read node identities of generation %d: %w", generation, err)
	}

	l := &GenerationLayer{
		handle:      handle,
		covered:     make(map[string]store_sqlite.OwnershipMode, len(fileMasks)),
		removed:     make(map[string]struct{}, len(tombstones)),
		removedID:   slices.Clone(tombstones),
		edgeSources: make(map[string]struct{}, len(edgeSources)),
		nodeByID:    make(map[string]*graph.Node),
		fileNodes:   make(map[string][]*graph.Node),
	}
	for _, mask := range fileMasks {
		l.covered[mask.FilePath] = mask.Mode
	}
	l.paths = slices.Sorted(maps.Keys(l.covered))
	for _, id := range tombstones {
		l.removed[id] = struct{}{}
	}
	slices.Sort(l.removedID)
	for _, mask := range edgeSources {
		l.edgeSources[mask.SourceID] = struct{}{}
	}
	for _, node := range summaries {
		if node == nil || node.ID == "" {
			return nil, fmt.Errorf("graphview: invalid node identity in generation %d", generation)
		}
		if l.HasFile(node.FilePath) {
			continue
		}
		if l.detachedIDs == nil {
			l.detachedIDs = make(map[string]struct{})
			l.detachedPaths = make(map[string]struct{})
			l.detachedRepos = make(map[string]struct{})
			l.detachedFileIndexes = make(map[string][]int)
		}
		l.detachedIDs[node.ID] = struct{}{}
		l.detachedFileIndexes[node.FilePath] = append(l.detachedFileIndexes[node.FilePath], len(l.detachedNodes))
		l.detachedNodes = append(l.detachedNodes, *node)
		l.detachedPaths[node.FilePath] = struct{}{}
		l.detachedRepos[node.RepoPrefix] = struct{}{}
	}
	return l, nil
}

// HasFile reports whether the generation claims a path, under either
// ownership mode.
func (l *GenerationLayer) HasFile(graphPath string) bool {
	_, ok := l.covered[graphPath]
	return ok
}

// IsTombstone reports whether the generation's claim on a path is a
// deletion, so nothing below shows through and the layer itself carries
// nothing for it either.
func (l *GenerationLayer) IsTombstone(graphPath string) bool {
	return l.covered[graphPath] == store_sqlite.OwnershipDelete
}

// FilePaths lists every claimed path in sorted order.
func (l *GenerationLayer) FilePaths() []string { return slices.Clone(l.paths) }

// CoversNodeID reports whether the generation claims the file an ID
// belongs to. A symbol ID carries its file before the "::" separator; a
// file node's ID is the bare path, which IDFile reports as no file, so
// the ID itself is checked against the covered set.
func (l *GenerationLayer) CoversNodeID(id string) bool {
	if id == "" {
		return false
	}
	if file := graph.IDFile(id); file != "" {
		return l.HasFile(file)
	}
	return l.HasFile(id)
}

// OwnsNodeIdentity reports whether the generation speaks for an ID.
// An explicit identity-only marker replaces a node without owning outgoing
// adjacency. Legacy tombstones preserve their independent outgoing claim.
func (l *GenerationLayer) OwnsNodeIdentity(id string) bool {
	if id == "" {
		return false
	}
	if l.IsRemovedID(id) {
		return true
	}
	if _, replaced := l.detachedIDs[id]; replaced {
		return true
	}
	return l.CoversNodeID(id) && l.NodeByID(id) != nil
}

// OwnsOutEdges reports whether the generation replaces a node's whole
// outgoing edge set wherever the layer below recorded it. Claiming the
// node's file is deliberately NOT such a claim: re-deriving a file
// re-derives the edges recorded in it, and the composition settles
// those against the generation's file masks, edge by edge. What is left
// here is the adjacency no file mask reaches — a tombstoned identity,
// and an edge-source replacement marker, which is the case a
// file-granular layer cannot express: the node stays where it was and
// only what it points at moved.
func (l *GenerationLayer) OwnsOutEdges(id string) bool {
	if id == "" || l.CoversNodeID(id) {
		return false
	}
	if _, marked := l.edgeSources[id]; marked {
		return true
	}
	// Identity-only masks do not own adjacency. Legacy tombstones retain
	// outgoing ownership, whether or not they also carry a node row.
	return l.IsRemovedID(id)
}

// IsRemovedID reports whether the generation tombstoned an identity.
func (l *GenerationLayer) IsRemovedID(id string) bool {
	_, ok := l.removed[id]
	return ok
}

// RemovedIDs iterates the tombstoned identities.
func (l *GenerationLayer) RemovedIDs() iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, id := range l.removedID {
			if !yield(id) {
				return
			}
		}
	}
}

// IsNameRemoved reports whether the generation hid the ID a lower layer
// carries under this short name. The tombstone table records identities,
// not names, and hiding a tombstoned identity is right under whatever
// name it was found by, so the name is not consulted.
func (l *GenerationLayer) IsNameRemoved(_, id string) bool { return l.IsRemovedID(id) }

// RemovedIDsForName lists the identities the generation can hide from a
// name lookup. It is the whole tombstone set rather than a per-name
// slice, for the same reason IsNameRemoved ignores the name: the
// persisted tombstones are identities. The set is used to compensate a
// bounded page for rows that may vanish, so naming an identity that this
// particular lookup could not have returned costs a slot in that
// compensation and changes no answer.
//
// The cost is a slot, not nothing: the bounded exact-name projection
// budgets its inspection over this set, so a generation carrying more
// tombstones than that budget makes the projection refuse with its
// typed limit error for every name, not only for the removed ones.
// Narrowing the set would mean guessing which name a removed identity
// was known by, and a wrong guess leaves a row in the page that the
// generation removed — a stale answer instead of a refused one.
func (l *GenerationLayer) RemovedIDsForName(name string) []string {
	if name == "" || len(l.removedID) == 0 {
		return nil
	}
	return slices.Clone(l.removedID)
}

// NodeByID returns the generation's node for an ID, or nil when it
// carries none. Misses are memoized too: an ID the composition probes
// repeatedly costs one query whether or not the generation has it.
func (l *GenerationLayer) NodeByID(id string) *graph.Node {
	if id == "" {
		return nil
	}
	l.mu.Lock()
	cached, ok := l.nodeByID[id]
	l.mu.Unlock()
	if ok {
		return cached
	}
	node := l.handle.GetNode(id)
	l.mu.Lock()
	l.nodeByID[id] = node
	l.mu.Unlock()
	return node
}

// NodeByQualName returns the generation's node for a qualified name.
func (l *GenerationLayer) NodeByQualName(qualName string) *graph.Node {
	if qualName == "" {
		return nil
	}
	return l.handle.GetNodeByQualName(qualName)
}

// NodesByName returns the generation's nodes carrying one short name.
func (l *GenerationLayer) NodesByName(name string) []*graph.Node {
	if name == "" {
		return nil
	}
	return l.handle.FindNodesByName(name)
}

// NamedNodes iterates the generation's short names with the nodes
// carrying each, grouped from the one whole-generation scan Nodes uses.
func (l *GenerationLayer) NamedNodes() iter.Seq2[string, []*graph.Node] {
	return func(yield func(string, []*graph.Node) bool) {
		l.loadNodes()
		for name, bucket := range l.named {
			if !yield(name, bucket) {
				return
			}
		}
	}
}

// Nodes iterates every node the generation carries.
func (l *GenerationLayer) Nodes() iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		l.loadNodes()
		for _, n := range l.nodes {
			if !yield(n) {
				return
			}
		}
	}
}

// loadNodes materializes the generation's node set and its short-name
// index in one scan. Both are whole-layer reads, so paying for the scan
// twice would only add a second round trip for the same rows.
func (l *GenerationLayer) loadNodes() {
	l.nodesOnce.Do(func() {
		l.nodes = l.handle.AllNodes()
		l.named = make(map[string][]*graph.Node, len(l.nodes))
		for _, n := range l.nodes {
			if n == nil || n.Name == "" {
				continue
			}
			l.named[n.Name] = append(l.named[n.Name], n)
		}
	})
}

// FileNodes returns the generation's nodes for a claimed path — nil for
// a tombstone and for a path the generation does not claim, because
// neither is a file this layer answers from its own payload.
func (l *GenerationLayer) FileNodes(graphPath string) []*graph.Node {
	mode, claimed := l.covered[graphPath]
	if !claimed || mode == store_sqlite.OwnershipDelete {
		return nil
	}
	l.mu.Lock()
	cached, ok := l.fileNodes[graphPath]
	l.mu.Unlock()
	if ok {
		return cached
	}
	nodes := l.handle.GetFileNodes(graphPath)
	l.mu.Lock()
	l.fileNodes[graphPath] = nodes
	l.mu.Unlock()
	return nodes
}

// OutEdges returns the generation's edges leaving one node.
func (l *GenerationLayer) OutEdges(nodeID string) []*graph.Edge {
	if nodeID == "" {
		return nil
	}
	return l.handle.GetOutEdges(nodeID)
}

// InEdges returns the generation's edges entering one node.
func (l *GenerationLayer) InEdges(nodeID string) []*graph.Edge {
	if nodeID == "" {
		return nil
	}
	return l.handle.GetInEdges(nodeID)
}

// Edges iterates every edge the generation carries.
func (l *GenerationLayer) Edges() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		l.edgesOnce.Do(func() { l.edges = l.handle.AllEdges() })
		for _, e := range l.edges {
			if !yield(e) {
				return
			}
		}
	}
}
