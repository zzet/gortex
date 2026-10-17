package indexer

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Enrichment after publication.
//
// A working-tree edit publishes and flips its route without the semantic
// enrichment stage. Its generation records graph.semantic incomplete with
// graphview.ReasonDeferredToFollowup (and, holding a changed function body,
// graph.similarity the same way, clone_carry.go). Once the checkout has been
// quiet for enrichmentFollowupQuiet, one follow-up builds a working-tree
// generation over the routed top: the owed paths forced into its change set
// (content unchanged), enrichment on, their clone rows recomputed
// (RecomputeDerivedPaths). It records both producers complete and is
// published and flipped like any edit, if the route still names the top it
// was built over.
//
// Nothing about the debt is stored beyond those producer rows and the
// generations' file masks: derivedDebt reads it off the routed chain
// bottom-up, so a restart, an undo, a fold and a HEAD move follow from the
// one rule. A view counts a deferred layer satisfied when a complete layer
// above it covers its paths (graphview.followupSatisfied).
//
// No edit waits for a follow-up: the follow-up takes the build lane at
// background priority and yields to the checkout's foreground work
// (yieldToForeground); an edit that arrives while it builds cancels it, and
// the edit's own follow-up owes, and later enriches, the same paths.

// enrichmentFollowupQuiet is how long the checkout must have had no
// foreground activity before a follow-up starts. Two seconds: an editor's
// save bursts and an agent's multi-file edits arrive well under a second
// apart, so a burst finishes before its follow-up starts and pays for one
// follow-up layer; an agent working at its usual pace (about five seconds
// between edits) leaves three seconds after the quiet period, enough for the
// follow-up of an ordinary file (0.3-1 s of go/types) to land before the next
// edit.
const enrichmentFollowupQuiet = 2 * time.Second

// enrichmentFollowupStarveAfter and enrichmentFollowupStarvedQuiet are the
// guard against starvation. Under steady editing whose gaps never reach the
// quiet period, the debt only grows; once its oldest path has been owed this
// long, the follow-up waits only enrichmentFollowupStarvedQuiet. It still runs
// at background priority and yields to the next edit, so it never blocks one:
// a checkout edited without any half-second pause keeps its debt, marked,
// until the first such pause (or a fold, which carries the marker).
const (
	enrichmentFollowupStarveAfter  = 30 * time.Second
	enrichmentFollowupStarvedQuiet = 500 * time.Millisecond
)

// followupProducers are the producers a follow-up completes. Never
// graph.resolution.local or graph.incoming_edges: an older binary reads those
// as a truncated closure and refuses to chain on the generation.
var followupProducers = []graphview.CapabilityID{graphview.CapSemantic, graphview.CapSimilarity}

// enrichmentFollowupEnabled reports whether working-tree edits defer their
// semantic enrichment to a follow-up. Off until the owner switches the
// default (GORTEX_ENRICHMENT_FOLLOWUP=on enables it): with it off every edit
// enriches inside its own build, as before.
func enrichmentFollowupEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_ENRICHMENT_FOLLOWUP"))) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

// enrichmentFollowup is a coordinator's follow-up schedule. It holds no debt:
// only whether a follow-up is running or owed, and the test seams.
type enrichmentFollowup struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	// again is set when a follow-up was asked for while one ran: the running
	// one reschedules when it ends.
	again bool
	wg    sync.WaitGroup
	stats EnrichmentFollowupStats
	// motionAt is when a working-tree build last found the tree moving under
	// it (noteBuildMotion); zero once a follow-up has caught up with every
	// such build. While it is set, edits defer their enrichment.
	motionAt time.Time

	// quiet is a test seam: the quiet interval (<0: none, 0: the default).
	quiet time.Duration
	// manual is a test seam: no follow-up is scheduled after a cycle; the
	// test runs runEnrichmentFollowup itself.
	manual atomic.Bool
}

