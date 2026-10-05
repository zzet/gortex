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
	return c.flattenDirtyChainOver(ctx, commit, commit, top, c.copyChainAtOnce)
}

// chainCopier copies a chain (oldest first) into the empty building
// generation to. finish runs once the fold is published (ok) or abandoned.
type chainCopier func(ctx context.Context, oldestFirst []int64, to int64) (counts store_sqlite.GenerationCopyCounts, finish func(ctx context.Context, ok bool), err error)

// copyChainAtOnce is the one-shot copy (one transaction per member).
func (c *CheckoutCoordinator) copyChainAtOnce(ctx context.Context, oldestFirst []int64, to int64) (store_sqlite.GenerationCopyCounts, func(context.Context, bool), error) {
	counts, err := c.store.FlattenGenerationChain(ctx, oldestFirst, to)
	return counts, func(context.Context, bool) {}, err
}

// flattenDirtyChainOver folds the part of the chain topped by top that
// stands on root into one generation over root, with copy, verifies and
// publishes it. root is the commit generation for a whole-chain fold, or a
// working-tree generation of the chain for the fold of the layers above it
// (the fold above a running fold). The folded generation carries the whole
// chain's manifest, since it describes the whole working tree.
func (c *CheckoutCoordinator) flattenDirtyChainOver(
	ctx context.Context, commit, root store_sqlite.ViewGeneration, top int64, copy chainCopier,
) (dirtyLayerBuild, error) {
	built, _, err := c.flattenDirtyChainChecked(ctx, commit, root, top, copy, c.verifyFlattenedChain)
	return built, err
}

// foldVerifier checks a copied fold against the chain it folds (oldest first)
// over root, before the fold is published.
type foldVerifier func(ctx context.Context, root int64, oldestFirst []int64, folded int64) error

