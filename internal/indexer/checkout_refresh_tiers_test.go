package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/semantic"
)

// checkoutDebtPaths is the routed chain's debt per producer, as path sets.
func checkoutDebtPaths(t *testing.T, c *CheckoutCoordinator) map[graphview.CapabilityID]map[string]bool {
	t.Helper()
	debt, _, err := c.PendingDerived(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[graphview.CapabilityID]map[string]bool, len(debt))
	for producer, d := range debt {
		out[producer] = make(map[string]bool, len(d.Paths))
		for _, p := range d.Paths {
			out[producer][p] = true
		}
	}
	return out
}

func requireCheckoutDerivedFresh(t *testing.T, label string, s MutationTierState) {
	t.Helper()
	if s.Failed || s.Partial || s.FileGraphAt.IsZero() || s.FileGraphAt.Before(s.AckAt) ||
		!s.RelationsAt.Equal(s.FileGraphAt) || !s.DerivedAt.Equal(s.FileGraphAt) {
		t.Fatalf("%s: a published generation stamps file_graph, relations and derived at one instant: %+v", label, s)
	}
}

func requireCheckoutDerivedPartial(t *testing.T, label string, s MutationTierState) {
	t.Helper()
	if s.Failed || !s.Partial || !s.DerivedAt.IsZero() || s.FileGraphAt.IsZero() || !s.RelationsAt.Equal(s.FileGraphAt) {
		t.Fatalf("%s: a published generation stamps file_graph and relations, and derived partial with no stamp: %+v", label, s)
	}
}

// A committed checkout write's ticket carries the tier evidence a watcher
// tool ticket carries: the acknowledgement at admission, the graph tiers when
// the checkout publishes a generation holding the bytes, and failed tiers when
// the ticket ends without one. Its derived tier means what the primary's
// does: clone rows owed to a follow-up leave it fresh, and semantic
// enrichment owed for one of the ticket's own paths makes it partial.
func TestCheckoutRefreshTicketStampsPublicationTiers(t *testing.T) {
	// An installed semantic manager lets a build defer its enrichment; it
	// has no provider, so a build that does not defer records no debt.
	f, c, l := newCheckoutMutationFixtureWith(t, func(b *SparseGenerationBuilder) {
		b.Semantic = semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
		t.Cleanup(func() { _ = b.Semantic.Close() })
	})
	c.followup.manual.Store(true)
	helper := builderRepoPrefix + "/helper.go"
	ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\n\nfunc TieredHelper() { _ = 1 }\n")
	admitted := ticket.Ticket.TierState()
	if admitted.AckAt.IsZero() || admitted.Generation != ticket.Ticket.Generation ||
		!admitted.FileGraphAt.IsZero() || !admitted.RelationsAt.IsZero() || !admitted.DerivedAt.IsZero() ||
		admitted.Failed || admitted.Partial {
		t.Fatalf("an admitted checkout ticket carries only its acknowledgement: %+v (generation %d)", admitted, ticket.Ticket.Generation)
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("refresh did not publish: %+v", result)
	}
	if debt := checkoutDebtPaths(t, c); !debt[graphview.CapSimilarity][helper] || debt[graphview.CapSemantic][helper] {
		t.Fatalf("a changed body owes its clone rows and no semantic data: %v", debt)
	}
	requireCheckoutDerivedFresh(t, "body edit owing only clone rows", ticket.Ticket.TierState())

	// With the follow-up switched on, the next edit publishes without its
	// semantic enrichment: helper.go owes it, and the ticket reads partial.
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "on")
	deferred := queueCheckoutSourceEdit(t, f, l, "package fixture\n\nfunc TieredHelper() { _ = 2 }\n")
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, deferred); result.Err != nil || !result.Reindexed {
		t.Fatalf("refresh did not publish: %+v", result)
	}
	if debt := checkoutDebtPaths(t, c); !debt[graphview.CapSemantic][helper] {
		t.Fatalf("a deferring build owes helper.go semantic data: %v", debt)
	}
	requireCheckoutDerivedPartial(t, "edit owing semantic data", deferred.Ticket.TierState())

	// A batch is judged by its own files, not by the root its ticket is
	// filed under, while helper.go still owes semantic data.
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "off")
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(t.Context()); err != nil {
		m.Close()
		t.Fatal(err)
	}
	var files []CheckoutBatchFile
	for _, file := range []struct{ name, text string }{
		{"tiered_one.go", "package fixture\n\nconst TieredOne = 1\n"},
		{"tiered_two.go", "package fixture\n\nconst TieredTwo = 2\n"},
	} {
		builderWriteFile(t, f.worktree, file.name, file.text)
		path := filepath.Join(f.worktree, file.name)
		files = append(files, CheckoutBatchFile{Path: path, SHA256: checkoutBatchHash(t, path)})
	}
	batch, err := m.EnqueueBatchRefresh(t.Context(), files)
	m.Close()
	if err != nil {
		t.Fatal(err)
	}
	if batch.Ticket.TierState().AckAt.IsZero() {
		t.Fatalf("a batch ticket opens with its acknowledgement: %+v", batch.Ticket.TierState())
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, batch); result.Err != nil || !result.Reindexed {
		t.Fatalf("batch did not publish: %+v", result)
	}
	if debt := checkoutDebtPaths(t, c); !debt[graphview.CapSemantic][helper] ||
		debt[graphview.CapSemantic][builderRepoPrefix+"/tiered_one.go"] || debt[graphview.CapSemantic][builderRepoPrefix+"/tiered_two.go"] {
		t.Fatalf("helper.go, and not the batch, owes semantic data: %v", debt)
	}
	requireCheckoutDerivedFresh(t, "const batch while helper.go owes semantic data", batch.Ticket.TierState())

	stopped := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc StoppedTieredHelper() {}\n")
	_ = c.Close()
	if result := awaitCheckoutRefresh(t, stopped); !errors.Is(result.Err, ErrCheckoutRefreshStopped) {
		t.Fatalf("stopped ticket: %+v", result)
	}
	failed := stopped.Ticket.TierState()
	if !failed.Failed || failed.AckAt.IsZero() || !failed.FileGraphAt.IsZero() || !failed.RelationsAt.IsZero() || !failed.DerivedAt.IsZero() {
		t.Fatalf("a ticket ended without publication reports failed tiers and no stamps: %+v", failed)
	}
}

