package graph

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
)

// DeltaPayload is what a delta publishes: the rows that differ from the view
// below and the ownership claims that make the composition serve them.
//
//   - ReplacePaths: file masks that replace a path. Nodes carries every node
//     at such a path and Edges every edge recorded at it.
//   - DeletePaths: file masks for paths the delta emptied.
//   - EdgeSources: sources whose whole outgoing edge set the delta replaces
//     (edge-source markers). Edges carries every edge out of each.
//   - Tombstones: identities outside every replaced path that the delta
//     carries (Nodes has the row) or removes (no row).
//   - IdentityClaims: identities of rows carried at a replaced path whose
//     identity names no replaced path (a shared registry row), which the
//     composition serves from this generation only under an identity claim.
//
// Everything a claim would restate — a covered path whose rows equal the view
// below, a claimed source whose final edge set equals what the view below
// shows, a detached node identical to the one below — is left out.
type DeltaPayload struct {
	ReplacePaths []string
	DeletePaths  []string
	EdgeSources  []string
	Tombstones   []string
	// IdentityClaims are node-identity replacement claims for rows Nodes
	// carries at a replaced path.
	IdentityClaims []string
	Nodes          []*Node
	Edges          []*Edge

	// Dropped counts the claims the payload left out because they restated
	// the layer below, by kind.
	DroppedPaths   int
	DroppedSources int
	DroppedNodes   int
	// OrphanEdges counts edges recorded at an emptied path that the payload
	// could not carry (a delete mask carries no rows).
	OrphanEdges int
	// RestatedNodes / RestatedEdges count the rows at replaced paths that are
	// identical to the view below's rows there: what a file mask forces the
	// generation to restate because it replaces the whole path.
	RestatedNodes int
	RestatedEdges int
}

// deltaEdgeRender is an edge's persisted content, the fields a store keeps.
func deltaEdgeRender(e *Edge) string {
	if e == nil {
		return "<nil>"
	}
	c := *e
	c.Context = ""
	c.ReturnUsage = ""
	c.Via = ""
	c.Alias = ""
	c.NameOnly = false
	return fmt.Sprintf("%+v", c)
}

// deltaNodeRender is a node's persisted content.
func deltaNodeRender(n *Node) string {
	if n == nil {
		return "<nil>"
	}
	c := *n
	c.AbsoluteFilePath = ""
	return fmt.Sprintf("%+v", c)
}

// deltaEdgeSetsEqual reports whether two edge sets hold the same persisted
// rows. Rows pair up by identity and compare field by field; only a Meta
// pair that is not deeply equal (a store decodes lists as []any where the
// engine holds []string) is compared by its rendering.
func deltaEdgeSetsEqual(a, b []*Edge) bool {
	if len(a) != len(b) {
		return false
	}
	byKey := make(map[edgeHash][]*Edge, len(b))
	for _, e := range b {
		if e == nil {
			return deltaEdgeSetsEqualRendered(a, b)
		}
		h := hashEdgeKey(keyOf(e))
		byKey[h] = append(byKey[h], e)
	}
	for _, e := range a {
		if e == nil {
			return deltaEdgeSetsEqualRendered(a, b)
		}
		h := hashEdgeKey(keyOf(e))
		candidates := byKey[h]
		if len(candidates) != 1 {
			return deltaEdgeSetsEqualRendered(a, b)
		}
		if !deltaEdgeContentEqual(e, candidates[0]) {
			return false
		}
	}
	return true
}

func deltaEdgeContentEqual(a, b *Edge) bool {
	if a.From != b.From || a.To != b.To || a.Kind != b.Kind || a.FilePath != b.FilePath || a.Line != b.Line ||
		a.Confidence != b.Confidence || a.ConfidenceLabel != b.ConfidenceLabel || a.Origin != b.Origin ||
		a.Tier != b.Tier || a.CrossRepo != b.CrossRepo || len(a.Meta) != len(b.Meta) {
		return false
	}
	if deltaEdgeFields != deltaEdgeFieldsCompared {
		return deltaEdgeRender(a) == deltaEdgeRender(b)
	}
	if len(a.Meta) == 0 || reflect.DeepEqual(a.Meta, b.Meta) {
		return true
	}
	return deltaEdgeRender(a) == deltaEdgeRender(b)
}

// deltaEdgeFieldsCompared is the number of Edge fields deltaEdgeContentEqual
// accounts for (the ten it compares, Meta, and the five that are not
// persisted). An Edge with another field falls back to the rendered
// comparison until the comparison learns it.
const deltaEdgeFieldsCompared = 16

var deltaEdgeFields = reflect.TypeOf(Edge{}).NumField()

func deltaEdgeSetsEqualRendered(a, b []*Edge) bool {
	count := make(map[string]int, len(a))
	for _, e := range a {
		count[deltaEdgeRender(e)]++
	}
	for _, e := range b {
		key := deltaEdgeRender(e)
		if count[key] == 0 {
			return false
		}
		count[key]--
	}
	return true
}

