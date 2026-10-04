package indexer

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A follow-up is the one place a working-tree delta may pay the full clone
// corpus read. Its parent is a composition of immutable generations, while
// each clone sidecar belongs to one generation. The selected row for a visible
// body must come from the generation that owns that node, never from a stale
// ancestor with the same ID.
type cloneFollowupProjection struct {
	repo string
	rows []cloneFollowupSignature
}

type cloneFollowupSignature struct {
	row      graph.CloneCorpusRow
	filePath string
}

type cloneFollowupEdgeKey struct {
	from, to, file string
	kind           graph.EdgeKind
	line           int
}

func cloneFollowupKey(e *graph.Edge) cloneFollowupEdgeKey {
	return cloneFollowupEdgeKey{e.From, e.To, e.FilePath, e.Kind, e.Line}
}

// The direct and diffused clone relations are both emitted by clones.go with
// this provenance and numeric similarity metadata. Other edge producers must
// keep their rows even if they use the semantically_related kind in future.
func cloneFollowupDerivedEdge(e *graph.Edge) bool {
	if e == nil || (e.Kind != graph.EdgeSimilarTo && e.Kind != graph.EdgeSemanticallyRelated) ||
		e.Origin != graph.OriginASTInferred || e.Meta == nil {
		return false
	}
	_, ok := e.Meta["similarity"]
	return ok
}

func cloneFollowupEdges(r graph.Reader, visible map[string]*graph.Node) map[cloneFollowupEdgeKey]*graph.Edge {
	out := make(map[cloneFollowupEdgeKey]*graph.Edge)
	for _, kind := range []graph.EdgeKind{graph.EdgeSimilarTo, graph.EdgeSemanticallyRelated} {
		for e := range r.EdgesByKind(kind) {
			if cloneFollowupDerivedEdge(e) && visible[e.From] != nil {
				out[cloneFollowupKey(e)] = e
			}
		}
	}
	return out
}

func cloneFollowupEdgeDiff(dw *graph.DeltaWriter, scratch *graph.Graph, visible map[string]*graph.Node) {
	prior := cloneFollowupEdges(dw.View(), visible)
	fresh := cloneFollowupEdges(scratch, visible)
	var remove, add []*graph.Edge
	for key, before := range prior {
		if after := fresh[key]; after == nil || !reflect.DeepEqual(before, after) {
			remove = append(remove, before)
		}
	}
	for key, after := range fresh {
		if before := prior[key]; before == nil || !reflect.DeepEqual(before, after) {
			add = append(add, after)
		}
	}
	less := func(a, b *graph.Edge) bool {
		ka, kb := cloneFollowupKey(a), cloneFollowupKey(b)
		if ka.from != kb.from {
			return ka.from < kb.from
		}
		if ka.to != kb.to {
			return ka.to < kb.to
		}
		if ka.kind != kb.kind {
			return ka.kind < kb.kind
		}
		if ka.file != kb.file {
			return ka.file < kb.file
		}
		return ka.line < kb.line
	}
	sort.Slice(remove, func(i, j int) bool { return less(remove[i], remove[j]) })
	sort.Slice(add, func(i, j int) bool { return less(add[i], add[j]) })
	if len(remove) > 0 {
		dw.RemoveEdgesExact(remove)
	}
	if len(add) > 0 {
		dw.AddBatch(nil, add)
	}
}

