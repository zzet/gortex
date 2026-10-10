package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
)

// Working-tree generation chains.
//
// A working-tree (dirty) generation's BaseGenerationID is its PHYSICAL parent:
// the generation whose composed view it is a delta over. Built direct, that is
// the checkout's routed commit generation. A chained child names the previous
// published dirty generation of the same checkout instead, and the route's
// commit generation stays the one LOGICAL base: walking BaseGenerationID from
// any servable dirty generation of a checkout must end at the route's commit
// generation within maxChainWalkDepth hops, through rows that all describe the
// same checkout, layer, graph, tree and build policy. That walk is the
// ancestry-root predicate, and every guard that used to require
// "dirty.BaseGenerationID == commit" asks it instead. The guards keep their
// fingerprint checks unchanged: LowerViewFingerprint always names the
// whole-checkout fingerprint the generation was built from, whatever its
// parent.

// maxDirtyChainDepth bounds how many working-tree generations a checkout
// chains before the next edit must fold: the EFFECTIVE depth, which counts the
// layers a running fold is replacing as one. Parent selection and the
// compactor's bound use it.
const maxDirtyChainDepth = 8

// maxChainWalkDepth bounds every walk that validates, serves or lists an
// existing chain: the PHYSICAL depth, which a fold in flight lets grow past
// maxDirtyChainDepth (edits keep chaining over the layers being folded). It
// never exceeds the materializer's bound, so a chain the coordinator accepts
// is always one a reader can compose; until the materializer allows more,
// both depths are the same.
const maxChainWalkDepth = min(maxPhysicalChainDepth, graphview.MaxDirtyChainDepth)

// dirtyChainRoot walks a routed or candidate working-tree generation down its
// BaseGenerationID and reports whether it is rooted at commit: every hop is
// servable (Ready or Superseded), a working-tree generation of this checkout's
// layer on commit's graph, over commit's tree, built under the same config,
// extractor, resolver and dependency revision as the top; and the walk reaches
// commit.GenerationID within maxDepth generations.
//
// chain lists the working-tree generations from the top down to the one that
// sits on commit. A failure carries a reason code: head_or_base_moved when the
// chain is well-formed but lands on another generation than commit,
// chain_depth_exhausted past the bound, and no_parent for anything else. A
// catalog read error is returned as err and never as a verdict.
//
// It performs at most maxDepth point reads and caches nothing.
func (c *CheckoutCoordinator) dirtyChainRoot(
	ctx context.Context, top int64, commit store_sqlite.ViewGeneration, maxDepth int,
) (chain []store_sqlite.ViewGeneration, ok bool, reason string, err error) {
	row, found, err := c.catalog.GetViewGeneration(ctx, top)
	if err != nil {
		return nil, false, "", err
	}
	if !found {
		return nil, false, dirtyChainFallbackNoParent, nil
	}
	return c.dirtyChainRootFrom(ctx, row, commit, maxDepth)
}

// dirtyChainRootFrom is dirtyChainRoot for a top row the caller already read.
func (c *CheckoutCoordinator) dirtyChainRootFrom(
	ctx context.Context, top store_sqlite.ViewGeneration, commit store_sqlite.ViewGeneration, maxDepth int,
) (chain []store_sqlite.ViewGeneration, ok bool, reason string, err error) {
	if maxDepth <= 0 {
		maxDepth = maxChainWalkDepth
	}
	// The top must be this checkout's own working-tree layer (the commit
	// generation's checkout used to guarantee it; an adopted commit layer
	// names another checkout).
	if !dirtyChainHopMatches(top, top, commit) || (c.checkoutID != "" && top.CheckoutID != c.checkoutID) {
		return nil, false, dirtyChainFallbackNoParent, nil
	}
	seen := map[int64]struct{}{}
	row := top
	for {
		if _, loop := seen[row.GenerationID]; loop {
			return nil, false, dirtyChainFallbackNoParent, nil
		}
		seen[row.GenerationID] = struct{}{}
		chain = append(chain, row)
		if len(chain) > maxDepth {
			return nil, false, dirtyChainFallbackChainDepthExhausted, nil
		}
		if row.BaseGenerationID == commit.GenerationID {
			return chain, true, "", nil
		}
		if row.BaseGenerationID <= 0 {
			return nil, false, dirtyChainFallbackHeadOrBaseMoved, nil
		}
		next, found, err := c.catalog.GetViewGeneration(ctx, row.BaseGenerationID)
		if err != nil {
			return nil, false, "", err
		}
		if !found {
			return nil, false, dirtyChainFallbackNoParent, nil
		}
		if next.GenerationKind != DirtyLayerGenerationKind {
			// The walk left the working-tree layer somewhere other than the
			// route's commit generation: the chain is rooted at a commit the
			// route no longer names.
			return nil, false, dirtyChainFallbackHeadOrBaseMoved, nil
		}
		if !dirtyChainHopMatches(next, top, commit) {
			return nil, false, dirtyChainFallbackNoParent, nil
		}
		row = next
	}
}