func deltaNodeSetsEqual(a, b []*Node) bool {
	if len(a) != len(b) {
		return false
	}
	count := make(map[string]int, len(a))
	for _, n := range a {
		count[deltaNodeRender(n)]++
	}
	for _, n := range b {
		key := deltaNodeRender(n)
		if count[key] == 0 {
			return false
		}
		count[key]--
	}
	return true
}

// Payload computes what the delta publishes. It reads the working graph and
// the view below; call it once the engine has finished writing.
func (dw *DeltaWriter) Payload(fixedPaths map[string]struct{}) DeltaPayload {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	var out DeltaPayload

	dw.layer.mu.RLock()
	covered := make([]string, 0, len(dw.layer.covered))
	for p := range dw.layer.covered {
		covered = append(covered, p)
	}
	claimed := make([]string, 0, len(dw.layer.claimed))
	for id := range dw.layer.claimed {
		claimed = append(claimed, id)
	}
	removed := make([]string, 0, len(dw.layer.removed))
	for id := range dw.layer.removed {
		removed = append(removed, id)
	}
	dw.layer.mu.RUnlock()
	sort.Strings(covered)
	sort.Strings(claimed)
	sort.Strings(removed)

	workEdges := dw.work.AllEdges()
	recordedAt := make(map[string][]*Edge)
	for _, e := range workEdges {
		if e != nil {
			recordedAt[e.FilePath] = append(recordedAt[e.FilePath], e)
		}
	}

	// A covered path whose rows equal the layer below is not published: it
	// was covered on the way to a write that restored it. The paths the
	// caller changed are always published.
	var belowRecorded RecordedEdgeReader
	if reader, ok := RecordedEdgesOf(dw.below); ok {
		belowRecorded = reader
	}
	final := make(map[string]struct{}, len(covered))
	emptied := make(map[string]struct{})
	for _, p := range covered {
		nodes := dw.work.GetFileNodes(p)
		edges := recordedAt[p]
		if _, fixed := fixedPaths[p]; !fixed && belowRecorded != nil &&
			deltaNodeSetsEqual(nodes, dw.below.GetFileNodes(p)) &&
			deltaEdgeSetsEqual(edges, belowRecorded.RecordedEdgesAt([]string{p})) {
			out.DroppedPaths++
			continue
		}
		if len(nodes) == 0 {
			emptied[p] = struct{}{}
			out.DeletePaths = append(out.DeletePaths, p)
			out.OrphanEdges += len(edges)
			continue
		}
		final[p] = struct{}{}
		out.ReplacePaths = append(out.ReplacePaths, p)
	}

	emitted := make(map[edgeHash]struct{})
	emit := func(e *Edge) {
		h := hashEdgeKey(keyOf(e))
		if _, dup := emitted[h]; dup {
			return
		}
		emitted[h] = struct{}{}
		out.Edges = append(out.Edges, cloneDeltaEdge(e))
	}
	if belowRecorded != nil && len(out.ReplacePaths) > 0 {
		out.RestatedNodes, out.RestatedEdges = dw.restatedRows(out.ReplacePaths, recordedAt, belowRecorded)
	}
	for _, p := range out.ReplacePaths {
		for _, n := range dw.work.GetFileNodes(p) {
			out.Nodes = append(out.Nodes, cloneDeltaNode(n))
			if key := deltaPathKey(n.ID); n.ID != p && !inFinalPath(final, key) {
				if _, gone := emptied[key]; !gone {
					out.IdentityClaims = append(out.IdentityClaims, n.ID)
				}
			}
		}
		for _, e := range recordedAt[p] {
			emit(e)
		}
	}

	inFinal := func(p string) bool {
		_, ok := final[p]
		return ok
	}
	// identityVisible is the composition's endpoint rule for the published
	// generation: an identity at a replaced or emptied path survives only
	// when the delta carries it.
	identityVisible := func(id string) bool {
		key := deltaPathKey(id)
		_, gone := emptied[key]
		if !inFinal(key) && !gone {
			return true
		}
		return dw.work.GetNode(id) != nil
	}

	// Detached identities: carried with a tombstone when they differ from
	// the layer below.
	tombstoned := make(map[string]struct{})
	for _, n := range dw.work.AllNodes() {
		if n == nil || n.ID == "" {
			continue
		}
		if n.FilePath != "" && inFinal(n.FilePath) {
			continue
		}
		if _, gone := emptied[n.FilePath]; gone && n.FilePath != "" {
			continue
		}
		key := deltaPathKey(n.ID)
		if inFinal(key) {
			// Carried under a replaced path's claim, as the sparse builder
			// does for a row whose id names a claimed file.
			out.Nodes = append(out.Nodes, cloneDeltaNode(n))
			continue
		}
		belowNode := dw.below.GetNode(n.ID)
		if deltaNodeRender(n) == deltaNodeRender(belowNode) {
			out.DroppedNodes++
			continue
		}
		if n.Kind == KindBuiltin && belowNode != nil {
			// A builtin sentinel is a deterministic shared row the stores
			// materialize lazily on first use; the working graph's lazily
			// materialized copy restates the one below (without the repository
			// stamps the whole index gives it) and is never published.
			out.DroppedNodes++
			continue
		}
		out.Nodes = append(out.Nodes, cloneDeltaNode(n))
		tombstoned[n.ID] = struct{}{}
	}
	for _, id := range removed {
		tombstoned[id] = struct{}{}
	}
	// A shared registry row a covered file stopped emitting, that no file
	// re-emitted, is gone from a whole index; no file mask reaches its
	// identity, so it is removed by name. So is a builtin sentinel whose
	// every referrer the delta removed.
	var orphanCandidates []string
	for _, n := range dw.lostForeignNodes() {
		if sharedCopyNode(n) {
			tombstoned[n.ID] = struct{}{}
			continue
		}
		orphanCandidates = append(orphanCandidates, n.ID)
	}
	// Any other row a covered file stopped emitting under an identity no
	// file mask reaches (a contract node such as `env::NAME` of a deleted
	// file) is removed by name when nothing left in the composition refers
	// to it; a row something still refers to is left to the file that does.
	for _, id := range dw.unreferencedIdentities(orphanCandidates) {
		tombstoned[id] = struct{}{}
	}
	for _, id := range dw.unreferencedBuiltins() {
		tombstoned[id] = struct{}{}
	}

	// Claimed sources: a marker exactly where the final outgoing set, outside
	// the replaced paths, differs from what the layer below shows there. A
	// source at a replaced path is marked too — its edges recorded in other
	// files are the ones a replaced path cannot speak for — and the published
	// generation layer honours such a marker (graphview.GenerationLayer.
	// OwnsOutEdges).
	belowOut := make(map[string][]*Edge, len(claimed))
	var unread []string
	for _, id := range claimed {
		if rows, ok := dw.belowOut[id]; ok {
			belowOut[id] = rows
		} else {
			unread = append(unread, id)
		}
	}
	if len(unread) > 0 {
		for id, rows := range dw.below.GetOutEdgesByNodeIDs(unread) {
			belowOut[id] = rows
		}
	}
	outside := func(id string) (finalOutside, belowOutside []*Edge) {
		for _, e := range dw.work.GetOutEdges(id) {
			if e != nil && !inFinal(e.FilePath) {
				finalOutside = append(finalOutside, e)
			}
		}
		if identityVisible(id) {
			for _, e := range belowOut[id] {
				if e == nil || inFinal(e.FilePath) {
					continue
				}
				if !identityVisible(e.From) || !identityVisible(e.To) {
					continue
				}
				belowOutside = append(belowOutside, e)
			}
		}
		return finalOutside, belowOutside
	}
	for _, id := range claimed {
		finalOut := dw.work.GetOutEdges(id)
		finalOutside, belowOutside := outside(id)
		_, carriedTombstone := tombstoned[id]
		if !carriedTombstone && deltaEdgeSetsEqual(finalOutside, belowOutside) {
			out.DroppedSources++
			continue
		}
		if !carriedTombstone {
			out.EdgeSources = append(out.EdgeSources, id)
		}
		// A marked or tombstoned source owns its whole outgoing set, so the
		// generation carries all of it.
		for _, e := range finalOut {
			if e != nil {
				emit(e)
			}
		}
	}
	for id := range tombstoned {
		out.Tombstones = append(out.Tombstones, id)
	}
	sort.Strings(out.Tombstones)
	sort.Strings(out.IdentityClaims)
	out.IdentityClaims = slices.Compact(out.IdentityClaims)
	sort.Strings(out.DeletePaths)
	sort.Strings(out.ReplacePaths)
	sort.Strings(out.EdgeSources)
	return out
}

