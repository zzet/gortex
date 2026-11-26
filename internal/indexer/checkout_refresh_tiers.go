package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
)

// Checkout publication tiers.
//
// A watcher ticket's tiers publish in stages, and each is stamped as it lands
// (mutation_tiers.go). A checkout instead publishes one working-tree
// generation that holds an edited file's syntax graph, its resolution and its
// re-derived dependents, and routes it in one flip. So a committed checkout
// write's ticket stamps file_graph, relations and derived at the instant its
// publication is verified (completeCheckoutRefreshTickets).
//
// Derived means what it means on the primary, so the two compare. It is
// partial instead, with no stamp, when the routed chain owes one of the
// ticket's paths a deferred producer the primary counts (semantic enrichment,
// checkoutDerivedProducers), or when the published generation's affected
// closure was truncated. Clone rows owed to a follow-up do not make it
// partial. The stamp is terminal: a follow-up that later pays the debt does
// not revisit it.
//
// Every ticket committedRefreshRequest admits carries this evidence: a
// committed write's, single-file or batch, and a batch journal recovery's,
// which re-admits its batch through EnqueueBatchRefresh. Each opens with the
// acknowledgement stamp (newMutationTierTicket). A whole-checkout recovery or
// require_fresh ticket (captureCheckoutRefreshFrom,
// captureBoundCheckoutRefresh) has no acknowledgement, so it has nothing to
// time tiers from.

// bindTierGeneration records the admission sequence on a ticket that carries
// tier evidence, so its tier state names the generation its ticket does.
func (t *MutationTicket) bindTierGeneration(generation uint64) {
	t.tierMu.Lock()
	if !t.tiers.AckAt.IsZero() {
		t.tiers.Generation = generation
	}
	t.tierMu.Unlock()
}

// stampCheckoutPublicationTiers records a checkout ticket's terminal tiers. It
// runs before Done is delivered, as finishMutationTierLocked does for a watcher
// ticket, so a receipt that reads the result also reads its stamps.
func stampCheckoutPublicationTiers(ticket *MutationTicket, err error, derivedOwed bool) {
	if ticket == nil {
		return
	}
	ticket.tierMu.Lock()
	defer ticket.tierMu.Unlock()
	state := &ticket.tiers
	if state.AckAt.IsZero() {
		return
	}
	at := time.Now()
	if err != nil {
		state.Failed = true
		state.failedAt = at
		return
	}
	state.FileGraphAt, state.RelationsAt = at, at
	if derivedOwed {
		state.Partial = true
		return
	}
	state.DerivedAt = at
}

// checkoutDerivedProducers are the deferred producers whose debt makes a
// checkout ticket's derived tier partial: those the primary's derived tier
// counts. A watcher edit that skips its semantic pass reads derived partial,
// while its clone index catching up later does not, so graph.similarity is
// not among them.
var checkoutDerivedProducers = []graphview.CapabilityID{graphview.CapSemantic}

// checkoutDerivedGaps reads a routed chain, oldest first with the published
// generation last, for what makes a checkout ticket's derived tier partial:
// the graph paths owed a producer the primary counts, and whether the
// published generation's affected closure was truncated. A truncated closure
// leaves dependents past the cap on a stale resolution whatever paths a
// ticket names, as an incomplete bounded fan-out does on the primary.
func checkoutDerivedGaps(layers []derivedDebtLayer) (owed map[string]struct{}, truncated bool) {
	if len(layers) == 0 {
		// The chain always lists its top; without it nothing vouches for
		// the closure.
		return nil, true
	}
	for _, row := range layers[len(layers)-1].rows {
		if truncatedClosureRow(row) {
			truncated = true
			break
		}
	}
	debt := derivedDebt(layers)
	for _, producer := range checkoutDerivedProducers {
		for _, path := range debt[producer].Paths {
			if owed == nil {
				owed = make(map[string]struct{})
			}
			owed[path] = struct{}{}
		}
	}
	return owed, truncated
}

// derivedOwedLookup answers, for the tickets one publication completes,
// whether a ticket's derived tier is partial (checkoutDerivedGaps). The chain
// under top is read at most once, and only when a ticket that carries tier
// evidence asks. A chain that cannot be read never certifies derived data.
func (c *CheckoutCoordinator) derivedOwedLookup(ctx context.Context, top int64) func(*checkoutRefreshRequest) bool {
	var (
		read      bool
		err       error
		owed      map[string]struct{}
		truncated bool
		root      string
	)
	return func(request *checkoutRefreshRequest) bool {
		ticket := request.ticket.Ticket
		if ticket.TierState().AckAt.IsZero() {
			return false
		}
		if !read {
			read = true
			var layers []derivedDebtLayer
			if layers, err = c.derivedDebtLayers(ctx, top); err == nil {
				owed, truncated = checkoutDerivedGaps(layers)
				root = pathkey.CanonicalExistingRoot(c.root)
			}
		}
		if err != nil || truncated {
			return true
		}
		if len(owed) == 0 {
			return false
		}
		paths := []string{ticket.Path}
		if len(request.batchFiles) > 0 {
			paths = paths[:0]
			for _, file := range request.batchFiles {
				paths = append(paths, file.Path)
			}
		}
		for _, path := range paths {
			if c.owesCheckoutPath(owed, root, path) {
				return true
			}
		}
		return false
	}
}

// owesCheckoutPath reports whether owed, a debt keyed by graph path, covers an
// absolute path in the checkout rooted at root. A root path, or one outside
// the checkout, stands for the whole checkout.
func (c *CheckoutCoordinator) owesCheckoutPath(owed map[string]struct{}, root, path string) bool {
	path = pathkey.CanonicalPath(path)
	if pathkey.EqualPaths(path, root) || !pathkey.HasPathPrefix(path, root) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	key := filepath.ToSlash(rel)
	if c.repoPrefix != "" {
		key = c.repoPrefix + "/" + key
	}
	_, found := owed[key]
	return found
}