// dirtyChainHopMatches is the per-hop half of the ancestry-root predicate. A
// chain is one checkout's working-tree layer: every hop carries the top's
// checkout and layer. The commit generation contributes only its graph and
// tree — it may be another checkout's layer the coordinator adopted
// (sharedCommit), so its checkout says nothing about the chain's.
func dirtyChainHopMatches(hop, top, commit store_sqlite.ViewGeneration) bool {
	return servableGeneration(hop.State) &&
		hop.GenerationKind == DirtyLayerGenerationKind &&
		hop.CheckoutID == top.CheckoutID &&
		hop.LayerID == dirtyLayerID(top.CheckoutID) &&
		hop.GraphID == commit.GraphID &&
		hop.TreeOID == commit.TreeOID &&
		hop.ConfigHash == top.ConfigHash &&
		hop.ExtractorVersions == top.ExtractorVersions &&
		hop.ResolverVersion == top.ResolverVersion &&
		hop.DependencyRevision == top.DependencyRevision
}

// dirtyRootedAt is the ancestry-root predicate as the guards ask it: is this
// routed working-tree row a coherent layer over the commit generation named by
// commitID? A row built direct over that commit generation is accepted exactly
// as before (servable and sitting on it); anything deeper must pass the full
// chain walk. It reads the commit row only for a chained row.
func (c *CheckoutCoordinator) dirtyRootedAt(ctx context.Context, dirty store_sqlite.ViewGeneration, commitID int64) (bool, error) {
	if !servableGeneration(dirty.State) || commitID <= 0 {
		return false, nil
	}
	if dirty.BaseGenerationID == commitID {
		return true, nil
	}
	commit, found, err := c.catalog.GetViewGeneration(ctx, commitID)
	if err != nil || !found {
		return false, err
	}
	return c.dirtyRootedAtCommit(ctx, dirty, commit)
}

// dirtyRootedAtCommit is dirtyRootedAt for a commit row the caller already
// read.
func (c *CheckoutCoordinator) dirtyRootedAtCommit(ctx context.Context, dirty, commit store_sqlite.ViewGeneration) (bool, error) {
	if !servableGeneration(dirty.State) {
		return false, nil
	}
	if dirty.BaseGenerationID == commit.GenerationID {
		return true, nil
	}
	_, ok, _, err := c.dirtyChainRootFrom(ctx, dirty, commit, maxChainWalkDepth)
	return ok, err
}

// logicalDirtyKey renders a working-tree row's REUSE identity: its catalog
// identity with the physical parent replaced by the commit generation its chain
// is rooted at. Two generations describing the same working tree over the same
// commit generation are the same state to a reader whatever physical parent
// each was built over, so the undo cache (cachedDirty, retainDirty) keys on
// this. Build adoption and BeginPayloadGeneration keep the physical identity
// (generationIdentityKey), because two builds over different parents are not
// the same physical build. For a generation built direct the two keys are
// equal.
func logicalDirtyKey(row store_sqlite.ViewGeneration, rootID int64) string {
	row.BaseGenerationID = rootID
	return generationRowKey(row)
}

