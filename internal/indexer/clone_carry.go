package indexer

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// Clone rows of the functions a per-file delta re-derives unchanged.
//
// A per-file delta runs the engine on a fresh Indexer whose clone index is
// not built (building it reads every function of the repository), so the
// incremental clone step only marks itself pending: a re-derived function
// comes back without clone_sig, and the eviction before the batch deletes
// every similar_to row incident to it.
//
// A function whose body is unchanged keeps what the layer below says: its
// clone_sig, and every similar_to row incident to it whose partner is
// unchanged too (not re-derived by the batch, or re-derived with an unchanged
// body). "Unchanged" is the file for a file the batch re-derives only because
// the change can move its rows (forcedReparse: an unchanged dependent, a
// deleted file's importer), and the function's body identity
// (cloneBodyMetaKey) for a file whose bytes changed.
//
// A changed body keeps nothing: its signature depends on the repository-wide
// shingle counts and its pairs on the whole clone index, which a per-file
// delta does not read. Its prior rows are removed by the eviction and not
// re-stated, and its partners' rows into it are published away (the claim
// rule does not keep similar_to), so the view never claims a clone relation
// the delta has not recomputed; the post-publication follow-up recomputes it.

// carryCloneRowsOfUnchangedBodies stamps the prior clone_sig on the fresh
// function nodes whose body is unchanged and returns the prior similar_to rows
// incident to them whose partner is unchanged as well. It runs before the
// eviction's AddBatch; fresh is that batch's node set.
func (idx *Indexer) carryCloneRowsOfUnchangedBodies(stages []*incrementalBatchStage, view incrementalPriorView, fresh []*graph.Node) []*graph.Edge {
	if idx.cloneIndex != nil && idx.cloneIndex.Ready() {
		return nil
	}
	freshByID := make(map[string]*graph.Node, len(fresh))
	for _, n := range fresh {
		if n != nil && n.ID != "" {
			freshByID[n.ID] = n
		}
	}
	evicted := structuralPriorIDs(stages)
	unchangedFile := make(map[string]bool)
	for _, stage := range stages {
		if stage == nil {
			continue
		}
		if _, recompute := idx.cloneRecompute[stage.graphPath]; recompute {
			continue
		}
		if idx.forceReparseDropsResolutions(stage.absPath) {
			for _, prior := range stage.priorNodes {
				if prior != nil {
					unchangedFile[prior.ID] = true
				}
			}
		}
	}
	// unchanged reports whether an identity's body is what the layer below
	// holds: untouched by the batch, or re-derived with the same body.
	unchanged := func(id string) bool {
		now := freshByID[id]
		if now == nil {
			_, gone := evicted[id]
			return !gone
		}
		if unchangedFile[id] {
			return true
		}
		if _, recompute := idx.cloneRecompute[now.FilePath]; recompute {
			return false
		}
		prior := view.nodesByID[id]
		if prior == nil {
			return false
		}
		was, _ := prior.Meta[cloneBodyMetaKey].(string)
		is, _ := now.Meta[cloneBodyMetaKey].(string)
		return was != "" && was == is
	}
	// Every re-derived body's shingles are this generation's side-table rows,
	// as a whole index and the primary per-save path write them: the next
	// clone finalize over the generation reads them from here.
	shingles := make(map[string][]uint64)
	for _, n := range fresh {
		if n == nil || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
			continue
		}
		if sh, ok := n.Meta[cloneShinglesMetaKey].([]uint64); ok && len(sh) > 0 {
			shingles[n.ID] = sh
		}
	}
	if w, ok := idx.graph.(graph.CloneShingleWriter); ok && len(shingles) > 0 {
		_ = w.BulkSetCloneShingles(idx.repoPrefix, shingles)
	}
	var carried []*graph.Edge
	seen := make(map[*graph.Edge]struct{})
	carry := func(e *graph.Edge) {
		if e == nil || e.Kind != graph.EdgeSimilarTo || !unchanged(e.From) || !unchanged(e.To) {
			return
		}
		if _, dup := seen[e]; dup {
			return
		}
		seen[e] = struct{}{}
		// A similar_to row sits at its source's declaration line, which
		// moves with the source's file even when its body does not.
		if from := freshByID[e.From]; from != nil && from.StartLine != e.Line {
			moved := *e
			moved.Line = from.StartLine
			e = &moved
		}
		carried = append(carried, e)
	}
	for _, stage := range stages {
		if stage == nil {
			continue
		}
		// A function the batch re-derived with a changed or new body leaves
		// its file's clone rows owed to the follow-up.
		if stage.result != nil {
			for _, n := range stage.result.Nodes {
				if n != nil && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) && !unchanged(n.ID) {
					idx.noteChangedBody(stage.graphPath)
					break
				}
			}
		}
		for _, prior := range stage.priorNodes {
			if prior == nil || (prior.Kind != graph.KindFunction && prior.Kind != graph.KindMethod) {
				continue
			}
			now := freshByID[prior.ID]
			if now == nil || !unchanged(prior.ID) {
				continue
			}
			// A function's covered_by rows come from an imported coverage
			// profile, recorded at the function's own file; no pass of a save
			// re-derives them. They hold while the body is unchanged, and go
			// with a changed body until the next import measures it again.
			for _, e := range view.outByNode[prior.ID] {
				if e != nil && e.Kind == graph.EdgeCoveredBy && unchanged(e.To) {
					if _, dup := seen[e]; !dup {
						seen[e] = struct{}{}
						carried = append(carried, e)
					}
				}
			}
			if sig, ok := prior.Meta[cloneSigMetaKey]; ok {
				if now.Meta == nil {
					now.Meta = map[string]any{}
				}
				now.Meta[cloneSigMetaKey] = sig
			}
			delete(now.Meta, cloneShinglesMetaKey)
			for _, e := range view.outByNode[prior.ID] {
				carry(e)
			}
			for _, e := range view.inByNode[prior.ID] {
				carry(e)
			}
		}
	}
	return carried
}

// noteChangedBody records a graph path holding a changed body.
func (idx *Indexer) noteChangedBody(graphPath string) {
	if graphPath == "" {
		return
	}
	if idx.cloneChangedBodies == nil {
		idx.cloneChangedBodies = make(map[string]struct{})
	}
	idx.cloneChangedBodies[graphPath] = struct{}{}
}

// cloneChangedBodyFiles lists, sorted, the graph paths holding a changed body.
func (idx *Indexer) cloneChangedBodyFiles() []string {
	if len(idx.cloneChangedBodies) == 0 {
		return nil
	}
	out := make([]string, 0, len(idx.cloneChangedBodies))
	for p := range idx.cloneChangedBodies {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// cloneRecomputePaths maps repository-relative paths to graph paths.
func cloneRecomputePaths(repoPrefix string, rels []string) map[string]struct{} {
	if len(rels) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(rels))
	for _, rel := range rels {
		out[builderGraphPath(repoPrefix, rel)] = struct{}{}
	}
	return out
}
