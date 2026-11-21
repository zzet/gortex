package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// A new checkout may reuse one complete committed delta above its dedicated
// base. Never consider a child of such a delta: repeated worktree creation
// must not grow the persisted ancestry. The small search budget also bounds
// Git subprocesses on the admission path.
const maxCommittedParentDiffs = 8

// Borrowing a sibling's branch is a startup optimization, not a reason to
// keep undoing that branch after this checkout switches back to main. Only
// reconsider borrowed parents when this checkout's own committed tree moves;
// the existing dedicated-base pins keep their retention policy unchanged.
func (c *CheckoutCoordinator) preferPrimaryAfterCommitMove(
	ctx context.Context, primary, parent primaryBase, route store_sqlite.CheckoutRoute, targetTree string,
) bool {
	if parent.pinAnchor <= 0 {
		return false
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil || !found || row.TreeOID == targetTree {
		return false
	}
	if primary.treeOID == targetTree {
		return true
	}
	direct, err := diffTreeChanges(ctx, c.root, primary.treeOID, targetTree)
	if err != nil {
		return false
	}
	borrowed, err := diffTreeChanges(ctx, c.root, parent.treeOID, targetTree)
	return err == nil && len(direct) <= len(borrowed)
}

// chooseCommittedParent substitutes a substantially closer immutable tree for
// a fresh checkout's physical build base. The returned release keeps its whole
// ancestry leased until the caller has published and routed the child.
// Every refusal keeps the ordinary primary-based build available.
func (c *CheckoutCoordinator) chooseCommittedParent(
	ctx context.Context, base primaryBase, targetTree string,
) (primaryBase, func(), bool) {
	if base.pinned || base.generationID <= 0 || !validGitOID(base.treeOID) || !validGitOID(targetTree) || base.treeOID == targetTree ||
		c.releaseRequestedBasePin() == base.generationID {
		return base, nil, false
	}
	identity := c.commitIdentity(base, targetTree)
	// Preserve exact own-checkout adoption even when its row is older than
	// the bounded shared scan. This is metadata only: storedCommit can evict
	// cache entries, which requires build admission the caller has not taken.
	own, err := c.catalog.FindReusableViewGenerations(ctx, commitGenerationLookup(identity), maxStoredCommitLayerCandidates)
	if err != nil {
		return base, nil, false
	}
	if _, ok := selectReusableCommitGeneration(own, generationIdentityKey(identity)); ok {
		return base, nil, false
	}
	rows, err := c.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID: base.graphID, OwnerKind: checkoutLayerOwnerKind,
		States: []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady, store_sqlite.ViewGenerationSuperseded},
		Limit:  maxSharedCommitLayerCandidates,
	})
	if err != nil {
		return base, nil, false
	}
	candidates := make([]store_sqlite.ViewGeneration, 0, maxCommittedParentDiffs)
	for _, row := range rows {
		if row.GenerationKind != CommitLayerGenerationKind || !servableGeneration(row.State) {
			continue
		}
		if sameCommitLayerInputs(row, identity) {
			// Exact sharing is cheaper than any new build, including one over
			// a closer parent. Keep its established adoption path unchanged.
			return base, nil, false
		}
		probe := c.commitIdentity(base, row.TreeOID)
		if row.CheckoutID != "" && validGitOID(row.TreeOID) && sameCommitLayerInputs(row, probe) && len(candidates) < maxCommittedParentDiffs {
			candidates = append(candidates, row)
		}
	}
	if len(candidates) == 0 {
		return base, nil, false
	}
	primaryChanges, err := diffTreeChanges(ctx, c.root, base.treeOID, targetTree)
	if err != nil || len(primaryChanges) < 16 {
		return base, nil, false
	}
	// A candidate must remove more than half the changed paths. Small wins
	// do not justify another layer in every subsequent graph read.
	bestChanges := (len(primaryChanges) + 1) / 2
	selected := base
	var release func()
	for _, row := range candidates {
		changes, err := diffTreeChanges(ctx, c.root, row.TreeOID, targetTree)
		if err != nil || len(changes) >= bestChanges {
			continue
		}
		parent, view, ok := c.openCommittedParent(ctx, base.graphID, row)
		if !ok {
			continue
		}
		if release != nil {
			release()
		}
		selected, release, bestChanges = parent, view.Close, len(changes)
	}
	if release == nil {
		return base, nil, false
	}
	return selected, release, true
}