// dirtyRowRendersKey reports whether a retained working-tree row still renders
// the reuse key it was filed under: its physical key (a direct generation), or
// its logical key over the commit generation its chain is rooted at.
func (c *CheckoutCoordinator) dirtyRowRendersKey(ctx context.Context, row store_sqlite.ViewGeneration, key string) bool {
	if generationRowKey(row) == key {
		return true
	}
	root, ok := c.dirtyChainTerminal(ctx, row)
	return ok && logicalDirtyKey(row, root) == key
}

// dirtyChainTerminal walks a working-tree row down to the first generation that
// is not a working-tree generation and returns its id, provided the walk is a
// well-formed chain of this row's layer within the bound. The terminal is then
// checked as a root by the caller's key comparison, which carries its id.
func (c *CheckoutCoordinator) dirtyChainTerminal(ctx context.Context, row store_sqlite.ViewGeneration) (int64, bool) {
	if row.GenerationKind != DirtyLayerGenerationKind || row.BaseGenerationID <= 0 {
		return 0, false
	}
	parent, found, err := c.catalog.GetViewGeneration(ctx, row.BaseGenerationID)
	if err != nil || !found {
		return 0, false
	}
	if parent.GenerationKind != DirtyLayerGenerationKind {
		return parent.GenerationID, true
	}
	// Walk the chain as a predicate over a synthetic commit that carries the
	// top's layer coordinates; the terminal is wherever it lands.
	probe := store_sqlite.ViewGeneration{CheckoutID: row.CheckoutID, GraphID: row.GraphID, TreeOID: row.TreeOID}
	current := parent
	seen := map[int64]struct{}{row.GenerationID: {}}
	for depth := 2; depth <= maxChainWalkDepth; depth++ {
		if _, loop := seen[current.GenerationID]; loop || !dirtyChainHopMatches(current, row, probe) {
			return 0, false
		}
		seen[current.GenerationID] = struct{}{}
		next, found, err := c.catalog.GetViewGeneration(ctx, current.BaseGenerationID)
		if err != nil || !found {
			return 0, false
		}
		if next.GenerationKind != DirtyLayerGenerationKind {
			return next.GenerationID, true
		}
		current = next
	}
	return 0, false
}

// dirtyParentSelection is what selectDirtyParent decided, for the cycle report.
type dirtyParentSelection struct {
	// Parent is the generation a chained child would be built over, 0 when the
	// build must go direct.
	Parent int64
	// Depth is the parent's chain depth (1 = it sits on the commit generation).
	Depth int
	// Manifest is the parent chain's resolved manifest.
	Manifest resolvedManifest
	// Reason is the fallback code when Parent is 0.
	Reason string
	// PreviewChanges is how many paths the delta over the parent would plan,
	// computed from the samples alone (reported only).
	PreviewChanges int
}

// selectDirtyParent decides which generation the next working-tree build over
// commit could stand on: the routed working-tree generation, when it is rooted
// at commit, below the depth bound, built under the current policy, carries a
// complete manifest chain and no truncated closure, and the delta from its
// resolved manifest to sample is usable. Otherwise it returns 0 and one of the
// fallback reason codes.
//
// The cycle builds over the parent it returns. The delta preview is computed with the parent's recorded admission for
// content it already admitted and with HEAD membership inferred from the two
// samples; the build that uses a parent computes the authoritative delta
// itself and may still refuse it.
func (c *CheckoutCoordinator) selectDirtyParent(
	ctx context.Context,
	route store_sqlite.CheckoutRoute,
	commit store_sqlite.ViewGeneration,
	sample gitstate.DirtySnapshot,
) (int64, resolvedManifest, string) {
	sel := c.selectDirtyParentDetail(ctx, route, commit, sample, maxDirtyChainDepth)
	return sel.Parent, sel.Manifest, sel.Reason
}

