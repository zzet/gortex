package indexer

import (
	"context"
	"database/sql"
	"errors"
	"github.com/zzet/gortex/internal/gitstate"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Test-side helpers for building working-tree chains directly through the
// builder, without a coordinator: the accumulated-edit characterisation and
// the chain parity oracle. The fixtures here compose over the base corpus
// (generation 0), the legacy regime, where a chain's root sits on 0.

// dirtyChainComposed stacks a working-tree chain, oldest first, over the base
// corpus the way the materializer composes a chain: every generation is a
// layer over everything beneath it.
func dirtyChainComposed(t testing.TB, store *store_sqlite.Store, chain []int64) graph.Reader {
	t.Helper()
	var reader graph.Reader = store.AtGeneration(0)
	for _, generationID := range chain {
		layer, err := graphview.NewGenerationLayer(store.AtGeneration(generationID))
		if err != nil {
			t.Fatalf("NewGenerationLayer(%d): %v", generationID, err)
		}
		reader = graph.NewOverlaidViewWithLayer(reader, layer)
	}
	id, err := graphview.NewRepoViewID(builderRepoPrefix, "graph-fixture", chain[len(chain)-1])
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	composed, _, err := graphview.ComposeRepoView(reader, nil, id)
	if err != nil {
		t.Fatalf("ComposeRepoView: %v", err)
	}
	return composed
}

// dirtyChainBuilder drives BuildDirtyLayer the way the coordinator does with
// chaining on, over the base corpus: each build stands on the previous
// published generation when the builder accepts the delta, and goes direct
// (with the reason recorded) when it does not.
type dirtyChainBuilder struct {
	t       testing.TB
	builder *SparseGenerationBuilder
	store   *store_sqlite.Store
	repoDir string
	// chained turns chaining on; off, every build goes direct.
	chained bool
	// compact enables the compaction step at the coordinator's soft depth
	// (dirtyChainCompactionDepth).
	compact bool
	// chain is the current parent chain, oldest first.
	chain []int64
	// compactions counts the off-path direct rebuilds.
	compactions int
	// fallbacks counts chained attempts the builder refused, by reason.
	fallbacks map[string]int
	// sampler, when set, is the checkout's working-copy sampler every build
	// samples through, as the coordinator's are (so the prepublish fence can
	// confirm by read set). nil samples afresh.
	sampler *gitstate.DirtySampler
}

func newDirtyChainBuilder(t testing.TB, builder *SparseGenerationBuilder, store *store_sqlite.Store, repoDir string, chained bool) *dirtyChainBuilder {
	return &dirtyChainBuilder{
		t: t, builder: builder, store: store, repoDir: repoDir,
		chained: chained, compact: chained, fallbacks: map[string]int{},
	}
}

func (h *dirtyChainBuilder) request() DirtyLayerRequest {
	return DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         h.store,
		CheckoutRoot: h.repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		Sampler:      h.sampler,
	}
}

// build publishes one working-tree generation of the checkout's current disk
// and returns it with its report and the chain (oldest first) it tops.
func (h *dirtyChainBuilder) build() (int64, BuildReport, []int64) {
	h.t.Helper()
	ctx := context.Background()
	reason := ""
	if h.chained && len(h.chain) > 0 {
		manifest, why := loadDirtyChainManifest(ctx, h.store, h.chain)
		switch {
		case why != "":
			reason = why
		case len(h.chain) >= maxDirtyChainDepth:
			reason = dirtyChainFallbackChainDepthExhausted
		default:
			parent := h.chain[len(h.chain)-1]
			req := h.request()
			req.Base = commitLayerBase{Reader: dirtyChainComposed(h.t, h.store, h.chain), corpus: h.store}
			req.Identity.BaseGenerationID = parent
			req.parent, req.parentManifest, req.parentDepth = parent, manifest, len(h.chain)
			id, report, err := h.builder.BuildDirtyLayer(ctx, req)
			var fallback *DirtyChainFallbackError
			switch {
			case err == nil:
				h.chain = append(h.chain, id)
				return id, report, slices.Clone(h.chain)
			case errors.As(err, &fallback):
				reason = fallback.Reason
			default:
				h.t.Fatalf("chained BuildDirtyLayer over %d: %v", parent, err)
			}
		}
		h.fallbacks[reason]++
	}
	req := h.request()
	req.chainFallbackReason = reason
	id, report, err := h.builder.BuildDirtyLayer(ctx, req)
	if err != nil {
		h.t.Fatalf("direct BuildDirtyLayer: %v", err)
	}
	h.chain = []int64{id}
	return id, report, slices.Clone(h.chain)
}