// openCommittedParent is the common admission rule for selection, persisted
// pins and publication revalidation. It proves the physical parent's input
// identity and its direct dedicated anchor before acquiring the ancestry lease.
// Unknown producer state is a refusal; the materializer's compatibility default
// for legacy missing rows is not evidence that this parent can be extended.
func (c *CheckoutCoordinator) openCommittedParent(
	ctx context.Context, graphID string, row store_sqlite.ViewGeneration,
) (primaryBase, *graphview.RepoView, bool) {
	var empty primaryBase
	if !servableGeneration(row.State) || row.OwnerKind != checkoutLayerOwnerKind ||
		row.GraphID != graphID || row.GenerationKind != CommitLayerGenerationKind ||
		row.CheckoutID == "" || row.BaseGenerationID <= 0 || !validGitOID(row.TreeOID) {
		return empty, nil, false
	}
	released := c.releaseRequestedBasePin()
	if released == row.GenerationID || released == row.BaseGenerationID {
		return empty, nil, false
	}
	anchor, found, err := c.catalog.GetViewGeneration(ctx, row.BaseGenerationID)
	if err != nil || !found || !servableGeneration(anchor.State) ||
		anchor.GraphID != graphID || anchor.OwnerKind != checkoutLayerOwnerKind ||
		anchor.GenerationKind != DedicatedBaseGenerationKind || !validGitOID(anchor.TreeOID) {
		return empty, nil, false
	}
	base := primaryBase{graphID: graphID, generationID: anchor.GenerationID, treeOID: anchor.TreeOID}
	if !sameCommitLayerInputs(row, c.commitIdentity(base, row.TreeOID)) {
		return empty, nil, false
	}
	view, err := c.baseViewMaterializer().MaterializeRefView(ctx, graphID, row.GenerationID)
	if err != nil {
		return empty, nil, false
	}
	// Leave room for the child within the existing read-side depth bound.
	if view.ComposesBaseCorpus() || len(view.Generations()) >= graphview.MaxGenerationAncestryDepth {
		view.Close()
		return empty, nil, false
	}
	var parentSemantic, anchorSemantic store_sqlite.ProducerState
	for _, source := range view.GenerationSources() {
		states, err := source.Handle.ProducerStates()
		if err != nil || !completeCommittedParentProducers(states) {
			view.Close()
			return empty, nil, false
		}
		for _, state := range states {
			if state.Producer != string(graphview.CapSemantic) {
				continue
			}
			if source.Handle.ViewGeneration() == row.GenerationID {
				parentSemantic = state.State
			} else if source.Handle.ViewGeneration() == anchor.GenerationID {
				anchorSemantic = state.State
			}
		}
	}
	// Do not replace a usable compiler baseline with an interrupted pass.
	// Equal non-complete states remain eligible: without a semantic manager
	// both generations honestly declare the same structural-only capability.
	if parentSemantic != store_sqlite.ProducerStateComplete && parentSemantic != anchorSemantic {
		view.Close()
		return empty, nil, false
	}
	return primaryBase{
		graphID: graphID, generationID: row.GenerationID, treeOID: row.TreeOID,
		pinned: true, pinAnchor: anchor.GenerationID,
	}, view, true
}

func completeCommittedParentProducers(states []store_sqlite.ProducerCompleteness) bool {
	local, incoming := false, false
	for _, state := range states {
		switch state.Producer {
		case string(graphview.CapResolutionLocal):
			local = state.State == store_sqlite.ProducerStateComplete
		case string(graphview.CapIncomingEdges):
			incoming = state.State == store_sqlite.ProducerStateComplete
		}
	}
	return local && incoming
}

func (base primaryBase) retirementAnchor() int64 {
	if base.pinAnchor > 0 {
		return base.pinAnchor
	}
	return base.generationID
}

func commitGenerationLookup(identity GenerationIdentity) store_sqlite.ViewGeneration {
	return store_sqlite.ViewGeneration{
		OwnerKind: identity.OwnerKind, GraphID: identity.GraphID,
		LayerID: identity.LayerID, CheckoutID: identity.CheckoutID,
		GenerationKind: identity.GenerationKind, BaseGenerationID: identity.BaseGenerationID,
		LowerViewFingerprint: identity.LowerViewFingerprint, TreeOID: identity.TreeOID,
		ProvenanceCommitOID: identity.ProvenanceCommitOID, ConfigHash: identity.ConfigHash,
		ExtractorVersions: identity.ExtractorVersions, ResolverVersion: identity.ResolverVersion,
		DependencyRevision: identity.DependencyRevision,
	}
}
