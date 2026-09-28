package analysis

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// CallRefProjection is one analysis pass's shared read of the call/reference
// graph. PageRank, HITS and the adjacency snapshot each stream the same input
// — every node (NodesLightSeq) and every call and reference edge
// (EdgesLightSeq(EdgeCalls, EdgeReferences)), each after a NodeCount — so a
// pass that ran them over the store read the whole corpus three times. Over a
// CallRefProjection the first consumer reads the store and the projection
// records what it saw; the others replay the record from memory.
//
// Exactness. A replay yields the same nodes and edges in the same order the
// store streamed them, carrying every field the three consumers read: a node's
// ID, Stub and Origin (IsProxyNode); an edge's From, To, Kind and its
// provenance weight. The weight is kept by storing the edge's effective origin
// as its Origin, which is what graph.ProvenanceWeight reads, so the weighted
// sums are the same floating-point operations in the same order.
// TestCallRefProjectionReplaysTheStoreExactly pins the three results against
// their direct reads.
//
// Only the exact call/reference kind list is recorded; any other read is the
// store's own. A consumer that stops early leaves no record, and the next one
// reads the store again. The record is compact (interned endpoint strings,
// 16 bytes per edge) and lives only as long as the projection; drop it when
// the pass no longer needs it.
type CallRefProjection struct {
	graph.Store
	pace *Pace

	nodesRecorded bool
	nodeIDs       []string
	nodeStub      []bool
	nodeOrigin    []uint32 // index into origins
	origins       []string
	originIndex   map[string]uint32

	edgesRecorded bool
	endpoints     []string
	endpointIndex map[string]int32
	edgeFrom      []int32
	edgeTo        []int32
	edgeKind      []uint16 // index into kinds
	edgeOrigin    []uint32 // index into origins
	kinds         []graph.EdgeKind
	kindIndex     map[graph.EdgeKind]uint16

	storeNodeScans int
	storeEdgeScans int
}

var (
	_ graph.NodeLightSequencer = (*CallRefProjection)(nil)
	_ graph.LightEdgeSequencer = (*CallRefProjection)(nil)
)

// NewCallRefProjection returns a projection over g. The Pace ticks once per
// row the projection records or replays.
func NewCallRefProjection(g graph.Store, pace *Pace) *CallRefProjection {
	return &CallRefProjection{Store: g, pace: pace, originIndex: map[string]uint32{}, endpointIndex: map[string]int32{}, kindIndex: map[graph.EdgeKind]uint16{}}
}

// StoreScans reports how many times the projection read the store's nodes
// and call/reference edges.
func (c *CallRefProjection) StoreScans() (nodes, edges int) {
	return c.storeNodeScans, c.storeEdgeScans
}

// NodeCount is the recorded node count once the nodes were read, and zero
// before: the consumers use it only as a capacity hint, and the store's count
// is a whole-table scan.
func (c *CallRefProjection) NodeCount() int {
	if c.nodesRecorded {
		return len(c.nodeIDs)
	}
	return 0
}

func (c *CallRefProjection) origin(o string) uint32 {
	if i, ok := c.originIndex[o]; ok {
		return i
	}
	i := uint32(len(c.origins))
	c.origins = append(c.origins, o)
	c.originIndex[o] = i
	return i
}

func (c *CallRefProjection) endpoint(id string) int32 {
	if i, ok := c.endpointIndex[id]; ok {
		return i
	}
	i := int32(len(c.endpoints))
	c.endpoints = append(c.endpoints, id)
	c.endpointIndex[id] = i
	return i
}

func (c *CallRefProjection) kind(k graph.EdgeKind) uint16 {
	if i, ok := c.kindIndex[k]; ok {
		return i
	}
	i := uint16(len(c.kinds))
	c.kinds = append(c.kinds, k)
	c.kindIndex[k] = i
	return i
}

// NodesLightSeq replays the recorded nodes, or reads and records them.
func (c *CallRefProjection) NodesLightSeq() iter.Seq[*graph.Node] {
	if c.nodesRecorded {
		return func(yield func(*graph.Node) bool) {
			for i, id := range c.nodeIDs {
				c.pace.Tick()
				if !yield(&graph.Node{ID: id, Stub: c.nodeStub[i], Origin: c.origins[c.nodeOrigin[i]]}) {
					return
				}
			}
		}
	}
	return func(yield func(*graph.Node) bool) {
		c.storeNodeScans++
		c.nodeIDs, c.nodeStub, c.nodeOrigin = c.nodeIDs[:0], c.nodeStub[:0], c.nodeOrigin[:0]
		for node := range graph.NodesLightSeq(c.Store) {
			if node == nil {
				continue
			}
			c.nodeIDs = append(c.nodeIDs, node.ID)
			c.nodeStub = append(c.nodeStub, node.Stub)
			c.nodeOrigin = append(c.nodeOrigin, c.origin(node.Origin))
			if !yield(node) {
				c.nodeIDs, c.nodeStub, c.nodeOrigin = nil, nil, nil
				return
			}
		}
		c.nodesRecorded = true
	}
}

// isCallRefKinds reports whether kinds is exactly the consumers' kind list.
func isCallRefKinds(kinds []graph.EdgeKind) bool {
	return len(kinds) == 2 && kinds[0] == graph.EdgeCalls && kinds[1] == graph.EdgeReferences
}

// EdgesLightSeq replays the recorded call/reference edges, or reads and
// records them. Any other kind list is the store's own read.
func (c *CallRefProjection) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	if !isCallRefKinds(kinds) {
		return graph.EdgesLightSeq(c.Store, kinds...)
	}
	if c.edgesRecorded {
		return func(yield func(*graph.Edge) bool) {
			for i := range c.edgeFrom {
				c.pace.Tick()
				e := &graph.Edge{
					From:   c.endpoints[c.edgeFrom[i]],
					To:     c.endpoints[c.edgeTo[i]],
					Kind:   c.kinds[c.edgeKind[i]],
					Origin: c.origins[c.edgeOrigin[i]],
				}
				if !yield(e) {
					return
				}
			}
		}
	}
	return func(yield func(*graph.Edge) bool) {
		c.storeEdgeScans++
		c.edgeFrom, c.edgeTo, c.edgeKind, c.edgeOrigin = c.edgeFrom[:0], c.edgeTo[:0], c.edgeKind[:0], c.edgeOrigin[:0]
		for e := range graph.EdgesLightSeq(c.Store, kinds...) {
			if e == nil {
				continue
			}
			c.edgeFrom = append(c.edgeFrom, c.endpoint(e.From))
			c.edgeTo = append(c.edgeTo, c.endpoint(e.To))
			c.edgeKind = append(c.edgeKind, c.kind(e.Kind))
			c.edgeOrigin = append(c.edgeOrigin, c.origin(e.EffectiveOrigin()))
			if !yield(e) {
				c.edgeFrom, c.edgeTo, c.edgeKind, c.edgeOrigin = nil, nil, nil, nil
				return
			}
		}
		c.edgesRecorded = true
	}
}

// Release drops the record.
func (c *CallRefProjection) Release() {
	*c = CallRefProjection{Store: c.Store}
}