// settle applies the coordinator's compaction rule between edits: once the
// published chain reaches the soft depth (dirtyChainCompactionDepth, the
// constant the coordinator schedules its background compaction at), a direct
// build of the same state — what compactDirtyChain builds — becomes the next
// parent. It is not a measured build. The builder-level harness has no
// coordinator (no route, lane or cycle lock), so it cannot run
// compactDirtyChain itself; the coordinator-level acceptance
// (TestDirtyChainCoordinatorAcceptance*) runs the real compactor.
func (h *dirtyChainBuilder) settle() {
	h.t.Helper()
	if !h.compact || len(h.chain) < dirtyChainCompactionDepth {
		return
	}
	id, _, err := h.builder.BuildDirtyLayer(context.Background(), h.request())
	if err != nil {
		h.t.Fatalf("compacting BuildDirtyLayer: %v", err)
	}
	h.chain = []int64{id}
	h.compactions++
}

// assertCleanIndexParityChain is assertCleanIndexParity for a working-tree
// chain (oldest first) over the base corpus: the reader surface composes the
// whole chain, and every sidecar is folded generation by generation with the
// same mask contract the single-generation oracle applies (a replace or
// delete mask hides every lower row at its path, a node tombstone hides that
// lower identity, pathless rows are hidden only by tombstones). A one-element
// chain is exactly the single-generation oracle.
func assertCleanIndexParityChain(
	t *testing.T, store *store_sqlite.Store, chain []int64, repoDir, label string, strict bool,
) cleanParityResult {
	t.Helper()
	if len(chain) == 1 {
		return assertCleanIndexParity(t, store, chain[0], repoDir, label, strict)
	}
	var result cleanParityResult
	clean := builderOpenStore(t, "clean-"+label)
	builderIndex(t, clean, repoDir)
	composed := dirtyChainComposed(t, store, chain)

	composedNodes := builderRenderNodes(composed.AllNodes())
	cleanNodes := builderRenderNodes(clean.AllNodes())
	composedEdges := builderRenderEdges(composed.AllEdges())
	cleanEdges := builderRenderEdges(clean.AllEdges())
	result.Nodes, result.Edges = len(cleanNodes), len(cleanEdges)
	result.NodesEqual = slices.Equal(composedNodes, cleanNodes)
	result.EdgesEqual = slices.Equal(composedEdges, cleanEdges)
	if !result.NodesEqual {
		result.Diffs = append(result.Diffs, parityDiff("nodes", composedNodes, cleanNodes)...)
	}
	if !result.EdgesEqual {
		result.Diffs = append(result.Diffs, parityDiff("edges", composedEdges, cleanEdges)...)
	}
	result.ReaderSurfaceProbed = strict
	result.ReaderSurfaceAgree = true
	if strict {
		result.ReaderSurfaceAgree = t.Run("reader-surface-"+label, func(t *testing.T) {
			builderAssertReadersAgree(t, composed, clean)
		})
	}
	result.MasksValid = true
	for _, generationID := range chain {
		if store.AtGeneration(generationID).ValidateGenerationMasks() != nil {
			result.MasksValid = false
		}
	}

	dirtyDB := parityOpenRaw(t, store)
	cleanDB := parityOpenRaw(t, clean)
	masks := make([]parityMaskSet, len(chain))
	for i, generationID := range chain {
		masks[i] = parityMasks(t, dirtyDB, generationID)
	}
	fold := func(query string) []string {
		acc := parityRows(t, dirtyDB, query, 0)
		for i, generationID := range chain {
			kept := acc[:0:0]
			for _, row := range acc {
				file, _, _ := strings.Cut(row, "\t")
				if _, masked := masks[i].files[file]; masked {
					continue
				}
				kept = append(kept, row)
			}
			acc = append(kept, parityRows(t, dirtyDB, query, generationID)...)
		}
		slices.Sort(acc)
		return acc
	}
	const filesQuery = `SELECT file_path, content_hash || '|' || size || '|' || node_count || '|' || errors FROM files WHERE repo_prefix = ? AND view_gen = ?`
	const semanticQuery = `SELECT file_path, line || '|' || name || '|' || type_name FROM semantic_binding_types WHERE repo_prefix = ? AND view_gen = ?`
	const constantsQuery = `SELECT file_path, node_id || '|' || value FROM constant_values WHERE repo_prefix = ? AND view_gen = ?`
	composedFiles, cleanFiles := fold(filesQuery), parityRows(t, cleanDB, filesQuery, 0)
	composedSemantic, cleanSemantic := fold(semanticQuery), parityRows(t, cleanDB, semanticQuery, 0)
	composedConstants, cleanConstants := fold(constantsQuery), parityRows(t, cleanDB, constantsQuery, 0)
	result.FilesEqual = slices.Equal(composedFiles, cleanFiles)
	result.SemanticEqual = slices.Equal(composedSemantic, cleanSemantic)
	result.ConstantsEqual = slices.Equal(composedConstants, cleanConstants)
	if !result.FilesEqual {
		result.Diffs = append(result.Diffs, parityDiff("files", composedFiles, cleanFiles)...)
	}
	if !result.SemanticEqual {
		result.Diffs = append(result.Diffs, parityDiff("semantic_binding_types", composedSemantic, cleanSemantic)...)
	}
	if !result.ConstantsEqual {
		result.Diffs = append(result.Diffs, parityDiff("constant_values", composedConstants, cleanConstants)...)
	}

	composedFTS := parityComposeFTSChain(t, dirtyDB, chain)
	cleanFTS := parityFTSRows(t, cleanDB, 0)
	result.SymbolFTSRows = len(cleanFTS)
	result.SymbolFTSEqual = slices.Equal(composedFTS, cleanFTS)
	if !result.SymbolFTSEqual {
		result.Diffs = append(result.Diffs, parityDiff("symbol_fts", composedFTS, cleanFTS)...)
	}
	result.SearchProbesEqual = paritySearchProbes(composedFTS, cleanFTS)
	result.ComposedDigest = parityDigest(composedNodes, composedEdges, composedFiles, composedSemantic, composedConstants, composedFTS)
	result.CleanDigest = parityDigest(cleanNodes, cleanEdges, cleanFiles, cleanSemantic, cleanConstants, cleanFTS)
	if len(result.Diffs) > 20 {
		result.Diffs = append(result.Diffs[:20], "...")
	}
	if strict && !result.ok() {
		t.Errorf("%s: composed chain %v does not match the clean index: %+v", label, chain, result)
	}
	return result
}

// parityComposeFTSChain composes symbol FTS over a working-tree chain, oldest
// first: each generation hides the lower documents at the paths it replaces or
// deletes and the lower identities it tombstones, then adds its own.
func parityComposeFTSChain(t *testing.T, db *sql.DB, chain []int64) []string {
	t.Helper()
	fts := parityFTSRowsWithFile(t, db, 0)
	for _, generationID := range chain {
		masks := parityMasks(t, db, generationID)
		kept := fts[:0:0]
		for _, row := range fts {
			if _, masked := masks.files[row[0]]; masked && row[0] != "" {
				continue
			}
			id, _, _ := strings.Cut(row[1], "\t")
			if _, gone := masks.tombstones[id]; gone {
				continue
			}
			kept = append(kept, row)
		}
		fts = append(kept, parityFTSRowsWithFile(t, db, generationID)...)
	}
	out := make([]string, 0, len(fts))
	for _, row := range fts {
		out = append(out, row[1])
	}
	slices.Sort(out)
	return out
}