// flattenDirtyChainChecked is flattenDirtyChainOver with the verifier named:
// nil deliberately skips validation; production inline folds supply the exact
// verifier. It also returns the folded members, oldest first.
func (c *CheckoutCoordinator) flattenDirtyChainChecked(
	ctx context.Context, commit, root store_sqlite.ViewGeneration, top int64, copy chainCopier, verify foldVerifier,
) (dirtyLayerBuild, []int64, error) {
	phases := inlineFoldPhasesFrom(ctx)
	// A planning fold must reenter with ordinary interactive cancellation.
	// A fold following a committed import already owns its protected publication
	// lane; rearming it here could prevent compaction under sustained demand.
	foldPublication, _ := ctx.Value(importFoldPublicationKey{}).(*importFoldPublication)
	if lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane); lane != nil && lane.detached && foldPublication == nil {
		var admissionErr error
		ctx, admissionErr = lane.reenter(ctx, true)
		if admissionErr != nil {
			return dirtyLayerBuild{}, nil, admissionErr
		}
	}
	started := time.Now()
	whole, ok, reason, err := c.dirtyChainRoot(ctx, top, commit, maxChainWalkDepth)
	if err != nil {
		return dirtyLayerBuild{}, nil, err
	}
	if !ok || len(whole) == 0 {
		return dirtyLayerBuild{}, nil, fmt.Errorf("%w: %s", errFlattenRefused, reason)
	}
	chain := whole
	if root.GenerationID != commit.GenerationID {
		chain = nil
		for _, row := range whole {
			if row.GenerationID == root.GenerationID {
				break
			}
			chain = append(chain, row)
		}
		if len(chain) == len(whole) {
			return dirtyLayerBuild{}, nil, fmt.Errorf("%w: generation %d is not in the chain", errFlattenRefused, root.GenerationID)
		}
	}
	if len(chain) <= 1 {
		return dirtyLayerBuild{}, nil, fmt.Errorf("%w: the chain is one generation deep", errFlattenRefused)
	}
	head := chain[0]
	oldestFirst := make([]int64, len(chain))
	for i, row := range chain {
		oldestFirst[len(chain)-1-i] = row.GenerationID
	}
	wholeOldestFirst := make([]int64, len(whole))
	for i, row := range whole {
		wholeOldestFirst[len(whole)-1-i] = row.GenerationID
	}
	phases.next("manifest_read")
	manifest, why := loadDirtyChainManifest(ctx, c.store, wholeOldestFirst)
	if why != "" {
		return dirtyLayerBuild{}, nil, fmt.Errorf("%w: %s", errFlattenRefused, why)
	}

	phases.next("reservation")
	generationID, handle, adopted, err := c.store.BeginPayloadGenerationWithStatus(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: head.OwnerKind, GraphID: head.GraphID, LayerID: head.LayerID,
		CheckoutID: head.CheckoutID, GenerationKind: head.GenerationKind,
		BaseGenerationID:     root.GenerationID,
		LowerViewFingerprint: head.LowerViewFingerprint, TreeOID: head.TreeOID,
		ProvenanceCommitOID: head.ProvenanceCommitOID, ConfigHash: head.ConfigHash,
		ExtractorVersions: head.ExtractorVersions, ResolverVersion: head.ResolverVersion,
		DependencyRevision: head.DependencyRevision, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return dirtyLayerBuild{}, nil, fmt.Errorf("indexer: begin the folded working-tree generation: %w", err)
	}
	if adopted {
		return dirtyLayerBuild{}, nil, fmt.Errorf("%w: generation %d is being built by another writer", errFlattenRefused, generationID)
	}
	phases.next("copy")
	counts, finish, err := copy(ctx, oldestFirst, generationID)
	abandon := func() {
		phases.fail()
		if finish != nil {
			finish(context.WithoutCancel(ctx), false)
		}
		c.abandonCopiedGeneration(context.WithoutCancel(ctx), generationID)
	}
	if err != nil {
		abandon()
		return dirtyLayerBuild{}, nil, fmt.Errorf("indexer: fold working-tree chain %v: %w", oldestFirst, err)
	}
	phases.next("manifest_write")
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
		return dirtyLayerBuild{}, nil, fmt.Errorf("indexer: write the folded manifest: %w", err)
	}
	phases.next("exact_validation")
	if err := verifyFold(ctx, verify, root.GenerationID, oldestFirst, generationID); err != nil {
		abandon()
		return dirtyLayerBuild{}, nil, fmt.Errorf("%w: %v", errFlattenRefused, err)
	}
	phases.next("derivation_stamps")
	if err := stampFoldedGeneration(ctx, c.store, oldestFirst, handle); err != nil {
		abandon()
		return dirtyLayerBuild{}, nil, err
	}
	phases.next("publish_finish")
	if err := publishCopiedGeneration(ctx, generationID, foldPublication, c.store.PreparePayloadGenerationPublication); err != nil {
		abandon()
		return dirtyLayerBuild{}, nil, fmt.Errorf("indexer: publish the folded working-tree generation %d: %w", generationID, err)
	}
	finish(context.WithoutCancel(ctx), true)
	markPublicationPhase(ctx, PublicationPublished)
	row, found, err := c.catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !found {
		abandon()
		if err == nil {
			err = fmt.Errorf("indexer: folded generation %d vanished", generationID)
		}
		return dirtyLayerBuild{}, nil, err
	}
	c.logger.Debug("checkout coordinator: working-tree chain folded by copy",
		zap.String("checkout", c.checkoutID),
		zap.Int64s("chain", oldestFirst),
		zap.Int64("folded_generation", generationID),
		zap.Int64("nodes", counts.Nodes), zap.Int64("edges", counts.Edges), zap.Int64("rows", counts.Rows),
		zap.Duration("elapsed", time.Since(started)))
	return dirtyLayerBuild{GenerationID: generationID, Key: logicalDirtyKey(row, commit.GenerationID)}, oldestFirst, nil
}

// publishCopiedGeneration prepares only report metadata outside an import's
// physical lane. The caller's immutable-ancestry fence and live admission still
// precede every payload seal, validation and guarded publication.
func publishCopiedGeneration(ctx context.Context, generationID int64, admission *importFoldPublication,
	prepare func(context.Context, int64) (*store_sqlite.PreparedPayloadGenerationPublication, error),
) error {
	publication, err := prepare(ctx, generationID)
	if err != nil {
		return err
	}
	if admission != nil {
		if err := admission.beforePublish(ctx); err != nil {
			return err
		}
	}
	if err := publication.Publish(ctx, time.Now().Unix()); err != nil {
		return err
	}
	if admission != nil && admission.afterPublish != nil {
		admission.afterPublish()
	}
	return nil
}

// verifyFlattenedChain compares, over everything the chain speaks for, the
// chain composed over the commit generation with the folded generation over
// the same commit generation: the nodes at every path a member claims, every
// identity a member masks or marks, and the outgoing and incoming edges of
// all of them. It reads in steps (verifyFlattenedChainInSteps) and never runs
// inside an edit: its cost is three reads (the node, its out-edges, its
// in-edges) per claimed identity through every layer of the chain.
func (c *CheckoutCoordinator) verifyFlattenedChain(ctx context.Context, commitGeneration int64, oldestFirst []int64, folded int64) error {
	return c.verifyFlattenedChainInSteps(ctx, commitGeneration, oldestFirst, folded)
}

// verifyFold runs verify, when there is one.
func verifyFold(ctx context.Context, verify foldVerifier, root int64, oldestFirst []int64, folded int64) error {
	if verify == nil {
		return nil
	}
	return verify(ctx, root, oldestFirst, folded)
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