// EnrichmentFollowupStats counts what the follow-ups of one checkout did.
type EnrichmentFollowupStats struct {
	Started, Landed, Canceled, HeadMoved, NotNeeded, Failed int
	// LastPaths is how many paths the last landed follow-up enriched.
	LastPaths int
}

// DerivedDebt is what a checkout owes one producer: the graph paths published
// without its data, and when the oldest generation owing them was created.
type DerivedDebt struct {
	Paths []string
	Since time.Time
}

// derivedDebtLayer is one generation of a routed chain as the derivation reads
// it: the paths its file masks cover and its rows of the follow-up producers.
type derivedDebtLayer struct {
	generation int64
	createdAt  time.Time
	// covered is the paths the generation replaces; deleted the paths it
	// removes (nothing is owed for a file that is gone).
	covered []string
	deleted []string
	rows    map[string]store_sqlite.ProducerCompleteness
}

// derivedDebt is the debt of a routed chain, per producer, bottom-up:
//
//	debt[p] = debt[p] − deleted(g)   always (a removed file owes nothing)
//	debt[p] = debt[p] − covered(g)   when g records p complete
//	debt[p] = debt[p] ∪ covered(g)   when g records p incomplete with the token
//	debt[p] unchanged                otherwise (no row, or incomplete for good)
//
// covered(g) rather than the manifest's changed paths: a delta re-derives
// dependents it did not change, and they are published without enrichment
// just as the edited file is. A generation incomplete for good (a sparse
// generation's similarity, the corpus reason) neither adds nor clears.
func derivedDebt(layers []derivedDebtLayer) map[graphview.CapabilityID]DerivedDebt {
	out := map[graphview.CapabilityID]DerivedDebt{}
	for _, producer := range followupProducers {
		owed := map[string]time.Time{}
		for _, g := range layers {
			for _, p := range g.deleted {
				delete(owed, p)
			}
			row, ok := g.rows[string(producer)]
			if !ok {
				continue
			}
			switch {
			case row.State == store_sqlite.ProducerStateComplete:
				for _, p := range g.covered {
					delete(owed, p)
				}
			case row.State == store_sqlite.ProducerStateIncomplete && row.Reason == graphview.ReasonDeferredToFollowup:
				for _, p := range g.covered {
					if _, already := owed[p]; !already {
						owed[p] = g.createdAt
					}
				}
			}
		}
		if len(owed) == 0 {
			continue
		}
		d := DerivedDebt{}
		for p, at := range owed {
			d.Paths = append(d.Paths, p)
			if d.Since.IsZero() || at.Before(d.Since) {
				d.Since = at
			}
		}
		sort.Strings(d.Paths)
		out[producer] = d
	}
	return out
}

// derivedDebtLayers reads a routed working-tree chain, oldest first.
func (c *CheckoutCoordinator) derivedDebtLayers(ctx context.Context, top int64) ([]derivedDebtLayer, error) {
	chain := c.dirtyChainMembers(ctx, top)
	slices.Reverse(chain)
	out := make([]derivedDebtLayer, 0, len(chain))
	for _, id := range chain {
		handle := c.store.AtGeneration(id)
		rows, err := handle.ProducerStates()
		if err != nil {
			return nil, err
		}
		masks, err := handle.FileMasksContext(ctx)
		if err != nil {
			return nil, err
		}
		layer := derivedDebtLayer{generation: id, rows: map[string]store_sqlite.ProducerCompleteness{}}
		for _, r := range rows {
			layer.rows[r.Producer] = r
		}
		for _, m := range masks {
			if m.Mode == store_sqlite.OwnershipDelete {
				layer.deleted = append(layer.deleted, m.FilePath)
				continue
			}
			layer.covered = append(layer.covered, m.FilePath)
		}
		if row, found, err := c.catalog.GetViewGeneration(ctx, id); err == nil && found {
			layer.createdAt = time.Unix(row.CreatedAt, 0)
		}
		out = append(out, layer)
	}
	return out, nil
}

