package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/pathkey"
)

// Checkout publication tiers.
//
// A watcher ticket's tiers publish in stages, and each is stamped as it lands
// (mutation_tiers.go). A checkout instead publishes one working-tree
// generation that holds an edited file's syntax graph, its resolution and its
// re-derived dependents, and routes it in one flip. So a committed checkout
// write's ticket stamps file_graph, relations and derived at the instant its
// publication is verified (completeCheckoutRefreshTickets). Derived is partial
// instead while the routed chain owes the path a deferred derived producer
// (derivedDebt): the generation was published without that data.
//
// Only a committed write's ticket carries this evidence. It opens with the
// acknowledgement stamp (newMutationTierTicket). A recovery or require_fresh
// ticket has no acknowledgement, so it has nothing to time tiers from.

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

// derivedOwedLookup answers, for the tickets one publication completes,
// whether the routed chain under top still owes a ticket's paths a deferred
// derived producer. The chain is read at most once, and only when a ticket
// that carries tier evidence asks. A debt that cannot be read never
// certifies derived data.
func (c *CheckoutCoordinator) derivedOwedLookup(ctx context.Context, top int64) func(*checkoutRefreshRequest) bool {
	var (
		read bool
		err  error
		owed map[string]struct{}
		root string
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
				for _, debt := range derivedDebt(layers) {
					for _, path := range debt.Paths {
						if owed == nil {
							owed = make(map[string]struct{})
						}
						owed[path] = struct{}{}
					}
				}
				root = pathkey.CanonicalExistingRoot(c.root)
			}
		}
		if err != nil {
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
