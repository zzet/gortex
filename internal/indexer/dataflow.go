package indexer

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const dataflowRewriteBatchSize = 2048

// materializeDataflowParams runs after the regular call resolver
// pass to lift the placeholder targets carried by EdgeArgOf and
// EdgeReturnsTo edges to concrete graph IDs. The Go dataflow
// extractor (see internal/parser/languages/go_dataflow.go) emits
// these edges with an `unresolved::` text on the side that
// references the callee — exactly the shape the call resolver
// already knows how to lift. After Resolver.ResolveAll has run
// every placeholder side has been rewritten to a real function /
// method node ID; this pass then:
//
//  1. EdgeArgOf — joins the now-resolved To (a function/method
//     node) against its incoming EdgeParamOf edges to find the
//     param node at the recorded position (Meta["arg_position"]),
//     and rewrites the edge target to the param node ID. When no
//     matching param exists (variadic position past the declared
//     count, signature mismatch from extern callees, etc.) the
//     edge stays pointed at the function node — still a useful
//     dataflow hop.
//
//  2. EdgeReturnsTo — joins the placeholder From (currently the
//     enclosing caller's function ID) against the resolved
//     EdgeCalls edge from the same caller at the same line,
//     and rewrites From to the resolved callee. Falls back to
//     leaving the placeholder in place when no matching call
//     edge can be found (rare; usually means the call resolver
//     declined to lift the call edge too).
//
// Both rewrite paths use graph.RemoveEdge + graph.AddEdge so the
// shard buckets / inverted indexes stay consistent with the new
// (From, To, Kind, Line) tuple. Edges whose Meta no longer
// matches their state are stripped of the dataflow markers so a
// re-run of this pass becomes a no-op.
func (idx *Indexer) materializeDataflowParams() {
	materializeDataflowParamsInGraph(idx.graph)
}

// materializeDataflowParamsInGraph is materializeDataflowParams over any
// store: the whole index (IndexCtx, right after its ResolveAll) and the
// multi-repository master resolve run it, so a whole index writes the same
// arg_of / returns_to rows the per-save and delta paths write
// (materializeDataflowParamsForStages / ForFiles). The rewrite is idempotent.
func materializeDataflowParamsInGraph(g graph.Store) {
	if g == nil {
		return
	}
	forEachDataflowEdgeBatch(g, dataflowRewriteBatchSize, func(edges []*graph.Edge) bool {
		rewriteDataflowBatch(g, edges)
		return true
	})
}

func forEachDataflowEdgeBatch(g graph.Store, batchSize int, yield func([]*graph.Edge) bool) {
	graph.ScanEdgesByKindsBatched(
		g,
		[]graph.EdgeKind{graph.EdgeArgOf, graph.EdgeReturnsTo},
		batchSize,
		yield,
	)
}

// materializeDataflowParamsForFile is the single-file equivalent of
// materializeDataflowParams, used on the incremental (fsnotify /
// edit_file) re-index path so a one-line edit doesn't scan the whole
// edge set. fileEdges is the file's freshly-extracted edge slice
// (result.Edges from indexFile); only its From endpoints are read, so
// stale To/From values from before resolution don't matter.
//
// A file's arg_of / returns_to From is NOT always a node in the file,
// so node membership alone is insufficient. Two From classes exist:
//   - file nodes: returns_to's From is the caller function, and an
//     arg_of whose argument is a bare in-scope identifier has its From
//     rewritten by the resolver to that local/param — GetFileNodes
//     covers both.
//   - synthetic ids: arg_of for a selector (obj.Field), package-
//     qualified (pkg.V), global, or nested-call (f(g())) argument keeps
//     a synthetic `unresolved::` / `external::` From that never becomes
//     a file node. The resolver leaves these untouched, so the id the
//     extractor emitted (still present in fileEdges) is the id in the
//     graph.
//
// Probing the union of both, then keeping only edges whose FilePath is
// this file, yields exactly the arg_of+returns_to set the whole-graph
// pass would touch for it — faithful, not approximate. Each rewrite
// needs only the edge plus a targeted callee lookup (paramNodeAtPosition
// / findCallTarget). The batch path (Resolver.ResolveAll) still runs the
// whole-graph variant once, where amortising one scan over many files
// is the right trade.
func (idx *Indexer) materializeDataflowParamsForFile(graphPath string, fileEdges []*graph.Edge) {
	g := idx.graph
	fromSet := make(map[string]struct{})
	for _, n := range g.GetFileNodes(graphPath) {
		if n != nil && n.ID != "" {
			fromSet[n.ID] = struct{}{}
		}
	}
	for _, e := range fileEdges {
		if e != nil && (e.Kind == graph.EdgeArgOf || e.Kind == graph.EdgeReturnsTo) && e.From != "" {
			fromSet[e.From] = struct{}{}
		}
	}
	if len(fromSet) == 0 {
		return
	}
	froms := make([]string, 0, len(fromSet))
	for id := range fromSet {
		froms = append(froms, id)
	}
	// A synthetic From can be shared across files, so restrict the rewrite to
	// edges this file actually emitted. The union adjacency read is one batched
	// query; rewriteDataflowBatch then performs the same bounded joins and one
	// identity-write batch as the cold path.
	var dataflowEdges []*graph.Edge
	for _, edges := range g.GetOutEdgesByNodeIDs(froms) {
		for _, e := range edges {
			if e == nil || e.FilePath != graphPath {
				continue
			}
			switch e.Kind {
			case graph.EdgeArgOf, graph.EdgeReturnsTo:
				dataflowEdges = append(dataflowEdges, e)
			}
		}
	}
	rewriteDataflowBatch(g, dataflowEdges)
}

