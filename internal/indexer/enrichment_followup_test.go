package indexer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Enrichment after publication, with the settings the daemon starts with (the
// product's compiler load, the default admission floor, the follow-up on by
// default): at every edit of the kind-parity fixture's chain,
//
//   - before the follow-up lands, the served view equals a clean semantic
//     index except for rows the marker names (rows recorded at an owed path,
//     and the external symbol rows the go/types pass derives for them), and
//     the view reads graph.semantic incomplete;
//   - after it lands, the view equals the clean semantic index with no
//     exception beyond the engine residuals, reads graph.semantic complete,
//     and nothing is owed.
func TestEnrichmentFollowupParityBeforeAndAfter(t *testing.T) {
	// The follow-up is off by default until the owner switches it on;
	// these tests switch it on and otherwise run the product's defaults.
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "on")
	f, c, mgr := semanticChainFixtureProduct(t, kindParityTree())
	if !c.defersEnrichment() {
		t.Fatal("GORTEX_ENRICHMENT_FOLLOWUP=on does not defer enrichment")
	}
	c.followup.quiet = -1
	coordinatorReconcile(t, c)
	ctx := context.Background()
	primary := newKindParityPrimaryWith(t, f.worktree, mgr)
	landed := 0
	for _, edit := range kindParityEdits() {
		applyKindParityEdit(t, f.worktree, edit)
		out := coordinatorReconcile(t, c)
		primary.save(edit.paths())
		label := fmt.Sprintf("depth %d/%s", out.DirtyChainDepth, edit.name)

		pending, err := c.SemanticPending(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if out.DirtyBuilt && len(edit.write) > 0 && len(pending.Paths) == 0 {
			// A deletion owes nothing (the file is gone); a write owes at
			// least the written path.
			t.Fatalf("%s: a deferred edit wrote a file and nothing is owed", label)
		}
		view := chainMaterialize(t, f)
		if len(pending.Paths) > 0 && view.Completeness.State(graphview.CapSemantic) == graphview.StateComplete {
			t.Errorf("%s: graph.semantic reads complete while %v is owed", label, pending.Paths)
		}
		followupMarkerParity(t, label+" before the follow-up", f.worktree, view.Reader, primary.store, builderIndexSemantic(mgr), pending.Paths)
		view.Close()

		switch outcome := c.runEnrichmentFollowup(ctx); outcome {
		case followupLanded:
			landed++
		case followupNotNeeded:
			if len(pending.Paths) > 0 {
				// Only at the chain bound: the next edit builds direct.
				t.Logf("%s: follow-up not needed at depth %d with %d owed", label, out.DirtyChainDepth, len(pending.Paths))
				continue
			}
		default:
			t.Fatalf("%s: follow-up %s", label, outcome)
		}
		after, err := c.SemanticPending(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Paths) > 0 {
			t.Errorf("%s: %v still owed after the follow-up landed", label, after.Paths)
		}
		if complete, _ := c.SemanticComplete(ctx, nil); !complete {
			t.Errorf("%s: SemanticComplete is false after the follow-up", label)
		}
		view = chainMaterialize(t, f)
		// Every layer of the stack declares graph.semantic; once the
		// follow-up landed nothing is owed, and the view reads it complete.
		if s := view.Completeness.State(graphview.CapSemantic); s != graphview.StateComplete {
			t.Errorf("%s: graph.semantic reads %s after the follow-up, want %s", label, s, graphview.StateComplete)
		}
		kindParityCheckIndexed(t, label+" after the follow-up", f.worktree, view.Reader, nil,
			func() graph.Reader { return primary.store }, builderIndexSemantic(mgr))
		view.Close()
	}
	if landed == 0 {
		t.Fatal("no follow-up landed")
	}
	t.Logf("follow-ups landed: %d; stats %+v", landed, c.EnrichmentFollowupStats())
}

// followupMarkerParity compares the view with a clean semantic index before a
// follow-up lands. A difference is excused as an engine residual when the
// primary per-save path holds the view's side of it, and as owed when the
// marker names it: the row is recorded at an owed path, or it is an external
// symbol or module row (the go/types pass derives those for the owed paths).
// Anything else fails.
func followupMarkerParity(t *testing.T, label, repoDir string, view graph.Reader, primary graph.Reader, index func(testing.TB, *store_sqlite.Store, string), owed []string) {
	t.Helper()
	clean := builderOpenStore(t, "followup-marker-clean-"+strings.NewReplacer("/", "-", " ", "-").Replace(label))
	index(t, clean, repoDir)
	owedSet := make(map[string]struct{}, len(owed))
	for _, p := range owed {
		owedSet[p] = struct{}{}
	}
	named := func(row string) bool {
		if strings.HasPrefix(row, "ext::") || strings.HasPrefix(row, "module::go:") || strings.Contains(row, ">ext::") {
			return true
		}
		for p := range owedSet {
			if strings.Contains(row, "|"+p+"|") || strings.Contains(row, "|"+p+":") || strings.HasPrefix(row, p+"::") {
				return true
			}
		}
		return false
	}
	primaryRows := collectKindParityRows(primary)
	var failing []string
	excused := 0
	for _, l := range compareKindParity(collectKindParityRows(view), collectKindParityRows(clean)) {
		rows := primaryRows.nodes[l.kind]
		if l.class == "edge" {
			rows = primaryRows.edges[l.kind]
		}
		inPrimary := map[string]bool{}
		for _, r := range rows {
			inPrimary[r] = true
		}
		for _, r := range append(append([]string(nil), l.onlyView...), l.onlyClean...) {
			switch {
			case named(r):
				excused++
			case inPrimary[r]:
				// an engine residual: the primary per-save path holds it too
			default:
				failing = append(failing, fmt.Sprintf("%s %s: %s", l.class, l.kind, r))
			}
		}
	}
	t.Logf("%s: %d differences named by the marker (%d owed paths)", label, excused, len(owed))
	if len(failing) > 0 {
		t.Errorf("%s: differences the marker does not name:\n  %s", label, strings.Join(failing, "\n  "))
	}
}

// A follow-up yields to the checkout's foreground work: foreground activity
// while it waits or builds cancels it, nothing is published, the debt stays,
// and the follow-up after the next edit enriches both edits' paths.
func TestEnrichmentFollowupYieldsToTheNextEdit(t *testing.T) {
	// The follow-up is off by default until the owner switches it on;
	// these tests switch it on and otherwise run the product's defaults.
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "on")
	f, c, mgr := semanticChainFixtureProduct(t, kindParityTree())
	c.followup.quiet = -1
	coordinatorReconcile(t, c)
	ctx := context.Background()
	edits := kindParityEdits()
	applyKindParityEdit(t, f.worktree, edits[0])
	coordinatorReconcile(t, c)
	first, err := c.SemanticPending(ctx)
	if err != nil || len(first.Paths) == 0 {
		t.Fatalf("the first edit owes nothing (%v)", err)
	}
	route := f.route()

	// Foreground demand while the follow-up runs: it must give way.
	c.demand <- struct{}{}
	canceled := make(chan EnrichmentFollowupOutcome, 1)
	go func() { canceled <- c.runEnrichmentFollowup(ctx) }()
	select {
	case outcome := <-canceled:
		if outcome == followupLanded {
			t.Fatalf("the follow-up landed through foreground demand")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the follow-up neither yielded nor finished")
	}
	<-c.demand
	if got := f.route(); got.DirtyGenerationID != route.DirtyGenerationID {
		t.Fatalf("a yielded follow-up moved the route: %d -> %d", route.DirtyGenerationID, got.DirtyGenerationID)
	}

	// The next edit publishes without waiting; its follow-up takes over the
	// first edit's debt as well.
	applyKindParityEdit(t, f.worktree, edits[1])
	coordinatorReconcile(t, c)
	second, err := c.SemanticPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range first.Paths {
		if !containsString(second.Paths, p) {
			t.Fatalf("the first edit's owed path %s is no longer owed before any follow-up landed", p)
		}
	}
	if outcome := c.runEnrichmentFollowup(ctx); outcome != followupLanded {
		t.Fatalf("follow-up after the second edit: %s", outcome)
	}
	if after, _ := c.SemanticPending(ctx); len(after.Paths) > 0 {
		t.Fatalf("still owed after the follow-up: %v", after.Paths)
	}
	primary := newKindParityPrimaryWith(t, f.worktree, mgr)
	primary.save(append(edits[0].paths(), edits[1].paths()...))
	view := chainMaterialize(t, f)
	defer view.Close()
	kindParityCheckIndexed(t, "after the taken-over follow-up", f.worktree, view.Reader, nil,
		func() graph.Reader { return primary.store }, builderIndexSemantic(mgr))
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// derivedDebt from rows: a deferring layer adds its paths, a complete layer
// covering them clears them, a layer incomplete for good does neither, and a
// later deferring layer owes a cleared path again.
func TestDerivedDebtFromProducerRows(t *testing.T) {
	deferred := func(g int64, paths ...string) derivedDebtLayer {
		return derivedDebtLayer{generation: g, createdAt: time.Unix(g, 0), covered: paths, rows: map[string]store_sqlite.ProducerCompleteness{
			string(graphview.CapSemantic):   {Producer: string(graphview.CapSemantic), State: store_sqlite.ProducerStateIncomplete, Reason: graphview.ReasonDeferredToFollowup},
			string(graphview.CapSimilarity): {Producer: string(graphview.CapSimilarity), State: store_sqlite.ProducerStateIncomplete, Reason: "near-duplicate detection ranks bodies against a corpus"},
		}}
	}
	complete := func(g int64, paths ...string) derivedDebtLayer {
		return derivedDebtLayer{generation: g, createdAt: time.Unix(g, 0), covered: paths, rows: map[string]store_sqlite.ProducerCompleteness{
			string(graphview.CapSemantic): {Producer: string(graphview.CapSemantic), State: store_sqlite.ProducerStateComplete},
		}}
	}
	debt := derivedDebt([]derivedDebtLayer{deferred(1, "r/a.go", "r/b.go"), complete(2, "r/a.go"), deferred(3, "r/c.go")})
	if got := strings.Join(debt[graphview.CapSemantic].Paths, ","); got != "r/b.go,r/c.go" {
		t.Fatalf("semantic debt %q, want r/b.go,r/c.go", got)
	}
	if !debt[graphview.CapSemantic].Since.Equal(time.Unix(1, 0)) {
		t.Fatalf("since %v, want the oldest owing generation", debt[graphview.CapSemantic].Since)
	}
	if len(debt[graphview.CapSimilarity].Paths) != 0 {
		t.Fatalf("a similarity row incomplete for good became debt: %v", debt[graphview.CapSimilarity].Paths)
	}
	debt = derivedDebt([]derivedDebtLayer{deferred(1, "r/a.go"), complete(2, "r/a.go"), deferred(3, "r/a.go")})
	if got := debt[graphview.CapSemantic].Paths; len(got) != 1 || got[0] != "r/a.go" {
		t.Fatalf("a path deferred again after its follow-up: debt %v", got)
	}
}

// A burst accumulates debt: three edits in a row, no follow-up between them.
// After every edit of the burst the view equals the clean semantic index
// except for the rows the marker names, and the debt holds every written path
// of the burst so far; one follow-up then covers all of them, adding one chain
// member, and the view equals the clean index with no exception.
func TestEnrichmentFollowupAfterABurst(t *testing.T) {
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "on")
	f, c, mgr := semanticChainFixtureProduct(t, kindParityTree())
	c.followup.quiet = -1
	coordinatorReconcile(t, c)
	ctx := context.Background()
	primary := newKindParityPrimaryWith(t, f.worktree, mgr)
	var written []string
	depth := 0
	for _, edit := range kindParityEdits()[:3] {
		applyKindParityEdit(t, f.worktree, edit)
		out := coordinatorReconcile(t, c)
		depth = out.DirtyChainDepth
		primary.save(edit.paths())
		for rel := range edit.write {
			written = append(written, builderGraphPath(builderRepoPrefix, rel))
		}
		pending, err := c.SemanticPending(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range written {
			if !containsString(pending.Paths, p) {
				t.Fatalf("burst depth %d: written path %s not owed (owed %v)", depth, p, pending.Paths)
			}
		}
		view := chainMaterialize(t, f)
		followupMarkerParity(t, fmt.Sprintf("burst depth %d/%s", depth, edit.name), f.worktree, view.Reader, primary.store, builderIndexSemantic(mgr), pending.Paths)
		view.Close()
	}
	if outcome := c.runEnrichmentFollowup(ctx); outcome != followupLanded {
		t.Fatalf("the burst's follow-up: %s", outcome)
	}
	if got := f.route(); len(c.dirtyChainMembers(ctx, got.DirtyGenerationID)) != depth+1 {
		t.Fatalf("the follow-up added %d chain members, want 1", len(c.dirtyChainMembers(ctx, got.DirtyGenerationID))-depth)
	}
	if after, _ := c.SemanticPending(ctx); len(after.Paths) > 0 {
		t.Fatalf("owed after the burst's follow-up: %v", after.Paths)
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	kindParityCheckIndexed(t, "after the burst's follow-up", f.worktree, view.Reader, nil,
		func() graph.Reader { return primary.store }, builderIndexSemantic(mgr))
}

// A deferred edit that changes a function body leaves that body's clone rows
// owed (graph.similarity with the token, clone_carry.go); the follow-up
// recomputes them (RecomputeDerivedPaths), and after it lands the view's clone
// rows equal a clean index.
func TestEnrichmentFollowupRecomputesCloneRows(t *testing.T) {
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "on")
	f, c, mgr := semanticChainFixtureProduct(t, kindParityTree())
	c.followup.quiet = -1
	coordinatorReconcile(t, c)
	ctx := context.Background()
	src := kindParityTree()["prod/producers.go"]
	edited := strings.Replace(src, "func CloneB(xs []int) int {\n", "func CloneB(xs []int) int {\n\tif len(xs) == 0 {\n\t\treturn -1\n\t}\n", 1)
	if edited == src {
		t.Fatal("fixture precondition: CloneB's body did not change")
	}
	builderWriteFile(t, f.worktree, "prod/producers.go", edited)
	coordinatorReconcile(t, c)
	debt, _, err := c.PendingDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(debt[graphview.CapSimilarity].Paths, builderGraphPath(builderRepoPrefix, "prod/producers.go")) {
		t.Fatalf("the changed body's clone rows are not owed: %+v", debt)
	}
	if outcome := c.runEnrichmentFollowup(ctx); outcome != followupLanded {
		t.Fatalf("follow-up: %s", outcome)
	}
	primary := newKindParityPrimaryWith(t, f.worktree, mgr)
	primary.save([]string{"prod/producers.go"})
	view := chainMaterialize(t, f)
	defer view.Close()
	kindParityCheckIndexed(t, "after the clone follow-up", f.worktree, view.Reader, nil,
		func() graph.Reader { return primary.store }, builderIndexSemantic(mgr))
}