func inFinalPath(final map[string]struct{}, p string) bool {
	_, ok := final[p]
	return ok
}

// restatedRows counts the rows at replaced paths identical to the rows the
// view below holds there.
func (dw *DeltaWriter) restatedRows(paths []string, recordedAt map[string][]*Edge, below RecordedEdgeReader) (nodes, edges int) {
	belowNodes := make(map[string]*Node)
	for _, p := range paths {
		for _, n := range dw.below.GetFileNodes(p) {
			if n != nil {
				belowNodes[n.ID] = n
			}
		}
	}
	for _, p := range paths {
		for _, n := range dw.work.GetFileNodes(p) {
			if b := belowNodes[n.ID]; b != nil && deltaNodeRender(b) == deltaNodeRender(n) {
				nodes++
			}
		}
	}
	belowEdges := make(map[edgeHash]*Edge)
	for _, e := range below.RecordedEdgesAt(paths) {
		if e != nil {
			belowEdges[hashEdgeKey(keyOf(e))] = e
		}
	}
	for _, p := range paths {
		for _, e := range recordedAt[p] {
			if b := belowEdges[hashEdgeKey(keyOf(e))]; b != nil && deltaEdgeContentEqual(b, e) {
				edges++
			}
		}
	}
	return nodes, edges
}