// argOfRewriteTarget reports whether an arg_of edge is a rewrite
// candidate and, if so, the resolved callee id and the argument
// position. An edge already pointing at a param node, or still
// pointing at an unresolved / external stub, is not a candidate. Shared
// by the per-edge (rewriteArgOf) and indexed (rewriteArgOfIndexed) paths
// so the guard lives in one place.
func argOfRewriteTarget(e *graph.Edge) (calleeID string, pos int, ok bool) {
	if e == nil || e.Meta == nil {
		return "", 0, false
	}
	pos, ok = argPositionFromMeta(e.Meta)
	if !ok {
		return "", 0, false
	}
	to := e.To
	if strings.Contains(to, "#param:") {
		return "", 0, false
	}
	if strings.HasPrefix(to, "unresolved::") || strings.HasPrefix(to, "external::") {
		return "", 0, false
	}
	return to, pos, true
}

type dataflowParamEdgeBatchStore interface {
	GetDataflowParamEdgesByOwnerIDs(ownerIDs []string) map[string][]*graph.Edge
}

type dataflowCallEdgeBatchStore interface {
	GetDataflowCallEdgesByCallerIDs(callerIDs []string) map[string][]*graph.Edge
}

type pendingReturnsTo struct {
	edge       *graph.Edge
	callerID   string
	callLine   int
	calleeText string
}

// rewriteDataflowBatch performs every lookup and mutation at batch
// granularity. At most one parameter-adjacency query, one parameter-node
// query, one call-adjacency query, and one ReindexEdges call are made for a
// batch, regardless of how many arg_of / returns_to edges it contains.
func rewriteDataflowBatch(g graph.Store, edges []*graph.Edge) int {
	return rewriteDataflowBatchLegs(g, edges, nil, nil)
}

