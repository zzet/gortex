package indexer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The copy-based chain compactor.
//
// A working-tree chain is folded into ONE generation over the chain's commit
// generation by copying rows (store_sqlite.FlattenGenerationChain), never by
// re-parsing the working tree: the chain's rows already describe it. The
// folded generation is checked against the chain's own composed view at every
// path and identity the chain speaks for BEFORE it is published, and a fold
// that does not reproduce it is abandoned — the chain stays routed and nothing
// changes for a reader. It serves three callers:
//
//   - the chain compaction at depth (dirty_chain_compaction.go), which
//     otherwise rebuilds the whole working tree direct;
//   - the fold of a large overlay (dirtyChainFoldPaths), so an overlay a
//     long-lived branch or a large import grew stays one layer deep;
//   - the file-by-file import of a large working-tree change, which folds its
//     chain whenever it reaches the depth bound (checkout_import.go).

// dirtyChainFoldPaths is the covered-path count past which a working-tree
// chain of depth > 1 is folded into one generation. A variable only so a
// fixture can fold a small overlay.
var dirtyChainFoldPaths = 2000

// errFlattenRefused is a fold the coordinator declines; nothing was published.
var errFlattenRefused = errors.New("indexer: working-tree chain not folded")

// flattenDirtyChain folds the chain topped by top (rooted at commit) into one
// generation over commit and publishes it. It does not route it.
func (c *CheckoutCoordinator) flattenDirtyChain(
	ctx context.Context, commit store_sqlite.ViewGeneration, top int64,
) (dirtyLayerBuild, error) {
	started := time.Now()
	chain, ok, reason, err := c.dirtyChainRoot(ctx, top, commit, maxDirtyChainDepth)
	if err != nil {
		return dirtyLayerBuild{}, err
	}
	if !ok || len(chain) == 0 {
		return dirtyLayerBuild{}, fmt.Errorf("%w: %s", errFlattenRefused, reason)
	}
	if len(chain) == 1 {
		return dirtyLayerBuild{}, fmt.Errorf("%w: the chain is one generation deep", errFlattenRefused)
	}
	head := chain[0]
	oldestFirst := make([]int64, len(chain))
	for i, row := range chain {
		oldestFirst[len(chain)-1-i] = row.GenerationID
	}
	manifest, why := loadDirtyChainManifest(ctx, c.store, oldestFirst)
	if why != "" {
		return dirtyLayerBuild{}, fmt.Errorf("%w: %s", errFlattenRefused, why)
	}

	generationID, handle, adopted, err := c.store.BeginPayloadGenerationWithStatus(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: head.OwnerKind, GraphID: head.GraphID, LayerID: head.LayerID,
		CheckoutID: head.CheckoutID, GenerationKind: head.GenerationKind,
		BaseGenerationID:     commit.GenerationID,
		LowerViewFingerprint: head.LowerViewFingerprint, TreeOID: head.TreeOID,
		ProvenanceCommitOID: head.ProvenanceCommitOID, ConfigHash: head.ConfigHash,
		ExtractorVersions: head.ExtractorVersions, ResolverVersion: head.ResolverVersion,
		DependencyRevision: head.DependencyRevision, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return dirtyLayerBuild{}, fmt.Errorf("indexer: begin the folded working-tree generation: %w", err)
	}
	if adopted {
		return dirtyLayerBuild{}, fmt.Errorf("%w: generation %d is being built by another writer", errFlattenRefused, generationID)
	}
	abandon := func() { c.abandonCopiedGeneration(context.WithoutCancel(ctx), generationID) }
	counts, err := c.store.FlattenGenerationChain(ctx, oldestFirst, generationID)
	if err != nil {
		abandon()
		return dirtyLayerBuild{}, fmt.Errorf("indexer: fold working-tree chain %v: %w", oldestFirst, err)
	}
	entries := make([]store_sqlite.InputManifestEntry, 0, len(manifest.entries))
	for _, e := range manifest.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].FilePath < entries[j].FilePath })
	meta := store_sqlite.InputManifestMeta{
		ManifestVersion: store_sqlite.InputManifestVersion,
		IsFull:          true,
		EntryCount:      len(entries),
		PolicyDigest:    manifest.policy,
	}
	if err := handle.WriteInputManifest(ctx, meta, entries); err != nil {
		abandon()
		return dirtyLayerBuild{}, fmt.Errorf("indexer: write the folded manifest: %w", err)
	}
	if err := c.verifyFlattenedChain(ctx, commit.GenerationID, oldestFirst, generationID); err != nil {
		abandon()
		return dirtyLayerBuild{}, fmt.Errorf("%w: %v", errFlattenRefused, err)
	}
	if err := c.store.PublishPayloadGeneration(ctx, generationID, time.Now().Unix()); err != nil {
		abandon()
		return dirtyLayerBuild{}, fmt.Errorf("indexer: publish the folded working-tree generation %d: %w", generationID, err)
	}
	markPublicationPhase(ctx, PublicationPublished)
	row, found, err := c.catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !found {
		abandon()
		if err == nil {
			err = fmt.Errorf("indexer: folded generation %d vanished", generationID)
		}
		return dirtyLayerBuild{}, err
	}
	c.logger.Debug("checkout coordinator: working-tree chain folded by copy",
		zap.String("checkout", c.checkoutID),
		zap.Int64s("chain", oldestFirst),
		zap.Int64("folded_generation", generationID),
		zap.Int64("nodes", counts.Nodes), zap.Int64("edges", counts.Edges), zap.Int64("rows", counts.Rows),
		zap.Duration("elapsed", time.Since(started)))
	return dirtyLayerBuild{GenerationID: generationID, Key: logicalDirtyKey(row, commit.GenerationID)}, nil
}