// selectDirtyParentDetail is selectDirtyParent with the whole decision and an
// explicit depth bound (production passes maxDirtyChainDepth).
func (c *CheckoutCoordinator) selectDirtyParentDetail(
	ctx context.Context,
	route store_sqlite.CheckoutRoute,
	commit store_sqlite.ViewGeneration,
	sample gitstate.DirtySnapshot,
	maxDepth int,
) dirtyParentSelection {
	fallback := func(reason string) dirtyParentSelection { return dirtyParentSelection{Reason: reason} }
	dirtyPaths := 0
	for _, content := range sample.Contents {
		if !content.HeadEqual {
			dirtyPaths++
		}
	}
	if dirtyPaths == 0 {
		return fallback(dirtyChainFallbackCleanCheckout)
	}
	if route.DirtyGenerationID <= 0 || route.CommitGenerationID <= 0 {
		return fallback(dirtyChainFallbackNoParent)
	}
	if route.CommitGenerationID != commit.GenerationID {
		return fallback(dirtyChainFallbackHeadOrBaseMoved)
	}
	chain, ok, reason, err := c.dirtyChainRoot(ctx, route.DirtyGenerationID, commit, maxDepth)
	if err != nil {
		return fallback(dirtyChainFallbackNoParent)
	}
	if !ok {
		return fallback(reason)
	}
	if len(chain) >= maxDepth {
		// The routed top stands at the depth bound, so a child of it would be
		// one layer past what a reader composes. The fold the soft depth owed
		// did not land (it was refused or has not run), so this state is
		// built direct.
		return fallback(dirtyChainFallbackChainDepthExhausted)
	}
	top := chain[0]
	current := c.dirtyIdentity(commit.GraphID, commit.GenerationID)
	if top.ConfigHash != current.ConfigHash || top.ExtractorVersions != current.ExtractorVersions ||
		top.ResolverVersion != current.ResolverVersion || top.DependencyRevision != current.DependencyRevision {
		return fallback(dirtyChainFallbackPolicyChanged)
	}
	if c.store == nil {
		return fallback(dirtyChainFallbackParentManifestMissing)
	}
	oldestFirst := make([]int64, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		oldestFirst = append(oldestFirst, chain[i].GenerationID)
	}
	parentManifest, reason := loadDirtyChainManifest(ctx, c.store, oldestFirst)
	if reason != "" {
		return fallback(reason)
	}
	policy := c.builder.dirtyManifestPolicyDigest(StampDirtyLayerIdentity(current, sample))
	if parentManifest.policy != policy {
		return fallback(dirtyChainFallbackPolicyChanged)
	}
	admit := func(p string) store_sqlite.InputManifestAdmission {
		// Same bytes under the same policy get the same verdict; anything else
		// is in the delta whatever its admission, so it cannot change the plan.
		for _, content := range sample.Contents {
			if content.Path != p {
				continue
			}
			if prior, ok := parentManifest.entry(p); ok && prior.Mode == content.Mode && prior.ContentSHA256 == content.SHA256 {
				return prior.Admission
			}
			break
		}
		return store_sqlite.InputManifestAdmitted
	}
	nextMeta, nextEntries := admittedManifest(sample, admit, policy)
	next := resolvedFromFull(nextMeta, nextEntries)
	changes, reason := planDelta(parentManifest, next, sampledHeadHolds(parentManifest, nextEntries))
	if reason != "" {
		return fallback(reason)
	}
	return dirtyParentSelection{
		Parent:         top.GenerationID,
		Depth:          len(chain),
		Manifest:       parentManifest,
		PreviewChanges: len(changes),
	}
}

// reportDirtyParent records on the cycle report which parent a chained build
// can use, or why none, and returns the selection. It routes nothing.
func (c *CheckoutCoordinator) reportDirtyParent(
	ctx context.Context,
	route store_sqlite.CheckoutRoute,
	commitGeneration int64,
	sample gitstate.DirtySnapshot,
	out *CheckoutCycle,
) dirtyParentSelection {
	selection := dirtyParentSelection{Reason: dirtyChainFallbackNoParent}
	commit, found, err := c.catalog.GetViewGeneration(ctx, commitGeneration)
	if err == nil && found {
		selection = c.selectDirtyParentDetail(ctx, route, commit, sample, c.parentChainBound(ctx, route))
	}
	out.DirtyParentCandidate, out.DirtyChainReason = selection.Parent, selection.Reason
	c.logger.Debug("checkout coordinator: working-tree parent selection",
		zap.String("checkout", c.checkoutID),
		zap.Int64("commit_generation", commitGeneration),
		zap.Int64("routed_dirty_generation", route.DirtyGenerationID),
		zap.String("dirty_parent", selection.String()),
		zap.String("dirty_chain_reason", selection.Reason))
	return selection
}

