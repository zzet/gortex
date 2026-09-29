package indexer

import (
	"context"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
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
}

// checkoutLanguageCensus is the language census of the committed state a
// working-tree layer over commitGeneration composes over: the base corpus
// plus the commit generation and its committed ancestry, each a
// generation-scoped grouped count. It is cached per commit generation, which
// is immutable.
func (c *CheckoutCoordinator) checkoutLanguageCensus(ctx context.Context, commitGeneration int64) map[string]int {
	c.compaction.mu.Lock()
	if cached, ok := c.compaction.census[commitGeneration]; ok {
		c.compaction.mu.Unlock()
		return cached
	}
	c.compaction.mu.Unlock()
	census := map[string]int{}
	add := func(generationID int64) {
		for language, count := range c.store.AtGeneration(generationID).RepoLanguageCounts([]string{c.repoPrefix})[c.repoPrefix] {
			census[language] += count
		}
	}
	add(0)
	seen := map[int64]bool{0: true}
	id := commitGeneration
	for depth := 0; id > 0 && !seen[id] && depth < graphview.MaxGenerationAncestryDepth; depth++ {
		seen[id] = true
		row, found, err := c.catalog.GetViewGeneration(ctx, id)
		if err != nil || !found {
			break
		}
		add(id)
		id = row.BaseGenerationID
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