// verifyFlattenedChain compares, over everything the chain speaks for, the
// chain composed over the commit generation with the folded generation over
// the same commit generation: the nodes at every path a member claims, every
// identity a member masks or marks, and the outgoing and incoming edges of
// all of them.
func (c *CheckoutCoordinator) verifyFlattenedChain(ctx context.Context, commitGeneration int64, oldestFirst []int64, folded int64) error {
	base, release, err := c.generationLayerReader(ctx, commitGeneration)
	if err != nil {
		return err
	}
	defer release()
	var chainView graph.Reader = base
	paths := map[string]struct{}{}
	ids := map[string]struct{}{}
	for _, id := range oldestFirst {
		layer, err := graphview.NewGenerationLayerContext(ctx, c.store.AtGeneration(id))
		if err != nil {
			return err
		}
		for _, p := range layer.FilePaths() {
			paths[p] = struct{}{}
		}
		for removed := range layer.RemovedIDs() {
			ids[removed] = struct{}{}
		}
		marks, err := c.store.AtGeneration(id).EdgeSourceMasksContext(ctx)
		if err != nil {
			return err
		}
		for _, m := range marks {
			ids[m.SourceID] = struct{}{}
		}
		chainView = graph.NewOverlaidViewWithLayer(chainView, layer)
	}
	foldedLayer, err := graphview.NewGenerationLayerContext(ctx, c.store.AtGeneration(folded))
	if err != nil {
		return err
	}
	foldedView := graph.NewOverlaidViewWithLayer(base, foldedLayer)
	if got, want := foldedLayer.FilePaths(), foldSortedKeys(paths); !foldEqualStrings(got, want) {
		return fmt.Errorf("the fold claims %d paths, the chain %d", len(got), len(want))
	}
	render := func(r graph.Reader) []string {
		var out []string
		seen := map[string]struct{}{}
		visit := func(n *graph.Node) {
			if n == nil {
				return
			}
			if _, dup := seen[n.ID]; dup {
				return
			}
			seen[n.ID] = struct{}{}
			out = append(out, renderFoldNode(n))
			for _, e := range r.GetOutEdges(n.ID) {
				out = append(out, renderFoldEdge(e))
			}
			// In-edges too: an edge recorded at a claimed path that names
			// an identity the chain removed is visible only from its
			// target's side.
			for _, e := range r.GetInEdges(n.ID) {
				out = append(out, "in "+renderFoldEdge(e))
			}
		}
		for p := range paths {
			for _, n := range r.GetFileNodes(p) {
				visit(n)
			}
		}
		for id := range ids {
			if n := r.GetNode(id); n != nil {
				visit(n)
			} else {
				out = append(out, "absent "+id)
				for _, e := range r.GetOutEdges(id) {
					out = append(out, renderFoldEdge(e))
				}
			}
		}
		sort.Strings(out)
		return out
	}
	want, got := render(chainView), render(foldedView)
	if !foldEqualStrings(got, want) {
		return fmt.Errorf("the fold serves %d rows where the chain serves %d (first difference: %s)",
			len(got), len(want), foldFirstDifference(got, want))
	}
	return nil
}

func renderFoldNode(n *graph.Node) string {
	return fmt.Sprintf("node %s|%s|%s|%s|%d-%d|%s|%v", n.ID, n.Kind, n.Name, n.FilePath, n.StartLine, n.EndLine, n.QualName, n.Meta)
}

func renderFoldEdge(e *graph.Edge) string {
	if e == nil {
		return "edge <nil>"
	}
	return fmt.Sprintf("edge %s->%s|%s|%s:%d|%v|%s", e.From, e.To, e.Kind, e.FilePath, e.Line, e.Confidence, e.Origin)
}

func foldSortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func foldEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func foldFirstDifference(a, b []string) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return strings.TrimSpace(a[i]) + " vs " + strings.TrimSpace(b[i])
		}
	}
	if len(a) > len(b) {
		return "extra " + a[len(b)]
	}
	if len(b) > len(a) {
		return "missing " + b[len(a)]
	}
	return ""
}

// chainCoveredPaths counts the paths a working-tree chain's members claim.
func (c *CheckoutCoordinator) chainCoveredPaths(ctx context.Context, top int64) int {
	covered, err := c.coveredPaths(ctx, c.dirtyChainMembers(ctx, top)...)
	if err != nil {
		return 0
	}
	return len(covered)
}

// foldDue reports whether the routed chain is large enough to fold.
func (c *CheckoutCoordinator) foldDue(ctx context.Context, out CheckoutCycle) bool {
	if out.DirtyGenerationID <= 0 || out.DirtyChainDepth < 2 {
		return false
	}
	return c.chainCoveredPaths(ctx, out.DirtyGenerationID) > dirtyChainFoldPaths
}