// rewriteDataflowBatchLegs is rewriteDataflowBatch timing its legs (the
// callee parameter index, the caller call index, the reindex) on legs, and
// reading the callee parameter index through params when set.
func rewriteDataflowBatchLegs(g graph.Store, edges []*graph.Edge, legs *refFactLegs, params *editDeltaParamIndex) int {
	if len(edges) == 0 {
		return 0
	}
	var argEdges []*graph.Edge
	var returns []pendingReturnsTo
	callees := make(map[string]struct{})
	callers := make(map[string]struct{})
	for _, edge := range edges {
		switch {
		case edge == nil:
			continue
		case edge.Kind == graph.EdgeArgOf:
			if calleeID, _, ok := argOfRewriteTarget(edge); ok {
				argEdges = append(argEdges, edge)
				callees[calleeID] = struct{}{}
			}
		case edge.Kind == graph.EdgeReturnsTo:
			callerID, callLine, calleeText, ok := returnsToRewriteTarget(edge)
			if ok {
				returns = append(returns, pendingReturnsTo{
					edge: edge, callerID: callerID,
					callLine: callLine, calleeText: calleeText,
				})
				callers[callerID] = struct{}{}
			}
		}
	}

	legs.lap("classify")
	var paramIdx map[string]map[int]string
	if params != nil {
		paramIdx = params.index(g, callees)
	} else {
		paramIdx = buildParamPositionIndex(g, callees)
	}
	legs.lap("param_index")
	callIdx := buildCallTargetIndex(g, callers)
	legs.lap("call_index")
	reindexes := make([]graph.EdgeReindex, 0, len(argEdges)+len(returns))
	// A bounded input batch can contain duplicate pointers when a synthetic
	// source is shared. Stage each stored identity once so ordered delete/insert
	// semantics stay deterministic.
	seen := make(map[*graph.Edge]struct{}, len(edges))
	for _, edge := range argEdges {
		if _, duplicate := seen[edge]; duplicate {
			continue
		}
		seen[edge] = struct{}{}
		calleeID, pos, _ := argOfRewriteTarget(edge)
		paramID := paramIdx[calleeID][pos]
		if paramID == "" || paramID == edge.To {
			continue
		}
		oldFrom, oldTo := edge.From, edge.To
		oldFilePath, oldLine := edge.FilePath, edge.Line
		edge.To = paramID
		reindexes = append(reindexes, graph.EdgeReindex{
			Edge: edge, OldFrom: oldFrom, OldTo: oldTo,
			RefreshIdentity: true, OldFilePath: oldFilePath, OldLine: oldLine,
		})
	}
	for _, pending := range returns {
		edge := pending.edge
		if _, duplicate := seen[edge]; duplicate {
			continue
		}
		seen[edge] = struct{}{}
		resolvedCallee := callIdx.resolve(pending.callerID, pending.callLine, pending.calleeText)
		if resolvedCallee == "" || resolvedCallee == edge.From {
			continue
		}
		oldFrom, oldTo := edge.From, edge.To
		oldFilePath, oldLine := edge.FilePath, edge.Line
		edge.From = resolvedCallee
		reindexes = append(reindexes, graph.EdgeReindex{
			Edge: edge, OldFrom: oldFrom, OldTo: oldTo,
			RefreshIdentity: true, OldFilePath: oldFilePath, OldLine: oldLine,
		})
	}
	legs.lap("rewrite")
	if len(reindexes) > 0 {
		g.ReindexEdges(reindexes)
	}
	legs.lap("reindex")
	return len(reindexes)
}