// buildDirtyLayerForSlot builds the working tree for the dirty slot: over the
// selected working-tree parent when a parent was selected, and direct over
// the commit generation otherwise — including when
// the builder refuses the chained delta, in which case the refusal's reason
// is recorded on the cycle and carried into the direct build's report.
// sample is the cycle's own sample and is the first attempt's change set.
func (c *CheckoutCoordinator) buildDirtyLayerForSlot(
	ctx context.Context,
	graphID string,
	commitGeneration int64,
	selection dirtyParentSelection,
	sample gitstate.DirtySnapshot,
	out *CheckoutCycle,
) (int64, string, error) {
	fallback := selection.Reason
	if selection.Parent > 0 {
		built, err := c.buildDirtyLayerAttempts(ctx, graphID, commitGeneration, selection, &sample, "")
		if built.Work != nil {
			out.DirtyWork = built.Work
		}
		if err != nil {
			return 0, "", err
		}
		if built.Reason == "" {
			if built.GenerationID > 0 {
				out.DirtyParentGenerationID = selection.Parent
				out.DirtyChainDepth = selection.Depth + 1
				out.DirtyBatchRemaining = built.Remaining
				out.DirtyOutpaced = built.Outpaced
				out.dirtySample = built.Sample
			}
			return built.GenerationID, built.Key, nil
		}
		fallback = built.Reason
		out.DirtyChainReason = built.Reason
		out.DirtyParentPreferred = false
		c.logger.Debug("checkout coordinator: working-tree delta refused; building direct",
			zap.String("checkout", c.checkoutID),
			zap.Int64("dirty_parent", selection.Parent),
			zap.String("dirty_chain_reason", built.Reason))
	}
	built, err := c.buildDirtyLayerAttempts(ctx, graphID, commitGeneration, dirtyParentSelection{}, &sample, fallback)
	if built.Work != nil {
		out.DirtyWork = built.Work
	}
	if err == nil && built.GenerationID > 0 {
		out.DirtyChainDepth = 1
		out.DirtyBatchRemaining = built.Remaining
		out.DirtyOutpaced = built.Outpaced
		out.dirtySample = built.Sample
	}
	return built.GenerationID, built.Key, err
}

// deferRetire owes a generation a retirement without performing it: it joins
// the backlog the background sweep drains (SweepRetirements, and the
// lifecycle's deferred-retirement worker), newest first. A foreground cycle
// never deletes payload itself — a chunked payload delete inside an edit's
// cycle is latency the edit pays for work nobody is waiting on.
func (c *CheckoutCoordinator) deferRetire(generationID int64, why string) {
	if generationID <= 0 {
		return
	}
	c.mu.Lock()
	c.backlog[generationID] = struct{}{}
	held := len(c.backlog)
	c.mu.Unlock()
	c.logger.Debug("checkout coordinator: generation retirement owed to the background sweep",
		zap.String("checkout", c.checkoutID),
		zap.Int64("generation", generationID),
		zap.String("why", why),
		zap.Int("backlog", held))
}

// releaseDirtyChain is what the working-tree generations a route leaves get
// when the route moves from oldTop to newTop: every member of oldTop's chain
// that is not also in newTop's chain (its own ancestry, which the new route
// still reads) is released — kept by the reuse cache when the cache holds it,
// owed to the background sweep otherwise. A chain's commit generation is never
// a member. newTop == 0 releases the whole old chain.
func (c *CheckoutCoordinator) releaseDirtyChain(ctx context.Context, oldTop, newTop int64) {
	if oldTop <= 0 || oldTop == newTop {
		return
	}
	keep := map[int64]struct{}{}
	for _, id := range c.dirtyChainMembers(ctx, newTop) {
		keep[id] = struct{}{}
	}
	for _, id := range c.dirtyChainMembers(ctx, oldTop) {
		if _, kept := keep[id]; kept {
			continue
		}
		c.releaseDirty(ctx, id)
	}
}

