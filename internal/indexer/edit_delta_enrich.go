package indexer

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The delta's ownership claims are written before the enrichment stage runs,
// and the stage writes through the same generation handle. An edge it adds
// recorded at a path the generation does not replace, out of a source the
// generation neither marks nor tombstones, is composed alongside the view
// below rather than instead of it — so a copy of an edge the view below
// already serves would be served twice. editDeltaSettleEnrichment removes
// such restatements after the stage: the row below is the one a whole index
// holds (the enrichment of an unchanged file is the one already below).
// Edges below that differ from the stage's copy only in payload are left to
// the row below as well; the count is reported. New edges stay: the
// composition adds them to the source's served set, as the stage meant.

// editDeltaOwnership is what a published delta speaks for: the paths its file
// masks replace or delete and the sources it marks or tombstones.
type editDeltaOwnership struct {
	paths   map[string]struct{}
	sources map[string]struct{}
}

func (o editDeltaOwnership) owns(e *graph.Edge) bool {
	if e == nil {
		return true
	}
	if _, ok := o.paths[e.FilePath]; ok {
		return true
	}
	_, ok := o.sources[e.From]
	return ok
}

// editDeltaSettleEnrichment removes, from the generation, every edge the
// enrichment stage wrote outside the delta's ownership whose identity the view
// below already serves. It returns how many it removed.
func editDeltaSettleEnrichment(handle *store_sqlite.Store, below graph.Reader, own editDeltaOwnership) int {
	var stray []*graph.Edge
	for _, e := range handle.AllEdgesLight() {
		if !own.owns(e) {
			stray = append(stray, e)
		}
	}
	if len(stray) == 0 {
		return 0
	}
	sources := make([]string, 0, len(stray))
	seen := make(map[string]struct{}, len(stray))
	for _, e := range stray {
		if _, dup := seen[e.From]; !dup {
			seen[e.From] = struct{}{}
			sources = append(sources, e.From)
		}
	}
	sort.Strings(sources)
	type identity struct {
		from, to, kind, file string
		line                 int
	}
	served := make(map[identity]struct{})
	for _, out := range below.GetOutEdgesByNodeIDs(sources) {
		for _, e := range out {
			if e != nil {
				served[identity{e.From, e.To, string(e.Kind), e.FilePath, e.Line}] = struct{}{}
			}
		}
	}
	var restated []*graph.Edge
	for _, e := range stray {
		if _, ok := served[identity{e.From, e.To, string(e.Kind), e.FilePath, e.Line}]; ok {
			restated = append(restated, e)
		}
	}
	if len(restated) == 0 {
		return 0
	}
	return handle.RemoveEdgesExact(restated)
}

// editDeltaClaimEnrichedNodes gives an identity replacement claim to every
// node the enrichment stage wrote outside the delta's ownership whose identity
// the view below already serves. The stage reads the generation handle, so it
// re-emits a row a lower generation of the chain carries (the go/types pass's
// external symbols and module nodes, at `external::go:<path>`); no file mask
// reaches such a row, and without a claim the composition would serve both
// copies. Under the claim it serves this generation's copy only, the row the
// primary per-save path upserts over the one it held. It returns how many it
// claimed.
func editDeltaClaimEnrichedNodes(handle *store_sqlite.Store, below graph.Reader, own editDeltaOwnership) (int, error) {
	var ids []string
	for _, n := range handle.AllNodesLight() {
		if n == nil || n.ID == "" {
			continue
		}
		if _, ok := own.paths[n.FilePath]; ok {
			continue
		}
		if _, ok := own.sources[n.ID]; ok {
			continue
		}
		ids = append(ids, n.ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	masks, err := handle.NodeIdentityMasksContext(context.Background())
	if err != nil {
		return 0, err
	}
	claimed := make(map[string]struct{}, len(masks))
	for _, m := range masks {
		claimed[m.NodeID] = struct{}{}
	}
	open := ids[:0]
	for _, id := range ids {
		if _, ok := claimed[id]; !ok {
			open = append(open, id)
		}
	}
	if len(open) == 0 {
		return 0, nil
	}
	served := below.GetNodesByIDs(open)
	var claims []string
	for _, id := range open {
		if served[id] != nil {
			claims = append(claims, id)
		}
	}
	if len(claims) == 0 {
		return 0, nil
	}
	sort.Strings(claims)
	if err := handle.SetNodeIdentityReplacements(claims); err != nil {
		return 0, err
	}
	return len(claims), nil
}