// buildParamPositionIndex maps each callee id to its argument
// position → param-node-id table, built from two batched queries
// (in-edges of all callees, then the param nodes those edges point
// from). It replaces a per-arg_of-edge paramNodeAtPosition, which
// re-fetched a popular callee's whole in-edge list once per argument —
// the dominant cost of the per-file dataflow pass on a large file. The
// position is read from the param node's Meta exactly as
// paramNodeAtPosition does, with the first param at a position winning.
func buildParamPositionIndex(g graph.Store, callees map[string]struct{}) map[string]map[int]string {
	if len(callees) == 0 {
		return nil
	}
	ids := make([]string, 0, len(callees))
	for id := range callees {
		ids = append(ids, id)
	}
	var inEdges map[string][]*graph.Edge
	if reader, ok := g.(dataflowParamEdgeBatchStore); ok {
		inEdges = reader.GetDataflowParamEdgesByOwnerIDs(ids)
	} else {
		inEdges = g.GetInEdgesByNodeIDs(ids)
	}
	type ownerParam struct{ owner, param string }
	var pairs []ownerParam
	paramSet := make(map[string]struct{})
	for owner, edges := range inEdges {
		for _, e := range edges {
			if e != nil && e.Kind == graph.EdgeParamOf && e.From != "" {
				pairs = append(pairs, ownerParam{owner: owner, param: e.From})
				paramSet[e.From] = struct{}{}
			}
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	paramIDs := make([]string, 0, len(paramSet))
	for id := range paramSet {
		paramIDs = append(paramIDs, id)
	}
	nodes := g.GetNodesByIDs(paramIDs)
	idx := make(map[string]map[int]string, len(inEdges))
	for _, pr := range pairs {
		n := nodes[pr.param]
		if n == nil || n.Kind != graph.KindParam {
			continue
		}
		pos, ok := intFromMeta(n.Meta, "position")
		if !ok {
			continue
		}
		m := idx[pr.owner]
		if m == nil {
			m = make(map[int]string)
			idx[pr.owner] = m
		}
		if _, exists := m[pos]; !exists {
			m[pos] = n.ID
		}
	}
	return idx
}

func returnsToRewriteTarget(e *graph.Edge) (callerID string, callLine int, calleeText string, ok bool) {
	if e == nil || e.Meta == nil {
		return "", 0, "", false
	}
	if _, ok := e.Meta["returns_to_call"]; !ok {
		return "", 0, "", false
	}
	callLine, _ = intFromMeta(e.Meta, "call_line")
	if callLine == 0 {
		callLine = e.Line
	}
	calleeText, _ = e.Meta["callee_target"].(string)
	return e.From, callLine, calleeText, e.From != ""
}

type dataflowCallTargets struct {
	fallback string
	byName   map[string]string
}

func (targets *dataflowCallTargets) add(to string) {
	if targets.fallback == "" {
		targets.fallback = to
	}
	name := resolvedCallTargetName(to)
	if name == "" {
		return
	}
	if targets.byName == nil {
		targets.byName = make(map[string]string)
	}
	if _, exists := targets.byName[name]; !exists {
		targets.byName[name] = to
	}
}

func (targets *dataflowCallTargets) resolve(calleeText string) string {
	if targets == nil {
		return ""
	}
	if name := recordedCallTargetName(calleeText); name != "" {
		// The recorded callee names the call the value comes from. Only a
		// resolved call of that name may take the rewrite: falling back to
		// whatever else resolved on the line (a type conversion in
		// `strconv.FormatInt(int64(n), 10)`) would attribute the value to
		// the wrong callee, and which one it picked depended on the order
		// the calls were listed in.
		return targets.byName[name]
	}
	return targets.fallback
}

type dataflowCallTargetIndex struct {
	allByCaller  map[string]*dataflowCallTargets
	lineByCaller map[string]map[int]*dataflowCallTargets
}

func buildCallTargetIndex(g graph.Store, callers map[string]struct{}) dataflowCallTargetIndex {
	idx := dataflowCallTargetIndex{
		allByCaller:  make(map[string]*dataflowCallTargets, len(callers)),
		lineByCaller: make(map[string]map[int]*dataflowCallTargets, len(callers)),
	}
	if len(callers) == 0 {
		return idx
	}
	ids := make([]string, 0, len(callers))
	for id := range callers {
		ids = append(ids, id)
	}
	var outgoing map[string][]*graph.Edge
	if reader, ok := g.(dataflowCallEdgeBatchStore); ok {
		outgoing = reader.GetDataflowCallEdgesByCallerIDs(ids)
	} else {
		outgoing = g.GetOutEdgesByNodeIDs(ids)
	}
	for callerID, edges := range outgoing {
		// The first target recorded per caller and per line is the fallback
		// a returns_to rewrite takes, so the candidates are visited in a
		// total order (line, target), never in the store's listing order.
		edges = append([]*graph.Edge(nil), edges...)
		sort.SliceStable(edges, func(i, j int) bool {
			a, b := edges[i], edges[j]
			if a == nil || b == nil {
				return b != nil
			}
			if a.Line != b.Line {
				return a.Line < b.Line
			}
			return a.To < b.To
		})
		for _, edge := range edges {
			if edge == nil || edge.Kind != graph.EdgeCalls || strings.HasPrefix(edge.To, "unresolved::") {
				continue
			}
			all := idx.allByCaller[callerID]
			if all == nil {
				all = &dataflowCallTargets{}
				idx.allByCaller[callerID] = all
			}
			all.add(edge.To)
			byLine := idx.lineByCaller[callerID]
			if byLine == nil {
				byLine = make(map[int]*dataflowCallTargets)
				idx.lineByCaller[callerID] = byLine
			}
			lineTargets := byLine[edge.Line]
			if lineTargets == nil {
				lineTargets = &dataflowCallTargets{}
				byLine[edge.Line] = lineTargets
			}
			lineTargets.add(edge.To)
		}
	}
	return idx
}

func (idx dataflowCallTargetIndex) resolve(callerID string, line int, calleeText string) string {
	if line == 0 {
		return idx.allByCaller[callerID].resolve(calleeText)
	}
	return idx.lineByCaller[callerID][line].resolve(calleeText)
}

func recordedCallTargetName(calleeText string) string {
	bare := strings.TrimPrefix(calleeText, "unresolved::")
	bare = strings.TrimPrefix(bare, "extern::")
	return strings.TrimPrefix(bare, "*.")
}

func resolvedCallTargetName(to string) string {
	if i := strings.LastIndex(to, "::"); i >= 0 {
		to = to[i+2:]
	}
	if i := strings.LastIndex(to, "."); i >= 0 {
		to = to[i+1:]
	}
	return to
}

// argPositionFromMeta extracts the recorded argument position. The
// metadata roundtrip can yield int or float64 depending on origin
// (extractor vs JSON deserialisation), so accept both.
func argPositionFromMeta(m map[string]any) (int, bool) {
	return intFromMeta(m, "arg_position")
}

func intFromMeta(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}
