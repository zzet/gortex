package indexer

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// Pathless identities the layer below already serves.
//
// A pass re-derives the edited file's references to external symbols, and the
// resolver mints (or re-mints) the pathless identities they point at: a
// `stdlib::…` or `dep::…` stub, its module, their membership edge. The layer
// below holds each of them already, byte for byte. Carrying such a copy in the
// generation is not harmless: a generation that carries a node at no path
// tombstones it, and a tombstone owns the node's whole outgoing adjacency, so
// every base edge out of the stub recorded in a file the generation never
// touched — every other file's value flowing out of fmt.Errorf, say — is
// hidden by a body edit that merely calls fmt.Errorf. On this repository a
// one-line edit in internal/config/config.go hid 3,332 such rows.
//
// pruneRedundantPathless drops those copies: a pathless node whose layer-below
// copy is identical, together with its out-edges recorded at no file when the
// layer below holds each of them identically. Its other edges stay — they are
// recorded at paths the generation claims and belong to the edited file — and
// the composition settles them edge by edge (withoutSettledContextSources then
// drops the node's edge-source marker, since the generation no longer carries
// the node). A pathless node that is new, or differs from its layer-below
// copy, or has an out-edge at no file the layer below lacks, stays as before.
func pruneRedundantPathless(corpus contextCorpus, base LayerBase) (nodesRemoved, edgesRemoved int, pruned []string, err error) {
	if base == nil {
		return 0, 0, nil, fmt.Errorf("indexer: pruning redundant pathless identities needs the layer below")
	}
	pathless := make(map[string]*graph.Node)
	for _, node := range corpus.AllNodes() {
		// Builtin stubs stay: the generation claims them to serve the
		// repository-stamped row (their boundary columns are stamped at the
		// write, and a builtin is a target-only sentinel with no adjacency
		// of its own to hide).
		if node != nil && node.ID != "" && node.FilePath == "" && node.Kind != graph.KindBuiltin {
			pathless[node.ID] = node
		}
	}
	if len(pathless) == 0 {
		return 0, 0, nil, nil
	}
	ids := make([]string, 0, len(pathless))
	for id := range pathless {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	below := base.GetNodesByIDs(ids)
	if len(below) == 0 {
		// Nothing below carries any of them (a dedicated base builds over an
		// empty layer): no copy can be redundant, and there is no adjacency
		// below to compare against.
		return 0, 0, nil, nil
	}

	var corpusEdges []*graph.Edge
	unpathedOut := make(map[string][]*graph.Edge)
	for _, edge := range corpus.AllEdges() {
		if edge == nil {
			continue
		}
		corpusEdges = append(corpusEdges, edge)
		if _, isPathless := pathless[edge.From]; isPathless && edge.FilePath == "" {
			unpathedOut[edge.From] = append(unpathedOut[edge.From], edge)
		}
	}
	var belowUnpathed map[string][]*graph.Edge
	if len(unpathedOut) > 0 {
		reader, ok := graph.RecordedEdgesOf(base)
		if !ok {
			// Without it no stub copy can be proven redundant and every one
			// stays carried, hiding the layer below's adjacency of the stub
			// in files the generation never touched. Refuse rather than
			// bring that back silently.
			return 0, 0, nil, fmt.Errorf("indexer: the layer below (%T) serves no edges by recording file; redundant pathless identities cannot be pruned", base)
		}
		belowUnpathed = make(map[string][]*graph.Edge)
		for _, edge := range reader.RecordedEdgesAt([]string{""}) {
			if edge == nil {
				continue
			}
			if _, isPathless := pathless[edge.From]; isPathless {
				belowUnpathed[edge.From] = append(belowUnpathed[edge.From], edge)
			}
		}
	}
	redundant := make(map[string]struct{})
	for _, id := range ids {
		if !pathlessNodeEquivalent(pathless[id], below[id]) {
			continue
		}
		if !edgesCoveredBy(unpathedOut[id], belowUnpathed[id]) {
			continue
		}
		redundant[id] = struct{}{}
		pruned = append(pruned, id)
	}
	if len(pruned) == 0 {
		return 0, 0, nil, nil
	}
	// Every edge touching a pruned node leaves with it; the ones recorded at
	// a path (the edited file's own) are put back.
	var restore []*graph.Edge
	for _, edge := range corpusEdges {
		_, fromPruned := redundant[edge.From]
		_, toPruned := redundant[edge.To]
		if !fromPruned && !toPruned {
			continue
		}
		if edge.FilePath == "" && fromPruned {
			continue // served identically by the layer below
		}
		restore = append(restore, edge)
	}
	evicter, ok := corpus.(graph.PathlessNodeBatchEvicter)
	if !ok {
		return 0, 0, nil, fmt.Errorf("indexer: the corpus cannot evict %d redundant pathless identities", len(pruned))
	}
	nodesRemoved, edgesRemoved = evicter.EvictPathlessNodesByIDs(pruned)
	if len(restore) > 0 {
		corpus.AddBatch(nil, restore)
		edgesRemoved -= len(restore)
	}
	return nodesRemoved, edgesRemoved, pruned, nil
}

// pathlessNodeEquivalent compares the fields a node row carries, Meta through a
// JSON round trip so a value decoded from the store (float64, []any) compares
// equal to the in-memory original (int, []string).
func pathlessNodeEquivalent(a, b *graph.Node) bool {
	if a == nil || b == nil {
		return false
	}
	if a.ID != b.ID || a.Kind != b.Kind || a.Name != b.Name || a.QualName != b.QualName ||
		a.FilePath != b.FilePath || a.StartLine != b.StartLine || a.EndLine != b.EndLine ||
		a.StartColumn != b.StartColumn || a.EndColumn != b.EndColumn || a.Language != b.Language ||
		a.RepoPrefix != b.RepoPrefix || a.WorkspaceID != b.WorkspaceID || a.ProjectID != b.ProjectID ||
		a.Origin != b.Origin || a.Stub != b.Stub {
		return false
	}
	return metaEquivalent(a.Meta, b.Meta)
}

// edgesCoveredBy reports whether every edge of want has an identical row in
// have.
func edgesCoveredBy(want, have []*graph.Edge) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if edgeRowEquivalent(w, h) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func edgeRowEquivalent(a, b *graph.Edge) bool {
	if a == nil || b == nil {
		return false
	}
	return a.From == b.From && a.To == b.To && a.Kind == b.Kind && a.FilePath == b.FilePath &&
		a.Line == b.Line && a.Confidence == b.Confidence && a.ConfidenceLabel == b.ConfidenceLabel &&
		a.Origin == b.Origin && a.Tier == b.Tier && a.CrossRepo == b.CrossRepo &&
		metaEquivalent(a.Meta, b.Meta)
}

func metaEquivalent(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	na, errA := normalizedMeta(a)
	nb, errB := normalizedMeta(b)
	if errA != nil || errB != nil {
		return false
	}
	return reflect.DeepEqual(na, nb)
}

func normalizedMeta(m map[string]any) (map[string]any, error) {
	if len(m) == 0 {
		return map[string]any{}, nil
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}