// PendingDerived reports, per producer, the paths the checkout's routed
// working-tree chain publishes without their derived data (the debt a
// follow-up owes), and the routed top they stand under. Empty when nothing is
// owed. It is what an answer's rider reads (semantic_pending and the clone
// equivalent): SemanticPending and SemanticComplete below.
func (c *CheckoutCoordinator) PendingDerived(ctx context.Context) (map[graphview.CapabilityID]DerivedDebt, int64, error) {
	if c == nil || c.catalog == nil {
		return nil, 0, nil
	}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil || !found || route.State != store_sqlite.RouteActive || route.DirtyGenerationID <= 0 {
		return nil, 0, err
	}
	layers, err := c.derivedDebtLayers(ctx, route.DirtyGenerationID)
	if err != nil {
		return nil, 0, err
	}
	return derivedDebt(layers), route.DirtyGenerationID, nil
}

// SemanticPending is the rider's semantic_pending: the graph paths whose
// go/types facts are owed, and since when. Empty when nothing is owed.
func (c *CheckoutCoordinator) SemanticPending(ctx context.Context) (DerivedDebt, error) {
	debt, _, err := c.PendingDerived(ctx)
	if err != nil {
		return DerivedDebt{}, err
	}
	return debt[graphview.CapSemantic], nil
}

// SemanticComplete is the rider's semantic_complete for an answer whose
// scope touches paths: false when any of them is owed. A nil scope asks about
// the whole checkout.
func (c *CheckoutCoordinator) SemanticComplete(ctx context.Context, paths []string) (bool, error) {
	pending, err := c.SemanticPending(ctx)
	if err != nil {
		return false, err
	}
	if len(pending.Paths) == 0 {
		return true, nil
	}
	if paths == nil {
		return false, nil
	}
	owed := make(map[string]struct{}, len(pending.Paths))
	for _, p := range pending.Paths {
		owed[p] = struct{}{}
	}
	for _, p := range paths {
		if _, ok := owed[p]; ok {
			return false, nil
		}
	}
	return true, nil
}

// EnrichmentFollowupStats reports what this checkout's follow-ups did.
func (c *CheckoutCoordinator) EnrichmentFollowupStats() EnrichmentFollowupStats {
	if c == nil {
		return EnrichmentFollowupStats{}
	}
	c.followup.mu.Lock()
	defer c.followup.mu.Unlock()
	return c.followup.stats
}

// defersEnrichment reports whether this checkout's working-tree edits publish
// without their semantic enrichment: always with the follow-up switched on,
// and otherwise while the working tree moves under its builds
// (enrichmentDeferredByMotion).
func (c *CheckoutCoordinator) defersEnrichment() bool {
	return c != nil && c.builder != nil && c.builder.Semantic != nil && (enrichmentFollowupEnabled() || c.enrichmentDeferredByMotion())
}

// Enrichment deferred by motion.
//
// The enrichment stage loads the changed files' packages and their imports
// from the working copy, after the parse and beyond what the build's content
// proof records, so the prepublish fence confirms an enriched build by change
// stamps alone (buildContentProof.noteUnscopedReader): a save to any file it
// may have read, while it runs or before the fence, tears the build. Under a
// sustained edit stream that is nearly every build, and the route stops
// advancing. So once a working-tree build of this checkout finds the tree
// moving under it (torn by the fence or the pass, published as a sample the
// tree had already left, or abandoned by the watcher), its edits publish
// without enrichment, marked as owed (graphview.ReasonDeferredToFollowup),
// exactly as with the follow-up switched on. The follow-up enriches the owed
// paths once the checkout and its working tree have both been quiet for the
// follow-up's window; landing with no motion since it began ends the
// deferral, and the next edit enriches in its own build again.

// noteBuildMotion records that a working-tree build found the tree moving
// under it. Without a semantic manager there is nothing to defer.
func (c *CheckoutCoordinator) noteBuildMotion() {
	if c == nil || c.builder == nil || c.builder.Semantic == nil {
		return
	}
	c.followup.mu.Lock()
	c.followup.motionAt = time.Now()
	c.followup.mu.Unlock()
}