// dirtyChainMembers lists a working-tree generation and the working-tree
// generations of the same checkout layer beneath it, top first, bounded by
// the chain depth. A generation that cannot be read ends the walk; the top is
// always listed.
func (c *CheckoutCoordinator) dirtyChainMembers(ctx context.Context, top int64) []int64 {
	if top <= 0 {
		return nil
	}
	members := []int64{top}
	row, found, err := c.catalog.GetViewGeneration(ctx, top)
	if err != nil || !found || row.GenerationKind != DirtyLayerGenerationKind {
		return members
	}
	for len(members) <= maxChainWalkDepth && row.BaseGenerationID > 0 {
		next, found, err := c.catalog.GetViewGeneration(ctx, row.BaseGenerationID)
		if err != nil || !found || next.GenerationKind != DirtyLayerGenerationKind ||
			next.CheckoutID != row.CheckoutID || next.LayerID != row.LayerID {
			break
		}
		members = append(members, next.GenerationID)
		row = next
	}
	return members
}

// loadDirtyChainManifest reads and resolves the input manifests of a physical
// working-tree chain, oldest (the one on the commit generation) first. It
// refuses with closure_truncated_parent when any link declared a truncated
// closure, and with the resolveChainLinks codes (parent_manifest_missing,
// policy_changed) when the manifests cannot be folded.
func loadDirtyChainManifest(ctx context.Context, store *store_sqlite.Store, oldestFirst []int64) (resolvedManifest, string) {
	if store == nil || len(oldestFirst) == 0 {
		return resolvedManifest{}, dirtyChainFallbackParentManifestMissing
	}
	links := make([]chainManifestLink, 0, len(oldestFirst))
	for _, generationID := range oldestFirst {
		handle := store.AtGeneration(generationID)
		if truncatedClosureProducers(handle) {
			return resolvedManifest{}, dirtyChainFallbackClosureTruncatedParent
		}
		meta, entries, found, err := handle.InputManifest(ctx)
		if err != nil {
			return resolvedManifest{}, dirtyChainFallbackParentManifestMissing
		}
		links = append(links, chainManifestLink{Meta: meta, Entries: entries, OK: found})
	}
	return resolveChainLinks(links...)
}

// truncatedClosureProducers reports whether a generation declared its
// resolution or incoming-edge producer incomplete — what a build whose
// affected closure was truncated declares. A generation whose producer states
// cannot be read is treated the same way.
func truncatedClosureProducers(handle *store_sqlite.Store) bool {
	rows, err := handle.ProducerStates()
	if err != nil {
		return true
	}
	for _, row := range rows {
		switch row.Producer {
		case string(graphview.CapResolutionLocal), string(graphview.CapIncomingEdges):
			if row.State == store_sqlite.ProducerStateIncomplete {
				return true
			}
		}
	}
	return false
}

// sampledHeadHolds infers HEAD membership from the two samples: a path
// recorded absent was deleted from HEAD's content, a path recorded
// head_equal with bytes is HEAD's content, and one recorded head_equal
// without bytes is absent from HEAD. A path neither sample recorded as
// HEAD-relative is assumed held, which only decides between modified and
// deleted for an undo — both of which are in the delta either way.
func sampledHeadHolds(parent resolvedManifest, next []store_sqlite.InputManifestEntry) func(string) bool {
	known := map[string]bool{}
	for _, e := range next {
		switch e.State {
		case store_sqlite.InputManifestAbsent:
			known[e.FilePath] = true
		case store_sqlite.InputManifestHeadEqual:
			known[e.FilePath] = e.ContentSHA256 != ""
		}
	}
	for p, e := range parent.entries {
		if _, ok := known[p]; !ok && e.State == store_sqlite.InputManifestAbsent {
			known[p] = true
		}
	}
	return func(p string) bool {
		if held, ok := known[p]; ok {
			return held
		}
		return true
	}
}

// String renders a selection for logs.
func (s dirtyParentSelection) String() string {
	if s.Parent == 0 {
		return "direct:" + s.Reason
	}
	return fmt.Sprintf("parent:%d depth:%d delta:%d", s.Parent, s.Depth, s.PreviewChanges)
}