// prepareCloneFollowup recomputes the exact repo-wide CMS, LSH and diffusion
// result on a private minimal graph. Only clone-derived differences are
// applied to the DeltaWriter; the signature projection waits until after
// semantic enrichment and is small enough to retain across that stage.
func prepareCloneFollowup(
	ctx context.Context, below LayerBase, dw *graph.DeltaWriter, current *store_sqlite.Store,
	repo string, threshold float64,
) (*cloneFollowupProjection, error) {
	if repo == "" {
		return nil, fmt.Errorf("clone follow-up requires a non-empty repository prefix")
	}
	base, ok := below.(commitLayerBase)
	if !ok {
		return nil, fmt.Errorf("clone follow-up requires a pinned composed ancestry, got %T", below)
	}
	type source struct {
		handle *store_sqlite.Store
		layer  graph.OverlayLayerReader
		rows   map[string]graph.CloneCorpusRow
	}
	// Base corpus participates only in corpus-backed views. Dedicated full
	// roots deliberately omit it, even if generation zero has matching IDs.
	sources := make([]source, 0, len(base.cloneSources)+2)
	baseIndex := -1
	if base.cloneCorpusBase != nil {
		baseIndex = len(sources)
		sources = append(sources, source{handle: base.cloneCorpusBase, rows: map[string]graph.CloneCorpusRow{}})
	}
	for _, generation := range base.cloneSources {
		if generation.Handle == nil || generation.Layer == nil {
			return nil, fmt.Errorf("clone follow-up has an unpinned generation source")
		}
		sources = append(sources, source{handle: generation.Handle, layer: generation.Layer, rows: map[string]graph.CloneCorpusRow{}})
	}
	currentIndex := len(sources)
	sources = append(sources, source{handle: current, rows: map[string]graph.CloneCorpusRow{}})
	visible := make(map[string]*graph.Node)
	wanted := make(map[string]*graph.Node)
	bodyless := make(map[string]int)
	owners := make(map[string]int)
	for _, n := range dw.GetRepoNodes(repo) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if n == nil || n.ID == "" || n.RepoPrefix != repo ||
			(n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
			continue
		}
		visible[n.ID] = n
		body, _ := n.Meta[cloneBodyMetaKey].(string)
		owner := -1
		if dw.CoversPath(n.FilePath) {
			owner = currentIndex
		} else {
			for i := len(base.cloneSources) - 1; i >= 0; i-- {
				layer := base.cloneSources[i].Layer
				if layer.HasFile(n.FilePath) || layer.OwnsNodeIdentity(n.ID) {
					owner = i + baseIndex + 1
					break
				}
			}
			if owner < 0 {
				owner = baseIndex
			}
		}
		if owner < 0 {
			return nil, fmt.Errorf("no clone-corpus owner for visible body %s", n.ID)
		}
		if body == "" {
			if sig, _ := n.Meta[cloneSigMetaKey].(string); sig != "" {
				return nil, fmt.Errorf("visible clone body %s has a signature without body identity", n.ID)
			}
			bodyless[n.ID] = owner
			continue
		}
		if strings.HasPrefix(body, "t") {
			continue // too short to have clone shingles
		}
		wanted[n.ID] = n
		owners[n.ID] = owner
	}

	// Page the pinned sources once each. An upper metadata-only identity
	// override may have no clone sidecar, but a file-owning layer must own its
	// own corpus. Never borrow an older token count for a zero-count row:
	// clone_body hashes unique shingles, not the repetition-sensitive length.
	for i := range sources {
		src := &sources[i]
		after := ""
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			page, err := src.handle.CloneCorpusPage(repo, after, cloneCorpusFinalizeBatch)
			if err != nil {
				return nil, fmt.Errorf("page clone corpus after %q: %w", after, err)
			}
			for _, row := range page {
				if wanted[row.NodeID] != nil {
					src.rows[row.NodeID] = row
				} else if _, check := bodyless[row.NodeID]; check {
					src.rows[row.NodeID] = row
				}
			}
			if len(page) < cloneCorpusFinalizeBatch {
				break
			}
			after = page[len(page)-1].NodeID
		}
	}
	for id, owner := range bodyless {
		path := visible[id].FilePath
		for i := owner; i >= 0; i-- {
			if _, exists := sources[i].rows[id]; exists {
				return nil, fmt.Errorf("visible clone body %s has corpus without body identity", id)
			}
			if i == currentIndex || i == baseIndex ||
				(sources[i].layer != nil && sources[i].layer.HasFile(path)) {
				break // lower sidecars are hidden by this file owner
			}
		}
	}
	rows := make(map[string]graph.CloneCorpusRow, len(wanted))
	for id, n := range wanted {
		body, _ := n.Meta[cloneBodyMetaKey].(string)
		var selected graph.CloneCorpusRow
		found := false
		for i := owners[id]; i >= 0; i-- {
			if i < owners[id] {
				lower := sources[i].handle.GetNode(id)
				if lower == nil {
					if layer := sources[i].layer; layer != nil &&
						(layer.HasFile(n.FilePath) || layer.OwnsNodeIdentity(id)) {
						break // deletion or identity tombstone bars older rows
					}
					continue
				}
				lowerBody, _ := lower.Meta[cloneBodyMetaKey].(string)
				if lowerBody != body {
					break
				}
			}
			row, ok := sources[i].rows[id]
			if !ok {
				if i == currentIndex || i == baseIndex ||
					(sources[i].layer != nil && sources[i].layer.HasFile(n.FilePath)) {
					break // a file owner without its row cannot use stale ancestry
				}
				continue
			}
			if len(row.Shingles) == 0 {
				return nil, fmt.Errorf("visible clone body %s has an empty shingle row", id)
			}
			selected, found = row, true
			break
		}
		if !found {
			return nil, fmt.Errorf("visible shingled clone body %s has no owner corpus row", id)
		}
		if selected.TokenCount <= 0 && owners[id] == currentIndex {
			// The current parsed node still has its exact normalized length;
			// SQLite strips that transient Meta key from persisted nodes.
			selected.TokenCount = tokensFromMeta(n)
		}
		if selected.TokenCount <= 0 {
			return nil, fmt.Errorf("visible clone body %s has no token count", id)
		}
		selected.RepoPrefix = repo
		rows[id] = selected
	}

	scratch := graph.New()
	nodes := make([]*graph.Node, 0, len(visible))
	for _, n := range visible {
		// No parent Meta map or mutable pointer enters the detector.
		nodes = append(nodes, &graph.Node{
			ID: n.ID, Kind: n.Kind, RepoPrefix: n.RepoPrefix,
			FilePath: n.FilePath, StartLine: n.StartLine,
		})
	}
	scratch.AddBatch(nodes, nil)
	pending := make([]graph.CloneCorpusRow, 0, len(rows))
	for _, row := range rows {
		row.Finalized, row.Signature = false, ""
		pending = append(pending, row)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].NodeID < pending[j].NodeID })
	for start := 0; start < len(pending); start += cloneCorpusFinalizeBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+cloneCorpusFinalizeBatch, len(pending))
		if err := scratch.BulkSetCloneCorpus(repo, pending[start:end]); err != nil {
			return nil, err
		}
	}
	_, baseline := detectClonesAndEmitEdgesWithBaselineCtx(ctx, scratch, repo, threshold)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if baseline == nil || !baseline.complete {
		return nil, fmt.Errorf("clone follow-up did not finalize the composed corpus")
	}
	projection := &cloneFollowupProjection{repo: repo}
	after := ""
	for {
		page, err := scratch.CloneCorpusPage(repo, after, cloneCorpusFinalizeBatch)
		if err != nil {
			return nil, err
		}
		for _, row := range page {
			prior := rows[row.NodeID]
			n := visible[row.NodeID]
			oldSig, _ := n.Meta[cloneSigMetaKey].(string)
			if !prior.Finalized || oldSig != row.Signature || prior.TokenCount != row.TokenCount {
				projection.rows = append(projection.rows, cloneFollowupSignature{row: row, filePath: n.FilePath})
			}
		}
		if len(page) < cloneCorpusFinalizeBatch {
			break
		}
		after = page[len(page)-1].NodeID
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cloneFollowupEdgeDiff(dw, scratch, visible)
	if unsupported := dw.Unsupported(); len(unsupported) > 0 {
		return nil, fmt.Errorf("clone follow-up edge changes unsupported: %s", strings.Join(unsupported, ", "))
	}
	return projection, nil
}