// owesCheckoutPath places a checkout path among the debt's graph keys, and a
// path it cannot place stands for the whole checkout. A debt the chain cannot
// read, or a published generation whose affected closure was truncated, never
// certifies derived data.
func TestCheckoutDerivedOwedFallbacks(t *testing.T) {
	root := pathkey.CanonicalExistingRoot(t.TempDir())
	sep := string(filepath.Separator)
	owed := map[string]struct{}{"repo/a.go": {}, "repo/sub/b.go": {}, "c.go": {}}
	for _, tc := range []struct {
		name, prefix, path string
		want               bool
	}{
		{"owed file", "repo", filepath.Join(root, "a.go"), true},
		{"owed nested file", "repo", filepath.Join(root, "sub", "b.go"), true},
		{"file owing nothing", "repo", filepath.Join(root, "d.go"), false},
		{"key without the repository prefix", "repo", filepath.Join(root, "c.go"), false},
		{"checkout without a prefix", "", filepath.Join(root, "c.go"), true},
		{"checkout without a prefix, prefixed key", "", filepath.Join(root, "a.go"), false},
		{"checkout root", "repo", root, true},
		{"outside the checkout", "repo", filepath.Join(filepath.Dir(root), "elsewhere", "a.go"), true},
		{"sibling sharing the root's spelling", "repo", root + "-other" + sep + "a.go", true},
		{"escaping through ..", "repo", root + sep + ".." + sep + "a.go", true},
	} {
		c := &CheckoutCoordinator{repoPrefix: tc.prefix}
		if got := c.owesCheckoutPath(owed, root, tc.path); got != tc.want {
			t.Errorf("%s: owesCheckoutPath(%q) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}

	// A one-file cap truncates the closure of a build that must re-resolve
	// more than one file.
	f, c, l := newCheckoutMutationFixtureWith(t, func(b *SparseGenerationBuilder) { b.Config.AffectedByReresolveMax = 1 })
	top := f.route().DirtyGenerationID
	untouched := &checkoutRefreshRequest{ticket: &CheckoutRefreshTicket{
		Ticket: newMutationTierTicket(filepath.Join(f.worktree, "tiers_untouched.go"), 1, nil),
	}}
	if c.derivedOwedLookup(t.Context(), top)(untouched) {
		t.Fatal("a path no layer covers owes nothing")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if !c.derivedOwedLookup(cancelled, top)(untouched) {
		t.Fatal("a debt that cannot be read certified derived data")
	}

	// core.go now calls into two other files, and the manifest change sends
	// the build through the sparse closure builder, whose closure the cap
	// truncates. Nothing is owed to a follow-up producer the primary counts.
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(t.Context()); err != nil {
		m.Close()
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "tsconfig.json", "{}\n")
	builderWriteFile(t, f.worktree, "core.go", "package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n\tHelper()\n\tIsland()\n}\n")
	truncating, err := m.EnqueueRefresh(t.Context(), filepath.Join(f.worktree, "core.go"))
	m.Close()
	if err != nil {
		t.Fatal(err)
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, truncating); result.Err != nil || !result.Reindexed {
		t.Fatalf("refresh did not publish: %+v", result)
	}
	if !truncatedClosureProducers(f.store.AtGeneration(f.route().DirtyGenerationID)) {
		t.Fatalf("the capped build did not truncate its closure: %v", checkoutDebtPaths(t, c))
	}
	if debt := checkoutDebtPaths(t, c); len(debt[graphview.CapSemantic]) > 0 {
		t.Fatalf("no semantic data is owed without a semantic manager: %v", debt)
	}
	requireCheckoutDerivedPartial(t, "edit whose affected closure was truncated", truncating.Ticket.TierState())
}