// enrichmentDeferredByMotion reports whether the tree has moved under a build
// since the last follow-up caught up.
func (c *CheckoutCoordinator) enrichmentDeferredByMotion() bool {
	c.followup.mu.Lock()
	defer c.followup.mu.Unlock()
	return !c.followup.motionAt.IsZero()
}

// settleBuildMotion ends the deferral when no build has found the tree moving
// since began, the start of a follow-up that left nothing owed.
func (c *CheckoutCoordinator) settleBuildMotion(began time.Time) {
	c.followup.mu.Lock()
	if !c.followup.motionAt.After(began) {
		c.followup.motionAt = time.Time{}
	}
	c.followup.mu.Unlock()
}

// awaitTreeQuiet waits until the checkout's watcher has reported no change
// for quiet, polling at dirtyChainCompactionYieldPoll. A checkout without a
// watcher has nothing to wait for.
func (c *CheckoutCoordinator) awaitTreeQuiet(ctx context.Context, quiet time.Duration) error {
	ticker := time.NewTicker(dirtyChainCompactionYieldPoll)
	defer ticker.Stop()
	for {
		c.motion.mu.Lock()
		last := c.motion.lastEvent
		c.motion.mu.Unlock()
		if last.IsZero() || time.Since(last) >= quiet {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// scheduleEnrichmentFollowup starts the follow-up worker for a cycle that
// published a deferred edit. One runs at a time; a request while it runs is
// remembered and served when it ends. A closed coordinator schedules nothing.
func (c *CheckoutCoordinator) scheduleEnrichmentFollowup() {
	if !c.defersEnrichment() || c.followup.manual.Load() {
		return
	}
	f := &c.followup
	lifetime := c.lifetimeContext()
	f.mu.Lock()
	if lifetime.Err() != nil {
		f.mu.Unlock()
		return
	}
	if f.running {
		f.again = true
		f.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(lifetime)
	f.running, f.cancel = true, cancel
	f.wg.Add(1)
	f.mu.Unlock()
	go func() {
		defer f.wg.Done()
		defer cancel()
		for {
			c.runEnrichmentFollowup(ctx)
			f.mu.Lock()
			again := f.again && ctx.Err() == nil
			f.again = false
			if !again {
				f.running, f.cancel = false, nil
				f.mu.Unlock()
				return
			}
			f.mu.Unlock()
		}
	}()
}

// waitEnrichmentFollowups waits for the running follow-up (a test seam and a
// shutdown step).
func (c *CheckoutCoordinator) waitEnrichmentFollowups() {
	if c != nil {
		c.followup.wg.Wait()
	}
}

// cancelEnrichmentFollowup stops a running follow-up (the coordinator is
// closing).
func (c *CheckoutCoordinator) cancelEnrichmentFollowup() {
	if c == nil {
		return
	}
	c.followup.mu.Lock()
	cancel := c.followup.cancel
	c.followup.again = false
	c.followup.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// EnrichmentFollowupOutcome is what one follow-up attempt did.
type EnrichmentFollowupOutcome string

const (
	followupLanded    EnrichmentFollowupOutcome = "landed"
	followupNotNeeded EnrichmentFollowupOutcome = "not_needed"
	followupCanceled  EnrichmentFollowupOutcome = "canceled"
	followupHeadMoved EnrichmentFollowupOutcome = "head_moved"
	followupFailed    EnrichmentFollowupOutcome = "failed"
)

// runEnrichmentFollowup waits for quiet, derives the routed chain's debt and,
// when anything is owed, builds, publishes and routes one follow-up over the
// routed top. It returns what it did.
func (c *CheckoutCoordinator) runEnrichmentFollowup(ctx context.Context) EnrichmentFollowupOutcome {
	quiet := c.followup.quiet
	if quiet == 0 {
		quiet = enrichmentFollowupQuiet
		if debt, _, err := c.PendingDerived(ctx); err == nil {
			for _, d := range debt {
				if !d.Since.IsZero() && time.Since(d.Since) >= enrichmentFollowupStarveAfter {
					quiet = enrichmentFollowupStarvedQuiet
				}
			}
		}
	}
	if err := c.awaitForegroundQuiet(ctx, quiet); err != nil {
		return c.noteFollowup(followupCanceled, 0, 0, time.Time{}, err)
	}
	// Deferred because the tree was moving: the follow-up's own enrichment
	// reads the working copy too, so it waits for the tree to settle as well.
	if c.enrichmentDeferredByMotion() {
		if err := c.awaitTreeQuiet(ctx, quiet); err != nil {
			return c.noteFollowup(followupCanceled, 0, 0, time.Time{}, err)
		}
	}
	started := time.Now()
	outcome := c.runEnrichmentFollowupBuild(ctx, started)
	if c.enrichmentDeferredByMotion() && (outcome == followupLanded || outcome == followupNotNeeded) {
		// The deferral ends only when nothing is owed any more: a debt the
		// follow-up could not take (a chain at its bound) is still the
		// next follow-up's, which only a deferring edit schedules.
		if debt, _, err := c.PendingDerived(ctx); err == nil && len(debt) == 0 {
			c.settleBuildMotion(started)
		}
	}
	return outcome
}

// runEnrichmentFollowupBuild is runEnrichmentFollowup once the checkout is
// quiet: it takes the lane, derives the debt and builds the follow-up.
func (c *CheckoutCoordinator) runEnrichmentFollowupBuild(ctx context.Context, started time.Time) EnrichmentFollowupOutcome {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	release, err := c.gate.AcquirePromotable(ctx, ViewBuildBackground, nil)
	if err != nil {
		if ctx.Err() != nil {
			return c.noteFollowup(followupCanceled, 0, 0, started, nil)
		}
		return c.noteFollowup(followupFailed, 0, 0, started, err)
	}
	defer release()
	stop, _ := c.yieldToForeground(ctx, cancel)
	defer stop()
	defer c.gate.NoteHolder(ViewBuildLaneHolder{
		Kind: "enrichment_followup", CheckoutID: c.checkoutID,
		Priority: viewBuildPriorityLabel(ViewBuildBackground),
	})()

	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil || !found || route.State != store_sqlite.RouteActive || route.DirtyGenerationID <= 0 {
		return c.noteFollowup(followupNotNeeded, 0, 0, started, err)
	}
	top := route.DirtyGenerationID
	layers, err := c.derivedDebtLayers(ctx, top)
	if err != nil {
		return c.noteFollowup(followupFailed, top, 0, started, err)
	}
	owed := map[string]struct{}{}
	for _, d := range derivedDebt(layers) {
		for _, p := range d.Paths {
			owed[p] = struct{}{}
		}
	}
	if len(owed) == 0 {
		return c.noteFollowup(followupNotNeeded, top, 0, started, nil)
	}
	// Room for the follow-up and for the next edit: a follow-up never takes
	// the last slot, which would force the next edit to build direct. At the
	// bound the fold lands first and the next quiet period runs over it.
	if len(layers)+1 >= maxChainWalkDepth {
		return c.noteFollowup(followupNotNeeded, top, len(owed), started, nil)
	}
	manifest, why := loadDirtyChainManifest(ctx, c.store, generationsOf(layers))
	if why != "" {
		return c.noteFollowup(followupNotNeeded, top, len(owed), started, nil)
	}
	paths := make([]string, 0, len(owed))
	prefix := c.repoPrefix + "/"
	for p := range owed {
		if c.repoPrefix != "" {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			p = strings.TrimPrefix(p, prefix)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	built, err := c.buildEnrichmentFollowup(ctx, route, dirtyParentSelection{Parent: top, Depth: len(layers), Manifest: manifest}, paths)
	if err != nil || built.GenerationID <= 0 {
		if ctx.Err() != nil {
			return c.noteFollowup(followupCanceled, top, len(paths), started, nil)
		}
		return c.noteFollowup(followupFailed, top, len(paths), started, err)
	}
	if !c.installEnrichmentFollowup(ctx, top, built) {
		c.deferRetire(built.GenerationID, "enrichment follow-up over a moved head")
		return c.noteFollowup(followupHeadMoved, top, len(paths), started, nil)
	}
	c.logger.Info("checkout coordinator: enrichment follow-up landed",
		zap.String("checkout", c.checkoutID), zap.Int64("generation", built.GenerationID),
		zap.Int64("parent", top), zap.Int("paths", len(paths)),
		zap.Float64("ms", float64(time.Since(started).Microseconds())/1000))
	return c.noteFollowup(followupLanded, top, len(paths), started, nil)
}

func generationsOf(layers []derivedDebtLayer) []int64 {
	out := make([]int64, len(layers))
	for i, l := range layers {
		out[i] = l.generation
	}
	return out
}

// noteFollowup counts and, unless landed (logged by the caller), logs one
// follow-up outcome.
func (c *CheckoutCoordinator) noteFollowup(outcome EnrichmentFollowupOutcome, top int64, paths int, started time.Time, err error) EnrichmentFollowupOutcome {
	c.followup.mu.Lock()
	s := &c.followup.stats
	if outcome != followupCanceled || !started.IsZero() {
		s.Started++
	}
	switch outcome {
	case followupLanded:
		s.Landed++
		s.LastPaths = paths
	case followupCanceled:
		s.Canceled++
	case followupHeadMoved:
		s.HeadMoved++
	case followupNotNeeded:
		s.NotNeeded++
	case followupFailed:
		s.Failed++
	}
	c.followup.mu.Unlock()
	if outcome != followupLanded && outcome != followupNotNeeded && c.logger != nil {
		fields := []zap.Field{zap.String("checkout", c.checkoutID), zap.String("outcome", string(outcome)),
			zap.Int64("parent", top), zap.Int("paths", paths)}
		if !started.IsZero() {
			fields = append(fields, zap.Float64("ms", float64(time.Since(started).Microseconds())/1000))
		}
		if err != nil {
			fields = append(fields, zap.Error(err))
		}
		c.logger.Info("checkout coordinator: enrichment follow-up not published", fields...)
	}
	return outcome
}

// installEnrichmentFollowup routes built when the route still names the top
// it was built over (the flip half of installCompactedDirty). It takes the
// cycle lock without waiting: a cycle holding it is publishing an edit, whose
// own follow-up owes the same paths.
func (c *CheckoutCoordinator) installEnrichmentFollowup(ctx context.Context, top int64, built dirtyLayerBuild) bool {
	if ctx.Err() != nil || !c.cycleMu.TryLock() {
		return false
	}
	defer c.cycleMu.Unlock()
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil || !found || route.State != store_sqlite.RouteActive || route.DirtyGenerationID != top {
		return false
	}
	if err := c.flip(ctx, &route, store_sqlite.RouteSlotDirty, built.GenerationID); err != nil {
		return false
	}
	c.retainDirty(ctx, built.Key, built.GenerationID)
	return true
}

// buildEnrichmentFollowup builds the follow-up over parent (the routed top):
// the working tree's state again, with paths forced into the change set,
// enrichment on and their clone rows recomputed.
func (c *CheckoutCoordinator) buildEnrichmentFollowup(ctx context.Context, route store_sqlite.CheckoutRoute, parent dirtyParentSelection, paths []string) (dirtyLayerBuild, error) {
	commit, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil || !found {
		return dirtyLayerBuild{}, err
	}
	return c.buildDirtyLayerAttempts(ctx, commit.GraphID, route.CommitGenerationID, parent, nil, "", func(req *DirtyLayerRequest) {
		req.deferEnrichment = false
		req.followupPaths = paths
		req.RecomputeDerivedPaths = paths
		// A follow-up is one link over the top, never an import batch.
		req.importLarge, req.continueImport = false, false
	})
}
