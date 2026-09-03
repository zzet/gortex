package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The view lifecycle's health rollup.
//
// FamiliesOverview is the administrative picture: every family, every checkout,
// every route and every ref view, with their ids and their clocks. It is what a
// person reads when they are looking at one worktree.
//
// This is the other question — how many of each thing exist and what state they
// are in — and it is deliberately a different shape. It carries no ids at all,
// so it is small enough to ride on every daemon status poll, and it is exactly
// the four operational questions in aggregate: how many views exist, how many
// are stuck part-built, how many checkouts are inside a grace window, and how
// many generations are still being held open.

// ViewsHealth is the compact view-lifecycle census.
//
// Every map is keyed by a lifecycle state, so the key vocabulary is the
// catalog's own and cannot grow with the workload. Counters carries the
// view-lifecycle metric registry's non-zero series, which is where the rates
// behind these levels live.
type ViewsHealth struct {
	// Families is how many checkout families the catalog holds.
	Families int `json:"families"`
	// Checkouts counts working copies by lifecycle state.
	Checkouts map[string]int `json:"checkouts,omitempty"`
	// Coordinators is how many checkout build loops this process runs.
	Coordinators int `json:"coordinators"`
	// Generations counts payload generations by state — the direct answer to
	// "how much derived payload is this store holding, and why".
	Generations map[string]int `json:"generations,omitempty"`
	// Leases is how many generations live views currently pin. A generation
	// under a lease cannot be retired, so a lease count that does not fall is
	// the reason a retiring generation is still there.
	Leases int `json:"leases"`
	// RefViews counts named views of committed state by state.
	RefViews map[string]int `json:"ref_views,omitempty"`
	// Counters is the view-lifecycle metric registry, flattened: series key to
	// value, zero-valued series omitted.
	Counters map[string]int64 `json:"counters,omitempty"`
}

// ViewsHealth counts what the view lifecycle currently holds.
//
// It is four catalog listings and a lease read — bounded by the number of
// families, graphs and generations rather than by corpus size — so it is
// cheap enough for a status payload. A listing that fails aborts the census
// and surfaces the error rather than leaving that part at zero: in a census
// a zero is a fact, so a partly-read one would report "no ref views exist"
// where the truth is "the ref views could not be counted". The caller drops
// the whole block on an error, which reads as "not available" instead.
func (l *CheckoutLifecycle) ViewsHealth(ctx context.Context) (ViewsHealth, error) {
	out := ViewsHealth{Counters: viewmetrics.Read().Flat()}
	if l == nil || l.catalog == nil {
		return out, errNoCatalog
	}
	out.Coordinators = l.liveCoordinators("")
	out.Leases = l.leases.Held()

	families, err := l.catalog.ListRepositoryFamilies(ctx)
	if err != nil {
		return out, err
	}
	out.Families = len(families)
	out.Checkouts = map[string]int{}
	out.RefViews = map[string]int{}
	for _, family := range families {
		checkouts, err := l.catalog.ListCheckouts(ctx, family.FamilyID)
		if err != nil {
			return out, err
		}
		for _, checkout := range checkouts {
			out.Checkouts[string(checkout.State)]++
		}
		graphs, err := l.catalog.ListDedicatedGraphs(ctx, family.FamilyID)
		if err != nil {
			return out, err
		}
		for _, dedicated := range graphs {
			views, err := l.catalog.ListRefViews(ctx, dedicated.GraphID)
			if err != nil {
				return out, err
			}
			for _, view := range views {
				out.RefViews[string(view.State)]++
			}
		}
	}

	generations, err := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{})
	if err != nil {
		return out, err
	}
	out.Generations = map[string]int{}
	for _, generation := range generations {
		out.Generations[string(generation.State)]++
	}
	return out, nil
}