// apply runs after semantic enrichment. A partner's node is copied from the
// generation if enrichment wrote one, otherwise from the pinned parent; only
// clone_sig changes. Identity claims publish those rows without claiming the
// partner's file or its unrelated outgoing adjacency.
func (p *cloneFollowupProjection) apply(ctx context.Context, handle *store_sqlite.Store, below LayerBase) (int, error) {
	if p == nil {
		return 0, nil
	}
	var detached []*graph.Node
	var claims []string
	rows := make([]graph.CloneCorpusRow, 0, len(p.rows))
	masks, err := handle.FileMasksContext(ctx)
	if err != nil {
		return 0, err
	}
	coveredPaths := make(map[string]struct{}, len(masks))
	for _, mask := range masks {
		if mask.Mode == store_sqlite.OwnershipReplace {
			coveredPaths[mask.FilePath] = struct{}{}
		}
	}
	for _, change := range p.rows {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		id := change.row.NodeID
		_, covered := coveredPaths[change.filePath]
		n := handle.GetNode(id)
		if n == nil {
			if covered {
				return 0, fmt.Errorf("covered clone body %s is missing from the generation", id)
			}
			n = below.GetNode(id)
			if n == nil {
				return 0, fmt.Errorf("clone partner %s is missing from the parent", id)
			}
			copyNode := *n
			copyNode.Meta = make(map[string]any, len(n.Meta)+1)
			for key, value := range n.Meta {
				copyNode.Meta[key] = value
			}
			if change.row.Signature == "" {
				delete(copyNode.Meta, cloneSigMetaKey)
			} else {
				copyNode.Meta[cloneSigMetaKey] = change.row.Signature
			}
			detached = append(detached, &copyNode)
		}
		if !covered {
			claims = append(claims, id)
		}
		rows = append(rows, change.row)
	}
	if len(detached) > 0 {
		if err := handle.AddBatchChecked(detached, nil); err != nil {
			return 0, err
		}
	}
	for start := 0; start < len(rows); start += cloneCorpusFinalizeBatch {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		end := min(start+cloneCorpusFinalizeBatch, len(rows))
		if err := handle.BulkSetCloneCorpus(p.repo, rows[start:end]); err != nil {
			return 0, err
		}
	}
	if len(claims) > 0 {
		sort.Strings(claims)
		if err := handle.SetNodeIdentityReplacements(claims); err != nil {
			return 0, err
		}
	}
	return len(detached), ctx.Err()
}
