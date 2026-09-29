package indexer

import (
	"context"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// dirtyChainCompactionDepth is the soft chain depth: a build that publishes a
// chain this deep schedules a background compaction.
const dirtyChainCompactionDepth = 4

// dirtyChainCompactor is the coordinator's compaction state.
type dirtyChainCompactor struct {
	mu sync.Mutex
	// census caches checkoutLanguageCensus per commit generation.
	census map[int64]map[string]int
	// lastForeground is when this checkout's latest foreground cycle ended.
	lastForeground time.Time
}

// deferFailedGeneration owes a failed build's generation a retirement: one the
// builder abandoned (failed) or refused at its pre-publish fence
// (superseded). A generation still building or ready is left alone: a
// coalesced follower's error says nothing about the leader's generation. The
// sweep retires nothing a route, a lease or a layer above still references.
func (c *CheckoutCoordinator) deferFailedGeneration(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	lookup := context.WithoutCancel(ctx)
	row, found, err := c.catalog.GetViewGeneration(lookup, generationID)
	if err != nil || !found ||
		(row.State != store_sqlite.ViewGenerationFailed && row.State != store_sqlite.ViewGenerationSuperseded) {
		return
	}
	c.deferRetire(generationID, "failed working-tree build")
}

// cycleAdmission is how long a cycle waited before building, by stage: the
// settle check (the shared working-copy sample), the cycle lock, and the
// build lane; and what held the lane when the cycle queued for it (a Kind
// "undeclared" holder is a builder that declares nothing, nil an idle lane).
type cycleAdmission struct {
	Preflight  time.Duration
	CycleLock  time.Duration
	Lane       time.Duration
	LaneHeldBy *ViewBuildLaneHolder
}

// checkoutLanguageCensus is the language census of the committed state a
// working-tree layer over commitGeneration composes over: the commit
// generation and its committed ancestry, each a generation-scoped grouped
// count, plus the base corpus (generation 0) when the view actually composes
// over it. The materializer stands a chain whose root is a full dedicated
// generation on that generation alone (graphview's assemble: base =
// firstHandle), so generation 0 is not part of such a view and is not
// counted; counting it paid a whole-repository grouped scan of the flat base
// per coordinator and commit generation for rows the view never serves. A
// chain whose root is not a dedicated generation, or whose walk did not reach
// its root, keeps the base count. It is cached per commit generation, which
// is immutable.
func (c *CheckoutCoordinator) checkoutLanguageCensus(ctx context.Context, commitGeneration int64) map[string]int {
	c.compaction.mu.Lock()
	if cached, ok := c.compaction.census[commitGeneration]; ok {
		c.compaction.mu.Unlock()
		return cached
	}
	c.compaction.mu.Unlock()
	started := time.Now()
	counted := 0
	census := map[string]int{}
	add := func(generationID int64, published bool) {
		counted++
		handle := c.store.AtGeneration(generationID)
		var counts map[string]int
		if published {
			// A ready generation's rows are immutable: its count is shared by
			// every checkout standing on it and paid once per process.
			counts = handle.PublishedRepoLanguageCounts(c.repoPrefix)
		} else {
			counts = handle.RepoLanguageCounts([]string{c.repoPrefix})[c.repoPrefix]
		}
		for language, count := range counts {
			census[language] += count
		}
	}
	dedicatedRoot := false
	seen := map[int64]bool{0: true}
	id := commitGeneration
	for depth := 0; id > 0 && !seen[id] && depth < graphview.MaxGenerationAncestryDepth; depth++ {
		seen[id] = true
		row, found, err := c.catalog.GetViewGeneration(ctx, id)
		if err != nil || !found {
			break
		}
		add(id, row.State == store_sqlite.ViewGenerationReady)
		if row.BaseGenerationID <= 0 {
			dedicatedRoot = row.GenerationKind == dedicatedGenerationKind
		}
		id = row.BaseGenerationID
	}
	if !dedicatedRoot {
		add(0, false)
	}
	c.compaction.mu.Lock()
	if c.compaction.census == nil {
		c.compaction.census = map[int64]map[string]int{}
	}
	if len(c.compaction.census) > 8 {
		clear(c.compaction.census)
	}
	c.compaction.census[commitGeneration] = census
	c.compaction.mu.Unlock()
	// The one cold cost of the census: a whole-generation count per level the
	// process has not counted yet (the published ones are memoized per
	// process), paid only by a build whose own files leave an enrichable
	// language below the admission floor.
	if c.logger == nil {
		return census
	}
	c.logger.Info("indexer: checkout language census counted",
		zap.String("checkout", c.checkoutID),
		zap.Int64("commit_generation", commitGeneration),
		zap.Int("generations", counted),
		zap.Bool("base_counted", !dedicatedRoot),
		zap.Duration("elapsed", time.Since(started)))
	return census
}

const publicationSourceObservedChange = "observed_change"

var observedChangeSequence atomic.Uint64

// openObservedChangeRecord opens the publication record of a change a
// ticketless cycle found: the cycle's shared sample (taken by its settle
// check) differs from what the routed working-tree generation describes. Its
// origin is when that sample's git status started, and change_observed is
// marked now, when the cycle has it. nil when nothing changed or the route
// cannot be read.
func (c *CheckoutCoordinator) openObservedChangeRecord(ctx context.Context) *PublicationPhaseRecord {
	sample, err := c.cycleSample(ctx)
	if err != nil || sample.Fingerprint == "" {
		return nil
	}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return nil
	}
	if found && route.DirtyGenerationID > 0 {
		row, found, err := c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
		if err != nil || (found && row.LowerViewFingerprint == sample.Fingerprint) {
			return nil
		}
	}
	origin := c.sampler.LastSampleStarted()
	if origin.IsZero() {
		origin = time.Now()
	}
	key := "observed-" + strconv.FormatUint(observedChangeSequence.Add(1), 10)
	record := DefaultPublicationPhases().Begin(c.checkoutID, key, publicationSourceObservedChange, origin)
	record.Mark(PublicationChangeObserved)
	return record
}

// finishObservedChangeRecord closes a cycle's observed-change record with
// what the cycle published: completed when the route names a working-tree
// generation it built or re-routed, failed otherwise.
func finishObservedChangeRecord(ctx context.Context, out CheckoutCycle) {
	record := phaseRecordFrom(ctx)
	if record == nil {
		return
	}
	if out.Err == nil && out.DirtyGenerationID > 0 && (out.DirtyBuilt || out.DirtyReused) {
		record.SetGeneration(out.DirtyGenerationID)
		record.Mark(PublicationTicketCompleted)
		return
	}
	record.Mark(PublicationTicketFailed)
}

// noteForegroundCycle records the end of one foreground cycle of this
// checkout (one that served a ticket or built a working tree).
func (c *CheckoutCoordinator) noteForegroundCycle(ended time.Time) {
	if c == nil {
		return
	}
	k := &c.compaction
	k.mu.Lock()
	defer k.mu.Unlock()
	if ended.After(k.lastForeground) {
		k.lastForeground = ended
	}
}

// dedicatedGenerationKind is the catalog kind of a dedicated graph's
// generations; a chain rooted at one does not compose over generation 0.
const dedicatedGenerationKind = "dedicated"
